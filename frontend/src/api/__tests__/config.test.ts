import { describe, it, expect } from 'vitest';
import { API_BASE_URL, endpoints, axiosConfig, WS_BASE_URL } from '../config';

describe('API Config', () => {
  it('calls the API same-origin through the /api proxy (never a fixed backend host)', () => {
    expect(API_BASE_URL).toBe('/api');
  });

  it('defines health endpoints', () => {
    expect(endpoints.health.live).toBe(`${API_BASE_URL}/health/live`);
    expect(endpoints.health.ready).toBe(`${API_BASE_URL}/health/ready`);
  });

  it('configures axios with correct baseURL and headers', () => {
    expect(axiosConfig.baseURL).toBe(API_BASE_URL);
    expect(axiosConfig.headers['Content-Type']).toBe('application/json');
  });

  it('derives the WebSocket URL from the page origin', () => {
    const scheme = window.location.protocol === 'https:' ? 'wss' : 'ws';
    expect(WS_BASE_URL).toBe(`${scheme}://${window.location.host}`);
  });
});
