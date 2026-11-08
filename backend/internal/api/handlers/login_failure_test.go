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

// attemptOnce runs one complete attempt for key with outcome and reports
// whether the throttle granted it.
func attemptOnce(th *loginThrottle, key string, outcome attemptOutcome) bool {
	a, _ := th.begin(key)
	if a == nil {
		return false
	}
	a.outcome = outcome
	a.settle()
	return true
}

// entryState returns the failures and attempts in flight of key.
func entryState(th *loginThrottle, key string) (failures, inflight int) {
	th.mu.Lock()
	defer th.mu.Unlock()
	if e, ok := th.entries[key]; ok {
		return e.failures, e.inflight
	}
	return 0, 0
}

// totalInflight returns the attempts in flight over all keys.
func totalInflight(th *loginThrottle) int {
	th.mu.Lock()
	defer th.mu.Unlock()
	n := 0
	for _, e := range th.entries {
		n += e.inflight
	}
	return n
}

func TestLoginThrottleDefaults(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 5, loginFailureThreshold)
	assert.Equal(t, 15*time.Minute, loginFailureWindow)
	assert.Equal(t, time.Second, loginBusyRetryAfter)
	assert.Equal(t, 10000, loginThrottleMaxEntries)
}

func TestLoginThrottleLocksAfterThreshold(t *testing.T) {
	t.Parallel()
	th, clock := newTestThrottle(3, time.Minute, 100)

	for i := 0; i < 2; i++ {
		require.True(t, attemptOnce(th, "alice", outcomeFailure))
		_, blocked := th.check("alice")
		require.False(t, blocked, "failure %d", i+1)
	}
	require.True(t, attemptOnce(th, "alice", outcomeFailure))

	retry, blocked := th.check("alice")
	require.True(t, blocked)
	assert.Equal(t, time.Minute, retry)
	a, retry := th.begin("alice")
	assert.Nil(t, a, "a locked key grants no attempt")
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
	assert.True(t, attemptOnce(th, "alice", outcomeSuccess))
}

// Failures further apart than the window never add up to a lock.
func TestLoginThrottleWindowResets(t *testing.T) {
	t.Parallel()
	th, clock := newTestThrottle(3, time.Minute, 100)

	attemptOnce(th, "alice", outcomeFailure)
	attemptOnce(th, "alice", outcomeFailure)
	clock.Advance(time.Minute)
	attemptOnce(th, "alice", outcomeFailure)
	attemptOnce(th, "alice", outcomeFailure)

	_, blocked := th.check("alice")
	assert.False(t, blocked)
	attemptOnce(th, "alice", outcomeFailure)
	_, blocked = th.check("alice")
	assert.True(t, blocked)
}

// A success clears the failures; a neutral outcome (server error) changes
// nothing but the slot.
func TestLoginThrottleOutcomes(t *testing.T) {
	t.Parallel()
	th, _ := newTestThrottle(3, time.Minute, 100)
	attemptOnce(th, "alice", outcomeFailure)
	attemptOnce(th, "alice", outcomeFailure)

	require.True(t, attemptOnce(th, "alice", outcomeNeutral))
	failures, inflight := entryState(th, "alice")
	assert.Equal(t, 2, failures)
	assert.Equal(t, 0, inflight)

	require.True(t, attemptOnce(th, "alice", outcomeSuccess))
	assert.Equal(t, 0, th.size(), "a success leaves no entry")

	require.True(t, attemptOnce(th, "bob", outcomeNeutral))
	assert.Equal(t, 0, th.size(), "a neutral attempt on a new key leaves no entry")
}

// Attempts in flight take the slots: with failures + in flight at the
// threshold, a further attempt is refused with loginBusyRetryAfter until one
// settles.
func TestLoginThrottleReservesSlots(t *testing.T) {
	t.Parallel()
	th, _ := newTestThrottle(3, time.Minute, 100)
	attemptOnce(th, "alice", outcomeFailure)

	first, _ := th.begin("alice")
	second, _ := th.begin("alice")
	require.NotNil(t, first)
	require.NotNil(t, second)
	refused, retry := th.begin("alice")
	assert.Nil(t, refused)
	assert.Equal(t, loginBusyRetryAfter, retry)

	first.settle() // neutral
	again, _ := th.begin("alice")
	require.NotNil(t, again, "a released slot is free again")

	second.outcome = outcomeFailure
	second.settle()
	again.outcome = outcomeFailure
	again.settle()
	_, blocked := th.check("alice")
	assert.True(t, blocked, "three failures reached the threshold")
	_, inflight := entryState(th, "alice")
	assert.Equal(t, 0, inflight)
}

// settle releases every reserved key exactly once; add never reserves a key
// twice, and a refused add keeps the other keys.
func TestLoginAttemptKeys(t *testing.T) {
	t.Parallel()
	th, _ := newTestThrottle(2, time.Minute, 100)

	a, _ := th.begin("ádmin")
	require.NotNil(t, a)
	_, ok := a.add("ádmin")
	require.True(t, ok)
	_, ok = a.add("admin")
	require.True(t, ok)
	assert.Equal(t, []string{"ádmin", "admin"}, a.keys)
	_, inflight := entryState(th, "admin")
	assert.Equal(t, 1, inflight)

	a.outcome = outcomeFailure
	a.settle()
	a.settle() // no second release
	for _, k := range []string{"ádmin", "admin"} {
		failures, inflight := entryState(th, k)
		assert.Equal(t, 1, failures, k)
		assert.Equal(t, 0, inflight, k)
	}

	// Lock the account key; a further variant cannot add it.
	attemptOnce(th, "admin", outcomeFailure)
	b, _ := th.begin("àdmin")
	require.NotNil(t, b)
	retry, ok := b.add("admin")
	assert.False(t, ok)
	assert.Equal(t, time.Minute, retry)
	assert.Equal(t, []string{"àdmin"}, b.keys)
	b.settle()
	assert.Equal(t, 0, totalInflight(th))

	var nilAttempt *loginAttempt
	nilAttempt.settle() // nil-safe
}

// The map never grows past maxEntries: expired entries go first, then the
// oldest counters that are not locked, and a lock is evicted only when every
// entry is locked. An entry with an attempt in flight is never evicted; a map
// full of them refuses a new key.
func TestLoginThrottleIsBounded(t *testing.T) {
	t.Parallel()

	t.Run("flood of usernames", func(t *testing.T) {
		t.Parallel()
		th, _ := newTestThrottle(3, time.Minute, 50)
		for i := 0; i < 1000; i++ {
			require.True(t, attemptOnce(th, fmt.Sprintf("user-%d", i), outcomeFailure))
			require.LessOrEqual(t, th.size(), 50)
		}
		assert.Equal(t, 50, th.size())
	})

	t.Run("expired entries are evicted first", func(t *testing.T) {
		t.Parallel()
		th, clock := newTestThrottle(3, time.Minute, 3)
		attemptOnce(th, "old-1", outcomeFailure)
		attemptOnce(th, "old-2", outcomeFailure)
		clock.Advance(30 * time.Second)
		attemptOnce(th, "fresh", outcomeFailure)
		clock.Advance(31 * time.Second) // old-1 and old-2 expired

		attemptOnce(th, "new", outcomeFailure)

		assert.Equal(t, 2, th.size())
		failures, _ := entryState(th, "fresh")
		assert.Equal(t, 1, failures)
		failures, _ = entryState(th, "new")
		assert.Equal(t, 1, failures)
	})

	t.Run("a lock survives a flood of unlocked counters", func(t *testing.T) {
		t.Parallel()
		th, clock := newTestThrottle(2, time.Minute, 10)
		attemptOnce(th, "victim", outcomeFailure)
		attemptOnce(th, "victim", outcomeFailure)
		for i := 0; i < 500; i++ {
			clock.Advance(time.Millisecond)
			attemptOnce(th, fmt.Sprintf("flood-%d", i), outcomeFailure)
		}
		_, blocked := th.check("victim")
		assert.True(t, blocked)
		assert.LessOrEqual(t, th.size(), 10)
	})

	t.Run("only locks: the oldest lock goes", func(t *testing.T) {
		t.Parallel()
		th, clock := newTestThrottle(1, time.Minute, 2)
		attemptOnce(th, "first", outcomeFailure)
		clock.Advance(time.Second)
		attemptOnce(th, "second", outcomeFailure)
		clock.Advance(time.Second)
		attemptOnce(th, "third", outcomeFailure)

		assert.Equal(t, 2, th.size())
		_, first := th.check("first")
		_, second := th.check("second")
		_, third := th.check("third")
		assert.False(t, first)
		assert.True(t, second)
		assert.True(t, third)
	})

	t.Run("entries in flight are kept; a full map fails closed", func(t *testing.T) {
		t.Parallel()
		th, _ := newTestThrottle(3, time.Minute, 2)
		a, _ := th.begin("one")
		b, _ := th.begin("two")
		require.NotNil(t, a)
		require.NotNil(t, b)

		refused, retry := th.begin("three")
		assert.Nil(t, refused)
		assert.Equal(t, loginBusyRetryAfter, retry)
		_, ok := a.add("three")
		assert.False(t, ok)
		assert.Equal(t, 2, th.size())

		a.settle()
		require.True(t, attemptOnce(th, "three", outcomeFailure), "a settled entry frees its slot")
		b.settle()
		assert.Equal(t, 0, totalInflight(th))
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
				a, _ := th.begin(key)
				th.check(key)
				if a == nil {
					continue
				}
				if i%3 == 0 {
					_, _ = a.add(fmt.Sprintf("account-%d", i%10))
				}
				a.outcome = attemptOutcome(i % 3)
				a.settle()
			}
		}(g)
	}
	wg.Wait()
	assert.LessOrEqual(t, th.size(), 64)
	assert.Equal(t, 0, totalInflight(th), "no reservation leaks")
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
		{name: "unicode lower-cased", in: "ÅSA", want: "åsa"},
		// Accents and width are not folded: the collation of the users
		// table folds them, so Login also reserves the key of the stored
		// username (see TestLoginAccentVariantsShareTheAccountThrottle).
		{name: "accents kept", in: "ádmin", want: "ádmin"},
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
