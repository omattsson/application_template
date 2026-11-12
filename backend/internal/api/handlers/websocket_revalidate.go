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

// defaultLookupTimeout bounds each lookup of a run (one batch read or one
// blocklist check), well below websocket.RevalidateTimeout: a hung store or
// database connection then fails the lookup while the run still has time,
// which fails closed, instead of using up the run.
const defaultLookupTimeout = 5 * time.Second

// defaultMaxUnverifiedRuns is the number of revalidation runs in a row in
// which an identity may stay unverified before the checker closes it (fail
// closed).
const defaultMaxUnverifiedRuns = 3

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
// Each lookup has its own timeout (defaultLookupTimeout, 5s, below the 30s
// websocket.RevalidateTimeout of a run). A failed lookup is one of two
// things:
//   - The lookup failed while the run had time (a store or database error,
//     or its own timeout: a hung connection): failClosed selects the policy
//     for the identities that need that lookup. true reports them revoked, so
//     their sockets close (the production wiring, R20: a revocation that
//     cannot be checked must not keep a revoked socket open); false lets them
//     pass, unverified. The failed dependency is not queried again in the
//     run: a key batch failure concerns only the API key sockets, a user batch
//     failure the sockets of a user, a session store failure every later
//     identity.
//   - The run ran out of time or was cancelled (the run context ended): the
//     identities it did not verify stay open in this run.
//
// Unverified identities, in degraded mode: a revalidation run checks the
// identities in the order of their last verification, oldest first; an
// identity the checker has not seen yet counts as verified when it is first
// seen (the WebSocket handler checked it at registration), so new sockets
// cannot get ahead of older unverified ones. With failClosed, an identity
// that stays unverified (not reached, or a lookup failed) in
// defaultMaxUnverifiedRuns (M = 3) revalidation runs in a row is closed; a
// verification resets the count, so a healthy run that reaches every
// identity closes nothing. A cancelled run (hub shutdown) counts for nothing.
//
// Worst case, with failClosed and a store that stays slow without failing:
// a socket revoked at time t closes at the end of the M-th run that starts
// after t, so within M*P + T of t, with T = websocket.RevalidateTimeout and
// P = max(revalidation interval, T) the most time between two run starts;
// with the defaults (1m interval, 30s, M = 3): 3m30s.
//
// A store that fails closes the sockets that need it in the first run that
// starts after the failure: within P + T. A store that hangs does the same
// (the lookup times out after defaultLookupTimeout, which is a failed lookup),
// with one exception: when the hang begins within the last
// defaultLookupTimeout of a run, the run ends before the lookup times out.
// That is the run running out of time, not a failed lookup, so nothing closes
// in it and the next run closes the sockets: within 2P + T.
//
// The re-check of a new socket neither uses nor changes this state. Errors
// are logged at most once a minute (the rest at debug level); a cancelled
// run logs at debug level. Each dependency may be nil to skip its checks;
// with all nil it returns nil.
func NewWebSocketRevocationChecker(store sessionstore.SessionStore, users models.UserRepository, keys models.APIKeyRepository, failClosed bool) websocket.IdentityChecker {
	c := newRevocationChecker(store, users, keys, checkerConfig{
		failClosed:        failClosed,
		lookupTimeout:     defaultLookupTimeout,
		maxUnverifiedRuns: defaultMaxUnverifiedRuns,
	})
	if c == nil {
		return nil
	}
	return c.check
}

// checkerConfig holds the policy of a revocation checker.
type checkerConfig struct {
	// lookupTimeout bounds each lookup; <= 0: only the run context.
	lookupTimeout time.Duration
	// maxUnverifiedRuns is the number of runs in a row an identity may stay
	// unverified before it is closed (with failClosed); <= 0: never.
	maxUnverifiedRuns int
	failClosed        bool
}

func newRevocationChecker(store sessionstore.SessionStore, users models.UserRepository, keys models.APIKeyRepository, cfg checkerConfig) *revocationChecker {
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
	return &revocationChecker{
		store: store, users: users, keys: keys, cfg: cfg,
		errLog:  &logLimiter{interval: time.Minute},
		slowLog: &logLimiter{interval: time.Minute},
		state:   map[checkKey]identityState{},
	}
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

// registrationRecheckKey marks the context of the re-check of a new socket.
type registrationRecheckKey struct{}

// withRegistrationRecheck marks ctx as the re-check of a new socket: the
// checker then runs without the order and the unverified counts of the
// revalidation runs.
func withRegistrationRecheck(ctx context.Context) context.Context {
	return context.WithValue(ctx, registrationRecheckKey{}, true)
}

func isRegistrationRecheck(ctx context.Context) bool {
	v, _ := ctx.Value(registrationRecheckKey{}).(bool)
	return v
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

// identityState is what the checker keeps of an identity between runs.
type identityState struct {
	// stamp orders the next run: 2*run for the run that last verified the
	// identity, 2*run-1 for the run that first saw it (between the identities
	// verified in the run before and those verified in that run).
	stamp uint64
	// unverified counts the revalidation runs in a row that did not verify it.
	unverified int
}

func verifiedStamp(run uint64) uint64  { return 2 * run }
func firstSeenStamp(run uint64) uint64 { return 2*run - 1 }

type revocationChecker struct {
	store   sessionstore.SessionStore
	users   models.UserRepository
	keys    models.APIKeyRepository
	errLog  *logLimiter
	slowLog *logLimiter
	cfg     checkerConfig

	mu sync.Mutex
	// run counts the revalidation runs; state holds the identities of the
	// last run (the closed sockets drop out), so it is bounded by the open
	// identities.
	run   uint64
	state map[checkKey]identityState
}

func (r *revocationChecker) check(ctx context.Context, identities []websocket.ClientIdentity) []bool {
	cr := newCheckRun(r, ctx, identities)
	if isRegistrationRecheck(ctx) {
		order := make([]int, len(identities))
		for i := range order {
			order[i] = i
		}
		cr.process(order)
		return cr.revoked
	}
	run, order := r.startRun(identities)
	cr.process(order)
	r.finishRun(run, cr)
	return cr.revoked
}

// startRun numbers a revalidation run and returns the order of its
// identities: oldest verification first, with an identity seen for the first
// time counted as verified then; ties keep the given order.
func (r *revocationChecker) startRun(identities []websocket.ClientIdentity) (uint64, []int) {
	stamps := make([]uint64, len(identities))
	r.mu.Lock()
	r.run++
	run := r.run
	for i, id := range identities {
		if st, ok := r.state[keyOfIdentity(id)]; ok {
			stamps[i] = st.stamp
		} else {
			stamps[i] = firstSeenStamp(run)
		}
	}
	r.mu.Unlock()
	order := make([]int, len(identities))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return stamps[order[a]] < stamps[order[b]] })
	return run, order
}

// finishRun records the outcome of a revalidation run and closes (fail
// closed) the identities that stayed unverified for maxUnverifiedRuns runs in
// a row. It keeps only the identities of this run.
func (r *revocationChecker) finishRun(run uint64, cr *checkRun) {
	cancelled := errors.Is(cr.ctx.Err(), context.Canceled)
	limit := r.cfg.maxUnverifiedRuns
	escalate := r.cfg.failClosed && limit > 0 && !cancelled
	closed := 0
	r.mu.Lock()
	next := make(map[checkKey]identityState, len(cr.identities))
	for i, id := range cr.identities {
		k := keyOfIdentity(id)
		st, ok := r.state[k]
		if !ok {
			st = identityState{stamp: firstSeenStamp(run)}
		}
		switch {
		case cr.verified[i]:
			st = identityState{stamp: verifiedStamp(run)}
		case cancelled:
			// A shutdown is no evidence about the identity.
		default:
			st.unverified++
			if escalate && st.unverified >= limit && !cr.revoked[i] {
				cr.revoked[i] = true
				closed++
			}
		}
		next[k] = st
	}
	r.state = next
	r.mu.Unlock()
	if closed == 0 {
		return
	}
	msg := "WebSocket revalidation: closing sockets that could not be verified in several runs in a row (fail closed)"
	if r.errLog.allow() {
		slog.Error(msg, "identities", closed, "runs", limit)
		return
	}
	slog.Debug(msg, "identities", closed, "runs", limit)
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
	// verified marks the identities whose every check this run completed
	// (revoked or not). The others were not reached or needed a lookup that
	// failed.
	verified []bool

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
		verified:       make([]bool, len(identities)),
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
// the store; an error while the run still has time is a failure, also the
// lookup timeout of lookupCtx.
func (cr *checkRun) ended() bool { return cr.ctx.Err() != nil }

// lookupCtx returns the context of one lookup: the run context bounded by
// the lookup timeout.
func (cr *checkRun) lookupCtx() (context.Context, context.CancelFunc) {
	if cr.r.cfg.lookupTimeout <= 0 {
		return cr.ctx, func() { /* the run context is not ours to cancel */ }
	}
	return context.WithTimeout(cr.ctx, cr.r.cfg.lookupTimeout)
}

// ranOut logs the end of a run that did not reach every identity.
func (cr *checkRun) ranOut(checked int) {
	left := len(cr.identities) - checked
	if errors.Is(cr.ctx.Err(), context.Canceled) {
		slog.Debug("WebSocket revalidation cancelled", "checked", checked, "identities", len(cr.identities))
		return
	}
	msg := "WebSocket revalidation ran out of time; the next run starts with the unchecked sockets, which close (fail closed) after several such runs in a row"
	if cr.r.slowLog.allow() {
		slog.Warn(msg, "checked", checked, "unchecked", left, "identities", len(cr.identities))
		return
	}
	slog.Debug(msg, "checked", checked, "unchecked", left, "identities", len(cr.identities))
}

// failure logs a lookup that failed while the run still had time.
func (cr *checkRun) failure(what string, err error, args ...any) {
	args = append(args, "identities", len(cr.identities), "fail_closed", cr.r.cfg.failClosed, "error", err)
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
	ctx, cancel := cr.lookupCtx()
	found, err := cr.r.keys.FindByIDs(ctx, ids)
	cancel()
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
	ctx, cancel := cr.lookupCtx()
	found, err := cr.r.users.FindByIDs(ctx, ids)
	cancel()
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

// verdict is the outcome of one check of an identity.
type verdict int

const (
	verdictPass    verdict = iota // not revoked by this check
	verdictRevoked                // revoked by this check
	verdictFailed                 // the lookup of this check failed
)

// checkOne checks identity i. It returns false when the run ended before
// the identity was checked (it stays open and unverified).
func (cr *checkRun) checkOne(i int) bool {
	id := cr.identities[i]
	failed := false
	for _, v := range []verdict{cr.keyVerdict(id), cr.userVerdict(id)} {
		switch {
		case v == verdictRevoked:
			cr.finish(i, true, true)
			return true
		case v == verdictFailed && cr.r.cfg.failClosed:
			cr.finish(i, true, false)
			return true
		case v == verdictFailed:
			failed = true // fail open: go on with the other checks
		}
	}
	v := verdictPass
	if cr.r.store != nil {
		var ok bool
		if v, ok = cr.storeVerdict(id); !ok {
			return false
		}
	}
	switch v {
	case verdictRevoked:
		cr.finish(i, true, true)
	case verdictFailed:
		cr.finish(i, cr.r.cfg.failClosed, false)
	case verdictPass:
		cr.finish(i, false, !failed)
	}
	return true
}

func (cr *checkRun) finish(i int, revoked, verified bool) {
	cr.revoked[i] = revoked
	cr.verified[i] = verified
}

// keyVerdict checks the API key of the identity: deleted, expired or of
// another user revokes it.
func (cr *checkRun) keyVerdict(id websocket.ClientIdentity) verdict {
	switch {
	case id.APIKeyID == "" || cr.r.keys == nil:
		return verdictPass
	case cr.keysFailed:
		return verdictFailed
	}
	key := cr.keys[id.APIKeyID]
	if key == nil || key.UserID != id.UserID || (key.ExpiresAt != nil && !time.Now().Before(*key.ExpiresAt)) {
		return verdictRevoked
	}
	return verdictPass
}

// userVerdict checks the user of the identity: deleted or disabled revokes
// it.
func (cr *checkRun) userVerdict(id websocket.ClientIdentity) verdict {
	switch {
	case id.UserID == "" || cr.r.users == nil:
		return verdictPass
	case cr.usersFailed:
		return verdictFailed
	}
	if u := cr.users[id.UserID]; u == nil || u.Disabled {
		return verdictRevoked
	}
	return verdictPass
}

// storeVerdict runs the blocklist checks of the identity. ok is false when
// the run ended during a lookup.
func (cr *checkRun) storeVerdict(id websocket.ClientIdentity) (v verdict, ok bool) {
	blocked, ok := cr.storeChecks(id)
	switch {
	case !ok:
		return verdictPass, false
	case cr.storeFailed:
		return verdictFailed, true
	case blocked:
		return verdictRevoked, true
	default:
		return verdictPass, true
	}
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
	ctx, cancel := cr.lookupCtx()
	v, err := l.fn(ctx, l.key)
	cancel()
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
		ctx, cancel := cr.lookupCtx()
		v, err := cr.r.store.IsUserBlocked(ctx, userID, issueTimeOf(at))
		cancel()
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
