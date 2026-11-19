package outbox

import (
	"errors"
	"testing"

	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type inserted struct {
	eventType string
	message   string
}

func TestItemHook(t *testing.T) {
	t.Parallel()

	item := &models.Item{Base: models.Base{ID: 7}, Name: "pen", Price: 1.5, Version: 2}

	tests := []struct {
		entity    interface{}
		name      string
		kind      models.ChangeKind
		wantType  string
		wantMsg   string
		wantWrite bool
	}{
		{name: "created", kind: models.ChangeCreated, entity: item, wantWrite: true, wantType: "item.created",
			wantMsg: `{"type":"item.created","payload":{"id":7,"created_at":"0001-01-01T00:00:00Z","updated_at":"0001-01-01T00:00:00Z","name":"pen","price":1.5,"version":2}}`},
		{name: "updated", kind: models.ChangeUpdated, entity: item, wantWrite: true, wantType: "item.updated",
			wantMsg: `{"type":"item.updated","payload":{"id":7,"created_at":"0001-01-01T00:00:00Z","updated_at":"0001-01-01T00:00:00Z","name":"pen","price":1.5,"version":2}}`},
		{name: "deleted carries only the id", kind: models.ChangeDeleted, entity: item, wantWrite: true, wantType: "item.deleted",
			wantMsg: `{"type":"item.deleted","payload":{"id":7}}`},
		{name: "another entity writes nothing", kind: models.ChangeCreated, entity: &models.User{}},
		{name: "an unknown kind writes nothing", kind: models.ChangeKind("other"), entity: item},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got []inserted
			hook := ItemHook(func(_ *gorm.DB, eventType string, message []byte) error {
				got = append(got, inserted{eventType, string(message)})
				return nil
			})

			require.NoError(t, hook(nil, tt.kind, tt.entity))

			if !tt.wantWrite {
				assert.Empty(t, got)
				return
			}
			require.Len(t, got, 1)
			assert.Equal(t, tt.wantType, got[0].eventType)
			assert.JSONEq(t, tt.wantMsg, got[0].message)
		})
	}

	t.Run("the insert error is returned", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("insert failed")
		hook := ItemHook(func(*gorm.DB, string, []byte) error { return boom })
		assert.ErrorIs(t, hook(nil, models.ChangeCreated, item), boom)
	})
}
