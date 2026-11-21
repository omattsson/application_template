package handlers

import (
	"log/slog"
	"net/http"
	"strconv"

	"backend/internal/api/middleware"
	"backend/internal/models"

	"github.com/gin-gonic/gin"
)

// Bounds of the job list.
const (
	// maxJobsOffset is the deepest offset a page can start at: an offset scan
	// costs the database every skipped row.
	maxJobsOffset = 100_000

	msgInvalidJobStatus = "Invalid status parameter"
	msgInvalidJobType   = "Invalid type parameter"
	msgInvalidLimit     = "Invalid limit parameter"
	msgInvalidOffset    = "Invalid offset parameter"
)

// JobHandler serves the admin list of the optional job module
// (JOBS_ENABLED=true). It only reads: a job is enqueued through
// jobs.Queue by the handler of the feature that needs it.
type JobHandler struct {
	repo models.JobRepository
}

// NewJobHandler creates a JobHandler on repo.
func NewJobHandler(repo models.JobRepository) *JobHandler {
	return &JobHandler{repo: repo}
}

// JobListResponse is one page of jobs.
type JobListResponse struct {
	Data   []models.Job `json:"data"`
	Total  int64        `json:"total"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
}

// ListJobs godoc
// @Summary      List background jobs
// @Description  Returns one page of jobs of the queue (optional module, JOBS_ENABLED=true; the route does not exist otherwise), newest first unless sort and order say otherwise. Rows with an equal sort value are ordered by id in the same direction, so pages never repeat or skip a row. Never the payload: a job shows its type, status, attempts, schedule, the worker that holds it and the text of the last error. Admin only with AUTH_ENABLED=true. A read: no audit entry.
// @Tags         jobs
// @Produce      json
// @Security     BearerAuth
// @Security     ApiKeyAuth
// @Param        limit   query  int     false  "Page size (default 25, at most 100)"
// @Param        offset  query  int     false  "Rows to skip (default 0, at most 100000)"
// @Param        status  query  string  false  "Status" Enums(pending, running, succeeded, dead)
// @Param        type    query  string  false  "Job type (exact, at most 100 characters)"
// @Param        sort    query  string  false  "Sort key (default id)" Enums(created_at, run_at, id)
// @Param        order   query  string  false  "Sort direction (default desc)" Enums(asc, desc)
// @Success      200  {object}  JobListResponse
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string "Authentication required (AUTH_ENABLED=true)"
// @Failure      403  {object}  map[string]string
// @Failure      499  {object}  map[string]string "Client closed request"
// @Failure      500  {object}  map[string]string
// @Router       /api/v1/jobs [get]
func (h *JobHandler) ListJobs(c *gin.Context) {
	limit, offset, ok := parseJobPaging(c)
	if !ok {
		return
	}

	filter := models.JobFilter{Status: c.Query("status"), Type: c.Query("type")}
	if filter.Status != "" && !models.ValidJobStatus(filter.Status) {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidJobStatus})
		return
	}
	if len(filter.Type) > models.JobTypeMaxLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidJobType})
		return
	}

	var asc bool
	if filter.SortBy, asc, ok = parseJobSort(c); !ok {
		return
	}
	filter.SortDesc = !asc

	jobs, total, err := h.repo.ListPaged(c.Request.Context(), filter, limit, offset)
	if err != nil {
		status, message := mapError(err, "Job")
		if status >= http.StatusInternalServerError {
			slog.ErrorContext(c.Request.Context(), "Failed to list jobs", "error", err, "request_id", c.GetString(middleware.RequestIDKey))
		}
		c.JSON(status, gin.H{"error": message})
		return
	}
	if jobs == nil {
		jobs = []models.Job{}
	}

	noStore(c) // last_error may hold operational detail
	c.JSON(http.StatusOK, JobListResponse{Data: jobs, Total: total, Limit: limit, Offset: offset})
}

// parseJobPaging reads limit (default models.DefaultPageSize, at most
// models.MaxPageSize) and offset (at most maxJobsOffset). A value that is not
// a number, or is not positive (limit) or negative (offset), is 400.
func parseJobPaging(c *gin.Context) (limit, offset int, ok bool) {
	limit = models.DefaultPageSize

	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidLimit})
			return 0, 0, false
		}
		limit = min(n, models.MaxPageSize)
	}

	if v := c.Query("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidOffset})
			return 0, 0, false
		}
		offset = min(n, maxJobsOffset)
	}

	return limit, offset, true
}

// parseJobSort reads sort and order with parseSort. The default differs from
// the other lists: without sort the key is id, and without order the
// direction is descending (newest first). asc reports an explicit "asc".
func parseJobSort(c *gin.Context) (key string, asc, ok bool) {
	key, desc, ok := parseSort(c, models.JobSortKeys)
	if !ok {
		return "", false, false
	}
	if key == "" {
		key = "id"
	}

	return key, c.Query("order") != "" && !desc, true
}
