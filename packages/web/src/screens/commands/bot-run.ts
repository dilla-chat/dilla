// Bot v2 (task 20): a Bot command from the community's catalog, run as one sealed command. Its arguments are sent as the
// catalog's shapes say (text, a whole number, a member or channel id, seconds, on/off); its answers stream into the private
// result row ("Searching…", then "Now playing: …"); with no first answer in 5 s the run is uncertain and the composer keeps
// the draft with a Retry that reuses the request id. Nothing here polls: the answers come with the socket.
import { botCommands, fromHex, parseReminderTime, type BotAnswer, type BotArg, type BotCatalogView, type BotCommandRun, type CommandDefinition, type CommandInvocation,
  type CommandResult } from '@dilla/client-core';
import { t } from '../../strings/index.ts';
import type { CommandExecutionContext, CommandHandler } from './command-types.ts';
import { commandPickerCopy } from './copy.ts';
import { MUSIC_COMMANDS, registryDuration } from './picker-catalog.ts';
import { browserTimeZone } from './picker-when.ts';

const ID = /^[0-9a-f]{32}$/u;
const SCALE: Readonly<Record<string, number>> = { ms: 0.001, s: 1, m: 60, h: 3600, d: 86_400, w: 604_800 };

/** "06:24": the local clock time of a Bot status's since. */
export function botSince(since: number | null): string {
  if (since === null) return '—';
  return new Date(since * 1000).toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' });
}

/**
 * Why a catalog command is greyed for this member, or undefined when it can run. A Bot that is not ready greys every
 * command with its state and since; a key this device cannot check, or a Bot account other than the one it pinned, has its
 * own reason; a command whose result is a channel post needs a Bot channel this member can view. Commands never vanish.
 */
export function botCommandReason(view: BotCatalogView | null | undefined, definition: CommandDefinition): string | undefined {
  if (view === null || view === undefined || definition.bot === undefined) return undefined;
  switch (view.status.state) {
    case 'degraded': return t('commands.botDegraded', { time: botSince(view.status.since) });
    case 'offline': return t('commands.botOffline', { time: botSince(view.status.since) });
    case 'disabled': return t('commands.botDisabled');
    case 'absent': return t('commands.botNotOnInstance');
    case 'unverified': return t('commands.botUnverified');
    case 'changed': return t('commands.botChanged');
    case 'ready': break;
  }
  if (definition.bot.needsBotChannel && (view.botChannels ?? []).length === 0) return t('commands.needsBotChannel');
  return undefined;
}

/** Whole seconds of a registry duration ("90s", "1h30m") or a spoken one ("10 minutes"); null otherwise. */
function durationSeconds(text: string): number | null {
  const form = registryDuration(text);
  if (form === null) return null;
  const parts = [...form.matchAll(/(\d+(?:\.\d+)?)(ms|s|m|h|d|w)/gu)];
  const seconds = Math.round(parts.reduce((sum, part) => sum + Number(part[1]) * SCALE[part[2]], 0));
  return Number.isSafeInteger(seconds) && seconds >= 0 ? seconds : null;
}

/** The sealed arguments of an invocation, in the catalog's order, or the problem to show. */
export function botArgsOf(definition: CommandDefinition, args: Readonly<Record<string, string>>, now: Date): BotArg[] | { error: string } {
  const types = definition.bot?.argTypes ?? [];
  const out: BotArg[] = [];
  for (const [index, option] of definition.options.entries()) {
    const value = args[option.name];
    if (value === undefined || value.trim() === '') { out.push(null); continue; }
    switch (types[index]) {
      case 1: {
        if (!/^\d+$/u.test(value.trim()) || !Number.isSafeInteger(Number(value.trim()))) return { error: `${option.name} must be a whole number` };
        out.push(Number(value.trim()));
        break;
      }
      case 2: case 3: {
        if (!ID.test(value)) return { error: `Choose ${option.name} from the suggestions.` };
        out.push(fromHex(value));
        break;
      }
      case 4: {
        if (option.kind === 'time') {
          // The picker's when chip: a duration or a local date and time (an ambiguous DST time carries its chosen instant).
          const chosen = args.chosen === undefined ? null : Number(args.chosen);
          const parsed = parseReminderTime(value, args.timezone ?? browserTimeZone(), now);
          const nowS = Math.floor(now.getTime() / 1000);
          const seconds = chosen !== null && Number.isSafeInteger(chosen) ? chosen - nowS
            : parsed.kind === 'valid' ? (parsed.mode === 'relative' ? parsed.seconds : parsed.unixSeconds - nowS) : null;
          if (seconds === null || seconds < 0) return { error: commandPickerCopy.whenHint };
          out.push(seconds);
        } else {
          const seconds = durationSeconds(value);
          if (seconds === null) return { error: `${option.name} must be a duration such as 10m or 2h` };
          out.push(seconds);
        }
        break;
      }
      case 5: out.push(value === 'on'); break;
      default: out.push(value);
    }
  }
  return out;
}

const decoder = new TextDecoder('utf-8', { fatal: true });
function answerText(body: Uint8Array): string | null {
  if (body.length === 0) return null;
  try { return decoder.decode(body); } catch { return null; }
}

/** A refusal code as the result row says it (M3): before the Bot saw the command, or a Bot answer whose body is a code. */
function codeText(code: string, view: BotCatalogView | null | undefined): string | null {
  switch (code) {
    case 'E_BOT_OFFLINE': return t('commands.botOffline', { time: botSince(view?.status.since ?? null) });
    case 'E_NOT_BOT_CHANNEL': return t('commands.needsBotChannel');
    // The command route answers 403 only when the community's Bot is turned off.
    case 'E_BOT_DISABLED': return t('commands.botDisabled');
    case 'E_BOT_KEY_STALE': case 'E_BOT_STALE': return t('commands.botUnverified');
    default: return null;
  }
}

/** The result of one Bot answer: its text, or for a refusal (status ≥ 400) its code's copy. */
function answerResult(answer: BotAnswer, view: BotCatalogView | null | undefined): CommandResult {
  const text = answerText(answer.body);
  if (answer.status < 400) return { status: 'success', private: true, ...(text === null ? {} : { message: text }) };
  const coded = text !== null && /^E_[A-Z_]+$/u.test(text) ? codeText(text, view) ?? (text === 'E_FORBIDDEN' ? t('commands.botForbidden') : null) : null;
  return { status: 'failure', private: true, message: coded ?? (text !== null && !/^E_[A-Z_]+$/u.test(text) ? text
    : answer.status === 403 ? t('commands.botForbidden') : t('privateResult.failed')) };
}

export interface BotRunHooks {
  /** A streamed answer before the last ("Searching…"): the result row shows it. */
  progress(context: CommandExecutionContext, text: string): void;
  /** After a timeout: the first answer that still arrived (M4). */
  late(context: CommandExecutionContext, result: CommandResult): void;
  /** The run started: the host cancels it when the composer closes (M5). */
  started(run: BotCommandRun): void;
}

/**
 * The handler of one catalog command: sends it sealed, shows each answer as it arrives (progress) and returns the last
 * one. A command whose result is a channel post, answered with no text, says where it went.
 */
export function botCommandHandler(definition: CommandDefinition, hooks: BotRunHooks): CommandHandler {
  return async (invocation: CommandInvocation, context: CommandExecutionContext): Promise<CommandResult> => {
    const communityId = context.binding.communityId;
    const bot = definition.bot;
    if (communityId === null || bot === undefined) return { status: 'failure', private: true, message: t('privateResult.failed') };
    const view = context.client.get(`botCatalog:${communityId}`);
    const reason = botCommandReason(view, definition);
    if (reason !== undefined) return { status: 'failure', private: true, message: reason };
    const args = botArgsOf(definition, invocation.args, new Date());
    if (!Array.isArray(args)) return { status: 'failure', private: true, message: args.error };
    // Music has no text channel: the channel is the voice room this device is in, when it is in one.
    const callChannel = MUSIC_COMMANDS.has(definition.id) ? context.getCall().channelId ?? null : null;
    const run = botCommands(context.client).sendBotCommand({ communityId, channelId: callChannel ?? context.binding.channelId,
      operation: bot.operation, args, requestId: context.requestId });
    hooks.started(run);
    let last: string | null = null;
    let ended = false;
    for await (const item of run.answers) {
      if ('kind' in item) {
        if (item.kind === 'timeout') {
          run.onLate(answer => { hooks.late(context, answerResult(answer, context.client.get(`botCatalog:${communityId}`))); });
          return { status: 'uncertain', private: true, message: t('commands.botNoAnswer'), retry: true };
        }
        // The held status said ready, but the Bot is not: read the catalog once so the picker greys with the real reason.
        if (item.code === 'E_BOT_OFFLINE') void context.client.call({ m: 'syncBotCatalog', communityId }).catch(() => undefined);
        return { status: 'failure', private: true, message: codeText(item.code, view)
          ?? (item.status === 403 ? t('commands.botDisabled') : t('privateResult.failed')) };
      }
      if (item.status >= 400) return answerResult(item, view);
      const text = answerText(item.body);
      if (text !== null) last = text;
      if (!item.final) { if (text !== null) hooks.progress(context, text); continue; }
      ended = true;
      break;
    }
    // The composer closed (cancel): no answer is shown for it.
    if (!ended && last === null) return { status: 'uncertain', private: true, message: t('commands.botNoAnswer') };
    if (last === null && bot.needsBotChannel) {
      const channelId = definition.options.find((_, index) => bot.argTypes[index] === 3)?.name;
      const target = channelId === undefined ? undefined : invocation.args[channelId];
      const name = target === undefined ? undefined : context.client.get(`channels:${communityId}`)?.find(row => row.id === target)?.name;
      if (name !== undefined) last = t('commands.postedIn', { what: `/${definition.id}`, channel: name });
    }
    return { status: 'success', private: true, ...(last === null ? {} : { message: last }) };
  };
}
