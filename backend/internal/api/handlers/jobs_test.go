package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xeipuuv/gojsonschema"
)

// fakeJobRepo is a job repository that records the arguments of ListPaged.
type fakeJobRepo struct {
	models.JobRepository
	err    error
	jobs   []models.Job
	filter models.JobFilter
	limit  int
	offset int
	total  int64
	calls  int
}

func (f *fakeJobRepo) ListPaged(_ context.Context, filter models.JobFilter, limit, offset int) ([]models.Job, int64, error) {
	f.calls++
	f.filter, f.limit, f.offset = filter, limit, offset
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.jobs, f.total, nil
}

func jobsRouter(repo models.JobRepository) *gin.Engine {
	r := gin.New()
	r.GET("/api/v1/jobs", NewJobHandler(repo).ListJobs)
	return r
}

func getJobs(r http.Handler, query string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs"+query, nil)
	r.ServeHTTP(w, req)
	return w
}

func TestListJobs(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	jobs := []models.Job{
		{ID: 2, Type: "email.send", Status: models.JobStatusDead, Attempts: 5, MaxAttempts: 5, LastError: "smtp down",
			Payload: "secret-payload", RunAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: 1, Type: "email.send", Status: models.JobStatusPending, MaxAttempts: 5, RunAt: now, CreatedAt: now, UpdatedAt: now},
	}

	t.Run("a page without the payload, newest first by default", func(t *testing.T) {
		t.Parallel()
		repo := &fakeJobRepo{jobs: jobs, total: 2}
		w := getJobs(jobsRouter(repo), "")

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		assert.Equal(t, models.JobFilter{SortBy: "id", SortDesc: true}, repo.filter)
		assert.Equal(t, 25, repo.limit)
		assert.Equal(t, 0, repo.offset)
		assert.NotContains(t, w.Body.String(), "secret-payload")
		assert.NotContains(t, w.Body.String(), "payload")

		res, err := gojsonschema.Validate(gojsonschema.NewStringLoader(jobListSchema), gojsonschema.NewBytesLoader(w.Body.Bytes()))
		require.NoError(t, err)
		assert.True(t, res.Valid(), "%v", res.Errors())

		var body JobListResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, int64(2), body.Total)
		assert.Equal(t, 25, body.Limit)
		require.Len(t, body.Data, 2)
		assert.Equal(t, "smtp down", body.Data[0].LastError)
	})

	t.Run("an empty page is an empty array", func(t *testing.T) {
		t.Parallel()
		w := getJobs(jobsRouter(&fakeJobRepo{}), "")
		require.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), `"data":[]`)
	})

	tests := []struct {
		name       string
		query      string
		wantFilter models.JobFilter
		wantLimit  int
		wantOffset int
	}{
		{"status and type", "?status=dead&type=email.send", models.JobFilter{Status: "dead", Type: "email.send", SortBy: "id", SortDesc: true}, 25, 0},
		{"sort and ascending order", "?sort=run_at&order=asc", models.JobFilter{SortBy: "run_at"}, 25, 0},
		{"sort alone is descending", "?sort=created_at", models.JobFilter{SortBy: "created_at", SortDesc: true}, 25, 0},
		{"order alone", "?order=asc", models.JobFilter{SortBy: "id"}, 25, 0},
		{"limit and offset", "?limit=10&offset=30", models.JobFilter{SortBy: "id", SortDesc: true}, 10, 30},
		{"limit is capped", "?limit=100000", models.JobFilter{SortBy: "id", SortDesc: true}, models.MaxPageSize, 0},
		{"offset is capped", "?offset=99999999", models.JobFilter{SortBy: "id", SortDesc: true}, 25, maxJobsOffset},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := &fakeJobRepo{}
			w := getJobs(jobsRouter(repo), tt.query)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, tt.wantFilter, repo.filter)
			assert.Equal(t, tt.wantLimit, repo.limit)
			assert.Equal(t, tt.wantOffset, repo.offset)
		})
	}

	bad := []struct{ name, query string }{
		{"unknown status", "?status=queued"},
		{"unknown sort", "?sort=payload"},
		{"unknown order", "?order=sideways"},
		{"limit zero", "?limit=0"},
		{"limit text", "?limit=ten"},
		{"limit negative", "?limit=-1"},
		{"offset text", "?offset=x"},
		{"offset negative", "?offset=-1"},
		{"type too long", "?type=" + strings.Repeat("a", models.JobTypeMaxLen+1)},
	}
	for _, tt := range bad {
		tt := tt
		t.Run("bad request: "+tt.name, func(t *testing.T) {
			t.Parallel()
			repo := &fakeJobRepo{}
			w := getJobs(jobsRouter(repo), tt.query)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Zero(t, repo.calls, "nothing reaches the repository")
			res, err := gojsonschema.Validate(gojsonschema.NewStringLoader(errorSchema), gojsonschema.NewBytesLoader(w.Body.Bytes()))
			require.NoError(t, err)
			assert.True(t, res.Valid(), "%v", res.Errors())
		})
	}

	t.Run("a validation error of the repository is 400", func(t *testing.T) {
		t.Parallel()
		repo := &fakeJobRepo{err: dberrors.NewDatabaseError("job list", dberrors.ErrValidation)}
		assert.Equal(t, http.StatusBadRequest, getJobs(jobsRouter(repo), "").Code)
	})

	t.Run("a database error is a generic 500", func(t *testing.T) {
		t.Parallel()
		repo := &fakeJobRepo{err: errors.New("dial tcp 10.0.0.5:3306: connection refused")}
		w := getJobs(jobsRouter(repo), "")
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.JSONEq(t, `{"error":"Internal server error"}`, w.Body.String())
	})

	t.Run("a cancelled request is 499", func(t *testing.T) {
		t.Parallel()
		repo := &fakeJobRepo{err: context.Canceled}
		assert.Equal(t, statusClientClosedRequest, getJobs(jobsRouter(repo), "").Code)
	})
}
