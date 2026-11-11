// Package sessionstore keeps the short-lived auth state that must be shared
// by all replicas: the blocklist of revoked access tokens, users and
// sessions, and the pending OpenID Connect (OIDC) login states.
//
// There are two implementations of SessionStore. NewMySQL keeps the state in
// the session_entries table (migration 20260101000002), so every replica
// sees a revocation at once. NewMemory keeps it in the process; it is for
// tests and a single process only.
package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

// ErrInvalidKey is returned for a token ID, user ID, session ID or OIDC
// state that is empty, longer than MaxKeyLen characters, or not valid UTF-8.
// The text never contains the key.
var ErrInvalidKey = errors.New("invalid session store key")

// ErrInvalidTTL is returned by SaveOIDCState for a ttl that is not positive.
var ErrInvalidTTL = errors.New("session store ttl must be positive")

// ErrStateTooLarge is returned by SaveOIDCState for a state whose JSON is
// longer than MaxStateDataLen bytes.
var ErrStateTooLarge = errors.New("oidc state data too large")

const (
	// MaxKeyLen is the length in characters of the key column
	// (session_entries.entry_key VARCHAR(255)).
	MaxKeyLen = 255
	// MaxStateDataLen is the size in bytes of the data column
	// (session_entries.data TEXT).
	MaxStateDataLen = 65535
)

// The kinds of row in session_entries (column kind, VARCHAR(20)). A key is
// unique per kind, so a token and a session with the same ID do not clash.
const (
	kindTokenBlock   = "token_block"
	kindUserBlock    = "user_block"
	kindSessionBlock = "session_block"
	kindOIDCState    = "oidc_state"
)

// OIDCStateData is what a login keeps between the redirect to the identity
// provider and the callback. It is stored as given (as JSON in the database).
type OIDCStateData struct {
	CodeVerifier string `json:"code_verifier"`
	RedirectURL  string `json:"redirect_url"`
}

// SessionStore is the shared auth state. Every method takes the request
// context first; a cancelled context ends the call with an error that
// matches context.Canceled (errors.Is).
//
// Keys (token IDs, user IDs, session IDs, OIDC states) are stored as given
// and compared case-sensitively. A key must be valid (see ErrInvalidKey). All
// times are kept to the millisecond. An entry is active while now is before
// its expiry; an expired entry is never reported, whether or not Cleanup has
// removed it yet.
type SessionStore interface {
	// BlockToken revokes the access token with the ID jti until expiresAt
	// (its own expiry). A second block keeps the later expiry.
	BlockToken(ctx context.Context, jti string, expiresAt time.Time) error
	IsTokenBlocked(ctx context.Context, jti string) (bool, error)
	// BlockUser revokes every access token of the user that was issued at or
	// before now, to the millisecond. The block lasts until `until` (the
	// longest token lifetime). A token issued after the block (for example
	// after a password reset) stays valid. A second block keeps the later
	// cut (a block time read earlier never lowers it) and the later expiry.
	BlockUser(ctx context.Context, userID string, until time.Time) error
	// IsUserBlocked reports whether a token of the user issued at issuedAt is
	// revoked. See userBlockApplies for the rule.
	IsUserBlocked(ctx context.Context, userID string, issuedAt time.Time) (bool, error)
	UnblockUser(ctx context.Context, userID string) error
	// BlockSession revokes every access token of the session (the sid claim,
	// the refresh token family) until `until`. It is a plain block, not based
	// on the issue time. A second block keeps the later expiry.
	BlockSession(ctx context.Context, sessionID string, until time.Time) error
	// BlockSessions blocks several sessions until the same time, with the
	// rules of BlockSession, in as few writes as the store allows. An empty
	// slice does nothing. Every ID is checked before the first write.
	BlockSessions(ctx context.Context, sessionIDs []string, until time.Time) error
	IsSessionBlocked(ctx context.Context, sessionID string) (bool, error)
	// SaveOIDCState keeps data for ttl under state. Saving the same state
	// again replaces it.
	SaveOIDCState(ctx context.Context, state string, data OIDCStateData, ttl time.Duration) error
	// ConsumeOIDCState returns the data of an unexpired state and removes it,
	// so a state works once, also when requests consume it at the same time.
	// An unknown, used or expired state gives (nil, nil).
	ConsumeOIDCState(ctx context.Context, state string) (*OIDCStateData, error)
	// Cleanup removes the expired entries. It is not needed for correctness;
	// both stores also run it every minute until Stop.
	Cleanup(ctx context.Context) error
	// Stop ends the cleanup goroutine and waits for it. It is safe to call
	// more than once. The store stays usable.
	Stop()
}

// userBlockApplies is the rule of IsUserBlocked. blockedAtMs is the block
// time in Unix milliseconds; 0 (a row without a readable block time) blocks
// every token. A token is blocked when it was issued at or before the block,
// to the millisecond: a token issued in the same millisecond may be older than
// the block, so it is blocked (fail closed). A zero issuedAt (a token without
// iat) is always blocked.
//
// A token without a millisecond claim has a whole-second issuedAt. For it
// the rule blocks every token issued in the block second or earlier, because
// such a token may be older than the block.
//
// The block time and iat come from the clocks of different replicas. The
// rule relies on synchronised clocks: a skew moves the cut by the skew.
func userBlockApplies(blockedAtMs int64, issuedAt time.Time) bool {
	if blockedAtMs <= 0 || issuedAt.IsZero() {
		return true
	}
	return issuedAt.UnixMilli() <= blockedAtMs
}

// checkKey returns ErrInvalidKey for a key that the key column cannot hold.
func checkKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: empty", ErrInvalidKey)
	}
	if !utf8.ValidString(key) {
		return fmt.Errorf("%w: not valid UTF-8", ErrInvalidKey)
	}
	if utf8.RuneCountInString(key) > MaxKeyLen {
		return fmt.Errorf("%w: longer than %d characters", ErrInvalidKey, MaxKeyLen)
	}
	return nil
}

// checkKeys checks every key of a batch, so a batch writes all or nothing of
// its validation.
func checkKeys(keys []string) error {
	for _, k := range keys {
		if err := checkKey(k); err != nil {
			return err
		}
	}
	return nil
}

// expiryMs returns t as Unix milliseconds, rounded up: the entry then lasts
// at least until t, never up to a millisecond less.
func expiryMs(t time.Time) int64 {
	ms := t.UnixMilli()
	if t.Nanosecond()%int(time.Millisecond) != 0 {
		ms++
	}
	return ms
}

// marshalState checks ttl and returns the JSON of data.
func marshalState(data OIDCStateData, ttl time.Duration) ([]byte, error) {
	if ttl <= 0 {
		return nil, ErrInvalidTTL
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxStateDataLen {
		return nil, ErrStateTooLarge
	}
	return raw, nil
}

// dedupe returns ids without repeated values, in the order of first
// appearance.
func dedupe(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
