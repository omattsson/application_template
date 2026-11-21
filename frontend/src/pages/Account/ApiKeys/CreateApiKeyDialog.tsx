import { useEffect, useId, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Alert, Box, Button, Dialog, DialogActions, DialogContent, DialogTitle, MenuItem, TextField, Typography } from '@mui/material';
import CheckIcon from '@mui/icons-material/Check';
import ContentCopyIcon from '@mui/icons-material/ContentCopy';
import { apiKeyService } from '../../../api/client';
import type { CreatedApiKey } from '../../../api/client';
import { getApiErrorMessage } from '../../../utils/apiError';
import { DEFAULT_EXPIRY_DAYS, EXPIRY_OPTIONS, KEY_NAME_MAX, validateKeyName } from './validation';

interface CreateApiKeyDialogProps {
  /** Closes the dialog. The page unmounts this component, so the raw key goes with it. */
  onClose: () => void;
  /** Called after the key was created. The dialog stays open to show the key once. */
  onCreated: () => void;
}

type CopyState = 'idle' | 'copied' | 'failed';

/**
 * Creates an API key. The form comes first; after a successful create the
 * raw key is shown once, with a copy button. The raw key lives in this
 * component's state only (never in the URL, storage or a log). The page
 * mounts this dialog only while it is open, so closing it drops the key.
 */
const CreateApiKeyDialog = ({ onClose, onCreated }: CreateApiKeyDialogProps) => {
  const titleId = useId();
  const [name, setName] = useState('');
  const [expiryDays, setExpiryDays] = useState(DEFAULT_EXPIRY_DAYS);
  const [nameError, setNameError] = useState<string | undefined>(undefined);
  const [serverError, setServerError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  // Set once the key exists: the dialog then shows the secret and only Done closes it.
  const [created, setCreated] = useState<CreatedApiKey | null>(null);
  const [copyState, setCopyState] = useState<CopyState>('idle');
  const inFlight = useRef(false);
  const nameRef = useRef<HTMLInputElement>(null);
  const keyRef = useRef<HTMLInputElement>(null);

  // Select the secret at once, so that one keystroke copies it.
  useEffect(() => {
    if (created) {
      keyRef.current?.focus();
      keyRef.current?.select();
    }
  }, [created]);

  // Escape and a click on the backdrop do nothing while the request runs, and
  // after the key is shown: the user closes that view with Done.
  const handleDialogClose = () => {
    if (inFlight.current || created) return;
    onClose();
  };

  const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (inFlight.current) return;
    setServerError(null);
    const found = validateKeyName(name);
    setNameError(found);
    if (found) {
      // A screen reader announces nothing for a message that appears elsewhere: move to the field.
      nameRef.current?.focus();
      return;
    }
    inFlight.current = true;
    setSubmitting(true);
    try {
      const result = await apiKeyService.create({ name: name.trim(), expires_in_days: expiryDays });
      setCreated(result);
      onCreated();
    } catch (err: unknown) {
      setServerError(getApiErrorMessage(err));
    } finally {
      inFlight.current = false;
      setSubmitting(false);
    }
  };

  const handleCopy = async () => {
    if (!created) return;
    try {
      if (!navigator.clipboard) throw new Error('The clipboard is not available');
      await navigator.clipboard.writeText(created.raw_key);
      setCopyState('copied');
    } catch {
      // Not logged: the clipboard error has no use here and the key must stay out of the console.
      setCopyState('failed');
    }
  };

  const copyMessage =
    copyState === 'copied'
      ? 'Copied to the clipboard.'
      : copyState === 'failed'
        ? 'Copy failed: select the key and copy it by hand.'
        : '';

  return (
    <Dialog open onClose={handleDialogClose} aria-labelledby={titleId} fullWidth maxWidth="sm">
      {created ? (
        <>
          <DialogTitle id={titleId}>API key created</DialogTitle>
          <DialogContent>
            <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2, pt: 1 }}>
              <Alert severity="warning">
                Copy this key now. It is shown only once and cannot be shown again. If you lose it, revoke it and create a new
                key.
              </Alert>
              <Typography variant="body2" color="text.secondary">
                {`Name: ${created.name}. Expires: ${created.expires_at ? new Date(created.expires_at).toLocaleString() : 'never'}.`}
              </Typography>
              <Box sx={{ display: 'flex', gap: 1, alignItems: 'flex-start', flexWrap: 'wrap' }}>
                <TextField
                  label="Your API key"
                  value={created.raw_key}
                  inputRef={keyRef}
                  fullWidth
                  sx={{ flex: '1 1 320px' }}
                  slotProps={{
                    input: { readOnly: true, sx: { fontFamily: 'monospace' } },
                    htmlInput: { spellCheck: false },
                  }}
                />
                <Button
                  variant="outlined"
                  onClick={() => {
                    void handleCopy();
                  }}
                  startIcon={copyState === 'copied' ? <CheckIcon /> : <ContentCopyIcon />}
                  sx={{ mt: 1 }}
                >
                  {copyState === 'copied' ? 'Copied' : 'Copy key'}
                </Button>
              </Box>
              <Typography variant="body2" role="status" aria-live="polite">
                {copyMessage}
              </Typography>
            </Box>
          </DialogContent>
          <DialogActions>
            <Button variant="contained" onClick={onClose}>
              Done
            </Button>
          </DialogActions>
        </>
      ) : (
        <Box
          component="form"
          noValidate
          onSubmit={(e: FormEvent<HTMLFormElement>) => {
            void handleSubmit(e);
          }}
        >
          <DialogTitle id={titleId}>Create API key</DialogTitle>
          <DialogContent>
            <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2, pt: 1 }}>
              {serverError && <Alert severity="error">{serverError}</Alert>}
              <TextField
                label="Name"
                name="name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                error={Boolean(nameError)}
                helperText={nameError ?? `Up to ${KEY_NAME_MAX} characters: letters, digits, spaces and . _ - : @ / ( ) +`}
                autoComplete="off"
                inputRef={nameRef}
                required
                fullWidth
              />
              <TextField
                select
                label="Expires in"
                name="expires_in_days"
                value={expiryDays}
                onChange={(e) => setExpiryDays(Number(e.target.value))}
                helperText="The key stops working after this time."
                fullWidth
              >
                {EXPIRY_OPTIONS.map((days) => (
                  <MenuItem key={days} value={days}>
                    {`${days} days`}
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
      )}
    </Dialog>
  );
};

export default CreateApiKeyDialog;
