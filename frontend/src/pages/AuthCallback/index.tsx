import { useEffect, useState } from 'react';
import { Navigate, useSearchParams, Link as RouterLink } from 'react-router-dom';
import { Alert, Box, Button, Paper } from '@mui/material';
import LoadingState from '../../components/LoadingState';
import { clearOidcReturnPath, readOidcReturnPath, useAuth } from '../../context/AuthContext';

export const CALLBACK_ERROR_MESSAGE = 'Sign-in could not be completed. Please try again.';

/**
 * Landing page after the OIDC login (backend OIDC_FRONTEND_PATH, default
 * /auth/callback). The backend has set the refresh cookie and redirected here
 * without any token in the URL; the access token comes from POST
 * /api/v1/auth/refresh, which the AuthProvider's session restore sends once
 * on page load (single flight, never aborted). This page waits for it, then
 * goes to the page stored before the login (validated) or shows an error.
 *
 * A refused callback never lands here: the backend answers it with a JSON
 * error (400 "Login failed", 403, 502). An `error` parameter is still shown.
 */
const AuthCallback = () => {
  const { authEnabled, isLoading, isAuthenticated } = useAuth();
  const [searchParams] = useSearchParams();
  const failedAtProvider = searchParams.has('error');
  // Read once; removed when the outcome is known (no stale path for a later login).
  const [returnPath] = useState(readOidcReturnPath);

  useEffect(() => {
    if (!isLoading) clearOidcReturnPath();
  }, [isLoading]);

  if (isLoading && !failedAtProvider) return <LoadingState label="Completing sign-in…" />;
  if (!isLoading && !authEnabled) return <Navigate to="/" replace />;
  if (!failedAtProvider && isAuthenticated) return <Navigate to={returnPath} replace />;

  return (
    <Box sx={{ display: 'flex', justifyContent: 'center', alignItems: 'center', minHeight: '60vh', px: 2 }}>
      <Paper sx={{ p: 4, maxWidth: 440, width: '100%', textAlign: 'center' }}>
        <Alert severity="error" sx={{ mb: 3, textAlign: 'left' }}>
          {CALLBACK_ERROR_MESSAGE}
        </Alert>
        <Button variant="contained" component={RouterLink} to="/login">
          Back to sign in
        </Button>
      </Paper>
    </Box>
  );
};

export default AuthCallback;
