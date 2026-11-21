import { describe, it, expect, afterEach, vi } from 'vitest';
import api, { auditLogService } from '../client';
import { resetSessionForTesting, setAuthMode } from '../session';
import { installMockBackend, pathOf } from '../../test/mockHttp';

const ENTRY = {
  id: '55555555-5555-4555-8555-555555555555',
  timestamp: '2026-11-21T10:00:00Z',
  user_id: '99999999-9999-4999-8999-999999999999',
  username: 'admin',
  action: 'create',
  entity_type: 'item',
  entity_id: '42',
  details: '{"method":"POST","route":"/api/v1/items","status":201}',
};

function methodOf(config: { method?: string }): string {
  return (config.method ?? '').toUpperCase();
}

/** The query string of the request, as the server reads it. */
function queryOf(path: string): URLSearchParams {
  return new URLSearchParams(path.split('?')[1] ?? '');
}

describe('auditLogService', () => {
  afterEach(() => {
    resetSessionForTesting();
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('list: GET /api/v1/audit-logs with only the parameters that are set', async () => {
    setAuthMode('disabled');
    const adapter = installMockBackend([api], () => ({
      status: 200,
      data: { data: [ENTRY], total: 1, limit: 25, offset: 0 },
    }));

    await auditLogService.list({ limit: 25, offset: 0, action: 'create', entity_type: 'item' });

    const config = adapter.mock.calls[0][0];
    expect(methodOf(config)).toBe('GET');
    const path = pathOf(config);
    expect(path.startsWith('/api/v1/audit-logs?')).toBe(true);
    expect(Object.fromEntries(queryOf(path))).toEqual({ limit: '25', offset: '0', action: 'create', entity_type: 'item' });
  });

  it('list: sends every filter, the dates as given, and omits the unset ones', async () => {
    setAuthMode('disabled');
    const adapter = installMockBackend([api], () => ({
      status: 200,
      data: { data: [], total: 0, limit: 10, offset: 20 },
    }));

    await auditLogService.list({
      limit: 10,
      offset: 20,
      action: 'change_role',
      entity_type: 'user',
      user_id: 'abc-123',
      start_date: '2026-11-01T00:00:00.000Z',
      end_date: '2026-11-30T23:59:59.000Z',
    });

    expect(Object.fromEntries(queryOf(pathOf(adapter.mock.calls[0][0])))).toEqual({
      limit: '10',
      offset: '20',
      action: 'change_role',
      entity_type: 'user',
      user_id: 'abc-123',
      start_date: '2026-11-01T00:00:00.000Z',
      end_date: '2026-11-30T23:59:59.000Z',
    });
  });

  it('list: sends no query string when no parameter is set', async () => {
    setAuthMode('disabled');
    const adapter = installMockBackend([api], () => ({
      status: 200,
      data: { data: [], total: 0, limit: 25, offset: 0 },
    }));

    await auditLogService.list();

    expect(pathOf(adapter.mock.calls[0][0])).toBe('/api/v1/audit-logs');
  });

  it('list: returns the entries and the paging of the answer, without the cursor', async () => {
    setAuthMode('disabled');
    installMockBackend([api], () => ({
      status: 200,
      data: { data: [ENTRY], total: 31, limit: 25, offset: 25, next_cursor: 'abc' },
    }));

    const result = await auditLogService.list({ limit: 25, offset: 25 });

    expect(result).toEqual({ data: [ENTRY], total: 31, limit: 25, offset: 25 });
    expect(result).not.toHaveProperty('next_cursor');
  });

  it('rethrows a refused list (400 for a bad filter) and logs only the summary', async () => {
    setAuthMode('disabled');
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    installMockBackend([api], () => ({ status: 400, data: { error: 'Invalid action parameter' } }));

    await expect(auditLogService.list({ action: 'Bad Action' })).rejects.toThrow();

    expect(consoleError).toHaveBeenCalledWith('Failed to list audit logs:', expect.any(String));
  });
});
