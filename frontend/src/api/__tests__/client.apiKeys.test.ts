import { describe, it, expect, afterEach, vi } from 'vitest';
import api, { apiKeyService } from '../client';
import { resetSessionForTesting, setAuthMode } from '../session';
import { installMockBackend, pathOf, reachableText } from '../../test/mockHttp';

const KEY_ID = '44444444-4444-4444-8444-444444444444';
const RAW_KEY = `sk_${'ab12'.repeat(16)}`;

function methodOf(config: { method?: string }): string {
  return (config.method ?? '').toUpperCase();
}

describe('apiKeyService', () => {
  afterEach(() => {
    resetSessionForTesting();
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('list: GET /api/v1/api-keys with page and pageSize', async () => {
    setAuthMode('disabled');
    const body = {
      data: [
        {
          id: KEY_ID,
          user_id: '22222222-2222-4222-8222-222222222222',
          name: 'ci',
          prefix: 'sk_ab12',
          created_at: '2026-01-01T10:00:00Z',
          expires_at: '2026-04-01T10:00:00Z',
        },
      ],
      total: 1,
      page: 2,
      pageSize: 10,
    };
    const adapter = installMockBackend([api], () => ({ status: 200, data: body }));

    const result = await apiKeyService.list(2, 10);

    const config = adapter.mock.calls[0][0];
    expect(methodOf(config)).toBe('GET');
    expect(pathOf(config)).toBe('/api/v1/api-keys?page=2&pageSize=10');
    expect(result).toEqual(body);
  });

  it('create: POST /api/v1/api-keys with the name and expires_in_days, and returns the raw key', async () => {
    setAuthMode('disabled');
    const created = {
      id: KEY_ID,
      name: 'ci',
      prefix: 'sk_ab12',
      raw_key: RAW_KEY,
      created_at: '2026-01-01T10:00:00Z',
      expires_at: '2026-04-01T10:00:00Z',
    };
    const adapter = installMockBackend([api], () => ({ status: 201, data: created }));

    const result = await apiKeyService.create({ name: 'ci', expires_in_days: 90 });

    const config = adapter.mock.calls[0][0];
    expect(methodOf(config)).toBe('POST');
    expect(pathOf(config)).toBe('/api/v1/api-keys');
    expect(JSON.parse(String(config.data))).toEqual({ name: 'ci', expires_in_days: 90 });
    expect(result).toEqual(created);
  });

  it('delete: DELETE /api/v1/api-keys/:id and encodes the id', async () => {
    setAuthMode('disabled');
    const adapter = installMockBackend([api], () => ({ status: 204 }));

    await apiKeyService.delete(KEY_ID);

    const config = adapter.mock.calls[0][0];
    expect(methodOf(config)).toBe('DELETE');
    expect(pathOf(config)).toBe(`/api/v1/api-keys/${KEY_ID}`);
  });

  it('delete: encodes an id with a reserved character in the path', async () => {
    setAuthMode('disabled');
    const adapter = installMockBackend([api], () => ({ status: 204 }));

    await apiKeyService.delete('a/b?c');

    expect(pathOf(adapter.mock.calls[0][0])).toBe('/api/v1/api-keys/a%2Fb%3Fc');
  });

  it('rethrows a failed list and logs only the summary', async () => {
    setAuthMode('disabled');
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    installMockBackend([api], () => ({ status: 500, data: { error: 'Internal server error' } }));

    await expect(apiKeyService.list(1, 25)).rejects.toMatchObject({ response: { status: 500 } });

    expect(consoleError).toHaveBeenCalledWith('Failed to list API keys:', 'GET /api/v1/api-keys -> 500 ERR_BAD_RESPONSE');
  });

  it('rethrows a refused create and never logs the name or the body', async () => {
    setAuthMode('disabled');
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    installMockBackend([api], () => ({ status: 409, data: { error: 'You have reached the limit of 25 active API keys' } }));

    await expect(apiKeyService.create({ name: 'secret-name-xyz', expires_in_days: 30 })).rejects.toMatchObject({
      response: { status: 409 },
    });

    expect(consoleError).toHaveBeenCalled();
    expect(reachableText(consoleError.mock.calls.flat())).not.toContain('secret-name-xyz');
  });

  it('never logs the raw key of a failed create answer', async () => {
    setAuthMode('disabled');
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    installMockBackend([api], () => ({ status: 400, data: { error: 'name is required', raw_key: RAW_KEY } }));

    await expect(apiKeyService.create({ name: '', expires_in_days: 90 })).rejects.toBeDefined();

    expect(reachableText(consoleError.mock.calls.flat())).not.toContain(RAW_KEY);
  });
});
