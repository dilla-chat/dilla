import { useEffect, useId, useRef, type KeyboardEvent, type ReactNode } from 'react';
import { Icon, type IconName } from '../Icon/Icon.tsx';
import { Segmented } from '../Segmented/Segmented.tsx';
import { Toggle } from '../Toggle/Toggle.tsx';
import './SharePicker.css';

export type SharePickerSource = 'screen' | 'window' | 'tab';
export type SharePickerResolution = '720p' | '1080p' | '1440p' | 'source';
export type SharePickerFps = 15 | 30 | 60;
export interface SharePickerValue { source: SharePickerSource; resolution: SharePickerResolution; fps: SharePickerFps; audio: boolean }

const RESOLUTIONS: readonly SharePickerResolution[] = ['720p', '1080p', '1440p', 'source'];
const FPS: readonly SharePickerFps[] = [15, 30, 60];
const SOURCE_ICON: Record<SharePickerSource, IconName> = { screen: 'monitor', window: 'app-window', tab: 'globe' };

export interface SharePickerProps {
  /** A share is running: the title is "Stream quality" and the action "Apply" (the prototype's chevron popover). */
  sharing: boolean;
  value: SharePickerValue;
  onChange(value: SharePickerValue): void;
  /**
   * The sources this browser can pick (Chromium: all three). Empty: the source row and its preview are not drawn, because the
   * browser ignores the hint (Firefox, Safari) and a choice there would do nothing.
   */
  sources: readonly SharePickerSource[];
  /**
   * The sound row: 'toggle' (the switch and its note), 'note' (the note alone: this browser cannot take sound from the picked
   * source, so no switch is drawn), 'none' (no row: the browser takes no sound at all).
   */
  audio: 'toggle' | 'note' | 'none';
  /** The preview's line for the picked source ("Display · 2560×1440"). */
  preview: string;
  /** The live upload estimate ("≈ 4.5 Mb/s up"); `heavy` draws it in --warn. */
  estimate: { text: string; heavy: boolean };
  labels: {
    title: string; titleSharing: string; close: string; source: string; resolution: string; frameRate: string;
    sources: Record<SharePickerSource, string>;
    /** The 'source' resolution's word ("Source"); the others are their own names. */
    native: string;
    audio: string; audioNote: string; go: string; apply: string;
  };
  /** Go live (not sharing) or Apply (sharing). */
  onSubmit(): void;
  /** The ×, Escape, or a press outside the picker and outside `anchor`. */
  onClose(reason: 'close' | 'escape' | 'outside'): void;
  /** The controls that open it (Share, the chevron): a press on them is not "outside" (they toggle it themselves). */
  anchor?(target: Node): boolean;
}

/**
 * The share picker (Signal README §5.6, the prototype's popover above Share): a non-modal dialog with the source (Screen, Window,
 * Tab) and its striped preview, Resolution (720p, 1080p, 1440p, Source), Frame rate (15, 30, 60), "Also share system audio" with
 * its note, and the foot: the upload estimate and Go live (Apply while sharing). Focus goes to its first choice when it opens;
 * Escape closes it, as does the × and a press outside. The caller places it (CallControls hangs it over the Share control) and
 * says what this browser can act on, so no control here does nothing.
 */
export function SharePicker({ sharing, value, onChange, sources, audio, preview, estimate, labels, onSubmit, onClose, anchor }: SharePickerProps) {
  const id = useId();
  const root = useRef<HTMLDivElement>(null);
  useEffect(() => {
    root.current?.querySelector<HTMLElement>('[role="radio"][tabindex="0"]')?.focus();
  }, []);
  useEffect(() => {
    const down = (e: PointerEvent) => {
      if (!(e.target instanceof Node)) return;
      if (root.current?.contains(e.target) || anchor?.(e.target) === true) return;
      onClose('outside');
    };
    document.addEventListener('pointerdown', down, true);
    return () => document.removeEventListener('pointerdown', down, true);
  }, [onClose, anchor]);
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key !== 'Escape') return;
    e.preventDefault();
    e.stopPropagation();
    onClose('escape');
  };
  const set = (patch: Partial<SharePickerValue>) => { onChange({ ...value, ...patch }); };
  const field = (label: string, control: ReactNode) => (
    <div className="d-share-picker__field">
      <span className="d-share-picker__label" aria-hidden="true">{label}</span>
      {control}
    </div>
  );
  return (
    <div ref={root} className="d-share-picker" role="dialog" aria-modal="false" aria-labelledby={`${id}-title`} onKeyDown={onKeyDown}>
      <div className="d-share-picker__head">
        <b id={`${id}-title`} className="d-share-picker__title">{sharing ? labels.titleSharing : labels.title}</b>
        <button type="button" className="d-share-picker__close" aria-label={labels.close} onClick={() => { onClose('close'); }}>
          <Icon name="x" size={14} />
        </button>
      </div>
      {sources.length > 0 ? (
        <>
          {field(labels.source, <Segmented id={`${id}-source`} label={labels.source} value={value.source}
            options={sources.map(s => ({ value: s, label: labels.sources[s] }))} onChange={v => { set({ source: v as SharePickerSource }); }} />)}
          <div className="d-share-picker__preview" aria-hidden="true"><Icon name={SOURCE_ICON[value.source]} size={14} />{preview}</div>
        </>
      ) : null}
      {field(labels.resolution, <Segmented id={`${id}-resolution`} label={labels.resolution} value={value.resolution}
        options={RESOLUTIONS.map(r => ({ value: r, label: r === 'source' ? labels.native : r }))} onChange={v => { set({ resolution: v as SharePickerResolution }); }} />)}
      {field(labels.frameRate, <Segmented id={`${id}-fps`} label={labels.frameRate} value={String(value.fps)}
        options={FPS.map(f => ({ value: String(f), label: String(f) }))} onChange={v => { set({ fps: Number(v) as SharePickerFps }); }} />)}
      {audio !== 'none' ? (
        <div className="d-share-picker__audio" data-note-only={audio === 'note' ? 'true' : undefined}>
          <span className="d-share-picker__audio-text">
            <span className="d-share-picker__audio-label" aria-hidden={audio === 'toggle' ? 'true' : undefined}>{labels.audio}</span>
            <span id={`${id}-audio-note`} className="d-share-picker__note">{labels.audioNote}</span>
          </span>
          {audio === 'toggle' ? <Toggle id={`${id}-audio`} label={labels.audio} checked={value.audio} describedBy={`${id}-audio-note`}
            onChange={on => { set({ audio: on }); }} /> : null}
        </div>
      ) : null}
      <div className="d-share-picker__foot">
        <span className="d-share-picker__estimate" data-heavy={estimate.heavy ? 'true' : undefined} role="status">{estimate.text}</span>
        <button type="button" className="d-share-picker__go" onClick={onSubmit}>
          <Icon name="monitor-up" size={14} />{sharing ? labels.apply : labels.go}
        </button>
      </div>
    </div>
  );
}
