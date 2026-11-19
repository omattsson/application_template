# Frontend: TypeScript/React Instructions

## Project Setup

The frontend lives in `frontend/`: Vite, React 19, MUI v9, react-router-dom v7, TypeScript 6 in strict mode (`noUnusedLocals` and `noUnusedParameters` are enforced, target ES2020, JSX `react-jsx`). Node 22.12 or later. The file tree is in `docs/claude/structure.md` at the repository root.

## Architecture

- **Entry**: `src/main.tsx`, then `App.tsx` (providers: theme, UI config, auth, WebSocket; the `Layout`), then `routes.tsx`.
- **Routing**: `react-router-dom` with `<Routes>` and `<Route>` in `src/routes.tsx`. Every page except `/login`, `/auth/callback` and the 404 is wrapped in `ProtectedRoute`. A new page goes in `routes.tsx` and gets a navigation button in `components/Layout/index.tsx`.
- **API config**: `src/api/config.ts` sets `API_BASE_URL = '/api'`. The browser always calls the web UI origin: the Vite dev server and the bundled nginx forward `/api/*` (one `/api` is stripped) and `/ws` to the backend, so a request to `/api/api/v1/items` reaches the backend as `/api/v1/items`. Never hard-code the backend port in the SPA: the httpOnly refresh cookie is `SameSite=Strict` and only goes to the origin of the page.
- **API client**: `src/api/client.ts` has the Axios instance and the service objects (`itemService`, `authService`, `userService`, `uiConfigService`, `healthService`). Requests carry the bearer token; a 401 triggers one refresh and one retry.
- **Password reset**: `passwordResetService` (`/forgot-password`, `/reset-password`, registered only with `password_reset_enabled` and no session) uses `unauthenticatedApi`: no bearer header, no cookie, no 401 refresh. The token is read from the URL fragment and removed from the address at once; it is never stored or logged.
- **Session**: `src/api/session.ts` holds the auth mode and the in-memory access token (never in storage or a cookie) and announces tokens and logouts to the other tabs. `src/api/refresh.ts` refreshes once per tab and once across tabs (Web Lock plus `BroadcastChannel`), never aborts a refresh request and does not retry on its own failure.
- **Contexts**: `UIConfigContext` (title and auth flags from `GET /api/v1/ui-config`, public in every mode), `AuthContext` (session restore with `POST /auth/refresh`, login, logout, role), `WebSocketContext` (one socket that follows the session: a new socket for each new access token), `ThemeContext` (light and dark).
- **Auth off**: with `AUTH_ENABLED=false` the UI shows no login, no Users page and no role checks; `ProtectedRoute` renders the page directly.
- **WebSocket**: pages subscribe to message types through `useWebSocketContext().subscribe(type, handler)`, for example `item.created`. The Items page re-fetches on `item.*` messages.
- **Styling**: the MUI theme is in `src/theme/` (palette, typography, component overrides). Use theme tokens, never fixed colours.

## Component Patterns

- Use MUI components (no raw HTML for layout or controls) and the `sx` prop for styling.
- Use functional components only, with hooks (`useState`, `useEffect`, `useCallback`, `useRef`).
- Pages are directories under `src/pages/` with an `index.tsx` default export. Shared building blocks are in `src/components/`: `ConfirmDialog` (every destructive action), `EmptyState`, `LoadingState`, `ErrorBoundary`, `ProtectedRoute`, `Layout`.
- Use service objects with async methods for API calls. Each service object and method in `api/client.ts` needs a TSDoc comment with `@param`, `@returns` and `@see` (HTTP method and route).
- Convert an API error to text with `getApiErrorMessage` (`utils/apiError.ts`). Log an error with `summarizeRequestError` (`api/errorSummary.ts`), never the full Axios error: it holds the request body (a password) and the bearer header.
- Return paths after login go through `safeReturnPath` (`utils/returnPath.ts`): only same-origin paths.
- `useUnsavedChanges(dirty)` asks the browser to confirm before the tab is closed or reloaded while a form has unsaved edits. No page uses it yet; use it in a new form page.

## Role-Based UI

`<ProtectedRoute roles={['admin']}>` shows "Access denied" to a signed-in user without the role, and the Layout hides the navigation entry. Both are hints only: the API checks the role itself. Do not rely on the UI for access control.

## Page Pattern

- Show a loading state (`LoadingState`) first, then an error state (an `Alert` with `severity="error"`), then the content (or `EmptyState`).
- Page titles: `Typography variant="h4" component="h1"`.
- Wrap the content in MUI `Box` and `Paper` components.
- A delete asks for confirmation (`ConfirmDialog`) before it calls the API.

## Accessibility

The accessibility spec (`e2e/accessibility.spec.ts`) runs axe (WCAG 2.0 to 2.2 A and AA and the best practices) on each page in light and dark mode. `eslint-plugin-jsx-a11y` runs in `npm run lint`.

- Add a new page to `PAGES` in `e2e/accessibility.spec.ts` with what it needs (`needsAuth`, `needsSignIn`).
- A page whose precondition is missing is skipped with a console warning, unless `A11Y_REQUIRE_AUTH=1` is set, which turns the skip into a failure. `make test-e2e-auth` sets it, so Login and Admin Users are really checked. The decision is in `e2e/skipDecision.ts` and has a unit test.
- A node that axe leaves undecided for `color-contrast` fails. Add it to `KNOWN_UNDECIDED` in `e2e/a11y.ts` only with a selector and a reason.
- Behind text, use a theme token such as `action.hover`, not a fixed `grey.*`. Never show a state by colour alone: add a shape, a weight or text. An accessible name must contain the visible text, in the same order.
- In `sx`, a `width` or `height` of 1 means 100%. Write `'1px'` for one pixel.

## End-to-End Tests

`make test-e2e` runs with auth off, `make test-e2e-auth` with auth on (`E2E_SECURE_COOKIES=true` for the production cookie name). `E2E_AUTH=1` tells the specs that auth is on; `global-setup.ts` fails the run when that and the backend disagree. Tests that need a failed login use the non-admin account `e2e-user`, never the administrator: five failures lock a username (see `docs/claude/operations.md`, "End-to-end tests"). The credentials in the Makefile are test values only.

## TypeScript Config

- Target ES2020, strict mode; `noUnusedLocals` and `noUnusedParameters` are enforced.
- `npm run lint` runs `tsc --noEmit`, a second type check of the e2e files (`tsconfig.a11y.json`) and ESLint with at most 5 warnings.

## Development

- Dev server: `npm run dev` (port 3000, HMR; in Docker through `make dev`). It proxies `/api` and `/ws` to `VITE_API_URL` (default `http://localhost:8081`; Compose sets `http://backend:8081`). There is no `PORT` variable.
- Build: `npm run build` (`tsc`, then `vite build`).
- Lint: `npm run lint`. Format: `npm run format`.
- Tests: Vitest and Testing Library (unit, `npm test`), Playwright (e2e).
- Create a `__tests__/` directory next to each page or component. Add at least one render test. The auth test harness is in `src/test/authHarness.tsx`.
