import { describe, it, expect } from 'vitest';
import {
  EMPTY_FILTERS,
  MAX_AUDIT_OFFSET,
  buildAuditQuery,
  formatDetails,
  hasFilters,
  pagerCount,
  rangeError,
  toApiTime,
} from '../filters';
import type { AuditFilters } from '../filters';

describe('pagerCount', () => {
  it('is the total when the list is short', () => {
    expect(pagerCount(30, 25)).toBe(30);
    expect(pagerCount(0, 25)).toBe(0);
  });

  it('stops so that the last page starts at the largest reachable offset, for every page size', () => {
    expect(MAX_AUDIT_OFFSET).toBe(100000);
    for (const size of [10, 25, 50, 100]) {
      const count = pagerCount(200000, size);
      const lastPage = Math.ceil(count / size) - 1;
      expect(lastPage * size).toBe(MAX_AUDIT_OFFSET);
    }
    expect(pagerCount(200000, 25)).toBe(100025);
  });
});

describe('toApiTime', () => {
  it('converts a local date-time to RFC3339 in UTC (the zone is always sent)', () => {
    const local = '2026-11-21T10:30';
    expect(toApiTime(local)).toBe(new Date(local).toISOString());
    expect(toApiTime(local)).toMatch(/Z$/);
  });

  it('gives no value for an empty or an incomplete input', () => {
    expect(toApiTime('')).toBeUndefined();
    expect(toApiTime('2026-13-45T99:99')).toBeUndefined();
  });
});

describe('rangeError', () => {
  it('is null when the start is before the end, equal to it, or when one side is empty', () => {
    expect(rangeError('2026-11-01T00:00', '2026-11-30T23:59')).toBeNull();
    expect(rangeError('2026-11-01T10:00', '2026-11-01T10:00')).toBeNull();
    expect(rangeError('', '2026-11-30T23:59')).toBeNull();
    expect(rangeError('2026-11-01T00:00', '')).toBeNull();
  });

  it('gives a message when the start is after the end', () => {
    expect(rangeError('2026-11-30T00:00', '2026-11-01T00:00')).toBe('The start must not be after the end.');
  });
});

describe('buildAuditQuery', () => {
  it('sends only the set filters, with the page as limit and offset', () => {
    const applied: AuditFilters = { ...EMPTY_FILTERS, action: 'create', entityType: 'item' };
    expect(buildAuditQuery(applied, 2, 25)).toEqual({ limit: 25, offset: 50, action: 'create', entity_type: 'item' });
  });

  it('trims the user ID and sends nothing for a blank one', () => {
    expect(buildAuditQuery({ ...EMPTY_FILTERS, userId: '  abc-1  ' }, 0, 10)).toEqual({
      limit: 10,
      offset: 0,
      user_id: 'abc-1',
    });
    expect(buildAuditQuery({ ...EMPTY_FILTERS, userId: '   ' }, 0, 10)).toEqual({ limit: 10, offset: 0 });
  });

  it('converts both dates to RFC3339', () => {
    const applied: AuditFilters = { ...EMPTY_FILTERS, start: '2026-11-01T00:00', end: '2026-11-30T23:59' };
    expect(buildAuditQuery(applied, 0, 25)).toEqual({
      limit: 25,
      offset: 0,
      start_date: new Date('2026-11-01T00:00').toISOString(),
      end_date: new Date('2026-11-30T23:59').toISOString(),
    });
  });
});

describe('hasFilters', () => {
  it('is false for the empty filters and true when any field is set', () => {
    expect(hasFilters(EMPTY_FILTERS)).toBe(false);
    expect(hasFilters({ ...EMPTY_FILTERS, action: 'delete' })).toBe(true);
    expect(hasFilters({ ...EMPTY_FILTERS, userId: 'x' })).toBe(true);
    expect(hasFilters({ ...EMPTY_FILTERS, end: '2026-11-30T23:59' })).toBe(true);
  });
});

describe('formatDetails', () => {
  it('pretty-prints JSON text', () => {
    expect(formatDetails('{"method":"POST","status":201}')).toBe('{\n  "method": "POST",\n  "status": 201\n}');
  });

  it('returns the raw text when it is not JSON, and the text of an empty value as an empty string', () => {
    expect(formatDetails('not json <b>x</b>')).toBe('not json <b>x</b>');
    expect(formatDetails('')).toBe('');
  });
});
