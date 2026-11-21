import { describe, it, expect, afterEach, vi } from 'vitest';
import api, { itemService } from '../client';
import { authApi, resetRefreshForTesting } from '../refresh';
import { resetSessionForTesting, setAuthMode } from '../session';
import { installMockBackend, pathOf } from '../../test/mockHttp';

const sampleItems = [
  { id: 1, name: 'Widget', price: 9.99, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' },
  { id: 2, name: 'Gadget', price: 19.99, created_at: '2026-01-02T00:00:00Z', updated_at: '2026-01-02T00:00:00Z' },
];

describe('itemService.list', () => {
  afterEach(() => {
    resetRefreshForTesting();
    resetSessionForTesting();
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('sends no query when called without parameters', async () => {
    setAuthMode('disabled');
    const adapter = installMockBackend([api, authApi], () => ({ status: 200, data: sampleItems, headers: { 'X-Total-Count': '2' } }));

    await itemService.list();

    expect(pathOf(adapter.mock.calls[0][0])).toBe('/api/v1/items');
  });

  it('sends limit, offset, sort, order and name as query parameters', async () => {
    setAuthMode('disabled');
    const adapter = installMockBackend([api, authApi], () => ({ status: 200, data: [], headers: { 'X-Total-Count': '0' } }));

    await itemService.list({ limit: 10, offset: 20, sort: 'created_at', order: 'desc', name: 'wid get' });

    const url = new URL(pathOf(adapter.mock.calls[0][0]), 'http://localhost');
    expect(url.pathname).toBe('/api/v1/items');
    expect(url.searchParams.get('limit')).toBe('10');
    expect(url.searchParams.get('offset')).toBe('20');
    expect(url.searchParams.get('sort')).toBe('created_at');
    expect(url.searchParams.get('order')).toBe('desc');
    expect(url.searchParams.get('name')).toBe('wid get');
  });

  it('omits the parameters that were not given', async () => {
    setAuthMode('disabled');
    const adapter = installMockBackend([api, authApi], () => ({ status: 200, data: [], headers: { 'X-Total-Count': '0' } }));

    await itemService.list({ limit: 25, offset: 0 });

    const url = new URL(pathOf(adapter.mock.calls[0][0]), 'http://localhost');
    expect([...url.searchParams.keys()].sort()).toEqual(['limit', 'offset']);
  });

  it('returns the rows of the body and the total of the X-Total-Count header', async () => {
    setAuthMode('disabled');
    installMockBackend([api, authApi], () => ({ status: 200, data: sampleItems, headers: { 'X-Total-Count': '42' } }));

    const result = await itemService.list({ limit: 2, offset: 0 });

    expect(result).toEqual({ items: sampleItems, total: 42 });
  });

  it('falls back to the number of rows when the X-Total-Count header is missing', async () => {
    setAuthMode('disabled');
    installMockBackend([api, authApi], () => ({ status: 200, data: sampleItems }));

    const result = await itemService.list();

    expect(result).toEqual({ items: sampleItems, total: 2 });
  });

  it('falls back to the number of rows when the X-Total-Count header is not a count', async () => {
    setAuthMode('disabled');
    installMockBackend([api, authApi], () => ({ status: 200, data: sampleItems, headers: { 'X-Total-Count': 'many' } }));

    const result = await itemService.list();

    expect(result.total).toBe(2);
  });

  it('rejects when the request fails', async () => {
    setAuthMode('disabled');
    vi.spyOn(console, 'error').mockImplementation(() => {});
    installMockBackend([api, authApi], () => ({ status: 500, data: {} }));

    await expect(itemService.list({ limit: 10 })).rejects.toMatchObject({ response: { status: 500 } });
  });
});
