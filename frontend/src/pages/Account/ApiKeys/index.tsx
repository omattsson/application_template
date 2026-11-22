import { useCallback, useEffect, useRef, useState } from 'react';
import {
  Alert,
  Box,
  Button,
  Chip,
  Paper,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TablePagination,
  TableRow,
  Typography,
} from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import DeleteIcon from '@mui/icons-material/Delete';
import VpnKeyIcon from '@mui/icons-material/VpnKey';
import { isAxiosError } from 'axios';
import { apiKeyService } from '../../../api/client';
import type { ApiKey } from '../../../api/client';
import ConfirmDialog from '../../../components/ConfirmDialog';
import { useNotification } from '../../../context/NotificationContext';
import EmptyState from '../../../components/EmptyState';
import LoadingState from '../../../components/LoadingState';
import { getApiErrorMessage } from '../../../utils/apiError';
import CreateApiKeyDialog from './CreateApiKeyDialog';

/** The backend default and the largest page it serves (listPageSizeDefault, listPageSizeMax). */
const DEFAULT_PAGE_SIZE = 25;
const PAGE_SIZE_OPTIONS = [10, 25, 50, 100];

/** Date and time as the user's locale shows them. */
function formatDate(value: string): string {
  return new Date(value).toLocaleString();
}

/** Whether a key has expired by the clock of this browser (the server still decides). */
function isExpired(expiresAt: string | null | undefined, now: number): boolean {
  if (!expiresAt) return false;
  return new Date(expiresAt).getTime() <= now;
}

/**
 * The signed-in user's API keys (GET /api/v1/api-keys), newest first, with
 * creation and revocation. The raw key of a new key is shown once by the
 * create dialog and is never kept by this page.
 */
const ApiKeysPage = () => {
  const [keys, setKeys] = useState<ApiKey[]>([]);
  const [total, setTotal] = useState(0);
  const { notify } = useNotification();
  const [pageIndex, setPageIndex] = useState(0);
  const [pageSize, setPageSize] = useState(DEFAULT_PAGE_SIZE);
  const [loading, setLoading] = useState(true);
  const [fetching, setFetching] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [createOpen, setCreateOpen] = useState(false);
  // The revoke target is kept after the dialog closed: its exit transition still shows the name.
  const [revokeTarget, setRevokeTarget] = useState<ApiKey | null>(null);
  const [revokeOpen, setRevokeOpen] = useState(false);

  const fetchCounterRef = useRef(0);
  const busyRef = useRef(false);
  // The query of the latest commit, for a reload after a change.
  const queryRef = useRef({ pageIndex, pageSize });
  useEffect(() => {
    queryRef.current = { pageIndex, pageSize };
  }, [pageIndex, pageSize]);

  const fetchKeys = useCallback(async (index: number, size: number): Promise<void> => {
    fetchCounterRef.current += 1;
    const mine = fetchCounterRef.current;
    setFetching(true);
    try {
      const result = await apiKeyService.list(index + 1, size);
      if (mine !== fetchCounterRef.current) return;
      if (result.data.length === 0 && index > 0) {
        // The page is past the end (the last key of it was revoked): go to the last page.
        // That sets the page, which starts the next fetch (and `fetching` stays on until it ends).
        const last = Math.max(0, Math.ceil(result.total / size) - 1);
        if (last !== index) {
          setPageIndex(last);
          return;
        }
      }
      setKeys(result.data);
      setTotal(result.total);
      setLoadError(null);
      setLoading(false);
      setFetching(false);
    } catch (err: unknown) {
      if (mine !== fetchCounterRef.current) return;
      setLoadError(getApiErrorMessage(err));
      setLoading(false);
      setFetching(false);
    }
  }, []);

  useEffect(() => {
    void fetchKeys(pageIndex, pageSize);
  }, [fetchKeys, pageIndex, pageSize]);

  /** Reload the current page. Used after every change, whether it worked or not. */
  const reload = () => fetchKeys(queryRef.current.pageIndex, queryRef.current.pageSize);

  const showNotice = (message: string) => notify(message, 'success');

  const runRevoke = async (target: ApiKey): Promise<void> => {
    if (busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    setActionError(null);
    try {
      await apiKeyService.delete(target.id);
      showNotice(`Key ${target.name} revoked`);
    } catch (err: unknown) {
      // 404: the key is already gone (revoked elsewhere). The list is reloaded below either way.
      if (isAxiosError(err) && err.response?.status === 404) {
        setActionError('This key was already revoked. The list has been reloaded.');
      } else {
        setActionError(getApiErrorMessage(err));
      }
    }
    await reload();
    busyRef.current = false;
    setBusy(false);
  };

  const handleRevokeConfirm = () => {
    const target = revokeTarget;
    if (!target || busyRef.current) return;
    setRevokeOpen(false);
    void runRevoke(target);
  };

  const handleCreated = () => {
    void reload();
  };

  const handleRetry = () => {
    setLoading(true);
    setLoadError(null);
    void reload();
  };

  const renderContent = () => {
    if (loading) return <LoadingState label="Loading API keys" />;
    if (loadError) {
      return (
        <Alert
          severity="error"
          action={
            <Button color="inherit" size="small" onClick={handleRetry}>
              Retry
            </Button>
          }
        >
          {loadError}
        </Alert>
      );
    }
    if (keys.length === 0 && total === 0) {
      return (
        <EmptyState
          icon={<VpnKeyIcon />}
          title="No API keys yet"
          description="A key lets a script or a tool call the API as you. Create one to start."
        />
      );
    }
    const now = Date.now();
    return (
      <Paper>
        <TableContainer aria-busy={fetching}>
          <Table aria-label="API keys">
            <TableHead>
              <TableRow>
                <TableCell>Name</TableCell>
                <TableCell>Prefix</TableCell>
                <TableCell>Created</TableCell>
                <TableCell>Last used</TableCell>
                <TableCell>Expires</TableCell>
                <TableCell>Actions</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {keys.map((key) => {
                const expired = isExpired(key.expires_at, now);
                return (
                  <TableRow key={key.id} hover>
                    <TableCell component="th" scope="row">
                      {key.name}
                    </TableCell>
                    <TableCell sx={{ fontFamily: 'monospace' }}>{key.prefix}</TableCell>
                    <TableCell>{formatDate(key.created_at)}</TableCell>
                    <TableCell>{key.last_used_at ? formatDate(key.last_used_at) : 'Never'}</TableCell>
                    <TableCell>
                      {key.expires_at ? (
                        <Box sx={{ display: 'flex', gap: 1, alignItems: 'center', flexWrap: 'wrap' }}>
                          <span>{formatDate(key.expires_at)}</span>
                          {expired && <Chip size="small" label="Expired" variant="outlined" />}
                        </Box>
                      ) : (
                        'No expiry'
                      )}
                    </TableCell>
                    <TableCell>
                      <Button
                        size="small"
                        color="error"
                        variant="outlined"
                        startIcon={<DeleteIcon fontSize="small" />}
                        disabled={busy}
                        onClick={() => {
                          setRevokeTarget(key);
                          setRevokeOpen(true);
                        }}
                        aria-label={`Revoke ${key.name}`}
                      >
                        Revoke
                      </Button>
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        </TableContainer>
        <TablePagination
          component="div"
          count={total}
          page={pageIndex}
          rowsPerPage={pageSize}
          rowsPerPageOptions={PAGE_SIZE_OPTIONS}
          onPageChange={(_event, next) => setPageIndex(next)}
          onRowsPerPageChange={(e) => {
            setPageSize(Number(e.target.value));
            setPageIndex(0);
          }}
        />
      </Paper>
    );
  };

  return (
    <Box>
      <Box sx={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', mb: 2, gap: 2, flexWrap: 'wrap' }}>
        <Typography variant="h4" component="h1">
          API keys
        </Typography>
        <Button variant="contained" startIcon={<AddIcon />} onClick={() => setCreateOpen(true)}>
          Create API key
        </Button>
      </Box>
      <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
        An API key calls the API as you, with your role. Each key has an expiry. Revoke a key that you no longer use.
      </Typography>

      {actionError && (
        <Alert severity="error" onClose={() => setActionError(null)} sx={{ mb: 2 }}>
          {actionError}
        </Alert>
      )}

      {renderContent()}

      {/* Mounted only while open: closing it unmounts the dialog and the raw key in it. */}
      {createOpen && <CreateApiKeyDialog onClose={() => setCreateOpen(false)} onCreated={handleCreated} />}

      <ConfirmDialog
        open={revokeOpen}
        title="Revoke API key"
        message={`Revoke the key "${revokeTarget?.name ?? ''}"? Anything that uses it stops working at once. This cannot be undone.`}
        confirmLabel="Revoke"
        onConfirm={handleRevokeConfirm}
        onCancel={() => setRevokeOpen(false)}
      />
    </Box>
  );
};

export default ApiKeysPage;
