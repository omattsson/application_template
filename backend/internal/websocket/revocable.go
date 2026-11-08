package websocket

import (
	"context"
	"log/slog"
)

// Close codes and reasons that the hub passes to Revocable.Revoke.
const (
	// CloseCodeGoingAway closes a revocable when the hub shuts down (1001).
	CloseCodeGoingAway = 1001
	// CloseCodePolicyViolation closes a revocable whose credential or access
	// was revoked (1008).
	CloseCodePolicyViolation = 1008
	// CloseReasonAccessLost is the reason for a revocable whose resource went
	// away or whose user lost access to it.
	CloseReasonAccessLost = "access revoked"
	// CloseReasonShutdown is the reason on hub shutdown.
	CloseReasonShutdown = "server shutdown"
)

// Revocable is a long-lived connection that is not a /ws client but must be
// closed by the same revocations (for example a streaming session served by
// another endpoint). The hub closes it on DisconnectUser, DisconnectToken,
// DisconnectAPIKey, on a failed revalidation, on DisconnectTopic and on
// shutdown. The implementation must be comparable (use a pointer type): the
// hub keeps revocables in a map. Identity and Topic are called under the hub
// lock: keep them cheap. A revocable does not count in ClientCount and does
// not receive broadcasts.
type Revocable interface {
	// Identity is the credential of the connection. The revalidation runs the
	// IdentityChecker on it like on the identity of a /ws client.
	Identity() ClientIdentity
	// Topic names the resource the connection serves (access re-check and
	// DisconnectTopic). Empty: not tied to a resource.
	Topic() string
	// Kind names the feature of the connection. The access re-check uses it
	// to apply the rules of that feature only.
	Kind() string
	// Revoke closes the connection with a WebSocket close code and reason. It
	// must not block (start the close in a goroutine or signal a channel) and
	// may be called from any goroutine. The hub calls it at most once, outside
	// its lock.
	Revoke(code int, reason string)
}

// RevocableAccessChecker reports which revocables lost their access (true:
// close with 1008). It has one result per revocable. On a lookup error it
// returns false for the affected ones (fail open) and logs the error.
type RevocableAccessChecker func(ctx context.Context, revocables []Revocable) []bool

// RegisterRevocable adds r to the hub. The returned function removes it
// again; call it when the connection ends. It is safe to call more than
// once. It returns ErrHubClosed when the hub has shut down.
func (h *Hub) RegisterRevocable(r Revocable) (unregister func(), err error) {
	h.mu.Lock()
	select {
	case <-h.done:
		h.mu.Unlock()
		return nil, ErrHubClosed
	default:
	}
	h.revocables[r] = struct{}{}
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		delete(h.revocables, r)
		h.mu.Unlock()
	}, nil
}

// RevocableCount returns the number of registered revocables.
func (h *Hub) RevocableCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.revocables)
}

// SetRevocableAccessChecker sets the access re-check that every revalidation
// run applies to the registered revocables. Call it before the traffic
// starts. A nil checker removes the re-check.
func (h *Hub) SetRevocableAccessChecker(check RevocableAccessChecker) {
	if check == nil {
		h.accessChecker.Store(nil)
		return
	}
	h.accessChecker.Store(&check)
}

// DisconnectTopic closes the local revocables of the topic with 1008 (the
// resource was deleted or the access to it ended). It returns the number of
// closed revocables. An empty topic or a nil hub closes nothing.
func (h *Hub) DisconnectTopic(topic string) int {
	if h == nil || topic == "" {
		return 0
	}
	return h.revokeRevocables(func(r Revocable) bool { return r.Topic() == topic },
		CloseCodePolicyViolation, CloseReasonAccessLost)
}

// takeRevocablesLocked removes the matching revocables from the set and
// returns them. Caller holds h.mu (write lock).
func (h *Hub) takeRevocablesLocked(match func(Revocable) bool) []Revocable {
	var taken []Revocable
	for r := range h.revocables {
		if match(r) {
			taken = append(taken, r)
			delete(h.revocables, r)
		}
	}
	return taken
}

// revokeRevocables removes the matching revocables and revokes them outside
// the lock. It returns their number.
func (h *Hub) revokeRevocables(match func(Revocable) bool, code int, reason string) int {
	h.mu.Lock()
	taken := h.takeRevocablesLocked(match)
	h.mu.Unlock()
	revokeAll(taken, code, reason)
	return len(taken)
}

// revokeAll calls Revoke on each revocable. A panic in Revoke is logged and
// does not stop the others.
func revokeAll(rs []Revocable, code int, reason string) {
	for _, r := range rs {
		func() {
			defer func() {
				if p := recover(); p != nil {
					slog.Error("Panic in Revocable.Revoke", "recover", p)
				}
			}()
			r.Revoke(code, reason)
		}()
	}
}
