// Task 10c: the Roles editor as RolesSection drives it, with the prototype's sample roles (stories and whole screens).
import { useState } from 'react';
import { RoleEditor, type RoleEditorProps } from './RoleEditor.tsx';
import type { PermValue } from '../PermissionGrid/PermissionGrid.tsx';
// The shipped copy, permission groups and swatches of @dilla/web, so the story draws what Server settings → Roles draws.
import { t, type StringKey } from '../../../web/src/strings/index.ts';
import { signalPalette } from '../../../web/src/screens/admin/role-palette.ts';


// The prototype's sample server: @everyone, raid lead, crew and DJ with their member counts.
export const ROLES = [
  { id: 'everyone', name: t('roles.everyone'), color: 0x8C939E, count: 3 },
  { id: 'raid', name: 'raid lead', color: 0xE9B6EC, count: 1 },
  { id: 'crew', name: 'crew', color: 0x5AA8E6, count: 2 },
  { id: 'dj', name: 'DJ', color: 0xFFD16A, count: 2 },
].map(r => ({ ...r, label: t(r.count === 1 ? 'serverSettings.roles.row.one' : 'serverSettings.roles.row.other', { name: r.name, count: r.count }) }));
// client-core admin/perm.ts's ROLE_PERM_GROUPS (copied, as PermissionGrid's stories do; the library does not depend on the core).
type PermKey = string;
const PERM_GROUPS: readonly { id: string; keys: readonly PermKey[] }[] = [
  { id: 'general', keys: ['view', 'send', 'pin', 'everyone', 'attach', 'react', 'history', 'invite'] },
  { id: 'moderation', keys: ['manageChannels', 'kick', 'ban', 'manageMessages', 'moderate', 'nicknames', 'slowmode', 'manageRoles', 'manageServer', 'auditLog', 'admin'] },
  { id: 'voice', keys: ['connect', 'speak', 'screen', 'video', 'mute', 'move'] },
  { id: 'music', keys: ['music'] },
];
export const ALL_KEYS = PERM_GROUPS.flatMap(g => g.keys);
const RAID: PermKey[] = ['view', 'send', 'pin', 'everyone', 'manageChannels', 'kick', 'ban', 'connect', 'speak', 'screen'];
const EVERYONE: PermKey[] = ['view', 'send', 'connect', 'speak', 'screen'];
const DJ: PermKey[] = ['view', 'send', 'connect', 'speak', 'music'];
export const ALLOWS: Record<string, PermKey[]> = { raid: RAID, everyone: EVERYONE, crew: ['view', 'send', 'pin', 'connect', 'speak', 'screen'], dj: DJ };
export const GROUPS = PERM_GROUPS.map(g => ({ id: g.id, title: t(`perm.group.${g.id}` as StringKey),
  rows: g.keys.map(k => ({ id: k, label: t(`perm.${k}` as StringKey), hint: t(`perm.${k}.hint` as StringKey), disabled: false })) }));
export const LABELS: RoleEditorProps['labels'] = { list: t('serverSettings.roles.list'), newRole: t('serverSettings.roles.new'),
  name: t('roles.editor.name'), color: t('roles.editor.color'), permissions: t('roles.editor.permissions'),
  allow: t('perm.allow'), inherit: t('perm.inherit'), deny: t('perm.deny') };

/** The editor as RolesSection drives it: the selected role's name, colour and toggles, saved as they change. */
export function LiveRoleEditor({ initial = 'raid' }: { initial?: string }) {
  const [selected, setSelected] = useState(initial);
  const [allows, setAllows] = useState(ALLOWS);
  const [colors, setColors] = useState<Record<string, number>>(Object.fromEntries(ROLES.map(r => [r.id, r.color])));
  const role = ROLES.find(r => r.id === selected) ?? ROLES[1];
  const everyone = role.id === 'everyone';
  const values: Record<string, PermValue> = Object.fromEntries(ALL_KEYS.map(k => [k, allows[role.id].includes(k) ? 'allow' : 'inherit']));
  return (
    <RoleEditor roles={ROLES.map(r => ({ ...r, color: colors[r.id] }))} selectedId={role.id} onSelect={setSelected} onNew={() => {}}
      name={role.name} nameReadOnly={everyone} onName={() => {}} onNameCommit={() => {}}
      {...(everyone ? {} : { colors: signalPalette(colors[role.id]) })} color={colors[role.id]}
      onColor={c => setColors(s => ({ ...s, [role.id]: c }))} groups={GROUPS} values={values}
      onPermission={(k, on) => setAllows(s => ({ ...s, [role.id]: on ? [...s[role.id], k] : s[role.id].filter(x => x !== k) }))}
      menu={{ label: r => t('serverSettings.roles.menu', { name: r.name }), onSelect: () => {},
        items: r => (r.id === 'everyone' ? [] : [{ id: 'up', label: t('serverSettings.roles.menu.up') }, { id: 'delete', label: t('serverSettings.roles.menu.delete'), danger: true, sep: true }]) }}
      labels={LABELS} />
  );
}

