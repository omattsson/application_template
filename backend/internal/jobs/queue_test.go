package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// enqueueRecorder is a models.JobRepository that records Enqueue and fails
// every other method (a Queue must not use them).
type enqueueRecorder struct {
	models.JobRepository
	err   error
	specs []models.JobSpec
}

func (r *enqueueRecorder) Enqueue(_ context.Context, spec models.JobSpec) (uint64, error) {
	if r.err != nil {
		return 0, r.err
	}
	r.specs = append(r.specs, spec)
	return uint64(len(r.specs)), nil
}

func TestQueueEnqueue(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	type payload struct {
		To string `json:"to"`
	}
	tests := []struct {
		payload     any
		check       func(t *testing.T, s models.JobSpec)
		name        string
		typ         string
		opts        []Option
		wantErr     error
		wantInvalid bool
	}{
		{name: "defaults", typ: "mail", payload: payload{To: "a@b.c"}, check: func(t *testing.T, s models.JobSpec) {
			assert.Equal(t, "mail", s.Type)
			assert.JSONEq(t, `{"to":"a@b.c"}`, string(s.Payload))
			assert.Equal(t, 7, s.MaxAttempts, "the configured default")
			assert.Zero(t, s.Delay)
		}},
		{name: "nil payload is JSON null", typ: "mail", payload: nil, check: func(t *testing.T, s models.JobSpec) {
			assert.Equal(t, "null", string(s.Payload))
		}},
		{name: "delay", typ: "mail", payload: 1, opts: []Option{WithDelay(time.Minute)}, check: func(t *testing.T, s models.JobSpec) {
			assert.Equal(t, time.Minute, s.Delay)
		}},
		{name: "run at in the future", typ: "mail", payload: 1, opts: []Option{WithRunAt(fixed.Add(90 * time.Second))}, check: func(t *testing.T, s models.JobSpec) {
			assert.Equal(t, 90*time.Second, s.Delay)
		}},
		{name: "run at in the past is now", typ: "mail", payload: 1, opts: []Option{WithRunAt(fixed.Add(-time.Hour))}, check: func(t *testing.T, s models.JobSpec) {
			assert.Zero(t, s.Delay)
		}},
		{name: "the last time option wins", typ: "mail", payload: 1, opts: []Option{WithRunAt(fixed.Add(time.Hour)), WithDelay(time.Second)}, check: func(t *testing.T, s models.JobSpec) {
			assert.Equal(t, time.Second, s.Delay)
		}},
		{name: "max attempts", typ: "mail", payload: 1, opts: []Option{WithMaxAttempts(1)}, check: func(t *testing.T, s models.JobSpec) {
			assert.Equal(t, 1, s.MaxAttempts)
		}},
		{name: "max attempts 50", typ: "mail", payload: 1, opts: []Option{WithMaxAttempts(50)}, check: func(t *testing.T, s models.JobSpec) {
			assert.Equal(t, 50, s.MaxAttempts)
		}},
		{name: "invalid type", typ: "Mail!", payload: 1, wantErr: ErrInvalidType},
		{name: "empty type", typ: "", payload: 1, wantErr: ErrInvalidType},
		{name: "zero max attempts", typ: "mail", payload: 1, opts: []Option{WithMaxAttempts(0)}, wantErr: ErrInvalidOption},
		{name: "51 max attempts", typ: "mail", payload: 1, opts: []Option{WithMaxAttempts(51)}, wantErr: ErrInvalidOption},
		{name: "negative delay", typ: "mail", payload: 1, opts: []Option{WithDelay(-time.Second)}, wantErr: ErrInvalidOption},
		{name: "payload that does not marshal", typ: "mail", payload: make(chan int), wantErr: ErrInvalidPayload},
		{name: "payload over 64 KiB", typ: "mail", payload: strings.Repeat("a", models.JobPayloadMaxBytes), wantErr: ErrPayloadTooLarge},
		{name: "payload of exactly 64 KiB", typ: "mail", payload: strings.Repeat("a", models.JobPayloadMaxBytes-2), check: func(t *testing.T, s models.JobSpec) {
			assert.Len(t, s.Payload, models.JobPayloadMaxBytes)
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &enqueueRecorder{}
			q := NewQueue(rec, 7)
			q.now = func() time.Time { return fixed }
			id, err := q.Enqueue(context.Background(), tt.typ, tt.payload, tt.opts...)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.Empty(t, rec.specs, "nothing is stored")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, uint64(1), id)
			require.Len(t, rec.specs, 1)
			tt.check(t, rec.specs[0])
		})
	}
}

func TestQueueEnqueueRepositoryError(t *testing.T) {
	t.Parallel()
	want := dberrors.NewDatabaseError("job enqueue", errors.New("down"))
	q := NewQueue(&enqueueRecorder{err: want}, 5)
	_, err := q.Enqueue(context.Background(), "mail", 1)
	assert.ErrorIs(t, err, want)
}

func TestQueueNilIsDisabled(t *testing.T) {
	t.Parallel()
	var q *Queue
	_, err := q.Enqueue(context.Background(), "mail", 1)
	assert.ErrorIs(t, err, ErrDisabled)
}

func TestQueueDefaultMaxAttemptsOutOfRange(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, -1, 51} {
		rec := &enqueueRecorder{}
		_, err := NewQueue(rec, n).Enqueue(context.Background(), "mail", 1)
		require.NoError(t, err)
		assert.Equal(t, 5, rec.specs[0].MaxAttempts, n)
	}
}

func TestPermanent(t *testing.T) {
	t.Parallel()
	base := errors.New("bad input")
	p := Permanent(base)
	assert.Equal(t, "bad input", p.Error())
	assert.True(t, IsPermanent(p))
	assert.ErrorIs(t, p, base)
	assert.True(t, IsPermanent(errors.Join(errors.New("x"), p)), "found through a wrapper")
	assert.False(t, IsPermanent(base))
	assert.False(t, IsPermanent(nil))
	assert.Nil(t, Permanent(nil))
}
