import { canModerate, COMMANDS, commandById, formatClassicCommandMessage, PERM, resolveCommunity, topOf, type CommandContext as Binding, type CommandInvocation, type CommandResult, type RoleSummary, type TimelineItem } from '@dilla/client-core';
import type { CoreClient } from '@dilla/client-core';
import type { CommandExecutionContext, CommandHandler, CommandProviderHandlers } from './command-types.ts';
import { excerpt } from '../shell-model.ts';
import { resolveManualRemoval, sendManualRemovalEvidence } from '../conversation/manual-removal.ts';
import type { EvidenceUpload } from '../conversation/manual-removal.ts';

const result = (status: CommandResult['status'], message: string, isPrivate = true): CommandResult => ({ status, message, private: isPrivate });
const success = (message: string, isPrivate = true) => result('success', message, isPrivate);
const failure = (message: string) => result('failure', message);
const definitiveMutationCodes = new Set(['E_BAD_INPUT', 'E_FORBIDDEN', 'E_NOT_FOUND', 'E_UNAUTHORIZED', 'E_CONFLICT', 'E_RATE_LIMITED', 'E_BOT_PERMISSION', 'E_BOT_BAD_INPUT']);
function mutationError(error: unknown): CommandResult {
  const code = typeof error === 'object' && error !== null && 'code' in error ? String(error.code) : '';
  const status = typeof error === 'object' && error !== null && 'status' in error ? Number(error.status) : 0;
  if (definitiveMutationCodes.has(code) || (status >= 400 && status < 500))
    return failure(code ? `Command was refused (${code}).` : 'Command was refused.');
  return result('uncertain', code ? `The command result is uncertain (${code}). Check the current server state before retrying.` : 'The command result is uncertain. Check the current server state before retrying.');
}
const targetThread = (binding: Readonly<Binding>) => binding.threadId === undefined ? {} : { threadId: binding.threadId };
const encodedSize = (value: string) => new TextEncoder().encode(value).length;
const uncertainPublications = new Set<string>();
interface TimeoutAttempt { communityId: string; userId: string; expires: string; seconds: number; sent: boolean; settled: boolean }
const timeoutAttempts = new WeakMap<CoreClient, Map<string, TimeoutAttempt>>();

function helpText(id: string): string | null {
  const definition = commandById(id.trim());
  if (!definition) return null;
  const usage = definition.options.map(option => `${option.required ? '<' : '['}${option.name}${option.required ? '>' : ']'}`).join(' ');
  const details = definition.options.map(option => `• ${option.name} (${option.kind}${option.required ? ', required' : ', optional'}): ${option.description}${option.choices ? ` Allowed: ${option.choices.join(', ')}.` : ''}${option.min !== undefined || option.max !== undefined ? ` Range: ${option.min ?? '−∞'} to ${option.max ?? '∞'}.` : ''}`).join('\n');
  return `/${definition.id}${usage ? ` ${usage}` : ''}\n${definition.description}${details ? `\n${details}` : ''}`;
}
const COMMANDS_HELP = COMMANDS.map(command => helpText(command.id)!).join('\n\n');

async function publish(context: CommandExecutionContext, text: string, replyTo?: string): Promise<CommandResult> {
  if (text.trim() === '') return failure('Message cannot be empty.');
  const attemptKey = `${context.requestId}\u0000${context.binding.channelId}\u0000${context.binding.threadId ?? ''}\u0000${replyTo ?? ''}\u0000${text}`;
  if (uncertainPublications.has(attemptKey)) return result('uncertain', 'This send attempt has no returned message ID. Check the conversation and outbox before starting another send.', true);
  const encoded = context.encodeMessage(text);
  if (encodedSize(encoded) > 4000) return failure('Message exceeds the 4000 byte limit.');
  try {
    const response = await context.client.call({ m: 'send', channelId: context.binding.channelId, ...targetThread(context.binding), text: encoded,
      ...(replyTo === undefined ? {} : { replyTo }) });
    const msgId = typeof response === 'object' && response !== null && 'msgId' in response ? (response as { msgId?: unknown }).msgId : null;
    return success(typeof msgId === 'string' ? `Sent. Message ${msgId} is in the conversation outbox.` : 'Sent.', false);
  } catch (error) {
    const code = typeof error === 'object' && error !== null && 'code' in error ? String(error.code) : '';
    if (['E_BAD_INPUT', 'E_FORBIDDEN', 'E_NOT_FOUND', 'E_ENVELOPE_LIMIT', 'E_NOT_READY', 'E_TRAY_NOT_READY', 'E_OFFLINE'].includes(code)) return failure(`Message was not sent (${code}).`);
    uncertainPublications.delete(attemptKey); uncertainPublications.add(attemptKey);
    while (uncertainPublications.size > 128) uncertainPublications.delete(uncertainPublications.values().next().value!);
    return result('uncertain', 'The send result is uncertain and this attempt cannot safely be retried. Check the conversation and outbox before starting another send.', true);
  }
}

function exactMessage(context: CommandExecutionContext, id: string): TimelineItem | undefined {
  return context.items.find(item => item.msgId === id && item.state === 'ok' && item.seq !== null);
}

const handler = (fn: (invocation: CommandInvocation, context: CommandExecutionContext) => Promise<CommandResult> | CommandResult): CommandHandler => async (invocation, context) => {
  try { return await fn(invocation, context); }
  catch (error) { return mutationError(error); }
};

const entries: Record<string, CommandHandler> = {
  help: handler(invocation => {
    if (invocation.args.command) {
      const text = helpText(invocation.args.command);
      return text === null ? failure(`Unknown command: ${invocation.args.command}.`) : success(text);
    }
    return success(COMMANDS_HELP);
  }),
  me: handler((invocation, context) => publish(context, `_${invocation.args.text}_`)),
  shrug: handler((invocation, context) => publish(context, formatClassicCommandMessage('shrug', invocation.args.text))),
  tableflip: handler((invocation, context) => publish(context, formatClassicCommandMessage('tableflip', invocation.args.text))),
  unflip: handler((invocation, context) => publish(context, formatClassicCommandMessage('unflip', invocation.args.text))),
  code: handler((invocation, context) => {
    const language = invocation.args.language.trim();
    if (!/^[\w+-]{1,32}$/.test(language)) return failure('Language must be 1 to 32 letters, digits, underscores, plus signs, or hyphens.');
    if (invocation.args.text.includes('```')) return failure('Code text cannot contain a closing code fence.');
    return publish(context, `\`\`\`${language}\n${invocation.args.text}\n\`\`\``);
  }),
  giphy: handler((invocation, context) => {
    if (context.binding.communityId === null) return failure('GIF search is available only in a community channel.');
    context.openGif(invocation.args.query ?? '');
    return success('GIF search opened.');
  }),
  invite: handler(async (_invocation, context) => {
    if (context.binding.communityId === null || ((context.getCurrentBinding().bits ?? 0) & PERM.createInvite) === 0) return failure('You cannot create invites here.');
    const created = await context.openInvite();
    return created ? success('Invite created and shown privately.') : failure('Invite creation cancelled.');
  }),
  join: handler(async (invocation, context) => {
    const communityId = requireCommunity(context);
    const target = context.client.get(`channels:${communityId}`)?.find(channel => channel.id === invocation.args['voice-channel'] || channel.id === invocation.args.channel);
    if (target?.kind !== 1) return failure('Choose an accessible voice channel.');
    context.getCall().join(target.id);
    return await waitForVoiceJoin(context, target.id);
  }),
  dm: handler(async (invocation, context) => {
    const value = await context.client.call({ m: 'openDm', userId: invocation.args.user });
    const channelId = typeof value === 'object' && value !== null ? (value as { channelId?: unknown }).channelId : null;
    if (typeof channelId !== 'string') return failure('Could not open that direct message.');
    if (!context.isCurrent()) return result('uncertain', 'The direct message was opened, but navigation was skipped because the conversation changed.', true);
    context.navigate({ name: 'dm', channelId });
    return success('Direct message opened.');
  }),
  search: handler((invocation, context) => {
    const scopeRaw = invocation.args.scope ?? 'channel';
    if (!['channel', 'community', 'all'].includes(scopeRaw)) return failure('Scope must be channel, community, or all.');
    const scope = scopeRaw as 'channel' | 'community' | 'all';
    if (scope === 'community' && context.binding.communityId === null) return failure('Community search is unavailable here.');
    context.openSearch(invocation.args.query, scope);
    return success('Search opened.');
  }),
  topic: handler(async (invocation, context) => {
    if (context.binding.communityId === null || ((context.getCurrentBinding().bits ?? 0) & PERM.manageChannels) === 0) return failure('You cannot manage this channel.');
    await context.client.call({ m: 'updateChannel', channelId: context.binding.channelId, name: null, topic: invocation.args.text, parent: 'keep', slowmode: null });
    return success('Channel topic updated.');
  }),
  thread: handler((invocation, context) => {
    const item = exactMessage(context, invocation.args.message);
    if (context.binding.communityId === null || item?.msgId === null || item === undefined) return failure('That message is not available in this channel.');
    if (!context.isCurrent()) return failure('The conversation changed before opening the thread.');
    context.openThread(item.msgId);
    return success('Thread opened.');
  }),
  reply: handler((invocation, context) => {
    if (exactMessage(context, invocation.args.message) === undefined) return failure('That message is not available in this conversation.');
    return publish(context, invocation.args.text, invocation.args.message);
  }),
  edit: handler(async (invocation, context) => {
    const item = exactMessage(context, invocation.args.message);
    if (item === undefined || item.senderUser !== context.selfId || item.msgId === null) return failure('You can edit only your own loaded message.');
    const encoded = context.encodeMessage(invocation.args.text);
    if (encodedSize(encoded) > 4000) return failure('Message exceeds the 4000 byte limit.');
    await context.client.call({ m: 'editMessage', channelId: context.binding.channelId, msgId: item.msgId, text: encoded });
    return success('Message edited.', false);
  }),
  delete: handler(async (invocation, context) => {
    const item = exactMessage(context, invocation.args.message);
    if (item === undefined || item.msgId === null) return failure('That message is not available in this conversation.');
    if (item.senderUser !== context.selfId && (((context.getCurrentBinding().bits ?? 0) & PERM.manageMessages) === 0 || !invocation.args.reason?.trim())) return failure('Deleting another person’s message requires Manage Messages and a reason.');
    if (!await context.confirm('Delete this message?', item.senderUser === context.selfId ? 'This permanently deletes your message.' : `Remove this message from everyone? Reason: ${invocation.args.reason?.trim() || 'Removed by a moderator.'}`)) return failure('Cancelled.');
    if (!context.isCurrent()) return failure('The conversation changed before deletion.');
    if (item.senderUser === context.selfId) {
      await context.client.call({ m: 'deleteMessage', channelId: context.binding.channelId, msgId: item.msgId, seq: item.seq! });
      return success('Message deleted.', false);
    }
    if (((context.getCurrentBinding().bits ?? 0) & PERM.manageMessages) === 0 || !invocation.args.reason?.trim()) return failure('Deleting another person’s message requires Manage Messages and a reason.');
    return removeOtherMessage(context, item, invocation.args.reason.trim());
  }),
  pin: handler(async (invocation, context) => pinMessage(invocation, context, true)),
  unpin: handler(async (invocation, context) => pinMessage(invocation, context, false)),
  leave: handler((_invocation, context) => {
    const call = context.getCall();
    if (call.channelId === null) return failure('You are not in a voice call.');
    call.leave();
    return success('Left voice.');
  }),
  mute: handler((_invocation, context) => { const view = context.getCall(); return setVoice(context, true, view.session?.state.deafened ?? view.selfDeafened); }),
  unmute: handler((_invocation, context) => { const view = context.getCall(); return setVoice(context, false, view.session?.state.deafened ?? view.selfDeafened); }),
  deafen: handler((_invocation, context) => setVoice(context, true, true)),
  undeafen: handler((_invocation, context) => { const view = context.getCall(); return setVoice(context, view.session?.state.muted ?? view.selfMuted, false); }),
  kick: handler(async (invocation, context) => {
    const communityId = requireCommunity(context);
    const member = moderationTarget(context, invocation.args.user, 'kick');
    if (!member) return failure('This member cannot be kicked with your current permissions and rank.');
    if (!await context.confirm(`Kick ${member.name}?`, `This removes ${member.name} from this community. Confirm to continue.`)) return failure('Cancelled.');
    if (!context.isCurrent() || !moderationTarget(context, member.userId, 'kick')) return failure('Your permissions or the member’s rank changed.');
    await context.client.call({ m: 'kickMember', communityId, userId: member.userId });
    return success(`${member.name} was kicked.`);
  }),
  ban: handler(async (invocation, context) => {
    const communityId = requireCommunity(context);
    const member = moderationTarget(context, invocation.args.user, 'ban');
    if (!member) return failure('This member cannot be banned with your current permissions and rank.');
    const durationText = invocation.args.duration ?? '1d';
    const seconds = durationSeconds(durationText);
    if (seconds === null || seconds < 3600 || seconds > 3_153_600_000) return failure('Ban duration must be from 1h to 100y.');
    const reason = (invocation.args.reason ?? '').trim();
    if (reasonProblem(reason)) return failure('Reason must be at most 512 UTF-8 bytes and contain no control characters.');
    if (!await context.confirm(`Ban ${member.name}?`, `Ban length: ${durationText}. ${reason || 'No reason supplied.'}`)) return failure('Cancelled.');
    if (!context.isCurrent() || !moderationTarget(context, member.userId, 'ban')) return failure('Your permissions or the member’s rank changed.');
    await context.client.call({ m: 'banMember', communityId, userId: member.userId, reason, seconds });
    return success(`${member.name} was banned.`);
  }),
  unban: handler(async (invocation, context) => {
    const communityId = requireCommunity(context);
    const canBan = () => {
      const access = context.client.get(`permissions:${communityId}`);
      return access?.loaded === true && access.banMembers;
    };
    if (!canBan()) return failure('You cannot unban members here.');
    await context.client.call({ m: 'loadBans', communityId });
    const ban = context.client.get(`bans:${communityId}`)?.find(row => row.userId === invocation.args.user);
    if (!ban) return failure('That member is not currently banned.');
    if (!await context.confirm(`Unban ${ban.name}?`, 'This restores the member’s ability to join the community.')) return failure('Cancelled.');
    if (!context.isCurrent() || !canBan()) return failure('Your permissions or conversation changed.');
    await context.client.call({ m: 'loadBans', communityId });
    if (!context.client.get(`bans:${communityId}`)?.some(row => row.userId === ban.userId)) return failure('That member is no longer banned.');
    await context.client.call({ m: 'unbanMember', communityId, userId: ban.userId });
    return success(`${ban.name} was unbanned.`);
  }),
  timeout: handler(async (invocation, context) => {
    const communityId = requireCommunity(context);
    const member = moderationTarget(context, invocation.args.user, 'timeout');
    const seconds = durationSeconds(invocation.args.duration);
    if (!member) return failure('This member cannot be timed out with your current permissions and rank.')
    if (seconds === null || seconds < 60 || seconds > 2_419_200) return failure('Timeout duration must be from 1m to 28d.');
    let attempts = timeoutAttempts.get(context.client);
    if (attempts === undefined) { attempts = new Map(); timeoutAttempts.set(context.client, attempts); }
    const account = context.client.get('account');
    const key = `${account?.instance?.id ?? ''}:${account?.user?.id ?? ''}:${context.requestId}`;
    let attempt = attempts.get(key);
    if (attempt !== undefined && (attempt.communityId !== communityId || attempt.userId !== member.userId || attempt.seconds !== seconds))
      return failure('This timeout request ID already belongs to a different timeout.');
    if (attempt?.sent) {
      // This endpoint does not deduplicate request IDs. An uncertain write is read-only forever:
      // even a missing timeout could be a later moderator removal, so it cannot justify replay.
      const expires = await context.client.call({ m: 'getMemberTimeout', communityId, userId: member.userId });
      if (expires === attempt.expires) { attempt.settled = true; return success(`${member.name} was timed out.`); }
      return result('uncertain', 'The current timeout differs from this attempt. It may have expired or another moderator may have changed it. Review the current timeout before starting a new command.');
    }
    if (!await context.confirm(`Timeout ${member.name}?`, `Restrict this member for ${invocation.args.duration}.`)) return failure('Cancelled.');
    if (!context.isCurrent() || !moderationTarget(context, member.userId, 'timeout')) return failure('Your permissions or the member’s rank changed.');
    if (attempts.size >= 128) {
      for (const [oldKey, old] of attempts) { if (old.settled) attempts.delete(oldKey); if (attempts.size < 128) break; }
      if (attempts.size >= 128) return failure('Reconcile a pending timeout before starting another timeout command.');
    }
    attempt = { communityId, userId: member.userId, expires: String(Math.floor(Date.now() / 1000) + seconds), seconds, sent: true, settled: false };
    attempts.set(key, attempt);
    try {
      await context.client.call({ m: 'setMemberTimeout', communityId, userId: member.userId, requestId: context.requestId, expires: attempt.expires });
      attempt.settled = true;
    } catch (error) {
      if (mutationError(error).status === 'failure') attempts.delete(key);
      throw error;
    }
    return success(`${member.name} was timed out.`);
  }),
  untimeout: handler(async (invocation, context) => {
    const communityId = requireCommunity(context);
    const member = moderationTarget(context, invocation.args.user, 'timeout');
    if (!member) return failure('This member cannot be timed out with your current permissions and rank.');
    const expires = await context.client.call({ m: 'getMemberTimeout', communityId, userId: member.userId });
    if (typeof expires !== 'string') return failure(`${member.name} is not timed out.`);
    if (!await context.confirm(`Remove ${member.name}’s timeout?`, 'This restores the member’s ability to send messages.')) return failure('Cancelled.');
    if (!context.isCurrent() || !moderationTarget(context, member.userId, 'timeout')) return failure('Your permissions or the member’s rank changed.');
    await context.client.call({ m: 'clearMemberTimeout', communityId, userId: member.userId, requestId: context.requestId });
    return success(`${member.name}’s timeout was removed.`);
  }),
  warn: handler(async (invocation, context) => {
    const communityId = requireCommunity(context);
    const member = moderationTarget(context, invocation.args.user, 'timeout');
    const reason = invocation.args.reason.trim();
    if (!member) return failure('This member cannot be warned with your current permissions and rank.');
    if (reason.length === 0 || new TextEncoder().encode(reason).length > 512 || /[\u0000-\u0008\u000b\u000c\u000d-\u001f\u007f-\u009f]/.test(reason)) return failure('Enter a reason of at most 512 UTF-8 bytes without control characters.');
    if (!await context.confirm(`Warn ${member.name}?`, reason)) return failure('Cancelled.');
    if (!context.isCurrent() || !moderationTarget(context, member.userId, 'timeout')) return failure('Your permissions or the member’s rank changed.');
    await context.client.call({ m: 'warnMember', communityId, userId: member.userId, requestId: context.requestId, reason });
    return success(`${member.name} was warned.`);
  }),
  'role add': handler(async (invocation, context) => updateMemberRole(invocation, context, true)),
  'role remove': handler(async (invocation, context) => updateMemberRole(invocation, context, false)),
  'selfrole list': handler(async (_invocation, context) => {
    const communityId = requireCommunity(context);
    const rows = await context.client.call({ m: 'listSelfRoles', communityId }) as { id: string; name: string; selfSelected: boolean }[];
    return success(rows.length ? rows.map(row => `${row.selfSelected ? '[x]' : '[ ]'} ${row.name}`).join(' · ') : 'No self-assignable roles are available.');
  }),
  'selfrole add': handler(async (invocation, context) => selfRole(invocation, context, true)),
  'selfrole remove': handler(async (invocation, context) => selfRole(invocation, context, false)),
  modlog: handler((_invocation, context) => {
    if (context.binding.communityId === null || ((context.getCurrentBinding().bits ?? 0) & PERM.viewAuditLog) === 0) return failure('Moderation history needs a community and View Audit Log permission.');
    context.openModerationLog();
    return success('Moderation history opened.');
  }),
};



function waitForVoiceJoin(context: CommandExecutionContext, channelId: string): Promise<CommandResult> {
  const read = () => context.getCall();
  const current = read();
  if (current.channelId === channelId && current.session !== null && !current.joining) return Promise.resolve(success('Joined voice.'));
  return new Promise(resolve => {
    let settled = false;
    let timer = 0;
    let deadline = 0;
    const finish = (value: CommandResult) => { if (settled) return; settled = true; clearInterval(timer); clearTimeout(deadline); resolve(value); };
    const check = () => {
      if (!context.isCurrent()) { finish(result('uncertain', 'Voice join is still running after this conversation changed.', true)); return; }
      const state = read();
      if (state.channelId === channelId && state.session !== null && !state.joining) finish(success('Joined voice.'));
      else if (state.notice?.channelId === channelId && state.session === null && !state.joining) finish(failure(`Could not join voice (${state.notice.key}).`));
    };
    timer = window.setInterval(check, 50);
    deadline = window.setTimeout(() => finish(result('uncertain', 'Voice join is still pending. Check the call before trying again.', true)), 35_000);
    check();
  });
}

function requireCommunity(context: CommandExecutionContext): string {
  if (context.binding.communityId === null) throw new Error('This command needs a community channel.');
  return context.binding.communityId;
}

type ModerationAction = 'kick' | 'ban' | 'timeout';
function moderationTarget(context: CommandExecutionContext, id: string, action: ModerationAction): { userId: string; name: string } | undefined {
  const communityId = context.binding.communityId;
  if (communityId === null || context.selfId === null) return undefined;
  return eligibleModerationTarget(context.client, communityId, context.selfId, id, action);
}

export function eligibleModerationTarget(client: CoreClient, communityId: string, selfId: string, id: string, action: ModerationAction): { userId: string; name: string } | undefined {
  const access = client.get(`permissions:${communityId}`);
  const community = client.get('communities')?.find(row => row.id === communityId);
  const roles = client.get(`roles:${communityId}`);
  const members = client.get(`members:${communityId}`);
  const target = members?.find(row => row.userId === id);
  const self = members?.find(row => row.userId === selfId);
  if (access?.loaded !== true || !community || !roles || !target || !self) return undefined;
  const permitted = action === 'kick' ? access.kickMembers : action === 'ban' ? access.banMembers : access.moderateMembers === true;
  if (!permitted) return undefined;
  if (!canModerate({ me: selfId, target: target.userId, owner: community.owner, myTop: topOf(self.roleIds, roles), targetTop: topOf(target.roleIds, roles) })) return undefined;
  return { userId: target.userId, name: target.display || target.username };
}

function reasonProblem(reason: string): boolean {
  return new TextEncoder().encode(reason).length > 512 || /[\u0000-\u0008\u000b\u000c\u000d-\u001f\u007f-\u009f]/.test(reason);
}

async function removeOtherMessage(context: CommandExecutionContext, item: TimelineItem, reason: string): Promise<CommandResult> {
  const communityId = context.binding.communityId;
  if (communityId === null || item.msgId === null || item.seq === null) return failure('That message cannot be removed in this conversation.');
  const safeExcerpt = excerpt(item.body);
  let outcome: { removal?: unknown; proofRequestId?: unknown; evidenceRequestId?: unknown; evidenceUpload?: unknown };
  try {
    outcome = await context.client.call({ m: 'moderateRemoveMessage', channelId: context.binding.channelId, msgId: item.msgId,
      seq: item.seq, reason, excerpt: safeExcerpt }) as typeof outcome;
  } catch (error) {
    const refusal = typeof error === 'object' && error !== null ? error as { code?: unknown; status?: unknown } : {};
    if (['E_BAD_INPUT', 'E_FORBIDDEN', 'E_NOT_FOUND'].includes(String(refusal.code)) || refusal.status === 400 || refusal.status === 403 || refusal.status === 404) {
      return failure('The message could not be removed with the current permissions or message state.');
    }
    return result('uncertain', 'Message removal outcome is uncertain. Check the conversation before retrying.', true);
  }
  const proofRequestId = typeof outcome.proofRequestId === 'string' ? outcome.proofRequestId : null;
  if (outcome.removal === 'rejected') return failure('The message could not be removed.');
  if (outcome.removal === 'unknown') {
    if (proofRequestId === null) return result('uncertain', 'Message removal is unresolved and has no recovery ID.', true);
    const resolved = await resolveManualRemoval(context.client, proofRequestId);
    if (resolved.removal === 'rejected') return failure('The message could not be removed.');
    if (resolved.removal !== 'accepted') return result('uncertain', `Message removal is unresolved. Recovery proof: ${proofRequestId}.`, true);
    let evidenceUpload = resolved.evidenceUpload;
    if (evidenceUpload !== 'accepted' && typeof outcome.evidenceRequestId === 'string') {
      const retry = await sendManualRemovalEvidence(context.client, communityId, { key: item.key, msgId: item.msgId, seq: item.seq, reason, excerpt: safeExcerpt,
        phase: 'result', proofRequestId, evidenceRequestId: outcome.evidenceRequestId, evidenceUpload, removal: 'accepted' });
      evidenceUpload = retry.upload;
    }
    if (evidenceUpload !== 'accepted') return result('uncertain', `Message removal was accepted; moderation evidence is ${evidenceUpload}. Recovery proof: ${proofRequestId}.`, true);
    return success('Message removed and moderation evidence recorded.', false);
  }
  if (outcome.removal !== 'accepted' || proofRequestId === null) return result('uncertain', 'Message removal outcome is malformed or unknown.', true);
  let evidenceUpload: EvidenceUpload = outcome.evidenceUpload === 'accepted' || outcome.evidenceUpload === 'pending' || outcome.evidenceUpload === 'rejected' || outcome.evidenceUpload === 'unknown'
    ? outcome.evidenceUpload : 'unknown';
  if (evidenceUpload !== 'accepted' && typeof outcome.evidenceRequestId === 'string') {
    const retry = await sendManualRemovalEvidence(context.client, communityId, { key: item.key, msgId: item.msgId, seq: item.seq, reason, excerpt: safeExcerpt,
      phase: 'result', proofRequestId, evidenceRequestId: outcome.evidenceRequestId, evidenceUpload, removal: 'accepted' });
    evidenceUpload = retry.upload;
  }
  if (evidenceUpload !== 'accepted') return result('uncertain', `Message removed; moderation evidence is ${evidenceUpload}. Recovery proof: ${proofRequestId}.`, true);
  return success('Message removed and moderation evidence recorded.', false);
}

function assignableRole(context: CommandExecutionContext, userId: string, roleId: string, add: boolean) {
  const communityId = context.binding.communityId;
  if (communityId === null || context.selfId === null) return undefined;
  return eligibleAssignableRole(context.client, communityId, context.selfId, userId, roleId, add ? 'add' : 'remove');
}

export function eligibleAssignableRole(client: CoreClient, communityId: string, selfId: string, userId: string, roleId: string, mode: 'add' | 'remove' = 'add'): { role: RoleSummary; member: { userId: string; name: string } } | undefined {
  const access = client.get(`permissions:${communityId}`);
  const community = client.get('communities')?.find(row => row.id === communityId);
  const roles = client.get(`roles:${communityId}`);
  const members = client.get(`members:${communityId}`);
  const target = members?.find(row => row.userId === userId);
  const self = members?.find(row => row.userId === selfId);
  const role = roles?.find(row => row.id === roleId);
  if (access?.loaded !== true || !access.manageRoles || !community || !roles || !members || !target || !self || !role
    || target.userId === selfId || target.userId === community.owner || access.everyoneId === '' || role.id === access.everyoneId || role.position === 0) return undefined;
  if (mode === 'add' ? target.roleIds.includes(roleId) : !target.roleIds.includes(roleId)) return undefined;
  if (!access.owner && role.position >= access.top) return undefined;
  const mine = access.owner ? 0x3ffffff : resolveCommunity({ owner: community.owner, everyoneId: access.everyoneId, roles, held: self.roleIds }, selfId);
  if (!access.owner && (role.allow & ~mine & 0x3ffffff) !== 0) return undefined;
  return { role, member: { userId: target.userId, name: target.display || target.username } };
}

function durationSeconds(value: string): number | null {
  const parts = [...value.matchAll(/(\d+(?:\.\d+)?)(ms|s|m|h|d|w)/g)];
  if (parts.length === 0 || parts.map(part => part[0]).join('') !== value.trim()) return null;
  const scale: Record<string, number> = { ms: 0.001, s: 1, m: 60, h: 3600, d: 86400, w: 604800 };
  return Math.floor(parts.reduce((sum, part) => sum + Number(part[1]) * scale[part[2]], 0));
}
async function updateMemberRole(invocation: CommandInvocation, context: CommandExecutionContext, add: boolean): Promise<CommandResult> {
  const communityId = requireCommunity(context);
  const target = assignableRole(context, invocation.args.user, invocation.args.role, add);
  if (!target) return failure('This member or role is not assignable with your current permissions and rank.');
  if (!add && !await context.confirm(`Remove ${target.role.name} from ${target.member.name}?`, 'This changes the member’s roles.')) return failure('Cancelled.');
  if (!context.isCurrent()) return failure('The conversation changed before the role update.');
  const current = assignableRole(context, target.member.userId, target.role.id, add);
  if (!current) return failure('Your permissions or role rank changed.');
  await context.client.call({ m: add ? 'grantRole' : 'revokeRole', communityId, userId: current.member.userId, roleId: current.role.id });
  return success(add ? `${current.role.name} added to ${current.member.name}.` : `${current.role.name} removed from ${current.member.name}.`);
}
async function selfRole(invocation: CommandInvocation, context: CommandExecutionContext, enabled: boolean): Promise<CommandResult> {
  const communityId = requireCommunity(context);
  const rows = await context.client.call({ m: 'listSelfRoles', communityId }) as { id: string; name: string; selfSelected: boolean }[];
  const role = rows.find(row => row.id === invocation.args.role);
  if (!role) return failure('That role is not currently self-assignable.');
  if (role.selfSelected === enabled) return success(enabled ? `You already have ${role.name}.` : `You do not have ${role.name}.`);
  await context.client.call({ m: 'selectSelfRole', communityId, roleId: role.id, requestId: context.requestId, enabled });
  return success(enabled ? `${role.name} added.` : `${role.name} removed.`);
}

async function pinMessage(invocation: CommandInvocation, context: CommandExecutionContext, on: boolean): Promise<CommandResult> {
  const item = exactMessage(context, invocation.args.message);
  if (item === undefined || item.msgId === null) return failure('That message is not available in this channel.');
  if (context.binding.threadId !== undefined) return failure('Messages in threads cannot be pinned from the command composer.');
  if (context.binding.communityId !== null && ((context.getCurrentBinding().bits ?? 0) & PERM.pinMessages) === 0) return failure('You cannot pin messages here.');
  if (item.pinned === on) return success(on ? 'Message is already pinned.' : 'Message is already unpinned.');
  await context.client.call({ m: 'pin', channelId: context.binding.channelId, msgId: item.msgId, on });
  return success(on ? 'Message pinned.' : 'Message unpinned.', false);
}

function setVoice(context: CommandExecutionContext, muted: boolean, deafened: boolean): CommandResult {
  let view = context.getCall();
  if (view.channelId === null || view.session === null) return failure('Join a voice call first.');
  let currentDeafened = view.session.state.deafened;
  if (currentDeafened !== deafened) { view.toggleDeafen(); view = context.getCall(); currentDeafened = view.session?.state.deafened ?? view.selfDeafened; }
  const currentMuted = view.session?.state.muted ?? view.selfMuted;
  if (currentMuted !== muted) view.toggleMute();
  return success('Voice settings updated.');
}

export const builtinHandlers: CommandProviderHandlers = Object.freeze(entries);
