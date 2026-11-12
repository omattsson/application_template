import { useCallback, useEffect, useRef, useState } from 'react';
import {
  Typography,
  Box,
  Alert,
  IconButton,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Paper,
  Snackbar,
} from '@mui/material';
import DeleteIcon from '@mui/icons-material/Delete';
import { itemService } from '../../api/client';
import type { Item } from '../../api/client';
import { useWebSocketContext } from '../../context/WebSocketContext';
import type { WebSocketMessage } from '../../hooks/useWebSocket';
import LoadingState from '../../components/LoadingState';
import EmptyState from '../../components/EmptyState';
import ConfirmDialog from '../../components/ConfirmDialog';
import { getApiErrorMessage } from '../../utils/apiError';

type ToastSeverity = 'success' | 'info' | 'warning' | 'error';

interface Toast {
  open: boolean;
  message: string;
  severity: ToastSeverity;
}

const Items = () => {
  const [items, setItems] = useState<Item[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [toast, setToast] = useState<Toast>({ open: false, message: '', severity: 'success' });
  const [pendingDelete, setPendingDelete] = useState<Item | null>(null);
  const { subscribe } = useWebSocketContext();
  const fetchCounterRef = useRef(0);
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const fetchItems = useCallback(async (silent = false) => {
    fetchCounterRef.current += 1;
    const myCounter = fetchCounterRef.current;
    try {
      const data = await itemService.list();
      if (myCounter === fetchCounterRef.current) {
        setItems(data);
        setError(null);
      }
    } catch {
      if (!silent && myCounter === fetchCounterRef.current) {
        setError('Failed to load items');
      }
    }
  }, []);

  useEffect(() => {
    const load = async () => {
      await fetchItems();
      setLoading(false);
    };
    void load();
  }, [fetchItems]);

  useEffect(() => {
    const unsubCreated = subscribe('item.created', (msg: WebSocketMessage) => {
      const item = msg.payload as Item;
      setToast({ open: true, message: `Item '${item.name}' created`, severity: 'success' });
      if (debounceRef.current) clearTimeout(debounceRef.current);
      debounceRef.current = setTimeout(() => { void fetchItems(true); }, 300);
    });

    const unsubUpdated = subscribe('item.updated', (msg: WebSocketMessage) => {
      const item = msg.payload as Item;
      setToast({ open: true, message: `Item '${item.name}' updated`, severity: 'info' });
      if (debounceRef.current) clearTimeout(debounceRef.current);
      debounceRef.current = setTimeout(() => { void fetchItems(true); }, 300);
    });

    const unsubDeleted = subscribe('item.deleted', () => {
      setToast({ open: true, message: 'Item deleted', severity: 'warning' });
      if (debounceRef.current) clearTimeout(debounceRef.current);
      debounceRef.current = setTimeout(() => { void fetchItems(true); }, 300);
    });

    return () => {
      unsubCreated();
      unsubUpdated();
      unsubDeleted();
      if (debounceRef.current) clearTimeout(debounceRef.current);
    };
  }, [subscribe, fetchItems]);

  const handleToastClose = () => {
    setToast((prev) => ({ ...prev, open: false }));
  };

  const handleDeleteConfirm = async () => {
    if (!pendingDelete) return;
    const target = pendingDelete;
    setPendingDelete(null);
    try {
      await itemService.delete(target.id);
      setToast({ open: true, message: 'Item deleted', severity: 'warning' });
      await fetchItems(true);
    } catch (err) {
      setToast({ open: true, message: getApiErrorMessage(err), severity: 'error' });
    }
  };

  if (loading) {
    return <LoadingState />;
  }

  if (error) {
    return <Alert severity="error">{error}</Alert>;
  }

  return (
    <Box>
      <Typography variant="h4" component="h1" gutterBottom>
        Items
      </Typography>

      {items.length === 0 ? (
        <EmptyState title="No items found" description="There are no items to show yet." />
      ) : (
        <TableContainer component={Paper}>
          <Table>
            <TableHead>
              <TableRow>
                <TableCell>ID</TableCell>
                <TableCell>Name</TableCell>
                <TableCell>Price</TableCell>
                <TableCell>Description</TableCell>
                <TableCell>Created At</TableCell>
                <TableCell>Updated At</TableCell>
                <TableCell>Actions</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {items.map((item) => (
                <TableRow key={item.id}>
                  <TableCell>{item.id}</TableCell>
                  <TableCell>{item.name}</TableCell>
                  <TableCell>${item.price.toFixed(2)}</TableCell>
                  <TableCell>{item.description ?? '—'}</TableCell>
                  <TableCell>{new Date(item.created_at).toLocaleString()}</TableCell>
                  <TableCell>{new Date(item.updated_at).toLocaleString()}</TableCell>
                  <TableCell>
                    <IconButton
                      aria-label={`Delete ${item.name}`}
                      color="error"
                      size="small"
                      onClick={() => setPendingDelete(item)}
                    >
                      <DeleteIcon fontSize="small" />
                    </IconButton>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableContainer>
      )}

      <ConfirmDialog
        open={pendingDelete !== null}
        title="Delete item"
        message={`Delete "${pendingDelete?.name ?? ''}"? This cannot be undone.`}
        confirmLabel="Delete"
        onConfirm={() => { void handleDeleteConfirm(); }}
        onCancel={() => setPendingDelete(null)}
      />

      <Snackbar
        open={toast.open}
        autoHideDuration={4000}
        onClose={handleToastClose}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'right' }}
      >
        <Alert onClose={handleToastClose} severity={toast.severity} sx={{ width: '100%' }}>
          {toast.message}
        </Alert>
      </Snackbar>
    </Box>
  );
};

export default Items;
