import { useAuth } from '../../context/AuthContext';

/** One page of the navigation (app bar, drawer and the Home page share the list). */
export interface NavItem {
  label: string;
  path: string;
  /** One line for the Home page. */
  description: string;
  /** User administration exists with auth on only, and the API requires the admin role (a UI hint). */
  adminOnly?: boolean;
}

export const NAV_ITEMS: readonly NavItem[] = [
  { label: 'Home', path: '/', description: 'This page.' },
  { label: 'Health', path: '/health', description: 'Liveness and readiness of the API and its dependencies.' },
  { label: 'Items', path: '/items', description: 'The example resource: list, create, edit and delete.' },
  { label: 'Users', path: '/admin/users', description: 'Manage user accounts and roles.', adminOnly: true },
  { label: 'Audit log', path: '/admin/audit-logs', description: 'Who changed what, and when.', adminOnly: true },
];

/**
 * The navigation items this visitor may see. Nothing while the session loads
 * (no flicker); with auth on only for signed-in users (the pages would redirect
 * to login); the admin items for an admin with auth on.
 * @returns The visible items, in display order
 */
export function useNavItems(): NavItem[] {
  const { authEnabled, isLoading, isAuthenticated, role } = useAuth();
  const showNavigation = !isLoading && (!authEnabled || isAuthenticated);
  if (!showNavigation) return [];
  const showAdmin = authEnabled && role === 'admin';
  return NAV_ITEMS.filter((item) => !item.adminOnly || showAdmin);
}
