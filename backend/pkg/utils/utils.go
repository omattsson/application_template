package utils

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

// GenerateRandomString generates a cryptographically random string of the specified length.
// Returns an error if length is negative or if the system random source fails.
func GenerateRandomString(length int) (string, error) {
	if length < 0 {
		return "", fmt.Errorf("length must be non-negative, got %d", length)
	}
	if length == 0 {
		return "", nil
	}

	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	b := make([]byte, length)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", fmt.Errorf("failed to generate random byte at index %d: %w", i, err)
		}
		b[i] = charset[int(n.Int64())]
	}
	return string(b), nil
}
