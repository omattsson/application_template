package handlers

import (
	"net/http"
	"strconv"
	"testing"

	"backend/internal/api/middleware"
	"backend/internal/models"
	"backend/internal/sessionstore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func itoa(id uint) string { return strconv.FormatUint(uint64(id), 10) }

// TestAuditRecordsItemsAndAPIKeys runs the audit middleware in front of the
// real item and API key handlers: a mutating request writes one row with the
// user, the entity and its ID, and no secret.
func TestAuditRecordsItemsAndAPIKeys(t *testing.T) {
	t.Parallel()
	e := newAdminEnv(t)
	user := e.bearer(t, idUserU)

	w := e.do(http.MethodPost, "/api/v1/items", `{"name":"widget","price":9.5}`, user)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	created := decodeInto[models.Item](t, w)
	require.NotZero(t, created.ID)
	itemID := itoa(created.ID)

	w = e.do(http.MethodPut, "/api/v1/items/"+itemID, `{"name":"widget2","price":10,"version":1}`, user)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, http.StatusNotFound, e.do(http.MethodPut, "/api/v1/items/99999", `{"name":"x","price":1,"version":1}`, user).Code, "a failed request is not audited")
	require.Equal(t, http.StatusNoContent, e.do(http.MethodDelete, "/api/v1/items/"+itemID, "", user).Code)

	w = e.do(http.MethodPost, "/api/v1/api-keys", `{"name":"ci","expires_in_days":30}`, user)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	key := decodeInto[CreateAPIKeyResponse](t, w)
	require.Equal(t, http.StatusNoContent, e.do(http.MethodDelete, "/api/v1/api-keys/"+key.ID, "", user).Code)

	// An admin deletes another user's key: the owner is in the details.
	_, otherKey := e.newKey(t, idUserV)
	require.Equal(t, http.StatusNoContent, e.do(http.MethodDelete, "/api/v1/api-keys/"+otherKey, "", e.bearer(t, idAdminA)).Code)

	rows := e.auditRows(t)
	type row struct{ action, entity, id string }
	var got []row
	for _, r := range rows {
		got = append(got, row{r.Action, r.EntityType, r.EntityID})
		assert.NotContains(t, r.Details, key.RawKey)
		assert.NotContains(t, r.Details, "widget")
	}
	assert.Equal(t, []row{
		{middleware.AuditActionCreate, middleware.AuditEntityItem, itemID},
		{middleware.AuditActionUpdate, middleware.AuditEntityItem, itemID},
		{middleware.AuditActionDelete, middleware.AuditEntityItem, itemID},
		{middleware.AuditActionCreate, middleware.AuditEntityAPIKey, key.ID},
		{middleware.AuditActionDelete, middleware.AuditEntityAPIKey, key.ID},
		{middleware.AuditActionDelete, middleware.AuditEntityAPIKey, otherKey},
	}, got)
	assert.Equal(t, idUserU, rows[0].UserID)
	assert.Equal(t, "user-u", rows[0].Username)
	assert.Contains(t, rows[5].Details, `"owner_id":"`+idUserV+`"`)
	assert.Equal(t, idAdminA, rows[5].UserID)
}

// TestAuditRecordsACommittedChangeThatEndedInAnError: the role change
// committed, the revocation failed, the answer is 500, and the entry exists.
func TestAuditRecordsACommittedChangeThatEndedInAnError(t *testing.T) {
	t.Parallel()
	e := newAdminEnv(t, adminOpt{store: func(s sessionstore.SessionStore) sessionstore.SessionStore {
		return &faultyStore{SessionStore: s, blockErr: assert.AnError}
	}})
	w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU+"/role", `{"role":"devops"}`)
	require.Equal(t, http.StatusInternalServerError, w.Code)

	rows := e.auditRows(t)
	require.Len(t, rows, 1)
	assert.Equal(t, middleware.AuditActionChangeRole, rows[0].Action)
	assert.Equal(t, idUserU, rows[0].EntityID)
	assert.Contains(t, rows[0].Details, `"revocation_failed":true`)
	assert.Contains(t, rows[0].Details, `"status":500`)
}
