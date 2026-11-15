//go:build integration

package database

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/database/schema"
	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDatabaseAuthTables runs the auth migration and repositories on MySQL:
// the second AutoMigrate is a no-op, the column types are the ones the models
// need, a duplicate username (in any letter case, as the collation compares)
// maps to ErrDuplicateKey, and the guarded admin change locks rows with
// SELECT ... FOR UPDATE.
func TestDatabaseAuthTables(t *testing.T) {
	db, err := NewFromAppConfig(mysqlTestConfig(false))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate())
	require.NoError(t, db.AutoMigrate(), "a second run is a no-op")
	ctx := context.Background()

	for _, table := range []string{"users", "refresh_tokens", "api_keys", "audit_logs"} {
		assert.True(t, db.Migrator().HasTable(table), table)
	}
	var idType string
	require.NoError(t, db.Raw("SELECT DATA_TYPE FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'users' AND column_name = 'id'").Scan(&idType).Error)
	assert.Equal(t, "varchar", idType, "users.id is a UUID string")
	var detailsType string
	require.NoError(t, db.Raw("SELECT DATA_TYPE FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'audit_logs' AND column_name = 'details'").Scan(&detailsType).Error)
	assert.Equal(t, "longtext", detailsType)

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	users := NewGORMUserRepository(db.DB)
	admin := &models.User{Username: "it-admin-" + suffix, Role: models.RoleAdmin}
	member := &models.User{Username: "it-user-" + suffix, Role: models.RoleUser, PasswordHash: "hash"}
	require.NoError(t, users.Create(ctx, admin))
	require.NoError(t, users.Create(ctx, member))
	t.Cleanup(func() {
		db.Where("id IN ?", []string{admin.ID, member.ID}).Delete(&models.User{})
		db.Where("user_id = ?", member.ID).Delete(&models.RefreshToken{})
		db.Where("user_id = ?", member.ID).Delete(&models.APIKey{})
		db.Where("user_id = ?", admin.ID).Delete(&models.AuditLog{})
	})

	err = users.Create(ctx, &models.User{Username: strings.ToUpper(member.Username), Role: models.RoleUser})
	require.ErrorIs(t, err, dberrors.ErrDuplicateKey)
	assert.NotContains(t, err.Error(), suffix, "the duplicate value is not in the error")

	got, err := users.FindByUsername(ctx, member.Username)
	require.NoError(t, err)
	assert.Equal(t, "hash", got.PasswordHash)

	old, err := users.UpdateRole(ctx, admin.ID, member.ID, models.RoleDevOps)
	require.NoError(t, err)
	assert.Equal(t, models.RoleUser, old)
	require.NoError(t, users.SetDisabled(ctx, admin.ID, member.ID, true))
	got, err = users.FindByID(ctx, member.ID)
	require.NoError(t, err)
	assert.Equal(t, models.RoleDevOps, got.Role)
	assert.True(t, got.Disabled)

	page, total, err := users.ListPaged(ctx, models.UserFilter{Search: suffix}, 1, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(2), total)
	require.Len(t, page, 1)
	assert.Empty(t, page[0].PasswordHash)

	// Refresh token: stored hashed, rotated once.
	tokens := NewGORMRefreshTokenRepository(db.DB)
	now := time.Now().UTC()
	rt := &models.RefreshToken{UserID: member.ID, TokenHash: hashOf("raw-" + suffix), ExpiresAt: now.Add(time.Hour), LastActivity: now}
	require.NoError(t, tokens.Create(ctx, rt))
	err = tokens.WithTx(ctx, func(tx models.RefreshTokenRepository) error {
		n, err := tx.MarkRotatedIfActive(ctx, rt.ID, now)
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("rotated %d rows", n)
		}
		return nil
	})
	require.NoError(t, err)
	stored, err := tokens.FindByTokenHash(ctx, hashOf("raw-"+suffix))
	require.NoError(t, err)
	assert.True(t, stored.Revoked)
	require.NotNil(t, stored.RotatedAt)

	// API key: found by prefix and verified against the stored hash.
	raw, prefix, hash, err := models.GenerateAPIKey()
	require.NoError(t, err)
	keys := NewGORMAPIKeyRepository(db.DB)
	require.NoError(t, keys.Create(ctx, &models.APIKey{UserID: member.ID, Name: "ci", KeyHash: hash, Prefix: prefix}))
	found, err := keys.FindByPrefix(ctx, prefix)
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.True(t, models.VerifyAPIKeyHash(raw, found[0].KeyHash))

	// Audit log: filtered list and cursor page.
	audit := NewGORMAuditLogRepository(db.DB)
	for i := 0; i < 3; i++ {
		require.NoError(t, audit.Create(ctx, &models.AuditLog{UserID: admin.ID, Action: "update", EntityType: "item", EntityID: fmt.Sprint(i), Details: strings.Repeat("x", 70000)}))
	}
	res, err := audit.List(ctx, models.AuditLogFilters{UserID: admin.ID, Limit: 2})
	require.NoError(t, err)
	assert.Equal(t, int64(3), res.Total)
	require.Len(t, res.Data, 2)
	next, err := audit.List(ctx, models.AuditLogFilters{UserID: admin.ID, Limit: 2, Cursor: encodeAuditCursor(res.Data[1].Timestamp, res.Data[1].ID)})
	require.NoError(t, err)
	assert.Len(t, next.Data, 1)
}

// TestDatabaseConcurrentAutoMigrate starts several "replicas" at once on a
// database whose users table still has the old shape (the auth migration is
// rolled back first). The named lock serializes them: exactly one applies
// the migration, the others find it applied, and none fails.
func TestDatabaseConcurrentAutoMigrate(t *testing.T) {
	db, err := NewFromAppConfig(mysqlTestConfig(false))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate())
	m := schema.NewMigrator(db.DB)
	registerMigrations(m)
	require.NoError(t, m.MigrateDown(), "roll back 20261115000001")
	require.NoError(t, m.MigrateDown(), "roll back 20260101000003")
	require.NoError(t, m.MigrateDown(), "roll back 20260101000002")
	require.NoError(t, m.MigrateDown(), "roll back 20260101000001")
	require.True(t, db.Migrator().HasColumn("users", "name"), "the old users table is back")

	const replicas = 4
	errs := make(chan error, replicas)
	var wg sync.WaitGroup
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rdb, err := NewFromAppConfig(mysqlTestConfig(false))
			if err != nil {
				errs <- err
				return
			}
			defer func() {
				if sqlDB, err := rdb.DB.DB(); err == nil {
					_ = sqlDB.Close()
				}
			}()
			errs <- rdb.AutoMigrate()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	assert.True(t, db.Migrator().HasColumn("users", "password_hash"))
	var n int64
	require.NoError(t, db.Model(&schema.SchemaVersion{}).Where("version = ?", "20260101000001").Count(&n).Error)
	assert.Equal(t, int64(1), n)
}

// TestDatabaseRefreshRotationIsSingleUse rotates one refresh token from many
// goroutines at once on MySQL, the way the refresh handler does (consume and
// create the successor in one transaction): exactly one rotation commits and
// the family has exactly one active token. Then many goroutines claim the
// reuse grace of the consumed token: exactly one wins.
func TestDatabaseRefreshRotationIsSingleUse(t *testing.T) {
	db, err := NewFromAppConfig(mysqlTestConfig(false))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate())
	ctx := context.Background()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	userID := "it-rotate-" + suffix
	t.Cleanup(func() { db.Where("user_id = ?", userID).Delete(&models.RefreshToken{}) })

	tokens := NewGORMRefreshTokenRepository(db.DB)
	now := time.Now().UTC()
	first := &models.RefreshToken{UserID: userID, TokenHash: hashOf("first-" + suffix), ExpiresAt: now.Add(time.Hour), LastActivity: now}
	require.NoError(t, tokens.Create(ctx, first))
	require.Equal(t, first.ID, first.FamilyID, "a first token is its own family")

	errConsumed := fmt.Errorf("consumed")
	const racers = 8
	results := make(chan error, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- tokens.WithTx(ctx, func(tx models.RefreshTokenRepository) error {
				n, err := tx.MarkRotatedIfActive(ctx, first.ID, time.Now().UTC())
				if err != nil {
					return err
				}
				if n == 0 {
					return errConsumed
				}
				return tx.Create(ctx, &models.RefreshToken{
					UserID: userID, FamilyID: first.FamilyID, TokenHash: hashOf(fmt.Sprintf("next-%s-%d", suffix, i)),
					ExpiresAt: now.Add(time.Hour), LastActivity: time.Now().UTC(), SessionStartedAt: first.SessionStartedAt,
				})
			})
		}(i)
	}
	wg.Wait()
	close(results)
	won := 0
	for err := range results {
		if err == nil {
			won++
			continue
		}
		require.ErrorIs(t, err, errConsumed)
	}
	assert.Equal(t, 1, won, "exactly one rotation commits")
	active, err := tokens.CountActiveInFamily(ctx, first.FamilyID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), active, "the family has one successor")

	claims := make(chan bool, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := tokens.ClaimReuseGrace(ctx, first.ID, now.Add(-time.Minute))
			assert.NoError(t, err)
			claims <- ok
		}()
	}
	wg.Wait()
	close(claims)
	wins := 0
	for ok := range claims {
		if ok {
			wins++
		}
	}
	assert.Equal(t, 1, wins, "the reuse grace is spent once")
}
