import { describe, it, expect, vi, beforeEach } from 'vitest';
import axios from 'axios';
import { healthService } from '../client';

vi.mock('axios', () => {
  const mockAxiosInstance = {
    get: vi.fn(),
    post: vi.fn(),
    put: vi.fn(),
    delete: vi.fn(),
    interceptors: {
      request: { use: vi.fn() },
      response: { use: vi.fn() },
    },
  };
  return {
    default: {
      create: vi.fn(() => mockAxiosInstance),
      __mockInstance: mockAxiosInstance,
    },
  };
});

function getMockApi() {
  return (axios as unknown as { __mockInstance: ReturnType<typeof axios.create> }).__mockInstance;
}

describe('healthService', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  describe('checkLiveness', () => {
    it('returns liveness data on success', async () => {
      const mockData = { status: 'UP' };
      const mockApi = getMockApi();
      (mockApi.get as ReturnType<typeof vi.fn>).mockResolvedValueOnce({ data: mockData });

      const result = await healthService.checkLiveness();

      expect(mockApi.get).toHaveBeenCalledWith('/health/live');
      expect(result).toEqual(mockData);
    });

    it('throws on network error', async () => {
      const mockApi = getMockApi();
      (mockApi.get as ReturnType<typeof vi.fn>).mockRejectedValueOnce(new Error('Network Error'));

      await expect(healthService.checkLiveness()).rejects.toThrow('Network Error');
    });
  });

  describe('checkReadiness', () => {
    it('returns readiness data on success', async () => {
      const mockData = { status: 'UP' };
      const mockApi = getMockApi();
      (mockApi.get as ReturnType<typeof vi.fn>).mockResolvedValueOnce({ data: mockData });

      const result = await healthService.checkReadiness();

      expect(mockApi.get).toHaveBeenCalledWith('/health/ready');
      expect(result).toEqual(mockData);
    });

    it('throws on network error', async () => {
      const mockApi = getMockApi();
      (mockApi.get as ReturnType<typeof vi.fn>).mockRejectedValueOnce(new Error('Service Unavailable'));

      await expect(healthService.checkReadiness()).rejects.toThrow('Service Unavailable');
    });
  });
});
