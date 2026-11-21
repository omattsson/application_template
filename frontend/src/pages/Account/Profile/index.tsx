import { useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { isAxiosError } from 'axios';
import { Link as RouterLink } from 'react-router-dom';
import { Alert, Box, Button, Link, Paper, TextField, Typography } from '@mui/material';
import { authService } from '../../../api/client';
import { useAuth } from '../../../context/AuthContext';
import { useUnsavedChanges } from '../../../hooks/useUnsavedChanges';
import { getApiErrorMessage } from '../../../utils/apiError';
import {
  CURRENT_PASSWORD_INCORRECT_MESSAGE,
  PASSWORD_UNCHANGED_MESSAGE,
  describeWait,
  errorTextOf,
  retryAfterSeconds,
} from '../../../utils/passwordChange';
import { PASSWORD_MISMATCH_MESSAGE, passwordProblem } from '../../../utils/passwordReset';

const CURRENT_REQUIRED_MESSAGE = 'Enter your current password.';
const RULES_HELP = 'At least 8 characters, at most 72 bytes, and not your current password.';
const FIELD_ERRORS_SUMMARY = 'Your password was not changed. Fix the fields with an error.';
const SESSION_EXPIRED_MESSAGE = 'Your session has expired. Sign in again to change your password.';
const MANAGED_ELSEWHERE_MESSAGE = 'Your password is managed elsewhere (password sign-in is turned off for your account), so it cannot be changed here.';
const MAY_HAVE_CHANGED_MESSAGE =
  'Something went wrong on the server. Your password may have been changed. If you are signed out, sign in with the new password; otherwise reload this page and try again.';
const GENERIC_MESSAGE = 'Your password could not be changed. Try again.';
const SUCCESS_MESSAGE =
  'Your password was changed. Your other sessions were signed out; this session stays signed in.';

interface FieldErrors {
  current: string | null;
  next: string | null;
  confirm: string | null;
}

const NO_FIELD_ERRORS: FieldErrors = { current: null, next: null, confirm: null };

/** The wait of a 429 in words, for the message. */
function rateLimitMessage(err: unknown): string {
  const seconds = retryAfterSeconds(err);
  return seconds === null
    ? 'Too many attempts. Try again later.'
    : `Too many attempts. Try again in ${describeWait(seconds)}.`;
}

/** One line of the profile: a label and its value. */
const Detail = ({ label, value }: { label: string; value: string }) => (
  <Box sx={{ display: 'contents' }}>
    <Typography component="dt" variant="body2" color="text.secondary">
      {label}
    </Typography>
    <Typography component="dd" variant="body1" sx={{ m: 0 }}>
      {value}
    </Typography>
  </Box>
);

/**
 * The signed-in user's account: name, username, email, role and sign-in
 * method (from the session, no request), a link to the API keys, and the
 * password change for a local user (`PUT /api/v1/auth/me/password`). The
 * three passwords live in this component's state only: they are never logged,
 * stored or put in the address, and they are cleared after a success. A user
 * of an identity provider sees a notice instead of the form.
 */
const ProfilePage = () => {
  const { user } = useAuth();
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [confirmation, setConfirmation] = useState('');
  const [fieldErrors, setFieldErrors] = useState<FieldErrors>(NO_FIELD_ERRORS);
  const [formError, setFormError] = useState<string | null>(null);
  const [succeeded, setSucceeded] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const submittingRef = useRef(false);
  const currentRef = useRef<HTMLInputElement>(null);
  const nextRef = useRef<HTMLInputElement>(null);
  const confirmRef = useRef<HTMLInputElement>(null);
  const noticeRef = useRef<HTMLDivElement>(null);

  useUnsavedChanges(current !== '' || next !== '' || confirmation !== '');

  useEffect(() => {
    if (succeeded) noticeRef.current?.focus();
  }, [succeeded]);

  if (!user) {
    return (
      <Box sx={{ mt: 2 }}>
        <Typography variant="h4" component="h1" gutterBottom>
          Account
        </Typography>
        <Alert severity="error">Account details are not available. Sign in again.</Alert>
      </Box>
    );
  }

  const isLocal = !user.auth_provider || user.auth_provider === 'local';
  const signInMethod = isLocal ? 'Password' : user.auth_provider ?? 'Password';

  const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (submittingRef.current) return;
    setFormError(null);
    setSucceeded(false);

    const errors: FieldErrors = {
      current: current === '' ? CURRENT_REQUIRED_MESSAGE : null,
      next: passwordProblem(next) ?? (next === current && current !== '' ? PASSWORD_UNCHANGED_MESSAGE : null),
      confirm: next !== confirmation ? PASSWORD_MISMATCH_MESSAGE : null,
    };
    setFieldErrors(errors);
    if (errors.current || errors.next || errors.confirm) {
      setFormError(FIELD_ERRORS_SUMMARY);
      (errors.current ? currentRef : errors.next ? nextRef : confirmRef).current?.focus();
      return;
    }

    submittingRef.current = true;
    setSubmitting(true);
    try {
      await authService.changePassword(current, next);
      setCurrent('');
      setNext('');
      setConfirmation('');
      setSucceeded(true);
    } catch (err: unknown) {
      handleFailure(err);
    } finally {
      submittingRef.current = false;
      setSubmitting(false);
    }
  };

  const handleFailure = (err: unknown) => {
    const status = isAxiosError(err) ? err.response?.status : undefined;
    if (status === 401 && errorTextOf(err) === CURRENT_PASSWORD_INCORRECT_MESSAGE) {
      // Keep the new password and its confirmation: the user retypes the current one only.
      setCurrent('');
      setFieldErrors({ ...NO_FIELD_ERRORS, current: CURRENT_PASSWORD_INCORRECT_MESSAGE });
      setFormError(FIELD_ERRORS_SUMMARY);
      currentRef.current?.focus();
      return;
    }
    if (status === 401) setFormError(SESSION_EXPIRED_MESSAGE);
    else if (status === 400) setFormError(getApiErrorMessage(err));
    else if (status === 403) setFormError(MANAGED_ELSEWHERE_MESSAGE);
    else if (status === 429) setFormError(rateLimitMessage(err));
    else if (status !== undefined && status >= 500) setFormError(MAY_HAVE_CHANGED_MESSAGE);
    else if (status === undefined) setFormError(getApiErrorMessage(err));
    else setFormError(GENERIC_MESSAGE);
  };

  return (
    <Box>
      <Typography variant="h4" component="h1" gutterBottom>
        Account
      </Typography>

      <Paper sx={{ p: 3, mb: 3 }}>
        <Typography variant="h6" component="h2" gutterBottom>
          Profile
        </Typography>
        <Box component="dl" sx={{ display: 'grid', gridTemplateColumns: 'max-content 1fr', columnGap: 3, rowGap: 1, m: 0 }}>
          <Detail label="Name" value={user.display_name} />
          <Detail label="Username" value={user.username} />
          {user.email && <Detail label="Email" value={user.email} />}
          <Detail label="Role" value={user.role ?? 'none'} />
          <Detail label="Sign-in method" value={signInMethod} />
        </Box>
        <Box sx={{ mt: 2 }}>
          <Link component={RouterLink} to="/account/api-keys">
            Manage your API keys
          </Link>
        </Box>
      </Paper>

      <Paper sx={{ p: 3, maxWidth: 480 }}>
        <Typography variant="h6" component="h2" gutterBottom>
          Change password
        </Typography>
        {!isLocal ? (
          <Alert severity="info">
            Your password is managed by your identity provider ({signInMethod}). Change it there.
          </Alert>
        ) : (
          <>
            {succeeded && (
              <Alert severity="success" role="status" tabIndex={-1} ref={noticeRef} sx={{ mb: 2 }}>
                {SUCCESS_MESSAGE}
              </Alert>
            )}
            {formError && (
              <Alert severity="error" role="alert" sx={{ mb: 2 }}>
                {formError}
              </Alert>
            )}
            <Box component="form" onSubmit={handleSubmit} noValidate>
              <TextField
                inputRef={currentRef}
                label="Current password"
                name="current-password"
                type="password"
                autoComplete="current-password"
                value={current}
                onChange={(e) => setCurrent(e.target.value)}
                error={fieldErrors.current !== null}
                helperText={fieldErrors.current ?? undefined}
                fullWidth
                required
                sx={{ mb: 2 }}
              />
              <TextField
                inputRef={nextRef}
                label="New password"
                name="new-password"
                type="password"
                autoComplete="new-password"
                value={next}
                onChange={(e) => setNext(e.target.value)}
                error={fieldErrors.next !== null}
                helperText={fieldErrors.next ?? RULES_HELP}
                fullWidth
                required
                sx={{ mb: 2 }}
              />
              <TextField
                inputRef={confirmRef}
                label="Confirm new password"
                name="confirm-password"
                type="password"
                autoComplete="new-password"
                value={confirmation}
                onChange={(e) => setConfirmation(e.target.value)}
                error={fieldErrors.confirm !== null}
                helperText={fieldErrors.confirm ?? undefined}
                fullWidth
                required
                sx={{ mb: 3 }}
              />
              <Button type="submit" variant="contained" disabled={submitting}>
                {submitting ? 'Saving…' : 'Change password'}
              </Button>
            </Box>
          </>
        )}
      </Paper>
    </Box>
  );
};

export default ProfilePage;
