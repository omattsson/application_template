// Pure decision logic of the accessibility spec: run, skip or fail a page. No
// Playwright and no vitest import, so that both the spec and a unit test
// (src/__tests__/a11ySkipDecision.test.ts) can use it.

/** What the probe of the backend found. */
export type BackendState =
  | { kind: 'responded'; authEnabled: boolean }
  | { kind: 'unreachable'; reason: string };

export interface DecisionInput {
  backend: BackendState;
  /** The page exists only with auth on (it redirects home with auth off). */
  needsAuth: boolean;
  /** The page needs a signed-in administrator always. */
  needsSignIn: boolean;
  /** The page needs a sign-in only when auth is on (it is open with auth off). */
  signInWhenAuthOn: boolean;
  /** ADMIN_PASSWORD is set. */
  hasPassword: boolean;
  /** A11Y_REQUIRE_AUTH=1: a condition that would skip the page fails it instead. */
  strict: boolean;
}

export type Decision =
  | { action: 'run'; signIn: boolean }
  | { action: 'skip'; reason: string; warn: true }
  | { action: 'fail'; reason: string };

/**
 * Whether the strict switch A11Y_REQUIRE_AUTH is on (exactly "1").
 * @param env - The environment (process.env)
 */
export function isStrict(env: Record<string, string | undefined>): boolean {
  return env.A11Y_REQUIRE_AUTH === '1';
}

/**
 * Decide what to do with a page. Without the strict switch a page whose
 * precondition is missing is skipped (with a warning that names the reason);
 * with it the same condition is a failure, so that a CI run cannot pass with a
 * page unchecked.
 * @param input - The probe result, what the page needs, and the strict switch
 * @returns run (with or without a sign-in), skip, or fail, with the reason
 */
export function decideRun(input: DecisionInput): Decision {
  const stop = (reason: string): Decision =>
    input.strict ? { action: 'fail', reason } : { action: 'skip', reason, warn: true };

  if (input.backend.kind === 'unreachable') {
    return stop(`the backend could not be reached or answered no usable UI config (${input.backend.reason})`);
  }
  const { authEnabled } = input.backend;
  if (input.needsAuth && !authEnabled) {
    return stop('the page needs the backend with AUTH_ENABLED=true, and it runs with auth off');
  }
  const signIn = authEnabled && (input.needsSignIn || input.signInWhenAuthOn);
  if (signIn && !input.hasPassword) {
    return stop('the page needs ADMIN_PASSWORD (the local administrator) and it is not set');
  }
  return { action: 'run', signIn };
}
