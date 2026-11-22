import { Component } from 'react';
import type { ReactNode, ErrorInfo } from 'react';
import { Box, Typography, Button, Paper } from '@mui/material';
import { isChunkLoadError } from '../../utils/chunkError';
import { reloadPage } from '../../utils/navigation';

interface ErrorBoundaryProps {
  children: ReactNode;
  /** Rendered instead of the default message when a child throws. */
  fallback?: ReactNode;
}

interface ErrorBoundaryState {
  hasError: boolean;
  /** The error was a page chunk that failed to load: only a reload helps (see `isChunkLoadError`). */
  chunkError: boolean;
}

class ErrorBoundary extends Component<ErrorBoundaryProps, ErrorBoundaryState> {
  constructor(props: ErrorBoundaryProps) {
    super(props);
    this.state = { hasError: false, chunkError: false };
  }

  static getDerivedStateFromError(error: unknown): ErrorBoundaryState {
    return { hasError: true, chunkError: isChunkLoadError(error) };
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    console.error('ErrorBoundary caught an error:', error, info.componentStack);
  }

  handleReset = () => {
    this.setState({ hasError: false, chunkError: false });
  };

  render() {
    if (this.state.hasError) {
      if (this.props.fallback !== undefined) {
        return this.props.fallback;
      }
      // React.lazy keeps a failed import for good, so "Try again" would fail again: reload instead.
      const { chunkError } = this.state;
      return (
        <Box
          role="alert"
          sx={{
            display: 'flex',
            justifyContent: 'center',
            alignItems: 'center',
            minHeight: '60vh',
          }}
        >
          <Paper sx={{ p: 4, textAlign: 'center', maxWidth: 480 }}>
            <Typography variant="h5" component="h1" gutterBottom>
              Something went wrong
            </Typography>
            <Typography variant="body1" color="text.secondary" sx={{ mb: 3 }}>
              {chunkError
                ? 'This page could not be loaded. The application may have been updated, or the connection dropped. Reload the page to continue.'
                : 'An unexpected error occurred. Please try again.'}
            </Typography>
            <Button variant="contained" onClick={chunkError ? reloadPage : this.handleReset}>
              {chunkError ? 'Reload page' : 'Try again'}
            </Button>
          </Paper>
        </Box>
      );
    }

    return this.props.children;
  }
}

export default ErrorBoundary;
