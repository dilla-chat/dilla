import { render, screen, fireEvent } from '@testing-library/react';
import { describe, it, expect, vi } from 'vitest';
import { BotResult, type BotStatus } from './BotResult.tsx';
const labels = { status: { pending: 'Working', success: 'Done', failed: 'Failed', cancelled: 'Not confirmed' }, onlyYou: 'Only you can see this', dismiss: 'Dismiss', tag: 'bot' };
describe('BotResult', () => {
  it.each<[BotStatus, string | null]>([['pending', 'loader'], ['success', 'check'], ['failed', 'x'], ['cancelled', null]])('%s renders its status label and icon', (status, icon) => {
    render(<BotResult author={{ name: 'Glenn', avatarName: 'Glenn', shape: 'bot' }} time="10:24" status={status} body="Result" onDismiss={() => undefined} labels={labels} />);
    const chip = screen.getByRole('status');
    expect(chip).toHaveTextContent(labels.status[status]);
    expect(chip.querySelector('svg')?.getAttribute('data-icon') ?? null).toBe(icon);
    expect(screen.getByText(labels.onlyYou)).toBeInTheDocument();
  });
  it('Dismiss calls onDismiss and built-ins use the D tile', () => {
    const dismiss = vi.fn();
    render(<BotResult author={{ name: 'dilla', avatarName: 'dilla', shape: 'circle' }} time="10:24" status="success" body="Result" onDismiss={dismiss} labels={labels} />);
    expect(screen.getByText('D')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(dismiss).toHaveBeenCalledOnce();
  });
});
