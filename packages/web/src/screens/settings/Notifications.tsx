import { useState } from 'react';
import { SETTINGS, type NotifyMode } from '@dilla/client-core';
import { Banner, Segmented, SettingsRow, Toggle } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import { errorOf, type UiError } from '../../core/errors.ts';
import { useSlice } from '../../core/use-slice.ts';
import { askPermission, readPermission, type PermissionState } from '../../notify-permission.ts';
import { t, type StringKey } from '../../strings/index.ts';

const TITLE_ID = 'notify-title';
const MODES: readonly { value: NotifyMode; key: StringKey }[] = [
  { value: 'all', key: 'notify.mode.all' },
  { value: 'mentions', key: 'notify.mode.mentions' },
  { value: 'nothing', key: 'notify.mode.nothing' },
];
const BLOCKED: Readonly<Record<'denied' | 'unsupported', StringKey>> = {
  denied: 'notify.permission.denied',
  unsupported: 'notify.permission.unsupported',
};

/** What the server row shows while the server has no value of its own: the device default, as the notifier reads it. */
function defaultMode(stored: string | undefined): NotifyMode {
  return stored === 'everything' ? 'all' : stored === 'nothing' ? 'nothing' : 'mentions';
}

/**
 * Settings → Notifications (Signal task 10 fix round 1, README §5.9): exactly the prototype's four rows. The selected
 * server's mode and its @everyone switch (absent with no server selected), desktop notifications on this device (the
 * browser's permission is asked for only from the switch's own click), and one switch for the join, leave and ring
 * sounds. Every value is written with setSetting; the section keeps no copy and renders what the worker republishes.
 * Older per-channel and per-server values stay stored and keep working where the notifier reads them.
 */
export function Notifications({ communityId = null }: { communityId?: string | null }): React.JSX.Element {
  const client = useCore();
  const communities = useSlice('communities') ?? [];
  const settings = useSlice('settings') ?? {};
  const [permission, setPermission] = useState<PermissionState>(() => readPermission());
  const [error, setError] = useState<UiError | null>(null);
  const server = communities.find(c => c.id === communityId) ?? null;

  const write = (key: string, value: string | null) => {
    setError(null);
    client.call({ m: 'setSetting', key, value }).catch((e: unknown) => setError(errorOf(e)));
  };
  const blocked = permission === 'denied' || permission === 'unsupported';
  const desktopOn = permission === 'granted' && settings[SETTINGS.notifyDesktop] !== 'off';
  const setDesktop = (on: boolean) => {
    if (!on) { write(SETTINGS.notifyDesktop, 'off'); return; }
    write(SETTINGS.notifyDesktop, null);
    if (permission === 'default') void askPermission().then(setPermission);
  };
  const soundsOn = settings[SETTINGS.soundVoice] !== 'off' && settings[SETTINGS.soundRing] !== 'off';
  const setSounds = (on: boolean) => {
    write(SETTINGS.soundVoice, on ? 'on' : 'off');
    write(SETTINGS.soundRing, on ? 'on' : 'off');
  };

  return (
    <section className="dw-settings-section" aria-labelledby={TITLE_ID}>
      <h2 id={TITLE_ID} tabIndex={-1}>{t('notify.title')}</h2>
      <p>{t('notify.sub')}</p>
      {error === null ? null : <Banner tone="danger">{t('settings.error.other', { code: error.code })}</Banner>}
      {server === null ? null : <>
        <SettingsRow id="notify-server" icon="bell" label={server.name} hint={t('notify.server.hint')}>
          <Segmented id="notify-server-mode" label={t('notify.server.label', { server: server.name })}
            options={MODES.map(o => ({ value: o.value, label: t(o.key) }))}
            value={settings[SETTINGS.notifyCommunity(server.id)] ?? defaultMode(settings[SETTINGS.notifyDefault])}
            onChange={value => write(SETTINGS.notifyCommunity(server.id), value)} />
        </SettingsRow>
        <SettingsRow id="notify-everyone" icon="at-sign" label={t('notify.everyone.label')} hint={t('notify.everyone.hint')}>
          <Toggle id="notify-everyone-switch" label={t('notify.everyone.label')} checked={settings[SETTINGS.everyoneCommunity(server.id)] !== '1'}
            onChange={on => write(SETTINGS.everyoneCommunity(server.id), on ? null : '1')} />
        </SettingsRow>
      </>}
      <SettingsRow id="notify-desktop" icon="monitor-smartphone" label={t('notify.desktop.label')}
        hint={permission === 'denied' || permission === 'unsupported' ? t(BLOCKED[permission]) : t('notify.desktop.hint')}>
        <Toggle id="notify-desktop-switch" label={t('notify.desktop.label')} checked={desktopOn} disabled={blocked} onChange={setDesktop} />
      </SettingsRow>
      <SettingsRow id="notify-sounds" icon="volume-2" label={t('notify.sounds.label')} hint={t('notify.sounds.hint')}>
        <Toggle id="notify-sounds-switch" label={t('notify.sounds.label')} checked={soundsOn} onChange={setSounds} />
      </SettingsRow>
    </section>
  );
}
