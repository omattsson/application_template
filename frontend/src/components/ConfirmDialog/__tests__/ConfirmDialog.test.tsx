import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import ConfirmDialog from '../index';

describe('ConfirmDialog', () => {
  const defaultProps = {
    open: true,
    title: 'Delete Item',
    message: 'Are you sure you want to delete this item?',
    confirmLabel: 'Delete',
    onConfirm: vi.fn(),
    onCancel: vi.fn(),
  };

  afterEach(() => {
    vi.clearAllMocks();
  });

  it('renders title and message when open', () => {
    render(<ConfirmDialog {...defaultProps} />);
    expect(screen.getByText('Delete Item')).toBeInTheDocument();
    expect(screen.getByText('Are you sure you want to delete this item?')).toBeInTheDocument();
  });

  it('is a dialog labelled by its title and described by its message', () => {
    render(<ConfirmDialog {...defaultProps} />);
    const dialog = screen.getByRole('dialog', { name: 'Delete Item' });
    expect(dialog).toHaveAccessibleDescription('Are you sure you want to delete this item?');
  });

  it('renders the confirm label and a default cancel button', () => {
    render(<ConfirmDialog {...defaultProps} />);
    expect(screen.getByRole('button', { name: 'Delete' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeInTheDocument();
  });

  it('renders a custom cancel label', () => {
    render(<ConfirmDialog {...defaultProps} cancelLabel="No, keep" />);
    expect(screen.getByRole('button', { name: 'No, keep' })).toBeInTheDocument();
  });

  it('calls onConfirm when confirm button is clicked', async () => {
    const user = userEvent.setup();
    render(<ConfirmDialog {...defaultProps} />);
    await user.click(screen.getByRole('button', { name: 'Delete' }));
    expect(defaultProps.onConfirm).toHaveBeenCalledTimes(1);
    expect(defaultProps.onCancel).not.toHaveBeenCalled();
  });

  it('calls onCancel when cancel button is clicked', async () => {
    const user = userEvent.setup();
    render(<ConfirmDialog {...defaultProps} />);
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(defaultProps.onCancel).toHaveBeenCalledTimes(1);
    expect(defaultProps.onConfirm).not.toHaveBeenCalled();
  });

  it('calls onCancel when Escape is pressed', async () => {
    const user = userEvent.setup();
    render(<ConfirmDialog {...defaultProps} />);
    await user.keyboard('{Escape}');
    expect(defaultProps.onCancel).toHaveBeenCalledTimes(1);
    expect(defaultProps.onConfirm).not.toHaveBeenCalled();
  });

  it('does not render dialog content when open is false', () => {
    render(<ConfirmDialog {...defaultProps} open={false} />);
    expect(screen.queryByText('Delete Item')).not.toBeInTheDocument();
    expect(screen.queryByText('Are you sure you want to delete this item?')).not.toBeInTheDocument();
  });

  it('confirm button has error color (destructive variant)', () => {
    render(<ConfirmDialog {...defaultProps} />);
    const confirmButton = screen.getByRole('button', { name: 'Delete' });
    expect(confirmButton).toHaveClass('MuiButton-contained');
    expect(confirmButton).toHaveClass('MuiButton-colorError');
  });
});
