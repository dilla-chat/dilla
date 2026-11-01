import { render, screen, fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { MessageLog, DateDivider } from './MessageLog.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

type Metrics = { scrollHeight: number; clientHeight: number };
function metrics(el: HTMLElement, m: Metrics) {
  Object.defineProperty(el, 'scrollHeight', { configurable: true, get: () => m.scrollHeight });
  Object.defineProperty(el, 'clientHeight', { configurable: true, get: () => m.clientHeight });
}
function log(keys: string[], extra: { earlier?: { label: string; onLoad(): void } } = {}) {
  return (
    <MessageLog label="messages in #general" emptyLabel="No messages yet." earlier={extra.earlier}>
      {keys.map(k => <p key={k}>{k}</p>)}
    </MessageLog>
  );
}

describe('MessageLog', () => {
  it('is a polite log named by its label and reachable by Tab', () => {
    render(log(['a']));
    const el = screen.getByRole('log', { name: 'messages in #general' });
    expect(el).toHaveClass('d-message-log');
    expect(el).toHaveAttribute('aria-live', 'polite');
    expect(el).toHaveAttribute('tabindex', '0');
  });

  it('is busy exactly while the caller says so', () => {
    const { rerender } = render(<MessageLog label="messages in #general" emptyLabel="No messages yet." busy>{[]}</MessageLog>);
    expect(screen.getByRole('log', { name: 'messages in #general' })).toHaveAttribute('aria-busy', 'true');
    rerender(<MessageLog label="messages in #general" emptyLabel="No messages yet.">{[]}</MessageLog>);
    expect(screen.getByRole('log', { name: 'messages in #general' })).not.toHaveAttribute('aria-busy');
    rerender(<MessageLog label="messages in #general" emptyLabel="No messages yet." busy={false}>{[]}</MessageLog>);
    expect(screen.getByRole('log', { name: 'messages in #general' })).not.toHaveAttribute('aria-busy');
  });

  it('shows the empty label only when there are no rows', () => {
    const { rerender } = render(log([]));
    expect(screen.getByText('No messages yet.')).toBeInTheDocument();
    rerender(log(['a']));
    expect(screen.queryByText('No messages yet.')).toBeNull();
  });

  it('offers load earlier as the first control inside the log and calls onLoad', async () => {
    const user = userEvent.setup();
    const onLoad = vi.fn();
    render(log(['a', 'b'], { earlier: { label: 'load earlier', onLoad } }));
    const el = screen.getByRole('log');
    const button = screen.getByRole('button', { name: 'load earlier' });
    expect(el.querySelector('button')).toBe(button);
    expect(el.firstElementChild).toBe(button);
    await user.click(button);
    expect(onLoad).toHaveBeenCalledTimes(1);
  });

  it('keeps the view pinned to the end while the reader is at the end', () => {
    const { rerender } = render(log(['a']));
    const el = screen.getByRole('log');
    const m = { scrollHeight: 1000, clientHeight: 200 };
    metrics(el, m);
    rerender(log(['a', 'b']));
    expect(el.scrollTop).toBe(1000);
    m.scrollHeight = 1100;
    rerender(log(['a', 'b', 'c']));
    expect(el.scrollTop).toBe(1100);
  });

  describe('on a resize of the log', () => {
    // jsdom has no ResizeObserver: a mock that keeps the callback so the test can fire it after changing a height.
    const callbacks: Array<() => void> = [];
    const observed: Element[] = [];
    class MockResizeObserver {
      constructor(cb: () => void) { callbacks.push(cb); }
      observe(el: Element) { observed.push(el); }
      unobserve() {}
      disconnect() {}
    }
    const install = () => { callbacks.length = 0; observed.length = 0; vi.stubGlobal('ResizeObserver', MockResizeObserver); };

    it('re-pins to the end when the reader was at the end', () => {
      install();
      render(log(['a', 'b']));
      const el = screen.getByRole('log');
      expect(observed).toContain(el);
      const m = { scrollHeight: 1000, clientHeight: 400 };
      metrics(el, m);
      el.scrollTop = 600;
      fireEvent.scroll(el);
      m.clientHeight = 150;
      callbacks.forEach(cb => cb());
      expect(el.scrollTop).toBe(1000);
      vi.unstubAllGlobals();
    });

    it('re-pins when a row grows without a render of the log, though the pin\'s own scroll event already saw the growth', () => {
      // 2026-10-30 CI (Firefox): two rows arrived and were pinned; an image card then grew as its thumbnail decoded,
      // before the browser sent the pin's scroll event. That event saw the log 29 px short of the end and unpinned it.
      install();
      const { rerender } = render(log(['a']));
      const el = screen.getByRole('log');
      const m = { scrollHeight: 1000, clientHeight: 400 };
      metrics(el, m);
      // A browser clamps scrollTop to scrollHeight - clientHeight, so the pin lands at the end (600), not at 1000.
      let top = 0;
      Object.defineProperty(el, 'scrollTop', { configurable: true, get: () => top,
        set: (v: number) => { top = Math.max(0, Math.min(v, m.scrollHeight - m.clientHeight)); } });
      rerender(log(['a', 'b']));
      expect(el.scrollTop).toBe(600);
      // The row grows by 29 px with no render of the log; the pin's own scroll event comes after, still at 600, and sees
      // the log 29 px (more than PIN_SLACK_PX) short of the end. It is the log's own move, so it must not unpin.
      m.scrollHeight = 1029;
      expect(m.scrollHeight - el.scrollTop - m.clientHeight).toBeGreaterThan(24);
      fireEvent.scroll(el);
      callbacks.forEach(cb => cb());
      expect(el.scrollTop).toBe(629);
      // Still pinned: the next row that arrives is followed to the end too.
      m.scrollHeight = 1100;
      rerender(log(['a', 'b', 'c']));
      expect(el.scrollTop).toBe(700);
      // In a browser only an observed element calls back: the rows themselves must be observed, not only the log.
      expect(observed).toContain(el.querySelector('.d-message-log__rows'));
      vi.unstubAllGlobals();
    });

    it('a reader who scrolls up after a pin is not pulled back by a row that grows', () => {
      install();
      const { rerender } = render(log(['a']));
      const el = screen.getByRole('log');
      const m = { scrollHeight: 1000, clientHeight: 400 };
      metrics(el, m);
      rerender(log(['a', 'b']));
      el.scrollTop = 300;
      fireEvent.scroll(el);
      m.scrollHeight = 1029;
      callbacks.forEach(cb => cb());
      expect(el.scrollTop).toBe(300);
      vi.unstubAllGlobals();
    });

    it('leaves the scroll position alone when the reader had scrolled up', () => {
      install();
      render(log(['a', 'b']));
      const el = screen.getByRole('log');
      const m = { scrollHeight: 1000, clientHeight: 400 };
      metrics(el, m);
      el.scrollTop = 100;
      fireEvent.scroll(el);
      m.clientHeight = 150;
      callbacks.forEach(cb => cb());
      expect(el.scrollTop).toBe(100);
      vi.unstubAllGlobals();
    });
  });

  it('stays where the reader scrolled to when rows arrive at the end', () => {
    const { rerender } = render(log(['a', 'b']));
    const el = screen.getByRole('log');
    const m = { scrollHeight: 1000, clientHeight: 200 };
    metrics(el, m);
    el.scrollTop = 300;
    fireEvent.scroll(el);
    m.scrollHeight = 1100;
    rerender(log(['a', 'b', 'c']));
    expect(el.scrollTop).toBe(300);
  });

  it('re-pins once the reader scrolls back within 24 px of the end', () => {
    const { rerender } = render(log(['a', 'b']));
    const el = screen.getByRole('log');
    const m = { scrollHeight: 1000, clientHeight: 200 };
    metrics(el, m);
    el.scrollTop = 300;
    fireEvent.scroll(el);
    el.scrollTop = 780;
    fireEvent.scroll(el);
    m.scrollHeight = 1100;
    rerender(log(['a', 'b', 'c']));
    expect(el.scrollTop).toBe(1100);
  });

  it('a render that adds no height leaves a reader near the end where they are, so a press is not moved under the pointer', () => {
    // 2026-10-30 CI (Firefox): "save data.bin" was pressed with the log 20 px short of the end; the row taking focus
    // re-rendered the log, which jumped to the end between press and release, and no click reached the button.
    const { rerender } = render(log(['a', 'b']));
    const el = screen.getByRole('log');
    const m = { scrollHeight: 1000, clientHeight: 200 };
    metrics(el, m);
    rerender(log(['a', 'b', 'c']));
    expect(el.scrollTop).toBe(1000);
    el.scrollTop = 780;
    fireEvent.scroll(el);
    rerender(log(['a', 'b', 'c']));
    expect(el.scrollTop).toBe(780);
    m.scrollHeight = 1100;
    rerender(log(['a', 'b', 'c', 'd']));
    expect(el.scrollTop).toBe(1100);
  });

  it('keeps the reader in place when earlier rows are put in front', () => {
    const { rerender } = render(log(['b', 'c']));
    const el = screen.getByRole('log');
    const m = { scrollHeight: 1000, clientHeight: 200 };
    metrics(el, m);
    rerender(log(['b', 'c']));
    el.scrollTop = 0;
    fireEvent.scroll(el);
    m.scrollHeight = 1400;
    rerender(log(['a', 'b', 'c']));
    expect(el.scrollTop).toBe(400);
  });

  it('never moves focus when rows arrive', () => {
    const { rerender } = render(<><button type="button">elsewhere</button>{log(['a'])}</>);
    const elsewhere = screen.getByRole('button', { name: 'elsewhere' });
    elsewhere.focus();
    rerender(<><button type="button">elsewhere</button>{log(['a', 'b'])}</>);
    expect(elsewhere).toHaveFocus();
  });

  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root">{log(['a', 'b'], { earlier: { label: 'load earlier', onLoad: () => {} } })}</div>);
    await expectNoAxeViolations(container);
  });

  it('forwards keys to onKeyDown and gives up its own tab stop while a row holds one', () => {
    const onKeyDown = vi.fn();
    const { rerender } = render(<MessageLog label="messages in #general" emptyLabel="No messages yet." onKeyDown={onKeyDown}>
      <article className="d-message-row" tabIndex={-1}>one</article>
    </MessageLog>);
    const log = screen.getByRole('log', { name: 'messages in #general' });
    expect(log).toHaveAttribute('aria-relevant', 'additions');
    expect(log).toHaveAttribute('tabindex', '0');
    rerender(<MessageLog label="messages in #general" emptyLabel="No messages yet." onKeyDown={onKeyDown}>
      <article className="d-message-row" tabIndex={-1}>one</article>
      <article className="d-message-row" tabIndex={0}>two</article>
    </MessageLog>);
    expect(log).toHaveAttribute('tabindex', '-1');
    fireEvent.keyDown(screen.getByText('two'), { key: 'ArrowUp' });
    expect(onKeyDown.mock.calls.map((c) => (c[0] as KeyboardEvent).key)).toEqual(['ArrowUp']);
    rerender(<MessageLog label="messages in #general" emptyLabel="No messages yet." onKeyDown={onKeyDown}>{[]}</MessageLog>);
    expect(log).toHaveAttribute('tabindex', '0');
  });

  it('shows a date divider as a separator named by its label', () => {
    render(<MessageLog label="messages in #general" emptyLabel="none"><DateDivider label="Mon 19 Oct" /><p>a</p></MessageLog>);
    expect(screen.getByRole('separator', { name: 'Mon 19 Oct' })).toHaveClass('d-date-divider');
  });

  describe('jump to present (R29)', () => {
    const present = (atPresent: boolean, onJumpToPresent = vi.fn()) => ({ atPresent, olderLabel: 'viewing older messages', jumpLabel: 'jump to present', onJumpToPresent });
    const withPresent = (p: ReturnType<typeof present> | undefined, onKeyDown?: (e: React.KeyboardEvent) => void) => (
      <MessageLog label="messages in #general" emptyLabel="none" present={p} onKeyDown={onKeyDown}><p><button type="button">inside</button></p></MessageLog>
    );
    it('shows both labels away from the present, and the button jumps', async () => {
      const p = present(false);
      render(withPresent(p));
      expect(screen.getByText('viewing older messages', { selector: '.d-message-log__present span' })).toBeInTheDocument();
      await userEvent.setup().click(screen.getByRole('button', { name: 'jump to present' }));
      expect(p.onJumpToPresent).toHaveBeenCalledTimes(1);
    });
    it('Ctrl+End from anywhere inside the log jumps', () => {
      const p = present(false);
      render(withPresent(p));
      fireEvent.keyDown(screen.getByRole('button', { name: 'inside' }), { key: 'End', ctrlKey: true });
      expect(p.onJumpToPresent).toHaveBeenCalledTimes(1);
    });
    it('keeps the bar out of the live region and announces it once through a status', () => {
      render(withPresent(present(false)));
      const bar = screen.getByRole('button', { name: 'jump to present' });
      expect(screen.getByRole('log').contains(bar)).toBe(false);
      expect(screen.getByRole('status')).toHaveTextContent('viewing older messages');
    });
    it('shows no bar and ignores Ctrl+End at the present, passing the key on', () => {
      const p = present(true);
      const onKeyDown = vi.fn();
      render(withPresent(p, onKeyDown));
      expect(screen.queryByText('viewing older messages')).toBeNull();
      expect(screen.queryByRole('button', { name: 'jump to present' })).toBeNull();
      fireEvent.keyDown(screen.getByRole('button', { name: 'inside' }), { key: 'End', ctrlKey: true });
      expect(p.onJumpToPresent).not.toHaveBeenCalled();
      expect(onKeyDown).toHaveBeenCalledTimes(1);
    });
    it('has no bar without the prop', () => {
      render(withPresent(undefined));
      expect(screen.queryByText('viewing older messages')).toBeNull();
    });
    it('has no axe violations with a divider and the bar', async () => {
      const { container } = render(<MessageLog label="messages" emptyLabel="none" present={present(false)}><DateDivider label="Mon 19 Oct" /><p>a</p></MessageLog>);
      await expectNoAxeViolations(container);
    });
  });
});
