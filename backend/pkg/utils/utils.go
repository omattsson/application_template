package utils

import (
	"crypto/rand"
	"math/big"
)

// CheckError returns the error for the caller to handle.
// Deprecated: Use explicit error handling instead of this wrapper.
func CheckError(err error) error {
	return err
}

// GenerateRandomString generates a cryptographically random string of the specified length.
func GenerateRandomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	b := make([]byte, length)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		b[i] = charset[n.Int64()]
	}
	return string(b)
}
