package models

import (
	"errors"
	"regexp"
)

// Compile email regex once at package level for performance.
var emailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)

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
