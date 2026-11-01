import type { Meta, StoryObj } from '@storybook/react-vite';
import { userEvent } from 'storybook/test';
import { VoiceChannelRow } from './VoiceChannelRow.tsx';
import type { VoiceParticipantView } from '../CallTile/CallTile.tsx';

const person = (name: string, more: Partial<VoiceParticipantView> = {}): VoiceParticipantView => ({
  deviceId: `d-${name}`, userId: `u-${name}`, name, state: 'ok', speaking: false, muted: false, deafened: false,
  video: false, screen: false, web: true, bot: false, ...more,
});

/**
 * web3.8 (R29, U3): in a participant row (`.d-voice-row__person`) or a tile caption (`.d-call-tile__caption`) the name and
 * its glyphs share one line: each glyph's box lies within the name's line box (2px of slack), and the row is one line
 * high. Throws on the first row that wraps or sits off the name's line.
 */
export function expectGlyphLine(root: HTMLElement, story: string): void {
  const rows = Array.from(root.querySelectorAll<HTMLElement>('.d-voice-row__person, .d-call-tile__caption'));
  for (const row of rows) {
    const name = row.querySelector<HTMLElement>('.d-voice-row__person-name, .d-call-tile__name');
    if (name === null) throw new Error(`${story}: a row has no name`);
    const line = name.getBoundingClientRect();
    const box = row.getBoundingClientRect();
    // A caption is one line high. A sidebar person row is --row-h-sub high (R25): its glyphs sit on the name's centre line.
    const sidebar = row.classList.contains('d-voice-row__person');
    // One line high, or the caption's floor around a shorter line: a stage caption is the prototype's 22px chip (1.375rem), the
    // compact one 20px (1.25rem).
    const floor = row.closest('.d-call-tile[data-stage]') !== null ? 22 : 20;
    if (!sidebar && box.height > Math.max(line.height, floor) + 0.5) throw new Error(`${story}: the row is ${String(box.height)} high, over one line (${String(line.height)})`);
    for (const glyph of Array.from(row.querySelectorAll<HTMLElement>(':scope > svg.d-icon, :scope > .d-state-dot, .d-voice-row__state svg.d-icon, .d-voice-row__state .d-state-dot, .d-call-tile__state svg.d-icon, .d-call-tile__state .d-state-dot'))) {
      const r = glyph.getBoundingClientRect();
      const off = sidebar ? Math.abs((r.top + r.bottom) / 2 - (line.top + line.bottom) / 2) > 2 : r.top < line.top - 2 || r.bottom > line.bottom + 2;
      if (off) {
        throw new Error(`${story}: a glyph at ${String(r.top)}-${String(r.bottom)} is off the name's line ${String(line.top)}-${String(line.bottom)}`);
      }
    }
  }
}

/**
 * Every person row shares one avatar column and one name column (R25): each starts at one x. Signal §5.4 sets the state
 * glyphs after the name and at the right edge (the prototype's flex row), so they no longer share a column.
 */
export function expectPeopleColumns(root: HTMLElement, story: string): void {
  const people = Array.from(root.querySelectorAll<HTMLElement>('.d-voice-row__person'));
  if (people.length < 2) throw new Error(`${story}: too few people to compare`);
  for (const sel of ['.d-voice-row__avatar', '.d-voice-row__person-name']) {
    const lefts = people.map(p => p.querySelector<HTMLElement>(sel)?.getBoundingClientRect().left ?? Number.NaN);
    if (lefts.some(Number.isNaN) || Math.max(...lefts) - Math.min(...lefts) > 0.5) throw new Error(`${story}: ${sel} starts at ${lefts.join(', ')}`);
  }
}

const rgb = (s: string): [number, number, number] => { const m = /rgba?\(([\d.]+)[, ]+([\d.]+)[, ]+([\d.]+)/.exec(s); if (m === null) throw new Error(`cannot read colour ${s}`); return [Number(m[1]), Number(m[2]), Number(m[3])]; };
const lum = ([r, g, b]: [number, number, number]) => { const f = (c: number) => { const x = c / 255; return x <= 0.03928 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4; }; return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b); };
/** The first non-transparent background behind an element. */
function backdrop(el: HTMLElement): string {
  for (let n: HTMLElement | null = el; n !== null; n = n.parentElement) { const c = getComputedStyle(n).backgroundColor; if (!/rgba\(.*, 0\)$|transparent/.test(c)) return c; }
  return 'rgb(0, 0, 0)';
}
/** Tab to the join icon and assert it shows a 2px inset ring at 3:1 or better against what is behind it. */
async function expectJoinRing(root: HTMLElement, story: string): Promise<void> {
  await userEvent.tab();
  await userEvent.tab();
  const join = root.querySelector<HTMLElement>('.d-voice-row__join button');
  if (join === null || document.activeElement !== join) throw new Error(`${story}: the join icon does not take focus`);
  await Promise.all(root.getAnimations({ subtree: true }).map(a => a.finished));
  const s = getComputedStyle(join);
  if (s.outlineStyle === 'none' || parseFloat(s.outlineWidth) < 2) throw new Error(`${story}: no focus ring (${s.outlineStyle} ${s.outlineWidth})`);
  if (parseFloat(s.outlineOffset) >= 0) throw new Error(`${story}: the ring is not inset (${s.outlineOffset})`);
  if (getComputedStyle(join.closest('.d-voice-row__join') as HTMLElement).opacity !== '1') throw new Error(`${story}: the join icon is not shown`);
  const a = lum(rgb(s.outlineColor)); const b = lum(rgb(backdrop(join)));
  const ratio = (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
  if (ratio < 3) throw new Error(`${story}: the ring is ${ratio.toFixed(2)}:1`);
}

// Every string is L-COPY-05's, quoted (en.ts gains them in task 9).
const meta = {
  title: 'Voice/VoiceChannelRow', component: VoiceChannelRow,
  excludeStories: ['expectGlyphLine', 'expectPeopleColumns'],
  play: ({ canvasElement }) => { expectGlyphLine(canvasElement, 'VoiceChannelRow'); },
  args: {
    name: 'harbour', current: false, joined: false, participants: [person('ada', { speaking: true }), person('bo', { muted: true })],
    labels: {
      open: 'voice {name}', count: '{n} in voice', playbackOnly: 'playback only', join: 'join voice', joined: 'connected',
      state: {
        speaking: 'speaking', muted: 'muted', deafened: 'deafened', notHeard: 'not heard yet', unverified: 'unverified stream',
        connecting: 'connecting', video: 'camera on', screen: 'sharing screen', web: 'web', bot: 'bot', noSound: 'no sound',
      },
    },
    onOpen: () => {}, onJoin: () => {},
  },
  // The sidebar's scroller (244px less its 8px sides) on bg-2, in the list item that positions the join icon.
  decorators: [Story => <div style={{ width: 244, boxSizing: 'border-box', padding: '4px 8px', background: 'var(--bg-2)' }}><div className="d-voice-item"><Story /></div></div>],
} satisfies Meta<typeof VoiceChannelRow>;
export default meta;
type Story = StoryObj<typeof meta>;

/** Live, not joined: the count and one sub-row per person with their state words. */
export const Live: Story = {};
/** No one in voice: no count and no sub-rows. */
export const Empty: Story = { args: { participants: [] } };
/** This device is in it, and the main pane shows it. */
/** The join icon shows on focus or hover (here: focused); a joined row has neither the join icon nor a connected glyph. */
export const JoinFocused: Story = {
  globals: { theme: 'signal' },
  play: async ({ canvasElement }) => { await expectJoinRing(canvasElement, 'JoinFocused'); },
};
export const JoinFocusedLight: Story = { ...JoinFocused, globals: { theme: 'light' } };
export const JoinFocusedHighContrast: Story = { ...JoinFocused, globals: { theme: 'high-contrast' } };
/** V2: a participant's mute and deafen glyphs are 0.25rem apart and end 8px inside the list's right edge, one glyph or two. */
export const GlyphInset: Story = {
  args: { current: true, joined: true, participants: [person('ada', { muted: true, deafened: true }), person('bo', { muted: true }), person('cy')] },
  play: ({ canvasElement }) => {
    const list = canvasElement.querySelector<HTMLElement>('.d-voice-row__people');
    if (list === null) throw new Error('GlyphInset: no people list');
    const edge = list.getBoundingClientRect().right - 8;
    for (const person of Array.from(list.querySelectorAll<HTMLElement>('.d-voice-row__person'))) {
      const glyphs = Array.from(person.querySelectorAll<HTMLElement>('.d-slot .d-voice-row__state')).map(g => g.getBoundingClientRect());
      for (const [i, r] of glyphs.entries()) {
        if (r.right > edge + 1) throw new Error(`GlyphInset: a glyph ends at ${String(r.right)}, past the inset edge ${String(edge)}`);
        if (i > 0 && r.left < glyphs[i - 1].right - 0.5) throw new Error('GlyphInset: two glyphs overlap');
      }
      if (glyphs.length > 0 && Math.abs(glyphs[glyphs.length - 1].right - edge) > 1) throw new Error(`GlyphInset: the right edge is ${String(glyphs[glyphs.length - 1].right)}, not ${String(edge)}`);
    }
  },
};
export const JoinedCurrent: Story = {
  args: { current: true, joined: true, participants: [person('ada', { speaking: true }), person('bo', { muted: true }), person('you')] },
};
export const EveryState: Story = {
  play: ({ canvasElement }) => { expectGlyphLine(canvasElement, 'EveryState'); expectPeopleColumns(canvasElement, 'EveryState'); },
  args: {
    participants: [person('ada', { speaking: true, video: true }), person('bo', { muted: true, deafened: true }), person('cy', { screen: true }),
      person('dee', { state: 'connecting' }), person('eli', { state: 'not-heard' }), person('fay', { state: 'unverified' })],
  },
};
/** A bot in voice: the bot tag and `playback only`, visible before anyone joins (brief, trust facts). */
export const BotPlaybackOnly: Story = { args: { participants: [person('ada', { speaking: true }), person('relay', { bot: true, web: false })] } };
/** A voice channel readable by this server keeps its readable glyph. */
export const Readable: Story = { args: { readable: true } };
/** A long name truncates before its glyphs: they stay on its line, and nothing wraps (R29). */
export const LongNameWithGlyphs: Story = {
  args: { participants: [person('augusta-ada-king-noel-countess-of-lovelace', { muted: true, deafened: true, video: true, screen: true, speaking: true }),
    person('relay', { bot: true, web: false, state: 'unverified', muted: true })] },
};
/** The same list in each theme. */
/** The mute and deafen glyphs sit in the person's trailing slot; a speaking person's avatar has the accent ring. */
export const SpeakingAndMuted: Story = { args: { participants: [person('ada', { speaking: true }), person('bo', { muted: true, deafened: true }), person('cy')] } };
/** AW6: the AFK channel lists its away roster: people with no voice connection, counted, each with the `away` word. */
export const AwayRoster: Story = {
  play: ({ canvasElement }) => { expectGlyphLine(canvasElement, 'AwayRoster'); expectPeopleColumns(canvasElement, 'AwayRoster'); },
  args: { name: 'AFK', labels: { ...meta.args.labels, away: 'away' },
    participants: [person('ada', { away: true, web: false }), person('bo', { away: true, web: false })] },
};
export const EveryStateLight: Story = { ...EveryState, globals: { theme: 'light' } };
export const EveryStateHighContrast: Story = { ...EveryState, globals: { theme: 'high-contrast' } };

// Signal §8: one story per state, with the prototype's people (harbour, Midgard Crew).
const HARBOUR = [person('Ada'), person('Björn'), person('Kari'), person('Ole')];
/** People in the room, nobody speaking: the count at the right, an 18px avatar per person. */
export const WithPeople: Story = { args: { name: 'harbour', onJoin: undefined, participants: HARBOUR } };
/** Speaking is only the --live ring on the avatar (no meter in the list). */
export const Speaking: Story = {
  args: { name: 'harbour', onJoin: undefined, participants: [person('Ada', { speaking: true }), ...HARBOUR.slice(1)] },
  play: ({ canvasElement }) => {
    expectGlyphLine(canvasElement, 'Speaking');
    if (canvasElement.querySelector('[role="meter"], .d-audio-meter') !== null) throw new Error('Speaking: a meter in the list');
  },
};
/** Muted and deafened: the --danger glyphs at the right. */
export const MutedDeafened: Story = { args: { name: 'harbour', onJoin: undefined, participants: [person('Ada', { muted: true }), person('Björn', { muted: true, deafened: true }), person('Kari')] } };
/** A camera (--fg-2 glyph) and a shared screen (the LIVE tag). */
export const CameraAndLive: Story = { args: { name: 'harbour', onJoin: undefined, participants: [person('Ada', { screen: true }), person('Björn', { video: true }), person('Kari', { video: true, screen: true }), person('Ole')] } };
/** Glenn: the rounded-square avatar, the BOT tag and "playback only". */
export const WithGlenn: Story = { args: { name: 'harbour', onJoin: undefined, participants: [person('Ada'), person('Glenn', { bot: true, web: false })] } };
/** The prototype's harbour at its first paint (not joined): Ada speaking and sharing, Björn's camera, Kari both, Ole, Glenn. */
export const Harbour: Story = {
  args: { name: 'harbour', onJoin: undefined, participants: [person('Ada', { speaking: true, screen: true }), person('Björn', { video: true }), person('Kari', { video: true, screen: true }), person('Ole'), person('Glenn', { bot: true, web: false })] },
};
