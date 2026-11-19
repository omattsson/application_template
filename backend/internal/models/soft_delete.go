package models

import (
	"context"
	"errors"
	"reflect"
	"time"

	"backend/pkg/dberrors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// softDeletedAtFormat is the layout of the purge cutoff: UTC with millisecond
// precision, the same text on MySQL (datetime(3)) and SQLite.
const softDeletedAtFormat = "2006-01-02 15:04:05.000"

// SoftDeleter is the part of a soft delete repository that the plain
// Repository interface does not have. A caller type-asserts it and then checks
// SoftDeleteEnabled, because the assertion always succeeds:
//
//	if g, ok := repo.(*models.GenericRepository); ok && g.SoftDeleteEnabled() { ... }
//
// GenericRepository implements it always, but both methods return
// dberrors.ErrNotImplemented unless the repository was built with
// WithSoftDelete or WithSoftDeleteOf, so a repository that hard deletes never
// pretends to restore or purge.
type SoftDeleter interface {
	// Restore clears deleted_at of the row id. The row must be deleted:
	// dberrors.ErrNotFound for a missing or a live row, dberrors.ErrDuplicateKey
	// when a unique index now conflicts with a live row (see EXTENDING.md,
	// "Adding soft delete to an entity").
	Restore(ctx context.Context, id uint) error
	// PurgeDeleted removes for good at most limit rows that were deleted before
	// the cutoff (oldest first) and returns how many it removed. A result below
	// limit means nothing older is left. limit must be positive.
	PurgeDeleted(ctx context.Context, before time.Time, limit int) (int64, error)
}

var _ SoftDeleter = (*GenericRepository)(nil)

// WithSoftDelete makes the repository delete softly: Delete sets deleted_at
// to the database time instead of removing the row, and every read (FindByID,
// ListPaged and its count, the deprecated List, Update) skips the rows that
// have a deleted_at. Restore and PurgeDeleted work on the Item table; use
// WithSoftDeleteOf for another entity.
//
// Without the option nothing changes: the statements are the same as before.
func WithSoftDelete() RepositoryOption {
	return WithSoftDeleteOf(&Item{})
}

// WithSoftDeleteOf is WithSoftDelete for an entity other than Item. model is
// a pointer to the struct (for example &Note{}) that Restore and PurgeDeleted
// work on; the struct embeds Base, so it has the indexed deleted_at column.
// nil means Item. The pointer is only read for its type.
func WithSoftDeleteOf(model interface{}) RepositoryOption {
	if model == nil {
		model = &Item{}
	}
	return func(r *GenericRepository) { r.softModel = model }
}

// SoftDeleteEnabled reports whether the repository was built with
// WithSoftDelete or WithSoftDeleteOf.
func (r *GenericRepository) SoftDeleteEnabled() bool { return r.softModel != nil }

// live adds the condition that hides the deleted rows. It is the only place
// that decides it, and every read and update goes through it, so a query
// cannot forget it. Without the option it returns q as it is.
//
// Base.DeletedAt is a plain *time.Time, not gorm.DeletedAt, so GORM adds no
// condition of its own.
func (r *GenericRepository) live(q *gorm.DB) *gorm.DB {
	if r.softModel == nil {
		return q
	}
	return q.Where("deleted_at IS NULL")
}

// softDeleteNowSQL is the database time in UTC with millisecond precision, a
// constant per dialect (UTC_TIMESTAMP(3) on MySQL, independent of the session
// time zone; an ISO text on SQLite for the unit tests). It is never built
// from input. The database clock keeps the replica clocks out of it.
func softDeleteNowSQL(dialect string) string {
	if dialect == "mysql" {
		return "UTC_TIMESTAMP(3)"
	}
	return "strftime('%Y-%m-%d %H:%M:%f', 'now')"
}

// primaryKeyOf returns the primary key value of entity (a non-nil pointer to a
// struct) and false when it is the zero value or cannot be read. A write that
// is scoped by condition only (deleted_at IS NULL) must never run without it:
// it would touch every live row.
func primaryKeyOf(ctx context.Context, db *gorm.DB, entity interface{}) (interface{}, bool) {
	rv := reflect.Indirect(reflect.ValueOf(entity))
	if !rv.IsValid() || rv.Kind() != reflect.Struct {
		return nil, false
	}
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(entity); err != nil || stmt.Schema == nil || stmt.Schema.PrioritizedPrimaryField == nil {
		return nil, false
	}
	v, isZero := stmt.Schema.PrioritizedPrimaryField.ValueOf(ctx, rv)
	return v, !isZero
}

// clearDeletedAt sets the DeletedAt field (Base.DeletedAt) of entity to nil.
// Create calls it on a soft delete repository: the field binds from JSON, and
// a created row must always be live (a row inserted as deleted is hidden,
// escapes the unique guard of the live rows and can carry a back-dated
// timestamp that skips the retention window). An entity without such a field
// is left as it is.
func clearDeletedAt(entity interface{}) {
	rv := reflect.ValueOf(entity)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return
	}
	f := rv.Elem().FieldByName("DeletedAt")
	if f.IsValid() && f.CanSet() && f.Type() == reflect.TypeOf((*time.Time)(nil)) {
		f.Set(reflect.Zero(f.Type()))
	}
}

// blankOf returns a new zero value of the type behind entity (a pointer to a
// struct). A write that goes through it cannot change the caller's entity.
func blankOf(entity interface{}) interface{} {
	return reflect.New(reflect.Indirect(reflect.ValueOf(entity)).Type()).Interface()
}

// softModelBlank returns a new zero value of the model of WithSoftDeleteOf.
func (r *GenericRepository) softModelBlank(op string) (interface{}, error) {
	t := reflect.TypeOf(r.softModel)
	if t == nil || t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
		return nil, dberrors.NewDatabaseError(op, errors.Join(dberrors.ErrValidation,
			errors.New("the soft delete model must be a pointer to a struct")))
	}
	return reflect.New(t.Elem()).Interface(), nil
}

func (r *GenericRepository) requireSoftDelete(op string) error {
	if r.softModel == nil {
		return dberrors.NewDatabaseError(op, errors.Join(dberrors.ErrNotImplemented,
			errors.New("the repository was built without WithSoftDelete")))
	}
	return nil
}

// softDelete is Delete with the option: it sets deleted_at on the live row of
// entity, in the same lock retry (and the same transaction as the change
// hook) as the hard delete. A deleted or missing row is ErrNotFound. The
// caller's entity is not changed.
func (r *GenericRepository) softDelete(ctx context.Context, entity interface{}) error {
	id, ok := primaryKeyOf(ctx, r.db, entity)
	if !ok {
		return dberrors.NewDatabaseError("delete", dberrors.ErrNotFound)
	}
	change := func(tx *gorm.DB) error {
		result := r.live(tx.Model(blankOf(entity))).
			Where(clause.Eq{Column: clause.PrimaryColumn, Value: id}).
			UpdateColumn("deleted_at", gorm.Expr(softDeleteNowSQL(tx.Dialector.Name())))
		if result.Error != nil {
			return r.handleError("delete", result.Error)
		}
		if result.RowsAffected == 0 {
			return dberrors.NewDatabaseError("delete", dberrors.ErrNotFound)
		}
		return nil
	}
	if r.hook != nil {
		return r.writeWithHook(ctx, ChangeDeleted, entity, change)
	}
	return RetryOnLock(ctx, func() error { return change(r.db.WithContext(ctx)) })
}

// softUpdate is Update with the option. It updates the live row only and never
// writes deleted_at (Delete and Restore own it). A deleted or missing row is
// ErrNotFound, also for a Versionable entity: when no row matched, a second
// query tells a stale version (a mismatch, as without the option) from a row
// that is gone. It never falls back to an insert.
func (r *GenericRepository) softUpdate(ctx context.Context, entity interface{}) error {
	if _, ok := primaryKeyOf(ctx, r.db, entity); !ok {
		return dberrors.NewDatabaseError("update", dberrors.ErrNotFound)
	}
	ver, versioned := entity.(Versionable)
	var current uint
	if versioned {
		current = ver.GetVersion()
	}
	change := func(tx *gorm.DB) error {
		q := r.live(tx.Model(entity)).Select("*").Omit("deleted_at")
		if versioned {
			ver.SetVersion(current + 1)
			q = q.Where("version = ?", current)
		}
		result := q.Updates(entity)
		if result.Error != nil {
			if versioned {
				ver.SetVersion(current)
			}
			return r.handleError("update", result.Error)
		}
		if result.RowsAffected > 0 {
			return nil
		}
		// No row changed: the row is gone, the version is stale, or (without a
		// version) the values were the same.
		if versioned {
			ver.SetVersion(current)
		}
		var live int64
		id, _ := primaryKeyOf(ctx, tx, entity)
		err := r.live(tx.Model(blankOf(entity))).
			Where(clause.Eq{Column: clause.PrimaryColumn, Value: id}).
			Count(&live).Error
		if err != nil {
			return r.handleError("update", err)
		}
		switch {
		case live == 0:
			return dberrors.NewDatabaseError("update", dberrors.ErrNotFound)
		case versioned:
			return dberrors.NewDatabaseError("update", errors.New("version mismatch"))
		default:
			return nil
		}
	}
	if r.hook != nil {
		return r.writeWithHook(ctx, ChangeUpdated, entity, change)
	}
	return RetryOnLock(ctx, func() error { return change(r.db.WithContext(ctx)) })
}

// Restore implements SoftDeleter. A row that was restored while a unique index
// now conflicts is ErrDuplicateKey. No change hook runs: a restore is not one
// of the three change kinds.
func (r *GenericRepository) Restore(ctx context.Context, id uint) error {
	if err := r.requireSoftDelete("restore"); err != nil {
		return err
	}
	model, err := r.softModelBlank("restore")
	if err != nil {
		return err
	}
	return RetryOnLock(ctx, func() error {
		result := r.db.WithContext(ctx).Model(model).
			Where(clause.Eq{Column: clause.PrimaryColumn, Value: id}).
			Where("deleted_at IS NOT NULL").
			UpdateColumn("deleted_at", nil)
		if result.Error != nil {
			return r.handleError("restore", result.Error)
		}
		if result.RowsAffected == 0 {
			return dberrors.NewDatabaseError("restore", dberrors.ErrNotFound)
		}
		return nil
	})
}

// PurgeDeleted implements SoftDeleter. It selects the oldest ids first (the
// deleted_at index serves the order) and deletes them by id with the cutoff
// condition repeated, so a row that another replica purged or a caller
// restored in between is skipped, and the count is the rows this call removed.
func (r *GenericRepository) PurgeDeleted(ctx context.Context, before time.Time, limit int) (int64, error) {
	if err := r.requireSoftDelete("purge"); err != nil {
		return 0, err
	}
	if limit <= 0 {
		return 0, dberrors.NewDatabaseError("purge", dberrors.ErrValidation)
	}
	model, err := r.softModelBlank("purge")
	if err != nil {
		return 0, err
	}
	cutoff := before.UTC().Format(softDeletedAtFormat)
	var removed int64
	err = RetryOnLock(ctx, func() error {
		removed = 0
		var ids []uint
		err := r.db.WithContext(ctx).Model(model).
			Where("deleted_at IS NOT NULL AND deleted_at < ?", cutoff).
			Order("deleted_at ASC, id ASC").
			Limit(limit).
			Pluck("id", &ids).Error
		if err != nil {
			return r.handleError("purge", err)
		}
		if len(ids) == 0 {
			return nil
		}
		result := r.db.WithContext(ctx).Unscoped().
			Where("id IN ?", ids).
			Where("deleted_at IS NOT NULL AND deleted_at < ?", cutoff).
			Delete(model)
		if result.Error != nil {
			return r.handleError("purge", result.Error)
		}
		removed = result.RowsAffected
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}
