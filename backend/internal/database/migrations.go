package database

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"backend/internal/database/schema"
	"backend/internal/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AutoMigrate runs database migrations for all models. On MySQL it holds a
// named lock while it runs, so replicas that start at the same time apply
// each migration once: the others wait and then find it applied.
func (d *Database) AutoMigrate() error {
	slog.Info("Running database migrations...")

	err := withMigrationLock(d.DB, func(db *gorm.DB) error {
		// Initialize migrator
		migrator := schema.NewMigrator(db)

		// Add migrations
		registerMigrations(migrator)

		// Run migrations
		return migrator.MigrateUp()
	})
	if err != nil {
		return err
	}

	slog.Info("Database migrations completed successfully")
	return nil
}

// migrationLockTimeout is how long AutoMigrate waits for another process
// that is migrating the same database.
const migrationLockTimeout = 5 * time.Minute

// errMigrationLockTimeout: another process held the migration lock for
// longer than migrationLockTimeout.
var errMigrationLockTimeout = errors.New("timed out waiting for the migration lock")

// withMigrationLock runs fn on one connection that holds the MySQL named lock
// "schema_migrations.<database>" (GET_LOCK is per connection, so fn must run
// every statement on the db it gets). Other dialects (SQLite in unit tests)
// have one process and run fn on db without a lock.
func withMigrationLock(db *gorm.DB, fn func(db *gorm.DB) error) error {
	if db.Dialector.Name() != "mysql" {
		return fn(db)
	}
	return db.Connection(func(pinned *gorm.DB) error {
		// Connection hands over a db whose chained calls share one
		// statement; a new session on the pinned connection keeps each
		// statement separate.
		conn := pinned.Session(&gorm.Session{NewDB: true})
		const name = "LEFT(CONCAT('schema_migrations.', DATABASE()), 64)"
		var got sql.NullInt64
		if err := conn.Raw("SELECT GET_LOCK("+name+", ?)", int(migrationLockTimeout.Seconds())).Row().Scan(&got); err != nil {
			return fmt.Errorf("acquire the migration lock: %w", err)
		}
		if !got.Valid || got.Int64 != 1 {
			return errMigrationLockTimeout
		}
		defer func() {
			var released sql.NullInt64
			if err := conn.Raw("SELECT RELEASE_LOCK(" + name + ")").Row().Scan(&released); err != nil {
				slog.Warn("Failed to release the migration lock; it ends with the connection", "error", err)
			}
		}()
		return fn(conn)
	})
}

// registerMigrations adds every migration, oldest first. A new migration is
// appended with the next version; an applied one is never edited or
// renumbered.
func registerMigrations(migrator *schema.Migrator) {
	migrator.AddMigration(createBaseTablesMigration())
	migrator.AddMigration(addIndexesMigration())
	migrator.AddMigration(updateItemsVersionDefaultMigration())
	migrator.AddMigration(replaceUsersAndAddAuthTablesMigration())
	migrator.AddMigration(createSessionEntriesMigration())
}

// legacyUser is the users table of migrations 20231201000001 and
// 20231201000002 (uint ID, username, email, name). Those migrations use it
// instead of models.User, so they keep their behavior after models.User
// changed; 20260101000001 replaces the table. The fields copy the old
// models.User with its embedded models.Base, so a later change to Base does
// not change them either. The field order is the old column order.
//
//nolint:govet // fieldalignment: the order of the old columns is kept.
type legacyUser struct {
	ID        uint `gorm:"primarykey"`
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time `gorm:"index"`
	Username  string     `gorm:"size:255;not null;unique"`
	Email     string     `gorm:"size:255;not null;unique"`
	Name      string     `gorm:"size:255"`
}

func (legacyUser) TableName() string { return "users" }

func createBaseTablesMigration() schema.Migration {
	return schema.Migration{
		Version:     "20231201000001",
		Name:        "create_base_tables",
		Description: "Create initial user and item tables",
		Up: func(tx *gorm.DB) error {
			return tx.AutoMigrate(&legacyUser{}, &models.Item{})
		},
		Down: func(tx *gorm.DB) error {
			return tx.Migrator().DropTable(&models.Item{}, &legacyUser{})
		},
	}
}

// Example of adding indexes and constraints in a separate migration
func addIndexesMigration() schema.Migration {
	return schema.Migration{
		Version:     "20231201000002",
		Name:        "add_indexes",
		Description: "Add indexes for performance optimization",
		Up: func(tx *gorm.DB) error {
			// Add composite index on items
			var count int64
			tx.Raw("SELECT COUNT(1) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'items' AND index_name = 'idx_items_name_price'").Scan(&count)
			if count == 0 {
				if err := tx.Exec("CREATE INDEX idx_items_name_price ON items(name, price)").Error; err != nil {
					return err
				}
			}

			// Add unique index on email (uniqueness already enforced by schema)
			tx.Raw("SELECT COUNT(1) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'users' AND index_name = 'idx_users_email'").Scan(&count)
			if count == 0 {
				if err := tx.Exec("CREATE INDEX idx_users_email ON users(email)").Error; err != nil {
					return err
				}
			}
			return nil
		},
		Down: func(tx *gorm.DB) error {
			if err := tx.Exec("DROP INDEX idx_items_name_price ON items").Error; err != nil {
				return err
			}
			return tx.Exec("DROP INDEX idx_users_email ON users").Error
		},
	}
}

// Ensure version column exists and update defaults for optimistic locking
func updateItemsVersionDefaultMigration() schema.Migration {
	return schema.Migration{
		Version:     "20231201000003",
		Name:        "update_items_version_default",
		Description: "Add version column if missing, set default to 1, and update existing rows",
		Up: func(tx *gorm.DB) error {
			// Ensure the version column exists (handles cases where migration 000001
			// was applied before the Version field was added to the Item model)
			if err := tx.AutoMigrate(&models.Item{}); err != nil {
				return err
			}
			// Update existing rows that still have the old default of 0 to the new default of 1
			if err := tx.Exec("UPDATE items SET version = 1 WHERE version = 0").Error; err != nil {
				return err
			}
			// Alter column default to 1 (MySQL syntax; SQLite defaults are set via AutoMigrate)
			if tx.Dialector.Name() == "mysql" {
				return tx.Exec("ALTER TABLE items ALTER COLUMN version SET DEFAULT 1").Error
			}
			return nil
		},
		Down: func(tx *gorm.DB) error {
			if tx.Dialector.Name() == "mysql" {
				return tx.Exec("ALTER TABLE items ALTER COLUMN version SET DEFAULT 0").Error
			}
			return nil
		},
	}
}

// The auth tables as 20260101000001 creates them. They are frozen copies of
// models.User, RefreshToken, APIKey and AuditLog, so this migration keeps its
// behavior when a model changes later; a model change needs a new migration
// (TestAuthTablesMatchModels fails without one).

type authUserV1 struct {
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ExternalID     *string `gorm:"size:255;uniqueIndex:idx_users_auth_provider_external_id,priority:2"`
	ID             string  `gorm:"primaryKey;size:36"`
	Username       string  `gorm:"size:100;not null;uniqueIndex:idx_users_username"`
	PasswordHash   string  `gorm:"size:255"`
	DisplayName    string  `gorm:"size:255"`
	Role           string  `gorm:"size:50;not null;index:idx_users_role"`
	AuthProvider   string  `gorm:"size:50;not null;default:local;uniqueIndex:idx_users_auth_provider_external_id,priority:1"`
	Email          string  `gorm:"size:255"`
	Disabled       bool    `gorm:"not null;default:false"`
	ServiceAccount bool    `gorm:"not null;default:false"`
}

func (authUserV1) TableName() string { return "users" }

type authRefreshTokenV1 struct {
	ExpiresAt        time.Time `gorm:"not null;index:idx_refresh_tokens_expires_at"`
	LastActivity     time.Time `gorm:"not null"`
	CreatedAt        time.Time
	SessionStartedAt time.Time
	RotatedAt        *time.Time
	ID               string `gorm:"primaryKey;size:36"`
	UserID           string `gorm:"size:36;not null;index:idx_refresh_tokens_user_id"`
	TokenHash        string `gorm:"size:64;not null;uniqueIndex:idx_refresh_tokens_token_hash"`
	FamilyID         string `gorm:"size:36;index:idx_refresh_tokens_family_id"`
	UserAgent        string `gorm:"size:500"`
	IPAddress        string `gorm:"size:45"`
	Revoked          bool   `gorm:"not null;default:false"`
}

func (authRefreshTokenV1) TableName() string { return "refresh_tokens" }

type authAPIKeyV1 struct {
	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	ID         string `gorm:"primaryKey;size:36"`
	UserID     string `gorm:"size:36;not null;index:idx_api_keys_user_id"`
	Name       string `gorm:"size:255;not null"`
	KeyHash    string `gorm:"size:64;not null"`
	Prefix     string `gorm:"size:20;not null;index:idx_api_keys_prefix"`
}

func (authAPIKeyV1) TableName() string { return "api_keys" }

type authAuditLogV1 struct {
	Timestamp  time.Time `gorm:"not null;index:idx_audit_logs_timestamp;index:idx_audit_logs_action_timestamp,priority:2,sort:desc"`
	ID         string    `gorm:"primaryKey;size:36"`
	UserID     string    `gorm:"size:36;index:idx_audit_logs_user_id"`
	Username   string    `gorm:"size:100"`
	Action     string    `gorm:"size:100;index:idx_audit_logs_action_timestamp,priority:1"`
	EntityType string    `gorm:"size:100;index:idx_audit_logs_entity,priority:1"`
	EntityID   string    `gorm:"size:64;index:idx_audit_logs_entity,priority:2"`
	Details    string    `gorm:"type:longtext"`
}

func (authAuditLogV1) TableName() string { return "audit_logs" }

// dropTables drops the tables (IF EXISTS) on the connection of tx. GORM's
// MySQL Migrator().DropTable runs on another pooled connection: the migration
// transaction then keeps its snapshot of information_schema, still sees the
// dropped table, and AutoMigrate fails on it.
func dropTables(tx *gorm.DB, names ...string) error {
	for _, name := range names {
		if err := tx.Exec("DROP TABLE IF EXISTS ?", clause.Table{Name: name}).Error; err != nil {
			return err
		}
	}
	return nil
}

// replaceUsersAndAddAuthTablesMigration replaces the unused users table of
// migration 20231201000001 (uint ID) with the auth users table (UUID string
// ID) and creates refresh_tokens, api_keys and audit_logs. The tables exist
// whether or not auth is enabled, so the flag never needs a migration.
//
// Both directions are idempotent. Up drops users only while it has the old
// shape (no password_hash column), so a second Up keeps the new rows. Down
// drops the auth tables and restores the old users table with the index of
// 20231201000002, so that migration's Down still works.
func replaceUsersAndAddAuthTablesMigration() schema.Migration {
	return schema.Migration{
		Version:     "20260101000001",
		Name:        "replace_users_and_add_auth_tables",
		Description: "Replace the users table with a UUID-keyed one and create refresh_tokens, api_keys and audit_logs",
		Up:          replaceUsersUp,
		Down:        replaceUsersDown,
	}
}

// hasAuthUsersTable reports whether users exists with the auth shape.
func hasAuthUsersTable(tx *gorm.DB) bool {
	m := tx.Migrator()
	return m.HasTable(&authUserV1{}) && m.HasColumn(&authUserV1{}, "password_hash")
}

func replaceUsersUp(tx *gorm.DB) error {
	if tx.Migrator().HasTable(&authUserV1{}) && !hasAuthUsersTable(tx) {
		if err := dropTables(tx, "users"); err != nil {
			return err
		}
	}
	return tx.AutoMigrate(&authUserV1{}, &authRefreshTokenV1{}, &authAPIKeyV1{}, &authAuditLogV1{})
}

func replaceUsersDown(tx *gorm.DB) error {
	if err := dropTables(tx, "audit_logs", "api_keys", "refresh_tokens"); err != nil {
		return err
	}
	if hasAuthUsersTable(tx) {
		if err := dropTables(tx, "users"); err != nil {
			return err
		}
	}
	if err := tx.AutoMigrate(&legacyUser{}); err != nil {
		return err
	}
	if !tx.Migrator().HasIndex(&legacyUser{}, "idx_users_email") {
		return tx.Exec("CREATE INDEX idx_users_email ON users(email)").Error
	}
	return nil
}

// sessionEntryV1 is the session_entries table as 20260101000002 creates it, a
// frozen copy of sessionstore.SessionEntry: a model change needs a new
// migration (TestSessionEntriesMatchModel fails without one).
type sessionEntryV1 struct {
	EntryKey  string `gorm:"column:entry_key;primaryKey;size:255"`
	Kind      string `gorm:"column:kind;primaryKey;size:20"`
	Data      string `gorm:"column:data;type:text"`
	ExpiresAt int64  `gorm:"column:expires_at;not null;index:idx_session_entries_expires_at"`
}

func (sessionEntryV1) TableName() string { return "session_entries" }

// createSessionEntriesMigration creates session_entries, the one table of the
// session store: the blocklist of revoked tokens, users and sessions, and the
// pending OIDC login states (see package sessionstore). A row is told apart by
// kind, and the index on expires_at serves the cleanup of expired rows.
//
// On MySQL the key column gets the binary NO PAD collation utf8mb4_0900_bin
// (MySQL 8.0.17 and later): a token ID or an OIDC state that differs only in
// letter case or in trailing spaces is another key. Both directions are
// idempotent.
func createSessionEntriesMigration() schema.Migration {
	return schema.Migration{
		Version:     "20260101000002",
		Name:        "create_session_entries",
		Description: "Create session_entries for the token blocklist and OIDC login state",
		Up:          createSessionEntriesUp,
		Down: func(tx *gorm.DB) error {
			return dropTables(tx, "session_entries")
		},
	}
}

// dialectMySQL is the GORM dialector name of MySQL.
const dialectMySQL = "mysql"

func createSessionEntriesUp(tx *gorm.DB) error {
	if err := tx.AutoMigrate(&sessionEntryV1{}); err != nil {
		return err
	}
	if tx.Dialector.Name() != dialectMySQL {
		return nil
	}
	var collation string
	if err := tx.Raw("SELECT COLLATION_NAME FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'session_entries' AND column_name = 'entry_key'").Scan(&collation).Error; err != nil {
		return err
	}
	if collation == "utf8mb4_0900_bin" {
		return nil
	}
	return tx.Exec("ALTER TABLE session_entries MODIFY entry_key VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL").Error
}
