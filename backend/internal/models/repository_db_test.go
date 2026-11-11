package models

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"gorm.io/gorm"
)

// TestGenericRepositoryDB: the bootstrap builds the auth repositories on the
// handle of the Items repository, so they share its pool.
func TestGenericRepositoryDB(t *testing.T) {
	t.Parallel()
	db := &gorm.DB{}
	repo := NewRepository(db)
	g, ok := repo.(interface{ DB() *gorm.DB })
	assert.True(t, ok, "the repository exposes its handle")
	assert.Same(t, db, g.DB())
	assert.Same(t, db, NewRepositoryWithFilterFields(db, nil).(*GenericRepository).DB())
}
