package handlers

import (
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"backend/internal/api/middleware"
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
	// loginReasonAccountThrottled: the account key of the found user is
	// locked or busy (any spelling, also the stored name); the password is
	// not checked.
	loginReasonAccountThrottled = "account_throttled"
	// loginReasonRecheckFailed: the user was disabled or deleted while the
	// token was signed. loginReasonSessionRevoked: a logout-all or revoke
	// ended the new session before the answer.
	loginReasonRecheckFailed  = "recheck_failed"
	loginReasonSessionRevoked = "session_revoked"

	// Server-side failures (500).
	loginReasonLookupError  = "lookup_error"  // the user lookup failed
	loginReasonCompareError = "compare_error" // the request ended (deadline) while it waited for a bcrypt slot
	loginReasonRereadError  = "reread_error"  // the re-read after the password check failed
	loginReasonTokenError   = "token_error"   // the access token could not be signed
	loginReasonRecheckError = "recheck_error" // the re-read after signing failed
	loginReasonSessionError = "session_error" // the refresh session could not be stored or checked
)

// knownLoginReasons is the set of loginReason* constants. The "reason" metric
// label takes only these: anything else is "other", so no caller can open a
// series per free-text value.
var knownLoginReasons = map[string]bool{
	loginReasonUserNotFound: true, loginReasonPasswordMismatch: true, loginReasonNoPassword: true,
	loginReasonHashInvalid: true, loginReasonNotLocal: true, loginReasonPasswordTooLong: true,
	loginReasonRereadNotFound: true, loginReasonHashChanged: true, loginReasonDisabled: true,
	loginReasonLocalLoginNotAllowed: true, loginReasonAccountThrottled: true, loginReasonRecheckFailed: true,
	loginReasonSessionRevoked: true, loginReasonLookupError: true, loginReasonCompareError: true,
	loginReasonRereadError: true, loginReasonTokenError: true, loginReasonRecheckError: true,
	loginReasonSessionError: true,
}

// boundedLoginReason returns reason when it is a loginReason* constant, else
// "other".
func boundedLoginReason(reason string) string {
	if knownLoginReasons[reason] {
		return reason
	}
	return "other"
}

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
	// The reason is one of the loginReason* constants, so the label stays
	// low-cardinality. Throttled attempts are counted by their own log line.
	middleware.RecordLoginFailure("local", "failure", boundedLoginReason(f.reason))
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
// credential failures for one key within loginFailureWindow, every login for
// that key is refused for loginFailureWindow (also with the correct password).
// It complements the per-IP LOGIN_RATE_LIMIT: that one limits one client,
// this one limits guesses at one account from many clients.
//
// An attempt reserves its slot before the password check, so a parallel
// burst cannot get more than loginFailureThreshold guesses in.
//
// Only a name key (the submitted name, checked before the user lookup, for
// every name known or not) answers 429: Retry-After loginFailureWindow when it
// is locked, loginBusyRetryAfter when its slots are taken by attempts in
// flight. A locked or busy account key (the found user, any spelling of the
// name) answers the generic 401 "Invalid username or password", like a wrong
// password, so no answer tells whether a name is an account.
//
// The state is in memory and per replica: with N replicas a username can get
// up to N*loginFailureThreshold guesses per window.
const (
	loginFailureThreshold = 5
	loginFailureWindow    = 15 * time.Minute
	// loginBusyRetryAfter is the Retry-After of an attempt refused because
	// the slots of its key are taken by attempts in flight.
	loginBusyRetryAfter = time.Second
	// loginThrottleMaxEntries bounds the memory of the throttle (one entry is
	// a key of at most models.MaxUsernameLen bytes and a few words).
	loginThrottleMaxEntries = 10000
)

// attemptOutcome is how a reserved login attempt ended.
type attemptOutcome int

const (
	// outcomeNeutral releases the slot without counting (server error,
	// cancelled request, panic). It is the default.
	outcomeNeutral attemptOutcome = iota
	// outcomeFailure counts a credential failure (401).
	outcomeFailure
	// outcomeSuccess clears the failures of the key.
	outcomeSuccess
)

// throttleEntry is the state of one key.
type throttleEntry struct {
	windowStart time.Time // first failure of the current window
	lockedUntil time.Time // zero: not locked
	failures    int
	inflight    int // reserved attempts that have not settled
}

// expiry is the time after which the failures and the lock have no effect.
func (e *throttleEntry) expiry(window time.Duration) time.Time {
	end := e.windowStart.Add(window)
	if e.lockedUntil.After(end) {
		return e.lockedUntil
	}
	return end
}

// roll drops failures and the lock once they expired. It keeps inflight.
func (e *throttleEntry) roll(now time.Time, window time.Duration) {
	if e.failures > 0 && !now.Before(e.expiry(window)) {
		e.failures = 0
		e.lockedUntil = time.Time{}
	}
}

// idle reports whether the entry holds no state any more (it can be deleted).
func (e *throttleEntry) idle() bool {
	return e.inflight == 0 && e.failures == 0 && e.lockedUntil.IsZero()
}

// loginThrottle counts credential failures and attempts in flight per key. It
// is safe for concurrent use. The map holds at most maxEntries entries; an
// entry with an attempt in flight is never evicted. When the map is full,
// expired entries go first, then the counter (not locked) that expires first,
// and a lock only when every evictable entry is locked; when every entry has
// an attempt in flight, a new key is refused (fail closed). A flood of new
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

// loginThrottleKey returns the folded form of a submitted username: lower
// case and at most models.MaxUsernameLen bytes (a longer name cannot exist,
// and the key size stays bounded). nameThrottleKey prefixes it.
//
// The folded name is not canonical: the users table uses the MySQL 8 default
// collation utf8mb4_0900_ai_ci, which also ignores accents and width (and
// folds ß/ss), so "ádmin" or "ａdmin" find the user "admin". Login therefore
// also reserves the account key (accountThrottleKey) after a successful
// lookup.
func loginThrottleKey(username string) string {
	return truncateAtRune(strings.ToLower(truncateAtRune(username, 4*models.MaxUsernameLen)), models.MaxUsernameLen)
}

// The throttle has two key namespaces. They never share an entry: an account
// key and a name key are distinct even when the submitted name equals the
// stored one.
//
//   - Name keys ("n:" + folded submitted name) are reserved before the lookup,
//     for every name, known or not. Only they produce a 429, so a 429 says
//     nothing about whether an account exists.
//   - Account keys ("u:" + user ID) are reserved after a successful lookup.
//     They cap the password checks of one account over every spelling. A
//     locked or busy account key gives the generic 401 (dummy compare, no
//     password check) for every spelling, including the stored name.
//
// Each attempt counts once on each key it holds. A direct login reserves
// n:<name> and u:<id> and counts on both, so both reach the threshold
// together: the account still gets loginFailureThreshold guesses per window,
// not fewer.
const (
	nameKeyPrefix    = "n:"
	accountKeyPrefix = "u:"
)

// nameThrottleKey is the name key of a submitted username.
func nameThrottleKey(username string) string { return nameKeyPrefix + loginThrottleKey(username) }

// accountThrottleKey is the account key of a user ID.
func accountThrottleKey(userID string) string { return accountKeyPrefix + userID }

// loginAttempt is one login with its reserved keys. settle ends it exactly
// once with the outcome set by the handler.
type loginAttempt struct {
	t       *loginThrottle
	keys    []string
	outcome attemptOutcome
	settled bool
}

// begin reserves a slot under key for a new attempt. It returns nil and the
// Retry-After when the key is locked, its slots are taken, or the map is full
// of attempts in flight.
func (t *loginThrottle) begin(key string) (*loginAttempt, time.Duration) {
	if retryAfter, ok := t.reserve(key); !ok {
		return nil, retryAfter
	}
	return &loginAttempt{t: t, keys: []string{key}}, 0
}

// add reserves a second key (the account key) for the attempt. A key that the
// attempt already holds is not reserved twice. It returns false and the
// Retry-After when the key is refused; the attempt keeps its other keys.
func (a *loginAttempt) add(key string) (time.Duration, bool) {
	for _, k := range a.keys {
		if k == key {
			return 0, true
		}
	}
	retryAfter, ok := a.t.reserve(key)
	if ok {
		a.keys = append(a.keys, key)
	}
	return retryAfter, ok
}

// settle releases every reserved key with the outcome. Later calls do
// nothing.
func (a *loginAttempt) settle() {
	if a == nil || a.settled {
		return
	}
	a.settled = true
	for _, k := range a.keys {
		a.t.release(k, a.outcome)
	}
}

// reserve takes one slot of key.
func (t *loginThrottle) reserve(key string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	e, ok := t.entries[key]
	if !ok {
		if !t.makeRoom(now) {
			return loginBusyRetryAfter, false
		}
		e = &throttleEntry{}
		t.entries[key] = e
	}
	e.roll(now, t.window)
	if now.Before(e.lockedUntil) {
		return e.lockedUntil.Sub(now), false
	}
	if e.failures+e.inflight >= t.threshold {
		return loginBusyRetryAfter, false
	}
	e.inflight++
	return 0, true
}

// release returns one slot of key and applies the outcome.
func (t *loginThrottle) release(key string, outcome attemptOutcome) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[key]
	if !ok {
		return
	}
	if e.inflight > 0 {
		e.inflight--
	}
	now := t.now()
	e.roll(now, t.window)
	switch outcome {
	case outcomeFailure:
		if e.failures == 0 {
			e.windowStart = now
		}
		e.failures++
		if e.failures >= t.threshold && !now.Before(e.lockedUntil) {
			e.lockedUntil = now.Add(t.window)
		}
	case outcomeSuccess:
		e.failures = 0
		e.lockedUntil = time.Time{}
	case outcomeNeutral:
	}
	if e.idle() {
		delete(t.entries, key)
	}
}

// clear forgets the failures and the lock of key. A password reset by mail
// calls it for the account and the name of the user: the owner proved control
// of the mailbox, so a lock that guessing caused must not keep them out. An
// attempt in flight keeps its slot (its release still balances), and the entry
// goes when nothing is left in it.
func (t *loginThrottle) clear(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	e, ok := t.entries[key]
	if !ok {
		return
	}

	e.failures = 0
	e.lockedUntil = time.Time{}
	e.windowStart = time.Time{}

	if e.idle() {
		delete(t.entries, key)
	}
}

// check reports whether key is locked and for how long. It reserves nothing.
func (t *loginThrottle) check(key string) (retryAfter time.Duration, blocked bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[key]
	if !ok {
		return 0, false
	}
	now := t.now()
	e.roll(now, t.window)
	if e.idle() {
		delete(t.entries, key)
		return 0, false
	}
	if now.Before(e.lockedUntil) {
		return e.lockedUntil.Sub(now), true
	}
	return 0, false
}

// makeRoom frees one slot when the map is full and reports whether there is
// room. The caller holds t.mu.
func (t *loginThrottle) makeRoom(now time.Time) bool {
	if len(t.entries) < t.maxEntries {
		return true
	}
	for k, e := range t.entries {
		e.roll(now, t.window)
		if e.idle() {
			delete(t.entries, k)
		}
	}
	if len(t.entries) < t.maxEntries {
		return true
	}
	var victim string
	var victimExpiry time.Time
	victimLocked := true
	for k, e := range t.entries {
		if e.inflight > 0 {
			continue
		}
		locked := now.Before(e.lockedUntil)
		exp := e.expiry(t.window)
		// An unlocked counter always goes before a lock; within the same
		// kind the one that expires first goes.
		if victim == "" || (victimLocked && !locked) || (locked == victimLocked && exp.Before(victimExpiry)) {
			victim, victimExpiry, victimLocked = k, exp, locked
		}
	}
	if victim == "" {
		return false
	}
	delete(t.entries, victim)
	return true
}

// size returns the number of entries.
func (t *loginThrottle) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
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
