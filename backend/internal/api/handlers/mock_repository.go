package handlers

import (
	"backend/internal/models"
	"errors"
)

// MockRepository is a mock implementation of the Repository interface for testing
type MockRepository struct {
	items  map[uint]*models.Item
	nextID uint
}

func NewMockRepository() *MockRepository {
	return &MockRepository{
		items:  make(map[uint]*models.Item),
		nextID: 1,
	}
}

func (m *MockRepository) Create(entity interface{}) error {
	item, ok := entity.(*models.Item)
	if !ok {
		return errors.New("invalid entity type")
	}

	item.ID = m.nextID
	m.nextID++
	m.items[item.ID] = item
	return nil
}

func (m *MockRepository) FindByID(id uint, dest interface{}) error {
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
	item, ok := entity.(*models.Item)
	if !ok {
		return errors.New("invalid entity type")
	}

	if _, exists := m.items[item.ID]; !exists {
		return errors.New("item not found")
	}

	m.items[item.ID] = item
	return nil
}

func (m *MockRepository) Delete(entity interface{}) error {
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
	items, ok := dest.(*[]models.Item)
	if !ok {
		return errors.New("invalid destination type")
	}

	*items = make([]models.Item, 0, len(m.items))
	for _, item := range m.items {
		*items = append(*items, *item)
	}
	return nil
}
