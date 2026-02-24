package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUtilFunctions(t *testing.T) {
	t.Parallel()

	t.Run("GenerateRandomString returns correct length", func(t *testing.T) {
		t.Parallel()
		s, err := GenerateRandomString(16)
		require.NoError(t, err)
		assert.Len(t, s, 16)
	})

	t.Run("GenerateRandomString with zero length", func(t *testing.T) {
		t.Parallel()
		s, err := GenerateRandomString(0)
		require.NoError(t, err)
		assert.Equal(t, "", s)
	})

	t.Run("GenerateRandomString with negative length", func(t *testing.T) {
		t.Parallel()
		_, err := GenerateRandomString(-1)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "non-negative")
	})

	t.Run("GenerateRandomString contains valid characters", func(t *testing.T) {
		t.Parallel()
		s, err := GenerateRandomString(100)
		require.NoError(t, err)
		for _, c := range s {
			assert.True(t,
				(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'),
				"unexpected character: %c", c)
		}
	})
}
