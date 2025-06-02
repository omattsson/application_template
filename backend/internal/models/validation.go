package models

import (
	"errors"
	"regexp"
)

var (
	ErrInvalidEmail  = errors.New("invalid email format")
	ErrEmptyUsername = errors.New("username cannot be empty")
	ErrInvalidPrice  = errors.New("price must be positive")
	ErrEmptyItemName = errors.New("item name cannot be empty")
)

// Validate implements model validation
func (u *User) Validate() error {
	if u.Username == "" {
		return ErrEmptyUsername
	}

	emailRegex := regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)
	if !emailRegex.MatchString(u.Email) {
		return ErrInvalidEmail
	}

	return nil
}

// Validate implements model validation
func (i *Item) Validate() error {
	if i.Name == "" {
		return ErrEmptyItemName
	}

	if i.Price <= 0 {
		return ErrInvalidPrice
	}

	return nil
}
