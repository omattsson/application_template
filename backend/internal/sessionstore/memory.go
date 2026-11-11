package sessionstore

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// memEntry is a blocked token, user or session, or a pending OIDC state.
type memEntry struct {
	// data is the JSON of an OIDC state.
	data string
	// expiresMs is the end of the entry in Unix milliseconds.
	expiresMs int64
	// blockedAtMs is the block time of a user block in Unix milliseconds.
	blockedAtMs int64
}

// memoryStore is the in-process SessionStore, for tests and a single
// process. The state is lost on restart and not shared with other replicas.
type memoryStore struct {
	now      func() time.Time
	tokens   map[string]memEntry
	users    map[string]memEntry
	sessions map[string]memEntry
	states   map[string]memEntry
	janitor  *janitor
	mu       sync.Mutex
}

// NewMemory returns the in-memory store and starts its cleanup goroutine,
// which Stop ends. Use it in tests and for a single process; replicas need
// NewMySQL.
func NewMemory() SessionStore {
	return newMemory(time.Now, defaultCleanupEvery)
}

func newMemory(now func() time.Time, cleanupEvery time.Duration) *memoryStore {
	s := &memoryStore{
		now:      now,
		tokens:   make(map[string]memEntry),
		users:    make(map[string]memEntry),
		sessions: make(map[string]memEntry),
		states:   make(map[string]memEntry),
	}
	s.janitor = startJanitor(cleanupEvery, s.Cleanup)
	return s
}

func (s *memoryStore) nowMs() int64 { return s.now().UnixMilli() }

// put stores an entry. The expiry of an existing entry stays when it is
// later, so a shorter block never shortens a longer one.
func put(m map[string]memEntry, key string, e memEntry) {
	if cur, ok := m[key]; ok && cur.expiresMs > e.expiresMs {
		e.expiresMs = cur.expiresMs
	}
	m[key] = e
}

func (s *memoryStore) BlockToken(ctx context.Context, jti string, expiresAt time.Time) error {
	if err := preflight(ctx, jti); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	put(s.tokens, jti, memEntry{expiresMs: expiryMs(expiresAt)})
	return nil
}

func (s *memoryStore) IsTokenBlocked(ctx context.Context, jti string) (bool, error) {
	if err := preflight(ctx, jti); err != nil {
		return false, err
	}
	return s.active(s.tokens, jti), nil
}

// active reports whether the map has an unexpired entry for key.
func (s *memoryStore) active(m map[string]memEntry, key string) bool {
	now := s.nowMs()
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := m[key]
	return ok && e.expiresMs > now
}

func (s *memoryStore) BlockUser(ctx context.Context, userID string, until time.Time) error {
	if err := preflight(ctx, userID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Read the time under the lock and keep the later cut, so the cut never
	// moves back (a block that read its time earlier but writes later).
	blockedAt := s.nowMs()
	if cur, ok := s.users[userID]; ok && cur.blockedAtMs > blockedAt {
		blockedAt = cur.blockedAtMs
	}
	put(s.users, userID, memEntry{expiresMs: expiryMs(until), blockedAtMs: blockedAt})
	return nil
}

func (s *memoryStore) IsUserBlocked(ctx context.Context, userID string, issuedAt time.Time) (bool, error) {
	if err := preflight(ctx, userID); err != nil {
		return false, err
	}
	now := s.nowMs()
	s.mu.Lock()
	e, ok := s.users[userID]
	s.mu.Unlock()
	if !ok || e.expiresMs <= now {
		return false, nil
	}
	return userBlockApplies(e.blockedAtMs, issuedAt), nil
}

func (s *memoryStore) UnblockUser(ctx context.Context, userID string) error {
	if err := preflight(ctx, userID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.users, userID)
	return nil
}

func (s *memoryStore) BlockSession(ctx context.Context, sessionID string, until time.Time) error {
	return s.BlockSessions(ctx, []string{sessionID}, until)
}

// BlockSessions blocks every session under one lock.
func (s *memoryStore) BlockSessions(ctx context.Context, sessionIDs []string, until time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkKeys(sessionIDs); err != nil {
		return err
	}
	expires := expiryMs(until)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range sessionIDs {
		put(s.sessions, id, memEntry{expiresMs: expires})
	}
	return nil
}

func (s *memoryStore) IsSessionBlocked(ctx context.Context, sessionID string) (bool, error) {
	if err := preflight(ctx, sessionID); err != nil {
		return false, err
	}
	return s.active(s.sessions, sessionID), nil
}

func (s *memoryStore) SaveOIDCState(ctx context.Context, state string, data OIDCStateData, ttl time.Duration) error {
	if err := preflight(ctx, state); err != nil {
		return err
	}
	raw, err := marshalState(data, ttl)
	if err != nil {
		return err
	}
	expires := expiryMs(s.now().Add(ttl))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[stateKey(state)] = memEntry{data: string(raw), expiresMs: expires}
	return nil
}

// ConsumeOIDCState removes the state under the lock, so one consumer gets it.
func (s *memoryStore) ConsumeOIDCState(ctx context.Context, state string) (*OIDCStateData, error) {
	if err := preflight(ctx, state); err != nil {
		return nil, err
	}
	now := s.nowMs()
	s.mu.Lock()
	key := stateKey(state)
	e, ok := s.states[key]
	delete(s.states, key)
	s.mu.Unlock()
	if !ok || e.expiresMs <= now {
		return nil, nil
	}
	var data OIDCStateData
	if err := json.Unmarshal([]byte(e.data), &data); err != nil {
		return nil, fmt.Errorf("decode oidc state: %w", err)
	}
	return &data, nil
}

func (s *memoryStore) Cleanup(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := s.nowMs()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range []map[string]memEntry{s.tokens, s.users, s.sessions, s.states} {
		for k, e := range m {
			if e.expiresMs <= now {
				delete(m, k)
			}
		}
	}
	return nil
}

func (s *memoryStore) Stop() { s.janitor.stop() }

// count returns the number of stored entries, expired or not (for tests).
func (s *memoryStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tokens) + len(s.users) + len(s.sessions) + len(s.states)
}

// preflight returns the context error, or ErrInvalidKey for a bad key.
func preflight(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return checkKey(key)
}
