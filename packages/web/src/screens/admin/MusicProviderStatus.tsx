import { useEffect, useRef, useState } from 'react';
import { BotOperationUncertainError, catalogOffers, createBotOperations, decodeMusicProviderStatus, toHex, type MusicProviderStatusSnapshot } from '@dilla/client-core';
import { Button, SettingsRow } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import { useSlice } from '../../core/use-slice.ts';
import { t } from '../../strings/index.ts';

/** Operation 24 observes runtime playback requirements. Bot v2 Task 21b: it is a sealed command (app operation 124), sent
 *  only when the catalog this device holds offers it; the run's own 5 s bound decides when it went unanswered. */
export function MusicProviderStatus({ communityId, installationId }: { communityId: string; installationId: string | null }) {
  const client = useCore();
  const access = useSlice(`permissions:${communityId}`);
  const authorized = access?.loaded === true && access.manageCommunity;
  // The catalog answers what operation 0 used to: whether this Bot reports provider status at all.
  const catalog = useSlice(`botCatalog:${communityId}`);
  const offered = catalog !== undefined && catalog !== null && catalogOffers(client, communityId, 24);
  const [generation, refresh] = useState(0);
  const [snapshot, setSnapshot] = useState<MusicProviderStatusSnapshot | null>(null);
  // partial: the Bot offers provider status but did not answer in time.
  const [phase, setPhase] = useState<'loading' | 'known' | 'partial' | 'unavailable'>('loading');
  const [now, setNow] = useState(Date.now());
  const live = useRef({ communityId, authorized });
  live.current = { communityId, authorized };
  useEffect(() => {
    const clock = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(clock);
  }, []);
  useEffect(() => {
    let stopped = false;
    const current = () => !stopped && live.current.communityId === communityId && live.current.authorized;
    setSnapshot(null);
    if (!authorized || installationId === null || !offered) { setPhase('unavailable'); return; }
    setPhase('loading');
    void (async () => {
      try {
        const response = await createBotOperations(client, communityId).request({ operation: 24, args: [], channelId: null });
        if (!current()) return;
        if (response.status !== 0) { setPhase('unavailable'); return; }
        const observed = decodeMusicProviderStatus(response.data);
        if (toHex(observed.installationId) !== installationId || observed.observedAt > BigInt(Math.floor(Date.now() / 1000) + 5)) throw new Error('invalid observation');
        setSnapshot(observed); setNow(Date.now()); setPhase('known');
      } catch (error) {
        if (!current()) return;
        // An answer that never came is "not answered yet"; anything else (refused, malformed or foreign) is unavailable.
        setPhase(error instanceof BotOperationUncertainError ? 'partial' : 'unavailable');
      }
    })();
    return () => { stopped = true; };
  }, [client, communityId, installationId, authorized, offered, generation]);
  const shown = authorized ? snapshot : null;
  const stale = shown !== null && now / 1000 - Number(shown.observedAt) > 60;
  const current = phase === 'known' && !stale;
  const youtube = current && shown ? shown.youtubePlayback : 'unknown';
  const busy = authorized && phase === 'loading';
  const youtubeHint = busy ? t('workspace.providers.loading') : stale ? t('workspace.providers.stale')
    : phase === 'unavailable' || !authorized ? t('workspace.providers.unavailable')
      : phase === 'partial' ? t('workspace.providers.partial')
      : youtube === 'configured' ? t('serverSettings.music.youtubeReady')
        : youtube === 'missing_requirements' ? t('serverSettings.music.youtubeMissing') : t('serverSettings.music.youtubeUnknown');
  // No wrapper: the rows must stay siblings of the page's other rows so they draw one group (README §5.9).
  return <>
    <SettingsRow id="music-provider-youtube" anchor="music-providers" icon="youtube" label={t('serverSettings.music.youtube')} hint={youtubeHint}
      {...(youtube === 'configured' && !stale ? { tag: t('serverSettings.music.youtubeTag') } : {})}>
      <Button size="sm" aria-busy={busy || undefined} aria-disabled={!authorized || installationId === null || busy || undefined}
        onClick={() => { if (authorized && installationId !== null && !busy) refresh(value => value + 1); }}>{t('serverSettings.music.checkAgain')}</Button>
      <span className="d-sr-only" role="status">{youtubeHint}</span>
    </SettingsRow>
  </>;
}
