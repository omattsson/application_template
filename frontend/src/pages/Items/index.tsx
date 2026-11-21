import { useCallback, useEffect, useRef, useState } from 'react';
import type { ChangeEvent, MouseEvent } from 'react';
import { useSearchParams } from 'react-router-dom';
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
  TablePagination,
  TableRow,
  TableSortLabel,
  TextField,
  Paper,
  Snackbar,
} from '@mui/material';
import DeleteIcon from '@mui/icons-material/Delete';
import { itemService } from '../../api/client';
import type { Item, ItemListResult, ItemSortKey, SortOrder } from '../../api/client';
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

/** The page, size, sort, order and filter of the list, as the URL holds them. */
interface ListQuery {
  page: number;
  size: number;
  sort: ItemSortKey;
  order: SortOrder;
  /** Trimmed name filter; empty when there is no filter. */
  q: string;
}

/** A request to load the list again. `silent` drops the error of a background refresh. */
interface RefreshRequest {
  id: number;
  silent: boolean;
}

const SIZE_OPTIONS = [10, 25, 50, 100];
const DEFAULT_SIZE = 25;
const DEFAULT_SORT: ItemSortKey = 'id';
const DEFAULT_ORDER: SortOrder = 'asc';
const SORT_KEYS: readonly ItemSortKey[] = ['id', 'name', 'price', 'created_at', 'updated_at'];
/** The URL keys that this page owns; every other key of the URL is kept. */
const QUERY_KEYS = ['page', 'size', 'sort', 'order', 'q'];
const FILTER_DEBOUNCE_MS = 300;
const REFRESH_DEBOUNCE_MS = 300;

function isSortKey(value: string | null): value is ItemSortKey {
  return SORT_KEYS.some((key) => key === value);
}

/** A page number from the URL: a whole number from 1; anything else is page 1. */
function parsePage(value: string | null): number {
  if (value === null || !/^\d+$/.test(value)) return 1;
  const page = Number(value);
  return Number.isSafeInteger(page) && page >= 1 ? page : 1;
}

/** Read the list state from the URL. An invalid value falls back to its default. */
function parseQuery(params: URLSearchParams): ListQuery {
  const size = Number(params.get('size'));
  const sortValue = params.get('sort');
  return {
    page: parsePage(params.get('page')),
    size: SIZE_OPTIONS.includes(size) ? size : DEFAULT_SIZE,
    sort: isSortKey(sortValue) ? sortValue : DEFAULT_SORT,
    order: params.get('order') === 'desc' ? 'desc' : DEFAULT_ORDER,
    q: (params.get('q') ?? '').trim(),
  };
}

/** The URL for a list state. A default is not written, so the URL stays short. */
function buildSearch(previous: URLSearchParams, query: ListQuery): URLSearchParams {
  const next = new URLSearchParams(previous);
  for (const key of QUERY_KEYS) next.delete(key);
  if (query.page !== 1) next.set('page', String(query.page));
  if (query.size !== DEFAULT_SIZE) next.set('size', String(query.size));
  if (query.sort !== DEFAULT_SORT) next.set('sort', query.sort);
  if (query.order !== DEFAULT_ORDER) next.set('order', query.order);
  if (query.q !== '') next.set('q', query.q);
  return next;
}

/** The last page of a list with `total` rows; an empty list has one page. */
function lastPageOf(total: number, size: number): number {
  return Math.max(1, Math.ceil(total / size));
}

const Items = () => {
  const [searchParams, setSearchParams] = useSearchParams();
  const { page, size, sort, order, q } = parseQuery(searchParams);

  const [result, setResult] = useState<ItemListResult | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [refresh, setRefresh] = useState<RefreshRequest>({ id: 0, silent: false });
  const [toast, setToast] = useState<Toast>({ open: false, message: '', severity: 'success' });
  const [pendingDelete, setPendingDelete] = useState<Item | null>(null);
  // The text in the filter field. It is committed to the URL after a pause.
  const [filterInput, setFilterInput] = useState(q);
  // The URL filter that the field last followed; a change of it (Back, a link) updates the field.
  const [followedQ, setFollowedQ] = useState(q);
  const { subscribe } = useWebSocketContext();
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // The refresh id of the last load that the fetch effect started.
  const lastRunRefreshIdRef = useRef(refresh.id);
  // The fetch effect may move the URL to the last page; it reads the setter through this ref
  // so that a new setter (it changes with every navigation) does not start a new fetch.
  const navigateRef = useRef(setSearchParams);

  useEffect(() => {
    navigateRef.current = setSearchParams;
  }, [setSearchParams]);

  if (q !== followedQ) {
    setFollowedQ(q);
    if (filterInput.trim() !== q) setFilterInput(q);
  }

  // Commit the filter to the URL after the pause. A filter change replaces the history entry.
  useEffect(() => {
    if (filterInput.trim() === q) return;
    const timer = setTimeout(() => {
      const value = filterInput.trim();
      setSearchParams((prev) => buildSearch(prev, { ...parseQuery(prev), q: value, page: 1 }), { replace: true });
    }, FILTER_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [filterInput, q, setSearchParams]);

  // Load the page. A response for an older request is dropped: the cleanup marks it cancelled.
  useEffect(() => {
    let cancelled = false;
    // Only the run that a refresh caused is silent. A run caused by a change of the URL query
    // is the user's request, so its failure is shown even when an earlier refresh was silent.
    const refreshCausedRun = refresh.id !== lastRunRefreshIdRef.current;
    lastRunRefreshIdRef.current = refresh.id;
    const silent = refreshCausedRun && refresh.silent;
    itemService
      .list({ limit: size, offset: (page - 1) * size, sort, order, name: q || undefined })
      .then((data) => {
        if (cancelled) return;
        setResult(data);
        setError(null);
        setLoading(false);
        // The rows of this page are gone (the total shrank): show the last page instead.
        const last = lastPageOf(data.total, size);
        if (page > last) {
          navigateRef.current((prev) => buildSearch(prev, { ...parseQuery(prev), page: last }), { replace: true });
        }
      })
      .catch(() => {
        if (cancelled) return;
        setLoading(false);
        if (!silent) setError('Failed to load items');
      });
    return () => {
      cancelled = true;
    };
  }, [page, size, sort, order, q, refresh]);

  const requestRefresh = useCallback(() => {
    setRefresh((prev) => ({ id: prev.id + 1, silent: true }));
  }, []);

  const scheduleRefresh = useCallback(() => {
    if (debounceRef.current) clearTimeout(debounceRef.current);
    debounceRef.current = setTimeout(() => {
      debounceRef.current = null;
      requestRefresh();
    }, REFRESH_DEBOUNCE_MS);
  }, [requestRefresh]);

  useEffect(() => {
    const unsubCreated = subscribe('item.created', (msg: WebSocketMessage) => {
      const item = msg.payload as Item;
      setToast({ open: true, message: `Item '${item.name}' created`, severity: 'success' });
      scheduleRefresh();
    });

    const unsubUpdated = subscribe('item.updated', (msg: WebSocketMessage) => {
      const item = msg.payload as Item;
      setToast({ open: true, message: `Item '${item.name}' updated`, severity: 'info' });
      scheduleRefresh();
    });

    const unsubDeleted = subscribe('item.deleted', () => {
      setToast({ open: true, message: 'Item deleted', severity: 'warning' });
      scheduleRefresh();
    });

    return () => {
      unsubCreated();
      unsubUpdated();
      unsubDeleted();
      if (debounceRef.current) clearTimeout(debounceRef.current);
    };
  }, [subscribe, scheduleRefresh]);

  const updateQuery = (change: (current: ListQuery) => ListQuery, replace = false) => {
    setSearchParams((prev) => buildSearch(prev, change(parseQuery(prev))), { replace });
  };

  const handleSort = (key: ItemSortKey) => {
    updateQuery((current) =>
      current.sort === key
        ? { ...current, order: current.order === 'asc' ? 'desc' : 'asc', page: 1 }
        : { ...current, sort: key, order: 'asc', page: 1 },
    );
  };

  const handlePageChange = (_event: MouseEvent<HTMLButtonElement> | null, newPage: number) => {
    updateQuery((current) => ({ ...current, page: newPage + 1 }));
  };

  const handleRowsPerPageChange = (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
    const newSize = Number(event.target.value);
    if (!SIZE_OPTIONS.includes(newSize)) return;
    updateQuery((current) => ({ ...current, size: newSize, page: 1 }));
  };

  const handleFilterChange = (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
    setFilterInput(event.target.value);
  };

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
      requestRefresh();
    } catch (err) {
      setToast({ open: true, message: getApiErrorMessage(err), severity: 'error' });
    }
  };

  if (loading) {
    return <LoadingState />;
  }

  // The first load failed: there is nothing to show but the error.
  if (!result) {
    return <Alert severity="error">{error ?? 'Failed to load items'}</Alert>;
  }

  const sortHeader = (key: ItemSortKey, label: string) => {
    const active = sort === key;
    return (
      <TableCell sortDirection={active ? order : false}>
        <TableSortLabel active={active} direction={active ? order : DEFAULT_ORDER} onClick={() => handleSort(key)}>
          {label}
        </TableSortLabel>
      </TableCell>
    );
  };

  const hasFilter = q !== '';

  return (
    <Box>
      <Typography variant="h4" component="h1" gutterBottom>
        Items
      </Typography>

      {error && (
        <Alert severity="error" sx={{ mb: 2 }}>
          {error}
        </Alert>
      )}

      <Box sx={{ mb: 2 }}>
        <TextField
          type="search"
          label="Filter by name"
          size="small"
          value={filterInput}
          onChange={handleFilterChange}
          autoComplete="off"
          sx={{ minWidth: 280 }}
        />
      </Box>

      {result.total === 0 ? (
        hasFilter ? (
          <EmptyState
            title="No matching items"
            description={`No item name contains "${q}". Change or clear the filter to see other items.`}
          />
        ) : (
          <EmptyState title="No items found" description="There are no items to show yet." />
        )
      ) : (
        <TableContainer component={Paper}>
          <Table>
            <TableHead>
              <TableRow>
                {sortHeader('id', 'ID')}
                {sortHeader('name', 'Name')}
                {sortHeader('price', 'Price')}
                <TableCell>Description</TableCell>
                {sortHeader('created_at', 'Created At')}
                {sortHeader('updated_at', 'Updated At')}
                <TableCell>Actions</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {result.items.map((item) => (
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
          <TablePagination
            component="div"
            count={result.total}
            page={Math.min(page, lastPageOf(result.total, size)) - 1}
            rowsPerPage={size}
            rowsPerPageOptions={SIZE_OPTIONS}
            onPageChange={handlePageChange}
            onRowsPerPageChange={handleRowsPerPageChange}
          />
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
