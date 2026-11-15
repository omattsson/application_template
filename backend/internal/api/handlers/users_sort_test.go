package handlers

import (
	"net/http"
	"testing"
	"time"

	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sortedUsersEnv returns the admin fixture with an email and a creation time
// that differ from the username order. The user IDs ascend in this order:
//
//	id  username  role    email  created_at
//	1   admin-a   admin   m@x    2024-03
//	2   admin-b   admin   z@x    2024-01
//	3   devops    devops  b@x    2024-06
//	4   user-u    user    a@x    2024-02
//	5   oidc-o    user    k@x    2024-05
//	6   user-v    user    c@x    2024-04
func sortedUsersEnv(t *testing.T) *adminEnv {
	t.Helper()
	e := newAdminEnv(t)
	rows := []struct {
		id, email string
		created   time.Month
	}{
		{idAdminA, "m@x", time.March},
		{idAdminB, "z@x", time.January},
		{idDevops, "b@x", time.June},
		{idUserU, "a@x", time.February},
		{idOIDC, "k@x", time.May},
		{idUserV, "c@x", time.April},
	}
	for _, r := range rows {
		created := time.Date(2024, r.created, 1, 0, 0, 0, 0, time.UTC)
		require.NoError(t, e.db.Model(&models.User{}).Where("id = ?", r.id).
			Updates(map[string]any{"email": r.email, "created_at": created}).Error)
	}
	return e
}

func (e *adminEnv) listUsers(t *testing.T, query string) UserListResponse {
	t.Helper()
	w := e.asAdmin(t, http.MethodGet, "/api/v1/users"+query, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return decodeInto[UserListResponse](t, w)
}

func usernames(users []models.User) []string {
	names := make([]string, 0, len(users))
	for _, u := range users {
		names = append(names, u.Username)
	}
	return names
}

func TestListUsersSort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"default is username ascending", "", []string{"admin-a", "admin-b", "devops", "oidc-o", "user-u", "user-v"}},
		{"empty sort and order are the default", "?sort=&order=", []string{"admin-a", "admin-b", "devops", "oidc-o", "user-u", "user-v"}},
		{"order alone sorts username", "?order=desc", []string{"user-v", "user-u", "oidc-o", "devops", "admin-b", "admin-a"}},
		{"order asc alone", "?order=asc", []string{"admin-a", "admin-b", "devops", "oidc-o", "user-u", "user-v"}},
		{"username asc", "?sort=username&order=asc", []string{"admin-a", "admin-b", "devops", "oidc-o", "user-u", "user-v"}},
		{"username desc", "?sort=username&order=desc", []string{"user-v", "user-u", "oidc-o", "devops", "admin-b", "admin-a"}},
		{"email asc", "?sort=email&order=asc", []string{"user-u", "devops", "user-v", "oidc-o", "admin-a", "admin-b"}},
		{"email desc", "?sort=email&order=desc", []string{"admin-b", "admin-a", "oidc-o", "user-v", "devops", "user-u"}},
		{"role asc, ties by id asc", "?sort=role&order=asc", []string{"admin-a", "admin-b", "devops", "user-u", "oidc-o", "user-v"}},
		{"role desc, ties by id desc", "?sort=role&order=desc", []string{"user-v", "oidc-o", "user-u", "devops", "admin-b", "admin-a"}},
		{"created_at asc", "?sort=created_at&order=asc", []string{"admin-b", "user-u", "admin-a", "user-v", "oidc-o", "devops"}},
		{"created_at desc", "?sort=created_at&order=desc", []string{"devops", "oidc-o", "user-v", "admin-a", "user-u", "admin-b"}},
		{"sort without order is ascending", "?sort=email", []string{"user-u", "devops", "user-v", "oidc-o", "admin-a", "admin-b"}},
		{"filter and sort", "?role=user&sort=username&order=desc", []string{"user-v", "user-u", "oidc-o"}},
		{"search and sort", "?search=admin&sort=email&order=desc", []string{"admin-b", "admin-a"}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := sortedUsersEnv(t)
			resp := e.listUsers(t, tt.query)
			assert.Equal(t, tt.want, usernames(resp.Data))
			assert.EqualValues(t, len(tt.want), resp.Total)
		})
	}
}

func TestListUsersSortRejectsBadValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		query   string
		wantMsg string
	}{
		{"column that is not listed", "?sort=display_name", "Invalid sort parameter"},
		{"id is not listed", "?sort=id", "Invalid sort parameter"},
		{"password hash", "?sort=password_hash", "Invalid sort parameter"},
		{"disabled", "?sort=disabled", "Invalid sort parameter"},
		{"sort with the wrong case", "?sort=Username", "Invalid sort parameter"},
		{"sort with SQL", "?sort=username%3BDROP+TABLE+users", "Invalid sort parameter"},
		{"unknown order", "?order=up", "Invalid order parameter"},
		{"order with the wrong case", "?sort=username&order=ASC", "Invalid order parameter"},
		{"order with SQL", "?order=desc,id", "Invalid order parameter"},
		{"bad sort wins with a bad order", "?sort=zzz&order=zzz", "Invalid sort parameter"},
		{"bad sort next to a valid page size", "?sort=zzz&pageSize=5", "Invalid sort parameter"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newAdminEnv(t)
			w := e.asAdmin(t, http.MethodGet, "/api/v1/users"+tt.query, "")
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Equal(t, tt.wantMsg, errorOf(t, w))
		})
	}
}

// Pages of a sort on a column with repeated values never repeat or skip a
// row, in both directions, and the total stays the same.
func TestListUsersSortPagesAreStable(t *testing.T) {
	t.Parallel()
	for _, sortKey := range models.UserSortKeys {
		for _, order := range []string{"asc", "desc"} {
			sortKey, order := sortKey, order
			t.Run(sortKey+" "+order, func(t *testing.T) {
				t.Parallel()
				e := sortedUsersEnv(t)
				base := "?sort=" + sortKey + "&order=" + order

				all := e.listUsers(t, base)
				require.Len(t, all.Data, 6)

				var paged []models.User
				for _, page := range []string{"1", "2", "3", "4"} {
					resp := e.listUsers(t, base+"&pageSize=2&page="+page)
					assert.EqualValues(t, 6, resp.Total, "the sort does not change the total")
					paged = append(paged, resp.Data...)
				}
				assert.Equal(t, usernames(all.Data), usernames(paged))
			})
		}
	}
}

func TestListUsersSortIsAdminOnly(t *testing.T) {
	t.Parallel()
	e := newAdminEnv(t)
	w := e.do(http.MethodGet, "/api/v1/users?sort=bogus", "", e.bearer(t, idUserU))
	assert.Equal(t, http.StatusForbidden, w.Code, "the role check runs before the parameters")
}
