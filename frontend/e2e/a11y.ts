import { test, expect, type Page } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { BROWSER_API } from './authEnv';
import { decideRun, isStrict } from './skipDecision';
import type { BackendState, DecisionInput } from './skipDecision';

// Shared part of the accessibility specs: opening a page in one colour mode,
// the axe run, and the checks that make sure the page really loaded before axe
// ran (a page that failed to load must not pass).
//
// Environment of the run:
//   ADMIN_USERNAME (default "admin"), ADMIN_PASSWORD  The local administrator;
//       the signed-in pages need the password.
//   A11Y_REQUIRE_AUTH=1  Strict switch (set it in CI). Without it, a page whose
//       precondition is missing (backend unreachable or answering no UI config,
//       auth off for a page that needs auth, no ADMIN_PASSWORD) is skipped with a
//       "[a11y] SKIPPED" console warning that names the reason. With it, the same
//       condition FAILS the test, so a run cannot pass with a page unchecked.
//       The decision is in skipDecision.ts (unit-tested in
//       src/__tests__/a11ySkipDecision.test.ts).
//
// Axe rules: the WCAG 2.0 to 2.2 level A and AA rules and the best practices.
// `color-contrast` also measures one-character text. A node that axe leaves
// undecided for `color-contrast` fails, unless KNOWN_UNDECIDED names its
// selector and why.

export type Mode = 'light' | 'dark';
export const MODES: Mode[] = ['light', 'dark'];

/** Computed background of `body` per mode (theme/palette.ts, CssBaseline). */
const BODY_BACKGROUND: Record<Mode, string> = {
  dark: 'rgb(18, 18, 18)',
  light: 'rgb(245, 245, 245)',
};

/** Fewest `color-contrast` nodes that must pass inside <main>. A page with a heading and an empty <main> fails. */
const MIN_CONTRAST_PASSES = 3;

/** Local administrator of the backend (the same variables the backend reads). Without a password the signed-in pages are skipped. */
export const ADMIN_USERNAME = process.env.ADMIN_USERNAME ?? 'admin';
export const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD ?? '';

/**
 * The only nodes that axe may leave undecided. Any other undecided node fails, so
 * a new surface cannot escape the check unseen.
 */
const KNOWN_UNDECIDED: { rule: string; reason: string; selector: string; why: string }[] = [
  {
    rule: 'color-contrast',
    reason: 'emptyValue',
    selector: '.MuiInputBase-input',
    why: 'An empty field has no text. Filled fields are measured.',
  },
];

type AxeResults = Awaited<ReturnType<AxeBuilder['analyze']>>;
type AxeNode = AxeResults['violations'][number]['nodes'][number];

/** What a test checks before axe runs. */
export interface PageCheck {
  /** Path the test opens. The final URL must have this path. */
  path: string;
  /** Text of the h1 (exact). */
  heading: string;
  /** Sign in as the local administrator first (the page is for signed-in users). */
  signIn?: boolean;
  /**
   * API calls that may answer 5xx on this page, matched against `METHOD /path`
   * (no query). Each entry needs a comment that says why the call fails.
   */
  allowed5xx?: RegExp[];
  /** Runs after the page loaded and before axe (open a drawer, for example). */
  beforeAxe?: (page: Page) => Promise<void>;
}

/**
 * Ask the public UI config whether the backend runs with authentication.
 * Tells "the backend answered, auth is on/off" apart from "could not ask"
 * (connection refused, a non-2xx status, an answer that is not a UI config).
 * Never throws.
 */
export async function probeBackend(page: Page): Promise<BackendState> {
  // The path the app itself calls: the dev server strips the first /api, so
  // /api/v1/ui-config on the page origin would reach the backend as
  // /v1/ui-config (404).
  const path = `${BROWSER_API}/ui-config`;
  try {
    const res = await page.request.get(path);
    if (!res.ok()) return { kind: 'unreachable', reason: `GET ${path} answered ${res.status()}` };
    const config = (await res.json()) as { auth_enabled?: unknown } | null;
    if (!config || typeof config.auth_enabled !== 'boolean') {
      return { kind: 'unreachable', reason: `GET ${path} did not answer a UI config` };
    }
    return { kind: 'responded', authEnabled: config.auth_enabled };
  } catch (error) {
    return { kind: 'unreachable', reason: error instanceof Error ? error.message : 'request failed' };
  }
}

/**
 * Decide whether a page runs, is skipped (with a console warning) or fails,
 * from the probe, what the page needs, ADMIN_PASSWORD and A11Y_REQUIRE_AUTH.
 * Call `test.skip` / throw from the result.
 * @param page - Playwright page (for the probe)
 * @param needs - What the page needs
 * @returns Whether to sign in; throws when strict mode makes a missing precondition a failure; skips otherwise
 */
export async function prepareRun(
  page: Page,
  needs: Pick<DecisionInput, 'needsAuth' | 'needsSignIn' | 'signInWhenAuthOn'>,
): Promise<{ signIn: boolean }> {
  const decision = decideRun({
    ...needs,
    backend: await probeBackend(page),
    hasPassword: ADMIN_PASSWORD !== '',
    strict: isStrict(process.env),
  });
  if (decision.action === 'fail') {
    throw new Error(`A11Y_REQUIRE_AUTH=1: ${decision.reason}. The page would have been skipped, and was not checked.`);
  }
  if (decision.action === 'skip') {
    console.warn(`[a11y] SKIPPED: ${decision.reason}. Set A11Y_REQUIRE_AUTH=1 to fail instead.`);
    test.skip(true, decision.reason);
  }
  return { signIn: decision.action === 'run' && decision.signIn };
}

/**
 * Sign in through the login form and land on `next`. Goes through the UI: the
 * access token lives in memory only, so a later `page.goto` would restore the
 * session from the refresh cookie instead.
 */
export async function signInAs(page: Page, next: string): Promise<void> {
  await page.goto(`/login?next=${encodeURIComponent(next)}`);
  await page.getByLabel('Username', { exact: false }).fill(ADMIN_USERNAME);
  await page.getByLabel('Password', { exact: false }).fill(ADMIN_PASSWORD);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
}

/**
 * MUI draws every dark Paper with `background-image: linear-gradient(c, c)`
 * (two equal stops: the elevation overlay). Axe cannot read the colour behind
 * text on such an element and returns the text as `incomplete` (messageKey
 * `bgGradient`) instead of checking it. Draw the same pixels without the
 * gradient: composite the overlay colour over the element's own background
 * colour and set it inline with `background-image: none`.
 */
export async function flattenGradients(page: Page): Promise<void> {
  await page.evaluate(() => {
    const parse = (c: string): [number, number, number, number] => {
      const m = c.match(/rgba?\(([^)]+)\)/);
      if (!m) return [0, 0, 0, 0];
      const p = m[1].split(/[, /]+/).filter(Boolean).map(Number);
      return [p[0], p[1], p[2], p[3] ?? 1];
    };
    for (const el of Array.from(document.querySelectorAll<HTMLElement>('*'))) {
      const cs = getComputedStyle(el);
      const m = cs.backgroundImage.match(/^linear-gradient\((rgba?\([^)]+\)), (rgba?\([^)]+\))\)$/);
      if (!m || m[1] !== m[2]) continue;
      const [r, g, b, a] = parse(m[1]);
      const [br, bg, bb, ba] = parse(cs.backgroundColor);
      const outA = a + ba * (1 - a);
      if (outA === 0) continue;
      const mix = (over: number, under: number) => Math.round((over * a + under * ba * (1 - a)) / outA);
      el.style.backgroundImage = 'none';
      el.style.backgroundColor = `rgba(${mix(r, br)}, ${mix(g, bg)}, ${mix(b, bb)}, ${outA})`;
    }
  });
}

/**
 * Wait until no CSS transition or animation with an end is running. A colour in
 * the middle of a transition (hover, theme, Collapse) gives a wrong ratio.
 * Endless animations (a spinner) never end and do not change a colour that axe
 * reads, so they do not count.
 */
export async function waitForTransitions(page: Page): Promise<void> {
  // A transition that a state change just asked for starts on a later frame.
  await page.evaluate(
    () => new Promise<void>((done) => requestAnimationFrame(() => requestAnimationFrame(() => done()))),
  );
  await page.waitForFunction(() =>
    document.getAnimations().every(
      (a) => (a.playState !== 'running' && !a.pending) || a.effect?.getComputedTiming().iterations === Infinity,
    ),
  );
}

/**
 * Two elements paint nothing over text, but axe puts them on top of it
 * (`bgOverlap`), so the text goes unmeasured: the ripple element of a control
 * (hidden while empty) and the `fieldset` of an outlined field (only a border).
 * Returns the function that puts them back.
 */
async function hideTransparentOverlays(page: Page): Promise<() => Promise<void>> {
  const style = await page.addStyleTag({
    content: '.MuiTouchRipple-root:empty, .MuiOutlinedInput-notchedOutline { display: none !important; }',
  });
  return async () => {
    await style.evaluate((node) => (node as Element).remove());
  };
}

function describeNode(rule: string, node: AxeNode): string {
  const d = (node.any[0]?.data ?? {}) as {
    fgColor?: string;
    bgColor?: string;
    contrastRatio?: number;
    expectedContrastRatio?: string;
  };
  const colours = d.fgColor
    ? `fg ${d.fgColor} bg ${d.bgColor} ratio ${d.contrastRatio} (needs ${d.expectedContrastRatio}) `
    : '';
  return `  ${rule}: ${colours}${node.target.join(' ')}\n    ${node.html}`;
}

const messageKeyOf = (node: AxeNode): string => {
  for (const check of [...node.any, ...node.all, ...node.none]) {
    const key = (check.data as { messageKey?: string } | null)?.messageKey;
    if (key) return key;
  }
  return 'none';
};

/**
 * Run the rules and fail on a violation, on an undecided `color-contrast` node
 * outside KNOWN_UNDECIDED, and on a page whose content `color-contrast` checked
 * too little.
 * @param page - The loaded page
 * @param what - Name of the check, for the failure message
 * @param hasMain - Count the contrast passes inside <main> only
 */
export async function runAxe(page: Page, what: string, hasMain = true): Promise<void> {
  await waitForTransitions(page);
  await flattenGradients(page);
  const showOverlays = await hideTransparentOverlays(page);
  const results = await new AxeBuilder({ page })
    .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa', 'best-practice'])
    // One-character text is measured too, for example a badge count.
    .options({ checks: { 'color-contrast': { options: { ignoreLength: true } } } } as unknown as Parameters<AxeBuilder['options']>[0])
    .analyze();
  await showOverlays();

  const violations = results.violations.flatMap((v) => v.nodes.map((n) => describeNode(v.id, n)));
  expect(violations, `${what}: ${violations.length} axe violation(s)\n${violations.join('\n')}`).toEqual([]);

  const undecided: string[] = [];
  const reasons: Record<string, number> = {};
  for (const result of results.incomplete) {
    if (result.id !== 'color-contrast') continue;
    for (const node of result.nodes) {
      const reason = messageKeyOf(node);
      reasons[reason] = (reasons[reason] ?? 0) + 1;
      const selectors = KNOWN_UNDECIDED.filter((k) => k.rule === result.id && k.reason === reason).map((k) => k.selector);
      const known = await page
        .evaluate(
          ({ sel, list }) => {
            const el = document.querySelector(sel);
            return el !== null && list.some((s) => el.matches(s));
          },
          { sel: node.target[0] as string, list: selectors },
        )
        .catch(() => false);
      if (!known) undecided.push(`  ${result.id}, reason ${reason}: ${node.target.join(' ')}\n    ${node.html}`);
    }
  }
  expect(
    undecided,
    `${what}: axe could not check ${undecided.length} node(s), and KNOWN_UNDECIDED does not name them\n${undecided.join('\n')}`,
  ).toEqual([]);

  const passedNodes = results.passes.find((p) => p.id === 'color-contrast')?.nodes ?? [];
  const passed = hasMain
    ? await page.evaluate(
        (sels) =>
          sels.filter((sel) => {
            try {
              return Boolean(document.querySelector(sel)?.closest('main'));
            } catch {
              return false;
            }
          }).length,
        passedNodes.map((n) => n.target[0] as string),
      )
    : passedNodes.length;
  expect(
    passed,
    `${what}: only ${passed} color-contrast node(s) passed ${hasMain ? 'inside <main>' : 'on the page'}; is the page content empty?`,
  ).toBeGreaterThanOrEqual(MIN_CONTRAST_PASSES);

  // Shown in the HTML report; the reasons stay visible when nothing fails.
  test.info().annotations.push({
    type: 'axe',
    description: `${what}: color-contrast passes ${passedNodes.length} (${passed} in the content), incomplete ${JSON.stringify(reasons)}`,
  });
}

/**
 * Open a page in one colour mode, check that it really loaded, then run axe.
 * @param page - Playwright page
 * @param mode - Colour mode (stored the way the toggle stores it)
 * @param spec - What to open and what to expect
 */
export async function checkPage(page: Page, mode: Mode, spec: PageCheck): Promise<void> {
  const { path, heading } = spec;
  const what = `${path} (${mode})`;

  // The colour mode comes from localStorage, as the toggle stores it.
  await page.addInitScript((m) => localStorage.setItem('theme-mode', m), mode);

  const failed: string[] = [];
  page.on('response', (res) => {
    if (res.status() < 500 || !res.url().includes('/api/')) return;
    const key = `${res.request().method()} ${new URL(res.url()).pathname}`;
    if (!spec.allowed5xx?.some((re) => re.test(key))) failed.push(`${res.status()} ${key}`);
  });

  if (spec.signIn) await signInAs(page, path);
  else await page.goto(path);

  const url = () => new URL(page.url()).pathname;
  await expect.poll(url, { message: `${what}: final URL (a redirect to /login means no session)` }).toBe(path);
  await expect(
    page.getByRole('heading', { name: heading, level: 1, exact: true }),
    `${what}: heading "${heading}" is not visible`,
  ).toBeVisible({ timeout: 15_000 });
  await expect(page.locator('main'), `${what}: the page has no <main> element`).toBeVisible();
  await expect(page.locator('body'), `${what}: the ${mode} mode did not take effect`).toHaveCSS(
    'background-color',
    BODY_BACKGROUND[mode],
  );
  // No spinner may be left: axe would measure a half-loaded page.
  await expect(page.getByRole('progressbar'), `${what}: a spinner is still shown`).toHaveCount(0, { timeout: 15_000 });
  expect(failed, `${what}: API call(s) answered 5xx\n  ${failed.join('\n  ')}`).toEqual([]);

  await spec.beforeAxe?.(page);
  await runAxe(page, what);
}
