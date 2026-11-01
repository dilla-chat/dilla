import { readFileSync } from 'node:fs';
import { render, screen, fireEvent, act } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { MessageRow } from './MessageRow.tsx';
import { MessageToolbar } from '../MessageToolbar/MessageToolbar.tsx';
import { PhoneLayoutProvider } from '../internal/phoneLayout.ts';
import { expectNoAxeViolations } from '../test/setup.ts';

const BODY = 'anyone up for a round tonight?';
const UNREADABLE = 'This message could not be read on this device.';

describe('MessageRow', () => {
  it('shows a thread chip as a button named by its label, under the body, and calls onOpen', async () => {
    const onOpen = vi.fn();
    const { container } = render(<MessageRow author="ada" time="21:02" body={BODY} state="ok" thread={{ label: '2 replies', onOpen }} />);
    const chip = screen.getByRole('button', { name: '2 replies' });
    expect(chip).toHaveClass('d-message-row__thread');
    expect(chip.querySelector('svg[data-icon="messages-square"]')).not.toBeNull();
    const body = container.querySelector('.d-message-row__body') as Node;
    expect(Boolean(body.compareDocumentPosition(chip) & Node.DOCUMENT_POSITION_FOLLOWING)).toBe(true);
    await userEvent.setup().click(chip);
    expect(onOpen).toHaveBeenCalledTimes(1);
  });

  it('keeps the chip still on pointerdown (no focus shift), so pointerup and click reach it', () => {
    const onOpen = vi.fn();
    render(<MessageRow author="ada" time="21:02" body={BODY} state="ok" thread={{ label: '2 replies', onOpen }} />);
    const chip = screen.getByRole('button', { name: '2 replies' });
    expect(fireEvent.pointerDown(chip)).toBe(false);
    fireEvent.pointerUp(chip);
    fireEvent.click(chip);
    expect(onOpen).toHaveBeenCalledTimes(1);
  });
  it('has no thread chip without a thread, and the chip follows the row’s roving tab stop', () => {
    const { rerender } = render(<MessageRow author="ada" time="21:02" body={BODY} state="ok" />);
    expect(screen.queryByRole('button')).toBeNull();
    rerender(<MessageRow author="ada" time="21:02" body={BODY} state="ok" active thread={{ label: '1 reply', onOpen: () => {} }} />);
    expect(screen.getByRole('button', { name: '1 reply' }).tabIndex).toBe(0);
    rerender(<MessageRow author="ada" time="21:02" body={BODY} state="ok" thread={{ label: '1 reply', onOpen: () => {} }} />);
    expect(screen.getByRole('button', { name: '1 reply' }).tabIndex).toBe(-1);
  });

  it('shows author, time and body and hides the avatar from assistive tech', () => {
    const { container } = render(<MessageRow author="ada" time="21:02" body={BODY} state="ok" />);
    const row = container.firstElementChild;
    expect(row).toHaveClass('d-message-row');
    expect(row).toHaveAttribute('data-state', 'ok');
    expect(row).not.toHaveAttribute('data-own');
    expect(screen.getByText('ada')).toHaveClass('d-message-row__author');
    // Task 7: the time stands in the gutter (seen) and in the head (heard).
    expect(container.querySelector('.d-message-row__gutter')).toHaveTextContent('21:02');
    expect(container.querySelector('.d-message-row__head .d-message-row__time')).toHaveClass('d-sr-only');
    expect(screen.getByText(BODY)).toHaveClass('d-message-row__body');
    expect(screen.queryByRole('img', { name: 'ada' })).toBeNull();
    expect(container.querySelector('.d-message-row__state')).toBeNull();
  });

  it('marks web and bot senders with their tag, and the own row', () => {
    const { rerender, container } = render(<MessageRow author="björn" time="21:03" body="after nine" state="ok" tag="web" />);
    expect(screen.getByText('web')).toHaveClass('d-tag');
    rerender(<MessageRow author="loot-bot" time="21:05" body="weekly reset in 2 h" state="ok" tag="bot" own />);
    expect(screen.getByText('bot')).toHaveClass('d-tag');
    expect(container.firstElementChild).toHaveAttribute('data-own', 'true');
  });

  it('shows a pending row with its body and its state label', () => {
    render(<MessageRow author="mira" time="21:04" body="count me in" state="pending" stateLabel="sending" detail="E_IGNORED" />);
    expect(screen.getByText('count me in')).toBeInTheDocument();
    expect(screen.getByText('sending')).toHaveClass('d-message-row__state');
    expect(screen.queryByText('E_IGNORED')).toBeNull();
  });

  it('shows a failed row with its state label, its code and its actions', async () => {
    const user = userEvent.setup();
    const retry = vi.fn();
    const discard = vi.fn();
    render(<MessageRow author="mira" time="21:04" body="count me in" state="failed" stateLabel="not sent" detail="E_TOO_LARGE"
      actions={[{ label: 'retry', onAction: retry }, { label: 'discard', onAction: discard }]} />);
    expect(screen.getByText('count me in')).toBeInTheDocument();
    expect(screen.getByText('not sent')).toHaveClass('d-message-row__state');
    expect(screen.getByText('E_TOO_LARGE').tagName).toBe('CODE');
    expect(screen.getAllByRole('button').map(b => b.textContent)).toEqual(['retry', 'discard']);
    await user.click(screen.getByRole('button', { name: 'retry' }));
    await user.click(screen.getByRole('button', { name: 'discard' }));
    expect(retry).toHaveBeenCalledTimes(1);
    expect(discard).toHaveBeenCalledTimes(1);
  });

  it('never shows the body of an unreadable row, only the fixed text and the code', () => {
    const { container } = render(<MessageRow author="ada" time="21:06" body="this text must not render" state="cannot-read"
      stateLabel={UNREADABLE} detail="E_SENDER_MISMATCH" />);
    expect(container.textContent).not.toContain('this text must not render');
    expect(screen.getByText(UNREADABLE)).toHaveClass('d-message-row__note');
    expect(screen.getByText('E_SENDER_MISMATCH').tagName).toBe('CODE');
    expect(container.querySelector('.d-message-row__body')).toBeNull();
  });

  it('shows only the state label for a deleted row', () => {
    const { container } = render(<MessageRow author="ada" time="21:07" body="gone" state="deleted" stateLabel="message deleted" detail="E_IGNORED" />);
    expect(container.textContent).not.toContain('gone');
    expect(screen.getByText('message deleted')).toHaveClass('d-message-row__note');
    expect(screen.queryByText('E_IGNORED')).toBeNull();
  });

  it('keeps the line breaks of the body', () => {
    const { container } = render(<MessageRow author="ada" time="21:08" body={'line one\nline two'} state="ok" />);
    expect(container.querySelector('.d-message-row__body')?.textContent).toBe('line one\nline two');
  });

  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><MessageRow author="mira" time="21:04" body="count me in" state="failed" stateLabel="not sent"
      detail="E_TOO_LARGE" tag="web" actions={[{ label: 'retry', onAction: () => {} }, { label: 'discard', onAction: () => {} }]} /></div>);
    await expectNoAxeViolations(container);
  });

  // ---- web-2b (L-UI-40) ----
  const MSG = 'ab'.repeat(16);

  it('is an article named by its head, carrying the DOM contract, and a tab stop only while active', () => {
    const { container, rerender } = render(<MessageRow author="ada" time="21:02" body="hi" state="ok" seq="12" msgId={MSG} />);
    const row = container.querySelector('article')!;
    expect(row).toHaveClass('d-message-row');
    expect(row).toHaveAttribute('tabindex', '-1');
    expect(row).toHaveAttribute('data-seq', '12');
    expect(row).toHaveAttribute('data-msg-id', MSG);
    expect(row).not.toHaveAttribute('data-active');
    expect(row).not.toHaveAttribute('data-edited');
    expect(row).not.toHaveAttribute('data-pinned');
    expect(row).not.toHaveAttribute('data-mention');
    expect(row.getAttribute('aria-labelledby')).toBe(container.querySelector('.d-message-row__head')?.id);
    expect(screen.getByRole('article')).toHaveAccessibleName(/^ada/);
    rerender(<MessageRow author="ada" time="21:02" body="hi" state="ok" seq={null} msgId={null} active />);
    expect(row).toHaveAttribute('tabindex', '0');
    expect(row).toHaveAttribute('data-active', 'true');
    expect(row).not.toHaveAttribute('data-seq');
    expect(row).not.toHaveAttribute('data-msg-id');
  });

  it('renders body parts as text and mention pills, never the raw token and never HTML', () => {
    const { container } = render(<MessageRow author="mira" time="21:03" state="ok" mention body={[
      { kind: 'text', text: 'hey ' }, { kind: 'mention', label: '@ada', me: true, broadcast: false },
      { kind: 'text', text: ' and ' }, { kind: 'mention', label: '@everyone', me: true, broadcast: true },
      { kind: 'text', text: ' and ' }, { kind: 'mention', label: '@björn', me: false, broadcast: false },
      { kind: 'text', text: ' <b>not bold</b>' },
    ]} />);
    const body = container.querySelector('.d-message-row__body')!;
    expect(body.textContent).toBe('hey @ada and @everyone and @björn <b>not bold</b>');
    expect(body.querySelector('b')).toBeNull();
    expect([...body.querySelectorAll('.d-mention')].map((p) => [p.tagName, p.textContent, p.getAttribute('data-me'), p.getAttribute('data-broadcast')])).toEqual([
      ['SPAN', '@ada', 'true', null], ['SPAN', '@everyone', 'true', 'true'], ['SPAN', '@björn', null, null],
    ]);
    expect(container.querySelector('article')).toHaveAttribute('data-mention', 'true');
    expect(body.innerHTML).not.toContain('&lt;@');
  });

  it('marks an edited and a pinned row in its head, as text and as data attributes', () => {
    const { container } = render(<MessageRow author="ada" time="21:04" body="meet at nine" state="ok" editedLabel="edited" pinnedLabel="pinned" />);
    const head = container.querySelector('.d-message-row__head')!;
    expect(head.querySelector('.d-message-row__edited')?.textContent).toBe('edited');
    expect(head.querySelector('.d-message-row__pinned')?.textContent).toBe('pinned');
    const row = container.querySelector('article')!;
    expect(row).toHaveAttribute('data-edited', 'true');
    expect(row).toHaveAttribute('data-pinned', 'true');
  });

  it('places its parts in the order of the keyboard map', () => {
    const { container } = render(<MessageRow author="ada" time="21:04" state="ok" active body="the body"
      toolbar={<button type="button">tb</button>}
      reply={{ label: 'reply to björn', author: 'björn', excerpt: 'after nine', state: 'ok', jumpLabel: 'go to the original message', onJump: () => {} }}
      attachments={<button type="button">card</button>}
      reactions={<button type="button">chip</button>}
      pendingAction={{ label: 'not saved', tone: 'failed', actions: [{ label: 'retry', onAction: () => {} }] }} />);
    // The visible text of each button without its aria-hidden glyphs: the reply button has no aria-label (SLICE-UX-07).
    const shown = (b: Element): string => b.getAttribute('aria-label')
      ?? [...b.childNodes].filter((n) => !(n instanceof Element && n.getAttribute('aria-hidden') === 'true')).map((n) => n.textContent).join('');
    expect([...container.querySelectorAll('button')].map(shown)).toEqual([
      'tb', 'björn after nine', 'card', 'chip', 'retry',
    ]);
    const main = container.querySelector('.d-message-row__main')!;
    expect([...main.children].map((c) => c.className.split(' ')[0])).toEqual([
      // Task 7: CSS draws the reply line above the head, as in the prototype (§6.5); the DOM keeps the keyboard map.
      'd-message-row__head', 'd-message-row__toolbar', 'd-message-row__reply', 'd-message-row__body',
      'd-message-row__attachments', 'd-message-row__reactions', 'd-message-row__pending',
    ]);
  });

  it('the reply line is named by its visible text, jumps when the original is held, and says why it cannot when it is not', async () => {
    const user = userEvent.setup();
    const onJump = vi.fn();
    const { container, rerender } = render(<MessageRow author="ada" time="21:04" body="yes" state="ok" active
      reply={{ label: 'reply to björn', author: 'björn', excerpt: 'after nine', state: 'ok', jumpLabel: 'go to the original message', onJump }} />);
    const jump = screen.getByRole('button', { name: 'björn after nine' });
    expect(jump).toHaveAccessibleDescription('reply to björn go to the original message');
    expect(jump).not.toHaveAttribute('aria-label');
    expect(jump).toHaveTextContent('björn');
    expect(jump).toHaveTextContent('after nine');
    await user.click(jump);
    expect(onJump).toHaveBeenCalledTimes(1);
    rerender(<MessageRow author="ada" time="21:04" body="yes" state="ok" active
      reply={{ label: 'reply to someone', author: '', excerpt: '', state: 'missing', stateText: 'the original message cannot be shown here', jumpLabel: 'go to the original message' }} />);
    expect(container.querySelector('.d-message-row__reply button')).toBeNull();
    expect(container.querySelector('.d-message-row__reply')).toHaveAttribute('data-state', 'missing');
    expect(screen.getByText('the original message cannot be shown here')).toHaveClass('d-message-row__reply-state');
    rerender(<MessageRow author="ada" time="21:04" body="yes" state="ok" active
      reply={{ label: 'reply to björn', author: 'björn', excerpt: '', state: 'deleted', stateText: 'the original message was deleted', jumpLabel: 'go to the original message' }} />);
    expect(screen.getByText('the original message was deleted')).toBeInTheDocument();
    expect(screen.queryByRole('button')).toBeNull();
  });

  it("keeps an inactive row's own buttons out of the tab order", () => {
    const props = {
      author: 'mira', time: '21:05', body: 'count me in', state: 'failed' as const, stateLabel: 'not sent', detail: 'E_TOO_LARGE',
      actions: [{ label: 'retry', onAction: () => {} }],
      reply: { label: 'reply to ada', author: 'ada', excerpt: 'meet at nine', state: 'ok' as const, jumpLabel: 'go to the original message', onJump: () => {} },
      pendingAction: { label: 'not saved', tone: 'failed' as const, actions: [{ label: 'discard', onAction: () => {} }] },
    };
    const { container, rerender } = render(<MessageRow {...props} active={false} />);
    expect([...container.querySelectorAll('button')].map((b) => b.tabIndex)).toEqual([-1, -1, -1]);
    rerender(<MessageRow {...props} active />);
    expect([...container.querySelectorAll('button')].map((b) => b.tabIndex)).toEqual([0, 0, 0]);
  });

  it('Escape inside the row returns focus to the row; keys on the row go to onKeyDown', () => {
    const onKeyDown = vi.fn();
    const { container } = render(<MessageRow author="ada" time="21:04" body="x" state="ok" active onKeyDown={onKeyDown}
      toolbar={<button type="button">tb</button>} />);
    const row = container.querySelector('article')!;
    const tb = screen.getByRole('button', { name: 'tb' });
    tb.focus();
    fireEvent.keyDown(tb, { key: 'Escape' });
    expect(row).toHaveFocus();
    expect(onKeyDown).not.toHaveBeenCalled();
    fireEvent.keyDown(row, { key: 'Escape' });
    fireEvent.keyDown(row, { key: 'ArrowUp' });
    expect(onKeyDown.mock.calls.map((c) => (c[0] as KeyboardEvent).key)).toEqual(['Escape', 'ArrowUp']);
  });

  it('shows the pending action with its tone and its buttons, the editor in place of the body, and the flash', async () => {
    const user = userEvent.setup();
    const retry = vi.fn();
    const { container, rerender } = render(<MessageRow author="ada" time="21:04" body="meet at nine" state="ok" active
      pendingAction={{ label: 'not saved', tone: 'failed', actions: [{ label: 'retry', onAction: retry }] }} />);
    const line = container.querySelector('.d-message-row__pending')!;
    expect(line).toHaveAttribute('data-tone', 'failed');
    expect(line).toHaveTextContent('not saved');
    await user.click(screen.getByRole('button', { name: 'retry' }));
    expect(retry).toHaveBeenCalledTimes(1);
    rerender(<MessageRow author="ada" time="21:04" body="meet at nine" state="ok" flash editor={<textarea aria-label="edit your message" />} />);
    expect(container.querySelector('.d-message-row__body')).toBeNull();
    expect(screen.getByRole('textbox', { name: 'edit your message' })).toBeInTheDocument();
    expect(container.querySelector('article')).toHaveAttribute('data-flash', 'true');
  });

  it('attaches rowRef to the article', () => {
    const ref = { current: null as HTMLElement | null };
    render(<MessageRow author="ada" time="21:04" body="x" state="ok" rowRef={ref} />);
    expect(ref.current?.tagName).toBe('ARTICLE');
  });

  it('has no serious axe violations with every part shown', async () => {
    const { container } = render(<div className="d-root" role="log" aria-label="messages in #general"><MessageRow author="ada" time="21:04" state="ok" active
      editedLabel="edited" pinnedLabel="pinned" mention seq="4" msgId={MSG}
      body={[{ kind: 'text', text: 'see ' }, { kind: 'mention', label: '@mira', me: false, broadcast: false }]}
      toolbar={<div role="toolbar" aria-label="actions for this message"><button type="button">react</button></div>}
      reply={{ label: 'reply to björn', author: 'björn', excerpt: 'after nine', state: 'ok', jumpLabel: 'go to the original message', onJump: () => {} }}
      reactions={<div role="group" aria-label="reactions"><button type="button" aria-pressed="true" aria-label="thumbs up, 3">👍 3</button></div>}
      pendingAction={{ label: 'saving…', tone: 'pending' }} /></div>);
    await expectNoAxeViolations(container);
  });
});

describe('MessageRow mention colour dot', () => {
  const parts = [
    { kind: 'mention', label: '@mods', me: false, broadcast: false, color: 0x3498db },
    { kind: 'text', text: ' ' },
    { kind: 'mention', label: '@none', me: false, broadcast: false, color: null },
    { kind: 'text', text: ' ' },
    { kind: 'mention', label: '@zero', me: false, broadcast: false, color: 0 },
    { kind: 'text', text: ' ' },
    { kind: 'mention', label: '@ada', me: true, broadcast: false },
  ] as const;

  it('draws an aria-hidden dot in the colour before a coloured mention only, leaving its text unchanged', () => {
    const { container } = render(<MessageRow author="mira" time="21:03" state="ok" body={parts} />);
    const pills = [...container.querySelectorAll('.d-mention')];
    expect(pills.map(p => p.textContent)).toEqual(['@mods', '@none', '@zero', '@ada']);
    const dot = pills[0]!.querySelector('.d-role-dot') as HTMLElement;
    expect(dot).not.toBeNull();
    expect(dot).toHaveAttribute('aria-hidden', 'true');
    expect(dot.style.getPropertyValue('--d-swatch')).toBe('#3498db');
    expect(pills[0]!.firstElementChild).toBe(dot);
    expect(dot).toHaveClass('d-role-dot--inline');
    expect(container.querySelectorAll('.d-role-dot')).toHaveLength(1);
  });

  it('has no serious axe violations with a dotted mention', async () => {
    const { container } = render(<div className="d-root"><MessageRow author="mira" time="21:03" state="ok" body={parts} /></div>);
    await expectNoAxeViolations(container);
  });

  describe('grouped (R29)', () => {
    it('hides the author and time text visually, and shows the time in the gutter', () => {
      const { container } = render(<MessageRow author="ada" time="21:02" body={BODY} state="ok" grouped />);
      expect(container.querySelector('.d-message-row__author')).toHaveClass('d-sr-only');
      expect(container.querySelector('.d-message-row__head .d-message-row__time')).toHaveClass('d-sr-only');
      const gutter = container.querySelector('.d-message-row__gutter') as HTMLElement;
      expect(gutter).toHaveTextContent('21:02');
      expect(gutter).toHaveAttribute('aria-hidden', 'true');
      expect(container.querySelector('.d-message-row__avatar .d-avatar')).toBeNull();
      expect(container.querySelector('article')).toHaveAttribute('data-grouped', 'true');
    });
    it('shows the full 12-hour time in the gutter, and the same text for the screen reader', () => {
      const { container } = render(<MessageRow author="ada" time="10:06 PM" body={BODY} state="ok" grouped />);
      expect(container.querySelector('.d-message-row__gutter')).toHaveTextContent('10:06 PM');
      expect(container.querySelector('.d-message-row__head .d-message-row__time')).toHaveTextContent('10:06 PM');
    });
    it('shows gutterTime in the gutter and the full time as its title, while the head keeps the full time', () => {
      const { container } = render(<MessageRow author="ada" time="Oct 21, 2026, 3:49 PM" gutterTime="3:49 PM" body={BODY} state="ok" grouped />);
      const gutter = container.querySelector('.d-message-row__gutter') as HTMLElement;
      expect(gutter).toHaveTextContent(/^3:49 PM$/);
      expect(gutter).toHaveAttribute('title', 'Oct 21, 2026, 3:49 PM');
      expect(gutter).toHaveAttribute('aria-hidden', 'true');
      expect(container.querySelector('.d-message-row__head .d-message-row__time')).toHaveTextContent('Oct 21, 2026, 3:49 PM');
    });
    it('keeps the row named by its author and time for a screen reader', () => {
      render(<MessageRow author="ada" time="21:02" body={BODY} state="ok" grouped />);
      expect(screen.getByRole('article', { name: /ada.*21:02/ })).toBeInTheDocument();
    });
    it('shows the author and avatar when not grouped, and the time in the gutter (README §5.5)', () => {
      const { container } = render(<MessageRow author="ada" time="21:02" body={BODY} state="ok" />);
      expect(container.querySelector('.d-message-row__author')).not.toHaveClass('d-sr-only');
      expect(container.querySelector('.d-message-row__avatar .d-avatar')).not.toBeNull();
      expect(container.querySelector('.d-message-row__gutter')).toHaveTextContent('21:02');
    });
  });

  describe('device tag (V4)', () => {
    it('shows the web tag always, with no hover wrapper', () => {
      const { container } = render(<MessageRow author="ada" time="21:02" body={BODY} state="ok" tag="web" />);
      expect(screen.getByText('web').closest('.d-message-row__tag')).toBeNull();
      expect(container.querySelector('[data-reveal]')).toBeNull();
    });
    it('shows no tag on a grouped row', () => {
      render(<MessageRow author="ada" time="21:02" body={BODY} state="ok" tag="web" grouped />);
      expect(screen.queryByText('web')).toBeNull();
    });
  });

  describe('tap to reveal on a phone (R30)', () => {
    const toolbar = <MessageToolbar moreLabel="more actions" label="actions" tabbable={false} items={[{ id: 'reply', label: 'reply', onAction: () => {} }]} />;
    const row = (over: Partial<React.ComponentProps<typeof MessageRow>> = {}) => (
      <PhoneLayoutProvider value><MessageRow author="ada" time="21:02" body={BODY} state="ok" toolbar={toolbar} onReveal={over.onReveal ?? (() => {})} revealed={over.revealed} /></PhoneLayoutProvider>
    );
    it('renders no toolbar until revealed', () => {
      const { rerender } = render(row());
      expect(screen.queryByRole('toolbar')).toBeNull();
      rerender(row({ revealed: true }));
      expect(screen.getByRole('toolbar', { name: 'actions' })).toBeInTheDocument();
    });
    it('a click on the body asks to reveal, and a second click asks to hide', async () => {
      const onReveal = vi.fn();
      const { rerender } = render(row({ onReveal }));
      await userEvent.setup().click(screen.getByText(BODY));
      expect(onReveal).toHaveBeenLastCalledWith(true);
      rerender(row({ onReveal, revealed: true }));
      await userEvent.setup().click(screen.getByText(BODY));
      expect(onReveal).toHaveBeenLastCalledWith(false);
    });
    it('Enter on the focused row reveals; Escape hides', () => {
      const onReveal = vi.fn();
      const { rerender } = render(row({ onReveal }));
      const article = screen.getByRole('article');
      fireEvent.keyDown(article, { key: 'Enter' });
      expect(onReveal).toHaveBeenLastCalledWith(true);
      rerender(row({ onReveal, revealed: true }));
      fireEvent.keyDown(screen.getByRole('article'), { key: 'Escape' });
      expect(onReveal).toHaveBeenLastCalledWith(false);
    });
    it('Enter on an already revealed row keeps it revealed (focus may have shown it); only Escape or a tap hides', () => {
      const onReveal = vi.fn();
      render(row({ onReveal, revealed: true }));
      fireEvent.keyDown(screen.getByRole('article'), { key: 'Enter' });
      expect(onReveal).toHaveBeenLastCalledWith(true);
    });
    it('a click on a button inside the row does not toggle', async () => {
      const onReveal = vi.fn();
      render(<PhoneLayoutProvider value><MessageRow author="ada" time="21:02" body={BODY} state="ok" thread={{ label: '2 replies', onOpen: () => {} }} onReveal={onReveal} /></PhoneLayoutProvider>);
      await userEvent.setup().click(screen.getByRole('button', { name: '2 replies' }));
      expect(onReveal).not.toHaveBeenCalled();
    });
    it('off a phone the toolbar is always rendered and a click does not reveal', async () => {
      const onReveal = vi.fn();
      render(<MessageRow author="ada" time="21:02" body={BODY} state="ok" toolbar={toolbar} onReveal={onReveal} />);
      expect(screen.getByRole('toolbar')).toBeInTheDocument();
      await userEvent.setup().click(screen.getByText(BODY));
      expect(onReveal).not.toHaveBeenCalled();
    });

    it('a phone caller with no onReveal still gets its toolbar', () => {
      render(<PhoneLayoutProvider value><MessageRow author="ada" time="21:02" body={BODY} state="ok" toolbar={toolbar} /></PhoneLayoutProvider>);
      expect(screen.getByRole('toolbar')).toBeInTheDocument();
    });
  });
});

describe('MessageRow profile opens (MP7)', () => {
  const body = [
    { kind: 'text' as const, text: 'hi ' }, { kind: 'mention' as const, label: '@bob', me: false, broadcast: false, userId: 'u-bob' },
    { kind: 'text' as const, text: ' ' }, { kind: 'mention' as const, label: '@everyone', me: false, broadcast: true },
    { kind: 'text' as const, text: ' ' }, { kind: 'mention' as const, label: '@mods', me: false, broadcast: false, color: 0x3498db },
  ];
  it('makes a user mention a button that opens that user, and leaves role and broadcast pills plain', async () => {
    const onOpenProfile = vi.fn();
    const onReveal = vi.fn();
    const { container } = render(<PhoneLayoutProvider value><MessageRow author="ada" time="21:02" state="ok" body={body} onOpenProfile={onOpenProfile} onReveal={onReveal} /></PhoneLayoutProvider>);
    expect(screen.getAllByRole('button').map(b => b.textContent)).toEqual(['@bob']);
    expect(container.querySelectorAll('.d-mention').length).toBe(3);
    expect(screen.getByText('@everyone').tagName).toBe('SPAN');
    const pill = screen.getByRole('button', { name: '@bob' });
    await userEvent.setup().click(pill);
    expect(onOpenProfile).toHaveBeenCalledWith('u-bob', pill, 'mention');
    expect(onReveal).not.toHaveBeenCalled();
  });
  it('keeps the pill plain with no handler', () => {
    render(<MessageRow author="ada" time="21:02" state="ok" body={body} />);
    expect(screen.queryByRole('button')).toBeNull();
  });
  it('opens on Enter from the focused pill', async () => {
    const onOpenProfile = vi.fn();
    render(<MessageRow author="ada" time="21:02" state="ok" active body={body} onOpenProfile={onOpenProfile} />);
    const user = userEvent.setup();
    const pill = screen.getByRole('button', { name: '@bob' });
    pill.focus();
    await user.keyboard('{Enter}');
    expect(onOpenProfile).toHaveBeenCalledWith('u-bob', pill, 'mention');
  });
  it('CI: the pill and the author name are tab stops only once focus is in the row, so Shift+Tab from after the log lands on the row itself', () => {
    const { container } = render(<MessageRow author="ada" authorId="u-ada" time="21:02" state="ok" active body={body} onOpenProfile={vi.fn()} />);
    const row = container.querySelector('article') as HTMLElement;
    const stops = () => [screen.getByRole('button', { name: '@bob' }), screen.getByRole('button', { name: 'ada' })].map(b => b.getAttribute('tabindex'));
    expect(stops()).toEqual(['-1', '-1']);
    act(() => row.focus());
    expect(stops()).toEqual(['0', '0']);
    act(() => row.blur());
    expect(stops()).toEqual(['-1', '-1']);
  });
  it('takes the pill out of the tab order while the row is not active', () => {
    render(<MessageRow author="ada" time="21:02" state="ok" body={body} onOpenProfile={vi.fn()} />);
    expect(screen.getByRole('button', { name: '@bob' })).toHaveAttribute('tabindex', '-1');
  });
  it('makes the author name a button and the avatar a click target when the author is known', async () => {
    const onOpenProfile = vi.fn();
    const { container } = render(<MessageRow author="ada" authorId="u-ada" time="21:02" state="ok" body="hello" onOpenProfile={onOpenProfile} />);
    const name = screen.getByRole('button', { name: 'ada' });
    await userEvent.setup().click(name);
    expect(onOpenProfile).toHaveBeenLastCalledWith('u-ada', name, 'author');
    await userEvent.setup().click(container.querySelector('.d-message-row__avatar') as Element);
    expect(onOpenProfile).toHaveBeenCalledTimes(2);
    expect(onOpenProfile.mock.calls[1]?.[0]).toBe('u-ada');
  });
  it('on a phone a tap on the avatar opens the profile and does not reveal the row toolbar', async () => {
    const onOpenProfile = vi.fn();
    const onReveal = vi.fn();
    const { container } = render(<PhoneLayoutProvider value><MessageRow author="ada" authorId="u-ada" time="21:02" state="ok" body="hello" onOpenProfile={onOpenProfile} onReveal={onReveal} /></PhoneLayoutProvider>);
    await userEvent.setup().click(container.querySelector('.d-message-row__avatar') as Element);
    expect(onOpenProfile).toHaveBeenCalledTimes(1);
    expect(onReveal).not.toHaveBeenCalled();
  });
  it('N3: the phone target of the avatar is 44 px square whatever the avatar size', () => {
    const css = readFileSync(`${process.cwd()}/src/MessageRow/MessageRow.css`, 'utf8');
    const rule = /\[data-layout="phone"\] \.d-message-row__avatar\[data-profile="true"\]::after \{([^}]*)\}/.exec(css)?.[1] ?? '';
    expect(rule).toMatch(/width: 2\.75rem/);
    expect(rule).toMatch(/height: 2\.75rem/);
  });
  it('leaves the author plain without an id', () => {
    render(<MessageRow author="ada" time="21:02" state="ok" body="hello" onOpenProfile={vi.fn()} />);
    expect(screen.queryByRole('button')).toBeNull();
  });
});
