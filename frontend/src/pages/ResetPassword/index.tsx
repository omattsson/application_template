import { useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { isAxiosError } from 'axios';
import { Link as RouterLink, useNavigate } from 'react-router-dom';
import { Alert, Box, Button, Link, Paper, TextField, Typography } from '@mui/material';
import { passwordResetService } from '../../api/client';
import LoadingState from '../../components/LoadingState';
import { useUnsavedChanges } from '../../hooks/useUnsavedChanges';
import { errorTextOf } from '../../utils/apiError';
import { rememberLoginNotice, RESET_CHECK_LOGIN, RESET_COMPLETED } from '../../utils/loginNotice';
import {
  PASSWORD_MISMATCH_MESSAGE,
  PASSWORD_TOO_LONG_MESSAGE,
  PASSWORD_TOO_SHORT_MESSAGE,
  passwordProblem,
  readResetToken,
  removeTokenFromAddress,
} from '../../utils/passwordReset';

const INVALID_LINK_MESSAGE = 'This reset link is not valid. Open the link from the email again, or request a new reset link.';
const EXPIRED_LINK_MESSAGE = 'This reset link is invalid or has expired. Request a new reset link.';
const RATE_LIMITED_MESSAGE = 'Too many attempts. Wait a few minutes and try again.';
const NETWORK_MESSAGE = 'Your password could not be reset. Check your connection and try again.';
const GENERIC_MESSAGE = 'Your password could not be reset. Try again.';
const RULES_HELP = 'At least 8 characters, at most 72 bytes.';

/** The two 400 texts of the password rules: the only server text that is shown. */
const PASSWORD_RULE_MESSAGES: readonly string[] = [PASSWORD_TOO_SHORT_MESSAGE, PASSWORD_TOO_LONG_MESSAGE];

/**
 * Set a new password from the mail link. The token is read from the URL
 * fragment once, the fragment is removed from the address bar at once, and the
 * token lives in this component only: it is never stored, logged or shown.
 * Success goes to Login with a notice. A 400 for the token shows one generic
 * message with a way to a new link; a 400 for the password keeps the form (the
 * token is not used up on the server).
 */
const ResetPassword = () => {
  const navigate = useNavigate();
  // undefined until the fragment was read, then the token or null (no valid token).
  const [token, setToken] = useState<string | null | undefined>(undefined);
  const [tokenRejected, setTokenRejected] = useState(false);
  const [password, setPassword] = useState('');
  const [confirmation, setConfirmation] = useState('');
  const [passwordError, setPasswordError] = useState<string | null>(null);
  const [confirmError, setConfirmError] = useState<string | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  // Keeps the first reading: a React StrictMode re-run of the effect sees the fragment already removed.
  const captured = useRef<string | null | undefined>(undefined);
  const passwordRef = useRef<HTMLInputElement>(null);
  const confirmRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (captured.current === undefined) {
      captured.current = readResetToken(window.location.hash);
      removeTokenFromAddress();
    }
    setToken(captured.current);
  }, []);

  const showForm = typeof token === 'string' && !tokenRejected;

  useEffect(() => {
    if (showForm) passwordRef.current?.focus();
  }, [showForm]);

  useUnsavedChanges(showForm && !submitting && (password !== '' || confirmation !== ''));

  if (token === undefined) return <LoadingState />;

  if (!showForm) {
    return (
      <Box sx={{ display: 'flex', justifyContent: 'center', alignItems: 'center', minHeight: '60vh', px: 2 }}>
        <Paper sx={{ p: 4, maxWidth: 400, width: '100%' }}>
          <Typography variant="h5" component="h1" gutterBottom sx={{ textAlign: 'center' }}>
            Reset password
          </Typography>
          <Alert severity="error" sx={{ mb: 3 }}>
            {tokenRejected ? EXPIRED_LINK_MESSAGE : INVALID_LINK_MESSAGE}
          </Alert>
          <Button component={RouterLink} to="/forgot-password" variant="contained" fullWidth size="large">
            Request a new reset link
          </Button>
        </Paper>
      </Box>
    );
  }

  const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (typeof token !== 'string') return;
    setFormError(null);
    const problem = passwordProblem(password);
    if (problem) {
      setPasswordError(problem);
      setConfirmError(null);
      passwordRef.current?.focus();
      return;
    }
    setPasswordError(null);
    if (password !== confirmation) {
      setConfirmError(PASSWORD_MISMATCH_MESSAGE);
      confirmRef.current?.focus();
      return;
    }
    setConfirmError(null);
    setSubmitting(true);
    try {
      await passwordResetService.confirmPasswordReset(token, password);
      rememberLoginNotice(RESET_COMPLETED);
      navigate('/login', { replace: true });
    } catch (err: unknown) {
      handleFailure(err);
    } finally {
      setSubmitting(false);
    }
  };

  const handleFailure = (err: unknown) => {
    const status = isAxiosError(err) ? err.response?.status : undefined;
    if (status === 400) {
      const text = errorTextOf(err);
      if (text !== null && PASSWORD_RULE_MESSAGES.includes(text)) {
        setPasswordError(text);
        passwordRef.current?.focus();
        return;
      }
      // Any other 400 is the token (the server gives one generic text for it).
      setPassword('');
      setConfirmation('');
      setTokenRejected(true);
      return;
    }
    if (status === 500) {
      // The password was stored, but the sessions could not be ended: sign in with it.
      rememberLoginNotice(RESET_CHECK_LOGIN);
      navigate('/login', { replace: true });
      return;
    }
    if (status === 429) {
      setFormError(RATE_LIMITED_MESSAGE);
      return;
    }
    setFormError(isAxiosError(err) && status === undefined ? NETWORK_MESSAGE : GENERIC_MESSAGE);
  };

  return (
    <Box sx={{ display: 'flex', justifyContent: 'center', alignItems: 'center', minHeight: '60vh', px: 2 }}>
      <Paper sx={{ p: 4, maxWidth: 400, width: '100%' }}>
        <Typography variant="h5" component="h1" gutterBottom sx={{ textAlign: 'center' }}>
          Set a new password
        </Typography>
        <Typography variant="body2" color="text.secondary" sx={{ mb: 3 }}>
          Choose the password that you will use to sign in from now on.
        </Typography>

        {formError && (
          <Alert severity="error" sx={{ mb: 2 }}>
            {formError}
          </Alert>
        )}

        <Box component="form" onSubmit={handleSubmit} noValidate>
          <TextField
            inputRef={passwordRef}
            label="New password"
            name="password"
            type="password"
            autoComplete="new-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            error={passwordError !== null}
            helperText={passwordError ?? RULES_HELP}
            fullWidth
            required
            sx={{ mb: 2 }}
          />
          <TextField
            inputRef={confirmRef}
            label="Confirm new password"
            name="confirmation"
            type="password"
            autoComplete="new-password"
            value={confirmation}
            onChange={(e) => setConfirmation(e.target.value)}
            error={confirmError !== null}
            helperText={confirmError ?? undefined}
            fullWidth
            required
            sx={{ mb: 3 }}
          />
          <Button type="submit" variant="contained" fullWidth size="large" disabled={submitting}>
            {submitting ? 'Saving…' : 'Reset password'}
          </Button>
        </Box>

        <Box sx={{ mt: 2, textAlign: 'center' }}>
          <Link component={RouterLink} to="/login" variant="body2">
            Back to sign in
          </Link>
        </Box>
      </Paper>
    </Box>
  );
};

export default ResetPassword;
