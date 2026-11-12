/**
 * Leave the SPA for a full-page navigation (for example the OIDC login
 * endpoint, which redirects to the identity provider). A separate function so
 * tests can replace it (jsdom does not navigate).
 * @param url - Same-origin URL to load
 */
export function redirectBrowser(url: string): void {
  window.location.assign(url);
}
