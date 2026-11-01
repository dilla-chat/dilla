import { useState, type ReactNode } from 'react';
import { Button, Icon } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import { t } from '../../strings/index.ts';
import { KeyDialog } from './KeyDialog.tsx';
import { useSettingsWorkspace, useSettingsPendingOperation } from '../../settings/edit-state.tsx';

const LOG_OUT = 'log-out';

/**
 * The danger button that signs this browser out (devices.signOut) and its recovery-key dialog, shared by Settings →
 * Devices and the phone's You tab. The worker wipes the store and publishes `cleared`, which the App answers with a
 * reload; nothing here navigates. `icon` puts the log-out icon before the label (H6, the phone's You tab). The dialog hands focus back to the button when it closes.
 */
export function SignOutAction({ icon = false, label }: { icon?: boolean; label?: string } = {}): React.JSX.Element {
  const { requestOpen, dialog } = useSignOut();
  return (
    <>
      <Button variant="danger" onClick={requestOpen}>{icon ? <span style={{ display: 'inline-flex', alignItems: 'center', gap: '0.75rem' }}><Icon name={LOG_OUT} size={20} />{t('devices.signOut')}</span> : label ?? t('devices.signOut')}</Button>
      {dialog}
    </>
  );
}

/**
 * The sign-out flow without its button (Signal task 10 fix round 1): Settings → Devices draws Sign out on this device's own
 * row, as the prototype does. `requestOpen` passes the workspace's unsaved-changes prompt first.
 */
export function useSignOut(): { requestOpen: () => void; dialog: ReactNode } {
  const client = useCore();
  const workspace = useSettingsWorkspace();
  const [open, setOpen] = useState(false);
  const [pending, setPending] = useState(false);
  useSettingsPendingOperation(pending);
  const requestOpen = () => {
    const openDialog = () => setOpen(true);
    if (workspace) void workspace.requestNavigation(openDialog);
    else openDialog();
  };
  const confirm = async (key: string) => {
    setPending(true);
    try { await client.call({ m: 'signOutRevoke', recoveryKey: key }); }
    finally { setPending(false); }
  };
  return {
    requestOpen,
    dialog: <KeyDialog open={open} id="devices-signout" title={t('devices.signOutTitle')} body={t('devices.signOutBody')}
      confirmLabel={t('devices.confirmSignOut')} onClose={() => setOpen(false)} onConfirm={confirm} />,
  };
}
