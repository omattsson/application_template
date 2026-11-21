/** Longest name of a key, in characters (the server counts code points). */
export const KEY_NAME_MAX = 100;

/** The server rule (validKeyName, handlers/api_keys.go), applied to the trimmed name. */
const KEY_NAME_PATTERN = /^[\p{L}\p{N} ._:@/()+-]+$/u;

/** Lifetimes that the create dialog offers, in days. The server maximum (365 by default) is the last one. */
export const EXPIRY_OPTIONS: readonly number[] = [30, 90, 180, 365];

/** The lifetime that the create dialog starts with. */
export const DEFAULT_EXPIRY_DAYS = 90;

/**
 * Check a key name the way the server does, so that an obvious mistake is
 * shown before the request. The server stays the authority: its 400 text is
 * still shown when it refuses a name.
 * @param name - The name as typed
 * @returns The message for the first rule that fails, or undefined when the name is valid
 */
export function validateKeyName(name: string): string | undefined {
  const trimmed = name.trim();
  if (trimmed === '') return 'Enter a name for the key.';
  if ([...trimmed].length > KEY_NAME_MAX) return `The name can be at most ${KEY_NAME_MAX} characters.`;
  if (!KEY_NAME_PATTERN.test(trimmed)) return 'Use only letters, digits, spaces and . _ - : @ / ( ) +.';
  return undefined;
}
