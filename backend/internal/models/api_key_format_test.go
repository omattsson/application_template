package models

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyFormatAndParse(t *testing.T) {
	t.Parallel()
	raw, prefix, _, err := GenerateAPIKey()
	require.NoError(t, err)

	formatted := FormatAPIKey(raw)
	assert.Equal(t, APIKeyRawPrefix+raw, formatted)
	assert.True(t, strings.HasPrefix(formatted, "sk_"), "the key has a recognisable non-secret prefix")

	got, ok := ParseAPIKey(formatted)
	require.True(t, ok)
	assert.Equal(t, raw, got)
	assert.Equal(t, prefix, got[:APIKeyPrefixLen])

	hex64 := strings.Repeat("a", 64)
	tests := []struct {
		name   string
		header string
		want   bool
	}{
		{name: "valid", header: "sk_" + hex64, want: true},
		{name: "empty", header: ""},
		{name: "no prefix", header: hex64},
		{name: "wrong prefix", header: "pk_" + hex64},
		{name: "upper-case prefix", header: "SK_" + hex64},
		{name: "upper-case hex", header: "sk_" + strings.ToUpper(hex64)},
		{name: "too short", header: "sk_" + hex64[:63]},
		{name: "too long", header: "sk_" + hex64 + "a"},
		{name: "not hex", header: "sk_" + strings.Repeat("g", 64)},
		{name: "space inside", header: "sk_" + hex64[:63] + " "},
		{name: "newline at the end", header: "sk_" + hex64 + "\n"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, ok := ParseAPIKey(tt.header)
			assert.Equal(t, tt.want, ok)
		})
	}
}
