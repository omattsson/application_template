package config

// OIDCConfig holds the OpenID Connect settings that the local login reads.
// OIDC is off by default. The provider settings (OIDC_ENABLED, issuer, client
// and redirect settings, role mapping) and their loading and validation are
// added with the OIDC login; until then LoadConfig does not fill this struct
// and every field is false.
type OIDCConfig struct {
	// Enabled turns on the OIDC login.
	Enabled bool
	// LocalAuth keeps the password login open to every local user while OIDC
	// is enabled. With false only service accounts (break-glass) may sign in
	// with a password when Enabled is true.
	LocalAuth bool
}
