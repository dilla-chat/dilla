import { useState } from 'react';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { expect, userEvent, within } from 'storybook/test';
import { PermissionGrid, type PermissionGridGroup, type PermValue } from './PermissionGrid.tsx';
// The shipped copy of @dilla/web, so the Everyone story draws what the role editor draws.
import { t, type StringKey } from '../../../web/src/strings/index.ts';

// Sample strings only; the real copy comes from the web package's perm.* keys.
const GROUPS: readonly PermissionGridGroup[] = [
  { id: 'general', title: 'General', rows: [
    { id: 'view', label: 'View channels', hint: 'See channels and read them.', disabled: false },
    { id: 'manage', label: 'Manage channels', hint: 'Create, edit and delete channels.', disabled: true, disabledHint: 'You cannot give a right you do not have.' },
  ] },
  { id: 'text', title: 'Text', rows: [
    { id: 'send', label: 'Send messages', hint: 'Post in text channels.', disabled: false },
    { id: 'pin', label: 'Pin messages', hint: 'Pin and unpin messages.', disabled: false },
  ] },
];
const LABELS = { allow: 'allow', inherit: 'inherit', deny: 'deny' };

function Demo({ mode, width, initial }: { mode: 'toggle' | 'tri'; width?: number; initial: Record<string, PermValue> }) {
  const [values, setValues] = useState<Record<string, PermValue>>(initial);
  return (
    <div style={{ width, maxWidth: '100%' }}>
      <PermissionGrid label="Permissions" groups={GROUPS} mode={mode} values={values} labels={LABELS}
        onChange={(id, v) => setValues(cur => ({ ...cur, [id]: v }))} />
    </div>
  );
}

const meta: Meta = { title: 'Settings/PermissionGrid' };
export default meta;
type Story = StoryObj;
const render = (mode: 'toggle' | 'tri', width: number, initial: Record<string, PermValue>): Story['render'] => () => <Demo mode={mode} width={width} initial={initial} />;

export const ToggleWide: Story = { render: render('toggle', 1280, { view: 'allow' }) };
export const ToggleNarrow: Story = { render: render('toggle', 360, { view: 'allow' }) };
/** Keyboard in a real browser: the arrows move and select within a row, wrapping; Tab leaves the row. */
export const TriWide: Story = {
  render: render('tri', 1280, { view: 'allow', send: 'deny' }),
  play: async ({ canvasElement }) => {
    const send = within(within(canvasElement).getByRole('radiogroup', { name: 'Send messages' }));
    send.getByRole('radio', { name: 'deny' }).focus();
    await userEvent.keyboard('{ArrowRight}');
    await expect(send.getByRole('radio', { name: 'allow' })).toHaveFocus();
    await expect(send.getByRole('radio', { name: 'allow' })).toHaveAttribute('aria-checked', 'true');
    await userEvent.keyboard('{ArrowLeft}');
    await expect(send.getByRole('radio', { name: 'deny' })).toHaveFocus();
    await userEvent.tab();
    await expect(within(canvasElement).getByRole('radiogroup', { name: 'Send messages' }).contains(document.activeElement)).toBe(false);
  },
};
export const TriNarrow: Story = { render: render('tri', 360, { view: 'allow', send: 'deny' }) };
/** The "manage channels" row is disabled with its reason shown and described. */
export const DisabledRow: Story = { render: render('tri', 720, { manage: 'inherit' }) };

/** Inside a flex parent with `flex: 1` (as in a settings panel) the grid takes the free width and does not collapse. */
export const InFlexParent: Story = {
  render: () => (
    <div style={{ display: 'flex', gap: 16, width: 720, maxWidth: '100%' }}>
      <nav aria-label="sections" style={{ flex: 'none', width: 120 }}>sections</nav>
      <div style={{ flex: 1, minWidth: 0 }}><Demo mode="tri" initial={{ view: 'allow' }} /></div>
    </div>
  ),
  play: async ({ canvasElement }) => {
    const grid = canvasElement.querySelector('.d-permission-grid') as HTMLElement;
    await expect(grid.getBoundingClientRect().width).toBeGreaterThan(300);
  },
};

// Task 10, README §5.9: @everyone in the role editor (packages/web RoleEditorFlow): PERM_GROUPS (client-core admin/perm.ts) in
// their order, every row a toggle; what @everyone holds by default is on. Control music (DJ) is wave C.
const PERM_GROUPS: readonly { id: string; keys: readonly string[] }[] = [
  { id: 'general', keys: ['view', 'manageChannels', 'manageRoles', 'manageServer', 'invite', 'auditLog', 'admin'] },
  { id: 'members', keys: ['kick', 'ban', 'nicknames', 'moderate'] },
  { id: 'text', keys: ['send', 'attach', 'react', 'history', 'pin', 'everyone', 'manageMessages', 'slowmode'] },
  { id: 'voice', keys: ['connect', 'speak', 'video', 'screen', 'mute', 'move'] },
  { id: 'music', keys: ['music'] },
];
const EVERYONE_GROUPS: readonly PermissionGridGroup[] = PERM_GROUPS.map(g => ({
  id: g.id, title: t(`perm.group.${g.id}` as StringKey),
  rows: g.keys.map(k => ({ id: k, label: t(`perm.${k}` as StringKey), hint: t(`perm.${k}.hint` as StringKey), disabled: false })),
}));
const EVERYONE: Record<string, PermValue> = { view: 'allow', invite: 'allow', send: 'allow', attach: 'allow', react: 'allow', history: 'allow', connect: 'allow', speak: 'allow', video: 'allow', screen: 'allow' };
function EveryoneDemo() {
  const [values, setValues] = useState<Record<string, PermValue>>(EVERYONE);
  return (
    <div style={{ width: 502, maxWidth: '100%' }}>
      <PermissionGrid label={t('roles.editor.permissions')} groups={EVERYONE_GROUPS} mode="toggle" values={values}
        labels={{ allow: t('perm.allow'), inherit: t('perm.inherit'), deny: t('perm.deny') }}
        onChange={(id, v) => setValues(cur => ({ ...cur, [id]: v }))} />
    </div>
  );
}
export const Everyone: Story = { render: () => <EveryoneDemo /> };
