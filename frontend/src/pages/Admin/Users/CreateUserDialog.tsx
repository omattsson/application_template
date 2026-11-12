import { useEffect, useId, useRef, useState } from 'react';
import type { FormEvent, RefObject } from 'react';
import {
  Alert,
  Box,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  MenuItem,
  TextField,
} from '@mui/material';
import { userService } from '../../../api/client';
import { getApiErrorMessage } from '../../../utils/apiError';
import { ROLES } from '../../../utils/jwt';
import type { Role } from '../../../utils/jwt';
import { validateCreateUser } from './validation';
import type { CreateUserErrors, CreateUserForm } from './validation';

interface CreateUserDialogProps {
  open: boolean;
  onClose: () => void;
  /** Called with the username after the user was created (the parent closes the dialog). */
  onCreated: (username: string) => void;
}

const EMPTY_FORM: CreateUserForm = { username: '', email: '', displayName: '', password: '', role: 'user' };

interface FieldsProps {
  titleId: string;
  /** True from the submit until the dialog is reopened: no second request, and the dialog does not close meanwhile. */
  inFlight: RefObject<boolean>;
  onClose: () => void;
  onCreated: (username: string) => void;
}

/**
 * The form. It lives inside the Dialog, which unmounts it after the exit
 * transition: reopening starts empty, so a typed password never survives a close.
 */
const CreateUserFields = ({ titleId, inFlight, onClose, onCreated }: FieldsProps) => {
  const [form, setForm] = useState<CreateUserForm>(EMPTY_FORM);
  const [errors, setErrors] = useState<CreateUserErrors>({});
  const [serverError, setServerError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const usernameRef = useRef<HTMLInputElement>(null);
  const emailRef = useRef<HTMLInputElement>(null);
  const displayNameRef = useRef<HTMLInputElement>(null);
  const passwordRef = useRef<HTMLInputElement>(null);

  const setField = <K extends keyof CreateUserForm>(key: K, value: CreateUserForm[K]) =>
    setForm((prev) => ({ ...prev, [key]: value }));

  const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (inFlight.current) return;
    setServerError(null);
    const found = validateCreateUser(form);
    setErrors(found);
    if (Object.keys(found).length > 0) {
      // A screen reader announces nothing for a message that appears elsewhere: move to the first invalid field.
      const order = [
        ['username', usernameRef],
        ['email', emailRef],
        ['displayName', displayNameRef],
        ['password', passwordRef],
      ] as const;
      order.find(([field]) => found[field])?.[1].current?.focus();
      return;
    }
    inFlight.current = true;
    setSubmitting(true);
    try {
      await userService.create({
        username: form.username,
        email: form.email,
        display_name: form.displayName,
        password: form.password,
        role: form.role,
      });
      onCreated(form.username);
    } catch (err: unknown) {
      setServerError(getApiErrorMessage(err));
      setSubmitting(false);
      inFlight.current = false;
    }
  };

  return (
    <Box component="form" onSubmit={(e: FormEvent<HTMLFormElement>) => { void handleSubmit(e); }} noValidate>
      <DialogTitle id={titleId}>Create user</DialogTitle>
      <DialogContent>
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2, pt: 1 }}>
          {serverError && <Alert severity="error">{serverError}</Alert>}
          <TextField
            label="Username"
            name="username"
            value={form.username}
            onChange={(e) => setField('username', e.target.value)}
            error={Boolean(errors.username)}
            helperText={errors.username}
            autoComplete="off"
            inputRef={usernameRef}
            required
            fullWidth
          />
          <TextField
            label="Email"
            name="email"
            type="email"
            value={form.email}
            onChange={(e) => setField('email', e.target.value)}
            error={Boolean(errors.email)}
            helperText={errors.email}
            autoComplete="off"
            inputRef={emailRef}
            fullWidth
          />
          <TextField
            label="Display name"
            name="display_name"
            value={form.displayName}
            onChange={(e) => setField('displayName', e.target.value)}
            error={Boolean(errors.displayName)}
            helperText={errors.displayName ?? 'Defaults to the username'}
            autoComplete="off"
            inputRef={displayNameRef}
            fullWidth
          />
          <TextField
            label="Password"
            name="password"
            type="password"
            value={form.password}
            onChange={(e) => setField('password', e.target.value)}
            error={Boolean(errors.password)}
            helperText={errors.password ?? 'At least 8 characters'}
            autoComplete="new-password"
            inputRef={passwordRef}
            required
            fullWidth
          />
          <TextField
            select
            label="Role"
            name="role"
            value={form.role}
            onChange={(e) => setField('role', e.target.value as Role)}
            fullWidth
          >
            {ROLES.map((role) => (
              <MenuItem key={role} value={role}>
                {role}
              </MenuItem>
            ))}
          </TextField>
        </Box>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose} disabled={submitting}>
          Cancel
        </Button>
        <Button type="submit" variant="contained" disabled={submitting}>
          Create
        </Button>
      </DialogActions>
    </Box>
  );
};

/** Dialog that creates a local user (POST /api/v1/users). */
const CreateUserDialog = ({ open, onClose, onCreated }: CreateUserDialogProps) => {
  const titleId = useId();
  const inFlight = useRef(false);
  useEffect(() => {
    if (open) inFlight.current = false;
  }, [open]);
  return (
    // Escape and a click on the backdrop do nothing while the request runs (Cancel is disabled too).
    <Dialog
      open={open}
      onClose={() => {
        if (!inFlight.current) onClose();
      }}
      aria-labelledby={titleId}
      fullWidth
      maxWidth="sm"
    >
      <CreateUserFields titleId={titleId} inFlight={inFlight} onClose={onClose} onCreated={onCreated} />
    </Dialog>
  );
};

export default CreateUserDialog;
