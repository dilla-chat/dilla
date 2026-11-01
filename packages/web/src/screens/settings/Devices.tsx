import { useCallback, useEffect, useRef, useState } from 'react';
import type { Command, DeviceSummary } from '@dilla/client-core';
import { Banner, Button, DeviceRow, Dialog } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import { errorOf, type UiError } from '../../core/errors.ts';
import { useSlice } from '../../core/use-slice.ts';
import { t } from '../../strings/index.ts';
import { timeAgo } from './SignInFactors.tsx';
import { suggestDeviceName } from './device-name.ts';
import { RenameDialog } from './RenameDialog.tsx';
import { KeyDialog } from './KeyDialog.tsx';
import { useSignOut } from './SignOut.tsx';

const TITLE_ID = 'devices-title';

type Target = { kind: 'revoke'; deviceId: string } | { kind: 'removeUnlisted'; deviceId: string }
  | { kind: 'rename'; deviceId: string } | null;

function byId(a: DeviceSummary, b: DeviceSummary): number {
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
}

/**
 * The device list in display order: this device first, then the devices that are not removed by last seen
 * (newest first), then the removed ones by when they were removed (newest first); ties by id. Returns a new array.
 */
export function orderDevices(devices: readonly DeviceSummary[]): DeviceSummary[] {
  return [...devices].sort((a, b) => {
    if (a.own !== b.own) return a.own ? -1 : 1;
    if (a.revokedAt === null && b.revokedAt !== null) return -1;
    if (a.revokedAt !== null && b.revokedAt === null) return 1;
    if (a.revokedAt === null || b.revokedAt === null) return b.lastSeen - a.lastSeen || byId(a, b);
    return b.revokedAt - a.revokedAt || byId(a, b);
  });
}

/** "Firefox on Linux" for this browser, or an empty string when the user agent is not one it knows. */
function suggestedName(): string {
  const parts = suggestDeviceName(window.navigator.userAgent);
  return parts === null ? '' : t('devices.renameSuggestion', { browser: parts.browser, os: parts.os });
}

/**
 * Settings → Devices (flow 03; Q13, rulings 17, 30, 33). It asks the worker for a fresh list once per mount. A
 * listed device is removed with the recovery key; a live device in no signed list is removed without it (the
 * worker decides from its own refreshed list, so this page's choice of dialog grants nothing). This browser's row
 * signs out (Signal task 10 fix round 1, the prototype's row); after a sign-out the worker wipes the store and publishes `cleared`, which the App answers with a reload: nothing
 * here navigates. After a removal the opener's row has no button any more, so focus goes to the heading.
 */
export function Devices(): React.JSX.Element {
  const client = useCore();
  const devices = useSlice('devices');
  const account = useSlice('account');
  const signOut = useSignOut();
  const [target, setTarget] = useState<Target>(null);
  const [loadError, setLoadError] = useState<UiError | null>(null);
  const [plainBusy, setPlainBusy] = useState(false);
  const [plainError, setPlainError] = useState<UiError | null>(null);
  const plainRunning = useRef(false);
  const headingRef = useRef<HTMLHeadingElement>(null);
  // Set by a resolved removal; read once the dialog has closed (its own close effect runs first, as a child's).
  const focusHeading = useRef(false);

  const refresh = useCallback(() => {
    setLoadError(null);
    client.call({ m: 'refreshDevices' }).catch((e: unknown) => setLoadError(errorOf(e)));
  }, [client]);
  useEffect(refresh, [refresh]);

  useEffect(() => {
    if (target !== null || !focusHeading.current) return;
    focusHeading.current = false;
    headingRef.current?.focus();
  }, [target]);

  const choose = (next: Target) => {
    setPlainError(null);
    setTarget(next);
  };
  const close = () => { if (!plainRunning.current) setTarget(null); };

  const confirmPlain = async (command: Command, removal: boolean) => {
    if (plainRunning.current) return;
    plainRunning.current = true;
    setPlainBusy(true);
    setPlainError(null);
    try {
      await client.call(command);
      if (removal) focusHeading.current = true;
      setTarget(null);
    } catch (e) {
      setPlainError(errorOf(e));
    } finally {
      plainRunning.current = false;
      setPlainBusy(false);
    }
  };

  const ordered = devices === undefined ? [] : orderDevices(devices);
  const now = Date.now();

  // The name dialog starts on the current name; this browser without one starts on "Firefox on Linux" and the like.
  const renaming = target?.kind === 'rename' ? devices?.find(d => d.id === target.deviceId) : undefined;
  const renameStart = renaming === undefined ? '' : renaming.label ?? (renaming.own ? suggestedName() : '');

  // The remove dialogs name the device by its label and its id: the label is the server's to change.
  const subjectOf = (deviceId: string | null) => {
    const device = deviceId === null ? undefined : devices?.find(d => d.id === deviceId);
    if (deviceId === null) return undefined;
    return device?.label == null
      ? <span>{deviceId.slice(0, 8)}</span>
      : <><bdi>{device.label}</bdi> <span>{deviceId.slice(0, 8)}</span></>;
  };
  const revokeId = target?.kind === 'revoke' ? target.deviceId : null;
  const unlistedId = target?.kind === 'removeUnlisted' ? target.deviceId : null;
  const plainBanner = plainError === null ? null : <Banner tone="danger">{t('settings.error.other', { code: plainError.code })}</Banner>;
  const plainConfirm = (label: 'devices.confirmRevoke', onPress: () => void) => (
    <Button variant="danger" onClick={onPress} {...(plainBusy ? { 'aria-disabled': true } : {})}>
      {plainBusy ? t('devices.working') : t(label)}
    </Button>
  );

  return (
    <section className="dw-settings-section" aria-labelledby={TITLE_ID}>
      <h2 id={TITLE_ID} ref={headingRef} tabIndex={-1} className="dw-settings-heading">{t('devices.title')}</h2>
      {account?.rootMismatch === true ? (
        <Banner tone="danger">{t('devices.rootMismatch', { instance: account.instance?.name ?? '' })}</Banner>
      ) : null}
      {/* Privacy lists no own row; the core's word on the own key heads the own devices instead. */}
      {account?.ownTrust === 'changed' || account?.ownTrust === 'unlisted' ? (
        <Banner tone="danger">{t(account.ownTrust === 'changed' ? 'privacy.ownChanged' : 'privacy.ownUnlisted')}</Banner>
      ) : null}
      {loadError === null ? null : <Banner tone="danger">{t('settings.error.other', { code: loadError.code })}</Banner>}
      <p>{t('devices.body')}</p>
      <ul className="dw-device-list">
        {/* Signal task 10 fix round 3: Sign out lives on this browser's row, so while the list is missing (not loaded yet, or
            refused) this browser's row is drawn from the local identity and stays reachable. */}
        {!ordered.some(d => d.own) && account?.deviceId != null ? (
          <DeviceRow key="own" name={account.deviceId.slice(0, 8)} tier="web" own ownLabel={t('devices.thisBrowser')}
            action={{ label: t('workspace.signOut'), onAction: signOut.requestOpen }} />
        ) : null}
        {ordered.map(d => (
          // Fix round 2 (the prototype's sub line): "this browser · active now · d3f9a1" for this browser, "active 2 days ago ·
          // 8b20ce" for another; the id stays on every row, since a label is the server's to change.
          <DeviceRow key={d.id} name={d.label ?? d.id.slice(0, 8)} tier={d.tier === 1 ? 'web' : 'native'}
            own={d.own} ownLabel={d.own ? t('devices.thisBrowser') : undefined}
            state={d.revokedAt !== null ? t('devices.revoked') : !d.listed ? t('devices.unlisted') : undefined}
            lastSeen={d.own ? t('devices.subOwn', { id: d.id.slice(0, 6) })
              : d.lastSeen > 0 ? t('devices.subOther', { when: timeAgo(d.lastSeen, now), id: d.id.slice(0, 6) }) : d.id.slice(0, 6)}
            // Signal task 10 fix round 2 (Jonas): Rename is in the row's context menu (right-click, long press, Shift+F10).
            menu={d.revokedAt === null ? { label: t('signalSettings.menu.device', { name: d.label ?? d.id.slice(0, 8) }), items: [{ id: 'rename', label: t('devices.rename') }],
              onSelect: () => choose({ kind: 'rename', deviceId: d.id }) } : undefined}
            // README §5.9: this browser's row signs out (the prototype's plain button); every other live device is removed.
            action={d.revokedAt !== null ? undefined : d.own ? { label: t('workspace.signOut'), onAction: signOut.requestOpen } : {
              label: t('devices.revoke'), danger: true,
              onAction: () => choose(d.listed ? { kind: 'revoke', deviceId: d.id } : { kind: 'removeUnlisted', deviceId: d.id }),
            }} />
        ))}
      </ul>
      {signOut.dialog}

      <KeyDialog open={revokeId !== null} id="devices-revoke" subject={subjectOf(revokeId)} title={t('devices.revokeTitle')} body={t('devices.revokeBody')}
        confirmLabel={t('devices.confirmRevoke')} onClose={() => setTarget(null)}
        onConfirm={async key => {
          if (revokeId === null) return;
          await client.call({ m: 'revokeDevice', deviceId: revokeId, recoveryKey: key });
          focusHeading.current = true;
        }} />
      <RenameDialog open={renaming !== undefined} id="devices-rename" initial={renameStart} onClose={close}
        onSave={label => client.call({ m: 'renameDevice', deviceId: renaming?.id ?? '', label })} />
      <Dialog open={unlistedId !== null} title={t('devices.removeUnlistedTitle')} closeLabel={t('dialog.close')} onClose={close}
        footer={plainConfirm('devices.confirmRevoke', () => {
          if (unlistedId !== null) void confirmPlain({ m: 'revokeDevice', deviceId: unlistedId, recoveryKey: null }, true);
        })}>
        {plainBanner}
        <p className="dw-device-subject">{subjectOf(unlistedId)}</p>
        <p>{t('devices.removeUnlistedBody')}</p>
      </Dialog>
    </section>
  );
}
