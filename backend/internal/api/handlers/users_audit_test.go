package handlers

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"backend/internal/api/middleware"
	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// UpdateUser writes the profile and the password in separate statements. A
// change that committed is in the audit log although the request ends in an
// error afterwards (the middleware records only below 400 otherwise).
func TestUpdateUserAuditsACommittedChangeThatEndsInAnError(t *testing.T) {
	t.Parallel()

	t.Run("profile commits, then the password write fails", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t, adminOpt{users: func(r models.UserRepository) models.UserRepository {
			return &userRepoWrap{UserRepository: r, passErr: errors.New("Error 1213: deadlock")}
		}})

		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserV, `{"display_name":"Renamed","password":"Brand-New-Pass-1"}`)

		require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
		assert.Equal(t, msgInternalServerError, errorOf(t, w))
		assert.Equal(t, "Renamed", e.user(t, idUserV).DisplayName, "the profile change committed")
		rows := e.auditRows(t)
		require.Len(t, rows, 1, "exactly one audit row")
		assert.Equal(t, middleware.AuditActionUpdate, rows[0].Action)
		assert.Equal(t, middleware.AuditEntityUser, rows[0].EntityType)
		assert.Equal(t, idUserV, rows[0].EntityID)
		assert.Contains(t, rows[0].Details, `"status":500`)
		assert.NotContains(t, rows[0].Details, "password_changed", "the password write failed")
	})

	t.Run("password commits, then the final read fails", func(t *testing.T) {
		t.Parallel()
		var reads atomic.Int32
		e := newAdminEnv(t, adminOpt{users: func(r models.UserRepository) models.UserRepository {
			return &userRepoWrap{UserRepository: r, findErr: func(_ context.Context, id string) error {
				// The 1st read of the target finds it, the 2nd is the final read.
				if id == idUserV && reads.Add(1) == 2 {
					return errors.New("dial tcp 10.1.2.3:3306: refused")
				}
				return nil
			}}
		}})

		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserV, `{"password":"Brand-New-Pass-1"}`)

		require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
		assert.Equal(t, msgInternalServerError, errorOf(t, w))
		assert.NotContains(t, w.Body.String(), "dial tcp")
		rows := e.auditRows(t)
		require.Len(t, rows, 1, "exactly one audit row")
		assert.Equal(t, idUserV, rows[0].EntityID)
		assert.Contains(t, rows[0].Details, `"password_changed":true`)
		assert.Contains(t, rows[0].Details, `"status":500`)
	})

	t.Run("nothing committed, nothing recorded", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t, adminOpt{users: func(r models.UserRepository) models.UserRepository {
			return &userRepoWrap{UserRepository: r, profileErr: errors.New("Error 1213: deadlock")}
		}})

		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserV, `{"display_name":"x","password":"Brand-New-Pass-1"}`)

		require.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Empty(t, e.auditRows(t))
	})
}
