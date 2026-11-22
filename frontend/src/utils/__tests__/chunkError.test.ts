import { describe, it, expect } from 'vitest';
import { isChunkLoadError } from '../chunkError';

describe('isChunkLoadError', () => {
  it.each([
    ['Chrome', new TypeError('Failed to fetch dynamically imported module: https://x/assets/Items-abc.js')],
    ['Firefox', new TypeError('error loading dynamically imported module: https://x/assets/Items-abc.js')],
    ['Safari', new TypeError('Importing a module script failed.')],
    ['Vite CSS preload', new Error('Unable to preload CSS for /assets/Items-abc.css')],
    ['a named ChunkLoadError', Object.assign(new Error('Loading chunk 3 failed.'), { name: 'ChunkLoadError' })],
  ])('recognises the %s message', (_browser, error) => {
    expect(isChunkLoadError(error)).toBe(true);
  });

  it.each([
    ['an ordinary error', new Error('Test error')],
    ['a render error', new TypeError("Cannot read properties of undefined (reading 'name')")],
    ['a string', 'Failed to fetch dynamically imported module'],
    ['null', null],
    ['undefined', undefined],
  ])('does not take %s for a chunk failure', (_what, value) => {
    expect(isChunkLoadError(value)).toBe(false);
  });
});
