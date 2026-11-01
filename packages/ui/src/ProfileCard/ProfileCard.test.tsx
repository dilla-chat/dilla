import { readFileSync } from 'node:fs';
import { fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ProfileCard, type ProfileCardProps } from './ProfileCard.tsx';
import { PhoneLayoutProvider } from '../internal/phoneLayout.ts';
import { expectNoAxeViolations } from '../test/setup.ts';

const css = readFileSync(`${process.cwd()}/src/ProfileCard/ProfileCard.css`, 'utf8');
const actions = [{ id: 'message', label: 'message' }, { id: 'safety', label: 'compare safety numbers' }, { id: 'kick', label: 'kick' }, { id: 'ban', label: 'ban', danger: true }];
function props(over: Partial<ProfileCardProps> = {}): ProfileCardProps {
  return {
    name: 'Astrid', username: 'astrid', presence: 'online', isOwner: true, isYou: true,
    roles: [{ name: 'moderators', color: 0xe74c3c }, { name: 'members', color: null }],
    actions, onAction: vi.fn(), onClose: vi.fn(), ...over,
  };
}
function anchorButton(): HTMLButtonElement {
  const b = document.createElement('button');
  b.textContent = 'Astrid';
  document.body.appendChild(b);
  return b;
}

afterEach(() => { delete document.documentElement.dataset.theme; document.body.innerHTML = ''; });

describe('ProfileCard content', () => {
  it('is a dialog named by the member, with the avatar, display name, @username and the presence word', () => {
    render(<ProfileCard {...props()} />);
    const card = screen.getByRole('dialog', { name: 'Astrid' });
    expect(within(card).getByRole('heading', { name: 'Astrid' })).toBeInTheDocument();
    // Task 9 (Signal §5.7): the prototype's "@handle · status" line under the name, and the 56px avatar.
    expect(card.querySelector('.d-profile-card__status')).toHaveTextContent('@astrid · online');
    expect(card.querySelector('.d-avatar')).toHaveAttribute('data-size', 'card');
  });

  it('shows a custom status after the presence word when there is one', () => {
    render(<ProfileCard {...props({ status: 'in a meeting' })} />);
    expect(screen.getByText('in a meeting')).toBeInTheDocument();
  });

  it('shows the bio under the head, labelled, keeping its line breaks, and nothing when there is none', () => {
    const { rerender } = render(<ProfileCard {...props({ bio: 'line one\nline two', bioLabel: 'about' })} />);
    const card = screen.getByRole('dialog');
    const about = within(card).getByRole('group', { name: 'about' });
    expect(about).toHaveTextContent('line one line two');
    expect(about.querySelector('.d-profile-card__bio')?.textContent).toBe('line one\nline two');
    expect(css).toMatch(/\.d-profile-card__bio[^}]*white-space: pre-wrap/);
    rerender(<ProfileCard {...props()} />);
    expect(within(card).queryByRole('group', { name: 'about' })).toBeNull();
    rerender(<ProfileCard {...props({ bio: '' })} />);
    expect(within(card).queryByRole('group', { name: 'about' })).toBeNull();
  });

  // jsdom has no layout, so the box's measures are set by hand.
  function measure(scrollHeight: number, clientHeight: number) {
    vi.spyOn(HTMLElement.prototype, 'scrollHeight', 'get').mockReturnValue(scrollHeight);
    vi.spyOn(HTMLElement.prototype, 'clientHeight', 'get').mockReturnValue(clientHeight);
  }
  afterEach(() => { vi.restoreAllMocks(); });

  it('makes a bio that overflows reachable by keyboard: focusable, named, with a visible focus ring', () => {
    measure(300, 128);
    render(<ProfileCard {...props({ bio: 'x '.repeat(200) })} />);
    const about = screen.getByRole('group', { name: 'about' });
    expect(about).toHaveAttribute('tabindex', '0');
    expect(css).toMatch(/\.d-profile-card__about:focus-visible[^}]*outline: [^;}]*var\(--accent\)/);
    expect(css).toMatch(/\.d-profile-card__about[^}]*overflow-y: auto/);
    about.focus();
    expect(about).toHaveFocus();
  });

  it('is not a tab stop when the bio fits', () => {
    measure(60, 128);
    render(<ProfileCard {...props({ bio: 'short' })} />);
    expect(screen.getByRole('group', { name: 'about' })).not.toHaveAttribute('tabindex');
  });

  it('isolates the direction of the status message and the bio from their surroundings', () => {
    render(<ProfileCard {...props({ status: 'in a meeting', bio: 'hello' })} />);
    const status = screen.getByText('in a meeting');
    expect(status.tagName).toBe('BDI');
    expect(status).toHaveAttribute('dir', 'auto');
    expect(screen.getByRole('group', { name: 'about' }).querySelector('.d-profile-card__bio')).toHaveAttribute('dir', 'auto');
  });

  it('labels the bio "about" by default', () => {
    render(<ProfileCard {...props({ bio: 'hello' })} />);
    expect(screen.getByRole('group', { name: 'about' })).toHaveTextContent('hello');
  });

  // Task 9: the prototype draws the owner's crown after the name and no "you" mark.
  it('shows the owner crown (named) when it applies, and no "you" mark', () => {
    const { rerender } = render(<ProfileCard {...props()} />);
    const card = screen.getByRole('dialog');
    expect(within(card).getByRole('img', { name: 'owner' })).toBeInTheDocument();
    expect(card.querySelector('[data-icon="crown"]')).not.toBeNull();
    expect(within(card).queryByText('you')).toBeNull();
    rerender(<ProfileCard {...props({ isOwner: false, isYou: false })} />);
    expect(screen.queryByRole('img', { name: 'owner' })).toBeNull();
    expect(screen.getByRole('dialog').querySelector('[data-icon="crown"]')).toBeNull();
  });

  it('takes the owner and roles words from its props', () => {
    render(<ProfileCard {...props({ ownerLabel: 'ägare', rolesLabel: 'roller' })} />);
    expect(screen.getByRole('img', { name: 'ägare' })).toBeInTheDocument();
    expect(screen.getByRole('list', { name: 'roller' })).toBeInTheDocument();
  });

  it('lists the roles as chips in order, each with its 6px dot in the role colour (--fg-2 without one)', () => {
    render(<ProfileCard {...props()} />);
    const chips = within(screen.getByRole('list', { name: 'roles' })).getAllByRole('listitem');
    expect(chips.map(c => c.textContent)).toEqual(['moderators', 'members']);
    expect(chips[0]!.querySelector<HTMLElement>('.d-profile-card__role-dot')!.style.background).toBe('rgb(231, 76, 60)');
    expect(chips[1]!.querySelector<HTMLElement>('.d-profile-card__role-dot')!.style.background).toBe('');
  });

  it('draws a primary action as the labelled accent button with its glyph, the others as named glyph buttons; a bot is square', async () => {
    const user = userEvent.setup();
    const p = props({ isBot: true, actions: [
      { id: 'message', label: 'Message', icon: 'message-square', primary: true },
      { id: 'compare', label: 'compare safety numbers', icon: 'shield' },
      { id: 'roles', label: 'edit roles', icon: 'square-pen' },
    ] });
    render(<ProfileCard {...p} />);
    const card = screen.getByRole('dialog');
    expect(card.querySelector('.d-profile-card__action')).toHaveTextContent('Message');
    expect(card.querySelectorAll('.d-profile-card__icon-action')).toHaveLength(2);
    await user.click(screen.getByRole('button', { name: 'edit roles' }));
    expect(p.onAction).toHaveBeenCalledExactlyOnceWith('roles');
    expect(card.querySelector('.d-avatar')).toHaveAttribute('data-shape', 'bot');
    expect(card.style.getPropertyValue('--avatar-hue')).not.toBe('');
  });

  it('draws no roles list without roles and no action group without actions', () => {
    render(<ProfileCard {...props({ roles: [], actions: [] })} />);
    expect(screen.queryByRole('list', { name: 'roles' })).toBeNull();
    expect(screen.getByRole('dialog').querySelector('.d-profile-card__actions')).toBeNull();
    expect(screen.getAllByRole('button')).toHaveLength(1);
  });

  it('draws one button per action, a danger one marked, and reports the id on a click', async () => {
    const user = userEvent.setup();
    const p = props();
    render(<ProfileCard {...p} />);
    const buttons = within(screen.getByRole('group', { name: 'Astrid' })).getAllByRole('button');
    expect(buttons.map(b => b.textContent)).toEqual(['message', 'compare safety numbers', 'kick', 'ban']);
    expect(buttons[3]).toHaveAttribute('data-danger', 'true');
    expect(buttons[2]).not.toHaveAttribute('data-danger');
    await user.click(buttons[3]!);
    expect(p.onAction).toHaveBeenCalledExactlyOnceWith('ban');
  });
});

describe('ProfileCard behaviour', () => {
  it('moves focus in on open: the first action, else the card itself (task 9: the close shows only once Tab reaches it)', () => {
    const { unmount } = render(<ProfileCard {...props()} />);
    expect(screen.getByRole('button', { name: 'message' })).toHaveFocus();
    unmount();
    render(<ProfileCard {...props({ actions: [] })} />);
    expect(screen.getByRole('dialog')).toHaveFocus();
  });

  it('Escape closes it, and the close button too', async () => {
    const user = userEvent.setup();
    const p = props();
    render(<ProfileCard {...p} />);
    await user.keyboard('{Escape}');
    expect(p.onClose).toHaveBeenCalledOnce();
    await user.click(screen.getByRole('button', { name: 'Close' }));
    expect(p.onClose).toHaveBeenCalledTimes(2);
  });

  it('gives focus back to the row it was opened from when it closes', () => {
    const anchor = anchorButton();
    anchor.focus();
    const { unmount } = render(<ProfileCard {...props({ anchor })} />);
    expect(anchor).not.toHaveFocus();
    unmount();
    expect(anchor).toHaveFocus();
  });

  it('gives focus back to the row that replaced the opener while the card was open (same focus key)', () => {
    const anchor = anchorButton();
    anchor.dataset.focusKey = 'member:u1';
    anchor.focus();
    const { unmount } = render(<ProfileCard {...props({ anchor })} />);
    const fresh = anchorButton();
    fresh.dataset.focusKey = 'member:u1';
    anchor.remove();
    unmount();
    expect(fresh).toHaveFocus();
  });

  it('keeps Tab and Shift+Tab inside the card', async () => {
    const user = userEvent.setup();
    render(<><button>before</button><ProfileCard {...props()} /><button>after</button></>);
    const first = screen.getByRole('button', { name: 'Close' });
    const last = screen.getByRole('button', { name: 'ban' });
    first.focus();
    await user.tab({ shift: true });
    expect(last).toHaveFocus();
    await user.tab();
    expect(first).toHaveFocus();
  });

  it('a popover closes on a pointerdown outside, but not inside', () => {
    const p = props();
    render(<ProfileCard {...p} />);
    fireEvent.pointerDown(screen.getByRole('dialog').querySelector('.d-profile-card__status')!);
    expect(p.onClose).not.toHaveBeenCalled();
    fireEvent.pointerDown(document.body);
    expect(p.onClose).toHaveBeenCalledOnce();
  });

  it('a pointerdown on its own anchor does not close it (the row decides)', () => {
    const anchor = anchorButton();
    const p = props({ anchor });
    render(<ProfileCard {...p} />);
    fireEvent.pointerDown(anchor);
    expect(p.onClose).not.toHaveBeenCalled();
  });

  it('MP7: a pointerdown on the avatar of the author whose name opened it does not close it (the click toggles)', () => {
    const row = document.createElement('article');
    row.className = 'd-message-row';
    const avatar = document.createElement('span');
    avatar.dataset.profile = 'true';
    const name = document.createElement('button');
    name.className = 'd-message-row__author-open';
    row.append(avatar, name);
    document.body.appendChild(row);
    const p = props({ anchor: name });
    render(<ProfileCard {...p} />);
    fireEvent.pointerDown(avatar);
    expect(p.onClose).not.toHaveBeenCalled();
    fireEvent.pointerDown(document.body);
    expect(p.onClose).toHaveBeenCalledTimes(1);
  });
});

describe('ProfileCard placement', () => {
  const rect = (left: number, top: number, width = 200, height = 44) => ({ left, top, width, height, right: left + width, bottom: top + height, x: left, y: top, toJSON: () => ({}) });

  it('is a popover by default, placed fixed beside the row: left of it when there is room', () => {
    const anchor = anchorButton();
    vi.spyOn(anchor, 'getBoundingClientRect').mockReturnValue(rect(900, 100));
    render(<ProfileCard {...props({ anchor })} />);
    const card = screen.getByRole('dialog');
    expect(card).toHaveAttribute('data-layout', 'popover');
    expect(card).toHaveStyle({ position: 'fixed' });
    expect(parseFloat(card.style.left)).toBeLessThan(900);
    expect(card.style.top).toBe('100px');
  });

  it('MP7: placement "below" sits under the anchor at its left edge, and above it when there is no room below', () => {
    const anchor = anchorButton();
    const spy = vi.spyOn(anchor, 'getBoundingClientRect').mockReturnValue(rect(300, 100, 60, 20));
    vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockReturnValue(200);
    render(<ProfileCard {...props({ anchor, placement: 'below' })} />);
    const card = screen.getByRole('dialog');
    expect(card.style.left).toBe('300px');
    expect(card.style.top).toBe('128px');
    spy.mockReturnValue(rect(300, window.innerHeight - 40, 60, 20));
    fireEvent(window, new Event('resize'));
    expect(card.style.top).toBe(`${window.innerHeight - 40 - 8 - 200}px`);
    vi.restoreAllMocks();
  });

  it('goes right of the row when there is no room on its left', () => {
    const anchor = anchorButton();
    vi.spyOn(anchor, 'getBoundingClientRect').mockReturnValue(rect(4, 100));
    render(<ProfileCard {...props({ anchor })} />);
    expect(parseFloat(screen.getByRole('dialog').style.left)).toBeGreaterThanOrEqual(204);
  });

  it('M4: re-places the popover on a window resize and on a scroll anywhere', () => {
    const anchor = anchorButton();
    const spy = vi.spyOn(anchor, 'getBoundingClientRect').mockReturnValue(rect(900, 100));
    render(<ProfileCard {...props({ anchor })} />);
    const card = screen.getByRole('dialog');
    expect(card.style.top).toBe('100px');
    spy.mockReturnValue(rect(900, 300));
    fireEvent(window, new Event('resize'));
    expect(card.style.top).toBe('300px');
    spy.mockReturnValue(rect(900, 350));
    fireEvent.scroll(document.body);
    expect(card.style.top).toBe('350px');
  });

  it('N3: closes when its anchor is no longer connected on a reposition', () => {
    const anchor = anchorButton();
    const p = props({ anchor });
    render(<ProfileCard {...p} />);
    anchor.remove();
    fireEvent(window, new Event('resize'));
    expect(p.onClose).toHaveBeenCalledOnce();
  });

  it('is a sheet when asked to, or inside a phone shell: a modal dialog on a scrim, with no position of its own', () => {
    const { unmount } = render(<ProfileCard {...props({ layout: 'sheet' })} />);
    expect(screen.getByRole('dialog')).toHaveAttribute('data-layout', 'sheet');
    expect(screen.getByRole('dialog')).toHaveAttribute('aria-modal', 'true');
    expect(screen.getByRole('dialog').style.left).toBe('');
    unmount();
    render(<PhoneLayoutProvider value><ProfileCard {...props()} /></PhoneLayoutProvider>);
    expect(screen.getByRole('dialog')).toHaveAttribute('data-layout', 'sheet');
  });

  it('a click on the sheet\'s scrim closes it; a click inside does not, and a sheet ignores pointerdown outside', () => {
    const p = props({ layout: 'sheet' });
    const { container } = render(<ProfileCard {...p} />);
    fireEvent.click(screen.getByRole('dialog').querySelector('.d-profile-card__status')!);
    expect(p.onClose).not.toHaveBeenCalled();
    fireEvent.click(container.querySelector('.d-profile-card__scrim')!);
    expect(p.onClose).toHaveBeenCalledOnce();
  });

  // Task 9 (Signal §5.7): the sheet's 16px top corners and d-sheet-up; the popover 300px with the card shadow and d-modal 260ms.
  it('CSS pins: the sheet sits at the bottom with 16px top corners and slides up, the popover is 300px with the card shadow', () => {
    expect(css).toMatch(/\.d-profile-card\[data-layout="sheet"\] \{[^}]*bottom: 0[^}]*border-radius: var\(--r-sheet\) var\(--r-sheet\) 0 0[^}]*animation-name: d-sheet-up/);
    expect(css).toMatch(/\.d-profile-card \{[^}]*width: 300px[^}]*box-shadow: var\(--shadow-profile\)[^}]*animation: d-modal var\(--duration-profile\)/);
  });
});

describe('ProfileCard accessibility', () => {
  it.each(['signal', 'light', 'high-contrast'])('has no serious axe violations in %s, as popover and as sheet', async theme => {
    document.documentElement.dataset.theme = theme;
    for (const layout of ['popover', 'sheet'] as const) {
      const { container, unmount } = render(<div className="d-root"><ProfileCard {...props({ layout, status: 'in a meeting', bio: 'line one\nline two' })} /></div>);
      await expectNoAxeViolations(container);
      unmount();
    }
  });
});

describe('ProfileCard presence words (M2)', () => {
  it('reads the presence word from presenceLabels', () => {
    render(<ProfileCard {...props({ presence: 'dnd', presenceLabels: { dnd: 'do not disturb' } })} />);
    expect(screen.getByRole('dialog').querySelector('.d-profile-card__status')).toHaveTextContent('@astrid · do not disturb');
    expect(screen.queryByText('Do Not Disturb')).toBeNull();
  });
});

describe('ProfileCard name only (MP7)', () => {
  it('draws no @username line, roles, marks or actions for a person who is no longer a member', () => {
    const { container } = render(<ProfileCard {...props({ username: '', presence: undefined, isOwner: false, isYou: false, roles: [], actions: [] })} />);
    expect(screen.getByRole('dialog', { name: 'Astrid' })).toBeInTheDocument();
    expect(container.querySelector('.d-profile-card__username')).toBeNull();
    expect(screen.queryByText('@')).toBeNull();
  });
});
