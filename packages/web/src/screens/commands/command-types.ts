import type { CoreClient } from '@dilla/client-core';
import type { CommandContext as Binding, CommandInvocation, CommandResult, CommandOption } from '@dilla/client-core';
import type { CallView } from '../voice/useCall.tsx';
import type { Route } from '../../router.ts';

export interface CommandExecutionContext {
  /** Stable for retries of one unchanged invocation in one conversation context. */
  requestId: string;
  /** Snapshot captured when execution starts. */
  binding: Readonly<Binding>;
  /** Latest permission snapshot for the same open host, without retargeting this execution. */
  getCurrentBinding(): Readonly<Binding>;
  /** False after this host unmounts or its conversation binding changes. */
  isCurrent(): boolean;
  client: CoreClient;
  /** Reads the live React call view after asynchronous admission/session work. */
  getCall(): CallView;
  call: CallView;
  navigate(route: Route): void;
  openThread(rootMsgId: string): void;
  openSearch(query: string, scope: 'channel' | 'community' | 'all'): void;
  items: readonly import('@dilla/client-core').TimelineItem[];
  selfId: string | null;
  encodeMessage(text: string): string;
  confirm(title: string, body: string): Promise<boolean>;
  openInvite(): Promise<boolean>;
  openGif(query: string): void;
  openModerationLog(): void;
}

export type CommandHandler = (invocation: CommandInvocation, context: CommandExecutionContext) => Promise<CommandResult>;
export type CommandProviderHandlers = Readonly<Partial<Record<string, CommandHandler>>>;
export interface CommandSuggestion { id: string; label: string; description?: string; value: string }
/** A provider-owned list may include a nonselectable discovery notice. Undefined means unhandled. */
export type CommandSuggestionList = readonly CommandSuggestion[] & { readonly notice?: string };
export type CommandSuggestionProvider = (commandId: string, option: CommandOption, query: string, context: Readonly<Binding>, values?: Readonly<Record<string, string>>) => Promise<CommandSuggestionList | undefined>;

export interface CommandProviderValue {
  handlers?: CommandProviderHandlers;
  suggest?: CommandSuggestionProvider;
}
