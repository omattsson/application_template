package utils

import (
	"math/rand"
	"time"
)

// Utility function to check for errors and handle them appropriately
func CheckError(err error) {
	if err != nil {
		panic(err) // Handle error as needed
	}
}

// Function to generate a random string of a specified length
func GenerateRandomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	var seededRand = rand.New(rand.NewSource(time.Now().UnixNano()))

	b := make([]byte, length)
	for i := range b {
		b[i] = charset[seededRand.Intn(len(charset))]
	}
	return string(b)
}
