package jobs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestBackoff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		attempt int
		base    time.Duration
		max     time.Duration
		jitter  float64
		want    time.Duration
	}{
		{"first failure waits the base", 1, 10 * time.Second, time.Hour, 0, 10 * time.Second},
		{"doubles", 2, 10 * time.Second, time.Hour, 0, 20 * time.Second},
		{"doubles again", 4, 10 * time.Second, time.Hour, 0, 80 * time.Second},
		{"capped", 10, 10 * time.Second, time.Hour, 0, time.Hour},
		{"huge attempt does not overflow", 5000, 10 * time.Second, time.Hour, 0, time.Hour},
		{"huge base and attempt do not overflow", 60, 200 * 24 * time.Hour, 290 * 365 * 24 * time.Hour, 0, 290 * 365 * 24 * time.Hour},
		{"attempt below one counts as one", 0, 10 * time.Second, time.Hour, 0, 10 * time.Second},
		{"max below base is the base", 3, 10 * time.Second, time.Second, 0, 10 * time.Second},
		{"full jitter takes 20% off", 1, 10 * time.Second, time.Hour, 1, 8 * time.Second},
		{"half jitter takes 10% off", 1, 10 * time.Second, time.Hour, 0.5, 9 * time.Second},
		{"jitter above one is one", 1, 10 * time.Second, time.Hour, 7, 8 * time.Second},
		{"negative jitter is zero", 1, 10 * time.Second, time.Hour, -1, 10 * time.Second},
		{"jitter keeps the cap a bound", 10, 10 * time.Second, time.Hour, 0, time.Hour},
		{"no base no delay", 3, 0, time.Hour, 0, 0},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, Backoff(tt.attempt, tt.base, tt.max, tt.jitter))
		})
	}
}

func TestBackoffNeverExceedsTheCapOrGoesBelowEightyPercent(t *testing.T) {
	t.Parallel()
	for attempt := 1; attempt <= 60; attempt++ {
		full := Backoff(attempt, time.Second, time.Minute, 0)
		for _, j := range []float64{0, 0.3, 1} {
			d := Backoff(attempt, time.Second, time.Minute, j)
			assert.LessOrEqual(t, d, time.Minute)
			assert.GreaterOrEqual(t, d, time.Duration(float64(full)*0.8)-time.Nanosecond)
			assert.LessOrEqual(t, d, full)
		}
	}
}
