import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, it, expect, vi } from 'vitest';
import { axe } from 'vitest-axe';
import { groupPeople, VoiceChannelRow, type VoiceChannelRowProps } from './VoiceChannelRow.tsx';
import { ChannelList } from '../ChannelList/ChannelList.tsx';
import type { VoiceParticipantView } from '../CallTile/CallTile.tsx';
import meta from './VoiceChannelRow.stories.tsx';

const person = (name: string, more: Partial<VoiceParticipantView> = {}): VoiceParticipantView => ({
  deviceId: `d-${name}`, userId: `u-${name}`, name, state: 'ok', speaking: false, muted: false, deafened: false,
  video: false, screen: false, web: true, bot: false, ...more,
});
const labels: VoiceChannelRowProps['labels'] = {
  open: 'voice {name}', count: '{n} in voice', playbackOnly: 'playback only', join: 'join voice', joined: 'connected',
  state: {
    speaking: 'speaking', muted: 'muted', deafened: 'deafened', notHeard: 'not heard yet', unverified: 'unverified stream',
    connecting: 'connecting', video: 'camera on', screen: 'sharing screen', web: 'web', bot: 'bot', noSound: 'no sound',
  },
};
const base: VoiceChannelRowProps = {
  name: 'harbour', current: false, joined: false, participants: [person('ada', { speaking: true }), person('bo', { muted: true })], labels, onOpen: () => {},
};

afterEach(() => { delete document.documentElement.dataset.theme; });

/** The text a screen reader reads: aria-hidden glyphs left out, white space collapsed. */
function spoken(el: Element): string {
  const copy = el.cloneNode(true) as Element;
  copy.querySelectorAll('[aria-hidden="true"]').forEach(n => n.remove());
  return (copy.textContent ?? '').replace(/\s+/g, ' ').trim();
}

describe('VoiceChannelRow', () => {
  it('is one button named for the voice, with the count, and the people as text below it', async () => {
    const onOpen = vi.fn();
    render(<VoiceChannelRow {...base} onOpen={onOpen} />);
    const row = screen.getByRole('button', { name: 'voice harbour' });
    expect(row).toHaveAccessibleDescription('2 in voice');
    expect(screen.getAllByRole('button')).toEqual([row]);
    const people = screen.getByRole('list');
    expect(within(people).getAllByRole('listitem').map(spoken)).toEqual(['ada speaking', 'bo']);
    expect(within(people).queryByRole('img', { name: 'speaking' })).toBeNull(); // the ring is the only visible mark; the word is sr-only
    expect(people.querySelector('.d-sr-only')).toHaveTextContent('speaking');
    expect(within(people).getByRole('img', { name: 'muted' })).toBeInTheDocument();
    await userEvent.click(row);
    expect(onOpen).toHaveBeenCalledTimes(1);
  });

  it('shows the occupant count at the right of the row (Signal §5.4), and no count when nobody is in it', () => {
    const { container, rerender } = render(<VoiceChannelRow {...base} participants={[person('a'), person('b'), person('c')]} />);
    expect(container.querySelector('.d-voice-row__title')?.textContent).toBe('harbour');
    expect(container.querySelector('.d-voice-row__count')?.textContent).toBe('3');
    expect(container.querySelector('.d-voice-row__count')).toHaveAttribute('aria-hidden', 'true');
    rerender(<VoiceChannelRow {...base} participants={[]} />);
    expect(container.querySelector('.d-voice-row__count')).toBeNull();
  });
  it('draws the moon for the AFK channel', () => {
    const { container } = render(<VoiceChannelRow {...base} name="AFK" afk participants={[]} />);
    expect(container.querySelector('.d-voice-row__glyph svg')).toHaveAttribute('data-icon', 'moon');
  });
  it('speaking is a ring only: no AudioMeter, meter or level element inside the row', () => {
    const { container } = render(<VoiceChannelRow {...base} joined current participants={[person('ada', { speaking: true }), person('bo')]} />);
    expect(container.querySelector('[role="meter"], .d-audio-meter, .d-meter, meter')).toBeNull();
    expect(container.querySelector('.d-voice-row__person[data-speaking="true"] .d-avatar')).not.toBeNull();
  });
  it('Glenn (a bot) has the rounded-square avatar and "playback only"; the bot word is for a screen reader only (fix round 1)', () => {
    const { container } = render(<VoiceChannelRow {...base} participants={[person('Glenn', { bot: true, web: false })]} />);
    const row = container.querySelector('.d-voice-row__person') as HTMLElement;
    expect(row.querySelector('.d-avatar')).toHaveAttribute('data-shape', 'bot');
    expect(row.querySelector('.d-tag[data-kind="bot"]')).toBeNull();
    expect(row.querySelector('.d-sr-only')).toHaveTextContent('bot');
    expect(row.querySelector('.d-tag[data-kind="playbackOnly"]')).toHaveTextContent('playback only');
  });

  it('the trailing slot holds the join icon button; when joined (in the call) the row has no phone glyph and no join icon', async () => {
    const onJoin = vi.fn();
    const { container, rerender } = render(<VoiceChannelRow {...base} onJoin={onJoin} />);
    const join = screen.getByRole('button', { name: 'join voice' });
    expect(join.closest('.d-slot')).not.toBeNull();
    expect(container.querySelector('.d-voice-row__connected')).toBeNull();
    await userEvent.click(join);
    expect(onJoin).toHaveBeenCalledTimes(1);
    rerender(<VoiceChannelRow {...base} joined onJoin={onJoin} />);
    expect(screen.queryByRole('button', { name: 'join voice' })).toBeNull();
    // Signal: with no join icon the row keeps no empty slot, so the count ends at the row's padding edge.
    expect(container.querySelector('.d-voice-row > .d-slot')).toBeNull();
    expect(container.querySelector('.d-voice-row__connected')).toBeNull();
    expect(container.querySelector('.d-voice-row')).toHaveAttribute('data-joined', 'true');
    rerender(<VoiceChannelRow {...base} />);
    expect(screen.queryByRole('button', { name: 'join voice' })).toBeNull();
  });

  it('a participant row is data-row at its own height with an 18px avatar, the speaking mark, and the mute glyph in the slot', () => {
    const { container } = render(<VoiceChannelRow {...base} participants={[person('ada', { speaking: true }), person('bo', { muted: true, deafened: true })]} />);
    const [ada, bo] = [...container.querySelectorAll<HTMLElement>('.d-voice-row__person')];
    expect(ada).toHaveAttribute('data-row');
    expect(ada).toHaveAttribute('data-speaking', 'true');
    expect(bo).not.toHaveAttribute('data-speaking');
    expect(ada.querySelector('.d-avatar')).toHaveAttribute('data-size', 'voice');
    const slot = bo.querySelector('.d-slot') as HTMLElement;
    // Signal §5.4: deafened draws headphone-off alone (it already silences the mic).
    expect([...slot.querySelectorAll('.d-voice-row__state')].map(s => s.getAttribute('data-state'))).toEqual(['deafened']);
    expect(slot.querySelector('svg')).toHaveAttribute('data-icon', 'headphone-off');
    expect(ada.querySelector('.d-slot')?.children).toHaveLength(0);
  });

  it('AW6: an away person (the AFK channel) is listed and counted, with the away word and no voice glyphs', () => {
    const { container } = render(<VoiceChannelRow {...base} labels={{ ...labels, away: 'away' }}
      participants={[person('ada', { away: true, web: false }), person('bo', { away: true, web: false })]} />);
    expect(screen.getByRole('button', { name: 'voice harbour' })).toHaveAccessibleDescription('2 in voice');
    expect(container.querySelector('.d-voice-row__count')?.textContent).toBe('2');
    const people = within(screen.getByRole('list')).getAllByRole('listitem');
    expect(people.map(spoken)).toEqual(['ada away', 'bo away']);
    for (const p of people) {
      expect(p).toHaveAttribute('data-away', 'true');
      expect(p.querySelector('[data-state="away"]')).toHaveTextContent('away');
      expect(p.querySelector('.d-slot')?.children).toHaveLength(0);
    }
    // A person in voice is not away.
    render(<VoiceChannelRow {...base} labels={{ ...labels, away: 'away' }} participants={[person('cy')]} />);
    expect(document.querySelectorAll('[data-away]')).toHaveLength(2);
  });

  it('the row marker is the volume-2 icon, live or not; live and joined are data attributes', () => {
    const { container, rerender } = render(<VoiceChannelRow {...base} participants={[]} />);
    expect(container.querySelector('.d-voice-row__glyph svg')).toHaveAttribute('data-icon', 'volume-2');
    expect(container.querySelector('.d-voice-row__glyph .d-state-dot')).toBeNull();
    expect(container.querySelector('.d-voice-row')).not.toHaveAttribute('data-live');
    rerender(<VoiceChannelRow {...base} participants={[person('bo', {})]} />);
    expect(container.querySelector('.d-voice-row__glyph svg')).toHaveAttribute('data-icon', 'volume-2');
    expect(container.querySelector('.d-voice-row')).toHaveAttribute('data-live', 'true');
  });

  it('the row you are connected to says "connected" in its description, after the count; its name stays "voice harbour"', () => {
    const { rerender } = render(<VoiceChannelRow {...base} />);
    expect(screen.getByRole('button', { name: 'voice harbour' })).toHaveAccessibleDescription('2 in voice');
    rerender(<VoiceChannelRow {...base} joined />);
    expect(screen.getByRole('button', { name: 'voice harbour' })).toHaveAccessibleDescription('2 in voice, connected');
    rerender(<VoiceChannelRow {...base} joined participants={[]} />);
    expect(screen.getByRole('button', { name: 'voice harbour' })).toHaveAccessibleDescription('connected');
  });
  it('with nobody in voice shows no count and no sub-rows; current and joined are marked', () => {
    const { rerender } = render(<VoiceChannelRow {...base} participants={[]} />);
    expect(screen.queryByRole('list')).toBeNull();
    expect(screen.getByRole('button', { name: 'voice harbour' })).not.toHaveAccessibleDescription();
    rerender(<VoiceChannelRow {...base} current joined participants={[]} />);
    const row = screen.getByRole('button', { name: 'voice harbour' });
    expect(row).toHaveAttribute('aria-current', 'page');
    expect(row).toHaveAttribute('data-joined', 'true');
  });

  it('AW6 fix round 1 (M4): a room may name its own count, as the AFK row does with {n} in AFK', () => {
    render(<ChannelList label="channels" title="harbour club" activeId={null} onSelect={() => {}} emptyLabel="none"
      channels={[{ id: 'v1', name: 'harbour', kind: 'voice', readable: false }, { id: 'afk', name: 'AFK', kind: 'voice', readable: false }]}
      voice={{ labels, rooms: { v1: { joined: false, participants: base.participants },
        afk: { joined: false, participants: [person('cy', { away: true })], count: '{n} in AFK' } } }} />);
    expect(screen.getByRole('button', { name: 'voice AFK' })).toHaveAccessibleDescription('1 in AFK');
    expect(screen.getByRole('button', { name: 'voice harbour' })).toHaveAccessibleDescription('2 in voice');
  });

  it('ChannelList renders voice channels as voice rows, still one roving tab stop', async () => {
    const onSelect = vi.fn();
    render(<ChannelList label="channels" title="harbour club" activeId="t1" onSelect={onSelect} emptyLabel="none"
      channels={[{ id: 't1', name: 'general', kind: 'text', readable: false }, { id: 'v1', name: 'harbour', kind: 'voice', readable: false },
        { id: 'v2', name: 'workshop', kind: 'voice', readable: false }]}
      voice={{ labels, rooms: { v1: { joined: true, participants: base.participants } } }} />);
    const harbour = screen.getByRole('button', { name: 'voice harbour' });
    expect(harbour).toHaveAccessibleDescription('2 in voice, connected');
    expect(harbour).toHaveAttribute('data-joined', 'true');
    expect(screen.getByRole('button', { name: 'voice workshop' })).toBeInTheDocument();
    expect(screen.getAllByRole('button').filter(b => b.tabIndex === 0)).toHaveLength(1);
    await userEvent.keyboard('{Tab}{ArrowDown}');
    expect(harbour).toHaveFocus();
    await userEvent.keyboard('{ArrowDown}');
    expect(screen.getByRole('button', { name: 'voice workshop' })).toHaveFocus();
    await userEvent.keyboard('{Enter}');
    expect(onSelect).toHaveBeenCalledWith('v2');
  });

  it('keeps the readable-by-this-server glyph of a voice channel, also inside ChannelList (I4)', () => {
    const { rerender } = render(<VoiceChannelRow {...base} readable />);
    expect(screen.getByRole('img', { name: 'Readable by this server' })).toBeInTheDocument();
    rerender(<ChannelList label="channels" title="harbour club" activeId={null} onSelect={() => {}} emptyLabel="none"
      channels={[{ id: 'v1', name: 'harbour', kind: 'voice', readable: true }]} voice={{ labels, rooms: {} }} />);
    expect(screen.getByRole('img', { name: 'Readable by this server' })).toBeInTheDocument();
  });

  it('shows a bot in voice with the bot tag and playback only before anyone joins (I5)', () => {
    render(<VoiceChannelRow {...base} participants={[person('relay', { bot: true, web: false })]} />);
    const row = screen.getByRole('listitem');
    expect(spoken(row)).toBe('relay bot playback only');
  });

  // web3.8 (R29), Signal §5.4: muted, deafened and camera are 12px icons, a shared screen the LIVE tag, speaking the ring alone (its word sr-only); each is a named image with no text.
  it('a glyph-only state is a role=img named and titled by its voice.state word, with no text node', () => {
    render(<VoiceChannelRow {...base} participants={[person('bo', { muted: true, deafened: true, video: true, screen: true, speaking: true })]} />);
    const row = screen.getByRole('listitem');
    const states = [...row.querySelectorAll<HTMLElement>('.d-voice-row__state')];
    // The mute and deafen glyphs are in the trailing slot, after the others.
    expect(states.map(s => s.dataset.state)).toEqual(['video', 'screen', 'deafened']);
    expect(states.map(s => s.getAttribute('aria-label'))).toEqual(['camera on', 'sharing screen', 'deafened']);
    expect(states.map(s => s.getAttribute('title'))).toEqual(['camera on', 'sharing screen', 'deafened']);
    for (const s of states) expect(s).toHaveAttribute('role', 'img');
    for (const s of states.filter(x => x.dataset.state !== 'screen')) expect(s.textContent).toBe('');
    expect(states[1].querySelector('.d-tag[data-kind="live"]')).not.toBeNull();
    expect(states.map(s => s.querySelector('svg')?.getAttribute('data-icon') ?? null)).toEqual(['video', null, 'headphone-off']);
    expect(states.map(s => s.querySelector('svg')?.getAttribute('width') ?? null)).toEqual(['12', null, '12']);
    expect(row.querySelector('.d-state-dot')).toBeNull(); // WEB-DOCK: speaking is the ring, never a dot
    expect(spoken(row)).toBe('bo speaking');
  });

  it('a muted camera participant shows two icons on one row, named by the labels, and no text besides the name', () => {
    render(<VoiceChannelRow {...base} participants={[person('bo', { muted: true, video: true })]} />);
    const row = screen.getByRole('listitem');
    expect(within(row).getAllByRole('img').map(i => i.getAttribute('aria-label'))).toEqual(['camera on', 'muted']);
    expect(row.querySelectorAll('svg')).toHaveLength(2);
    expect(spoken(row)).toBe('bo');
  });

  it('an unverified participant also shows the unverified stream word, after the icons', () => {
    render(<VoiceChannelRow {...base} participants={[person('bo', { muted: true, state: 'unverified' })]} />);
    const row = screen.getByRole('listitem');
    expect(row.querySelector('.d-voice-row__person-name')?.textContent).toBe('bo');
    expect(row.querySelector('[data-state="unverified"]')).toHaveTextContent('unverified stream');
    expect(spoken(row)).toBe('bounverified stream');
    const kids = [...row.querySelectorAll<HTMLElement>('.d-voice-row__state')].map(s => s.dataset.state);
    expect(kids).toEqual(['unverified', 'muted']);
    expect(row.querySelector('[data-state="unverified"]')).not.toHaveAttribute('role', 'img');
  });

  it('the old font glyphs are gone', () => {
    render(<VoiceChannelRow {...base} participants={[person('bo', { muted: true, deafened: true })]} />);
    expect(screen.getByRole('listitem').textContent).not.toMatch(/[⊘⊗]/);
  });

  it('the voice row marker is volume-2 and never carries a badge', () => {
    const { container } = render(<ChannelList label="channels" title="x" activeId={null} onSelect={() => {}} emptyLabel="none"
      channels={[{ id: 'v1', name: 'harbour', kind: 'voice', readable: false, unread: 3, mentions: 1 }]} voice={{ labels, rooms: {} }} />);
    expect(container.querySelector('.d-voice-row__glyph [data-icon="volume-2"]')).not.toBeNull();
    expect(container.querySelector('.d-chrow__badge, .d-chrow__mention, .d-chrow__count')).toBeNull();
    expect(container.querySelector('.d-voice-row')?.textContent).toBe('harbour');
  });

  it.each(['signal', 'light', 'high-contrast'])('passes axe at its default story args in %s', async theme => {
    document.documentElement.dataset.theme = theme;
    const { container } = render(<div className="d-root"><VoiceChannelRow {...(meta.args as VoiceChannelRowProps)} /></div>);
    // jsdom has no layout or canvas, so contrast is left to the Storybook axe run in a real browser.
    expect((await axe(container, { rules: { 'color-contrast': { enabled: false } } })).violations).toEqual([]);
  });

  describe('one row per person (A7)', () => {
    const device = (name: string, id: string, more: Partial<VoiceParticipantView> = {}) => person(name, { deviceId: id, ...more });
    const rowsOf = () => within(screen.getByRole('list')).getAllByRole('listitem');

    it('lists a person with two devices once, and counts people', () => {
      render(<VoiceChannelRow {...base} participants={[device('ada', 'd1'), device('ada', 'd2'), person('bo')]} />);
      expect(rowsOf()).toHaveLength(2);
      expect(screen.getByRole('button', { name: 'voice harbour' })).toHaveAccessibleDescription('2 in voice');
    });

    it('says speaking when any device speaks', () => {
      render(<VoiceChannelRow {...base} participants={[device('ada', 'd1'), device('ada', 'd2', { speaking: true })]} />);
      expect(rowsOf()).toHaveLength(1);
      expect(rowsOf()[0]).toHaveAttribute('data-speaking', 'true');
    });

    it('takes the worst stream state across devices', () => {
      const states = (a: VoiceParticipantView['state'], b: VoiceParticipantView['state']) =>
        groupPeople([device('ada', 'd1', { state: a }), device('ada', 'd2', { state: b })])[0].state;
      expect(states('ok', 'unverified')).toBe('unverified');
      expect(states('connecting', 'unverified')).toBe('unverified');
      expect(states('ok', 'connecting')).toBe('connecting');
      expect(states('not-heard', 'connecting')).toBe('connecting');
      expect(states('ok', 'not-heard')).toBe('not-heard');
      expect(states('ok', 'ok')).toBe('ok');
    });

    it('is web or bot when any device is', () => {
      const [p] = groupPeople([device('ada', 'd1'), device('ada', 'd2', { web: false, bot: true })]);
      expect(p.web).toBe(true);
      expect(p.bot).toBe(true);
      expect(groupPeople([device('ada', 'd1', { web: false }), device('ada', 'd2', { web: true })])[0].web).toBe(true);
    });

    it('says muted only when every device is muted, and deafened likewise', () => {
      const { rerender } = render(<VoiceChannelRow {...base} participants={[device('ada', 'd1', { muted: true, deafened: true }), device('ada', 'd2')]} />);
      expect(within(rowsOf()[0]).queryByRole('img', { name: 'muted' })).toBeNull();
      expect(within(rowsOf()[0]).queryByRole('img', { name: 'deafened' })).toBeNull();
      rerender(<VoiceChannelRow {...base} participants={[device('ada', 'd1', { muted: true, deafened: true }), device('ada', 'd2', { muted: true })]} />);
      expect(within(rowsOf()[0]).getByRole('img', { name: 'muted' })).toBeTruthy();
      expect(within(rowsOf()[0]).queryByRole('img', { name: 'deafened' })).toBeNull();
    });
  });
});
