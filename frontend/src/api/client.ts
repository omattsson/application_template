import axios from 'axios';
import { axiosConfig } from './config';

const api = axios.create(axiosConfig);

// Add response interceptor for error handling
api.interceptors.response.use(
  (response) => response,
  (error) => {
    console.error('API Error:', error);
    return Promise.reject(error);
  }
);

export interface Item {
  id: number;
  name: string;
  price: number;
  description?: string;
  created_at: string;
  updated_at: string;
}

export const itemService = {
  list: async (limit?: number, offset?: number): Promise<Item[]> => {
    try {
      const params = new URLSearchParams();
      if (limit !== undefined) params.set('limit', String(limit));
      if (offset !== undefined) params.set('offset', String(offset));
      const query = params.toString() ? `?${params.toString()}` : '';
      const response = await api.get<Item[]>(`/api/v1/items${query}`);
      return response.data;
    } catch (error) {
      console.error('Failed to fetch items:', error);
      throw error;
    }
  },

  get: async (id: number): Promise<Item> => {
    try {
      const response = await api.get<Item>(`/api/v1/items/${id}`);
      return response.data;
    } catch (error) {
      console.error('Failed to fetch item:', error);
      throw error;
    }
  },

  create: async (item: Omit<Item, 'id' | 'created_at' | 'updated_at'>): Promise<Item> => {
    try {
      const response = await api.post<Item>('/api/v1/items', item);
      return response.data;
    } catch (error) {
      console.error('Failed to create item:', error);
      throw error;
    }
  },

  update: async (id: number, item: Partial<Omit<Item, 'id' | 'created_at' | 'updated_at'>>): Promise<Item> => {
    try {
      const response = await api.put<Item>(`/api/v1/items/${id}`, item);
      return response.data;
    } catch (error) {
      console.error('Failed to update item:', error);
      throw error;
    }
  },

  delete: async (id: number): Promise<void> => {
    try {
      await api.delete(`/api/v1/items/${id}`);
    } catch (error) {
      console.error('Failed to delete item:', error);
      throw error;
    }
  },
};

export const healthService = {
  checkLiveness: async () => {
    try {
      const response = await api.get('/health/live');
      return response.data;
    } catch (error) {
      console.error('Liveness check failed:', error);
      throw error;
    }
  },

  checkReadiness: async () => {
    try {
      const response = await api.get('/health/ready');
      return response.data;
    } catch (error) {
      console.error('Readiness check failed:', error);
      throw error;
    }
  },
};

export default api;
