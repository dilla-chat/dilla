import type { Meta, StoryObj } from '@storybook/react-vite';
import { useEffect, useRef, type ReactNode } from 'react';
import { SafetyCompare } from '../SafetyCompare/SafetyCompare.tsx';
import { TrustAlert } from '../TrustAlert/TrustAlert.tsx';
import { SearchPalette } from '../SearchPalette/SearchPalette.tsx';
import { DiagnosticsPanel } from '../DiagnosticsPanel/DiagnosticsPanel.tsx';
import { TabHandover } from '../TabHandover/TabHandover.tsx';
import { TypingIndicator } from '../TypingIndicator/TypingIndicator.tsx';
import { en, t } from '../../../web/src/strings/index.ts';

const meta = { id: 'screens-trustandpolish', title: 'Screens/TrustAndPolish', parameters: { layout: 'fullscreen' } } satisfies Meta;
export default meta;
type Story = StoryObj<typeof meta>;
const noop = () => {};
const frame = (child: ReactNode) => <main style={{ maxWidth: 820, margin: 'auto', padding: 24 }}>{child}</main>;
function FocusSearch({ children }: { children: ReactNode }) {
  const root = useRef<HTMLElement>(null);
  useEffect(() => { root.current?.querySelector<HTMLInputElement>('input')?.focus(); }, []);
  return <section ref={root} data-doc-state="search-results">{children}</section>;
}
const compareLabels = {
  title: t('privacy.title'), copy: t('privacy.copy'), verify: t('privacy.verify'), close: t('dialog.close'),
  instructions: t('privacy.instructions'), confirm: t('privacy.confirm'), firstSeen: t('privacy.firstSeen', { name: 'Ada' }),
  states: { 'first-seen': t('privacy.firstSeenLabel'), changed: t('privacy.changed'), verified: t('privacy.verifiedLabel'), unlisted: t('privacy.unlisted') },
};

export const Privacy: Story = { render: () => frame(<section data-doc-state="trust-privacy">
  <h1>{t('privacy.section')}</h1><p>Ada</p><p>{t('privacy.firstSeenLabel')}</p><p>{t('privacy.firstSeen', { name: 'Ada' })}</p>
  <button type="button">{t('privacy.compare', { name: 'Ada' })}</button>
</section>) };

export const Changed: Story = { render: () => frame(<section data-doc-state="trust-changed">
  <h1>{t('privacy.section')}</h1><TrustAlert kind="changed" name="Ada" onVerify={noop}
    labels={{ changed: t('privacy.changed'), unlisted: t('privacy.unlisted'), verify: t('privacy.compare', { name: 'Ada' }) }} />
  <SafetyCompare open contactName="Ada" state="changed" approvedUmk={'ab'.repeat(32)}
    groups={Array.from({ length: 12 }, (_, i) => String(i + 1).padStart(5, '0'))} canVerify
    labels={compareLabels} onCopy={noop} onVerify={noop} onClose={noop} />
</section>) };

export const SearchResults: Story = { render: () => frame(<FocusSearch>
  <SearchPalette open query="lamp" groups={[{ id: 'messages', label: t('search.group'), items: [{ id: 'hit-1', label: 'a lamp beside the table', sub: '#general', icon: 'message-square' }] }]}
    labels={{ search: t('search.placeholder'), placeholder: t('search.placeholder'), empty: t('search.empty', { query: 'bridge' }), close: t('dialog.close') }} onQuery={noop} onPick={noop} onClose={noop} />
  <TypingIndicator names={['Ada']} labels={{ one: en['typing.one'], many: t('typing.many') }} />
</FocusSearch>) };

export const Diagnostics: Story = { render: () => frame(<section data-doc-state="settings-diagnostics">
  <DiagnosticsPanel data={{ connection: 'online', generation: '8', instance: 'dilla.test', devices: 2,
    errors: [{ code: 'E_NOT_FOUND', at: 1 }] }}
    labels={{ title: t('diagnostics.title'), copy: t('diagnostics.copy'), fields: {
      connection: t('diagnostics.connection'), generation: t('diagnostics.generation'),
      instance: t('diagnostics.instance'), devices: t('diagnostics.devices', { n: 2 }),
    } }} onCopy={noop} />
</section>) };

export const Handover: Story = { render: () => frame(<section data-doc-state="tabs-handover">
  <TabHandover phase="waiting" labels={{ otherTab: t('boot.otherTab.status'), use: t('tabs.use'), wait: t('tabs.wait'), failed: t('tabs.failed'), paused: t('tabs.paused') }}
    onUse={noop} onRetry={noop} />
</section>) };
