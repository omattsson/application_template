package handlers

import (
	"net/http"
	"slices"

	"github.com/gin-gonic/gin"
)

// List endpoints take sort (a key of the entity's whitelist) and order (asc
// or desc). Both are optional: without them the endpoint keeps its default
// order, and order alone reverses the default column. A value is matched
// exactly (lower case); an empty value counts as absent. The repository maps
// the key to a column through its own whitelist, so a request value never
// reaches SQL.
const (
	msgInvalidSort  = "Invalid sort parameter"
	msgInvalidOrder = "Invalid order parameter"

	sortOrderAsc  = "asc"
	sortOrderDesc = "desc"
)

// parseSort reads the sort and order query parameters. keys is the whitelist
// of the endpoint. It returns the key ("" when the caller gave none) and
// whether the order is descending. For a value that is not allowed it answers
// 400 and returns ok false: the caller returns at once.
func parseSort(c *gin.Context, keys []string) (key string, desc, ok bool) {
	key = c.Query("sort")
	if key != "" && !slices.Contains(keys, key) {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidSort})
		return "", false, false
	}
	switch c.Query("order") {
	case "", sortOrderAsc:
	case sortOrderDesc:
		desc = true
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": msgInvalidOrder})
		return "", false, false
	}
	return key, desc, true
}
