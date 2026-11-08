package sessionstore

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// The test hooks: a store with a fixed clock and cleanup interval, the row
// count of a store, and the channel that closes when its cleanup goroutine
// has returned.

// NewMemoryWithClock returns an in-memory store that reads the time from now
// and runs its cleanup every cleanupEvery.
func NewMemoryWithClock(now func() time.Time, cleanupEvery time.Duration) SessionStore {
	return newMemory(now, cleanupEvery)
}

// NewMySQLWithClock is NewMemoryWithClock for the database store.
func NewMySQLWithClock(db *gorm.DB, now func() time.Time, cleanupEvery time.Duration) SessionStore {
	return newMySQL(db, now, cleanupEvery)
}

// EntryCount returns the number of stored rows of every kind, expired or not.
func EntryCount(s SessionStore) int {
	switch st := s.(type) {
	case *memoryStore:
		return st.count()
	case *mysqlStore:
		var n int64
		if err := st.db.WithContext(context.Background()).Model(&SessionEntry{}).Count(&n).Error; err != nil {
			panic(err)
		}
		return int(n)
	}
	panic("unknown store type")
}

// CleanupExited returns a channel that closes when the cleanup goroutine of s
// has returned.
func CleanupExited(s SessionStore) <-chan struct{} {
	switch st := s.(type) {
	case *memoryStore:
		return st.janitor.exited
	case *mysqlStore:
		return st.janitor.exited
	}
	panic("unknown store type")
}
