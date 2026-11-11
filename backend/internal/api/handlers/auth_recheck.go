package handlers

import (
	"context"
	"errors"

	"backend/internal/models"
)

// Results of the re-checks after an access token was signed. Callers fail
// closed on every non-nil result.
var (
	// errUserGone means the user was deleted while a token was being issued.
	errUserGone = errors.New("user not found")
	// errAccountDisabled means the user was disabled while a token was being
	// issued.
	errAccountDisabled = errors.New("account disabled")
	// errSessionRevoked means the session family has no active refresh token
	// any more (a logout-all, an admin revoke or a replay revocation won a
	// race).
	errSessionRevoked = errors.New("session revoked")
)

// recheckUserActive re-reads the user after a token is signed and before it
// leaves the server. It closes the race where an admin disables the user
// between the first Disabled check and the signing: the user block applies
// only to tokens issued at or before the block, so a token signed just after
// the block would otherwise stay valid.
//
// It returns nil for an active user, errAccountDisabled, errUserGone, or the
// repository error (a cancelled ctx matches context.Canceled).
func recheckUserActive(ctx context.Context, repo models.UserRepository, userID string) error {
	u, err := repo.FindByID(ctx, userID)
	if err != nil {
		if isNotFoundError(err) {
			return errUserGone
		}
		return err
	}
	if u.Disabled {
		return errAccountDisabled
	}
	return nil
}

// recheckSessionActive runs after an access token of a session is signed and
// confirms that the session family still has an active refresh token. It
// returns nil, errSessionRevoked, or the repository error.
func recheckSessionActive(ctx context.Context, repo models.RefreshTokenRepository, familyID string) error {
	active, err := repo.CountActiveInFamily(ctx, familyID)
	if err != nil {
		return err
	}
	if active == 0 {
		return errSessionRevoked
	}
	return nil
}
