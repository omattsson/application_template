# Frontend: TypeScript/React Instructions

## Project Setup

The frontend lives in `frontend/`: Vite, React 19, MUI v9, react-router-dom v7, TypeScript 6 in strict mode (`noUnusedLocals` and `noUnusedParameters` are enforced, target ES2020, JSX `react-jsx`). Node 22.12 or later. The file tree is in `docs/claude/structure.md` at the repository root.

## Architecture

- **Entry**: `src/main.tsx`, then `App.tsx` (providers: theme, UI config, auth, notifications, WebSocket; the `Layout`), then `routes.tsx`.
- **Routing**: `react-router-dom` with `<Routes>` and `<Route>` in `src/routes.tsx`. Every page except `/login`, `/auth/callback` and the 404 is wrapped in `ProtectedRoute`. A new page goes in `routes.tsx` and gets an entry in `NAV_ITEMS` (`components/Layout/navItems.ts`).
- **API config**: `src/api/config.ts` sets `API_BASE_URL = '/api'`. The browser always calls the web UI origin: the Vite dev server and the bundled nginx forward `/api/*` (one `/api` is stripped) and `/ws` to the backend, so a request to `/api/api/v1/items` reaches the backend as `/api/v1/items`. Never hard-code the backend port in the SPA: the httpOnly refresh cookie is `SameSite=Strict` and only goes to the origin of the page.
- **API client**: `src/api/client.ts` has the Axios instance and the service objects (`itemService`, `authService`, `userService`, `apiKeyService`, `uiConfigService`, `healthService`). Requests carry the bearer token; a 401 triggers one refresh and one retry.
- **Password reset**: `passwordResetService` (`/forgot-password`, `/reset-password`, registered only with `password_reset_enabled` and no session) uses `unauthenticatedApi`: no bearer header, no cookie, no 401 refresh. The token is read from the URL fragment and removed from the address at once; it is never stored or logged.
- **API keys page**: `pages/Account/ApiKeys` at `/account/api-keys` (`apiKeyService`: list, create, revoke) is registered only with `auth_enabled` and wrapped in `ProtectedRoute`; the Layout shows its "API keys" button only to a signed-in user. The create dialog shows the raw key once: it lives in that dialog's state only (the dialog is unmounted when it closes), never in the URL, storage or a log. Revoke asks with `ConfirmDialog`.
- **Account page**: `pages/Account/Profile` at `/account` shows the name, username, email, role and sign-in method from `AuthContext` (no request) and links to the API keys. For a local user (`auth_provider` missing or `local`) it has the change-password form (`authService.changePassword`, `PUT /auth/me/password`); an SSO user sees a notice instead. The route is registered only with `auth_enabled` and `local_auth_enabled` and wrapped in `ProtectedRoute`, and the header user name (`data-testid="current-user"`) is a link to it only where the route exists. The three passwords live in the page's state only: never logged, stored or put in the address, cleared after a success; the client checks (`passwordProblem`, mismatch, new differs from current) run before the request.
- **Password change and the 401 interceptor**: `authService.changePassword` must never go through the `api` instance. Its 401 interceptor refreshes and retries, and a retry of a wrong current password counts a second failure on the account (5 failures lock it). It uses `authApi` with `CSRF_HEADERS` and an explicit bearer header, so one submit is one request. The one exception is an expired access token: a 401 whose text is not `Current password is incorrect` (the middleware refuses it before the password is checked, so no failure is counted) refreshes once through `refreshAccessToken` and sends the request once more; a refused or failed refresh throws the first 401 and the page asks the user to sign in again.
- **Audit log page**: `pages/Admin/AuditLogs` at `/admin/audit-logs` (`auditLogService.list`: offset paging, filters on action, entity type, user ID and a date range) is registered only with `auth_enabled` and wrapped in `ProtectedRoute roles={['admin']}`; the Layout shows its "Audit log" button only to an admin. The filter lists are copies of the backend's known lists (`options.ts`). Details are shown as text only, behind a per-row toggle. The page reads only; the API checks the role.
- **Session**: `src/api/session.ts` holds the auth mode and the in-memory access token (never in storage or a cookie) and announces tokens and logouts to the other tabs. `src/api/refresh.ts` refreshes once per tab and once across tabs (Web Lock plus `BroadcastChannel`), never aborts a refresh request and does not retry on its own failure.
- **Contexts**: `UIConfigContext` (title and auth flags from `GET /api/v1/ui-config`, public in every mode), `AuthContext` (session restore with `POST /auth/refresh`, login, logout, role), `WebSocketContext` (one socket that follows the session: a new socket for each new access token), `NotificationContext` (the one toast host and its queue, see "Notifications"), `ThemeContext` (light and dark).
- **Layout and Home**: `Layout` shows `config.title` from `UIConfigContext` in the app bar and the footer and sets `document.title` (nothing while the config loads, so the fallback name does not flash). Below the `sm` breakpoint (`useMediaQuery`) the navigation links and the account items (user name, API keys, Log out) move into a temporary `Drawer` opened by the "Open navigation menu" button; the theme toggle stays in the app bar. `NAV_ITEMS` and `useNavItems()` (`components/Layout/navItems.ts`) hold the links and their visibility rules once, for the app bar, the drawer and the Home page ("Where to start"). A test of the narrow case mocks `window.matchMedia` (JSDOM has none).
- **Auth off**: with `AUTH_ENABLED=false` the UI shows no login, no Users page and no role checks; `ProtectedRoute` renders the page directly.
- **WebSocket**: pages subscribe to message types through `useWebSocketContext().subscribe(type, handler)`, for example `item.created`. The Items page re-fetches its current page (with the sort and the name filter) on `item.*` messages. Its page, size, sort, order and filter are the URL query (`page`, `size`, `sort`, `order`, `q`), so a reload keeps them.
- **Styling**: the MUI theme is in `src/theme/` (palette, typography, component overrides). Use theme tokens, never fixed colours.

## Component Patterns

- Use MUI components (no raw HTML for layout or controls) and the `sx` prop for styling.
- Use functional components only, with hooks (`useState`, `useEffect`, `useCallback`, `useRef`).
- Pages are directories under `src/pages/` with an `index.tsx` default export. Shared building blocks are in `src/components/`: `ConfirmDialog` (every destructive action), `EmptyState`, `LoadingState`, `ErrorBoundary`, `ProtectedRoute`, `Layout`.
- Use service objects with async methods for API calls. Each service object and method in `api/client.ts` needs a TSDoc comment with `@param`, `@returns` and `@see` (HTTP method and route).
- Convert an API error to text with `getApiErrorMessage` (`utils/apiError.ts`). Log an error with `summarizeRequestError` (`api/errorSummary.ts`), never the full Axios error: it holds the request body (a password) and the bearer header.
- Return paths after login go through `safeReturnPath` (`utils/returnPath.ts`): only same-origin paths.
- `useUnsavedChanges(dirty)` asks the browser to confirm before the tab is closed or reloaded while a form has unsaved edits. The Account and Reset password pages use it; use it in a new form page.

## Role-Based UI

`<ProtectedRoute roles={['admin']}>` shows "Access denied" to a signed-in user without the role, and `useNavItems` hides the navigation entry. Both are hints only: the API checks the role itself. Do not rely on the UI for access control.

## Page Pattern

- Show a loading state (`LoadingState`) first, then an error state (an `Alert` with `severity="error"`), then the content (or `EmptyState`).
- Page titles: `Typography variant="h4" component="h1"`.
- Wrap the content in MUI `Box` and `Paper` components.
- A delete asks for confirmation (`ConfirmDialog`) before it calls the API.

## Notifications

A page never owns a `Snackbar` or toast state. It calls `const { notify } = useNotification()` (`context/NotificationContext.tsx`) and `notify(message, severity)` with `'success'`, `'info'` (the default), `'warning'` or `'error'`. `NotificationProvider` in `App.tsx` renders the one host: messages queue and show one at a time (4 s each, closable), and an `error` is announced as `role="alert"`, the others as `role="status"`.

- Use it for the result of an action (created, deleted), for `item.*` WebSocket messages and for an API error of an action: `notify(getApiErrorMessage(err), 'error')`.
- Keep the error of a page load as an inline `Alert` (see Page Pattern), and an error that belongs to a dialog or a form in that dialog or form.
- `notify` has a stable identity: it can go in an effect's dependency list. A test of a page that notifies wraps it in `NotificationProvider`.

## Accessibility

The accessibility spec (`e2e/accessibility.spec.ts`) runs axe (WCAG 2.0 to 2.2 A and AA and the best practices) on each page in light and dark mode. `eslint-plugin-jsx-a11y` runs in `npm run lint`.

- Add a new page to `PAGES` in `e2e/accessibility.spec.ts` with what it needs (`needsAuth`, `needsSignIn`).
- A page whose precondition is missing is skipped with a console warning, unless `A11Y_REQUIRE_AUTH=1` is set, which turns the skip into a failure. `make test-e2e-auth` sets it, so Login, Admin Users and the Admin audit log are really checked. The decision is in `e2e/skipDecision.ts` and has a unit test.
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
