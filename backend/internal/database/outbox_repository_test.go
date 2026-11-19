package database

import (
	"context"
	"testing"
	"time"

	"backend/internal/config"
	"backend/internal/models"
	"backend/internal/outbox"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func insertEvent(t *testing.T, db *gorm.DB, eventType, msg string) {
	t.Helper()
	require.NoError(t, InsertOutboxEvent(db, eventType, []byte(msg)))
}

func TestOutboxRepositoryReads(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	repo := NewGORMOutboxRepository(db)
	ctx := context.Background()

	maxID, err := repo.MaxID(ctx)
	require.NoError(t, err)
	assert.Zero(t, maxID, "an empty table")

	for _, m := range []string{`{"n":1}`, `{"n":2}`, `{"n":3}`} {
		insertEvent(t, db, "item.created", m)
	}

	maxID, err = repo.MaxID(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(3), maxID)

	rows, err := repo.ListAfter(ctx, 1, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, uint64(2), rows[0].ID)
	assert.Equal(t, `{"n":3}`, rows[1].Message)
	assert.Equal(t, "item.created", rows[0].EventType)

	rows, err = repo.ListAfter(ctx, 0, 2)
	require.NoError(t, err)
	assert.Len(t, rows, 2, "the limit bounds the read")

	rows, err = repo.ListByIDs(ctx, []uint64{3, 1, 99})
	require.NoError(t, err)
	require.Len(t, rows, 2, "ids that do not exist are left out")
	assert.Equal(t, []uint64{1, 3}, []uint64{rows[0].ID, rows[1].ID}, "in id order")

	rows, err = repo.ListByIDs(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, rows)

	age, ok, err := repo.Age(ctx, 2)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Less(t, age, time.Minute)
	assert.GreaterOrEqual(t, age, time.Duration(0)-time.Second)

	_, ok, err = repo.Age(ctx, 99)
	require.NoError(t, err)
	assert.False(t, ok, "a missing row has no age")
}

func TestOutboxRepositoryDeleteOlderThan(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	repo := NewGORMOutboxRepository(db)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		insertEvent(t, db, "item.created", `{}`)
	}
	// Rows 1 to 4 are two hours old, row 5 is new.
	require.NoError(t, db.Exec("UPDATE outbox_events SET created_at = strftime('%Y-%m-%d %H:%M:%f', 'now', '-2 hours') WHERE id <= 4").Error)

	n, err := repo.DeleteOlderThan(ctx, time.Hour, 3)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n, "a batch is bounded by the limit and takes the oldest first")

	n, err = repo.DeleteOlderThan(ctx, time.Hour, 3)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	n, err = repo.DeleteOlderThan(ctx, time.Hour, 3)
	require.NoError(t, err)
	assert.Zero(t, n, "nothing left that is old")

	var left int64
	require.NoError(t, db.Model(&models.OutboxEvent{}).Count(&left).Error)
	assert.Equal(t, int64(1), left, "the new row stays")
}

func TestOutboxRepositoryCancelledContext(t *testing.T) {
	t.Parallel()
	repo := NewGORMOutboxRepository(openMigratedDB(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := repo.MaxID(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

// The Items repository with the hook writes the event row in the transaction
// of the change, and a failed change leaves none.
func TestItemRepositoryWritesTheOutboxInTheTransaction(t *testing.T) {
	t.Parallel()
	db := openMigratedDB(t)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	repo := models.NewRepository(db, models.WithChangeHook(outbox.ItemHook(InsertOutboxEvent)))
	ctx := context.Background()
	outboxRepo := NewGORMOutboxRepository(db)

	item := &models.Item{Name: "pen", Price: 2, Version: 1}
	require.NoError(t, repo.Create(ctx, item))
	item.Price = 3
	require.NoError(t, repo.Update(ctx, item))
	require.NoError(t, repo.Delete(ctx, &models.Item{Base: models.Base{ID: item.ID}}))

	rows, err := outboxRepo.ListAfter(ctx, 0, 10)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	assert.Equal(t, []string{"item.created", "item.updated", "item.deleted"}, []string{rows[0].EventType, rows[1].EventType, rows[2].EventType})
	assert.Contains(t, rows[1].Message, `"version":2`)
	assert.JSONEq(t, `{"type":"item.deleted","payload":{"id":1}}`, rows[2].Message)

	// A validation error, a duplicate-free failure and a stale version leave no row.
	require.Error(t, repo.Create(ctx, &models.Item{Name: "", Version: 1}))
	stale := &models.Item{Base: models.Base{ID: 99}, Name: "x", Price: 1, Version: 4}
	require.Error(t, repo.Update(ctx, stale))
	require.Error(t, repo.Delete(ctx, &models.Item{Base: models.Base{ID: 99}}))
	maxID, err := outboxRepo.MaxID(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(3), maxID, "no row for a failed change")
}

func TestRepositoryOptions(t *testing.T) {
	t.Parallel()
	assert.Empty(t, repositoryOptions(nil))
	assert.Empty(t, repositoryOptions(&config.Config{}), "module off: no hook")

	cfg := &config.Config{Outbox: config.OutboxConfig{Enabled: true}}
	opts := repositoryOptions(cfg)
	require.Len(t, opts, 1)
	repo := models.NewRepository(&gorm.DB{}, opts...)
	assert.True(t, repo.(*models.GenericRepository).HasChangeHook())
}
