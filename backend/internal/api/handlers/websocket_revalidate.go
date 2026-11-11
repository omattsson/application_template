package handlers

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"reflect"
	"sort"
	"sync"
	"time"

	"backend/internal/models"
	"backend/internal/sessionstore"
	"backend/internal/websocket"
)

// revalidateChunkSize is the number of identities whose API keys and users one
// batch reads. A run works through the identities chunk by chunk, so a run
// that runs out of time has still finished the chunks before.
const revalidateChunkSize = 200

// NewWebSocketRevocationChecker returns the check of the open sockets: the
// hub runs it every revalidation interval (Hub.StartRevalidation) and the
// WebSocket handler runs it once on each new socket (SetRecheck). For each
// identity it applies the checks of the HTTP auth:
//   - with keys, for a socket opened with an API key: a deleted or expired
//     key, or a key of another user;
//   - with users: a deleted or disabled user;
//   - with store: the token, session and user blocklists, with the rules of
//     middleware.RevocationStatus (a user block covers the sockets issued at
//     or before it, to the millisecond; no issue time counts as blocked).
//
// Cost of a run: keys and users are read in one batch per chunk of
// revalidateChunkSize identities (FindByIDs); each distinct token and session
// is looked up once; each distinct user needs one user-block lookup (at the
// oldest issue time of its sockets: not blocked there means not blocked for
// any of them) or two (also at the newest: blocked there means blocked for
// all), and one per further issue time only when a block falls between the
// sockets of the user.
//
// A failed lookup is one of two things:
//   - The run ran out of time or was cancelled (the run context ended): the
//     identities it did not reach are not checked in this run and stay open.
//     The next run starts with them (least recently checked first), so a run
//     that is always too slow still checks every identity in turn.
//   - The lookup itself failed while the run had time (a store or database
//     error, also a query timeout of its own): failClosed selects the policy
//     for the identities that need that lookup. true reports them revoked, so
//     their sockets close (the production wiring, R20: a revocation that
//     cannot be checked must not keep a revoked socket open); false lets them
//     pass. The failed dependency is not queried again in the run: a key
//     batch failure concerns only the API key sockets, a user batch failure
//     the sockets of a user, a session store failure every later identity.
//
// Errors are logged at most once a minute (the rest at debug level); a
// cancelled run (hub shutdown) logs at debug level. Each dependency may be nil
// to skip its checks; with all nil it returns nil.
func NewWebSocketRevocationChecker(store sessionstore.SessionStore, users models.UserRepository, keys models.APIKeyRepository, failClosed bool) websocket.IdentityChecker {
	if isNilDep(store) && isNilDep(users) && isNilDep(keys) {
		return nil
	}
	if isNilDep(store) {
		store = nil
	}
	if isNilDep(users) {
		users = nil
	}
	if isNilDep(keys) {
		keys = nil
	}
	c := &revocationChecker{
		store: store, users: users, keys: keys, failClosed: failClosed,
		errLog:      &logLimiter{interval: time.Minute},
		slowLog:     &logLimiter{interval: time.Minute},
		lastChecked: map[checkKey]uint64{},
	}
	return c.check
}

// isNilDep reports whether a dependency interface is nil or holds a nil
// pointer (a typed nil is not == nil and would be called).
func isNilDep(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// checkKey is the comparable form of a socket identity.
type checkKey struct {
	userID, tokenID, sessionID, apiKeyID string
	issuedAtMs                           int64
}

func keyOfIdentity(id websocket.ClientIdentity) checkKey {
	return checkKey{userID: id.UserID, tokenID: id.TokenID, sessionID: id.SessionID, apiKeyID: id.APIKeyID, issuedAtMs: issueMs(id.IssuedAt)}
}

// noIssueTime is the issue time of an identity without one: below every real
// time, and blocked by every user block (the rule of the HTTP auth).
const noIssueTime = math.MinInt64

// issueMs is the issue time at the precision of the user blocklist.
func issueMs(t time.Time) int64 {
	if t.IsZero() {
		return noIssueTime
	}
	return t.UnixMilli()
}

func issueTimeOf(ms int64) time.Time {
	if ms == noIssueTime {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

type revocationChecker struct {
	store      sessionstore.SessionStore
	users      models.UserRepository
	keys       models.APIKeyRepository
	errLog     *logLimiter
	slowLog    *logLimiter
	failClosed bool

	mu sync.Mutex
	// run counts the multi-identity runs; lastChecked holds the run in which
	// each identity was last checked (absent: never), for the order of the
	// next run. Only the identities of the last run are kept.
	run         uint64
	lastChecked map[checkKey]uint64
}

func (r *revocationChecker) check(ctx context.Context, identities []websocket.ClientIdentity) []bool {
	cr := newCheckRun(r, ctx, identities)
	cr.process(r.order(identities))
	r.remember(identities, cr.decided)
	return cr.revoked
}

// order returns the indices of identities, least recently checked first
// (never checked first of all); ties keep the given order. A single identity
// (the re-check of a new socket) does not use the state.
func (r *revocationChecker) order(identities []websocket.ClientIdentity) []int {
	idx := make([]int, len(identities))
	for i := range idx {
		idx[i] = i
	}
	if len(identities) < 2 {
		return idx
	}
	last := make([]uint64, len(identities))
	r.mu.Lock()
	for i, id := range identities {
		last[i] = r.lastChecked[keyOfIdentity(id)]
	}
	r.mu.Unlock()
	sort.SliceStable(idx, func(a, b int) bool { return last[idx[a]] < last[idx[b]] })
	return idx
}

// remember records which identities this run checked. It keeps only the
// identities of this run (the closed sockets drop out). A single-identity
// call changes nothing.
func (r *revocationChecker) remember(identities []websocket.ClientIdentity, decided []bool) {
	if len(identities) < 2 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.run++
	next := make(map[checkKey]uint64, len(identities))
	for i, id := range identities {
		k := keyOfIdentity(id)
		switch {
		case decided[i]:
			next[k] = r.run
		default:
			if v, ok := r.lastChecked[k]; ok {
				next[k] = v
			}
		}
	}
	r.lastChecked = next
}

// userBlockState caches the user-block lookups of one user in a run.
type userBlockState struct {
	exact              map[int64]bool
	minMs, maxMs       int64
	minKnown, maxKnown bool
	minBlocked         bool
	maxBlocked         bool
}

// checkRun is one run of the checker.
type checkRun struct {
	ctx        context.Context
	r          *revocationChecker
	identities []websocket.ClientIdentity
	revoked    []bool
	// decided marks the identities this run checked (or revoked on a failed
	// lookup); the others were not reached.
	decided []bool

	keys       map[string]*models.APIKey // key ID -> key; a looked-up ID that is missing does not exist
	keysLooked map[string]bool
	keysFailed bool

	users       map[string]*models.User
	usersLooked map[string]bool
	usersFailed bool

	storeFailed    bool
	tokenBlocked   map[string]bool
	sessionBlocked map[string]bool
	userBlocks     map[string]*userBlockState
}

func newCheckRun(r *revocationChecker, ctx context.Context, identities []websocket.ClientIdentity) *checkRun {
	cr := &checkRun{
		ctx: ctx, r: r, identities: identities,
		revoked:        make([]bool, len(identities)),
		decided:        make([]bool, len(identities)),
		keys:           map[string]*models.APIKey{},
		keysLooked:     map[string]bool{},
		users:          map[string]*models.User{},
		usersLooked:    map[string]bool{},
		tokenBlocked:   map[string]bool{},
		sessionBlocked: map[string]bool{},
		userBlocks:     map[string]*userBlockState{},
	}
	// The oldest and newest issue time of each user's sockets, for the
	// user-block lookups.
	for _, id := range identities {
		if id.UserID == "" {
			continue
		}
		ms := issueMs(id.IssuedAt)
		st, ok := cr.userBlocks[id.UserID]
		if !ok {
			cr.userBlocks[id.UserID] = &userBlockState{minMs: ms, maxMs: ms, exact: map[int64]bool{}}
			continue
		}
		st.minMs = min(st.minMs, ms)
		st.maxMs = max(st.maxMs, ms)
	}
	return cr
}

// process checks the identities in this order, chunk by chunk, until all are
// checked or the run context ends.
func (cr *checkRun) process(order []int) {
	for start := 0; start < len(order); start += revalidateChunkSize {
		chunk := order[start:min(start+revalidateChunkSize, len(order))]
		if !cr.loadKeys(chunk) || !cr.loadUsers(chunk) {
			cr.ranOut(start)
			return
		}
		for n, i := range chunk {
			if !cr.checkOne(i) {
				cr.ranOut(start + n)
				return
			}
		}
	}
}

// ended reports whether the run context ended (a cancel or the deadline of
// the run). A lookup error after that is the end of the run, not a failure of
// the store: an error of a lookup whose own query timed out while the run
// still had time is a failure.
func (cr *checkRun) ended() bool { return cr.ctx.Err() != nil }

// ranOut logs the end of a run that did not reach every identity.
func (cr *checkRun) ranOut(checked int) {
	left := len(cr.identities) - checked
	if errors.Is(cr.ctx.Err(), context.Canceled) {
		slog.Debug("WebSocket revalidation cancelled", "checked", checked, "identities", len(cr.identities))
		return
	}
	msg := "WebSocket revalidation ran out of time; the next run starts with the unchecked sockets"
	if cr.r.slowLog.allow() {
		slog.Warn(msg, "checked", checked, "unchecked", left, "identities", len(cr.identities))
		return
	}
	slog.Debug(msg, "checked", checked, "unchecked", left, "identities", len(cr.identities))
}

// failure logs a lookup that failed while the run still had time.
func (cr *checkRun) failure(what string, err error, args ...any) {
	args = append(args, "identities", len(cr.identities), "fail_closed", cr.r.failClosed, "error", err)
	msg := "WebSocket revalidation: " + what + " failed; the next run retries"
	if cr.r.errLog.allow() {
		slog.Error(msg, args...)
		return
	}
	slog.Debug(msg, args...)
}

// loadKeys reads the API keys of the chunk that were not read yet. It returns
// false when the run ended during the read.
func (cr *checkRun) loadKeys(chunk []int) bool {
	if cr.r.keys == nil || cr.keysFailed {
		return true
	}
	var ids []string
	for _, i := range chunk {
		if k := cr.identities[i].APIKeyID; k != "" && !cr.keysLooked[k] {
			cr.keysLooked[k] = true
			ids = append(ids, k)
		}
	}
	if len(ids) == 0 {
		return true
	}
	found, err := cr.r.keys.FindByIDs(cr.ctx, ids)
	if err != nil {
		if cr.ended() {
			return false
		}
		cr.keysFailed = true
		cr.failure("API key lookup", err, "keys", len(ids))
		return true
	}
	for _, k := range found {
		cr.keys[k.ID] = k
	}
	return true
}

// loadUsers reads the users of the chunk that were not read yet. It returns
// false when the run ended during the read.
func (cr *checkRun) loadUsers(chunk []int) bool {
	if cr.r.users == nil || cr.usersFailed {
		return true
	}
	var ids []string
	for _, i := range chunk {
		if u := cr.identities[i].UserID; u != "" && !cr.usersLooked[u] {
			cr.usersLooked[u] = true
			ids = append(ids, u)
		}
	}
	if len(ids) == 0 {
		return true
	}
	found, err := cr.r.users.FindByIDs(cr.ctx, ids)
	if err != nil {
		if cr.ended() {
			return false
		}
		cr.usersFailed = true
		cr.failure("user lookup", err, "users", len(ids))
		return true
	}
	for id, u := range found {
		cr.users[id] = u
	}
	return true
}

// checkOne checks identity i. It returns false when the run ended before
// the identity was checked (it stays open and undecided).
func (cr *checkRun) checkOne(i int) bool {
	id := cr.identities[i]
	decide := func(revoked bool) bool {
		cr.revoked[i] = revoked
		cr.decided[i] = true
		return true
	}
	if cr.keyRevoked(id) || cr.userRevoked(id) {
		return decide(true)
	}
	if cr.r.store == nil {
		return decide(false)
	}
	blocked, ok := cr.storeChecks(id)
	switch {
	case !ok:
		return false
	case cr.storeFailed && !cr.r.failClosed:
		// Fail open: the identity passes, but it was not checked.
		return true
	case cr.storeFailed:
		return decide(true)
	default:
		return decide(blocked)
	}
}

// keyRevoked reports whether the API key of the identity revokes it: deleted,
// expired or of another user. After a failed key lookup in this run, fail
// closed revokes and fail open lets the identity go on to the next checks.
func (cr *checkRun) keyRevoked(id websocket.ClientIdentity) bool {
	if id.APIKeyID == "" || cr.r.keys == nil {
		return false
	}
	if cr.keysFailed {
		return cr.r.failClosed
	}
	key := cr.keys[id.APIKeyID]
	return key == nil || key.UserID != id.UserID || (key.ExpiresAt != nil && !time.Now().Before(*key.ExpiresAt))
}

// userRevoked reports whether the user of the identity is deleted or
// disabled, with the failure policy of keyRevoked.
func (cr *checkRun) userRevoked(id websocket.ClientIdentity) bool {
	if id.UserID == "" || cr.r.users == nil {
		return false
	}
	if cr.usersFailed {
		return cr.r.failClosed
	}
	u := cr.users[id.UserID]
	return u == nil || u.Disabled
}

// blockLookup is one blocklist lookup by key, cached for the run.
type blockLookup struct {
	cache map[string]bool
	key   string
	fn    func(context.Context, string) (bool, error)
	what  string
}

// cachedLookup runs a blocklist lookup once per key and run. ok is false when
// the run ended during it; a failure sets storeFailed.
func (cr *checkRun) cachedLookup(l blockLookup) (blocked, ok bool) {
	if v, seen := l.cache[l.key]; seen {
		return v, true
	}
	v, err := l.fn(cr.ctx, l.key)
	if err != nil {
		if cr.ended() {
			return false, false
		}
		cr.storeFailed = true
		cr.failure(l.what, err)
		return false, true
	}
	l.cache[l.key] = v
	return v, true
}

// storeChecks runs the blocklist lookups of an identity (each distinct key
// once per run). ok is false when the run ended during a lookup. After a
// store failure (storeFailed) it does not query the store again.
func (cr *checkRun) storeChecks(id websocket.ClientIdentity) (blocked, ok bool) {
	if cr.storeFailed {
		return false, true
	}
	for _, l := range []blockLookup{
		{cache: cr.tokenBlocked, key: id.TokenID, fn: cr.r.store.IsTokenBlocked, what: "token blocklist check"},
		{cache: cr.sessionBlocked, key: id.SessionID, fn: cr.r.store.IsSessionBlocked, what: "session blocklist check"},
	} {
		if l.key == "" {
			continue
		}
		if v, ok := cr.cachedLookup(l); !ok || v || cr.storeFailed {
			return v, ok
		}
	}
	if id.UserID == "" {
		return false, true
	}
	return cr.userBlocked(id.UserID, issueMs(id.IssuedAt))
}

// userBlocked reports whether a user block covers a socket of the user
// issued at ms. The blocklist rule is monotonic in the issue time (a block
// covers every socket issued at or before it), so the lookups at the oldest
// and newest issue time of the user's sockets decide most sockets.
func (cr *checkRun) userBlocked(userID string, ms int64) (blocked, ok bool) {
	st := cr.userBlocks[userID]
	query := func(at int64) (bool, bool) {
		v, err := cr.r.store.IsUserBlocked(cr.ctx, userID, issueTimeOf(at))
		if err != nil {
			if cr.ended() {
				return false, false
			}
			cr.storeFailed = true
			cr.failure("user blocklist check", err, "user_id", userID)
			return false, true
		}
		return v, true
	}
	if !st.minKnown {
		v, ok := query(st.minMs)
		if !ok || cr.storeFailed {
			return false, ok
		}
		st.minKnown, st.minBlocked = true, v
	}
	if !st.minBlocked || ms == st.minMs {
		return st.minBlocked, true
	}
	if !st.maxKnown {
		v, ok := query(st.maxMs)
		if !ok || cr.storeFailed {
			return false, ok
		}
		st.maxKnown, st.maxBlocked = true, v
	}
	if st.maxBlocked || ms == st.maxMs {
		return st.maxBlocked, true
	}
	if v, seen := st.exact[ms]; seen {
		return v, true
	}
	v, ok := query(ms)
	if !ok || cr.storeFailed {
		return false, ok
	}
	st.exact[ms] = v
	return v, true
}
