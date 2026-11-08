//go:build integration

package sessionstore_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/sessionstore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// The integration tests run the shared tests of sessionstore_test.go on a
// real MySQL, and add the ones that need one: the migration, the collation of
// the key, and the concurrent consume of an OIDC state.
//
// The table is shared, so a test that uses it holds mysqlTable until it ends
// (lockedTable) and starts from an empty table. Nothing else writes to
// session_entries. The database package integration tests roll the migration
// back, so the Makefile runs the integration packages one at a time (-p 1).
var mysqlTable sync.Mutex

var (
	mysqlOnce sync.Once
	mysqlDB   *gorm.DB
	mysqlErr  error
)

func mysqlConfig() *config.Config {
	getenv := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	return &config.Config{
		App: config.AppConfig{Name: "testapp", Environment: "testing"},
		Database: config.DatabaseConfig{
			Host:            getenv("DB_HOST", "localhost"),
			Port:            getenv("DB_PORT", "3306"),
			User:            getenv("DB_USER", "appuser"),
			Password:        getenv("DB_PASSWORD", "apppass"),
			DBName:          getenv("DB_NAME", "app"),
			MaxOpenConns:    8,
			MaxIdleConns:    4,
			ConnMaxLifetime: 5 * time.Minute,
		},
	}
}

// openMySQL returns the migrated database, connected once for all tests.
func openMySQL(t *testing.T) *gorm.DB {
	t.Helper()
	mysqlOnce.Do(func() {
		db, err := database.NewFromAppConfig(mysqlConfig())
		if err != nil {
			mysqlErr = err
			return
		}
		mysqlErr = db.AutoMigrate()
		mysqlDB = db.DB
	})
	require.NoError(t, mysqlErr)
	return mysqlDB
}

// lockedTable takes the table for the test and empties it.
func lockedTable(t *testing.T) *gorm.DB {
	t.Helper()
	db := openMySQL(t)
	mysqlTable.Lock()
	t.Cleanup(mysqlTable.Unlock)
	require.NoError(t, db.Exec("DELETE FROM session_entries").Error)
	return db
}

func init() {
	extraBackends = append(extraBackends, backend{
		name: "mysql",
		build: func(t *testing.T, clk *fakeClock) sessionstore.SessionStore {
			t.Helper()
			s := sessionstore.NewMySQLWithClock(lockedTable(t), clk.Now, quiet)
			t.Cleanup(s.Stop)
			return s
		},
	})
}

// The key column compares letter case and bytes, and holds 255 characters.
//
//nolint:paralleltest // the tests share the session_entries table
func TestMySQLTable(t *testing.T) {
	db := lockedTable(t)
	var collation, keyType string
	require.NoError(t, db.Raw("SELECT COLLATION_NAME FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'session_entries' AND column_name = 'entry_key'").Scan(&collation).Error)
	assert.Equal(t, "utf8mb4_0900_bin", collation, "keys are case-sensitive")
	require.NoError(t, db.Raw("SELECT COLUMN_TYPE FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'session_entries' AND column_name = 'entry_key'").Scan(&keyType).Error)
	assert.Equal(t, "varchar(255)", keyType)
	var idx int64
	require.NoError(t, db.Raw("SELECT COUNT(1) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'session_entries' AND index_name = 'idx_session_entries_expires_at'").Scan(&idx).Error)
	assert.Equal(t, int64(1), idx)

	s := sessionstore.NewMySQL(db)
	defer s.Stop()
	ctx := context.Background()
	until := time.Now().Add(time.Hour)

	// "AbC" and "abc" are two keys; MySQL's default collation would merge them.
	require.NoError(t, s.BlockToken(ctx, "AbC", until))
	upper, err := s.IsTokenBlocked(ctx, "AbC")
	require.NoError(t, err)
	lower, err := s.IsTokenBlocked(ctx, "abc")
	require.NoError(t, err)
	assert.True(t, upper)
	assert.False(t, lower)
	require.NoError(t, s.BlockToken(ctx, "abc", until))

	// 255 four-byte characters fit the column; trailing spaces count.
	wide := strings.Repeat("\U0001F600", 255)
	require.NoError(t, s.BlockToken(ctx, wide, until))
	blocked, err := s.IsTokenBlocked(ctx, wide)
	require.NoError(t, err)
	assert.True(t, blocked)
	require.NoError(t, s.BlockToken(ctx, "pad", until))
	padded, err := s.IsTokenBlocked(ctx, "pad ")
	require.NoError(t, err)
	assert.False(t, padded, "a key with a trailing space is another key")

	// The largest state fits the TEXT column.
	big := sessionstore.OIDCStateData{RedirectURL: strings.Repeat("å", 30000)}
	require.NoError(t, s.SaveOIDCState(ctx, "big", big, time.Minute))
	got, err := s.ConsumeOIDCState(ctx, "big")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, big, *got)
}

// A state is single-use when several replicas consume it at once: each
// consumer is another store on another connection, and exactly one gets the
// data.
//
//nolint:paralleltest // the tests share the session_entries table
func TestMySQLConcurrentConsumeIsSingleUse(t *testing.T) {
	db := lockedTable(t)
	ctx := context.Background()
	const replicas, rounds = 8, 40

	stores := make([]sessionstore.SessionStore, replicas)
	for i := range stores {
		stores[i] = sessionstore.NewMySQL(db)
		defer stores[i].Stop()
	}

	for round := 0; round < rounds; round++ {
		state := fmt.Sprintf("state-%d", round)
		require.NoError(t, stores[0].SaveOIDCState(ctx, state, sessionstore.OIDCStateData{CodeVerifier: state}, time.Minute))

		var wg sync.WaitGroup
		results := make(chan *sessionstore.OIDCStateData, replicas)
		errs := make(chan error, replicas)
		start := make(chan struct{})
		for _, s := range stores {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				got, err := s.ConsumeOIDCState(ctx, state)
				errs <- err
				results <- got
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		close(errs)

		for err := range errs {
			require.NoError(t, err, "round %d", round)
		}
		winners := 0
		for got := range results {
			if got != nil {
				winners++
				assert.Equal(t, state, got.CodeVerifier)
			}
		}
		require.Equal(t, 1, winners, "round %d: exactly one consumer gets the state", round)
	}
}

// Cleanup on MySQL removes the expired rows of every kind and nothing else,
// also while another replica consumes and blocks at the same time.
//
//nolint:paralleltest // the tests share the session_entries table
func TestMySQLCleanup(t *testing.T) {
	db := lockedTable(t)
	ctx := context.Background()
	clk := newFakeClock()
	s := sessionstore.NewMySQLWithClock(db, clk.Now, quiet)
	defer s.Stop()

	soon, later := clk.Now().Add(time.Minute), clk.Now().Add(time.Hour)
	require.NoError(t, s.BlockToken(ctx, "tok-soon", soon))
	require.NoError(t, s.BlockToken(ctx, "tok-later", later))
	require.NoError(t, s.BlockUser(ctx, "user-soon", soon))
	require.NoError(t, s.BlockSessions(ctx, []string{"sid-soon-1", "sid-soon-2"}, soon))
	require.NoError(t, s.BlockSession(ctx, "sid-later", later))
	require.NoError(t, s.SaveOIDCState(ctx, "state-soon", sessionstore.OIDCStateData{}, time.Minute))
	require.NoError(t, s.SaveOIDCState(ctx, "state-later", sessionstore.OIDCStateData{}, time.Hour))
	require.Equal(t, 8, sessionstore.EntryCount(s))

	clk.Advance(time.Minute)
	other := sessionstore.NewMySQLWithClock(db, clk.Now, quiet)
	defer other.Stop()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); assert.NoError(t, s.Cleanup(ctx)) }()
		go func() { defer wg.Done(); assert.NoError(t, other.Cleanup(ctx)) }()
	}
	wg.Wait()

	assert.Equal(t, 3, sessionstore.EntryCount(s), "five expired rows are gone; tok-later, sid-later and state-later stay")
	tok, err := s.IsTokenBlocked(ctx, "tok-later")
	require.NoError(t, err)
	sess, err := s.IsSessionBlocked(ctx, "sid-later")
	require.NoError(t, err)
	state, err := s.ConsumeOIDCState(ctx, "state-later")
	require.NoError(t, err)
	assert.True(t, tok)
	assert.True(t, sess)
	assert.NotNil(t, state)
}

// The block of many sessions is one multi-row upsert per batch on MySQL, and a
// second block of the same sessions keeps the later expiry (GREATEST).
//
//nolint:paralleltest // the tests share the session_entries table
func TestMySQLBlockSessionsBatches(t *testing.T) {
	db := lockedTable(t)
	ctx := context.Background()
	clk := newFakeClock()
	s := sessionstore.NewMySQLWithClock(db, clk.Now, quiet)
	defer s.Stop()

	ids := make([]string, 1203)
	for i := range ids {
		ids[i] = fmt.Sprintf("sid-%04d", i)
	}
	require.NoError(t, s.BlockSessions(ctx, ids, clk.Now().Add(time.Hour)))
	require.NoError(t, s.BlockSessions(ctx, ids, clk.Now().Add(time.Minute)), "a shorter block of the same sessions")
	assert.Equal(t, len(ids), sessionstore.EntryCount(s))

	clk.Advance(30 * time.Minute)
	for _, id := range []string{ids[0], ids[500], ids[1202]} {
		blocked, err := s.IsSessionBlocked(ctx, id)
		require.NoError(t, err)
		assert.True(t, blocked, id)
	}
	require.NoError(t, s.BlockSessions(ctx, nil, clk.Now().Add(time.Hour)), "an empty slice is a no-op")
	assert.Equal(t, len(ids), sessionstore.EntryCount(s))
}
