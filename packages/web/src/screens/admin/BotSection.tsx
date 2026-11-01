// Server Settings → Bot: inspect the managed installation and enable/disable it through the core worker.
import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react';
import { Button, Dialog, Segmented, SettingsRow, TextField, Toggle, type IconName } from '@dilla/ui';
import { BOT_MUSIC_IDLE_MINUTES, BOT_MUSIC_VOLUMES, PERM } from '@dilla/client-core';
import { useCore } from '../../core/context.tsx';
import { errorOf, type UiError } from '../../core/errors.ts';
import { useSlice } from '../../core/use-slice.ts';
import { t, type StringKey } from '../../strings/index.ts';
import { StepUpDialog } from '../settings/StepUpDialog.tsx';
import { useSettingsEdit, useSettingsWorkspace, type SettingsEditState } from '../../settings/edit-state.tsx';
import { ModerationLog } from './ModerationLog.tsx';
import { AutoModerationSettingsPanel } from './AutoModerationSettingsPanel.tsx';
import { WelcomeSettingsPanel, type WelcomeSettingsGateway } from './WelcomeSettingsPanel.tsx';
import { type SectionProps } from './shared.ts';
import { authorizeSpotify, spotifySession } from './spotify-link.ts';
import { MusicProviderStatus } from './MusicProviderStatus.tsx';
import { useRoute } from '../../router.ts';
import { useBotDraftState, useBotMutation } from './bot-settings-context.tsx';
import { openRoleOnNextVisit } from './RolesSection.tsx';
import { communityBotMember } from '../bot-member.ts';

export type BotPageKind = 'bot' | 'welcome' | 'joinRoles' | 'music' | 'automoderation' | 'moderationLog';

type Installation = { id: string; communityId: string; state: 0 | 1 | 2 | 3 | 4 | 5; revision: string;
  userId: string | null; deviceId: string | null; capabilities: number; updatedAt: string;
  // Task 10c: present on the manager's read (and the music settings answer), absent on a mutation's answer.
  restartRequested?: string; restartHandled?: string; version?: string | null; musicVolume?: number; musicIdleMinutes?: number };
type Phase = 'idle' | 'stepup';
type StepUpPurpose = 'bot' | 'spotify';
type JoinRoleSetting = { roleId: string | null; installationId: string };
const SPOTIFY_ATTEMPT_KEY = 'dilla.spotifyAttempt';
const BOT_STATUS_POLL_INTERVAL_MS = 2_000;
const BOT_STATUS_POLL_LIMIT = 30;

// Polls and Reminders (bits 8 and 9) start on, as the prototype shows them: a first enable (no
// installation yet) offers both switched on (Task 10c fix round 1, I1). A disabled installation keeps
// the mask it was approved with: migration 00050 already switched both on for every installation
// that existed, so a bit that is off there was switched off by a manager.
const POLLS_AND_REMINDERS = (1 << 8) | (1 << 9);
function startingCapabilities(installation: Installation | null | undefined): number {
  return installation === null || installation === undefined ? POLLS_AND_REMINDERS : installation.capabilities;
}

// Task 10c (Jonas, Q5 a): the prototype's Music, Polls and Reminders first, then dilla's other capabilities — ten rows.
const CAPABILITY_OPTIONS = [
  { name: 'music', bit: 1 << 0, permission: null, requirement: 'music', icon: 'music' },
  { name: 'polls', bit: 1 << 8, permission: null, requirement: null, icon: 'vote' },
  { name: 'reminders', bit: 1 << 9, permission: null, requirement: null, icon: 'alarm-clock' },
  { name: 'welcome', bit: 1 << 1, permission: null, requirement: null, icon: 'hand' },
  { name: 'roles', bit: 1 << 2, permission: 'manageRoles', requirement: 'roles', icon: 'user-plus' },
  { name: 'announcements', bit: 1 << 3, permission: null, requirement: null, icon: 'megaphone' },
  { name: 'warnings', bit: 1 << 4, permission: 'moderateMembers', requirement: 'warnings', icon: 'triangle-alert' },
  { name: 'removal', bit: 1 << 5, permission: null, requirement: 'removal', icon: 'message-square-x' },
  { name: 'kick', bit: 1 << 6, permission: 'kickMembers', requirement: 'kick', icon: 'user-x' },
  { name: 'ban', bit: 1 << 7, permission: 'banMembers', requirement: 'ban', icon: 'ban' },
] as const satisfies readonly { name: string; bit: number; permission: string | null; requirement: string | null; icon: IconName }[];
const RESTART_POLL_INTERVAL_MS = 2_000;
const RESTART_POLL_LIMIT = 45;

const requestId = (): string => [...crypto.getRandomValues(new Uint8Array(16))].map(b => b.toString(16).padStart(2, '0')).join('');
const stateKey = (state: Installation['state']): StringKey => state <= 1 ? 'admin.bot.status.pending'
  : state === 2 ? 'admin.bot.status.joining' : state === 3 ? 'admin.bot.status.ready'
    : state === 4 ? 'admin.bot.status.disabled' : 'admin.bot.status.failed';

function errorMessage(error: UiError): string {
  if (error.code === 'E_FORBIDDEN' || error.status === 403) return t('admin.bot.error.forbidden');
  if (error.status === 409 || error.code === 'E_CONFLICT') return t('admin.bot.error.conflict');
  return t('admin.bot.error.other', { code: error.code });
}

export function BotSection({ communityId, announce, page = 'bot' }: SectionProps & { page?: BotPageKind }): React.JSX.Element {
  const client = useCore();
  const base = useId();
  const factors = useSlice('factors');
  const workspace = useSettingsWorkspace();
  const [, navigate] = useRoute();
  const beginMutation = useBotMutation();
  const members = useSlice(`members:${communityId}`);
  const channelsState = useSlice(`channels:${communityId}`);
  const communities = useSlice('communities');
  const roles = useSlice(`roles:${communityId}`);
  const access = useSlice(`permissions:${communityId}`);
  const [installation, setInstallation] = useBotDraftState<Installation | null>(communityId, 'bot.installation', null);
  const [statusRefreshing, setStatusRefreshing] = useState(false);
  const [statusRefreshError, setStatusRefreshError] = useState<string | null>(null);
  const [statusPollGeneration, setStatusPollGeneration] = useState(0);
  const [joinRole, setJoinRole] = useBotDraftState<JoinRoleSetting | null>(communityId, 'bot.joinRole', null);
  const [joinRoleValue, setJoinRoleValue] = useBotDraftState(communityId, 'bot.joinRoleValue', '');
  const [joinRoleLoadedFor, setJoinRoleLoadedFor] = useBotDraftState<string | null>(communityId, 'bot.joinRoleLoadedFor', null);
  const [joinRoleError, setJoinRoleError] = useBotDraftState<string | null>(communityId, 'bot.joinRoleError', null);
  const [spotifyClientId, setSpotifyClientId] = useBotDraftState(communityId, 'bot.spotifyClientId', '');
  const [spotifyRedirectUrl, setSpotifyRedirectUrl] = useBotDraftState(communityId, 'bot.spotifyRedirectUrl', '');
  const [spotifyConfigError, setSpotifyConfigError] = useBotDraftState<string | null>(communityId, 'bot.spotifyConfigError', null);
  const [botNameDraft, setBotNameDraft] = useBotDraftState(communityId, 'bot.nameDraft', '');
  const [botNameOverride, setBotNameOverride] = useBotDraftState<{ userId: string; display: string } | null>(communityId, 'bot.nameOverride', null);
  const [botNameError, setBotNameError] = useBotDraftState<string | null>(communityId, 'bot.nameError', null);
  const [loadedFor, setLoadedFor] = useBotDraftState<string | null>(communityId, 'bot.loadedFor', null);
  const [error, setError] = useBotDraftState<string | null>(communityId, 'bot.error', null);
  const [phase, setPhase] = useState<Phase>('idle');
  const [stepUpPurpose, setStepUpPurpose] = useState<StepUpPurpose>('bot');
  const capabilitySaveResolver = useRef<((saved: boolean) => void) | null>(null);
  const [busyFor, setBusyFor] = useBotDraftState<string | null>(communityId, 'bot.busyFor', null);
  const [selectedCapabilities, setSelectedCapabilities] = useBotDraftState(communityId, 'bot.capabilities', POLLS_AND_REMINDERS);
  const [editingCapabilities, setEditingCapabilities] = useBotDraftState(communityId, 'bot.editingCapabilities', false);
  const editingCapabilitiesRef = useRef(false);
  const readGeneration = useRef(0);
  const statusPollAttempts = useRef({ communityId, attempts: 0 });
  const selectedCommunity = useRef(communityId);
  selectedCommunity.current = communityId;
  editingCapabilitiesRef.current = editingCapabilities;
  const serverName = communities?.find(community => community.id === communityId)?.name ?? '';
  const textChannels = useMemo(() => (channelsState ?? []).filter(channel => channel.kind === 0)
    .map(channel => ({ id: channel.id, name: channel.name })), [channelsState]);
  const welcomeGateway = useMemo<WelcomeSettingsGateway>(() => ({
    request: async input => {
      const result = await client.call({ m: 'humanInteractionRequest', ...input });
      return result as Awaited<ReturnType<WelcomeSettingsGateway['request']>>;
    },
    read: async (id, operations, requestIds) => {
      const page = await client.call({ m: 'readBotInteractionResponses', communityId: id, operations, requestIds });
      return page as Awaited<ReturnType<WelcomeSettingsGateway['read']>>;
    },
    ack: async (id, through) => { await client.call({ m: 'ackBotInteractionResponses', communityId: id, through }); return null; },
  }), [client]);

  useEffect(() => {
    const generation = ++readGeneration.current;
    let canceled = false;
    statusPollAttempts.current = { communityId, attempts: 0 };
    setStatusRefreshing(false);
    setStatusRefreshError(null);
    setPhase('idle');
    void client.call({ m: 'getBotInstallation', communityId }).then(value => {
      if (canceled || generation !== readGeneration.current) return;
      const loadedInstallation = value as Installation | null;
      if (loadedInstallation !== null && loadedInstallation.communityId !== communityId) return;
      setInstallation(loadedInstallation);
      if (!editingCapabilitiesRef.current) setSelectedCapabilities(startingCapabilities(loadedInstallation));
      setLoadedFor(communityId);
    }).catch((e: unknown) => {
      if (canceled || generation !== readGeneration.current) return;
      setError(errorMessage(errorOf(e)));
      setLoadedFor(communityId);
    });
    void client.call({ m: 'refreshFactors' }).catch(() => undefined);
    return () => {
      canceled = true;
    };
  }, [client, communityId, setError, setInstallation, setLoadedFor, setSelectedCapabilities]);

  useEffect(() => {
    if (page !== 'joinRoles' || access?.loaded !== true || !access.manageCommunity || !access.manageRoles) return;
    let canceled = false;
    const generation = readGeneration.current;
    setJoinRoleLoadedFor(null); setJoinRoleError(null);
    void client.call({ m: 'getBotJoinRole', communityId }).then(value => {
      if (canceled || generation !== readGeneration.current) return;
      const setting = value as JoinRoleSetting | null;
      setJoinRole(setting); setJoinRoleValue(setting?.roleId ?? ''); setJoinRoleLoadedFor(communityId);
    }).catch((e: unknown) => {
      if (!canceled && generation === readGeneration.current) { setJoinRoleError(errorMessage(errorOf(e))); setJoinRoleLoadedFor(communityId); }
    });
    return () => { canceled = true; };
  }, [access?.loaded, access?.manageCommunity, access?.manageRoles, client, communityId, page, setJoinRole, setJoinRoleError, setJoinRoleLoadedFor, setJoinRoleValue]);


  useEffect(() => {
    if (page !== 'music' || access?.loaded !== true || !access.manageCommunity) return;
    let canceled = false;
    const generation = readGeneration.current;
    void client.call({ m: 'getBotSpotifyConfig', communityId }).then(value => {
      if (canceled || generation !== readGeneration.current || value === null) return;
      const setting = value as { clientId: string; redirectUrl: string };
      setSpotifyClientId(setting.clientId); setSpotifyRedirectUrl(setting.redirectUrl);
    }).catch(() => { if (!canceled && generation === readGeneration.current) setSpotifyConfigError(t('admin.bot.music.spotifyConfigFailed')); });
    return () => { canceled = true; };
  }, [access?.loaded, access?.manageCommunity, client, communityId, page, setSpotifyClientId, setSpotifyConfigError, setSpotifyRedirectUrl]);


  const installationCommunityId = installation?.communityId;
  const installationState = installation?.state;
  useEffect(() => {
    if (loadedFor !== communityId || installationCommunityId !== communityId || installationState === undefined
      || installationState > 2) return;
    const generation = readGeneration.current;
    let canceled = false;
    let statusTimer: number | undefined;
    const scheduleStatusRefresh = (): void => {
      if (canceled || statusPollAttempts.current.communityId !== communityId
        || statusPollAttempts.current.attempts >= BOT_STATUS_POLL_LIMIT) return;
      statusTimer = window.setTimeout(() => {
        statusTimer = undefined;
        if (canceled || generation !== readGeneration.current) return;
        statusPollAttempts.current.attempts += 1;
        void client.call({ m: 'getBotInstallation', communityId }).then(value => {
          if (canceled || generation !== readGeneration.current) return;
          const next = value as Installation | null;
          if (next !== null && next.communityId !== communityId) return;
          if (editingCapabilitiesRef.current) {
            // Keep the edit's revision and mask together. If the server changed
            // capabilities meanwhile, saving with this older revision must conflict.
            setInstallation(previous => previous === null || next === null ? next : { ...previous, state: next.state });
          } else {
            setInstallation(next);
            setSelectedCapabilities(startingCapabilities(next));
          }
          // A state transition restarts this effect; unchanged transient states
          // continue the same bounded loop without creating a second poller.
          if (next !== null && next.state <= 2 && next.state === installationState) scheduleStatusRefresh();
        }).catch(() => {
          if (!canceled && generation === readGeneration.current) scheduleStatusRefresh();
        });
      }, BOT_STATUS_POLL_INTERVAL_MS);
    };
    scheduleStatusRefresh();
    return () => {
      canceled = true;
      if (statusTimer !== undefined) window.clearTimeout(statusTimer);
    };
  }, [client, communityId, installationCommunityId, installationState, loadedFor, statusPollGeneration, setInstallation, setSelectedCapabilities]);

  const current = loadedFor === communityId;
  const currentInstallation = current && installation?.communityId === communityId ? installation : null;
  const currentError = current ? error : null;
  const busy = busyFor === communityId;
  const refreshStatus = () => {
    if (!current || statusRefreshing) return;
    const generation = readGeneration.current;
    setStatusRefreshing(true);
    setStatusRefreshError(null);
    void client.call({ m: 'getBotInstallation', communityId }).then(value => {
      if (generation !== readGeneration.current) return;
      const refreshed = value as Installation | null;
      if (refreshed !== null && refreshed.communityId !== communityId) return;
      if (editingCapabilitiesRef.current && refreshed?.state !== 3) {
        editingCapabilitiesRef.current = false;
        setEditingCapabilities(false);
        setInstallation(refreshed);
        setSelectedCapabilities(startingCapabilities(refreshed));
      } else if (editingCapabilitiesRef.current) {
        setInstallation(previous => previous === null || refreshed === null ? refreshed : { ...previous, state: refreshed.state });
      } else {
        setInstallation(refreshed);
        setSelectedCapabilities(startingCapabilities(refreshed));
      }
      statusPollAttempts.current = { communityId, attempts: 0 };
      setStatusPollGeneration(value => value + 1);
    }).catch((e: unknown) => {
      if (generation === readGeneration.current) setStatusRefreshError(errorMessage(errorOf(e)));
    }).finally(() => {
      if (generation === readGeneration.current) setStatusRefreshing(false);
    });
  };
  const enable = () => {
    if (busy || !current || currentError !== null || factors === undefined || access?.loaded !== true || !access.manageCommunity) return;
    setStepUpPurpose('bot');
    setPhase('stepup');
  };
  const configure = async (stepUp: string): Promise<boolean> => {
    if (busy || selectedCommunity.current !== communityId || !current || currentError !== null || factors === undefined
      || access?.loaded !== true || !access.manageCommunity) return false;
    const requestCommunity = communityId;
    const finishMutation = beginMutation?.();
    setBusyFor(requestCommunity);
    setError(null);
    try {
      const value = await client.call({ m: 'configureBotInstallation', communityId, requestId: requestId(),
        expectedRevision: currentInstallation?.revision ?? null, capabilities: selectedCapabilities, stepUp });
      if (selectedCommunity.current !== requestCommunity) return false;
      const updatedInstallation = value as Installation;
      setInstallation(updatedInstallation);
      setSelectedCapabilities(updatedInstallation.capabilities);
      setEditingCapabilities(false);
      editingCapabilitiesRef.current = false;
      announce(currentInstallation === null || currentInstallation.state === 4
        ? t('admin.bot.enabledNotice') : t('admin.bot.capabilities.updated'));
      return true;
    } catch (e) {
      if (selectedCommunity.current === requestCommunity) setError(errorMessage(errorOf(e)));
      return false;
    } finally {
      setBusyFor(value => value === requestCommunity ? null : value);
      finishMutation?.();
    }
  };
  const disable = () => {
    if (busy || !current || currentError !== null || currentInstallation === null) return;
    const requestCommunity = communityId;
    const generation = readGeneration.current;
    const finishMutation = beginMutation?.();
    setBusyFor(requestCommunity);
    setError(null);
    void client.call({ m: 'disableBotInstallation', communityId: requestCommunity, requestId: requestId(), expectedRevision: currentInstallation.revision }).then(value => {
      if (generation !== readGeneration.current) return;
      setInstallation(value as Installation);
      announce(t('admin.bot.disabledNotice'));
    }).catch((e: unknown) => { if (generation === readGeneration.current) setError(errorMessage(errorOf(e))); })
      .finally(() => { setBusyFor(value => value === requestCommunity ? null : value); finishMutation?.(); });
  };
  const saveJoinRole = useCallback(async (): Promise<boolean> => {
    if (busy || selectedCommunity.current !== communityId || !current || currentError !== null || !access?.manageRoles
      || joinRoleValue === (joinRole?.roleId ?? '')
      || joinRole === null || currentInstallation?.id !== joinRole.installationId) return false;
    const requestCommunity = communityId;
    const zeroId = '0'.repeat(32);
    const roleId = joinRoleValue || zeroId;
    const finishMutation = beginMutation?.();
    setBusyFor(requestCommunity);
    setJoinRoleError(null);
    try {
      const value = await client.call({ m: 'configureBotJoinRole', communityId: requestCommunity, requestId: requestId(), roleId,
        installationId: joinRole.installationId });
      if (selectedCommunity.current !== requestCommunity) return false;
      const ack = value as { installationId?: unknown };
      if (ack?.installationId !== joinRole.installationId) throw new Error('bot installation changed');
      setJoinRole(value => value === null ? value : { ...value, roleId: roleId === zeroId ? null : roleId });
      announce(t('admin.bot.joinRole.saved'));
      return true;
    } catch (e) { if (selectedCommunity.current === requestCommunity) setJoinRoleError(errorMessage(errorOf(e))); return false; }
    finally { setBusyFor(value => value === requestCommunity ? null : value); finishMutation?.(); }
  }, [busy, selectedCommunity, communityId, current, currentError, access?.manageRoles, joinRoleValue, joinRole, currentInstallation?.id, beginMutation,
    setBusyFor, setJoinRoleError, client, announce, setJoinRole]);
  const linkSpotify = () => {
    if (busy || !current || currentError !== null || factors === undefined) return;
    if (!/^[A-Za-z0-9]{1,256}$/u.test(spotifyClientId)) {
      setSpotifyConfigError(t('admin.bot.music.spotifyClientIdInvalid'));
      return;
    }
    setSpotifyConfigError(null);
    setStepUpPurpose('spotify');
    setPhase('stepup');
  };
  const beginSpotify = (stepUp: string) => {
    if (busy || selectedCommunity.current !== communityId || !current || currentError !== null) return;
    const requestCommunity = communityId;
    const generation = readGeneration.current;
    const finishMutation = beginMutation?.();
    setBusyFor(requestCommunity);
    setError(null);
    let attemptId: string | null = null;
    void client.call({ m: 'beginBotSpotifyAttempt', communityId: requestCommunity, stepUp, clientId: spotifyClientId }).then((value) => {
      if (generation !== readGeneration.current || selectedCommunity.current !== requestCommunity) return;
      const attempt = value as { attemptId?: unknown; authorizeUrl?: unknown; expires?: unknown };
      if (typeof attempt.attemptId !== 'string' || !/^[0-9a-f]{32}$/.test(attempt.attemptId)
        || typeof attempt.authorizeUrl !== 'string' || typeof attempt.expires !== 'string' || !/^\d+$/.test(attempt.expires)) {
        throw new Error('invalid Spotify attempt');
      }
      attemptId = attempt.attemptId;
      const authorize = new URL(attempt.authorizeUrl);
      const states = authorize.searchParams.getAll('state');
      const expires = Number(attempt.expires);
      if (authorize.protocol !== 'https:' || authorize.hostname !== 'accounts.spotify.com' || authorize.port !== ''
        || authorize.pathname !== '/authorize' || states.length !== 1 || states[0].length < 1 || states[0].length > 256
        || !Number.isSafeInteger(expires) || expires <= Math.floor(Date.now() / 1000)) throw new Error('invalid Spotify authorization URL');
      const account = client.get('account');
      if (account?.phase !== 'ready' || !account.instance?.id || !account.user?.id || !account.deviceId) throw new Error('no active account');
      // This tab stores only public identifiers, the CSRF state and expiry. Authorization codes and all credentials
      // remain transient in the callback request and worker bridge.
      spotifySession.setItem(SPOTIFY_ATTEMPT_KEY, JSON.stringify({ communityId: requestCommunity,
        attemptId: attempt.attemptId, state: states[0], expires: attempt.expires,
        instanceId: account.instance.id, userId: account.user.id, deviceId: account.deviceId }));
      authorizeSpotify(attempt.authorizeUrl);
    }).catch(() => {
      try { spotifySession.removeItem(SPOTIFY_ATTEMPT_KEY); } catch { /* storage may be unavailable */ }
      if (attemptId !== null) void client.call({ m: 'cancelBotSpotifyAttempt', communityId: requestCommunity, attemptId }).catch(() => undefined);
      if (generation === readGeneration.current) setError(t('admin.bot.music.callbackFailed'));
    }).finally(() => { setBusyFor(value => value === requestCommunity ? null : value); finishMutation?.(); });
  };

  const bot = communityBotMember(members, currentInstallation?.userId);
  const botDisplay = bot && botNameOverride?.userId === bot.userId ? botNameOverride.display : bot?.display || bot?.username || '';
  useEffect(() => {
    if (bot && botNameOverride?.userId !== bot.userId && botNameDraft === '') setBotNameDraft(bot.display || bot.username || '');
  }, [bot, botNameDraft, botNameOverride?.userId, setBotNameDraft]);
  const saveBotName = useCallback(async (): Promise<boolean> => {
    if (busy || !current || currentError !== null || bot === undefined || access?.manageCommunity !== true) return false;
    const display = botNameDraft.trim();
    if (display === '') { setBotNameError(t('admin.bot.name.invalid')); return false; }
    const requestCommunity = communityId;
    const finishMutation = beginMutation?.();
    setBusyFor(requestCommunity);
    setBotNameError(null);
    try {
      const value = await client.call({ m: 'patchBotProfile', communityId: requestCommunity, display });
      if (selectedCommunity.current !== requestCommunity) return false;
      const saved = value as string;
      setBotNameDraft(saved);
      setBotNameOverride({ userId: bot.userId, display: saved });
      announce(t('admin.bot.name.saved'));
      return true;
    } catch (e) {
      if (selectedCommunity.current === requestCommunity) setBotNameError(errorMessage(errorOf(e)));
      return false;
    } finally { setBusyFor(value => value === requestCommunity ? null : value); finishMutation?.(); }
  }, [busy, current, currentError, bot, access?.manageCommunity, botNameDraft, communityId, beginMutation, setBusyFor, setBotNameError,
    client, selectedCommunity, setBotNameDraft, setBotNameOverride, announce]);
  const enabled = currentInstallation !== null && currentInstallation.state !== 4;
  const welcomeEnabled = currentInstallation?.state === 3 && (currentInstallation.capabilities & (1 << 1)) !== 0;
  const canEditInstallationCapabilities = currentInstallation !== null && currentInstallation.state <= 3;
  const enableUnavailable = !current || currentError !== null || factors === undefined
    || access?.loaded !== true || !access.manageCommunity;
  const status = currentInstallation === null ? null : t(stateKey(currentInstallation.state));
  const joinRoleCandidates = (roles ?? []).filter(role => role.position > 0
    && (access?.owner === true || role.position < (access?.top ?? 0)));
  const joinRoleReady = current && joinRole !== null && currentInstallation?.id === joinRole.installationId;
  const canSelectCapability = (option: typeof CAPABILITY_OPTIONS[number]): boolean => access?.loaded === true && access.manageCommunity
    && (option.permission === null || access[option.permission] === true);
  const capabilityDirty = editingCapabilities && currentInstallation !== null && currentInstallation.state <= 3
    && selectedCapabilities !== currentInstallation.capabilities;
  const joinRoleDirty = page === 'joinRoles' && joinRole !== null && joinRoleValue !== (joinRole.roleId ?? '');
  // The bot's name is renamed in its own dialog (Glenn's row menu), so the page form holds only the capability switches.
  const formDirty = page === 'bot' && capabilityDirty || joinRoleDirty;
  const saveBotForm = useCallback(async (): Promise<boolean> => {
    if (page === 'joinRoles') return saveJoinRole();
    if (!capabilityDirty) return true;
    return new Promise(resolve => {
      capabilitySaveResolver.current = resolve;
      setStepUpPurpose('bot'); setPhase('stepup');
    });
  }, [page, saveJoinRole, capabilityDirty]);
  const canSavePage = page === 'bot' ? access?.loaded === true && access.manageCommunity && factors !== undefined
    : page === 'joinRoles' ? access?.loaded === true && access.manageCommunity && access.manageRoles && joinRole !== null : false;
  const botEditState = useMemo<SettingsEditState>(() => ({ dirty: formDirty, busy, canSave: formDirty && !busy && canSavePage, save: saveBotForm, reset: () => {
      if (page === 'joinRoles' && joinRole !== null) setJoinRoleValue(joinRole.roleId ?? '');
      else {
        setSelectedCapabilities(startingCapabilities(currentInstallation));
        setEditingCapabilities(false);
        editingCapabilitiesRef.current = false;
      }
    },
  }), [formDirty, busy, canSavePage, saveBotForm, page, joinRole, setJoinRoleValue,
    setSelectedCapabilities, setEditingCapabilities, currentInstallation]);
  const managedBotForm = useSettingsEdit(botEditState);
  const botName = botDisplay || t('serverSettings.bot.fallbackName');
  const manager = access?.loaded === true && access.manageCommunity;

  // Task 10c: Restart. The instance counts the request (a second one while it is pending joins it); the runtime picks it
  // up on its next check-in and reports back. The button says "Restarting…" until the reported count catches up.
  const [restartWanted, setRestartWanted] = useState<bigint | null>(null);
  const [restartError, setRestartError] = useState<string | null>(null);
  // Fix round 1 (M1): when the runtime has not reported the restart by the end of the polling window, the button comes
  // back with a note; the request stays counted on the instance, and pressing Restart again joins it.
  const [restartNote, setRestartNote] = useState<string | null>(null);
  const restartHandled = BigInt(currentInstallation?.restartHandled ?? '0');
  const restarting = restartWanted !== null && restartHandled < restartWanted;
  const restart = () => {
    if (restarting || currentInstallation === null || !manager) return;
    setRestartError(null);
    setRestartNote(null);
    const generation = readGeneration.current;
    void client.call({ m: 'restartBot', communityId, installationId: currentInstallation.id }).then(value => {
      if (generation !== readGeneration.current) return;
      const counts = value as { requested: string; handled: string };
      setRestartWanted(BigInt(counts.requested));
    }).catch((e: unknown) => { if (generation === readGeneration.current) setRestartError(errorMessage(errorOf(e))); });
  };
  useEffect(() => {
    if (restartWanted === null) return;
    if (restartHandled >= restartWanted) {
      announce(t('serverSettings.bot.restarted', { bot: botName }));
      setRestartWanted(null);
      return;
    }
    let polls = 0;
    const timer = window.setInterval(() => {
      if (++polls > RESTART_POLL_LIMIT) {
        window.clearInterval(timer);
        setRestartWanted(null);
        setRestartNote(t('serverSettings.bot.restartPending', { bot: botName }));
        return;
      }
      void client.call({ m: 'getBotInstallation', communityId }).then(value => {
        const next = value as Installation | null;
        if (next !== null && next.communityId === communityId) setInstallation(previous => previous === null ? next : { ...previous, ...next });
      }).catch(() => undefined);
    }, RESTART_POLL_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [restartWanted, restartHandled, client, communityId, setInstallation, announce, botName]);

  // Glenn's row menu (Ruling X): rename, refresh status and disable, which the prototype's card draws no button for.
  const [renaming, setRenaming] = useState(false);
  const glennMenuItems = [
    ...(bot !== undefined && manager ? [{ id: 'rename', label: t('serverSettings.bot.menu.rename') }] : []),
    ...(currentInstallation !== null ? [{ id: 'refresh', label: t('serverSettings.bot.menu.refresh') }] : []),
    ...(enabled && manager ? [{ id: 'disable', label: t('serverSettings.bot.menu.disable', { bot: botName }), danger: true, sep: true }] : []),
  ];
  const presence = currentInstallation === null ? t('admin.bot.status.unavailable')
    : currentInstallation.state === 3 ? t('serverSettings.bot.online')
      : currentInstallation.state === 4 ? t('serverSettings.bot.offline') : (status ?? '').toLowerCase();
  const version = currentInstallation?.version ?? null;
  const cardHint = currentInstallation === null ? t('admin.bot.none')
    : version !== null ? t('serverSettings.bot.cardVersion', { presence, version }) : t('serverSettings.bot.card', { presence });

  // Task 10c: the Music page's defaults save as they are picked (manage_community; the Bot enforces the same options).
  const [musicError, setMusicError] = useState<string | null>(null);
  const musicVolume = currentInstallation?.musicVolume ?? 100;
  const musicIdle = currentInstallation?.musicIdleMinutes ?? 30;
  const saveMusic = (volume: number, idleMinutes: number) => {
    if (!manager || currentInstallation === null || busy) return;
    setMusicError(null);
    const generation = readGeneration.current;
    setInstallation(previous => previous === null ? previous : { ...previous, musicVolume: volume, musicIdleMinutes: idleMinutes });
    void client.call({ m: 'setBotMusicSettings', communityId, installationId: currentInstallation.id, volume, idleMinutes }).then(value => {
      if (generation !== readGeneration.current) return;
      const next = value as Installation;
      setInstallation(previous => previous === null ? next : { ...previous, ...next });
      announce(t('serverSettings.music.saved'));
    }).catch((e: unknown) => {
      if (generation !== readGeneration.current) return;
      setInstallation(previous => previous === null ? previous : { ...previous, musicVolume, musicIdleMinutes: musicIdle });
      setMusicError(errorMessage(errorOf(e)));
    });
  };
  const djRoles = (roles ?? []).filter(role => (role.allow & PERM.controlMusic) !== 0);
  const djNames = djRoles.map(role => role.id === access?.everyoneId ? t('roles.everyone') : role.name).join(', ');
  const editDjRoles = () => {
    const first = djRoles.find(role => role.id !== access?.everyoneId) ?? djRoles[0];
    if (first !== undefined) openRoleOnNextVisit(communityId, first.id);
    void navigate({ name: 'serverSettings', communityId, section: 'roles', from: null });
  };

  const pageTitle: StringKey = page === 'bot' ? 'admin.bot.title' : page === 'welcome' ? 'admin.bot.welcomeSettings.title'
    : page === 'joinRoles' ? 'admin.bot.joinRole.title' : page === 'music' ? 'serverSettings.music.title'
      : page === 'automoderation' ? 'admin.bot.automod.title' : 'admin.bot.moderationLog.title';

  return (
    <section className="dw-settings-section dw-bot-settings" data-measure="full" aria-labelledby={`${base}-title`}>
      <h2 id={`${base}-title`} tabIndex={-1}>{t(pageTitle)}</h2>
      {page === 'bot' ? <p>{t('serverSettings.bot.body', { bot: botName, server: serverName })}</p> : null}
      {page === 'music' ? <p>{t('serverSettings.music.body', { bot: botName })}</p> : null}
      {!current ? <p aria-busy="true">{t('admin.bot.loading')}</p> : null}
      {page !== 'bot' && page !== 'moderationLog' && current && currentInstallation?.state !== 3 ? <Button onClick={() => void navigate({ name: 'serverSettings', communityId, section: 'bot', from: null }, true)}>{t('workspace.botSetup')}</Button> : null}
      {page === 'bot' && current ? <>
        <SettingsRow id={`${base}-glenn`} icon="bot" label={botName} tag={t('serverSettings.bot.tag')} hint={cardHint}
          {...(glennMenuItems.length > 0 ? { menu: { label: t('serverSettings.bot.menu', { bot: botName }), items: glennMenuItems,
            onSelect: (item: string) => { if (item === 'rename') setRenaming(true); else if (item === 'refresh') refreshStatus(); else if (item === 'disable') disable(); } } } : {})}>
          {enabled ? <Button size="sm" aria-disabled={restarting || !manager || undefined} onClick={restart}>
            {restarting ? t('serverSettings.bot.restarting') : t('serverSettings.bot.restart')}
          </Button> : <Button size="sm" variant="accent" aria-disabled={busy || enableUnavailable || undefined}
            aria-describedby={factors === undefined ? `${base}-stepup-reason` : undefined} onClick={enable}>
            {busy ? t('admin.bot.enabling') : t('serverSettings.bot.enable')}
          </Button>}
        </SettingsRow>
        <p className="d-sr-only" role="status">{status}</p>
        {CAPABILITY_OPTIONS.map(option => {
          const checked = (selectedCapabilities & option.bit) !== 0;
          const requirement = option.requirement === null ? null : t(`admin.bot.capability.requires.${option.requirement}` as StringKey);
          const lacking = option.permission !== null && access?.loaded === true && access[option.permission] !== true;
          // A lost permission must block adding a capability, but it must not trap an already-approved one in the mask.
          const disabled = busy || !manager || (!checked && !canSelectCapability(option)) || (currentInstallation !== null && currentInstallation.state > 3 && currentInstallation.state !== 4);
          return <SettingsRow key={option.name} id={`${base}-capability-${option.name}`} icon={option.icon}
            label={t(`serverSettings.bot.cap.${option.name}` as StringKey)}
            hint={lacking && requirement !== null ? requirement : t(`serverSettings.bot.cap.${option.name}.hint` as StringKey)}>
            <Toggle id={`${base}-capability-${option.name}-switch`} label={t(`serverSettings.bot.cap.${option.name}` as StringKey)} checked={checked}
              disabled={disabled} onChange={on => {
                editingCapabilitiesRef.current = true;
                setEditingCapabilities(true);
                setSelectedCapabilities(mask => on ? mask | option.bit : mask & ~option.bit);
              }} />
          </SettingsRow>;
        })}
        <SettingsRow id={`${base}-voice`} icon="lock" label={t('serverSettings.bot.voice')} hint={t('serverSettings.bot.voiceHint', { bot: botName })}>
          <Button size="sm" onClick={() => void navigate({ name: 'serverSettings', communityId, section: 'roles', from: null })}>{t('serverSettings.bot.voiceRoles')}</Button>
        </SettingsRow>
        {canEditInstallationCapabilities && capabilityDirty && !managedBotForm ? <div className="dw-settings-actions">
          <Button variant="accent" aria-disabled={busy || enableUnavailable || undefined} onClick={enable}>{t('admin.bot.capabilities.save')}</Button>
          <Button aria-disabled={busy || undefined} onClick={() => {
            setSelectedCapabilities(startingCapabilities(currentInstallation));
            setEditingCapabilities(false);
            editingCapabilitiesRef.current = false;
          }}>{t('admin.bot.capabilities.cancel')}</Button>
        </div> : null}
      </> : null}
      {page === 'joinRoles' ? <section aria-labelledby={`${base}-join-role-title`}>
        <h3 id={`${base}-join-role-title`}>{t('admin.bot.joinRole.title')}</h3>
        <p>{t('admin.bot.joinRole.honest')}</p>
        {joinRoleLoadedFor === communityId && joinRole === null && joinRoleError === null && currentError === null ? <p>{t('admin.bot.joinRole.requiresBot')}</p> : null}
        {access?.loaded === true && (!access.manageCommunity || !access.manageRoles) ? <p role="note">{t('admin.bot.joinRole.requiresBot')}</p> : null}
        {joinRoleLoadedFor === communityId && joinRoleError !== null && access?.manageCommunity && access.manageRoles ? <p role="alert">{joinRoleError}</p> : null}
        {joinRoleLoadedFor !== communityId ? <p aria-busy="true">{t('admin.bot.loading')}</p> : null}
        {joinRoleReady && access?.manageRoles ? <SettingsRow id={`${base}-join-role-row`} label={t('admin.bot.joinRole.label')}>
          <select aria-label={t('admin.bot.joinRole.label')} value={joinRoleValue} disabled={busy}
            onChange={event => setJoinRoleValue(event.currentTarget.value)}>
            <option value="">{t('admin.bot.joinRole.none')}</option>
            {joinRoleCandidates.map(role => <option key={role.id} value={role.id}>{role.name}</option>)}
          </select>
          {!managedBotForm ? <Button aria-disabled={busy || joinRoleValue === (joinRole?.roleId ?? '') || undefined} onClick={() => void saveJoinRole()}>
            {busy ? t('admin.bot.joinRole.saving') : t('admin.bot.joinRole.save')}
          </Button> : null}
        </SettingsRow> : null}
      </section> : null}
      {page === 'music' ? <>
        <SettingsRow id={`${base}-dj`} icon="disc-3" label={t('serverSettings.music.dj')}
          hint={djRoles.length > 0 ? t('serverSettings.music.djRoles', { roles: djNames }) : t('serverSettings.music.djNone')}>
          <Button size="sm" onClick={editDjRoles}>{t('serverSettings.music.djEdit')}</Button>
        </SettingsRow>
        <MusicProviderStatus communityId={communityId} installationId={currentInstallation?.id ?? null} />
        <SettingsRow id={`${base}-volume`} icon="volume-2" label={t('serverSettings.music.volume')} hint={t('serverSettings.music.volumeHint')}>
          <Segmented id={`${base}-volume-choice`} label={t('serverSettings.music.volume')} value={String(musicVolume)}
            disabled={!manager || currentInstallation === null}
            options={BOT_MUSIC_VOLUMES.map(v => ({ value: String(v), label: String(v) }))} onChange={v => saveMusic(Number(v), musicIdle)} />
        </SettingsRow>
        <SettingsRow id={`${base}-idle`} icon="timer" label={t('serverSettings.music.idle')}
          hint={t('serverSettings.music.idleHint', { bot: botName })}>
          <Segmented id={`${base}-idle-choice`} label={t('serverSettings.music.idle')} value={String(musicIdle)}
            disabled={!manager || currentInstallation === null}
            options={BOT_MUSIC_IDLE_MINUTES.map(v => ({ value: String(v), label: t('serverSettings.music.idle.option', { minutes: v }) }))}
            onChange={v => saveMusic(musicVolume, Number(v))} />
        </SettingsRow>
        {musicError !== null ? <p role="alert">{musicError}</p> : null}
        <SettingsRow id={`${base}-spotify-row`} icon="music" label={t('serverSettings.music.spotify')} hint={t('admin.bot.music.spotifyClientIdHelp')} layout="stacked">
          <TextField id={`${base}-spotify-client-id`} label={t('admin.bot.music.spotifyClientId')} value={spotifyClientId}
            maxLength={256} disabled={busy || access?.manageCommunity !== true} autoComplete="off" spellCheck={false}
            onChange={value => { setSpotifyClientId(value.trim()); setSpotifyConfigError(null); }} />
          {spotifyRedirectUrl !== '' ? <p><strong>{t('admin.bot.music.spotifyCallback')}</strong> <code>{spotifyRedirectUrl}</code></p> : null}
          {spotifyConfigError !== null ? <p role="alert">{spotifyConfigError}</p> : null}
          <Button size="sm" aria-disabled={busy || !current || currentError !== null || factors === undefined || undefined}
            onClick={linkSpotify}>{busy ? t('admin.bot.music.spotifyLinking') : t('admin.bot.music.spotifyLink')}</Button>
        </SettingsRow>
      </> : null}
      {page === 'welcome' ? <WelcomeSettingsPanel communityId={communityId} serverName={serverName} channels={textChannels}
        enabled={welcomeEnabled} canManage={access?.loaded === true && access.manageCommunity} gateway={welcomeGateway} />
        : null}
      {page === 'automoderation' ? <AutoModerationSettingsPanel communityId={communityId} botReady={currentInstallation?.state === 3}
        canManage={access?.loaded === true && access.manageCommunity}
        canManageMessages={access?.loaded === true && (access.manageMessages ?? access.owner)}
        canModerateMembers={access?.loaded === true && (access.moderateMembers ?? access.owner)}
        capabilities={currentInstallation?.capabilities ?? 0} /> : null}
      {page === 'moderationLog' ? <ModerationLog communityId={communityId} showHeading={false} /> : null}
      {page === 'bot' && currentError !== null ? <p role="alert">{currentError}</p> : null}
      {page === 'bot' && statusRefreshError !== null ? <p role="alert">{statusRefreshError}</p> : null}
      {page === 'bot' && restartError !== null ? <p role="alert">{restartError}</p> : null}
      {page === 'bot' && restartNote !== null ? <p role="status">{restartNote}</p> : null}
      {page === 'bot' && factors === undefined && !enabled ? <p id={`${base}-stepup-reason`}>{t('admin.bot.stepUpMissing')}</p> : null}
      <Dialog open={renaming} title={t('serverSettings.bot.rename.title', { bot: botName })} closeLabel={t('admin.cancel')}
        onClose={() => { setRenaming(false); setBotNameError(null); setBotNameDraft(botDisplay); }}
        footer={<Button variant="accent" aria-disabled={busy || botNameDraft.trim() === '' || undefined}
          onClick={() => void saveBotName().then(saved => { if (saved) setRenaming(false); })}>{t('admin.bot.name.save')}</Button>}>
        <TextField id={`${base}-bot-display`} label={t('admin.bot.name.label')} maxLength={64} value={botNameDraft} disabled={busy}
          onChange={value => { setBotNameDraft(value); setBotNameError(null); }} error={botNameError ?? undefined} initialFocus />
      </Dialog>
      <StepUpDialog open={phase === 'stepup'} hasPassword={factors?.hasPassword ?? true} notice={null}
        onToken={value => { setPhase('idle'); if (stepUpPurpose === 'spotify') {
          const authorize = () => beginSpotify(value);
          if (workspace) void workspace.requestNavigation(authorize); else authorize();
        } else void configure(value).then(saved => { capabilitySaveResolver.current?.(saved); capabilitySaveResolver.current = null; }); }}
        onClose={() => { setPhase('idle'); capabilitySaveResolver.current?.(false); capabilitySaveResolver.current = null; }} />
    </section>
  );
}
