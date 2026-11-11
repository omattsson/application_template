package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xeipuuv/gojsonschema"
)

var auditSeedBase = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// seedAudit writes n rows, one minute apart (newest last): user A on even
// rows and user B on odd rows; actions create, update, delete in turn; entity
// type item (user for every third row).
func seedAudit(t *testing.T, e *adminEnv, n int) {
	t.Helper()
	actions := []string{"create", "update", "delete"}
	for i := 0; i < n; i++ {
		user := idAdminA
		if i%2 == 1 {
			user = idAdminB
		}
		entity := "item"
		if i%3 == 2 {
			entity = "user"
		}
		require.NoError(t, e.audit.Create(context.Background(), &models.AuditLog{
			UserID: user, Username: "u" + strconv.Itoa(i), Action: actions[i%3], EntityType: entity,
			EntityID: "e" + strconv.Itoa(i), Details: `{"method":"POST"}`,
			Timestamp: auditSeedBase.Add(time.Duration(i) * time.Minute),
		}))
	}
}

func listAudit(t *testing.T, e *adminEnv, query string) *httptest.ResponseRecorder {
	t.Helper()
	return e.asAdmin(t, http.MethodGet, "/api/v1/audit-logs"+query, "")
}

func TestListAuditLogsFilters(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		query url.Values
		want  int
	}{
		{"all", url.Values{}, 12},
		{"by user", url.Values{"user_id": {idAdminA}}, 6},
		{"by action", url.Values{"action": {"create"}}, 4},
		{"by entity type", url.Values{"entity_type": {"user"}}, 4},
		{"by entity id", url.Values{"entity_id": {"e5"}}, 1},
		{"user and action", url.Values{"user_id": {idAdminA}, "action": {"update"}}, 2},
		{"from", url.Values{"start_date": {auditSeedBase.Add(10 * time.Minute).Format(time.RFC3339)}}, 2},
		{"until", url.Values{"end_date": {auditSeedBase.Add(1 * time.Minute).Format(time.RFC3339)}}, 2},
		{"range", url.Values{
			"start_date": {auditSeedBase.Add(2 * time.Minute).Format(time.RFC3339)},
			"end_date":   {auditSeedBase.Add(4 * time.Minute).Format(time.RFC3339)},
		}, 3},
		{"range with an offset in the zone", url.Values{
			"start_date": {auditSeedBase.Add(2 * time.Minute).In(time.FixedZone("x", 2*3600)).Format(time.RFC3339)},
			"end_date":   {auditSeedBase.Add(4 * time.Minute).In(time.FixedZone("x", -5*3600)).Format(time.RFC3339)},
		}, 3},
		{"fractional seconds", url.Values{"start_date": {auditSeedBase.Add(11 * time.Minute).Format(time.RFC3339Nano)}}, 1},
		{"no match", url.Values{"entity_id": {"nothing"}}, 0},
		{"sql meta characters are not a filter", url.Values{"action": {"create' OR '1'='1"}}, -1},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newAdminEnv(t)
			seedAudit(t, e, 12)
			w := listAudit(t, e, "?"+tt.query.Encode()+"&limit=100")
			if tt.want < 0 {
				assert.Equal(t, http.StatusBadRequest, w.Code)
				return
			}
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			resp := decodeInto[models.PaginatedAuditLogs](t, w)
			assert.Len(t, resp.Data, tt.want)
			assert.EqualValues(t, tt.want, resp.Total)
			for i := 1; i < len(resp.Data); i++ {
				assert.True(t, !resp.Data[i-1].Timestamp.Before(resp.Data[i].Timestamp), "newest first")
			}
		})
	}
}

func TestListAuditLogsRejectsInvalidParameters(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 101)
	tests := []struct{ name, query string }{
		{"date without time", "start_date=2026-03-01"},
		{"date with a space", "start_date=2026-03-01%2012:00:00"},
		{"not a date", "end_date=yesterday"},
		{"unix time", "start_date=1772366400"},
		{"empty-ish offset", "end_date=2026-03-01T12:00:00"},
		{"start after end", "start_date=2026-03-02T00:00:00Z&end_date=2026-03-01T00:00:00Z"},
		{"limit not a number", "limit=abc"},
		{"limit negative", "limit=-1"},
		{"limit float", "limit=1.5"},
		{"offset not a number", "offset=x"},
		{"offset negative", "offset=-1"},
		{"action too long", "action=" + long},
		{"action with a quote", "action=a%27b"},
		{"action upper case", "action=Create"},
		{"entity type with a space", "entity_type=a%20b"},
		{"entity type too long", "entity_type=" + long},
		{"entity id too long", "entity_id=" + strings.Repeat("1", 65)},
		{"entity id with a control character", "entity_id=a%0Ab"},
		{"user id too long", "user_id=" + strings.Repeat("0", 37)},
		{"user id with a quote", "user_id=%27"},
		{"cursor too long", "cursor=" + strings.Repeat("A", 513)},
		{"cursor is not a cursor", "cursor=!!!"},
		{"cursor with valid base64 but no content", "cursor=YWJj"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newAdminEnv(t)
			w := listAudit(t, e, "?"+tt.query)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.NotContains(t, w.Body.String(), "SELECT")
		})
	}
}

func TestListAuditLogsPaging(t *testing.T) {
	t.Parallel()

	t.Run("default page size and clamping", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		seedAudit(t, e, 130)
		tests := []struct {
			query     string
			wantLen   int
			wantLimit int
			wantOff   int
		}{
			{"", listPageSizeDefault, listPageSizeDefault, 0},
			{"?limit=0", listPageSizeDefault, listPageSizeDefault, 0},
			{"?limit=10", 10, 10, 0},
			{"?limit=100", 100, 100, 0},
			{"?limit=100000", 100, 100, 0},
			{"?limit=10&offset=125", 5, 10, 125},
			{"?limit=10&offset=999999999", 0, 10, maxAuditOffset},
		}
		for _, tt := range tests {
			w := listAudit(t, e, tt.query)
			require.Equal(t, http.StatusOK, w.Code, tt.query)
			resp := decodeInto[models.PaginatedAuditLogs](t, w)
			assert.Len(t, resp.Data, tt.wantLen, tt.query)
			assert.Equal(t, tt.wantLimit, resp.Limit, tt.query)
			assert.Equal(t, tt.wantOff, resp.Offset, tt.query)
			assert.EqualValues(t, 130, resp.Total, tt.query)
		}
	})

	t.Run("cursor pages cover every row once", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		seedAudit(t, e, 23)
		seen := map[string]bool{}
		cursor := ""
		pages := 0
		for {
			q := "?limit=10"
			if cursor != "" {
				q += "&cursor=" + url.QueryEscape(cursor)
			}
			w := listAudit(t, e, q)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			resp := decodeInto[models.PaginatedAuditLogs](t, w)
			for _, l := range resp.Data {
				assert.False(t, seen[l.ID], "no row twice")
				seen[l.ID] = true
			}
			pages++
			require.LessOrEqual(t, pages, 5)
			if resp.NextCursor == "" {
				break
			}
			cursor = resp.NextCursor
		}
		assert.Len(t, seen, 23)
		assert.Equal(t, 3, pages)
	})

	t.Run("response matches the schema and carries only audit fields", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		seedAudit(t, e, 3)
		w := listAudit(t, e, "")
		schema := gojsonschema.NewStringLoader(`{
			"type":"object","required":["data","total","limit","offset"],"additionalProperties":false,
			"properties":{"total":{"type":"integer"},"limit":{"type":"integer"},"offset":{"type":"integer"},"next_cursor":{"type":"string"},
				"data":{"type":"array","items":{"type":"object","additionalProperties":false,
					"required":["id","timestamp","user_id","username","action","entity_type","entity_id","details"],
					"properties":{"id":{"type":"string"},"timestamp":{"type":"string"},"user_id":{"type":"string"},"username":{"type":"string"},
						"action":{"type":"string"},"entity_type":{"type":"string"},"entity_id":{"type":"string"},"details":{"type":"string"}}}}}}`)
		res, err := gojsonschema.Validate(schema, gojsonschema.NewBytesLoader(w.Body.Bytes()))
		require.NoError(t, err)
		assert.True(t, res.Valid(), "%v", res.Errors())
		assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	})

	t.Run("an empty log is an empty list, not null", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		w := listAudit(t, e, "")
		require.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), `"data":[]`)
	})
}

func TestListAuditLogsRepositoryErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want int
		msg  string
	}{
		{"database error", errors.New("Error 1045: Access denied for user 'root'@'10.0.0.1'"), http.StatusInternalServerError, msgInternalServerError},
		{"deadline", context.DeadlineExceeded, http.StatusInternalServerError, msgInternalServerError},
		{"cancelled", context.Canceled, statusClientClosedRequest, msgClientClosedRequest},
		{"validation", dberrors.NewDatabaseError("list", dberrors.ErrValidation), http.StatusBadRequest, ""},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newAdminEnv(t, adminOpt{audit: func(r models.AuditLogRepository) models.AuditLogRepository {
				return failingAuditList{AuditLogRepository: r, err: tt.err}
			}})
			w := listAudit(t, e, "")
			assert.Equal(t, tt.want, w.Code, w.Body.String())
			assert.NotContains(t, w.Body.String(), "Access denied")
			if tt.msg != "" {
				assert.Equal(t, tt.msg, errorOf(t, w))
			}
		})
	}
}

type failingAuditList struct {
	models.AuditLogRepository
	err error
}

func (f failingAuditList) List(context.Context, models.AuditLogFilters) (*models.AuditLogResult, error) {
	return nil, f.err
}

func TestListAuditLogsAdminOnlyEvenWithoutRouteGuards(t *testing.T) {
	t.Parallel()
	e := newAdminEnv(t)
	seedAudit(t, e, 2)
	for name, headers := range map[string]map[string]string{
		"none":    nil,
		"user":    e.bearer(t, idUserU),
		"devops":  e.bearer(t, idDevops),
		"stale":   bearerOf(tokenWith(t, idDevops, "devops", models.RoleAdmin)),
		"api key": keyHeader(mustKey(t, e, idAdminA)),
	} {
		w := e.do(http.MethodGet, "/api/v1/audit-logs", "", headers)
		assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, w.Code, name)
		assert.NotContains(t, w.Body.String(), "e0", name)
	}
	assert.Equal(t, http.StatusOK, e.asAdmin(t, http.MethodGet, "/api/v1/audit-logs", "").Code)
}

func mustKey(t *testing.T, e *adminEnv, userID string) string {
	t.Helper()
	raw, _ := e.newKey(t, userID)
	return raw
}

func TestAuditEndToEndFromTheAdminAPI(t *testing.T) {
	t.Parallel()
	e := newAdminEnv(t)
	require.Equal(t, http.StatusCreated, e.asAdmin(t, http.MethodPost, "/api/v1/users", `{"username":"audited","password":"Correct-Horse-9","role":"user"}`).Code)
	require.Equal(t, http.StatusOK, e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU+"/role", `{"role":"devops"}`).Code)
	// The audit recorder writes asynchronously.
	require.Eventually(t, func() bool {
		w := listAudit(t, e, "?entity_type=user")
		return w.Code == http.StatusOK && len(decodeInto[models.PaginatedAuditLogs](t, w).Data) == 2
	}, 5*time.Second, 10*time.Millisecond)

	w := listAudit(t, e, "?action=change_role&entity_id="+idUserU+"&user_id="+idAdminA)
	resp := decodeInto[models.PaginatedAuditLogs](t, w)
	require.Len(t, resp.Data, 1)
	assert.Equal(t, "admin-a", resp.Data[0].Username)
	assert.NotContains(t, w.Body.String(), "Correct-Horse")
}
