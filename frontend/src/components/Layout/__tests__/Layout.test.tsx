import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import Layout from '../index';

function renderLayout(children: React.ReactNode = <div>Test Content</div>) {
  return render(
    <MemoryRouter>
      <Layout>{children}</Layout>
    </MemoryRouter>
  );
}

describe('Layout', () => {
  it('renders the app title', () => {
    renderLayout();
    expect(screen.getByText('Full Stack App')).toBeInTheDocument();
  });

  it('renders Home, Health, and Items nav links', () => {
    renderLayout();
    expect(screen.getByRole('link', { name: /home/i })).toHaveAttribute('href', '/');
    expect(screen.getByRole('link', { name: /health/i })).toHaveAttribute('href', '/health');
    expect(screen.getByRole('link', { name: /items/i })).toHaveAttribute('href', '/items');
  });

  it('renders children content', () => {
    renderLayout(<p>Child paragraph</p>);
    expect(screen.getByText('Child paragraph')).toBeInTheDocument();
  });

  it('renders footer with current year', () => {
    renderLayout();
    const year = new Date().getFullYear();
    expect(screen.getByText(new RegExp(`© ${year}`))).toBeInTheDocument();
  });
});
