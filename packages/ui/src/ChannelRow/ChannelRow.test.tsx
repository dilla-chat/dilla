import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { ChannelRow } from './ChannelRow.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

describe('ChannelRow', () => {
  it('shows the megaphone for an announcement channel and keeps its accessible name', () => {
    render(<ChannelRow name="news" kind="announcement" onSelect={() => {}} />);
    const row = screen.getByRole('button', { name: 'news' });
    expect(row).toHaveAttribute('data-kind', 'announcement');
    expect(row.querySelector('svg[data-icon="megaphone"]')).not.toBeNull();
    expect(row.querySelector('.d-chrow__glyph')?.textContent).not.toContain('#');
  });
  it('shows a badge as text after the name, inside the row’s accessible name', () => {
    render(<ChannelRow name="general" kind="text" badge="slow mode" onSelect={() => {}} />);
    const row = screen.getByRole('button', { name: 'general slow mode' });
    const badge = screen.getByText('slow mode');
    expect(badge).toHaveClass('d-chrow__badge');
    const name = row.querySelector('.d-chrow__name') as Node;
    expect(Boolean(name.compareDocumentPosition(badge) & Node.DOCUMENT_POSITION_FOLLOWING)).toBe(true);
  });
  it('shows no badge by default', () => {
    const { container } = render(<ChannelRow name="general" kind="text" onSelect={() => {}} />);
    expect(container.querySelector('.d-chrow__badge')).toBeNull();
  });
  it('is a button named after the channel and selectable by keyboard', async () => {
    const onSelect = vi.fn();
    render(<ChannelRow name="loot" kind="text" onSelect={onSelect} />);
    const row = screen.getByRole('button', { name: /^loot/ });
    row.focus();
    await userEvent.keyboard('{Enter}');
    expect(onSelect).toHaveBeenCalledTimes(1);
  });
  it('marks the active channel with aria-current', () => {
    render(<ChannelRow name="loot" kind="text" active onSelect={() => {}} />);
    expect(screen.getByRole('button', { name: /^loot/ })).toHaveAttribute('aria-current', 'page');
  });
  it('shows the unread pill, the mention pill (which replaces unread), and the readable glyph', () => {
    render(<><ChannelRow name="general" kind="text" unread={3} onSelect={() => {}} /><ChannelRow name="screenshots" kind="text" unread={3} mentions={12} onSelect={() => {}} /><ChannelRow name="lfg" kind="text" readable onSelect={() => {}} /></>);
    // The count reaches assistive tech as part of the row's own name, not
    // as a separate live region that interrupts.
    expect(screen.getByRole('button', { name: 'general 3 unread' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'screenshots 12 mentions' })).toBeInTheDocument();
    expect(screen.queryAllByRole('button', { name: /3 unread/ })).toHaveLength(1);
    expect(screen.getByRole('img', { name: 'Readable by this server' })).toBeInTheDocument();
  });
  it('shows the lock on private voice channels and a muted state', () => {
    render(<ChannelRow name="sauna" kind="voice" private muted onSelect={() => {}} />);
    const row = screen.getByRole('button', { name: /^sauna/ });
    expect(row).toHaveAttribute('data-muted', 'true');
    expect(screen.getByRole('img', { name: 'Private' })).toBeInTheDocument();
  });
  it('carries the shown unread count, the mention count and the muted flag as data', () => {
    render(<>
      <ChannelRow name="general" kind="text" unread={3} onSelect={() => {}} />
      <ChannelRow name="random" kind="text" unread={7} mentions={2} muted onSelect={() => {}} />
      <ChannelRow name="lfg" kind="text" onSelect={() => {}} />
    </>);
    const general = screen.getByRole('button', { name: 'general 3 unread' });
    expect(general).toHaveAttribute('data-unread', '3');
    expect(general).toHaveAttribute('data-mentions', '0');
    expect(general).not.toHaveAttribute('data-muted');
    const random = screen.getByRole('button', { name: 'random 2 mentions' });
    expect(random).toHaveAttribute('data-unread', '0');
    expect(random).toHaveAttribute('data-mentions', '2');
    expect(random).toHaveAttribute('data-muted', 'true');
    const lfg = screen.getByRole('button', { name: 'lfg' });
    expect(lfg).toHaveAttribute('data-unread', '0');
    expect(lfg).toHaveAttribute('data-mentions', '0');
  });
  it('hides the unread pill of a muted row and keeps its mention pill', () => {
    render(<>
      <ChannelRow name="random" kind="text" unread={7} muted onSelect={() => {}} />
      <ChannelRow name="loot" kind="text" unread={7} mentions={2} muted onSelect={() => {}} />
    </>);
    expect(screen.getByRole('button', { name: 'random' })).toBeInTheDocument();
    expect(screen.queryByText('7 unread')).toBeNull();
    expect(screen.getByRole('button', { name: 'loot 2 mentions' })).toBeInTheDocument();
  });
  it('draws a direct message with a decorative dot in place of the channel glyph', () => {
    const { container } = render(<ChannelRow name="ada" kind="dm" unread={2} onSelect={() => {}} />);
    const row = screen.getByRole('button', { name: 'ada 2 unread' });
    expect(row).toHaveAttribute('data-kind', 'dm');
    const glyph = container.querySelector('.d-chrow__glyph');
    expect(glyph).toHaveAttribute('aria-hidden', 'true');
    expect(glyph?.querySelector('.d-chrow__dot')).not.toBeNull();
    expect(row.textContent).not.toMatch(/[#♪]/);
  });
  it('takes its accessible name from ariaLabel when one is given', () => {
    render(<ChannelRow name="general" kind="text" unread={3} mentions={1} ariaLabel="general, unread 3, mentions 1" onSelect={() => {}} />);
    expect(screen.getByRole('button', { name: 'general, unread 3, mentions 1' })).toHaveAttribute('aria-label', 'general, unread 3, mentions 1');
  });
  // web3.9 (UI-2): one row grammar.
  it('holds the mention pill in the trailing slot when both counts are set, and the unread pill when only that is', () => {
    const { container, rerender } = render(<ChannelRow name="screenshots" kind="text" unread={3} mentions={12} onSelect={() => {}} />);
    const slot = container.querySelector('.d-slot');
    expect(slot?.querySelectorAll('.d-pill')).toHaveLength(1);
    expect(slot?.querySelector('.d-pill')).toHaveAttribute('data-kind', 'mention');
    expect(slot?.parentElement).toBe(screen.getByRole('button'));
    rerender(<ChannelRow name="screenshots" kind="text" unread={3} onSelect={() => {}} />);
    expect(container.querySelector('.d-slot .d-pill')).toHaveAttribute('data-kind', 'unread');
    rerender(<ChannelRow name="screenshots" kind="text" onSelect={() => {}} />);
    expect(container.querySelector('.d-slot')).not.toBeNull();
    expect(container.querySelector('.d-slot .d-pill')).toBeNull();
  });
  it('draws the lock as a 12px Lucide icon named Private, after the name, and no lock emoji', () => {
    const { container } = render(<ChannelRow name="sauna" kind="voice" private onSelect={() => {}} />);
    const lock = screen.getByRole('img', { name: 'Private' });
    const icon = lock.querySelector('svg');
    expect(icon).toHaveAttribute('data-icon', 'lock');
    expect(icon).toHaveAttribute('width', '12');
    const name = container.querySelector('.d-chrow__name') as Node;
    expect(Boolean(name.compareDocumentPosition(lock) & Node.DOCUMENT_POSITION_FOLLOWING)).toBe(true);
    expect(container.textContent).not.toContain('🔒');
  });
  it('is a data-row, with the glyph column holding a 14px icon: hash, volume-2, moon (Signal §5.4)', () => {
    const { container } = render(<><ChannelRow name="a" kind="text" onSelect={() => {}} /><ChannelRow name="b" kind="voice" onSelect={() => {}} /><ChannelRow name="AFK" kind="afk" onSelect={() => {}} /></>);
    for (const row of container.querySelectorAll('.d-chrow')) expect(row).toHaveAttribute('data-row');
    expect(container.querySelector('.d-chrow[data-kind="text"] .d-chrow__glyph svg')).toHaveAttribute('data-icon', 'hash');
    expect(container.querySelector('.d-chrow[data-kind="text"] .d-chrow__glyph svg')).toHaveAttribute('width', '14');
    expect(container.querySelector('.d-chrow[data-kind="afk"] .d-chrow__glyph svg')).toHaveAttribute('data-icon', 'moon');
    expect(container.querySelector('.d-chrow[data-kind="voice"] .d-chrow__glyph svg')).toHaveAttribute('data-icon', 'volume-2');
  });
  it('draws the unread tag as "{n} new" and the mention tag as "@{n}", the hidden count still naming the row', () => {
    render(<><ChannelRow name="loot" kind="text" unread={1} newLabel={n => `${n} new`} onSelect={() => {}} /><ChannelRow name="raids" kind="text" mentions={2} onSelect={() => {}} /></>);
    const loot = screen.getByRole('button', { name: 'loot 1 unread' });
    expect(loot.querySelector('.d-pill [aria-hidden="true"]')?.textContent).toBe('1 new');
    const raids = screen.getByRole('button', { name: 'raids 2 mentions' });
    expect(raids.querySelector('.d-pill [aria-hidden="true"]')?.textContent).toBe('@2');
  });
  it('carries the active bar as a decorative element (it grows with scaleY in CSS)', () => {
    const { container } = render(<ChannelRow name="general" kind="text" active onSelect={() => {}} />);
    expect(container.querySelector('.d-chrow__bar')).toHaveAttribute('aria-hidden', 'true');
  });
  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><ChannelRow name="loot" kind="text" active unread={4} onSelect={() => {}} /></div>);
    await expectNoAxeViolations(container);
  });
});

// Task 11 (Jonas, Q2): a DM drawn as a person.
describe('ChannelRow person (a DM row)', () => {
  it('shows the avatar, the name over its sub, keeps the name as the row name and the sub as its description, and a dot for unread', async () => {
    const { container } = render(<div className="d-root"><ChannelRow name="Björn" kind="dm" unread={1} onSelect={vi.fn()}
      person={{ presence: 'online', sub: 'bridge troll survivor' }} /></div>);
    const row = screen.getByRole('button', { name: /^Björn/ });
    expect(row).toHaveAttribute('data-person', 'true');
    expect(row).toHaveAccessibleName('Björn 1 unread');
    expect(row).toHaveAttribute('aria-description', 'bridge troll survivor');
    expect(container.querySelector('.d-avatar')).not.toBeNull();
    expect(container.querySelector('.d-avatar__presence')).not.toBeNull();
    expect(container.querySelector('.d-chrow__unread-dot')).not.toBeNull();
    expect(container.querySelector('.d-pill')).toBeNull();
    await expectNoAxeViolations(container);
  });
  it('draws a group DM with the neutral avatar and no presence dot', () => {
    const { container } = render(<ChannelRow name="raid crew" kind="dm" onSelect={vi.fn()} person={{ group: true, sub: 'Ada, Björn, you', initials: 'RC' }} />);
    expect(container.querySelector('.d-chrow')).toHaveAttribute('data-group', 'true');
    expect(container.querySelector('.d-avatar__presence')).toBeNull();
    expect(container.querySelector('.d-avatar__initials')).toHaveTextContent('RC');
  });
});
