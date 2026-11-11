package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"backend/internal/api/middleware"
	"backend/internal/config"
	"backend/internal/models"
	"backend/internal/sessionstore"
	"backend/internal/websocket"
	"backend/pkg/dberrors"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xeipuuv/gojsonschema"
	"golang.org/x/crypto/bcrypt"
)

// userEndpoints lists every endpoint of the user administration with a valid
// request for the fixture user idUserU.
var userEndpoints = []struct {
	name, method, path, body string
}{
	{"list", http.MethodGet, "/api/v1/users", ""},
	{"get", http.MethodGet, "/api/v1/users/" + idUserV, ""},
	{"create", http.MethodPost, "/api/v1/users", `{"username":"newbie","password":"Correct-Horse-9","role":"user"}`},
	{"update", http.MethodPut, "/api/v1/users/" + idUserV, `{"display_name":"New Name"}`},
	{"delete", http.MethodDelete, "/api/v1/users/" + idUserV, ""},
	{"role", http.MethodPut, "/api/v1/users/" + idUserV + "/role", `{"role":"admin"}`},
	{"disabled", http.MethodPut, "/api/v1/users/" + idUserV + "/disabled", `{"disabled":true}`},
	{"audit logs", http.MethodGet, "/api/v1/audit-logs", ""},
}

// assertNothingChanged checks that the fixture state is the initial one.
func assertNothingChanged(t *testing.T, e *adminEnv) {
	t.Helper()
	v := e.user(t, idUserV)
	assert.Equal(t, models.RoleUser, v.Role)
	assert.False(t, v.Disabled)
	assert.Equal(t, "Display user-v", v.DisplayName)
	_, err := e.users.FindByUsername(context.Background(), "newbie")
	assert.ErrorIs(t, err, dberrors.ErrNotFound)
}

func TestUserAdminAccessControl(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		header func(t *testing.T, e *adminEnv) map[string]string
		want   int
		msg    string
	}{
		{"no credentials", func(*testing.T, *adminEnv) map[string]string { return nil }, http.StatusUnauthorized, ""},
		{"role user", func(t *testing.T, e *adminEnv) map[string]string { return e.bearer(t, idUserU) }, http.StatusForbidden, msgAdminRequired},
		{"role devops", func(t *testing.T, e *adminEnv) map[string]string { return e.bearer(t, idDevops) }, http.StatusForbidden, msgAdminRequired},
		{"api key of an admin", func(t *testing.T, e *adminEnv) map[string]string {
			raw, _ := e.newKey(t, idAdminA)
			return keyHeader(raw)
		}, http.StatusForbidden, msgAdminSessionOnly},
		{"token claims admin but the user is only devops now", func(t *testing.T, e *adminEnv) map[string]string {
			return bearerOf(tokenWith(t, idDevops, "devops", models.RoleAdmin))
		}, http.StatusForbidden, msgAdminRequired},
		{"admin disabled after the token was issued", func(t *testing.T, e *adminEnv) map[string]string {
			require.NoError(t, e.db.Model(&models.User{}).Where("id = ?", idAdminB).Update("disabled", true).Error)
			return bearerOf(tokenWith(t, idAdminB, "admin-b", models.RoleAdmin))
		}, http.StatusForbidden, msgAdminRequired},
		{"admin deleted after the token was issued", func(t *testing.T, e *adminEnv) map[string]string {
			require.NoError(t, e.db.Delete(&models.User{}, "id = ?", idAdminB).Error)
			return bearerOf(tokenWith(t, idAdminB, "admin-b", models.RoleAdmin))
		}, http.StatusUnauthorized, msgAuthRequired},
	}
	for _, tt := range tests {
		for _, ep := range userEndpoints {
			tt, ep := tt, ep
			t.Run(tt.name+"/"+ep.name, func(t *testing.T) {
				t.Parallel()
				e := newAdminEnv(t)
				w := e.do(ep.method, ep.path, ep.body, tt.header(t, e))
				assert.Equal(t, tt.want, w.Code, w.Body.String())
				if tt.msg != "" {
					assert.Equal(t, tt.msg, errorOf(t, w))
				}
				assertNothingChanged(t, e)
				assert.True(t, e.userExists(idUserV))
				assert.Empty(t, e.auditRows(t), "a refused request is not audited")
			})
		}
	}
}

func TestUserAdminHandlersCheckTheCallerThemselves(t *testing.T) {
	t.Parallel()
	// No auth middleware at all in front of the handler: it must still refuse.
	e := newAdminEnv(t)
	w := e.do(http.MethodGet, "/open/users", "", nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// A context with an admin role but no auth method (a wiring mistake)
	// is refused: only a login session counts.
	r := gin.New()
	r.GET("/x", func(c *gin.Context) {
		c.Set("userID", idAdminA)
		c.Set("role", models.RoleAdmin)
		e.handler.ListUsers(c)
	})
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUserAdminCallerLookupFailures(t *testing.T) {
	t.Parallel()
	t.Run("database error is 500 without detail", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t, adminOpt{users: func(r models.UserRepository) models.UserRepository {
			return &userRepoWrap{UserRepository: r, findErr: func(context.Context, string) error { return errors.New("dial tcp 10.1.2.3:3306: refused") }}
		}})
		w := e.asAdmin(t, http.MethodGet, "/api/v1/users", "")
		// The auth middleware does not read the user repository, the handler does.
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Equal(t, msgInternalServerError, errorOf(t, w))
		assert.NotContains(t, w.Body.String(), "dial tcp")
	})
	t.Run("cancelled request is 499", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		e := newAdminEnv(t, adminOpt{users: func(r models.UserRepository) models.UserRepository {
			return &userRepoWrap{UserRepository: r, findErr: func(c context.Context, id string) error {
				if id == idAdminA {
					cancel()
					return c.Err()
				}
				return nil
			}}
		}})
		req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil).WithContext(ctx)
		req.Header.Set("Authorization", e.bearer(t, idAdminA)["Authorization"])
		w := httptest.NewRecorder()
		e.router.ServeHTTP(w, req)
		assert.Equal(t, statusClientClosedRequest, w.Code)
	})
	t.Run("deadline exceeded stays 500", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t, adminOpt{users: func(r models.UserRepository) models.UserRepository {
			return &userRepoWrap{UserRepository: r, findErr: func(context.Context, string) error { return context.DeadlineExceeded }}
		}})
		w := e.asAdmin(t, http.MethodGet, "/api/v1/users", "")
		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})
}

// userRepoWrap wraps a repository and lets a test fail or observe calls.
type userRepoWrap struct {
	models.UserRepository
	findErr    func(ctx context.Context, id string) error
	listErr    error
	createErr  error
	profileErr error
	passErr    error
	roleErr    error
	disableErr error
	deleteErr  error
	// beforeGuarded runs before a guarded change (a test cancels the request
	// or changes the caller there).
	beforeGuarded func()
}

func (w *userRepoWrap) FindByID(ctx context.Context, id string) (*models.User, error) {
	if w.findErr != nil {
		if err := w.findErr(ctx, id); err != nil {
			return nil, err
		}
	}
	return w.UserRepository.FindByID(ctx, id)
}

func (w *userRepoWrap) ListPaged(ctx context.Context, f models.UserFilter, limit, offset int) ([]models.User, int64, error) {
	if w.listErr != nil {
		return nil, 0, w.listErr
	}
	return w.UserRepository.ListPaged(ctx, f, limit, offset)
}

func (w *userRepoWrap) Create(ctx context.Context, u *models.User) error {
	if w.createErr != nil {
		return w.createErr
	}
	return w.UserRepository.Create(ctx, u)
}

func (w *userRepoWrap) UpdateProfile(ctx context.Context, id string, upd models.UserProfileUpdate) error {
	if w.profileErr != nil {
		return w.profileErr
	}
	return w.UserRepository.UpdateProfile(ctx, id, upd)
}

func (w *userRepoWrap) UpdatePassword(ctx context.Context, id, hash string) error {
	if w.passErr != nil {
		return w.passErr
	}
	return w.UserRepository.UpdatePassword(ctx, id, hash)
}

func (w *userRepoWrap) guarded() {
	if w.beforeGuarded != nil {
		w.beforeGuarded()
	}
}

func (w *userRepoWrap) UpdateRole(ctx context.Context, caller, id, role string) (string, error) {
	w.guarded()
	if w.roleErr != nil {
		return "", w.roleErr
	}
	return w.UserRepository.UpdateRole(ctx, caller, id, role)
}

func (w *userRepoWrap) SetDisabled(ctx context.Context, caller, id string, disabled bool) error {
	w.guarded()
	if w.disableErr != nil {
		return w.disableErr
	}
	return w.UserRepository.SetDisabled(ctx, caller, id, disabled)
}

func (w *userRepoWrap) DeleteGuarded(ctx context.Context, caller, id string) error {
	w.guarded()
	if w.deleteErr != nil {
		return w.deleteErr
	}
	return w.UserRepository.DeleteGuarded(ctx, caller, id)
}

func TestListUsers(t *testing.T) {
	t.Parallel()

	t.Run("admin gets a page without password hashes", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		w := e.asAdmin(t, http.MethodGet, "/api/v1/users", "")
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		assert.NotContains(t, w.Body.String(), "password")
		assert.NotContains(t, w.Body.String(), "$2a$")
		resp := decodeInto[UserListResponse](t, w)
		assert.EqualValues(t, 6, resp.Total)
		assert.Len(t, resp.Data, 6)
		assert.Equal(t, 1, resp.Page)
		assert.Equal(t, listPageSizeDefault, resp.PageSize)
		for i := 1; i < len(resp.Data); i++ {
			assert.Less(t, resp.Data[i-1].Username, resp.Data[i].Username, "ordered by username")
		}
		for _, u := range resp.Data {
			assert.Empty(t, u.PasswordHash)
		}
	})

	t.Run("response matches the schema", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		w := e.asAdmin(t, http.MethodGet, "/api/v1/users?pageSize=2", "")
		schema := gojsonschema.NewStringLoader(`{
			"type":"object","required":["data","total","page","pageSize"],"additionalProperties":false,
			"properties":{
				"total":{"type":"integer"},"page":{"type":"integer"},"pageSize":{"type":"integer"},
				"data":{"type":"array","items":{"type":"object","required":["id","username","role","disabled"],
					"properties":{"id":{"type":"string"},"username":{"type":"string"},"role":{"enum":["admin","devops","user"]},"disabled":{"type":"boolean"}}}}}}`)
		res, err := gojsonschema.Validate(schema, gojsonschema.NewBytesLoader(w.Body.Bytes()))
		require.NoError(t, err)
		assert.True(t, res.Valid(), "%v", res.Errors())
	})

	pageTests := []struct {
		name         string
		query        string
		wantLen      int
		wantPage     int
		wantPageSize int
	}{
		{"page size", "?pageSize=2", 2, 1, 2},
		{"second page", "?pageSize=4&page=2", 2, 2, 4},
		{"page size over the maximum is clamped", "?pageSize=100000", 6, 1, listPageSizeMax},
		{"zero page size is the default", "?pageSize=0", 6, 1, listPageSizeDefault},
		{"negative page size is the default", "?pageSize=-5", 6, 1, listPageSizeDefault},
		{"page size that is not a number is the default", "?pageSize=abc", 6, 1, listPageSizeDefault},
		{"negative page is page 1", "?page=-3", 6, 1, listPageSizeDefault},
		{"huge page does not overflow", "?page=99999999999999999999", 6, 1, listPageSizeDefault},
		{"page past the end is empty", "?page=500000", 0, 500000, listPageSizeDefault},
		{"filter by role", "?role=admin", 2, 1, listPageSizeDefault},
		{"search", "?search=oidc", 1, 1, listPageSizeDefault},
		{"search matches like characters literally", "?search=%25", 0, 1, listPageSizeDefault},
	}
	for _, tt := range pageTests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newAdminEnv(t)
			w := e.asAdmin(t, http.MethodGet, "/api/v1/users"+tt.query, "")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			resp := decodeInto[UserListResponse](t, w)
			assert.Len(t, resp.Data, tt.wantLen)
			assert.Equal(t, tt.wantPage, resp.Page)
			assert.Equal(t, tt.wantPageSize, resp.PageSize)
		})
	}

	badTests := []struct{ name, query string }{
		{"unknown role", "?role=root"},
		{"role with wrong case", "?role=Admin"},
		{"search too long", "?search=" + strings.Repeat("a", 101)},
	}
	for _, tt := range badTests {
		tt := tt
		t.Run("bad filter: "+tt.name, func(t *testing.T) {
			t.Parallel()
			e := newAdminEnv(t)
			w := e.asAdmin(t, http.MethodGet, "/api/v1/users"+tt.query, "")
			assert.Equal(t, http.StatusBadRequest, w.Code)
		})
	}

	t.Run("repository error is 500 without detail", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t, adminOpt{users: func(r models.UserRepository) models.UserRepository {
			return &userRepoWrap{UserRepository: r, listErr: errors.New("Error 1045: Access denied for user 'root'")}
		}})
		w := e.asAdmin(t, http.MethodGet, "/api/v1/users", "")
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Equal(t, msgInternalServerError, errorOf(t, w))
	})
}

func TestGetUser(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		id   string
		want int
	}{
		{"found", idUserV, http.StatusOK},
		{"unknown id", idMissing, http.StatusNotFound},
		{"not a uuid", "not-a-uuid", http.StatusBadRequest},
		{"too long", strings.Repeat("a", 37), http.StatusBadRequest},
		{"uuid in another form", "urn:uuid:" + idUserV, http.StatusBadRequest},
		{"sql in the id", "x'%20OR%20'1'='1", http.StatusBadRequest},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newAdminEnv(t)
			w := e.asAdmin(t, http.MethodGet, "/api/v1/users/"+tt.id, "")
			assert.Equal(t, tt.want, w.Code, w.Body.String())
			if tt.want == http.StatusOK {
				u := decodeInto[models.User](t, w)
				assert.Equal(t, idUserV, u.ID)
				assert.NotContains(t, w.Body.String(), "password")
			}
		})
	}
}

func TestCreateUser(t *testing.T) {
	t.Parallel()

	t.Run("creates a local user with the given role and a bcrypt hash", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		w := e.asAdmin(t, http.MethodPost, "/api/v1/users", `{"username":"new.user@x","password":"Correct-Horse-9","display_name":" Newbie ","email":"n@example.com","role":"devops"}`)
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		assert.NotContains(t, w.Body.String(), "$2a$")
		assert.NotContains(t, w.Body.String(), "Correct-Horse")
		created := decodeInto[models.User](t, w)
		assert.Equal(t, models.RoleDevOps, created.Role)
		assert.Equal(t, "Newbie", created.DisplayName)
		assert.Equal(t, models.AuthProviderLocal, created.AuthProvider)

		stored, err := e.users.FindByID(context.Background(), created.ID)
		require.NoError(t, err)
		cost, err := bcrypt.Cost([]byte(stored.PasswordHash))
		require.NoError(t, err)
		assert.Equal(t, passwordHashCost, cost, "the same cost as every other stored hash")
		assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(stored.PasswordHash), []byte("Correct-Horse-9")))
		assert.False(t, stored.Disabled)
		assert.False(t, stored.ServiceAccount)

		// The new user can sign in with the password.
		assert.Equal(t, http.StatusOK, loginAs(t, e, "new.user@x", "Correct-Horse-9"))

		rows := e.auditRows(t)
		require.Len(t, rows, 1)
		assert.Equal(t, middleware.AuditActionCreate, rows[0].Action)
		assert.Equal(t, middleware.AuditEntityUser, rows[0].EntityType)
		assert.Equal(t, created.ID, rows[0].EntityID, "the target user is the entity")
		assert.Equal(t, idAdminA, rows[0].UserID)
		assert.NotContains(t, rows[0].Details, "Correct-Horse")
	})

	t.Run("an empty display name defaults to the username", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		w := e.asAdmin(t, http.MethodPost, "/api/v1/users", `{"username":"plain","password":"Correct-Horse-9","role":"user"}`)
		require.Equal(t, http.StatusCreated, w.Code)
		assert.Equal(t, "plain", decodeInto[models.User](t, w).DisplayName)
	})

	bad := []struct {
		name, body string
	}{
		{"role is required", `{"username":"newbie","password":"Correct-Horse-9"}`},
		{"role empty", `{"username":"newbie","password":"Correct-Horse-9","role":""}`},
		{"unknown role", `{"username":"newbie","password":"Correct-Horse-9","role":"superuser"}`},
		{"role wrong case", `{"username":"newbie","password":"Correct-Horse-9","role":"Admin"}`},
		{"username too short", `{"username":"ab","password":"Correct-Horse-9","role":"user"}`},
		{"username too long", `{"username":"` + strings.Repeat("a", 101) + `","password":"Correct-Horse-9","role":"user"}`},
		{"username with a space", `{"username":"new user","password":"Correct-Horse-9","role":"user"}`},
		{"username not ASCII", `{"username":"jürgen","password":"Correct-Horse-9","role":"user"}`},
		{"username starts with a dot", `{"username":".hidden","password":"Correct-Horse-9","role":"user"}`},
		{"password missing", `{"username":"newbie","role":"user"}`},
		{"password too short", `{"username":"newbie","password":"short","role":"user"}`},
		{"password over 72 bytes", `{"username":"newbie","password":"` + strings.Repeat("a", 73) + `","role":"user"}`},
		{"email invalid", `{"username":"newbie","password":"Correct-Horse-9","role":"user","email":"not-an-email"}`},
		{"display name too long", `{"username":"newbie","password":"Correct-Horse-9","role":"user","display_name":"` + strings.Repeat("d", 256) + `"}`},
		{"unknown field auth_provider", `{"username":"newbie","password":"Correct-Horse-9","role":"user","auth_provider":"oidc"}`},
		{"unknown field disabled", `{"username":"newbie","password":"Correct-Horse-9","role":"user","disabled":true}`},
		{"unknown field service_account", `{"username":"newbie","password":"Correct-Horse-9","role":"user","service_account":true}`},
		{"unknown field id", `{"id":"` + idMissing + `","username":"newbie","password":"Correct-Horse-9","role":"user"}`},
		{"trailing data", `{"username":"newbie","password":"Correct-Horse-9","role":"user"} {}`},
		{"not json", `username=newbie`},
		{"json array", `[]`},
	}
	for _, tt := range bad {
		tt := tt
		t.Run("invalid: "+tt.name, func(t *testing.T) {
			t.Parallel()
			e := newAdminEnv(t)
			w := e.asAdmin(t, http.MethodPost, "/api/v1/users", tt.body)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.NotContains(t, w.Body.String(), "Correct-Horse")
			var n int64
			require.NoError(t, e.db.Model(&models.User{}).Count(&n).Error)
			assert.EqualValues(t, 6, n)
			assert.Empty(t, e.auditRows(t))
		})
	}

	t.Run("duplicate username is 409", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		w := e.asAdmin(t, http.MethodPost, "/api/v1/users", `{"username":"user-u","password":"Correct-Horse-9","role":"user"}`)
		assert.Equal(t, http.StatusConflict, w.Code)
		assert.Equal(t, "User already exists", errorOf(t, w))
	})

	t.Run("a body over the size limit is 413", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(`{"username":"newbie","password":"Correct-Horse-9","role":"user","display_name":"`+strings.Repeat("x", 4096)+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", e.bearer(t, idAdminA)["Authorization"])
		w := httptest.NewRecorder()
		limited := gin.New()
		limited.Use(middleware.MaxBodySize(512))
		limited.Any("/*any", func(c *gin.Context) { e.router.ServeHTTP(c.Writer, c.Request) })
		limited.ServeHTTP(w, req)
		assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	})

	t.Run("repository error is 500 without detail", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t, adminOpt{users: func(r models.UserRepository) models.UserRepository {
			return &userRepoWrap{UserRepository: r, createErr: errors.New("Error 1114: table full")}
		}})
		w := e.asAdmin(t, http.MethodPost, "/api/v1/users", `{"username":"newbie","password":"Correct-Horse-9","role":"user"}`)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Equal(t, msgInternalServerError, errorOf(t, w))
	})

	t.Run("a cancelled client gets 499 before the hash is computed", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(`{"username":"newbie","password":"Correct-Horse-9","role":"user"}`)).WithContext(ctx)
		req.Header.Set("Authorization", e.bearer(t, idAdminA)["Authorization"])
		w := httptest.NewRecorder()
		e.router.ServeHTTP(w, req)
		assert.Equal(t, statusClientClosedRequest, w.Code)
	})
}

// loginAs posts a login and returns the status.
func loginAs(t *testing.T, e *adminEnv, username, password string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(loginBody(t, username, password)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w.Code
}

func TestUpdateUser(t *testing.T) {
	t.Parallel()

	t.Run("profile fields change and nothing is revoked", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		tok, _ := e.login(t, "user-u")
		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU, `{"display_name":"  Renamed ","email":"renamed@example.com"}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		got := decodeInto[models.User](t, w)
		assert.Equal(t, "Renamed", got.DisplayName)
		assert.Equal(t, "renamed@example.com", got.Email)
		assert.Equal(t, models.RoleUser, got.Role)
		assert.NotContains(t, w.Body.String(), "password")
		assert.Equal(t, http.StatusOK, e.tokenStatus(bearerOf(tok)), "a profile change keeps the sessions")
		users, _ := e.revoker.snapshot()
		assert.Empty(t, users)

		rows := e.auditRows(t)
		require.Len(t, rows, 1)
		assert.Equal(t, middleware.AuditActionUpdate, rows[0].Action)
		assert.Equal(t, idUserU, rows[0].EntityID)
	})

	t.Run("email can be cleared", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU, `{"email":""}`)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Empty(t, e.user(t, idUserU).Email)
	})

	t.Run("a password reset ends every session and keeps the API keys", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		tok, refresh := e.login(t, "user-u")
		raw, _ := e.newKey(t, idUserU)
		require.Equal(t, http.StatusOK, e.tokenStatus(bearerOf(tok)))

		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU, `{"password":"Brand-New-Pass-1"}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.NotContains(t, w.Body.String(), "Brand-New")
		assert.NotContains(t, w.Body.String(), "$2a$")

		assert.Equal(t, http.StatusUnauthorized, e.tokenStatus(bearerOf(tok)), "tokens issued before the reset are blocked")
		assert.NotEmpty(t, refresh)
		assert.Zero(t, e.activeRefreshTokens(t, idUserU))
		users, _ := e.revoker.snapshot()
		assert.Equal(t, []string{idUserU}, users)
		assert.EqualValues(t, 1, e.keyCount(t, idUserU))
		assert.Equal(t, http.StatusOK, e.tokenStatus(keyHeader(raw)), "a password reset does not touch the keys")

		stored := e.user(t, idUserU)
		assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(stored.PasswordHash), []byte("Brand-New-Pass-1")))
		assert.Equal(t, http.StatusUnauthorized, loginAs(t, e, "user-u", testPassword), "the old password no longer works")
		assert.Equal(t, http.StatusOK, loginAs(t, e, "user-u", "Brand-New-Pass-1"), "a login after the reset works")

		rows := e.auditRows(t)
		require.Len(t, rows, 1)
		assert.Contains(t, rows[0].Details, `"password_changed":true`)
		assert.NotContains(t, rows[0].Details, "Brand-New")
	})

	bad := []struct{ name, body string }{
		{"empty object", `{}`},
		{"role is not an update field", `{"role":"admin"}`},
		{"disabled is not an update field", `{"disabled":true}`},
		{"username is not an update field", `{"username":"renamed"}`},
		{"auth_provider is not an update field", `{"auth_provider":"oidc"}`},
		{"service_account is not an update field", `{"service_account":true}`},
		{"known and unknown field", `{"display_name":"x","role":"admin"}`},
		{"display name empty", `{"display_name":"  "}`},
		{"display name too long", `{"display_name":"` + strings.Repeat("d", 256) + `"}`},
		{"email invalid", `{"email":"nope"}`},
		{"password too short", `{"password":"short"}`},
		{"password over 72 bytes", `{"password":"` + strings.Repeat("p", 73) + `"}`},
		{"wrong type", `{"display_name":5}`},
		{"trailing data", `{"display_name":"x"}x`},
	}
	for _, tt := range bad {
		tt := tt
		t.Run("invalid: "+tt.name, func(t *testing.T) {
			t.Parallel()
			e := newAdminEnv(t)
			w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserV, tt.body)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assertNothingChanged(t, e)
			assert.Equal(t, models.RoleUser, e.user(t, idUserV).Role)
			assert.Empty(t, e.auditRows(t))
		})
	}

	t.Run("a password cannot be set for a non-local user", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idOIDC, `{"password":"Brand-New-Pass-1"}`)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, msgPasswordNotLocal, errorOf(t, w))
		assert.Empty(t, e.user(t, idOIDC).PasswordHash)
		users, _ := e.revoker.snapshot()
		assert.Empty(t, users)
	})

	t.Run("an SSO user's profile can be edited", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idOIDC, `{"display_name":"Edited"}`)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	idTests := []struct {
		name, id string
		want     int
	}{
		{"unknown user", idMissing, http.StatusNotFound},
		{"bad id", "nope", http.StatusBadRequest},
	}
	for _, tt := range idTests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newAdminEnv(t)
			w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+tt.id, `{"display_name":"x"}`)
			assert.Equal(t, tt.want, w.Code)
		})
	}

	t.Run("repository errors are 500 without detail", func(t *testing.T) {
		t.Parallel()
		for name, wrap := range map[string]*userRepoWrap{
			"profile":  {profileErr: errors.New("Error 1213: deadlock")},
			"password": {passErr: errors.New("Error 1213: deadlock")},
		} {
			wrap := wrap
			e := newAdminEnv(t, adminOpt{users: func(r models.UserRepository) models.UserRepository {
				wrap.UserRepository = r
				return wrap
			}})
			body := `{"display_name":"x"}`
			if name == "password" {
				body = `{"password":"Brand-New-Pass-1"}`
			}
			w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserV, body)
			assert.Equal(t, http.StatusInternalServerError, w.Code, name)
			assert.Equal(t, msgInternalServerError, errorOf(t, w), name)
		}
	})
}

func TestChangeUserRole(t *testing.T) {
	t.Parallel()

	t.Run("a role change ends the sessions and the keys follow the new role", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		tok, _ := e.login(t, "user-u")
		raw, _ := e.newKey(t, idUserU)
		require.Equal(t, http.StatusOK, e.tokenStatus(bearerOf(tok)))
		require.Equal(t, models.RoleUser, decodeInto[map[string]string](t, e.do(http.MethodGet, "/api/v1/whoami", "", keyHeader(raw)))["role"])

		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU+"/role", `{"role":"devops"}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		resp := decodeInto[ChangeRoleResponse](t, w)
		assert.Equal(t, idUserU, resp.ID)
		assert.Equal(t, models.RoleUser, resp.OldRole)
		assert.Equal(t, models.RoleDevOps, resp.NewRole)
		assert.True(t, resp.Changed)
		assert.False(t, resp.IDPManaged)

		assert.Equal(t, models.RoleDevOps, e.user(t, idUserU).Role)
		assert.Equal(t, http.StatusUnauthorized, e.tokenStatus(bearerOf(tok)), "the old token is blocked")
		assert.Zero(t, e.activeRefreshTokens(t, idUserU), "refresh tokens are revoked")
		users, _ := e.revoker.snapshot()
		assert.Equal(t, []string{idUserU}, users, "the sockets are closed")
		assert.EqualValues(t, 1, e.keyCount(t, idUserU), "keys are kept")
		assert.Equal(t, models.RoleDevOps, decodeInto[map[string]string](t, e.do(http.MethodGet, "/api/v1/whoami", "", keyHeader(raw)))["role"], "the key follows the owner's current role")

		// A login after the change works and carries the new role.
		tok2, _ := e.login(t, "user-u")
		assert.Equal(t, http.StatusOK, e.tokenStatus(bearerOf(tok2)))
		assert.Equal(t, models.RoleDevOps, decodeInto[map[string]string](t, e.do(http.MethodGet, "/api/v1/whoami", "", bearerOf(tok2)))["role"])

		rows := e.auditRows(t)
		require.Len(t, rows, 1)
		assert.Equal(t, middleware.AuditActionChangeRole, rows[0].Action)
		assert.Equal(t, middleware.AuditEntityUser, rows[0].EntityType)
		assert.Equal(t, idUserU, rows[0].EntityID)
		assert.Contains(t, rows[0].Details, `"old_role":"user"`)
		assert.Contains(t, rows[0].Details, `"new_role":"devops"`)
	})

	t.Run("an unchanged role changes nothing and writes no audit row, but ends the sessions again", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		tok, _ := e.login(t, "user-u")
		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU+"/role", `{"role":"user"}`)
		require.Equal(t, http.StatusOK, w.Code)
		assert.False(t, decodeInto[ChangeRoleResponse](t, w).Changed)
		assert.Equal(t, models.RoleUser, e.user(t, idUserU).Role)
		assert.Equal(t, http.StatusUnauthorized, e.tokenStatus(bearerOf(tok)), "the revocation is idempotent and runs again")
		assert.Zero(t, e.activeRefreshTokens(t, idUserU))
		users, _ := e.revoker.snapshot()
		assert.Equal(t, []string{idUserU}, users)
		assert.Empty(t, e.auditRows(t))
	})

	t.Run("an SSO user's role can be changed, the answer says the identity provider may override it", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idOIDC+"/role", `{"role":"devops"}`)
		require.Equal(t, http.StatusOK, w.Code)
		resp := decodeInto[ChangeRoleResponse](t, w)
		assert.True(t, resp.IDPManaged)
		assert.Contains(t, resp.Message, "identity provider")
		assert.Equal(t, models.RoleDevOps, e.user(t, idOIDC).Role)
	})

	bad := []struct{ name, body string }{
		{"missing role", `{}`},
		{"empty role", `{"role":""}`},
		{"unknown role", `{"role":"root"}`},
		{"role wrong case", `{"role":"ADMIN"}`},
		{"role with whitespace", `{"role":" admin"}`},
		{"wrong type", `{"role":1}`},
		{"unknown field", `{"role":"admin","disabled":true}`},
		{"trailing data", `{"role":"admin"} x`},
	}
	for _, tt := range bad {
		tt := tt
		t.Run("invalid: "+tt.name, func(t *testing.T) {
			t.Parallel()
			e := newAdminEnv(t)
			w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserV+"/role", tt.body)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assertNothingChanged(t, e)
		})
	}

	t.Run("unknown and malformed ids", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		assert.Equal(t, http.StatusNotFound, e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idMissing+"/role", `{"role":"admin"}`).Code)
		assert.Equal(t, http.StatusBadRequest, e.asAdmin(t, http.MethodPut, "/api/v1/users/xyz/role", `{"role":"admin"}`).Code)
	})

	t.Run("the last admin cannot be demoted", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		require.NoError(t, e.db.Model(&models.User{}).Where("id = ?", idAdminB).Update("role", models.RoleUser).Error)
		tok, _ := e.login(t, "admin-a")
		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idAdminA+"/role", `{"role":"user"}`)
		assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		assert.Equal(t, msgLastAdmin, errorOf(t, w))
		assert.Equal(t, models.RoleAdmin, e.user(t, idAdminA).Role)
		assert.Equal(t, http.StatusOK, e.tokenStatus(bearerOf(tok)), "a refused change revokes nothing")
		users, _ := e.revoker.snapshot()
		assert.Empty(t, users)
		assert.Empty(t, e.auditRows(t))
	})

	t.Run("self-demotion works when another admin exists", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idAdminA+"/role", `{"role":"user"}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.EqualValues(t, 1, e.enabledAdmins(t))
	})

	t.Run("a demoted admin's token stops working at once", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		old := tokenWith(t, idAdminB, "admin-b", models.RoleAdmin)
		require.Equal(t, http.StatusOK, e.do(http.MethodGet, "/api/v1/users", "", bearerOf(old)).Code)
		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idAdminB+"/role", `{"role":"user"}`)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, http.StatusUnauthorized, e.do(http.MethodGet, "/api/v1/users", "", bearerOf(old)).Code, "blocked by the user block, not only by the database check")
		// A token minted later with a stale admin claim (the claim is not
		// trusted for the change itself) is refused by the database check.
		stale := tokenWith(t, idAdminB, "admin-b", models.RoleAdmin)
		assert.Equal(t, http.StatusForbidden, e.do(http.MethodGet, "/api/v1/users", "", bearerOf(stale)).Code)
		assert.Equal(t, http.StatusForbidden, e.do(http.MethodPut, "/api/v1/users/"+idUserV+"/role", `{"role":"admin"}`, bearerOf(stale)).Code)
		assert.Equal(t, models.RoleUser, e.user(t, idUserV).Role)
	})

	t.Run("a revocation step that fails is 500 but the other steps still run", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t, adminOpt{store: func(s sessionstore.SessionStore) sessionstore.SessionStore {
			return &faultyStore{SessionStore: s, blockErr: errors.New("dial tcp 10.0.0.1: refused")}
		}})
		_, _ = e.login(t, "user-u")
		w := e.do(http.MethodPut, "/api/v1/users/"+idUserU+"/role", `{"role":"devops"}`, bearerOf(tokenWith(t, idAdminA, "admin-a", models.RoleAdmin)))
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Equal(t, msgInternalServerError, errorOf(t, w))
		assert.NotContains(t, w.Body.String(), "dial tcp")
		assert.Equal(t, models.RoleDevOps, e.user(t, idUserU).Role, "the change itself committed")
		assert.Zero(t, e.activeRefreshTokens(t, idUserU), "refresh tokens were revoked anyway")
		users, _ := e.revoker.snapshot()
		assert.Equal(t, []string{idUserU}, users, "the sockets were closed anyway")
		assert.Contains(t, e.logs.String(), "Failed to block the user")
	})
}

func TestSetUserDisabled(t *testing.T) {
	t.Parallel()

	t.Run("disable ends the sessions, keeps the keys (which stop working), and enable restores logins", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		tok, _ := e.login(t, "user-u")
		raw, _ := e.newKey(t, idUserU)
		require.Equal(t, http.StatusOK, e.tokenStatus(keyHeader(raw)))

		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU+"/disabled", `{"disabled":true}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		resp := decodeInto[SetDisabledResponse](t, w)
		assert.True(t, resp.Disabled)
		assert.True(t, resp.Changed)

		assert.True(t, e.user(t, idUserU).Disabled)
		assert.Equal(t, http.StatusUnauthorized, e.tokenStatus(bearerOf(tok)))
		assert.Zero(t, e.activeRefreshTokens(t, idUserU))
		users, _ := e.revoker.snapshot()
		assert.Equal(t, []string{idUserU}, users)
		assert.EqualValues(t, 1, e.keyCount(t, idUserU), "a disabled user's keys are not deleted")
		assert.Equal(t, http.StatusUnauthorized, e.tokenStatus(keyHeader(raw)), "they stop authenticating because the owner is disabled")
		assert.Equal(t, http.StatusUnauthorized, loginAs(t, e, "user-u", testPassword), "a disabled user cannot sign in")

		// Enable: nothing is un-blocked, the old token stays dead, and a new
		// login works (its iat_ms is later than the block).
		w = e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU+"/disabled", `{"disabled":false}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.False(t, decodeInto[SetDisabledResponse](t, w).Disabled)
		assert.False(t, e.user(t, idUserU).Disabled)
		assert.Equal(t, http.StatusUnauthorized, e.tokenStatus(bearerOf(tok)), "tokens issued before the disable stay revoked")
		assert.Equal(t, http.StatusOK, e.tokenStatus(keyHeader(raw)), "the key works again")
		tok2, _ := e.login(t, "user-u")
		assert.Equal(t, http.StatusOK, e.tokenStatus(bearerOf(tok2)), "a login after the enable works")

		rows := e.auditRows(t)
		require.Len(t, rows, 2)
		assert.Equal(t, middleware.AuditActionDisable, rows[0].Action)
		assert.Equal(t, middleware.AuditActionEnable, rows[1].Action)
		for _, r := range rows {
			assert.Equal(t, middleware.AuditEntityUser, r.EntityType)
			assert.Equal(t, idUserU, r.EntityID)
		}
	})

	t.Run("disable and enable again at once: the login after the answer works", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		for i := 0; i < 5; i++ {
			require.Equal(t, http.StatusOK, e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU+"/disabled", `{"disabled":true}`).Code)
			require.Equal(t, http.StatusOK, e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU+"/disabled", `{"disabled":false}`).Code)
			tok, _ := e.login(t, "user-u")
			require.Equal(t, http.StatusOK, e.tokenStatus(bearerOf(tok)), "round %d", i)
		}
	})

	t.Run("enabling an enabled user and disabling a disabled one change nothing and write no audit row", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserU+"/disabled", `{"disabled":false}`)
		require.Equal(t, http.StatusOK, w.Code)
		assert.False(t, decodeInto[SetDisabledResponse](t, w).Changed)
		require.Equal(t, http.StatusOK, e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserV+"/disabled", `{"disabled":true}`).Code)
		w = e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserV+"/disabled", `{"disabled":true}`)
		require.Equal(t, http.StatusOK, w.Code)
		assert.False(t, decodeInto[SetDisabledResponse](t, w).Changed)
		assert.Len(t, e.auditRows(t), 1, "only the real change")
	})

	bad := []struct{ name, body string }{
		{"missing", `{}`},
		{"null", `{"disabled":null}`},
		{"string", `{"disabled":"true"}`},
		{"number", `{"disabled":1}`},
		{"unknown field", `{"disabled":true,"role":"user"}`},
		{"trailing data", `{"disabled":true}x`},
	}
	for _, tt := range bad {
		tt := tt
		t.Run("invalid: "+tt.name, func(t *testing.T) {
			t.Parallel()
			e := newAdminEnv(t)
			w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserV+"/disabled", tt.body)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assertNothingChanged(t, e)
		})
	}

	t.Run("unknown and malformed ids", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		assert.Equal(t, http.StatusNotFound, e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idMissing+"/disabled", `{"disabled":true}`).Code)
		assert.Equal(t, http.StatusBadRequest, e.asAdmin(t, http.MethodPut, "/api/v1/users/zzz/disabled", `{"disabled":true}`).Code)
	})

	t.Run("the last admin cannot be disabled, also not by themselves", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		require.NoError(t, e.db.Model(&models.User{}).Where("id = ?", idAdminB).Update("disabled", true).Error)
		tok, _ := e.login(t, "admin-a")
		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idAdminA+"/disabled", `{"disabled":true}`)
		assert.Equal(t, http.StatusConflict, w.Code)
		assert.Equal(t, msgLastAdmin, errorOf(t, w))
		assert.False(t, e.user(t, idAdminA).Disabled)
		assert.Equal(t, http.StatusOK, e.tokenStatus(bearerOf(tok)))
		users, _ := e.revoker.snapshot()
		assert.Empty(t, users)
	})

	t.Run("an admin can disable themselves when another admin exists", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		w := e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idAdminA+"/disabled", `{"disabled":true}`)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.EqualValues(t, 1, e.enabledAdmins(t))
	})
}

func TestDeleteUser(t *testing.T) {
	t.Parallel()

	t.Run("deleting a user revokes everything and deletes the keys", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		tok, _ := e.login(t, "user-u")
		raw, _ := e.newKey(t, idUserU)
		_, _ = e.newKey(t, idUserU)
		_, otherKey := e.newKey(t, idUserV)

		w := e.asAdmin(t, http.MethodDelete, "/api/v1/users/"+idUserU, "")
		require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
		assert.False(t, e.userExists(idUserU))
		assert.Equal(t, http.StatusUnauthorized, e.tokenStatus(bearerOf(tok)))
		assert.Zero(t, e.activeRefreshTokens(t, idUserU))
		assert.Zero(t, e.keyCount(t, idUserU), "the keys are deleted")
		assert.Equal(t, http.StatusUnauthorized, e.tokenStatus(keyHeader(raw)))
		assert.EqualValues(t, 1, e.keyCount(t, idUserV), "other users' keys are untouched")
		var kept int64
		require.NoError(t, e.db.Model(&models.APIKey{}).Where("id = ?", otherKey).Count(&kept).Error)
		assert.EqualValues(t, 1, kept)
		users, _ := e.revoker.snapshot()
		assert.Equal(t, []string{idUserU}, users)

		rows := e.auditRows(t)
		require.Len(t, rows, 1)
		assert.Equal(t, middleware.AuditActionDelete, rows[0].Action)
		assert.Equal(t, middleware.AuditEntityUser, rows[0].EntityType)
		assert.Equal(t, idUserU, rows[0].EntityID)
	})

	t.Run("unknown and malformed ids", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		assert.Equal(t, http.StatusNotFound, e.asAdmin(t, http.MethodDelete, "/api/v1/users/"+idMissing, "").Code)
		assert.Equal(t, http.StatusBadRequest, e.asAdmin(t, http.MethodDelete, "/api/v1/users/1", "").Code)
	})

	t.Run("the last admin cannot be deleted; only the pre-delete block applies", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		require.NoError(t, e.db.Model(&models.User{}).Where("id = ?", idAdminB).Update("disabled", true).Error)
		_, _ = e.newKey(t, idAdminA)
		tok, _ := e.login(t, "admin-a")
		w := e.asAdmin(t, http.MethodDelete, "/api/v1/users/"+idAdminA, "")
		assert.Equal(t, http.StatusConflict, w.Code)
		assert.Equal(t, msgLastAdmin, errorOf(t, w))
		assert.True(t, e.userExists(idAdminA))
		assert.EqualValues(t, 1, e.keyCount(t, idAdminA))
		assert.Equal(t, http.StatusUnauthorized, e.tokenStatus(bearerOf(tok)), "the block is written before the delete, so a refused delete has blocked the tokens issued so far")
		assert.EqualValues(t, 1, e.activeRefreshTokens(t, idAdminA), "nothing else is revoked")
		assert.Empty(t, e.auditRows(t))
	})

	t.Run("an admin can delete themselves when another admin exists", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t)
		w := e.asAdmin(t, http.MethodDelete, "/api/v1/users/"+idAdminA, "")
		assert.Equal(t, http.StatusNoContent, w.Code)
		assert.EqualValues(t, 1, e.enabledAdmins(t))
	})

	t.Run("a revocation step that fails is 500 after the delete committed", func(t *testing.T) {
		t.Parallel()
		e := newAdminEnv(t, adminOpt{keys: func(r models.APIKeyRepository) models.APIKeyRepository {
			return failingDeleteAll{APIKeyRepository: r}
		}})
		_, _ = e.newKey(t, idUserU)
		w := e.asAdmin(t, http.MethodDelete, "/api/v1/users/"+idUserU, "")
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Equal(t, msgInternalServerError, errorOf(t, w))
		assert.False(t, e.userExists(idUserU))
		users, _ := e.revoker.snapshot()
		assert.Equal(t, []string{idUserU}, users, "the later steps still ran")
	})
}

type failingDeleteAll struct{ models.APIKeyRepository }

func (failingDeleteAll) DeleteAllForUser(context.Context, string) (int64, error) {
	return 0, errors.New("Error 1205: lock wait timeout")
}

func TestGuardedChangeErrorsAreMappedBeforeTheGenericMapping(t *testing.T) {
	t.Parallel()
	wrapped := func(base error) error { return dberrors.NewDatabaseError("update_role", base) }
	tests := []struct {
		name string
		wrap *userRepoWrap
		call func(e *adminEnv) *httptest.ResponseRecorder
		want int
		msg  string
	}{
		{"role: last admin", &userRepoWrap{roleErr: wrapped(models.ErrLastAdmin)}, func(e *adminEnv) *httptest.ResponseRecorder {
			return e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserV+"/role", `{"role":"user"}`)
		}, http.StatusConflict, msgLastAdmin},
		{"role: caller not admin", &userRepoWrap{roleErr: wrapped(models.ErrCallerNotAdmin)}, func(e *adminEnv) *httptest.ResponseRecorder {
			return e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserV+"/role", `{"role":"user"}`)
		}, http.StatusForbidden, msgAdminRequired},
		{"disabled: last admin", &userRepoWrap{disableErr: wrapped(models.ErrLastAdmin)}, func(e *adminEnv) *httptest.ResponseRecorder {
			return e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserV+"/disabled", `{"disabled":true}`)
		}, http.StatusConflict, msgLastAdmin},
		{"disabled: caller not admin", &userRepoWrap{disableErr: wrapped(models.ErrCallerNotAdmin)}, func(e *adminEnv) *httptest.ResponseRecorder {
			return e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserV+"/disabled", `{"disabled":true}`)
		}, http.StatusForbidden, msgAdminRequired},
		{"delete: last admin", &userRepoWrap{deleteErr: wrapped(models.ErrLastAdmin)}, func(e *adminEnv) *httptest.ResponseRecorder {
			return e.asAdmin(t, http.MethodDelete, "/api/v1/users/"+idUserV, "")
		}, http.StatusConflict, msgLastAdmin},
		{"delete: caller not admin", &userRepoWrap{deleteErr: wrapped(models.ErrCallerNotAdmin)}, func(e *adminEnv) *httptest.ResponseRecorder {
			return e.asAdmin(t, http.MethodDelete, "/api/v1/users/"+idUserV, "")
		}, http.StatusForbidden, msgAdminRequired},
		{"role: generic database error", &userRepoWrap{roleErr: errors.New("Error 1040: too many connections")}, func(e *adminEnv) *httptest.ResponseRecorder {
			return e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserV+"/role", `{"role":"admin"}`)
		}, http.StatusInternalServerError, msgInternalServerError},
		{"disabled: generic database error", &userRepoWrap{disableErr: errors.New("Error 1040")}, func(e *adminEnv) *httptest.ResponseRecorder {
			return e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserV+"/disabled", `{"disabled":true}`)
		}, http.StatusInternalServerError, msgInternalServerError},
		{"delete: generic database error", &userRepoWrap{deleteErr: errors.New("Error 1040")}, func(e *adminEnv) *httptest.ResponseRecorder {
			return e.asAdmin(t, http.MethodDelete, "/api/v1/users/"+idUserV, "")
		}, http.StatusInternalServerError, msgInternalServerError},
		{"role: the user vanished meanwhile", &userRepoWrap{roleErr: dberrors.NewDatabaseError("update_role", dberrors.ErrNotFound)}, func(e *adminEnv) *httptest.ResponseRecorder {
			return e.asAdmin(t, http.MethodPut, "/api/v1/users/"+idUserV+"/role", `{"role":"admin"}`)
		}, http.StatusNotFound, "User not found"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newAdminEnv(t, adminOpt{users: func(r models.UserRepository) models.UserRepository {
				tt.wrap.UserRepository = r
				return tt.wrap
			}})
			w := tt.call(e)
			assert.Equal(t, tt.want, w.Code, w.Body.String())
			assert.Equal(t, tt.msg, errorOf(t, w))
			assert.NotContains(t, w.Body.String(), "Error 10")
			users, _ := e.revoker.snapshot()
			assert.Empty(t, users, "a refused change revokes nothing")
		})
	}
}

func TestChangeCommitsEvenWhenTheClientLeaves(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	e := newAdminEnv(t, adminOpt{users: func(r models.UserRepository) models.UserRepository {
		return &userRepoWrap{UserRepository: r, beforeGuarded: cancel}
	}})
	tok, _ := e.login(t, "user-u")

	req := httptest.NewRequest(http.MethodPut, "/api/v1/users/"+idUserU+"/role", strings.NewReader(`{"role":"devops"}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", e.bearer(t, idAdminA)["Authorization"])
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "the answer does not turn a committed change into a 499")
	assert.Equal(t, models.RoleDevOps, e.user(t, idUserU).Role)
	assert.Equal(t, http.StatusUnauthorized, e.tokenStatus(bearerOf(tok)), "the revocation ran on a detached context")
	assert.Zero(t, e.activeRefreshTokens(t, idUserU))
	users, _ := e.revoker.snapshot()
	assert.Equal(t, []string{idUserU}, users)
	assert.Len(t, e.auditRows(t), 1, "and the audit row was written")
}

func TestAdminChangesWithoutOptionalDependencies(t *testing.T) {
	t.Parallel()
	e := newAdminEnv(t)
	h := NewUserHandler(e.users, nil, nil, nil)
	r := gin.New()
	r.Use(middleware.AuthRequiredWithOptions(middleware.JWTAuthOptions{JWTSecret: testJWTSecret}))
	r.PUT("/users/:id/role", h.ChangeUserRole)
	r.DELETE("/users/:id", h.DeleteUser)

	req := httptest.NewRequest(http.MethodPut, "/users/"+idUserU+"/role", strings.NewReader(`{"role":"devops"}`))
	req.Header.Set("Authorization", e.bearer(t, idAdminA)["Authorization"])
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, "no session store, token or key repository, and no hub: nothing to revoke, no panic")

	h.SetWebSocketRevoker((*websocket.Hub)(nil))
	req = httptest.NewRequest(http.MethodDelete, "/users/"+idUserU, nil)
	req.Header.Set("Authorization", e.bearer(t, idAdminA)["Authorization"])
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNoContent, w.Code, "a typed nil hub is safe")
}

// TestAdminsDemotingEachOtherNeverLeaveNoAdmin fires two admins that demote,
// disable or delete each other at the same moment: the repository serializes
// the changes, so at least one enabled admin remains. The MySQL integration
// test (database package) runs the same race on real row locks.
func TestAdminsDemotingEachOtherNeverLeaveNoAdmin(t *testing.T) {
	t.Parallel()
	type action struct {
		name   string
		method string
		path   func(target string) string
		body   string
	}
	actions := []action{
		{"demote", http.MethodPut, func(id string) string { return "/api/v1/users/" + id + "/role" }, `{"role":"user"}`},
		{"disable", http.MethodPut, func(id string) string { return "/api/v1/users/" + id + "/disabled" }, `{"disabled":true}`},
		{"delete", http.MethodDelete, func(id string) string { return "/api/v1/users/" + id }, ""},
	}
	for _, act := range actions {
		act := act
		t.Run(act.name, func(t *testing.T) {
			t.Parallel()
			for round := 0; round < 15; round++ {
				e := newAdminEnv(t)
				// Only the two admins matter; the others stay.
				hA, hB := e.bearer(t, idAdminA), e.bearer(t, idAdminB)
				var wg sync.WaitGroup
				codes := make([]int, 2)
				start := make(chan struct{})
				for i, call := range []struct {
					headers map[string]string
					target  string
				}{{hA, idAdminB}, {hB, idAdminA}} {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						codes[i] = e.do(act.method, act.path(call.target), act.body, call.headers).Code
					}()
				}
				close(start)
				wg.Wait()
				require.GreaterOrEqual(t, e.enabledAdmins(t), int64(1), "round %d: codes %v", round, codes)
				// Exactly one request won; the other was refused (401: its token
				// was blocked already, 403: its caller was no longer an admin
				// in the database, or 409: the last admin).
				succeeded := 0
				for _, c := range codes {
					switch c {
					case http.StatusOK, http.StatusNoContent:
						succeeded++
					case http.StatusForbidden, http.StatusConflict, http.StatusUnauthorized:
					default:
						t.Fatalf("round %d: unexpected status %d (%v)", round, c, codes)
					}
				}
				assert.Equal(t, 1, succeeded, "round %d: codes %v", round, codes)
			}
		})
	}
}

func TestUserBlockLifetime(t *testing.T) {
	t.Parallel()
	until := userBlockUntil(&config.AuthConfig{AccessTokenExpiration: 15 * time.Minute, JWTExpiration: 3 * time.Hour})
	assert.WithinDuration(t, time.Now().Add(3*time.Hour+sessionBlockSkew), until, 5*time.Second, "the longest token lifetime plus the skew")
	assert.WithinDuration(t, time.Now().Add(defaultUserBlockLifetime+sessionBlockSkew), userBlockUntil(nil), 5*time.Second, "a missing configuration still blocks for a safe time")
}
