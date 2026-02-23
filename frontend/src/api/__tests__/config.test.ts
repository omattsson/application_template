import { describe, it, expect } from 'vitest';
import { API_BASE_URL, endpoints, axiosConfig } from '../config';

describe('API Config', () => {
  it('sets API_BASE_URL to localhost:8081 in non-production', () => {
    expect(API_BASE_URL).toBe('http://localhost:8081');
  });

  it('defines health endpoints', () => {
    expect(endpoints.health.live).toBe(`${API_BASE_URL}/health/live`);
    expect(endpoints.health.ready).toBe(`${API_BASE_URL}/health/ready`);
  });

  it('configures axios with correct baseURL and headers', () => {
    expect(axiosConfig.baseURL).toBe(API_BASE_URL);
    expect(axiosConfig.headers['Content-Type']).toBe('application/json');
  });
});
