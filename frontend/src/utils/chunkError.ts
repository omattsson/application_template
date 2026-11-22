/**
 * True when an error is a page chunk that failed to load (a dynamic `import()`
 * that was refused or answered 404). It happens after a redeploy, when the
 * open page still points at hashed files that are gone, or when the network drops.
 * Browsers word it differently; Webpack-style loaders name the error.
 */
export function isChunkLoadError(error: unknown): boolean {
  if (!(error instanceof Error)) return false;
  if (error.name === 'ChunkLoadError') return true;
  return /Failed to fetch dynamically imported module|error loading dynamically imported module|Importing a module script failed|Unable to preload CSS/i.test(
    error.message,
  );
}
