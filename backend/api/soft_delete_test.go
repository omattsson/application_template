package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// fakePurger is a models.SoftDeleter that returns scripted batch sizes.
type fakePurger struct {
	err     error
	cutoffs []time.Time
	limits  []int
	batches []int64
	mu      sync.Mutex
}

func (f *fakePurger) Restore(context.Context, uint) error { return nil }

func (f *fakePurger) PurgeDeleted(ctx context.Context, before time.Time, limit int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cutoffs = append(f.cutoffs, before)
	f.limits = append(f.limits, limit)
	if f.err != nil {
		return 0, f.err
	}
	if len(f.batches) == 0 {
		return 0, nil
	}
	n := f.batches[0]
	f.batches = f.batches[1:]
	return n, nil
}

func (f *fakePurger) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.limits)
}

func TestPurgeJob(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err       error
		name      string
		batches   []int64
		wantCalls int
		wantErr   bool
	}{
		{name: "nothing to purge", batches: nil, wantCalls: 1},
		{name: "a short batch ends the run", batches: []int64{3}, wantCalls: 1},
		{name: "full batches repeat until a short one", batches: []int64{5, 5, 2}, wantCalls: 3},
		{name: "a run is bounded", batches: []int64{5, 5, 5, 5, 5, 5, 5, 5}, wantCalls: 4},
		{name: "an error ends the run", err: errors.New("db down"), wantCalls: 1, wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakePurger{batches: tt.batches, err: tt.err}
			start := time.Now()
			err := purgeJob(f, 24*time.Hour, 5, 4)(context.Background())
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.wantCalls, f.calls())
			for _, l := range f.limits {
				assert.Equal(t, 5, l)
			}
			for _, c := range f.cutoffs {
				assert.WithinDuration(t, start.Add(-24*time.Hour), c, 5*time.Second, "cutoff is now minus the retention")
			}
		})
	}

	t.Run("a cancelled context stops before the next batch", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		f := &fakePurger{batches: []int64{5, 5, 5, 5}}
		var n atomic.Int32
		job := purgeJob(&cancelAfter{SoftDeleter: f, cancel: cancel, after: 1, n: &n}, time.Hour, 5, 100)
		err := job(ctx)
		assert.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, 1, f.calls())
	})
}

// cancelAfter cancels the context after the given number of batches.
type cancelAfter struct {
	models.SoftDeleter
	cancel func()
	n      *atomic.Int32
	after  int32
}

func (c *cancelAfter) PurgeDeleted(ctx context.Context, before time.Time, limit int) (int64, error) {
	n, err := c.SoftDeleter.PurgeDeleted(ctx, before, limit)
	if c.n.Add(1) >= c.after {
		c.cancel()
	}
	return n, err
}

type purgeNote struct {
	models.Base
	Name string `gorm:"size:255;not null"`
}

func TestPurgeWorker(t *testing.T) {
	t.Parallel()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&purgeNote{}))
	repo := models.NewRepositoryWithFilterFields(db, nil, models.WithSoftDeleteOf(&purgeNote{}))
	ctx := context.Background()

	old := time.Now().UTC().Add(-48 * time.Hour)
	var ids []uint
	for _, name := range []string{"a", "b", "c", "live", "fresh"} {
		n := &purgeNote{Name: name}
		require.NoError(t, repo.Create(ctx, n))
		ids = append(ids, n.ID)
	}
	for _, id := range ids[:3] {
		require.NoError(t, db.Model(&purgeNote{}).Where("id = ?", id).UpdateColumn("deleted_at", old).Error)
	}
	require.NoError(t, repo.Delete(ctx, &purgeNote{Base: models.Base{ID: ids[4]}}))

	sd, ok := repo.(models.SoftDeleter)
	require.True(t, ok)
	w := purgeWorker("note-purge", 5*time.Millisecond, 24*time.Hour, sd)
	assert.Equal(t, "note-purge", w.Name)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(runCtx)
	}()
	require.Eventually(t, func() bool {
		var left int64
		db.Model(&purgeNote{}).Count(&left)
		return left == 2
	}, 2*time.Second, 5*time.Millisecond, "old rows are purged on the first run")
	time.Sleep(30 * time.Millisecond) // several more ticks: idempotent
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker did not stop with its context")
	}

	var left int64
	require.NoError(t, db.Model(&purgeNote{}).Count(&left).Error)
	assert.Equal(t, int64(2), left, "the live row and the recently deleted row stay")
}
