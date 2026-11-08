//go:build integration

package database

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"backend/internal/models"
	"backend/pkg/dberrors"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// mysqlUniqueRow is a test-only table with a string ID and a unique key.
type mysqlUniqueRow struct {
	ID       string `gorm:"primaryKey;size:36"`
	Username string `gorm:"size:64;not null;uniqueIndex"`
}

func (mysqlUniqueRow) TableName() string { return "it_base_repo_unique_rows" }

// TestMySQLDuplicateKeyAndTx checks on a real MySQL server that a 1062 from
// the driver maps to ErrDuplicateKey (409) through the repository mapping and
// IsDuplicateKey, and that BaseRepository.Tx commits and rolls back.
func TestMySQLDuplicateKeyAndTx(t *testing.T) {
	db, err := NewFromAppConfig(mysqlTestConfig(false))
	require.NoError(t, err)
	require.NoError(t, db.DB.AutoMigrate(&mysqlUniqueRow{}))
	t.Cleanup(func() { _ = db.DB.Migrator().DropTable(&mysqlUniqueRow{}) })
	ctx := context.Background()
	user := fmt.Sprintf("u-%d", time.Now().UnixNano())

	repo := models.NewRepository(db.DB)
	require.NoError(t, repo.Create(ctx, &mysqlUniqueRow{ID: "1", Username: user}))
	err = repo.Create(ctx, &mysqlUniqueRow{ID: "2", Username: user})
	assert.ErrorIs(t, err, dberrors.ErrDuplicateKey)
	assert.NotContains(t, err.Error(), user)

	base := NewBaseRepository(db.DB)
	var attempts int
	err = base.Tx(ctx, func(tx *gorm.DB) error {
		attempts++
		if err := tx.Create(&mysqlUniqueRow{ID: "3", Username: user + "-ok"}).Error; err != nil {
			return err
		}
		return tx.Create(&mysqlUniqueRow{ID: "4", Username: user}).Error
	})
	require.Error(t, err)
	var mysqlErr *mysql.MySQLError
	require.True(t, errors.As(err, &mysqlErr))
	assert.Equal(t, uint16(1062), mysqlErr.Number)
	assert.True(t, IsDuplicateKey(err))
	assert.Equal(t, 1, attempts, "a duplicate key is not retried")
	var n int64
	require.NoError(t, db.DB.Model(&mysqlUniqueRow{}).Where("username = ?", user+"-ok").Count(&n).Error)
	assert.Zero(t, n, "the transaction rolled back")

	require.NoError(t, base.Write(ctx, func(db *gorm.DB) error {
		return db.Create(&mysqlUniqueRow{ID: "5", Username: user + "-w"}).Error
	}))
}
