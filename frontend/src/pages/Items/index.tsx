import { useCallback, useEffect, useState } from 'react';
import {
  Typography,
  Box,
  CircularProgress,
  Alert,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Paper,
  Snackbar,
} from '@mui/material';
import { itemService } from '../../api/client';
import type { Item } from '../../api/client';
import { useWebSocketContext } from '../../context/WebSocketContext';
import type { WebSocketMessage } from '../../hooks/useWebSocket';

type ToastSeverity = 'success' | 'info' | 'warning';

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
  const { subscribe } = useWebSocketContext();

  const fetchItems = useCallback(async (silent = false) => {
    try {
      const data = await itemService.list();
      setItems(data);
      setError(null);
    } catch {
      if (!silent) {
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
      void fetchItems(true);
    });

    const unsubUpdated = subscribe('item.updated', (msg: WebSocketMessage) => {
      const item = msg.payload as Item;
      setToast({ open: true, message: `Item '${item.name}' updated`, severity: 'info' });
      void fetchItems(true);
    });

    const unsubDeleted = subscribe('item.deleted', () => {
      setToast({ open: true, message: 'Item deleted', severity: 'warning' });
      void fetchItems(true);
    });

    return () => {
      unsubCreated();
      unsubUpdated();
      unsubDeleted();
    };
  }, [subscribe, fetchItems]);

  const handleToastClose = () => {
    setToast((prev) => ({ ...prev, open: false }));
  };

  if (loading) {
    return (
      <Box display="flex" justifyContent="center" alignItems="center" minHeight="200px">
        <CircularProgress />
      </Box>
    );
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
        <Alert severity="info">No items found.</Alert>
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
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableContainer>
      )}

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
