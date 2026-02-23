import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import App from '../App';

// Mock the health service to prevent actual API calls during render
vi.mock('../api/client', () => ({
  healthService: {
    checkLiveness: vi.fn().mockReturnValue(new Promise(() => {})),
    checkReadiness: vi.fn().mockReturnValue(new Promise(() => {})),
  },
}));

describe('App', () => {
  it('renders the layout with navigation', () => {
    render(<App />);
    expect(screen.getByText('Full Stack App')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /home/i })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /health/i })).toBeInTheDocument();
  });

  it('renders the Home page by default', () => {
    render(<App />);
    expect(
      screen.getByRole('heading', { name: /welcome to the full stack application/i })
    ).toBeInTheDocument();
  });
});
