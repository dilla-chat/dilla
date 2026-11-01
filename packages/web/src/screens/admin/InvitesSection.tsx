// Flow 08 / Signal task 10c: Server settings → Invites as the prototype draws it (README §5.9) — expiry and use limits as
// segmented controls, the link shown once with Copy, then the live links with Revoke.
import { useCallback, useEffect, useId, useRef, useState } from 'react';
import { Button, InvitePanel, type InvitePanelRow } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import { errorOf } from '../../core/errors.ts';
import { useSlice } from '../../core/use-slice.ts';
import { t, type StringKey } from '../../strings/index.ts';
import { forbidden, shownName, type SectionProps } from './shared.ts';

/**
 * "Expires after" and "Uses" (prototype). 0 is "never" and "∞": the instance asks Manage server for either (Task 10c), so
 * they are offered only to someone who holds it; everyone else sees and makes only the finite links.
 */
export const TTL_OPTIONS: readonly { seconds: number; key: StringKey; nameKey: StringKey }[] = [
  { seconds: 3600, key: 'serverSettings.invites.ttl.hour', nameKey: 'serverSettings.invites.ttl.hour' },
  { seconds: 86_400, key: 'serverSettings.invites.ttl.day', nameKey: 'serverSettings.invites.ttl.day' },
  { seconds: 604_800, key: 'serverSettings.invites.ttl.week', nameKey: 'serverSettings.invites.ttl.week' },
  { seconds: 0, key: 'serverSettings.invites.ttl.never', nameKey: 'serverSettings.invites.ttl.never' },
];
export const USES_OPTIONS: readonly number[] = [1, 10, 0];
export const DEFAULT_TTL_S = 604_800;
export const DEFAULT_USES = 10;
const usesLabel = (n: number): string => (n === 0 ? t('serverSettings.invites.uses.unlimited') : String(n));

interface Made { url: string; note: string }

/** Copies `text`; true when the browser let it. */
async function copied(text: string): Promise<boolean> {
  try { await navigator.clipboard.writeText(text); return true; } catch { return false; }
}

/** "in 5 days", "in 3 hours", "in 12 minutes": the prototype's expiry, from now. */
function inTime(expires: number, nowMs: number): string {
  const left = Math.max(0, expires - Math.floor(nowMs / 1000));
  const days = Math.floor(left / 86_400);
  if (days >= 1) return t(days === 1 ? 'serverSettings.invites.in.day' : 'serverSettings.invites.in.days', { n: days });
  const hours = Math.floor(left / 3600);
  if (hours >= 1) return t(hours === 1 ? 'serverSettings.invites.in.hour' : 'serverSettings.invites.in.hours', { n: hours });
  return t('serverSettings.invites.in.minutes', { n: Math.max(1, Math.floor(left / 60)) });
}

/**
 * Invites tab: make a link with an expiry and a use limit (shown once: the instance keeps only a fingerprint, so a link
 * cannot be shown again; Copy copies it and files it under the live links) and the live links with a revoke each. The
 * list is read when the tab opens and again after a make or a revoke.
 */
export function InvitesSection({ communityId, serverName, announce }: SectionProps): React.JSX.Element {
  const client = useCore();
  const base = useId();
  const invites = useSlice(`invites:${communityId}`);
  const members = useSlice(`members:${communityId}`);
  const access = useSlice(`permissions:${communityId}`);
  const selfId = useSlice('account')?.user?.id ?? null;
  const manager = access?.manageCommunity === true;
  const [ttl, setTtl] = useState(DEFAULT_TTL_S);
  const [uses, setUses] = useState(DEFAULT_USES);
  const [made, setMade] = useState<Made | null>(null);
  const [making, setMaking] = useState(false);
  const [message, setMessage] = useState<{ text: string; alert: boolean; n: number } | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [busyId, setBusyId] = useState<string | null>(null);
  const revoking = useRef(false);
  const headingRef = useRef<HTMLHeadingElement>(null);
  const say = (text: string, alert: boolean) => setMessage(m => ({ text, alert, n: (m?.n ?? 0) + 1 }));

  const load = useCallback(() => {
    setLoadError(null);
    client.call({ m: 'loadInvites', communityId }).catch((e: unknown) => setLoadError(t('admin.invites.error.load', { code: errorOf(e).code })));
  }, [client, communityId]);
  useEffect(load, [load]);
  // A choice someone without Manage server cannot make falls back to the default.
  useEffect(() => { if (!manager) { setTtl(v => (v === 0 ? DEFAULT_TTL_S : v)); setUses(v => (v === 0 ? DEFAULT_USES : v)); } }, [manager]);

  const make = () => {
    if (making) return;
    setMaking(true);
    setMessage(null);
    const ttlSeconds = ttl, maxUses = uses;
    const ttlName = t(TTL_OPTIONS.find(o => o.seconds === ttlSeconds)?.nameKey ?? 'serverSettings.invites.ttl.week');
    const note = t('serverSettings.invites.note', {
      uses: maxUses === 0 ? t('serverSettings.invites.note.any') : maxUses === 1 ? t('serverSettings.invites.note.once') : t('serverSettings.invites.note.times', { n: maxUses }),
      expires: ttlSeconds === 0 ? t('serverSettings.invites.note.never') : t('serverSettings.invites.note.in', { ttl: ttlName }),
    });
    client.call({ m: 'createInvite', communityId, maxUses, ttlSeconds }).then(value => {
      const url = typeof value === 'object' && value !== null ? (value as Record<string, unknown>).url : undefined;
      if (typeof url !== 'string' || url === '') throw Object.assign(new Error('E_BAD_RESULT'), { code: 'E_BAD_RESULT' });
      setMade({ url, note });
      load();
    }).catch((e: unknown) => {
      const err = errorOf(e);
      say(forbidden(err) ? t('invite.error.forbidden') : t('invite.error.other', { code: err.code }), true);
    }).finally(() => setMaking(false));
  };
  const copy = () => {
    if (made === null) return;
    void copied(made.url).then(ok => {
      if (!ok) { say(t('invite.blocked'), false); return; }
      announce(t('serverSettings.invites.copied'));
      setMade(null);
    });
  };

  const revoke = (id: string) => {
    const row = invites?.find(i => i.id === id);
    if (row === undefined || revoking.current) return;
    revoking.current = true;
    setBusyId(id);
    setMessage(null);
    client.call({ m: 'revokeInvite', communityId, inviteId: id }).then(() => {
      announce(t('admin.invites.revoked'));
      headingRef.current?.focus();
    }).catch((e: unknown) => {
      const err = errorOf(e);
      say(forbidden(err) ? t('admin.invites.error.forbidden') : t('admin.invites.error.other', { code: err.code }), true);
    }).finally(() => { revoking.current = false; setBusyId(null); });
  };

  // No maker on record is an account the instance no longer names; a maker who is not in the member list has left.
  const nameOf = (userId: string | null, listed: readonly { userId: string; display: string; username: string }[]): string => {
    if (userId === null) return t('admin.bans.unknownUser');
    const m = listed.find(x => x.userId === userId);
    return m === undefined ? t('admin.member.former') : shownName(m);
  };
  const now = Date.now();
  const host = (() => { try { return new URL(made?.url ?? window.location.href).host; } catch { return window.location.host; } })();
  // Until the members are read a maker cannot be named, so no row is listed rather than naming one wrongly.
  const rows: InvitePanelRow[] = (members === undefined ? [] : invites ?? []).map(i => {
    const name = nameOf(i.createdBy, members ?? []);
    const date = i.expires === 0 ? t('serverSettings.invites.ttl.never') : inTime(i.expires, now);
    // Final fix M1: revoke is offered on a link this account made, or to someone who manages the server (the instance's rule).
    const mayRevoke = (selfId !== null && i.createdBy === selfId) || manager;
    return { id: i.id, code: t('serverSettings.invites.code', { host }),
      uses: t('serverSettings.invites.uses.row', { used: i.uses, max: i.maxUses === 0 ? t('serverSettings.invites.uses.unlimited') : i.maxUses }),
      expires: i.expires === 0 ? t('serverSettings.invites.expires.never') : t('serverSettings.invites.expires.row', { date }),
      ...(mayRevoke ? { revokeLabel: t('serverSettings.invites.revokeLabel', { name, date }) } : {}) };
  });
  const ttlOptions = TTL_OPTIONS.filter(o => o.seconds !== 0 || manager).map(o => ({ value: String(o.seconds), label: t(o.key) }));
  const useOptions = USES_OPTIONS.filter(n => n !== 0 || manager).map(n => ({ value: String(n), label: usesLabel(n) }));

  return (
    <section className="dw-settings-section" data-measure="full" aria-labelledby={`${base}-title`}>
      <h2 id={`${base}-title`} ref={headingRef} tabIndex={-1}>{t('admin.invites.title')}</h2>
      <p>{t('serverSettings.invites.body', { server: serverName })}</p>
      {loadError !== null ? (
        <div role="alert" className="dw-admin-error">
          <p>{loadError}</p>
          <Button onClick={load}>{t('invite.retry')}</Button>
        </div>
      ) : null}
      <InvitePanel
        expiry={{ label: t('serverSettings.invites.expires'), options: ttlOptions, value: String(ttl), onChange: v => setTtl(Number(v)) }}
        uses={{ label: t('serverSettings.invites.uses'), options: useOptions, value: String(uses), onChange: v => setUses(Number(v)) }}
        made={made} making={making} onMake={make} onCopy={copy} message={message}
        activeTitle={t('serverSettings.invites.active', { count: rows.length })} rows={rows} busyId={busyId} onRevoke={revoke}
        labels={{ make: t('serverSettings.invites.make'), making: t('serverSettings.invites.making'), copy: t('serverSettings.invites.copy'),
          link: t('serverSettings.invites.link'), revoke: t('serverSettings.invites.revoke'), list: t('admin.invites.title') }} />
    </section>
  );
}
