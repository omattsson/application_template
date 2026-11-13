---
applyTo: "**/*.{ts,tsx}"
---

# TypeScript/React Frontend Instructions

The frontend rules are in `frontend/CLAUDE.md`; the repository-wide rules are in `CLAUDE.md`. Summary:

- Vite, React 19, MUI v9, react-router-dom v7, TypeScript 6 (strict; `noUnusedLocals` and `noUnusedParameters` are enforced). Node 22.12 or later.
- Entry `src/main.tsx`, then `App.tsx` (providers) and `routes.tsx`. A page is a directory `src/pages/<Name>/index.tsx`, registered in `routes.tsx` inside `ProtectedRoute` (with `roles` for an admin page) and linked in `components/Layout/index.tsx`.
- The API is always same-origin: `API_BASE_URL` is `/api` and the dev server and nginx strip one `/api`, so a call is `/api/api/v1/...`. Never call the backend port from the browser (the refresh cookie is `SameSite=Strict`).
- API calls go through service objects in `src/api/client.ts` with a TSDoc comment (`@param`, `@returns`, `@see`). Log errors with `summarizeRequestError`, never the full Axios error.
- The access token lives in memory (`src/api/session.ts`), never in storage. Do not add token handling to pages.
- Functional components, hooks, MUI components and the `sx` prop; no separate CSS files. Loading state, then error state, then content. `ConfirmDialog` before a delete. Page titles use `Typography variant="h4" component="h1"`.
- Role checks in the UI are hints only; the API enforces roles.
- Accessibility is tested with axe in Playwright (`e2e/accessibility.spec.ts`): add each new page to `PAGES`. Never show a state by colour alone.
- Tests: Vitest and Testing Library next to the code (`__tests__/`), Playwright in `e2e/`.
- Commands: `npm run dev`, `npm run build`, `npm run lint`, `npm test`.
