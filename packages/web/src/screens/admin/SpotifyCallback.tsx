import { useCallback, useEffect, useRef, useState } from 'react';
import { Button } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import { useSlice } from '../../core/use-slice.ts';
import { t } from '../../strings/index.ts';
import { spotifySession } from './spotify-link.ts';
import { noteSpotifyReturn } from '../../settings/spotify-return.ts';
import { replaceHistoryState, useRoute } from '../../router.ts';

const ATTEMPT_KEY = 'dilla.spotifyAttempt';
type Attempt = { communityId: string; attemptId: string; state: string; expires: string;
  instanceId: string; userId: string; deviceId: string };
type View = 'working' | 'waitingSession' | 'cancelReady' | 'delivered' | 'denied' | 'expired' | 'invalid' | 'failed' | 'cancelled' | 'cancelFailed';

function readAttempt(): Attempt | null {
  try {
    const raw = spotifySession.getItem(ATTEMPT_KEY);
    if (raw === null) return null;
    const value: unknown = JSON.parse(raw);
    if (typeof value !== 'object' || value === null) return null;
    const item = value as Record<string, unknown>;
    const expected = ['communityId', 'attemptId', 'state', 'expires', 'instanceId', 'userId', 'deviceId'];
    if (Object.keys(item).length !== expected.length || !expected.every(key => key in item)) return null;
    if (!['communityId', 'attemptId', 'instanceId', 'userId', 'deviceId'].every(key => typeof item[key] === 'string' && /^[0-9a-f]{32}$/.test(item[key]))) return null;
    if (typeof item.state !== 'string' || item.state.length < 1 || item.state.length > 256
      || typeof item.expires !== 'string' || !/^\d+$/.test(item.expires)) return null;
    return item as Attempt;
  } catch { return null; }
}

function removeAttempt(): void {
  try { spotifySession.removeItem(ATTEMPT_KEY); } catch { /* a blocked store cannot retain a usable attempt */ }
}

export function SpotifyCallback({ search }: { search?: string }): React.JSX.Element {
  const client = useCore();
  const account = useSlice('account');
  const [, navigate] = useRoute();
  const [view, setView] = useState<View>('working');
  const [busy, setBusy] = useState(false);
  const attemptRef = useRef<Attempt | null>(null);
  const codeRef = useRef<string | null>(null);
  const started = useRef(false);
  const scrubbed = useRef(false);
  const callbackSearch = useRef(search ?? window.location.search);

  const sendCallback = useCallback((attempt: Attempt, code: string) => {
    setView('working'); setBusy(true);
    void client.call({ m: 'callbackBotSpotifyAttempt', communityId: attempt.communityId,
      attemptId: attempt.attemptId, state: attempt.state, code }).then(() => {
      removeAttempt(); codeRef.current = null; setView('delivered');
      noteSpotifyReturn(attempt.communityId);
      void navigate({ name: 'serverSettings', communityId: attempt.communityId, section: 'music', from: null }, true, { from: `/c/${attempt.communityId}` });
    }).catch(() => setView('failed')).finally(() => setBusy(false));
  }, [client, navigate]);

  useEffect(() => {
    // Scrub the authorization code as soon as this route mounts, even while sign-in is required.
    if (!scrubbed.current) {
      replaceHistoryState(window.history.state, '', window.location.pathname);
      scrubbed.current = true;
    }
    if (started.current) return;
    if (account?.phase !== 'ready' || !account.instance?.id || !account.user?.id || !account.deviceId) {
      setView('waitingSession'); return;
    }
    started.current = true;
    const attempt = readAttempt();
    attemptRef.current = attempt;
    if (attempt === null || attempt.instanceId !== account.instance.id || attempt.userId !== account.user.id
      || attempt.deviceId !== account.deviceId) {
      removeAttempt(); setView('invalid'); return;
    }
    const params = new URLSearchParams(callbackSearch.current);
    const codes = params.getAll('code');
    const states = params.getAll('state');
    const errors = params.getAll('error');
    const code = codes.length === 1 ? codes[0] : null;
    const returnedState = states.length === 1 ? states[0] : null;
    const error = errors.length === 1 ? errors[0] : null;
    if ((code === null && error === null && returnedState === null) && codes.length === 0 && errors.length === 0 && states.length === 0) {
      setView('cancelReady'); return;
    }
    if (returnedState !== attempt.state || codes.length > 1 || states.length !== 1 || errors.length > 1) {
      removeAttempt(); setView('invalid'); return;
    }
    if (Number(attempt.expires) <= Math.floor(Date.now() / 1000)) {
      void client.call({ m: 'cancelBotSpotifyAttempt', communityId: attempt.communityId, attemptId: attempt.attemptId }).catch(() => undefined);
      removeAttempt(); setView('expired');
      return;
    }
    if (error !== null) {
      void client.call({ m: 'cancelBotSpotifyAttempt', communityId: attempt.communityId, attemptId: attempt.attemptId })
        .then(() => { removeAttempt(); setView('denied'); }).catch(() => setView('cancelFailed'));
      return;
    }
    if (code === null || code.length < 1 || code.length > 2048 || /[\r\n\0]/u.test(code)) {
      removeAttempt(); setView('invalid'); return;
    }
    // Keep this transient code only in component memory so an ambiguous network failure can retry.
    codeRef.current = code;
    sendCallback(attempt, code);
  }, [account, client, sendCallback]);

  const cancel = () => {
    const attempt = attemptRef.current;
    if (attempt === null || busy) return;
    setBusy(true);
    void client.call({ m: 'cancelBotSpotifyAttempt', communityId: attempt.communityId, attemptId: attempt.attemptId })
      .then(() => { removeAttempt(); codeRef.current = null; setView('cancelled'); })
      .catch(() => setView('cancelFailed')).finally(() => setBusy(false));
  };

  const message: Partial<Record<View, string>> = {
    delivered: t('workspace.providers.callback'), denied: t('admin.bot.music.callbackDenied'),
    expired: t('admin.bot.music.callbackExpired'), invalid: t('admin.bot.music.callbackInvalid'),
    failed: t('admin.bot.music.callbackFailed'), cancelled: t('admin.bot.music.callbackCancelled'),
    cancelFailed: t('admin.bot.music.callbackCancelError'),
  };
  const returnToServer = () => {
    const communityId = attemptRef.current?.communityId;
    if (communityId !== undefined) void navigate({ name: 'serverSettings', communityId, section: 'music', from: null }, true, { from: `/c/${communityId}` });
  };
  return <main className="dw-settings-frame" aria-labelledby="spotify-callback-title">
    <h1 id="spotify-callback-title">{t('admin.bot.music.spotify')}</h1>
    {view === 'working' ? <p role="status" aria-busy="true">{t('admin.bot.music.spotifyLinking')}</p> : null}
    {view === 'waitingSession' ? <p role="status">{t('admin.bot.music.callbackSignIn')}</p> : null}
    {message[view] ? <p role={view === 'invalid' || view === 'failed' || view === 'cancelFailed' ? 'alert' : 'status'}>{message[view]}</p> : null}
    {(view === 'failed' || view === 'cancelFailed') && codeRef.current !== null && attemptRef.current !== null ? <Button aria-disabled={busy || undefined}
      onClick={() => sendCallback(attemptRef.current!, codeRef.current!)}>{t('admin.bot.music.callbackRetry')}</Button> : null}
    {view === 'cancelReady' ? <Button aria-disabled={busy || undefined} onClick={cancel}>
      {busy ? t('admin.bot.music.callbackCancelling') : t('admin.bot.music.callbackCancel')}
    </Button> : null}
    {(view === 'failed' || view === 'cancelFailed') && attemptRef.current !== null ? <Button aria-disabled={busy || undefined} onClick={cancel}>
      {busy ? t('admin.bot.music.callbackCancelling') : t('admin.bot.music.callbackCancel')}
    </Button> : null}
    {view === 'delivered' || view === 'denied' || view === 'expired' || view === 'cancelled' ? <Button onClick={returnToServer}>
      {t('admin.bot.music.callbackReturn')}
    </Button> : null}
  </main>;
}
