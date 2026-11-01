import type { Meta, StoryObj } from '@storybook/react-vite';
import { UserPanel } from './UserPanel.tsx';

/** Every button is at least 30 × 30 CSS px (Signal §5.4's 30px toggles), above WCAG 2.5.8's 24. */
function expectPanelTargets(root: HTMLElement, story: string): void {
  for (const el of Array.from(root.querySelectorAll<HTMLElement>('button'))) {
    const r = el.getBoundingClientRect();
    if (r.width < 30 || r.height < 30) {
      throw new Error(`${story}: "${el.getAttribute('aria-label') ?? el.textContent ?? ''}" is ${String(r.width)} × ${String(r.height)}, under 30 px`);
    }
  }
}
/** Signal §5.4: the panel is 64 high (--panel-h), its icon buttons 30 x 30, its avatar 30. */
function expectPanelBar(root: HTMLElement, story: string): void {
  const panel = root.querySelector<HTMLElement>('.d-user-panel');
  if (panel === null) throw new Error(`${story}: no panel`);
  const h = panel.getBoundingClientRect().height;
  if (Math.abs(h - 64) > 0.5) throw new Error(`${story}: the panel is ${String(h)} high, not 64`);
  for (const b of Array.from(panel.querySelectorAll<HTMLElement>('.d-icon-button'))) {
    const r = b.getBoundingClientRect();
    if (Math.abs(r.width - 30) > 0.5 || Math.abs(r.height - 30) > 0.5) throw new Error(`${story}: an icon button is ${String(r.width)} x ${String(r.height)}, not 30 x 30`);
  }
  const a = panel.querySelector<HTMLElement>('.d-user-panel__who .d-avatar')?.getBoundingClientRect();
  if (a === undefined || Math.abs(a.width - 30) > 0.5) throw new Error(`${story}: the avatar is ${String(a?.width)} wide, not 30`);
}
/** The row stays one line at the sidebar's width: the controls do not wrap and the name block gives way. */
function expectOneLine(root: HTMLElement, story: string): void {
  const panel = root.querySelector<HTMLElement>('.d-user-panel');
  if (panel === null) throw new Error(`${story}: no panel`);
  const tops = Array.from(panel.querySelectorAll<HTMLElement>('.d-user-panel__who, .d-user-panel__controls')).map(e => Math.round(e.getBoundingClientRect().top));
  if (Math.max(...tops) - Math.min(...tops) > 4) throw new Error(`${story}: the panel wraps (tops ${tops.join(', ')})`);
  if (panel.scrollWidth > panel.clientWidth + 1) throw new Error(`${story}: the panel overflows sideways`);
}

/**
 * Left-aligned: the avatar sits on the panel's left padding edge (inside the status button's own padding), the last
 * control (deafen, or the gear when given) on its right padding edge, and the name block takes the free width between them.
 */
function expectEdges(root: HTMLElement, story: string): void {
  const panel = root.querySelector<HTMLElement>('.d-user-panel');
  const who = root.querySelector<HTMLElement>('.d-user-panel__who');
  const avatar = root.querySelector<HTMLElement>('.d-user-panel__who .d-avatar');
  const all = Array.from(root.querySelectorAll<HTMLElement>('.d-user-panel .d-icon-button'));
  const gear = all.length > 0 ? all[all.length - 1] : null;
  if (panel === null || who === null || avatar === null || gear === null) throw new Error(`${story}: no panel, avatar or gear`);
  const box = panel.getBoundingClientRect();
  const style = getComputedStyle(panel);
  const left = box.left + parseFloat(style.borderLeftWidth) + parseFloat(style.paddingLeft);
  const right = box.right - parseFloat(style.borderRightWidth) - parseFloat(style.paddingRight);
  const whoStyle = getComputedStyle(who);
  const inner = left + parseFloat(whoStyle.borderLeftWidth) + parseFloat(whoStyle.paddingLeft);
  const a = avatar.getBoundingClientRect().left;
  if (Math.abs(a - inner) > 1) throw new Error(`${story}: the avatar starts at ${String(a)}, not at ${String(inner)} (the panel's left padding edge)`);
  const g = gear.getBoundingClientRect().right;
  if (Math.abs(g - right) > 1) throw new Error(`${story}: the last control ends at ${String(g)}, not at the panel's right padding edge ${String(right)}`);
}

const meta = {
  title: 'Shell/UserPanel', component: UserPanel,
  excludeStories: [],
  args: {
    name: 'Ada Lovelace', username: 'ada', muted: false, deafened: false,
    shortcuts: { mute: 'Ctrl+Shift+M', deafen: 'Ctrl+Shift+D' },
    labels: { region: 'your account', mute: 'mute', deafen: 'deafen', settings: 'settings' },
    status: {
      current: 'online', label: 'your status: Online',
      items: [{ id: 'online', label: 'Online' }, { id: 'idle', label: 'Idle' }, { id: 'dnd', label: 'Do Not Disturb' }, { id: 'invisible', label: 'Invisible' }],
      onChoose: () => {},
    },
    onMute: () => {}, onDeafen: () => {}, onSettings: () => {},
  },
  // The sidebar's default width.
  decorators: [Story => <div style={{ width: 244 }}><Story /></div>],
  play: ({ canvasElement }) => { expectPanelTargets(canvasElement, 'UserPanel'); expectOneLine(canvasElement, 'UserPanel'); expectEdges(canvasElement, 'UserPanel'); expectPanelBar(canvasElement, 'UserPanel'); },
} satisfies Meta<typeof UserPanel>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = { globals: { theme: 'signal' } };
export const Light: Story = { globals: { theme: 'light' } };
export const HighContrast: Story = { globals: { theme: 'high-contrast' } };
/** Muted and deafened: the glyphs swap and the toggles are pressed, in every theme. */
export const MutedAndDeafened: Story = { args: { muted: true, deafened: true }, globals: { theme: 'signal' } };
export const MutedAndDeafenedLight: Story = { args: { muted: true, deafened: true }, globals: { theme: 'light' } };
export const MutedAndDeafenedHighContrast: Story = { args: { muted: true, deafened: true }, globals: { theme: 'high-contrast' } };
/** A long name truncates; the controls keep their size. */
export const LongName: Story = { args: { name: 'Augusta Ada King-Noel, Countess of Lovelace', username: 'countess.of.lovelace' } };

/** The sidebar dragged wide: the avatar stays on the left edge and the gear on the right. */
export const WideSidebar: Story = {
  decorators: [Story => <div style={{ width: 360 }}><Story /></div>],
  globals: { theme: 'signal' },
};

/** Push to talk: the PTT tag on the mic, and the ring while the key is held, in every theme. */
const PTT = { tag: 'PTT', description: 'push to talk', held: false };
export const PushToTalk: Story = { args: { ptt: PTT }, globals: { theme: 'signal' } };
export const PushToTalkLight: Story = { args: { ptt: PTT }, globals: { theme: 'light' } };
export const PushToTalkHighContrast: Story = { args: { ptt: { ...PTT, held: true } }, globals: { theme: 'high-contrast' } };
export const PushToTalkHeld: Story = { args: { ptt: { ...PTT, held: true } }, globals: { theme: 'signal' } };

// Signal §5.4: the panel as the prototype draws it (no gear: the top bar holds it; the status under the name), one story per
// status dot, and mute and deafen on. `onSettings` is left out.
const SIGNAL = { name: 'Mira', username: 'mira', line: 'online', onSettings: undefined, labels: { region: 'your account', mute: 'mute', deafen: 'deafen' } };
const status = (current: 'online' | 'idle' | 'dnd' | 'invisible') => ({ ...meta.args.status, current });
export const Signal: Story = { args: { ...SIGNAL }, globals: { theme: 'signal' } };
export const SignalIdle: Story = { args: { ...SIGNAL, line: 'idle', status: status('idle') }, globals: { theme: 'signal' } };
export const SignalDnd: Story = { args: { ...SIGNAL, line: 'do not disturb', status: status('dnd') }, globals: { theme: 'signal' } };
export const SignalInvisible: Story = { args: { ...SIGNAL, line: 'invisible', status: status('invisible') }, globals: { theme: 'signal' } };
export const SignalMuted: Story = { args: { ...SIGNAL, muted: true }, globals: { theme: 'signal' } };
export const SignalMutedAndDeafened: Story = { args: { ...SIGNAL, muted: true, deafened: true }, globals: { theme: 'signal' } };
