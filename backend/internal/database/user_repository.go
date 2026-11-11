package database

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Compile-time interface check.
var _ models.UserRepository = (*GORMUserRepository)(nil)

// userPublicColumns are the users columns without password_hash: the
// columns of a list or batch lookup.
var userPublicColumns = []string{
	"id", "username", "display_name", "role", "auth_provider", "external_id",
	"email", "disabled", "service_account", "created_at", "updated_at",
}

// GORMUserRepository implements models.UserRepository using GORM.
type GORMUserRepository struct {
	*BaseRepository
}

// NewGORMUserRepository creates a new GORM-backed user repository.
func NewGORMUserRepository(db *gorm.DB) *GORMUserRepository {
	return &GORMUserRepository{BaseRepository: NewBaseRepository(db)}
}

// Create validates and inserts a user. See models.UserRepository.
func (r *GORMUserRepository) Create(ctx context.Context, user *models.User) error {
	if user.Role == "" {
		user.Role = models.RoleUser
	}
	if user.AuthProvider == "" {
		user.AuthProvider = models.AuthProviderLocal
	}
	if err := user.Validate(); err != nil {
		return validationError("create", err)
	}
	if user.ID == "" {
		user.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	user.CreatedAt = now
	user.UpdatedAt = now
	err := r.Write(ctx, func(db *gorm.DB) error { return db.Create(user).Error })
	return repoError(ctx, "create", err)
}

// FindByID returns a user by ID.
func (r *GORMUserRepository) FindByID(ctx context.Context, id string) (*models.User, error) {
	return r.findOne(ctx, "find_by_id", "id = ?", id)
}

// FindByUsername returns a user by username.
func (r *GORMUserRepository) FindByUsername(ctx context.Context, username string) (*models.User, error) {
	return r.findOne(ctx, "find_by_username", "username = ?", username)
}

// FindByExternalID returns a user by auth provider and external ID.
func (r *GORMUserRepository) FindByExternalID(ctx context.Context, provider, externalID string) (*models.User, error) {
	return r.findOne(ctx, "find_by_external_id", "auth_provider = ? AND external_id = ?", provider, externalID)
}

func (r *GORMUserRepository) findOne(ctx context.Context, op, query string, args ...any) (*models.User, error) {
	var user models.User
	if err := r.DB.WithContext(ctx).Where(query, args...).Take(&user).Error; err != nil {
		return nil, repoError(ctx, op, err)
	}
	return &user, nil
}

// FindByIDs returns the users with these IDs by ID, in a single query,
// without the password hash.
func (r *GORMUserRepository) FindByIDs(ctx context.Context, ids []string) (map[string]*models.User, error) {
	if len(ids) == 0 {
		return make(map[string]*models.User), nil
	}
	var users []models.User
	if err := r.DB.WithContext(ctx).Select(userPublicColumns).Where("id IN ?", ids).Find(&users).Error; err != nil {
		return nil, repoError(ctx, "find_by_ids", err)
	}
	result := make(map[string]*models.User, len(users))
	for i := range users {
		result[users[i].ID] = &users[i]
	}
	return result, nil
}

// Delete removes a user by ID, without the last-admin guard.
func (r *GORMUserRepository) Delete(ctx context.Context, id string) error {
	var affected int64
	err := r.Write(ctx, func(db *gorm.DB) error {
		res := db.Where("id = ?", id).Delete(&models.User{})
		affected = res.RowsAffected
		return res.Error
	})
	if err != nil {
		return repoError(ctx, "delete", err)
	}
	if affected == 0 {
		return dberrors.NewDatabaseError("delete", dberrors.ErrNotFound)
	}
	return nil
}

// Count returns the total number of users.
func (r *GORMUserRepository) Count(ctx context.Context) (int64, error) {
	var count int64
	if err := r.DB.WithContext(ctx).Model(&models.User{}).Count(&count).Error; err != nil {
		return 0, repoError(ctx, "count", err)
	}
	return count, nil
}

// userFilterWhere applies the filter to a query on the users table. The count and the
// page query both use it.
func userFilterWhere(q *gorm.DB, f models.UserFilter) *gorm.DB {
	if f.Role != "" {
		q = q.Where("role = ?", f.Role)
	}
	if f.Search != "" {
		p := containsPattern(f.Search)
		q = q.Where("(username LIKE ? ESCAPE '!' OR display_name LIKE ? ESCAPE '!' OR email LIKE ? ESCAPE '!')", p, p, p)
	}
	return q
}

// ListPaged returns one page of users and the number of matches. See
// models.UserRepository.
func (r *GORMUserRepository) ListPaged(ctx context.Context, filter models.UserFilter, limit, offset int) ([]models.User, int64, error) {
	db := r.DB.WithContext(ctx)
	var total int64
	if err := userFilterWhere(db.Model(&models.User{}), filter).Count(&total).Error; err != nil {
		return nil, 0, repoError(ctx, "list_paged", err)
	}
	q := userFilterWhere(db.Model(&models.User{}), filter).Select(userPublicColumns).Order("username ASC").Order("id ASC")
	if limit > 0 {
		q = q.Limit(limit)
		if offset > 0 {
			q = q.Offset(offset)
		}
	}
	users := []models.User{}
	if err := q.Find(&users).Error; err != nil {
		return nil, 0, repoError(ctx, "list_paged", err)
	}
	return users, total, nil
}

// guardTarget is the locked target row of a guarded admin change.
type guardTarget struct {
	user models.User
	// otherEnabledAdmins counts the enabled admins other than the target.
	otherEnabledAdmins int
}

// wouldRemoveLastAdmin reports whether removing the target from the enabled
// admins (demote, disable, delete) leaves none.
func (g guardTarget) wouldRemoveLastAdmin() bool {
	return g.user.Role == models.RoleAdmin && !g.user.Disabled && g.otherEnabledAdmins == 0
}

// lockAdminRows locks the admin rows, ordered by ID. A transaction that
// changes a role must call it before it locks any user row.
func lockAdminRows(tx *gorm.DB) ([]models.User, error) {
	var admins []models.User
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id", "disabled").
		Where("role = ?", models.RoleAdmin).
		Order("id").
		Find(&admins).Error
	return admins, err
}

// guardedAdminChange runs fn in a transaction for a change that an admin
// makes to a user.
//
// Locking: the transaction first locks all admin rows (ordered by ID), then
// the target row. Every guarded change takes the admin locks in the same
// order, so two admins who demote, disable or delete each other at the same
// time are serialized: the second one sees the result of the first one.
// Inside the lock, the caller must still be an enabled admin
// (ErrCallerNotAdmin). IDs compare without case, as the MySQL collation does.
//
// A deadlock (next-key locks on idx_users_role) runs the whole transaction
// again (BaseRepository.Tx), so fn must only write through tx.
func (r *GORMUserRepository) guardedAdminChange(ctx context.Context, op, callerID, id string, fn func(tx *gorm.DB, t guardTarget) error) error {
	err := r.Tx(ctx, func(tx *gorm.DB) error {
		admins, err := lockAdminRows(tx)
		if err != nil {
			return err
		}

		callerOK := false
		for _, a := range admins {
			if strings.EqualFold(a.ID, callerID) && !a.Disabled {
				callerOK = true
				break
			}
		}
		if !callerOK {
			return models.ErrCallerNotAdmin
		}

		var target models.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Take(&target).Error; err != nil {
			return err
		}

		g := guardTarget{user: target}
		for _, a := range admins {
			if !strings.EqualFold(a.ID, target.ID) && !a.Disabled {
				g.otherEnabledAdmins++
			}
		}
		return fn(tx, g)
	})
	return repoError(ctx, op, err)
}

// UpdateRole sets the role of a user and returns the previous role. See
// models.UserRepository.
func (r *GORMUserRepository) UpdateRole(ctx context.Context, callerID, id, role string) (string, error) {
	if !models.IsValidRole(role) {
		return "", validationError("update_role", models.ErrInvalidRole)
	}
	var oldRole string
	err := r.guardedAdminChange(ctx, "update_role", callerID, id, func(tx *gorm.DB, g guardTarget) error {
		oldRole = g.user.Role
		if oldRole == role {
			return nil
		}
		if role != models.RoleAdmin && g.wouldRemoveLastAdmin() {
			return models.ErrLastAdmin
		}
		return updateUserColumns(tx, g.user.ID, map[string]any{"role": role})
	})
	if err != nil {
		return "", err
	}
	return oldRole, nil
}

// SetDisabled disables or enables a user. See models.UserRepository.
func (r *GORMUserRepository) SetDisabled(ctx context.Context, callerID, id string, disabled bool) error {
	return r.guardedAdminChange(ctx, "set_disabled", callerID, id, func(tx *gorm.DB, g guardTarget) error {
		if disabled && g.wouldRemoveLastAdmin() {
			return models.ErrLastAdmin
		}
		return updateUserColumns(tx, g.user.ID, map[string]any{"disabled": disabled})
	})
}

// DeleteGuarded deletes a user. See models.UserRepository.
func (r *GORMUserRepository) DeleteGuarded(ctx context.Context, callerID, id string) error {
	return r.guardedAdminChange(ctx, "delete", callerID, id, func(tx *gorm.DB, g guardTarget) error {
		if g.wouldRemoveLastAdmin() {
			return models.ErrLastAdmin
		}
		return tx.Where("id = ?", g.user.ID).Delete(&models.User{}).Error
	})
}

func updateUserColumns(tx *gorm.DB, id string, cols map[string]any) error {
	cols["updated_at"] = time.Now().UTC()
	return tx.Model(&models.User{}).Where("id = ?", id).Updates(cols).Error
}

// UpdatePassword sets only the password hash. See models.UserRepository.
func (r *GORMUserRepository) UpdatePassword(ctx context.Context, id, passwordHash string) error {
	var affected int64
	err := r.Write(ctx, func(db *gorm.DB) error {
		res := db.Model(&models.User{}).Where("id = ?", id).Updates(map[string]any{
			"password_hash": passwordHash,
			"updated_at":    time.Now().UTC(),
		})
		affected = res.RowsAffected
		return res.Error
	})
	if err != nil {
		return repoError(ctx, "update_password", err)
	}
	if affected == 0 {
		return dberrors.NewDatabaseError("update_password", dberrors.ErrNotFound)
	}
	return nil
}

// validateProviderLink checks the link part of a profile update. A link needs
// a known identity provider (not empty, not local, not an unknown name) and a
// non-empty external ID that fits the column; an external ID without a
// provider is refused, because it would be written to a local user's row.
func validateProviderLink(upd models.UserProfileUpdate) error {
	if upd.LinkProvider == nil {
		if upd.ExternalID != nil {
			return models.ErrInvalidExternalID
		}
		return nil
	}
	if !models.IsSSOProvider(*upd.LinkProvider) {
		return models.ErrInvalidAuthProvider
	}
	if upd.ExternalID == nil || *upd.ExternalID == "" || utf8.RuneCountInString(*upd.ExternalID) > models.MaxExternalIDLen {
		return models.ErrInvalidExternalID
	}
	return nil
}

// UpdateProfile sets only the columns listed in upd. See
// models.UserRepository.
func (r *GORMUserRepository) UpdateProfile(ctx context.Context, id string, upd models.UserProfileUpdate) error {
	if upd.Role != nil && !models.IsValidRole(*upd.Role) {
		return validationError("update_profile", models.ErrInvalidRole)
	}
	if upd.Email != nil && *upd.Email != "" && !models.IsValidEmail(*upd.Email) {
		return validationError("update_profile", models.ErrInvalidEmail)
	}
	if err := validateProviderLink(upd); err != nil {
		return validationError("update_profile", err)
	}
	err := r.Tx(ctx, func(tx *gorm.DB) error {
		if upd.Role != nil {
			// Same lock order as guardedAdminChange: without it, the two
			// deadlock on every overlap.
			if _, err := lockAdminRows(tx); err != nil {
				return err
			}
		}
		cols := map[string]any{"updated_at": time.Now().UTC()}
		if upd.Email != nil {
			cols["email"] = *upd.Email
		}
		if upd.DisplayName != nil {
			cols["display_name"] = *upd.DisplayName
		}
		if upd.LinkProvider != nil {
			cols["auth_provider"] = *upd.LinkProvider
			cols["external_id"] = upd.ExternalID
			cols["password_hash"] = ""
		}
		res := tx.Model(&models.User{}).Where("id = ?", id).Updates(cols)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		if upd.Role != nil {
			// Only an SSO user takes the role from the identity provider.
			return tx.Model(&models.User{}).
				Where("id = ? AND auth_provider <> ? AND auth_provider <> ?", id, "", models.AuthProviderLocal).
				Updates(map[string]any{"role": *upd.Role}).Error
		}
		return nil
	})
	return repoError(ctx, "update_profile", err)
}
