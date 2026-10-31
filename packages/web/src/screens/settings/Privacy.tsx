import type { ReactNode } from 'react';
import type { TrustSummary } from '@dilla/client-core';
import { Button, SettingsRow, TrustAlert } from '@dilla/ui';
import { t } from '../../strings/index.ts';

/**
 * The contacts this device pinned, in Flow 05's states: changed (an alert that cannot be dismissed), unlisted (no
 * comparison), verified (the compared public key) or first seen here. Names and the shared flag come from the worker's
 * trust slice only; the worker and core own every trust decision.
 */
export function Privacy({ records, onCompare, children }: { children?: ReactNode; records: readonly TrustSummary[]; onCompare: (record: TrustSummary) => void }): React.JSX.Element {
  return <section className="dw-settings-section" data-doc-state="trust-privacy" aria-labelledby="privacy-title">
    <h2 id="privacy-title" tabIndex={-1} data-privacy-heading>{t('privacy.section')}</h2>
    {children}
    <ul className="dw-trust-list">{records.map(record => {
      const compare = () => onCompare(record);
      // C1: a changed-key verdict with no pending key is changed too, but there is no key to compare; only a Remove ends it.
      const changed = record.changedUmk !== null || record.blocked === 'E_UMK_CHANGED';
      const comparable = record.changedUmk !== null;
      const unlisted = record.unlisted === true;
      const compareButton = <Button variant="ghost" onClick={compare}>{t('privacy.compare', { name: record.name })}</Button>;
      return <li key={record.userId} data-doc-state={changed ? 'trust-changed' : undefined}>
        <SettingsRow id={`trust-${record.userId}`} label={record.name}>
          {/* The alert is for a contact the person can act on here; otherwise the state is plain text (M-04). */}
          {changed && record.shared ? <TrustAlert kind="changed" name={record.name}
            labels={{ changed: t(comparable ? 'privacy.changed' : 'privacy.changedNoKey'), unlisted: t('privacy.unlisted'), verify: t('privacy.compare', { name: record.name }) }}
            onVerify={comparable ? compare : undefined} /> : null}
          {changed && !record.shared ? <p className="dw-trust-state" data-tone="danger">{t(comparable ? 'privacy.changed' : 'privacy.changedNoKey')}</p>
            : null}
          {unlisted ? <p className="dw-trust-state" data-tone="warn">{t('privacy.unlisted')}</p>
            : changed ? null
            : record.verified ? <><p className="d-label dw-trust-state">{t('privacy.verifiedLabel')}</p><code className="dw-trust-key">{record.umk}</code></>
            : <><p className="d-label dw-trust-state">{t('privacy.firstSeenLabel')}</p><p>{t('privacy.firstSeen', { name: record.name })}</p></>}
          {!record.shared ? <p>{t('privacy.noShared')}</p> : !changed && !unlisted ? compareButton : null}
        </SettingsRow>
      </li>;
    })}</ul>
  </section>;
}
