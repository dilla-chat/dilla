import { useEffect, useRef, useState } from 'react';
import { BotInteractionUncertainError, createBotInteractionClient, decodeMusicProviderCapability, decodeMusicProviderStatus, toHex, type MusicProviderStatusSnapshot } from '@dilla/client-core';
import { Button, SettingsRow } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import { useSlice } from '../../core/use-slice.ts';
import { t, type StringKey } from '../../strings/index.ts';
import { spotifyReturnDeadline } from '../../settings/spotify-return.ts';

const labels: Record<string, StringKey> = {
  unknown: 'workspace.providers.unknown', not_linked: 'workspace.providers.notLinked', linked: 'workspace.providers.linked',
  needs_attention: 'workspace.providers.attention', configured: 'workspace.providers.configured', missing_requirements: 'workspace.providers.missing',
};
const newRequestId = () => toHex(crypto.getRandomValues(new Uint8Array(16)));
/** How long each operation's answer may take, from that operation's own send. */
const ANSWER_DEADLINE_MS = 30_000;

/** Operation 24 observes runtime state. OAuth delivery and local configuration never imply an account is linked. */
export function MusicProviderStatus({ communityId, installationId }: { communityId: string; installationId: string | null }) {
  const client = useCore();
  const access = useSlice(`permissions:${communityId}`);
  const authorized = access?.loaded === true && access.manageCommunity;
  const [generation, refresh] = useState(0);
  const [snapshot, setSnapshot] = useState<MusicProviderStatusSnapshot | null>(null);
  // partial: operation 0 answered (the bot runs and reports provider status) but operation 24 did not in time.
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
    let timer: number | undefined;
    let answerTimer: number | undefined;
    let attempt = 0;
    let known = false;
    const canRead = () => !stopped && live.current.communityId === communityId && live.current.authorized;
    setSnapshot(null);
    if (!authorized || installationId === null) { setPhase('unavailable'); return; }
    setPhase('loading');
    const oauthDeadline = spotifyReturnDeadline(communityId);
    const interactions = createBotInteractionClient(client, communityId);
    let requestId = newRequestId();
    // Ask an operation understood by older bots before sending the new status operation.
    let operation: 0 | 24 = 0;
    // Each answer has its own deadline, measured from its own send (Jonas, 2026-10-30): a slow
    // operation 0 no longer eats operation 24's time. An operation 24 left unanswered keeps what
    // operation 0 showed, that the bot answered, and says to check again; a re-poll after an
    // observation keeps that observation.
    const unanswered = () => {
      if (known) return;
      setPhase(operation === 0 ? 'unavailable' : 'partial');
    };
    const later = (work: () => Promise<void>) => {
      if (!canRead()) return;
      if (oauthDeadline !== null && Date.now() >= oauthDeadline) { setPhase('unavailable'); return; }
      timer = window.setTimeout(() => void work(), 1000);
    };
    const request = async (): Promise<void> => {
      if (!canRead()) return;
      const mine = ++attempt;
      const current = () => canRead() && mine === attempt;
      // A worker request can itself stall before polling begins, so the deadline does not rely on it settling.
      window.clearTimeout(answerTimer);
      answerTimer = window.setTimeout(() => {
        if (!current()) return;
        stopped = true;
        if (timer !== undefined) window.clearTimeout(timer);
        unanswered();
      }, ANSWER_DEADLINE_MS);
      try {
        const response = await interactions.request({ requestId, operation, args: [], revision: null, channelId: null });
        if (!current()) return;
        window.clearTimeout(answerTimer);
        if (response.status !== 0) { setPhase('unavailable'); return; }
        if (operation === 0) {
          if (!decodeMusicProviderCapability(response.data)) { setPhase('unavailable'); return; }
          operation = 24;
          requestId = newRequestId();
          await request();
          return;
        }
        const observed = decodeMusicProviderStatus(response.data);
        if (toHex(observed.installationId) !== installationId || observed.observedAt > BigInt(Math.floor(Date.now() / 1000) + 5)) throw new Error('invalid observation');
        known = true;
        setSnapshot(observed); setNow(Date.now()); setPhase('known');
        if (oauthDeadline !== null && observed.spotifyAccount !== 'linked' && Date.now() < oauthDeadline) {
          requestId = newRequestId(); later(request);
        }
      } catch (error) {
        if (!current()) return;
        window.clearTimeout(answerTimer);
        // A response that never came is "not answered yet"; anything else (a malformed or foreign answer) is unavailable.
        if (error instanceof BotInteractionUncertainError) unanswered();
        else setPhase('unavailable');
      }
    };
    void request();
    return () => { stopped = true; window.clearTimeout(answerTimer); if (timer !== undefined) window.clearTimeout(timer); };
  }, [client, communityId, installationId, authorized, generation]);
  const shown = authorized ? snapshot : null;
  const stale = shown !== null && now / 1000 - Number(shown.observedAt) > 60;
  const current = phase === 'known' && !stale;
  const youtube = current && shown ? shown.youtubePlayback : 'unknown';
  const busy = authorized && phase === 'loading';
  // Task 10c, the prototype's Music page: YouTube is one row (its host status, a READY tag, "Check again"); Spotify's
  // observed account and setup follow as the row the Spotify link sits under.
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
    {shown ? <SettingsRow id="music-provider-spotify" icon="music" label={t('workspace.providers.spotifyAccount')}
      hint={`${t(labels[current ? shown.spotifyAccount : 'unknown'])} · ${t('workspace.providers.spotifySetup')}: ${t(labels[current ? shown.spotifyPlayback : 'unknown'])} · ${t('workspace.providers.observed', { time: new Date(Number(shown.observedAt) * 1000).toLocaleString() })}`}>
      <span />
    </SettingsRow> : null}
  </>;
}
