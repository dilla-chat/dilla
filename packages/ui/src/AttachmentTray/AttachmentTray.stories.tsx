import type { Meta, StoryObj } from '@storybook/react-vite';
import type React from 'react';
import { AttachmentTray, type AttachmentTrayProps } from './AttachmentTray.tsx';
import { useDrawnImage } from '../internal/drawn-image.ts';

// Every string is L-COPY-03's, quoted (en.ts gains them in task 9). The tray never says how a file is protected.
const STEPS = ['reading', 'preparing', 'uploading', 'ready', 'failed'] as const;
const NAMES = ['drawn.png', 'notes.txt', 'route.gpx', 'harbour.jpg', 'packing-list.pdf'];
const SIZES = ['48 KB', '2 KB', '12.4 KB', '1.2 MB', '310 KB'];
const HINT = 'Remove it and attach it again.';

const everyPhase: AttachmentTrayProps['items'] = STEPS.map((step, i) => ({
  id: `t${String(i)}`, name: NAMES[i] ?? '', size: SIZES[i] ?? '', step, failed: step === 'failed',
  phase: step === 'failed' ? 'failed (E_TOO_LARGE)' : step, ...(step === 'failed' ? { hint: HINT } : {}),
  removeLabel: `remove ${NAMES[i] ?? ''}`, onRemove: () => {},
}));

const meta = {
  title: 'Conversation/AttachmentTray', component: AttachmentTray,
  args: { label: 'files to send', items: everyPhase },
  decorators: [Story => <div style={{ maxWidth: 720 }}><Story /></div>],
} satisfies Meta<typeof AttachmentTray>;
export default meta;
type Story = StoryObj<typeof meta>;
export const EveryPhase: Story = {};
// The refusal the page shows above the tray is not this component's: this story is one failed entry.
export const TooLarge: Story = {
  args: {
    items: [{ id: 't0', name: 'harbour-video.mov', size: '31.5 MB', step: 'failed', failed: true, phase: 'failed (E_ATTACHMENT_TOO_LARGE)',
      hint: HINT, removeLabel: 'remove harbour-video.mov', onRemove: () => {} }],
  },
};

// ---- Task 7 fix round 1: the prototype's tray, one story per phase (the progress line fills by phase), and the previews.
const signal = { globals: { theme: 'signal' }, decorators: [(Story: () => React.ReactNode) => <div style={{ maxWidth: 900, background: 'var(--bg)' }}><Story /></div>] };
const one = (step: (typeof STEPS)[number], name = 'packing-list.txt', size = '2 KB') => ({ id: 's', name, size, step, failed: step === 'failed',
  phase: step === 'failed' ? 'failed (E_NETWORK)' : step, removeLabel: `remove ${name}`, onRemove: () => {} });
export const SignalReading: Story = { ...signal, args: { removeTip: 'Remove', items: [one('reading')] } };
export const SignalPreparing: Story = { ...signal, args: { removeTip: 'Remove', items: [one('preparing')] } };
export const SignalUploading: Story = { ...signal, args: { removeTip: 'Remove', items: [one('uploading')] } };
export const SignalReady: Story = { ...signal, args: { removeTip: 'Remove', items: [one('ready')] } };
export const SignalFailed: Story = { ...signal, args: { removeTip: 'Remove', items: [one('failed')] } };
function PreviewTray() {
  const url = useDrawnImage(224, 168);
  return <AttachmentTray label="files to send" removeTip="Remove" items={[
    ...(url === null ? [] : [{ ...one('ready', 'map.png', '1.2 MB'), id: 'p1', preview: { url } }]),
    { ...one('ready', 'victory.gif', '2.1 MB'), id: 'p2', preview: { gif: true } },
    { ...one('uploading'), id: 'p3' },
  ]} />;
}
export const SignalPreviews: Story = { ...signal, render: () => <PreviewTray /> };
