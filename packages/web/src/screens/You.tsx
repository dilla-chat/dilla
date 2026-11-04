import { useEffect } from 'react';
import { Avatar, Icon } from '@dilla/ui';
import { useCore } from '../core/context.tsx';
import { useSlice, useSlices } from '../core/use-slice.ts';
import type { SettingsSection } from '../router.ts';
import { t } from '../strings/index.ts';
import { mergeMembers, selfName } from './shell-model.ts';
import { useSignOut } from './settings/SignOut.tsx';
import { suggestDeviceName } from './settings/device-name.ts';

const SETTINGS_ICON = 'settings';
const CHEVRON = 'chevron-right';
const SIGN_OUT_ICON = 'log-out';

/**
 * The phone's You tab (flow 12; Task 11, Jonas Q3: the prototype's). Who this is ("@mira · packing rope"), one box with the
 * connection ("Connected to midgard.gg · 24 ms", the gateway heartbeat's round trip) and this device ("Firefox on Linux · device
 * d3f9a1"), then two rows: Settings and Sign out (Jonas, wave B: the status menu is the sidebar user panel's, as in the prototype). The node, link, generation and device count stay in Settings → Diagnostics.
 */
export function You(props: { onOpenSettings(section: SettingsSection | null): void }): React.JSX.Element {
  const client = useCore();
  const account = useSlice('account');
  const connection = useSlice('connection');
  const communities = useSlice('communities');
  const memberLists = useSlices((communities ?? []).map(c => `members:${c.id}` as const));
  const self = account?.user ?? null;
  const name = selfName(self, mergeMembers(memberLists));
  const choice = account?.presenceChoice ?? 'online';
  const statusLine = self?.statusMsg !== undefined && self.statusMsg !== '' ? self.statusMsg : t(`presence.${choice}`);
  const host = account?.instance?.name ?? location.host;
  const status = connection?.status ?? 'connecting';
  const linkLine = status === 'online'
    ? (connection?.pingMs === undefined ? t('you.connectedNoPing', { host }) : t('you.connected', { host, ms: connection.pingMs }))
    : status === 'connecting' ? t('you.connecting', { host }) : t('you.offline', { host });
  const parts = suggestDeviceName(navigator.userAgent);
  const deviceId = (account?.deviceId ?? '').slice(0, 6);
  const deviceLine = parts === null ? t('you.deviceUnknown', { id: deviceId }) : t('you.device', { browser: parts.browser, os: parts.os, id: deviceId });

  const signOut = useSignOut();
  useEffect(() => { client.call({ m: 'refreshDevices' }).catch(() => undefined); }, [client]);

  return (
    <div className="dw-phone-pane dw-you">
      {/* The prototype's You has no title bar; the tab root's heading is for screen readers and the tab's focus. */}
      <h2 tabIndex={-1} className="d-sr-only" data-tab-heading="">{t('you.title')}</h2>
      <div className="dw-you__account">
        <Avatar name={name} size="card" />
        <div className="dw-you__who">
          <strong className="dw-you__name">{name}</strong>
          {self === null ? null : <span className="dw-you__line">{t('you.line', { handle: self.username, status: statusLine })}</span>}
        </div>
      </div>
      <div className="dw-you__facts">
        <span>{linkLine}</span>
        <span>{deviceLine}</span>
      </div>
      <div className="dw-you__rows">
        <button type="button" className="dw-you__row" data-settings-section="index" onClick={() => props.onOpenSettings(null)}>
          <Icon name={SETTINGS_ICON} size={16} /><span className="dw-you__label">{t('you.settings')}</span><Icon name={CHEVRON} size={15} />
        </button>
        <button type="button" className="dw-you__row" data-danger="true" onClick={signOut.requestOpen}>
          <Icon name={SIGN_OUT_ICON} size={16} /><span className="dw-you__label">{t('workspace.signOut')}</span>
        </button>
      </div>
      {signOut.dialog}
    </div>
  );
}
