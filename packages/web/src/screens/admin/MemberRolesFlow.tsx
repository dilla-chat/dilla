// Flow 09: give and take roles from the member list. A Dialog (through its portal, so it works from the modal narrow
// drawer) over a CheckList; every tick is one grantRole or revokeRole, applied at once. It opens on the first enabled box,
// else on Done.
import { useEffect, useRef, useState } from 'react';
import { CheckList, Dialog } from '@dilla/ui';
import { resolveCommunity, type RoleSummary } from '@dilla/client-core';
import { useCore } from '../../core/context.tsx';
import { errorOf } from '../../core/errors.ts';
import { useSlice } from '../../core/use-slice.ts';
import { t } from '../../strings/index.ts';
import { forbidden, statusIs, type Announce } from './shared.ts';
import { memberName } from './members-model.ts';

const ALL_BITS = 0x3FFFFFF;

export interface MemberRolesFlowProps {
  communityId: string;
  userId: string;
  onClose: () => void;
  announce: Announce;
}

/** Roles in the server's order: highest first, without @everyone. */
const assignable = (roles: readonly RoleSummary[], everyoneId: string): RoleSummary[] =>
  roles.filter(r => r.id !== everyoneId).sort((a, b) => b.position - a.position || (a.id < b.id ? -1 : 1));

export function MemberRolesFlow({ communityId, userId, onClose, announce }: MemberRolesFlowProps): React.JSX.Element {
  const client = useCore();
  const roles = useSlice(`roles:${communityId}`);
  const members = useSlice(`members:${communityId}`);
  const access = useSlice(`permissions:${communityId}`);
  const me = useSlice('account')?.user?.id ?? '';
  const community = useSlice('communities')?.find(c => c.id === communityId);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [problem, setProblem] = useState<{ message: string; n: number } | null>(null);

  const target = members?.find(m => m.userId === userId);
  // The last name seen, so a member who leaves while this is open can still be named in the message.
  const seen = useRef('');
  if (target !== undefined) seen.current = memberName(target);
  const name = seen.current;
  const latestRoles = useRef(roles);
  latestRoles.current = roles;
  const left = target === undefined && members !== undefined;
  const told = useRef(false);
  useEffect(() => {
    if (!left || told.current) return;
    told.current = true;
    announce(t('roles.assign.error.gone', { name }));
    onClose();
  });
  const everyoneId = access?.everyoneId ?? '';
  const list = assignable(roles ?? [], everyoneId);
  const mine = access?.owner === true ? ALL_BITS
    : resolveCommunity({ owner: community?.owner ?? '', everyoneId, roles: roles ?? [], held: members?.find(m => m.userId === me)?.roleIds ?? [] }, me);
  // A role at or above my highest, or carrying a bit I lack, is not mine to give (the instance decides again).
  const rankLocked = (r: RoleSummary): boolean => access?.owner !== true && r.position >= (access?.top ?? 0);
  const lacking = (r: RoleSummary): boolean => (r.allow & ~mine & ALL_BITS) !== 0;
  const held = new Set(target?.roleIds ?? []);

  const toggle = (id: string, on: boolean) => {
    const r = list.find(x => x.id === id);
    if (r === undefined || busyId !== null) return;
    setBusyId(id);
    setProblem(null);
    const command = on ? { m: 'grantRole' as const, communityId, userId, roleId: id } : { m: 'revokeRole' as const, communityId, userId, roleId: id };
    client.call(command).then(() => {
      announce(t(on ? 'roles.assign.given' : 'roles.assign.taken', { role: r.name, name }));
      setBusyId(null);
    }).catch((e: unknown) => {
      const err = errorOf(e);
      // The server names a target with no second factor by its own code; checked before the generic 403 (never by detail).
      const message = err.code === 'E_TARGET_NO_FACTOR' ? t('roles.assign.error.twoFactor', { name })
        : forbidden(err) ? t('roles.assign.error.forbidden', { role: r.name })
        : statusIs(err, 404) ? (latestRoles.current?.some(x => x.id === id) === false ? t('roles.error.gone') : t('roles.assign.error.gone', { name }))
          : t('roles.assign.error.other', { code: err.code });
      // The refusal and the end of busy land in one update.
      setProblem(p => ({ message, n: (p?.n ?? 0) + 1 }));
      setBusyId(null);
    });
  };

  const items = list.map(r => {
    const rank = rankLocked(r);
    return { id: r.id, label: r.name, checked: held.has(r.id), disabled: rank || lacking(r), color: r.color,
      ...(rank ? { detail: t('roles.assign.locked', { role: r.name }) } : lacking(r) ? { detail: t('roles.editor.notHeld') } : {}) };
  });

  if (roles === undefined || members === undefined) {
    return (
      <Dialog open title={t('roles.title')} closeLabel={t('roles.assign.close')} initialFocus="close" onClose={onClose}>
        <p aria-busy="true">{t('roles.loading')}</p>
      </Dialog>
    );
  }
  return (
    <Dialog open title={t('roles.assign.title', { name })} closeLabel={t('roles.assign.close')} initialFocus="close" onClose={onClose}>
      <p>{t('roles.assign.body', { name })}</p>
      <p>{t('roles.access')}</p>
      <CheckList label={t('roles.assign.title', { name })} items={items} busyId={busyId} emptyLabel={t('roles.assign.none')} onToggle={toggle} initialFocus />
      {problem !== null ? <p key={problem.n} role="alert" className="dw-admin-error">{problem.message}</p> : null}
    </Dialog>
  );
}
