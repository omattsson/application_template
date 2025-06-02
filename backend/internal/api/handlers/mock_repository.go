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

	// Increment version and update
	item.Version++
	m.items[item.ID] = item
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
	filteredItems := make([]models.Item, 0)

	// Apply filters
	filteredItems = allItems[:] // Start with all items
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
				for _, item := range allItems {
					switch cond.Op {
					case ">=":
						if item.Price >= price {
							filteredItems = append(filteredItems, item)
						}
					case "<=":
						if item.Price <= price {
							filteredItems = append(filteredItems, item)
						}
					}
				}
				allItems = filteredItems
				filteredItems = make([]models.Item, 0)
			}
		case models.Pagination:
			pagination = &cond
		}
	}

	// Apply pagination
	if pagination != nil {
		start := pagination.Offset
		end := start + pagination.Limit
		if start >= len(allItems) {
			*items = []models.Item{}
			return nil
		}
		if end > len(allItems) {
			end = len(allItems)
		}
		*items = allItems[start:end]
		return nil
	}

	*items = allItems
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
