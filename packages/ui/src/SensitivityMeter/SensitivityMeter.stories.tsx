// README §8 SensitivityMeter: Idle, TestingHeard, TestingTooQuiet, AutoOn (line locked), ManualDrag, from the worked example
// (docs/design/signal/stories/SensitivityMeter.stories.tsx). Levels are fixed (no timers) so screenshots stay deterministic.
import type { Meta, StoryObj } from '@storybook/react-vite';
import { useState } from 'react';
import { SensitivityMeter, segmentStates, type SensitivityMeterProps } from './SensitivityMeter.tsx';

/** Ruling C: the prototype's geometry and colours, measured in the real browser (scripts/check-stories.mjs runs every play). */
function checkMeter(root: HTMLElement, args: SensitivityMeterProps) {
  const fail = (what: string) => { throw new Error(`SensitivityMeter: ${what}`); };
  const meter = root.querySelector<HTMLElement>('.d-sens__meter');
  const line = root.querySelector<HTMLElement>('.d-sens__line');
  const value = root.querySelector<HTMLElement>('.d-sens__value');
  const input = root.querySelector<HTMLInputElement>('.d-sens__input');
  const scale = root.querySelector<HTMLElement>('.d-sens__scale');
  if (!meter || !line || !value || !input || !scale) return fail('missing a part');
  const segs = [...root.querySelectorAll<HTMLElement>('.d-sens__seg')];
  if (segs.length !== 50) fail(`${segs.length} segments, not 50`);
  const r0 = segs[0].getBoundingClientRect(), r1 = segs[1].getBoundingClientRect();
  if (Math.abs(r0.height - 28) > 0.5) fail(`segment height ${r0.height}`);
  if (Math.abs(r1.left - r0.right - 2) > 0.5) fail(`segment gap ${r1.left - r0.right}`);
  if (getComputedStyle(segs[0]).borderTopLeftRadius !== '2px') fail('segment radius');
  // Each lit segment's colour is its state's token (the prototype's rule).
  const probe = document.createElement('span');
  root.appendChild(probe);
  const colour = (v: string) => { probe.style.color = `var(${v})`; return getComputedStyle(probe).color; };
  const want = { off: colour('--meter-off'), below: colour('--fg-4'), on: colour('--ok'), hot: colour('--warn') };
  probe.remove();
  segmentStates(args.levelDb, args.thresholdDb, args.testing).forEach((s, i) => {
    if (getComputedStyle(segs[i]).backgroundColor !== want[s]) fail(`segment ${i} is not ${s}`);
  });
  const m = meter.getBoundingClientRect(), l = line.getBoundingClientRect(), v = value.getBoundingClientRect(), sc = scale.getBoundingClientRect();
  const expected = m.left + (args.thresholdDb + 100) / 100 * m.width;
  if (Math.abs((l.left + l.right) / 2 - expected) > 1) fail(`line at ${(l.left + l.right) / 2}, want ${expected}`);
  if (Math.abs(l.width - 2) > 0.5 || Math.abs(l.height - 40) > 0.5) fail(`line ${l.width}×${l.height}, want 2×40`);
  if (Math.abs((v.left + v.right) / 2 - expected) > 1 || v.bottom > l.top + 4) fail('the dB label is not centred above the line');
  if (Math.abs(sc.top - m.bottom - 6) > 1) fail(`scale ${sc.top - m.bottom}px under the meter, want 6`);
  const ib = input.getBoundingClientRect();
  if (Math.abs(ib.width - m.width) > 0.5 || Math.abs(ib.height - m.height) > 0.5) fail('the slider does not cover the meter');
  const cursor = getComputedStyle(input).cursor;
  if (cursor !== (args.auto ? 'not-allowed' : 'ew-resize')) fail(`cursor ${cursor}`);
  if (input.disabled !== args.auto) fail('disabled does not follow automatic');
}
const play = ({ canvasElement, args }: { canvasElement: HTMLElement; args: SensitivityMeterProps }) => { checkMeter(canvasElement, args); };

const meta = {
  title: 'Settings/SensitivityMeter',
  component: SensitivityMeter,
  // The prototype's resting state: automatic sensitivity, the line at −48 dB, not testing.
  args: { onThresholdChange: () => {}, levelDb: -100, thresholdDb: -48, auto: true, testing: false, label: 'Sensitivity threshold' },
  // 686px: the prototype's Mic test content width (720 minus the group's 1px border and 16px padding).
  decorators: [Story => <div style={{ width: 686, padding: 16, background: 'var(--bg)' }}><Story /></div>],
  play,
} satisfies Meta<typeof SensitivityMeter>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Idle: Story = {};
export const TestingHeard: Story = { args: { testing: true, levelDb: -28, thresholdDb: -48 } };
export const TestingTooQuiet: Story = { args: { testing: true, levelDb: -62, thresholdDb: -48 } };
/** Over −10 dB the top segments warn. */
export const Clipping: Story = { args: { testing: true, levelDb: -6, thresholdDb: -48 } };
/** Automatic sensitivity: the line sits where dilla put it and cannot be dragged (cursor not-allowed). */
export const AutoOn: Story = { args: { testing: true, levelDb: -36, thresholdDb: -52, auto: true } };
/** Automatic off: the line is a slider (arrows 1 dB, Page Up/Down 10 dB), here dragged to −35 dB. */
export const ManualDrag: Story = {
  args: { auto: false, testing: true, levelDb: -40, thresholdDb: -35 },
  // The line follows a press, so scripts/check-stories.mjs can click a spot and read the value under it (fix round 1).
  render: function Render(args) {
    const [db, setDb] = useState(args.thresholdDb);
    return <SensitivityMeter {...args} thresholdDb={db} onThresholdChange={setDb} />;
  },
};
