import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react';
import { commandById, parseCommand, searchCommands, validateCommandArgs, type CommandDefinition, type CommandInvocation, type CommandOption, type CommandResult } from '@dilla/client-core';
import { commandComposerCopy as copy } from './copy.ts';
import type { CommandSuggestionList } from './command-types.ts';
import './CommandComposer.css';

export interface CommandSuggestion { id: string; label: string; description?: string; value: string }

export interface CommandComposerProps {
  definitions: readonly CommandDefinition[];
  initialText: string;
  onExecute: (invocation: CommandInvocation) => Promise<CommandResult>;
  onCancel: (text: string) => void;
  onSendLiteral: (text: string) => void;
  suggest: (commandId: string, option: CommandOption, query: string, values?: Readonly<Record<string, string>>) => Promise<CommandSuggestionList>;
  /** Live snapshots that invalidate cached entity queries without editing the draft. */
  suggestionSources?: readonly unknown[];
  contextKey: string;
  disabledReason?: string;
  onDraftChange?: (text: string) => void;
}

interface FieldState {
  text: string;
  selected?: CommandSuggestion;
  suggestions: CommandSuggestionList;
}

const ENTITY_KINDS = new Set(['user', 'channel', 'role', 'message', 'queue-item', 'poll', 'announcement', 'reminder']);
const isEntity = (option: CommandOption): boolean => ENTITY_KINDS.has(option.kind);
const quote = (value: string): string => `"${value.replaceAll('\\', '\\\\').replaceAll('"', '\\"')}"`;

export function CommandComposer(props: CommandComposerProps): React.JSX.Element | null {
  const { initialText, onCancel, onSendLiteral, onDraftChange } = props;
  const parsed = useMemo(() => parseCommand(initialText), [initialText]);
  const original = useRef(initialText);
  const started = useRef(false);
  const literalSent = useRef(false);
  const dirty = useRef(false);
  const contextNow = useRef(props.contextKey);
  const contextAtMount = useRef(props.contextKey);
  const suggestionsGeneration = useRef(new Map<string, number>());
  const submitGeneration = useRef(0);
  const inFlight = useRef(false);
  const pickerId = useId();
  const parsedCommandId = parsed.type === 'invocation' ? parsed.invocation.commandId : parsed.type === 'incomplete' ? parsed.commandId : '';
  const parsedArgs = parsed.type === 'invocation' ? parsed.invocation.args : parsed.type === 'incomplete' ? parsed.validation.args : {};
  const initialAvailableCommand = props.definitions.find(command => command.id === parsedCommandId) ?? null;
  const [pickerQuery, setPickerQuery] = useState(() => parsed.type === 'unknown' ? parsed.commandText : parsedCommandId && !initialAvailableCommand ? parsedCommandId : '');
  const [selectedCommand, setSelectedCommand] = useState<CommandDefinition | null>(() => {
    return initialAvailableCommand;
  });
  const [fields, setFields] = useState<Record<string, FieldState>>(() => {
    const values = parsedArgs;
    const definitionId = parsedCommandId;
    const definition = props.definitions.find(command => command.id === definitionId);
    return Object.fromEntries((definition?.options ?? []).map(option => [option.name, {
      text: values[option.name] ?? '',
      suggestions: [],
    }]));
  });
  const fieldsNow = useRef(fields);
  fieldsNow.current = fields;
  const fieldRefs = useRef(new Map<string, HTMLElement>());
  const composerRef = useRef<HTMLElement>(null);
  const pickerRef = useRef<HTMLInputElement>(null);
  const executeButtonRef = useRef<HTMLButtonElement>(null);
  const [popup, setPopup] = useState<string | null>(() => selectedCommand ? selectedCommand.options.find(option => fields[option.name]?.text)?.name ?? null : 'picker');
  const [activeIndex, setActiveIndex] = useState(-1);
  const [error, setError] = useState('');
  const [result, setResult] = useState('');
  const [pending, setPending] = useState(false);
  const [optionalVisible, setOptionalVisible] = useState<Set<string>>(() => {
    const definition = initialAvailableCommand;
    return new Set(definition?.options.filter(option => !option.required && parsedArgs[option.name] !== undefined).map(option => option.name) ?? []);
  });
  const [submitted, setSubmitted] = useState(false);
  const suggestionQuery = selectedCommand?.options.map(option => `${option.name}:${fields[option.name]?.text ?? ''}`).join('\u0000') ?? '';
  const { suggest: getSuggestions, suggestionSources } = props;
  const suggest = useCallback((commandId: string, option: CommandOption, query: string) => getSuggestions(commandId, option, query,
    Object.fromEntries(Object.entries(fieldsNow.current).map(([name, value]) => [name, value.selected?.value ?? value.text]))), [getSuggestions]);
  const contextKey = props.contextKey;

  const definitionsById = useMemo(() => new Map(props.definitions.map(command => [command.id, command])), [props.definitions]);
  const pickerCommands = useMemo(() => {
    const ranked = searchCommands(pickerQuery, 25).filter(command => definitionsById.has(command.id));
    const included = new Set(ranked.map(command => command.id));
    const needle = pickerQuery.trim().toLowerCase();
    const remaining = props.definitions.filter(command => !included.has(command.id) && (needle === '' || command.id.toLowerCase().includes(needle) || command.description.toLowerCase().includes(needle)));
    return [...ranked, ...remaining].slice(0, 25);
  }, [definitionsById, pickerQuery, props.definitions]);

  contextNow.current = props.contextKey;
  useEffect(() => () => {
    submitGeneration.current++;
    for (const [name, generation] of suggestionsGeneration.current) suggestionsGeneration.current.set(name, generation + 1);
    contextNow.current = '';
  }, []);
  const getDraft = useCallback(() => buildDraft(original.current, dirty.current, selectedCommand, pickerQuery, fields), [selectedCommand, pickerQuery, fields]);
  useEffect(() => {
    if (dirty.current) onDraftChange?.(getDraft());
  }, [onDraftChange, getDraft, optionalVisible]);
  const sendLiteralOnce = useCallback((text: string) => {
    if (literalSent.current) return;
    literalSent.current = true;
    onSendLiteral(text);
  }, [onSendLiteral]);
  const handleComposerKeyDown = useCallback((event: KeyboardEvent) => {
    if (event.key !== 'Escape' || event.isComposing || event.keyCode === 229) return;
    event.preventDefault();
    event.stopPropagation();
    if (parsed.type === 'literal') { onCancel(getDraft()); return; }
    const popupHasResults = popup === 'picker'
      ? pickerCommands.length > 0
      : popup !== null && Boolean(fieldsNow.current[popup]?.suggestions.length);
    if (popupHasResults) { setPopup(null); setActiveIndex(-1); }
    else onCancel(getDraft());
  }, [getDraft, onCancel, parsed.type, pickerCommands, popup]);
  useEffect(() => {
    const root = composerRef.current;
    root?.addEventListener('keydown', handleComposerKeyDown);
    return () => root?.removeEventListener('keydown', handleComposerKeyDown);
  }, [handleComposerKeyDown]);
  useEffect(() => {
    if (!started.current) {
      started.current = true;
      if (parsed.type === 'literal' && !initialText.startsWith('//')) onCancel(initialText);
    }
  }, [parsed, initialText, onCancel]);

  useEffect(() => {
    if (contextAtMount.current === props.contextKey) return;
    contextAtMount.current = props.contextKey;
    suggestionsGeneration.current.clear();
    submitGeneration.current++;
    inFlight.current = false;
    setPending(false);
    setError('');
    setResult('');
    setFields(previous => Object.fromEntries(Object.entries(previous).map(([name, field]) => [name, {
      ...field, selected: undefined, suggestions: [],
    }])));
  }, [props.contextKey]);

  useEffect(() => {
    if (!selectedCommand) return;
    const available = definitionsById.get(selectedCommand.id);
    if (!available) {
      onCancel(buildDraft(original.current, dirty.current, selectedCommand, pickerQuery, fields));
      setSelectedCommand(null);
      setFields({});
      setPopup('picker');
      return;
    }
    if (available !== selectedCommand) setSelectedCommand(available);
  }, [definitionsById, fields, onCancel, pickerQuery, selectedCommand]);

  useEffect(() => {
    if (!selectedCommand) { pickerRef.current?.focus(); return; }
    const first = selectedCommand.options.find(option => option.required) ?? selectedCommand.options[0];
    if (first) fieldRefs.current.get(first.name)?.focus();
    else executeButtonRef.current?.focus();
  }, [selectedCommand]);

  useEffect(() => {
    if (!selectedCommand) return;
    for (const option of selectedCommand.options) {
      const field = fieldsNow.current[option.name];
      if (!field?.text) continue;
      const generation = (suggestionsGeneration.current.get(option.name) ?? 0) + 1;
      suggestionsGeneration.current.set(option.name, generation);
      const requestedContext = contextKey;
      void suggest(selectedCommand.id, option, field.text).then(suggestions => {
        if (suggestionsGeneration.current.get(option.name) !== generation || contextNow.current !== requestedContext) return;
        setFields(previous => {
          const current = previous[option.name];
          if (!current || current.text !== field.text) return previous;
          let selected = current.selected;
          if (isEntity(option) && !selected) {
            const exactValue = suggestions.find(candidate => candidate.value === current.text);
            const labels = suggestions.filter(candidate => candidate.label === current.text);
            selected = exactValue ?? (labels.length === 1 ? labels[0] : undefined);
          }
          return { ...previous, [option.name]: { ...current, selected, suggestions } };
        });
      }).catch(() => {
        if (suggestionsGeneration.current.get(option.name) !== generation || contextNow.current !== requestedContext) return;
        setFields(previous => {
          const current = previous[option.name];
          return current?.text === field.text
            ? { ...previous, [option.name]: { ...current, suggestions: Object.assign([], { notice: copy.suggestionsFailed }) } } : previous;
        });
      });
    }
  }, [contextKey, suggest, selectedCommand, suggestionQuery, suggestionSources]);

  if (parsed.type === 'literal') {
    if (!initialText.startsWith('//')) return null;
    return <section ref={composerRef} className="d-command-composer" aria-label={copy.region}>
      <p className="d-command-composer__notice">{copy.sendTextQuestion}</p>
      <p className="d-command-composer__preview">{parsed.text}</p>
      <button type="button" className="d-command-composer__execute" onClick={() => sendLiteralOnce(parsed.text)}>{copy.sendAsText}</button>
    </section>;
  }

  const visibleOptions = selectedCommand?.options.filter(option => option.required || optionalVisible.has(option.name)) ?? [];
  const optionalOptions = selectedCommand?.options.filter(option => !option.required && !optionalVisible.has(option.name)) ?? [];
  const chooseCommand = (command: CommandDefinition) => {
    dirty.current = true;
    const initialArgs = parsedCommandId === command.id ? parsedArgs : {};
    setSelectedCommand(command);
    setFields(Object.fromEntries(command.options.map(option => [option.name, { text: initialArgs[option.name] ?? '', suggestions: [] }])));
    setOptionalVisible(new Set(command.options.filter(option => !option.required && initialArgs[option.name] !== undefined).map(option => option.name)));
    setPickerQuery(command.id);
    setPopup(null);
    setActiveIndex(-1);
    setError('');
    setResult('');
  };

  const chooseTypedInvocation = (invocation: CommandInvocation) => {
    const command = definitionsById.get(invocation.commandId);
    if (!command) return false;
    dirty.current = true;
    setSelectedCommand(command);
    setFields(Object.fromEntries(command.options.map(option => [option.name, { text: invocation.args[option.name] ?? '', suggestions: [] }])));
    setOptionalVisible(new Set(command.options.filter(option => !option.required && invocation.args[option.name] !== undefined).map(option => option.name)));
    setPickerQuery(command.id);
    setPopup(null);
    setActiveIndex(-1);
    setError('');
    return true;
  };

  const updateField = (option: CommandOption, value: string) => {
    dirty.current = true;
    setFields(previous => ({ ...previous, [option.name]: { text: value, suggestions: [] } }));
    setPopup(option.name);
    setActiveIndex(-1);
    setError('');
    setResult('');
  };

  const chooseSuggestion = (option: CommandOption, candidate: CommandSuggestion) => {
    dirty.current = true;
    setFields(previous => ({ ...previous, [option.name]: { text: candidate.label, selected: candidate, suggestions: previous[option.name]?.suggestions ?? [] } }));
    setPopup(null);
    setActiveIndex(-1);
    setError('');
  };

  const execute = async () => {
    if (!selectedCommand || inFlight.current || pending || props.disabledReason) return;
    const args: Record<string, string> = {};
    const entityErrors: string[] = [];
    for (const option of selectedCommand.options) {
      const field = fields[option.name];
      if (!field || field.text === '') continue;
      if (isEntity(option)) {
        if (field.selected) args[option.name] = field.selected.value;
        else entityErrors.push(copy.chooseEntity(option.name));
      } else args[option.name] = field.selected?.value ?? field.text;
    }
    const validation = validateCommandArgs(selectedCommand, args);
    if (entityErrors.length || !validation.valid) {
      setError(entityErrors[0] ?? validation.issues[0]?.message ?? copy.invalidArguments);
      setSubmitted(true);
      return;
    }
    inFlight.current = true;
    const generation = ++submitGeneration.current;
    const context = props.contextKey;
    setPending(true);
    setResult('');
    setError('');
    setSubmitted(true);
    try {
      const response = await props.onExecute({ commandId: selectedCommand.id, args: validation.args });
      if (submitGeneration.current !== generation || contextNow.current !== context) return;
      setPending(response.status === 'pending');
      if (response.status === 'failure') setError(response.message ?? copy.commandFailed);
      else if (response.status === 'pending') setResult(response.message ?? copy.stillPending);
      else if (response.status === 'uncertain') setResult(response.message ?? copy.outcomeUnconfirmed);
      else setResult(response.message ?? (response.private ? copy.completedPrivately : copy.completed));
    } catch (cause) {
      if (submitGeneration.current === generation && contextNow.current === context) {
        setPending(false);
        setError(cause instanceof Error ? cause.message : copy.couldNotConfirm);
      }
    } finally {
      if (submitGeneration.current === generation && contextNow.current === context) inFlight.current = false;
    }
  };

  const keyDown = (event: React.KeyboardEvent<HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement>, option?: CommandOption) => {
    if (event.nativeEvent.isComposing || event.keyCode === 229) return;
    const list = option ? fields[option.name]?.suggestions ?? [] : pickerCommands;
    const listOpen = popup === (option?.name ?? 'picker') && list.length > 0;
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      if (!listOpen) return;
      event.preventDefault();
      setActiveIndex(index => event.key === 'ArrowDown' ? (index + 1 + list.length) % list.length : (index - 1 + list.length) % list.length);
      return;
    }
    if (event.key === 'Tab' && listOpen && activeIndex >= 0) {
      event.preventDefault();
      if (option) {
        const candidate = fields[option.name]?.suggestions[activeIndex];
        if (candidate) chooseSuggestion(option, candidate);
      } else {
        const command = pickerCommands[activeIndex];
        if (command) chooseCommand(command);
      }
      return;
    }
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault();
      if (!option && !selectedCommand) {
        const typed = parseCommand(`/${pickerQuery.trim()}`);
        if (typed.type === 'invocation' && chooseTypedInvocation(typed.invocation)) return;
        if (typed.type === 'incomplete' && chooseTypedInvocation({ commandId: typed.commandId, args: typed.validation.args })) return;
      }
      if (listOpen && activeIndex >= 0) {
        if (option) {
          const candidate = fields[option.name]?.suggestions[activeIndex];
          if (candidate) chooseSuggestion(option, candidate);
        } else {
          const command = pickerCommands[activeIndex];
          if (command) chooseCommand(command);
        }
      } else if (option) void execute();
      else if (listOpen && pickerCommands[0]) chooseCommand(pickerCommands[0]);
    }
  };

  const unavailableExact = !selectedCommand && commandById(pickerQuery.trim()) !== undefined && !props.definitions.some(command => command.id === pickerQuery.trim());
  const unknown = !selectedCommand && pickerCommands.length === 0 && pickerQuery.trim() !== '';
  return <section ref={composerRef} className="d-command-composer" aria-label={copy.region} data-disabled={Boolean(props.disabledReason)}>
    {selectedCommand ? <>
      <div className="d-command-composer__heading">
        <span className="d-command-composer__chip">{copy.slash}{selectedCommand.id}</span>
        <h2>{selectedCommand.description}</h2>
        <button type="button" className="d-command-composer__cancel" onClick={() => props.onCancel(getDraft())}>{copy.cancel}</button>
      </div>
      {props.disabledReason && <p className="d-command-composer__notice">{props.disabledReason}</p>}
      <div className="d-command-composer__fields">
        {visibleOptions.map(option => {
          const field = fields[option.name] ?? { text: '', suggestions: [] };
          const listId = `${pickerId}-${option.name}`;
          const isChoice = option.kind === 'choice' && option.choices;
          return <div className="d-command-composer__field" key={option.name}>
            <label htmlFor={`${pickerId}-${option.name}-input`}>{option.name}{option.required && <span aria-hidden="true">{copy.required}</span>}</label>
            <p id={`${pickerId}-${option.name}-hint`} className="d-command-composer__hint">{option.description}</p>
            {isChoice ? <select ref={node => { if (node) fieldRefs.current.set(option.name, node); else fieldRefs.current.delete(option.name); }} id={`${pickerId}-${option.name}-input`} aria-label={option.name} value={field.text} onChange={event => updateField(option, event.currentTarget.value)} onKeyDown={event => keyDown(event, option)}>
              <option value="">{copy.choose} {option.name}</option>{option.choices!.map(choice => <option key={choice} value={choice}>{choice}</option>)}
            </select> : option.kind === 'number' ? <input id={`${pickerId}-${option.name}-input`} aria-label={option.name} type="number" min={option.min} max={option.max} value={field.text} aria-invalid={Boolean(error && submitted)} aria-describedby={`${pickerId}-${option.name}-hint`} onChange={event => updateField(option, event.currentTarget.value)} onKeyDown={event => keyDown(event, option)} />
              : option.freeText ? <textarea ref={node => { if (node) fieldRefs.current.set(option.name, node); else fieldRefs.current.delete(option.name); }} id={`${pickerId}-${option.name}-input`} aria-label={option.name} role="combobox" aria-autocomplete="list" aria-expanded={popup === option.name && field.suggestions.length > 0} aria-controls={listId} aria-activedescendant={popup === option.name && activeIndex >= 0 ? `${listId}-${activeIndex}` : undefined} aria-describedby={`${pickerId}-${option.name}-hint`} aria-invalid={Boolean(error && submitted)} value={field.text} onFocus={() => { if (field.suggestions.length) setPopup(option.name); }} onChange={event => updateField(option, event.currentTarget.value)} onKeyDown={event => keyDown(event, option)} />
                : <input ref={node => { if (node) fieldRefs.current.set(option.name, node); else fieldRefs.current.delete(option.name); }} id={`${pickerId}-${option.name}-input`} aria-label={option.name} role="combobox" aria-autocomplete="list" aria-expanded={popup === option.name && field.suggestions.length > 0} aria-controls={listId} aria-activedescendant={popup === option.name && activeIndex >= 0 ? `${listId}-${activeIndex}` : undefined} aria-describedby={`${pickerId}-${option.name}-hint`} aria-invalid={Boolean(error && submitted)} value={field.text} onFocus={() => { if (field.suggestions.length) setPopup(option.name); }} onChange={event => updateField(option, event.currentTarget.value)} onKeyDown={event => keyDown(event, option)} />}
            {field.suggestions.notice && <p className="d-command-composer__notice" role="note">{field.suggestions.notice}</p>}
            {popup === option.name && field.suggestions.length > 0 && <ul className="d-command-composer__list" id={listId} role="listbox" aria-label={`${option.name} suggestions`}>
              {field.suggestions.map((candidate, index) => <li id={`${listId}-${index}`} role="option" aria-selected={index === activeIndex} key={candidate.id} onMouseDown={event => event.preventDefault()} onClick={() => chooseSuggestion(option, candidate)} onKeyDown={event => { if (event.key === 'Enter') chooseSuggestion(option, candidate); }}>
                <span>{candidate.label}</span>{candidate.description && <small>{candidate.description}</small>}
              </li>)}
            </ul>}
          </div>;
        })}
      </div>
      {optionalOptions.length > 0 && <div className="d-command-composer__optional">
        <span>{copy.optionalArguments}</span>
        {optionalOptions.map(option => <button type="button" key={option.name} onClick={() => {
          dirty.current = true;
          setOptionalVisible(current => new Set(current).add(option.name));
          setFields(current => ({ ...current, [option.name]: { text: '', suggestions: [] } }));
        }}>{copy.addOptional} {option.name}</button>)}
      </div>}
      {error && <p className="d-command-composer__error" role="alert">{error}</p>}
      <div className="d-command-composer__footer">
        <p className="d-command-composer__live" role="status" aria-live="polite">{pending ? copy.runningPrivately : result}</p>
        <button ref={executeButtonRef} type="button" className="d-command-composer__execute" disabled={Boolean(props.disabledReason) || pending} onClick={() => void execute()}>{pending ? copy.running : copy.run}</button>
      </div>
    </> : <>
      <label className="d-sr-only" htmlFor={`${pickerId}-command`}>{copy.command}</label>
      <input ref={pickerRef} id={`${pickerId}-command`} className="d-command-composer__picker" type="text" role="combobox" aria-label={copy.command} aria-autocomplete="list" aria-expanded={popup === 'picker' && pickerCommands.length > 0} aria-controls={`${pickerId}-commands`} aria-activedescendant={popup === 'picker' && activeIndex >= 0 ? `${pickerId}-command-${activeIndex}` : undefined} placeholder={copy.searchCommands} value={pickerQuery} onFocus={() => setPopup('picker')} onChange={event => { dirty.current = true; setPickerQuery(event.currentTarget.value); setPopup('picker'); setActiveIndex(-1); setError(''); }} onKeyDown={event => keyDown(event)} />
      {popup === 'picker' && pickerCommands.length > 0 && <ul className="d-command-composer__list" id={`${pickerId}-commands`} role="listbox" aria-label={copy.commandList}>
        {pickerCommands.map((command, index) => <li id={`${pickerId}-command-${index}`} role="option" aria-selected={index === activeIndex} key={command.id} onMouseDown={event => event.preventDefault()} onClick={() => chooseCommand(command)} onKeyDown={event => { if (event.key === 'Enter') chooseCommand(command); }}>
          <span>{copy.slash}{command.id}</span><small>{command.provider === 'builtin' ? copy.builtIn : copy.bot} {copy.separator} {command.description}</small>
        </li>)}
      </ul>}
      {(unknown || unavailableExact) && <p className="d-command-composer__error" role="alert">{unavailableExact ? copy.commandUnavailable(pickerQuery) : copy.unknownCommand(pickerQuery)}</p>}
      {unknown && <button type="button" onClick={() => {
        const draft = getDraft();
        sendLiteralOnce(draft.startsWith('//') ? draft.slice(1) : draft);
      }}>{copy.sendAsText}</button>}
      {error && <p className="d-command-composer__error" role="alert">{error}</p>}
    </>}
  </section>;
}

function buildDraft(original: string, dirty: boolean, command: CommandDefinition | null, pickerQuery: string, fields: Record<string, FieldState>): string {
  if (!dirty) return original;
  if (!command) return `/${pickerQuery}`;
  const args = command.options.flatMap(option => {
    const field = fields[option.name];
    const value = field?.selected?.value ?? field?.text ?? '';
    return value === '' ? [] : [`${option.name}:${quote(value)}`];
  });
  return `/${command.id}${args.length ? ` ${args.join(' ')}` : ''}`;
}
