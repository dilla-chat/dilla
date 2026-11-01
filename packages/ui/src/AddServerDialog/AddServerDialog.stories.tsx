import type { Meta, StoryObj } from '@storybook/react-vite';
import { AddServerDialog } from './AddServerDialog.tsx';
import { PhoneLayoutProvider } from '../internal/phoneLayout.ts';

// The strings are layout.add.title, layout.add.join and layout.add.create.
const meta = {
  title: 'Shell/AddServerDialog', component: AddServerDialog,
  args: { open: true, title: 'add a server', joinLabel: 'join a server', createLabel: 'new server', onClose: () => {}, onJoin: () => {}, onCreate: () => {} },
} satisfies Meta<typeof AddServerDialog>;
export default meta;
type Story = StoryObj<typeof meta>;

/** Focus is on the first choice. */
const play = async ({ canvasElement }: { canvasElement: HTMLElement }) => {
  // Focus lands in an effect after the first paint: wait for it, up to half a second.
  for (let i = 0; i < 30 && document.activeElement?.closest('.d-add-server') == null; i += 1) await new Promise(r => setTimeout(r, 16));
  const first = canvasElement.querySelector<HTMLElement>('.d-add-server__choice');
  if (first === null || document.activeElement !== first) throw new Error('AddServerDialog: focus is not on the first choice');
  // The dialog enters with d-modal (a 0.985 scale): measure where it comes to rest. (The prototype has no add-server dialog —
  // its "+" only toasts — so the choice keeps its own 44px.)
  await Promise.all(document.getAnimations().filter(a => a.effect?.getTiming().iterations !== Infinity).map(a => a.finished.catch(() => undefined)));
  for (const c of Array.from(canvasElement.querySelectorAll<HTMLElement>('.d-add-server__choice'))) {
    if (c.getBoundingClientRect().height < 44) throw new Error(`AddServerDialog: a choice is ${String(c.getBoundingClientRect().height)}px, under 44`);
  }
};
export const Open: Story = { globals: { theme: 'signal' }, play };
export const OpenLight: Story = { globals: { theme: 'light' }, play };
export const OpenHighContrast: Story = { globals: { theme: 'high-contrast' }, play };
export const OnPhone: Story = { globals: { theme: 'signal' }, play,
  decorators: [Story => <PhoneLayoutProvider value><div data-layout="phone" data-phone style={{ width: '100%' }}><Story /></div></PhoneLayoutProvider>] };
