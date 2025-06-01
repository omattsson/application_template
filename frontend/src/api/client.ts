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
