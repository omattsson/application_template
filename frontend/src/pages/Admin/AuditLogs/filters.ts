import type { AuditLogListParams } from '../../../api/client';

/**
 * The filter fields of the form. The text of the date fields is the value of a
 * `datetime-local` input (local time, no zone); the query converts it to UTC.
 */
export interface AuditFilters {
  action: string;
  entityType: string;
  /** The user ID as typed (the server has no username filter). */
  userId: string;
  /** Local date-time of the start, or '' for no start. */
  start: string;
  /** Local date-time of the end, or '' for no end. */
  end: string;
}

/**
 * The largest offset the server reads. A larger `offset` is clamped to this value
 * and the answer carries the clamped offset (the same rows again). It mirrors
 * `maxAuditOffset` in `backend/internal/api/handlers/audit_logs.go`: keep them in sync.
 */
export const MAX_AUDIT_OFFSET = 100_000;

/**
 * The count that the pager gets. It stops the pager so that the last page starts
 * at MAX_AUDIT_OFFSET (the last reachable page), while the real total stays in the label.
 * @param total - The real total of the filtered list
 * @param pageSize - Rows per page
 * @returns The count for TablePagination
 */
export function pagerCount(total: number, pageSize: number): number {
  const reachable = (Math.floor(MAX_AUDIT_OFFSET / pageSize) + 1) * pageSize;
  return Math.min(total, reachable);
}

/** No filter. */
export const EMPTY_FILTERS: AuditFilters = {
  action: '',
  entityType: '',
  userId: '',
  start: '',
  end: '',
};

/**
 * Convert a local date-time of a `datetime-local` input to RFC3339 in UTC, as
 * the API reads it (with a zone).
 * @param local - The input value, for example `2026-11-21T10:30`, or ''
 * @returns The UTC time, or undefined for an empty or an invalid value
 */
export function toApiTime(local: string): string | undefined {
  if (!local) return undefined;
  const time = new Date(local);
  return Number.isNaN(time.getTime()) ? undefined : time.toISOString();
}

/**
 * Check the range of the form before a request: a start after the end is refused.
 * @param start - The start input value, or ''
 * @param end - The end input value, or ''
 * @returns A message for the form, or null when the range is fine (or open)
 */
export function rangeError(start: string, end: string): string | null {
  const from = toApiTime(start);
  const to = toApiTime(end);
  if (from !== undefined && to !== undefined && Date.parse(from) > Date.parse(to)) {
    return 'The start must not be after the end.';
  }
  return null;
}

/**
 * Whether any filter is set (the empty state says so).
 * @param filters - The applied filters
 * @returns True when at least one field has a value
 */
export function hasFilters(filters: AuditFilters): boolean {
  return Object.values(filters).some((value) => value !== '');
}

/**
 * The list query of the applied filters and the current page. A blank filter is not sent.
 * @param filters - The applied filters
 * @param pageIndex - Page number, from 0
 * @param pageSize - Rows per page
 * @returns The query of the audit log list
 */
export function buildAuditQuery(filters: AuditFilters, pageIndex: number, pageSize: number): AuditLogListParams {
  const query: AuditLogListParams = { limit: pageSize, offset: pageIndex * pageSize };
  const userId = filters.userId.trim();
  if (filters.action) query.action = filters.action;
  if (filters.entityType) query.entity_type = filters.entityType;
  if (userId) query.user_id = userId;
  const start = toApiTime(filters.start);
  const end = toApiTime(filters.end);
  if (start !== undefined) query.start_date = start;
  if (end !== undefined) query.end_date = end;
  return query;
}

/**
 * The text of the details: pretty-printed when it is JSON, the raw text otherwise.
 * The result is always shown as text, never as HTML.
 * @param details - The `details` of an entry
 * @returns The text to show
 */
export function formatDetails(details: string): string {
  try {
    return JSON.stringify(JSON.parse(details) as unknown, null, 2);
  } catch {
    return details;
  }
}
