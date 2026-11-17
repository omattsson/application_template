package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"backend/internal/config"
	"backend/internal/leader"
	"backend/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// idempotencyConfig is the bootstrap config with the module switched.
func idempotencyConfig(on bool) *config.Config {
	cfg := bootstrapTestConfig()
	cfg.Idempotency = config.IdempotencyConfig{Enabled: on, TTL: time.Hour, LockTimeout: time.Minute}
	return cfg
}

// With the module off nothing is built: no worker and no query on the table
// (the test database has no idempotency_keys table, so a query would fail).
func TestBootstrapIdempotencyOff(t *testing.T) {
	t.Setenv("GIN_MODE", gin.TestMode)
	repo, db := openSQLiteRepo(t)
	require.False(t, db.Migrator().HasTable("idempotency_keys"))
	s, url := startServices(t, idempotencyConfig(false), repo)
	g, ok := s.leader.(*leader.Group)
	require.True(t, ok)
	assert.NotContains(t, g.Names(), "idempotency-cleanup")

	hdr := map[string]string{"Idempotency-Key": "k"}
	for i := 0; i < 2; i++ {
		code, body := call(t, http.MethodPost, url+"/api/v1/items", `{"name":"widget","price":3}`, hdr)
		assert.Equal(t, http.StatusCreated, code, body)
	}
}

func TestBootstrapIdempotencyOn(t *testing.T) {
	t.Setenv("GIN_MODE", gin.TestMode)
	repo, db := openSQLiteRepo(t)
	require.NoError(t, db.AutoMigrate(&models.IdempotencyKey{}))
	s, url := startServices(t, idempotencyConfig(true), repo)
	g, ok := s.leader.(*leader.Group)
	require.True(t, ok)
	assert.Contains(t, g.Names(), "idempotency-cleanup")
	assert.True(t, g.Running())

	hdr := map[string]string{"Idempotency-Key": "k"}
	code, first := call(t, http.MethodPost, url+"/api/v1/items", `{"name":"widget","price":3}`, hdr)
	require.Equal(t, http.StatusCreated, code, first)
	code, second := call(t, http.MethodPost, url+"/api/v1/items", `{"name":"widget","price":3}`, hdr)
	assert.Equal(t, http.StatusCreated, code)
	assert.Equal(t, first, second, "the stored response is replayed")
	var n int64
	require.NoError(t, db.Model(&models.Item{}).Count(&n).Error)
	assert.Equal(t, int64(1), n)
}

// The module with auth on gives the worker and the per-user scope.
func TestBootstrapIdempotencyWithAuth(t *testing.T) {
	t.Setenv("GIN_MODE", gin.TestMode)
	repo, db := openSQLiteRepo(t)
	require.NoError(t, db.AutoMigrate(&models.IdempotencyKey{}))
	cfg := authModeConfig(true)
	cfg.Idempotency = config.IdempotencyConfig{Enabled: true, TTL: time.Hour, LockTimeout: time.Minute}
	s, _ := startServices(t, cfg, repo)
	g, ok := s.leader.(*leader.Group)
	require.True(t, ok)
	assert.ElementsMatch(t, []string{"refresh-token-cleanup", "session-store-cleanup", "idempotency-cleanup"}, g.Names())
}

// The module needs the database of the repository: a missing one stops the
// start, it is never skipped.
func TestBootstrapIdempotencyNeedsGormRepository(t *testing.T) {
	t.Setenv("GIN_MODE", gin.TestMode)
	s, router, err := buildServices(idempotencyConfig(true), &stubRepository{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "IDEMPOTENCY_ENABLED")
	assert.Nil(t, s)
	assert.Nil(t, router)
}

// fakeCleanupRepo serves DeleteExpired from a list of batch sizes.
type fakeCleanupRepo struct {
	models.IdempotencyRepository
	err     error
	batches []int64
	limits  []int
	mu      sync.Mutex
}

func (f *fakeCleanupRepo) DeleteExpired(ctx context.Context, limit int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limits = append(f.limits, limit)
	if f.err != nil {
		return 0, f.err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(f.batches) == 0 {
		return 0, nil
	}
	n := f.batches[0]
	f.batches = f.batches[1:]
	return n, nil
}

func TestIdempotencyCleanupJob(t *testing.T) {
	t.Parallel()

	t.Run("loops until a batch is short", func(t *testing.T) {
		t.Parallel()
		repo := &fakeCleanupRepo{batches: []int64{100, 100, 100, 7, 55}}
		require.NoError(t, idempotencyCleanupJob(repo, 100)(context.Background()))
		assert.Equal(t, []int{100, 100, 100, 100}, repo.limits, "every call is bounded by the batch size and the loop stops after the short batch")
		assert.Equal(t, []int64{55}, repo.batches)
	})

	t.Run("nothing to delete is one call", func(t *testing.T) {
		t.Parallel()
		repo := &fakeCleanupRepo{}
		require.NoError(t, idempotencyCleanupJob(repo, 100)(context.Background()))
		assert.Len(t, repo.limits, 1)
	})

	t.Run("an error ends the run", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("db down")
		repo := &fakeCleanupRepo{err: boom}
		require.ErrorIs(t, idempotencyCleanupJob(repo, 100)(context.Background()), boom)
		assert.Len(t, repo.limits, 1)
	})

	t.Run("a cancelled context ends a long backlog", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		repo := &cancellingRepo{cancel: cancel}
		start := time.Now()
		err := idempotencyCleanupJob(repo, 10)(ctx)
		require.ErrorIs(t, err, context.Canceled)
		assert.Less(t, time.Since(start), time.Second)
		assert.LessOrEqual(t, repo.calls, 4)
	})
}

// cancellingRepo always returns a full batch and cancels after two calls.
type cancellingRepo struct {
	models.IdempotencyRepository
	cancel context.CancelFunc
	calls  int
}

func (c *cancellingRepo) DeleteExpired(_ context.Context, limit int) (int64, error) {
	c.calls++
	if c.calls == 2 {
		c.cancel()
	}
	return int64(limit), nil
}
