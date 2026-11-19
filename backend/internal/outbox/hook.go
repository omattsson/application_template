package outbox

import (
	"fmt"

	"backend/internal/models"
	"backend/internal/websocket"

	"gorm.io/gorm"
)

// The message types of the Items events. They are the types that the Items
// handler sends directly with the module off, so the frontend needs no change.
const (
	TypeItemCreated = "item.created"
	TypeItemUpdated = "item.updated"
	TypeItemDeleted = "item.deleted"
)

// InsertFunc writes one outbox row on tx, the transaction of the change
// (database.InsertOutboxEvent).
type InsertFunc func(tx *gorm.DB, eventType string, message []byte) error

// ItemHook returns the change hook of the Items repository
// (models.WithChangeHook). For a create, update or delete of a *models.Item it
// builds the message {"type": ..., "payload": ...} that the handler sends
// without the module (the item for a create and an update, {"id": id} for a
// delete) and writes it with insert, in the transaction of the change. The
// message is stored as final bytes, so every replica sends the same text.
// Any other entity is ignored.
//
// The hook keeps no state and uses only tx, so the repository can run the whole
// transaction again after a deadlock.
func ItemHook(insert InsertFunc) models.ChangeHook {
	return func(tx *gorm.DB, kind models.ChangeKind, entity interface{}) error {
		item, ok := entity.(*models.Item)
		if !ok {
			return nil
		}
		var (
			msgType string
			payload interface{}
		)
		switch kind {
		case models.ChangeCreated:
			msgType, payload = TypeItemCreated, item
		case models.ChangeUpdated:
			msgType, payload = TypeItemUpdated, item
		case models.ChangeDeleted:
			msgType, payload = TypeItemDeleted, map[string]uint64{"id": uint64(item.ID)}
		default:
			return nil
		}
		msg, err := websocket.NewMessage(msgType, payload)
		if err != nil {
			return fmt.Errorf("build the %s event: %w", msgType, err)
		}
		b, err := msg.Bytes()
		if err != nil {
			return fmt.Errorf("encode the %s event: %w", msgType, err)
		}
		return insert(tx, msgType, b)
	}
}
