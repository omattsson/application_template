import { useId } from 'react';
import {
  Dialog,
  DialogTitle,
  DialogContent,
  DialogContentText,
  DialogActions,
  Button,
} from '@mui/material';

interface ConfirmDialogProps {
  open: boolean;
  title: string;
  message: string;
  /** Label of the confirm button, for example "Delete". */
  confirmLabel: string;
  /** Label of the cancel button. Defaults to "Cancel". */
  cancelLabel?: string;
  onConfirm: () => void;
  onCancel: () => void;
}

/**
 * A modal that asks the user to confirm a destructive action. MUI's Dialog
 * traps focus, restores it on close and closes on Escape or a backdrop click
 * (both call `onCancel`).
 */
const ConfirmDialog = ({
  open,
  title,
  message,
  confirmLabel,
  cancelLabel = 'Cancel',
  onConfirm,
  onCancel,
}: ConfirmDialogProps) => {
  const titleId = useId();
  const messageId = useId();
  return (
    <Dialog open={open} onClose={onCancel} aria-labelledby={titleId} aria-describedby={messageId}>
      <DialogTitle id={titleId}>{title}</DialogTitle>
      <DialogContent>
        <DialogContentText id={messageId}>{message}</DialogContentText>
      </DialogContent>
      <DialogActions>
        <Button onClick={onCancel}>{cancelLabel}</Button>
        <Button onClick={onConfirm} color="error" variant="contained">
          {confirmLabel}
        </Button>
      </DialogActions>
    </Dialog>
  );
};

export default ConfirmDialog;
