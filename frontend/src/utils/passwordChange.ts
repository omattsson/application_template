/**
 * Shared texts of the self-service password change
 * (`PUT /api/v1/auth/me/password`). The two server texts are the ones that the
 * backend sends for a wrong current password (401) and for a new password that
 * equals the current one (400, also checked in the browser).
 */

/** The 401 text of a wrong current password (a failure that counts on the account throttle). */
export const CURRENT_PASSWORD_INCORRECT_MESSAGE = 'Current password is incorrect';
/** The 400 text of a new password that equals the current one. */
export const PASSWORD_UNCHANGED_MESSAGE = 'The new password must differ from the current password';
