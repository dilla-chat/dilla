import {
  useId, useLayoutEffect, useRef, useState, type ChangeEvent, type ClipboardEvent, type FormEvent, type KeyboardEvent, type ReactNode, type Ref,
  type SyntheticEvent,
} from 'react';
import { Icon } from '../Icon/Icon.tsx';
import './Composer.css';

/** The keys the composer hands to an attached list (the mention list) before its own rules. */
type ComboKey = 'ArrowUp' | 'ArrowDown' | 'Enter' | 'Tab' | 'Escape';
const COMBO_KEYS: readonly string[] = ['ArrowUp', 'ArrowDown', 'Enter', 'Tab', 'Escape'];

export interface ComposerProps {
  label: string;
  placeholder: string;
  /** The budget in UTF-8 bytes, the unit of the core's body limit (L-CORE-09), unless `measure` says otherwise. */
  maxLength: number;
  value: string;
  onChange(value: string): void;
  disabled?: boolean;
  disabledReason?: string;
  onSend(text: string): void;
  sendLabel: string;
  counterLabel(remaining: number): string;
  /** The attach button (`+`) before the textarea, shown when both are given. */
  attachLabel?: string;
  onAttach?(): void;
  /**
   * The GIF picker's opener (E8), after the attach button: a text button that toggles the owner's picker panel
   * (`aria-haspopup="dialog"`, `aria-expanded`, `aria-controls` while open). `data-gif-toggle` lets the panel ignore
   * a pointer down on it, so the click toggles instead of closing and reopening.
   */
  gif?: { label: string; open: boolean; controls?: string; onToggle(): void; buttonRef?: Ref<HTMLButtonElement> };
  /** Rendered above the box: the reply chip, the tray, the mention list. */
  top?: ReactNode;
  /**
   * Text above the box, after `top`: the slow-mode countdown, the announcement channel's read-only line. It never blocks:
   * the textarea and send stay enabled (the host decides). Whether it is spoken is the caller's: give it `role="status"`
   * content for a line that should be, and plain text for one that should not repeat (a countdown).
   */
  notice?: ReactNode;
  /** The used part of the budget; UTF-8 bytes when absent. */
  measure?(v: string): number;
  /**
   * An attached list (the mention list). While `expanded` the textarea points at it (`aria-controls`,
   * `aria-activedescendant`, `aria-autocomplete="list"`) and keeps its textbox role (pre-flight ruling F10); `onKey`
   * is offered the five keys whenever `combobox` is given, and a `true` answer consumes the key.
   */
  combobox?: { expanded: boolean; controls: string; activeDescendant: string | null; onKey(key: ComboKey): boolean };
  onCaret?(caret: number): void;
  onArrowUpEmpty?(): void;
  onPasteFiles?(files: File[]): void;
  textareaRef?: Ref<HTMLTextAreaElement>;
  /** Send a blank text (an attachment-only message): the owner passes it while the tray holds an entry. */
  canSendEmpty?: boolean;
  /**
   * The emoji picker's opener in the footer (README §5.5): toggles the owner's picker panel, as `gif` does. A pick is the
   * owner's to insert (`insertAtCaret` on the textarea ref is not needed: the owner holds the text).
   */
  emoji?: { label: string; open: boolean; controls?: string; onToggle(): void; buttonRef?: Ref<HTMLButtonElement> };
  /** The `</>` button: inserts a fenced code block and puts the caret inside it. `label` names it. */
  codeLabel?: string;
  /** Someone else typing: the footer's "Björn is typing…" (a TypingIndicator); the box's top edge sweeps while it shows. */
  typing?: ReactNode;
  /** The footer's "⇧↵ new line" hint (hidden on a phone). */
  hint?: string;
  /** A reply bar sits on the box (in `top`): the box's top corners square off so the two join. */
  replying?: boolean;
}

/** The prototype's `</>` insert: the draft (ending in one line break) and a yaml fence, the caret on the empty line inside. */
export function insertCodeFence(draft: string): { text: string; caret: number } {
  const fence = '`'.repeat(3);
  const text = (draft === '' ? '' : draft.replace(/\n?$/, '\n')) + fence + 'yaml\n\n' + fence;
  return { text, caret: text.length - 4 };
}

const ENCODER = new TextEncoder();

/**
 * The label split into its label part and its target, the `#channel` (or `@person`) that follows the
 * first space: the target is a value and keeps its case, so the label treatment never reaches it.
 */
function labelParts(label: string): { lead: string; target: string | null } {
  const m = /^(.*?\s)([#@].*)$/s.exec(label);
  return m === null ? { lead: label, target: null } : { lead: m[1], target: m[2] };
}

/**
 * The message composer, controlled (pre-flight ruling (a), L-UI-13): the owner
 * holds the text and the component never clears it on send, so a refused
 * message is not lost. Enter sends, Shift+Enter breaks the line, and neither
 * fires while an IME is composing. The budget is counted in UTF-8 bytes; the
 * textarea carries no native maxLength (that counts UTF-16 units and would cut
 * a paste silently). Blocked, the textarea stays focusable (readOnly with
 * aria-disabled) so its reason is heard; only the send button is disabled.
 * Over the budget the counter takes the danger tone, a polite status says so,
 * and send stays focusable (aria-disabled) with the counter as its reason.
 */
export function Composer({
  label, placeholder, maxLength, value, onChange, disabled, disabledReason, onSend, sendLabel, counterLabel,
  attachLabel, onAttach, gif, top, notice, measure, combobox, onCaret, onArrowUpEmpty, onPasteFiles, textareaRef, canSendEmpty,
  emoji, codeLabel, typing, hint, replying,
}: ComposerProps) {
  const ownInput = useRef<HTMLTextAreaElement | null>(null);
  const pendingCaret = useRef<number | null>(null);
  const setInput = (el: HTMLTextAreaElement | null) => {
    ownInput.current = el;
    if (typeof textareaRef === 'function') textareaRef(el);
    else if (textareaRef) (textareaRef as { current: HTMLTextAreaElement | null }).current = el;
  };
  useLayoutEffect(() => {
    if (pendingCaret.current === null || ownInput.current === null) return;
    const at = pendingCaret.current;
    pendingCaret.current = null;
    ownInput.current.focus();
    ownInput.current.setSelectionRange(at, at);
    onCaret?.(at);
  });
  const insertCode = () => {
    if (disabled) return;
    const next = insertCodeFence(value);
    pendingCaret.current = next.caret;
    onChange(next.text);
  };
  const inputId = useId();
  const reasonId = useId();
  const counterId = useId();
  const noticeId = useId();
  const used = measure ? measure(value) : ENCODER.encode(value).length;
  const remaining = maxLength - used;
  const overBudget = remaining < 0;
  const showCounter = remaining <= Math.floor(maxLength / 10);
  const showReason = disabled === true && !!disabledReason;
  const showNotice = notice !== undefined && notice !== null && notice !== false;
  const described = [showReason ? reasonId : null, showNotice ? noticeId : null, showCounter ? counterId : null].filter((x): x is string => x !== null);
  const describedBy = described.length > 0 ? described.join(' ') : undefined;
  const { lead, target } = labelParts(label);
  // Over the budget (and not blocked) send stays focusable, so the counter is heard as its reason.
  const sendRefused = disabled !== true && overBudget;

  // The counter is a status message: the polite status speaks it when it appears and when the text crosses the
  // budget, never on every keystroke (WCAG 4.1.3). State adjusted during render, React's pattern for derived state.
  const phase = !showCounter ? 'none' : overBudget ? 'over' : 'near';
  const [status, setStatus] = useState<{ phase: typeof phase; text: string }>({ phase: 'none', text: '' });
  if (status.phase !== phase) setStatus({ phase, text: phase === 'none' ? '' : counterLabel(remaining) });

  const submit = () => {
    if (disabled) return;
    // A blank text is sent only when the owner says it may be (an attachment-only message).
    if (value.trim() === '' && canSendEmpty !== true) return;
    // Over the budget the text stays in the field; the counter already says by how much.
    if (overBudget) return;
    onSend(value);
  };

  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    const composing = e.nativeEvent.isComposing || e.keyCode === 229;
    // (a) The attached list sees its keys first, open or closed; a consumed key does nothing else.
    if (combobox !== undefined && COMBO_KEYS.includes(e.key) && !e.altKey && !e.ctrlKey && !e.metaKey && !composing) {
      if (combobox.onKey(e.key as ComboKey)) {
        e.preventDefault();
        return;
      }
    }
    // (b) ArrowUp in an empty composer with no open list asks to edit the newest own message.
    if (e.key === 'ArrowUp' && !e.altKey && !e.ctrlKey && !e.metaKey && !e.shiftKey && value === '' && onArrowUpEmpty
      && combobox?.expanded !== true) {
      e.preventDefault();
      onArrowUpEmpty();
      return;
    }
    // (c) web-1's Enter rules.
    if (e.key !== 'Enter') return;
    if (e.shiftKey) return; // the browser inserts the line break
    if (e.nativeEvent.isComposing || e.keyCode === 229) return; // the IME owns this Enter
    e.preventDefault();
    // A held Enter auto-repeats: its repeats neither send again nor break the line.
    if (e.repeat) return;
    submit();
  };

  const onInput = (e: ChangeEvent<HTMLTextAreaElement>) => {
    if (!disabled) onChange(e.currentTarget.value);
    onCaret?.(e.currentTarget.selectionStart);
  };

  const reportCaret = (e: SyntheticEvent<HTMLTextAreaElement>) => {
    onCaret?.(e.currentTarget.selectionStart);
  };

  // Pasted files go to the owner (the tray); a paste of text is the browser's.
  const onPaste = (e: ClipboardEvent<HTMLTextAreaElement>) => {
    const files = e.clipboardData.files;
    if (files.length > 0 && onPasteFiles) {
      e.preventDefault();
      if (disabled) return;
      onPasteFiles(Array.from(files));
    }
  };

  // While the list is open the textarea points at it and keeps its textbox role (F10; no role=combobox).
  const listAttributes = combobox?.expanded === true ? {
    'aria-autocomplete': 'list' as const,
    'aria-controls': combobox.controls,
    ...(combobox.activeDescendant === null ? {} : { 'aria-activedescendant': combobox.activeDescendant }),
  } : {};

  const onSubmit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    submit();
  };

  const empty = value.trim() === '' && canSendEmpty !== true;
  const showTyping = typing !== undefined && typing !== null && typing !== false;

  return (
    <form className="d-composer" data-disabled={disabled ? 'true' : undefined} data-empty={empty ? 'true' : undefined}
      data-replying={replying ? 'true' : undefined} data-typing={showTyping ? 'true' : undefined} noValidate onSubmit={onSubmit}>
      {top !== undefined ? <div className="d-composer__top">{top}</div> : null}
      {showNotice ? <div id={noticeId} className="d-composer__notice">{notice}</div> : null}
      <label className="d-composer__label d-label d-sr-only" htmlFor={inputId}>
        {lead}{target === null ? null : <span className="d-composer__target">{target}</span>}
      </label>
      <div className="d-composer__box">
        {/* The 2px typing sweep on the box's top edge (§5.5), shown while someone else types. */}
        <div className="d-composer__sweep" aria-hidden="true"><span /></div>
        <div className="d-composer__row">
          <span className="d-composer__prompt" aria-hidden="true">›</span>
          <textarea ref={setInput} id={inputId} className="d-composer__input" rows={1} value={value} placeholder={placeholder}
            readOnly={disabled} aria-disabled={disabled ? true : undefined} aria-describedby={describedBy} {...listAttributes}
            onChange={onInput} onKeyDown={onKeyDown} onSelect={reportCaret} onKeyUp={reportCaret} onPaste={onPaste} />
          <button type="submit" className="d-composer__send" disabled={disabled === true}
            aria-disabled={sendRefused ? true : undefined} aria-describedby={sendRefused ? counterId : undefined}>
            {sendLabel}<span className="d-composer__send-key" aria-hidden="true"> ↵</span>
          </button>
        </div>
        <div className="d-composer__foot">
          {attachLabel !== undefined && onAttach ? (
            <button type="button" className="d-composer__tool d-composer__attach" aria-label={attachLabel} title={attachLabel} disabled={disabled === true} onClick={() => onAttach()}>
              <Icon name="paperclip" size={14} />
            </button>
          ) : null}
          {emoji !== undefined ? (
            <button ref={emoji.buttonRef} type="button" className="d-composer__tool d-composer__emoji" aria-label={emoji.label} title={emoji.label} aria-haspopup="dialog"
              aria-expanded={emoji.open} aria-controls={emoji.open ? emoji.controls : undefined} data-emoji-toggle="" disabled={disabled === true}
              onClick={() => emoji.onToggle()}>
              <Icon name="smile" size={14} />
            </button>
          ) : null}
          {gif !== undefined ? (
            <button ref={gif.buttonRef} type="button" className="d-composer__tool d-composer__gif" aria-label={gif.label} aria-haspopup="dialog"
              aria-expanded={gif.open} aria-controls={gif.open ? gif.controls : undefined} data-gif-toggle="" disabled={disabled === true}
              onClick={() => gif.onToggle()}>
              <span aria-hidden="true">GIF</span>
            </button>
          ) : null}
          {codeLabel !== undefined ? (
            <button type="button" className="d-composer__tool d-composer__code" aria-label={codeLabel} title={codeLabel} disabled={disabled === true}
              onClick={insertCode}>
              <Icon name="code-xml" size={14} />
            </button>
          ) : null}
          <span className="d-composer__spacer" />
          {showTyping ? <span className="d-composer__typing">{typing}</span> : null}
          {hint !== undefined ? <span className="d-composer__hint" aria-hidden="true">{hint}</span> : null}
        </div>
      </div>
      {showReason ? <p id={reasonId} className="d-composer__reason">{disabledReason}</p> : null}
      {showCounter ? (
        <p id={counterId} className="d-composer__counter" data-over={overBudget ? 'true' : undefined}>{counterLabel(remaining)}</p>
      ) : null}
      {/* The counter's polite status: heard, never seen (the visible counter says the same). */}
      <p className="d-composer__status d-sr-only" role="status">{status.text}</p>
    </form>
  );
}
