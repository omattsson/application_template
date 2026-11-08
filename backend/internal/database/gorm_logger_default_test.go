package database

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// setDefaultRecorder installs a recording logger as slog.Default() for the
// test. These tests are not parallel: they swap the process default.
func setDefaultRecorder(t *testing.T, level slog.Level) *recordingHandler {
	t.Helper()
	prev := slog.Default()
	log, h := newRecordingLogger(level)
	slog.SetDefault(log)
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

func TestGormLoggerRedactsValuesWhenNotDebug(t *testing.T) {
	dup := &mysqldriver.MySQLError{Number: 1062, Message: "Duplicate entry 'secret-user' for key 'users.idx_users_username'"}
	badValue := &mysqldriver.MySQLError{Number: 1366, Message: "Incorrect integer value: 'secret-value' for column 'qty' at row 1"}

	tests := []struct {
		name    string
		debug   bool
		err     error
		wantErr []string
		denyErr []string
		wantSQL string
		denySQL string
	}{
		{name: "release: placeholders, no values in the error", err: badValue, wantErr: []string{"Error 1366", "qty"}, denyErr: []string{"secret-value"}, wantSQL: "name = ?", denySQL: "secret-sql"},
		{name: "release: duplicate keeps the key name", err: dup, wantErr: []string{"Error 1062", "users.idx_users_username"}, denyErr: []string{"secret-user"}, wantSQL: "name = ?", denySQL: "secret-sql"},
		{name: "debug: values stay", debug: true, err: badValue, wantErr: []string{"secret-value"}, wantSQL: "secret-sql"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := setDefaultRecorder(t, slog.LevelDebug)
			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{SkipDefaultTransaction: true, Logger: newGormLogger(tt.debug)})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = sqlDB.Close() })

			// A real statement with a value that fails with a MySQL error.
			require.NoError(t, db.Callback().Query().After("gorm:query").Register("test:inject", func(tx *gorm.DB) {
				_ = tx.AddError(tt.err)
			}))
			var out []map[string]any
			_ = db.Table("missing").Where("name = ?", "secret-sql").Find(&out).Error

			var found *slog.Record
			for _, r := range h.all() {
				if _, ok := recordAttrs(r)["error"]; ok {
					r := r
					found = &r
				}
			}
			require.NotNil(t, found, "an error line is logged")
			attrs := recordAttrs(*found)
			for _, s := range tt.wantErr {
				assert.Contains(t, attrs["error"].String(), s)
			}
			for _, s := range tt.denyErr {
				assert.NotContains(t, attrs["error"].String(), s)
			}
			assert.Contains(t, attrs["sql"].String(), tt.wantSQL)
			if tt.denySQL != "" {
				assert.NotContains(t, attrs["sql"].String(), tt.denySQL)
			}
		})
	}
}

func TestGormLoggerDuplicateKeyIsWarn(t *testing.T) {
	h := setDefaultRecorder(t, slog.LevelDebug)
	dup := &mysqldriver.MySQLError{Number: 1062, Message: "Duplicate entry 'x' for key 'items.name'"}

	for _, debug := range []bool{false, true} {
		newGormLogger(debug).Trace(context.Background(), time.Now(), func() (string, int64) { return "INSERT INTO items (name) VALUES (?)", 0 }, dup)
	}

	records := h.all()
	require.Len(t, records, 2)
	for _, r := range records {
		assert.Equal(t, slog.LevelWarn, r.Level)
	}
}

func TestGormLoggerCancelledQueryIsDebug(t *testing.T) {
	h := setDefaultRecorder(t, slog.LevelDebug)

	for _, debug := range []bool{false, true} {
		newGormLogger(debug).Trace(context.Background(), time.Now(), func() (string, int64) { return "SELECT 1", 0 }, context.Canceled)
	}

	records := h.all()
	require.Len(t, records, 2)
	for _, r := range records {
		assert.Equal(t, slog.LevelDebug, r.Level)
	}
}

func TestGormLoggerRecordNotFoundIsNotLogged(t *testing.T) {
	h := setDefaultRecorder(t, slog.LevelDebug)

	newGormLogger(false).Trace(context.Background(), time.Now(), func() (string, int64) { return "SELECT 1", 0 }, gorm.ErrRecordNotFound)
	newGormLogger(false).Trace(context.Background(), time.Now(), func() (string, int64) { return "SELECT 1", 0 }, fmt.Errorf("find: %w", gorm.ErrRecordNotFound))

	assert.Empty(t, h.all())
}

// The logger reads slog.Default() at each call: a later SetDefault applies,
// and the logger never replaces or wraps a handler.
func TestGormLoggerReadsDefaultAtEachCall(t *testing.T) {
	l := newGormLogger(false)
	h1 := setDefaultRecorder(t, slog.LevelDebug)
	l.Trace(context.Background(), time.Now(), func() (string, int64) { return "SELECT 1", 0 }, errors.New("boom"))
	h2 := setDefaultRecorder(t, slog.LevelDebug)
	l.Trace(context.Background(), time.Now(), func() (string, int64) { return "SELECT 2", 0 }, errors.New("boom"))

	assert.Len(t, h1.all(), 1)
	assert.Len(t, h2.all(), 1)
}
