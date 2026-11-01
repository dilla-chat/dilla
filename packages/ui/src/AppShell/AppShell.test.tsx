import { act, fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, it, expect, vi } from 'vitest';
import { ComposedShell, fixtureTopBar } from './AppShell.fixture.tsx';
import { AppShell } from './AppShell.tsx';
import { CommunityRail } from '../CommunityRail/CommunityRail.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

describe('AppShell', () => {
  it('follows the focus order of the keyboard map', async () => {
    const user = userEvent.setup();
    const { container } = render(<ComposedShell />);
    expect(container.firstElementChild).toHaveClass('d-app-shell');
    await user.tab();
    expect(screen.getByRole('link', { name: 'skip to messages' })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole('button', { name: 'Midgard Crew' })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole('button', { name: 'general' })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole('log', { name: 'messages in #general' })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole('textbox', { name: 'message #general' })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole('button', { name: 'send' })).toHaveFocus();
  });

  // Signal task 5: the top bar spans the desktop shell above the rail and main; without one there is no wrapper (and no row).
  it('desktop renders topBar in .d-app-shell__topbar above the rail and main, first after the skip link', async () => {
    const user = userEvent.setup();
    const { container, rerender } = render(<ComposedShell />);
    expect(container.querySelector('.d-app-shell__topbar')).toBeNull();
    rerender(<ComposedShell shell={{ topBar: null }} />);
    expect(container.querySelector('.d-app-shell__topbar')).toBeNull();
    rerender(<ComposedShell shell={{ topBar: fixtureTopBar }} />);
    const bar = container.querySelector('.d-app-shell__topbar')!;
    expect(bar).toContainElement(container.querySelector('header.d-topbar'));
    const rail = container.querySelector('.d-app-shell__rail')!;
    expect(bar.compareDocumentPosition(rail) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(bar.compareDocumentPosition(screen.getByRole('main')) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    await user.tab();
    expect(screen.getByRole('link', { name: 'skip to messages' })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole('button', { name: 'Search or jump to…' })).toHaveFocus();
  });
  it('a phone shell renders no top bar', () => {
    const { container } = render(<AppShell layout="phone" skipLabel="skip" topBar={fixtureTopBar} rail={null} sidebar={<p>list</p>} header={null} composer={null}>main</AppShell>);
    expect(container.querySelector('.d-app-shell__topbar')).toBeNull();
    expect(container.querySelector('.d-topbar')).toBeNull();
  });

  // web3.8 (U3, R23): the bottom-left area (the voice dock over the user panel), after the sidebar and before main.
  it('desktop renders bottom in .d-app-shell__panel, after the sidebar and before main in DOM order', async () => {
    const user = userEvent.setup();
    const { container, rerender } = render(<ComposedShell />);
    expect(container.querySelector('.d-app-shell__panel')).toBeNull();
    rerender(<ComposedShell shell={{ bottom: <button type="button">panel control</button> }} />);
    const panel = container.querySelector('.d-app-shell__panel')!;
    expect(panel).toContainElement(screen.getByRole('button', { name: 'panel control' }));
    const sidebar = container.querySelector('.d-app-shell__sidebar')!;
    const main = screen.getByRole('main');
    expect(sidebar.compareDocumentPosition(panel) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(panel.compareDocumentPosition(main) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    screen.getByRole('button', { name: 'general' }).focus();
    await user.tab();
    expect(screen.getByRole('button', { name: 'panel control' })).toHaveFocus();
    await user.tab();
    expect(main.contains(document.activeElement)).toBe(true);
    for (const empty of [null, false, undefined]) {
      rerender(<ComposedShell shell={{ bottom: empty }} />);
      expect(container.querySelector('.d-app-shell__panel')).toBeNull();
    }
  });

  it('moves focus to main from the skip link without changing the URL', async () => {
    const user = userEvent.setup();
    render(<ComposedShell />);
    const before = window.location.href;
    await user.tab();
    await user.keyboard('{Enter}');
    expect(screen.getByRole('main')).toHaveFocus();
    expect(window.location.href).toBe(before);
  });

  it('holds the header, the log and the composer in main', () => {
    render(<ComposedShell />);
    const main = screen.getByRole('main');
    expect(within(main).getByRole('heading', { level: 2, name: 'general' })).toBeInTheDocument();
    expect(within(main).getByRole('log')).toBeInTheDocument();
    expect(within(main).getByRole('textbox', { name: 'message #general' })).toBeInTheDocument();
    expect(within(main).queryByRole('navigation')).toBeNull();
  });

  it('puts the banner above the rail in reading and focus order, only when given', async () => {
    const user = userEvent.setup();
    const { container, rerender } = render(<ComposedShell />);
    expect(container.querySelector('.d-app-shell__banner')).toBeNull();
    rerender(<ComposedShell banner />);
    const banner = container.querySelector('.d-app-shell__banner');
    const rail = container.querySelector('.d-app-shell__rail');
    expect(banner).not.toBeNull();
    expect(banner?.compareDocumentPosition(rail as Node)).toBe(Node.DOCUMENT_POSITION_FOLLOWING);
    await user.tab();
    await user.tab();
    expect(screen.getByRole('button', { name: 'retry now' })).toHaveFocus();
  });

  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><ComposedShell banner /></div>);
    await expectNoAxeViolations(container);
  });

  // web-3 (flow 06, task-9 ruling): the incoming-ring banner is the first thing in main; on a phone it is fixed to the top
  // of the shell and the grid moves down by its height, so it never covers the stage controls.
  it('puts the ring slot first in main, measures it and gives nothing without a ring', () => {
    const { rerender } = render(<ComposedShell ring={<section aria-label="incoming calls"><button type="button">answer</button></section>} />);
    const main = screen.getByRole('main');
    const slot = main.firstElementChild as HTMLElement;
    expect(slot).toHaveClass('d-app-shell__ring');
    expect(within(slot).getByRole('button', { name: 'answer' })).toBeInTheDocument();
    expect(within(main).getAllByRole('button')[0]).toHaveAccessibleName('answer');
    rerender(<ComposedShell />);
    expect(document.querySelector('.d-app-shell__ring')).toBeNull();
    expect(document.querySelector<HTMLElement>('.d-app-shell')!.style.getPropertyValue('--d-ring-h')).toBe('0px');
  });
});

// web3.4 (U6): the aside slot. Below 64rem of shell width it is an overlay drawer; at or above, a fourth grid column.
describe('AppShell aside', () => {
  const aside = <div><button type="button">pane control</button></div>;
  const open = (over: Record<string, unknown> = {}) =>
    ({ aside, asideLabel: 'members', asideOpen: true, onAsideClose: vi.fn(), ...over });

  it('without an aside the shell is unchanged: no aside landmark and no aside grid class', () => {
    const { container } = render(<ComposedShell />);
    expect(screen.queryByRole('complementary')).toBeNull();
    expect(container.querySelector('.d-app-shell__grid')?.className).toBe('d-app-shell__grid');
  });

  it('open: an aside landmark after main', () => {
    const { container } = render(<ComposedShell shell={open()} />);
    const side = screen.getByRole('complementary', { name: 'members' });
    const main = screen.getByRole('main');
    expect(main.compareDocumentPosition(side) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(container.querySelector('.d-app-shell__status')).toBeNull();
    expect(container.querySelector('.d-app-shell__grid')).toHaveClass('d-app-shell__grid--aside');
  });

  it('closed: absent, with the content not rendered either', () => {
    render(<ComposedShell shell={open({ asideOpen: false })} />);
    expect(screen.queryByRole('complementary')).toBeNull();
    expect(screen.queryByRole('button', { name: 'pane control' })).toBeNull();
  });

  it('the focus order runs main, then aside', async () => {
    const user = userEvent.setup();
    const width = vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockReturnValue(1280);
    render(<ComposedShell shell={open()} />);
    screen.getByRole('button', { name: 'send' }).focus();
    await user.tab();
    expect(screen.getByRole('button', { name: 'pane control' })).toHaveFocus();
    width.mockRestore();
  });

  it('as an overlay, focus moves into the aside on open, Escape calls onAsideClose and focus goes back to the opener', async () => {
    const user = userEvent.setup();
    const onAsideClose = vi.fn();
    const { rerender } = render(<ComposedShell shell={open({ asideOpen: false, onAsideClose })} />);
    const opener = screen.getByRole('button', { name: 'send' });
    opener.focus();
    rerender(<ComposedShell shell={open({ onAsideClose })} />);
    expect(screen.getByRole('button', { name: 'pane control' })).toHaveFocus();
    await user.keyboard('{Escape}');
    expect(onAsideClose).toHaveBeenCalledTimes(1);
    rerender(<ComposedShell shell={open({ asideOpen: false, onAsideClose })} />);
    expect(opener).toHaveFocus();
  });

  it('as a column (64rem or wider) Escape does not close it and opening does not take focus', async () => {
    const user = userEvent.setup();
    const onAsideClose = vi.fn();
    const width = vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockReturnValue(1280);
    const { rerender } = render(<ComposedShell shell={open({ asideOpen: false, onAsideClose })} />);
    const opener = screen.getByRole('button', { name: 'send' });
    opener.focus();
    rerender(<ComposedShell shell={open({ onAsideClose })} />);
    expect(opener).toHaveFocus();
    screen.getByRole('button', { name: 'pane control' }).focus();
    await user.keyboard('{Escape}');
    expect(onAsideClose).not.toHaveBeenCalled();
    width.mockRestore();
  });

  // Fix round 1: the overlay drawer is modal.
  const blockers = (c: HTMLElement) =>
    ['.d-app-shell__rail', '.d-app-shell__sidebar', '.d-app-shell__main'].map(s => c.querySelector(s) as HTMLElement);

  it('as an overlay the rest of the shell is inert and a backdrop sits behind the drawer; closed, neither', () => {
    const { container, rerender } = render(<ComposedShell shell={open()} />);
    for (const el of blockers(container)) expect(el).toHaveAttribute('inert');
    expect(container.querySelector('.d-app-shell__backdrop')).not.toBeNull();
    rerender(<ComposedShell shell={open({ asideOpen: false })} />);
    for (const el of blockers(container)) expect(el).not.toHaveAttribute('inert');
    expect(container.querySelector('.d-app-shell__backdrop')).toBeNull();
  });

  it('as a column nothing is inert and there is no backdrop', () => {
    const width = vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockReturnValue(1280);
    const { container } = render(<ComposedShell shell={open()} />);
    for (const el of blockers(container)) expect(el).not.toHaveAttribute('inert');
    expect(container.querySelector('.d-app-shell__backdrop')).toBeNull();
    width.mockRestore();
  });

  it('a click on the backdrop calls onAsideClose', async () => {
    const user = userEvent.setup();
    const onAsideClose = vi.fn();
    const { container } = render(<ComposedShell shell={open({ onAsideClose })} />);
    await user.click(container.querySelector('.d-app-shell__backdrop') as HTMLElement);
    expect(onAsideClose).toHaveBeenCalledTimes(1);
  });

  it('Escape anywhere in the document closes the overlay', () => {
    const onAsideClose = vi.fn();
    render(<ComposedShell shell={open({ onAsideClose })} />);
    fireEvent.keyDown(document.body, { key: 'Escape' });
    expect(onAsideClose).toHaveBeenCalledTimes(1);
  });

  it('an Escape the aside already handled (defaultPrevented) does not close the drawer', () => {
    const onAsideClose = vi.fn();
    const handled = <button type="button" onKeyDown={e => { if (e.key === 'Escape') e.preventDefault(); }}>menu</button>;
    render(<ComposedShell shell={open({ onAsideClose, aside: handled })} />);
    fireEvent.keyDown(screen.getByRole('button', { name: 'menu' }), { key: 'Escape' });
    expect(onAsideClose).not.toHaveBeenCalled();
  });

  it('when the opener was body, focus goes back to the toggle given by asideReturnRef', () => {
    const toggle = document.createElement('button');
    document.body.appendChild(toggle);
    const ref = { current: toggle };
    const { rerender } = render(<ComposedShell shell={open({ asideOpen: false, asideReturnRef: ref })} />);
    (document.activeElement as HTMLElement | null)?.blur();
    rerender(<ComposedShell shell={open({ asideReturnRef: ref })} />);
    rerender(<ComposedShell shell={open({ asideOpen: false, asideReturnRef: ref })} />);
    expect(toggle).toHaveFocus();
    toggle.remove();
  });

  it('follows the live width: crossing 64rem while open adds or removes inert and the backdrop', () => {
    let width = 1280;
    let notify: () => void = () => {};
    vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockImplementation(() => width);
    vi.stubGlobal('ResizeObserver', class {
      constructor(cb: () => void) { notify = cb; }
      observe() {}
      unobserve() {}
      disconnect() {}
    });
    const { container } = render(<ComposedShell shell={open()} />);
    expect(container.querySelector('.d-app-shell__main')).not.toHaveAttribute('inert');
    width = 500;
    act(() => notify());
    expect(container.querySelector('.d-app-shell__main')).toHaveAttribute('inert');
    expect(container.querySelector('.d-app-shell__backdrop')).not.toBeNull();
    width = 1300;
    act(() => notify());
    expect(container.querySelector('.d-app-shell__main')).not.toHaveAttribute('inert');
    expect(container.querySelector('.d-app-shell__backdrop')).toBeNull();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('when the shell becomes an overlay while open, focus in the now-inert content moves into the drawer; back to a column it stays', () => {
    let width = 1280;
    let notify: () => void = () => {};
    vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockImplementation(() => width);
    vi.stubGlobal('ResizeObserver', class {
      constructor(cb: () => void) { notify = cb; }
      observe() {}
      unobserve() {}
      disconnect() {}
    });
    render(<ComposedShell shell={open()} />);
    const send = screen.getByRole('button', { name: 'send' });
    send.focus();
    expect(send).toHaveFocus();
    width = 500;
    act(() => notify());
    expect(screen.getByRole('button', { name: 'pane control' })).toHaveFocus();
    width = 1300;
    act(() => notify());
    expect(screen.getByRole('button', { name: 'pane control' })).toHaveFocus();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  // Final fix I2: the drawer has a visible close control at its top; a column needs none.
  it('as an overlay with asideCloseLabel, a close button sits first in the drawer and calls onAsideClose', async () => {
    const user = userEvent.setup();
    const onAsideClose = vi.fn();
    render(<ComposedShell shell={open({ onAsideClose, asideCloseLabel: 'close members' })} />);
    const side = screen.getByRole('complementary', { name: 'members' });
    const close = within(side).getByRole('button', { name: 'close members' });
    expect(within(side).getAllByRole('button')[0]).toBe(close);
    await user.click(close);
    expect(onAsideClose).toHaveBeenCalledTimes(1);
  });

  it('as a column there is no close button, also with asideCloseLabel', () => {
    const width = vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockReturnValue(1280);
    render(<ComposedShell shell={open({ asideCloseLabel: 'close members' })} />);
    const side = screen.getByRole('complementary', { name: 'members' });
    expect(within(side).queryByRole('button', { name: 'close members' })).toBeNull();
    expect(within(side).getByRole('button', { name: 'pane control' })).toBeInTheDocument();
    width.mockRestore();
  });

  it('has no serious axe violations with the aside open', async () => {
    const { container } = render(<div className="d-root"><ComposedShell shell={open()} /></div>);
    await expectNoAxeViolations(container);
  });
});

// web3.7 (W-Phone-2): the phone layout. Desktop (no `layout`) renders today's DOM.
describe('phone layout', () => {
  const tabs = <nav aria-label="main" data-testid="tab-bar"><a href="/s">servers</a></nav>;
  const calls = <div role="group" aria-label="call" data-testid="call-bar"><button type="button">open call</button></div>;
  const phone = (over: Record<string, unknown> = {}) => ({ layout: 'phone', tabBar: tabs, callBar: calls, ...over }) as const;

  it('list pane: tab bar, rail and sidebar render, main is hidden, no status bar, the call bar renders', () => {
    const { container } = render(<ComposedShell shell={phone({ phonePane: 'list' })} />);
    expect(screen.getByTestId('tab-bar')).toBeInTheDocument();
    expect(container.querySelector('.d-app-shell__rail')).not.toBeNull();
    expect(container.querySelector('.d-app-shell__sidebar')).not.toBeNull();
    const main = container.querySelector('main') as HTMLElement;
    expect(main).toHaveAttribute('hidden');
    expect(container.querySelector('.d-app-shell__status')).toBeNull();
    expect(screen.getByTestId('call-bar')).toBeInTheDocument();
    expect(container.firstElementChild).toHaveAttribute('data-layout', 'phone');
    expect(container.firstElementChild).toHaveAttribute('data-pane', 'list');
  });

  it('defaults to the list pane', () => {
    const { container } = render(<ComposedShell shell={phone()} />);
    expect(container.firstElementChild).toHaveAttribute('data-pane', 'list');
  });

  it('list pane: the skip link targets the sidebar and focuses it', async () => {
    const user = userEvent.setup();
    const { container } = render(<ComposedShell shell={phone()} />);
    const skip = screen.getByRole('link', { name: 'skip to messages' });
    const sidebar = container.querySelector('.d-app-shell__sidebar') as HTMLElement;
    expect(skip.getAttribute('href')).toBe(`#${sidebar.id}`);
    expect(sidebar.id.endsWith('-list')).toBe(true);
    expect(sidebar).toHaveAttribute('tabindex', '-1');
    expect(container.querySelector('main')?.id).toBe(sidebar.id.replace(/-list$/, ''));
    await user.click(skip);
    expect(sidebar).toHaveFocus();
  });

  it('view pane: main shows with header, content and composer; the skip link targets main; no rail or sidebar; the call bar and the tab bar stay', async () => {
    const user = userEvent.setup();
    const { container } = render(<ComposedShell shell={phone({ phonePane: 'view' })} />);
    const main = screen.getByRole('main');
    expect(main).not.toHaveAttribute('hidden');
    expect(within(main).getByRole('heading', { level: 2, name: 'general' })).toBeInTheDocument();
    expect(within(main).getByRole('log')).toBeInTheDocument();
    expect(within(main).getByRole('textbox', { name: 'message #general' })).toBeInTheDocument();
    expect(container.querySelector('.d-app-shell__rail')).toBeNull();
    expect(container.querySelector('.d-app-shell__sidebar')).toBeNull();
    expect(screen.getByTestId('tab-bar')).toBeInTheDocument();
    expect(screen.getByTestId('call-bar')).toBeInTheDocument();
    expect(container.firstElementChild).toHaveAttribute('data-pane', 'view');
    const skip = screen.getByRole('link', { name: 'skip to messages' });
    expect(skip.getAttribute('href')).toBe(`#${main.id}`);
    await user.click(skip);
    expect(main).toHaveFocus();
  });

  it('with no rail (the DMs, activity and you tabs) no rail element renders and the root says so', () => {
    const { container } = render(
      <AppShell layout="phone" skipLabel="skip" rail={null} sidebar={<p>dms</p>} header={<h2>x</h2>} composer={null}>
        <p>content</p>
      </AppShell>,
    );
    expect(container.querySelector('.d-app-shell__rail')).toBeNull();
    expect(container.firstElementChild).toHaveAttribute('data-rail', 'none');
    expect(container.querySelector('.d-app-shell__sidebar')).not.toBeNull();
  });

  it('with a rail the root carries no data-rail', () => {
    const { container } = render(<ComposedShell shell={phone()} />);
    expect(container.firstElementChild).not.toHaveAttribute('data-rail');
  });

  it('keeps the DOM order skip link, banner, rail, sidebar, main, call bar, tab bar', () => {
    const { container } = render(<ComposedShell banner shell={phone()} />);
    const order = ['.d-app-shell__skip', '.d-app-shell__banner', '.d-app-shell__rail', '.d-app-shell__sidebar', 'main', '.d-app-shell__callbar', '.d-app-shell__tabbar']
      .map(s => container.querySelector(s) as Node);
    order.forEach(n => expect(n).not.toBeNull());
    for (let i = 1; i < order.length; i += 1) expect(order[i - 1].compareDocumentPosition(order[i]) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('the aside is still the overlay drawer on a phone', () => {
    const { container } = render(<ComposedShell shell={phone({ phonePane: 'view', aside: <button type="button">pane</button>, asideLabel: 'members', asideOpen: true, onAsideClose: vi.fn() })} />);
    expect(screen.getByRole('complementary', { name: 'members' })).toBeInTheDocument();
    expect(container.querySelector('.d-app-shell__backdrop')).not.toBeNull();
    // Fix round 2: the full-screen sheet covers the tab bar (Task 11, Q4), so the bar is inert under it.
    expect(container.querySelector('.d-app-shell__tabbar')).toHaveAttribute('inert');
  });

  it('the phone drops the rail settings tile; the desktop keeps it', () => {
    const railWith = <CommunityRail label="servers" activeId="a" onSelect={() => {}} addLabel="add a server" onAdd={() => {}} settingsLabel="settings" onSettings={() => {}} items={[{ id: 'a', name: 'Alpha' }]} />;
    const shell = (layout: 'phone' | 'desktop') => (
      <AppShell layout={layout} skipLabel="skip" rail={railWith} sidebar={<p>s</p>} header={<h2>x</h2>} composer={null}><p>c</p></AppShell>
    );
    const phoneView = render(shell('phone'));
    expect(screen.getByRole('button', { name: 'add a server' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'settings' })).toBeNull();
    phoneView.unmount();
    render(shell('desktop'));
    expect(screen.getByRole('button', { name: 'settings' })).toBeInTheDocument();
  });

  it('the members sheet on a phone has a phone header: back first, the aside label as its title, and back closes it', async () => {
    const user = userEvent.setup();
    const onAsideClose = vi.fn();
    render(<ComposedShell shell={phone({ phonePane: 'view', aside: <button type="button">pane</button>, asideLabel: 'members', asideOpen: true, onAsideClose, asideCloseLabel: 'back' })} />);
    const side = screen.getByRole('complementary', { name: 'members' });
    expect(side.querySelector('.d-phone-header')).not.toBeNull();
    expect(within(side).getByRole('heading', { level: 2, name: 'members' })).toBeInTheDocument();
    const back = within(side).getByRole('button', { name: 'back' });
    expect(back.querySelector('[data-icon="chevron-left"]')).not.toBeNull();
    expect(within(side).getAllByRole('button')[0]).toBe(back);
    expect(back).toHaveFocus();
    await user.click(back);
    expect(onAsideClose).toHaveBeenCalledTimes(1);
    fireEvent.keyDown(document.body, { key: 'Escape' });
    expect(onAsideClose).toHaveBeenCalledTimes(2);
  });

  it('the list pane main is out of the accessibility tree, and in it on the view pane', () => {
    const list = render(<ComposedShell shell={phone()} />);
    expect(screen.queryByRole('main')).toBeNull();
    expect(screen.getByRole('main', { hidden: true })).toHaveAttribute('hidden');
    list.unmount();
    render(<ComposedShell shell={phone({ phonePane: 'view' })} />);
    expect(screen.getByRole('main')).toBeInTheDocument();
  });

  it.each(['list', 'view'] as const)('the ring renders outside main on the %s pane, before the banner and rail', pane => {
    const ring = <section aria-label="incoming calls"><button type="button">answer</button></section>;
    const { container } = render(<ComposedShell ring={ring} shell={phone({ phonePane: pane })} />);
    const slot = container.querySelector('.d-app-shell__ring') as HTMLElement;
    expect(slot).not.toBeNull();
    expect(slot.closest('main')).toBeNull();
    expect(screen.getByRole('button', { name: 'answer' })).toBeVisible();
    expect(container.querySelector('main')?.querySelector('.d-app-shell__ring')).toBeNull();
    const next = container.querySelector(pane === 'list' ? '.d-app-shell__rail' : 'main') as Node;
    expect(slot.compareDocumentPosition(next) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it.each(['view', 'list'] as const)('the ring stays out of the inert scope while a sheet is open on the %s pane, so it can be answered', pane => {
    const ring = <section aria-label="incoming calls"><button type="button">answer</button></section>;
    const { container } = render(<ComposedShell ring={ring} shell={phone({ phonePane: pane, aside: <button type="button">pane</button>, asideLabel: 'threads', asideOpen: true, onAsideClose: vi.fn(), asideCloseLabel: 'back' })} />);
    expect(container.querySelector('.d-app-shell__backdrop')).not.toBeNull();
    expect(screen.getByRole('button', { name: 'answer' }).closest('[inert]')).toBeNull();
    expect(container.querySelector('main')).toHaveAttribute('inert');
  });

  it('desktop unchanged: no layout prop renders the same markup as layout="desktop", with no phone attributes', () => {
    // useId differs between renders; the markup is compared with the ids made alike.
    const markup = (el: HTMLElement) => el.innerHTML.replace(/_r_[0-9a-z]+_/g, '_id_');
    const a = render(<ComposedShell />);
    const html = markup(a.container);
    a.unmount();
    const b = render(<ComposedShell shell={{ layout: 'desktop' }} />);
    expect(markup(b.container)).toBe(html);
    expect(html).not.toContain('data-layout');
    expect(html).not.toContain('d-app-shell__tabbar');
  });

  it('view pane: the tab bar follows the call bar', () => {
    render(<ComposedShell shell={phone({ phonePane: 'view' })} />);
    expect(screen.getByTestId('call-bar').parentElement!.compareDocumentPosition(screen.getByTestId('tab-bar').parentElement!)).toBe(Node.DOCUMENT_POSITION_FOLLOWING);
  });

  it('typing: onTypingChange hears false first, then true on focus and false on blur', () => {
    const heard: boolean[] = [];
    render(<ComposedShell shell={phone({ phonePane: 'view', onTypingChange: (v: boolean) => heard.push(v) })} />);
    expect(heard.at(-1)).toBe(false);
    act(() => { screen.getByRole('textbox', { name: 'message #general' }).focus(); });
    expect(heard.at(-1)).toBe(true);
    act(() => { screen.getByRole('textbox', { name: 'message #general' }).blur(); });
    expect(heard.at(-1)).toBe(false);
  });

  it('typing: focusing a textarea marks the shell and hides the tab bar; blur brings both back', () => {
    const { container } = render(<ComposedShell shell={phone({ phonePane: 'view' })} />);
    const wrapper = screen.getByTestId('tab-bar').parentElement!;
    expect(container.firstElementChild).not.toHaveAttribute('data-typing');
    expect(wrapper).not.toHaveAttribute('hidden');
    act(() => { screen.getByRole('textbox', { name: 'message #general' }).focus(); });
    expect(container.firstElementChild).toHaveAttribute('data-typing', 'true');
    expect(wrapper).toHaveAttribute('hidden');
    expect(screen.queryByRole('navigation', { name: 'main' })).toBeNull();
    expect(screen.getByTestId('call-bar')).toBeInTheDocument();
    act(() => { screen.getByRole('textbox', { name: 'message #general' }).blur(); });
    expect(container.firstElementChild).not.toHaveAttribute('data-typing');
    expect(wrapper).not.toHaveAttribute('hidden');
  });

  it('typing: a field outside the shell (a dialog, a settings page) hides the tab bar too', () => {
    const { container } = render(<><ComposedShell shell={phone({ phonePane: 'view' })} /><textarea aria-label="elsewhere" /></>);
    const shell = container.firstElementChild as HTMLElement;
    act(() => { screen.getByRole('textbox', { name: 'elsewhere' }).focus(); });
    expect(shell).toHaveAttribute('data-typing', 'true');
    expect(screen.queryByRole('navigation', { name: 'main' })).toBeNull();
    act(() => { screen.getByRole('textbox', { name: 'elsewhere' }).blur(); });
    expect(shell).not.toHaveAttribute('data-typing');
  });

  it('typing: a focused field removed from the page with no focusout (WebKit) brings the tab bar back', async () => {
    const holder = document.createElement('div');
    const field = document.createElement('input');
    holder.append(field);
    const { container } = render(<ComposedShell shell={phone({ phonePane: 'view' })} />);
    document.body.append(holder);
    const shell = container.firstElementChild as HTMLElement;
    act(() => { field.focus(); });
    expect(shell).toHaveAttribute('data-typing', 'true');
    // WebKit fires no focusout when the focused element leaves the DOM; drop the event to stand in for it.
    const swallow = (e: Event) => e.stopImmediatePropagation();
    window.addEventListener('focusout', swallow, true);
    await act(async () => { holder.remove(); await Promise.resolve(); });
    window.removeEventListener('focusout', swallow, true);
    expect(shell).not.toHaveAttribute('data-typing');
    expect(screen.getByRole('navigation', { name: 'main' })).toBeInTheDocument();
  });

  it('covered: main, rail, sidebar, aside and call bar are inert; the tab bar and the ring are not', () => {
    const ring = <section aria-label="incoming calls"><button type="button">answer</button></section>;
    const { container } = render(<ComposedShell ring={ring} shell={phone({ phonePane: 'view', covered: true, aside: <p>pane</p>, asideLabel: 'members', asideOpen: true })} />);
    expect(screen.getByRole('main')).toHaveAttribute('inert');
    expect(screen.getByTestId('call-bar').closest('.d-app-shell__callbar')).toHaveAttribute('inert');
    expect(container.querySelector('.d-app-shell__aside')).toHaveAttribute('inert');
    expect(screen.getByTestId('tab-bar').closest('.d-app-shell__tabbar')).not.toHaveAttribute('inert');
    expect(container.querySelector('.d-app-shell__ring')).not.toHaveAttribute('inert');
  });

  it('covered alone (no aside) makes main and the call bar inert, and leaves the tab bar live', () => {
    render(<ComposedShell shell={phone({ phonePane: 'view', covered: true })} />);
    expect(screen.getByRole('main')).toHaveAttribute('inert');
    expect(screen.getByTestId('call-bar').closest('.d-app-shell__callbar')).toHaveAttribute('inert');
    expect(screen.getByTestId('tab-bar').closest('.d-app-shell__tabbar')).not.toHaveAttribute('inert');
  });

  it('typing: moving focus from one field to another never shows the tab bar in between', () => {
    render(<><ComposedShell shell={phone({ phonePane: 'view' })} /><input aria-label="other" /></>);
    const wrapper = document.querySelector('.d-app-shell__tabbar') as HTMLElement;
    const composer = screen.getByRole('textbox', { name: 'message #general' });
    act(() => { composer.focus(); });
    expect(wrapper).toHaveAttribute('hidden');
    const seen: Array<string | null> = [];
    const watch = new MutationObserver(() => seen.push(wrapper.getAttribute('hidden')));
    watch.observe(wrapper, { attributes: true, attributeFilter: ['hidden'] });
    act(() => { screen.getByRole('textbox', { name: 'other' }).focus(); });
    watch.disconnect();
    expect(seen).toEqual([]);
    expect(wrapper).toHaveAttribute('hidden');
  });

  it('typing: a read-only or disabled field does not count', () => {
    const { container } = render(<><ComposedShell shell={phone({ phonePane: 'view' })} /><textarea aria-label="ro" readOnly /><select aria-label="sel" disabled><option>a</option></select></>);
    act(() => { screen.getByRole('textbox', { name: 'ro' }).focus(); });
    expect(container.firstElementChild).not.toHaveAttribute('data-typing');
  });

  it('typing: a checkbox does not count', () => {
    const { container } = render(<ComposedShell shell={phone({ phonePane: 'view', tabBar: <input type="checkbox" aria-label="pick" /> })} />);
    act(() => { screen.getByRole('checkbox', { name: 'pick' }).focus(); });
    expect(container.firstElementChild).not.toHaveAttribute('data-typing');
  });

  // web3.8 (M9): a page above the phone shell (Settings) renders after the call bar and before the tab bar.
  it('page renders after the call bar and before the tab bar, on a phone only', () => {
    const page = <p data-testid="page">settings</p>;
    const { container, unmount } = render(<ComposedShell shell={phone({ phonePane: 'view', page })} />);
    const slot = screen.getByTestId('page').closest('.d-app-shell__page') as HTMLElement;
    expect(slot).not.toBeNull();
    const call = screen.getByTestId('call-bar').parentElement as HTMLElement;
    const tabs = screen.getByTestId('tab-bar').parentElement as HTMLElement;
    expect(call.compareDocumentPosition(slot) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(slot.compareDocumentPosition(tabs) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    unmount();
    render(<ComposedShell shell={{ page }} />);
    expect(screen.queryByTestId('page')).toBeNull();
    expect(container).toBeDefined();
  });

  // web3.8 (M10): a covering page leaves the banner live and takes the skip link out.
  it('covered: the skip link is inert and the banner wrapper is live; uncovered, neither', () => {
    const { container, rerender } = render(<ComposedShell banner shell={phone({ phonePane: 'view', covered: true })} />);
    expect(container.querySelector('.d-app-shell__skip')).toHaveAttribute('inert');
    expect(container.querySelector('.d-app-shell__banner')).not.toHaveAttribute('inert');
    rerender(<ComposedShell banner shell={phone({ phonePane: 'view' })} />);
    expect(container.querySelector('.d-app-shell__skip')).not.toHaveAttribute('inert');
  });

  it('covered: --d-banner-h on <html> follows the banner wrapper height; uncovered, it is cleared', () => {
    let notify: () => void = () => {};
    let height = 36;
    vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockImplementation(function (this: HTMLElement) {
      return this.classList.contains('d-app-shell__banner') ? height : 0;
    });
    vi.stubGlobal('ResizeObserver', class {
      constructor(cb: () => void) { notify = cb; }
      observe() {}
      unobserve() {}
      disconnect() {}
    });
    const html = document.documentElement;
    const { rerender, unmount } = render(<ComposedShell banner shell={phone({ phonePane: 'view', covered: true })} />);
    expect(html.style.getPropertyValue('--d-banner-h')).toBe('36px');
    height = 52;
    act(() => notify());
    expect(html.style.getPropertyValue('--d-banner-h')).toBe('52px');
    rerender(<ComposedShell banner shell={phone({ phonePane: 'view' })} />);
    expect(html.style.getPropertyValue('--d-banner-h')).toBe('');
    rerender(<ComposedShell banner shell={phone({ phonePane: 'view', covered: true })} />);
    expect(html.style.getPropertyValue('--d-banner-h')).toBe('52px');
    unmount();
    expect(html.style.getPropertyValue('--d-banner-h')).toBe('');
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  // web3.8 (M4): on Android the keyboard can close (the back button) while the field keeps focus; the tab bar follows the keyboard.
  describe('typing follows the visual viewport', () => {
    class FakeViewport extends EventTarget { height = window.innerHeight; scale = 1; }
    let vv: FakeViewport;
    const resize = (height: number) => { vv.height = height; act(() => { vv.dispatchEvent(new Event('resize')); }); };
    beforeEach(() => {
      vv = new FakeViewport();
      Object.defineProperty(window, 'visualViewport', { configurable: true, value: vv });
    });
    afterEach(() => { Reflect.deleteProperty(window, 'visualViewport'); });

    it('a closed keyboard with focus kept is not typing; the keyboard opening again is', () => {
      const { container } = render(<ComposedShell shell={phone({ phonePane: 'view' })} />);
      const shell = container.firstElementChild as HTMLElement;
      const wrapper = screen.getByTestId('tab-bar').parentElement!;
      resize(window.innerHeight - 300);
      act(() => { screen.getByRole('textbox', { name: 'message #general' }).focus(); });
      expect(shell).toHaveAttribute('data-typing', 'true');
      expect(wrapper).toHaveAttribute('hidden');
      resize(window.innerHeight);
      expect(screen.getByRole('textbox', { name: 'message #general' })).toHaveFocus();
      expect(shell).not.toHaveAttribute('data-typing');
      expect(wrapper).not.toHaveAttribute('hidden');
      resize(window.innerHeight - 300);
      expect(shell).toHaveAttribute('data-typing', 'true');
      expect(wrapper).toHaveAttribute('hidden');
    });

    it('a focused field with the keyboard still closed is not typing yet', () => {
      const { container } = render(<ComposedShell shell={phone({ phonePane: 'view' })} />);
      act(() => { screen.getByRole('textbox', { name: 'message #general' }).focus(); });
      expect(container.firstElementChild).not.toHaveAttribute('data-typing');
      resize(window.innerHeight - 300);
      expect(container.firstElementChild).toHaveAttribute('data-typing', 'true');
    });

    it('stops listening to the viewport on unmount', () => {
      const off = vi.spyOn(vv, 'removeEventListener');
      render(<ComposedShell shell={phone({ phonePane: 'view' })} />).unmount();
      expect(off).toHaveBeenCalledWith('resize', expect.any(Function));
    });
  });

  it('typing seeds from the focused element on mount', () => {
    const field = document.createElement('input');
    document.body.append(field);
    field.focus();
    const { container } = render(<ComposedShell shell={phone({ phonePane: 'view' })} />);
    expect(container.firstElementChild).toHaveAttribute('data-typing', 'true');
    expect(screen.getByTestId('tab-bar').parentElement).toHaveAttribute('hidden');
    field.remove();
  });

  it('desktop has no tab bar wrapper and no data-typing', () => {
    const { container } = render(<ComposedShell shell={{ tabBar: tabs }} />);
    act(() => { screen.getByRole('textbox', { name: 'message #general' }).focus(); });
    expect(container.querySelector('.d-app-shell__tabbar')).toBeNull();
    expect(container.firstElementChild).not.toHaveAttribute('data-typing');
  });

  it('desktop ignores the phone slots', () => {
    const { container } = render(<ComposedShell shell={{ tabBar: tabs, callBar: calls }} />);
    expect(screen.queryByTestId('tab-bar')).toBeNull();
    expect(screen.queryByTestId('call-bar')).toBeNull();
    expect(container.querySelector('.d-app-shell__status')).toBeNull();
  });

  it('has no serious axe violations on either pane', async () => {
    const list = render(<div className="d-root"><ComposedShell shell={phone()} /></div>);
    await expectNoAxeViolations(list.container);
    list.unmount();
    const view = render(<div className="d-root"><ComposedShell shell={phone({ phonePane: 'view' })} /></div>);
    await expectNoAxeViolations(view.container);
  });
});
