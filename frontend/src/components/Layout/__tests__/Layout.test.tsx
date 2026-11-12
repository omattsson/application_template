import { describe, it, expect, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import Layout from '../index';
import { ThemeModeProvider } from '../../../context/ThemeContext';

function renderLayout(children: React.ReactNode = <div>Test Content</div>) {
  return render(
    <ThemeModeProvider>
      <MemoryRouter>
        <Layout>{children}</Layout>
      </MemoryRouter>
    </ThemeModeProvider>
  );
}

describe('Layout', () => {
  afterEach(() => {
    localStorage.clear();
  });

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

  it('has a theme toggle that switches between light and dark and is persisted', async () => {
    const user = userEvent.setup();
    renderLayout();

    await user.click(screen.getByRole('button', { name: 'Switch to dark mode' }));
    expect(screen.getByRole('button', { name: 'Switch to light mode' })).toBeInTheDocument();
    expect(localStorage.getItem('theme-mode')).toBe('dark');

    await user.click(screen.getByRole('button', { name: 'Switch to light mode' }));
    expect(screen.getByRole('button', { name: 'Switch to dark mode' })).toBeInTheDocument();
    expect(localStorage.getItem('theme-mode')).toBe('light');
  });
});
