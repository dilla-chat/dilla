// Signal wave B Task 2 (README §6.1): the prototype's command picker, in place of the old search-and-dialog composer.
// `/` at the start of the draft lists the commands above the composer; picking one turns the composer's input row into
// chips, one per argument, with suggestions above the active chip and a one-line bar above the chips. The host
// (CommandHost) keeps the draft memory, the request ids and the generation guards; this component keeps the draft, the
// active chip and the one run in flight.
import { privateResultCopy } from './copy.ts';
import { useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { parseCommand, validateCommandArgs, type CommandDefinition, type CommandInvocation, type CommandOption, type CommandResult } from '@dilla/client-core';
import { CommandBar, CommandChips, CommandField, CommandList, CommandSuggestions, Composer, commandOptionId, type CommandListItem, type CommandSuggestion as UiSuggestion, type IconName } from '@dilla/ui';
import { t } from '../../strings/index.ts';
import { commandPickerCopy as copy } from './copy.ts';
import { COMMAND_LIST_WAIT_MS } from './command-bot-store.ts';
import type { CommandSuggestionList } from './command-types.ts';
import { DURATION_SUGGESTIONS, MODERATION_COMMANDS, MUSIC_COMMANDS, PERMANENT, WHEN_SUGGESTIONS, pickerEntry, registryDuration, type PickerEntry, type PickerSuggest } from './picker-catalog.ts';
import { browserTimeZone, resolveWhen, type WhenResult } from './picker-when.ts';

export type { CommandSuggestion } from './command-types.ts';

/** What the picker hands its frame: the popover and bar above the box, the row's content, the `/` draft ('' with chips: send dims as in the prototype) and send. */
export interface PickerFrame { top: ReactNode; input: ReactNode; draft: string; run(): void }

export interface CommandComposerProps {
  definitions: readonly CommandDefinition[];
  initialText: string;
  onUnknown?: (text: string) => void;
  onExecute: (invocation: CommandInvocation) => Promise<CommandResult>;
  onCancel: (text: string) => void;
  onSendLiteral: (text: string) => void;
  suggest: (commandId: string, option: CommandOption, query: string, values?: Readonly<Record<string, string>>) => Promise<CommandSuggestionList>;
  /** Live snapshots that invalidate cached entity queries without editing the draft. */
  suggestionSources?: readonly unknown[];
  /** Preserve the panel and composer focus paint behind a moderation confirmation; the native modal owns focus. */
  moderationConfirmOpen?: boolean;
  contextKey: string;
  disabledReason?: string;
  onDraftChange?: (text: string) => void;
  // ---- Wave B Task 2 (optional; the host passes them) ----
  /** The ids the `/` list shows (a Bot command needs its Bot operation, capabilities.ts); all `definitions` when absent. */
  listed?: ReadonlySet<string>;
  /** The Bot's command list is still loading for the first time: the list says so, and Enter on a name not yet listed waits (at most 10 s). */
  loading?: boolean;
  /** The `/` list opened (the host re-checks the Bot list then); a returned function runs when the list closes. */
  onListOpen?: () => void | (() => void);
  /** The Bot's display name, a Bot command's source tag (the prototype's "Glenn"). */
  botName?: string;
  /** The voice room this device is in, for a music command's bar; null: not in voice. */
  room?: string | null;
  /** A run ended in success: leave command mode with an empty draft, keeping what the run opened; `onCancel('')` when absent. */
  onDone?: () => void;
  /** `poll create` (it opens a form): run without chips, the composer back to an empty draft. */
  onRunDetached?: (invocation: CommandInvocation) => void;
  /** Draws the composer around the picker (ComposerArea's Composer); a bare Composer when absent. */
  frame?: (parts: PickerFrame) => ReactNode;
  /** The reminder's zone (Q10: the browser's) and clock; tests pin them. */
  timeZone?: string;
  now?: () => Date;
}

interface FieldState { text: string; selected?: Picked; suggestions: CommandSuggestionList }
/** A picked suggestion: an entity's stable value, or a DST candidate's instant. */
interface Picked { value: string; label: string; chosen?: number }
interface Sug extends UiSuggestion { value: string; chosen?: number }
interface FieldError { message: string; field: number | null }

const quote = (value: string): string => `"${value.replaceAll('\\', '\\\\').replaceAll('"', '\\"')}"`;
const SLASH_LIST = /^\/[a-z ]*$/iu;
const MAX_SUGGESTIONS = 6;

function emptyFields(command: CommandDefinition, values: Readonly<Record<string, string>> = {}): Record<string, FieldState> {
  return Object.fromEntries(command.options.map(option => [option.name, { text: values[option.name] ?? '', suggestions: [] }]));
}

/** The values a typed `/remind` carries: the longest leading phrase that reads as a time is `when`, the rest is text. */
function remindValues(args: Readonly<Record<string, string>>, timeZone: string, now: Date): Record<string, string> {
  if (args.when === undefined || args.text === undefined || /:/u.test(args.when) && !/^\d{1,2}:\d{2}$/u.test(args.when)) return { ...args };
  const words = `${args.when} ${args.text}`.split(/\s+/u);
  for (let count = Math.min(4, words.length - 1); count >= 1; count--) {
    const when = words.slice(0, count).join(' ');
    const result = resolveWhen(when, timeZone, now, () => '');
    if (result.kind === 'valid' || result.kind === 'ambiguous' || result.kind === 'invalid' && result.reason === 'range') return { when, text: words.slice(count).join(' ') };
  }
  return { ...args };
}

/** The chips' values a typed draft would open (the remind split included), for comparing a round trip. */
function reopened(text: string, command: CommandDefinition, timeZone: string, now: Date): Record<string, string> | null {
  const parsed = parseCommand(text);
  const id = parsed.type === 'invocation' ? parsed.invocation.commandId : parsed.type === 'incomplete' ? parsed.commandId : '';
  if (id !== command.id) return null;
  const args = parsed.type === 'invocation' ? parsed.invocation.args : parsed.type === 'incomplete' ? parsed.validation.args : {};
  return command.id === 'remind' ? remindValues(args, timeZone, now) : { ...args };
}

/**
 * Esc on filled chips: the prototype's `/name value value…` (Review Focus 3), as long as typing it back opens the same
 * chips; otherwise every value with a space or a quote is quoted (`/ban "Bea Smith" "7 days" spam`), so a multi-word
 * duration or a spaced name never shifts into the next chip.
 */
export function leaveText(command: CommandDefinition, values: Readonly<Record<string, string>>, timeZone: string, now: Date): string {
  const filled = command.options.filter(option => (values[option.name] ?? '') !== '');
  const want = Object.fromEntries(filled.map(option => [option.name, values[option.name]]));
  const same = (got: Record<string, string> | null) => got !== null && Object.keys(got).length === filled.length
    && filled.every(option => got[option.name] === want[option.name]);
  const plain = `/${command.id} ${filled.map(option => values[option.name]).join(' ')}`.trimEnd();
  if (same(reopened(plain, command, timeZone, now))) return plain;
  const positional = filled.every((option, index) => option === command.options[index]);
  const quoted = `/${command.id} ${filled.map((option, index) => {
    const value = values[option.name];
    const body = /[\s"'\\]/u.test(value) ? quote(value) : value;
    return positional && index === filled.length - 1 && option.freeText === true && !/["'\\]/u.test(value) && !/\n/u.test(value) ? value : (positional ? body : `${option.name}:${quote(value)}`);
  }).join(' ')}`.trimEnd();
  if (same(reopened(quoted, command, timeZone, now))) return quoted;
  return `/${command.id} ${filled.map(option => `${option.name}:${quote(values[option.name])}`).join(' ')}`.trimEnd();
}

export function CommandPicker(props: CommandComposerProps): React.JSX.Element | null {
  const { initialText, onCancel, onSendLiteral, onDraftChange, definitions } = props;
  const timeZone = props.timeZone ?? browserTimeZone();
  const nowOf = props.now ?? (() => new Date());
  const listId = useId().replace(/:/gu, '-');
  const sugsId = `${listId}-sugs`;
  const original = useRef(initialText);
  const dirty = useRef(false);
  const started = useRef(false);
  const literalSent = useRef(false);
  const contextNow = useRef(props.contextKey);
  const suggestionsGeneration = useRef(new Map<string, number>());
  const submitGeneration = useRef(0);
  const inFlight = useRef(false);
  const draftRef = useRef<HTMLTextAreaElement>(null);
  const fieldRefs = useRef(new Map<number, HTMLInputElement | HTMLTextAreaElement>());

  // The first render reads the draft the host hands over: a typed command opens its chips, anything else the list.
  const [initial] = useState(() => {
    const parsed = parseCommand(initialText);
    const id = parsed.type === 'invocation' ? parsed.invocation.commandId : parsed.type === 'incomplete' && parsed.subcommands === undefined ? parsed.commandId : '';
    const args = parsed.type === 'invocation' ? parsed.invocation.args : parsed.type === 'incomplete' ? parsed.validation.args : {};
    const command = definitions.find(entry => entry.id === id) ?? null;
    const values = command?.id === 'remind' ? remindValues(args, timeZone, nowOf()) : args;
    return { literal: parsed.type === 'literal' && !initialText.startsWith('//'), command, values };
  });
  const [draft, setDraft] = useState(initialText);
  const [selected, setSelected] = useState<CommandDefinition | null>(initial.command);
  const [fields, setFields] = useState<Record<string, FieldState>>(() => initial.command === null ? {} : emptyFields(initial.command, initial.values));
  const [active, setActive] = useState(() => {
    if (initial.command === null) return 0;
    const empty = initial.command.options.findIndex(option => (initial.values[option.name] ?? '') === '');
    return empty < 0 ? 0 : empty;
  });
  const [cmdSel, setCmdSel] = useState(0);
  const [sugSel, setSugSel] = useState(0);
  const [error, setError] = useState<FieldError | null>(null);
  const [listError, setListError] = useState<string | null>(null);
  /** The draft Enter was pressed on while the Bot's list was still loading: it runs when its command is listed. */
  const [waiting, setWaiting] = useState<string | null>(null);
  const [status, setStatus] = useState('');
  const [pending, setPending] = useState(false);
  const [focused, setFocused] = useState(true);
  const fieldsNow = useRef(fields); fieldsNow.current = fields;
  contextNow.current = props.contextKey;

  const definitionsById = useMemo(() => new Map(definitions.map(command => [command.id, command])), [definitions]);
  const botName = props.botName ?? privateResultCopy.author.bot;
  const entries = useMemo(() => new Map(definitions.map(command => [command.id, pickerEntry(command, botName)])), [definitions, botName]);
  const listedCommands = useMemo(() => definitions.filter(command => props.listed === undefined || props.listed.has(command.id)), [definitions, props.listed]);
  const entry: PickerEntry | null = selected === null ? null : entries.get(selected.id) ?? pickerEntry(selected, botName);

  // The draft memory keeps what the chips show (an entity chip's label, not its id): a remount re-resolves the label
  // against the provider's suggestions, so a member renamed meanwhile asks to be chosen again rather than hitting a stale id.
  const buildDraft = useCallback((command: CommandDefinition | null, list: string, values: Record<string, FieldState>): string => {
    if (!dirty.current) return original.current;
    if (command === null) return list;
    const args = command.options.flatMap(option => {
      const value = values[option.name]?.text ?? '';
      return value === '' ? [] : [`${option.name}:${quote(value)}`];
    });
    return `/${command.id}${args.length ? ` ${args.join(' ')}` : ''}`;
  }, []);
  const getDraft = useCallback(() => buildDraft(selected, draft, fields), [buildDraft, selected, draft, fields]);
  useEffect(() => { if (dirty.current) onDraftChange?.(getDraft()); }, [onDraftChange, getDraft]);

  useEffect(() => () => {
    submitGeneration.current++;
    for (const [name, generation] of suggestionsGeneration.current) suggestionsGeneration.current.set(name, generation + 1);
    contextNow.current = '';
  }, []);
  useEffect(() => {
    if (started.current) return;
    started.current = true;
    if (initial.literal) { onCancel(initialText); return; }
    // A typed `/poll create` submitted from the textarea opens its form, as picking it from the list does.
    if (initial.command !== null && props.onRunDetached !== undefined && pickerEntry(initial.command, botName).opensForm) {
      dirty.current = true;
      props.onRunDetached({ commandId: initial.command.id, args: { ...initial.values } });
    }
    // The start runs once; later prop changes are not a new start.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [initial.literal, initialText, onCancel]);

  // A command the context no longer allows (a permission changed) hands its draft back. A Bot operation switched off only
  // drops it from the list: an open form still runs and ends in the handler's "unavailable" failure (Review Focus 4).
  useEffect(() => {
    if (selected === null) return;
    const available = definitionsById.get(selected.id);
    if (available === undefined) { onCancel(buildDraft(selected, draft, fieldsNow.current)); setSelected(null); return; }
    if (available !== selected) setSelected(available);
  }, [definitionsById, selected, draft, onCancel, buildDraft]);

  // Focus follows the phase: the draft field in the list, the active chip in the form.
  useLayoutEffect(() => {
    if (selected === null) {
      const el = draftRef.current;
      if (el !== null && document.activeElement !== el) { el.focus(); el.setSelectionRange(el.value.length, el.value.length); }
      return;
    }
    const el = fieldRefs.current.get(active);
    if (el !== undefined && document.activeElement !== el) el.focus();
  }, [selected, active]);

  // ---- The `/` list ----
  const listOpenNow = useRef(props.onListOpen); listOpenNow.current = props.onListOpen;
  const listState = useMemo((): { open: false } | { open: true; items: CommandDefinition[] } => {
    if (selected !== null || !draft.startsWith('/') || draft.startsWith('//')) return { open: false };
    const parsed = parseCommand(draft);
    const typedId = parsed.type === 'invocation' ? parsed.invocation.commandId : parsed.type === 'incomplete' ? parsed.commandId : '';
    const typedArgs = parsed.type === 'invocation' ? parsed.invocation.args : parsed.type === 'incomplete' ? parsed.validation.args : {};
    if (definitionsById.has(typedId) && Object.keys(typedArgs).length > 0) return { open: false };
    if (!SLASH_LIST.test(draft)) return { open: true, items: [] };
    const query = draft.slice(1).toLowerCase().replace(/\s+/gu, ' ').trim();
    const starts = listedCommands.filter(command => command.id.startsWith(query));
    const contains = listedCommands.filter(command => !command.id.startsWith(query) && command.id.includes(query));
    return { open: true, items: [...starts, ...contains] };
  }, [selected, draft, definitionsById, listedCommands]);
  const listIsOpen = listState.open;
  useEffect(() => {
    if (!listIsOpen) return undefined;
    const closed = listOpenNow.current?.();
    return typeof closed === 'function' ? closed : undefined;
  }, [listIsOpen]);
  const listItems = listState.open ? listState.items : [];
  const listSel = Math.min(cmdSel, Math.max(listItems.length - 1, 0));

  const sendLiteralOnce = (text: string) => {
    if (literalSent.current) return;
    literalSent.current = true;
    onSendLiteral(text);
  };

  const enterCommand = (command: CommandDefinition, values: Readonly<Record<string, string>> = {}) => {
    dirty.current = true;
    const picked = entries.get(command.id) ?? pickerEntry(command, botName);
    if (picked.opensForm && props.onRunDetached !== undefined) { props.onRunDetached({ commandId: command.id, args: {} }); return; }
    const filled = command.id === 'remind' ? remindValues(values, timeZone, nowOf()) : values;
    setSelected(command);
    setFields(emptyFields(command, filled));
    const empty = command.options.findIndex(option => (filled[option.name] ?? '') === '');
    setActive(empty < 0 ? 0 : empty);
    setCmdSel(0); setSugSel(0); setError(null); setStatus('');
  };

  const refuse = (name: string) => {
    const text = copy.notACommand(name.toLowerCase());
    if (props.onUnknown !== undefined) props.onUnknown(text); else setListError(text);
  };

  const enterTyped = (): boolean => {
    const parsed = parseCommand(draft);
    const id = parsed.type === 'invocation' ? parsed.invocation.commandId : parsed.type === 'incomplete' && parsed.subcommands === undefined ? parsed.commandId : '';
    const command = definitionsById.get(id);
    if (command === undefined || (props.listed !== undefined && !props.listed.has(id))) return false;
    enterCommand(command, parsed.type === 'invocation' ? parsed.invocation.args : parsed.type === 'incomplete' ? parsed.validation.args : {});
    return true;
  };

  const draftKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.nativeEvent.isComposing || event.keyCode === 229) return;
    const items = listItems;
    if ((event.key === 'ArrowDown' || event.key === 'ArrowUp') && items.length > 0) {
      event.preventDefault();
      setCmdSel((listSel + (event.key === 'ArrowDown' ? 1 : -1) + items.length) % items.length);
      return;
    }
    if ((event.key === 'Tab' && !event.shiftKey || event.key === 'Enter' && !event.shiftKey) && items.length > 0) {
      event.preventDefault();
      // A fully typed invocation ("/me waves") wins over the highlighted row on Enter.
      if (event.key === 'Enter' && SLASH_LIST.test(draft) && enterTypedExact()) return;
      enterCommand(items[listSel]);
      return;
    }
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault();
      if (event.repeat) return;
      if (draft.startsWith('//')) { sendLiteralOnce(draft.slice(1)); return; }
      if (enterTyped()) return;
      // The prototype's send(): an unknown name says so (Task 6 turns this into a failed private result). While the
      // Bot's list is still loading, a name that is defined but not yet listed waits for it instead.
      const name = draft.slice(1).trim().split(/\s+/u)[0] ?? '';
      if (name !== '' && props.loading === true && definitionsById.has(name.toLowerCase())) { setWaiting(draft); return; }
      if (name !== '') refuse(name);
      return;
    }
    if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      // The prototype clears the draft while the list shows commands; otherwise the draft goes back to the textarea.
      onCancel(listItems.length > 0 ? '' : draft);
    }
  };
  /** Enter on "/leave" with /leave in the list: the exact command, whatever row is highlighted. */
  const enterTypedExact = (): boolean => {
    const id = draft.slice(1).toLowerCase().replace(/\s+/gu, ' ').trim();
    const command = definitionsById.get(id);
    if (command === undefined || (props.listed !== undefined && !props.listed.has(id))) return false;
    enterCommand(command);
    return true;
  };

  const draftChange = (value: string) => {
    dirty.current = true;
    if (!value.startsWith('/')) { onCancel(value); return; }
    setDraft(value); setCmdSel(0); setListError(null);
  };

  // Enter on a Bot command typed before its list arrived: run it once listed; refuse once the load ends without it or after 10 s.
  const waitingNow = useRef({ enterTyped, refuse });
  waitingNow.current = { enterTyped, refuse };
  useEffect(() => {
    if (waiting === null) return;
    const name = waiting.slice(1).trim().split(/\s+/u)[0] ?? '';
    if (draft !== waiting) { setWaiting(null); return; }
    if (waitingNow.current.enterTyped()) { setWaiting(null); return; }
    if (props.loading !== true) { setWaiting(null); waitingNow.current.refuse(name); }
  }, [waiting, draft, props.listed, props.loading]);
  useEffect(() => {
    if (waiting === null) return;
    const timer = window.setTimeout(() => {
      setWaiting(null);
      waitingNow.current.refuse(waiting.slice(1).trim().split(/\s+/u)[0] ?? '');
    }, COMMAND_LIST_WAIT_MS);
    return () => { window.clearTimeout(timer); };
  }, [waiting]);

  // ---- The chips ----
  const options = selected?.options ?? [];
  const activeOption: CommandOption | undefined = options[active];
  const activeField = activeOption === undefined ? undefined : entry?.fields[activeOption.name];
  const activeText = activeOption === undefined ? '' : fields[activeOption.name]?.text ?? '';
  const now = nowOf();
  const whenOf = (text: string): WhenResult => resolveWhen(text, timeZone, now, (index, candidate, preview) =>
    t('reminder.timeChoice', { choice: t(index === 0 ? 'reminder.earlier' : 'reminder.later', { offset: candidate.offsetLabel }), preview }));

  const suggestionsFor = (option: CommandOption | undefined, kind: PickerSuggest | undefined, text: string): Sug[] => {
    if (option === undefined || kind === undefined || selected === null) return [];
    const needle = text.trim().toLowerCase();
    const has = (value: string) => needle === '' || value.toLowerCase().includes(needle);
    let list: Sug[] = [];
    if (kind === 'member' || kind === 'provider') {
      list = (fields[option.name]?.suggestions ?? []).map(row => ({ id: row.id, label: row.label, value: row.value,
        ...(row.description !== undefined ? { sub: row.description } : {}),
        ...(kind === 'member' ? { avatar: { name: row.label } } : { icon: entry?.icon ?? 'hash' }) }));
    } else if (kind === 'duration') {
      list = [...DURATION_SUGGESTIONS, ...(selected.id === 'ban' ? [PERMANENT] : [])].filter(has).map(value => ({ id: value, label: value, value, icon: 'timer' }));
    } else if (kind === 'when') {
      const typed = whenOf(text);
      if (typed.kind === 'ambiguous') list = typed.candidates.map(candidate => ({ id: `at-${String(candidate.unixSeconds)}`, label: candidate.label, value: text, chosen: candidate.unixSeconds, icon: 'alarm-clock' }));
      else list = WHEN_SUGGESTIONS.filter(has).map(value => {
        const result = whenOf(value);
        return { id: value, label: value, value, sub: result.kind === 'valid' ? result.preview : '', icon: 'alarm-clock' };
      });
    } else if (kind === 'choice') {
      list = (option.choices ?? []).filter(has).map(value => ({ id: value, label: value, value, icon: entry?.icon ?? 'hash' }));
    } else if (kind === 'command') {
      list = listedCommands.filter(command => has(command.id)).map(command => {
        const e = entries.get(command.id)!;
        return { id: command.id, label: `/${command.id}`, value: command.id, sub: e.description, icon: e.icon };
      });
    }
    return list.slice(0, MAX_SUGGESTIONS);
  };
  const sg = suggestionsFor(activeOption, activeField?.suggest, activeText);
  const sugIndex = Math.min(sugSel, Math.max(sg.length - 1, 0));
  const activeSelected = activeOption === undefined ? undefined : fields[activeOption.name]?.selected;
  const exact = activeSelected !== undefined && sg.some(s => s.value === activeSelected.value && s.chosen === activeSelected.chosen)
    || sg.some(s => s.chosen === undefined && (s.label.toLowerCase() === activeText.toLowerCase() || s.value.toLowerCase() === activeText.toLowerCase()));
  const sugOpen = selected !== null && (focused || props.moderationConfirmOpen === true) && sg.length > 0;

  // Entity suggestions come from the providers: fetched for a chip with text, and for the active member chip while empty
  // (the prototype lists the members as soon as the chip is active).
  const { suggest: getSuggestions, suggestionSources } = props;
  const suggestionQuery = options.map(option => `${option.name}:${fields[option.name]?.text ?? ''}`).join('\u0000');
  const contextKey = props.contextKey;
  useEffect(() => {
    if (selected === null || entry === null) return;
    const values = Object.fromEntries(Object.entries(fieldsNow.current).map(([name, value]) => [name, value.selected?.value ?? value.text]));
    selected.options.forEach((option, index) => {
      const kind = entry.fields[option.name]?.suggest;
      if (kind !== 'member' && kind !== 'provider') return;
      const field = fieldsNow.current[option.name];
      if (field === undefined || field.text === '' && !(kind === 'member' && index === active)) return;
      const generation = (suggestionsGeneration.current.get(option.name) ?? 0) + 1;
      suggestionsGeneration.current.set(option.name, generation);
      const requestedContext = contextKey;
      const query = field.text;
      void getSuggestions(selected.id, option, query, values).then(suggestions => {
        if (suggestionsGeneration.current.get(option.name) !== generation || contextNow.current !== requestedContext) return;
        setFields(previous => {
          const current = previous[option.name];
          if (current?.text !== query) return previous;
          let picked = current.selected;
          if (picked === undefined && query !== '') {
            const lower = query.toLowerCase().replace(/^@/u, '');
            const byValue = suggestions.find(candidate => candidate.value === query);
            const byLabel = suggestions.filter(candidate => candidate.label.toLowerCase() === lower || candidate.description?.toLowerCase() === `@${lower}`);
            const hit = byValue ?? (byLabel.length === 1 ? byLabel[0] : undefined);
            if (hit !== undefined) picked = { value: hit.value, label: hit.label };
          }
          return { ...previous, [option.name]: { ...current, ...(picked !== undefined ? { selected: picked } : {}), suggestions } };
        });
      }).catch(() => {
        if (suggestionsGeneration.current.get(option.name) !== generation || contextNow.current !== requestedContext) return;
        setFields(previous => {
          const current = previous[option.name];
          return current?.text === query ? { ...previous, [option.name]: { ...current, suggestions: Object.assign([], { notice: copy.suggestionsFailed }) } } : previous;
        });
      });
    });
    // `entry` follows `selected`; `active` matters for the empty member chip only.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [contextKey, getSuggestions, selected, suggestionQuery, suggestionSources, active]);

  const setField = (index: number, value: string) => {
    const option = options[index];
    if (option === undefined) return;
    dirty.current = true;
    setFields(previous => ({ ...previous, [option.name]: { text: value, suggestions: [] } }));
    setSugSel(0); setError(null);
  };
  const accept = (pick: Sug) => {
    if (activeOption === undefined) return;
    dirty.current = true;
    const name = activeOption.name;
    setFields(previous => ({ ...previous, [name]: { text: pick.chosen !== undefined ? previous[name]?.text ?? '' : pick.label,
      selected: { value: pick.value, label: pick.label, ...(pick.chosen !== undefined ? { chosen: pick.chosen } : {}) }, suggestions: previous[name]?.suggestions ?? [] } }));
    setActive(index => Math.min(index + 1, Math.max(options.length - 1, 0)));
    setSugSel(0); setError(null);
  };
  const leave = (keep: boolean) => {
    if (selected === null) return;
    if (!keep) {
      // Backspace in the first empty chip: back to the `/name` draft with the list open (the prototype's leaveCmd(false)).
      dirty.current = true;
      setDraft(`/${selected.id}`); setSelected(null); setFields({}); setError(null); setCmdSel(0);
      return;
    }
    onCancel(leaveText(selected, Object.fromEntries(selected.options.map(option => [option.name, fields[option.name]?.text ?? ''])), timeZone, nowOf()));
  };

  const fail = (field: number | null, message: string) => {
    if (field !== null) setActive(field);
    setError({ message, field });
  };

  /** The prototype's runCommand: the chips in order, the first problem shown in the bar and its chip turned red. */
  const validate = (): { args: Record<string, string> } | null => {
    if (selected === null || entry === null) return null;
    const args: Record<string, string> = {};
    const extra: Record<string, string> = {};
    for (const [index, option] of selected.options.entries()) {
      const field = fields[option.name] ?? { text: '', suggestions: [] };
      const meta = entry.fields[option.name];
      const text = field.text.trim();
      if (text === '') {
        if (option.required) { fail(index, copy.required(meta.label)); return null; }
        continue;
      }
      if (meta.suggest === 'member') {
        if (field.selected === undefined) { fail(index, copy.noMember(field.text)); return null; }
        args[option.name] = field.selected.value;
      } else if (meta.suggest === 'provider' && !(selected.id === 'play' && option.name === 'query-or-url')) {
        if (field.selected === undefined) { fail(index, copy.chooseEntity(option.name)); return null; }
        args[option.name] = field.selected.value;
      } else if (meta.suggest === 'duration') {
        if (selected.id === 'ban' && text.toLowerCase() === PERMANENT) { args[option.name] = PERMANENT; continue; }
        args[option.name] = registryDuration(text) ?? text;
      } else if (meta.suggest === 'when') {
        const result = whenOf(text);
        if (result.kind === 'invalid') { fail(index, result.reason === 'range' ? copy.whenRange : copy.whenHint); return null; }
        if (result.kind === 'ambiguous') {
          if (field.selected?.chosen === undefined) { fail(index, t('reminder.ambiguous')); return null; }
          extra.chosen = String(field.selected.chosen);
        }
        if (result.kind !== 'empty') args[option.name] = result.when;
        extra.timezone = timeZone;
      } else if (option.kind === 'number' && option.min !== undefined && option.max !== undefined) {
        if (!/^\d+$/u.test(text) || Number(text) < option.min || Number(text) > option.max) { fail(index, copy.wholeNumber(option.min, option.max)); return null; }
        args[option.name] = text;
      } else if (meta.suggest === 'command') {
        args[option.name] = text.replace(/^\//u, '');
      } else if (selected.id === 'play' && field.selected !== undefined && field.selected.label === field.text) {
        args[option.name] = field.selected.value;
      } else {
        args[option.name] = option.freeText ? field.text : text;
      }
    }
    const validation = validateCommandArgs(selected, args);
    if (!validation.valid) {
      const issue = validation.issues[0];
      const index = issue === undefined ? -1 : selected.options.findIndex(option => option.name === issue.name);
      fail(index < 0 ? null : index, issue?.message ?? copy.invalidArguments);
      return null;
    }
    return { args: { ...validation.args, ...extra } };
  };

  const run = async () => {
    if (selected === null || inFlight.current || pending || props.disabledReason !== undefined) return;
    const valid = validate();
    if (valid === null) return;
    inFlight.current = true;
    const generation = ++submitGeneration.current;
    const context = props.contextKey;
    setPending(true); setError(null); setStatus('');
    try {
      const response = await props.onExecute({ commandId: selected.id, args: valid.args });
      if (submitGeneration.current !== generation || contextNow.current !== context) return;
      setPending(response.status === 'pending');
      if (response.status === 'success') { setStatus(response.message ?? (response.private ? copy.completedPrivately : copy.completed)); if (props.onDone !== undefined) props.onDone(); else onCancel(''); }
      else if (response.status === 'pending') setStatus(response.message ?? copy.stillPending);
      else if (response.status === 'uncertain') { setStatus(''); setError({ message: response.message ?? copy.outcomeUnconfirmed, field: null }); }
      else { setStatus(''); setError({ message: response.message ?? copy.commandFailed, field: null }); }
    } catch (cause) {
      if (submitGeneration.current === generation && contextNow.current === context) {
        setPending(false); setStatus('');
        setError({ message: cause instanceof Error ? cause.message : copy.couldNotConfirm, field: null });
      }
    } finally {
      if (submitGeneration.current === generation && contextNow.current === context) inFlight.current = false;
    }
  };

  const chipKeyDown = (event: KeyboardEvent<HTMLInputElement | HTMLTextAreaElement>) => {
    if (selected === null || event.nativeEvent.isComposing || event.keyCode === 229) return;
    const count = Math.max(options.length, 1);
    const canAccept = sg.length > 0 && !exact && activeText.length > 0;
    if ((event.key === 'ArrowDown' || event.key === 'ArrowUp') && sg.length > 0) {
      event.preventDefault();
      setSugSel((sugIndex + (event.key === 'ArrowDown' ? 1 : -1) + sg.length) % sg.length);
      return;
    }
    if (event.key === 'Tab') {
      event.preventDefault();
      if (!event.shiftKey && canAccept) { accept(sg[sugIndex]); return; }
      setActive(index => event.shiftKey ? Math.max(index - 1, 0) : Math.min(index + 1, count - 1));
      setSugSel(0); setError(null);
      return;
    }
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault();
      if (event.repeat) return;
      const noAccept = activeField?.suggest === 'none' || selected.id === 'play' || activeOption?.kind === 'number';
      if (!noAccept && canAccept) { accept(sg[sugIndex]); return; }
      void run();
      return;
    }
    if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      leave(true);
      return;
    }
    if (event.key === 'Backspace' && activeText === '') {
      event.preventDefault();
      if (active > 0) { setActive(active - 1); setSugSel(0); setError(null); return; }
      leave(false);
    }
  };

  if (initial.literal) return null;

  // ---- The bar ----
  let bar: { icon: IconName; text: string; tone: 'info' | 'danger' } | null = null;
  if (selected !== null && entry !== null) {
    bar = { icon: entry.icon, text: entry.description, tone: 'info' };
    const notice = activeOption === undefined ? undefined : fields[activeOption.name]?.suggestions.notice;
    if (error !== null) bar = { icon: 'triangle-alert', text: error.message, tone: 'danger' };
    else if (props.disabledReason !== undefined) bar = { icon: entry.icon, text: props.disabledReason, tone: 'info' };
    else if (activeField?.suggest === 'when' && activeText !== '') {
      const result = whenOf(activeText);
      const chosen = activeSelected?.chosen;
      bar = { icon: 'alarm-clock', tone: 'info', text: result.kind === 'valid' ? copy.remindsYou(result.preview)
        : result.kind === 'ambiguous' ? (chosen !== undefined ? copy.remindsYou(result.candidates.find(c => c.unixSeconds === chosen)?.label ?? '') : t('reminder.ambiguous'))
          : result.kind === 'invalid' && result.reason === 'range' ? copy.whenRange : copy.whenHint };
    } else if (notice !== undefined) bar = { icon: entry.icon, text: notice, tone: 'info' };
    else if (MUSIC_COMMANDS.has(selected.id)) bar = { ...bar, text: entry.description + copy.separator + (props.room ?? copy.joinVoiceFirst) };
    else if (MODERATION_COMMANDS.has(selected.id)) bar = { ...bar, text: entry.description + copy.separator + copy.checkedAgainstRoles };
  } else if (listError !== null) bar = { icon: 'triangle-alert', text: listError, tone: 'danger' };
  // A field error reddens its chip; a run that failed (no field) leaves the chips as they are.
  const invalidField = error?.field ?? null;

  const listItemsUi: CommandListItem[] = listItems.map(command => {
    const e = entries.get(command.id)!;
    return { id: command.id, icon: e.icon, name: command.id, args: e.args, description: e.description, source: e.source };
  });
  const listOpen = listState.open;
  const activeListId = listItemsUi[listSel]?.id ?? null;

  const top = <>
    {listOpen ? <CommandList id={listId} items={listItemsUi} activeId={activeListId} showHint labels={{ title: copy.commands, hint: copy.listHint, noMatch: props.loading === true ? t('commands.loading') : copy.noMatch(draft), sendAsText: copy.sendAsText }}
      query={draft} onActive={id => setCmdSel(Math.max(listItems.findIndex(command => command.id === id), 0))}
      onPick={id => { const command = definitionsById.get(id); if (command !== undefined) enterCommand(command); }}
      onSendAsText={() => sendLiteralOnce(draft)} /> : null}
    {sugOpen ? <CommandSuggestions id={sugsId} title={copy.sugTitle[activeField!.suggest as Exclude<PickerSuggest, 'none'>]} items={sg} activeId={sg[sugIndex]?.id ?? null}
      onActive={id => setSugSel(Math.max(sg.findIndex(s => s.id === id), 0))}
      onPick={id => { const pick = sg.find(s => s.id === id); if (pick !== undefined) accept(pick); }} /> : null}
    {bar !== null ? <CommandBar icon={bar.icon} text={bar.text} tone={bar.tone} hint={copy.fieldHint} /> : null}
  </>;

  const input = selected === null || entry === null
    // The draft field keeps the composer textarea's textbox role (pre-flight ruling F10) and points at the list while it shows.
    ? <textarea ref={draftRef} className="d-composer__input" rows={1} value={draft} aria-label={copy.command}
      {...(listOpen && listItemsUi.length > 0 ? { 'aria-autocomplete': 'list' as const, 'aria-controls': listId } : {})}
      {...(listOpen && activeListId !== null ? { 'aria-activedescendant': commandOptionId(listId, activeListId) } : {})}
      onChange={event => draftChange(event.currentTarget.value)} onKeyDown={draftKeyDown} />
    : <CommandChips icon={entry.icon} name={selected.id}>
      {options.length === 0
        ? <CommandField label="" optional={false} value="" placeholder={copy.pressToRun} active invalid={false} ghost
          inputRef={node => { if (node) fieldRefs.current.set(0, node); else fieldRefs.current.delete(0); }}
          onChange={() => {}} onFocus={() => setFocused(true)} onBlur={() => setFocused(false)} onKeyDown={chipKeyDown} />
        : options.map((option, index) => {
          const meta = entry.fields[option.name];
          const isActive = index === active;
          return <CommandField key={option.name} label={meta.label} optional={!option.required} value={fields[option.name]?.text ?? ''} placeholder={meta.placeholder}
            active={isActive} invalid={invalidField === index} multiline={option.freeText === true && meta.suggest === 'none'}
            {...(isActive && sugOpen ? { listId: sugsId, activeOptionId: commandOptionId(sugsId, sg[sugIndex]?.id ?? '') } : {})}
            inputRef={node => { if (node) fieldRefs.current.set(index, node); else fieldRefs.current.delete(index); }}
            onChange={value => setField(index, value)}
            onFocus={() => { setFocused(true); if (active !== index) { setActive(index); setSugSel(0); } }}
            onBlur={() => setFocused(false)} onKeyDown={chipKeyDown} />;
        })}
    </CommandChips>;

  const parts: PickerFrame = { top, input, draft: selected === null ? draft : '', run: () => { if (selected === null) { if (draft.startsWith('//')) sendLiteralOnce(draft.slice(1)); else enterTyped(); } else void run(); } };
  const frame = props.frame ?? defaultFrame;
  return <section className="dw-command-picker" data-confirming={props.moderationConfirmOpen ? 'true' : undefined} aria-label={copy.region} style={{ display: 'contents' }} data-pending={pending ? 'true' : undefined}>
    {frame(parts)}
    <p className="d-sr-only" role="status" aria-live="polite">{status}</p>
  </section>;
}

function defaultFrame(parts: PickerFrame): ReactNode {
  return <Composer label={copy.command} placeholder="" maxLength={4000} value={parts.draft} onChange={() => {}} onSend={() => parts.run()} sendLabel={t('shell.composer.send')}
    counterLabel={n => String(n)} top={parts.top} input={parts.input} />;
}
