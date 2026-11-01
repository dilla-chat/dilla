import { act, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, it, expect, vi } from 'vitest';
import { ComposedShell } from './AppShell.fixture.tsx';
import { clampPanes } from './AppShell.tsx';

const REM = 16;
const labels = { sidebar: 'resize the channel list', aside: 'resize the member list' };
const aside = <div><button type="button">pane control</button></div>;

describe('clampPanes (16px rem)', () => {
  const base = { rail: 60, rem: REM };

  it('shell 1280 with an aside column of 240: the sidebar may reach 24rem', () => {
    expect(clampPanes({ ...base, shell: 1280, sidebar: 500, aside: 240, asideColumn: true })).toEqual({ sidebar: 384, aside: 240 });
  });

  it('shell 1000 with an aside column of 240: the sidebar max is 316, so a requested 360 clamps to 316', () => {
    expect(clampPanes({ ...base, shell: 1000, sidebar: 360, aside: 240, asideColumn: true }).sidebar).toBe(316);
  });

  it('shell 800 with an aside column of 224: the minimums win (192 and 224)', () => {
    expect(clampPanes({ ...base, shell: 800, sidebar: 240, aside: 224, asideColumn: true })).toEqual({ sidebar: 192, aside: 224 });
  });

  it('shell 900 with an aside column of 224: they do not win (main is 424)', () => {
    const r = clampPanes({ ...base, shell: 900, sidebar: 240, aside: 224, asideColumn: true });
    expect(900 - 60 - r.sidebar - r.aside).toBeGreaterThanOrEqual(24 * REM);
    expect(r.sidebar).toBe(232);
  });

  it('keeps the sidebar in [12rem, 24rem] and the aside in [14rem, 28rem]', () => {
    expect(clampPanes({ ...base, shell: 2000, sidebar: 10, aside: 10, asideColumn: true })).toEqual({ sidebar: 192, aside: 224 });
    expect(clampPanes({ ...base, shell: 2000, sidebar: 900, aside: 900, asideColumn: true })).toEqual({ sidebar: 384, aside: 448 });
  });

  it('without an aside column the aside takes no room from main', () => {
    expect(clampPanes({ ...base, shell: 700, sidebar: 384, aside: 448, asideColumn: false }).sidebar).toBe(256);
  });

  it('scales with the root font size', () => {
    expect(clampPanes({ ...base, rem: 20, shell: 2400, sidebar: 900, aside: 240, asideColumn: false }).sidebar).toBe(480);
  });
});

describe('AppShell splitters', () => {
  afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

  let shellW = 1280;
  const stubWidths = () => {
    vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockImplementation(function (this: HTMLElement) {
      return this.classList.contains('d-app-shell') ? shellW : this.classList.contains('d-app-shell__rail') ? 60 : 0;
    });
  };
  const shellProps = (over: Record<string, unknown> = {}) => ({
    resizeLabels: labels, paneWidths: { sidebar: 240, aside: 240 }, onPaneResize: vi.fn(),
    aside, asideLabel: 'members', asideOpen: true, onAsideClose: vi.fn(), ...over,
  });
  const sep = (name: string) => screen.getByRole('separator', { name });

  it('renders a vertical separator with px values; the sidebar one ends the sidebar, the aside one starts the aside', () => {
    shellW = 1280; stubWidths();
    const { container } = render(<ComposedShell shell={shellProps()} />);
    const s = sep(labels.sidebar);
    expect(s).toHaveAttribute('aria-orientation', 'vertical');
    expect(s).toHaveAttribute('tabindex', '0');
    expect(s).toHaveAttribute('aria-valuenow', '240');
    expect(s).toHaveAttribute('aria-valuemin', '192');
    expect(s).toHaveAttribute('aria-valuemax', '384');
    const sidebar = container.querySelector('.d-app-shell__sidebar')!;
    expect(sidebar.nextElementSibling).toBe(s);
    expect(s).toHaveAttribute('aria-controls', sidebar.id);
    expect(sidebar.id).not.toBe('');
    const a = sep(labels.aside);
    const side = screen.getByRole('complementary', { name: 'members' });
    expect(side.firstElementChild).toBe(a);
    expect(a).toHaveAttribute('aria-controls', side.id);
    expect(a).toHaveAttribute('aria-valuemin', '224');
    expect(a).toHaveAttribute('aria-valuemax', '448');
    expect(container.querySelector('.d-app-shell')).toHaveStyle({ '--d-sidebar-w': '240px', '--d-aside-w': '240px' });
  });

  it('focus order: sidebar, its splitter, then main', () => {
    shellW = 1280; stubWidths();
    const { container } = render(<ComposedShell shell={shellProps({ aside: undefined })} />);
    const s = sep(labels.sidebar);
    const main = container.querySelector('main')!;
    expect(s.compareDocumentPosition(main) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.queryByRole('separator', { name: labels.aside })).toBeNull();
  });

  it('no splitter on a phone, none without resizeLabels, none for an overlay aside', () => {
    shellW = 1280; stubWidths();
    const { unmount } = render(<ComposedShell shell={shellProps({ layout: 'phone' })} />);
    expect(screen.queryAllByRole('separator')).toHaveLength(0);
    unmount();
    const b = render(<ComposedShell shell={shellProps({ resizeLabels: undefined })} />);
    expect(screen.queryAllByRole('separator')).toHaveLength(0);
    expect(b.container.querySelector('.d-app-shell')?.getAttribute('style') ?? '').not.toContain('--d-sidebar-w');
    b.unmount();
    shellW = 800; stubWidths();
    render(<ComposedShell shell={shellProps()} />);
    expect(screen.getByRole('separator', { name: labels.sidebar })).toBeInTheDocument();
    expect(screen.queryByRole('separator', { name: labels.aside })).toBeNull();
  });

  it('with the aside overlay open the sidebar splitter is inert (out of the tab order); on a column it is live', () => {
    shellW = 800; stubWidths();
    const a = render(<ComposedShell shell={shellProps()} />);
    expect(screen.getByRole('separator', { name: labels.sidebar, hidden: true })).toHaveAttribute('inert');
    a.unmount();
    shellW = 1280; stubWidths();
    render(<ComposedShell shell={shellProps()} />);
    expect(sep(labels.sidebar)).not.toHaveAttribute('inert');
  });

  const key = (el: HTMLElement, k: string, shiftKey = false) => fireEvent.keyDown(el, { key: k, shiftKey });

  it('ArrowRight and ArrowLeft move the sidebar by 16px, 64px with Shift', () => {
    shellW = 1280; stubWidths();
    const onPaneResize = vi.fn();
    render(<ComposedShell shell={shellProps({ onPaneResize })} />);
    const s = sep(labels.sidebar);
    key(s, 'ArrowRight');
    expect(onPaneResize).toHaveBeenLastCalledWith('sidebar', 256);
    key(s, 'ArrowLeft');
    expect(onPaneResize).toHaveBeenLastCalledWith('sidebar', 224);
    key(s, 'ArrowRight', true);
    expect(onPaneResize).toHaveBeenLastCalledWith('sidebar', 304);
    key(s, 'ArrowLeft', true);
    expect(onPaneResize).toHaveBeenLastCalledWith('sidebar', 192);
  });

  it('the aside splitter is mirrored: ArrowLeft widens it', () => {
    shellW = 1280; stubWidths();
    const onPaneResize = vi.fn();
    render(<ComposedShell shell={shellProps({ onPaneResize })} />);
    const a = sep(labels.aside);
    key(a, 'ArrowLeft');
    expect(onPaneResize).toHaveBeenLastCalledWith('aside', 256);
    key(a, 'ArrowRight', true);
    expect(onPaneResize).toHaveBeenLastCalledWith('aside', 224);
  });

  it('Home goes to the minimum, End to the maximum, Enter resets', () => {
    shellW = 1280; stubWidths();
    const onPaneResize = vi.fn();
    render(<ComposedShell shell={shellProps({ onPaneResize })} />);
    const s = sep(labels.sidebar);
    key(s, 'Home');
    expect(onPaneResize).toHaveBeenLastCalledWith('sidebar', 192);
    key(s, 'End');
    expect(onPaneResize).toHaveBeenLastCalledWith('sidebar', 384);
    key(s, 'Enter');
    expect(onPaneResize).toHaveBeenLastCalledWith('sidebar', null);
    const a = sep(labels.aside);
    key(a, 'Home');
    expect(onPaneResize).toHaveBeenLastCalledWith('aside', 224);
    key(a, 'End');
    expect(onPaneResize).toHaveBeenLastCalledWith('aside', 448);
  });

  it('a key stops at the limit that keeps main at 24rem', () => {
    // 64rem is the narrowest shell with an aside column: 1024 - 60 - 224 - 384 = 356.
    shellW = 1024; stubWidths();
    const onPaneResize = vi.fn();
    render(<ComposedShell shell={shellProps({ onPaneResize, paneWidths: { sidebar: 240, aside: 224 } })} />);
    const s = sep(labels.sidebar);
    expect(s).toHaveAttribute('aria-valuemax', '356');
    key(s, 'End');
    expect(onPaneResize).toHaveBeenLastCalledWith('sidebar', 356);
  });

  const pointer = (el: HTMLElement, type: string, clientX: number, button = 0) => {
    const e = new MouseEvent(type, { bubbles: true, cancelable: true, clientX, button });
    Object.assign(e, { pointerId: 7 });
    act(() => { el.dispatchEvent(e); });
  };

  it('a pointer drag moves the property live, sets data-resizing, and commits once on pointerup', () => {
    shellW = 1280; stubWidths();
    const onPaneResize = vi.fn();
    const { container } = render(<ComposedShell shell={shellProps({ onPaneResize })} />);
    const s = sep(labels.sidebar);
    const capture = vi.fn();
    Object.assign(s, { setPointerCapture: capture, releasePointerCapture: vi.fn(), hasPointerCapture: () => true });
    const root = container.querySelector('.d-app-shell') as HTMLElement;
    pointer(s, 'pointerdown', 300);
    expect(capture).toHaveBeenCalledWith(7);
    pointer(s, 'pointermove', 340);
    expect(root).toHaveAttribute('data-resizing');
    expect(root).toHaveStyle({ '--d-sidebar-w': '280px' });
    expect(onPaneResize).not.toHaveBeenCalled();
    pointer(s, 'pointermove', 360);
    pointer(s, 'pointerup', 360);
    pointer(s, 'lostpointercapture', 360);
    expect(onPaneResize).toHaveBeenCalledTimes(1);
    expect(onPaneResize).toHaveBeenCalledWith('sidebar', 300);
    expect(root).not.toHaveAttribute('data-resizing');
  });

  it('the aside drags the other way and a drag past the limit stops there', () => {
    shellW = 1280; stubWidths();
    const onPaneResize = vi.fn();
    render(<ComposedShell shell={shellProps({ onPaneResize })} />);
    const a = sep(labels.aside);
    Object.assign(a, { setPointerCapture: vi.fn() });
    pointer(a, 'pointerdown', 900);
    pointer(a, 'pointermove', 1500);
    pointer(a, 'pointerup', 1500);
    expect(onPaneResize).toHaveBeenCalledWith('aside', 224);
  });

  it('a secondary button does not start a drag', () => {
    shellW = 1280; stubWidths();
    const onPaneResize = vi.fn();
    const { container } = render(<ComposedShell shell={shellProps({ onPaneResize })} />);
    const s = sep(labels.sidebar);
    const capture = vi.fn();
    Object.assign(s, { setPointerCapture: capture });
    pointer(s, 'pointerdown', 300, 2);
    pointer(s, 'pointermove', 340);
    pointer(s, 'pointerup', 340);
    expect(capture).not.toHaveBeenCalled();
    expect(onPaneResize).not.toHaveBeenCalled();
    expect(container.querySelector('.d-app-shell')).not.toHaveAttribute('data-resizing');
  });

  it('a double-click resets', () => {
    shellW = 1280; stubWidths();
    const onPaneResize = vi.fn();
    render(<ComposedShell shell={shellProps({ onPaneResize })} />);
    fireEvent.doubleClick(sep(labels.sidebar));
    expect(onPaneResize).toHaveBeenCalledWith('sidebar', null);
    fireEvent.doubleClick(sep(labels.aside));
    expect(onPaneResize).toHaveBeenLastCalledWith('aside', null);
  });

  it('a reset shows the default width again', () => {
    shellW = 1280; stubWidths();
    const { container } = render(<ComposedShell shell={shellProps({ paneWidths: {} })} />);
    // --sidebar-w 244 (README §5.1) and --members-w 232.
    expect(sep(labels.sidebar)).toHaveAttribute('aria-valuenow', '244');
    expect(sep(labels.aside)).toHaveAttribute('aria-valuenow', '232');
    expect(container.querySelector('.d-app-shell')?.getAttribute('style') ?? '').not.toContain('--d-sidebar-w');
  });

  it('a closed column aside with content stays mounted closed: data-open false, inert, no splitter', () => {
    shellW = 1280; stubWidths();
    const { container, rerender } = render(<ComposedShell shell={shellProps()} />);
    const el = container.querySelector('.d-app-shell__aside')!;
    expect(el).toHaveAttribute('data-open', 'true');
    expect(el).not.toHaveAttribute('inert');
    rerender(<ComposedShell shell={shellProps({ asideOpen: false })} />);
    const closed = container.querySelector('.d-app-shell__aside')!;
    expect(closed).toBe(el);
    expect(closed).toHaveAttribute('data-open', 'false');
    expect(closed).toHaveAttribute('inert');
    expect(screen.queryByRole('separator', { name: labels.aside })).toBeNull();
  });

  it('when the content goes with the close, the last content stays until the transition ends, then unmounts', () => {
    vi.useFakeTimers();
    try {
      shellW = 1280; stubWidths();
      const { container, rerender } = render(<ComposedShell shell={shellProps()} />);
      const open = container.querySelector('.d-app-shell__aside');
      expect(open).toHaveAttribute('data-open', 'true');
      rerender(<ComposedShell shell={shellProps({ asideOpen: false, aside: undefined })} />);
      const closing = container.querySelector('.d-app-shell__aside');
      // The same element, so its width transitions (a new one would start at 0).
      expect(closing).toBe(open);
      expect(closing).toHaveAttribute('data-open', 'false');
      expect(closing).toHaveTextContent('pane control');
      // A quick reopen before the transition ends reuses it too.
      rerender(<ComposedShell shell={shellProps()} />);
      expect(container.querySelector('.d-app-shell__aside')).toBe(open);
      expect(open).toHaveAttribute('data-open', 'true');
      rerender(<ComposedShell shell={shellProps({ asideOpen: false, aside: undefined })} />);
      expect(container.querySelector('.d-app-shell__aside')).toBe(open);
      act(() => { closing!.dispatchEvent(Object.assign(new Event('transitionend'), { propertyName: 'width' })); });
      expect(container.querySelector('.d-app-shell__aside')).toBeNull();
      // Reduced motion fires no transitionend: a fallback timer unmounts it too.
      rerender(<ComposedShell shell={shellProps()} />);
      rerender(<ComposedShell shell={shellProps({ asideOpen: false, aside: undefined })} />);
      expect(container.querySelector('.d-app-shell__aside')).not.toBeNull();
      act(() => { vi.advanceTimersByTime(600); });
      expect(container.querySelector('.d-app-shell__aside')).toBeNull();
    } finally { vi.useRealTimers(); }
  });

  it('a thread aside defaults to --thread-w (340) and marks its column', () => {
    shellW = 1280; stubWidths();
    const { container } = render(<ComposedShell shell={shellProps({ paneWidths: {}, asideKind: 'thread' })} />);
    expect(sep(labels.aside)).toHaveAttribute('aria-valuenow', '340');
    expect(container.querySelector('.d-app-shell__grid')).toHaveAttribute('data-aside', 'thread');
  });

  it('a shrink only clamps what is shown: the stored width is never reported, and a wider window restores it', () => {
    shellW = 1280;
    let notify: () => void = () => {};
    stubWidths();
    vi.stubGlobal('ResizeObserver', class {
      constructor(cb: () => void) { notify = cb; }
      observe() {}
      unobserve() {}
      disconnect() {}
    });
    const onPaneResize = vi.fn();
    const { container } = render(<ComposedShell shell={shellProps({ onPaneResize, paneWidths: { sidebar: 360, aside: 224 } })} />);
    const root = container.querySelector('.d-app-shell');
    expect(root).toHaveStyle({ '--d-sidebar-w': '360px' });
    shellW = 1024;
    act(() => notify());
    expect(root).toHaveStyle({ '--d-sidebar-w': '356px' });
    shellW = 1280;
    act(() => notify());
    expect(root).toHaveStyle({ '--d-sidebar-w': '360px' });
    expect(onPaneResize).not.toHaveBeenCalled();
  });

  it('after the layout flips desktop -> phone -> desktop the new root is measured and observed, so the splitter range is the desktop one again (FX-misc)', () => {
    shellW = 1280;
    const observed: Element[] = [];
    let notify: () => void = () => {};
    stubWidths();
    vi.stubGlobal('ResizeObserver', class {
      constructor(cb: () => void) { notify = cb; }
      observe(el: Element) { observed.push(el); }
      unobserve() {}
      disconnect() {}
    });
    const props = shellProps({ asideOpen: false });
    const { container, rerender } = render(<ComposedShell shell={props} />);
    expect(sep(labels.sidebar)).toHaveAttribute('aria-valuemax', '384');
    // The window becomes a phone: another root element, the old observer fires for the shell it still watches.
    shellW = 390;
    rerender(<ComposedShell shell={{ ...props, layout: 'phone' }} />);
    act(() => notify());
    expect(screen.queryAllByRole('separator')).toHaveLength(0);
    // And back: the desktop root is a new element, which a stale observer never sees.
    shellW = 1280;
    rerender(<ComposedShell shell={props} />);
    const root = container.querySelector('.d-app-shell');
    expect(observed).toContain(root);
    expect(sep(labels.sidebar)).toHaveAttribute('aria-valuemin', '192');
    expect(sep(labels.sidebar)).toHaveAttribute('aria-valuemax', '384');
    expect(root).toHaveStyle({ '--d-sidebar-w': '240px' });
  });

  it('sets data-main-narrow while main is under 28rem (the channel header then wraps), and not otherwise, never on a phone', () => {
    const root = (w: number, over: Record<string, unknown> = {}) => {
      shellW = w; stubWidths();
      const view = render(<ComposedShell shell={shellProps({ asideOpen: false, ...over })} />);
      const el = view.container.querySelector('.d-app-shell')!;
      const narrow = el.hasAttribute('data-main-narrow');
      view.unmount();
      return narrow;
    };
    // main = shell - rail (60) - sidebar (the default 240 here); 28rem is 448.
    expect(root(1280)).toBe(false);
    expect(root(748)).toBe(false);      // 448: not under
    expect(root(747)).toBe(true);       // 447
    expect(root(700, { paneWidths: { sidebar: 384, aside: 240 } })).toBe(true);   // 256 clamps to the 24rem floor, 384 < 448
    expect(root(700, { layout: 'phone' })).toBe(false);
  });

  it('opening the aside column clamps what is shown and reports nothing', () => {
    shellW = 1024; stubWidths();
    const onPaneResize = vi.fn();
    const props = shellProps({ onPaneResize, paneWidths: { sidebar: 360, aside: 224 } });
    const { container, rerender } = render(<ComposedShell shell={{ ...props, asideOpen: false }} />);
    const root = container.querySelector('.d-app-shell');
    expect(root).toHaveStyle({ '--d-sidebar-w': '360px' });
    rerender(<ComposedShell shell={props} />);
    expect(root).toHaveStyle({ '--d-sidebar-w': '356px' });
    expect(onPaneResize).not.toHaveBeenCalled();
  });
});
