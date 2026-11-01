import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { ConfirmDialog, Dialog } from '@dilla/ui';
import { InviteFlow } from '../Create.tsx';
import { ModerationLog } from '../admin/ModerationLog.tsx';
import { COMMANDS, commandAvailability, type CommandContext as Binding, type CommandInvocation, type CommandOption, type CommandResult } from '@dilla/client-core';
import { useCore } from '../../core/context.tsx';
import { useSlices } from '../../core/use-slice.ts';
import { t } from '../../strings/index.ts';
import { useCommandProviders } from './CommandContext.tsx';
import { CommandComposer } from './CommandComposer.tsx';
import { builtinHandlers, eligibleAssignableRole, eligibleModerationTarget } from './builtin.ts';
import type { CommandExecutionContext, CommandSuggestionList } from './command-types.ts';
import type { TimelineItem } from '@dilla/client-core';

interface Confirmation { title: string; body: string }
interface SavedDraft { text: string; requestId: string; invocationKey: string; mode: 'active' | 'canceled' | 'consumed' }
const clientOwners = new WeakMap<object, number>();
let nextClientOwner = 1;
let ownerDrafts = new WeakMap<object, Map<string, Map<string, SavedDraft>>>();
const LIMIT = 128;
export function clearCommandDraftMemory(): void { ownerDrafts = new WeakMap(); }
const requestId = (): string => [...crypto.getRandomValues(new Uint8Array(16))].map(byte => byte.toString(16).padStart(2, '0')).join('');
function memoryFor(client: object, selfId: string | null, accountScope: string): Map<string, SavedDraft> {
  let owner = clientOwners.get(client);
  if (owner === undefined) { owner = nextClientOwner++; clientOwners.set(client, owner); }
  let accounts = ownerDrafts.get(client);
  if (accounts === undefined) { accounts = new Map(); ownerDrafts.set(client, accounts); }
  const accountKey = `${owner}:${accountScope}:${selfId ?? 'anonymous'}`;
  let memory = accounts.get(accountKey);
  if (memory === undefined) { memory = new Map(); accounts.set(accountKey, memory); }
  return memory;
}
function clientOwnerId(client: object): number {
  let owner = clientOwners.get(client);
  if (owner === undefined) { owner = nextClientOwner++; clientOwners.set(client, owner); }
  return owner;
}
function remember(memory: Map<string, SavedDraft>, key: string, value: SavedDraft): void {
  memory.delete(key); memory.set(key, value);
  while (memory.size > LIMIT) memory.delete(memory.keys().next().value!);
}

export interface CommandHostProps {
  communityId: string | null; channelId: string; threadId?: string; bits?: number; items: readonly TimelineItem[]; selfId: string | null;
  draft: string; onDraft(text: string): void; onSendLiteral(text: string): void; onModeChange?(active: boolean): void; encodeMessage(text: string): string; openGif(query: string): void; gifsEnabled: boolean;
  slashSubmission?: { text: string; id: number } | null;
}

export function CommandHost(props: CommandHostProps): React.JSX.Element | null {
  const client = useCore();
  const { draft, slashSubmission, items, selfId, gifsEnabled } = props;
  const propsRef = useRef(props);
  propsRef.current = props;
  const onDraft = useCallback((text: string) => propsRef.current.onDraft(text), []);
  const onModeChange = useCallback((active: boolean) => propsRef.current.onModeChange?.(active), []);
  const encodeMessage = useCallback((text: string) => propsRef.current.encodeMessage(text), []);
  const openGif = useCallback((query: string) => propsRef.current.openGif(query), []);
  const account = client.get('account');
  const accountScope = `${account?.instance?.id ?? ''}:${account?.user?.id ?? ''}`;
  const memory = memoryFor(client, props.selfId, accountScope);
  const providers = useCommandProviders();
  // Builtin suggestions read these live slices. Their arrival or policy changes
  // must also invalidate the composer's cached query without another keystroke.
  const suggestionSources = useSlices(['communities', `members:${props.communityId ?? ''}`,
    `roles:${props.communityId ?? ''}`, `permissions:${props.communityId ?? ''}`]);
  const handlers = providers.handlers;
  const suggestionProvider = providers.suggest;
  const runtime = providers.runtime;
  const binding = useMemo<Binding>(() => ({ communityId: props.communityId, channelId: props.channelId,
    ...(props.threadId === undefined ? {} : { threadId: props.threadId }), ...(props.bits === undefined ? {} : { bits: props.bits }) }),
  [props.communityId, props.channelId, props.threadId, props.bits]);
  const latestBinding = useRef(binding); latestBinding.current = binding;
  const key = `${binding.communityId ?? 'dm'}/${binding.channelId}/${binding.threadId ?? ''}`;
  const [active, setActive] = useState(() => memory.get(key)?.mode === 'active');
  const [commandText, setCommandText] = useState(() => memory.get(key)?.text ?? '');
  const [confirmation, setConfirmation] = useState<Confirmation | null>(null);
  const [modlogOpen, setModlogOpen] = useState(false);
  const [inviteOpen, setInviteOpen] = useState(false);
  const inviteResolve = useRef<((created: boolean) => void) | null>(null);
  const confirmationResolve = useRef<((accepted: boolean) => void) | null>(null);
  const suppress = useRef<string | null>(null);
  const seenSlashSubmission = useRef<number | null>(null);
  const activeRef = useRef(false); activeRef.current = active;
  const currentKey = useRef(key); currentKey.current = key;
  const currentOwner = useRef(memory); currentOwner.current = memory;
  const generation = useRef(0);
  const previousLayoutKey = useRef(key);
  const mounted = useRef(true);
  const contextKey = `${clientOwnerId(client)}\u0000${accountScope}\u0000${props.selfId ?? ''}\u0000${key}`;

  const open = useCallback((text: string) => {
    setCommandText(text);
    setActive(true);
    onModeChange?.(true);
    activeRef.current = true;
    const saved = memory.get(key);
    remember(memory, key, { text, requestId: saved?.text === text ? saved.requestId : requestId(), invocationKey: saved?.text === text ? saved.invocationKey : text, mode: 'active' });
    onDraft('');
  }, [key, memory, onDraft, onModeChange]);
  useLayoutEffect(() => {
    if (previousLayoutKey.current !== key) {
      generation.current++;
      confirmationResolve.current?.(false); confirmationResolve.current = null; setConfirmation(null);
      inviteResolve.current?.(false); inviteResolve.current = null; setInviteOpen(false);
      setModlogOpen(false);
    }
    previousLayoutKey.current = key;
    currentKey.current = key;
    const saved = memory.get(key);
    const shouldActivate = saved?.mode === 'active';
    setActive(shouldActivate);
    activeRef.current = shouldActivate;
    setCommandText(saved?.text ?? '');
    onModeChange?.(shouldActivate);
  }, [key, memory, onModeChange]);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      confirmationResolve.current?.(false); confirmationResolve.current = null;
      inviteResolve.current?.(false); inviteResolve.current = null;
    };
  }, []);
  useLayoutEffect(() => {
    generation.current++;
    confirmationResolve.current?.(false); confirmationResolve.current = null; setConfirmation(null);
    inviteResolve.current?.(false); inviteResolve.current = null; setInviteOpen(false);
    setModlogOpen(false);
    const saved = memory.get(key);
    const shouldActivate = saved?.mode === 'active';
    setCommandText(saved?.text ?? ''); setActive(shouldActivate); activeRef.current = shouldActivate;
    onModeChange?.(shouldActivate);
  }, [client, selfId, accountScope, memory, key, onModeChange]);
  useEffect(() => { mounted.current = true; }, [client, selfId]);
  useEffect(() => {
    const submission = slashSubmission;
    if (submission === undefined || submission === null || submission.id === seenSlashSubmission.current) return;
    seenSlashSubmission.current = submission.id;
    suppress.current = null;
    open(submission.text);
  }, [slashSubmission, open]);
  useEffect(() => {
    const saved = memory.get(key);
    // Suppress only the canceled draft itself. Once the user clears it or types
    // ordinary text, a new slash must open the picker again.
    if (!active && !draft.startsWith('/')) {
      suppress.current = null;
      if (saved?.mode === 'canceled') remember(memory, key, { ...saved, text: draft, mode: 'consumed' });
      return;
    }
    const canceledDraft = saved?.mode === 'canceled' ? saved.text : null;
    if (!active && draft.startsWith('/') && suppress.current !== draft && canceledDraft !== draft) open(draft);
  }, [active, draft, open, memory, key]);
  const definitions = useMemo(() => {
    const extra = handlers ?? {};
    return COMMANDS.filter(command => commandAvailability(command, binding).available).filter(command => (command.id !== 'giphy' || gifsEnabled) && (command.provider === 'builtin'
      ? builtinHandlers[command.id] !== undefined || extra[command.id] !== undefined
      : extra[command.id] !== undefined));
  }, [binding, handlers, gifsEnabled]);
  const execution = useCallback((invocation: CommandInvocation): Promise<CommandResult> => {
    const handler = (handlers ?? {})[invocation.commandId] ?? builtinHandlers[invocation.commandId];
    if (handler === undefined) return Promise.resolve({ status: 'failure', private: true, message: 'This command is temporarily unavailable.' });
    const invocationKey = `${contextKey}\u0000${invocation.commandId}\u0000${JSON.stringify(invocation.args)}`;
    const previous = memory.get(key);
    const attempt = previous?.invocationKey === invocationKey ? previous.requestId : requestId();
    const snapshot = Object.freeze({ ...binding });
    remember(memory, key, { text: commandText, requestId: attempt, invocationKey, mode: 'active' });
    const ownerSnapshot = memory;
    const generationSnapshot = generation.current;
    const isCurrent = () => mounted.current && currentOwner.current === ownerSnapshot && currentKey.current === key && generation.current === generationSnapshot;
    const confirm = (title: string, body: string) => new Promise<boolean>(resolve => { if (!isCurrent()) { resolve(false); return; } confirmationResolve.current?.(false); confirmationResolve.current = resolve; setConfirmation({ title, body }); });
    const openInvite = () => new Promise<boolean>(resolve => { if (!isCurrent()) { resolve(false); return; } inviteResolve.current?.(false); inviteResolve.current = resolve; setInviteOpen(true); });
    const openModerationLog = () => { if (!isCurrent()) return; setModlogOpen(true); runtime?.openModerationLog?.(); };
    const context: CommandExecutionContext = { requestId: attempt, binding: snapshot, getCurrentBinding: () => latestBinding.current, client,
      isCurrent,
      getCall: () => runtime?.getCall?.() ?? runtime?.call ?? ({} as never),
      call: runtime?.call ?? ({} as never), navigate: route => { if (isCurrent()) runtime?.navigate(route); },
      openThread: root => { if (isCurrent()) runtime?.openThread(root); }, openSearch: (query, scope) => { if (isCurrent()) runtime?.openSearch(query, scope); },
      items, selfId, encodeMessage, confirm, openInvite,
      openGif: query => { if (isCurrent()) openGif(query); }, openModerationLog };
    return handler(invocation, context).then(response => {
      if (response.status === 'success' || response.status === 'failure') {
        const row = memory.get(key);
        if (row?.requestId === attempt) remember(memory, key, { ...row, requestId: requestId(), invocationKey: '', mode: 'canceled' });
      }
      return response;
    });
  }, [handlers, contextKey, key, memory, binding, commandText, client, runtime, items, selfId, encodeMessage, openGif]);
  const suggest = useCallback(async (commandId: string, option: CommandOption, query: string, values?: Readonly<Record<string, string>>): Promise<CommandSuggestionList> => {
    const supplied = await suggestionProvider?.(commandId, option, query, binding, values);
    if (supplied !== undefined) return supplied;
    if (option.kind === 'message') return items.filter((item): item is TimelineItem & { msgId: string } => item.state === 'ok' && item.msgId !== null && (item.msgId === query || item.body.toLowerCase().includes(query.toLowerCase())))
      .slice(-20).map(item => ({ id: item.msgId, value: item.msgId, label: item.body.slice(0, 100), description: item.senderUser === selfId ? 'Your message' : undefined }));
    if (option.kind === 'user' && binding.communityId !== null && commandId === 'unban') {
      await client.call({ m: 'loadBans', communityId: binding.communityId });
      return (client.get(`bans:${binding.communityId}`) ?? []).filter(row => row.userId === query || row.name.toLowerCase().includes(query.toLowerCase()))
        .slice(0, 20).map(row => ({ id: row.userId, value: row.userId, label: row.name, description: 'Banned member' }));
    }
    if (option.kind === 'user' && binding.communityId !== null) {
      const communityId = binding.communityId;
      const rows = client.get(`members:${communityId}`) ?? [];
      return rows.filter(row => {
        const name = row.display || row.username;
        if (row.userId !== query && !name.toLowerCase().includes(query.toLowerCase())) return false;
        if (commandId === 'kick' || commandId === 'ban' || commandId === 'timeout' || commandId === 'untimeout' || commandId === 'warn')
          return selfId !== null && eligibleModerationTarget(client, communityId, selfId, row.userId, commandId === 'kick' ? 'kick' : commandId === 'ban' ? 'ban' : 'timeout') !== undefined;
        if (commandId === 'role add' || commandId === 'role remove') return selfId !== null && (client.get(`roles:${communityId}`) ?? []).some(role => eligibleAssignableRole(client, communityId, selfId, row.userId, role.id, commandId === 'role add' ? 'add' : 'remove') !== undefined);
        return true;
      }).slice(0, 20).map(row => ({ id: row.userId, value: row.userId, label: row.display || row.username }));
    }
    if (option.kind === 'user' && binding.communityId === null && commandId === 'dm') {
      const dms = client.get('dms') ?? [];
      const users = new Map<string, string>();
      for (const dm of dms) if (dm.kind === 3) for (const userId of dm.members) if (userId !== selfId) users.set(userId, dm.name);
      return [...users].filter(([userId, name]) => userId === query || name.toLowerCase().includes(query.toLowerCase())).slice(0, 20).map(([userId, name]) => ({ id: userId, value: userId, label: name }));
    }
    if (option.kind === 'role' && binding.communityId !== null && commandId.startsWith('selfrole ')) {
      const rows = await client.call({ m: 'listSelfRoles', communityId: binding.communityId }) as { id: string; name: string }[];
      return rows.filter(row => row.id === query || row.name.toLowerCase().includes(query.toLowerCase())).slice(0, 20).map(row => ({ id: row.id, value: row.id, label: row.name }));
    }
    if (option.kind === 'role' && binding.communityId !== null) {
      const communityId = binding.communityId;
      return (client.get(`roles:${communityId}` as never) as unknown as { id: string; name: string }[] | undefined ?? [])
      .filter(row => row.id === query || row.name.toLowerCase().includes(query.toLowerCase()))
      .filter(row => commandId === 'role add' || commandId === 'role remove'
        ? selfId !== null && values?.user !== undefined && eligibleAssignableRole(client, communityId, selfId, values.user, row.id, commandId === 'role add' ? 'add' : 'remove') !== undefined
        : true)
      .slice(0, 20).map(row => ({ id: row.id, value: row.id, label: row.name }));
    }
    if (option.kind === 'channel' && binding.communityId !== null && commandId === 'join') return (client.get(`channels:${binding.communityId}` as never) as unknown as { id: string; name: string; kind: number; mode: number }[] | undefined ?? [])
      .filter(row => row.kind === 1 && row.mode === 0 && (row.id === query || row.name.toLowerCase().includes(query.toLowerCase()))).slice(0, 20).map(row => ({ id: row.id, value: row.id, label: row.name, description: 'Voice channel' }));
    return [];
  }, [suggestionProvider, binding, items, selfId, client]);
  const cancel = (text: string) => {
    const saved = memory.get(key);
    remember(memory, key, { text, requestId: saved?.requestId ?? requestId(), invocationKey: saved?.invocationKey ?? text, mode: 'canceled' });
    generation.current++;
    confirmationResolve.current?.(false); confirmationResolve.current = null; setConfirmation(null);
    inviteResolve.current?.(false); inviteResolve.current = null; setInviteOpen(false);
    setModlogOpen(false);
    setCommandText(''); setActive(false); onModeChange?.(false); activeRef.current = false;
    suppress.current = text;
    onDraft(text);
  };
  const sendLiteral = (text: string) => {
    const saved = memory.get(key);
    remember(memory, key, { text: '', requestId: saved?.requestId ?? requestId(), invocationKey: '', mode: 'consumed' });
    generation.current++;
    confirmationResolve.current?.(false); confirmationResolve.current = null;
    inviteResolve.current?.(false); inviteResolve.current = null;
    setCommandText(''); setActive(false); onModeChange?.(false); activeRef.current = false;
    props.onSendLiteral(text);
  };
  if (!active) return null;
  const saved = memory.get(key)?.text ?? commandText;
  const community = binding.communityId === null ? null : client.get('communities')?.find(row => row.id === binding.communityId) ?? null;
  return <>{inviteOpen && community !== null ? <InviteFlow open community={{ id: community.id, name: community.name }} onClose={() => { setInviteOpen(false); inviteResolve.current?.(false); inviteResolve.current = null; }} onCreated={() => { inviteResolve.current?.(true); inviteResolve.current = null; }} /> : null}
    {modlogOpen && binding.communityId !== null ? <Dialog open title={t('admin.bot.moderationLog.title')} closeLabel={t('dialog.close')} onClose={() => setModlogOpen(false)}><ModerationLog communityId={binding.communityId} /></Dialog> : null}
    {confirmation !== null ? <ConfirmDialog open title={confirmation.title} body={confirmation.body} confirmLabel="Continue" busyLabel="Working…" cancelLabel="Cancel" busy={false} error={null}
    onConfirm={() => { setConfirmation(null); confirmationResolve.current?.(true); confirmationResolve.current = null; }}
    onClose={() => { setConfirmation(null); confirmationResolve.current?.(false); confirmationResolve.current = null; }} /> : null}<CommandComposer key={contextKey} definitions={definitions} initialText={saved}
    onExecute={execution} onCancel={cancel} onSendLiteral={sendLiteral} suggest={suggest} suggestionSources={suggestionSources} contextKey={contextKey}
    onDraftChange={text => { setCommandText(text); const row = memory.get(key); remember(memory, key, { text, requestId: row?.requestId ?? requestId(), invocationKey: row?.invocationKey ?? text, mode: 'active' }); }} /></>;
}
