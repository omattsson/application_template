# Frontend Application

This is a React-based frontend application built with TypeScript, Vite, and other modern web technologies.

## Features

- ⚡️ Lightning-fast development with Vite
- 🔧 TypeScript for type safety
- 📱 Responsive layouts
- 🔄 Efficient API integration with axios
- 🛣️ Clean routing with React Router
- 🧪 Testing setup included
- 🐳 Docker support for development and production

## Project Structure

```text
frontend/
├── Dockerfile           # Docker configuration for development and production
├── index.html          # HTML entry point
├── nginx.conf          # Nginx configuration for production
├── package.json        # Project dependencies and scripts
├── tsconfig.json       # TypeScript configuration
├── vite.config.ts      # Vite configuration
└── src/
    ├── api/           # API client configuration and services
    │   ├── client.ts  # Axios client setup
    │   └── config.ts  # API configuration
    ├── components/    # Reusable React components
    │   └── Layout/    # Layout components
    ├── pages/         # Page components
    │   ├── Health/    # Health check page
    │   └── Home/      # Home page
    ├── services/      # Business logic and services
    ├── utils/         # Utility functions
    ├── App.tsx        # Root React component
    ├── main.tsx       # Application entry point
    └── routes.tsx     # Application routing configuration
```

## Prerequisites

- Node.js 22.12 or later
- npm 9.x or later
- Docker (for containerized development)

## Development Setup

1. **Install dependencies**:

   ```bash
   npm install
   ```

2. **Start the development server**:

   With Docker (recommended):

   ```bash
   docker compose up frontend
   ```

   Without Docker:

   ```bash
   npm run dev
   ```

   The application will be available at [http://localhost:3000](http://localhost:3000)

## Available Scripts

- `npm run dev` - Start development server
- `npm run build` - Build for production
- `npm run preview` - Preview production build
- `npm run test` - Run tests
- `npm run lint` - Run ESLint
- `npm run format` - Format code with Prettier

## Environment Variables

Create a `.env` file in the root directory:

```text
VITE_API_URL=http://localhost:8081  # Backend that the dev server proxies /api and /ws to
```

Available environment variables:

- `VITE_API_URL` - Target of the Vite dev proxy (default `http://localhost:8081`; Compose sets `http://backend:8081`). The browser always calls the API same-origin through `/api` (dev server or nginx), which the optional login needs for its httpOnly refresh cookie.
- `PORT` - Development server port (default: 3000)

## API Integration

The `api` directory contains all API-related code:

- `client.ts` - Axios client configuration
- `config.ts` - API endpoints and configuration

Example usage:

```typescript
import { apiClient } from '@/api/client';

// Make API calls
const response = await apiClient.get('/health/live');
```

## Routing

Routes are defined in `src/routes.tsx`:

```typescript
import { Route } from 'react-router-dom';

<Route path="/" element={<Home />} />
<Route path="/health" element={<Health />} />
```

## Styling

The project uses CSS modules for styling. Create a `.module.css` file next to your component:

```text
Component.tsx
Component.module.css
```

## Testing

Tests are written using Vitest and React Testing Library:

```bash
# Run tests
npm test

# Run tests in watch mode
npm test:watch
```

## Docker Support

The project includes multi-stage Dockerfile for both development and production:

Development:

```bash
docker compose up frontend
```

Production:

```bash
docker compose -f docker-compose.prod.yml up frontend
```

## Contributing

1. Follow the project structure
2. Write meaningful commit messages
3. Add tests for new features
4. Update documentation as needed
5. Format code before committing
