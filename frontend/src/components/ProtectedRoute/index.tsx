import type { ReactNode } from 'react';
import { Navigate, useLocation, Link as RouterLink } from 'react-router-dom';
import { Alert, Box, Button, Typography } from '@mui/material';
import LoadingState from '../LoadingState';
import { useAuth } from '../../context/AuthContext';
import type { Role } from '../../utils/jwt';
import { loginPathFor } from '../../utils/returnPath';

interface ProtectedRouteProps {
  children: ReactNode;
  /** Roles that may see the page. Omitted: every signed-in user. A UI hint only: the API checks the role itself. */
  roles?: readonly Role[];
}

/** Shown to a signed-in user without one of the required roles (never a redirect, so no loop). */
const Forbidden = () => (
  <Box sx={{ mt: 4, display: 'flex', flexDirection: 'column', alignItems: 'flex-start', gap: 2 }}>
    <Typography variant="h4" component="h1">
      Access denied
    </Typography>
    <Alert severity="error">You do not have permission to see this page.</Alert>
    <Button variant="contained" component={RouterLink} to="/">
      Back to Home
    </Button>
  </Box>
);

/**
 * Guard of a page. With auth off it renders the page directly. With auth on
 * it shows a loading state while the session is restored (no flash of the
 * login page), sends an anonymous visitor to `/login?next=<page>`, and shows
 * Access denied for a missing role.
 */
const ProtectedRoute = ({ children, roles }: ProtectedRouteProps) => {
  const { authEnabled, isLoading, isAuthenticated, role } = useAuth();
  const location = useLocation();

  if (isLoading) return <LoadingState />;
  if (!authEnabled) return <>{children}</>;
  if (!isAuthenticated) return <Navigate to={loginPathFor(location)} replace />;
  if (roles && (role === null || !roles.includes(role))) return <Forbidden />;
  return <>{children}</>;
};

export default ProtectedRoute;
