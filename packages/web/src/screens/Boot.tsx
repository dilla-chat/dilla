import { useEffect, useState } from 'react';
import type { AccountState } from '@dilla/client-core';
import { Button, Dialog, Splash } from '@dilla/ui';
import { browser } from '../browser.ts';
import { mobileBridge } from '../mobile.ts';
import { useCore } from '../core/context.tsx';
import { errorOf, type UiError } from '../core/errors.ts';
import { useSlice } from '../core/use-slice.ts';
import { t } from '../strings/index.ts';

type Phase = 'loading' | 'unsupported' | 'other-tab' | 'store-lost' | 'resetting' | 'revoked' | 'store-newer' | 'error';

function bootPhase(account: AccountState | undefined, fatal: UiError | null, resetError: UiError | null, resetting: boolean): Phase {
  if (fatal || resetError) return 'error';
  if (resetting) return 'resetting';
  // C4x: a store a newer version of dilla wrote is refused untouched; it is explained, never offered for a reset.
  if (account?.phase === 'error' && account.error?.code === 'E_STORE_NEWER' && account.error.store) return 'store-newer';
  switch (account?.phase) {
    case 'unsupported': case 'other-tab': case 'store-lost': case 'revoked': case 'error': return account.phase;
    default: return 'loading';
  }
}

export function Boot(props: { fatal: UiError | null }): React.JSX.Element {
  const client = useCore();
  const account = useSlice('account');
  const [confirming, setConfirming] = useState(false);
  const [resetting, setResetting] = useState(false);
  const [resetError, setResetError] = useState<UiError | null>(null);
  useEffect(() => { if (account?.phase !== 'store-lost' && account?.phase !== 'revoked') setResetting(false); }, [account?.phase]);

  const phase = bootPhase(account, props.fatal, resetError, resetting);
  const reset = () => {
    setConfirming(false);
    setResetting(true);
    // The clearing splash lasts while the call is pending (WEB-APP-02): a reset that settles with the store still
    // lost shows the store-lost splash and its reset again, never the splash for ever.
    void client.call({ m: 'resetDevice' })
      .catch(e => { setResetError(errorOf(e)); })
      .finally(() => setResetting(false));
  };
  // A revoked browser can forget itself (task 14 ruling (d)): the worker wipes its store and ends in `cleared`, which
  // the App answers with a reload; a refused wipe leaves `revoked` and is shown as an error, with Reload.
  const forget = () => {
    setConfirming(false);
    setResetting(true);
    void client.call({ m: 'forgetBrowser' })
      .catch(e => { setResetError(errorOf(e)); })
      .finally(() => setResetting(false));
  };
  const splash = (() => {
    switch (phase) {
      case 'unsupported': return <Splash status={t('boot.unsupported.status')} detail={t('boot.unsupported.detail')} />;
      case 'other-tab': return <Splash status={t('boot.otherTab.status')} detail={t('boot.otherTab.detail')} />;
      case 'store-lost': return <Splash status={t('boot.storeLost.status')} detail={t('boot.storeLost.detail')}
        action={{ label: t('boot.storeLost.action'), onAction: () => setConfirming(true) }} />;
      case 'resetting': return <Splash status={t('boot.resetting.status')} />;
      case 'revoked': return <Splash status={t('boot.revoked.status')} detail={t('boot.revoked.detail')}
        action={{ label: t('boot.revoked.action'), onAction: () => setConfirming(true) }} />;
      case 'store-newer': return <Splash status={t('boot.storeNewer.status')}
        detail={t('boot.storeNewer.detail', { found: account?.error?.store?.found ?? 0, supported: account?.error?.store?.supported ?? 0 })}
        action={{ label: t('boot.storeNewer.action'), onAction: () => browser.reload() }} />;
      case 'error': {
        const code = (props.fatal ?? resetError ?? account?.error)?.code ?? 'E_UNKNOWN';
        // The mobile app (final review I2): app words, and a way back to its instance picker when this one fails.
        const mobile = mobileBridge();
        if (mobile !== undefined) return <Splash status={t('boot.error.status')} detail={t('mobile.boot.error.detail', { code })}
          action={{ label: t('mobile.retry'), onAction: () => browser.reload() }}
          secondary={{ label: t('mobile.switchInstance'), onAction: () => { void mobile.send({ type: 'switch-instance' }).catch(() => undefined); } }} />;
        return <Splash status={t('boot.error.status')} detail={t('boot.error.detail', { code })}
          action={{ label: t('boot.error.action'), onAction: () => browser.reload() }} />;
      }
      default: return <Splash status={t('boot.loading.status')} />;
    }
  })();
  return <>
    {splash}
    {phase === 'store-lost' && <Dialog open={confirming} title={t('boot.storeLost.confirmTitle')}
      closeLabel={t('dialog.close')} onClose={() => setConfirming(false)}
      footer={<Button variant="danger" onClick={reset}>{t('boot.storeLost.confirmAction')}</Button>}>
      <p>{t('boot.storeLost.confirmBody')}</p>
    </Dialog>}
    {phase === 'revoked' && <Dialog open={confirming} title={t('boot.revoked.confirmTitle')}
      closeLabel={t('dialog.close')} onClose={() => setConfirming(false)}
      footer={<Button variant="danger" onClick={forget}>{t('boot.revoked.confirmAction')}</Button>}>
      <p>{t('boot.revoked.confirmBody')}</p>
    </Dialog>}
  </>;
}
