import { render, screen, fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { MessageRow } from '../MessageRow/MessageRow.tsx';
import { ThreadPane, type ThreadPaneProps } from './ThreadPane.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

function props(over: Partial<ThreadPaneProps> = {}): ThreadPaneProps {
  return {
    title: 'Thread', closeLabel: 'close thread', onClose: vi.fn(),
    root: <p>the root</p>,
    children: <p>a reply</p>,
    composer: <textarea aria-label="reply in thread" />,
    ...over,
  };
}

describe('ThreadPane', () => {
  it('has the title as a level-two heading and a close button', async () => {
    const p = props();
    render(<ThreadPane {...p} />);
    expect(screen.getByRole('heading', { level: 2, name: 'Thread' })).toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole('button', { name: 'close thread' }));
    expect(p.onClose).toHaveBeenCalledTimes(1);
  });

  it('renders the root, the replies and the composer in that DOM order', () => {
    const { container } = render(<ThreadPane {...props()} />);
    const root = screen.getByText('the root');
    const reply = screen.getByText('a reply');
    const composer = screen.getByRole('textbox', { name: 'reply in thread' });
    const follows = (a: Node, b: Node) => Boolean(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING);
    expect(follows(root, reply)).toBe(true);
    expect(follows(reply, composer)).toBe(true);
    expect(container.firstElementChild).toHaveClass('d-thread-pane');
    expect(container.firstElementChild).not.toHaveAttribute('data-phone');
  });

  it('calls onClose on Escape inside the pane and stops it there', async () => {
    const p = props();
    const outer = vi.fn();
    render(<div onKeyDown={outer}><ThreadPane {...p} /></div>);
    screen.getByRole('textbox', { name: 'reply in thread' }).focus();
    await userEvent.setup().keyboard('{Escape}');
    expect(p.onClose).toHaveBeenCalledTimes(1);
    expect(outer).not.toHaveBeenCalled();
  });

  it('leaves an Escape that something inside already handled', async () => {
    const p = props({ composer: <textarea aria-label="reply in thread" onKeyDown={e => { if (e.key === 'Escape') e.preventDefault(); }} /> });
    render(<ThreadPane {...p} />);
    screen.getByRole('textbox', { name: 'reply in thread' }).focus();
    await userEvent.setup().keyboard('{Escape}');
    expect(p.onClose).not.toHaveBeenCalled();
  });

  it('with a missing root shows the note in the root place and keeps the replies and the composer', () => {
    render(<ThreadPane {...props({ root: <p>The message that started this thread cannot be shown here.</p> })} />);
    const note = screen.getByText('The message that started this thread cannot be shown here.');
    expect(note.closest('.d-thread-pane__root')).not.toBeNull();
    expect(screen.getByText('a reply')).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'reply in thread' })).toBeInTheDocument();
  });

  it('closes on Escape from the close button', async () => {
    const p = props();
    render(<ThreadPane {...p} />);
    screen.getByRole('button', { name: 'close thread' }).focus();
    await userEvent.setup().keyboard('{Escape}');
    expect(p.onClose).toHaveBeenCalledTimes(1);
  });

  it('on a row: Escape from inside the row returns focus to it, and the next Escape on the row closes the thread', async () => {
    const p = props({ root: <MessageRow author="ada" time="21:02" body="hello" state="ok" active thread={{ label: '1 reply', onOpen: () => {} }} /> });
    render(<ThreadPane {...p} />);
    const user = userEvent.setup();
    const chip = screen.getByRole('button', { name: '1 reply' });
    chip.focus();
    await user.keyboard('{Escape}');
    expect(p.onClose).not.toHaveBeenCalled();
    expect(screen.getByRole('article')).toHaveFocus();
    await user.keyboard('{Escape}');
    expect(p.onClose).toHaveBeenCalledTimes(1);
  });

  it('ignores an Escape that ends an IME composition', () => {
    const p = props();
    render(<ThreadPane {...p} />);
    const box = screen.getByRole('textbox', { name: 'reply in thread' });
    fireEvent.keyDown(box, { key: 'Escape', isComposing: true });
    fireEvent.keyDown(box, { key: 'Escape', keyCode: 229 });
    expect(p.onClose).not.toHaveBeenCalled();
  });

  // Task 11 (Jonas, Q4): the phone sheet has the column's head (the prototype's), its × the close.
  it('on a phone is a sheet with the column head whose × is the close', async () => {
    const p = props({ phone: true });
    const { container } = render(<ThreadPane {...p} />);
    expect(container.firstElementChild).toHaveAttribute('data-phone', 'true');
    expect(container.querySelector('.d-phone-header')).toBeNull();
    expect(screen.getByRole('heading', { level: 2, name: 'Thread' })).toHaveClass('d-thread-pane__title');
    expect(screen.getAllByRole('button', { name: 'close thread' })).toHaveLength(1);
    await userEvent.setup().click(screen.getByRole('button', { name: 'close thread' }));
    expect(p.onClose).toHaveBeenCalledTimes(1);
  });

  it('has no axe violations as a column or a sheet', async () => {
    const col = render(<div className="d-root"><ThreadPane {...props()} /></div>);
    await expectNoAxeViolations(col.container);
    col.unmount();
    const sheet = render(<div className="d-root"><ThreadPane {...props({ phone: true })} /></div>);
    await expectNoAxeViolations(sheet.container);
  });
});
