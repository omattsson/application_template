import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Link, MemoryRouter } from 'react-router-dom';
import AppRoutes from '../routes';

vi.mock('../pages/Home', () => ({ default: () => <h1>Home page</h1> }));
vi.mock('../pages/Health', () => ({ default: () => <h1>Health page</h1> }));
const itemsState = { shouldThrow: false };
vi.mock('../pages/Items', () => ({
  default: () => {
    if (itemsState.shouldThrow) throw new Error('Items exploded');
    return <h1>Items page</h1>;
  },
}));

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AppRoutes />
    </MemoryRouter>,
  );
}

describe('AppRoutes', () => {
  afterEach(() => {
    itemsState.shouldThrow = false;
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it.each([
    ['/', 'Home page'],
    ['/health', 'Health page'],
    ['/items', 'Items page'],
  ])('renders %s', (path, heading) => {
    renderAt(path);
    expect(screen.getByRole('heading', { name: heading })).toBeInTheDocument();
  });

  it('renders NotFound for an unknown path', () => {
    renderAt('/no/such/page');
    expect(screen.getByText('404')).toBeInTheDocument();
    expect(screen.getByText('Page not found')).toBeInTheDocument();
  });

  it('contains a page crash in the error boundary and recovers on navigation', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    itemsState.shouldThrow = true;
    const user = userEvent.setup();
    render(
      <MemoryRouter initialEntries={['/items']}>
        <Link to="/health">Go to health</Link>
        <AppRoutes />
      </MemoryRouter>,
    );
    expect(screen.getByRole('heading', { name: 'Something went wrong' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Go to health' })).toBeInTheDocument();

    await user.click(screen.getByRole('link', { name: 'Go to health' }));
    expect(screen.getByRole('heading', { name: 'Health page' })).toBeInTheDocument();
  });
});
