import { useCallback, useEffect, useRef, useState } from 'react';
import {
  Alert,
  Box,
  Button,
  Chip,
  MenuItem,
  Paper,
  Select,
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
import { useNavigate } from 'react-router-dom';
import { endSession } from '../../../api/session';
import { userService } from '../../../api/client';
import { useAuth } from '../../../context/AuthContext';
import { useNotification } from '../../../context/NotificationContext';
import type { User } from '../../../api/client';
import ConfirmDialog from '../../../components/ConfirmDialog';
import EmptyState from '../../../components/EmptyState';
import LoadingState from '../../../components/LoadingState';
import { getApiErrorMessage } from '../../../utils/apiError';
import { OWN_ACCOUNT_CHANGED, rememberLoginNotice } from '../../../utils/loginNotice';
import { ROLES } from '../../../utils/jwt';
import type { Role } from '../../../utils/jwt';
import CreateUserDialog from './CreateUserDialog';

/** The backend default and the largest page it serves (listPageSizeDefault, listPageSizeMax). */
const DEFAULT_PAGE_SIZE = 25;
const PAGE_SIZE_OPTIONS = [10, 25, 50, 100];

/** What a change gives back: the toast text, and whether it ended the signed-in user's own sessions. */
type ChangeOutcome = { message: string; endsOwnSession?: boolean };

/**
 * Administration of users (admin role). Lists one page at a time, creates
 * users, changes roles, disables or enables users and deletes users. The API
 * enforces the rules (admin only, never the last enabled admin); its message
 * is shown as it is for a 4xx answer.
 */
const AdminUsers = () => {
  const { authEnabled, user: currentUser } = useAuth();
  const navigate = useNavigate();
  const { notify } = useNotification();
  const [users, setUsers] = useState<User[]>([]);
  const [total, setTotal] = useState(0);
  const [pageIndex, setPageIndex] = useState(0);
  const [pageSize, setPageSize] = useState(DEFAULT_PAGE_SIZE);
  const [loading, setLoading] = useState(true);
  const [fetching, setFetching] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [createOpen, setCreateOpen] = useState(false);
  // The delete target is kept after the dialog closed: its exit transition
  // still shows the message, which must not turn into an empty name.
  const [deleteTarget, setDeleteTarget] = useState<User | null>(null);
  const [deleteOpen, setDeleteOpen] = useState(false);

  const fetchCounterRef = useRef(0);
  const busyRef = useRef(false);
  // The query of the latest commit, for a reload after a change.
  const queryRef = useRef({ pageIndex, pageSize });
  useEffect(() => {
    queryRef.current = { pageIndex, pageSize };
  }, [pageIndex, pageSize]);

  const fetchUsers = useCallback(async (index: number, size: number): Promise<void> => {
    fetchCounterRef.current += 1;
    const mine = fetchCounterRef.current;
    setFetching(true);
    try {
      const result = await userService.list(index + 1, size);
      if (mine !== fetchCounterRef.current) return;
      if (result.data.length === 0 && index > 0) {
        // The page is past the end (the last user of it was deleted): go to the last page.
        // That sets the page, which starts the next fetch (and `fetching` stays on until it ends).
        const last = Math.max(0, Math.ceil(result.total / size) - 1);
        if (last !== index) {
          setPageIndex(last);
          return;
        }
        // The total claims this page exists but it is empty: show it as it is, so the pagination stays usable.
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
    // With auth off there is no user administration (and the API would refuse).
    if (!authEnabled) return;
    void fetchUsers(pageIndex, pageSize);
  }, [authEnabled, fetchUsers, pageIndex, pageSize]);

  /** Reload the current page. Used after every change, whether it worked or not. */
  const reload = () => fetchUsers(queryRef.current.pageIndex, queryRef.current.pageSize);

  /**
   * Run one change at a time. The list is loaded again after a failure as well
   * as after a success, so a row that changed or vanished meanwhile is not stale.
   *
   * A change of the signed-in user's own role, status or account makes the API
   * end all that user's sessions: loading the list again would answer 401, the
   * refresh would be refused and the route guard would bounce to /login before
   * the toast is seen. So the session ends here and the login page tells why.
   */
  const runChange = async (change: () => Promise<ChangeOutcome>): Promise<void> => {
    if (busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    setActionError(null);
    try {
      const outcome = await change();
      if (outcome.endsOwnSession) {
        // The notice is not router state: ending the session makes the route
        // guard redirect to /login itself, which replaces that state.
        rememberLoginNotice(OWN_ACCOUNT_CHANGED);
        navigate('/login', { replace: true });
        endSession({ broadcast: true });
        busyRef.current = false;
        return;
      }
      showNotice(outcome.message);
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

  const showNotice = (message: string) => notify(message, 'success');

  const isSelf = (user: User) => currentUser !== null && currentUser.id === user.id;

  const handleRoleChange = (user: User, role: Role) => {
    if (role === user.role) return;
    void runChange(async () => {
      const result = await userService.changeRole(user.id, role);
      if (!result.changed) return { message: `Role of ${user.username} is already ${role}` };
      const idp = result.idp_managed ? '; the identity provider may override it at the next login' : '';
      return { message: `Role of ${user.username} changed to ${role}${idp}`, endsOwnSession: isSelf(user) };
    });
  };

  const handleToggleDisabled = (user: User) => {
    const disabled = !user.disabled;
    void runChange(async () => {
      const result = await userService.setDisabled(user.id, disabled);
      return {
        message: `${user.username} ${disabled ? 'disabled' : 'enabled'}`,
        endsOwnSession: isSelf(user) && disabled && result.changed,
      };
    });
  };

  const handleDeleteConfirm = () => {
    const target = deleteTarget;
    if (!target || busyRef.current) return;
    setDeleteOpen(false);
    void runChange(async () => {
      await userService.delete(target.id);
      return { message: `User ${target.username} deleted`, endsOwnSession: isSelf(target) };
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

  if (!authEnabled) {
    return (
      <Box>
        <Typography variant="h4" component="h1" sx={{ mb: 2 }}>
          Users
        </Typography>
        <EmptyState
          icon={<PeopleIcon />}
          title="User management is unavailable"
          description="User management requires authentication to be enabled on the server."
        />
      </Box>
    );
  }

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
    if (users.length === 0 && total === 0) {
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
                    {isSelf(user) && (
                      <Typography component="span" variant="body2" color="text.secondary" sx={{ ml: 1 }}>
                        (you)
                      </Typography>
                    )}
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
        message={`Delete user "${deleteTarget?.username ?? ''}"? Their sessions end at once and this cannot be undone.${
          deleteTarget !== null && isSelf(deleteTarget) ? ' This is your own account: you will lose access.' : ''
        }`}
        confirmLabel="Delete"
        onConfirm={handleDeleteConfirm}
        onCancel={() => setDeleteOpen(false)}
      />
    </Box>
  );
};

export default AdminUsers;
