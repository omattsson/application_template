//go:build integration

package database

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUserGuardedChangesConcurrentAdminsMySQL fires two admins that demote,
// disable or delete each other at the same moment, on real MySQL row locks:
// the changes serialize, so exactly one wins and the other is refused
// (ErrCallerNotAdmin or ErrLastAdmin). At least one enabled admin remains.
func TestUserGuardedChangesConcurrentAdminsMySQL(t *testing.T) {
	db, err := NewFromAppConfig(mysqlTestConfig(false))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate())
	ctx := context.Background()
	users := NewGORMUserRepository(db.DB)

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	mk := func(name string) *models.User {
		return &models.User{Username: "it-guard-" + name + "-" + suffix, Role: models.RoleAdmin, PasswordHash: "hash"}
	}
	x, y := mk("x"), mk("y")
	require.NoError(t, users.Create(ctx, x))
	require.NoError(t, users.Create(ctx, y))
	t.Cleanup(func() { db.Where("id IN ?", []string{x.ID, y.ID}).Delete(&models.User{}) })

	// The last-admin rule counts every admin of the table: the test needs
	// the two to be the only ones.
	var others int64
	require.NoError(t, db.Model(&models.User{}).Where("role = ? AND disabled = ? AND id NOT IN ?", models.RoleAdmin, false, []string{x.ID, y.ID}).Count(&others).Error)
	if others > 0 {
		t.Skipf("%d other enabled admins in the database; the race needs only two", others)
	}

	reset := func() {
		for _, u := range []*models.User{x, y} {
			var n int64
			require.NoError(t, db.Model(&models.User{}).Where("id = ?", u.ID).Count(&n).Error)
			if n == 0 {
				require.NoError(t, db.Create(&models.User{ID: u.ID, Username: u.Username, Role: models.RoleAdmin, PasswordHash: "hash", AuthProvider: models.AuthProviderLocal}).Error)
				continue
			}
			require.NoError(t, db.Model(&models.User{}).Where("id = ?", u.ID).Updates(map[string]any{"role": models.RoleAdmin, "disabled": false}).Error)
		}
	}
	enabledAdmins := func() int64 {
		var n int64
		require.NoError(t, db.Model(&models.User{}).Where("id IN ? AND role = ? AND disabled = ?", []string{x.ID, y.ID}, models.RoleAdmin, false).Count(&n).Error)
		return n
	}

	changes := map[string]func(caller, target string) error{
		"demote": func(caller, target string) error {
			_, err := users.UpdateRole(ctx, caller, target, models.RoleUser)
			return err
		},
		"disable": func(caller, target string) error { return users.SetDisabled(ctx, caller, target, true) },
		"delete":  func(caller, target string) error { return users.DeleteGuarded(ctx, caller, target) },
	}
	for name, change := range changes {
		for round := 0; round < 25; round++ {
			reset()
			errs := make([]error, 2)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i, pair := range [][2]string{{x.ID, y.ID}, {y.ID, x.ID}} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					errs[i] = change(pair[0], pair[1])
				}()
			}
			close(start)
			wg.Wait()

			assert.GreaterOrEqual(t, enabledAdmins(), int64(1), "%s round %d: %v", name, round, errs)
			won := 0
			for _, e := range errs {
				switch {
				case e == nil:
					won++
				case errors.Is(e, models.ErrCallerNotAdmin), errors.Is(e, models.ErrLastAdmin):
				default:
					t.Fatalf("%s round %d: unexpected error %v", name, round, e)
				}
			}
			assert.Equal(t, 1, won, "%s round %d: %v", name, round, errs)
		}
	}
}
