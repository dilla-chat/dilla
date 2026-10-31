import { useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState, type ReactNode, type RefObject } from 'react';
import { PERM, SETTINGS } from '@dilla/client-core';
import type { ChannelGroupState, ChannelSummary, DmSummary, SendBlock, TrustBlock, TrustSummary } from '@dilla/client-core';
import {
  AddServerDialog, AppShell, ThreadPane, Banner, Button, CallBar, ChannelHeader, ChannelList, CommunityRail, Dialog, EmptyState, Icon, IconButton, LinkBanner,
  TabBar, Tooltip, TopBar, UnsupportedVoice, PhoneLayoutProvider,
} from '@dilla/ui';
import { useCore } from '../core/context.tsx';
import { errorOf, type UiError } from '../core/errors.ts';
import { useSlice, useSlices } from '../core/use-slice.ts';
import { useChannelBits } from '../core/use-channel-bits.ts';
import { useDocumentVisible } from '../core/use-visible.ts';
import { baseRoute, goBack, subscribeCommittedTraversal, pushedFrom, readJoinError, routeOf, routePath, tabOf, useRoute, type Route, type SettingsSection } from '../router.ts';
import { searchSettings } from '../settings/registry.ts';
import { phoneNow, readPaneWidths, rememberedTab, rememberTab, usePhoneLayout, writePaneWidths, usePhoneSheet, useVisualViewportHeight, type PhoneTab } from '../layout.ts';
import { en, formatTime, t, type StringKey } from '../strings/index.ts';
import { JoinCommunity } from './JoinCommunity.tsx';
import { CreateChannelFlow, CreateServerFlow } from './Create.tsx';
import { Conversation, focusComposer, focusLog, useConversationUi, type ConversationAction, type ConversationUi } from './conversation/Conversation.tsx';
import { ComposerArea, type OutgoingMessage } from './conversation/ComposerArea.tsx';
import type { ProfileOpener } from './conversation/MessageItem.tsx';
import { ThreadView } from './conversation/ThreadView.tsx';
import { ThreadList } from './conversation/ThreadList.tsx';
import { Search, type SearchHitView } from './Search.tsx';
import { actionsProvider, channelsProvider, messagesProvider, paletteGroups, peopleProvider, settingsProvider, type PaletteProvider } from './palette.ts';
import { messageTime } from './shell-model.ts';
import { ActionMenu, TrustAlert, TypingIndicator } from '@dilla/ui';
import {
  badgeLabel, composerBlock, defaultChannel, dmCandidates, isUnsupported, isVoice, mentionCandidates, mentionMembers, mergeMembers,
  nameBook, orderChannels, participantView, railLabel, rowBadge, selfName, stepChannel, sumBadges, unsupportedBody, userName,
} from './shell-model.ts';
import { useCall } from './voice/useCall.tsx';
import { callPhase, DmCall, dmCallStart, Dock, personMenu, removedFromServer, rowLabels, speakingDevices, VoiceView, type VoiceContext } from './voice/VoiceView.tsx';
import { isApplePlatform } from './voice/shortcuts.ts';
import { useMusicPlayer } from './voice/music.tsx';
import { RingFlow } from './voice/Ring.tsx';
import { useAnnouncer } from './announcer.ts';
import { MemberPane } from './admin/MemberPane.tsx';
import { MemberCardHost, type OpenCard } from './admin/MemberCardHost.tsx';
import { Settings } from './Settings.tsx';
import { ServerSettings, type AdminTab } from './admin/ServerSettings.tsx';
import { ChannelEditFlow } from './admin/ChannelEditFlow.tsx';
import { useSounds } from './voice/useSounds.tsx';
import { Activity } from './Activity.tsx';
import { requestThread, takeThreadRequest, useThreadRequest } from '../thread-request.ts';
import { You } from './You.tsx';
import { UserPanel } from './UserPanel.tsx';
import type { watchUpdates } from '../update.ts';
import type { createUpdateCheckpointStore } from '../update-checkpoint.ts';
import { updateTabId } from '../update-checkpoint.ts';
import { captureViewPlace, restoreViewPlace, validViewPlace, type ViewPlace } from '../update-view.ts';
import { browser } from '../browser.ts';
import { CommandProvider } from './commands/CommandContext.tsx';
import type { CommandProviderHandlers, CommandSuggestionProvider } from './commands/command-types.ts';
import { useBotCommandProvider } from './commands/bot-handlers.ts';
import { useUtilityCommandProvider } from './commands/UtilityCommandProvider.tsx';

const DRAFT_MEMORY_LIMIT = 128;
function rememberDraft(map: Map<string, string>, key: string, value: string): void {
  map.delete(key); map.set(key, value);
  while (map.size > DRAFT_MEMORY_LIMIT) map.delete(map.keys().next().value!);
}

export interface ShellUpdateDeps {
  currentEntry: string;
  watch: typeof watchUpdates;
  store: ReturnType<typeof createUpdateCheckpointStore>;
  reload(): void;
}

/** The icon of the channel list's new-channel button. */
/** Ruling AB: the server head's ⌄ (the prototype's 15px chevron) opens the server's settings. */
const SERVER_MENU = 'chevron-down';
/** The channel header's Threads and Members glyphs (Signal §5.5). */
const THREADS_GLYPH = 'messages-square';
const MEMBERS_GLYPH = 'users';
const CATEGORY = 2;
// The codes a send is refused with when the core's send gate holds the group (L-CORE-40, sec ruling 2).
const TRUST_BLOCKS: ReadonlySet<string> = new Set<TrustBlock>(['E_UMK_CHANGED', 'E_CREDENTIAL', 'E_DEVICE_UNLISTED', 'E_DEVICE_REVOKED', 'E_TIER_MISMATCH']);

/**
 * The composer's reason for a send block (W-01, R1). A block only own-user leaves hold names the own account and Devices;
 * a changed key asks for a comparison only when a changed member of the conversation (`changed`) has a key to compare,
 * else it says there is nothing to compare yet; any other failed leaf is privacy.leafFailed.
 */
export function blockReason(block: SendBlock, changed: readonly TrustSummary[]): StringKey {
  if (block.own) return block.code === 'E_UMK_CHANGED' ? 'privacy.ownChanged' : 'privacy.ownUnlisted';
  if (block.code !== 'E_UMK_CHANGED') return 'privacy.leafFailed';
  return changed.some(row => row.changedUmk !== null) ? 'privacy.changed' : 'privacy.changedNoKey';
}

/** A contact whose identity changed: a validated pending key, or a leaf the core holds under a key no list presents. */
export function isChanged(row: TrustSummary): boolean {
  return row.changedUmk !== null || row.blocked === 'E_UMK_CHANGED';
}
const HEX32 = /^[0-9a-f]{32}$/;
/** Flow 07: the selected server's lists are read again when the window comes back, at most this often, so a channel another
 *  member added shows without a reload even when its op 54 community_changed was missed (this is the fallback to op 54). */
export const LIST_REFRESH_MS = 15_000;
// Renew an unchanged typing signal after 5 s: above the server's 3 s meter, inside its 10 s hold (L-HTTP-96).
const TYPING_RENEW_MS = 5_000;
/** No device speaking: the voice rows of calls this device is not in (product walk M3). */
const NOBODY: ReadonlySet<string> = new Set();

/** True when focus was dropped to the document, as it is when the focused element leaves the DOM. */
/** Final review M-1: cancels a pending retry of the AFK exempt report. */
function stopRetry(timer: { current: number | null }): void {
  if (timer.current !== null) window.clearTimeout(timer.current);
  timer.current = null;
}

function focusLost(): boolean {
  return document.activeElement === null || document.activeElement === document.body;
}

/**
 * The search palette is a modal dialog, so the shell beneath it is inert and cannot take focus: a picked hit closes that
 * top layer first, then focuses its row (the palette's React state follows when the pick resolves).
 */
/** Signal task 10 fix round 2: a conversation's or a server's own notify mode in its menu; default deletes the stored value. */
const NOTIFY_MODES: readonly { value: string; key: StringKey }[] = [
  { value: 'default', key: 'signalSettings.menu.notifyDefault' }, { value: 'all', key: 'signalSettings.menu.notifyAll' },
  { value: 'mentions', key: 'signalSettings.menu.notifyMentions' }, { value: 'nothing', key: 'signalSettings.menu.notifyNothing' },
];

/** The device-wide default (it governs direct messages), in the DM list head's menu. */
const DM_DEFAULTS: readonly { value: string; key: StringKey }[] = [
  { value: 'dms-mentions', key: 'signalSettings.menu.dmsMentions' }, { value: 'everything', key: 'signalSettings.menu.everything' },
  { value: 'nothing', key: 'signalSettings.menu.notifyNothing' },
];

function leaveSearchLayer(): void {
  document.querySelector<HTMLDialogElement>('dialog.d-search-palette-dialog[open], dialog.dw-search[open]')?.close();
}

/** Focuses the action of an EmptyState in one pane of the shell; false when there is none. */
function focusEmptyAction(pane: 'content' | 'sidebar'): boolean {
  const button = document.querySelector<HTMLElement>(`.d-app-shell__${pane} .d-empty-state button`);
  button?.focus();
  return button !== null;
}

/**
 * A failed row's actions leave with its failed state (retry turns it pending, discard removes it), so a focused
 * action hands focus to the same action of the next failed row, else to the log (A11Y-DESIGN-05). Nothing moves
 * when focus is not in the row (a pointer that does not focus buttons).
 */
/** A channel row's or DM row's props: what ChannelList draws, with the badge of requirement 6. */
type ListRow = { id: string; name: string; kind: 'text' | 'voice' | 'dm' | 'announcement'; badge?: string; readable: boolean; unread: number; mentions: number; muted: boolean; parentId?: string | null;
  person?: { sub?: string; presence?: 'online' | 'idle' | 'dnd' | 'offline'; group?: boolean; initials?: string } };

/** What the aside shows instead of the member list (web3.7): a channel's thread, or the list of its threads. */
type Side = { channelId: string; view: 'list' } | { channelId: string; view: 'thread'; rootMsgId: string };

function restoredConversationUi(value: unknown): ConversationUi | null {
  if (typeof value !== 'object' || value === null) return null;
  const ui = value as Partial<ConversationUi>;
  const message = (row: unknown): row is { msgId: string } => typeof row === 'object' && row !== null
    && typeof (row as { msgId?: unknown }).msgId === 'string' && HEX32.test((row as { msgId: string }).msgId);
  if (ui.reply !== null && (!message(ui.reply) || typeof ui.reply.author !== 'string' || typeof ui.reply.excerpt !== 'string')) return null;
  if (ui.editing !== null && (!message(ui.editing) || typeof ui.editing.value !== 'string')) return null;
  if (typeof ui.pinsOpen !== 'boolean' || (ui.jump !== null && (!message(ui.jump) || typeof ui.jump.n !== 'number'))
    || (ui.notice !== null && ui.notice !== 'shell.message.replyNotLoaded')
    || (ui.trayError !== null && typeof ui.trayError !== 'string')) return null;
  return ui as ConversationUi;
}

/**
 * The signed-in shell: the server rail, the selected server's channels and the direct messages, the open
 * channel's or DM's timeline and composer and the banners. The route is the selection (L-TS-13);
 * under a settings route the shell shows the route Settings was opened over and never navigates (L-TS-27). The
 * worker loads a server's lists on selectCommunity and opens a channel's or DM's group on openChannel.
 */
export function Shell(props: { update?: ShellUpdateDeps; page?: ReactNode; commandHandlers?: CommandProviderHandlers; commandSuggest?: CommandSuggestionProvider } = {}): React.JSX.Element {
  const { commandHandlers: suppliedHandlers, commandSuggest: suppliedSuggest } = props;
  const client = useCore();
  const phone = usePhoneLayout();
  useVisualViewportHeight(phone);
  const [route, goTo] = useRoute();
  // Every push on a phone is one the app made, so back from it is the history's back (R13); a replace never is.
  const navigate = useCallback((to: Route, replace?: boolean) => goTo(to, replace, phone && replace !== true ? { phoneBack: true } : undefined), [goTo, phone]);
  const overlay = route.name === 'settings' || route.name === 'serverSettings';
  const base = useMemo(() => baseRoute(route), [route]);
  const routeCommunity = base.name === 'channel' ? base.communityId : route.name === 'serverSettings' && route.from === null ? route.communityId : null;
  const channelId = base.name === 'channel' ? base.channelId : null;
  const dmId = base.name === 'dm' ? base.channelId : null;

  const account = useSlice('account');
  const connection = useSlice('connection');
  const [browserOnline, setBrowserOnline] = useState(() => navigator.onLine);
  useEffect(() => {
    const sync = () => setBrowserOnline(navigator.onLine);
    window.addEventListener('online', sync);
    window.addEventListener('offline', sync);
    sync();
    return () => { window.removeEventListener('online', sync); window.removeEventListener('offline', sync); };
  }, []);
  const communities = useSlice('communities');
  // Re-read the browser's advisory timeout after a community_changed wakeup, even when the member
  // list itself is byte-for-byte unchanged.
  const timeoutRefresh = useSlice(routeCommunity === null ? 'communityChanges:none' : `communityChanges:${routeCommunity}`);
  const dms = useSlice('dms');
  const badges = useSlice('badges');
  const settings = useSlice('settings');
  const search = useSlice('search');
  const [searchOpen, setSearchOpen] = useState(false);
  const [searchQuery, setSearchQuery] = useState('');
  const [searchRequest, setSearchRequest] = useState(0);
  const searchRequestRef = useRef(0);
  // The page's own count of search commands still unanswered: the palette says it is searching until the newest settles.
  const searchCalls = useRef(0);
  const [searchBusy, setSearchBusy] = useState(false);
  // Where focus was when the palette opened; Escape or close returns it there, a picked hit keeps focus on its row.
  const searchOpener = useRef<HTMLElement | null>(null);
  const restoreSearchFocus = useRef<HTMLElement | null>(null);
  const trust = useSlice('trust');
  const sendBlocks = useSlice('sendBlocks');
  const callSlice = useSlice('call');
  const voiceSlice = useSlice('voice');
  const rings = useSlice('rings');
  // A send the core refused for a trust reason, until the next published gate says more (W-01).
  const [refusedSend, setRefusedSend] = useState<{ channelId: string; code: TrustBlock; gate: unknown } | null>(null);

  // A DM route names no server: the sidebar keeps the last one a channel route named in this mount, else the first.
  const [lastCommunity, setLastCommunity] = useState<string | null>(null);
  useEffect(() => {
    if (routeCommunity !== null) setLastCommunity(routeCommunity);
  }, [routeCommunity]);
  const fallbackCommunity = lastCommunity !== null && communities?.some(c => c.id === lastCommunity) === true
    ? lastCommunity : communities?.[0]?.id ?? null;
  const communityId = routeCommunity ?? (dmId !== null || base.name === 'dms' ? fallbackCommunity : null);
  const navigateToUtilitySource = useCallback((sourceCommunityId: string, sourceChannelId: string, sourceThreadId: string | null) => {
    if (sourceThreadId !== null) requestThread(sourceChannelId, sourceThreadId);
    void navigate({ name: 'channel', communityId: sourceCommunityId, channelId: sourceChannelId });
  }, [navigate]);
  const botCommands = useBotCommandProvider(client, communityId);
  const utilityCommands = useUtilityCommandProvider(client, communityId, navigateToUtilitySource);
  const { handlers: botHandlers, suggest: botSuggest } = botCommands;
  const { handlers: utilityHandlers, suggest: utilitySuggest } = utilityCommands;
  const commandHandlers = useMemo<CommandProviderHandlers>(() => ({
    ...botHandlers,
    ...utilityHandlers,
    ...suppliedHandlers,
  }), [botHandlers, utilityHandlers, suppliedHandlers]);
  const commandSuggest = useCallback<NonNullable<CommandSuggestionProvider>>((commandId, option, query, binding, values) => {
    if (suppliedHandlers?.[commandId] !== undefined && suppliedSuggest !== undefined)
      return suppliedSuggest(commandId, option, query, binding, values);
    if (utilityHandlers[commandId] !== undefined)
      return utilitySuggest(commandId, option, query, binding, values);
    if (botHandlers[commandId] !== undefined)
      return botSuggest(commandId, option, query, binding, values);
    return suppliedSuggest?.(commandId, option, query, binding, values) ?? Promise.resolve(undefined);
  }, [botHandlers, botSuggest, suppliedHandlers, suppliedSuggest, utilityHandlers, utilitySuggest]);

  // An empty id names a slice that never exists, so it reads undefined.
  const channels = useSlice(`channels:${communityId ?? ''}`);
  const members = useSlice(`members:${communityId ?? ''}`);
  // Signal §5.4: the sidebar's stat line counts the server's members who are not offline (the members aside's own presence slice).
  const presence = useSlice(`presence:${communityId ?? ''}`);
  const afkLists = useSlices((communities ?? []).map(c => `afk:${c.id}` as const));
  const railSlices = useSlices((communities ?? []).map(c => `channels:${c.id}` as const));
  // AW5: every listed server's built-in AFK channel. Nobody joins a call there (R-AW1): a press on it chooses AFK, which
  // leaves the current call and marks the person away (useCall); the sweep's cut says so (`afk.moved`) and joins nothing.
  const afkChannelsKey = afkLists.map(a => a?.channelId ?? '').join(',');
  const afkChannels = useMemo<ReadonlySet<string>>(() => new Set(afkChannelsKey.split(',').filter(id => id !== '')), [afkChannelsKey]);
  const afkOption = useMemo(() => ({ channels: afkChannels }), [afkChannels]);
  const memberLists = useSlices((communities ?? []).map(c => `members:${c.id}` as const));
  const allMembers = useMemo(() => mergeMembers(memberLists), [memberLists]);
  // Task 11 (Q2): every listed server's presence, for the DM rows' online dots and the DM list's "N online".
  const presenceLists = useSlices((communities ?? []).map(c => `presence:${c.id}` as const));
  const presenceOf = (userId: string): 'online' | 'idle' | 'dnd' | 'offline' => {
    for (const list of presenceLists) { const p = list?.[userId]; if (p !== undefined && p !== 'offline') return p; }
    return 'offline';
  };

  const [joinOpen, setJoinOpen] = useState(false);
  const [joinInvite, setJoinInvite] = useState<string | null>(null);
  const [joinError, setJoinError] = useState<UiError | null>(null);
  const [joined, setJoined] = useState<string | null>(null);
  const [commandError, setCommandError] = useState<UiError | null>(null);
  const [openFailure, setOpenFailure] = useState<{ channelId: string; error: UiError } | null>(null);
  const [selectFailure, setSelectFailure] = useState<{ communityId: string; error: UiError } | null>(null);
  // The channel or DM whose loadEarlier call is in flight.
  const [loadingEarlier, setLoadingEarlier] = useState<string | null>(null);
  // The composer is controlled: the text of the channel or DM it was typed in.
  // `typed`: only a draft edited in this page sends typing; a restored update checkpoint cannot clear AFK.
  const [draft, setDraft] = useState<{ channelId: string | null; text: string; typed?: true }>({ channelId: null, text: '' });
  // Mention (task 9): the composer takes focus, caret at the end, once nothing covers it.
  const mentionFocus = useRef(false);
  const [, setMentionTick] = useState(0);
  useEffect(() => {
    if (!mentionFocus.current) return;
    const box = document.querySelector<HTMLTextAreaElement>('.d-app-shell__main .d-composer textarea');
    if (box === null || box.closest('[inert]') !== null || document.querySelector('.d-app-shell__backdrop') !== null) return;
    mentionFocus.current = false;
    box.focus();
    box.setSelectionRange(box.value.length, box.value.length);
  });
  const ordinaryDrafts = useRef(new Map<string, string>());
  const [selfTimeout, setSelfTimeout] = useState<{ communityId: string; expires: number } | null>(null);
  const selfUserId = account?.user?.id;
  useEffect(() => {
    if (routeCommunity === null || selfUserId === undefined) { setSelfTimeout(null); return; }
    let live = true;
    let expiryTimer: number | undefined;
    const refresh = async () => {
      try {
        const value = await client.call({ m: 'getMemberTimeout', communityId: routeCommunity, userId: selfUserId });
        if (!live) return;
        const expires = value === null ? null : Number(value);
        if (expires === null || !Number.isSafeInteger(expires) || expires <= Math.floor(Date.now() / 1000)) {
          setSelfTimeout(current => current?.communityId === routeCommunity ? null : current);
          return;
        }
        setSelfTimeout({ communityId: routeCommunity, expires });
        expiryTimer = window.setTimeout(() => { void refresh(); }, Math.max(0, expires * 1000 - Date.now()));
      } catch {
        // The server enforces the timeout. A failed advisory read must not block unrelated app actions.
      }
    };
    setSelfTimeout(null);
    void refresh();
    return () => { live = false; if (expiryTimer !== undefined) window.clearTimeout(expiryTimer); };
  }, [client, routeCommunity, selfUserId, timeoutRefresh]);
  // Flow 07: start a server, add a channel. (The invite link lives under server settings → Invites.)
  const [createOpen, setCreateOpen] = useState(false);
  const [channelOpen, setChannelOpen] = useState(false);
  // The category a channel is created in (its header's "+"), or null for the top level.
  const [channelParent, setChannelParent] = useState<string | null>(null);
  // The rail's "+": one dialog whose two choices are the existing join and create flows.
  const [addOpen, setAddOpen] = useState(false);
  // Web3.8 (R18): the pane widths the person chose, kept in this browser. The ui never re-clamps what is stored; it clamps
  // what it shows, and only a drag or a key reports a new width.
  const [paneWidths, setPaneWidths] = useState(readPaneWidths);
  const paneRef = useRef(paneWidths);
  const onPaneResize = useCallback((pane: 'sidebar' | 'aside', px: number | null) => {
    const next = { ...paneRef.current };
    if (px === null) delete next[pane];
    else next[pane] = px;
    paneRef.current = next;
    setPaneWidths(next);
    writePaneWidths(next);
  }, []);
  // Flow 08: the member pane (a toggle in the channel header; open at 64rem or wider, not remembered) and the server settings
  // dialog (the server it is open for, and its tab).
  // The phone keyboard is up over a focused field (AppShell says so): the call bar steps aside for a dot in the header (H3).
  const [keyTyping, setTyping] = useState(false);
  const [membersOpen, setMembersOpen] = useState(() => typeof window.matchMedia === 'function' && window.matchMedia('(min-width: 64rem)').matches);
  // Final fix B2: the open default is the wide column's. Crossing below 64rem closes the pane unless it was opened while the
  // shell was already narrow (a drawer the person asked for there); widening forgets that. The drawer never opens by itself.
  const openedNarrow = useRef(false);
  useEffect(() => {
    if (typeof window.matchMedia !== 'function') return;
    const query = window.matchMedia('(min-width: 64rem)');
    const onChange = (e: { matches: boolean }) => {
      if (e.matches) openedNarrow.current = false;
      else if (!openedNarrow.current) setMembersOpen(false);
    };
    query.addEventListener('change', onChange);
    return () => query.removeEventListener('change', onChange);
  }, []);
  // web3.7: one aside. A thread or the thread list of the open channel takes the member list's place; the members' own state is kept
  // under it, so closing the thread gives back what was there. The toggle shows the members and ends the thread.
  const [side, setSide] = useState<Side | null>(null);
  const [threadDraft, setThreadDraft] = useState<{ channelId: string; rootMsgId: string; text: string } | null>(null);
  const threadDrafts = useRef(new Map<string, string>());
  const threadDraftKey = (channelId: string, rootMsgId: string) => `${channelId}/${rootMsgId}`;
  useEffect(() => { setSide(current => (current !== null && current.channelId !== channelId ? null : current)); }, [channelId]);
  const toggleMembers = () => {
    const narrowNow = typeof window.matchMedia !== 'function' || !window.matchMedia('(min-width: 64rem)').matches;
    const open = side !== null && side.channelId === channelId ? true : !membersOpen;
    openedNarrow.current = open && narrowNow;
    setSide(null);
    setMembersOpen(open);
  };
  // The open profile card from a mention pill or a message's author (MP7); the member pane holds its own for a row. Opening one closes the other.
  const [profileCard, setProfileCard] = useState<OpenCard | null>(null);
  const adminFor = route.name === 'serverSettings' ? route.communityId : null;
  const adminTab = route.name === 'serverSettings' ? route.section : null;
  // Final fix I1: where focus was when server settings opened (as searchOpener); it goes back there when the dialog closes.
  const adminOpener = useRef<HTMLElement | null>(null);
  const openAdminFor = (id: string, tab: AdminTab | null) => {
    const active = document.activeElement;
    adminOpener.current = active instanceof HTMLElement && active !== document.body ? active : null;
    void navigate({ name: 'serverSettings', communityId: id, section: tab, from: null });
  };
  const adminWas = useRef<string | null>(null);
  useEffect(() => {
    const was = adminWas.current;
    adminWas.current = adminFor;
    if (was === null || adminFor !== null) return;
    const opener = adminOpener.current;
    adminOpener.current = null;
    if (opener?.isConnected) { opener.focus(); return; }
    const settingsButton = document.querySelector<HTMLElement>('[data-focus-key="admin-open"]');
    if (settingsButton !== null) settingsButton.focus();
    else focusComposer();
  }, [adminFor]);
  // Final fix M2: the channel whose edit dialog the header's "edit channel" opened.
  const [editChannelId, setEditChannelId] = useState<string | null>(null);
  // Fix round 1 (Jonas): the call code of the voice channel whose row menu asked for it, in a dialog.
  const [codeFor, setCodeFor] = useState<string | null>(null);
  // Signal task 10 fix round 2: the server head's ⌄ and the DM list head's menus.
  const [headMenu, setHeadMenu] = useState<'server' | 'dms' | null>(null);
  const access = useSlice(`permissions:${communityId ?? ''}`);
  // The toggle is found by its marker: the shared Button takes no ref, and AppShell reads `current` only when the drawer closes.
  const membersToggle = useMemo<RefObject<HTMLElement | null>>(
    () => ({ get current() { return document.querySelector<HTMLElement>('[data-members-toggle], [data-focus-key="channel-menu"]'); } }), []);
  // The one polite announcer (always rendered); MemberPane and ServerSettings speak through it.
  const { text: announcement, announce } = useAnnouncer();
  const removals = useSlice('removals');
  const [dismissedRemovals, setDismissedRemovals] = useState<ReadonlySet<number>>(() => new Set());
  const seenRemovals = useRef<ReadonlySet<number>>(new Set());
  const createFlowOpen = createOpen || channelOpen;
  // The text channel a create just opened: its composer takes focus once it is on screen (after the dialog handed focus back).
  // `from` is the conversation open when the wish was made: the shell may still show it for a render or two.
  const pendingFocus = useRef<{ id: string; from: string | null } | null>(null);
  // The DM picker (requirement 5).
  const [pickerOpen, setPickerOpen] = useState(false);
  const [pickerBusy, setPickerBusy] = useState(false);
  const [pickerError, setPickerError] = useState<UiError | null>(null);

  const ordered = useMemo(() => orderChannels(channels ?? [], afkChannels), [channels, afkChannels]);
  const community = communities?.find(c => c.id === communityId) ?? null;
  const channel = ordered.find(c => c.id === channelId) ?? null;
  const dm = dmId === null ? null : dms?.find(d => d.id === dmId) ?? null;
  // A voice channel shows the voice view (web-3): no timeline, no composer.
  const voiceChannel = channel !== null && isVoice(channel) ? channel : null;
  const unsupported = channel !== null && voiceChannel === null && isUnsupported(channel);
  // What is open: an openable channel, else a listed DM. A channel that cannot open keeps no timeline.
  const openId = channel !== null && !unsupported && voiceChannel === null ? channel.id : dm !== null ? dm.id : null;
  // The channel or DM on screen, open or not: the composer, the draft and loadEarlier follow it.
  const targetId = (voiceChannel === null ? channel?.id : undefined) ?? dm?.id ?? null;
  const timeline = useSlice(`timeline:${openId ?? ''}`);
  const typing = useSlice(`typing:${openId ?? ''}`) ?? [];
  const typingNames = [...new Map(typing.map(row => [row.userId, row.name])).values()];
  const issueSearch = (query: string, scope: 'all', id: null) => {
    if (!query.trim()) return;
    searchRequestRef.current = Math.max(searchRequestRef.current, search?.requestId ?? 0);
    setSearchRequest(0);
    const call = ++searchCalls.current;
    setSearchBusy(true);
    client.call({ m: 'search', scope, id, query, cursor: null }).catch(report)
      .finally(() => { if (searchCalls.current === call) setSearchBusy(false); });
  };
  useEffect(() => {
    if (searchOpen && search?.requestId && search.requestId > searchRequestRef.current) {
      searchRequestRef.current = search.requestId;
      setSearchRequest(search.requestId);
    }
  }, [searchOpen, search]);
  // A button that opens search passes itself as `from`, so focus returns to it on close whatever a tap did to focus (iOS does not focus a tapped button).
  // Task 9 fix round 2: one model, the prototype's palette; messages are searched everywhere (the open conversation's hits first).
  const startSearch = (from?: HTMLElement) => {
    const active = document.activeElement;
    searchOpener.current = from ?? (active instanceof HTMLElement && active !== document.body ? active : null);
    setSearchQuery('');
    setSearchOpen(true);
  };
  useEffect(() => {
    const onKey = (event: globalThis.KeyboardEvent) => {
      if (event.isComposing || event.shiftKey || event.altKey || !(event.ctrlKey || event.metaKey)) return;
      // M-01: never under another modal (pins, lightbox, any open dialog), and never again while search is open.
      if (overlay || joinOpen || pickerOpen || createFlowOpen || searchOpen || document.querySelector('dialog[open]') !== null) return;
      // Control+/ opens the same palette as Control/⌘+K (it used to open a conversation-scoped search).
      if (event.key.toLowerCase() === 'k' || (event.ctrlKey && !event.metaKey && event.key === '/')) { event.preventDefault(); startSearch(); }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [overlay, joinOpen, pickerOpen, createFlowOpen, searchOpen]);
  // Ruling Z: the open DM's call is Control/⌘+Shift+Enter (no button in its header). Not while typing an IME composition, not
  // under a modal; nothing when this DM's call is already up or held.
  // Null while no DM is open: the shortcut then leaves the key alone (no preventDefault), so Ctrl+Shift+Enter keeps its default.
  const dmCallKey = useRef<(() => void) | null>(null);
  dmCallKey.current = dm !== null ? () => { dmCallStart(voiceCtx, dm.id)?.onStart(); } : null;
  useEffect(() => {
    const onKey = (event: globalThis.KeyboardEvent) => {
      if (event.isComposing || event.altKey || !event.shiftKey || !(event.ctrlKey || event.metaKey) || event.key !== 'Enter') return;
      if (overlay || joinOpen || pickerOpen || createFlowOpen || searchOpen || document.querySelector('dialog[open]') !== null) return;
      const start = dmCallKey.current;
      if (start === null) return;
      event.preventDefault();
      // A held chord auto-repeats: only its first press starts the call.
      if (event.repeat) return;
      start();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [overlay, joinOpen, pickerOpen, createFlowOpen, searchOpen]);
  useEffect(() => {
    if (searchOpen) return;
    const opener = restoreSearchFocus.current;
    restoreSearchFocus.current = null;
    if (opener?.isConnected) opener.focus();
  }, [searchOpen]);
  const self = account?.user ?? null;
  // A removal is told once: the banner below, and nothing of that server stays open (effects (2) and (1) below already take the route
  // from a server that left the list to the first remaining one).
  useEffect(() => {
    const fresh = (removals ?? []).filter(r => !seenRemovals.current.has(r.id));
    if (fresh.length === 0) return;
    seenRemovals.current = new Set([...seenRemovals.current, ...fresh.map(r => r.id)]);
    const gone = new Set(fresh.map(r => r.communityId));
    if (communityId !== null && gone.has(communityId)) {
      setChannelOpen(false);
    }
  }, [removals, communityId]);
  const conversationMembers = channel !== null ? members ?? [] : dm !== null ? allMembers.filter(m => dm.members.includes(m.userId)) : [];
  const book = useMemo(() => nameBook(self, channel !== null ? members ?? [] : allMembers), [self, channel, members, allMembers]);
  const encodeWith = mentionMembers(conversationMembers);
  const candidates = mentionCandidates(conversationMembers, self?.id ?? null);
  // Fallback only: what I may do in the open channel (roles, overwrites, access), null while unknown and for a DM.
  const bits = useChannelBits(channel === null ? null : communityId, channel?.id ?? null);
  const roles = useSlice(`roles:${communityId ?? ''}`);
  const broadcast = channel !== null;
  const [ui, rawDispatch] = useConversationUi(targetId);
  // Ruling X: a channel row's menu asks for the pins of a channel that may not be open yet: they open once it is.
  const pinsFor = useRef<string | null>(null);
  useEffect(() => {
    const wanted = pinsFor.current;
    if (wanted === null) return;
    // Any other target first (a deep link, a back) drops the request: the pins never open on a later visit by surprise.
    pinsFor.current = null;
    if (wanted === targetId) rawDispatch({ type: 'pins', open: true });
  }, [targetId, rawDispatch]);
  const slow = useSlice(`slow:${targetId ?? ''}`);
  const overwrites = useSlice(`overwrites:${communityId ?? ''}`);
  // A jump to a message that shows in a thread (a reply quote, a pin) opens that thread; any other jump stays in the channel.
  const threadOfJump = useRef<(msgId: string) => string | null>(() => null);
  threadOfJump.current = msgId => {
    if (channelId === null) return null;
    const pinned = client.get(`pins:${channelId}`)?.find(pin => pin.msgId === msgId)?.threadRoot ?? null;
    return pinned ?? client.get(`timeline:${channelId}`)?.items.find(item => item.reply?.msgId === msgId)?.reply?.threadRoot ?? null;
  };
  const openThreadRef = useRef<(rootMsgId: string) => void>(() => undefined);
  const dispatch = useCallback((a: ConversationAction) => {
    if (a.type === 'jump') {
      const root = threadOfJump.current(a.msgId);
      if (root !== null) { rawDispatch({ type: 'pins', open: false }); openThreadRef.current(root); return; }
    }
    rawDispatch(a);
  }, [rawDispatch]);
  const group: ChannelGroupState = channel !== null
    ? unsupported ? 'unsupported' : timeline?.group ?? channel.group
    : dm !== null ? timeline?.group ?? dm.group : 'none';
  const openError = openFailure !== null && openFailure.channelId === openId ? openFailure.error : null;
  // A server whose channels never arrived because its selectCommunity was refused (pre-flight ruling (c)).
  const selectError = selectFailure !== null && selectFailure.communityId === communityId && channels === undefined
    ? selectFailure.error : null;

  const report = useCallback((e: unknown) => setCommandError(errorOf(e)), []);

  // Voice (web-3, flow 06). The name of a channel or DM for {where}, from every loaded server and the DM list.
  const whereOf = useCallback((id: string) => (railSlices.flat() as ChannelSummary[]).find(c => c.id === id)?.name
    ?? dms?.find(d => d.id === id)?.name ?? id.slice(0, 8), [railSlices, dms]);
  // After a leave the person asked for, focus goes to the voice row (or the DM's call button) the call came from.
  const [leftFrom, setLeftFrom] = useState<{ id: string } | null>(null);
  // Product walk M8: {where} of a call names a DM's call as "the call with {name}", a voice channel by its name.
  const callWhere = useCallback((id: string) => (dms?.some(d => d.id === id) === true ? t('call.where', { name: whereOf(id) }) : whereOf(id)),
    [dms, whereOf]);
  // Task 11: the user panel's line names the room of this device's call ("in harbour").
  const selfRoom = callSlice?.phase === 'in' && callSlice.channelId !== null ? callWhere(callSlice.channelId) : null;
  const upgradeRejoinUntil = useRef(0);
  const recoverEnded = useCallback((id: string) => Date.now() < upgradeRejoinUntil.current
    && (railSlices.flat() as ChannelSummary[]).some(c => c.id === id && c.kind === 1), [railSlices]);
  const call = useCall({ client, call: callSlice, settings: settings ?? {}, whereOf: callWhere, onLeft: id => setLeftFrom({ id }),
    stageShown: id => id === (voiceChannel?.id ?? dm?.id), recoverEnded, selfShortcuts: !phone, announce,
    afk: afkOption });
  // The clock lives on the server. This device reports only real input or microphone speech;
  // listening, another tab and another device do not refresh this call's AFK timer.
  const afkTrackingKey = useRef<string | null>(null);
  const afkLastReport = useRef(0);
  // R5: a phone (or a coarse pointer) is exempt from the move; its first report after each join says so. Final review
  // M-1: that report is retried (1, 2, 4 s, doubling up to 30 s) until it is accepted or the call ends.
  const afkExemptSent = useRef<string | null>(null);
  const afkExemptRetry = useRef<number | null>(null);
  useEffect(() => () => stopRetry(afkExemptRetry), []);
  useEffect(() => {
    const channel = call.channelId;
    // A call on another channel (the AFK one included) ends the earlier join's exempt report and its retry.
    if (afkExemptSent.current !== null && afkExemptSent.current !== channel) { afkExemptSent.current = null; stopRetry(afkExemptRetry); }
    const home = (communities ?? []).findIndex((c, i) => railSlices[i]?.some(ch => ch.id === channel && ch.kind === 1));
    const callCommunityId = home < 0 ? null : communities?.[home]?.id ?? null;
    const callAFK = home < 0 ? null : afkLists[home];
    if (callCommunityId === null || channel === null || call.session === null || callAFK?.channelId === null || callAFK?.channelId === undefined
      || channel === callAFK.channelId) {
      if (channel === null || call.session === null) { afkTrackingKey.current = null; afkExemptSent.current = null; stopRetry(afkExemptRetry); }
      return;
    }
    // Owned by this join until accepted: a failure retries while afkExemptSent still names this channel.
    const sendExempt = (attempt: number) => {
      void client.call({ m: 'afkActivity', communityId: callCommunityId, channelId: channel, exempt: true }).catch(() => {
        if (afkExemptSent.current !== channel) return;
        stopRetry(afkExemptRetry);
        afkExemptRetry.current = window.setTimeout(() => {
          afkExemptRetry.current = null;
          if (afkExemptSent.current === channel) sendExempt(attempt + 1);
        }, Math.min(30_000, 1_000 * 2 ** attempt));
      });
    };
    const activity = (fromMic = false) => {
      if ((!fromMic && document.visibilityState !== 'visible') || Date.now() - afkLastReport.current < 5_000) return;
      afkLastReport.current = Date.now();
      const exempt = afkExemptSent.current !== channel
        && (phone || (typeof window.matchMedia === 'function' && window.matchMedia('(pointer: coarse)').matches));
      afkExemptSent.current = channel;
      if (exempt) { stopRetry(afkExemptRetry); sendExempt(0); return; }
      void client.call({ m: 'afkActivity', communityId: callCommunityId, channelId: channel }).catch(() => undefined);
    };
    const speech = () => { if (call.state?.speaking) activity(true); };
    const userActivity = () => activity();
    const key = `${callCommunityId}:${channel}:${callAFK.channelId}`;
    if (afkTrackingKey.current !== key) {
      afkTrackingKey.current = key;
      afkLastReport.current = 0;
      activity(true);
    }
    speech();
    const timer = window.setInterval(speech, 5_000);
    window.addEventListener('pointermove', userActivity);
    window.addEventListener('pointerdown', userActivity);
    window.addEventListener('keydown', userActivity);
    window.addEventListener('touchstart', userActivity);
    return () => {
      window.clearInterval(timer);
      window.removeEventListener('pointermove', userActivity);
      window.removeEventListener('pointerdown', userActivity);
      window.removeEventListener('keydown', userActivity);
      window.removeEventListener('touchstart', userActivity);
    };
  }, [client, communities, railSlices, afkLists, phone, call.channelId, call.session, call.state?.speaking]);
  const callRestoreRef = useRef(call.restore);
  callRestoreRef.current = call.restore;
  useLayoutEffect(() => {
    if (leftFrom === null) return;
    const name = whereOf(leftFrom.id);
    // WB a11y m5: only the call's own row, or its own DM's row (Ruling Z: the call lives in its menu); else the `@` tile, where the dock was
    // (never another DM's call button, never the page).
    const target = document.querySelector<HTMLElement>(`button.d-voice-row[aria-label="${CSS.escape(t('voice.open', { name }))}"]`)
      ?? (dm?.id === leftFrom.id ? document.querySelector<HTMLElement>('.dw-sidebar .d-chrow[aria-current="page"]') : null)
      ?? document.querySelector<HTMLElement>('.d-community-rail__home, .dw-sidebar h1');
    target?.focus();
    setLeftFrom(null);
  }, [leftFrom, whereOf, dm?.id]);
  // A typing signal is a hint: one the worker refuses (the channel is not viewable any more, or the socket is not online,
  // E_NOT_READY) is simply not sent, never an error banner (the docs shoot's shell-not-member image showed one).
  const typingRefused = useCallback(() => undefined, []);
  const typingSent = useRef<string | null>(null);
  const typingAt = useRef(0);
  useEffect(() => {
    const active = draft.channelId === targetId && draft.typed === true && draft.text.trim() !== '' ? targetId : null;
    if (typingSent.current === active) {
      // The server holds a typing signal for 10 s and meters one per 3 s (L-HTTP-96): a draft still changing renews it.
      if (active !== null && Date.now() - typingAt.current >= TYPING_RENEW_MS) {
        typingAt.current = Date.now();
        client.call({ m: 'typing', channelId: active, on: true }).catch(typingRefused);
      }
      return;
    }
    if (typingSent.current !== null) client.call({ m: 'typing', channelId: typingSent.current, on: false }).catch(typingRefused);
    typingSent.current = active;
    if (active !== null) {
      typingAt.current = Date.now();
      client.call({ m: 'typing', channelId: active, on: true }).catch(typingRefused);
    }
  }, [client, draft.channelId, draft.text, draft.typed, targetId, typingRefused]);
  useEffect(() => () => {
    if (typingSent.current !== null) client.call({ m: 'typing', channelId: typingSent.current, on: false }).catch(typingRefused);
  }, [client, typingRefused]);

  const selectCommunity = useCallback((id: string) => {
    setSelectFailure(null);
    client.call({ m: 'selectCommunity', communityId: id }).catch((e: unknown) => {
      setSelectFailure({ communityId: id, error: errorOf(e) });
      report(e);
    });
  }, [client, report]);

  // A refused open is shown in the main pane, never as a banner (Requirement 14).
  const openChannel = useCallback((id: string) => {
    setOpenFailure(null);
    client.call({ m: 'openChannel', channelId: id }).catch((e: unknown) => setOpenFailure({ channelId: id, error: errorOf(e) }));
  }, [client]);

  // Every effect below that navigates does nothing while Settings is open over the shell (L-TS-27, ruling 16).
  // (1) / and /welcome go to the first server; a followed invite link opens the join dialog first.
  useEffect(() => {
    if (overlay || communities === undefined || base.name === 'channel' || base.name === 'dm' || base.name === 'dms'
      || base.name === 'activity' || base.name === 'you') return;
    if (base.name === 'welcome' && base.invite !== null) {
      setJoinInvite(base.invite);
      // A join refused right after signup rides on this history entry (task 23, pre-flight ruling (g)).
      setJoinError(readJoinError(window.history.state));
      setJoinOpen(true);
    }
    if (communities.length > 0) void navigate({ name: 'channel', communityId: communities[0].id, channelId: null }, true);
    else if (base.name === 'welcome') void navigate({ name: 'root' }, true);
  }, [overlay, base, communities, navigate]);

  // (1b) A desktop has no You or Activity page: You is Settings → devices (opened over nothing, so closing it leaves for /), and
  // Activity is the server list. A phone shows both.
  useEffect(() => {
    if (phone || overlay) return;
    if (base.name === 'you') void goTo({ name: 'settings', section: null, from: null }, true, { from: null });
    else if (base.name === 'activity') void goTo({ name: 'root' }, true);
  }, [phone, overlay, base.name, goTo]);

  // (2) A server that is not in the list is left for /, except the one the join dialog just reported.
  useEffect(() => {
    if (overlay || communities === undefined) return;
    if (joined !== null && communities.some(c => c.id === joined)) setJoined(null);
    if (routeCommunity === null || routeCommunity === joined || communities.some(c => c.id === routeCommunity)) return;
    void navigate({ name: 'root' }, true);
  }, [overlay, routeCommunity, communities, joined, navigate]);

  // (2b) A DM the loaded list does not hold is left for /.
  useEffect(() => {
    if (overlay || dmId === null || dms === undefined || dm !== null) return;
    void navigate({ name: 'root' }, true);
  }, [overlay, dmId, dms, dm, navigate]);

  // (3) Once per server change, the worker loads its channels and members, and who is in its voice channels (L-TS-50
  // loadVoice; a refusal leaves the rows without people).
  const listed = community !== null;
  useEffect(() => {
    if (!listed || communityId === null) return;
    selectCommunity(communityId);
    client.call({ m: 'loadVoice', communityId, channelId: null }).catch(() => undefined);
  }, [communityId, listed, selectCommunity, client]);
  // (3a) Flow 07: the selected server's lists again when the window comes back, at most once per LIST_REFRESH_MS (counting the
  // load above). A background read: a failure keeps the lists and raises no banner, as the picker's re-read does. A return
  // inside the window is not dropped: it schedules one trailing read at the window's end (WB m1), which a hidden page skips
  // (its next return reads). The instance's op 54 community_changed is what announces a new channel; this re-read is the
  // fallback for a frame missed while the socket was down or the tab asleep.
  const listedAt = useRef(0);
  useEffect(() => {
    if (!listed || communityId === null) return;
    listedAt.current = Date.now();
    const id = communityId;
    let trailing: ReturnType<typeof setTimeout> | null = null;
    const read = () => {
      listedAt.current = Date.now();
      client.call({ m: 'selectCommunity', communityId: id }).catch(() => undefined);
    };
    const refresh = () => {
      if (document.visibilityState === 'hidden') return;
      const wait = listedAt.current + LIST_REFRESH_MS - Date.now();
      if (wait <= 0) { read(); return; }
      trailing ??= setTimeout(() => {
        trailing = null;
        if (document.visibilityState !== 'hidden') read();
      }, wait);
    };
    window.addEventListener('focus', refresh);
    document.addEventListener('visibilitychange', refresh);
    return () => {
      if (trailing !== null) clearTimeout(trailing);
      window.removeEventListener('focus', refresh);
      document.removeEventListener('visibilitychange', refresh);
    };
  }, [communityId, listed, client]);
  // (3b) An open DM: whether a call is live in it.
  const dmOpen = dm?.id ?? null;
  useEffect(() => {
    if (dmOpen !== null) client.call({ m: 'loadVoice', communityId: null, channelId: dmOpen }).catch(() => undefined);
  }, [dmOpen, client]);

  // (4) With the channels loaded, a channel route names a listed channel.
  useEffect(() => {
    if (overlay || base.name !== 'channel' || communityId === null || channels === undefined) return;
    if (channelId === null) {
      // A phone shows the server's channel list here; it never opens a channel by itself.
      const first = phone ? null : defaultChannel(ordered);
      if (first !== null) void navigate({ name: 'channel', communityId, channelId: first.id }, true);
    } else if (channel === null) {
      void navigate({ name: 'channel', communityId, channelId: null }, true);
    }
  }, [overlay, base.name, communityId, channelId, channels, ordered, channel, navigate, phone]);

  // (4b) Lost access: the open channel left the list while its server stays. Say so once; (4) takes the route to the server's
  // first channel, the draft stays in its store, and a dialog about the channel closes. Not told for a server that left.
  const openSeen = useRef<{ communityId: string; id: string; name: string; kind: number } | null>(null);
  useEffect(() => {
    const seen = openSeen.current;
    if (channel !== null) {
      if (communityId !== null) openSeen.current = { communityId, id: channel.id, name: channel.name, kind: channel.kind };
      return;
    }
    if (seen === null || overlay || channels === undefined || communityId !== seen.communityId || channelId !== seen.id) return;
    openSeen.current = null;
    if (communities?.some(c => c.id === communityId) !== true) return;
    // A text channel is said as #name; a voice channel by its bare name (it has no #).
    announce(t('chperm.lostAccess', { name: seen.kind === 0 ? `#${seen.name}` : seen.name }));
    setEditChannelId(open => (open === seen.id ? null : open));
  }, [overlay, channel, channels, communityId, channelId, communities, announce]);
  // The composer's cannot-send state is spoken once for each channel it is entered in (the draft and focus stay). The memo
  // resets on a channel change, or once the bits are known and allow sending; unknown bits (a slice re-read) never reset it.
  const cannotSend = bits !== null && (bits & PERM.sendMessages) === 0;
  const canSend = bits !== null && !cannotSend;
  const cannotTold = useRef<{ channelId: string | null; told: boolean }>({ channelId: null, told: false });
  const openName = channel?.name ?? '';
  const openChannelId = channel?.id ?? null;
  useEffect(() => {
    const memo = cannotTold.current;
    if (memo.channelId !== openChannelId) { memo.channelId = openChannelId; memo.told = false; }
    if (canSend) memo.told = false;
    if (!cannotSend || memo.told) return;
    memo.told = true;
    announce(t('chperm.cannotSend', { name: openName }));
  }, [cannotSend, canSend, openChannelId, openName, announce]);

  // (5) The channel or DM that can open is opened, and closed when it is left.
  useEffect(() => {
    if (openId === null) return;
    const id = openId;
    openChannel(id);
    return () => { client.call({ m: 'closeChannel', channelId: id }).catch(report); };
  }, [openId, openChannel, client, report]);

  // (6) Read markers (L-TS-23 markRead): while the open item's timeline is loaded, the page is visible and its badge
  // counts something, the worker moves the marker; once for each distinct (item, newest row, counts). A zero badge
  // means the marker already covers what the count sees, so nothing is sent for it.
  const visible = useDocumentVisible();
  const badge = openId === null ? undefined : badges?.[openId];
  const lastKey = timeline?.items.at(-1)?.key ?? '';
  const timelineLoaded = timeline !== undefined;
  const marked = useRef<string | null>(null);
  useEffect(() => {
    if (openId === null || !timelineLoaded || !visible) return;
    const unread = badge?.unread ?? 0;
    const mentions = badge?.mentions ?? 0;
    if (unread === 0 && mentions === 0) {
      marked.current = null;
      return;
    }
    const mark = `${openId}:${lastKey}:${unread}:${mentions}`;
    if (marked.current === mark) return;
    marked.current = mark;
    client.call({ m: 'markRead', channelId: openId }).catch(report);
  }, [openId, timelineLoaded, visible, badge?.unread, badge?.mentions, lastKey, client, report]);

  // The `@` Home tile (desktop and phone): selected on a DM route and on /dm, and with no server listed (WORKER-WEB-02: DMs
  // belong to no server, so the shell opens on them). Selecting it navigates; the route is the one source of truth.
  const noServer = communities !== undefined && communities.length === 0;
  const homeSelected = base.name === 'dm' || base.name === 'dms' || (noServer && base.name !== 'channel');
  // Alt+ArrowUp/Down select the previous/next listed channel from anywhere on a channel route.
  const channelRoute = base.name === 'channel';
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key !== 'ArrowUp' && e.key !== 'ArrowDown') return;
      if (!e.altKey || e.ctrlKey || e.metaKey || e.shiftKey || e.isComposing) return;
      if (overlay || joinOpen || pickerOpen || createFlowOpen || !channelRoute || communityId === null) return;
      const next = stepChannel(ordered, channelId, e.key === 'ArrowUp' ? -1 : 1);
      if (next === null) return;
      e.preventDefault();
      void navigate({ name: 'channel', communityId, channelId: next.id });
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [ordered, channelId, communityId, channelRoute, overlay, joinOpen, pickerOpen, createFlowOpen, navigate]);

  // A11Y-DESIGN-01: the log is keyed by its channel or DM and by its first slice, so it mounts with the rows it
  // holds and a polite log announces only rows added later. A log that held focus hands it on to its successor.
  const logKey = targetId === null ? null : `${targetId}:${timeline === undefined ? 'loading' : 'ready'}`;
  const logFocused = useRef(false);
  const conversationFocused = useRef(false);
  const composerFocused = useRef(false);
  useEffect(() => {
    const onFocusIn = (e: FocusEvent) => {
      logFocused.current = e.target instanceof Element && e.target.closest('[role="log"]') !== null;
      conversationFocused.current = e.target instanceof Element && e.target.closest('.dw-conversation') !== null;
      composerFocused.current = e.target instanceof HTMLTextAreaElement && e.target.closest('.d-composer') !== null;
    };
    window.addEventListener('focusin', onFocusIn);
    return () => window.removeEventListener('focusin', onFocusIn);
  }, []);
  useLayoutEffect(() => {
    if (logKey !== null && logFocused.current && focusLost()) focusLog();
  }, [logKey]);
  useLayoutEffect(() => {
    if (targetId !== null && conversationFocused.current && focusLost()) focusLog();
  }, [targetId]);
  useLayoutEffect(() => {
    if (targetId !== null && composerFocused.current && focusLost()) focusComposer();
  }, [targetId]);

  // A11Y-DESIGN-05: the two "try again" buttons are replaced in the render their click causes, so focus is placed
  // after it: the main pane's goes to the log, the sidebar's to the channel list's roving stop once its rows are
  // there and the route names a channel. A retry refused again puts focus back on the new "try again".
  const refocus = useRef<'log' | 'channels' | null>(null);
  useLayoutEffect(() => {
    if (openError !== null && focusLost() && (refocus.current === 'log' || logFocused.current)) {
      if (focusEmptyAction('content')) refocus.current = null;
    }
  }, [openError]);
  useLayoutEffect(() => {
    if (selectError !== null && focusLost() && refocus.current === 'channels') {
      if (focusEmptyAction('sidebar')) refocus.current = null;
    }
  }, [selectError]);
  const channelsReady = ordered.length > 0 && (channelId !== null || defaultChannel(ordered) === null);
  // No dependency list: the target may appear several renders after the click.
  useLayoutEffect(() => {
    const want = refocus.current;
    if (want === null) return;
    if (!focusLost()) { refocus.current = null; return; }
    const target = want === 'log' ? document.querySelector<HTMLElement>('[role="log"]')
      // The rows' roving stop, not the tablist's (which sits in the same navigation since task 20).
      : channelsReady ? document.querySelector<HTMLElement>('.d-channel-list__list button[tabindex="0"]') : null;
    if (target === null) return;
    target.focus();
    refocus.current = null;
  });
  const liveCall = useRef(call);
  liveCall.current = call;

  // When the join dialog closes, focus goes to the composer (after the dialog returned it to its opener).
  const wasJoinOpen = useRef(false);
  useEffect(() => {
    if (wasJoinOpen.current && !joinOpen) focusComposer();
    wasJoinOpen.current = joinOpen;
  }, [joinOpen]);

  // When the picker opened a DM, focus goes to its composer as the join dialog's does (pre-flight row 2.20); a picker
  // closed without one leaves focus where the dialog returned it, on "message someone".
  const pickedDm = useRef(false);
  useEffect(() => {
    if (pickerOpen || !pickedDm.current) return;
    pickedDm.current = false;
    focusComposer();
  }, [pickerOpen]);

  // Flow 07: a created text channel's composer takes focus once it is on screen. No dependency list: it may appear several
  // renders after the create answered. The wish is dropped once another conversation opens (review M6), so a composer that
  // shows later, after the person went elsewhere and came back, does not take focus.
  useEffect(() => {
    const pending = pendingFocus.current;
    if (pending === null) return;
    if (targetId !== null && targetId !== pending.id && targetId !== pending.from) { pendingFocus.current = null; return; }
    if (targetId !== pending.id) return;
    const box = document.querySelector<HTMLTextAreaElement>('.d-composer textarea');
    if (box === null || createFlowOpen) return;
    if (!phoneNow()) box.focus();
    pendingFocus.current = null;
  });

  const closeJoin = () => { setJoinOpen(false); setJoinInvite(null); setJoinError(null); };
  const onJoined = (id: string) => {
    setJoined(id);
    closeJoin();
    void navigate({ name: 'channel', communityId: id, channelId: null });
  };

  const closePicker = () => { setPickerOpen(false); setPickerError(null); };
  // One openDm at a time: the ref refuses a second click before the busy render lands.
  const picking = useRef(false);
  const startDm = (userId: string) => {
    if (picking.current) return;
    picking.current = true;
    setPickerBusy(true);
    setPickerError(null);
    const run = async () => {
      try {
        const result = await client.call({ m: 'openDm', userId });
        const id = typeof result === 'object' && result !== null ? (result as { channelId?: unknown }).channelId : undefined;
        if (typeof id === 'string' && HEX32.test(id)) {
          pickedDm.current = true;
          setPickerOpen(false);
          void navigate({ name: 'dm', channelId: id });
        }
      } catch (e) {
        setPickerError(errorOf(e));
      } finally {
        picking.current = false;
        setPickerBusy(false);
      }
    };
    void run();
  };

  const loadEarlier = () => {
    if (targetId === null) return;
    const id = targetId;
    setLoadingEarlier(id);
    const run = async () => {
      try {
        await client.call({ m: 'loadEarlier', channelId: id });
      } catch (e) {
        report(e);
      } finally {
        setLoadingEarlier(l => (l === id ? null : l));
      }
    };
    void run();
  };

  // The channels whose send is in flight: a second Enter or a click on send meanwhile posts nothing (WEB-APP-01).
  const sending = useRef(new Set<string>());
  // The text stays until the send resolves; a refused send keeps it (pre-flight ruling (d)).
  const send = (m: OutgoingMessage) => {
    if (targetId === null) return;
    const id = targetId;
    // Walk 3: the draft is cleared the moment the send starts, so a key that lands just after Enter begins the next
    // message and never joins the sent text. A text-only send does not wait for the one before it; only a send that
    // carries the file tray does, since the tray is cleared by the worker.
    const withTray = m.attachments.length > 0;
    if (withTray && sending.current.has(id)) return;
    if (withTray) sending.current.add(id);
    setDraft(d => {
      if (ordinaryDrafts.current.get(id) !== m.raw) return d;
      rememberDraft(ordinaryDrafts.current, id, '');
      return d.channelId === id && d.text === m.raw ? { channelId: id, text: '' } : { ...d };
    });
    const run = async () => {
      try {
        await client.call({ m: 'send', channelId: id, text: m.text,
          ...(m.replyTo !== null ? { replyTo: m.replyTo } : {}),
          ...(m.attachments.length > 0 ? { attachments: [...m.attachments] } : {}) });
        if (m.replyTo !== null) dispatch({ type: 'replySent', msgId: m.replyTo });
      } catch (e) {
        // A refused send gives the text back, unless something new has been typed since.
        setDraft(d => {
          if (ordinaryDrafts.current.get(id) !== '') return d;
          rememberDraft(ordinaryDrafts.current, id, m.raw);
          return d.channelId === id && d.text === '' ? { channelId: id, text: m.raw } : { ...d };
        });
        const error = errorOf(e);
        if (TRUST_BLOCKS.has(error.code)) setRefusedSend({ channelId: id, code: error.code as TrustBlock, gate: client.get('sendBlocks') });
        else report(e);
      } finally {
        if (withTray) sending.current.delete(id);
      }
    };
    void run();
  };

  // A click on the pill or author name that already holds the open card closes it (the card ignores a pointerdown on its anchor); any other opens, replacing the card.
  const openProfile: ProfileOpener = (userId, anchor, source, name) =>
    setProfileCard(cur => (cur !== null && cur.anchor === anchor ? null : { id: userId, source, anchor, name, rowId: anchor.closest<HTMLElement>('.d-message-row')?.dataset.msgId, scope: anchor.closest<HTMLElement>('.d-thread-pane, [role="log"]') ?? undefined }));
  const log = (label: string) => targetId === null || logKey === null ? null : <Conversation key={targetId} channelId={targetId}
    logKey={logKey} label={label} timeline={timeline} book={book} encodeWith={encodeWith} broadcast={broadcast} bits={bits} roles={roles}
    pinsTitle={channel !== null ? t('shell.pins.title', { channel: channel.name }) : t('shell.pins.titleDm', { name: dm?.name ?? '' })}
    ui={ui} dispatch={dispatch} loadingEarlier={loadingEarlier === targetId} onLoadEarlier={loadEarlier} onError={report}
    {...(channel !== null ? { onOpenThread: openThread } : {})}
    welcome={channel !== null
      ? { icon: channel.announcement ? 'megaphone' : 'hash', title: t('shell.log.welcome', { channel: channel.name }), body: t('shell.log.welcomeBody', { channel: channel.name }) }
      : { icon: 'at-sign', title: dm?.name ?? '', body: t('shell.log.welcomeDmBody') }}
    onOpenProfile={openProfile} />;
  const openFailed = (id: string, error: UiError) => (
    <EmptyState title={t('shell.openFailed.title')} body={t('shell.openFailed.body', { code: error.code })}
      action={{ label: t('shell.openFailed.action'), onAction: () => { refocus.current = 'log'; openChannel(id); } }} />
  );

  useSounds({ rings, room: call.channelId === null ? undefined : voiceSlice?.[call.channelId], inCall: call.channelId, presenceChoice: account?.presenceChoice,
    deafened: call.state?.deafened ?? false, settings: settings ?? {}, selfId: self?.id ?? null,
    ringCommunity: id => (railSlices.flat() as ChannelSummary[]).find(c => c.id === id)?.communityId });
  // A ring is answered in its DM: the DM opens and its call is joined (leaving the current voice first, useCall's join).
  const ringFlow = <RingFlow client={client} rings={rings} dms={dms} settings={settings ?? {}} presenceChoice={account?.presenceChoice}
    inCallWhere={call.channelId === null ? null : callWhere(call.channelId)}
    onOpen={id => void navigate({ name: 'dm', channelId: id })}
    onAnswer={id => { void navigate({ name: 'dm', channelId: id }).then(allowed => { if (allowed) call.join(id); }); }} />;
  // AW6: the AFK channel hosts no call (R-AW1). Its people are this server's away roster, named from its members and shown
  // as away, never the voice book's: under its row and on its page (fix round 1, Jonas's concern 2 ruling).
  const afkHere = afkLists[(communities ?? []).findIndex(c => c.id === communityId)];
  const awayPeople = (afkHere?.away ?? []).map(row => ({
    deviceId: `away:${row.userId}`, userId: row.userId, name: userName(row.userId, members ?? [], trust), state: 'ok' as const,
    speaking: false, muted: false, deafened: false, video: false, screen: false, web: false, bot: false, away: true,
  }));
  const selfAway = self !== null && (afkHere?.away ?? []).some(row => row.userId === self.id);
  // Task 8b: one music player for this device's call (README §9), read while the Bot is in that call.
  const callCommunityId = call.channelId === null ? null
    : (railSlices.flat() as ChannelSummary[]).find(ch => ch.id === call.channelId && ch.kind === 1)?.communityId ?? null;
  const callBot = call.channelId === null ? null : voiceSlice?.[call.channelId]?.participants.find(p => p.bot)?.name ?? null;
  const music = useMusicPlayer({ client, communityId: callCommunityId, channelId: call.channelId, room: call.channelId === null ? '' : whereOf(call.channelId),
    bot: callBot, members: allMembers, trust });
  const voiceCtx: VoiceContext = {
    client, call, slice: callSlice, music, voice: voiceSlice, members: allMembers, trust, sendBlocks, settings: settings ?? {}, self, ownDevice: account?.deviceId ?? null,
    onCompare: userId => void navigate({ name: 'settings', section: 'privacy', from: null, compare: userId }),
    onVoiceSettings: () => void navigate({ name: 'settings', section: 'voice', from: null }),
    onJoinVoice: id => call.join(id),
    afkPage: id => (afkChannels.has(id) && afkHere?.channelId === id ? { people: awayPeople, away: selfAway } : null),
  };

  // Flow 07: with no server, the one way in is to start one or to join one.
  const noServerState = <EmptyState title={t('shell.noServer.title')} body={t('shell.noServer.body')}
    action={{ label: t('shell.noServer.create'), onAction: () => setCreateOpen(true) }}
    secondary={{ label: t('shell.noServer.action'), onAction: () => setJoinOpen(true) }} />;

  let main: ReactNode = null;
  if (dm !== null) {
    main = openError !== null ? openFailed(dm.id, openError) : <>
      <DmCall v={voiceCtx} channelId={dm.id} name={dm.name} />
      {log(t('shell.dm.log', { name: dm.name }))}
    </>;
  } else if (voiceChannel !== null) {
    main = <VoiceView v={voiceCtx} channelId={voiceChannel.id} name={voiceChannel.name} />;
  } else if (communities !== undefined && communities.length === 0 && communityId === null) {
    main = noServerState;
  } else if (community !== null && channels !== undefined && ordered.length === 0) {
    main = <EmptyState title={t('shell.noChannel.title')} body={t('shell.noChannel.body')} />;
  } else if (channel !== null && unsupported) {
    main = <EmptyState title={t('shell.unsupported.title')} body={t(unsupportedBody(channel))} />;
  } else if (channel !== null && openError !== null) {
    main = openFailed(channel.id, openError);
  } else if (channel !== null) {
    main = log(t('shell.log.label', { channel: channel.name }));
  }

  // Badges (Q02, Q09, ruling 15): what each row shows, a muted row's unread hidden; the rail and the tabs sum them.
  // An announcement channel is a text channel whose @everyone overwrite denies send_messages (R36); slow mode is a badge.
  const everyoneDenied = (id: string) => (overwrites?.[id] ?? []).some(o => o.kind === 0 && o.targetId === access?.everyoneId && (o.deny & PERM.sendMessages) !== 0);
  const isAnnouncement = (c: ChannelSummary) => c.kind === 0 && (c.announcement || everyoneDenied(c.id));
  const channelRows: ListRow[] = ordered.map(c => ({
    id: c.id, name: c.name, kind: c.kind === 1 ? 'voice' : isAnnouncement(c) ? 'announcement' : 'text', readable: c.mode === 1, parentId: c.parentId,
    ...(afkChannels.has(c.id) ? { afk: true } : {}), ...(c.slowmode > 0 ? { badge: t('slow.badge') } : {}), ...rowBadge(badges, settings, c.id, communityId ?? undefined),
  }));
  // Task 11 (Jonas, Q2; the prototype's DM rows): a person's row is their avatar with the online dot over their status (or
  // "@handle"); a group's is the neutral avatar over its members ("Ada, Björn, you").
  // Fix round 2: a person this account shares no server with has no member row here, so nothing about them is known beyond the
  // DM's own name (the worker's short id): no dot and no sub line, rather than a guess. A group names every other member, the
  // unknown ones by that short id, so it never reads as just "you".
  const dmPerson = (d: DmSummary): NonNullable<ListRow['person']> => {
    const otherIds = d.members.filter(id => id !== self?.id);
    if (d.members.length > 2) {
      const names = [...otherIds.map(id => { const m = allMembers.find(x => x.userId === id); return m === undefined ? id.slice(0, 8) : m.display || m.username; }),
        t('shell.dm.you')].join(', ');
      const words = d.name.trim().split(/\s+/);
      return { group: true, sub: names, initials: (words.length > 1 ? words[0][0] + words[1][0] : d.name.slice(0, 2)).toUpperCase() };
    }
    const other = otherIds.length === 0 ? undefined : allMembers.find(m => m.userId === otherIds[0]);
    if (other === undefined) return {};
    return { presence: presenceOf(other.userId), sub: other.statusMsg ? other.statusMsg : `@${other.username}` };
  };
  const dmRows: ListRow[] = (dms ?? []).map(d => ({ id: d.id, name: d.name, kind: 'dm', readable: false, person: dmPerson(d), ...rowBadge(badges, settings, d.id) }));
  const dmOnline = (dms ?? []).filter(d => d.members.length === 2 && d.members.some(id => id !== self?.id && presenceOf(id) === 'online')).length;
  const dmCounts = sumBadges(dmRows);
  // R36: a category folds on this device only, the setting `channels.collapsed.<id>` = '1'; unfolding deletes it.
  const collapsedKey = (id: string) => `channels.collapsed.${id}`;
  const categoryRows = (channels ?? []).filter(c => c.kind === CATEGORY)
    .sort((a, b) => a.position - b.position || (a.id < b.id ? -1 : 1))
    .map(c => ({ id: c.id, name: c.name, collapsed: settings?.[collapsedKey(c.id)] === '1' }));
  const toggleCategory = (id: string) => {
    const folded = categoryRows.find(c => c.id === id)?.collapsed === true;
    client.call({ m: 'setSetting', key: collapsedKey(id), value: folded ? null : '1' }).catch(report);
  };
  // The voice channels sit under their own heading with their people (flow 06), not among the text rows.
  const speakingNow = speakingDevices(call.state, account?.deviceId ?? null);
  const voiceRows = ordered.filter(isVoice).map(c => ({ id: c.id, name: c.name, readable: false, current: c.id === channelId,
    joined: call.channelId === c.id, room: voiceSlice?.[c.id], speaking: call.channelId === c.id ? speakingNow : NOBODY,
    held: removedFromServer({ slice: callSlice }, c.id) }));
  // A voice channel this client cannot join (a readable one, `kind` voice but not `isVoice`) has no join and nobody under it.
  const voiceRooms: Record<string, { joined: boolean; held: boolean; participants: ReturnType<typeof participantView>[]; count?: string }> = {};
  for (const r of channelRows) if (r.kind === 'voice') voiceRooms[r.id] = { joined: false, held: true, participants: [] };
  for (const r of voiceRows) {
    // Fix round 1 (M4): nobody listed under AFK is in voice, so its count says '{n} in AFK'.
    voiceRooms[r.id] = afkChannels.has(r.id) ? { joined: r.joined, held: r.held, participants: awayPeople, count: en['voice.afk.count'] }
      : { joined: r.joined, held: r.held, participants: (r.room?.participants ?? []).map(p => participantView(p, 'ok', r.speaking.has(p.deviceId))) };
  }
  // The people in this server's voice rooms (not the AFK roster), each counted once.
  const inVoiceHere = new Set(voiceRows.filter(r => !afkChannels.has(r.id)).flatMap(r => (r.room?.participants ?? []).map(p => p.userId))).size;
  const openVoice = (id: string) => { if (communityId !== null) void navigate({ name: 'channel', communityId, channelId: id }); };
  const railItems = (communities ?? []).map((c, i) => {
    const list: readonly ChannelSummary[] = railSlices[i] ?? [];
    const counts = sumBadges(list.filter(ch => ch.kind !== CATEGORY).map(ch => rowBadge(badges, settings, ch.id, c.id)));
    return { id: c.id, name: c.name, unread: counts.unread, mentions: counts.mentions };
  });

  // Flow 07: the instance serves no role bits before the permissions read, so ownership stands in for ManageChannels until then.
  const owner = community !== null && self !== null && community.owner === self.id;
  const canManageChannels = access?.loaded === true ? access.manageChannels : owner;
  const editing = editChannelId === null ? null : (channels ?? []).find(c => c.id === editChannelId) ?? null;
  /** The call code of this device's call in `id`, once the call is in (30 digits), else null. */
  const callCodeOf = (id: string): string | null => (callSlice?.channelId === id && callSlice.phase === 'in' ? callSlice.code : null);
  const shownCode = codeFor === null ? null : callCodeOf(codeFor);
  // The sidebar with its tabs stands whenever a server is shown, DMs exist or a DM route is open (WORKER-WEB-02). On the
  // channels tab, a server that did not load and the absence of any server are said inside the tab panel, so the
  // DMs tab stays reachable (A11Y-DESIGN-02).
  let sidebar: ReactNode = null;
  const hasDms = (dms?.length ?? 0) > 0 || dmId !== null || base.name === 'dms';
  // The phone's tab (flow 12) follows the route; the desktop's sidebar tab is its own state.
  const phoneTab: PhoneTab = tabOf(base);
  // The pane a phone shows: a channel, a DM or a voice stage is a pushed view, and so is Settings; the rest are lists.
  const phonePane: 'list' | 'view' = overlay || base.name === 'dm' || (base.name === 'channel' && base.channelId !== null) ? 'view' : 'list';
  const openSettings = useRef<string | null>(null);
  if (phone && phonePane === 'list' && phoneTab === 'activity') {
    sidebar = <Activity reminders={{ due: utilityCommands.dueReminders, onOpen: from => utilityCommands.openInbox(from) }} onOpen={target => void navigate(target.communityId === null ? { name: 'dm', channelId: target.channelId }
      : { name: 'channel', communityId: target.communityId, channelId: target.channelId })} />;
  } else if (phone && phonePane === 'list' && phoneTab === 'you') {
    sidebar = <You onOpenSettings={section => { openSettings.current = section ?? 'index'; void navigate({ name: 'settings', section, from: null }); }} />;
  } else if (phone || community !== null || hasDms || noServer) {
    // Flow 07: with no server the sidebar still stands, on the DMs tab, so its picker can say why it is empty.
    const onDms = homeSelected;
    const serverId = community?.id ?? null;
    let content: ReactNode;
    if (!onDms && community !== null && selectError !== null) {
      const id = community.id;
      content = <EmptyState title={community.name} body={t('shell.sidebar.loadError', { code: selectError.code })}
        action={{ label: t('shell.sidebar.retry'), onAction: () => { refocus.current = 'channels'; selectCommunity(id); } }} />;
    } else if (!onDms && community === null) {
      content = noServerState;
    }
    const voiceHere = !onDms && community !== null && content === undefined;
    // Flow 07: the server's actions. The instance serves no role bits, so the one permission fact here is ownership: the
    // owner holds ManageChannels; every member holds CreateInvite by default (@everyone), and a refusal is said in the dialog.
    const openAdmin = (tab: AdminTab | null) => { if (community !== null) openAdminFor(community.id, tab); };
    // Final fix B1: "edit channel" sits in the channel header (M2), not here.
    // Web3.8 (WS8): the gear sits beside the server's name (leave and delete live in server settings); the phone adds search.
    // Product walk M12: opening the picker reads the server's members again (a removal, a newcomer since the load).
    // Fix round 3 (m4): a background read: a failure keeps the cached list and raises no banner.
    // Signal task 10 fix round 1: Settings → Notifications keeps only the prototype's rows, so muting a channel or a
    // conversation moves to its row's context menu (Ruling X's invisible route). The stored key is the one it always was.
    // Fix round 2 (Jonas): the menu also holds the conversation's own notify mode (default deletes it, as Settings did).
    const muted = (id: string) => settings?.[SETTINGS.muteChannel(id)] === '1';
    const notifyItems = (id: string) => {
      const mode = settings?.[SETTINGS.notifyChannel(id)] ?? 'default';
      return [{ id: 'mute', label: t('notify.mute'), check: muted(id) },
        ...NOTIFY_MODES.map(m => ({ id: `mode:${m.value}`, label: t(m.key), checked: mode === m.value, group: t('notify.title') }))];
    };
    const notifyChoice = (id: string, item: string): boolean => {
      if (item === 'mute') {
        client.call({ m: 'setSetting', key: SETTINGS.muteChannel(id), value: muted(id) ? null : '1' }).catch(report);
        return true;
      }
      if (!item.startsWith('mode:')) return false;
      const value = item.slice(5);
      client.call({ m: 'setSetting', key: SETTINGS.notifyChannel(id), value: value === 'default' ? null : value }).catch(report);
      return true;
    };
    const openDmPicker = () => {
      if (community !== null) client.call({ m: 'selectCommunity', communityId: community.id }).catch(() => undefined);
      setPickerOpen(true);
    };
    // H2, N2: the DM list's one action lives in its head (a square-pen) on every layout, not in a footer row of its own.
    // Ruling AB (Jonas 2026-10-27): a server's head is the prototype's: its name and the ⌄ that opens the server's settings (and
    // the "+" while it has no categories, the one place a channel is made then). Search is the top bar's and Control/⌘+K, or a
    // conversation's phone header; settings the top bar's gear or the You tab; reminders the bell or the Activity tab.
    // Signal task 10 fix round 2 (Jonas): the ⌄ is the server's menu: its settings, Mute server and the server's notify mode
    // (Settings → Notifications keeps only the prototype's rows). The DM list head's menu holds the device-wide default, which
    // governs direct messages. Same stored keys as Settings wrote them.
    const serverHex = community?.id ?? null;
    const serverMode = serverHex === null ? 'default' : settings?.[SETTINGS.notifyCommunity(serverHex)] ?? 'default';
    const serverMuted = serverHex !== null && settings?.[SETTINGS.muteCommunity(serverHex)] === '1';
    const serverMenu = (
      <ActionMenu label={t('admin.open')} focusKey="admin-open" open={headMenu === 'server'} onOpenChange={open => setHeadMenu(open ? 'server' : null)}
        triggerClassName="d-channel-list__head-btn" triggerContent={<Icon name={SERVER_MENU} size={15} />}
        items={[{ id: 'settings', label: t('signalSettings.menu.serverSettings') }, { id: 'mute', label: t('signalSettings.menu.muteServer'), check: serverMuted },
          ...NOTIFY_MODES.map(m => ({ id: `mode:${m.value}`, label: t(m.key), checked: serverMode === m.value, group: t('notify.title') }))]}
        onSelect={item => {
          if (item === 'settings') { openAdmin(null); return; }
          if (serverHex === null) return;
          if (item === 'mute') { client.call({ m: 'setSetting', key: SETTINGS.muteCommunity(serverHex), value: serverMuted ? null : '1' }).catch(report); return; }
          const value = item.slice(5);
          client.call({ m: 'setSetting', key: SETTINGS.notifyCommunity(serverHex), value: value === 'default' ? null : value }).catch(report);
        }} />
    );
    const dmDefault = settings?.[SETTINGS.notifyDefault] ?? 'dms-mentions';
    const dmsMenu = (
      <ActionMenu label={t('signalSettings.menu.dms')} open={headMenu === 'dms'} onOpenChange={open => setHeadMenu(open ? 'dms' : null)}
        triggerClassName="d-channel-list__head-btn" triggerContent={<Icon name={SERVER_MENU} size={15} />}
        items={DM_DEFAULTS.map(m => ({ id: m.value, label: t(m.key), checked: dmDefault === m.value, group: t('notify.title') }))}
        onSelect={value => { client.call({ m: 'setSetting', key: SETTINGS.notifyDefault, value: value === 'dms-mentions' ? null : value }).catch(report); }} />
    );
    const headerAction = onDms ? (
      <>
        <IconButton icon="square-pen" {...(phone ? {} : { size: 28 })} label={t('shell.dms.new')} onClick={openDmPicker} />
        {dmsMenu}
      </>
    ) : !voiceHere ? undefined : (
      <>
        {canManageChannels && categoryRows.length === 0 ? (
          <IconButton icon="plus" size={28} label={t('create.channel.open')} onClick={() => { setChannelParent(null); setChannelOpen(true); }} />
        ) : null}
        {serverMenu}
      </>
    );
    sidebar = (<div className="dw-sidebar">
      <ChannelList label={t(onDms ? 'shell.dms.label' : 'shell.channels.label')}
        title={onDms ? t('shell.dms.title') : community?.name ?? account?.instance?.name ?? t('shell.status.unknown')}
        {...(onDms ? { sectionLabel: t('shell.dms.section'), stats: [{ text: t('shell.dms.online', { n: dmOnline }), live: true }, { text: t('shell.dms.count', { n: (dms ?? []).length }) }] } : {})}
        headerAction={headerAction}
        // A1: with no categories the new-channel "+" sits in the head beside the gear (headerAction); with them each
        // category header carries its own, for a person who can manage channels.
        categories={onDms ? undefined : categoryRows}
        onToggleCategory={toggleCategory}
        {...(voiceHere && canManageChannels && !onDms ? { onAddInCategory: (id: string) => { setChannelParent(id); setChannelOpen(true); },
          addInCategoryLabel: (name: string) => t('layout.category.add', { name }) } : {})}
        // Walk 9: only the server name keeps the display serif; the DM list heading (Home) is the plain mono one.
        titleKind="server"
        channels={onDms ? dmRows : channelRows}
        // Web3.8 (item 12): the voice channels stand in position among the text rows, their people nested under them.
        voice={onDms ? undefined : { labels: { ...rowLabels(), join: t('voice.join'), joined: t('voice.row.joined') }, rooms: voiceRooms,
          onJoin: id => { openVoice(id); voiceCtx.onJoinVoice?.(id); },
          // Fix round 1 (Jonas): an occupant's person menu (right-click, long press, Shift+F10); the AFK roster is nobody in voice.
          personMenu: (roomId, person) => afkChannels.has(roomId) ? undefined : personMenu(voiceCtx, voiceRows.find(r => r.id === roomId)?.name ?? '',
            { userId: person.userId, name: person.name, bot: person.bot, own: person.userId === self?.id,
              ...(person.userId === self?.id ? { title: t('callStage.tile.you', { name: person.name }) } : {}) }) }}
        activeId={onDms ? dmId : channelId}
        onSelect={id => {
          void navigate(onDms || serverId === null ? { name: 'dm', channelId: id } : { name: 'channel', communityId: serverId, channelId: id });
          // R24: on a desktop a click on a voice row opens it and joins; a phone's tap only opens it (the 44px icon joins).
          const room = voiceRooms[id];
          if (!onDms && !phone && room !== undefined && !room.joined && !room.held) voiceCtx.onJoinVoice?.(id);
        }}
        emptyLabel={onDms ? t('shell.dms.empty') : channels === undefined ? t('shell.channels.loading') : t('shell.channels.empty')}
        rowLabel={badgeLabel}
        // Ruling X (Jonas 2026-10-27, pixel parity): the header has no "⋯"; a text channel's pins and edit channel are in its row's
        // context menu (right-click, long press, Shift+F10 or the ContextMenu key).
        // Ruling Z: a DM's call (Control/⌘+Shift+Enter too) and its pins are in the DM row's context menu.
        {...(onDms ? { rowMenu: {
          label: (name: string) => t('nav.channel.menuOf', { name }),
          items: (id: string) => {
            const start = dmCallStart(voiceCtx, id);
            return [...(start === null ? [] : [{ id: 'call', label: start.label }]), { id: 'pins', label: t('shell.pins.open') }, ...notifyItems(id)];
          },
          onSelect: (id: string, item: string) => {
            if (notifyChoice(id, item)) return;
            if (item === 'call') {
              dmCallStart(voiceCtx, id)?.onStart();
              if (id !== dmId) void navigate({ name: 'dm', channelId: id });
              return;
            }
            if (id === openId) { dispatch({ type: 'pins', open: true }); return; }
            pinsFor.current = id;
            void navigate({ name: 'dm', channelId: id });
          },
        } } : serverId === null ? {} : { rowMenu: {
          label: (name: string) => t('nav.channel.menuOf', { name }),
          // Fix round 1 (Jonas): a voice row's menu holds the call code while this device is in its call (the stage has none).
          items: (id: string) => voiceRooms[id] !== undefined ? (callCodeOf(id) === null ? [] : [{ id: 'code', label: t('voice.code') }])
            : [{ id: 'pins', label: t('shell.pins.open') }, ...notifyItems(id), ...(canManageChannels ? [{ id: 'edit', label: t('admin.channel.open') }] : [])],
          onSelect: (id: string, item: string) => {
            if (notifyChoice(id, item)) return;
            if (item === 'code') { setCodeFor(id); return; }
            if (item === 'edit') { setEditChannelId(id); return; }
            if (id === openId) { dispatch({ type: 'pins', open: true }); return; }
            pinsFor.current = id;
            void navigate({ name: 'channel', communityId: serverId, channelId: id });
          },
        } })}
        // Signal §5.4: the unread tag reads "{n} new"; the stat line under the server name counts who is online and who is in voice.
        newLabel={n => t('layout.row.new', { n })}
        // Ruling W: online counts people, not bots; before presence has loaded the line holds the in-voice count alone, no dot.
        {...(onDms || communityId === null ? {} : { stats: [
          ...(members == null || presence == null ? [] : [{ live: true, text: t('layout.sidebar.online', {
            n: members.filter(m => m.kind !== 1 && (presence[m.userId] ?? 'offline') !== 'offline').length }) }]),
          { text: t('voice.count', { n: inVoiceHere }) },
        ] })}
        content={content} />
      {/* Ruling AE (Jonas 2026-10-27): the phone's home lists end in the prototype's user panel, above the tab bar, as the desktop's
          sidebar does (the same panel, the same data). */}
      {phone && self !== null ? <UserPanel name={selfName(self, allMembers)} username={self.username} choice={account?.presenceChoice ?? 'online'} call={call} statusMsg={self.statusMsg} inRoom={selfRoom}
        ptt={settings?.['voice.mode'] === 'ptt' ? { held: call.state?.speaking === true } : false} /> : null}
    </div>);
  }

  // A validated Remove of a changed leaf unblocked sending here only; the contact stays changed elsewhere (Flow 05).
  const removedHere = targetId !== null && (trust ?? []).some(row => isChanged(row) && row.removedIn?.includes(targetId) === true);
  // Flow 05 "changed": a member of this conversation whose identity changed stands as an alert with no dismiss, until the
  // new key is verified or a validated Remove restored sending here. Names come from the worker's trust slice only.
  const audience = new Set(channel !== null ? (members ?? []).map(member => member.userId) : dm?.members ?? []);
  const changedMembers = targetId === null ? [] : (trust ?? []).filter(row => isChanged(row) && audience.has(row.userId)
    && row.removedIn?.includes(targetId) !== true);
  // L-CORE-40, W-01: the core's send gate of this conversation (sendBlocks) blocks the composer until the key is verified or
  // a validated Remove; the composer says why instead of letting a send fail. Before the worker first publishes the gate,
  // a changed member of this conversation blocks it (fail closed). A trust refusal of a send stands until the next gate.
  // A refused send or the fail-closed guess does not say whose leaf holds the block: it is the own account's only when no
  // changed contact is in the conversation and the worker reports an own key under another identity.
  const guess = (code: TrustBlock): SendBlock =>
    ({ code, own: code === 'E_UMK_CHANGED' && changedMembers.length === 0 && account?.ownTrust === 'changed' });
  const gate = sendBlocks === undefined ? (changedMembers.length > 0 ? guess('E_UMK_CHANGED') : undefined)
    : targetId === null ? undefined : sendBlocks[targetId];
  const refused = refusedSend !== null && refusedSend.channelId === targetId && refusedSend.gate === sendBlocks ? guess(refusedSend.code) : undefined;
  const block = gate ?? refused;
  const timeoutReason = channel?.kind === 0 && selfTimeout?.communityId === communityId
    ? t('shell.composer.timedOut', { time: formatTime(selfTimeout.expires) }) : null;
  const blockReasonKey = composerBlock(group) ?? (block !== undefined ? blockReason(block, changedMembers) : null);
  const reason: string | null = openError !== null ? t('shell.composer.openFailed') : timeoutReason ?? (blockReasonKey === null ? null : t(blockReasonKey));
  const composerLabel = channel !== null ? t('shell.composer.label', { channel: channel.name })
    : dm !== null ? t('shell.dm.composer', { name: dm.name }) : null;
  const composerPlaceholder = channel !== null ? t('shell.composer.placeholder', { channel: channel.name })
    : dm !== null ? t('shell.dm.placeholder', { name: dm.name }) : null;
  const composer = targetId === null || composerLabel === null || composerPlaceholder === null ? null : (
    <ComposerArea key={targetId} channelId={targetId} label={composerLabel} placeholder={composerPlaceholder}
      blocked={reason} broadcast={broadcast} bits={bits} roles={roles} channelName={channel?.name}
      encodeWith={encodeWith} candidates={candidates}
      items={timeline?.items ?? []} book={book} draft={ordinaryDrafts.current.get(targetId) ?? (draft.channelId === targetId ? draft.text : '')}
      onDraft={text => { rememberDraft(ordinaryDrafts.current, targetId, text); setDraft({ channelId: targetId, text, typed: true }); }} ui={ui} dispatch={dispatch} onSend={send} onError={report}
      {...(slow !== undefined ? { slow } : {})} bypass={bits !== null && (bits & PERM.bypassSlowmode) !== 0}
      announcement={channel !== null && isAnnouncement(channel)} announce={announce}
      typing={typingNames.length > 0 && !searchOpen ? <TypingIndicator names={typingNames} labels={{ one: en['typing.one'], many: t('typing.many') }} /> : null} />
  );

  // The pane belongs to a server shown by a channel route; a DM route, voice stage or Settings has none beside it.
  const channelAside = base.name === 'channel' && community !== null && channel !== null && voiceChannel === null;
  const sideNow = channelAside && side !== null && side.channelId === channel.id ? side : null;
  const membersShown = membersOpen && channelAside && sideNow === null;
  const asideShown = membersShown || sideNow !== null;
  // On a phone the member list is a sheet: a history entry, so the system back closes it and not the channel under it (R13).
  const memberSheet = usePhoneSheet('members', phone && membersShown, () => setMembersOpen(false));
  // A thread (or the list of threads) is a sheet of its own on a phone, with the same history rules.
  const threadSheet = usePhoneSheet('thread', phone && sideNow !== null, () => setSide(null));
  const closeSide = phone ? threadSheet.close : () => setSide(null);
  // Task 9 fix rounds 1-2 (Jonas): the member menu's Mention puts "@username " at the end of the open text channel's draft and
  // focuses the composer. Only while that composer can send (no timeout, block or failed open). An aside drawn over the
  // conversation (a phone's sheet, a narrow desktop's drawer) closes first; the focus waits in `mentionFocus` until the
  // aside is gone and the composer is no longer inert.
  const mentionFromMembers = channel !== null && channel.kind === 0 && voiceChannel === null && targetId !== null && reason === null ? (username: string) => {
    const id = targetId;
    const base = draft.channelId === id ? draft.text : ordinaryDrafts.current.get(id) ?? '';
    const text = `${base}${base === '' || /\s$/.test(base) ? '' : ' '}@${username} `;
    rememberDraft(ordinaryDrafts.current, id, text);
    setDraft({ channelId: id, text, typed: true });
    mentionFocus.current = true;
    if (phone) memberSheet.close();
    else if (document.querySelector('.d-app-shell__backdrop') !== null) setMembersOpen(false);
    setMentionTick(n => n + 1);
  } : undefined;
  // Where focus goes when the sheet or drawer closes: the control that opened it (kept from the first open, not from a list row),
  // else the root's message row, else the header's button.
  const sideOpener = useRef<Element | null>(null);
  const sideRoot = useRef<string | null>(null);
  // A pointer open leaves focus where it was (the composer, say): the message row under the pointer is the opener then.
  const lastPointer = useRef<{ row: Element | null; at: number } | null>(null);
  useEffect(() => {
    const note = (e: PointerEvent) => { lastPointer.current = { row: e.target instanceof Element ? e.target.closest('.d-message-row') : null, at: Date.now() }; };
    document.addEventListener('pointerdown', note, true);
    return () => document.removeEventListener('pointerdown', note, true);
  }, []);
  const noteOpener = () => {
    if (side !== null && side.channelId === channelId) return;
    const pointer = lastPointer.current;
    sideOpener.current = pointer !== null && pointer.row !== null && Date.now() - pointer.at < 1500 ? pointer.row : document.activeElement;
    sideRoot.current = null;
  };
  function openThread(rootMsgId: string): void {
    if (channel === null) return;
    noteOpener();
    sideRoot.current = rootMsgId;
    setSide({ channelId: channel.id, view: 'thread', rootMsgId });
  }
  openThreadRef.current = openThread;
  // FX-core (final review I1): a notice of a thread reply (an Activity row, a notification click) opens its thread once its
  // channel is the open one, as a search hit does; a request whose channel is no longer the route's is dropped.
  const threadRequest = useThreadRequest();
  useEffect(() => {
    if (threadRequest === null) return;
    if (channelId !== threadRequest.channelId) { takeThreadRequest(); return; }
    if (channel === null) return;
    takeThreadRequest();
    sideOpener.current = null;
    sideRoot.current = threadRequest.rootMsgId;
    if (phone) setMembersOpen(false);
    setSide({ channelId: channel.id, view: 'thread', rootMsgId: threadRequest.rootMsgId });
  }, [threadRequest, channelId, channel, phone]);
  function openList(): void {
    if (channel === null) return;
    noteOpener();
    setSide({ channelId: channel.id, view: 'list' });
  }
  const sideReturn = useMemo<RefObject<HTMLElement | null>>(() => ({
    get current() {
      const opener = sideOpener.current;
      if (opener instanceof HTMLElement && opener.isConnected && opener !== document.body) return opener;
      const row = sideRoot.current === null ? null : document.querySelector<HTMLElement>(`.d-message-row[data-msg-id="${sideRoot.current}"]`);
      return row ?? document.querySelector<HTMLElement>('[data-thread-list-toggle], [data-members-toggle], [data-focus-key="channel-menu"]');
    },
  }), []);
  // The list ends on Escape from wherever focus is (the pane is no drawer in a wide shell), and focus goes back to its button.
  const listOpen = sideNow?.view === 'list';
  useEffect(() => {
    if (!listOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented) return;
      e.preventDefault();
      closeSideRef.current();
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [listOpen]);
  const closeSideRef = useRef(closeSide);
  closeSideRef.current = closeSide;
  // Opening the list puts focus in it; closing it hands focus back to the control that opened it.
  // A swap from the list to a thread is no close: the thread takes focus itself.
  const listWas = useRef(false);
  const sideClosed = sideNow === null;
  useEffect(() => {
    const was = listWas.current;
    listWas.current = listOpen;
    if (listOpen) document.querySelector<HTMLElement>('.d-app-shell__aside')?.focus();
    else if (was && sideClosed && (focusLost() || document.activeElement?.closest('.d-app-shell__aside') != null)) sideReturn.current?.focus();
  }, [listOpen, sideClosed, sideReturn]);
  // Back on a phone: the history's back for a view the app pushed, else the tab's list (a deep link, a reload).
  const listRoute: Route = dmId !== null ? { name: 'dms' } : communityId !== null ? { name: 'channel', communityId, channelId: null } : { name: 'root' };
  const onBack = () => goBack(listRoute, navigate);
  // H3: while typing, the call bar steps aside and the header's dot says the call is still up (named as the bar's open button).
  const callBarUp = phone && call.channelId !== null && !(phonePane === 'view' && call.channelId === (voiceChannel?.id ?? dm?.id));
  // The dot is the call bar's open button, folded to 44 x 44: the same name, the same target.
  const dot = callBarUp && keyTyping && call.channelId !== null
    ? { label: t('nav.call.open', { where: callWhere(call.channelId) }), onOpen: () => { if (callTarget !== null) void navigate(callTarget); } } : undefined;
  const dotProp = dot === undefined ? {} : { dot };
  // The DM topic (Ruling Z): "@handle · status" for a one-to-one DM (the status message, when the person has one), "group DM ·
  // Ada, Björn and you" for a group.
  const dmTopic = (d: DmSummary): string | undefined => {
    const others = d.members.filter(id => id !== self?.id).map(id => allMembers.find(m => m.userId === id)).filter(m => m !== undefined);
    if (d.members.length > 2) {
      const names = new Intl.ListFormat('en', { type: 'conjunction' }).format([...others.map(m => m.display || m.username), t('shell.dm.you')]);
      return t('shell.dm.topicGroup', { names });
    }
    const other = others[0];
    if (other === undefined) return undefined;
    return other.statusMsg ? t('shell.dm.topic', { handle: other.username, status: other.statusMsg }) : t('shell.dm.topicHandle', { handle: other.username });
  };
  // E2: the voice stage's phone header carries search too (a touch device has no Control+K).
  // Signal §5.5, §5.10 (Ruling U): a channel's header has the prototype's controls. Threads (its icon and label; an icon alone on a
  // phone) and Members (its icon and the member count, named "Members, {n}", the number shown once the list has loaded); a phone
  // adds search. There is no "⋯" (Jonas 2026-10-27, Ruling X): pins and edit channel are in the channel row's context menu,
  // search is the top bar's (and Control/⌘+K) on a desktop, the reminders inbox the top bar's bell or the phone's Activity tab.
  const memberCount = members == null ? null : members.length;
  const membersName = memberCount === null ? t('layout.header.members') : t('layout.header.membersCount', { n: memberCount });
  // Task 11 (the prototype's thrBg): Threads is pressed while the aside shows a thread or the thread list; a press closes it.
  const threadsOn = sideNow !== null;
  const toggleThreads = () => (threadsOn ? closeSide() : openList());
  const membersButton = (
    <Tooltip label={t('layout.header.members')}>
      <Button variant="ghost" mark={false} data-members-toggle="" aria-label={membersName} pressed={membersShown} onClick={toggleMembers}>
        <Icon name={MEMBERS_GLYPH} size={14} />{memberCount === null ? null : <span aria-hidden="true">{memberCount}</span>}
      </Button>
    </Tooltip>
  );
  // Ruling Z (Jonas 2026-10-27): every conversation header is the prototype's one template: back (phone), glyph and name, topic;
  // a chat (a text channel or a DM) adds Threads (labelled on a desktop) when it has threads, Members with the count on a server,
  // and Search on a phone. A voice channel has no controls. The DM call and pins are in the DM row's context menu (and the call
  // under Control/⌘+Shift+Enter), search is the top bar's and Control/⌘+K, the reminders the bell and the Activity tab.
  const voiceTopic = voiceChannel === null ? undefined : call.channelId === voiceChannel.id
    ? t('voice.count', { n: voiceCtx.voice?.[voiceChannel.id]?.participants.length ?? 0 }) : t('voice.header.idle');
  const header = voiceChannel !== null ? (phone ? <ChannelHeader kind="voice" name={voiceChannel.name} topic={voiceTopic}
    phone={{ backLabel: t('nav.back'), onBack, ...dotProp }} /> : null) : channel !== null
    ? <ChannelHeader name={channel.name} kind={isAnnouncement(channel) ? 'announcement' : 'channel'} topic={channel.topic === '' ? undefined : channel.topic}
      readable={channel.mode === 1} readableLabel={t('shell.readable')}
      phone={phone ? { backLabel: t('nav.back'), onBack, ...dotProp, menu: (<>
        {/* The prototype's phone header: Threads as its 14px icon alone, Members, a 15px search, at its sizes (PhoneHeader.css). */}
        {openId !== null ? <Button variant="ghost" mark={false} data-thread-list-toggle="" aria-label={t('layout.header.threads')} pressed={threadsOn}
          onClick={toggleThreads}><Icon name={THREADS_GLYPH} size={14} /></Button> : null}
        {membersButton}
        <IconButton icon="search" size={28} label={t('search.open')} onClick={e => startSearch(e.currentTarget)} />
      </>) } : undefined}
      actions={<>
        {openId !== null ? <Button variant="ghost" mark={false} data-thread-list-toggle="" pressed={threadsOn} onClick={toggleThreads}>
          <Icon name={THREADS_GLYPH} size={14} />{t('layout.header.threads')}</Button> : null}
        {membersButton}
      </>} />
    // A DM has no threads in this client (threads are a server channel's), so its header carries no Threads button.
    : dm !== null ? <ChannelHeader kind="dm" name={dm.name} topic={dmTopic(dm)}
      phone={phone ? { backLabel: t('nav.back'), onBack, ...dotProp, menu: (
        <IconButton icon="search" size={28} label={t('search.open')} onClick={e => startSearch(e.currentTarget)} />
      ) } : undefined} /> : null;
  // While Settings covers the conversation, its Privacy section carries the alert and the action instead.
  const changedHere = overlay ? [] : changedMembers;

  // Signal task 10 fix round 1: settings are searched from ⌘K (the merge of lanes 9 and 10: a provider of lane 9's palette).
  // The user's sections, then the selected server's allowed ones; picking one opens its section (anchors are not focused).
  const openSetting = (scope: 'user' | 'server', section: string): boolean => {
    leaveSearchLayer();
    searchOpener.current = null;
    if (scope === 'server' && communityId !== null) void navigate({ name: 'serverSettings', communityId, section: section as AdminTab, from: null });
    else void navigate({ name: 'settings', section: section as SettingsSection, from: null });
    return true;
  };
  const settingsPalette = settingsProvider(query => [
    ...searchSettings('user', query).map(r => ({ id: `user|${r.section.id}|${r.anchor ?? ''}`, label: r.label, breadcrumb: r.breadcrumb,
      run: () => openSetting('user', r.section.id) })),
    ...(communityId === null || access?.loaded !== true ? [] : searchSettings('server', query, access)
      .map(r => ({ id: `server|${r.section.id}|${r.anchor ?? ''}`, label: r.label,
        breadcrumb: `${community?.name ?? t(r.section.group)} / ${t(r.section.labelKey)}`, run: () => openSetting('server', r.section.id) }))),
  ]);
  const searchHits: SearchHitView[] = (search?.hits ?? []).map(hit => {
    const location = hit.communityId === null ? dms?.find(row => row.id === hit.channelId)?.name
      : (railSlices.flat() as ChannelSummary[]).find(row => row.id === hit.channelId)?.name;
    return { ...hit, id: `${hit.channelId}:${hit.seq}`, location: location ?? hit.channelId };
  });
  // Task 9 fix round 1 (Jonas): the palette's groups, as the prototype: Channels, People, Messages, Actions. The message search
  // runs everywhere; the open conversation's hits come first. Another group joins by adding a provider here.
  const paletteNow = Date.now();
  const paletteProviders: PaletteProvider[] = [
    channelsProvider(communities ?? [], railSlices.flat().filter((c): c is ChannelSummary => c != null), afkChannels, c => {
      void navigate({ name: 'channel', communityId: c.communityId, channelId: c.id });
      leaveSearchLayer();
      searchOpener.current = null;
      return true;
    }),
    peopleProvider(dms ?? [], d => (d.kind === 4 ? t('search.palette.groupDm', { count: d.members.length }) : dmTopic(d) ?? ''), d => {
      void navigate({ name: 'dm', channelId: d.id });
      leaveSearchLayer();
      searchOpener.current = null;
      return true;
    }),
    settingsPalette,
    messagesProvider((searchRequest === (search?.requestId ?? 0) ? searchHits : []).map(hit => ({
      id: hit.id, snippet: hit.snippet, location: hit.communityId === null ? hit.location : `#${hit.location}`, current: hit.channelId === targetId,
      ...(hit.author === undefined ? {} : { author: hit.author }), ...(hit.ts === undefined ? {} : { time: messageTime(hit.ts, paletteNow) }),
      run: async () => { const opened = await pickSearchHit(hit); if (opened) searchOpener.current = null; return opened; },
    }))),
    actionsProvider([
      { id: 'settings', label: t('search.action.settings'), sub: t('search.action.settingsSub'), icon: 'settings', keywords: 'profile devices voice',
        run: () => { void navigate({ name: 'settings', section: 'profile', from: null }); return true; } },
      { id: 'mute', label: t(call.selfMuted ? 'search.action.unmute' : 'search.action.mute'), sub: isApplePlatform() ? '⌘⇧M' : 'Ctrl+Shift+M',
        icon: call.selfMuted ? 'mic' : 'mic-off', keywords: 'microphone', run: () => { call.selfMute(); return true; } },
      { id: 'dm', label: t('search.action.dm'), sub: t('search.action.dmSub'), icon: 'message-square', keywords: 'direct message dm',
        // The DM picker takes focus: the palette's close must not hand it back to the opener (fix round 2).
        run: () => { searchOpener.current = null; setPickerOpen(true); return true; } },
    ]),
  ];
  const pickSearchHit = async (hit: SearchHitView): Promise<boolean> => {
    const known = hit.communityId === null ? dms?.some(row => row.id === hit.channelId)
      : (railSlices.flat() as ChannelSummary[]).some(row => row.id === hit.channelId && row.communityId === hit.communityId);
    if (!known) return false;
    const threadRoot = (hit as SearchHitView & { threadRoot?: string | null }).threadRoot ?? null;
    if (threadRoot !== null && hit.communityId !== null) {
      // A reply shows in its thread, not in the channel's timeline: the thread opens beside the channel.
      void navigate({ name: 'channel', communityId: hit.communityId, channelId: hit.channelId });
      leaveSearchLayer();
      // The palette is gone, so the root's row is where focus goes back to; a phone's members sheet gives way to the thread.
      sideOpener.current = null;
      sideRoot.current = threadRoot;
      if (phone) setMembersOpen(false);
      setSide({ channelId: hit.channelId, view: 'thread', rootMsgId: threadRoot });
      return true;
    }
    const name = `timeline:${hit.channelId}` as const;
    const available = () => client.get(name)?.items.some(item => item.seq === hit.seq && item.state === 'ok') === true;
    void navigate(hit.communityId === null ? { name: 'dm', channelId: hit.channelId }
      : { name: 'channel', communityId: hit.communityId, channelId: hit.channelId });
    if (client.get(name) === undefined) {
      await new Promise<void>(resolve => {
        const timeout = setTimeout(() => { stop(); resolve(); }, 1000);
        const stop = client.subscribe(name, () => {
          const current = client.get(name);
          if (current === undefined) return;
          stop();
          clearTimeout(timeout);
          resolve();
        });
      });
    }
    if (client.get(name) === undefined) return false;
    while (!available() && client.get(name)?.hasEarlier) {
      const previous = client.get(name);
      await client.call({ m: 'loadEarlier', channelId: hit.channelId });
      if (client.get(name) === previous) break;
    }
    if (!available()) return false;
    await new Promise<void>(resolve => {
      const focus = () => {
        const row = document.querySelector<HTMLElement>(`.dw-conversation[data-channel-id="${hit.channelId}"] .d-message-row[data-seq="${hit.seq}"]`);
        if (!row) return false;
        leaveSearchLayer();
        row.focus();
        row.scrollIntoView?.({ block: 'center' });
        resolve();
        return true;
      };
      if (focus()) return;
      const observer = new MutationObserver(() => { if (focus()) { observer.disconnect(); clearTimeout(timeout); } });
      observer.observe(document.body, { childList: true, subtree: true });
      const timeout = setTimeout(() => {
        observer.disconnect();
        const heading = document.querySelector<HTMLElement>('.d-channel-header__name');
        leaveSearchLayer();
        heading?.setAttribute('tabindex', '-1');
        heading?.focus();
        resolve();
      }, 1000);
    });
    return true;
  };

  const pickerMembers = dmCandidates(members, self?.id ?? null);
  // Walk 7: the gateway reports a drop only after its own timeout (about 17 s); the browser knows at once. While the browser
  // says it is offline the link is offline; the gateway status still governs everything else (reconnecting, a version refusal).
  const status = !browserOnline ? 'offline' : connection?.status ?? 'connecting';
  const linkReason = connection?.status === 'offline' ? connection.reason : undefined;
  const offline = status === 'offline';
  // BACKUPS-RECOVERY-04: persistent, with no dismiss: it stands while the worker reports the mismatch.
  const rootMismatch = account?.rootMismatch === true;
  // Privacy lists no own row; what the core reports about the own key stands here, persistent, while the worker reports it.
  const ownTrust = account?.ownTrust ?? null;
  const removalNotices = (removals ?? []).filter(r => !dismissedRemovals.has(r.id));
  const banner = (offline && linkReason === 'version') || removalNotices.length > 0 || commandError !== null || rootMismatch || ownTrust !== null || removedHere || changedHere.length > 0 ? (
    <>
      {rootMismatch ? (
        <Banner tone="danger">{t('devices.rootMismatch', { instance: account.instance?.name ?? t('shell.status.unknown') })}</Banner>
      ) : null}
      {ownTrust !== null ? (
        <Banner tone="danger" action={{ label: t('privacy.ownAction'), onAction: () => void navigate({ name: 'settings', section: 'devices', from: null }) }}>
          {t(ownTrust === 'changed' ? 'privacy.ownChanged' : 'privacy.ownUnlisted')}
        </Banner>
      ) : null}
      {offline && linkReason === 'version' ? (
        <Banner tone="warn" action={{ label: t('reload.action'), onAction: () => {
          // The update effect checkpoints the draft, route and files, then reloads; with no update flow or storage it is a plain reload.
          if (update === undefined || updateScope === null) browser.reload();
          else setUpdateEntry(update.currentEntry);
        } }}>{t('shell.banner.clientTooOld')}</Banner>
      ) : null}
      {removalNotices.map(r => (
        <Banner key={r.id} tone="warn" action={{ label: t('shell.banner.dismiss'), onAction: () => {
          focusComposer();
          setDismissedRemovals(done => new Set([...done, r.id]));
        } }}>{t('removed.notice', { server: r.name })}</Banner>
      ))}
      {removedHere ? <p className="dw-trust-removed" role="status">{t('privacy.removed')}</p> : null}
      {changedHere.length > 0 ? <div className="dw-trust-alerts" data-doc-state="trust-conversation">{changedHere.map(row => <TrustAlert key={row.userId} kind="changed" name={row.name}
        labels={{ changed: t(row.changedUmk === null ? 'privacy.changedNoKey' : 'privacy.changed'), unlisted: t('privacy.unlisted'),
          verify: t('privacy.compare', { name: row.name }) }}
        onVerify={row.changedUmk === null ? undefined : () => void navigate({ name: 'settings', section: 'privacy', from: null })} />)}</div> : null}
      {commandError !== null ? (
        <Banner tone="danger" action={{ label: t('shell.banner.dismiss'), onAction: () => { focusComposer(); setCommandError(null); } }}>
          {TRUST_BLOCKS.has(commandError.code)
            ? t(blockReason(gate?.code === commandError.code ? gate : guess(commandError.code as TrustBlock), changedMembers))
            : t('shell.banner.commandError', { code: commandError.code })}
        </Banner>
      ) : null}
    </>
  ) : undefined;

  // The tabs (flow 12): a tab opens where it was last left, else at its root; the counts are what the sidebar's badges sum to.
  const serverRoot: Route = (routeCommunity ?? fallbackCommunity) !== null
    ? { name: 'channel', communityId: (routeCommunity ?? fallbackCommunity) as string, channelId: null } : { name: 'root' };
  // Home's root is the DM list while the `@` tile is selected (a DM is part of it), else the server's channel list.
  const tabRoot = (to: PhoneTab): Route => to === 'home' ? (homeSelected ? { name: 'dms' } : serverRoot) : { name: to };
  const tabTarget = (to: PhoneTab): Route => {
    const kept = rememberedTab(to);
    if (kept !== null) {
      const remembered = routeOf(kept);
      if (tabOf(remembered) === to && remembered.name !== 'root') return remembered;
    }
    return tabRoot(to);
  };
  // Home's badge is what the `@` tile carries: unread direct messages (servers carry theirs on the rail).
  const homeCount = dmCounts.unread;
  const activityCount = railItems.reduce((n, item) => n + item.mentions, 0) + dmCounts.unread;
  const tabLabel = (to: PhoneTab) => t(to === 'home' ? 'home.tab' : `nav.tab.${to}`);
  const tabItem = (to: PhoneTab, icon: 'house' | 'bell' | 'user', count?: number) => ({
    id: to, label: tabLabel(to), href: routePath(tabTarget(to)), icon, current: phoneTab === to,
    ...(count !== undefined && count > 0 ? { count, countLabel: t('nav.tab.unread', { name: tabLabel(to), count }) } : {}),
  });
  // The tab to open once an open sheet's entry has been popped (set while that pop is pending, so a second tap only retargets it).
  const pendingTab = useRef<{ to: PhoneTab; current: boolean } | null>(null);
  // The listener goBack leaves for the pop (N5); a newer tap or the shell going away removes it.
  const tabBackOff = useRef<() => void>(() => undefined);
  useEffect(() => () => tabBackOff.current(), []);
  const openTab = (to: PhoneTab, current: boolean) => {
    tabBackOff.current();
    // The current tab in its pushed view goes to its list (the iOS convention): back when the app pushed the view, else a replace after a
    // deep link. Another tab opens where it was left, replacing the entry.
    if (current) {
      // Back only when the view was pushed from this tab's own list; from anywhere else (Activity, another tab) back would land there.
      const from = pushedFrom();
      const fromList = from !== null && tabOf(from) === to && from.name !== 'dm' && from.name !== 'settings' && !(from.name === 'channel' && from.channelId !== null);
      if (fromList) tabBackOff.current = goBack(tabRoot(to), navigate, tabRoot(to));
      else void navigate(tabRoot(to), true);
    } else void navigate(tabTarget(to), true);
  };
  const tabBar = !phone || overlay ? undefined : (
    <TabBar label={t('nav.tabs.label')} onSelect={id => {
      const to = id as PhoneTab;
      const current = to === phoneTab && phonePane === 'view';
      // Server settings is a page above the tabs: leaving by a tab closes it.
      if (pendingTab.current !== null) { pendingTab.current = { to, current }; return; }
      // An open sheet closes first. Its entry is popped (async), and only then does the tab open on the view's entry.
      if (asideShown && (sideNow !== null ? threadSheet : memberSheet).close()) {
        pendingTab.current = { to, current };
        const off = subscribeCommittedTraversal(() => {
          off();
          const next = pendingTab.current;
          pendingTab.current = null;
          if (next !== null) openTab(next.to, next.current);
        });
      } else openTab(to, current);
    }} items={[
      tabItem('home', 'house', homeCount), tabItem('activity', 'bell', activityCount), tabItem('you', 'user'),
    ]} />
  );
  // The call bar stands on a phone while this device is in a call and its stage is not on screen.
  const callTarget = call.channelId === null ? null : dms?.some(d => d.id === call.channelId) === true
    ? { name: 'dm', channelId: call.channelId } as const
    : (() => {
      const home = (railSlices.flat() as ChannelSummary[]).find(c => c.id === call.channelId)?.communityId;
      return home === undefined ? null : { name: 'channel', communityId: home, channelId: call.channelId } as const;
    })();
  // The dock's second line names the call's channel and its server; a DM call has no server (its `where` already reads "the call with {name}").
  const dockServer = call.channelId === null || dms?.some(d => d.id === call.channelId) === true ? null
    : (communities ?? []).find(c => c.id === (railSlices.flat() as ChannelSummary[]).find(ch => ch.id === call.channelId)?.communityId)?.name ?? null;
  // H3: not rendered while typing; the header's dot stands for it.
  const callBar = !callBarUp || keyTyping || call.channelId === null ? undefined : (
    <CallBar phase={callPhase(voiceCtx)} where={whereOf(call.channelId)} muted={call.state?.muted ?? false}
      labels={{ open: t('nav.call.open', { where: callWhere(call.channelId) }), mute: t('voice.mute'), leave: t('voice.leave'), region: t('voice.dock.region') }}
      onOpen={() => { if (callTarget !== null) void navigate(callTarget); }} onMute={call.toggleMute} onLeave={() => call.leave()} />
  );
  // Signal top bar (README §5.2), desktop only: search opens the palette as Control/⌘+K does, the bell the reminders inbox, the
  // gear Settings, and the voice chip (while connected) the call's stage.
  // The bell the reminders popover hangs under, and gives focus back to (README §6.7).
  const bellOf = () => document.querySelector<HTMLElement>('.d-topbar__bell .d-icon-button');
  const topBar = phone ? undefined : (
    <TopBar searchLabel={t('topbar.search')} searchShortcut={isApplePlatform() ? '⌘K' : 'Ctrl K'} onSearch={() => startSearch()}
      voice={call.channelId !== null && callPhase(voiceCtx) === 'connected'
        ? { room: whereOf(call.channelId), pingMs: call.state?.link?.rttMs ?? null, onOpen: () => { if (callTarget !== null) void navigate(callTarget); } }
        : undefined}
      remindersDue={utilityCommands.dueCount} remindersOpen={utilityCommands.remindersOpen}
      onReminders={() => (utilityCommands.remindersOpen ? utilityCommands.closeInbox() : utilityCommands.openInbox(bellOf()))}
      reminders={utilityCommands.remindersPanel('popover', bellOf)}
      onSettings={() => void navigate({ name: 'settings', section: null, from: null })}
      labels={{ reminders: t('reminder.inbox'), settings: t('workspace.open'), remindersDue: count => t('reminder.badge', { count }),
        voice: call.channelId === null ? '' : t('nav.call.open', { where: callWhere(call.channelId) }) }} />
  );
  // A phone remembers where each tab was left (never a settings route, nor the root that only redirects).
  useEffect(() => {
    if (phone && route.name !== 'settings' && route.name !== 'serverSettings' && route.name !== 'root' && route.name !== 'welcome') rememberTab(tabOf(route), routePath(route));
  }, [phone, route]);
  // Focus follows the pane (flow 12): a pushed view's heading when focus was lost with the list; the opening row of Settings, else
  // the list's heading, when a pane or a tab comes back. Never on the first render.
  const paneKey = `${phonePane}:${phoneTab}`;
  const lastPane = useRef<string | null>(null);
  // The id of the conversation the view showed, so back lands on its row.
  const openedId = useRef<string | null>(null);
  if (phone && phonePane === 'view' && targetId !== null) openedId.current = targetId;
  useLayoutEffect(() => {
    const before = lastPane.current;
    lastPane.current = paneKey;
    if (!phone || before === null || before === paneKey) return;
    if (phonePane === 'view') {
      const title = document.querySelector<HTMLElement>('.d-app-shell__main .d-phone-header__title');
      if (focusLost() && title !== null) { title.tabIndex = -1; title.focus(); }
      return;
    }
    const opened = openedId.current;
    // The list's rows (voice rows among them, web3.8) are in the order of their ids; the row is found by position, never by its text.
    const rowIds = homeSelected ? (dms ?? []).map(d => d.id) : ordered.map(c => c.id);
    const at = opened === null ? -1 : rowIds.indexOf(opened);
    const row = openSettings.current !== null ? document.querySelector<HTMLElement>(`[data-settings-section="${openSettings.current}"]`)
      : at < 0 ? null : document.querySelectorAll<HTMLElement>('.d-app-shell__sidebar .d-channel-list__list > li > :first-child')[at] ?? null;
    openedId.current = null;
    openSettings.current = null;
    const target = row ?? document.querySelector<HTMLElement>('.d-app-shell__sidebar h1, .d-app-shell__sidebar .d-phone-header__title, .d-app-shell__sidebar .d-empty-state h2, .d-app-shell__sidebar [data-tab-heading]');
    if (target === null) return;
    target.tabIndex = row === null ? -1 : target.tabIndex;
    target.focus();
  }, [paneKey, phone, phonePane, phoneTab, homeSelected, dms, ordered]);
  const [updateEntry, setUpdateEntry] = useState<string | null>(null);
  const [threadEditing, setThreadEditing] = useState(false);
  const recentUpdateCall = call.recentlyEnded !== null && Date.now() - call.recentlyEnded.at < 15_000
    && (railSlices.flat() as ChannelSummary[]).some(c => c.id === call.recentlyEnded?.channelId && c.kind === 1)
    ? call.recentlyEnded : null;
  // These dialogs own form drafts in their own components. Apply the update after the person finishes the form.
  const updateBlocked = overlay || joinOpen || pickerOpen || createOpen || channelOpen
    || editChannelId !== null || adminFor !== null || ui.editing !== null || threadEditing;
  const updateCallBlocked = call.channelId !== null || call.recoveryChannelId !== null;
  const updateVoiceActive = useRef(false);
  updateVoiceActive.current = call.channelId !== null || call.recoveryChannelId !== null || recentUpdateCall !== null;
  const [updateError, setUpdateError] = useState(false);
  const [updateRetry, setUpdateRetry] = useState(0);
  const [restoreError, setRestoreError] = useState(false);
  const [restoreRetry, setRestoreRetry] = useState(0);
  const [restoreDone, setRestoreDone] = useState(props.update === undefined);
  const restoreApplied = useRef(false);
  const [pendingUi, setPendingUi] = useState<{ channelId: string; ui: ConversationUi } | null>(null);
  const [pendingView, setPendingView] = useState<ViewPlace | null>(null);
  const viewLoadEarlier = useRef(false);
  const viewLoadAttempts = useRef(0);
  const [pendingCamera, setPendingCamera] = useState<string | null>(null);
  const [sharingStopped, setSharingStopped] = useState(false);
  useEffect(() => { if (call.recoverySharing) setSharingStopped(true); }, [call.recoverySharing]);
  const updating = useRef(false);
  const update = props.update;
  const updateScope = useMemo(() => {
    if (update === undefined || account?.instance?.id === undefined || account.user?.id === undefined) return null;
    try {
      // eslint-disable-next-line no-restricted-globals -- only a random content-free tab identifier enters Web Storage
      const tab = updateTabId(sessionStorage, crypto);
      return { instance: account.instance.id, user: account.user.id, tab };
    }
    catch { return null; }
  }, [update, account?.instance?.id, account?.user?.id]);
  useEffect(() => {
    if (update === undefined || updateScope === null) { setRestoreDone(true); return; }
    let active = true;
    void update.store.load(updateScope).then(async restored => {
      if (!active) return;
      if (restored !== null && !restoreApplied.current && typeof restored.state === 'object' && restored.state !== null) {
        const saved = restored.state as { draft?: { channelId?: unknown; text?: unknown }; call?: { channelId?: unknown; camera?: unknown; sharing?: unknown } | null;
          conversationId?: unknown;
          side?: Side | null; threadDraft?: { channelId?: unknown; rootMsgId?: unknown; text?: unknown } | null;
          membersOpen?: unknown; tab?: unknown; ui?: unknown; view?: unknown;
          search?: { open?: unknown; query?: unknown; scope?: unknown; id?: unknown } };
        if (saved.draft !== undefined && (saved.draft.channelId === null || typeof saved.draft.channelId === 'string')
          && typeof saved.draft.text === 'string') {
          setDraft({ channelId: saved.draft.channelId, text: saved.draft.text });
          if (saved.draft.channelId !== null) rememberDraft(ordinaryDrafts.current, saved.draft.channelId, saved.draft.text);
        }
        if (saved.side?.channelId === channelId && (saved.side.view === 'list'
          || (saved.side.view === 'thread' && typeof saved.side.rootMsgId === 'string' && HEX32.test(saved.side.rootMsgId)))) setSide(saved.side);
        if (channelId !== null && saved.threadDraft !== null && saved.threadDraft?.channelId === channelId
          && typeof saved.threadDraft.rootMsgId === 'string' && HEX32.test(saved.threadDraft.rootMsgId)
          && typeof saved.threadDraft.text === 'string') {
          setThreadDraft({ channelId, rootMsgId: saved.threadDraft.rootMsgId, text: saved.threadDraft.text });
          rememberDraft(threadDrafts.current, threadDraftKey(channelId, saved.threadDraft.rootMsgId), saved.threadDraft.text);
        }
        if (typeof saved.membersOpen === 'boolean') setMembersOpen(saved.membersOpen);
        const view = validViewPlace(saved.view);
        if (view !== null) setPendingView(view);
        // An older checkpoint's scope is ignored: the palette searches everywhere.
        if (saved.search?.open === true && typeof saved.search.query === 'string' && saved.search.query.length <= 2_000) {
          setSearchQuery(saved.search.query);
          setSearchOpen(true);
          if (saved.search.query.trim().length >= 2) {
            setSearchBusy(true);
            void client.call({ m: 'search', scope: 'all', id: null, query: saved.search.query, cursor: null })
              .catch(() => undefined).finally(() => setSearchBusy(false));
          }
        }
        const owner = typeof saved.conversationId === 'string' && HEX32.test(saved.conversationId) ? saved.conversationId : null;
        const restoredUi = restoredConversationUi(saved.ui);
        if (restoredUi !== null && owner !== null) {
          setPendingUi({ channelId: owner, ui: restoredUi });
        }
        if (saved.call !== null && typeof saved.call?.channelId === 'string' && HEX32.test(saved.call.channelId)) {
          upgradeRejoinUntil.current = Date.now() + 120_000;
          if (saved.call.camera === true) setPendingCamera(saved.call.channelId);
          if (saved.call.sharing === true) setSharingStopped(true);
          callRestoreRef.current(saved.call.channelId, saved.call.camera === true, saved.call.sharing === true);
        }
        if (restored.files.length > 0 && owner !== null) {
          await client.call({ m: 'attachFiles', channelId: owner, files: restored.files });
        }
        restoreApplied.current = true;
      }
      if (restored !== null) await update.store.remove(updateScope);
      setRestoreDone(true);
    }).catch(() => { if (active) setRestoreError(true); });
    return () => { active = false; };
  }, [update, updateScope, client, channelId, restoreRetry]);
  useEffect(() => {
    if (pendingUi === null || pendingUi.channelId !== targetId) return;
    rawDispatch({ type: 'restore', ui: pendingUi.ui });
    setPendingUi(null);
  }, [pendingUi, targetId, rawDispatch]);
  useEffect(() => {
    if (pendingView === null) return;
    const apply = () => {
      const main = pendingView.main;
      const missingMain = main?.anchor !== undefined && document.querySelector(`.d-app-shell__main .d-message-row[data-seq="${main.anchor.seq}"]`) === null;
      const canLoad = missingMain && targetId !== null && viewLoadAttempts.current < 100
        && client.get(`timeline:${targetId}`)?.hasEarlier === true;
      if (canLoad && !viewLoadEarlier.current) {
        viewLoadEarlier.current = true;
        viewLoadAttempts.current++;
        void client.call({ m: 'loadEarlier', channelId: targetId }).catch(() => undefined).finally(() => { viewLoadEarlier.current = false; });
      }
      if (canLoad || viewLoadEarlier.current) return;
      if (restoreViewPlace(pendingView, true)) setPendingView(null);
    };
    apply();
    const timer = setInterval(apply, 150);
    return () => clearInterval(timer);
  }, [pendingView, targetId, client]);
  useEffect(() => {
    if (pendingCamera === null || call.channelId !== pendingCamera || call.session === null || call.state?.phase !== 'connected') return;
    setPendingCamera(null);
    if (!call.state.camera) call.toggleCamera();
  }, [pendingCamera, call]);
  useEffect(() => {
    if (update === undefined || !restoreDone) return;
    return update.watch({ currentEntry: update.currentEntry, fetch: window.fetch.bind(window),
      realtime: {
        state: () => { const c = client.get('connection'); return { ready: c?.status === 'online' && (c.wireVersion ?? 0) >= 5, entry: c?.appBuild ?? null }; },
        subscribe: listener => client.subscribe('connection', listener),
      },
      changed: entry => { upgradeRejoinUntil.current = Date.now() + 120_000; setUpdateEntry(entry); },
      callActive: () => updateVoiceActive.current });
  }, [update, restoreDone, client]);
  useEffect(() => {
    if (updateEntry === null || update === undefined || updateScope === null || !restoreDone || updating.current || updateBlocked
      || updateCallBlocked || updateError) return;
    updating.current = true;
    const lock = document.querySelector<HTMLElement>('.d-root');
    if (lock !== null) lock.inert = true;
    void (async () => {
      try {
        const files = targetId === null ? [] : await client.call({ m: 'checkpointFiles', channelId: targetId }) as File[];
        const resumeId = call.channelId ?? call.recoveryChannelId ?? recentUpdateCall?.channelId ?? null;
        await update.store.save(updateScope, { conversationId: targetId, draft, side, threadDraft, membersOpen, ui, tab: homeSelected ? 'dms' : 'channels', view: captureViewPlace(),
          search: { open: searchOpen, query: searchQuery, scope: 'all', id: null },
          call: resumeId === null ? null
            : { channelId: resumeId,
              camera: call.state?.camera ?? (call.recoveryChannelId !== null ? call.recoveryCamera : recentUpdateCall?.camera ?? false),
              sharing: call.state?.sharing ?? (call.recoveryChannelId !== null ? call.recoverySharing : recentUpdateCall?.sharing ?? false) } }, files);
        if (call.session !== null) await call.session.leave('user');
        update.reload();
      } catch {
        updating.current = false;
        setUpdateError(true);
      } finally {
        if (lock !== null) lock.inert = false;
      }
    })();
  }, [updateEntry, updateRetry, update, updateScope, restoreDone, updateBlocked, updateCallBlocked, updateError, targetId, client, draft, side, threadDraft, membersOpen, ui, homeSelected,
    searchOpen, searchQuery, call, recentUpdateCall]);
  // The /search command opens the palette with its query; like the palette, it searches everywhere (its scope argument no longer narrows).
  const openCommandSearch = (query: string) => {
    setSearchQuery(query); setSearchOpen(true); if (query.trim().length >= 2) issueSearch(query, 'all', null);
  };
  const updateNoStorage = updateEntry !== null && updateScope === null;
  const updateNotice = updateNoStorage || (updateEntry !== null && (updateBlocked || updateCallBlocked)) || updateError || restoreError || sharingStopped
    || call.recoveryChannelId !== null;
  const closeSettings = () => {
    const from = route.name === 'settings' || route.name === 'serverSettings' ? route.from : null;
    void navigate(from === null ? (adminFor ? { name: 'channel', communityId: adminFor, channelId: null } : { name: 'root' }) : routeOf(from), true);
  };
  const settingsActivity = <div className="dw-settings-activity">
    <LinkBanner status={status} labels={{ offline: t('nav.link.offline'), connecting: t('nav.link.connecting', { node: account?.instance?.name ?? t('shell.status.unknown') }) }} />
    {overlay ? ringFlow : null}
    {callTarget !== null ? <Button onClick={() => void navigate(callTarget)}>{t('workspace.returnCall')}</Button> : null}
  </div>;
  const settingsActivityVisible = status !== 'online' || (rings?.length ?? 0) > 0 || callTarget !== null;
  const adminPage = adminFor !== null ? <ServerSettings key={adminFor} communityId={adminFor} tab={adminTab}
    onTab={section => void navigate({ name: 'serverSettings', communityId: adminFor, section, from: null }, !phone || section === null)}
    onClose={closeSettings} announce={announce} activity={settingsActivity} activityVisible={settingsActivityVisible}
    onMessage={startDm} onCompare={userId => void navigate({ name: 'settings', section: 'privacy', from: null, compare: userId })} /> : null;
  // Task 11 (the prototype's phone Settings lists the server's rows and sections): opened from a page with no server of its own (a
  // phone's You tab, a DM), Settings speaks for the server you were last in.
  const settingsPage = route.name === 'settings' ? <Settings activity={settingsActivity} activityVisible={settingsActivityVisible} communityId={communityId ?? fallbackCommunity} /> : adminPage;
  return (
    <CommandProvider runtime={{ call, getCall: () => liveCall.current, navigate: route => { void navigate(route); }, openThread, openSearch: openCommandSearch }} handlers={commandHandlers} suggest={commandSuggest}>
    <>
      {botCommands.dialogs}
      {utilityCommands.dialogs}
      {/* On a phone the reminders popover is a sheet over the Activity tab (Ruling X, README §6.7). */}
      {phone ? utilityCommands.remindersPanel('sheet') : null}
      {updateNotice ? <div className="dw-update-notice" role="status">
          {updateNoStorage ? <Banner tone="warn" action={{ label: t('update.reloadNow'), onAction: () => update?.reload() }}>
            {t('update.noStorage')}</Banner> : null}
          {updateEntry !== null && updateScope !== null && updateCallBlocked ? <Banner tone="info">{t('update.finishCall')}</Banner> : null}
          {updateEntry !== null && updateScope !== null && updateBlocked && !updateCallBlocked ? <Banner tone="info">{t('update.finishDialog')}</Banner> : null}
          {updateError ? <Banner tone="warn" action={{ label: t('update.retry'), onAction: () => {
            setUpdateError(false);
            setUpdateRetry(n => n + 1);
          } }}>{t('update.failed')}</Banner> : null}
          {restoreError ? <Banner tone="warn" action={{ label: t('update.retry'), onAction: () => {
            setRestoreError(false);
            setRestoreRetry(n => n + 1);
          } }}>{t('update.restoreFailed')}</Banner> : null}
          {sharingStopped ? <Banner tone="info" action={{ label: t('shell.banner.dismiss'), onAction: () => setSharingStopped(false) }}>
            {t('update.sharingStopped')}</Banner> : null}
          {call.recoveryChannelId !== null ? <Banner tone="info" action={{ label: t('voice.leave'), onAction: () => call.leave() }}>
            {t('update.rejoiningVoice')}</Banner> : null}
        </div> : null}
      <p className="d-sr-only" role="status" aria-live="polite" data-testid="shell-announcer">{announcement}</p>
      <AppShell
        skipLabel={t('shell.skip')}
        topBar={topBar}
        {...(phone ? {} : { paneWidths, onPaneResize, resizeLabels: { sidebar: t('pane.resize.sidebar'), aside: t('pane.resize.aside') } })}
        layout={phone ? 'phone' : 'desktop'} phonePane={phone ? phonePane : undefined} tabBar={tabBar} callBar={callBar} onTypingChange={setTyping} covered={phone && (overlay || adminFor !== null)}
        page={undefined}
        rail={phone && phoneTab !== 'home' ? null : <CommunityRail label={t('shell.rail.label')} items={railItems} activeId={homeSelected ? null : communityId}
          onSelect={id => void navigate({ name: 'channel', communityId: id, channelId: null })}
          addLabel={t('layout.rail.add')} onAdd={() => setAddOpen(true)}
          itemLabel={railLabel}
          home={{ label: t('home.tile'), itemLabel: t('home.tileLabel', { unread: dmCounts.unread, mentions: dmCounts.mentions }),
            selected: homeSelected, unread: dmCounts.unread, mentions: dmCounts.mentions, onSelect: () => void navigate({ name: 'dms' }) }} />}
        sidebar={sidebar}
        bottom={phone || self === null ? undefined : <>
          <Dock v={voiceCtx} where={call.channelId === null ? '' : callWhere(call.channelId)}
            server={dockServer}
            stageShown={call.channelId !== null && call.channelId === (voiceChannel?.id ?? dm?.id)} />
          <UserPanel name={selfName(self, allMembers)} username={self.username} choice={account?.presenceChoice ?? 'online'} call={call} statusMsg={self.statusMsg} inRoom={selfRoom}
            ptt={settings?.['voice.mode'] === 'ptt' ? { held: call.state?.speaking === true } : false} />
        </>}
        ring={!overlay && rings !== undefined && rings.length > 0 ? ringFlow : undefined}
        header={<>
          {/* Outlives the dock: "You left {where}.", and a block or removal said while the stage is not on screen. */}
          <p className="d-sr-only dw-voice-lasting" role="status">{call.lasting}</p>
          {header}
        </>}
        composer={composer}
        banner={<>
          {linkReason === 'version' ? null : <LinkBanner status={status} labels={{ offline: t('nav.link.offline'),
            connecting: t('nav.link.connecting', { node: account?.instance?.name ?? t('shell.status.unknown') }) }} />}
          {banner}
        </>}
        aside={sideNow?.view === 'thread' && channel !== null
          ? <ThreadView key={sideNow.rootMsgId} channelId={channel.id} rootMsgId={sideNow.rootMsgId} channelName={channel.name} phone={phone}
            onClose={closeSide} announce={announce} returnFocus={() => sideReturn.current}
            blocked={timeoutReason}
            initialDraft={threadDrafts.current.get(threadDraftKey(channel.id, sideNow.rootMsgId)) ?? (threadDraft?.channelId === channel.id && threadDraft.rootMsgId === sideNow.rootMsgId ? threadDraft.text : '')}
            onEditingChange={setThreadEditing} onOpenProfile={openProfile}
            onDraftChange={text => { rememberDraft(threadDrafts.current, threadDraftKey(channel.id, sideNow.rootMsgId), text); setThreadDraft({ channelId: channel.id, rootMsgId: sideNow.rootMsgId, text }); }} />
          : sideNow?.view === 'list' && channel !== null
            ? <ThreadPane title={t('thread.list')} closeLabel={t(phone ? 'nav.back' : 'dialog.close')} onClose={closeSide} phone={phone} root={null} composer={null}>
              <ThreadList channelId={channel.id} channelName={channel.name} communityId={communityId} onOpen={openThread} heading={false} />
            </ThreadPane>
            : membersShown && community !== null && self !== null ? <MemberPane communityId={community.id} selfId={self.id}
              onMessage={startDm} onCompare={userId => void navigate({ name: 'settings', section: 'privacy', from: null, compare: userId })}
              onMention={mentionFromMembers}
              announce={announce} messageCard={profileCard} onCardOpen={() => setProfileCard(null)}
              onClose={phone ? () => { memberSheet.close(); } : () => setMembersOpen(false)} /> : undefined}
        asideLabel={sideNow?.view === 'thread' ? t('thread.title') : sideNow?.view === 'list' ? t('thread.list')
          : community === null ? undefined : t('members.region', { server: community.name })}
        asideOpen={asideShown}
        onAsideClose={sideNow !== null ? closeSide : phone ? memberSheet.close : () => setMembersOpen(false)}
        // Task 11 (Q4): the members sheet's head is the list's own (Members ×), as on a desktop; no PhoneHeader.
        asideCloseLabel={undefined}
        asideReturnRef={sideNow !== null ? sideReturn : membersToggle}
        asideKind={sideNow !== null ? 'thread' : 'members'}
      >
        {main}
      </AppShell>
      {/* The message card sits beside the shell, not inside it: main, the rail and the aside go inert while an overlay (the thread sheet) is open, and the card must stay live above it. */}
      {/* Fix round 1 (Jonas): the call code, from the voice row's menu: six groups of five digits and how to compare them. */}
      <Dialog open={shownCode !== null} title={t('voice.code')} closeLabel={t('dialog.close')} onClose={() => setCodeFor(null)}>
        <p className="dw-call-code__digits">{(shownCode ?? '').replace(/\s+/g, '').match(/\d{5}/g)?.join(' ') ?? shownCode}</p>
        <p className="dw-call-code__help">{t('voice.codeHelp')}</p>
      </Dialog>
      {self !== null && (dm !== null || community !== null) ? (
        <MemberCardHost communityId={dm !== null ? null : community?.id ?? null}
          {...(dm !== null ? { dm: { members: allMembers, peerId: dm.kind === 3 ? dm.members.find(id => id !== self.id) ?? null : null, peerName: dm.name } } : {})} selfId={self.id} card={profileCard} onCard={setProfileCard} announce={announce}
          onMessage={startDm} onCompare={userId => void navigate({ name: 'settings', section: 'privacy', from: null, compare: userId })} />
      ) : null}
      <Search open={searchOpen} query={searchQuery} loading={searchBusy || searchRequest !== (search?.requestId ?? 0)}
        groups={searchOpen ? paletteGroups(paletteProviders, searchQuery) : []}
        onQuery={query => { setSearchQuery(query); if (query.trim().length >= 2) issueSearch(query, 'all', null); }}
        onClose={() => { restoreSearchFocus.current = searchOpener.current; searchOpener.current = null; setSearchOpen(false); }} />
      <AddServerDialog open={addOpen} title={t('layout.add.title')} joinLabel={t('layout.add.join')} createLabel={t('layout.add.create')}
        closeLabel={t('dialog.close')} onClose={() => setAddOpen(false)} onJoin={() => setJoinOpen(true)} onCreate={() => setCreateOpen(true)} />
      <JoinCommunity open={joinOpen} initialInvite={joinInvite ?? undefined} initialError={joinError ?? undefined}
        onClose={closeJoin} onJoined={onJoined} />
      <UnsupportedVoice open={call.unsupported} labels={{ title: t('voice.unsupported.title'), body: t('voice.unsupported.body'),
        close: t('voice.unsupported.close') }} onClose={call.closeUnsupported} />
      <DmPicker open={pickerOpen} members={pickerMembers} busy={pickerBusy} error={pickerError}
        body={t(noServer ? 'shell.dms.noServer' : pickerMembers.length === 0 ? 'shell.dms.alone' : 'shell.dms.newBody')}
        onClose={closePicker} onPick={startDm} />
      <CreateServerFlow open={createOpen} onClose={() => setCreateOpen(false)}
        onServer={id => { setJoined(id); void navigate({ name: 'channel', communityId: id, channelId: null }); }}
        onDone={({ communityId: id, channelId: general }) => {
          pendingFocus.current = { id: general, from: targetId };
          setCreateOpen(false);
          void navigate({ name: 'channel', communityId: id, channelId: general }, true);
        }} />
      <CreateChannelFlow open={channelOpen} community={community} parentId={channelParent} onClose={() => setChannelOpen(false)}
        onDone={(id, kind) => {
          if (kind === 'text') pendingFocus.current = { id, from: targetId };
          setChannelOpen(false);
          if (community !== null) void navigate({ name: 'channel', communityId: community.id, channelId: id });
        }} />
      <ChannelEditFlow channel={editing} categories={(channels ?? []).filter(c => c.kind === CATEGORY)} announce={announce} onClose={() => setEditChannelId(null)} />
      <PhoneLayoutProvider value={phone}>{settingsPage}{props.page}</PhoneLayoutProvider>
    </>
    </CommandProvider>
  );
}

/** The DM picker (requirement 5): the server's members, one "message" button each, the refusal inside the dialog. */
function DmPicker(p: {
  open: boolean; members: readonly { userId: string; username: string; display: string }[]; busy: boolean; error: UiError | null;
  /** Who can be picked, or why no one can (flow 07: no server yet, or no one else in it). */
  body: string;
  onClose: () => void; onPick: (userId: string) => void;
}): React.JSX.Element {
  const idBase = useId();
  return (
    <Dialog open={p.open} title={t('shell.dms.newTitle')} closeLabel={t('dialog.close')} onClose={p.onClose}>
      {p.error !== null ? <Banner tone="danger">{t('shell.banner.commandError', { code: p.error.code })}</Banner> : null}
      <p>{p.body}</p>
      {p.members.length === 0 ? null : <ul className="dw-dm-picker">
        {p.members.map(m => {
          const nameId = `${idBase}-${m.userId}`;
          return (
            <li key={m.userId} className="dw-dm-picker__row">
              <span id={nameId} className="dw-dm-picker__name">{m.display || m.username}</span>
              <Button size="sm" aria-describedby={nameId} aria-disabled={p.busy ? true : undefined}
                onClick={() => { if (!p.busy) p.onPick(m.userId); }}>{t('shell.dms.open')}</Button>
            </li>
          );
        })}
      </ul>}
    </Dialog>
  );
}
