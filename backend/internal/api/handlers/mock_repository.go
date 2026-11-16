package handlers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"backend/internal/models"
)

// MockRepository is a mock implementation of the Repository interface for testing
type MockRepository struct {
	sync.RWMutex                       // size: 8
	items        map[uint]*models.Item // size: 8 (pointer)
	err          error                 // size: 8 (interface)
	updateError  error                 // size: 8 (interface)
	nextID       uint                  // size: 8
}

func NewMockRepository() *MockRepository {
	return &MockRepository{
		items:  make(map[uint]*models.Item),
		nextID: 1,
	}
}

func (m *MockRepository) Create(_ context.Context, entity interface{}) error {
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
	item.Version = 1 // Initialize version (1 = first version; 0 = "not provided" sentinel)
	m.nextID++
	m.items[item.ID] = item
	return nil
}

func (m *MockRepository) FindByID(_ context.Context, id uint, dest interface{}) error {
	m.RLock()
	defer m.RUnlock()

	if m.err != nil {
		return fmt.Errorf("database error: %w", m.err)
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

func (m *MockRepository) Update(_ context.Context, entity interface{}) error {
	m.Lock()
	defer m.Unlock()

	if m.updateError != nil {
		return m.updateError
	}

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

func (m *MockRepository) Delete(_ context.Context, entity interface{}) error {
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

func (m *MockRepository) List(_ context.Context, dest interface{}, conditions ...interface{}) error {
	m.RLock()
	defer m.RUnlock()

	if m.err != nil {
		return m.err
	}

	items, ok := dest.(*[]models.Item)
	if !ok {
		return errors.New("invalid destination type")
	}

	// Convert map to slice for easier filtering and ensure consistent order
	allItems := make([]models.Item, 0, len(m.items))
	var ids []uint
	for id := range m.items {
		ids = append(ids, id)
	}
	// Sort by ID for consistent ordering
	sort.Slice(ids, func(i, j int) bool {
		return ids[i] < ids[j]
	})
	for _, id := range ids {
		allItems = append(allItems, *m.items[id])
	}

	// Apply filters and pagination
	var pagination *models.Pagination
	filteredItems := allItems // Start with all items

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

// ListPaged implements the Repository interface on top of List, so the mock
// filters, sorts and pages like the handler expects. The sort follows the
// repository: the key's column, then id in the same direction, and an unknown
// key is dberrors.ErrValidation. The total counts the filtered rows only.
func (m *MockRepository) ListPaged(ctx context.Context, dest interface{}, limit, offset int, filter models.ItemFilter) (int64, error) {
	column, err := filter.SortColumn()
	if err != nil {
		return 0, err
	}

	var conditions []interface{}
	if filter.NameExact != "" {
		conditions = append(conditions, models.Filter{Field: "name", Op: "exact", Value: filter.NameExact})
	} else if filter.Name != "" {
		conditions = append(conditions, models.Filter{Field: "name", Value: filter.Name})
	}
	if filter.MinPrice > 0 {
		conditions = append(conditions, models.Filter{Field: "price", Op: ">=", Value: filter.MinPrice})
	}
	if filter.MaxPrice > 0 {
		conditions = append(conditions, models.Filter{Field: "price", Op: "<=", Value: filter.MaxPrice})
	}

	var all []models.Item
	if err := m.List(ctx, &all, conditions...); err != nil {
		return 0, err
	}
	total := int64(len(all))

	sort.SliceStable(all, func(i, j int) bool {
		c := compareItems(all[i], all[j], column)
		if c == 0 {
			c = cmpUint(all[i].ID, all[j].ID)
		}
		if filter.SortDesc {
			return c > 0
		}
		return c < 0
	})

	if limit > 0 {
		start := min(max(offset, 0), len(all))
		end := min(start+limit, len(all))
		all = all[start:end]
	}
	items, ok := dest.(*[]models.Item)
	if !ok {
		return 0, errors.New("invalid destination type")
	}
	*items = all
	return total, nil
}

// compareItems compares two items by a column of the item sort whitelist.
// Names compare case-insensitively, as the default MySQL collation does.
func compareItems(a, b models.Item, column string) int {
	switch column {
	case "name":
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	case "price":
		switch {
		case a.Price < b.Price:
			return -1
		case a.Price > b.Price:
			return 1
		}
		return 0
	case "created_at":
		return a.CreatedAt.Compare(b.CreatedAt)
	case "updated_at":
		return a.UpdatedAt.Compare(b.UpdatedAt)
	}
	return cmpUint(a.ID, b.ID)
}

func cmpUint(a, b uint) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Ping implements the Repository interface
func (m *MockRepository) Ping(_ context.Context) error {
	return nil
}

// Close implements the Repository interface
func (m *MockRepository) Close() error {
	return nil
}

func (m *MockRepository) SetError(err error) {
	m.Lock()
	defer m.Unlock()
	m.err = err
}

func (m *MockRepository) SetUpdateError(err error) {
	m.Lock()
	defer m.Unlock()
	m.updateError = err
}
