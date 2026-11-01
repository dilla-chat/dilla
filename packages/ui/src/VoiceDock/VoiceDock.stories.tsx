import type { ReactNode } from 'react';
import { DOCK_STATS_CONTRAST } from './dockStatsContrast.ts';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { VoiceDock } from './VoiceDock.tsx';
import { harbourCompact } from '../NowPlaying/np.fixture.tsx';
import { UserPanel } from '../UserPanel/UserPanel.tsx';
import { nextFrame } from '../CallControls/CallControls.stories.tsx';

/**
 * Signal §5.4: the dock is a card in the --live tints (radius 8px, padding 10px, margin 0 8px 8px), with the three bars
 * while connected and no level meter, the name over the stats, then camera, share and leave as 28px icon buttons in one row.
 */
function expectDock(root: HTMLElement, story: string): void {
  const dock = root.querySelector<HTMLElement>('.d-voice-dock');
  if (dock === null) throw new Error(`${story}: no dock`);
  const style = getComputedStyle(dock);
  if (style.borderTopWidth !== '1px' || parseFloat(style.borderTopLeftRadius) !== 8 || style.backgroundColor === 'rgba(0, 0, 0, 0)') {
    throw new Error(`${story}: the dock is not the card (border ${style.borderTopWidth}, radius ${style.borderTopLeftRadius}, fill ${style.backgroundColor})`);
  }
  if (parseFloat(style.paddingLeft) !== 10 || parseFloat(style.marginLeft) !== 8 || parseFloat(style.marginBottom) !== 8) throw new Error(`${story}: the dock's padding is ${style.paddingLeft} and margin ${style.marginLeft} ${style.marginBottom}`);
  if (dock.querySelector('[role="meter"], .d-audio-meter, .d-voice-dock__meter') !== null) throw new Error(`${story}: the dock draws a level meter`);
  const buttons = Array.from(dock.querySelectorAll<HTMLElement>('.d-voice-dock__controls .d-icon-button'));
  if (buttons.length === 0) return;
  const tops = new Set(buttons.map(b => Math.round(b.getBoundingClientRect().top)));
  if (tops.size > 1) throw new Error(`${story}: the dock controls wrap (tops ${[...tops].join(', ')})`);
  const phone = dock.closest('[data-layout="phone"]') !== null;
  for (const b of buttons) {
    const r = b.getBoundingClientRect();
    if (!phone && (Math.abs(r.width - 28) > 0.5 || Math.abs(r.height - 28) > 0.5)) throw new Error(`${story}: "${b.getAttribute('aria-label') ?? ''}" is ${String(r.width)} × ${String(r.height)}, not 28 × 28`);
  }
  const controls = dock.querySelector<HTMLElement>('.d-voice-dock__controls')!;
  if (controls.scrollWidth > controls.clientWidth + 1) throw new Error(`${story}: the dock controls scroll sideways`);
  if (dock.scrollWidth > dock.clientWidth + 1) throw new Error(`${story}: the dock scrolls sideways`);
}

// Every string is L-COPY-05's / layout's, quoted.
const meta = {
  title: 'Voice/VoiceDock', component: VoiceDock, parameters: DOCK_STATS_CONTRAST,
  args: {
    phase: 'connected', where: 'harbour', muted: false, deafened: false, announcement: '',
    labels: {
      region: 'voice controls', connecting: 'connecting', where: 'harbour · Midgard Crew', reconnecting: 'reconnecting',
      paused: 'paused', mute: 'mute', deafen: 'deafen', leave: 'leave', camera: 'camera', share: 'share screen',
      ping: '{ms} ms', pingSub: '<1 ms', pingNone: '— ms', rate: '↑ {up} ↓ {down} kb/s',
      stats: 'Ping {ms} milliseconds, {quality}. Sending {up}, receiving {down} kilobits per second.',
      quality: { good: 'good', poor: 'poor', bad: 'bad' },
      why: { paused: 'paused', joining: 'connecting', noShare: 'not available in this browser' },
    },
    camera: false, sharing: false, canCamera: true, canShare: true,
    onStatus: () => {}, onCamera: () => {}, onShare: () => {}, onLeave: () => {},
  },
  // The dock at the app's real widths: the desktop sidebar (240 px, its dock wrapper's 1 px edge), or with
  // `parameters.sidebar: 'phone'` the phone drawer of a 360 px screen (300 px, the 60 px rail beside it).
  decorators: [(Story, ctx) => (ctx.parameters.sidebar === 'phone' || window.innerWidth < 768
    ? <PhoneDrawer><Story /></PhoneDrawer> : <DesktopSidebar><Story /></DesktopSidebar>)],
  play: ({ canvasElement }) => { expectDock(canvasElement, 'VoiceDock'); },
} satisfies Meta<typeof VoiceDock>;
export default meta;
type Story = StoryObj<typeof meta>;

/** MP5: line 2 is checked in the browser: the quality colours it (normal, warn, danger), it uses tabular numerals and stays on one line. */
function expectStats(root: HTMLElement, story: string, quality: string, colour: string | null): void {
  const stats = root.querySelector<HTMLElement>('.d-voice-dock__stats');
  if (stats === null) throw new Error(`${story}: no stats on line 2`);
  if (stats.dataset.quality !== quality) throw new Error(`${story}: quality ${String(stats.dataset.quality)}, not ${quality}`);
  const style = getComputedStyle(stats);
  if (!style.fontVariantNumeric.includes('tabular-nums')) throw new Error(`${story}: the stats do not use tabular numerals`);
  const line2 = root.querySelector<HTMLElement>('.d-voice-dock__where')!;
  if (line2.getBoundingClientRect().height > parseFloat(getComputedStyle(line2).lineHeight) * 1.5) throw new Error(`${story}: line 2 wraps`);
  const probe = document.createElement('span');
  // Jonas 2026-10-27 (superseding Ruling V): the stats at rest are the prototype's --fg-3.
  probe.style.color = colour === null ? 'var(--fg-3)' : `var(${colour})`;
  root.append(probe);
  const want = getComputedStyle(probe).color;
  probe.remove();
  if (style.color !== want) throw new Error(`${story}: the stats are ${style.color}, not ${want}`);
}
const link = { rttMs: 24, upKbps: 48, downKbps: 96 } as const;
export const Connected: Story = { args: { announcement: 'You joined harbour.', stats: { ...link, quality: 'good' } },
  play: ({ canvasElement }) => { expectDock(canvasElement, 'VoiceDock'); expectStats(canvasElement, 'Connected', 'good', null); } };
/** 150 to 300 ms: the stats turn warn. */
export const PoorLink: Story = { args: { stats: { rttMs: 210, upKbps: 40, downKbps: 88, quality: 'poor' } },
  play: ({ canvasElement }) => { expectDock(canvasElement, 'VoiceDock'); expectStats(canvasElement, 'PoorLink', 'poor', '--warn'); } };
/** Over 300 ms or over 5 % loss: the stats turn danger. */
export const BadLink: Story = { args: { stats: { rttMs: 412, upKbps: 3, downKbps: 12, quality: 'bad' } },
  play: ({ canvasElement }) => { expectDock(canvasElement, 'VoiceDock'); expectStats(canvasElement, 'BadLink', 'bad', '--danger'); } };
/** Before the first sample (the first two seconds, or no candidate pair): "— ms". */
export const NoSampleYet: Story = {
  play: ({ canvasElement }) => {
    expectDock(canvasElement, 'VoiceDock');
    if (canvasElement.querySelector('.d-voice-dock__where')?.textContent !== '— ms') throw new Error('NoSampleYet: line 2 is not "— ms"');
  },
};
export const Connecting: Story = { args: { phase: 'connecting' } };
export const Reconnecting: Story = { args: { phase: 'reconnecting' } };
/** Camera on and sharing: both glyphs swap (video, monitor-x) and are pressed toggles. */
/** V3: camera, share and leave sit together at the right end of the controls row. */
export const ControlsOnTheRight: Story = {
  play: ({ canvasElement }) => {
    const dock = canvasElement.querySelector<HTMLElement>('.d-voice-dock')!.getBoundingClientRect();
    const camera = canvasElement.querySelector<HTMLElement>('.d-voice-dock__controls .d-icon-button')!.getBoundingClientRect();
    if (camera.left <= dock.left + dock.width / 2) throw new Error(`ControlsOnTheRight: the camera starts at ${String(camera.left)}, left of the dock's centre`);
  },
};
export const CameraAndSharing: Story = { args: { camera: true, sharing: true } };
/** Paused (RV-3): the dock says so, and camera and share are aria-disabled as on the stage; the banner with its action
 *  lives on the stage. */
export const Paused: Story = { args: { phase: 'paused', canCamera: false, canShare: false } };
/** Paused, with keyboard focus on camera: its tooltip shows the name, and the reason is its description; the name stays "camera". */
export const PausedWhy: Story = {
  args: { phase: 'paused', paused: true, canCamera: false, canShare: false },
  play: async ({ canvasElement }) => {
    expectDock(canvasElement, 'VoiceDock');
    const camera = canvasElement.querySelector<HTMLButtonElement>('.d-voice-dock__controls button[aria-label="camera"]');
    if (camera === null) throw new Error('VoiceDock: no camera control');
    camera.focus();
    await nextFrame();
    if (camera.getAttribute('aria-disabled') !== 'true') throw new Error('VoiceDock: the paused camera is not aria-disabled');
    const ids = (camera.getAttribute('aria-describedby') ?? '').split(' ').filter(Boolean);
    const described = ids.map(id => document.getElementById(id)?.textContent ?? '').join(' ');
    if (described !== 'paused') throw new Error(`VoiceDock: the camera is described as "${described}", not "paused"`);
  },
};
/** Joining: camera and share wait for the session (the stage's busy); leave stays live. */
export const Joining: Story = { args: { phase: 'connecting', busy: true } };
/** Keyboard focus on camera shows its tooltip, drawn in a portal and never inside a button. */
export const TooltipOnFocus: Story = {
  play: async ({ canvasElement }) => {
    expectDock(canvasElement, 'VoiceDock');
    canvasElement.querySelector<HTMLButtonElement>('.d-voice-dock__controls button')?.focus();
    await nextFrame();
    const tip = document.querySelector<HTMLElement>('[role="tooltip"]');
    if (tip === null || tip.textContent !== 'camera') throw new Error(`VoiceDock: the tooltip says "${tip?.textContent ?? ''}", not "camera"`);
    if (canvasElement.contains(tip)) throw new Error('VoiceDock: the tooltip sits inside the dock');
  },
};
/** Without a status handler, line 1 is plain text. */
export const StatusIsText: Story = {
  args: { onStatus: undefined },
  play: ({ canvasElement }) => {
    expectDock(canvasElement, 'VoiceDock');
    if (canvasElement.querySelector('button.d-voice-dock__line1') !== null) throw new Error('VoiceDock: line 1 is a button without onStatus');
  },
};
/** With a handler, the channel name is a button (it opens Diagnostics), its line's height (the prototype's 12px line; axe's
 *  target-size rule checks its spacing, WCAG 2.5.8). */
export const StatusIsButton: Story = {
  play: ({ canvasElement }) => {
    expectDock(canvasElement, 'VoiceDock');
    const b = canvasElement.querySelector<HTMLElement>('button.d-voice-dock__line1');
    if (b === null) throw new Error('VoiceDock: line 1 is not a button');
    if (b.getBoundingClientRect().height < 15) throw new Error('VoiceDock: the status button is under its 15.6px line');
  },
};
/** Before the web app wires camera and share, the dock keeps leave. */
export const WithoutCameraAndShare: Story = { args: { onCamera: undefined, onShare: undefined } };
/** A DM call: line 1 is the DM's name, and the outgoing ring is announced through the dock's status region. */
export const DmRinging: Story = { args: { phase: 'connecting', where: 'ada', announcement: 'ringing ada', labels: { ...meta.args.labels, where: 'ada' } } };
/** Rejoining: the dock stays at connecting while the join waits for its earlier Remove; the status region says so once. */
export const Rejoining: Story = { args: { phase: 'connecting', announcement: 'Rejoining harbour. This can take up to 30 seconds.' } };
/** A long channel name is cut with an ellipsis on line 1 (the server stays in the tooltip); line 2 and the controls hold; nothing scrolls sideways. */
export const LongNames: Story = {
  args: {
    where: 'planning-for-the-midsummer-weekend-and-the-night-owls-society', stats: { rttMs: 1234, upKbps: 12345, downKbps: 23456, quality: 'bad' },
    labels: { ...meta.args.labels, where: 'planning-for-the-midsummer-weekend-and-the-night-owls-society · Midgard Crew' },
  },
  play: ({ canvasElement }) => {
    expectDock(canvasElement, 'LongNames');
    const text = canvasElement.querySelector<HTMLElement>('.d-voice-dock__text')!;
    if (text.scrollWidth <= text.clientWidth) throw new Error('LongNames: the long name is not cut');
    const line1 = canvasElement.querySelector<HTMLElement>('.d-voice-dock__line1')!;
    if (line1.scrollWidth > line1.clientWidth + 1) throw new Error('LongNames: line 1 overflows');
  },
};

const userPanel = (
  <UserPanel name="Ada Lovelace" username="ada" muted={false} deafened={false} shortcuts={{ mute: 'Ctrl+Shift+M', deafen: 'Ctrl+Shift+D' }}
    labels={{ region: 'your account', mute: 'mute', deafen: 'deafen', settings: 'settings' }}
    onMute={() => {}} onDeafen={() => {}} onSettings={() => {}}
    status={{ current: 'online', label: 'your status: Online', items: [{ id: 'online', label: 'Online' }, { id: 'invisible', label: 'Invisible' }], onChoose: () => {} }} />
);
/** In a call: the dock card above the 64px user panel, 8px between the card and the panel's hairline. */
export const InCall: Story = {
  render: args => <><VoiceDock {...args} />{userPanel}</>,
  play: async ({ canvasElement }) => {
    // The dock rises in (d-up): measure where it comes to rest.
    await Promise.all(document.getAnimations().filter(a => a.effect?.getTiming().iterations !== Infinity).map(a => a.finished.catch(() => undefined)));
    expectDock(canvasElement, 'VoiceDock');
    const dock = canvasElement.querySelector<HTMLElement>('.d-voice-dock')!.getBoundingClientRect();
    const panel = canvasElement.querySelector<HTMLElement>('.d-user-panel')!.getBoundingClientRect();
    if (Math.abs(panel.top - dock.bottom - 8) > 1) throw new Error(`VoiceDock: the panel starts at ${String(panel.top)}, the dock ends at ${String(dock.bottom)}`);
    if (Math.abs(panel.height - 64) > 0.5) throw new Error(`VoiceDock: the panel is ${String(panel.height)} high, not 64`);
  },
};

/** The app's desktop sidebar: 244 px, and the dock wrapper's 1 px right edge (app.css `.dw-voice-dock`). */
function DesktopSidebar({ children }: { children: ReactNode }) {
  return <div style={{ width: 244 }}><div style={{ background: 'var(--bg-2)', borderRight: '1px solid var(--hairline)' }}>{children}</div></div>;
}

/** The phone drawer at 360 px: 300 px wide, the dock wrapper's edges. */
function PhoneDrawer({ children }: { children: ReactNode }) {
  return (
    <div className="sb-phone-drawer" style={{ width: 300 }}>
      <div style={{ background: 'var(--bg-2)', borderRight: '1px solid var(--hairline)' }}>{children}</div>
    </div>
  );
}

const phone = { parameters: { sidebar: 'phone' } } as const;
/** The phone drawer (360 px screen): status, where, and the three controls in one row. */
export const PhoneConnected: Story = { ...phone };
export const PhoneCameraAndSharing: Story = { ...phone, args: { camera: true, sharing: true } };
export const PhonePaused: Story = { ...phone, args: { phase: 'paused', canCamera: false, canShare: false } };

/** Signal parity (§5.4): the prototype's dock, leave only, harbour at 24 ms. */
export const Signal: Story = { args: { onCamera: undefined, onShare: undefined, onStatus: undefined, stats: { rttMs: 24, upKbps: 38, downKbps: 71, quality: 'good' } }, globals: { theme: 'signal' } };
/** Task 8b (§5.4, §6.6): the prototype's dock while Glenn plays in harbour: the compact now-playing row under the dock's row. */
export const SignalMusic: Story = { args: { ...Signal.args, music: harbourCompact }, globals: { theme: 'signal' } };
