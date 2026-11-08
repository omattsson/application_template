package websocket

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeRevocable struct {
	id    ClientIdentity
	topic string
	mu    sync.Mutex
	calls []revokeCall
}

type revokeCall struct {
	code   int
	reason string
}

func (f *fakeRevocable) Identity() ClientIdentity { return f.id }
func (f *fakeRevocable) Topic() string            { return f.topic }
func (f *fakeRevocable) Kind() string             { return "test" }
func (f *fakeRevocable) Revoke(code int, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, revokeCall{code, reason})
}
func (f *fakeRevocable) got() []revokeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]revokeCall(nil), f.calls...)
}

func TestHub_RevocableDisconnect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		disconnect func(h *Hub) int
		wantRevoke []bool // per revocable below
	}{
		{"user", func(h *Hub) int { return h.DisconnectUser("u1") }, []bool{true, true, false, true}},
		{"token", func(h *Hub) int { return h.DisconnectToken("jti-b") }, []bool{false, true, false, false}},
		{"api key", func(h *Hub) int { return h.DisconnectAPIKey("key-1") }, []bool{false, false, false, true}},
		{"unknown user", func(h *Hub) int { return h.DisconnectUser("nobody") }, []bool{false, false, false, false}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := NewHub()
			go hub.Run()
			defer hub.Shutdown()

			rs := []*fakeRevocable{
				{id: ClientIdentity{UserID: "u1", TokenID: "jti-a"}},
				{id: ClientIdentity{UserID: "u1", TokenID: "jti-b"}},
				{id: ClientIdentity{UserID: "u2", TokenID: "jti-c"}},
				{id: ClientIdentity{UserID: "u1", APIKeyID: "key-1"}},
			}
			for _, r := range rs {
				_, err := hub.RegisterRevocable(r)
				require.NoError(t, err)
			}
			assert.Equal(t, 0, hub.ClientCount(), "revocables are not clients")
			assert.Equal(t, 4, hub.RevocableCount())

			n := tt.disconnect(hub)
			want := 0
			for i, r := range rs {
				if tt.wantRevoke[i] {
					want++
					assert.Equal(t, []revokeCall{{CloseCodePolicyViolation, closeReasonRevoked}}, r.got(), "revocable %d", i)
				} else {
					assert.Empty(t, r.got(), "revocable %d", i)
				}
			}
			assert.Equal(t, want, n)
			assert.Equal(t, 4-want, hub.RevocableCount(), "a revoked revocable leaves the set")
		})
	}
}

func TestHub_DisconnectMixedClientsAndRevocables(t *testing.T) {
	t.Parallel()
	hub := NewHub()
	go hub.Run()
	defer hub.Shutdown()

	c := newRegistered(t, hub, ClientIdentity{UserID: "u1"}, 1)
	r := &fakeRevocable{id: ClientIdentity{UserID: "u1"}}
	_, err := hub.RegisterRevocable(r)
	require.NoError(t, err)

	assert.Equal(t, 2, hub.DisconnectUser("u1"))
	assert.True(t, isClosed(c.send))
	assert.Len(t, r.got(), 1)
}

func TestHub_RegisterRevocable(t *testing.T) {
	t.Parallel()

	t.Run("unregister removes and is idempotent", func(t *testing.T) {
		t.Parallel()
		hub := NewHub()
		go hub.Run()
		defer hub.Shutdown()
		r := &fakeRevocable{id: ClientIdentity{UserID: "u1"}}
		unregister, err := hub.RegisterRevocable(r)
		require.NoError(t, err)
		unregister()
		unregister()
		assert.Equal(t, 0, hub.RevocableCount())
		assert.Equal(t, 0, hub.DisconnectUser("u1"))
		assert.Empty(t, r.got())
	})

	t.Run("closed hub refuses", func(t *testing.T) {
		t.Parallel()
		hub := NewHub()
		hub.Shutdown()
		_, err := hub.RegisterRevocable(&fakeRevocable{})
		assert.ErrorIs(t, err, ErrHubClosed)
	})
}

func TestHub_ShutdownRevokesRevocables(t *testing.T) {
	t.Parallel()
	hub := NewHub()
	done := make(chan struct{})
	go func() { hub.Run(); close(done) }()
	r := &fakeRevocable{id: ClientIdentity{UserID: "u1"}}
	_, err := hub.RegisterRevocable(r)
	require.NoError(t, err)

	hub.Shutdown()
	<-done
	assert.Equal(t, []revokeCall{{CloseCodeGoingAway, CloseReasonShutdown}}, r.got())
	assert.Equal(t, 0, hub.RevocableCount())
}

func TestHub_DisconnectTopic(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		topic string
		want  []bool
	}{
		{"match", "t1", []bool{true, false, true}},
		{"no match", "nope", []bool{false, false, false}},
		{"empty", "", []bool{false, false, false}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := NewHub()
			go hub.Run()
			defer hub.Shutdown()
			rs := []*fakeRevocable{{topic: "t1"}, {topic: "t2"}, {topic: "t1"}}
			for _, r := range rs {
				_, err := hub.RegisterRevocable(r)
				require.NoError(t, err)
			}
			n := hub.DisconnectTopic(tt.topic)
			want := 0
			for i, r := range rs {
				if tt.want[i] {
					want++
					assert.Equal(t, []revokeCall{{CloseCodePolicyViolation, CloseReasonAccessLost}}, r.got())
				} else {
					assert.Empty(t, r.got())
				}
			}
			assert.Equal(t, want, n)
		})
	}
	var nilHub *Hub
	assert.Equal(t, 0, nilHub.DisconnectTopic("t1"))
}

func TestHub_RevalidateRevocables(t *testing.T) {
	t.Parallel()

	t.Run("identity check closes revoked revocables and clients", func(t *testing.T) {
		t.Parallel()
		hub := NewHub()
		go hub.Run()
		defer hub.Shutdown()
		good := &fakeRevocable{id: ClientIdentity{UserID: "u1", TokenID: "ok"}}
		bad := &fakeRevocable{id: ClientIdentity{UserID: "u2", TokenID: "bad"}}
		c := newRegistered(t, hub, ClientIdentity{UserID: "u2", TokenID: "bad"}, 1)
		for _, r := range []*fakeRevocable{good, bad} {
			_, err := hub.RegisterRevocable(r)
			require.NoError(t, err)
		}

		var seen []ClientIdentity
		closed := hub.revalidate(func(_ context.Context, ids []ClientIdentity) []bool {
			seen = ids
			out := make([]bool, len(ids))
			for i, id := range ids {
				out[i] = id.TokenID == "bad"
			}
			return out
		})
		assert.Equal(t, 2, closed)
		assert.Len(t, seen, 2, "the same identity of a client and a revocable is checked once")
		assert.Empty(t, good.got())
		assert.Equal(t, []revokeCall{{CloseCodePolicyViolation, closeReasonRevoked}}, bad.got())
		assert.True(t, isClosed(c.send))
	})

	t.Run("wrong identity result size is ignored", func(t *testing.T) {
		t.Parallel()
		hub := NewHub()
		go hub.Run()
		defer hub.Shutdown()
		r := &fakeRevocable{id: ClientIdentity{UserID: "u1", TokenID: "a"}}
		_, err := hub.RegisterRevocable(r)
		require.NoError(t, err)
		assert.Zero(t, hub.revalidate(func(context.Context, []ClientIdentity) []bool { return nil }))
		assert.Empty(t, r.got())
	})

	t.Run("access checker", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name     string
			checker  RevocableAccessChecker
			wantLost []bool
		}{
			{"lost access closes", func(_ context.Context, rs []Revocable) []bool {
				out := make([]bool, len(rs))
				for i, r := range rs {
					out[i] = r.Topic() == "gone"
				}
				return out
			}, []bool{false, true}},
			{"all fail open", func(_ context.Context, rs []Revocable) []bool { return make([]bool, len(rs)) }, []bool{false, false}},
			{"wrong result size is ignored", func(context.Context, []Revocable) []bool { return []bool{true} }, []bool{false, false}},
			{"no checker", nil, []bool{false, false}},
		}
		for _, tt := range tests {
			tt := tt
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				hub := NewHub()
				go hub.Run()
				defer hub.Shutdown()
				hub.SetRevocableAccessChecker(tt.checker)
				rs := []*fakeRevocable{
					{id: ClientIdentity{UserID: "u1", TokenID: "a"}, topic: "t1"},
					{id: ClientIdentity{UserID: "u1", TokenID: "b"}, topic: "gone"},
				}
				for _, r := range rs {
					_, err := hub.RegisterRevocable(r)
					require.NoError(t, err)
				}
				// The identity checker revokes nothing: only the access checker acts.
				hub.revalidate(func(_ context.Context, ids []ClientIdentity) []bool { return make([]bool, len(ids)) })
				for i, r := range rs {
					if tt.wantLost[i] {
						assert.Equal(t, []revokeCall{{CloseCodePolicyViolation, CloseReasonAccessLost}}, r.got())
					} else {
						assert.Empty(t, r.got())
					}
				}
			})
		}
	})

	t.Run("access checker runs without identities", func(t *testing.T) {
		t.Parallel()
		hub := NewHub()
		go hub.Run()
		defer hub.Shutdown()
		hub.SetRevocableAccessChecker(func(_ context.Context, rs []Revocable) []bool {
			out := make([]bool, len(rs))
			for i := range out {
				out[i] = true
			}
			return out
		})
		r := &fakeRevocable{} // no credential identity
		_, err := hub.RegisterRevocable(r)
		require.NoError(t, err)
		assert.Equal(t, 1, hub.revalidate(func(_ context.Context, ids []ClientIdentity) []bool { return make([]bool, len(ids)) }))
		assert.Len(t, r.got(), 1)
	})

	t.Run("panic in checker is contained", func(t *testing.T) {
		t.Parallel()
		hub := NewHub()
		go hub.Run()
		defer hub.Shutdown()
		_, err := hub.RegisterRevocable(&fakeRevocable{id: ClientIdentity{UserID: "u1"}})
		require.NoError(t, err)
		assert.NotPanics(t, func() {
			hub.revalidate(func(context.Context, []ClientIdentity) []bool { panic("boom") })
		})
	})
}

func TestHub_StartRevalidation(t *testing.T) {
	t.Parallel()

	t.Run("loop closes a revoked revocable and stops on shutdown", func(t *testing.T) {
		t.Parallel()
		hub := NewHub()
		go hub.Run()
		r := &fakeRevocable{id: ClientIdentity{UserID: "u1", TokenID: "bad"}}
		_, err := hub.RegisterRevocable(r)
		require.NoError(t, err)

		hub.StartRevalidation(5*time.Millisecond, func(_ context.Context, ids []ClientIdentity) []bool {
			out := make([]bool, len(ids))
			for i := range out {
				out[i] = true
			}
			return out
		})
		// A second call starts no second loop.
		hub.StartRevalidation(5*time.Millisecond, func(context.Context, []ClientIdentity) []bool { return nil })

		require.Eventually(t, func() bool { return len(r.got()) == 1 }, 2*time.Second, 5*time.Millisecond)
		hub.Shutdown()
		select {
		case <-hub.revalidateDone:
		case <-time.After(2 * time.Second):
			t.Fatal("revalidation loop did not stop on shutdown")
		}
	})

	t.Run("invalid arguments start nothing", func(t *testing.T) {
		t.Parallel()
		check := func(context.Context, []ClientIdentity) []bool { return nil }
		var nilHub *Hub
		assert.NotPanics(t, func() { nilHub.StartRevalidation(time.Second, check) })

		hub := NewHub()
		hub.StartRevalidation(time.Second, nil)
		hub.StartRevalidation(0, check)
		// The once is unused, so a valid call still starts the loop.
		hub.StartRevalidation(time.Hour, check)
		hub.Shutdown()
		select {
		case <-hub.revalidateDone:
		case <-time.After(2 * time.Second):
			t.Fatal("revalidation loop did not start or stop")
		}
	})
}

func TestHub_RevokeOutsideLock(t *testing.T) {
	t.Parallel()
	hub := NewHub()
	go hub.Run()
	defer hub.Shutdown()
	// A Revoke that calls back into the hub would deadlock under the lock.
	r := &reentrantRevocable{hub: hub}
	_, err := hub.RegisterRevocable(r)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() { hub.DisconnectTopic("t1"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: Revoke ran under the hub lock")
	}
	assert.Equal(t, 1, r.n)
}

type reentrantRevocable struct {
	hub *Hub
	n   int
}

func (r *reentrantRevocable) Identity() ClientIdentity { return ClientIdentity{} }
func (r *reentrantRevocable) Topic() string            { return "t1" }
func (r *reentrantRevocable) Kind() string             { return "test" }
func (r *reentrantRevocable) Revoke(int, string) {
	r.n++
	_ = r.hub.ClientCount()
	_ = r.hub.RevocableCount()
}

func TestHub_PanicInRevokeIsContained(t *testing.T) {
	t.Parallel()
	hub := NewHub()
	go hub.Run()
	defer hub.Shutdown()
	ok := &fakeRevocable{topic: "t1"}
	_, _ = hub.RegisterRevocable(&panicRevocable{})
	_, _ = hub.RegisterRevocable(ok)
	assert.Equal(t, 2, hub.DisconnectTopic("t1"))
	assert.Len(t, ok.got(), 1)
}

type panicRevocable struct{}

func (*panicRevocable) Identity() ClientIdentity { return ClientIdentity{} }
func (*panicRevocable) Topic() string            { return "t1" }
func (*panicRevocable) Kind() string             { return "test" }
func (*panicRevocable) Revoke(int, string)       { panic("boom") }
