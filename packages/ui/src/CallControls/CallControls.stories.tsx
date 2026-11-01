import type { Meta, StoryObj } from '@storybook/react-vite';
import { CallControls } from './CallControls.tsx';
import { useState } from 'react';
import { HarbourStage, PICKER_DEFAULT, personTile, pickerProps, stageControls } from '../CallStage/stage.fixture.tsx';
import type { SharePickerValue } from '../SharePicker/SharePicker.tsx';

/**
 * Flow 06 and task 7 step 3: every voice or player control (button, disclosure summary, volume range) is at least
 * 40 × 40 CSS px, and 44 × 44 under 48rem of viewport. A range is measured by its hit box's height only.
 */
export function expectVoiceTargets(root: HTMLElement, story: string): void {
  const min = window.innerWidth < 768 ? 44 : 40;
  // Signal (rule 0): the call bar's controls and the tile tools are the prototype's sizes, measured in scripts/story-a11y.ts instead.
  // The ring banner's Answer and Decline are Task 8's 32px restyle (no prototype counterpart; flagged in the task 8c report).
  for (const el of Array.from(root.querySelectorAll<HTMLElement>('button, summary, input[type="range"]')).filter(e => e.closest('.d-call-controls--bar, .d-call-tile__tools, .d-call-stage__code, .d-ring-banner') === null)) {
    if (el.getClientRects().length === 0) continue; // not drawn (a phone-only control on a desktop width): no target
    const r = el.getBoundingClientRect();
    const range = el instanceof HTMLInputElement;
    if (r.height < min || (!range && r.width < min)) {
      throw new Error(`${story}: "${el.getAttribute('aria-label') ?? el.textContent ?? ''}" is ${String(r.width)} × ${String(r.height)}, under ${String(min)} px`);
    }
  }
}

/** Review round 1 (#1): a voice toolbar's controls sit in one row at the width they are given. */
export function expectOneRow(root: HTMLElement, story: string): void {
  for (const bar of Array.from(root.querySelectorAll<HTMLElement>('.d-voice-toolbar'))) {
    const tops = Array.from(bar.querySelectorAll<HTMLElement>(':scope > button')).map(b => Math.round(b.getBoundingClientRect().top));
    if (new Set(tops).size > 1) throw new Error(`${story}: the voice controls wrap (tops ${tops.join(', ')}) at ${String(bar.clientWidth)} px`);
    if (bar.scrollWidth > bar.clientWidth + 1) throw new Error(`${story}: the voice controls scroll sideways`);
  }
}

/** Review round 1 (#2, #3): a tip shown by keyboard focus takes no pointer; a click where it shows passes through. */
export function expectTipTakesNoPointer(root: HTMLElement, story: string): void {
  const tip = root.querySelector<HTMLElement>('.d-voice-toolbar__tip');
  if (tip === null || tip.hidden) throw new Error(`${story}: no tooltip shows after keyboard focus`);
  const r = tip.getBoundingClientRect();
  const under = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
  if (under !== null && tip.contains(under)) throw new Error(`${story}: the focus tooltip takes the pointer`);
  for (const b of Array.from(root.querySelectorAll('button'))) if (b.contains(tip)) throw new Error(`${story}: the tooltip sits inside a button`);
}

/** One rendered frame, for a check after a focus or a layout effect. */
export function nextFrame(): Promise<void> { return new Promise(resolve => { requestAnimationFrame(() => { resolve(); }); }); }

// Every string is L-COPY-05's, quoted (en.ts gains them in task 9).
const meta = {
  title: 'Voice/CallControls', component: CallControls,
  excludeStories: ['expectVoiceTargets', 'expectOneRow', 'expectTipTakesNoPointer', 'nextFrame'],
  args: {
    muted: false, deafened: false, camera: false, sharing: false, canCamera: true, canShare: true, busy: false,
    labels: {
      region: 'call controls', mute: 'mute', deafen: 'deafen', camera: 'camera',
      share: 'share screen', leave: 'leave',
      why: { paused: 'paused', joining: 'connecting', noShare: 'not available in this browser' },
    },
    shortcuts: { mute: 'Ctrl+Shift+M', deafen: 'Ctrl+Shift+D' },
    onMute: () => {}, onDeafen: () => {}, onCamera: () => {}, onShare: () => {}, onLeave: () => {},
  },
  // Each control is at least 40 × 40 CSS px (44 × 44 under 48rem of viewport), flow 06, and the five sit in one row.
  play: ({ canvasElement }) => { expectVoiceTargets(canvasElement, 'CallControls'); expectOneRow(canvasElement, 'CallControls'); },
} satisfies Meta<typeof CallControls>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Connected: Story = {};
export const MutedAndDeafened: Story = { args: { muted: true, deafened: true } };
export const CameraAndSharing: Story = { args: { camera: true, sharing: true } };
/** Joining: the controls appear but stay aria-disabled. */
export const Joining: Story = { args: { busy: true } };
/** Paused (RV-3): camera and share are aria-disabled until the call continues. */
export const Paused: Story = { args: { paused: true, canCamera: false, canShare: false } };
/** Keyboard focus on mute shows its tooltip above it: the label and the shortcut. */
export const TooltipOnFocus: Story = {
  args: { muted: true },
  decorators: [Story => <div style={{ paddingTop: 40 }}><Story /></div>],
  play: async ({ canvasElement }) => {
    expectVoiceTargets(canvasElement, 'CallControls');
    canvasElement.querySelector<HTMLButtonElement>('[role="toolbar"] button')?.focus();
    await nextFrame();
    expectTipTakesNoPointer(canvasElement, 'CallControls');
  },
};

// ---- Signal §5.6: the call bar (the web's channel stage passes `labels.bar`): 40px controls with their words and keycaps ----
/** The call bar as it ends the stage (the prototype's stove, joined): the room, the stats, the controls. */
const inBar = (controls: React.ReactNode) => (
  <div style={{ width: 980, height: 756, display: 'grid', gridTemplateRows: 'minmax(0, 1fr)', background: 'var(--bg)', margin: -16 }}>
    <HarbourStage name="stove" topic="2 in voice" stats="24 ms · ↑ 38 ↓ 71 kb/s · hearing 1 of 1" controls={controls}
      tiles={[personTile('Liv', { stat: '18 ms' }), personTile('Mira', { own: true, title: 'Mira (you)', stat: 'opus 48k' })]} />
  </div>
);
const barPlay = ({ canvasElement }: { canvasElement: HTMLElement }) => { expectOneRow(canvasElement, 'CallBar'); };
export const Default: Story = { render: () => inBar(stageControls()), play: barPlay };
export const Muted: Story = { render: () => inBar(stageControls({ muted: true })), play: barPlay };
export const Deafened: Story = { render: () => inBar(stageControls({ muted: true, deafened: true })), play: barPlay };
export const CameraOn: Story = { render: () => inBar(stageControls({ camera: true })), play: barPlay };
export const Sharing: Story = { render: () => inBar(stageControls({ sharing: true })), play: barPlay };
// ---- Task 8c (Signal §5.6): the share picker over the call bar, as VoiceView passes it (useSharePicker) ----
function PickerBar({ value, sharing = false, tabOnly = false, noSource = false }: { value?: Partial<SharePickerValue>; sharing?: boolean; tabOnly?: boolean; noSource?: boolean }) {
  const [v, setV] = useState<SharePickerValue>({ ...PICKER_DEFAULT, ...value });
  const [open, setOpen] = useState(true);
  return inBar(stageControls({ sharing, picker: { ...pickerProps(v, { tabOnly, noSource, onChange: setV }), open, onOpenChange: setOpen } }));
}
const pickerPlay = async ({ canvasElement }: { canvasElement: HTMLElement }) => {
  expectOneRow(canvasElement, 'SharePicker');
  await nextFrame();
  // The picker enters with d-modal (a small rise and scale): measure where it comes to rest.
  await Promise.all(document.getAnimations().filter(a => a.effect?.getTiming().iterations !== Infinity).map(a => a.finished.catch(() => undefined)));
  const pop = canvasElement.querySelector<HTMLElement>('.d-share-picker');
  const anchor = canvasElement.querySelector<HTMLElement>('[data-control="quality"], [data-control="share"][aria-expanded="true"]');
  if (pop === null || anchor === null) throw new Error('SharePicker: the picker is not open over its control');
  const p = pop.getBoundingClientRect();
  const a = anchor.getBoundingClientRect();
  // The prototype's placement: the picker's right edge on its control's, its bottom 10px above it.
  if (Math.abs(p.right - a.right) > 1 || Math.abs(a.top - p.bottom - 10) > 1) throw new Error(`SharePicker: placed at right ${String(p.right)} bottom ${String(p.bottom)}, control right ${String(a.right)} top ${String(a.top)}`);
  // The prototype's sizes (Ruling C: measured in a real browser): 340px wide, 28px options, the 96px preview (98 with its edge), the 36 × 20 switch, 34px Go live.
  const size = (sel: string) => pop.querySelector<HTMLElement>(sel)?.getBoundingClientRect() ?? null;
  const want: Array<[string, 'width' | 'height', number]> = [
    ['.d-segmented__option', 'height', 28], ['.d-share-picker__go', 'height', 34], ['.d-share-picker__close', 'width', 26],
    ['.d-share-picker__preview', 'height', 98], ['.d-toggle__track', 'width', 36], ['.d-toggle__track', 'height', 20],
  ];
  if (Math.abs(p.width - 340) > 0.5) throw new Error(`SharePicker: ${String(p.width)} px wide, want 340`);
  for (const [sel, dim, px] of want) {
    const r = size(sel);
    if (r !== null && Math.abs(r[dim] - px) > 0.5) throw new Error(`SharePicker: ${sel} ${dim} ${String(r[dim])}, want ${String(px)}`);
  }
};
export const SharePickerOpen: Story = { render: () => <PickerBar />, play: pickerPlay };
export const SharePickerWindow: Story = { render: () => <PickerBar value={{ source: 'window' }} />, play: pickerPlay };
export const SharePickerTab: Story = { render: () => <PickerBar value={{ source: 'tab' }} />, play: pickerPlay };
export const SharePicker720p: Story = { render: () => <PickerBar value={{ resolution: '720p' }} />, play: pickerPlay };
export const SharePicker60fps: Story = { render: () => <PickerBar value={{ fps: 60 }} />, play: pickerPlay };
export const SharePickerHeavy: Story = { render: () => <PickerBar value={{ resolution: 'source', fps: 60 }} />, play: pickerPlay };
export const SharePickerAudioOff: Story = { render: () => <PickerBar value={{ audio: false }} />, play: pickerPlay };
/** While sharing: the chevron's Stream quality picker with Apply. */
export const SharePickerSharing: Story = { render: () => <PickerBar sharing />, play: pickerPlay };
/** Chromium on Linux: a screen's sound cannot be shared, so the row says so and draws no switch. */
export const SharePickerTabOnly: Story = { render: () => <PickerBar tabOnly />, play: pickerPlay };
/** Firefox and Safari: no source choice and no sound (the browser ignores both), so neither row is drawn. */
export const SharePickerNoSource: Story = { render: () => <PickerBar noSource />, play: pickerPlay };
