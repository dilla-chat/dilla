import { useState } from 'react';
import { render, screen, fireEvent } from '@testing-library/react';
import { describe, it, expect, vi } from 'vitest';
import { BotResult, type BotStatus } from './BotResult.tsx';
const labels = { status: { pending: 'Working', success: 'Done', failed: 'Failed', cancelled: 'Not confirmed' }, onlyYou: 'Only you can see this', dismiss: 'Dismiss', dismissName: 'Dismiss: Result', tag: 'bot' };
describe('BotResult', () => {
  it.each<[BotStatus, string | null]>([['pending', 'loader'], ['success', 'check'], ['failed', 'x'], ['cancelled', 'triangle-alert']])('%s renders its status label and icon', (status, icon) => {
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
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss: Result' }));
    expect(dismiss).toHaveBeenCalledOnce();
  });
});

it('names each Dismiss and moves focus to the next result, then the composer', () => {
  function Feed() {
    const [rows, setRows] = useState(['First result', 'Last result']);
    return <div>{rows.map(body => <BotResult key={body} author={{ name: 'dilla', avatarName: 'dilla', shape: 'circle' }} time="10:24" status="success"
      body={body} labels={{ ...labels, dismissName: `Dismiss: ${body}` }} onDismiss={() => setRows(old => old.filter(text => text !== body))} />)}
      <div className="d-composer"><textarea aria-label="Message" /></div></div>;
  }
  render(<Feed />);
  fireEvent.click(screen.getByRole('button', { name: 'Dismiss: First result' }));
  expect(screen.getByRole('button', { name: 'Dismiss: Last result' })).toHaveFocus();
  fireEvent.click(screen.getByRole('button', { name: 'Dismiss: Last result' }));
  expect(screen.getByRole('textbox', { name: 'Message' })).toHaveFocus();
});
it('has just one live region, on the status chip', () => {
  const { container } = render(<BotResult author={{ name: 'Glenn', avatarName: 'Glenn', shape: 'bot' }} time="10:24" status="success" body="Result" onDismiss={() => undefined} labels={labels} />);
  expect(container.querySelector('article')).toHaveAttribute('aria-live', 'off');
  expect(screen.getByRole('status')).toHaveAttribute('aria-live', 'polite');
});
