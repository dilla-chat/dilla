import type { KeyboardEvent } from 'react';
import { useRangeCommit } from '../internal/rangeCommit.ts';
import './SensitivityMeter.css';

export interface SensitivityMeterProps {
  /** The input level, −100..0 dB (ignored unless `testing`). */
  levelDb: number;
  /** The threshold in force: the manual one, or the computed one while `auto`. */
  thresholdDb: number;
  /** Automatic sensitivity: the line is fixed where dilla puts it and cannot be dragged. */
  auto: boolean;
  /** True while Let's check runs: only then are segments lit. */
  testing: boolean;
  /** The threshold as it moves (`commit` false) and when it is let go (`commit` true: pointer up, key up, Page Up/Down). */
  onThresholdChange(db: number, commit?: boolean): void;
  /** The threshold slider's name, e.g. "Sensitivity threshold". */
  label?: string;
}

export const SEGMENTS = 50;
const MIN = -100;
const MAX = 0;
const clamp = (db: number) => Math.max(MIN, Math.min(MAX, Math.round(db)));
/** A threshold or level as the prototype writes it: "-48 dB". */
export const formatDb = (db: number): string => `${clamp(db)} dB`;

/** Each segment's state, the prototype's rule: a segment at d dB (−100, −98, …, −2) is lit below the level; lit, it is
 *  `below` under the threshold, `hot` over −10 dB, else `on`. */
export function segmentStates(levelDb: number, thresholdDb: number, testing: boolean): Array<'off' | 'below' | 'on' | 'hot'> {
  const level = testing ? levelDb : MIN;
  return Array.from({ length: SEGMENTS }, (_, i) => {
    const db = MIN + i * 2;
    if (!(db < level)) return 'off';
    if (db < thresholdDb) return 'below';
    return db > -10 ? 'hot' : 'on';
  });
}

/**
 * README §5.9 Input sensitivity: a 50-segment meter from −100 to 0 dB with the white threshold line and its dB label above
 * it, then the scale. The line is a native range (role slider: arrows move 1 dB, Page Up/Down 10 dB) laid over the whole
 * meter; with automatic sensitivity it is disabled and the cursor says so. The segments are aria-hidden: the status pill
 * beside the meter carries the live text. The meter is exempt from reduced motion (its fade is --duration-meter-segment).
 */
export function SensitivityMeter({ levelDb, thresholdDb, auto, testing, onThresholdChange, label }: SensitivityMeterProps) {
  const thr = clamp(thresholdDb);
  // Fix round 2: moves preview; the native change (let go, a key step, an assistive set), blur or pointercancel commits.
  const range = useRangeCommit((db, commit) => onThresholdChange(db, commit), clamp);
  const left = `${thr - MIN}%`;
  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (auto || (e.key !== 'PageUp' && e.key !== 'PageDown')) return;
    e.preventDefault();
    onThresholdChange(clamp(thr + (e.key === 'PageUp' ? 10 : -10)), true);
  };
  return (
    <div className="d-sens" data-auto={auto ? 'true' : undefined}>
      <div className="d-sens__meter">
        <div className="d-sens__segs" aria-hidden="true">
          {segmentStates(levelDb, thr, testing).map((s, i) => <span key={i} className="d-sens__seg" data-state={s} />)}
        </div>
        <span className="d-sens__line" aria-hidden="true" style={{ left }} />
        <span className="d-sens__value" aria-hidden="true" style={{ left }}>{formatDb(thr)}</span>
        <input type="range" className="d-sens__input" min={MIN} max={MAX} step={1} value={thr} disabled={auto}
          aria-label={label} aria-valuetext={formatDb(thr)} onKeyDown={onKeyDown}
          {...range} />
      </div>
      <div className="d-sens__scale" aria-hidden="true"><span className="d-sens__tick">-100 dB</span><span className="d-sens__tick">-50</span><span className="d-sens__tick">0 dB</span></div>
    </div>
  );
}
