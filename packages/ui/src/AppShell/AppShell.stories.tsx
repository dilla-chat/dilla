import { useState } from 'react';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { ComposedShell, fixtureTopBar, SignalShell } from './AppShell.fixture.tsx';
import { HarbourPreview, HarbourStage, harbourTiles, PICKER_DEFAULT, pickerProps, stageControls } from '../CallStage/stage.fixture.tsx';
import { harbourStageBar } from '../NowPlaying/np.fixture.tsx';
import { GeneralThread } from '../MessageRow/conversation.fixture.tsx';
import { REMINDERS_DUE } from '../internal/sample/midgard.ts';
// The web's voice pane classes (dw-voice-channel, dw-voice-stage): app.css lays the stage out in the main pane.
import { TopBar } from '../TopBar/TopBar.tsx';
import { LinkBanner } from '../LinkBanner/LinkBanner.tsx';
import { MemberList } from '../MemberList/MemberList.tsx';
import { UserPanel } from '../UserPanel/UserPanel.tsx';
import { VoiceDock } from '../VoiceDock/VoiceDock.tsx';

const meta: Meta = { title: 'Shell/AppShell', parameters: { webStyles: true } };
export default meta;
type Story = StoryObj;
// 1280×800 less the preview decorator's 16px padding on each side.
export const Shell: Story = { render: () => <div style={{ ['--d-shell-h' as string]: '768px' }}><ComposedShell /></div> };
export const ShellNarrow: Story = { render: () => <div style={{ width: 360, ['--d-shell-h' as string]: '740px' }}><ComposedShell /></div> };
// web3.4: the aside open at 1280 (a fourth column), 900 and 360 (the overlay drawer).
const members = (
  <MemberList label="members" loading={false} loadingLabel="Loading members…" emptyLabel="No members yet."
    groups={[
      { id: 'mods', kind: 'role', title: 'moderators', members: [{ id: 'u1', name: 'Astrid', isOwner: true, presence: 'online' }] },
      { id: 'online', kind: 'online', title: 'online', members: [{ id: 'u2', name: 'Bjorn', presence: 'online' }] },
      { id: 'offline', kind: 'offline', title: 'offline', members: [{ id: 'u3', name: 'Cleo', presence: 'offline' }] },
    ]} />
);
const withAside = { aside: members, asideLabel: 'members', asideOpen: true, onAsideClose: () => {}, asideCloseLabel: 'close members' };
export const AsideWide: Story = { render: () => <div style={{ ['--d-shell-h' as string]: '768px' }}><ComposedShell shell={withAside} /></div> };
// Signal (README §5.1): the column aside is 232 (members) or 340 (thread); closed, it stays mounted at 0, hidden and inert.
export const AsideThread: Story = { render: () => <div style={{ ['--d-shell-h' as string]: '768px' }}><ComposedShell shell={{ ...withAside, asideKind: 'thread' }} /></div> };
export const AsideClosed: Story = { render: () => <div style={{ ['--d-shell-h' as string]: '768px' }}><ComposedShell shell={{ ...withAside, asideOpen: false }} /></div> };
// The web app's path (fix round 2): the content goes together with the close, and the same aside animates to 0. A button
// outside the shell toggles it; story-a11y clicks it and reads the width mid-transition.
function LiveAside() {
  const [open, setOpen] = useState(true);
  return (
    <div style={{ ['--d-shell-h' as string]: '736px' }}>
      <button type="button" className="story-aside-toggle" onClick={() => setOpen(o => !o)}>toggle members</button>
      <ComposedShell shell={{ aside: open ? members : undefined, asideLabel: 'members', asideOpen: open, onAsideClose: () => setOpen(false), asideCloseLabel: 'close members' }} />
    </div>
  );
}
export const AsideCloseLive: Story = { render: () => <LiveAside /> };
export const AsideMedium: Story = { render: () => <div style={{ width: 900, ['--d-shell-h' as string]: '768px' }}><ComposedShell shell={withAside} /></div> };
// Final fix I2: the drawer's close button, at the top of the overlay (a column has none).
export const AsideNarrowClose: Story = {
  render: () => <div style={{ width: 360, ['--d-shell-h' as string]: '740px' }}><ComposedShell shell={withAside} /></div>,
  play: ({ canvasElement }) => {
    const close = canvasElement.querySelector<HTMLButtonElement>('.d-app-shell__aside-head button');
    if (close === null) throw new Error('the overlay drawer has no close button');
  },
};
// UI-3 review I1, B1: with the aside open at 1024 (the overlay) and at 1440 (a column), the members bar and the header bar end at the same y.
const barsAlign = (name: string) => ({ canvasElement }: { canvasElement: HTMLElement }) => {
  const header = canvasElement.querySelector<HTMLElement>('.d-channel-header');
  const bar = canvasElement.querySelector<HTMLElement>('.d-member-list__bar');
  if (header === null || bar === null) throw new Error(`${name}: no header or members bar`);
  const [h, b] = [header.getBoundingClientRect().bottom, bar.getBoundingClientRect().bottom];
  if (Math.abs(h - b) > 0.5) throw new Error(`${name}: the header bar ends at y=${String(h)}, the members bar at y=${String(b)}`);
};
export const Aside1024Bars: Story = { render: () => <div style={{ width: 1024, ['--d-shell-h' as string]: '768px' }}><ComposedShell shell={withAside} /></div>, play: barsAlign('Aside1024Bars') };
export const Aside1440Bars: Story = { render: () => <div style={{ width: 1440, ['--d-shell-h' as string]: '768px' }}><ComposedShell shell={withAside} /></div>, play: barsAlign('Aside1440Bars') };
export const AsideNarrow: Story = { render: () => <div style={{ width: 360, ['--d-shell-h' as string]: '740px' }}><ComposedShell shell={withAside} /></div> };
export const Offline: Story = { render: () => <div style={{ ['--d-shell-h' as string]: '768px' }}><ComposedShell banner /></div> };
// web3.8 (R4): the desktop shell has no status bar, so the link banner is the only word on the link, in the banner slot as on a phone.
export const LinkBannerDesktop: Story = {
  render: () => (
    <div style={{ ['--d-shell-h' as string]: '768px' }}>
      <ComposedShell shell={{ banner: <LinkBanner status="offline" labels={{ offline: 'offline · reconnecting to dilla.thim.dev', connecting: 'connecting to dilla.thim.dev' }} /> }} />
    </div>
  ),
};
// Signal (README §5.1): the top bar over the whole shell.
export const WithTopBar: Story = { render: () => <div style={{ ['--d-shell-h' as string]: '768px' }}><ComposedShell shell={{ topBar: fixtureTopBar }} /></div> };
// web3.8 (U2): both splitters at 1280 (the aside is a column), with widths already dragged. Strict target size applies here (Shell/).
export const Splitters: Story = {
  render: () => (
    <div style={{ ['--d-shell-h' as string]: '768px' }}>
      <ComposedShell shell={{ ...withAside, paneWidths: { sidebar: 288, aside: 264 }, onPaneResize: () => {}, resizeLabels: { sidebar: 'resize the channel list', aside: 'resize the member list' } }} />
    </div>
  ),
};

// web3.8 (U3, R23), Signal §5.1: the bottom-left area sits under the sidebar (the rail runs the full height): the voice dock over the user panel.
const userPanel = (
  <UserPanel name="Ada Lovelace" username="ada" muted={false} deafened={false} shortcuts={{ mute: 'Ctrl+Shift+M', deafen: 'Ctrl+Shift+D' }}
    labels={{ region: 'your account', mute: 'mute', deafen: 'deafen', settings: 'settings' }}
    onMute={() => {}} onDeafen={() => {}} onSettings={() => {}}
    status={{ current: 'online', label: 'your status: Online', items: [{ id: 'online', label: 'Online' }, { id: 'invisible', label: 'Invisible' }], onChoose: () => {} }} />
);
const dock = (
  <VoiceDock phase="connected" where="longhouse" muted={false} deafened={false} announcement="" onCamera={() => {}} onShare={() => {}} onLeave={() => {}}
    labels={{ region: 'voice controls', connecting: 'connecting', reconnecting: 'reconnecting', paused: 'paused in {where}',
      ping: '{ms} ms', pingSub: '<1 ms', pingNone: '— ms', rate: '↑{up} ↓{down} kb/s', stats: 'Ping {ms} milliseconds, {quality}. Sending {up}, receiving {down} kilobits per second.',
      quality: { good: 'good', poor: 'poor', bad: 'bad' },
      mute: 'mute', deafen: 'deafen', leave: 'leave', camera: 'camera', share: 'share screen' }} />
);
const bottomPlay = ({ canvasElement }: { canvasElement: HTMLElement }) => {
  const panel = canvasElement.querySelector<HTMLElement>('.d-app-shell__panel');
  const rail = canvasElement.querySelector<HTMLElement>('.d-app-shell__rail');
  const main = canvasElement.querySelector<HTMLElement>('.d-app-shell__main');
  if (panel === null || rail === null || main === null) throw new Error('the shell has no panel, rail or main');
  const [p, r, m] = [panel, rail, main].map(e => e.getBoundingClientRect());
  if (Math.abs(p.left - r.right) > 1 || r.bottom < p.bottom - 1) throw new Error('the panel does not sit right of the full-height rail');
  if (p.right > m.left + 1) throw new Error('the panel overlaps main');
};
export const BottomPanel: Story = {
  render: () => <div style={{ ['--d-shell-h' as string]: '768px' }}><ComposedShell shell={{ bottom: <>{dock}{userPanel}</> }} /></div>,
  globals: { theme: 'signal' }, play: bottomPlay,
};
/** MP8: the sidebar splitter runs from the sidebar's top to the panel's bottom, and a drag from the panel's edge resizes the sidebar. */
const fullHeightPlay = async ({ canvasElement }: { canvasElement: HTMLElement }) => {
  const q = (sel: string) => canvasElement.querySelector<HTMLElement>(sel);
  const sep = canvasElement.querySelector<HTMLElement>('[role="separator"][aria-label="resize the channel list"]');
  const sidebar = q('.d-app-shell__sidebar');
  const panel = q('.d-app-shell__panel');
  if (sep === null || sidebar === null || panel === null) throw new Error('the shell has no sidebar splitter, sidebar or panel');
  const [s, sb, p] = [sep, sidebar, panel].map(e => e.getBoundingClientRect());
  if (Math.abs(s.top - sb.top) > 1) throw new Error(`the splitter starts at ${s.top}, the sidebar at ${sb.top}`);
  if (Math.abs(s.bottom - p.bottom) > 1) throw new Error(`the splitter ends at ${s.bottom}, the panel at ${p.bottom}`);
  sep.scrollIntoView({ block: 'end' });
  const pr = panel.getBoundingClientRect();
  const x = pr.right - 1, y = pr.bottom - 10;
  if (document.elementFromPoint(x, y) !== sep) throw new Error('the panel edge, 10px above its bottom, does not hit the splitter');
  // The resting hairline is the splitter's own one line, drawn above the sidebar and the panel (what they paint under it cannot cut it).
  const line = getComputedStyle(sep, '::before');
  if (line.borderLeftColor === 'rgba(0, 0, 0, 0)' || line.borderLeftWidth !== '1px') throw new Error(`the splitter draws no 1px resting line (${line.borderLeftColor}, ${line.borderLeftWidth})`);
  if (Math.abs(parseFloat(line.height) - s.height) > 1) throw new Error(`the resting line is ${line.height} high, the splitter ${s.height}px`);
  if (getComputedStyle(panel).boxShadow !== 'none') throw new Error('the panel draws a second line');
  const userRow = canvasElement.querySelector<HTMLElement>('.d-user-panel');
  if (userRow === null) throw new Error('no user panel');
  const u = userRow.getBoundingClientRect();
  if (u.top < s.top || u.bottom > s.bottom + 1) throw new Error('the line does not cover the user panel rows');
  const before = Number(sep.getAttribute('aria-valuenow'));
  const ev = (type: string, dx: number) => sep.dispatchEvent(new PointerEvent(type, { bubbles: true, pointerId: 1, button: 0, clientX: x + dx, clientY: y }));
  ev('pointerdown', 0); ev('pointermove', 40);
  await new Promise(r => setTimeout(r, 50));
  const after = Number(sep.getAttribute('aria-valuenow'));
  ev('pointerup', 40);
  if (after <= before) throw new Error(`a drag from the panel edge did not widen the sidebar (${before} -> ${after})`);
};
export const BottomPanelSplitter: Story = {
  render: () => <div style={{ ['--d-shell-h' as string]: '768px' }}><ComposedShell shell={{ bottom: <>{dock}{userPanel}</>, paneWidths: { sidebar: 240, aside: 264 }, onPaneResize: () => {}, resizeLabels: { sidebar: 'resize the channel list', aside: 'resize the member list' } }} /></div>,
  globals: { theme: 'signal' }, play: fullHeightPlay,
};
/** MP8 with the top bar: the splitter starts under the bar, at the sidebar's top. */
export const BottomPanelSplitterTopBar: Story = {
  render: () => <div style={{ ['--d-shell-h' as string]: '768px' }}><ComposedShell shell={{ topBar: fixtureTopBar, bottom: <>{dock}{userPanel}</>, paneWidths: { sidebar: 244, aside: 264 }, onPaneResize: () => {}, resizeLabels: { sidebar: 'resize the channel list', aside: 'resize the member list' } }} /></div>,
  globals: { theme: 'signal' },
  play: async ({ canvasElement }) => {
    await fullHeightPlay({ canvasElement });
    const sep = canvasElement.querySelector<HTMLElement>('[role="separator"][aria-label="resize the channel list"]');
    const bar = canvasElement.querySelector<HTMLElement>('.d-app-shell__topbar');
    if (sep === null || bar === null) throw new Error('no splitter or top bar');
    if (sep.getBoundingClientRect().top < bar.getBoundingClientRect().bottom - 1) throw new Error('the splitter runs into the top bar');
  },
};
export const BottomPanelLight: Story = {
  render: () => <div style={{ ['--d-shell-h' as string]: '768px' }}><ComposedShell shell={{ bottom: <>{dock}{userPanel}</> }} /></div>,
  globals: { theme: 'light' }, play: bottomPlay,
};
export const BottomPanelHighContrast: Story = {
  render: () => <div style={{ ['--d-shell-h' as string]: '768px' }}><ComposedShell shell={{ bottom: <>{dock}{userPanel}</> }} /></div>,
  globals: { theme: 'high-contrast' }, play: bottomPlay,
};
/**
 * B1, R12: under the 44px top bar, the main header and the members bar both have their hairline at y=44+44 from the shell's
 * top, and the user panel is 64 high (--panel-h).
 * (The sidebar head is UI-2's half of the same line; the E-GEO rect checks cover all four at integration.)
 */
export const Desktop: Story = {
  render: () => <div style={{ ['--d-shell-h' as string]: '768px' }}><ComposedShell shell={{ ...withAside, topBar: fixtureTopBar, bottom: userPanel }} /></div>,
  globals: { theme: 'signal' },
  play: ({ canvasElement }) => {
    const shell = canvasElement.querySelector<HTMLElement>('.d-app-shell')!.getBoundingClientRect();
    const edge = (selector: string) => {
      const el = canvasElement.querySelector<HTMLElement>(selector);
      if (el === null) throw new Error(`Desktop: no ${selector}`);
      return el.getBoundingClientRect().bottom - shell.top;
    };
    for (const selector of ['.d-channel-header', '.d-member-list__bar']) {
      const y = edge(selector);
      if (Math.abs(y - 88) > 0.5) throw new Error(`Desktop: the hairline of ${selector} is at y=${String(y)}, not 88`);
    }
    const panel = canvasElement.querySelector<HTMLElement>('.d-user-panel')!.getBoundingClientRect();
    if (Math.abs(panel.height - 64) > 0.5) throw new Error(`Desktop: the user panel is ${String(panel.height)} high, not 64`);
  },
};

// Signal task 5: the whole desktop frame at 1280×800 (the decorator's padding taken back), as the prototype starts in the app.
const frame = { width: 1280, height: 800, margin: -16, ['--d-shell-h' as string]: '800px' };
const topBar = (p: { voice?: boolean; due?: number }) => (
  <TopBar searchLabel="Search or jump to…" searchShortcut="⌘K" onSearch={() => {}} remindersDue={p.due ?? 0} onReminders={() => {}} onSettings={() => {}}
    voice={p.voice === true ? { room: 'harbour', pingMs: 24, onOpen: () => {} } : undefined}
    labels={{ reminders: 'Reminders', settings: 'Open settings', voice: 'open the call in harbour', remindersDue: n => `${String(n)} due` }} />
);
// Task 11: the prototype's sample has one reminder due ("bring two ropes"), so the bell carries 1 on every whole screen.
const due = REMINDERS_DUE;
export const Frame: Story = {
  parameters: { layout: 'fullscreen' },
  // Task 6b fix round 2 (rule 3d): the frame as packages/web composes it in Signal (SignalShell), not the older composition.
  render: () => <div style={frame}><SignalShell shell={{ topBar: topBar({ due }) }} /></div>,
};
export const FrameMembers: Story = {
  parameters: { layout: 'fullscreen' },
  render: () => <div style={frame}><SignalShell shell={{ ...withAside, topBar: topBar({ due }) }} /></div>,
};
export const FrameMembersClosed: Story = {
  parameters: { layout: 'fullscreen' },
  render: () => <div style={frame}><SignalShell shell={{ ...withAside, asideOpen: false, topBar: topBar({ due }) }} /></div>,
};
/** Task 11: the @ home with Ada's DM open (Shell.tsx's DM list and DM conversation). */
export const FrameDm: Story = {
  parameters: { layout: 'fullscreen' },
  render: () => <div style={frame}><SignalShell dm shell={{ topBar: topBar({ due }) }} /></div>,
};
/** Task 11: #general with Björn's thread open in the 340px aside (Shell.tsx's side panel with ThreadView), Threads pressed. */
export const FrameThread: Story = {
  parameters: { layout: 'fullscreen' },
  render: () => <div style={frame}><SignalShell shell={{ aside: <GeneralThread />, asideLabel: 'Thread', asideOpen: true, asideKind: 'thread',
    onAsideClose: () => {}, topBar: topBar({ due }) }} /></div>,
};
export const FrameInVoice: Story = {
  parameters: { layout: 'fullscreen' },
  render: () => <div style={frame}><ComposedShell shell={{ topBar: topBar({ voice: true, due: 2 }), bottom: <>{dock}{userPanel}</> }} /></div>,
};

// Task 8 (rule 3e): the whole desktop frame on the voice channel harbour, as Shell.tsx composes it (no channel header, no
// composer, no aside; VoiceView's stage in main with the voice header, the call bar and, joined, the top bar's voice chip).
const voiceMain = (stage: React.ReactNode) => <div className="dw-voice-channel"><div className="dw-voice-stage" data-call-phase="in">{stage}</div></div>;
export const FrameVoice: Story = {
  parameters: { layout: 'fullscreen' },
  render: () => <div style={frame}><SignalShell voice={{ joined: true }} shell={{ topBar: topBar({ voice: true, due: 1 }) }}
    main={voiceMain(<HarbourStage tiles={harbourTiles('Björn')} focus="d-Ada:screen" nowPlaying={harbourStageBar} />)} /></div>,
};
/** Task 8c: the same frame with Share pressed: the share picker open over the call bar (VoiceView's useSharePicker). */
export const FrameVoiceSharePicker: Story = {
  parameters: { layout: 'fullscreen' },
  render: () => <div style={frame}><SignalShell voice={{ joined: true }} shell={{ topBar: topBar({ voice: true, due: 1 }) }}
    main={voiceMain(<HarbourStage tiles={harbourTiles('Björn')} focus="d-Ada:screen" nowPlaying={harbourStageBar}
      controls={stageControls({ picker: pickerProps(PICKER_DEFAULT) })} />)} /></div>,
};
export const FrameVoiceGrid: Story = {
  parameters: { layout: 'fullscreen' },
  render: () => <div style={frame}><SignalShell voice={{ joined: true }} shell={{ topBar: topBar({ voice: true, due: 1 }) }}
    main={voiceMain(<HarbourStage tiles={harbourTiles()} focus={null} nowPlaying={harbourStageBar} />)} /></div>,
};
export const FrameVoicePreview: Story = {
  parameters: { layout: 'fullscreen' },
  render: () => <div style={frame}><SignalShell voice={{ joined: false }} shell={{ topBar: topBar({ due: 1 }) }}
    main={<div className="dw-voice-channel"><div className="dw-voice-stage" data-call-phase="idle"><HarbourPreview /></div></div>} /></div>,
};
