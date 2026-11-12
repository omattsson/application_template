package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"backend/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type logRecord struct {
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// recordsOf parses the JSON lines that a slog JSON handler wrote.
func recordsOf(t *testing.T, logs *syncBuffer) []logRecord {
	t.Helper()
	var out []logRecord
	sc := bufio.NewScanner(strings.NewReader(logs.String()))
	for sc.Scan() {
		var r logRecord
		require.NoError(t, json.Unmarshal(sc.Bytes(), &r))
		out = append(out, r)
	}
	return out
}

// The startup says why no admin exists (Info, so it shows at the default log
// level) and warns when ADMIN_PASSWORD cannot sign in to the existing user of
// that name. Neither line holds a secret.
func TestEnsureAdminUserBootstrapLogs(t *testing.T) {
	t.Parallel()
	const adminPassword = "Admin-Pass-123"
	const noPasswordMsg = "ADMIN_PASSWORD is not set; no admin user is created"
	const cannotSignInMsg = "ADMIN_PASSWORD cannot sign in to it"
	extID := "sub-1"

	tests := []struct {
		name     string
		password string
		seed     []models.User
		wantLog  logRecord // zero: none of the two messages
	}{
		{name: "no password: an Info line says so", password: "",
			wantLog: logRecord{Level: "INFO", Msg: noPasswordMsg}},
		{name: "existing OIDC user of that name: warning", password: adminPassword,
			seed:    []models.User{{ID: "a1", Username: "admin", Role: models.RoleAdmin, AuthProvider: models.AuthProviderOIDC, ExternalID: &extID}},
			wantLog: logRecord{Level: "WARN", Msg: cannotSignInMsg}},
		{name: "existing local user without a password hash: warning", password: adminPassword,
			seed:    []models.User{{ID: "a1", Username: "admin", Role: models.RoleAdmin, AuthProvider: models.AuthProviderLocal}},
			wantLog: logRecord{Level: "WARN", Msg: cannotSignInMsg}},
		{name: "existing local admin with a password: no warning", password: adminPassword,
			seed: []models.User{{ID: "a1", Username: "admin", Role: models.RoleAdmin, AuthProvider: models.AuthProviderLocal, PasswordHash: "keep-hash"}}},
		{name: "no existing user: created, no warning", password: adminPassword},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := testAuthConfig()
			cfg.AdminPassword = tt.password
			h, logs, _ := newTestAuthHandler(newMockUserRepo(tt.seed...), cfg, nil)
			// The default log level: a Debug line would not show.
			h.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

			require.NoError(t, h.EnsureAdminUser(context.Background()))

			var found []logRecord
			for _, r := range recordsOf(t, logs) {
				if strings.Contains(r.Msg, noPasswordMsg) || strings.Contains(r.Msg, cannotSignInMsg) {
					found = append(found, r)
				}
			}
			if tt.wantLog.Msg == "" {
				assert.Empty(t, found)
			} else {
				require.Len(t, found, 1)
				assert.Equal(t, tt.wantLog.Level, found[0].Level)
				assert.Contains(t, found[0].Msg, tt.wantLog.Msg)
			}
			assert.NotContains(t, logs.String(), adminPassword)
			assert.NotContains(t, logs.String(), "keep-hash")
		})
	}
}
