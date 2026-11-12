import { describe, it, expect, afterEach, vi } from 'vitest';
import api, { userService } from '../client';
import { resetSessionForTesting, setAuthMode } from '../session';
import { installMockBackend, pathOf, reachableText } from '../../test/mockHttp';

const USER_ID = '22222222-2222-4222-8222-222222222222';

function methodOf(config: { method?: string }): string {
  return (config.method ?? '').toUpperCase();
}

describe('userService', () => {
  afterEach(() => {
    resetSessionForTesting();
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it('list: GET /api/v1/users with page and pageSize', async () => {
    setAuthMode('disabled');
    const body = { data: [], total: 0, page: 3, pageSize: 50 };
    const adapter = installMockBackend([api], () => ({ status: 200, data: body }));

    const result = await userService.list(3, 50);

    const config = adapter.mock.calls[0][0];
    expect(methodOf(config)).toBe('GET');
    expect(pathOf(config)).toBe('/api/v1/users?page=3&pageSize=50');
    expect(result).toEqual(body);
  });

  it('create: POST /api/v1/users with the body', async () => {
    setAuthMode('disabled');
    const created = { id: USER_ID, username: 'dave' };
    const adapter = installMockBackend([api], () => ({ status: 201, data: created }));
    const request = { username: 'dave', email: '', display_name: '', password: 'long enough password', role: 'user' as const };

    const result = await userService.create(request);

    const config = adapter.mock.calls[0][0];
    expect(methodOf(config)).toBe('POST');
    expect(pathOf(config)).toBe('/api/v1/users');
    expect(JSON.parse(String(config.data))).toEqual(request);
    expect(result).toEqual(created);
  });

  it('changeRole: PUT /api/v1/users/:id/role with {role}', async () => {
    setAuthMode('disabled');
    const answer = { id: USER_ID, old_role: 'user', new_role: 'devops', message: 'Role changed', changed: true, idp_managed: false };
    const adapter = installMockBackend([api], () => ({ status: 200, data: answer }));

    const result = await userService.changeRole(USER_ID, 'devops');

    const config = adapter.mock.calls[0][0];
    expect(methodOf(config)).toBe('PUT');
    expect(pathOf(config)).toBe(`/api/v1/users/${USER_ID}/role`);
    expect(JSON.parse(String(config.data))).toEqual({ role: 'devops' });
    expect(result).toEqual(answer);
  });

  it('setDisabled: PUT /api/v1/users/:id/disabled with {disabled}', async () => {
    setAuthMode('disabled');
    const answer = { id: USER_ID, disabled: true, changed: true, message: 'User disabled' };
    const adapter = installMockBackend([api], () => ({ status: 200, data: answer }));

    const result = await userService.setDisabled(USER_ID, true);

    const config = adapter.mock.calls[0][0];
    expect(methodOf(config)).toBe('PUT');
    expect(pathOf(config)).toBe(`/api/v1/users/${USER_ID}/disabled`);
    expect(JSON.parse(String(config.data))).toEqual({ disabled: true });
    expect(result).toEqual(answer);
  });

  it('delete: DELETE /api/v1/users/:id', async () => {
    setAuthMode('disabled');
    const adapter = installMockBackend([api], () => ({ status: 204 }));

    await userService.delete(USER_ID);

    const config = adapter.mock.calls[0][0];
    expect(methodOf(config)).toBe('DELETE');
    expect(pathOf(config)).toBe(`/api/v1/users/${USER_ID}`);
  });

  it('encodes the id in the path', async () => {
    setAuthMode('disabled');
    const adapter = installMockBackend([api], () => ({ status: 204 }));

    await userService.delete('a/b');

    expect(pathOf(adapter.mock.calls[0][0])).toBe('/api/v1/users/a%2Fb');
  });

  it('rethrows a failed request and never logs the password or the token', async () => {
    setAuthMode('disabled');
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    installMockBackend([api], () => ({ status: 400, data: { error: 'Password must be at least 8 characters' } }));

    await expect(
      userService.create({ username: 'dave', email: '', display_name: '', password: 'hunter2hunter2', role: 'user' }),
    ).rejects.toMatchObject({ response: { status: 400 } });

    expect(consoleError).toHaveBeenCalled();
    expect(reachableText(consoleError.mock.calls.flat())).not.toContain('hunter2hunter2');
  });
});
