import { readFileSync } from 'node:fs';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { ChannelHeader } from './ChannelHeader.tsx';
import { IconButton } from '../IconButton/IconButton.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const css = readFileSync(`${process.cwd()}/src/ChannelHeader/ChannelHeader.css`, 'utf8');

describe('ChannelHeader', () => {
  it('is one 44 bar row (Signal §5.5): it never wraps, the name keeps 8rem, the actions never shrink (CSS pins, B1, C1)', () => {
    expect(css).not.toMatch(/^\.d-channel-header \{[^}]*flex-wrap/m);
    expect(css).toMatch(/\.d-channel-header \{[^}]*[^-]height: var\(--bar-h\)/m);
    expect(css).not.toContain('--channel-header-h');
    expect(css).not.toContain('data-main-narrow');
    expect(css).toMatch(/^\.d-channel-header__title \{[^}]*min-width: min\(8rem, 100%\); min-width: calc-size\(max-content, min\(size, 8rem\)\)/m);
    expect(css).toMatch(/^\.d-channel-header__name \{[^}]*text-overflow: ellipsis/m);
    expect(css).toMatch(/^\.d-channel-header__actions \{[^}]*flex-shrink: 0/m);
    expect(css).toMatch(/^\.d-channel-header \{[^}]*padding: 0 10px 0 20px;[^}]*/m);
    expect(css).toMatch(/^\.d-channel-header__topic \{[^}]*font-size: var\(--text-ui\);[^}]*color: var\(--fg-2\);/m);
    expect(css).toMatch(/\[aria-pressed="true"\] \{ color: var\(--accent\); background: var\(--accent-soft\);/);
    expect(css).toMatch(/^\.d-channel-header \{[^}]*container: d-channel-header \/ inline-size/m);
  });

  it('Ruling U: with overflow.always the "more" menu shows at every width (data-overflow="always", CSS pin)', () => {
    const { container } = render(<ChannelHeader name="general" overflow={{ label: 'more', items: [{ id: 'pins', label: 'pinned messages' }], onSelect: () => {}, always: true }} />);
    expect(container.querySelector('.d-channel-header')).toHaveAttribute('data-overflow', 'always');
    expect(css).toMatch(/\.d-channel-header\[data-overflow="always"\] \.d-channel-header__more \{ display: inline-flex; \}/);
  });

  it('folds the foldable actions into the "more" menu below 36rem of header width (CSS pin, R27)', () => {
    const q = css.match(/@container d-channel-header \(max-width: 36rem\) \{([\s\S]*?)\n\}/);
    expect(q).not.toBeNull();
    expect(q![1]).toMatch(/\[data-foldable\][^{]*\{[^}]*display: none/);
    expect(q![1]).toMatch(/\.d-channel-header \.d-channel-header__more \{[^}]*display: inline-flex/);
    // Two classes: ActionMenu.css sets `.d-action-menu { display: inline-block }` later in the bundle, and one class would lose to it.
    expect(css).toMatch(/^\.d-channel-header \.d-channel-header__more \{[^}]*display: none/m);
  });

  it('puts the foldable actions into a "more" menu named by overflow.label, with the same items', async () => {
    const onSelect = vi.fn();
    const items = [{ id: 'pins', label: 'pinned messages' }, { id: 'threads', label: 'threads' }];
    render(<ChannelHeader name="general" overflow={{ label: 'more', items, onSelect }}
      actions={<><IconButton icon="pin" label="pinned messages" data-foldable="true" /><IconButton icon="users" label="members" /></>} />);
    const more = screen.getByRole('button', { name: 'more' });
    expect(more).toHaveAttribute('aria-haspopup', 'menu');
    await userEvent.setup().click(more);
    expect(within(screen.getByRole('menu', { name: 'more' })).getAllByRole('menuitem').map(i => i.textContent)).toEqual(['pinned messages', 'threads']);
    await userEvent.setup().click(screen.getByRole('menuitem', { name: 'threads' }));
    expect(onSelect).toHaveBeenCalledExactlyOnceWith('threads');
    // The fold is CSS: both the foldable button and the menu are in the tree, the foldable one marked.
    expect(screen.getByRole('button', { name: 'pinned messages' })).toHaveAttribute('data-foldable', 'true');
    expect(screen.getByRole('button', { name: 'members' })).not.toHaveAttribute('data-foldable');
  });

  it('without overflow a foldable action is never folded away (CSS pin: the fold needs data-overflow)', () => {
    const { container } = render(<ChannelHeader name="general" actions={<IconButton icon="pin" label="pinned messages" data-foldable="true" />} />);
    expect(container.querySelector('.d-channel-header')).not.toHaveAttribute('data-overflow');
    const q = css.match(/@container d-channel-header \(max-width: 36rem\) \{([\s\S]*?)\n\}/)![1]!;
    expect(q).toMatch(/\.d-channel-header\[data-overflow\] \.d-channel-header__actions \[data-foldable\]/);
    expect(q).not.toMatch(/(^|\n)\s*\.d-channel-header__actions \[data-foldable\]/);
  });

  it('has no "more" menu without overflow', () => {
    const { container } = render(<ChannelHeader name="general" actions={<IconButton icon="pin" label="pinned messages" />} />);
    expect(container.querySelector('.d-channel-header__more')).toBeNull();
  });

  it('actions are a cluster of 32px icon buttons; a pressed toggle has aria-pressed', () => {
    render(<ChannelHeader name="general" actions={<><IconButton icon="pin" label="pinned messages" pressed /><IconButton icon="users" label="members" pressed={false} /></>} />);
    expect(screen.getByRole('button', { name: 'pinned messages' })).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByRole('button', { name: 'pinned messages' })).toHaveAttribute('data-size', '32');
    expect(screen.getByRole('button', { name: 'members' })).toHaveAttribute('aria-pressed', 'false');
  });

  it('names the channel in a level-two heading without the decorative brackets', () => {
    const { container } = render(<ChannelHeader name="general" />);
    expect(container.firstElementChild?.tagName).toBe('HEADER');
    expect(container.firstElementChild).toHaveClass('d-channel-header');
    expect(screen.getByRole('heading', { level: 2, name: 'general' })).toBeInTheDocument();
    expect(container.querySelector('.d-channel-header__topic')).toBeNull();
    expect(screen.queryByRole('img')).toBeNull();
  });

  it('shows the topic when there is one', () => {
    render(<ChannelHeader name="general" topic="evening plans, screenshots and the odd argument" />);
    expect(screen.getByText('evening plans, screenshots and the odd argument')).toHaveClass('d-channel-header__topic');
  });

  it('shows the readable glyph with its default name, or the name the caller gives', () => {
    const { rerender } = render(<ChannelHeader name="lfg" readable />);
    expect(screen.getByRole('img', { name: 'Readable by this server' }).querySelector('[data-icon="eye"]')).not.toBeNull();
    rerender(<ChannelHeader name="lfg" readable readableLabel="readable by dilla.thim.dev" />);
    expect(screen.getByRole('img', { name: 'readable by dilla.thim.dev' })).toBeInTheDocument();
  });

  it('holds nothing focusable', () => {
    const { container } = render(<ChannelHeader name="lfg" topic="looking for group" readable />);
    expect(container.querySelectorAll('a, button, input, textarea, select, [tabindex]')).toHaveLength(0);
  });

  it('heads a channel with the hash glyph and a direct message with the at-sign, named by the name alone (no brackets)', () => {
    const { container, rerender } = render(<ChannelHeader name="ada" kind="dm" />);
    expect(screen.getByRole('heading', { level: 2, name: 'ada' })).toBeInTheDocument();
    expect(container.querySelector('.d-channel-header__glyph [data-icon="at-sign"]')).not.toBeNull();
    expect(container.querySelector('.d-channel-header__glyph')).toHaveAttribute('aria-hidden', 'true');
    expect(container.querySelector('.d-channel-header__title')?.textContent).toBe('ada');
    expect(container.querySelector('.d-channel-header__bracket')).toBeNull();
    rerender(<ChannelHeader name="general" kind="channel" />);
    expect(container.querySelector('.d-channel-header__glyph [data-icon="hash"]')).not.toBeNull();
    expect(container.querySelector('.d-channel-header__title')?.textContent).toBe('general');
  });

  it('heads an announcement channel with the megaphone in place of #, named by the name alone', () => {
    const { container } = render(<ChannelHeader name="news" kind="announcement" />);
    expect(screen.getByRole('heading', { level: 2, name: 'news' })).toBeInTheDocument();
    // C3: the megaphone is its own flex item beside the name, never inside it.
    const glyph = container.querySelector('[data-icon="megaphone"]')!.closest('.d-channel-header__glyph')!;
    const name = screen.getByRole('heading', { level: 2, name: 'news' });
    expect(glyph.parentElement).toBe(name.parentElement);
    expect(name).not.toContainElement(glyph as HTMLElement);
    expect(container.textContent).not.toContain('#');
    expect(container.querySelector('.d-channel-header__title')?.textContent).toBe('news');
  });

  it('heads a voice channel with the volume glyph in place of #, named by the name alone', () => {
    const { container } = render(<ChannelHeader name="harbour" kind="voice" />);
    expect(screen.getByRole('heading', { level: 2, name: 'harbour' })).toBeInTheDocument();
    expect(container.querySelector('[data-icon="volume-2"]')?.closest('.d-channel-header__glyph')).not.toBeNull();
    expect(container.textContent).not.toContain('#');
    expect(container.querySelector('.d-channel-header__title')?.textContent).toBe('harbour');
  });

  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><ChannelHeader name="lfg" topic="looking for group" readable /></div>);
    await expectNoAxeViolations(container);
  });

  it('renders its actions after the topic', () => {
    const { container } = render(<ChannelHeader name="general" topic="say hi" actions={<button type="button">pinned</button>} />);
    const actions = container.querySelector('.d-channel-header__actions')!;
    expect(actions).toContainElement(screen.getByRole('button', { name: 'pinned' }));
    expect(container.querySelector('.d-channel-header__topic')!.compareDocumentPosition(actions) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  describe('phone', () => {
    it('renders a PhoneHeader with the back button, the glyph and channel heading and the menu', async () => {
      const onBack = vi.fn();
      const { container } = render(<ChannelHeader name="general" phone={{ backLabel: 'back to channels', onBack, menu: <button type="button">more</button> }} />);
      expect(container.querySelector('.d-phone-header')).not.toBeNull();
      expect(container.querySelector('.d-channel-header')).toBeNull();
      expect(container.querySelector('.d-channel-header__actions')).toBeNull();
      const heading = screen.getByRole('heading', { level: 2, name: 'general' });
      expect(heading.textContent).toBe('general');
      expect(heading.querySelector('[data-icon="hash"]')).not.toBeNull();
      expect(screen.getByRole('button', { name: 'more' })).toBeInTheDocument();
      await userEvent.setup().click(screen.getByRole('button', { name: 'back to channels' }));
      expect(onBack).toHaveBeenCalledTimes(1);
    });
    it('uses the megaphone for an announcement channel', () => {
      const { container } = render(<ChannelHeader kind="announcement" name="news" phone={{ backLabel: 'back', onBack: () => {} }} />);
      expect(screen.getByRole('heading', { level: 2, name: 'news' }).textContent).toBe('news');
      expect(container.querySelector('[data-icon="megaphone"]')).not.toBeNull();
      expect(container.textContent).not.toContain('#');
    });
    it('uses the at-sign for a direct message', () => {
      render(<ChannelHeader kind="dm" name="ada" phone={{ backLabel: 'back', onBack: () => {} }} />);
      const h = screen.getByRole('heading', { level: 2, name: 'ada' });
      expect(h.textContent).toBe('ada');
      expect(h.querySelector('[data-icon="at-sign"]')).not.toBeNull();
    });
    it('keeps the readable cue next to the title on a phone', () => {
      const { container } = render(<ChannelHeader name="lfg" readable readableLabel="readable by dilla.thim.dev" phone={{ backLabel: 'back', onBack: () => {} }} />);
      const cue = screen.getByRole('img', { name: 'readable by dilla.thim.dev' });
      expect(cue.querySelector('[data-icon="eye"]')).not.toBeNull();
      expect(container.querySelector('.d-phone-header')).toContainElement(cue);
      expect(screen.getByRole('heading', { level: 2 })).toHaveAccessibleName('lfg');
    });
    it('shows no cue on a phone for a channel the server cannot read', () => {
      render(<ChannelHeader name="lfg" phone={{ backLabel: 'back', onBack: () => {} }} />);
      expect(screen.queryByRole('img')).toBeNull();
    });
    it('without phone the markup is the desktop header', () => {
      const { container } = render(<ChannelHeader name="general" />);
      expect(container.querySelector('.d-phone-header')).toBeNull();
      expect(container.querySelector('header')).toHaveClass('d-channel-header');
    });
  });
});
