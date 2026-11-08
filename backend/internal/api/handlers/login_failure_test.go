package handlers

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock is a settable time source for the throttle.
type fakeClock struct {
	now time.Time
	mu  sync.Mutex
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestThrottle(threshold int, window time.Duration, maxEntries int) (*loginThrottle, *fakeClock) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	th := newLoginThrottle(threshold, window, maxEntries)
	th.now = clock.Now
	return th, clock
}

func TestLoginThrottleDefaults(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 5, loginFailureThreshold)
	assert.Equal(t, 15*time.Minute, loginFailureWindow)
	assert.Equal(t, 10000, loginThrottleMaxEntries)
}

func TestLoginThrottleLocksAfterThreshold(t *testing.T) {
	t.Parallel()
	th, clock := newTestThrottle(3, time.Minute, 100)

	for i := 0; i < 2; i++ {
		th.recordFailure("alice")
		_, blocked := th.check("alice")
		require.False(t, blocked, "failure %d", i+1)
	}
	th.recordFailure("alice")

	retry, blocked := th.check("alice")
	require.True(t, blocked)
	assert.Equal(t, time.Minute, retry)
	_, other := th.check("bob")
	assert.False(t, other, "other usernames are not affected")

	clock.Advance(40 * time.Second)
	retry, blocked = th.check("alice")
	require.True(t, blocked)
	assert.Equal(t, 20*time.Second, retry)

	clock.Advance(20 * time.Second)
	_, blocked = th.check("alice")
	assert.False(t, blocked, "the lock ends after the window")
	assert.Equal(t, 0, th.size(), "an expired entry is dropped on check")
}

// Failures further apart than the window never add up to a lock.
func TestLoginThrottleWindowResets(t *testing.T) {
	t.Parallel()
	th, clock := newTestThrottle(3, time.Minute, 100)

	th.recordFailure("alice")
	th.recordFailure("alice")
	clock.Advance(time.Minute)
	th.recordFailure("alice")
	th.recordFailure("alice")

	_, blocked := th.check("alice")
	assert.False(t, blocked)
	th.recordFailure("alice")
	_, blocked = th.check("alice")
	assert.True(t, blocked)
}

func TestLoginThrottleReset(t *testing.T) {
	t.Parallel()
	th, _ := newTestThrottle(2, time.Minute, 100)
	th.recordFailure("alice")
	th.recordFailure("alice")
	_, blocked := th.check("alice")
	require.True(t, blocked)

	th.reset("alice")

	_, blocked = th.check("alice")
	assert.False(t, blocked)
	assert.Equal(t, 0, th.size())
	th.reset("unknown") // no-op
}

// The map never grows past maxEntries: expired entries go first, then the
// oldest counters that are not locked, and a lock is evicted only when every
// entry is locked.
func TestLoginThrottleIsBounded(t *testing.T) {
	t.Parallel()

	t.Run("flood of usernames", func(t *testing.T) {
		t.Parallel()
		th, _ := newTestThrottle(3, time.Minute, 50)
		for i := 0; i < 1000; i++ {
			th.recordFailure(fmt.Sprintf("user-%d", i))
			require.LessOrEqual(t, th.size(), 50)
		}
		assert.Equal(t, 50, th.size())
	})

	t.Run("expired entries are evicted first", func(t *testing.T) {
		t.Parallel()
		th, clock := newTestThrottle(3, time.Minute, 3)
		th.recordFailure("old-1")
		th.recordFailure("old-2")
		clock.Advance(30 * time.Second)
		th.recordFailure("fresh")
		clock.Advance(31 * time.Second) // old-1 and old-2 expired

		th.recordFailure("new")

		assert.Equal(t, 2, th.size())
		assert.Equal(t, 1, th.failures("fresh"))
		assert.Equal(t, 1, th.failures("new"))
	})

	t.Run("a lock survives a flood of unlocked counters", func(t *testing.T) {
		t.Parallel()
		th, clock := newTestThrottle(2, time.Minute, 10)
		th.recordFailure("victim")
		th.recordFailure("victim")
		for i := 0; i < 500; i++ {
			clock.Advance(time.Millisecond)
			th.recordFailure(fmt.Sprintf("flood-%d", i))
		}
		_, blocked := th.check("victim")
		assert.True(t, blocked)
		assert.LessOrEqual(t, th.size(), 10)
	})

	t.Run("only locks: the oldest lock goes", func(t *testing.T) {
		t.Parallel()
		th, clock := newTestThrottle(1, time.Minute, 2)
		th.recordFailure("first")
		clock.Advance(time.Second)
		th.recordFailure("second")
		clock.Advance(time.Second)
		th.recordFailure("third")

		assert.Equal(t, 2, th.size())
		_, first := th.check("first")
		_, second := th.check("second")
		_, third := th.check("third")
		assert.False(t, first)
		assert.True(t, second)
		assert.True(t, third)
	})
}

func TestLoginThrottleConcurrentAccess(t *testing.T) {
	t.Parallel()
	th := newLoginThrottle(5, time.Minute, 64)
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				key := fmt.Sprintf("user-%d", (g*7+i)%100)
				th.recordFailure(key)
				th.check(key)
				if i%10 == 0 {
					th.reset(key)
				}
			}
		}(g)
	}
	wg.Wait()
	assert.LessOrEqual(t, th.size(), 64)
}

func TestLoginThrottleKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "lower case", in: "alice", want: "alice"},
		{name: "case folded", in: "AlIcE", want: "alice"},
		{name: "unicode case folded", in: "ÅSA", want: "åsa"},
		{name: "surrounding space kept", in: " alice ", want: " alice "},
		{name: "empty", in: "", want: ""},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, loginThrottleKey(tt.in))
		})
	}

	t.Run("length bounded", func(t *testing.T) {
		t.Parallel()
		key := loginThrottleKey(strings.Repeat("A", 10*models.MaxUsernameLen))
		assert.Len(t, key, models.MaxUsernameLen)
		assert.Equal(t, strings.Repeat("a", models.MaxUsernameLen), key)
	})

	t.Run("cut at a rune boundary", func(t *testing.T) {
		t.Parallel()
		key := loginThrottleKey("a" + strings.Repeat("é", models.MaxUsernameLen))
		assert.True(t, utf8.ValidString(key))
		assert.LessOrEqual(t, len(key), models.MaxUsernameLen)
	})
}

func TestRetryAfterSeconds(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "1", retryAfterSeconds(0))
	assert.Equal(t, "1", retryAfterSeconds(10*time.Millisecond))
	assert.Equal(t, "1", retryAfterSeconds(time.Second))
	assert.Equal(t, "2", retryAfterSeconds(1001*time.Millisecond))
	assert.Equal(t, "900", retryAfterSeconds(15*time.Minute))
}

func TestTruncateForLog(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
		n    int
	}{
		{name: "short", in: "alice", n: 8, want: "alice"},
		{name: "exact", in: "abcdefgh", n: 8, want: "abcdefgh"},
		{name: "cut", in: "abcdefghij", n: 8, want: "abcdefgh..."},
		{name: "rune boundary", in: "aé", n: 2, want: "a..."},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, truncateForLog(tt.in, tt.n))
		})
	}
}

// A log line never holds more than maxLoggedUsernameLen bytes of the
// submitted username.
func TestLoginFailureLogBoundsTheUsername(t *testing.T) {
	t.Parallel()
	h, logs, _ := newTestAuthHandler(newMockUserRepo(), testAuthConfig(), nil)
	long := strings.Repeat("u", 500)
	w := postJSON(t, newAuthRouter(h), "/api/v1/auth/login", loginBody(t, long, "pw-in-the-log?"))

	assert.Equal(t, 401, w.Code)
	out := logs.String()
	assert.NotContains(t, out, long)
	assert.Contains(t, out, strings.Repeat("u", maxLoggedUsernameLen)+"...")
	assert.NotContains(t, out, "pw-in-the-log?")
}
