/**
 * Leave the SPA for a full-page navigation (for example the OIDC login
 * endpoint, which redirects to the identity provider). A separate function so
 * tests can replace it (jsdom does not navigate).
 * @param url - Same-origin URL to load
 */
export function redirectBrowser(url: string): void {
  window.location.assign(url);
}

/**
 * Load the current page again from the server (a full reload). A separate
 * function so tests can replace it (jsdom cannot reload).
 */
export function reloadPage(): void {
  window.location.reload();
}
