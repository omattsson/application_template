package models

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"backend/pkg/dberrors"

	"gorm.io/gorm"
)

// Base model with common fields
type Base struct {
	ID        uint       `gorm:"primarykey" json:"id" example:"1" description:"@Description Unique identifier"`
	CreatedAt time.Time  `json:"created_at" example:"2025-06-02T10:00:00Z" description:"@Description Creation timestamp"`
	UpdatedAt time.Time  `json:"updated_at" example:"2025-06-02T10:00:00Z" description:"@Description Last update timestamp"`
	DeletedAt *time.Time `gorm:"index" json:"deleted_at,omitempty" format:"date-time" description:"@Description Soft delete timestamp"`
}

// Item represents a basic item in the system
type Item struct {
	Base
	Name    string  `gorm:"size:255;not null" json:"name"`
	Price   float64 `json:"price"`
	Version uint    `gorm:"not null;default:1" json:"version"` // For optimistic locking (1 = initial; 0 = not provided)
}

// User represents a user in the system
type User struct {
	Base
	Username string `gorm:"size:255;not null;unique" json:"username"`
	Email    string `gorm:"size:255;not null;unique" json:"email"`
	Name     string `gorm:"size:255" json:"name"`
}

// Validator is an interface for model validation
type Validator interface {
	Validate() error
}

// Versionable is an interface for models that support optimistic locking.
type Versionable interface {
	GetVersion() uint
	SetVersion(v uint)
}

// GetVersion implements Versionable for Item.
func (i *Item) GetVersion() uint { return i.Version }

// SetVersion implements Versionable for Item.
func (i *Item) SetVersion(v uint) { i.Version = v }

// Repository defines the interface for database operations.
type Repository interface {
	Create(ctx context.Context, entity interface{}) error
	FindByID(ctx context.Context, id uint, dest interface{}) error
	Update(ctx context.Context, entity interface{}) error
	Delete(ctx context.Context, entity interface{}) error
	List(ctx context.Context, dest interface{}, conditions ...interface{}) error
	// ListPaged fills dest (a *[]Item) with one page of the items that match
	// filter and returns the number of items that match, not the page size.
	// limit <= 0 returns all matches; offset applies only with a limit.
	ListPaged(ctx context.Context, dest interface{}, limit, offset int, filter ItemFilter) (total int64, err error)
	Ping(ctx context.Context) error
	Close() error
}

// GenericRepository implements the Repository interface
type GenericRepository struct {
	db                  *gorm.DB
	allowedFilterFields map[string]bool
}

// NewRepository creates a new GenericRepository with filter fields for the Item
// entity ("name", "price"). For other entity types, use NewRepositoryWithFilterFields.
func NewRepository(db *gorm.DB) Repository {
	return &GenericRepository{
		db: db,
		allowedFilterFields: map[string]bool{
			"name":  true,
			"price": true,
		},
	}
}

// NewRepositoryWithFilterFields creates a GenericRepository with a custom filter field whitelist.
func NewRepositoryWithFilterFields(db *gorm.DB, fields []string) Repository {
	allowed := make(map[string]bool, len(fields))
	for _, f := range fields {
		allowed[f] = true
	}
	return &GenericRepository{db: db, allowedFilterFields: allowed}
}

// Ping checks if the database is reachable
func (r *GenericRepository) Ping(ctx context.Context) error {
	sqlDB, err := r.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.PingContext(ctx)
}

// Close releases the underlying database connection pool.
func (r *GenericRepository) Close() error {
	sqlDB, err := r.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func (r *GenericRepository) Create(ctx context.Context, entity interface{}) error {
	if v, ok := entity.(Validator); ok {
		if err := v.Validate(); err != nil {
			return dberrors.NewDatabaseError("validate",
				fmt.Errorf("%w: %s", dberrors.ErrValidation, err.Error()))
		}
	}

	return retryOnLock(ctx, func() error {
		if err := r.db.WithContext(ctx).Create(entity).Error; err != nil {
			return r.handleError("create", err)
		}
		return nil
	})
}

func (r *GenericRepository) FindByID(ctx context.Context, id uint, dest interface{}) error {
	if err := r.db.WithContext(ctx).First(dest, id).Error; err != nil {
		return r.handleError("find", err)
	}
	return nil
}

func (r *GenericRepository) Update(ctx context.Context, entity interface{}) error {
	if v, ok := entity.(Validator); ok {
		if err := v.Validate(); err != nil {
			return dberrors.NewDatabaseError("validate",
				fmt.Errorf("%w: %s", dberrors.ErrValidation, err.Error()))
		}
	}

	// Optimistic locking for Versionable entities.
	// We increment the version optimistically, then issue an UPDATE with a
	// WHERE version=old clause. If no rows are affected, it means another
	// transaction modified this entity — we roll back the in-memory version
	// and return a version-mismatch error.
	// We use Model().Where().Select("*").Updates() instead of Where().Save()
	// because Save() may generate an upsert (INSERT … ON CONFLICT) on some
	// dialects (e.g. SQLite), which bypasses the WHERE version clause.
	// Select("*") ensures all columns are written, matching Save() semantics.
	if ver, ok := entity.(Versionable); ok {
		currentVersion := ver.GetVersion()
		// Each attempt is one UPDATE statement that InnoDB rolls back as a whole
		// on a deadlock, and it resets the in-memory version on every failure,
		// so a retry increments the version once. A version mismatch is not a
		// lock error: it is returned at once.
		return retryOnLock(ctx, func() error {
			ver.SetVersion(currentVersion + 1)
			result := r.db.WithContext(ctx).
				Model(entity).
				Where("version = ?", currentVersion).
				Select("*").
				Updates(entity)
			if result.Error != nil {
				ver.SetVersion(currentVersion) // Roll back version on error
				return r.handleError("update", result.Error)
			}
			if result.RowsAffected == 0 {
				ver.SetVersion(currentVersion) // Roll back version on mismatch
				return dberrors.NewDatabaseError("update", errors.New("version mismatch"))
			}
			return nil
		})
	}

	return retryOnLock(ctx, func() error {
		if err := r.db.WithContext(ctx).Save(entity).Error; err != nil {
			return r.handleError("update", err)
		}
		return nil
	})
}

func (r *GenericRepository) Delete(ctx context.Context, entity interface{}) error {
	var rowsAffected int64
	err := retryOnLock(ctx, func() error {
		result := r.db.WithContext(ctx).Delete(entity)
		if result.Error != nil {
			return r.handleError("delete", result.Error)
		}
		rowsAffected = result.RowsAffected
		return nil
	})
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return dberrors.NewDatabaseError("delete", dberrors.ErrNotFound)
	}
	return nil
}

func (r *GenericRepository) List(ctx context.Context, dest interface{}, conditions ...interface{}) error {
	query := r.db.WithContext(ctx)
	for _, cond := range conditions {
		switch c := cond.(type) {
		case Filter:
			if !r.allowedFilterFields[c.Field] {
				return dberrors.NewDatabaseError("list",
					fmt.Errorf("invalid filter field: %q", c.Field))
			}
			// SAFETY: c.Field is interpolated into fmt.Sprintf below, but it is
			// guaranteed to be one of the whitelisted column names checked above,
			// so SQL injection via field names is not possible.
			switch c.Op {
			case "exact":
				query = query.Where(fmt.Sprintf("%s = ?", c.Field), c.Value)
			case ">=":
				query = query.Where(fmt.Sprintf("%s >= ?", c.Field), c.Value)
			case "<=":
				query = query.Where(fmt.Sprintf("%s <= ?", c.Field), c.Value)
			default:
				// Default to LIKE for substring matching.
				// Escape SQL wildcards (% and _) so they are treated as literals.
				escaped := strings.NewReplacer("%", "\\%", "_", "\\_").Replace(fmt.Sprint(c.Value))
				query = query.Where(fmt.Sprintf("%s LIKE ?", c.Field), "%"+escaped+"%")
			}
		case Pagination:
			if c.Limit > 0 {
				query = query.Limit(c.Limit)
			}
			if c.Offset > 0 {
				query = query.Offset(c.Offset)
			}
		}
	}
	if err := query.Find(dest).Error; err != nil {
		return r.handleError("list", err)
	}
	return nil
}

// ItemFilter selects items for ListPaged. A zero field does not filter.
type ItemFilter struct {
	// Name matches items whose name contains it (case-insensitive on the
	// default MySQL collation). % and _ are literals.
	Name string
	// NameExact matches the whole name and takes precedence over Name.
	NameExact string
	// MinPrice and MaxPrice bound the price (inclusive) when greater than 0.
	MinPrice float64
	MaxPrice float64
}

// itemListColumns are the columns of an item list row, in place of SELECT *.
var itemListColumns = []string{"id", "name", "price", "version", "created_at", "updated_at", "deleted_at"}

// likeEscaper escapes the LIKE wildcards. "!" is the escape character, which
// works the same on MySQL and SQLite (a backslash needs different quoting).
var likeEscaper = strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")

// where applies the filter to a query on the items table. The count query and
// the page query both use it, so the total counts the matching rows.
func (f ItemFilter) where(q *gorm.DB) *gorm.DB {
	switch {
	case f.NameExact != "":
		q = q.Where("name = ?", f.NameExact)
	case f.Name != "":
		q = q.Where("name LIKE ? ESCAPE '!'", "%"+likeEscaper.Replace(f.Name)+"%")
	}
	if f.MinPrice > 0 {
		q = q.Where("price >= ?", f.MinPrice)
	}
	if f.MaxPrice > 0 {
		q = q.Where("price <= ?", f.MaxPrice)
	}
	return q
}

// ListPaged returns one page of items that match filter, ordered by id, and
// the number of all items that match. dest must be a *[]Item.
func (r *GenericRepository) ListPaged(ctx context.Context, dest interface{}, limit, offset int, filter ItemFilter) (int64, error) {
	if _, ok := dest.(*[]Item); !ok {
		return 0, dberrors.NewDatabaseError("list_paged", errors.New("destination must be *[]Item"))
	}

	var total int64
	if err := filter.where(r.db.WithContext(ctx).Model(&Item{})).Count(&total).Error; err != nil {
		return 0, r.handleError("list_paged", err)
	}

	query := filter.where(r.db.WithContext(ctx).Model(&Item{})).
		Select(itemListColumns).
		Order("id ASC")
	if limit > 0 {
		query = query.Limit(limit)
		if offset > 0 {
			query = query.Offset(offset)
		}
	}
	if err := query.Find(dest).Error; err != nil {
		return 0, r.handleError("list_paged", err)
	}
	return total, nil
}

// handleError translates database errors into our custom error types
func (r *GenericRepository) handleError(op string, err error) error {
	if err == nil {
		return nil
	}

	switch {
	case err == gorm.ErrRecordNotFound:
		return dberrors.NewDatabaseError(op, dberrors.ErrNotFound)
	case strings.Contains(err.Error(), "Duplicate entry"):
		return dberrors.NewDatabaseError(op, dberrors.ErrDuplicateKey)
	case strings.Contains(err.Error(), "validation failed"):
		return dberrors.NewDatabaseError(op, dberrors.ErrValidation)
	default:
		return dberrors.NewDatabaseError(op, err)
	}
}

// Filter represents a filter condition for queries
type Filter struct {
	Field string      `json:"field"`
	Op    string      `json:"op,omitempty"`
	Value interface{} `json:"value"`
}

// Pagination represents pagination parameters for queries
type Pagination struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}
