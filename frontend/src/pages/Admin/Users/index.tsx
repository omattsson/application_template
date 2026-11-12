import { useCallback, useEffect, useRef, useState } from 'react';
import {
  Alert,
  Box,
  Button,
  Chip,
  MenuItem,
  Paper,
  Select,
  Snackbar,
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
import PeopleIcon from '@mui/icons-material/People';
import { userService } from '../../../api/client';
import type { User } from '../../../api/client';
import ConfirmDialog from '../../../components/ConfirmDialog';
import EmptyState from '../../../components/EmptyState';
import LoadingState from '../../../components/LoadingState';
import { getApiErrorMessage } from '../../../utils/apiError';
import { ROLES } from '../../../utils/jwt';
import type { Role } from '../../../utils/jwt';
import CreateUserDialog from './CreateUserDialog';

/** The backend default and the largest page it serves (listPageSizeDefault, listPageSizeMax). */
const DEFAULT_PAGE_SIZE = 25;
const PAGE_SIZE_OPTIONS = [10, 25, 50, 100];

/**
 * Administration of users (admin role). Lists one page at a time, creates
 * users, changes roles, disables or enables users and deletes users. The API
 * enforces the rules (admin only, never the last enabled admin); its message
 * is shown as it is for a 4xx answer.
 */
const AdminUsers = () => {
  const [users, setUsers] = useState<User[]>([]);
  const [total, setTotal] = useState(0);
  const [pageIndex, setPageIndex] = useState(0);
  const [pageSize, setPageSize] = useState(DEFAULT_PAGE_SIZE);
  const [loading, setLoading] = useState(true);
  const [fetching, setFetching] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  // The text stays while the toast fades out (no empty flash).
  const [notice, setNotice] = useState({ message: '', open: false });
  const [busy, setBusy] = useState(false);
  const [createOpen, setCreateOpen] = useState(false);
  // The delete target is kept after the dialog closed: its exit transition
  // still shows the message, which must not turn into an empty name.
  const [deleteTarget, setDeleteTarget] = useState<User | null>(null);
  const [deleteOpen, setDeleteOpen] = useState(false);

  const fetchCounterRef = useRef(0);
  const busyRef = useRef(false);
  // The query of the latest render, for a reload after a change.
  const queryRef = useRef({ pageIndex, pageSize });
  queryRef.current = { pageIndex, pageSize };

  const fetchUsers = useCallback(async (index: number, size: number): Promise<void> => {
    fetchCounterRef.current += 1;
    const mine = fetchCounterRef.current;
    setFetching(true);
    try {
      const result = await userService.list(index + 1, size);
      if (mine !== fetchCounterRef.current) return;
      if (result.data.length === 0 && index > 0) {
        // The page is past the end (the last user of it was deleted): go to the last page.
        setPageIndex(Math.max(0, Math.ceil(result.total / size) - 1));
        return;
      }
      setUsers(result.data);
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
    void fetchUsers(pageIndex, pageSize);
  }, [fetchUsers, pageIndex, pageSize]);

  /** Reload the current page. Used after every change, whether it worked or not. */
  const reload = () => fetchUsers(queryRef.current.pageIndex, queryRef.current.pageSize);

  /**
   * Run one change at a time. The list is loaded again after a failure as well
   * as after a success, so a row that changed or vanished meanwhile is not stale.
   */
  const runChange = async (change: () => Promise<string>): Promise<void> => {
    if (busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    setActionError(null);
    try {
      showNotice(await change());
    } catch (err: unknown) {
      setActionError(getApiErrorMessage(err));
    }
    try {
      await reload();
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  const showNotice = (message: string) => setNotice({ message, open: true });
  const closeNotice = () => setNotice((prev) => ({ ...prev, open: false }));

  const handleRoleChange = (user: User, role: Role) => {
    if (role === user.role) return;
    void runChange(async () => {
      const result = await userService.changeRole(user.id, role);
      if (!result.changed) return `Role of ${user.username} is already ${role}`;
      return result.idp_managed
        ? `Role of ${user.username} changed to ${role}; the identity provider may override it at the next login`
        : `Role of ${user.username} changed to ${role}`;
    });
  };

  const handleToggleDisabled = (user: User) => {
    const disabled = !user.disabled;
    void runChange(async () => {
      await userService.setDisabled(user.id, disabled);
      return `${user.username} ${disabled ? 'disabled' : 'enabled'}`;
    });
  };

  const handleDeleteConfirm = () => {
    const target = deleteTarget;
    if (!target || busyRef.current) return;
    setDeleteOpen(false);
    void runChange(async () => {
      await userService.delete(target.id);
      return `User ${target.username} deleted`;
    });
  };

  const handleCreated = (username: string) => {
    setCreateOpen(false);
    showNotice(`User ${username} created`);
    void reload();
  };

  const handleRetry = () => {
    setLoading(true);
    setLoadError(null);
    void reload();
  };

  const renderContent = () => {
    if (loading) return <LoadingState label="Loading users" />;
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
    if (users.length === 0) {
      return (
        <EmptyState
          icon={<PeopleIcon />}
          title="No users found"
          description="There are no users to show yet."
        />
      );
    }
    return (
      <Paper>
        <TableContainer aria-busy={fetching}>
          <Table aria-label="Users">
            <TableHead>
              <TableRow>
                <TableCell>Username</TableCell>
                <TableCell>Email</TableCell>
                <TableCell>Display name</TableCell>
                <TableCell>Role</TableCell>
                <TableCell>Status</TableCell>
                <TableCell>Created</TableCell>
                <TableCell>Actions</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {users.map((user) => (
                <TableRow key={user.id} hover>
                  <TableCell component="th" scope="row">
                    {user.username}
                  </TableCell>
                  <TableCell>{user.email || '—'}</TableCell>
                  <TableCell>{user.display_name}</TableCell>
                  <TableCell>
                    <Select
                      size="small"
                      value={user.role}
                      disabled={busy}
                      onChange={(e) => handleRoleChange(user, e.target.value as Role)}
                      inputProps={{ 'aria-label': `Role of ${user.username}` }}
                      sx={{ minWidth: 110 }}
                    >
                      {ROLES.map((role) => (
                        <MenuItem key={role} value={role}>
                          {role}
                        </MenuItem>
                      ))}
                    </Select>
                  </TableCell>
                  <TableCell>
                    <Chip
                      size="small"
                      label={user.disabled ? 'Disabled' : 'Active'}
                      color={user.disabled ? 'default' : 'success'}
                      variant="outlined"
                    />
                  </TableCell>
                  <TableCell>{new Date(user.created_at).toLocaleString()}</TableCell>
                  <TableCell>
                    <Box sx={{ display: 'flex', gap: 1, alignItems: 'center' }}>
                      <Button
                        size="small"
                        variant="outlined"
                        disabled={busy}
                        onClick={() => handleToggleDisabled(user)}
                        aria-label={`${user.disabled ? 'Enable' : 'Disable'} ${user.username}`}
                      >
                        {user.disabled ? 'Enable' : 'Disable'}
                      </Button>
                      <Button
                        size="small"
                        color="error"
                        variant="outlined"
                        startIcon={<DeleteIcon fontSize="small" />}
                        disabled={busy}
                        onClick={() => {
                          setDeleteTarget(user);
                          setDeleteOpen(true);
                        }}
                        aria-label={`Delete ${user.username}`}
                      >
                        Delete
                      </Button>
                    </Box>
                  </TableCell>
                </TableRow>
              ))}
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
          Users
        </Typography>
        <Button variant="contained" startIcon={<AddIcon />} onClick={() => setCreateOpen(true)}>
          Create user
        </Button>
      </Box>

      {actionError && (
        <Alert severity="error" onClose={() => setActionError(null)} sx={{ mb: 2 }}>
          {actionError}
        </Alert>
      )}

      {renderContent()}

      <CreateUserDialog open={createOpen} onClose={() => setCreateOpen(false)} onCreated={handleCreated} />

      <ConfirmDialog
        open={deleteOpen}
        title="Delete user"
        message={`Delete user "${deleteTarget?.username ?? ''}"? Their sessions end at once and this cannot be undone.`}
        confirmLabel="Delete"
        onConfirm={handleDeleteConfirm}
        onCancel={() => setDeleteOpen(false)}
      />

      <Snackbar
        open={notice.open}
        autoHideDuration={5000}
        onClose={(_event, reason) => {
          if (reason !== 'clickaway') closeNotice();
        }}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'right' }}
      >
        <Alert severity="success" role="status" onClose={closeNotice} sx={{ width: '100%' }}>
          {notice.message}
        </Alert>
      </Snackbar>
    </Box>
  );
};

export default AdminUsers;
