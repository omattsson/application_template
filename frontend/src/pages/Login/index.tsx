import { useState } from 'react';
import type { FormEvent } from 'react';
import { Navigate, useLocation, useSearchParams } from 'react-router-dom';
import { Alert, Box, Button, Divider, Paper, TextField, Typography } from '@mui/material';
import LoginIcon from '@mui/icons-material/Login';
import LoadingState from '../../components/LoadingState';
import { useAuth } from '../../context/AuthContext';
import { useUIConfig } from '../../context/UIConfigContext';
import { loginNoticeFrom } from '../../utils/loginNotice';
import { safeReturnPath } from '../../utils/returnPath';
import { describeLoginError } from './loginError';

/**
 * Sign-in page. Shows the password form when `local_auth_enabled` and the SSO
 * button when `oidc_enabled`. After sign-in it goes to the `next` parameter
 * (validated: same-origin path only) or home. With auth off it goes home. A
 * known notice key in the router state (see utils/loginNotice) explains why
 * the user was sent here.
 */
const Login = () => {
  const { authEnabled, isLoading, isAuthenticated, login, loginWithOIDC } = useAuth();
  const { config } = useUIConfig();
  const [searchParams] = useSearchParams();
  // Set by the app itself (router state, a key of a closed list), never by the URL.
  const notice = loginNoticeFrom(useLocation().state);
  const next = safeReturnPath(searchParams.get('next'));
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [ssoStarting, setSsoStarting] = useState(false);

  if (isLoading) return <LoadingState />;
  if (!authEnabled) return <Navigate to="/" replace />;
  if (isAuthenticated) return <Navigate to={next} replace />;

  const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setError(null);
    setSubmitting(true);
    try {
      await login(username, password);
      // isAuthenticated now redirects to `next`.
    } catch (err: unknown) {
      setError(describeLoginError(err));
      setPassword('');
    } finally {
      setSubmitting(false);
    }
  };

  const handleSso = () => {
    setSsoStarting(true);
    loginWithOIDC(next);
  };

  const showLocal = config.local_auth_enabled;
  const showSso = config.oidc_enabled;

  return (
    <Box sx={{ display: 'flex', justifyContent: 'center', alignItems: 'center', minHeight: '60vh', px: 2 }}>
      <Paper sx={{ p: 4, maxWidth: 400, width: '100%' }}>
        <Typography variant="h6" component="p" sx={{ textAlign: 'center', fontWeight: 700 }}>
          {config.title}
        </Typography>
        <Typography variant="h5" component="h1" gutterBottom sx={{ textAlign: 'center' }}>
          Sign in
        </Typography>

        {notice && (
          <Alert severity="info" role="status" sx={{ mb: 2 }}>
            {notice}
          </Alert>
        )}

        {error && (
          <Alert severity="error" sx={{ mb: 2 }}>
            {error}
          </Alert>
        )}

        {showSso && (
          <Button
            variant="contained"
            fullWidth
            size="large"
            startIcon={<LoginIcon />}
            onClick={handleSso}
            disabled={ssoStarting}
          >
            Sign in with SSO
          </Button>
        )}

        {showSso && showLocal && (
          <Divider sx={{ my: 3 }}>
            <Typography variant="body2" color="text.secondary">
              or sign in with a password
            </Typography>
          </Divider>
        )}

        {showLocal && (
          <Box component="form" onSubmit={handleSubmit} sx={{ mt: showSso ? 0 : 2 }}>
            <TextField
              label="Username"
              name="username"
              autoComplete="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              fullWidth
              required
              sx={{ mb: 2 }}
            />
            <TextField
              label="Password"
              name="password"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              fullWidth
              required
              sx={{ mb: 3 }}
            />
            <Button
              type="submit"
              variant={showSso ? 'outlined' : 'contained'}
              fullWidth
              size="large"
              disabled={submitting}
            >
              {submitting ? 'Signing in…' : 'Sign in'}
            </Button>
          </Box>
        )}

        {!showLocal && !showSso && (
          <Alert severity="info">No sign-in method is enabled. Contact your administrator.</Alert>
        )}
      </Paper>
    </Box>
  );
};

export default Login;
