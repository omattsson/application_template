import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import Home from '../index';

describe('Home Page', () => {
  it('renders the welcome heading', () => {
    render(<Home />);
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent(
      'Welcome to the Full Stack Application'
    );
  });

  it('renders the technology list', () => {
    render(<Home />);
    expect(screen.getByText(/Go backend with Gin framework/)).toBeInTheDocument();
    expect(screen.getByText(/React frontend with Material-UI/)).toBeInTheDocument();
    expect(screen.getByText(/RESTful API architecture/)).toBeInTheDocument();
    expect(screen.getByText(/Health monitoring/)).toBeInTheDocument();
    expect(screen.getByText(/Swagger documentation/)).toBeInTheDocument();
  });

  it('renders the introductory text', () => {
    render(<Home />);
    expect(
      screen.getByText(/This is a modern full-stack application built with/)
    ).toBeInTheDocument();
  });
});
