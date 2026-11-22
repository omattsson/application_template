import { createContext, useCallback, useContext, useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { Alert, Snackbar } from '@mui/material';

export type NotificationSeverity = 'success' | 'info' | 'warning' | 'error';

interface Notification {
  id: number;
  message: string;
  severity: NotificationSeverity;
}

interface NotificationContextValue {
  /** Show a message. Messages queue and show one at a time, in the order they were sent. */
  notify: (message: string, severity?: NotificationSeverity) => void;
}

const AUTO_HIDE_MS = 4000;

const NotificationContext = createContext<NotificationContextValue | null>(null);

/**
 * Owns the one Snackbar of the application and the queue behind it. A page
 * calls `useNotification().notify(...)` and keeps no toast state of its own.
 */
export function NotificationProvider({ children }: { children: ReactNode }) {
  // The notification on screen. It is kept while it fades out, so its text does not empty first.
  const [current, setCurrent] = useState<Notification | null>(null);
  const [open, setOpen] = useState(false);
  const queueRef = useRef<Notification[]>([]);
  // True from the moment a notification is shown until it has left the screen.
  const showingRef = useRef(false);
  const lastIdRef = useRef(0);

  const show = useCallback((next: Notification) => {
    showingRef.current = true;
    setCurrent(next);
    setOpen(true);
  }, []);

  const notify = useCallback(
    (message: string, severity: NotificationSeverity = 'info') => {
      lastIdRef.current += 1;
      const next = { id: lastIdRef.current, message, severity };
      if (showingRef.current) {
        queueRef.current.push(next);
      } else {
        show(next);
      }
    },
    [show],
  );

  const handleClose = (_event: unknown, reason?: string) => {
    if (reason === 'clickaway') return;
    setOpen(false);
  };

  // The one that was showing has left: show the next one, or leave the host idle.
  const handleExited = () => {
    const next = queueRef.current.shift();
    if (next) {
      show(next);
    } else {
      showingRef.current = false;
      setCurrent(null);
    }
  };

  const value = useMemo(() => ({ notify }), [notify]);

  return (
    <NotificationContext.Provider value={value}>
      {children}
      <Snackbar
        key={current?.id}
        open={open}
        autoHideDuration={AUTO_HIDE_MS}
        onClose={handleClose}
        slotProps={{ transition: { onExited: handleExited } }}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'right' }}
      >
        <Alert
          severity={current?.severity ?? 'info'}
          // An error interrupts a screen reader; the rest wait for a pause.
          role={current?.severity === 'error' ? 'alert' : 'status'}
          onClose={() => handleClose(null)}
          sx={{ width: '100%' }}
        >
          {current?.message}
        </Alert>
      </Snackbar>
    </NotificationContext.Provider>
  );
}

export function useNotification(): NotificationContextValue {
  const ctx = useContext(NotificationContext);
  if (!ctx) throw new Error('useNotification must be used within <NotificationProvider>');
  return ctx;
}
