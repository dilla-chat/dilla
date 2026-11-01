import { Fragment, useEffect, useId, useLayoutEffect, useRef, useState, type FocusEvent, type KeyboardEvent, type PointerEvent, type ReactNode, type RefObject } from 'react';
import { Button } from '../Button/Button.tsx';
import { Icon, type IconName } from '../Icon/Icon.tsx';
import { arrowIndex } from '../internal/arrow-select.ts';
import { SharePicker, type SharePickerProps } from '../SharePicker/SharePicker.tsx';
import { newToastHostId, ToastHost } from '../Toast/Toast.tsx';
import './CallControls.css';

/**
 * Roving focus for a horizontal `role="toolbar"` of buttons (the voice dock and the call controls). One button holds
 * the tab stop: the one that last had focus, initially the first. ArrowLeft and ArrowRight move with wrap, Home and
 * End go to the ends; a key with Alt, Ctrl, Meta or Shift is left alone (Ctrl/⌘+Shift+M and D belong to the shell).
 * An `aria-disabled` button stays in the roving order, so it can be found and read.
 */
export function useToolbarFocus(count: number) {
  const ref = useRef<HTMLDivElement>(null);
  const [stop, setStop] = useState(0);
  const current = stop < count ? stop : 0;
  const buttons = (): HTMLButtonElement[] => Array.from(ref.current?.querySelectorAll<HTMLButtonElement>(':scope > button') ?? []);
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const list = buttons();
    const target: EventTarget = e.target;
    const from = target instanceof HTMLButtonElement ? list.indexOf(target) : -1;
    if (from < 0) return;
    const next = arrowIndex(e, from, list.length, 'horizontal');
    if (next === null) return;
    e.preventDefault();
    setStop(next);
    list[next].focus();
  };
  // A click or a programmatic focus moves the stop too.
  const onFocus = (e: FocusEvent<HTMLDivElement>) => {
    const target: EventTarget = e.target;
    const i = target instanceof HTMLButtonElement ? buttons().indexOf(target) : -1;
    if (i >= 0) setStop(i);
  };
  return { ref, onKeyDown, onFocus, tabIndex: (i: number) => (i === current ? 0 : -1) };
}

/**
 * The mute and deafen shortcuts as `aria-keyshortcuts` (flow 06, decision 1): Control or ⌘ with Shift, the web-2c
 * convention of Control/⌘+K, where either modifier works on every platform.
 */
export const VOICE_SHORTCUTS = { mute: 'Control+Shift+M Meta+Shift+M', deafen: 'Control+Shift+D Meta+Shift+D' } as const;

/**
 * The glyphs of the five voice controls (plan 2026-10-15). A toggle's glyph is its state, never colour alone: muted is
 * mic-off, deafened headphone-off, a live camera video (video-off while off), sharing monitor-x (monitor-up to start).
 */
export const VOICE_ICONS = {
  mute: { icon: 'mic', iconOn: 'mic-off', tint: 'danger' },
  deafen: { icon: 'headphones', iconOn: 'headphone-off', tint: 'danger' },
  camera: { icon: 'video-off', iconOn: 'video', tint: 'accent' },
  share: { icon: 'monitor-up', iconOn: 'monitor-x', tint: 'accent' },
  leave: { icon: 'phone-off', apart: true },
} as const satisfies Record<string, Pick<ToolbarControl, 'icon' | 'iconOn' | 'tint' | 'apart'>>;

/**
 * One control of a voice toolbar: an icon button named by `label` (`aria-label`). A toggle (`on` given) keeps one
 * label in both states with `aria-pressed` (WB a11y I4); its glyph swaps to `iconOn` while on, and it takes the `tint`
 * colour. An inert control is aria-disabled.
 */
export interface ToolbarControl {
  id: string; label: string; on?: boolean; inert?: boolean; danger?: boolean;
  icon: IconName;
  /** The glyph while `on`; `icon` when absent. */
  iconOn?: IconName;
  /** Set apart from the controls before it by a hairline divider (leave), in the same roving order. */
  apart?: boolean;
  /** The colour of the on state: `danger` (mute, deafen; the default) or `accent` (camera, share). */
  tint?: 'danger' | 'accent';
  /** `aria-keyshortcuts`, e.g. VOICE_SHORTCUTS.mute. */
  keys?: string;
  /** The shortcut as shown in the tooltip, formatted for the platform by the caller, e.g. "Ctrl+Shift+M" or "⌘⇧M". */
  shortcut?: string;
  /** Why an inert control is inert (`voice.why.*`): shown in the tooltip after the label and read as its description. */
  why?: string;
  /**
   * Signal's call bar (§5.6): the visible word after the glyph, which is then the control's name (no `aria-label`). With
   * `swap`, the word carries the state ("Unmute" while muted, the prototype's copy) and the control is a plain button with
   * no `aria-pressed`: a label and a pressed state must not both say it (WCAG 2.5.3, 4.1.2).
   */
  text?: string;
  swap?: boolean;
  /** The shortcut drawn after the word (10px at 0.6), e.g. ⌘⇧M. Decorative: `keys` and the description say it. */
  keycap?: string;
  /** Task 8c: the control opens a dialog (the share picker): `aria-haspopup="dialog"` and this as `aria-expanded`. */
  expanded?: boolean;
  onAction(): void;
}

/** The reasons an inert camera or share control gives (`voice.why.paused`, `voice.why.joining`, `voice.why.noShare`). */
export interface VoiceWhyLabels { paused: string; joining: string; noShare: string }

/**
 * Why camera and share are inert, one reason each, by priority (Jonas, 2026-10-15): joining, then paused, then (share
 * only) no screen capture. Nothing while a control is live, or without the labels.
 */
export function voiceWhy(
  labels: VoiceWhyLabels | undefined,
  { busy, paused, canCamera, canShare }: { busy: boolean; paused: boolean; canCamera: boolean; canShare: boolean },
): { camera?: string; share?: string } {
  if (labels === undefined) return {};
  const pick = (inert: boolean, capture: boolean): string | undefined => {
    if (!inert) return undefined;
    if (busy) return labels.joining;
    if (paused) return labels.paused;
    return capture ? undefined : labels.noShare;
  };
  return { camera: pick(busy || !canCamera, true), share: pick(busy || !canShare, canShare) };
}

/** The space between a control and its tooltip, in CSS px; the tip's transparent bridge covers it (CallControls.css). */
const TIP_GAP = 6;

type TipOrigin = 'hover' | 'focus';

/**
 * A voice toolbar: the controls as one roving tab stop, named by `label`, with a control marked `apart` (leave) set
 * off by a hairline divider. One tooltip serves the toolbar (WCAG 1.4.13), a sibling of the buttons and never part of
 * a button's hit area. It shows the label and any shortcut of the most recently hovered or keyboard-focused control:
 * - hover: a mouse or pen only, never a touch; the pointer can move onto the tip (a bridge covers the gap), and the tip
 *   takes the pointer then, over its own area only;
 * - keyboard focus: never a focus that a press caused; the tip takes no pointer at all;
 * - Escape, wherever focus is, hides a shown tip and is consumed (`preventDefault`), and does nothing else.
 * It is never a `title`. An inert control's tip adds why ("camera — paused"). Only the shortcut and the reason describe
 * a control (`aria-describedby`), so the name is not read twice.
 */
export function VoiceToolbar({ label, controls, className, keyshortcuts = true, popover, rootRef }: {
  label: string; controls: readonly ToolbarControl[]; className?: string;
  /** False: the controls carry no `aria-keyshortcuts` and no `-keys` description; a shortcut still shows in the tip (web3.8, R3). */
  keyshortcuts?: boolean;
  /**
   * Task 8c: a popover hung 10px above control `anchor`, its right edge on the control's (the prototype's share picker), kept
   * inside the window. While it shows, no tooltip does.
   */
  popover?: { anchor: string; node: ReactNode } | null;
  /** Given, it holds the toolbar's element (the caller finds a control by `data-control`). */
  rootRef?: RefObject<HTMLDivElement | null>;
}): ReactNode {
  const bar = useToolbarFocus(controls.length);
  useLayoutEffect(() => { if (rootRef !== undefined) rootRef.current = bar.ref.current; });
  const uid = useId();
  const tipRef = useRef<HTMLSpanElement>(null);
  const pressed = useRef(false);
  const lastHover = useRef<string | null>(null);
  const [hovered, setHovered] = useState<string | null>(null);
  const [focused, setFocused] = useState<string | null>(null);
  const [last, setLast] = useState<TipOrigin>('focus');
  const [dismissed, setDismissed] = useState<string | null>(null);
  const primary = last === 'hover' ? hovered ?? focused : focused ?? hovered;
  const origin: TipOrigin = primary !== null && primary === hovered && (last === 'hover' || primary !== focused) ? 'hover' : 'focus';
  const shown = primary !== null && primary !== dismissed && popover == null ? controls.find(c => c.id === primary) ?? null : null;
  const popRef = useRef<HTMLSpanElement>(null);
  const popAnchor = popover?.anchor ?? null;
  // The popover: its right edge on the anchor's, 10px above it; moved right when that puts it past the window's left 12px.
  useLayoutEffect(() => {
    const pop = popRef.current;
    const root = bar.ref.current;
    if (pop === null || root === null || popAnchor === null) return;
    const button = Array.from(root.querySelectorAll<HTMLElement>(':scope > button')).find(b => b.dataset.control === popAnchor);
    if (button === undefined) return;
    let right = root.clientWidth - (button.offsetLeft + button.offsetWidth);
    pop.style.right = `${String(right)}px`;
    pop.style.bottom = `${String(root.clientHeight - button.offsetTop + 10)}px`;
    const over = 12 - pop.getBoundingClientRect().left;
    if (over > 0) { right -= over; pop.style.right = `${String(right)}px`; }
  });
  const shownId = shown?.id ?? null;

  // Escape hides the tip wherever focus is, and only that: the key is consumed so nothing else acts on it.
  useEffect(() => {
    if (shownId === null) return;
    const onKey = (e: globalThis.KeyboardEvent) => {
      if (e.key !== 'Escape') return;
      e.preventDefault();
      e.stopPropagation();
      setDismissed(shownId);
    };
    document.addEventListener('keydown', onKey, true);
    return () => { document.removeEventListener('keydown', onKey, true); };
  }, [shownId]);

  // Above its control, centred on it and kept inside the toolbar's width.
  useLayoutEffect(() => {
    const tip = tipRef.current;
    const root = bar.ref.current;
    if (tip === null || root === null || shownId === null) return;
    const button = Array.from(root.querySelectorAll<HTMLElement>(':scope > button')).find(b => b.dataset.control === shownId);
    if (button === undefined) return;
    const left = button.offsetLeft + button.offsetWidth / 2 - tip.offsetWidth / 2;
    tip.style.left = `${String(Math.max(0, Math.min(left, root.clientWidth - tip.offsetWidth)))}px`;
    tip.style.top = `${String(button.offsetTop - tip.offsetHeight - TIP_GAP)}px`;
  });

  const focusVisible = (el: HTMLElement): boolean => {
    try { return el.matches(':focus-visible'); } catch { return true; }
  };
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => { pressed.current = false; bar.onKeyDown(e); };
  const intoTip = (to: EventTarget | null): boolean => to instanceof Node && tipRef.current?.contains(to) === true;
  const describedBy = (c: ToolbarControl): string | undefined => {
    const ids = [keyshortcuts && c.shortcut !== undefined ? `${uid}-${c.id}-keys` : null, c.why !== undefined ? `${uid}-${c.id}-why` : null];
    const list = ids.filter((id): id is string => id !== null);
    return list.length > 0 ? list.join(' ') : undefined;
  };

  return (
    <div ref={bar.ref} role="toolbar" aria-label={label} aria-orientation="horizontal"
      className={['d-voice-toolbar', className].filter(Boolean).join(' ')} onKeyDown={onKeyDown} onFocus={bar.onFocus}
      onPointerDownCapture={() => { pressed.current = true; }}>
      {controls.map((c, i) => (
        <Fragment key={c.id}>
          {c.apart === true ? <span className="d-voice-toolbar__divider" aria-hidden="true" /> : null}
          <Button variant={c.danger === true ? 'danger' : 'default'} className="d-voice-toolbar__control" data-control={c.id}
            data-tint={c.on === undefined ? undefined : (c.tint ?? 'danger')} data-on={c.on === true ? 'true' : undefined}
            pressed={c.swap === true ? undefined : c.on} mark={false} aria-label={c.text === undefined ? c.label : undefined}
            aria-keyshortcuts={keyshortcuts ? c.keys : undefined} aria-describedby={describedBy(c)}
            aria-disabled={c.inert === true ? true : undefined} tabIndex={bar.tabIndex(i)}
            aria-haspopup={c.expanded === undefined ? undefined : 'dialog'} aria-expanded={c.expanded}
            onFocus={e => {
              setDismissed(null);
              // A press focuses the control too; only a keyboard (or script) focus shows the focus tip.
              if (pressed.current) { pressed.current = false; return; }
              if (!focusVisible(e.currentTarget)) return;
              setFocused(c.id);
              setLast('focus');
            }}
            onBlur={() => { setFocused(f => (f === c.id ? null : f)); }}
            onPointerEnter={(e: PointerEvent<HTMLButtonElement>) => {
              if (e.pointerType === 'touch') return;
              setDismissed(null);
              lastHover.current = c.id;
              setHovered(c.id);
              setLast('hover');
            }}
            onPointerLeave={(e: PointerEvent<HTMLButtonElement>) => {
              if (intoTip(e.relatedTarget)) return;
              setHovered(h => (h === c.id ? null : h));
            }}
            onClick={() => { if (c.inert !== true) c.onAction(); }}>
            <Icon name={c.on === true ? c.iconOn ?? c.icon : c.icon} size={16} />
            {c.text !== undefined ? <span className="d-voice-toolbar__text">{c.text}</span> : null}
            {c.keycap !== undefined ? <span className="d-voice-toolbar__keycap" aria-hidden="true">{c.keycap}</span> : null}
          </Button>
        </Fragment>
      ))}
      {keyshortcuts ? controls.filter(c => c.shortcut !== undefined).map(c => (
        <span key={c.id} id={`${uid}-${c.id}-keys`} hidden>{c.shortcut}</span>
      )) : null}
      {controls.filter(c => c.why !== undefined).map(c => (
        <span key={c.id} id={`${uid}-${c.id}-why`} hidden>{c.why}</span>
      ))}
      <span ref={tipRef} className="d-voice-toolbar__tip" role="tooltip" hidden={shown === null}
        data-for={shown?.id} data-origin={shown === null ? undefined : origin}
        // Hoverable: the pointer moved from the control onto its tip (over the bridge), so the tip stays.
        onPointerEnter={e => { if (e.pointerType !== 'touch' && lastHover.current !== null) setHovered(lastHover.current); }}
        onPointerLeave={e => {
          const to = e.relatedTarget;
          if (to instanceof Element && to.closest('[data-control]')?.getAttribute('data-control') === shownId) return;
          lastHover.current = null;
          setHovered(null);
        }}>
        {shown !== null ? <span>{shown.why !== undefined ? `${shown.text ?? shown.label} — ${shown.why}` : shown.text ?? shown.label}</span> : null}
        {shown?.shortcut !== undefined ? <kbd className="d-voice-toolbar__keys">{shown.shortcut}</kbd> : null}
      </span>
      {popover != null ? <span ref={popRef} className="d-voice-toolbar__popover">{popover.node}</span> : null}
    </div>
  );
}

export interface CallControlsProps {
  muted: boolean; deafened: boolean; camera: boolean; sharing: boolean;
  /** False while the call is paused or held (RV-3): the camera control is aria-disabled. (No caller checks for a camera device yet.) */
  canCamera: boolean;
  /** False without screen capture, or while the call is paused (RV-3): the share control is aria-disabled. */
  canShare: boolean;
  /** True on a phone that cannot capture a screen: the share control is left out (a tap would show no reason). */
  hideShare?: boolean;
  /** True while joining: every control is aria-disabled. */
  busy: boolean;
  /** True while the call is paused or held; it picks the reason camera and share give. Absent, `!canCamera` stands for it. */
  paused?: boolean;
  /** `why`: the reasons an inert camera or share gives in its tooltip and description (`voice.why.*`); none without it. */
  labels: { region: string; mute: string; deafen: string; camera: string; share: string; leave: string; why?: VoiceWhyLabels;
    /**
     * Signal's call bar (§5.6): the visible words, which name the controls. Mute, deafen and share swap their word with the
     * state (Mute / Unmute, Deafen / Undeafen, Share / Stop sharing) and carry no `aria-pressed`; Camera keeps its word and
     * `aria-pressed`. Without them the controls are the icon-only toolbar named by the labels above.
     */
    bar?: { mute: string; unmute: string; deafen: string; undeafen: string; camera: string; share: string; stopShare: string; leave: string };
  };
  /** Optional: the mute and deafen shortcuts as the tooltips show them, formatted per platform by the caller. */
  shortcuts?: { mute: string; deafen: string };
  /**
   * Task 8c (Signal §5.6): the share picker. Given, Share opens it while not sharing (Share still stops a running share), and a
   * live Share gets the prototype's 30px "Stream quality" chevron (`quality`, its name) that opens it with Apply. The picker hangs
   * above them; Escape, the × and Go live / Apply hand focus back to the control that opened it.
   */
  picker?: Omit<SharePickerProps, 'sharing' | 'onClose' | 'anchor'> & { open: boolean; onOpenChange(open: boolean): void; quality: string };
  /**
   * Default true. False (web3.8, R3: the stage toolbar on desktop, where the user panel owns the shortcuts): mute and deafen
   * carry no `aria-keyshortcuts` and no `-keys` description, so one element per shortcut announces it; the tooltip still shows it.
   */
  keyshortcuts?: boolean;
  onMute(): void; onDeafen(): void; onCamera(): void; onShare(): void; onLeave(): void;
}

/**
 * The call stage's controls (`role="toolbar"`): mute, deafen, camera, share screen and leave, as icon buttons. The
 * toggles keep one label with `aria-pressed`, and their glyph swaps with the state, never colour alone (WB a11y I4).
 * Mute and deafen carry their shortcuts. Busy (joining) makes every control aria-disabled; it stays focusable.
 */
export function CallControls({
  muted, deafened, camera, sharing, canCamera, canShare, hideShare, busy, paused, labels, shortcuts, keyshortcuts, picker, onMute, onDeafen, onCamera, onShare, onLeave,
}: CallControlsProps) {
  const rootRef = useRef<HTMLDivElement>(null);
  const [toastHost] = useState(newToastHostId);
  const opener = sharing ? 'quality' : 'share';
  const focusOpener = () => { rootRef.current?.querySelector<HTMLElement>(`[data-control="${opener}"]`)?.focus(); };
  const shareInert = busy || !canShare;
  const onShareAction = picker === undefined || sharing ? onShare : () => { picker.onOpenChange(!picker.open); };
  const why = voiceWhy(labels.why, { busy, paused: paused ?? !canCamera, canCamera, canShare });
  const bar = labels.bar;
  // The prototype's mute shows on (mic-off, Unmute) while deafened too: deafened is muted as well.
  const muteOn = bar !== undefined ? muted || deafened : muted;
  const words = (text: string | undefined, keycap?: string) => (bar === undefined ? {} : { text, swap: true, ...(keycap !== undefined ? { keycap } : {}) });
  const controls: ToolbarControl[] = [
    { id: 'mute', label: labels.mute, on: muteOn, inert: busy, keys: VOICE_SHORTCUTS.mute, shortcut: shortcuts?.mute, ...VOICE_ICONS.mute,
      ...words(muteOn ? bar?.unmute : bar?.mute, shortcuts?.mute), onAction: onMute },
    { id: 'deafen', label: labels.deafen, on: deafened, inert: busy, keys: VOICE_SHORTCUTS.deafen, shortcut: shortcuts?.deafen, ...VOICE_ICONS.deafen,
      ...words(deafened ? bar?.undeafen : bar?.deafen, shortcuts?.deafen), onAction: onDeafen },
    { id: 'camera', label: labels.camera, on: camera, inert: busy || !canCamera, why: why.camera, ...VOICE_ICONS.camera,
      ...(bar === undefined ? {} : { text: bar.camera }), onAction: onCamera },
    { id: 'share', label: labels.share, on: sharing, inert: shareInert, why: why.share, ...VOICE_ICONS.share,
      ...(bar === undefined ? {} : { iconOn: 'monitor-up' as const }), ...words(sharing ? bar?.stopShare : bar?.share),
      ...(picker !== undefined && !sharing ? { expanded: picker.open } : {}), onAction: onShareAction },
    ...(picker !== undefined && sharing ? [{ id: 'quality', label: picker.quality, inert: shareInert, icon: 'chevron-up' as const, expanded: picker.open,
      onAction: () => { picker.onOpenChange(!picker.open); } }] : []),
    { id: 'leave', label: labels.leave, inert: busy, danger: true, ...VOICE_ICONS.leave, ...(bar === undefined ? {} : { text: bar.leave }), onAction: onLeave },
  ].filter(c => c.id !== 'share' || hideShare !== true);
  const popover = picker !== undefined && picker.open && !shareInert ? {
    anchor: opener,
    node: <SharePicker {...picker} sharing={sharing}
      anchor={target => target instanceof Element && target.closest('[data-control="share"], [data-control="quality"]') !== null && rootRef.current?.contains(target) === true}
      onSubmit={() => { picker.onOpenChange(false); picker.onSubmit(); focusOpener(); }}
      onClose={reason => { picker.onOpenChange(false); if (reason !== 'outside') focusOpener(); }} />,
  } : null;
  // Task 8c fix round 2: the share's toasts ("You're live", "Stream set to …", shown by the caller through showToast) need a
  // host while the call bar shows, as the prototype's toast stack.
  return <><VoiceToolbar label={labels.region} controls={controls} className={bar === undefined ? 'd-call-controls' : 'd-call-controls d-call-controls--bar'}
    keyshortcuts={keyshortcuts} popover={popover} rootRef={rootRef} />{picker !== undefined ? <ToastHost id={toastHost} /> : null}</>;
}

