import { useState } from 'react';
import { Banner, SettingsRow, Toggle } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import { useSlice } from '../../core/use-slice.ts';
import { t } from '../../strings/index.ts';
import { dmGifConsentKey } from '../conversation/dm-gif-consent.ts';

/** The same device-local setting read at the picker boundary; no server preference or account sync. */
export function DmGifPrivacySetting(): React.JSX.Element {
  const client = useCore();
  const key = dmGifConsentKey(useSlice('account'));
  const settings = useSlice('settings');
  const value = key === null ? undefined : settings?.[key];
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);
  const change = (on: boolean) => {
    if (key === null || busy) return;
    setFailed(false); setBusy(true);
    // An explicit settings enable still requires the named-server notice on the next use.
    void client.call({ m: 'setSetting', key, value: on ? 'on' : null })
      .catch(() => { setFailed(true); }).finally(() => { setBusy(false); });
  };
  return <>
    <SettingsRow id="privacy-dm-gifs" icon="image" label={t('gif.dm.title')} hint={t('gif.dm.hint')}>
      <Toggle id="privacy-dm-gifs-toggle" label={t('gif.dm.title')} checked={value !== undefined}
        disabled={key === null || busy} onChange={change} />
    </SettingsRow>
    {failed ? <Banner tone="danger">{t('workspace.preference.error')}</Banner> : null}
  </>;
}
