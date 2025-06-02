package handlers

import (
	"backend/internal/models"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// MockRepository is a mock implementation of the Repository interface for testing
type MockRepository struct {
	sync.RWMutex // For thread safety
	items        map[uint]*models.Item
	nextID       uint
	err          error
}

func NewMockRepository() *MockRepository {
	return &MockRepository{
		items:  make(map[uint]*models.Item),
		nextID: 1,
	}
}

func (m *MockRepository) Create(entity interface{}) error {
	m.Lock()
	defer m.Unlock()

	if m.err != nil {
		return m.err
	}

	item, ok := entity.(*models.Item)
	if !ok {
		return errors.New("invalid entity type")
	}

	item.ID = m.nextID
	item.Version = 0 // Initialize version
	m.nextID++
	m.items[item.ID] = item
	return nil
}

func (m *MockRepository) FindByID(id uint, dest interface{}) error {
	m.RLock()
	defer m.RUnlock()

	if m.err != nil {
		return fmt.Errorf("database error: %v", m.err)
	}

	itemDest, ok := dest.(*models.Item)
	if !ok {
		return errors.New("invalid destination type")
	}

	item, exists := m.items[id]
	if !exists {
		return errors.New("item not found")
	}

	*itemDest = *item
	return nil
}

func (m *MockRepository) Update(entity interface{}) error {
	m.Lock()
	defer m.Unlock()

	if m.err != nil {
		return m.err
	}

	item, ok := entity.(*models.Item)
	if !ok {
		return errors.New("invalid entity type")
	}

	currentItem, exists := m.items[item.ID]
	if !exists {
		return errors.New("item not found")
	}

	// Check version for optimistic locking
	if item.Version != currentItem.Version {
		return errors.New("version mismatch")
	}

	// Make a copy of the item to avoid other references being modified
	updatedItem := *item

	// Increment version and update
	updatedItem.Version = currentItem.Version + 1
	m.items[item.ID] = &updatedItem

	// Update the original item's version to match
	item.Version = updatedItem.Version

	return nil
}

func (m *MockRepository) Delete(entity interface{}) error {
	m.Lock()
	defer m.Unlock()

	if m.err != nil {
		return m.err
	}

	item, ok := entity.(*models.Item)
	if !ok {
		return errors.New("invalid entity type")
	}

	if _, exists := m.items[item.ID]; !exists {
		return errors.New("item not found")
	}

	delete(m.items, item.ID)
	return nil
}

func (m *MockRepository) List(dest interface{}, conditions ...interface{}) error {
	m.RLock()
	defer m.RUnlock()

	if m.err != nil {
		return m.err
	}

	items, ok := dest.(*[]models.Item)
	if !ok {
		return errors.New("invalid destination type")
	}

	// Convert map to slice for easier filtering
	allItems := make([]models.Item, 0, len(m.items))
	for _, item := range m.items {
		allItems = append(allItems, *item)
	}

	// Apply filters and pagination
	var pagination *models.Pagination
	filteredItems := allItems[:] // Start with all items

	// Apply filters
	for _, condition := range conditions {
		switch cond := condition.(type) {
		case models.Filter:
			switch cond.Field {
			case "name":
				name := cond.Value.(string)
				tmpItems := make([]models.Item, 0)
				if cond.Op == "exact" {
					for _, item := range filteredItems {
						if strings.EqualFold(item.Name, name) {
							tmpItems = append(tmpItems, item)
						}
					}
				} else {
					for _, item := range filteredItems {
						if strings.Contains(strings.ToLower(item.Name), strings.ToLower(name)) {
							tmpItems = append(tmpItems, item)
						}
					}
				}
				filteredItems = tmpItems
			case "price":
				price := cond.Value.(float64)
				tmpItems := make([]models.Item, 0)
				for _, item := range filteredItems {
					switch cond.Op {
					case ">=":
						if item.Price >= price {
							tmpItems = append(tmpItems, item)
						}
					case "<=":
						if item.Price <= price {
							tmpItems = append(tmpItems, item)
						}
					}
				}
				filteredItems = tmpItems
			}
		case models.Pagination:
			pagination = &cond
		}
	}

	// Apply pagination
	if pagination != nil {
		start := pagination.Offset
		end := start + pagination.Limit
		if start >= len(filteredItems) {
			*items = []models.Item{}
			return nil
		}
		if end > len(filteredItems) {
			end = len(filteredItems)
		}
		*items = filteredItems[start:end]
		return nil
	}

	*items = filteredItems
	return nil
}

// Ping implements the Repository interface
func (m *MockRepository) Ping() error {
	return nil
}

func (m *MockRepository) SetError(err error) {
	m.Lock()
	defer m.Unlock()
	m.err = err
}
