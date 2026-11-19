package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/leader"
	"backend/internal/models"
	"backend/internal/outbox"

	"github.com/gin-gonic/gin"
	gorilla "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// fakePoller records the stop of the outbox poller.
type fakePoller struct {
	rec     *callRecorder
	timeout time.Duration
}

func (f *fakePoller) Stop(timeout time.Duration) {
	f.timeout = timeout
	f.rec.add("outbox poller")
}

// The poller stops after the HTTP server and the in-flight work (nothing new
// is written) and before the hub closes (nothing is left to deliver to) and
// before the database closes, within its own fixed budget.
func TestShutdownStopsOutboxPollerBeforeHubAndDatabase(t *testing.T) {
	t.Parallel()
	rec := &callRecorder{}
	s := fakeServices(rec)
	p := &fakePoller{rec: rec}
	s.outboxPoller = p

	require.NoError(t, shutdown(context.Background(), s))
	assert.Equal(t, []string{
		"server", "leader group", "in-flight", "outbox poller", "hub", "telemetry", "rate limiters", "database",
	}, rec.list())
	assert.Equal(t, outboxStopTimeout, p.timeout)

	// A typed nil poller (module off) is skipped.
	rec2 := &callRecorder{}
	s2 := fakeServices(rec2)
	s2.outboxPoller = (*outbox.Poller)(nil)
	require.NoError(t, shutdown(context.Background(), s2))
	assert.NotContains(t, rec2.list(), "outbox poller")
}

// outboxRepo opens an SQLite repository with the outbox table, with or
// without the change hook.
func outboxRepo(t *testing.T, hook bool) (models.Repository, *gorm.DB) {
	t.Helper()
	_, db := openSQLiteRepo(t)
	require.NoError(t, db.AutoMigrate(&models.OutboxEvent{}))
	if !hook {
		return models.NewRepository(db), db
	}
	return models.NewRepository(db, models.WithChangeHook(outbox.ItemHook(database.InsertOutboxEvent))), db
}

func outboxConfig(enabled bool) *config.Config {
	cfg := authModeConfig(false)
	cfg.Outbox = config.OutboxConfig{Enabled: enabled, PollInterval: 20 * time.Millisecond, Retention: time.Hour}
	return cfg
}

// With the module off there is no poller, no janitor and no table access.
func TestBootstrapOutboxOff(t *testing.T) {
	t.Setenv("GIN_MODE", gin.TestMode)
	repo, db := openSQLiteRepo(t) // no outbox_events table at all
	require.False(t, db.Migrator().HasTable("outbox_events"))

	s, _ := startServices(t, outboxConfig(false), repo)

	assert.True(t, isNilInterface(s.outboxPoller))
	g, ok := s.leader.(*leader.Group)
	require.True(t, ok)
	assert.NotContains(t, g.Names(), "outbox-cleanup")
}

// With the module on, an item written through the API reaches a socket of
// this replica through the table and the poller, once.
func TestBootstrapOutboxOnDeliversThroughTheTable(t *testing.T) {
	t.Setenv("GIN_MODE", gin.TestMode)
	repo, db := outboxRepo(t, true)
	s, base := startServices(t, outboxConfig(true), repo)

	g, ok := s.leader.(*leader.Group)
	require.True(t, ok)
	assert.Contains(t, g.Names(), "outbox-cleanup")
	require.False(t, isNilInterface(s.outboxPoller))

	conn, resp, err := gorilla.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/ws", nil)
	require.NoError(t, err)
	_ = resp.Body.Close()
	t.Cleanup(func() { _ = conn.Close() })
	time.Sleep(100 * time.Millisecond) // the hub registers the client asynchronously

	status, _ := call(t, http.MethodPost, base+"/api/v1/items", `{"name":"pen","price":1.5}`, nil)
	require.Equal(t, http.StatusCreated, status)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, msg, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(msg), `"type":"item.created"`)

	// Exactly one copy: no direct broadcast next to the poller.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
	_, _, err = conn.ReadMessage()
	assert.Error(t, err, "a second copy would arrive here")

	var n int64
	require.NoError(t, db.Model(&models.OutboxEvent{}).Count(&n).Error)
	assert.Equal(t, int64(1), n)
}

// OUTBOX_ENABLED=true without the change hook would write no event at all:
// the start fails instead of dropping events silently.
func TestBootstrapOutboxNeedsTheHook(t *testing.T) {
	t.Setenv("GIN_MODE", gin.TestMode)
	repo, _ := outboxRepo(t, false)

	for name, r := range map[string]models.Repository{
		"a repository without the hook":   repo,
		"a repository without a database": &stubRepository{},
	} {
		s, router, err := buildServices(outboxConfig(true), r)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "outbox", name)
		assert.Nil(t, s, name)
		assert.Nil(t, router, name)
	}
}

func TestOutboxRepositoryOptionsMatchTheSwitch(t *testing.T) {
	t.Setenv("GIN_MODE", gin.TestMode)
	repo, _ := outboxRepo(t, true)
	assert.True(t, repo.(interface{ HasChangeHook() bool }).HasChangeHook())
}

// fakeOutboxStore counts deletes for the janitor tests.
type fakeOutboxStore struct {
	models.OutboxRepository
	err     error
	results []int64
	limits  []int
	ages    []time.Duration
	mu      sync.Mutex
}

func (f *fakeOutboxStore) DeleteOlderThan(_ context.Context, maxAge time.Duration, limit int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ages = append(f.ages, maxAge)
	f.limits = append(f.limits, limit)
	if f.err != nil {
		return 0, f.err
	}
	if len(f.results) == 0 {
		return 0, nil
	}
	n := f.results[0]
	f.results = f.results[1:]
	return n, nil
}

func TestOutboxCleanup(t *testing.T) {
	t.Parallel()

	t.Run("deletes in bounded batches until one is short", func(t *testing.T) {
		t.Parallel()
		store := &fakeOutboxStore{results: []int64{outboxCleanupBatch, outboxCleanupBatch, 7}}
		require.NoError(t, outboxCleanup(store, 2*time.Hour)(context.Background()))
		assert.Equal(t, []int{outboxCleanupBatch, outboxCleanupBatch, outboxCleanupBatch}, store.limits)
		assert.Equal(t, 2*time.Hour, store.ages[0])
	})

	t.Run("one run is bounded even when the table keeps filling", func(t *testing.T) {
		t.Parallel()
		results := make([]int64, outboxCleanupMaxBatches+50)
		for i := range results {
			results[i] = outboxCleanupBatch
		}
		store := &fakeOutboxStore{results: results}
		require.NoError(t, outboxCleanup(store, time.Hour)(context.Background()))
		assert.Len(t, store.limits, outboxCleanupMaxBatches)
	})

	t.Run("a cancelled context ends the run", func(t *testing.T) {
		t.Parallel()
		store := &fakeOutboxStore{results: []int64{outboxCleanupBatch, outboxCleanupBatch}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := outboxCleanup(store, time.Hour)(ctx)
		assert.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, store.limits, "no statement after the cancel")
	})

	t.Run("an error is returned", func(t *testing.T) {
		t.Parallel()
		store := &fakeOutboxStore{err: errors.New("database down")}
		assert.Error(t, outboxCleanup(store, time.Hour)(context.Background()))
	})
}
