import type { Role } from '../../../utils/jwt';

/**
 * Client-side checks of the create-user form. They mirror the backend rules
 * (validateRegistration, used by CreateUser in backend/internal/api/handlers)
 * so that a mistake is shown before the request; the API stays the authority and
 * its message is shown for anything that passes here.
 */

export const USERNAME_MIN = 3;
export const USERNAME_MAX = 100;
export const PASSWORD_MIN_CHARS = 8;
export const PASSWORD_MAX_BYTES = 72;
export const DISPLAY_NAME_MAX = 255;
export const EMAIL_MAX = 255;

export const MSG_USERNAME =
  'Username must be 3 to 100 characters: letters, digits and . _ - @, starting with a letter or digit';
export const MSG_PASSWORD_SHORT = 'Password must be at least 8 characters';
export const MSG_PASSWORD_LONG = 'Password must be at most 72 bytes';
export const MSG_EMAIL = 'Invalid email address';
export const MSG_DISPLAY_NAME = 'Display name must be at most 255 characters';

/** backend usernamePattern. */
const USERNAME_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._@-]*$/;
/** backend models.emailRegex. */
const EMAIL_PATTERN = /^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$/;

/** Number of Unicode characters (the backend counts runes). */
const charCount = (value: string): number => Array.from(value).length;
const byteCount = (value: string): number => new TextEncoder().encode(value).length;

export interface CreateUserForm {
  username: string;
  email: string;
  displayName: string;
  password: string;
  role: Role;
}

/** The message of each invalid field; a valid field has no entry. */
export type CreateUserErrors = Partial<Record<'username' | 'email' | 'displayName' | 'password', string>>;

/**
 * Check the create-user form.
 * @param form - The values as typed
 * @returns The message per invalid field (empty object when the form is valid)
 */
export function validateCreateUser(form: CreateUserForm): CreateUserErrors {
  const errors: CreateUserErrors = {};
  const { username, email, displayName, password } = form;
  if (username.length < USERNAME_MIN || username.length > USERNAME_MAX || !USERNAME_PATTERN.test(username)) {
    errors.username = MSG_USERNAME;
  }
  if (charCount(password) < PASSWORD_MIN_CHARS) errors.password = MSG_PASSWORD_SHORT;
  else if (byteCount(password) > PASSWORD_MAX_BYTES) errors.password = MSG_PASSWORD_LONG;
  if (email !== '' && (charCount(email) > EMAIL_MAX || !EMAIL_PATTERN.test(email))) errors.email = MSG_EMAIL;
  if (charCount(displayName) > DISPLAY_NAME_MAX) errors.displayName = MSG_DISPLAY_NAME;
  return errors;
}
