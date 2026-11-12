package database

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupAuditLogRepo(t *testing.T) *GORMAuditLogRepository {
	t.Helper()
	return NewGORMAuditLogRepository(openMigratedDB(t))
}

func TestAuditLogRepositoryCreate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		log     models.AuditLog
		wantErr error
	}{
		{name: "success", log: models.AuditLog{UserID: "u1", Username: "alice", Action: "create", EntityType: "item", EntityID: "1", Details: `{"a":1}`}},
		{name: "no user", log: models.AuditLog{Action: "create", EntityType: "item"}, wantErr: dberrors.ErrValidation},
		{name: "no action", log: models.AuditLog{UserID: "u1", EntityType: "item"}, wantErr: dberrors.ErrValidation},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := setupAuditLogRepo(t)
			l := tt.log
			err := repo.Create(context.Background(), &l)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.NotEmpty(t, l.ID)
			assert.False(t, l.Timestamp.IsZero())
		})
	}
}

// seedAuditLogs writes n logs, one second apart and newest last, alternating
// the users u1/u2 and the actions create/delete.
func seedAuditLogs(t *testing.T, repo *GORMAuditLogRepository, base time.Time, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		action, user := "create", "u1"
		if i%2 == 1 {
			action, user = "delete", "u2"
		}
		require.NoError(t, repo.Create(context.Background(), &models.AuditLog{
			ID:         fmt.Sprintf("log-%02d", i),
			UserID:     user,
			Username:   user,
			Action:     action,
			EntityType: "item",
			EntityID:   strconv.Itoa(i % 3),
			Timestamp:  base.Add(time.Duration(i) * time.Second),
		}))
	}
}

func logIDs(logs []models.AuditLog) []string {
	ids := make([]string, 0, len(logs))
	for _, l := range logs {
		ids = append(ids, l.ID)
	}
	return ids
}

func TestAuditLogRepositoryList(t *testing.T) {
	t.Parallel()
	repo := setupAuditLogRepo(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	seedAuditLogs(t, repo, base, 6)
	start, end := base.Add(2*time.Second), base.Add(4*time.Second)

	tests := []struct {
		name      string
		filters   models.AuditLogFilters
		wantIDs   []string
		wantTotal int64
	}{
		{name: "all, newest first", wantIDs: []string{"log-05", "log-04", "log-03", "log-02", "log-01", "log-00"}, wantTotal: 6},
		{name: "action", filters: models.AuditLogFilters{Action: "delete"}, wantIDs: []string{"log-05", "log-03", "log-01"}, wantTotal: 3},
		{name: "user", filters: models.AuditLogFilters{UserID: "u1"}, wantIDs: []string{"log-04", "log-02", "log-00"}, wantTotal: 3},
		{name: "entity", filters: models.AuditLogFilters{EntityType: "item", EntityID: "0"}, wantIDs: []string{"log-03", "log-00"}, wantTotal: 2},
		{name: "date range", filters: models.AuditLogFilters{StartDate: &start, EndDate: &end}, wantIDs: []string{"log-04", "log-03", "log-02"}, wantTotal: 3},
		{name: "offset page", filters: models.AuditLogFilters{Limit: 2, Offset: 2}, wantIDs: []string{"log-03", "log-02"}, wantTotal: 6},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := repo.List(context.Background(), tt.filters)
			require.NoError(t, err)
			assert.Equal(t, tt.wantIDs, logIDs(res.Data))
			assert.Equal(t, tt.wantTotal, res.Total)
			if tt.name == "offset page" {
				assert.Equal(t, encodeAuditCursor(res.Data[1].Timestamp, res.Data[1].ID), res.NextCursor, "a full page with rows after it has a cursor")
			} else {
				assert.Empty(t, res.NextCursor)
			}
		})
	}
}

// The next cursor comes from the rows themselves (one row more than the page
// is read), never from the total: the MySQL table statistics are an estimate
// that can lie below the real count, and a full page must still get its
// cursor.
func TestAuditLogRepositoryListCursorIgnoresTheEstimate(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		estimate   int64 // what the table statistics say; <= 0: no estimate
		n          int   // real rows
		limit      int
		offset     int
		wantLen    int
		wantCursor bool
		wantTotal  int64
	}{
		{name: "estimate below the real count, full first page", estimate: 2, n: 6, limit: 3, wantLen: 3, wantCursor: true, wantTotal: 2},
		{name: "estimate of zero rows, full first page", estimate: 1, n: 6, limit: 2, wantLen: 2, wantCursor: true, wantTotal: 1},
		{name: "estimate above the real count, last page is full", estimate: 100, n: 6, limit: 6, wantLen: 6, wantCursor: false, wantTotal: 100},
		{name: "estimate above, page of exactly the rest", estimate: 100, n: 6, limit: 3, offset: 3, wantLen: 3, wantCursor: false, wantTotal: 100},
		{name: "estimate below, second page has more", estimate: 2, n: 7, limit: 3, offset: 3, wantLen: 3, wantCursor: true, wantTotal: 2},
		{name: "exact count, full page with a row after it", estimate: 0, n: 4, limit: 3, wantLen: 3, wantCursor: true, wantTotal: 4},
		{name: "exact count, last full page", estimate: 0, n: 3, limit: 3, wantLen: 3, wantCursor: false, wantTotal: 3},
		{name: "short page", estimate: 0, n: 2, limit: 3, wantLen: 2, wantCursor: false, wantTotal: 2},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := setupAuditLogRepo(t)
			seedAuditLogs(t, repo, base, tt.n)
			if tt.estimate > 0 {
				repo.estimate = func(_ context.Context, total *int64) bool { *total = tt.estimate; return true }
			}

			res, err := repo.List(context.Background(), models.AuditLogFilters{Limit: tt.limit, Offset: tt.offset})

			require.NoError(t, err)
			require.Len(t, res.Data, tt.wantLen)
			assert.Equal(t, tt.wantTotal, res.Total, "Total keeps the estimate")
			if tt.wantCursor {
				assert.Equal(t, encodeAuditCursor(res.Data[len(res.Data)-1].Timestamp, res.Data[len(res.Data)-1].ID), res.NextCursor)
			} else {
				assert.Empty(t, res.NextCursor)
			}
		})
	}
}

func TestAuditLogRepositoryListLimits(t *testing.T) {
	t.Parallel()
	repo := setupAuditLogRepo(t)
	seedAuditLogs(t, repo, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), models.MaxPageSize+5)

	tests := []struct {
		name  string
		limit int
		want  int
	}{
		{name: "default", limit: 0, want: models.DefaultPageSize},
		{name: "negative", limit: -3, want: models.DefaultPageSize},
		{name: "capped", limit: models.MaxPageSize + 50, want: models.MaxPageSize},
		{name: "given", limit: 7, want: 7},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := repo.List(context.Background(), models.AuditLogFilters{Limit: tt.limit})
			require.NoError(t, err)
			assert.Len(t, res.Data, tt.want)
		})
	}
}

func TestAuditLogRepositoryListCursor(t *testing.T) {
	t.Parallel()
	repo := setupAuditLogRepo(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	seedAuditLogs(t, repo, base, 5)
	// Two logs at one timestamp: the ID breaks the tie.
	require.NoError(t, repo.Create(context.Background(), &models.AuditLog{ID: "log-04b", UserID: "u1", Action: "create", EntityType: "item", Timestamp: base.Add(4 * time.Second)}))

	first, err := repo.List(context.Background(), models.AuditLogFilters{Limit: 2})
	require.NoError(t, err)
	assert.Equal(t, []string{"log-04b", "log-04"}, logIDs(first.Data))

	var seen []string
	cursor := encodeAuditCursor(first.Data[1].Timestamp, first.Data[1].ID)
	for cursor != "" {
		page, err := repo.List(context.Background(), models.AuditLogFilters{Limit: 2, Cursor: cursor})
		require.NoError(t, err)
		assert.Equal(t, int64(-1), page.Total, "a cursor page has no total")
		seen = append(seen, logIDs(page.Data)...)
		cursor = page.NextCursor
	}
	assert.Equal(t, []string{"log-03", "log-02", "log-01", "log-00"}, seen)

	for _, bad := range []string{"%%%", "bm8tc2VwYXJhdG9y", "bm90LWEtdGltZXxpZA"} {
		_, err := repo.List(context.Background(), models.AuditLogFilters{Cursor: bad})
		require.ErrorIs(t, err, dberrors.ErrValidation, "cursor %q", bad)
	}
}
