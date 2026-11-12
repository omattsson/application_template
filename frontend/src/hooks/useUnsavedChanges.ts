import { useEffect } from 'react';

/**
 * Ask the browser to confirm before the page is closed or reloaded while a form has unsaved edits.
 * @param dirty - True while there are unsaved changes; the guard is only active then
 */
export function useUnsavedChanges(dirty: boolean): void {
  useEffect(() => {
    if (!dirty) return;
    const handler = (e: BeforeUnloadEvent) => {
      e.preventDefault();
    };
    window.addEventListener('beforeunload', handler);
    return () => window.removeEventListener('beforeunload', handler);
  }, [dirty]);
}
