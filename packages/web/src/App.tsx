import { useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react';
import type { BootPhase, FactorSetupOrigin } from '@dilla/client-core';
import { Banner } from '@dilla/ui';
import { t } from './strings/index.ts';
import { browser } from './browser.ts';
import { useCore } from './core/context.tsx';
import { errorOf, type UiError } from './core/errors.ts';
import { useSlice } from './core/use-slice.ts';
import { notificationCtor, startNotifier } from './notify.ts';
import { requestThread } from './thread-request.ts';
import { joinErrorState, replaceHistoryState, useRoute, type Route } from './router.ts';
import { Boot } from './screens/Boot.tsx';
import { ForcedFactor } from './screens/factor/ForcedFactor.tsx';
import { Onboarding, type SignupResult } from './screens/Onboarding.tsx';
import { Shell } from './screens/Shell.tsx';
import { visibleChannel } from './screens/shell-model.ts';
import { SignIn } from './screens/SignIn.tsx';
import { OtherTab } from './screens/OtherTab.tsx';
import { SpotifyCallback } from './screens/admin/SpotifyCallback.tsx';
import { requestHandover } from './handover.ts';
import { flushSync } from 'react-dom';
import type { ShellUpdateDeps } from './screens/Shell.tsx';
import { updateTabId, type CheckpointScope } from './update-checkpoint.ts';

export type Screen = 'boot' | 'onboarding' | 'signin' | 'shell' | 'factor';

/** web3.6: the set-up of a second factor belongs to the ceremony that led to it: sign-up, sign-in, or (an account found
 *  owing, with no ceremony) the forced screen. A factor-setup phase with no origin is the forced one. */
export function screenFor(phase: BootPhase | undefined, origin?: FactorSetupOrigin | null): Screen {
  switch (phase) {
    case 'needs-signup': case 'signup-keys': case 'registering': return 'onboarding';
    case 'signin-login': case 'signin-totp': case 'signin-passkey': case 'signin-key': case 'enrolling': return 'signin';
    case 'factor-setup': return origin === 'signup' ? 'onboarding' : origin === 'signin' ? 'signin' : 'factor';
    // The worker holds no store after a wipe: the Boot splash shows while the page reloads itself (L-TS-27).
    case 'cleared': return 'boot';
    case 'ready': return 'shell';
    default: return 'boot';
  }
}

function defaultReload(href: string): void {
  window.location.replace(href);
}

export function App(props: { fatal: UiError | null; reload?: (href: string) => void; update?: ShellUpdateDeps }): React.JSX.Element {
  const client = useCore();
  const [route, navigate] = useRoute();
  const callbackSearch = route.name === 'spotifyCallback' ? route.search : '';
  const spotifyCallbackSearch = useRef(callbackSearch);
  const spotifyCallbackScrubbed = useRef(false);
  useLayoutEffect(() => {
    if (route.name !== 'spotifyCallback') {
      spotifyCallbackScrubbed.current = false;
      spotifyCallbackSearch.current = '';
      return;
    }
    if (spotifyCallbackScrubbed.current) return;
    // Capture only in this page's memory, then remove the authorization code before Boot or sign-in can be shown.
    spotifyCallbackSearch.current = callbackSearch;
    replaceHistoryState(window.history.state, '', window.location.pathname);
    spotifyCallbackScrubbed.current = true;
  }, [route.name, callbackSearch]);
  const account = useSlice('account');
  const updateSignedIn = account?.phase === 'ready';
  const [preLoginUpdate, setPreLoginUpdate] = useState(false);
  useEffect(() => {
    if (props.update === undefined || updateSignedIn) return;
    return props.update.watch({ currentEntry: props.update.currentEntry, fetch: window.fetch.bind(window),
      changed: () => setPreLoginUpdate(true) });
  }, [props.update, updateSignedIn]);
  const checkpointOwner = useRef<CheckpointScope | null>(null);
  useEffect(() => {
    if (props.update === undefined) return;
    const previous = checkpointOwner.current;
    const current = account?.phase === 'ready' && account.instance?.id && account.user?.id
      ? (() => {
        try {
          // eslint-disable-next-line no-restricted-globals -- only a random content-free tab identifier enters Web Storage
          const tab = updateTabId(sessionStorage, crypto);
          return { instance: account.instance.id, user: account.user.id, tab };
        } catch { return null; }
      })() : null;
    if (previous !== null && (current === null || previous.instance !== current.instance || previous.user !== current.user)) {
      void props.update.store.remove(previous).catch(() => undefined);
    }
    checkpointOwner.current = current;
  }, [account?.phase, account?.instance?.id, account?.user?.id, props.update]);
  const diagnostics = useSlice('diagnostics');
  const settings = useSlice('settings');
  const [tabPhase, setTabPhase] = useState<'other-tab' | 'waiting' | 'failed' | 'paused' | 'confirm'>('other-tab');
  const call = useSlice('call');
  /** Product walk I7: whether the tab that holds dilla said it is in a call (false until it says so). */
  const [otherInCall, setOtherInCall] = useState(false);
  // The old leader's side of hand-over: 'resigning' shows the paused view over the still mounted shell and moves focus
  // into it before the shell (and its composer) leaves; 'released' follows the worker's release. One resign at a time.
  const [paused, setPaused] = useState<'no' | 'resigning' | 'released'>('no');
  const resigning = useRef(false);
  const [startError, setStartError] = useState<UiError | null>(null);
  useEffect(() => { client.call({ m: 'start' }).catch(e => setStartError(errorOf(e))); }, [client]);
  useEffect(() => {
    document.documentElement.dataset.density = settings?.density === 'compact' ? 'compact' : 'regular';
    document.documentElement.dataset.reduceMotion = settings?.reduceMotion === 'on' || settings?.reduceMotion === 'off'
      ? settings.reduceMotion : 'system';
  }, [settings?.density, settings?.reduceMotion]);
  const persisted = useRef(false);
  useEffect(() => {
    if (account?.phase === 'ready' && !persisted.current) {
      persisted.current = true;
      void browser.persistStorage().catch(() => false);
    }
  }, [account?.phase]);
  // Head ruling 26: `cleared` accepts nothing but `start`, so a reload is the only way on; once per mount, so a
  // republished slice or StrictMode's double effect never reloads twice. After a list race the query carries the
  // message to the connect step.
  const reloaded = useRef(false);
  const reload = props.reload;
  const phase = account?.phase;
  // Real input in the active tab refreshes its device at most once every five seconds. Hidden tabs send no
  // keepalive; the server derives Idle after ten minutes without activity on every live device.
  useEffect(() => {
    if (phase !== 'ready') return;
    let last = -Infinity;
    const activity = () => {
      if (document.visibilityState !== 'visible' || Date.now() - last < 5_000) return;
      last = Date.now();
      void client.call({ m: 'presenceActivity' }).catch(() => undefined);
    };
    window.addEventListener('pointerdown', activity);
    window.addEventListener('pointermove', activity);
    window.addEventListener('keydown', activity);
    return () => {
      window.removeEventListener('pointerdown', activity);
      window.removeEventListener('pointermove', activity);
      window.removeEventListener('keydown', activity);
    };
  }, [client, phase]);
  // Walk M9: the forced screen holds dilla as the shell does, so a tab that asks for it ("Use this tab") is answered there too.
  const holdsDilla = phase === 'ready' || (phase === 'factor-setup' && account?.factorSetup?.origin === 'required');
  useEffect(() => {
    const instance = diagnostics?.instance;
    if (!holdsDilla || !instance || !/^[0-9a-f]{32}$/.test(instance) || typeof BroadcastChannel === 'undefined') return;
    const channel = new BroadcastChannel(`dilla-core:${instance}`);
    const onMessage = (event: MessageEvent) => {
      const data: unknown = event.data;
      if (typeof data !== 'object' || data === null) return;
      const value = data as Record<string, unknown>;
      if (Object.keys(value).length !== 3 || value.t !== 'takeover' || value.instance !== instance
        || typeof value.nonce !== 'string' || !/^[0-9a-f]{32}$/.test(value.nonce)) return;
      if (resigning.current) return;
      resigning.current = true;
      flushSync(() => { setTabPhase('other-tab'); setPaused('resigning'); });
      document.querySelector<HTMLElement>('.dw-other-tab h2')?.focus();
      void client.call({ m: 'resign', nonce: value.nonce }).then(() => {
        const release = new BroadcastChannel(`dilla-core:${instance}`);
        release.postMessage({ t: 'released', instance, nonce: value.nonce });
        release.close();
        setPaused('released');
      }).catch(() => { resigning.current = false; setPaused('no'); });
    };
    channel.addEventListener('message', onMessage);
    return () => { channel.removeEventListener('message', onMessage); channel.close(); };
  }, [client, diagnostics?.instance, holdsDilla]);
  // Product walk I7: the tabs say, content-free, whether this browser's dilla is in a call ({t:'voice', instance, on}),
  // so a tab about to take over warns before it ends that call. The tab that holds dilla says it on every change and
  // when asked ({t:'voice?', instance}); a waiting tab asks once and keeps the last answer.
  const inCall = call !== undefined && (call.phase === 'joining' || call.phase === 'in' || call.phase === 'paused');
  const voiceInstance = diagnostics?.instance;
  const waitingTab = phase === 'other-tab' || paused === 'released';
  useEffect(() => {
    if (!voiceInstance || !/^[0-9a-f]{32}$/.test(voiceInstance) || typeof BroadcastChannel === 'undefined') return;
    if (phase !== 'ready' && !waitingTab) return;
    const channel = new BroadcastChannel(`dilla-voice:${voiceInstance}`);
    const leads = phase === 'ready' && paused === 'no';
    const say = () => channel.postMessage({ t: 'voice', instance: voiceInstance, on: inCall });
    const onMessage = (event: MessageEvent) => {
      const data: unknown = event.data;
      if (typeof data !== 'object' || data === null) return;
      const value = data as Record<string, unknown>;
      if (value.instance !== voiceInstance) return;
      const keys = Object.keys(value).length;
      if (leads && value.t === 'voice?' && keys === 2) say();
      if (!leads && value.t === 'voice' && keys === 3 && typeof value.on === 'boolean') setOtherInCall(value.on);
    };
    channel.addEventListener('message', onMessage);
    if (leads) say(); else channel.postMessage({ t: 'voice?', instance: voiceInstance });
    return () => { channel.removeEventListener('message', onMessage); channel.close(); };
  }, [voiceInstance, phase, paused, waitingTab, inCall]);
  // C1 hand-back: after a release the worker waits for the lock again (other-tab); once it leads (the other tab handed
  // back or closed) it boots from the store, and from then on the page follows the worker's phase again: the shell when
  // it is ready, the splash or an error otherwise.
  const leftReady = useRef(false);
  useEffect(() => {
    if (paused === 'no') { leftReady.current = false; return; }
    if (phase === 'ready' && !leftReady.current) return;
    if (phase === 'other-tab' || phase === 'paused-error' || phase === undefined) { leftReady.current = true; return; }
    if (paused === 'released' && leftReady.current) {
      leftReady.current = false;
      resigning.current = false;
      setTabPhase('other-tab');
      setPaused('no');
    }
  }, [paused, phase]);
  const takeThisTab = (confirmed = false) => {
    const instance = diagnostics?.instance;
    if (!instance) { setTabPhase('failed'); return; }
    // Product walk I7: taking over ends the other tab's call; ask first.
    if (otherInCall && !confirmed) { setTabPhase('confirm'); return; }
    setTabPhase('waiting');
    void requestHandover(instance, name => new BroadcastChannel(name), async () => {
      await client.call({ m: 'start' });
    }).catch(() => setTabPhase('failed'));
  };
  const errorCode = account?.error?.code;
  useEffect(() => {
    if (phase !== 'cleared' || reloaded.current) return;
    reloaded.current = true;
    (reload ?? defaultReload)(errorCode === 'E_LIST_RACE' ? '/welcome?signin=race'
      : errorCode === 'E_SIGNIN_EVICTED' ? '/welcome?signin=evicted' : '/');
  }, [phase, errorCode, reload]);
  // The route as of the last commit, for the notifier's callbacks: the channel on screen, and whether Settings is open.
  const shown = useRef<Route>(route);
  useEffect(() => { shown.current = route; }, [route]);
  // Desktop notifications (Q30, L-TS-25): the page follows the worker's notices once per client. A click opens the
  // notice's channel or DM, and the thread of a reply that shows in one; one that leaves Settings that way must not then
  // pull focus back to the rail.
  const leftByNotice = useRef(false);
  useEffect(() => startNotifier({
    client,
    Notification: notificationCtor(),
    visible: () => document.visibilityState === 'visible',
    onScreen: () => visibleChannel(shown.current),
    focus: () => window.focus(),
    open: n => {
      leftByNotice.current = shown.current.name === 'settings';
      void navigate(n.communityId === null ? { name: 'dm', channelId: n.channelId } : { name: 'channel', communityId: n.communityId, channelId: n.channelId }).then(allowed => {
        if (allowed && n.threadRoot !== null && n.communityId !== null) requestThread(n.channelId, n.threadRoot);
      });
    },
  }), [client, navigate]);
  const screen = screenFor(phase, account?.factorSetup?.origin);
  // Onboarding stays mounted after the phase turns ready, until its done step calls onFinish; so does SignIn.
  const [onboarding, setOnboarding] = useState(false);
  const [signingIn, setSigningIn] = useState(false);
  const signInForCallback = useRef(false);
  useEffect(() => {
    if (screen === 'onboarding') { setOnboarding(true); setSigningIn(false); }
    if (screen === 'signin') { signInForCallback.current = route.name === 'spotifyCallback'; setSigningIn(true); setOnboarding(false); }
  }, [screen, route.name]);
  const finish = (result: SignupResult) => {
    setOnboarding(false);
    if (result.communityId !== null) {
      void navigate({ name: 'channel', communityId: result.communityId, channelId: null }, true);
    } else if (result.joinError !== null) {
      void navigate({ name: 'welcome', invite: result.invite, signin: null }, true);
      // Pre-flight ruling (e): the refused join rides on this history entry for the join dialog.
      replaceHistoryState(joinErrorState(result.joinError), '');
    } else {
      void navigate({ name: 'root' }, true);
    }
  };
  const finishSignIn = () => {
    setSigningIn(false);
    if (!signInForCallback.current && route.name !== 'spotifyCallback') void navigate({ name: 'root' }, true);
    signInForCallback.current = false;
  };
  const failed = props.fatal ?? startError;
  if (failed) return <Boot fatal={failed} />;
  // The paused view keeps its place after the screens, so it stays mounted (and focused) while the shell goes.
  // A paused tab can take dilla back: its hand-over request shows the waiting or failed phase in the same place.
  const pausedPhase = paused === 'resigning' || (tabPhase !== 'waiting' && tabPhase !== 'failed' && tabPhase !== 'confirm') ? 'paused' : tabPhase;
  const keepCall = () => setTabPhase('other-tab');
  const pausedView = paused !== 'no' || phase === 'paused-error'
    ? <OtherTab phase={pausedPhase} overlay={paused === 'resigning' && phase === 'ready'} onUse={() => takeThisTab()}
      onConfirm={() => takeThisTab(true)} onKeep={keepCall} /> : null;
  let body: ReactNode;
  if (pausedView !== null) body = paused === 'resigning' && phase === 'ready' ? screenBody() : null;
  else if (phase === 'other-tab') body = <OtherTab phase={tabPhase === 'paused' ? 'other-tab' : tabPhase} onUse={() => takeThisTab()}
    onRetry={() => takeThisTab()} onConfirm={() => takeThisTab(true)} onKeep={keepCall} />;
  else body = screenBody();
  return <>{preLoginUpdate && account?.phase !== 'ready' ? <Banner tone="info"
    action={{ label: t('update.reloadNow'), onAction: () => props.update?.reload() }}>{t('update.beforeLogin')}</Banner> : null}{body}{pausedView}{route.name === 'spotifyCallback' ? <SpotifyCallback search={spotifyCallbackSearch.current} /> : null}</>;

  function screenBody(): ReactNode {
    if (screen === 'signin' || (signingIn && phase === 'ready')) return <SignIn onFinish={finishSignIn} />;
    if (screen === 'onboarding' || (onboarding && phase === 'ready')) return <Onboarding onFinish={finish} />;
    // The two lines above narrow `screen` to 'boot' | 'shell' | 'factor' ('cleared' is 'boot'), so this switch is total.
    switch (screen) {
      case 'factor': return <ForcedFactor />;
      case 'boot': return <Boot fatal={null} />;
      // Settings opens over the shell, which stays mounted and shows the route Settings was opened from (L-TS-27).
      case 'shell': return route.name === 'spotifyCallback' ? null
        : <Shell update={props.update} />;
    }
  }
}
