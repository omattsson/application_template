package handlers

import (
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"backend/internal/models"
)

// Reasons of a failed local login. They go into the "reason" attribute of the
// log line. Keep the set fixed: dashboards and alerts may match these values.
// The HTTP response never shows the reason.
const (
	// Credential failures (401 "Invalid username or password").
	loginReasonUserNotFound         = "user_not_found"          // no user with this username
	loginReasonPasswordMismatch     = "password_mismatch"       // bcrypt: wrong password
	loginReasonNoPassword           = "no_password"             // the user has no local password
	loginReasonHashInvalid          = "hash_invalid"            // the stored hash is malformed
	loginReasonNotLocal             = "not_local"               // the user signs in through an identity provider
	loginReasonPasswordTooLong      = "password_too_long"       // over maxPasswordBytes: bcrypt would only check a prefix
	loginReasonRereadNotFound       = "reread_not_found"        // the user was deleted during the password check
	loginReasonHashChanged          = "hash_changed"            // the password changed during the password check
	loginReasonDisabled             = "disabled"                // the account is disabled (checked after the password)
	loginReasonLocalLoginNotAllowed = "local_login_not_allowed" // OIDC only and the user is not a service account

	// Server-side failures (500).
	loginReasonLookupError = "lookup_error" // the user lookup failed
	loginReasonRereadError = "reread_error" // the re-read after the password check failed
	loginReasonTokenError  = "token_error"  // the access token could not be signed
)

// maxLoggedUsernameLen caps the username in a log line: the value is client
// input and is not checked before the lookup.
const maxLoggedUsernameLen = 64

// loginFailure describes one failed local login for logLoginFailure.
type loginFailure struct {
	err       error  // server-side cause; logged only for server-side failures
	username  string // submitted username
	clientIP  string // c.ClientIP(): the TCP peer or the client behind TRUSTED_PROXIES
	requestID string // set by middleware.RequestID; empty without it
	userID    string // empty when no user was found
	reason    string // one of the loginReason* constants
	httpCode  int
}

// logLoginFailure writes one structured log line for a failed local login. It
// never logs the password, the password hash or a token.
//
// Level: Warn for credential and account failures (a failed login is a
// security event; LOGIN_RATE_LIMIT and the username throttle bound the
// volume), Error for server-side failures.
func (h *AuthHandler) logLoginFailure(f loginFailure) {
	attrs := []any{
		"reason", f.reason,
		// The username is logged also for user_not_found, the case where it
		// is most often a password typed into the username field. Tradeoff:
		// forensics (credential stuffing, which account a client tried) need
		// it, and the value is capped.
		"username", truncateForLog(f.username, maxLoggedUsernameLen),
		"status", f.httpCode,
		"client_ip", f.clientIP,
		"request_id", f.requestID,
	}
	if f.userID != "" {
		attrs = append(attrs, "user_id", f.userID)
	}
	if f.httpCode >= 500 {
		if f.err != nil {
			attrs = append(attrs, "error", f.err)
		}
		h.log().Error("Login failed", attrs...)
		return
	}
	h.log().Warn("Login failed", attrs...)
}

// truncateForLog cuts s to at most n bytes at a rune boundary and marks the cut.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return truncateAtRune(s, n) + "..."
}

// truncateAtRune cuts s to at most n bytes without splitting a rune.
func truncateAtRune(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// Settings of the per-username login throttle. After loginFailureThreshold
// credential failures for one username within loginFailureWindow, every login
// for that username gets 429 for loginFailureWindow (also with the correct
// password). It complements the per-IP LOGIN_RATE_LIMIT: that one limits one
// client, this one limits guesses at one account from many clients.
//
// The state is in memory and per replica: with N replicas a username can get
// up to N*loginFailureThreshold guesses per window.
const (
	loginFailureThreshold = 5
	loginFailureWindow    = 15 * time.Minute
	// loginThrottleMaxEntries bounds the memory of the throttle (one entry is
	// a key of at most models.MaxUsernameLen bytes and a few words).
	loginThrottleMaxEntries = 10000
)

// throttleEntry is the failure state of one username.
type throttleEntry struct {
	windowStart time.Time // first failure of the current window
	lockedUntil time.Time // zero: not locked
	failures    int
}

// expiry is the time after which the entry has no effect.
func (e *throttleEntry) expiry(window time.Duration) time.Time {
	end := e.windowStart.Add(window)
	if e.lockedUntil.After(end) {
		return e.lockedUntil
	}
	return end
}

// loginThrottle counts credential failures per username. It is safe for
// concurrent use. The map holds at most maxEntries entries: when it is full,
// expired entries are dropped first, then the counter (not locked) that
// expires first, and a lock only when every entry is locked. A flood of new
// usernames therefore cannot evict a lock until it has locked maxEntries
// usernames itself. There is no background goroutine.
type loginThrottle struct {
	entries    map[string]*throttleEntry
	now        func() time.Time
	window     time.Duration
	threshold  int
	maxEntries int
	mu         sync.Mutex
}

func newLoginThrottle(threshold int, window time.Duration, maxEntries int) *loginThrottle {
	return &loginThrottle{
		entries:    make(map[string]*throttleEntry),
		now:        time.Now,
		window:     window,
		threshold:  threshold,
		maxEntries: maxEntries,
	}
}

// loginThrottleKey returns the throttle key of a submitted username: lower
// case (the default MySQL collation matches usernames case-insensitively, so
// "Alice" and "alice" are the same account) and at most models.MaxUsernameLen
// bytes (a longer name cannot exist, and the key size stays bounded).
func loginThrottleKey(username string) string {
	return truncateAtRune(strings.ToLower(truncateAtRune(username, 4*models.MaxUsernameLen)), models.MaxUsernameLen)
}

// check reports whether key is locked and for how long.
func (t *loginThrottle) check(key string) (retryAfter time.Duration, blocked bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[key]
	if !ok {
		return 0, false
	}
	now := t.now()
	if !now.Before(e.expiry(t.window)) {
		delete(t.entries, key)
		return 0, false
	}
	if now.Before(e.lockedUntil) {
		return e.lockedUntil.Sub(now), true
	}
	return 0, false
}

// recordFailure counts one credential failure for key and locks it at the
// threshold.
func (t *loginThrottle) recordFailure(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	e, ok := t.entries[key]
	if ok && !now.Before(e.expiry(t.window)) {
		delete(t.entries, key)
		ok = false
	}
	if !ok {
		t.makeRoom(now)
		e = &throttleEntry{windowStart: now}
		t.entries[key] = e
	}
	e.failures++
	if e.failures >= t.threshold && !now.Before(e.lockedUntil) {
		e.lockedUntil = now.Add(t.window)
	}
}

// makeRoom frees one slot when the map is full. The caller holds t.mu.
func (t *loginThrottle) makeRoom(now time.Time) {
	if len(t.entries) < t.maxEntries {
		return
	}
	for k, e := range t.entries {
		if !now.Before(e.expiry(t.window)) {
			delete(t.entries, k)
		}
	}
	if len(t.entries) < t.maxEntries {
		return
	}
	var victim string
	var victimExpiry time.Time
	victimLocked := true
	for k, e := range t.entries {
		locked := now.Before(e.lockedUntil)
		exp := e.expiry(t.window)
		// An unlocked counter always goes before a lock; within the same
		// kind the one that expires first goes.
		if victim == "" || (victimLocked && !locked) || (locked == victimLocked && exp.Before(victimExpiry)) {
			victim, victimExpiry, victimLocked = k, exp, locked
		}
	}
	delete(t.entries, victim)
}

// reset clears key (successful login).
func (t *loginThrottle) reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, key)
}

// size returns the number of entries.
func (t *loginThrottle) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}

// failures returns the failure count of key (tests).
func (t *loginThrottle) failures(key string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.entries[key]; ok {
		return e.failures
	}
	return 0
}

// retryAfterSeconds formats d as a Retry-After value: whole seconds, rounded
// up, at least 1.
func retryAfterSeconds(d time.Duration) string {
	secs := int64((d + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return strconv.FormatInt(secs, 10)
}

// log returns the logger of the handler (slog.Default when none is set).
func (h *AuthHandler) log() *slog.Logger {
	if h.logger != nil {
		return h.logger
	}
	return slog.Default()
}
