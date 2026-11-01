import { useLayoutEffect } from 'react';
import { act, fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeAll, describe, it, expect, vi } from 'vitest';
import { SettingsFrame, type SettingsFrameProps } from './SettingsFrame.tsx';
import { SettingsNav } from '../SettingsNav/SettingsNav.tsx';
import { Dialog } from '../Dialog/Dialog.tsx';
import { PhoneLayoutProvider } from '../internal/phoneLayout.ts';
import { expectNoAxeViolations } from '../test/setup.ts';

// L-COPY-02: settings.title, settings.close, settings.nav.*, devices.title, devices.refresh.
const ITEMS = [{ id: 'devices', label: 'devices' }, { id: 'notifications', label: 'notifications' }, { id: 'appearance', label: 'appearance' }] as const;
function props(over: Partial<SettingsFrameProps> = {}): SettingsFrameProps {
  return {
    open: true, title: 'Settings', closeLabel: 'Close settings', onClose: vi.fn(),
    nav: <SettingsNav label="settings sections" items={ITEMS} activeId="devices" onSelect={() => {}} />,
    children: <section aria-labelledby="devices-title"><h2 id="devices-title">Devices</h2><button type="button">refresh</button></section>,
    ...over,
  };
}

describe('SettingsFrame', () => {
  it('opens as a modal dialog named by its h1, and the h1 takes focus', () => {
    render(<SettingsFrame {...props()} />);
    const dialog = screen.getByRole('dialog', { name: 'Settings' });
    expect(dialog.tagName).toBe('DIALOG');
    expect(dialog).toHaveClass('d-settings-frame');
    expect(dialog).toHaveAttribute('aria-modal', 'true');
    expect(within(dialog).getByRole('heading', { level: 1, name: 'Settings' })).toHaveFocus();
  });

  it('always opens as a native modal workspace, including inside the phone layout', () => {
    const modal = vi.spyOn(HTMLDialogElement.prototype, 'showModal');
    const plain = vi.spyOn(HTMLDialogElement.prototype, 'show');
    render(<PhoneLayoutProvider value><SettingsFrame {...props()} /></PhoneLayoutProvider>);
    expect(modal).toHaveBeenCalledTimes(1);
    expect(plain).not.toHaveBeenCalled();
    expect(screen.getByRole('dialog', { name: 'Settings' })).toHaveAttribute('aria-modal', 'true');
    vi.restoreAllMocks();
  });

  it('shows the grouped index and identity on the phone index page', () => {
    render(<PhoneLayoutProvider value><SettingsFrame {...props({
      phonePage: 'index', identity: <div>Signed in as Ada</div>, search: <input aria-label="Search settings" />,
    })} /></PhoneLayoutProvider>);
    const dialog = screen.getByRole('dialog', { name: 'Settings' });
    expect(within(dialog).getByText('Signed in as Ada')).toBeInTheDocument();
    expect(within(dialog).getByRole('textbox', { name: 'Search settings' })).toBeInTheDocument();
    expect(within(dialog).getByRole('navigation', { name: 'settings sections' })).toBeVisible();
    expect(within(dialog).queryByRole('heading', { level: 2, name: 'Devices' })).toBeNull();
  });

  it('keeps workspace confirmations accessible on the phone index', () => {
    render(<PhoneLayoutProvider value><SettingsFrame {...props({ phonePage: 'index', overlays: <Dialog open title="Leave settings?" onClose={() => {}}><button type="button">Keep editing</button></Dialog> })} /></PhoneLayoutProvider>);
    expect(screen.getByRole('dialog', { name: 'Leave settings?' })).toBeVisible();
    expect(screen.getByRole('button', { name: 'Keep editing' })).toBeVisible();
  });

  // Task 10, README §5.10: a phone section shows the nav as a row of chips above the content, and the close button leaves.
  it('shows the nav and the section on a phone section page, with the close button and no Back', () => {
    render(<PhoneLayoutProvider value><SettingsFrame {...props({ phonePage: 'section' })} /></PhoneLayoutProvider>);
    const dialog = screen.getByRole('dialog', { name: 'Settings' });
    expect(within(dialog).getByRole('navigation', { name: 'settings sections' })).toBeInTheDocument();
    expect(within(dialog).queryByRole('button', { name: 'Back' })).toBeNull();
    expect(within(dialog).getByRole('heading', { level: 2, name: 'Devices' })).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Close settings' })).toBeInTheDocument();
  });

  it('keeps call controls visible beside the content', () => {
    render(<PhoneLayoutProvider value><SettingsFrame {...props({ phonePage: 'section', activity: <button type="button">Return to call</button> })} /></PhoneLayoutProvider>);
    expect(screen.getByRole('button', { name: 'Return to call' })).toBeVisible();
  });

  it('keeps idle activity announcements mounted when their visible header space is removed', () => {
    const p = props({ activity: <div role="status" />, activityVisible: false });
    const { rerender } = render(<PhoneLayoutProvider value><SettingsFrame {...p} /></PhoneLayoutProvider>);
    const region = screen.getByRole('status');
    expect(region.closest('.d-settings-frame__activity')).toHaveAttribute('data-visible', 'false');
    expect(region.closest('[hidden], [aria-hidden="true"]')).toBeNull();
    rerender(<PhoneLayoutProvider value><SettingsFrame {...p} activityVisible /></PhoneLayoutProvider>);
    expect(screen.getByRole('status')).toBe(region);
    expect(region.closest('.d-settings-frame__activity')).toHaveAttribute('data-visible', 'true');
  });

  it('moves focus to the current chip when a phone index opens a section', () => {
    const p = props({ phonePage: 'index' });
    const { rerender } = render(<PhoneLayoutProvider value><SettingsFrame {...p} /></PhoneLayoutProvider>);
    rerender(<PhoneLayoutProvider value><SettingsFrame {...p} phonePage="section" /></PhoneLayoutProvider>);
    expect(screen.getByRole('button', { name: 'devices' })).toHaveFocus();
  });

  it('keeps Close on a phone index page and makes backdrop clicks inert', () => {
    const p = props({ phonePage: 'index' });
    render(<PhoneLayoutProvider value><SettingsFrame {...p} /></PhoneLayoutProvider>);
    const dialog = screen.getByRole('dialog', { name: 'Settings' });
    expect(within(dialog).getByRole('button', { name: 'Close settings' })).toBeInTheDocument();
    fireEvent.click(dialog);
    expect(p.onClose).not.toHaveBeenCalled();
  });

  it('places activity controls inside the dialog and exposes the content container ref and wide measure', () => {
    let content: HTMLDivElement | null = null;
    render(<SettingsFrame {...props({
      activity: <button type="button">Return to call</button>, wide: true, contentRef: node => { content = node; },
    })} />);
    const dialog = screen.getByRole('dialog', { name: 'Settings' });
    expect(within(dialog).getByRole('button', { name: 'Return to call' })).toBeInTheDocument();
    expect(content).toBe(dialog.querySelector('.d-settings-frame__body'));
    expect(content).toHaveClass('d-settings-frame__body--wide');
  });

  it('holds the navigation and the section', () => {
    render(<SettingsFrame {...props()} />);
    const dialog = screen.getByRole('dialog', { name: 'Settings' });
    expect(within(dialog).getByRole('navigation', { name: 'settings sections' })).toBeInTheDocument();
    expect(within(dialog).getByRole('heading', { level: 2, name: 'Devices' })).toBeInTheDocument();
  });

  // Task 10: the close button is the viewport's (README §5.9), first in the order after the heading.
  it('tabs from the heading to the close button, the navigation, then the section', async () => {
    const user = userEvent.setup();
    render(<SettingsFrame {...props()} />);
    await user.tab();
    expect(screen.getByRole('button', { name: 'Close settings' })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole('button', { name: 'devices' })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole('button', { name: 'refresh' })).toHaveFocus();
  });

  it('calls onClose on Escape and from the close button', async () => {
    const user = userEvent.setup();
    const p = props();
    render(<SettingsFrame {...p} />);
    await user.keyboard('{Escape}');
    expect(p.onClose).toHaveBeenCalledTimes(1);
    await user.click(screen.getByRole('button', { name: 'Close settings' }));
    expect(p.onClose).toHaveBeenCalledTimes(2);
  });

  // Pre-flight ruling (row 1.2): Escape inside a nested dialog (remove, sign out, forget) belongs to that
  // dialog and never reaches the frame, so a revoke in flight is never unmounted with the frame.
  it('leaves an Escape or a cancel inside a nested dialog to that dialog', async () => {
    const user = userEvent.setup();
    const p = props();
    const onDialogClose = vi.fn();
    render(
      <SettingsFrame {...p}>
        <section aria-labelledby="devices-title">
          <h2 id="devices-title">Devices</h2>
          <Dialog open title="Remove this device?" onClose={onDialogClose}><p>x</p></Dialog>
        </section>
      </SettingsFrame>,
    );
    const nested = screen.getByRole('dialog', { name: 'Remove this device?' });
    act(() => { nested.focus(); });
    expect(nested).toHaveFocus();
    await user.keyboard('{Escape}');
    expect(onDialogClose).toHaveBeenCalledTimes(1);
    expect(p.onClose).not.toHaveBeenCalled();
    fireEvent(nested, new Event('cancel', { cancelable: true }));
    expect(onDialogClose).toHaveBeenCalledTimes(2);
    expect(p.onClose).not.toHaveBeenCalled();
    act(() => { screen.getByRole('heading', { level: 1, name: 'Settings' }).focus(); });
    await user.keyboard('{Escape}');
    expect(p.onClose).toHaveBeenCalledTimes(1);
  });

  // web3.8 (N6): an Escape that ends an input-method composition belongs to the composition, and a bare <dialog> child
  // (not a ui Dialog) owns its own Escape too.
  it('an Escape that ends a composition does not close the frame, on a control inside it or at the document', () => {
    const p = props({ children: <input aria-label="name" /> });
    render(<SettingsFrame {...p} />);
    fireEvent.keyDown(screen.getByRole('textbox', { name: 'name' }), { key: 'Escape', isComposing: true });
    expect(p.onClose).not.toHaveBeenCalled();
    fireEvent.keyDown(screen.getByRole('textbox', { name: 'name' }), { key: 'Escape' });
    expect(p.onClose).toHaveBeenCalledTimes(1);
  });

  it('with focus in a bare <dialog open> child, a document Escape does not close the frame', () => {
    const p = props({ children: <dialog open aria-label="bare"><button type="button">inside</button></dialog> });
    render(<PhoneLayoutProvider value><SettingsFrame {...p} /></PhoneLayoutProvider>);
    act(() => { screen.getByRole('button', { name: 'inside' }).focus(); });
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(p.onClose).not.toHaveBeenCalled();
    fireEvent.keyDown(screen.getByRole('button', { name: 'inside' }), { key: 'Escape' });
    expect(p.onClose).not.toHaveBeenCalled();
  });

  it('closes the native dialog when open flips to false, so the browser returns focus', () => {
    const close = vi.spyOn(HTMLDialogElement.prototype, 'close');
    const p = props();
    const { rerender } = render(<SettingsFrame {...p} />);
    expect(close).not.toHaveBeenCalled();
    rerender(<SettingsFrame {...p} open={false} />);
    expect(close).toHaveBeenCalledTimes(1);
    close.mockRestore();
  });

  it('returns focus to the opener, or to a visible main landmark when the opener disappeared', () => {
    const p = props();
    function Fixture({ open, showOpener }: { open: boolean; showOpener: boolean }) {
      return <><main tabIndex={-1} aria-label="Main content" />{showOpener ? <button type="button" data-testid="opener">Open settings</button> : null}<SettingsFrame {...p} open={open} /></>;
    }
    const { rerender } = render(<Fixture open={false} showOpener />);
    const opener = screen.getByTestId('opener');
    opener.focus();
    rerender(<Fixture open showOpener />);
    rerender(<Fixture open={false} showOpener />);
    expect(opener).toHaveFocus();
    rerender(<Fixture open showOpener />);
    rerender(<Fixture open={false} showOpener={false} />);
    expect(screen.getByRole('main', { name: 'Main content' })).toHaveFocus();
  });

  it('closes the native dialog in the commit that empties it, before any passive effect runs', () => {
    // As Dialog's: a later sibling's layout effect sees what a key event before React's next task would meet.
    const seen: string[] = [];
    function Probe({ open }: { open: boolean }) {
      useLayoutEffect(() => {
        const el = document.querySelector('dialog');
        seen.push(`${String(open)}:${String(el?.open)}:${String(el?.childElementCount)}`);
      }, [open]);
      return null;
    }
    const p = props();
    const { rerender } = render(<><SettingsFrame {...p} /><Probe open /></>);
    rerender(<><SettingsFrame {...p} open={false} /><Probe open={false} /></>);
    expect(seen.at(-1)).toBe('false:false:0');
  });

  // Signal task 10 fix round 1: the user's and a server's settings are one workspace; swapping frames in one commit keeps
  // the entrance still and hands the opener over, so closing the second frame still returns focus there.
  it('hands its opener to a frame that replaces it in the same commit, without replaying the entrance', () => {
    const opener = document.createElement('button');
    opener.textContent = 'gear';
    document.body.append(opener);
    opener.focus();
    const { rerender } = render(<SettingsFrame key="user" {...props()} />);
    rerender(<SettingsFrame key="server" {...props()} />);
    const dialog = screen.getByRole('dialog', { name: 'Settings' });
    expect(dialog).toHaveAttribute('data-handoff', 'true');
    expect(opener).not.toHaveFocus();
    rerender(<SettingsFrame key="server" {...props({ open: false })} />);
    expect(opener).toHaveFocus();
    opener.remove();
  });

  it('renders no content while closed', () => {
    render(<SettingsFrame {...props({ open: false })} />);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.queryByRole('heading')).toBeNull();
    expect(screen.queryByRole('navigation')).toBeNull();
  });

  it('focuses the heading again when it reopens', () => {
    const p = props();
    const { rerender } = render(<SettingsFrame {...p} />);
    rerender(<SettingsFrame {...p} open={false} />);
    rerender(<SettingsFrame {...p} open />);
    expect(screen.getByRole('heading', { level: 1, name: 'Settings' })).toHaveFocus();
  });

  it('on desktop opens with showModal', () => {
    const modal = vi.spyOn(HTMLDialogElement.prototype, 'showModal');
    const plain = vi.spyOn(HTMLDialogElement.prototype, 'show');
    render(<SettingsFrame {...props()} />);
    expect(modal).toHaveBeenCalledTimes(1);
    expect(plain).not.toHaveBeenCalled();
    vi.restoreAllMocks();
  });

  describe('inside the phone layout', () => {
    const phone = (over: Partial<SettingsFrameProps> = {}) => render(<PhoneLayoutProvider value><SettingsFrame {...props(over)} /></PhoneLayoutProvider>);

    it('uses the fullscreen phone layout and the title takes focus', () => {
      const modal = vi.spyOn(HTMLDialogElement.prototype, 'showModal');
      const plain = vi.spyOn(HTMLDialogElement.prototype, 'show');
      phone();
      expect(modal).toHaveBeenCalledTimes(1);
      expect(plain).not.toHaveBeenCalled();
      expect(screen.getByRole('dialog', { name: 'Settings' })).toHaveAttribute('aria-modal', 'true');
      expect(screen.getByRole('heading', { level: 1, name: 'Settings' })).toHaveFocus();
      vi.restoreAllMocks();
    });

    it('closes on Escape through its own key handler', async () => {
      const user = userEvent.setup();
      const p = props();
      render(<PhoneLayoutProvider value><SettingsFrame {...p} /></PhoneLayoutProvider>);
      await user.keyboard('{Escape}');
      expect(p.onClose).toHaveBeenCalledTimes(1);
    });

    it('keeps Escape outside the frame within the modal workspace', async () => {
      const user = userEvent.setup();
      const p = props();
      render(<><PhoneLayoutProvider value><SettingsFrame {...p} /></PhoneLayoutProvider><button type="button">tab</button></>);
      screen.getByRole('button', { name: 'tab' }).focus();
      await user.keyboard('{Escape}');
      expect(p.onClose).not.toHaveBeenCalled();
    });

    it('does not close on an Escape a nested dialog handled', async () => {
      const user = userEvent.setup();
      const p = props();
      render(<PhoneLayoutProvider value><SettingsFrame {...p}><Dialog open title="Remove this device?" onClose={vi.fn()}><p>x</p></Dialog></SettingsFrame></PhoneLayoutProvider>);
      act(() => { screen.getByRole('dialog', { name: 'Remove this device?' }).focus(); });
      await user.keyboard('{Escape}');
      expect(p.onClose).not.toHaveBeenCalled();
    });

    it('stays open when a nested element handled Escape (preventDefault)', () => {
      const p = props();
      render(<PhoneLayoutProvider value><SettingsFrame {...p}><input aria-label="key capture" onKeyDown={e => e.preventDefault()} /></SettingsFrame></PhoneLayoutProvider>);
      fireEvent.keyDown(screen.getByRole('textbox', { name: 'key capture' }), { key: 'Escape' });
      expect(p.onClose).not.toHaveBeenCalled();
    });

    it('ignores an Escape that ends an input-method composition', () => {
      const p = props();
      render(<><PhoneLayoutProvider value><SettingsFrame {...p} /></PhoneLayoutProvider><button type="button">tab</button></>);
      fireEvent.keyDown(screen.getByRole('button', { name: 'tab' }), { key: 'Escape', isComposing: true });
      expect(p.onClose).not.toHaveBeenCalled();
    });

    it('stays modal when the deprecated modal prop is false', () => {
      const modal = vi.spyOn(HTMLDialogElement.prototype, 'showModal');
      phone({ modal: false });
      expect(modal).toHaveBeenCalledTimes(1);
      vi.restoreAllMocks();
    });
  });

  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><SettingsFrame {...props()} /></div>);
    await expectNoAxeViolations(container);
  });
});

// jsdom computes no layout, so the frame geometry assertions below read the layout rules themselves.
describe('SettingsFrame stacked (A11Y-DESIGN-01)', () => {
  let stacked = '';
  let base = '';
  beforeAll(async () => {
    const fs = await import(/* @vite-ignore */ 'node:fs' as string) as { readFileSync(path: string, encoding: 'utf8'): string };
    const cwd = (globalThis as unknown as { process: { cwd(): string } }).process.cwd();
    stacked = fs.readFileSync(`${cwd}/src/SettingsFrame/SettingsFrame.css`, 'utf8');
    base = fs.readFileSync(`${cwd}/src/styles/base.css`, 'utf8');
  });

  it('keeps the sidebar and content as independent columns with their own scroll regions', () => {
    expect(stacked).not.toBe('');
    expect(stacked).toMatch(/\.d-settings-frame__side \{[^}]*overflow-y: auto;/);
    expect(stacked).toMatch(/\.d-settings-frame__body \{[^}]*overflow-y: auto;/);
  });

  it('uses a 240px grouped sidebar and bounded 720px and 64rem content measures (README §5.9)', async () => {
    const fs = await import(/* @vite-ignore */ 'node:fs' as string) as { readFileSync(path: string, encoding: 'utf8'): string };
    const cwd = (globalThis as unknown as { process: { cwd(): string } }).process.cwd();
    const css = fs.readFileSync(`${cwd}/src/SettingsFrame/SettingsFrame.css`, 'utf8');
    expect(css).toMatch(/grid-template-columns: 240px minmax\(0, 1fr\)/);
    expect(css).toContain('width: min(100%, 720px)');
    expect(css).toContain('width: min(100%, 64rem)');
    expect(css).toMatch(/\.d-settings-frame \{[^}]*width: 100vw;[^}]*height: 100dvh;/);
    expect(css).toMatch(/\.d-settings-frame__panel \{[^}]*max-width: 80rem;/);
  });

  it('leaves fullscreen and phone scroll ownership to SettingsFrame instead of legacy container overrides', () => {
    expect(base).not.toContain('@container d-settings');
    expect(base).not.toMatch(/\.d-settings-frame[^{}]*\[data-modeless\]/);
    expect(base).toContain('.d-settings-frame[data-layout="phone"] .d-settings-row, [data-phone] .d-settings-row');
    expect(stacked).toMatch(/\.d-settings-frame__body \{[^}]*overflow-y: auto;/);
    expect(stacked).toMatch(/\.d-settings-frame\[data-layout="phone"\]\[data-phone-page="index"\] \.d-settings-frame__nav \{[^}]*overflow-y: auto;/);
    expect(stacked).toMatch(/\.d-settings-frame\[data-layout="phone"\]\[data-phone-page="section"\] \.d-settings-frame__side \{[^}]*flex-direction: row; overflow-x: auto;/);
  });
});

// R17, I5, I8: one hairline at y=48 across the side title and the section bar; the one sticky save footer; no repeated
// title on a phone (H5). jsdom has no layout, so these read the rules.
describe('SettingsFrame bar and footer (I8, R17)', () => {
  let css = '';
  beforeAll(async () => {
    const fs = await import(/* @vite-ignore */ 'node:fs' as string) as { readFileSync(path: string, encoding: 'utf8'): string };
    const cwd = (globalThis as unknown as { process: { cwd(): string } }).process.cwd();
    css = fs.readFileSync(`${cwd}/src/SettingsFrame/SettingsFrame.css`, 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
  });
  const rule = (src: string, selector: string) => new RegExp(`${selector.replace(/[.[\]"=-]/g, '\\$&')} \\{([^}]*)\\}`).exec(src)?.[1] ?? '';

  it('uses the prototype 36/40/60 body padding and has no floating shadow or perimeter', () => {
    const body = rule(css, '.d-settings-frame__body');
    const frame = rule(css, '.d-settings-frame');
    expect(body).toMatch(/padding: 36px 40px 60px;/);
    expect(frame).toMatch(/border: 0;/);
    expect(frame).toMatch(/box-shadow: none;/);
  });

  it('renders the footer after the body, in the pane, and is absent without it', () => {
    const { rerender } = render(<SettingsFrame {...props({ footer: <button type="button">save</button> })} />);
    const footer = screen.getByRole('button', { name: 'save' }).closest('.d-settings-frame__footer');
    expect(footer).not.toBeNull();
    expect(footer?.previousElementSibling).toHaveClass('d-settings-frame__body');
    expect(footer?.parentElement).toHaveClass('d-settings-frame__pane');
    rerender(<SettingsFrame {...props()} />);
    expect(document.querySelector('.d-settings-frame__footer')).toBeNull();
  });

  it('the footer is sticky to the bottom of the pane', () => {
    const r = rule(css, '.d-settings-frame__footer');
    expect(r).toMatch(/position: sticky;/);
    expect(r).toMatch(/bottom: 0;/);
  });

  it('a phone layout sets the section page; the frame title is for assistive technology only', () => {
    render(<PhoneLayoutProvider value><SettingsFrame {...props()} /></PhoneLayoutProvider>);
    expect(screen.getByRole('dialog', { name: 'Settings' })).toHaveAttribute('data-layout', 'phone');
    expect(screen.getByRole('dialog', { name: 'Settings' })).toHaveAttribute('data-phone-page', 'section');
    expect(rule(css, '.d-settings-frame__title')).toMatch(/clip-path: inset\(50%\);/);
  });

  it('uses the existing narrow and landscape/coarse-pointer phone breakpoint with visual viewport and safe area', () => {
    expect(css).toContain('.d-settings-frame.d-settings-frame[data-layout="phone"]');
    expect(css).toContain('var(--d-vvtop, 0px)');
    expect(css).toContain('var(--d-vvh, 100dvh)');
    expect(css).toContain('env(safe-area-inset-bottom)');
  });

  it('draws the section title 20/700 on every layout (the prototype shows it on a phone too)', () => {
    expect(rule(css, '.d-settings-frame__body > section > h2')).toMatch(/font-size: var\(--text-heading\); font-weight: var\(--weight-display\);/);
    expect(css).not.toMatch(/section > h2:first-child/);
  });

  it('draws the close button as a 34px circle that turns 90° on hover, with ESC under it except on a phone', () => {
    render(<SettingsFrame {...props()} />);
    const close = screen.getByRole('button', { name: 'Close settings' });
    expect(close).toHaveAttribute('aria-keyshortcuts', 'Escape');
    expect(close.querySelector('.d-settings-frame__close-key')).toHaveAttribute('aria-hidden', 'true');
    expect(rule(css, '.d-settings-frame__close-ring')).toMatch(/width: 34px; height: 34px;/);
    expect(rule(css, '.d-settings-frame__close:hover .d-settings-frame__close-ring')).toMatch(/transform: rotate\(90deg\);/);
    expect(rule(css, '.d-settings-frame[data-layout="phone"] .d-settings-frame__close-key')).toMatch(/display: none;/);
  });

  it('a desktop frame has no data-layout', () => {
    render(<SettingsFrame {...props()} />);
    expect(screen.getByRole('dialog', { name: 'Settings' })).not.toHaveAttribute('data-layout');
  });
});
