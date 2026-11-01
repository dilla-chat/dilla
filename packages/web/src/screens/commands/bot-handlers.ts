import { createElement, useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { BotInteractionBindingMismatchError, BotInteractionRejectedError, BotInteractionUncertainError, createBotInteractionClient, createVoiceMusicAdapter, decode, encode, fromHex, PERM, type BotInteractionResult, type BotInteractionScope, type CoreClient,
  COMMANDS, parseSeekPosition, type CommandContext as Binding, type CommandInvocation, type CommandResult } from '@dilla/client-core';
import type { CommandExecutionContext, CommandProviderHandlers, CommandSuggestionProvider } from './command-types.ts';
import { BotCommandDialogs, type BotCommandDialogState, type BotCommandDialogValue } from './BotCommandDialogs.tsx';
import { commandComposerCopy as copy } from './copy.ts';
import { t } from '../../strings/index.ts';

const ID = /^[0-9a-f]{32}$/u;
// A link being typed or pasted belongs to strict enqueue validation, never text search.
const MUSIC_LINK_PREFIX = /^(?:https?:|\/\/|(?:(?:www|music|m)\.)?youtube\.com\/|youtu\.be\/|open\.spotify\.com\/|spotify:(?:track|album|playlist):[A-Za-z0-9]{22}$)/iu;
const MUSIC_COMMAND_IDS = new Set(['play', 'pause', 'resume', 'skip', 'stop', 'queue', 'nowplaying', 'seek', 'volume', 'shuffle', 'repeat',
  'queue remove', 'queue move', 'queue clear', 'music help']);
const ok = (message: string): CommandResult => ({ status: 'success', message, private: true });
const fail = (message: string): CommandResult => ({ status: 'failure', message, private: true });
const uncertain = (message: string): CommandResult => ({ status: 'uncertain', message, private: true });
const resultError = (error: unknown): CommandResult => {
  if (error instanceof Error && 'code' in error && error.code === 'operation-unavailable') return fail(t('music.error.operation-unavailable'));
  if (error instanceof Error && 'code' in error && error.code === 'unavailable') return fail(t('music.error.unavailable'));
  if (error instanceof BotInteractionRejectedError || error instanceof BotInteractionBindingMismatchError)
    return fail(error.message);
  const requestId = typeof error === 'object' && error !== null && 'requestId' in error ? String(error.requestId) : '';
  const code = typeof error === 'object' && error !== null && 'code' in error ? String(error.code) : '';
  if (requestId !== '' || error instanceof Error && /uncertain|timed out|accepted.*confirm/iu.test(error.message))
    return uncertain(error instanceof Error ? error.message : 'The Bot result is uncertain. Inspect the Bot state before trying again.');
  if (['E_FORBIDDEN', 'E_NOT_FOUND', 'E_CONFLICT', 'E_BAD_INPUT', 'E_BOT_PERMISSION', 'E_BOT_BAD_INPUT'].includes(code))
    return fail(error instanceof Error ? error.message : `The Bot refused the request (${code}).`);
  return fail(error instanceof Error ? error.message : 'The Bot command could not be completed.');
};
const childId = (parent: string, label: string): string => {
  const bytes = new TextEncoder().encode(`${parent}:${label}`);
  let result = '';
  for (let seed = 0; seed < 4; seed++) {
    let hash = (0x811c9dc5 ^ Math.imul(seed + 1, 0x9e3779b1)) >>> 0;
    for (const byte of bytes) { hash ^= byte; hash = Math.imul(hash, 0x01000193) >>> 0; }
    result += hash.toString(16).padStart(8, '0');
  }
  return result === '0'.repeat(32) ? '00'.repeat(15) + '01' : result;
};
const currentCallChannel = (client: CoreClient): string | null => client.get('call')?.channelId ?? null;
function requireCommunity(context: CommandExecutionContext, providerCommunity: string): string | null {
  const communityId = context.binding.communityId;
  if (communityId === null || communityId !== providerCommunity) return null;
  const call = context.getCall();
  const callChannel = call.channelId;
  const channel = clientChannels(context.client, communityId).find(row => row.id === callChannel && row.kind === 1);
  if (call.session === null || callChannel === null || channel === undefined || context.client.get('call')?.channelId !== callChannel) return null;
  return callChannel;
}
function clientChannels(client: CoreClient, communityId: string) {
  return client.get(`channels:${communityId}`) ?? [];
}
async function freshMusicState(context: CommandExecutionContext, communityId: string, channelId: string) {
  const adapter = createVoiceMusicAdapter(context.client, communityId);
  const unsubscribe = adapter.subscribe(channelId, () => undefined);
  try { await adapter.refresh(channelId); return { adapter, state: adapter.state(channelId) }; }
  finally { unsubscribe(); }
}
function musicReadSummary(commandId: string, invocation: CommandInvocation, context: CommandExecutionContext,
  state: Extract<ReturnType<ReturnType<typeof createVoiceMusicAdapter>['state']>, { status: 'available' }>): CommandResult {
  if (commandId === 'music' || commandId === 'music help') return ok(COMMANDS.filter(command => MUSIC_COMMAND_IDS.has(command.id))
    .map(command => `/${command.id}${command.options.length === 0 ? '' : ` ${command.options.map(option => `${option.required ? '<' : '['}${option.name}${option.required ? '>' : ']'}`).join(' ')}`} — ${command.description}`)
    .join('\n'));
  if (commandId === 'nowplaying') return ok(state.track === null ? 'Nothing is playing.' : `Now playing: ${state.track.title} — ${state.track.artist}${state.paused ? ' (paused)' : ''}.`);
  if (commandId === 'queue') {
    const current = state.track === null ? '' : `Now playing: ${state.track.title} — ${state.track.artist}${state.paused ? ' (paused)' : ''}\n`;
    const rows = state.queue.map((item, index) => `${index + 1}. ${item.track.title} — ${item.track.artist}`);
    if (state.queueTotal !== undefined) rows.push(`Showing ${state.queue.length} of ${state.queueTotal} queued tracks.`);
    return ok(current === '' && rows.length === 0 ? 'The music queue is empty.' : `${current}${rows.join('\n')}`.trim());
  }
  return fail('Unsupported music read command.');
}

interface ScheduleRow { id: string; channelId: string; text: string; kind: 'one_off' | 'daily' | 'weekly'; timezone: string; date: string;
  hour: number; minute: number; weekday: number; enabled: boolean; revision: number }
interface AnnouncementList { rows: ScheduleRow[]; revision: string }
interface PendingAnnouncementMutation { scopeKey: string; operation: 51 | 52; requestId: string; args: readonly (string | number | Uint8Array | null)[]; revision: string; scope: BotInteractionScope; sent: boolean }
const announcementAttempts = new WeakMap<CoreClient, Map<string, PendingAnnouncementMutation>>();
function announcementAttemptStore(client: CoreClient): Map<string, PendingAnnouncementMutation> {
  let store = announcementAttempts.get(client); if (store === undefined) { store = new Map(); announcementAttempts.set(client, store); }
  return store;
}
function accountScopeKey(client: CoreClient, communityId: string, requestId: string): string {
  const account = client.get('account');
  return `${account?.instance?.id ?? ''}:${account?.user?.id ?? ''}:${communityId}:${requestId}`;
}
const musicCommandAttempts = new WeakMap<CoreClient, Map<string, string>>();
function musicCommandAttemptStore(client: CoreClient): Map<string, string> {
  let store = musicCommandAttempts.get(client); if (store === undefined) { store = new Map(); musicCommandAttempts.set(client, store); }
  return store;
}
function hasMusicCommandAttempt(client: CoreClient, communityId: string, requestId: string): boolean {
  return musicCommandAttemptStore(client).has(accountScopeKey(client, communityId, requestId));
}
function musicCommandAttemptTarget(client: CoreClient, communityId: string, requestId: string): string | undefined {
  return musicCommandAttemptStore(client).get(accountScopeKey(client, communityId, requestId));
}
function markMusicCommandAttempt(client: CoreClient, communityId: string, requestId: string, channelId: string): void {
  const store = musicCommandAttemptStore(client); const key = accountScopeKey(client, communityId, requestId);
  if (!store.has(key)) store.set(key, channelId);
  while (store.size > 128) store.delete(store.keys().next().value!);
}
function clearMusicCommandAttempt(client: CoreClient, communityId: string, requestId: string): void {
  musicCommandAttemptStore(client).delete(accountScopeKey(client, communityId, requestId));
}
async function runAnnouncementMutation(client: CoreClient, communityId: string, parentId: string, stage: string, operation: 51 | 52,
  args: readonly (string | number | Uint8Array | null)[], revision: string, scope: BotInteractionScope): Promise<BotInteractionResult> {
  const store = announcementAttemptStore(client); const key = accountScopeKey(client, communityId, parentId);
  let attempt = store.get(key);
  if (attempt !== undefined && (attempt.operation !== operation || attempt.revision !== revision || attempt.scopeKey !== JSON.stringify(scope))) {
    if (!attempt.sent || attempt.operation !== operation) throw new BotInteractionUncertainError(attempt.requestId,
      'This announcement attempt is bound to a different schedule revision or Bot installation. Reconcile it before starting another command.');
    const prior = await createBotInteractionClient(client, communityId).read(attempt.requestId, operation);
    if (prior === null) throw new BotInteractionUncertainError(attempt.requestId, 'The prior announcement result is still pending.');
    return prior;
  }
  if (attempt === undefined) {
    attempt = { scopeKey: JSON.stringify(scope), operation, requestId: childId(parentId, `announcement:${stage}`), args: args.map(value => value instanceof Uint8Array ? value.slice() : value), revision, scope, sent: false };
    store.set(key, attempt);
    while (store.size > 128) store.delete(store.keys().next().value!);
  }
  const api = createBotInteractionClient(client, communityId);
  if (attempt.sent) {
    let prior: BotInteractionResult | null;
    try { prior = await api.read(attempt.requestId, operation); }
    catch (error) { throw new BotInteractionUncertainError(attempt.requestId, `Could not reconcile the prior schedule mutation: ${error instanceof Error ? error.message : 'read failed'}`); }
    if (prior === null) throw new BotInteractionUncertainError(attempt.requestId, 'The prior announcement result is still pending. Reconcile this same request before editing the schedule again.');
    return prior;
  }
  let persisted: BotInteractionResult | null;
  try { persisted = await api.read(attempt.requestId, operation); }
  catch (error) { throw new BotInteractionUncertainError(attempt.requestId,
    `Could not reconcile the prior schedule request ID: ${error instanceof Error ? error.message : 'read failed'}`); }
  if (persisted !== null) { attempt.sent = true; return persisted; }
  attempt.sent = true;
  try {
    return await api.request({ requestId: attempt.requestId, operation, args: attempt.args, revision: attempt.revision, expectedScope: attempt.scope });
  } catch (error) {
    if (error instanceof BotInteractionBindingMismatchError || error instanceof BotInteractionRejectedError) store.delete(key);
    throw error;
  }
}
function decodeAnnouncementList(data: Uint8Array, revision: string | null, expectedOffset: number): AnnouncementList & { total: number } {
  if (revision === null) throw new Error('The Bot announcement list has no revision.');
  const row = decode(data);
  if (!Array.isArray(row) || row.length !== 4 || row[0] !== 1n || row[1] !== BigInt(expectedOffset) || typeof row[2] !== 'bigint'
    || row[2] > 128n || !Array.isArray(row[3]) || row[3].length > 8) throw new Error('Malformed announcement list.');
  const total = Number(row[2]);
  if (expectedOffset > total || row[3].length > total - expectedOffset) throw new Error('Malformed announcement list page.');
  const rows = row[3].map((item): ScheduleRow => {
    if (!Array.isArray(item) || item.length !== 11 || !(item[0] instanceof Uint8Array) || !(item[1] instanceof Uint8Array)) throw new Error('Malformed announcement row.');
    const [id, channelId, text, kind, timezone, date, hour, minute, weekday, enabled, itemRevision] = item;
    const idHex = toHex(id); const channelHex = toHex(channelId);
    if (!ID.test(idHex) || idHex === '0'.repeat(32) || !ID.test(channelHex) || channelHex === '0'.repeat(32) || typeof text !== 'string' || text.trim() === ''
      || new TextEncoder().encode(text).length > 1800 || !['one_off', 'daily', 'weekly'].includes(String(kind))
      || typeof timezone !== 'string' || typeof date !== 'string' || typeof hour !== 'bigint' || typeof minute !== 'bigint'
      || typeof weekday !== 'bigint' || typeof enabled !== 'bigint' || enabled > 1n || typeof itemRevision !== 'bigint' || itemRevision < 1n
      || itemRevision > 0x7fff_ffff_ffff_ffffn || hour > 23n || minute > 59n || weekday > 6n)
      throw new Error('Malformed announcement row.');
    return { id: idHex, channelId: channelHex, text, kind: kind as ScheduleRow['kind'], timezone, date, hour: Number(hour), minute: Number(minute),
      weekday: Number(weekday), enabled: enabled === 1n, revision: Number(itemRevision) };
  });
  return { rows, revision, total };
}
function toHex(data: Uint8Array): string { return Array.from(data, byte => byte.toString(16).padStart(2, '0')).join(''); }
function confirmedAnnouncementMutation(response: { revision: string | null; data: Uint8Array }, previousRevision: string): boolean {
  if (response.revision === null || !/^(?:0|[1-9]\d*)$/u.test(response.revision) || BigInt(response.revision) <= BigInt(previousRevision)) return false;
  try {
    const decoded = decode(response.data);
    return Array.isArray(decoded) && decoded.length === 0;
  } catch { return false; }
}
// Historical receipts keep their original native provenance while the current config revision may change.
function sameInteractionIdentity(left: BotInteractionScope, right: BotInteractionScope): boolean {
  return left.installationId === right.installationId && left.botUserId === right.botUserId;
}
async function listAnnouncements(client: CoreClient, communityId: string, requestId: string, expectedScope?: { installationId: string; botUserId: string | null; revision: string }) {
  const api = createBotInteractionClient(client, communityId);
  let offset = 0; const rows: ScheduleRow[] = []; let revision = ''; let expectedTotal: number | null = null;
  for (;;) {
    const response = await api.request({ requestId: childId(requestId, `announcement:list:${offset}`), operation: 50, args: [offset], expectedScope });
    if (response.status !== 0) throw new Error(response.status === 2 ? 'Manage Community permission is required.' : 'The announcement list is unavailable.');
    const page = decodeAnnouncementList(response.data, response.revision, offset);
    if (expectedTotal !== null && (page.total !== expectedTotal || page.revision !== revision)) throw new Error('The announcement list changed between pages. Refresh it before editing.');
    expectedTotal = page.total;
    revision = page.revision; rows.push(...page.rows);
    offset += page.rows.length;
    if (offset >= page.total) break;
    if (page.rows.length === 0) throw new Error('The announcement list page made no progress.');
  }
  return { rows, revision };
}
function announcementSummary(row: ScheduleRow): string {
  return `${row.text} — ${row.kind}, ${row.timezone} ${String(row.hour).padStart(2, '0')}:${String(row.minute).padStart(2, '0')}${row.enabled ? '' : ' (disabled)'}`;
}
function validateCurrent(context: CommandExecutionContext, communityId: string, permission: 'manageCommunity' | 'manageRoles'): boolean {
  if (!context.isCurrent() || context.binding.communityId !== communityId || context.getCurrentBinding().communityId !== communityId) return false;
  return context.getCurrentBinding().bits !== undefined && (context.getCurrentBinding().bits! & (permission === 'manageRoles' ? PERM.manageRoles : PERM.manageCommunity)) !== 0;
}
function voiceStillCurrent(context: CommandExecutionContext, communityId: string, channelId: string): boolean {
  return context.isCurrent() && requireCommunity(context, communityId) === channelId;
}
/** Provider hook; the Shell owner renders `dialogs` alongside the ordinary command host. */
export function useBotCommandProvider(client: CoreClient, communityId: string | null): {
  handlers: CommandProviderHandlers; suggest: CommandSuggestionProvider; dialogs: ReactNode;
} {
  const [dialog, setDialog] = useState<BotCommandDialogState | null>(null);
  const resolveRef = useRef<((value: BotCommandDialogValue | null) => void) | null>(null);
  const alive = useRef(true);
  const staleSearch = useRef(new Map<string, number>());
  const openDialog = useCallback(<T extends BotCommandDialogState>(value: Omit<T, 'onSubmit' | 'onCancel' | 'isCurrent'>, context: CommandExecutionContext) => new Promise<BotCommandDialogValue | null>(resolve => {
    if (!context.isCurrent() || !alive.current) { resolve(null); return; }
    resolveRef.current?.(null);
    resolveRef.current = resolve;
    setDialog({ ...value, isCurrent: () => context.isCurrent(), onSubmit: (submitted: BotCommandDialogValue) => { resolveRef.current?.(submitted); resolveRef.current = null; setDialog(null); },
      onCancel: () => { resolveRef.current?.(null); resolveRef.current = null; setDialog(null); } } as unknown as BotCommandDialogState);
  }) as Promise<T extends { kind: 'announcement' } ? Extract<BotCommandDialogValue, { announcement: unknown }> | null : Extract<BotCommandDialogValue, { rolePanel: unknown }> | null>, []);
  useEffect(() => { alive.current = true; return () => { alive.current = false; resolveRef.current?.(null); resolveRef.current = null; }; }, [client, communityId]);

  const musicHandler = useCallback(async (invocation: CommandInvocation, context: CommandExecutionContext): Promise<CommandResult> => {
    const cid = context.binding.communityId;
    if (cid === null || cid !== communityId) return fail('Music commands need an active community voice call.');
    const pendingAttempt = hasMusicCommandAttempt(client, cid, context.requestId);
    if (!context.isCurrent()) return pendingAttempt ? uncertain('This music attempt is still pending; return to its original command context to reconcile it.')
      : fail('The command context is no longer active.');
    const channelId = requireCommunity(context, cid);
    if (channelId === null) return pendingAttempt ? uncertain('This music attempt is still pending. Rejoin its community voice call before reconciling it.')
      : fail('Join a voice channel in this community before using music commands.');
    const originalChannelId = musicCommandAttemptTarget(client, cid, context.requestId);
    if (originalChannelId !== undefined && channelId !== originalChannelId)
      return uncertain('This music attempt is still pending in its original voice channel. Return to that call to reconcile it; the attempt cannot be retargeted.');
    // Pause and skip are current-state controls. Authority is checked by the Bot;
    // dispatch directly so a refresh or announcement cannot delay the audio change.
    if (invocation.commandId === 'pause' || invocation.commandId === 'skip') {
      const adapter = createVoiceMusicAdapter(client, cid);
      markMusicCommandAttempt(client, cid, context.requestId, channelId);
      try {
        const attempt = { requestId: childId(context.requestId, `music:${invocation.commandId}`) };
        if (invocation.commandId === 'pause') await adapter.pause(channelId, true, attempt);
        else await adapter.skip(channelId, attempt);
        if (!voiceStillCurrent(context, cid, channelId)) return uncertain('Music was updated, but the active voice call changed before completion.');
        clearMusicCommandAttempt(client, cid, context.requestId);
        return ok(invocation.commandId === 'pause' ? 'Music paused.' : 'Skipped the current track.');
      } catch (error) {
        if (resultError(error).status !== 'uncertain') clearMusicCommandAttempt(client, cid, context.requestId);
        return resultError(error);
      }
    }
    try {
      let adapter: ReturnType<typeof createVoiceMusicAdapter>;
      try { ({ adapter } = await freshMusicState(context, cid, channelId)); }
      catch (error) {
        if (pendingAttempt) return uncertain(`This music attempt is still pending, but the Bot player is unavailable for reconciliation (${error instanceof Error ? error.message : 'unknown error'}).`);
        throw error;
      }
      if (!context.isCurrent()) return pendingAttempt ? uncertain('This music attempt is still pending, but the conversation changed while music state was loading.')
        : fail('The conversation changed while music state was loading.');
      const fresh = adapter.state(channelId);
      if (fresh.status !== 'available') return pendingAttempt ? uncertain('This music attempt is still pending; the Bot player is currently unavailable.')
        : fail('The Bot music player is unavailable.');
      if (['queue', 'nowplaying', 'music', 'music help'].includes(invocation.commandId)) return musicReadSummary(invocation.commandId, invocation, context, fresh);
      if (!fresh.permissions.control) return pendingAttempt ? uncertain('This music attempt is still pending; ask a DJ, owner, or administrator to restore control before reconciling it.')
        : fail('A DJ, owner, or administrator must control music in this voice channel.');
      if (!voiceStillCurrent(context, cid, channelId)) return pendingAttempt ? uncertain('This music attempt is still pending; return to its original active call to reconcile it.')
        : fail('The voice call changed before the command ran.');
      const attempt = (stage: string) => ({ requestId: childId(context.requestId, `music:${stage}`) });
      const runMutation = async (action: () => void | Promise<void>) => {
        markMusicCommandAttempt(client, cid, context.requestId, channelId);
        try { await action(); }
        catch (error) {
          if (resultError(error).status !== 'uncertain') clearMusicCommandAttempt(client, cid, context.requestId);
          throw error;
        }
      };
      const applyControl = async (action: () => void | Promise<void>) => {
        await runMutation(action);
        if (!voiceStillCurrent(context, cid, channelId)) return false;
        clearMusicCommandAttempt(client, cid, context.requestId);
        return true;
      };
      const registerDestination = async () => {
        // Notice registration is best effort, including with older Bots. It must
        // never create a pending playback mutation or prevent an authorized Resume.
        if (context.binding.channelId !== channelId) {
          try { await adapter.announce(channelId, context.binding.channelId, attempt('destination')); }
          catch { /* Playback remains available without a notice destination. */ }
        }
        return voiceStillCurrent(context, cid, channelId);
      };
      switch (invocation.commandId) {
        case 'play': {
          const input = invocation.args['query-or-url']?.trim() ?? '';
          if (!input) return fail('Choose a track or provide a supported Spotify or YouTube URL.');
          let selected = input;
          const url = MUSIC_LINK_PREFIX.test(input);
          if (!url) {
            const found = await adapter.search(channelId, input, attempt('search'));
            if (!context.isCurrent() || requireCommunity(context, cid) !== channelId) return uncertain('Search finished after the conversation or voice call changed.');
            if (found.length === 0) return fail('No YouTube tracks matched that search.');
            const first = found[0];
            if (first === undefined) return fail('No YouTube tracks matched that search.');
            selected = first.input;
          }
          await runMutation(() => adapter.enqueue(channelId, selected, attempt('enqueue')));
          if (!context.isCurrent() || requireCommunity(context, cid) !== channelId)
            { clearMusicCommandAttempt(client, cid, context.requestId); return ok('The track was added, but playback was not resumed because the voice call changed.'); }
          const afterEnqueue = adapter.state(channelId);
          if (afterEnqueue.status === 'available' && afterEnqueue.paused) {
            if (!await registerDestination()) {
              clearMusicCommandAttempt(client, cid, context.requestId);
              return ok('The track was added, but playback was not resumed because the conversation or voice call changed.');
            }
            try { await runMutation(() => adapter.pause(channelId, false, attempt('resume'))); }
            catch (error) {
              if (resultError(error).status === 'uncertain') return resultError(error);
              clearMusicCommandAttempt(client, cid, context.requestId);
              return ok(`Added ${selected}; playback did not resume (${error instanceof Error ? error.message : 'unknown result'}). The track is already in the queue.`);
            }
          }
          clearMusicCommandAttempt(client, cid, context.requestId);
          // Destination registration is separate from the public started-music
          // card, which remains best effort after playback has actually started.
          const afterPlay = adapter.state(channelId);
          if (fresh.track === null && afterPlay.status === 'available' && afterPlay.track !== null && !afterPlay.paused && context.binding.channelId !== channelId)
            void (async () => { await adapter.announce(channelId, context.binding.channelId, attempt('announce')); })().catch(() => undefined);
          return ok('Track added to the music queue.');
        }
        case 'resume': {
          if (!await registerDestination()) return pendingAttempt
            ? uncertain('This music attempt is still pending; return to its original active call to reconcile it.')
            : fail('The conversation or voice call changed before music resumed.');
          return await applyControl(() => adapter.pause(channelId, false, attempt('resume')))
            ? ok('Music resumed.') : uncertain('Music was updated, but the active voice call changed before completion.');
        }
        case 'stop': return await applyControl(() => adapter.stop(channelId, attempt('stop')))
          ? ok('Music stopped.') : uncertain('Music was updated, but the active voice call changed before completion.');
        case 'seek': {
          const ms = parseSeekPosition(invocation.args.position ?? '');
          if (ms === null) return fail('Use seconds, mm:ss, or hh:mm:ss within the current track.');
          return await applyControl(() => adapter.seek(channelId, Math.min(ms, fresh.track?.durationMs ?? Number.MAX_SAFE_INTEGER), attempt('seek')))
            ? ok('Playback position updated.') : uncertain('Music was updated, but the active voice call changed before completion.');
        }
        case 'volume': {
          const volume = Number(invocation.args.level);
          if (!Number.isInteger(volume) || volume < 0 || volume > 100) return fail('Volume must be from 0 to 100.');
          return await applyControl(() => adapter.setVolume(channelId, volume, attempt('volume')))
            ? ok(`Volume set to ${volume}%.`) : uncertain('Music was updated, but the active voice call changed before completion.');
        }
        case 'shuffle': {
          const enabled = invocation.args.mode === 'on';
          return await applyControl(() => adapter.setShuffle(channelId, enabled, attempt(`shuffle:${enabled ? 'on' : 'off'}`)))
            ? ok(`Shuffle ${enabled ? 'enabled' : 'disabled'}.`) : uncertain('Music was updated, but the active voice call changed before completion.');
        }
        case 'repeat': {
          const mode = invocation.args.mode;
          if (mode !== 'off' && mode !== 'one' && mode !== 'all') return fail('Repeat mode must be off, one, or all.');
          return await applyControl(() => adapter.setRepeat(channelId, mode, attempt(`repeat:${mode}`)))
            ? ok(`Repeat set to ${mode}.`) : uncertain('Music was updated, but the active voice call changed before completion.');
        }
        case 'queue remove': {
          const id = invocation.args.item ?? '';
          return await applyControl(() => adapter.remove(channelId, id, attempt(`queue:remove:${id}`)))
            ? ok('Removed the track from the queue.') : uncertain('The queue was updated, but the active voice call changed before completion.');
        }
        case 'queue move': {
          const id = invocation.args.item ?? '';
          const position = Number(invocation.args.position);
          if (!Number.isInteger(position) || position < 1) return fail('Choose a position within the current queue.');
          return await applyControl(() => adapter.move(channelId, id, position - 1, attempt(`queue:move:${id}:${position}`)))
            ? ok(`Moved the track to position ${position}.`) : uncertain('The queue was updated, but the active voice call changed before completion.');
        }
        case 'queue clear': return await applyControl(() => adapter.clearQueue(channelId, attempt('queue:clear')))
          ? ok('Music queue cleared.') : uncertain('The queue was cleared, but the active voice call changed before completion.');
        default: return fail('Unsupported music command.');
      }
    } catch (error) {
      const result = resultError(error);
      if (result.status === 'uncertain') markMusicCommandAttempt(client, cid, context.requestId, originalChannelId ?? channelId);
      else clearMusicCommandAttempt(client, cid, context.requestId);
      return result;
    }
  }, [client, communityId]);

  const announcementHandler = useCallback(async (invocation: CommandInvocation, context: CommandExecutionContext): Promise<CommandResult> => {
    const cid = context.binding.communityId;
    if (cid === null || cid !== communityId) return fail('Announcements are available only in this community.');
    const attemptKey = accountScopeKey(client, cid, context.requestId);
    const pendingAttempt = announcementAttemptStore(client).get(attemptKey);
    if (!validateCurrent(context, cid, 'manageCommunity')) return pendingAttempt?.sent === true
      ? uncertain('This schedule attempt is still pending. Restore Manage Community permission in its original community to reconcile it.')
      : fail('Manage Community permission is required.');
    try {
      let scope: BotInteractionScope;
      if (pendingAttempt?.sent === true) {
        // Reconciliation reads the immutable original request under its captured installation scope.
        // A fresh scope preflight could fail independently and must not block or retire that attempt.
        scope = pendingAttempt.scope;
      } else {
        const api = createBotInteractionClient(client, cid);
        const scopeResult = await api.request({ requestId: childId(context.requestId, 'announcement:scope'), operation: 50, args: [0] });
        if (!context.isCurrent() || !validateCurrent(context, cid, 'manageCommunity')) return fail('The community or permission changed before the announcement request.');
        if (scopeResult.status !== 0) return fail(scopeResult.status === 2 ? 'Manage Community permission is required.' : 'Announcements are unavailable.');
        scope = scopeResult.scope;
      }
      const pending = pendingAttempt;
      if (pending !== undefined) {
        const reconciled = await runAnnouncementMutation(client, cid, context.requestId, pending.operation === 52 ? 'delete' : 'save', pending.operation,
          pending.args, pending.revision, scope);
        if (reconciled.status === 0) {
          if (!validateCurrent(context, cid, 'manageCommunity')) return uncertain('The schedule result is available, but Manage Community permission changed before confirmation. Retry this same attempt when permission is restored.');
          if (!sameInteractionIdentity(reconciled.scope, pending.scope)) return uncertain('The original schedule attempt is bound to a previous Bot installation. Reconcile that result before starting another edit.');
          if (!confirmedAnnouncementMutation(reconciled, pending.revision)) return uncertain('The Bot accepted the schedule change, but its revision receipt was malformed. Refresh the list before retrying.');
          announcementAttemptStore(client).delete(attemptKey);
          return ok(pending.operation === 52 ? 'Scheduled announcement deleted.' : invocation.commandId === 'announcement edit' ? 'Announcement updated.' : 'Announcement scheduled.');
        }
        if (reconciled.status === 5) return uncertain('The Bot reported an ambiguous schedule mutation. Retry this same attempt to reconcile it; the schedule was not resent.');
        announcementAttemptStore(client).delete(attemptKey);
        return reconciled.status === 4 ? fail('The schedule changed before saving. Refresh the announcement list and try again.')
          : fail(reconciled.status === 2 ? 'Manage Community permission is required.' : 'The announcement could not be saved.');
      }
      if (invocation.commandId === 'announcement list') {
        const listed = await listAnnouncements(client, cid, context.requestId, scope);
        return ok(listed.rows.length === 0 ? 'No scheduled announcements.' : listed.rows.map(row => `${row.id}: ${announcementSummary(row)}`).join('\n'));
      }
      const listed = await listAnnouncements(client, cid, context.requestId, scope);
      if (invocation.commandId === 'announcement delete') {
        const selectedId = invocation.args.announcement ?? '';
        const row = listed.rows.find(item => item.id === selectedId);
        if (row === undefined) return fail('That announcement is no longer available. Refresh the list and choose it again.');
        const confirmed = await context.confirm('Delete this scheduled announcement?', announcementSummary(row));
        if (!confirmed || !context.isCurrent()) return fail('Announcement deletion cancelled.');
        if (!validateCurrent(context, cid, 'manageCommunity')) return fail('Manage Community permission changed before deletion.');
        const storeKey = accountScopeKey(client, cid, context.requestId);
        const result = await runAnnouncementMutation(client, cid, context.requestId, 'delete', 52, [fromHex(row.id), row.revision], listed.revision, scope);
        if (result.status === 0) {
          if (!validateCurrent(context, cid, 'manageCommunity')) return uncertain('The deletion was accepted, but Manage Community permission changed before confirmation. Retry this same attempt when permission is restored.');
          if (!sameInteractionIdentity(result.scope, scope)) return uncertain('The deletion was accepted, but the Bot installation changed before its result could be confirmed.');
          if (!confirmedAnnouncementMutation(result, listed.revision)) return uncertain('The Bot accepted deletion, but its revision receipt was malformed. Refresh the list before retrying.');
          announcementAttemptStore(client).delete(storeKey); return ok('Scheduled announcement deleted.');
        }
        if (result.status === 5) return uncertain('The Bot reported an ambiguous deletion. Retry this same attempt to reconcile it; deletion was not resent.');
        announcementAttemptStore(client).delete(storeKey);
        return result.status === 4 ? fail('The schedule changed before deletion. Refresh the announcement list and choose it again.')
          : fail(result.status === 2 ? 'Manage Community permission is required.' : 'The announcement could not be deleted.');
      }
      if (invocation.commandId !== 'announcement create' && invocation.commandId !== 'announcement edit') return fail('Unsupported announcement command.');
      const existing = invocation.commandId === 'announcement edit' ? listed.rows.find(item => item.id === invocation.args.announcement) : undefined;
      if (invocation.commandId === 'announcement edit' && existing === undefined) return fail('That announcement is no longer available. Refresh the list and choose it again.');
      const value = await openDialog<Extract<BotCommandDialogState, { kind: 'announcement' }>>({ kind: 'announcement', communityId: cid, channels: clientChannels(client, cid).filter(row => row.kind === 0),
        initial: existing ?? null, prefill: invocation.args.text ?? '', mode: invocation.commandId === 'announcement edit' ? 'edit' : 'create' }, context);
      if (value === null || !context.isCurrent()) return fail('Announcement editing cancelled.');
      if (!validateCurrent(context, cid, 'manageCommunity')) return fail('Manage Community permission changed before saving.');
      const schedule = value.announcement;
      const id = existing?.id ?? randomId();
      const itemRevision = existing === undefined ? 1 : existing.revision + 1;
      const encoded = encode([1n, fromHex(id), fromHex(schedule.channelId), schedule.text, schedule.kind, schedule.timezone, schedule.date,
        BigInt(schedule.hour), BigInt(schedule.minute), BigInt(schedule.weekday), existing?.enabled === false ? 0n : 1n, BigInt(itemRevision)]);
      const storeKey = accountScopeKey(client, cid, context.requestId);
      const saved = await runAnnouncementMutation(client, cid, context.requestId, 'save', 51,
        [existing === undefined ? null : fromHex(existing.id), encoded], listed.revision, scope);
      if (saved.status === 0) {
        if (!validateCurrent(context, cid, 'manageCommunity')) return uncertain('The schedule was accepted, but Manage Community permission changed before confirmation. Retry this same attempt when permission is restored.');
        if (!sameInteractionIdentity(saved.scope, scope)) return uncertain('The schedule save was accepted, but the Bot installation changed before its result could be confirmed.');
        if (!confirmedAnnouncementMutation(saved, listed.revision)) return uncertain('The Bot accepted the schedule, but its revision receipt was malformed. Refresh the list before retrying.');
        announcementAttemptStore(client).delete(storeKey);
        return ok(existing === undefined ? 'Announcement scheduled.' : 'Announcement updated.');
      }
      if (saved.status === 5) return uncertain('The Bot reported an ambiguous schedule save. Retry this same attempt to reconcile it; the schedule was not resent.');
      announcementAttemptStore(client).delete(storeKey);
      return saved.status === 4 ? fail('The schedule changed before saving. Refresh the announcement list and try again.')
        : fail(saved.status === 2 ? 'Manage Community permission is required.' : 'The announcement could not be saved.');
    } catch (error) {
      const result = resultError(error);
      const currentAttempt = announcementAttemptStore(client).get(attemptKey);
      return currentAttempt?.sent === true
        ? uncertain('This schedule mutation may have been sent. Its original request and body remain reserved; retry this same request to reconcile it.')
        : result;
    }
  }, [client, communityId, openDialog]);

  const rolePanelHandler = useCallback(async (_invocation: CommandInvocation, context: CommandExecutionContext): Promise<CommandResult> => {
    const cid = context.binding.communityId;
    if (cid === null || cid !== communityId) return fail('Role panels are available only in this community.');
    const accountScope = `${client.get('account')?.instance?.id ?? ''}:${client.get('account')?.user?.id ?? ''}`;
    const roleAttemptKey = `${accountScope}:${cid}:${context.requestId}`;
    let pending = rolePanelAttempts(client).get(roleAttemptKey);
    if (!validateCurrent(context, cid, 'manageRoles')) return pending?.sent === true || pending?.published === true
      ? uncertain('This role panel attempt is still pending. Restore Manage Roles permission in its original community to reconcile publication or registration.')
      : fail('Manage Roles permission is required.');
    try {
      if (pending === undefined) {
        const api = createBotInteractionClient(client, cid);
        const scopeRead = await api.request({ requestId: childId(context.requestId, 'rolepanel:scope'), operation: 3, args: [] });
        if (!context.isCurrent() || !validateCurrent(context, cid, 'manageRoles')) return fail('The community or permission changed before opening the role panel.');
        if (scopeRead.status !== 0) return fail('The Bot installation is unavailable for role panel publication.');
        const roles = await client.call({ m: 'listSelfRoles', communityId: cid }) as { id: string; name: string }[];
        if (!context.isCurrent() || !validateCurrent(context, cid, 'manageRoles')) return fail('The community or permission changed before opening the role panel.');
        if (roles.length === 0) return fail('Approve at least one self-service role before creating a panel.');
        const value = await openDialog<Extract<BotCommandDialogState, { kind: 'rolepanel' }>>({ kind: 'rolepanel', communityId: cid, channels: clientChannels(client, cid).filter(row => row.kind === 0), roles }, context);
        if (value === null || !context.isCurrent()) return fail('Role panel creation cancelled.');
        if (!validateCurrent(context, cid, 'manageRoles')) return fail('Manage Roles permission changed before publication.');
      if (!clientChannels(client, cid).some(channel => channel.id === value.rolePanel.channelId && channel.kind === 0)) return fail('Choose a text channel that still exists in this community.');
      const currentRoles = await client.call({ m: 'listSelfRoles', communityId: cid }) as { id: string; name: string }[];
      if (!context.isCurrent() || !validateCurrent(context, cid, 'manageRoles')) return fail('The community or permission changed before role panel publication.');
      if (value.rolePanel.roles.some(role => !currentRoles.some(current => current.id === role.id && current.name === role.name)))
        return fail('A selected role is no longer approved for self-service. Reopen the role panel editor and choose current roles.');
        const selected = value.rolePanel;
        const options = selected.roles.map(role => [selected.emojis[role.id] ?? '', fromHex(role.id), role.name]);
        const payload = encode(options);
        pending = { communityId: cid, channelId: selected.channelId, roles: selected.roles, emojis: selected.emojis,
          publishId: childId(context.requestId, 'rolepanel:publish'), registerId: childId(context.requestId, 'rolepanel:register'), payload,
          scope: scopeRead.scope, sent: false, published: false, bodyKnown: true, accepted: null };
        rolePanelAttempts(client).set(roleAttemptKey, pending);
        while (rolePanelAttempts(client).size > 128) rolePanelAttempts(client).delete(rolePanelAttempts(client).keys().next().value!);
      }
      if (pending.accepted === null) {
        const api = createBotInteractionClient(client, cid);
        let published;
        if (pending.sent) {
          try { published = await api.read(pending.publishId, 31); }
          catch (error) { throw new BotInteractionUncertainError(pending.publishId,
            `Could not reconcile the prior role panel publication: ${error instanceof Error ? error.message : 'read failed'}`); }
          if (published === null) return uncertain('The role panel publication may still be pending. Retry to reconcile the same request; it will not publish a second panel.');
        } else {
          try { published = await api.read(pending.publishId, 31); }
          catch (error) { throw new BotInteractionUncertainError(pending.publishId,
            `Could not reconcile the prior role panel request ID: ${error instanceof Error ? error.message : 'read failed'}`); }
          if (published !== null) {
            pending.sent = true;
            pending.published = true;
            pending.bodyKnown = false;
            return uncertain('A prior panel publication exists for this command ID, but its original role selection is no longer available. Inspect the channel before creating another panel.');
          }
          pending.sent = true;
          published = await api.request({ requestId: pending.publishId, operation: 31, args: [pending.channelId, pending.payload], revision: '0', expectedScope: pending.scope });
        }
        if (published.status !== 0) {
          if (published.status === 2) { rolePanelAttempts(client).delete(roleAttemptKey); return fail('Manage Roles permission is required.'); }
          return uncertain('The role panel publication result is not confirmed. Retry this command to reconcile it.');
        }
        if (pending.scope !== undefined && (pending.scope.installationId !== published.scope.installationId
          || pending.scope.botUserId !== published.scope.botUserId))
          return uncertain('The Bot installation changed after publication. Verify the channel before continuing registration.');
        pending.published = true;
        pending.scope = published.scope;
        let decoded;
        try { decoded = decode(published.data); }
        catch { return uncertain('The role panel was published, but its receipt could not be decoded. Retry this same attempt to reconcile it.'); }
        if (!Array.isArray(decoded) || decoded.length !== 2 || !(decoded[0] instanceof Uint8Array) || typeof decoded[1] !== 'bigint'
          || toHex(decoded[0]) !== pending.channelId || decoded[1] < 1n) return uncertain('The role panel was accepted with an invalid receipt. Inspect the channel before retrying.');
        pending.accepted = { channelId: pending.channelId, messageSeq: decoded[1].toString() };
      }
      if (pending?.accepted !== null && pending?.accepted !== undefined) {
        const receipt = await createBotInteractionClient(client, cid).read(pending.publishId, 31);
        if (receipt === null || pending.scope === undefined || receipt.scope.installationId !== pending.scope.installationId
          || receipt.scope.botUserId !== pending.scope.botUserId)
          return uncertain('The panel message was accepted, but the Bot installation binding changed before registration. Verify the channel and reconcile safely.');
      }
      if (pending?.bodyKnown === false) return uncertain('The accepted panel cannot be matched to the current role selection. Inspect the channel before retrying.');
      if (!context.isCurrent() || !validateCurrent(context, cid, 'manageRoles')) return uncertain('The role panel was published, but current authority changed before registration. Retry to finish registration.');
      if (pending?.accepted === null || pending?.accepted === undefined) return uncertain('The role panel publication is not confirmed yet.');
      const registrationScope = await createBotInteractionClient(client, cid).request({
        requestId: randomId(), operation: 3, args: [],
      });
      if (registrationScope.status !== 0 || pending.scope === undefined || !sameInteractionIdentity(registrationScope.scope, pending.scope))
        return uncertain('The original Bot installation must be ready before panel registration can finish.');
      if (!context.isCurrent() || !validateCurrent(context, cid, 'manageRoles'))
        return uncertain('Current authority changed before panel registration. Retry to finish registration.');
      const acceptedPending = pending;
      const acceptedPanel = pending.accepted;
      const registration = { m: 'registerRolePanel' as const, communityId: cid, requestId: acceptedPending.registerId, channelId: acceptedPending.channelId,
        messageSeq: acceptedPanel.messageSeq, options: acceptedPending.roles.map(role => ({ emoji: acceptedPending.emojis[role.id] ?? '', roleId: role.id })) };
      await client.call(registration);
      if (!context.isCurrent() || !validateCurrent(context, cid, 'manageRoles'))
        return uncertain('The panel registration was sent, but the current command context or permission changed before confirmation. Retry to reconcile this same registration ID.');
      rolePanelAttempts(client).delete(roleAttemptKey);
      return ok('Role panel published and registered.');
    } catch (error) {
      const result = resultError(error);
      if (pending?.sent === true && result.status === 'uncertain')
        return uncertain('This role panel publication or registration may already have been sent. Retry this same request to reconcile it; do not start another publication.');
      return pending?.published === true || pending?.accepted !== null && pending?.accepted !== undefined
        ? uncertain('The panel message was accepted, but local registration is not confirmed. Retry this command to finish registration without publishing again.')
        : result;
    }
  }, [client, communityId, openDialog]);

  const handlers = useMemo<CommandProviderHandlers>(() => ({
    play: musicHandler, pause: musicHandler, resume: musicHandler, skip: musicHandler, stop: musicHandler, queue: musicHandler, nowplaying: musicHandler,
    seek: musicHandler, volume: musicHandler, shuffle: musicHandler, repeat: musicHandler, 'queue remove': musicHandler, 'queue move': musicHandler,
    'queue clear': musicHandler, music: musicHandler, 'music help': musicHandler,
    'announcement list': announcementHandler, 'announcement create': announcementHandler, 'announcement edit': announcementHandler,
    'announcement delete': announcementHandler, 'rolepanel create': rolePanelHandler,
  }), [musicHandler, announcementHandler, rolePanelHandler]);

  const suggest = useCallback<CommandSuggestionProvider>(async (commandId, option, query, binding: Readonly<Binding>, values) => {
    void values;
    if (binding.communityId === null || binding.communityId !== communityId) return [];
    if (option.kind === 'queue-item') {
      const channelId = currentCallChannel(client);
      if (channelId === null || !clientChannels(client, communityId).some(row => row.id === channelId && row.kind === 1)) return [];
      try {
        const adapter = createVoiceMusicAdapter(client, communityId); const state = adapter.state(channelId);
        let queue: readonly { id: string; track: { title: string; artist: string } }[] = [];
        if (state.status === 'available') queue = state.queue;
        else { await adapter.refresh(channelId); const refreshed = adapter.state(channelId); if (refreshed.status === 'available') queue = refreshed.queue; }
        const q = query.toLocaleLowerCase();
        return queue.map((item, index) => ({ item, position: index + 1 }))
          .filter(({ item }) => item.id === query || item.track.title.toLocaleLowerCase().includes(q) || item.track.artist.toLocaleLowerCase().includes(q))
          .slice(0, 25).map(({ item, position }) => ({ id: item.id, value: item.id, label: item.track.title, description: `${item.track.artist} · position ${position}` }));
      } catch { return []; }
    }
    if (option.kind === 'announcement') {
      try {
        const listed = await listAnnouncements(client, communityId, randomId()); const q = query.toLocaleLowerCase();
        return listed.rows.filter(row => row.id === query || announcementSummary(row).toLocaleLowerCase().includes(q))
          .slice(0, 25).map(row => ({ id: row.id, value: row.id, label: row.text.slice(0, 100), description: announcementSummary(row) }));
      } catch { return []; }
    }
    if (commandId === 'play' && option.name === 'query-or-url') {
      const trimmed = query.trim();
      const key = `${binding.communityId}:${option.name}`;
      const generation = (staleSearch.current.get(key) ?? 0) + 1; staleSearch.current.set(key, generation);
      if (trimmed.length < 2 || MUSIC_LINK_PREFIX.test(trimmed)) return [];
      const channelId = currentCallChannel(client);
      if (channelId === null) return [];
      await new Promise(resolve => setTimeout(resolve, 250));
      if (staleSearch.current.get(key) !== generation || currentCallChannel(client) !== channelId) return [];
      try {
        const adapter = createVoiceMusicAdapter(client, communityId);
        const rows = await adapter.search(channelId, trimmed, { requestId: childId(randomId(), `suggest:${trimmed}`) });
        if (staleSearch.current.get(key) !== generation || currentCallChannel(client) !== channelId) return [];
        return rows.slice(0, 25).map(row => ({ id: row.id, value: row.input, label: row.title, description: row.artist }));
      } catch {
        if (staleSearch.current.get(key) !== generation || currentCallChannel(client) !== channelId) return [];
        return Object.assign([], { notice: copy.musicSearchFailed });
      }
    }
    return [];
  }, [client, communityId]);

  return { handlers, suggest, dialogs: createElement(BotCommandDialogs, { dialog }) };
}

interface RolePanelPending { communityId: string; channelId: string; roles: { id: string; name: string }[]; emojis: Record<string, string>;
  publishId: string; registerId: string; payload: Uint8Array; scope?: { installationId: string; botUserId: string | null; revision: string };
  sent: boolean; published: boolean; bodyKnown: boolean; accepted: { channelId: string; messageSeq: string } | null }
const rolePanelStores = new WeakMap<CoreClient, Map<string, RolePanelPending>>();
function rolePanelAttempts(client: CoreClient): Map<string, RolePanelPending> {
  let store = rolePanelStores.get(client); if (store === undefined) { store = new Map(); rolePanelStores.set(client, store); }
  return store;
}
function randomId(): string { const bytes = crypto.getRandomValues(new Uint8Array(16)); if (bytes.every(value => value === 0)) bytes[15] = 1; return toHex(bytes); }
