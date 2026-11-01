// Flow 09 / Signal task 10c: the Roles tab of Server settings as the prototype draws it — a role list beside the selected
// role's editor (README §5.9). Every change saves at once; moving, deleting and the two display options sit in each role's
// context menu (Ruling X), since the prototype draws no button for them.
import { useEffect, useId, useRef, useState } from 'react';
import { ConfirmDialog, RoleEditor, type ActionMenuItem, type PermValue, type RoleEditorRole } from '@dilla/ui';
import { PERM, PERM_BY_KEY, ROLE_PERM_GROUPS, resolveCommunity, type PermKey, type RoleSummary } from '@dilla/client-core';
import { useCore } from '../../core/context.tsx';
import { errorOf, type UiError } from '../../core/errors.ts';
import { useSlice } from '../../core/use-slice.ts';
import { t, type StringKey } from '../../strings/index.ts';
import { signalPalette } from './role-palette.ts';
import { forbidden, statusIs, useRun, type SectionProps } from './shared.ts';

export const ROLE_NAME_MAX_BYTES = 100;
const UTF8 = new TextEncoder();
/** Every one of the 26 bits (the owner's and an administrator's). */
const ALL_BITS = 0x3FFFFFF;
/** A new role (prototype): named "new role", grey, with View channels and Send messages. */
const NEW_ROLE_COLOR = 0xB0BEC5;

/** The prototype's order: @everyone first, then the other roles from the highest down. */
export function orderRoles(all: readonly RoleSummary[], everyoneId: string): RoleSummary[] {
  const rest = all.filter(r => r.id !== everyoneId).sort((a, b) => b.position - a.position || (a.id < b.id ? -1 : 1));
  return [...all.filter(r => r.id === everyoneId), ...rest];
}

// The Music page's "Edit in Roles" opens this tab on the first DJ role.
const pending = new Map<string, string>();
export function openRoleOnNextVisit(communityId: string, roleId: string): void { pending.set(communityId, roleId); }

/** The bits `me` holds community-wide: everything for the owner and administrators, else the resolver's answer. */
function heldBits(args: { owner: boolean; ownerId: string; everyoneId: string; roles: readonly RoleSummary[]; held: readonly string[]; me: string }): number {
  if (args.owner) return ALL_BITS;
  const bits = resolveCommunity({ owner: args.ownerId, everyoneId: args.everyoneId, roles: args.roles, held: args.held }, args.me);
  return (bits & PERM.administrator) !== 0 ? ALL_BITS : bits;
}

type Override = { roleId: string; allow?: number; color?: number };

/**
 * Roles tab: the role list (each with its member count) and, for the selected role, its name, colour and permission toggles.
 * A role at or above my highest role (when I am not the owner) shows but cannot change, and a permission I do not hold
 * cannot be given or taken. Every gate is advisory: the instance decides.
 */
export function RolesSection({ communityId, announce }: SectionProps): React.JSX.Element {
  const client = useCore();
  const base = useId();
  const roles = useSlice(`roles:${communityId}`);
  const access = useSlice(`permissions:${communityId}`);
  const members = useSlice(`members:${communityId}`);
  const account = useSlice('account');
  const community = useSlice('communities')?.find(c => c.id === communityId);
  const everyoneId = access?.everyoneId ?? '';
  const ordered = roles === undefined ? [] : orderRoles(roles, everyoneId);
  const [selected, setSelected] = useState<string | null>(() => { const id = pending.get(communityId) ?? null; pending.delete(communityId); return id; });
  const role = ordered.find(r => r.id === selected) ?? ordered.find(r => r.id !== everyoneId) ?? ordered[0];
  const [draft, setDraft] = useState<{ roleId: string; name: string } | null>(null);
  const [override, setOverride] = useState<Override | null>(null);
  const [status, setStatus] = useState<{ text: string; alert: boolean; n: number } | null>(null);
  const [nameError, setNameError] = useState<string | undefined>(undefined);
  const [deleting, setDeleting] = useState<RoleSummary | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  // Fix round 1 (M4): a role New role made, until the roles list carries it. Meanwhile the editor shows the role it fell
  // back to read-only, so a quick toggle cannot land on the wrong role; the new one is selected once it arrives.
  const [awaiting, setAwaiting] = useState<string | null>(null);
  const queue = useRef<Promise<unknown>>(Promise.resolve());

  const locked = (r: RoleSummary): boolean => access?.owner !== true && r.position >= (access?.top ?? 0);
  const canManage = access?.loaded === true && access.manageRoles;
  const me = account?.user?.id ?? '';
  const held = members?.find(m => m.userId === me)?.roleIds ?? [];
  const mine = heldBits({ owner: access?.owner === true, ownerId: community?.owner ?? '', everyoneId, roles: roles ?? [], held, me });

  // The worker reads the roles when a server is selected; a section opened before they arrived reads them once itself.
  const absent = roles === undefined;
  useEffect(() => {
    if (!absent) return;
    client.call({ m: 'selectCommunity', communityId }).catch((e: unknown) => setLoadError(t('roles.error.load', { code: errorOf(e).code })));
  }, [absent]); // eslint-disable-line react-hooks/exhaustive-deps
  // A change the instance has applied replaces the optimistic one.
  useEffect(() => { setOverride(null); }, [roles]);
  const arrived = awaiting !== null && ordered.some(r => r.id === awaiting);
  useEffect(() => { if (arrived) setAwaiting(null); }, [arrived]);

  const say = (text: string, alert = false) => setStatus(s => ({ text, alert, n: (s?.n ?? 0) + 1 }));
  const refuse = (e: UiError): string =>
    forbidden(e) ? t('roles.error.forbidden') : statusIs(e, 409) ? t('roles.error.conflict')
      : statusIs(e, 404) ? t('roles.error.gone') : t('roles.error.other', { code: e.code });
  /** Saves one change after the ones before it (a fast run of toggles applies in order). */
  const save = (r: RoleSummary, change: { name?: string; color?: number; hoist?: boolean; mentionable?: boolean; allow?: number }, shown: string) => {
    say(t('serverSettings.roles.saving'));
    queue.current = queue.current.then(() => client.call({ m: 'updateRole', communityId, roleId: r.id, name: change.name ?? null,
      color: change.color ?? null, hoist: change.hoist ?? null, mentionable: change.mentionable ?? null, allow: change.allow ?? null }))
      .then(() => { say(t('roles.editor.saved', { name: shown })); announce(t('roles.editor.saved', { name: shown })); })
      .catch((e: unknown) => { setOverride(null); say(refuse(errorOf(e)), true); });
  };

  if (roles === undefined || role === undefined) {
    return (
      <section className="dw-settings-section" data-measure="full" aria-labelledby={`${base}-title`}>
        <h2 id={`${base}-title`} tabIndex={-1}>{t('roles.title')}</h2>
        <p>{t('serverSettings.roles.body')}</p>
        <p aria-busy="true">{t('roles.loading')}</p>
        {loadError !== null ? <p role="alert" className="dw-admin-error">{loadError}</p> : null}
      </section>
    );
  }

  const isEveryone = role.id === everyoneId;
  const editable = canManage && !locked(role) && (awaiting === null || role.id === awaiting);
  const allow = override?.roleId === role.id && override.allow !== undefined ? override.allow : role.allow;
  const color = override?.roleId === role.id && override.color !== undefined ? override.color : role.color;
  const name = draft?.roleId === role.id ? draft.name : isEveryone ? t('roles.everyone') : role.name;
  const values: Record<string, PermValue> = Object.fromEntries((Object.keys(PERM_BY_KEY) as PermKey[])
    .map(k => [k, (allow & PERM_BY_KEY[k]) !== 0 ? 'allow' : 'inherit']));
  const groups = ROLE_PERM_GROUPS.map(g => ({
    id: g.id, title: t(`perm.group.${g.id}` as StringKey),
    rows: g.keys.map(k => {
      const reason = !editable ? t('serverSettings.roles.locked') : (mine & PERM_BY_KEY[k]) === 0 ? t('roles.editor.notHeld') : null;
      return { id: k, label: t(`perm.${k}` as StringKey), hint: t(`perm.${k}.hint` as StringKey), disabled: reason !== null,
        ...(reason !== null && editable ? { disabledHint: reason } : {}) };
    }),
  }));
  const list: RoleEditorRole[] = ordered.map(r => {
    const shown = r.id === everyoneId ? t('roles.everyone') : r.name;
    return { id: r.id, name: shown, color: r.color, count: r.members,
      label: locked(r) && r.id !== everyoneId ? t('serverSettings.roles.row.locked', { name: shown, count: r.members })
        : t(r.members === 1 ? 'serverSettings.roles.row.one' : 'serverSettings.roles.row.other', { name: shown, count: r.members }) };
  });
  const movable = ordered.filter(r => r.id !== everyoneId && !locked(r));

  const select = (id: string) => { setSelected(id); setDraft(null); setNameError(undefined); setStatus(null); };
  const commitName = () => {
    if (!editable || isEveryone || draft?.roleId !== role.id) return;
    const shown = draft.name.normalize('NFC').trim();
    if (shown === role.name) return;
    if (shown === '') { setNameError(t('roles.error.empty')); return; }
    if (UTF8.encode(shown).length > ROLE_NAME_MAX_BYTES) { setNameError(t('roles.error.long')); return; }
    setNameError(undefined);
    save(role, { name: shown }, shown);
  };
  const newRole = () => {
    say(t('serverSettings.roles.saving'));
    const created = t('serverSettings.roles.newName');
    client.call({ m: 'createRole', communityId, name: created, color: NEW_ROLE_COLOR, hoist: false, mentionable: false,
      allow: PERM.viewChannel | PERM.sendMessages }).then(value => {
      const id = typeof value === 'object' && value !== null ? (value as { roleId?: unknown }).roleId : undefined;
      if (typeof id === 'string') { setAwaiting(id); select(id); }
      announce(t('roles.editor.created', { name: created }));
      say(t('roles.editor.created', { name: created }));
    }).catch((e: unknown) => {
      const err = errorOf(e);
      say(forbidden(err) ? t('roles.error.forbidden') : statusIs(err, 400) ? t('roles.error.full') : t('roles.error.other', { code: err.code }), true);
    });
  };
  const move = (r: RoleSummary, direction: 'up' | 'down') => {
    client.call({ m: 'moveRole', communityId, roleId: r.id, direction })
      .then(() => announce(t('roles.moved', { name: r.name })))
      .catch((e: unknown) => say(t('roles.error.move', { name: r.name, code: errorOf(e).code }), true));
  };
  const menuItems = (entry: RoleEditorRole): readonly ActionMenuItem[] => {
    const r = ordered.find(x => x.id === entry.id);
    if (r === undefined || r.id === everyoneId || !canManage || locked(r)) return [];
    const at = movable.findIndex(x => x.id === r.id);
    return [
      ...(at > 0 ? [{ id: 'up', label: t('serverSettings.roles.menu.up') }] : []),
      ...(at >= 0 && at < movable.length - 1 ? [{ id: 'down', label: t('serverSettings.roles.menu.down') }] : []),
      { id: 'hoist', label: t('serverSettings.roles.menu.hoist'), check: r.hoist },
      { id: 'mentionable', label: t('serverSettings.roles.menu.mentionable'), check: r.mentionable },
      { id: 'delete', label: t('serverSettings.roles.menu.delete'), danger: true, sep: true },
    ];
  };
  const onMenu = (roleId: string, item: string) => {
    const r = ordered.find(x => x.id === roleId);
    if (r === undefined) return;
    if (item === 'up' || item === 'down') move(r, item);
    else if (item === 'hoist') save(r, { hoist: !r.hoist }, r.name);
    else if (item === 'mentionable') save(r, { mentionable: !r.mentionable }, r.name);
    else if (item === 'delete') setDeleting(r);
  };

  return (
    <section className="dw-settings-section" data-measure="full" aria-labelledby={`${base}-title`}>
      <h2 id={`${base}-title`} tabIndex={-1}>{t('roles.title')}</h2>
      <p>{t('serverSettings.roles.body')}</p>
      <RoleEditor roles={list} selectedId={role.id} onSelect={select} {...(canManage ? { onNew: newRole } : {})}
        name={name} nameReadOnly={isEveryone || !editable} nameError={nameError}
        onName={v => { setDraft({ roleId: role.id, name: v }); setNameError(undefined); }} onNameCommit={commitName}
        {...(isEveryone ? {} : { colors: signalPalette(role.color) })} color={color}
        onColor={c => { if (!editable || c === color) return; setOverride({ roleId: role.id, allow, color: c }); save(role, { color: c }, role.name); }}
        groups={groups} values={values}
        onPermission={(k, on) => {
          const bit = PERM_BY_KEY[k as PermKey] as number | undefined;
          if (!editable || bit === undefined || (mine & bit) === 0) return;
          const next = on ? allow | bit : allow & ~bit;
          setOverride({ roleId: role.id, allow: next, color });
          save(role, { allow: next }, isEveryone ? t('roles.everyone') : role.name);
        }}
        menu={{ label: r => t('serverSettings.roles.menu', { name: r.name }), items: menuItems, onSelect: onMenu }}
        labels={{ list: t('serverSettings.roles.list'), newRole: t('serverSettings.roles.new'), name: t('roles.editor.name'),
          color: t('roles.editor.color'), permissions: t('roles.editor.permissions'), allow: t('perm.allow'), inherit: t('perm.inherit'), deny: t('perm.deny') }}
        status={status} />
      <DeleteRole communityId={communityId} role={deleting} announce={announce}
        onDone={() => { setDeleting(null); setSelected(null); }} onClose={() => setDeleting(null)} />
    </section>
  );
}

function DeleteRole({ communityId, role, announce, onDone, onClose }: {
  communityId: string; role: RoleSummary | null; announce: (m: string) => void; onDone: () => void; onClose: () => void;
}): React.JSX.Element {
  const client = useCore();
  const action = useRun();
  // Keep the last title while closing, so the dialog does not flash empty.
  const last = useRef<RoleSummary | null>(null);
  if (role !== null) last.current = role;
  const name = (role ?? last.current)?.name ?? '';
  return (
    <ConfirmDialog open={role !== null} title={t('roles.deleteTitle', { name })} body={t('roles.deleteBody')}
      confirmLabel={t('roles.deleteConfirm')} busyLabel={t('roles.deleteBusy')} cancelLabel={t('roles.editor.cancel')}
      busy={action.busy} error={action.error} attempt={action.attempt}
      onConfirm={() => {
        if (role === null) return;
        void action.run(() => client.call({ m: 'deleteRole', communityId, roleId: role.id }), e => t('roles.error.delete', { name: role.name, code: e.code }))
          .then(ok => { if (ok) { announce(t('roles.deleted', { name: role.name })); onDone(); } });
      }}
      onClose={() => { if (!action.busy) { action.reset(); onClose(); } }} />
  );
}
