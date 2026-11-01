import { createContext, useContext } from 'react';
import type { CommandProviderHandlers, CommandSuggestionProvider } from './command-types.ts';
import type { CallView } from '../voice/useCall.tsx';
import type { Route } from '../../router.ts';

export interface CommandRuntime {
  call: CallView;
  getCall?(): CallView;
  navigate(route: Route): void;
  openThread(rootMsgId: string): void;
  openSearch(query: string, scope: 'channel' | 'community' | 'all'): void;
  openModerationLog?(): void;
}
export interface CommandProviderValue {
  handlers?: CommandProviderHandlers;
  suggest?: CommandSuggestionProvider;
  runtime?: CommandRuntime;
}

const CommandContext = createContext<CommandProviderValue>({});

export function CommandProvider(props: CommandProviderValue & { children: React.ReactNode }): React.JSX.Element {
  return <CommandContext.Provider value={{ handlers: props.handlers, suggest: props.suggest, runtime: props.runtime }}>{props.children}</CommandContext.Provider>;
}

export function useCommandProviders(): CommandProviderValue { return useContext(CommandContext); }
