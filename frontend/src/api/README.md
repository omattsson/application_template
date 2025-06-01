# API Integration Layer

This directory contains the API client configuration and service implementations for communicating with the backend service.

## Overview

The API layer provides:

- 🔧 Centralized API configuration
- 🌐 Axios client setup with interceptors
- 🚦 Health check service integration
- ⚠️ Global error handling
- 🔄 Request/response interceptors
- 📝 TypeScript types for API responses

## Directory Structure

```text
api/
├── client.ts   # Axios client setup and interceptors
├── config.ts   # API configuration and endpoints
└── README.md   # This documentation
```

## Configuration

The API configuration is managed in `config.ts`:

```typescript
export const API_BASE_URL = process.env.NODE_ENV === 'production' 
  ? '/api' 
  : 'http://localhost:8081';

export const endpoints = {
  health: {
    live: `${API_BASE_URL}/health/live`,
    ready: `${API_BASE_URL}/health/ready`,
  }
};
```

## API Client Setup

The Axios client is configured in `client.ts` with:

- Base URL configuration
- Default headers
- Request/response interceptors
- Error handling

```typescript
import axios from 'axios';
import { API_BASE_URL } from './config';

export const apiClient = axios.create({
  baseURL: API_BASE_URL,
  headers: {
    'Content-Type': 'application/json',
  },
});
```

## Available Services

### Health Service

Methods for checking backend health status:

```typescript
export const healthService = {
  checkLiveness: async () => {
    const response = await apiClient.get('/health/live');
    return response.data;
  },

  checkReadiness: async () => {
    const response = await apiClient.get('/health/ready');
    return response.data;
  },
};
```

## Error Handling

Global error handling through Axios interceptors:

```typescript
apiClient.interceptors.response.use(
  (response) => response,
  (error) => {
    // Log errors
    console.error('API Error:', error);
    
    // Handle specific error cases
    if (error.response?.status === 401) {
      // Handle unauthorized
    }
    
    return Promise.reject(error);
  }
);
```

## Usage Example

Using the API client in components:

```typescript
import { healthService } from '@/api/client';

const HealthCheck = () => {
  const [status, setStatus] = useState<string>('');

  const checkHealth = async () => {
    try {
      const result = await healthService.checkLiveness();
      setStatus(result.status);
    } catch (error) {
      console.error('Health check failed:', error);
    }
  };
};
```

## Type Safety

TypeScript interfaces for API responses:

```typescript
interface HealthResponse {
  status: string;
  timestamp: string;
}

interface ErrorResponse {
  error: string;
  message: string;
  status: number;
}
```

## Best Practices

1. Always use the centralized API client for requests
2. Handle errors appropriately at the service level
3. Use TypeScript interfaces for request/response types
4. Keep endpoint definitions in `config.ts`
5. Add interceptors for common operations (auth, logging)

## Adding New Services

When adding a new service:

1. Add endpoint configuration to `config.ts`
2. Create TypeScript interfaces for the data
3. Implement the service methods using `apiClient`
4. Add error handling specific to the service
5. Document the new service in this README

## Environment Configuration

The API layer supports different environments through environment variables:

- Development: `VITE_API_URL=http://localhost:8081`
- Production: `VITE_API_URL=/api`

Configure these in your `.env` files or deployment environment.
