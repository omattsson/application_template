import { Fragment, useCallback, useEffect, useId, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import {
  Alert,
  Box,
  Button,
  MenuItem,
  Paper,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TablePagination,
  TableRow,
  TextField,
  Typography,
} from '@mui/material';
import ExpandLessIcon from '@mui/icons-material/ExpandLess';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import HistoryIcon from '@mui/icons-material/History';
import { auditLogService } from '../../../api/client';
import type { AuditLog } from '../../../api/client';
import EmptyState from '../../../components/EmptyState';
import LoadingState from '../../../components/LoadingState';
import { getApiErrorMessage } from '../../../utils/apiError';
import { AUDIT_ACTIONS, AUDIT_ENTITY_TYPES } from './options';
import {
  EMPTY_FILTERS,
  MAX_AUDIT_OFFSET,
  buildAuditQuery,
  formatDetails,
  hasFilters,
  pagerCount,
  rangeError,
} from './filters';
import type { AuditFilters } from './filters';

/** The backend default and the largest page it serves (listPageSizeDefault, listPageSizeMax). */
const DEFAULT_PAGE_SIZE = 25;
const PAGE_SIZE_OPTIONS = [10, 25, 50, 100];
const COLUMN_COUNT = 6;

/** Date and time as the user's locale shows them. An unreadable value is shown as it is. */
function formatTime(value: string): string {
  const time = new Date(value);
  return Number.isNaN(time.getTime()) ? value : time.toLocaleString();
}

/**
 * The audit log (GET /api/v1/audit-logs), newest first, with filters on the
 * action, the entity type, the user ID and a date range. The filters apply on
 * Apply (Enter in a field submits); Clear removes them. The details of an entry
 * are shown as text behind a toggle. Admins only: the API checks the role too.
 */
const AuditLogsPage = () => {
  // The form, as the user edits it, and the filters that the list shows.
  const [draft, setDraft] = useState<AuditFilters>(EMPTY_FILTERS);
  const [applied, setApplied] = useState<AuditFilters>(EMPTY_FILTERS);
  const [rangeMessage, setRangeMessage] = useState<string | null>(null);
  const [pageIndex, setPageIndex] = useState(0);
  const [pageSize, setPageSize] = useState(DEFAULT_PAGE_SIZE);

  const [entries, setEntries] = useState<AuditLog[]>([]);
  const [total, setTotal] = useState(0);
  // True once a list answer has arrived: the paging is known from then on.
  const [loaded, setLoaded] = useState(false);
  const [fetching, setFetching] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [reloadTick, setReloadTick] = useState(0);
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(() => new Set());
  // True when the server answered a different offset than the one asked for (it clamps a larger one).
  const [clamped, setClamped] = useState(false);

  // Each request gets a number; an answer of an older request is dropped.
  const fetchCounterRef = useRef(0);
  const startRef = useRef<HTMLInputElement>(null);
  const rangeErrorId = useId();

  const fetchPage = useCallback(async (filters: AuditFilters, index: number, size: number): Promise<void> => {
    fetchCounterRef.current += 1;
    const mine = fetchCounterRef.current;
    const requestedOffset = index * size;
    setFetching(true);
    setLoadError(null);
    try {
      const answer = await auditLogService.list(buildAuditQuery(filters, index, size));
      if (mine !== fetchCounterRef.current) return;
      // A different offset means the server clamped it: its rows repeat an earlier page, so none is shown.
      const isClamped = answer.offset !== requestedOffset;
      setClamped(isClamped);
      setEntries(isClamped ? [] : answer.data);
      setTotal(answer.total);
      setLoaded(true);
      setExpanded(new Set());
      setFetching(false);
    } catch (err: unknown) {
      if (mine !== fetchCounterRef.current) return;
      setClamped(false);
      setEntries([]);
      setLoadError(getApiErrorMessage(err));
      setFetching(false);
    }
  }, []);

  useEffect(() => {
    void fetchPage(applied, pageIndex, pageSize);
  }, [fetchPage, applied, pageIndex, pageSize, reloadTick]);

  const setField = (field: keyof AuditFilters, value: string) => {
    setDraft((prev) => ({ ...prev, [field]: value }));
    setRangeMessage(null);
  };

  const handleApply = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const message = rangeError(draft.start, draft.end);
    setRangeMessage(message);
    if (message) {
      // The fault is in the start: the focus goes there, and the message is announced (role alert).
      startRef.current?.focus();
      return;
    }
    // A new filter starts on the first page.
    setApplied(draft);
    setPageIndex(0);
  };

  const handleClear = () => {
    setDraft(EMPTY_FILTERS);
    setApplied(EMPTY_FILTERS);
    setRangeMessage(null);
    setPageIndex(0);
  };

  const toggleDetails = (id: string) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const renderList = () => {
    if (loadError) {
      return (
        <Box sx={{ p: 2 }}>
          <Alert
            severity="error"
            action={
              <Button color="inherit" size="small" onClick={() => setReloadTick((tick) => tick + 1)}>
                Retry
              </Button>
            }
          >
            {loadError}
          </Alert>
        </Box>
      );
    }
    // A clamped answer shows no rows: the note under the table says why.
    if (clamped) return null;
    if (!loaded || (fetching && entries.length === 0)) return <LoadingState label="Loading audit log" />;
    if (entries.length === 0) {
      return hasFilters(applied) ? (
        <EmptyState
          icon={<HistoryIcon />}
          title="No entries match the filters"
          description="Change the filters or clear them to see more entries."
        />
      ) : (
        <EmptyState
          icon={<HistoryIcon />}
          title="No audit entries"
          description="An entry appears here when a change is made through the API, for example to an item, a user or an API key."
        />
      );
    }
    return (
      <TableContainer aria-busy={fetching}>
        <Table aria-label="Audit log entries" size="small">
          <TableHead>
            <TableRow>
              <TableCell>Time</TableCell>
              <TableCell>User</TableCell>
              <TableCell>Action</TableCell>
              <TableCell>Entity type</TableCell>
              <TableCell>Entity ID</TableCell>
              <TableCell>Details</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {entries.map((entry) => {
              const open = expanded.has(entry.id);
              const detailsId = `audit-details-${entry.id}`;
              return (
                <Fragment key={entry.id}>
                  <TableRow hover>
                    <TableCell>
                      <Box component="time" dateTime={entry.timestamp}>
                        {formatTime(entry.timestamp)}
                      </Box>
                    </TableCell>
                    <TableCell>
                      <Typography variant="body2">{entry.username || 'Unknown user'}</Typography>
                      <Typography
                        variant="caption"
                        component="div"
                        color="text.secondary"
                        sx={{ fontFamily: 'monospace', wordBreak: 'break-all' }}
                      >
                        {entry.user_id}
                      </Typography>
                    </TableCell>
                    <TableCell>{entry.action}</TableCell>
                    <TableCell>{entry.entity_type}</TableCell>
                    <TableCell sx={{ fontFamily: 'monospace', wordBreak: 'break-all' }}>{entry.entity_id}</TableCell>
                    <TableCell>
                      <Button
                        size="small"
                        variant="text"
                        aria-expanded={open}
                        aria-controls={open ? detailsId : undefined}
                        onClick={() => toggleDetails(entry.id)}
                        endIcon={open ? <ExpandLessIcon fontSize="small" /> : <ExpandMoreIcon fontSize="small" />}
                      >
                        Details
                      </Button>
                    </TableCell>
                  </TableRow>
                  {open && (
                    <TableRow id={detailsId}>
                      <TableCell colSpan={COLUMN_COUNT}>
                        {/* Text only: the details are JSON or raw text, never HTML. */}
                        <Box
                          component="pre"
                          sx={{
                            m: 0,
                            p: 1,
                            fontFamily: 'monospace',
                            fontSize: '0.8125rem',
                            whiteSpace: 'pre-wrap',
                            wordBreak: 'break-word',
                            bgcolor: 'action.hover',
                            borderRadius: 1,
                          }}
                        >
                          {formatDetails(entry.details) || 'No details'}
                        </Box>
                      </TableCell>
                    </TableRow>
                  )}
                </Fragment>
              );
            })}
          </TableBody>
        </Table>
      </TableContainer>
    );
  };

  return (
    <Box>
      <Typography variant="h4" component="h1" sx={{ mb: 1 }}>
        Audit log
      </Typography>
      <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
        Changes made through the API, newest first. Only an admin sees this page.
      </Typography>

      <Box
        component="form"
        role="search"
        aria-label="Filter audit entries"
        noValidate
        onSubmit={handleApply}
        sx={{ display: 'flex', flexWrap: 'wrap', alignItems: 'flex-start', gap: 2, mb: 2 }}
      >
        <TextField
          select
          size="small"
          label="Action"
          value={draft.action}
          onChange={(event) => setField('action', event.target.value)}
          slotProps={{ inputLabel: { shrink: true }, select: { displayEmpty: true } }}
          sx={{ minWidth: 180 }}
        >
          <MenuItem value="">Any</MenuItem>
          {AUDIT_ACTIONS.map((action) => (
            <MenuItem key={action} value={action}>
              {action}
            </MenuItem>
          ))}
        </TextField>
        <TextField
          select
          size="small"
          label="Entity type"
          value={draft.entityType}
          onChange={(event) => setField('entityType', event.target.value)}
          slotProps={{ inputLabel: { shrink: true }, select: { displayEmpty: true } }}
          sx={{ minWidth: 160 }}
        >
          <MenuItem value="">Any</MenuItem>
          {AUDIT_ENTITY_TYPES.map((entityType) => (
            <MenuItem key={entityType} value={entityType}>
              {entityType}
            </MenuItem>
          ))}
        </TextField>
        <TextField
          size="small"
          label="User ID"
          value={draft.userId}
          onChange={(event) => setField('userId', event.target.value)}
          slotProps={{ htmlInput: { spellCheck: false } }}
          sx={{ minWidth: 260 }}
        />
        <TextField
          size="small"
          type="datetime-local"
          label="Start"
          value={draft.start}
          onChange={(event) => setField('start', event.target.value)}
          error={rangeMessage !== null}
          inputRef={startRef}
          slotProps={{
            inputLabel: { shrink: true },
            htmlInput: { 'aria-describedby': rangeMessage !== null ? rangeErrorId : undefined },
          }}
        />
        <TextField
          size="small"
          type="datetime-local"
          label="End"
          value={draft.end}
          onChange={(event) => setField('end', event.target.value)}
          error={rangeMessage !== null}
          slotProps={{
            inputLabel: { shrink: true },
            htmlInput: { 'aria-describedby': rangeMessage !== null ? rangeErrorId : undefined },
          }}
        />
        {rangeMessage !== null && (
          // One message for both fields: each field names it in aria-describedby.
          <Alert id={rangeErrorId} severity="error" sx={{ width: '100%' }}>
            {rangeMessage}
          </Alert>
        )}
        <Box sx={{ display: 'flex', gap: 1, pt: 0.5 }}>
          <Button type="submit" variant="contained">
            Apply
          </Button>
          <Button type="button" variant="outlined" onClick={handleClear}>
            Clear
          </Button>
        </Box>
      </Box>

      <Paper>
        {renderList()}
        {(loaded || loadError !== null) && (
          <TablePagination
            component="div"
            count={pagerCount(total, pageSize)}
            page={pageIndex}
            rowsPerPage={pageSize}
            rowsPerPageOptions={PAGE_SIZE_OPTIONS}
            showLastButton
            // The label shows the real total; the pager stops at the last reachable page.
            labelDisplayedRows={({ from }) => `${from}–${Math.min(total, (pageIndex + 1) * pageSize)} of ${total}`}
            onPageChange={(_event, next) => setPageIndex(next)}
            onRowsPerPageChange={(event) => {
              setPageSize(Number(event.target.value));
              setPageIndex(0);
            }}
          />
        )}
      </Paper>
      {(clamped || pagerCount(total, pageSize) < total) && (
        <Alert severity="info" sx={{ mt: 2 }}>
          {`Only the first ${MAX_AUDIT_OFFSET.toLocaleString('en-US')} entries can be paged here. Narrow the filters to see the others.`}
        </Alert>
      )}
    </Box>
  );
};

export default AuditLogsPage;
