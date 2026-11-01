import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { createRef } from 'react';
import { describe, it, expect, vi } from 'vitest';
import { Composer, type ComposerProps } from './Composer.tsx';

function setup(over: Partial<ComposerProps> = {}) {
  const props: ComposerProps = {
    label: 'message #general', placeholder: 'write a message', maxLength: 4000, value: '', onChange: () => {}, onSend: () => {},
    sendLabel: 'send', counterLabel: n => String(n), ...over,
  };
  return render(<Composer {...props} />);
}

describe('Composer: the gif button (E8)', () => {
  it('shows no gif button unless the owner gives one', () => {
    setup();
    expect(screen.queryByRole('button', { name: 'add a gif' })).toBeNull();
  });

  it('offers a gif toggle after attach in the footer, with the popup state and the controlled panel', async () => {
    const onToggle = vi.fn();
    const ref = createRef<HTMLButtonElement>();
    setup({ attachLabel: 'attach files', onAttach: () => {}, gif: { label: 'add a gif', open: false, controls: 'picker-1', onToggle, buttonRef: ref } });
    const gif = screen.getByRole('button', { name: 'add a gif' });
    expect(gif).toHaveTextContent('GIF');
    expect(gif).toHaveAttribute('aria-haspopup', 'dialog');
    expect(gif).toHaveAttribute('aria-expanded', 'false');
    expect(gif).toHaveAttribute('data-gif-toggle');
    expect(ref.current).toBe(gif);
    const attach = screen.getByRole('button', { name: 'attach files' });
    expect(attach.compareDocumentPosition(gif) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByRole('textbox').compareDocumentPosition(gif) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    await userEvent.click(gif);
    expect(onToggle).toHaveBeenCalledTimes(1);
    expect(gif).not.toHaveAttribute('aria-controls');
  });

  it('points at the panel while it is open', () => {
    setup({ gif: { label: 'add a gif', open: true, controls: 'picker-1', onToggle: () => {} } });
    const gif = screen.getByRole('button', { name: 'add a gif' });
    expect(gif).toHaveAttribute('aria-expanded', 'true');
    expect(gif).toHaveAttribute('aria-controls', 'picker-1');
  });

  it('is disabled while the composer is', () => {
    setup({ disabled: true, disabledReason: 'paused', gif: { label: 'add a gif', open: false, onToggle: () => {} } });
    expect(screen.getByRole('button', { name: 'add a gif' })).toBeDisabled();
  });
});
