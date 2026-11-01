// Task 10c: the Invites panel as InvitesSection drives it, with the prototype's sample link (stories and whole screens).
import { useState } from 'react';
import { InvitePanel, type InvitePanelProps } from './InvitePanel.tsx';
// The shipped copy of @dilla/web, so the story draws what Server settings → Invites draws.
import { t } from '../../../web/src/strings/index.ts';


export const EXPIRY = [
  { value: '3600', label: t('serverSettings.invites.ttl.hour') }, { value: '86400', label: t('serverSettings.invites.ttl.day') },
  { value: '604800', label: t('serverSettings.invites.ttl.week') }, { value: '0', label: t('serverSettings.invites.ttl.never') },
];
export const USES = [{ value: '1', label: '1' }, { value: '10', label: '10' }, { value: '0', label: t('serverSettings.invites.uses.unlimited') }];
export const INVITE_LABELS: InvitePanelProps['labels'] = { make: t('serverSettings.invites.make'), making: t('serverSettings.invites.making'),
  copy: t('serverSettings.invites.copy'), link: t('serverSettings.invites.link'), revoke: t('serverSettings.invites.revoke'), list: t('admin.invites.title') };
export const INVITE_ROWS = [{ id: 'a', code: t('serverSettings.invites.code', { host: 'midgard.gg' }),
  uses: t('serverSettings.invites.uses.row', { used: 3, max: 10 }), expires: t('serverSettings.invites.expires.row', { date: t('serverSettings.invites.in.days', { n: 5 }) }),
  revokeLabel: t('serverSettings.invites.revokeLabel', { name: 'Ada', date: t('serverSettings.invites.in.days', { n: 5 }) }) }];
const NOTE = t('serverSettings.invites.note', { uses: t('serverSettings.invites.note.times', { n: 10 }), expires: t('serverSettings.invites.note.in', { ttl: t('serverSettings.invites.ttl.week') }) });

/** The panel as InvitesSection drives it: choose, make, copy (which files the link under the live ones). */
export function LiveInvites({ manager = true, made: initiallyMade = false }: { manager?: boolean; made?: boolean }) {
  const [expiry, setExpiry] = useState('604800');
  const [uses, setUses] = useState('10');
  const [made, setMade] = useState(initiallyMade ? { url: 'https://midgard.gg/i/k7Hq2mXa', note: NOTE } : null);
  return (
    <InvitePanel expiry={{ label: t('serverSettings.invites.expires'), options: manager ? EXPIRY : EXPIRY.slice(0, 3), value: expiry, onChange: setExpiry }}
      uses={{ label: t('serverSettings.invites.uses'), options: manager ? USES : USES.slice(0, 2), value: uses, onChange: setUses }}
      made={made} making={false} onMake={() => setMade({ url: 'https://midgard.gg/i/k7Hq2mXa', note: NOTE })} onCopy={() => setMade(null)}
      activeTitle={t('serverSettings.invites.active', { count: INVITE_ROWS.length })} rows={INVITE_ROWS} onRevoke={() => {}} labels={INVITE_LABELS} />
  );
}

