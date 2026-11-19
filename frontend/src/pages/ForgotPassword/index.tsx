import { useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { isAxiosError } from 'axios';
import { Link as RouterLink } from 'react-router-dom';
import { Alert, Box, Button, Link, Paper, TextField, Typography } from '@mui/material';
import { passwordResetService } from '../../api/client';
import { useUnsavedChanges } from '../../hooks/useUnsavedChanges';
import { NEUTRAL_RESET_MESSAGE } from '../../utils/passwordReset';

const USERNAME_REQUIRED = 'Enter your username.';
const USERNAME_REJECTED = 'The username could not be used. Check it and try again. It can be up to 100 characters long.';
/** Longest username the server takes, in UTF-8 bytes. */
const USERNAME_MAX_BYTES = 100;

/**
 * Request a password reset mail. The answer never tells whether the account
 * exists: after a submit the page shows the same confirmation for every result
 * (a 202, a network error, a 429, a 5xx). Only a 400 (a username the server
 * cannot take) and an empty field get their own message, because they say
 * nothing about the account.
 */
const ForgotPassword = () => {
  const [username, setUsername] = useState('');
  const [usernameError, setUsernameError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [sent, setSent] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  const confirmationRef = useRef<HTMLDivElement>(null);

  useUnsavedChanges(!sent && username !== '');

  useEffect(() => {
    if (sent) confirmationRef.current?.focus();
    else inputRef.current?.focus();
  }, [sent]);

  const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const name = username.trim();
    if (name === '') {
      setUsernameError(USERNAME_REQUIRED);
      inputRef.current?.focus();
      return;
    }
    if (new TextEncoder().encode(name).length > USERNAME_MAX_BYTES) {
      setUsernameError(USERNAME_REJECTED);
      inputRef.current?.focus();
      return;
    }
    setUsernameError(null);
    setSubmitting(true);
    try {
      await passwordResetService.requestPasswordReset(name);
      setSent(true);
    } catch (err: unknown) {
      if (isAxiosError(err) && err.response?.status === 400) {
        setUsernameError(USERNAME_REJECTED);
        inputRef.current?.focus();
      } else {
        setSent(true);
      }
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Box sx={{ display: 'flex', justifyContent: 'center', alignItems: 'center', minHeight: '60vh', px: 2 }}>
      <Paper sx={{ p: 4, maxWidth: 400, width: '100%' }}>
        <Typography variant="h5" component="h1" gutterBottom sx={{ textAlign: 'center' }}>
          Forgot password
        </Typography>

        {sent ? (
          <>
            <Alert severity="info" role="status" tabIndex={-1} ref={confirmationRef} sx={{ mb: 3 }}>
              {NEUTRAL_RESET_MESSAGE}
            </Alert>
            <Link component={RouterLink} to="/login" variant="body2">
              Back to sign in
            </Link>
          </>
        ) : (
          <>
            <Typography variant="body2" color="text.secondary" sx={{ mb: 3 }}>
              Enter your username. If the account can receive mail, we send a link to choose a new password.
            </Typography>
            <Box component="form" onSubmit={handleSubmit} noValidate>
              <TextField
                inputRef={inputRef}
                label="Username"
                name="username"
                autoComplete="username"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                error={usernameError !== null}
                helperText={usernameError ?? undefined}
                fullWidth
                required
                sx={{ mb: 3 }}
              />
              <Button type="submit" variant="contained" fullWidth size="large" disabled={submitting}>
                {submitting ? 'Sending…' : 'Send reset link'}
              </Button>
            </Box>
            <Box sx={{ mt: 2, textAlign: 'center' }}>
              <Link component={RouterLink} to="/login" variant="body2">
                Back to sign in
              </Link>
            </Box>
          </>
        )}
      </Paper>
    </Box>
  );
};

export default ForgotPassword;
