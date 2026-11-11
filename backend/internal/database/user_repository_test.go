package database

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupUserRepo(t *testing.T) *GORMUserRepository {
	t.Helper()
	return NewGORMUserRepository(openMigratedDB(t))
}

func strPtr(s string) *string { return &s }

func TestUserRepositoryCreate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		seed    []models.User
		user    models.User
		wantErr error
	}{
		{name: "success", user: models.User{Username: "alice", Role: models.RoleDevOps, Email: "alice@example.com"}},
		{name: "empty role defaults to user", user: models.User{Username: "bob"}},
		{name: "empty username", user: models.User{Role: models.RoleUser}, wantErr: dberrors.ErrValidation},
		{name: "unknown role", user: models.User{Username: "eve", Role: "root"}, wantErr: dberrors.ErrValidation},
		{name: "bad email", user: models.User{Username: "eve", Role: models.RoleUser, Email: "nope"}, wantErr: dberrors.ErrValidation},
		{
			name:    "duplicate username",
			seed:    []models.User{{Username: "alice", Role: models.RoleUser}},
			user:    models.User{Username: "alice", Role: models.RoleAdmin},
			wantErr: dberrors.ErrDuplicateKey,
		},
		{
			name: "two local users without email or external ID",
			seed: []models.User{{Username: "a", Role: models.RoleUser}},
			user: models.User{Username: "b", Role: models.RoleUser},
		},
		{
			name:    "duplicate external ID of one provider",
			seed:    []models.User{{Username: "a", Role: models.RoleUser, AuthProvider: models.AuthProviderOIDC, ExternalID: strPtr("sub-1")}},
			user:    models.User{Username: "b", Role: models.RoleUser, AuthProvider: models.AuthProviderOIDC, ExternalID: strPtr("sub-1")},
			wantErr: dberrors.ErrDuplicateKey,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := setupUserRepo(t)
			ctx := context.Background()
			for i := range tt.seed {
				require.NoError(t, repo.Create(ctx, &tt.seed[i]))
			}

			u := tt.user
			err := repo.Create(ctx, &u)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.NotContains(t, err.Error(), u.Username+"'", "the duplicate value is not in the error")
				return
			}
			require.NoError(t, err)
			_, parseErr := uuid.Parse(u.ID)
			require.NoError(t, parseErr, "the repository sets a UUID")
			assert.False(t, u.CreatedAt.IsZero())
			assert.False(t, u.UpdatedAt.IsZero())

			got, err := repo.FindByID(ctx, u.ID)
			require.NoError(t, err)
			assert.Equal(t, u.Username, got.Username)
			assert.NotEmpty(t, got.Role)
			wantRole, wantProvider := tt.user.Role, tt.user.AuthProvider
			if wantRole == "" {
				wantRole = models.RoleUser
			}
			if wantProvider == "" {
				wantProvider = models.AuthProviderLocal
			}
			assert.Equal(t, wantRole, got.Role)
			assert.Equal(t, wantProvider, got.AuthProvider)
		})
	}
}

func TestUserRepositoryCreateKeepsGivenID(t *testing.T) {
	t.Parallel()
	repo := setupUserRepo(t)
	u := &models.User{ID: "fixed-id", Username: "fixed", Role: models.RoleUser}
	require.NoError(t, repo.Create(context.Background(), u))
	assert.Equal(t, "fixed-id", u.ID)
	assert.Equal(t, models.AuthProviderLocal, u.AuthProvider)
}

func TestUserRepositoryFinders(t *testing.T) {
	t.Parallel()
	repo := setupUserRepo(t)
	ctx := context.Background()
	alice := &models.User{Username: "alice", Role: models.RoleUser, PasswordHash: "h"}
	sso := &models.User{Username: "sso", Role: models.RoleDevOps, AuthProvider: models.AuthProviderOIDC, ExternalID: strPtr("sub-9")}
	require.NoError(t, repo.Create(ctx, alice))
	require.NoError(t, repo.Create(ctx, sso))

	t.Run("by ID", func(t *testing.T) {
		t.Parallel()
		got, err := repo.FindByID(ctx, alice.ID)
		require.NoError(t, err)
		assert.Equal(t, "alice", got.Username)
		assert.Equal(t, "h", got.PasswordHash, "the auth path needs the hash")

		_, err = repo.FindByID(ctx, "missing")
		assert.ErrorIs(t, err, dberrors.ErrNotFound)
	})

	t.Run("by username", func(t *testing.T) {
		t.Parallel()
		got, err := repo.FindByUsername(ctx, "sso")
		require.NoError(t, err)
		assert.Equal(t, sso.ID, got.ID)

		_, err = repo.FindByUsername(ctx, "nobody")
		assert.ErrorIs(t, err, dberrors.ErrNotFound)
	})

	t.Run("by external ID", func(t *testing.T) {
		t.Parallel()
		got, err := repo.FindByExternalID(ctx, models.AuthProviderOIDC, "sub-9")
		require.NoError(t, err)
		assert.Equal(t, sso.ID, got.ID)

		_, err = repo.FindByExternalID(ctx, models.AuthProviderLocal, "sub-9")
		assert.ErrorIs(t, err, dberrors.ErrNotFound)
	})

	t.Run("by IDs", func(t *testing.T) {
		t.Parallel()
		got, err := repo.FindByIDs(ctx, []string{alice.ID, sso.ID, "missing"})
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, "alice", got[alice.ID].Username)
		assert.Empty(t, got[alice.ID].PasswordHash, "a batch lookup does not read the password hash")

		empty, err := repo.FindByIDs(ctx, nil)
		require.NoError(t, err)
		assert.Empty(t, empty)
	})

	t.Run("count", func(t *testing.T) {
		t.Parallel()
		n, err := repo.Count(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(2), n)
	})
}

func TestUserRepositoryDelete(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		seed    bool
		wantErr error
	}{
		{name: "success", seed: true},
		{name: "not found", wantErr: dberrors.ErrNotFound},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := setupUserRepo(t)
			ctx := context.Background()
			id := "u-del"
			if tt.seed {
				require.NoError(t, repo.Create(ctx, &models.User{ID: id, Username: "del", Role: models.RoleUser}))
			}

			err := repo.Delete(ctx, id)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			_, err = repo.FindByID(ctx, id)
			assert.ErrorIs(t, err, dberrors.ErrNotFound)
		})
	}
}

func TestUserRepositoryListPaged(t *testing.T) {
	t.Parallel()
	repo := setupUserRepo(t)
	ctx := context.Background()
	for i, role := range []string{models.RoleUser, models.RoleUser, models.RoleDevOps, models.RoleAdmin, models.RoleUser} {
		require.NoError(t, repo.Create(ctx, &models.User{
			Username:     fmt.Sprintf("user-%d", i),
			DisplayName:  fmt.Sprintf("Name %d", i),
			Email:        fmt.Sprintf("u%d@example.com", i),
			Role:         role,
			PasswordHash: "secret",
		}))
	}
	require.NoError(t, repo.Create(ctx, &models.User{Username: "100%_odd", Role: models.RoleUser}))

	tests := []struct {
		name      string
		filter    models.UserFilter
		limit     int
		offset    int
		wantTotal int64
		wantNames []string
	}{
		{name: "all", wantTotal: 6, wantNames: []string{"100%_odd", "user-0", "user-1", "user-2", "user-3", "user-4"}},
		{name: "page", limit: 2, offset: 2, wantTotal: 6, wantNames: []string{"user-1", "user-2"}},
		{name: "offset past the end", limit: 2, offset: 10, wantTotal: 6, wantNames: []string{}},
		{name: "role", filter: models.UserFilter{Role: models.RoleUser}, wantTotal: 4, wantNames: []string{"100%_odd", "user-0", "user-1", "user-4"}},
		{name: "search username", filter: models.UserFilter{Search: "ser-3"}, wantTotal: 1, wantNames: []string{"user-3"}},
		{name: "search display name", filter: models.UserFilter{Search: "Name 2"}, wantTotal: 1, wantNames: []string{"user-2"}},
		{name: "search email", filter: models.UserFilter{Search: "u4@"}, wantTotal: 1, wantNames: []string{"user-4"}},
		{name: "wildcards are literals", filter: models.UserFilter{Search: "%_"}, wantTotal: 1, wantNames: []string{"100%_odd"}},
		{name: "role and search", filter: models.UserFilter{Role: models.RoleAdmin, Search: "user"}, wantTotal: 1, wantNames: []string{"user-3"}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			users, total, err := repo.ListPaged(ctx, tt.filter, tt.limit, tt.offset)
			require.NoError(t, err)
			assert.Equal(t, tt.wantTotal, total)
			names := make([]string, 0, len(users))
			for _, u := range users {
				names = append(names, u.Username)
				assert.Empty(t, u.PasswordHash, "the list does not read the password hash")
				assert.NotEmpty(t, u.ID)
				assert.NotEmpty(t, u.Role)
			}
			assert.Equal(t, tt.wantNames, names)
		})
	}
}

func TestUserRepositoryGuardedChanges(t *testing.T) {
	t.Parallel()

	admin := func(id string, disabled bool) models.User {
		return models.User{ID: id, Username: id, Role: models.RoleAdmin, Disabled: disabled}
	}
	user := func(id, role string) models.User {
		return models.User{ID: id, Username: id, Role: role}
	}

	type result struct {
		role     string
		disabled bool
		gone     bool
	}

	tests := []struct {
		name      string
		users     []models.User
		run       func(r *GORMUserRepository) error
		target    string
		want      result
		wantErrIs error
	}{
		{
			name:  "role: user to devops",
			users: []models.User{admin("c", false), user("u1", models.RoleUser)},
			run: func(r *GORMUserRepository) error {
				old, err := r.UpdateRole(context.Background(), "c", "u1", models.RoleDevOps)
				if err == nil && old != models.RoleUser {
					return fmt.Errorf("old role %q", old)
				}
				return err
			},
			target: "u1", want: result{role: models.RoleDevOps},
		},
		{
			name:  "role: unchanged is a no-op",
			users: []models.User{admin("c", false), user("u1", models.RoleDevOps)},
			run: func(r *GORMUserRepository) error {
				_, err := r.UpdateRole(context.Background(), "c", "u1", models.RoleDevOps)
				return err
			},
			target: "u1", want: result{role: models.RoleDevOps},
		},
		{
			name:  "role: unknown role is refused",
			users: []models.User{admin("c", false), user("u1", models.RoleUser)},
			run: func(r *GORMUserRepository) error {
				_, err := r.UpdateRole(context.Background(), "c", "u1", "root")
				return err
			},
			target: "u1", want: result{role: models.RoleUser}, wantErrIs: dberrors.ErrValidation,
		},
		{
			name:  "role: demote another admin",
			users: []models.User{admin("c", false), admin("a1", false)},
			run: func(r *GORMUserRepository) error {
				_, err := r.UpdateRole(context.Background(), "c", "a1", models.RoleUser)
				return err
			},
			target: "a1", want: result{role: models.RoleUser},
		},
		{
			name:  "role: last enabled admin keeps the role",
			users: []models.User{admin("a1", false), admin("a2", true)},
			run: func(r *GORMUserRepository) error {
				_, err := r.UpdateRole(context.Background(), "a1", "a1", models.RoleDevOps)
				return err
			},
			target: "a1", want: result{role: models.RoleAdmin}, wantErrIs: models.ErrLastAdmin,
		},
		{
			name:  "role: disabled caller is refused",
			users: []models.User{admin("c", true), admin("a1", false), user("u1", models.RoleUser)},
			run: func(r *GORMUserRepository) error {
				_, err := r.UpdateRole(context.Background(), "c", "u1", models.RoleAdmin)
				return err
			},
			target: "u1", want: result{role: models.RoleUser}, wantErrIs: models.ErrCallerNotAdmin,
		},
		{
			name:  "role: non-admin caller is refused",
			users: []models.User{user("c", models.RoleDevOps), user("u1", models.RoleUser)},
			run: func(r *GORMUserRepository) error {
				_, err := r.UpdateRole(context.Background(), "c", "u1", models.RoleAdmin)
				return err
			},
			target: "u1", want: result{role: models.RoleUser}, wantErrIs: models.ErrCallerNotAdmin,
		},
		{
			name:  "role: unknown user",
			users: []models.User{admin("c", false)},
			run: func(r *GORMUserRepository) error {
				_, err := r.UpdateRole(context.Background(), "c", "ghost", models.RoleUser)
				return err
			},
			wantErrIs: dberrors.ErrNotFound,
		},
		{
			name:   "disable: other user",
			users:  []models.User{admin("c", false), user("u1", models.RoleUser)},
			run:    func(r *GORMUserRepository) error { return r.SetDisabled(context.Background(), "c", "u1", true) },
			target: "u1", want: result{role: models.RoleUser, disabled: true},
		},
		{
			name:   "disable: last enabled admin is refused",
			users:  []models.User{admin("a1", false)},
			run:    func(r *GORMUserRepository) error { return r.SetDisabled(context.Background(), "a1", "a1", true) },
			target: "a1", want: result{role: models.RoleAdmin}, wantErrIs: models.ErrLastAdmin,
		},
		{
			name:   "enable: allowed without another admin",
			users:  []models.User{admin("c", false), admin("a1", true)},
			run:    func(r *GORMUserRepository) error { return r.SetDisabled(context.Background(), "c", "a1", false) },
			target: "a1", want: result{role: models.RoleAdmin},
		},
		{
			name:   "disable: disabled caller is refused",
			users:  []models.User{admin("c", true), admin("a1", false), user("u1", models.RoleUser)},
			run:    func(r *GORMUserRepository) error { return r.SetDisabled(context.Background(), "c", "u1", true) },
			target: "u1", want: result{role: models.RoleUser}, wantErrIs: models.ErrCallerNotAdmin,
		},
		{
			name:   "delete: other admin",
			users:  []models.User{admin("c", false), admin("a1", false)},
			run:    func(r *GORMUserRepository) error { return r.DeleteGuarded(context.Background(), "c", "a1") },
			target: "a1", want: result{gone: true},
		},
		{
			name:   "delete: last enabled admin is refused",
			users:  []models.User{admin("a1", false), user("u1", models.RoleUser)},
			run:    func(r *GORMUserRepository) error { return r.DeleteGuarded(context.Background(), "a1", "a1") },
			target: "a1", want: result{role: models.RoleAdmin}, wantErrIs: models.ErrLastAdmin,
		},
		{
			name:   "delete: non-admin caller is refused",
			users:  []models.User{user("c", models.RoleUser), user("u1", models.RoleUser)},
			run:    func(r *GORMUserRepository) error { return r.DeleteGuarded(context.Background(), "c", "u1") },
			target: "u1", want: result{role: models.RoleUser}, wantErrIs: models.ErrCallerNotAdmin,
		},
		{
			name:      "delete: unknown user",
			users:     []models.User{admin("c", false)},
			run:       func(r *GORMUserRepository) error { return r.DeleteGuarded(context.Background(), "c", "ghost") },
			wantErrIs: dberrors.ErrNotFound,
		},
		{
			name:   "caller ID compares without case",
			users:  []models.User{admin("ADMIN-1", false), user("u1", models.RoleUser)},
			run:    func(r *GORMUserRepository) error { return r.SetDisabled(context.Background(), "admin-1", "u1", true) },
			target: "u1", want: result{role: models.RoleUser, disabled: true},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := setupUserRepo(t)
			for i := range tt.users {
				require.NoError(t, repo.Create(context.Background(), &tt.users[i]))
			}

			err := tt.run(repo)
			if tt.wantErrIs != nil {
				require.ErrorIs(t, err, tt.wantErrIs)
			} else {
				require.NoError(t, err)
			}
			if tt.target == "" {
				return
			}
			u, err := repo.FindByID(context.Background(), tt.target)
			if tt.want.gone {
				require.ErrorIs(t, err, dberrors.ErrNotFound)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want.role, u.Role)
			assert.Equal(t, tt.want.disabled, u.Disabled)
		})
	}
}

func TestUserRepositoryTargetedUpdates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		user      models.User
		run       func(r *GORMUserRepository) error
		want      models.User // Role, Disabled, Email, DisplayName, AuthProvider, PasswordHash checked
		wantErrIs error
	}{
		{
			name: "password update keeps role and disabled",
			user: models.User{ID: "u1", Username: "u1", Role: models.RoleDevOps, Disabled: true, PasswordHash: "old"},
			run:  func(r *GORMUserRepository) error { return r.UpdatePassword(context.Background(), "u1", "new") },
			want: models.User{Role: models.RoleDevOps, Disabled: true, PasswordHash: "new", AuthProvider: models.AuthProviderLocal},
		},
		{
			name:      "password update of an unknown user",
			user:      models.User{ID: "u1", Username: "u1", Role: models.RoleUser},
			run:       func(r *GORMUserRepository) error { return r.UpdatePassword(context.Background(), "ghost", "new") },
			want:      models.User{Role: models.RoleUser, AuthProvider: models.AuthProviderLocal},
			wantErrIs: dberrors.ErrNotFound,
		},
		{
			name: "profile update of an SSO user sets the role and keeps disabled",
			user: models.User{ID: "u1", Username: "u1", Role: models.RoleUser, AuthProvider: models.AuthProviderOIDC, Disabled: true},
			run: func(r *GORMUserRepository) error {
				return r.UpdateProfile(context.Background(), "u1", models.UserProfileUpdate{Email: strPtr("a@example.com"), DisplayName: strPtr("A"), Role: strPtr(models.RoleDevOps)})
			},
			want: models.User{Role: models.RoleDevOps, Disabled: true, Email: "a@example.com", DisplayName: "A", AuthProvider: models.AuthProviderOIDC},
		},
		{
			name: "profile update never sets the role of a local user",
			user: models.User{ID: "u1", Username: "u1", Role: models.RoleUser, PasswordHash: "h"},
			run: func(r *GORMUserRepository) error {
				return r.UpdateProfile(context.Background(), "u1", models.UserProfileUpdate{Role: strPtr(models.RoleAdmin)})
			},
			want: models.User{Role: models.RoleUser, AuthProvider: models.AuthProviderLocal, PasswordHash: "h"},
		},
		{
			name: "profile update with an unknown role is refused",
			user: models.User{ID: "u1", Username: "u1", Role: models.RoleUser, AuthProvider: models.AuthProviderOIDC},
			run: func(r *GORMUserRepository) error {
				return r.UpdateProfile(context.Background(), "u1", models.UserProfileUpdate{Role: strPtr("root")})
			},
			want:      models.User{Role: models.RoleUser, AuthProvider: models.AuthProviderOIDC},
			wantErrIs: dberrors.ErrValidation,
		},
		{
			name: "profile update with a bad email is refused",
			user: models.User{ID: "u1", Username: "u1", Role: models.RoleUser},
			run: func(r *GORMUserRepository) error {
				return r.UpdateProfile(context.Background(), "u1", models.UserProfileUpdate{Email: strPtr("nope")})
			},
			want:      models.User{Role: models.RoleUser, AuthProvider: models.AuthProviderLocal},
			wantErrIs: dberrors.ErrValidation,
		},
		{
			name: "link to a provider sets the role and clears the password",
			user: models.User{ID: "u1", Username: "u1", Role: models.RoleUser, PasswordHash: "h", Disabled: true},
			run: func(r *GORMUserRepository) error {
				return r.UpdateProfile(context.Background(), "u1", models.UserProfileUpdate{LinkProvider: strPtr(models.AuthProviderOIDC), ExternalID: strPtr("sub-1"), Role: strPtr(models.RoleDevOps)})
			},
			want: models.User{Role: models.RoleDevOps, AuthProvider: models.AuthProviderOIDC, Disabled: true},
		},
		{
			name: "link to an empty provider is refused",
			user: models.User{ID: "u1", Username: "u1", Role: models.RoleUser, PasswordHash: "h"},
			run: func(r *GORMUserRepository) error {
				return r.UpdateProfile(context.Background(), "u1", models.UserProfileUpdate{LinkProvider: strPtr(""), ExternalID: strPtr("sub-1")})
			},
			want:      models.User{Role: models.RoleUser, AuthProvider: models.AuthProviderLocal, PasswordHash: "h"},
			wantErrIs: dberrors.ErrValidation,
		},
		{
			name: "link to an unknown provider is refused",
			user: models.User{ID: "u1", Username: "u1", Role: models.RoleUser, PasswordHash: "h"},
			run: func(r *GORMUserRepository) error {
				return r.UpdateProfile(context.Background(), "u1", models.UserProfileUpdate{LinkProvider: strPtr("saml"), ExternalID: strPtr("sub-1")})
			},
			want:      models.User{Role: models.RoleUser, AuthProvider: models.AuthProviderLocal, PasswordHash: "h"},
			wantErrIs: dberrors.ErrValidation,
		},
		{
			name: "link to the local provider is refused",
			user: models.User{ID: "u1", Username: "u1", Role: models.RoleUser, PasswordHash: "h"},
			run: func(r *GORMUserRepository) error {
				return r.UpdateProfile(context.Background(), "u1", models.UserProfileUpdate{LinkProvider: strPtr(models.AuthProviderLocal), ExternalID: strPtr("sub-1")})
			},
			want:      models.User{Role: models.RoleUser, AuthProvider: models.AuthProviderLocal, PasswordHash: "h"},
			wantErrIs: dberrors.ErrValidation,
		},
		{
			name: "link without an external ID is refused",
			user: models.User{ID: "u1", Username: "u1", Role: models.RoleUser, PasswordHash: "h"},
			run: func(r *GORMUserRepository) error {
				return r.UpdateProfile(context.Background(), "u1", models.UserProfileUpdate{LinkProvider: strPtr(models.AuthProviderOIDC)})
			},
			want:      models.User{Role: models.RoleUser, AuthProvider: models.AuthProviderLocal, PasswordHash: "h"},
			wantErrIs: dberrors.ErrValidation,
		},
		{
			name: "link with an empty external ID is refused",
			user: models.User{ID: "u1", Username: "u1", Role: models.RoleUser, PasswordHash: "h"},
			run: func(r *GORMUserRepository) error {
				return r.UpdateProfile(context.Background(), "u1", models.UserProfileUpdate{LinkProvider: strPtr(models.AuthProviderOIDC), ExternalID: strPtr("")})
			},
			want:      models.User{Role: models.RoleUser, AuthProvider: models.AuthProviderLocal, PasswordHash: "h"},
			wantErrIs: dberrors.ErrValidation,
		},
		{
			name: "link with an external ID longer than the column is refused",
			user: models.User{ID: "u1", Username: "u1", Role: models.RoleUser, PasswordHash: "h"},
			run: func(r *GORMUserRepository) error {
				return r.UpdateProfile(context.Background(), "u1", models.UserProfileUpdate{LinkProvider: strPtr(models.AuthProviderOIDC), ExternalID: strPtr(strings.Repeat("x", models.MaxExternalIDLen+1))})
			},
			want:      models.User{Role: models.RoleUser, AuthProvider: models.AuthProviderLocal, PasswordHash: "h"},
			wantErrIs: dberrors.ErrValidation,
		},
		{
			name: "an external ID without a provider is refused",
			user: models.User{ID: "u1", Username: "u1", Role: models.RoleUser, PasswordHash: "h"},
			run: func(r *GORMUserRepository) error {
				return r.UpdateProfile(context.Background(), "u1", models.UserProfileUpdate{ExternalID: strPtr("sub-1")})
			},
			want:      models.User{Role: models.RoleUser, AuthProvider: models.AuthProviderLocal, PasswordHash: "h"},
			wantErrIs: dberrors.ErrValidation,
		},
		{
			name: "profile update of an unknown user",
			user: models.User{ID: "u1", Username: "u1", Role: models.RoleUser},
			run: func(r *GORMUserRepository) error {
				return r.UpdateProfile(context.Background(), "ghost", models.UserProfileUpdate{DisplayName: strPtr("x")})
			},
			want:      models.User{Role: models.RoleUser, AuthProvider: models.AuthProviderLocal},
			wantErrIs: dberrors.ErrNotFound,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := setupUserRepo(t)
			u := tt.user
			require.NoError(t, repo.Create(context.Background(), &u))

			err := tt.run(repo)
			if tt.wantErrIs != nil {
				require.ErrorIs(t, err, tt.wantErrIs)
			} else {
				require.NoError(t, err)
			}
			got, err := repo.FindByID(context.Background(), "u1")
			require.NoError(t, err)
			assert.Equal(t, tt.want.Role, got.Role)
			assert.Equal(t, tt.want.Disabled, got.Disabled)
			assert.Equal(t, tt.want.Email, got.Email)
			assert.Equal(t, tt.want.DisplayName, got.DisplayName)
			assert.Equal(t, tt.want.AuthProvider, got.AuthProvider)
			assert.Equal(t, tt.want.PasswordHash, got.PasswordHash)
		})
	}
}

func TestUserRepositoryLinkProviderDuplicateExternalID(t *testing.T) {
	t.Parallel()
	repo := setupUserRepo(t)
	ctx := context.Background()
	require.NoError(t, repo.Create(ctx, &models.User{ID: "sso", Username: "sso", Role: models.RoleUser, AuthProvider: models.AuthProviderOIDC, ExternalID: strPtr("sub-1")}))
	require.NoError(t, repo.Create(ctx, &models.User{ID: "local", Username: "local", Role: models.RoleUser}))

	err := repo.UpdateProfile(ctx, "local", models.UserProfileUpdate{LinkProvider: strPtr(models.AuthProviderOIDC), ExternalID: strPtr("sub-1")})

	require.ErrorIs(t, err, dberrors.ErrDuplicateKey)
}

func TestUserRepositoryCancelledContext(t *testing.T) {
	t.Parallel()
	repo := setupUserRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := repo.FindByUsername(ctx, "alice")
	require.ErrorIs(t, err, context.Canceled)

	err = repo.Create(ctx, &models.User{Username: "alice", Role: models.RoleUser})
	require.ErrorIs(t, err, context.Canceled)
}
