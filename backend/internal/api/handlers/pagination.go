package handlers

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

// List endpoints take page (from 1) and pageSize. The page size defaults to
// listPageSizeDefault and is clamped to listPageSizeMax; an invalid value
// (not a number, zero, negative) is the default, not an error.
const (
	listPageSizeDefault = 25
	listPageSizeMax     = 100
	// listPageMax bounds page, so (page-1)*pageSize cannot overflow.
	listPageMax = 1_000_000
)

// listPagination reads the page and pageSize query parameters and returns
// them with the row offset of the page.
func listPagination(c *gin.Context) (page, pageSize, offset int) {
	page, pageSize = 1, listPageSizeDefault
	if v, err := strconv.Atoi(c.Query("pageSize")); err == nil && v > 0 {
		pageSize = min(v, listPageSizeMax)
	}
	if v, err := strconv.Atoi(c.Query("page")); err == nil && v > 0 {
		page = min(v, listPageMax)
	}
	return page, pageSize, (page - 1) * pageSize
}
