import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, renderHook, screen, act, fireEvent } from '@testing-library/react';
import type { ReactNode } from 'react';
import { NotificationProvider, useNotification } from '../NotificationContext';

const wrapper = ({ children }: { children: ReactNode }) => (
  <NotificationProvider>{children}</NotificationProvider>
);

/** Renders the provider and returns its `notify`. */
function setup() {
  const { result } = renderHook(() => useNotification(), { wrapper });
  return result;
}

/** Moves the clock in small steps, so React renders between the auto-hide and the exit transition. */
async function elapse(ms: number) {
  for (let spent = 0; spent < ms; spent += 500) {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(500);
    });
  }
}

describe('NotificationContext', () => {
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it('throws when used outside NotificationProvider', () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    expect(() => renderHook(() => useNotification())).toThrow(
      'useNotification must be used within <NotificationProvider>',
    );
  });

  it('shows nothing until notify is called', () => {
    setup();
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('keeps the identity of notify across renders', () => {
    const { result, rerender } = renderHook(() => useNotification(), { wrapper });
    const first = result.current.notify;
    act(() => first('a'));
    rerender();
    expect(result.current.notify).toBe(first);
  });

  it('announces success, info and warning as a status', () => {
    const hook = setup();
    act(() => hook.current.notify('Saved', 'success'));
    const status = screen.getByRole('status');
    expect(status).toHaveTextContent('Saved');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('defaults to info', () => {
    const hook = setup();
    act(() => hook.current.notify('Hello'));
    expect(screen.getByRole('status')).toHaveClass('MuiAlert-colorInfo');
  });

  it('announces an error as an alert', () => {
    const hook = setup();
    act(() => hook.current.notify('Boom', 'error'));
    expect(screen.getByRole('alert')).toHaveTextContent('Boom');
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
  });

  it('shows queued messages one at a time, in order', async () => {
    vi.useFakeTimers();
    const hook = setup();
    act(() => {
      hook.current.notify('first', 'success');
      hook.current.notify('second', 'warning');
      hook.current.notify('third', 'error');
    });
    expect(screen.getByText('first')).toBeInTheDocument();
    expect(screen.queryByText('second')).not.toBeInTheDocument();
    expect(screen.queryByText('third')).not.toBeInTheDocument();

    await elapse(5000);
    expect(screen.queryByText('first')).not.toBeInTheDocument();
    expect(screen.getByText('second')).toBeInTheDocument();
    expect(screen.queryByText('third')).not.toBeInTheDocument();

    await elapse(5000);
    expect(screen.queryByText('second')).not.toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent('third');

    await elapse(5000);
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
  });

  it('shows the second message after the first was closed', async () => {
    const hook = setup();
    act(() => {
      hook.current.notify('first');
      hook.current.notify('second');
    });
    fireEvent.click(screen.getByRole('button', { name: /close/i }));
    expect(await screen.findByText('second')).toBeInTheDocument();
    expect(screen.queryByText('first')).not.toBeInTheDocument();
  });

  it('queues a message sent while the current one is leaving, then shows it', async () => {
    vi.useFakeTimers();
    const hook = setup();
    act(() => hook.current.notify('a'));
    fireEvent.click(screen.getByRole('button', { name: /close/i }));
    // `a` is on its way out but still on screen.
    act(() => hook.current.notify('b'));
    expect(screen.queryByText('b')).not.toBeInTheDocument();

    await elapse(1000);
    expect(screen.queryByText('a')).not.toBeInTheDocument();
    expect(screen.getByText('b')).toBeInTheDocument();
  });

  it('shows a message sent after the queue has drained', async () => {
    vi.useFakeTimers();
    const hook = setup();
    act(() => hook.current.notify('a'));
    await elapse(5000);
    expect(screen.queryByText('a')).not.toBeInTheDocument();
    act(() => hook.current.notify('b', 'error'));
    expect(screen.getByRole('alert')).toHaveTextContent('b');
  });

  it('hides a message by itself after a few seconds', async () => {
    vi.useFakeTimers();
    const hook = setup();
    act(() => hook.current.notify('gone soon'));
    expect(screen.getByText('gone soon')).toBeInTheDocument();
    await elapse(5000);
    expect(screen.queryByText('gone soon')).not.toBeInTheDocument();
  });

  it('is usable from a component below the provider', () => {
    const Button = () => {
      const { notify } = useNotification();
      return <button onClick={() => notify('clicked', 'success')}>go</button>;
    };
    render(
      <NotificationProvider>
        <Button />
      </NotificationProvider>,
    );
    fireEvent.click(screen.getByRole('button', { name: 'go' }));
    expect(screen.getByRole('status')).toHaveTextContent('clicked');
  });
});
