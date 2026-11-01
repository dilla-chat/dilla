// Push to talk (decision 2, flow 06 § Keyboard): opt-in, no default key, and it acts only while the key is held and
// the dilla tab has focus. Its handler calls preventDefault, so the key never types; and it never fires while focus
// is in an editable field, where the key types as usual (task-7 review ruling). Losing focus or hiding the tab
// releases a held key, so the microphone never stays open behind another window.
// Only a non-text key pressed on its own binds (task-8b review I2): a code outside PUSH_TO_TALK_KEYS binds nothing, and a
// press with Control, Alt, Meta, Shift or AltGr held is ignored.
import { isPushToTalkKey } from '@dilla/ui';

export interface PushToTalkOptions {
  /** The KeyboardEvent.code of the push-to-talk key (the voice.pttKey setting). */
  code: string;
  onChange(down: boolean): void;
  /** Defaults: the page’s document and window. */
  doc?: Document;
  win?: Window;
}

/** An input that takes text (not a button, checkbox, range and the like), a textarea, a select or a contenteditable. */
const TEXT_INPUTS = new Set(['', 'text', 'search', 'email', 'url', 'tel', 'password', 'number', 'date', 'datetime-local',
  'month', 'time', 'week']);

export function isEditable(target: EventTarget | null): boolean {
  if (typeof Element === 'undefined' || !(target instanceof Element)) return false;
  if (target instanceof HTMLTextAreaElement || target instanceof HTMLSelectElement) return true;
  if (target instanceof HTMLInputElement) return TEXT_INPUTS.has(target.type.toLowerCase());
  if (target instanceof HTMLElement && target.isContentEditable) return true;
  // jsdom has no isContentEditable; the attribute on the element or a host covers it.
  const host = target.closest('[contenteditable]');
  return host !== null && host.getAttribute('contenteditable') !== 'false';
}

/** Binds the key; returns the unbind, which also releases a held key. */
export function bindPushToTalk(o: PushToTalkOptions): () => void {
  if (!isPushToTalkKey(o.code)) return () => undefined;
  const doc = o.doc ?? document;
  const win = o.win ?? window;
  let down = false;
  const release = (): void => {
    if (!down) return;
    down = false;
    o.onChange(false);
  };
  const modified = (e: KeyboardEvent): boolean =>
    e.ctrlKey || e.altKey || e.metaKey || e.shiftKey || e.getModifierState('AltGraph');
  const editing = (e: KeyboardEvent): boolean => isEditable(e.target) || isEditable(doc.activeElement);
  const inDialog = (): boolean => doc.querySelector('dialog[open][aria-modal="true"]') !== null;
  const onFocus = (): void => { if (inDialog()) release(); };
  const onKeyDown = (e: KeyboardEvent): void => {
    if (inDialog()) { release(); return; }
    if (e.code !== o.code || modified(e) || editing(e) || !doc.hasFocus()) return;
    e.preventDefault();
    if (e.repeat || down) return;
    down = true;
    o.onChange(true);
  };
  const onKeyUp = (e: KeyboardEvent): void => {
    if (e.code !== o.code || !down) return;
    e.preventDefault();
    release();
  };
  const onHidden = (): void => { if (doc.visibilityState === 'hidden') release(); };
  doc.addEventListener('keydown', onKeyDown, true);
  doc.addEventListener('keyup', onKeyUp, true);
  doc.addEventListener('visibilitychange', onHidden);
  doc.addEventListener('focusin', onFocus);
  win.addEventListener('blur', release);
  return () => {
    doc.removeEventListener('keydown', onKeyDown, true);
    doc.removeEventListener('keyup', onKeyUp, true);
    doc.removeEventListener('visibilitychange', onHidden);
    doc.removeEventListener('focusin', onFocus);
    win.removeEventListener('blur', release);
    release();
  };
}
