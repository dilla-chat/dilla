import { useEffect, useId, useRef, useState, type CSSProperties, type KeyboardEvent } from 'react';
import { Segmented } from '../Segmented/Segmented.tsx';
import { Icon } from '../Icon/Icon.tsx';
import { Toggle } from '../Toggle/Toggle.tsx';
import { SensitivityMeter, formatDb } from '../SensitivityMeter/SensitivityMeter.tsx';
import { useRangeCommit } from '../internal/rangeCommit.ts';
import '../Button/Button.css';
import './VoiceSettings.css';

/** A captured push-to-talk key: the KeyboardEvent.code and the modifiers held with it. */
export interface CapturedKey { code: string; ctrl: boolean; alt: boolean; meta: boolean; shift: boolean }
export type CaptureKeyAction = 'start' | 'cancel' | CapturedKey;

/** Keys that never stand alone: they wait for the key pressed with them. */
const MODIFIERS = new Set(['Control', 'Alt', 'AltGraph', 'Meta', 'Shift', 'OS', 'Hyper', 'Super', 'CapsLock', 'NumLock', 'Fn', 'FnLock']);
/**
 * The keys push to talk may use (task-8b review I2, superseding the task-7 M9 ruling): non-text keys that neither move
 * focus nor have a browser function, pressed on their own. The voice.pttKey setting stores the KeyboardEvent.code
 * alone, so a combination with Control, Alt, Meta, Shift or AltGr could never be matched and is refused too.
 */
export const PUSH_TO_TALK_KEYS: ReadonlySet<string> = new Set([
  ...Array.from({ length: 12 }, (_, i) => `F${i + 13}`), 'Pause', 'ScrollLock', 'Insert',
]);

/** Whether `code` (a KeyboardEvent.code) may be the push-to-talk key. */
export function isPushToTalkKey(code: string): boolean {
  return PUSH_TO_TALK_KEYS.has(code);
}

/**
 * Whether a captured key is refused as the push-to-talk key: anything but one of PUSH_TO_TALK_KEYS on its own. A key
 * that types text (AltGr included), moves focus or operates the page is refused with or without modifiers.
 */
export function typesOrOperates(e: {
  key: string; code?: string; ctrlKey: boolean; altKey: boolean; metaKey: boolean; shiftKey?: boolean;
  getModifierState?(key: 'AltGraph'): boolean;
}): boolean {
  if (e.ctrlKey || e.altKey || e.metaKey || e.shiftKey === true || e.getModifierState?.('AltGraph') === true) return true;
  return !isPushToTalkKey(e.code !== undefined && e.code !== '' ? e.code : e.key);
}

/** Signal task 10b: the Mic test group's state (README §5.9 Input sensitivity, Let's check). */
export interface MicTestState {
  /** Let's check runs: the meter is live and the pill says whether you are heard. */
  testing: boolean;
  /** False while a call holds the microphone: Let's check cannot start. */
  available: boolean;
  /** The level after the input volume, −100..0 dB. */
  levelDb: number;
  /** The threshold in force (the computed one while automatic). */
  thresholdDb: number;
  auto: boolean;
  /** Fix round 1: the gate is open (the stage's own decision, with its hold), which the pill shows. */
  open: boolean;
}

/** Fix round 1: a slider's value while it moves (`commit` false) and when it is let go (`commit` true: pointer up or key up). */
export type SliderChange = (value: number, commit: boolean) => void;
/** The pill's state is announced only once it has held this long, so a talking voice is not read out word by word. */
export const PILL_ANNOUNCE_MS = 1000;

export interface VoiceSettingsProps {
  inputs: readonly { id: string; label: string }[];
  outputs: readonly { id: string; label: string }[];
  /** The chosen device ids; '' is the system default. */
  input: string;
  output: string;
  /** False where the browser cannot route output (no `setSinkId`): the output row says so instead. */
  canSelectOutput: boolean;
  /** Input and output volume, 0..200 %. */
  inputVolume: number;
  outputVolume: number;
  mode: 'vad' | 'ptt';
  /** The push-to-talk key's shown name, or null: there is no default key (decision 2). */
  pttKey: string | null;
  /** True while the next key press becomes the push-to-talk key. */
  capturing: boolean;
  /** Push to talk's release delay, 0..1000 ms in steps of 20. */
  pttReleaseMs: number;
  noise: 'automatic' | 'lightweight' | 'off';
  micTest: MicTestState;
  /** Echo cancellation and automatic gain; undefined where the browser cannot apply the switch (the row is not shown). */
  echo?: boolean;
  agc?: boolean;
  labels: {
    input: string; output: string; systemDefault: string; outputUnsupported: string;
    mode: string; vad: string; ptt: string; pttKey: string; pttNone: string; pttSet: string; pttPrompt: string; pttRefused: string;
    /** Ruling AX: while listening the key box says "Press a key…" and the button "Cancel". */
    pttListening: string; pttCancel: string;
    noise: string; automatic: string; lightweight: string; off: string;
    /** The prototype's section headings (README §5.9): Devices, Mic test, Input mode, Processing. */
    devices?: string; test?: string; inputMode?: string; processing?: string;
    /** Under the push-to-talk key: where it works and which keys it takes. */
    pttHint?: string;
    /** The noise suppression row's description for each choice, and its tag (BETA) while Automatic is a beta. */
    noiseHints?: { automatic: string; lightweight: string; off: string };
    noiseTag?: string;
    /** Task 10b: "Volume" beside each device's slider, and the sliders' names. */
    volume: string; inputVolume: string; outputVolume: string;
    /** Let's check / Stop testing, the Mic test description, and the description while a call holds the microphone. */
    testStart: string; testStop: string; testHint: string; testUnavailable: string;
    /** Fix round 1 (L3): said under the description while echo cancellation is off. */
    testEchoOff: string;
    /** The status pill and the two readouts ("level {value}", "threshold {value}"; the level is "—" while not testing). */
    heard: string; tooQuiet: string; notTesting: string; levelValue: string; thresholdValue: string; noLevel: string;
    /** The threshold slider's name; automatic sensitivity's row and its description in each state. */
    threshold: string; auto: string; autoHintOn: string; autoHintOff: string;
    /** Push to talk's release delay and its note. */
    release: string; releaseHint: string;
    echo: string; echoHint: string; agc: string; agcHint: string;
  };
  onInput(id: string): void;
  onOutput(id: string): void;
  onInputVolume: SliderChange;
  onOutputVolume: SliderChange;
  onMode(mode: 'vad' | 'ptt'): void;
  /**
   * 'start' from the set-key button; then the captured key (its KeyboardEvent.code and modifiers) for an accepted key,
   * or 'cancel' on Escape, Tab or leaving the button. A key that types text or operates the page is refused: the
   * prompt says why (`settings.voice.pttRefused`) and the capture goes on.
   */
  onCaptureKey(action: CaptureKeyAction): void;
  onPttRelease: SliderChange;
  onNoise(mode: 'automatic' | 'lightweight' | 'off'): void;
  /** Let's check pressed: starts the ten-second test, or stops it while it runs. */
  onMicTest(): void;
  onThreshold: SliderChange;
  onAuto(on: boolean): void;
  onEcho(on: boolean): void;
  onAgc(on: boolean): void;
}

const fill = (percent: number): CSSProperties => ({ ['--d-fill' as string]: `${percent}%` });
const fmt = (template: string, value: string) => template.replace('{value}', value);

/** A native select of devices, the system default first. */
function DeviceSelect({ id, value, devices, systemDefault, label, onChange }: {
  id: string; value: string; devices: VoiceSettingsProps['inputs']; systemDefault: string; label: string; onChange(id: string): void;
}) {
  return (
    <select id={id} className="d-voice-settings__select" aria-label={label} value={value} onChange={e => onChange(e.target.value)}>
      <option value="">{systemDefault}</option>
      {devices.map(d => <option key={d.id} value={d.id}>{d.label}</option>)}
    </select>
  );
}

/** The prototype's volume row under a device: "Volume", the 0–200 % range with its accent fill, the value. */
/** A range's handlers: each move previews; the native change (let go, a key step, an assistive set), blur or
 *  pointercancel commits (fix round 2). */
export const useSliding = (onChange: SliderChange) => useRangeCommit(onChange);

function VolumeRow({ label, name, value, onChange }: { label: string; name: string; value: number; onChange: SliderChange }) {
  const sliding = useSliding(onChange);
  return (
    <div className="d-voice-settings__volume">
      <span className="d-voice-settings__volume-label" aria-hidden="true">{label}</span>
      <input type="range" className="d-voice-settings__range" min={0} max={200} step={1} value={value} aria-label={name}
        aria-valuetext={`${value}%`} style={fill(value / 2)} {...sliding} />
      <span className="d-voice-settings__volume-value" aria-hidden="true">{value}%</span>
    </div>
  );
}

/** A switch row (the prototype's auto, echo and AGC rows): label and description, the switch on the right, a hairline above. */
function SwitchRow({ id, label, hint, checked, onChange }: { id: string; label: string; hint: string; checked: boolean; onChange(on: boolean): void }) {
  return (
    <div className="d-voice-settings__switch-row">
      <span className="d-voice-settings__text">
        <span className="d-voice-settings__row-label" aria-hidden="true">{label}</span>
        <span id={`${id}-desc`} className="d-voice-settings__row-hint">{hint}</span>
      </span>
      <Toggle id={id} label={label} checked={checked} onChange={onChange} describedBy={`${id}-desc`} />
    </div>
  );
}

/**
 * Settings → Voice (flow 06, README §5.9): devices with their volumes, the Mic test (Let's check, the sensitivity meter
 * with its threshold, the status pill, automatic sensitivity), the input mode (voice activity by default, push to talk
 * opt-in with its key and release delay) and processing (noise suppression, echo cancellation, automatic gain).
 * Capturing the key: the set-key button takes focus and the prompt is announced; the next accepted key is saved without
 * typing anything; a key that types text or operates the page is refused with a reason; Tab still leaves, which
 * cancels; Escape cancels.
 */
export function VoiceSettings({
  inputs, outputs, input, output, canSelectOutput, inputVolume, outputVolume, mode, pttKey, capturing, pttReleaseMs, noise, micTest,
  echo, agc, labels, onInput, onOutput, onInputVolume, onOutputVolume, onMode, onCaptureKey, onPttRelease, onNoise, onMicTest,
  onThreshold, onAuto, onEcho, onAgc,
}: VoiceSettingsProps) {
  const id = useId();
  const releaseSliding = useSliding(onPttRelease);
  const captureRef = useRef<HTMLButtonElement>(null);
  useEffect(() => { if (capturing) captureRef.current?.focus(); }, [capturing]);

  const [refused, setRefused] = useState(false);
  // WB a11y m7: a second refused key empties the region for a moment, so its text is said again.
  const [blank, setBlank] = useState(false);
  useEffect(() => { if (!capturing) { setRefused(false); setBlank(false); } }, [capturing]);
  useEffect(() => {
    if (!blank) return;
    const timer = setTimeout(() => setBlank(false), 100);
    return () => clearTimeout(timer);
  }, [blank]);

  const onCaptureKeyDown = (e: KeyboardEvent<HTMLButtonElement>) => {
    if (!capturing || e.key === 'Tab') return;
    e.preventDefault();
    e.stopPropagation();
    // Fix round 2: the button reads Cancel while it listens, so Enter and Space press it.
    if (e.key === 'Escape' || e.key === 'Enter' || e.key === ' ') { onCaptureKey('cancel'); return; }
    if (MODIFIERS.has(e.key)) return;
    if (typesOrOperates(e)) {
      if (refused) setBlank(true);
      setRefused(true);
      return;
    }
    onCaptureKey({ code: e.code !== '' ? e.code : e.key, ctrl: e.ctrlKey, alt: e.altKey, meta: e.metaKey, shift: e.shiftKey });
  };

  const heading = (text: string | undefined, hid: string) => text ? <h3 id={hid} className="d-voice-settings__heading">{text}</h3> : null;
  const noiseHint = labels.noiseHints?.[noise];
  // Fix round 1: the pill follows the gate's own decision (its hold debounces it); the live region says a state only once
  // it held for a second.
  const heard = micTest.testing ? (micTest.open ? 'heard' : 'quiet') : 'idle';
  const [announced, setAnnounced] = useState<'heard' | 'quiet' | 'idle'>('idle');
  // Fix round 2: a new Let's check starts with an empty live region.
  useEffect(() => { if (!micTest.testing) setAnnounced('idle'); }, [micTest.testing]);
  useEffect(() => {
    if (heard === announced) return;
    const timer = setTimeout(() => setAnnounced(heard), PILL_ANNOUNCE_MS);
    return () => clearTimeout(timer);
  }, [heard, announced]);
  const pillText = (state: typeof heard) => (state === 'heard' ? labels.heard : state === 'quiet' ? labels.tooQuiet : labels.notTesting);
  // README §5.9: four bordered groups (radius 10, 16px padding, 14px apart), each under a micro heading.
  return (
    <div className="d-voice-settings">
      <section className="d-voice-settings__group" aria-labelledby={labels.devices ? `${id}-devices` : undefined}>
        {heading(labels.devices, `${id}-devices`)}
        <div className="d-voice-settings__grid">
          <div className="d-voice-settings__field">
            <label className="d-voice-settings__label" htmlFor={`${id}-input-select`}>{labels.input}</label>
            <DeviceSelect id={`${id}-input-select`} value={input} devices={inputs} systemDefault={labels.systemDefault} label={labels.input} onChange={onInput} />
            <VolumeRow label={labels.volume} name={labels.inputVolume} value={inputVolume} onChange={onInputVolume} />
          </div>
          <div className="d-voice-settings__field">
            {canSelectOutput ? <label className="d-voice-settings__label" htmlFor={`${id}-output-select`}>{labels.output}</label>
              : <span className="d-voice-settings__label">{labels.output}</span>}
            {canSelectOutput ? (
              <DeviceSelect id={`${id}-output-select`} value={output} devices={outputs} systemDefault={labels.systemDefault} label={labels.output} onChange={onOutput} />
            ) : <p className="d-voice-settings__note">{labels.outputUnsupported}</p>}
            <VolumeRow label={labels.volume} name={labels.outputVolume} value={outputVolume} onChange={onOutputVolume} />
          </div>
        </div>
      </section>
      <section className="d-voice-settings__group" aria-labelledby={labels.test ? `${id}-test` : undefined} data-setting="voice-test">
        <div className="d-voice-settings__test-head">
          {heading(labels.test, `${id}-test`)}
          <span className="d-voice-settings__spacer" />
          <button type="button" className="d-voice-settings__check" data-testing={micTest.testing ? 'true' : undefined}
            disabled={!micTest.available && !micTest.testing} onClick={onMicTest}>
            <Icon name={micTest.testing ? 'square' : 'mic'} size={14} />{micTest.testing ? labels.testStop : labels.testStart}
          </button>
        </div>
        <p className="d-voice-settings__desc">{micTest.available || micTest.testing ? labels.testHint : labels.testUnavailable}</p>
        {echo === false ? <p className="d-voice-settings__desc d-voice-settings__echo-off">{labels.testEchoOff}</p> : null}
        <SensitivityMeter levelDb={micTest.levelDb} thresholdDb={micTest.thresholdDb} auto={micTest.auto} testing={micTest.testing}
          label={labels.threshold} onThresholdChange={(db, commit) => onThreshold(db, commit ?? true)} />
        <div className="d-voice-settings__readout">
          <span className="d-voice-settings__pill" data-state={heard}>
            <span className="d-voice-settings__pill-dot" aria-hidden="true" />
            {pillText(heard)}
          </span>
          <span className="d-sr-only" role="status">{micTest.testing && announced !== 'idle' ? pillText(announced) : null}</span>
          <span className="d-voice-settings__stat">{fmt(labels.levelValue, micTest.testing ? formatDb(micTest.levelDb) : labels.noLevel)}</span>
          <span className="d-voice-settings__stat">{fmt(labels.thresholdValue, formatDb(micTest.thresholdDb))}</span>
        </div>
        <SwitchRow id={`${id}-auto`} label={labels.auto} hint={micTest.auto ? labels.autoHintOn : labels.autoHintOff} checked={micTest.auto} onChange={onAuto} />
      </section>
      <section className="d-voice-settings__group" aria-labelledby={labels.inputMode ? `${id}-mode-heading` : undefined}>
        {heading(labels.inputMode, `${id}-mode-heading`)}
        <div className="d-voice-settings__mode">
          <Segmented id={`${id}-mode-group`} label={labels.mode} value={mode}
            options={[{ value: 'vad', label: labels.vad }, { value: 'ptt', label: labels.ptt }]}
            onChange={v => onMode(v === 'ptt' ? 'ptt' : 'vad')} />
        </div>
        {mode === 'ptt' ? (
          <div className="d-voice-settings__grid d-voice-settings__ptt">
            <div className="d-voice-settings__field">
              <span className="d-voice-settings__label" id={`${id}-ptt-label`}>{labels.pttKey}</span>
              <span className="d-voice-settings__key" role="group" aria-labelledby={`${id}-ptt-label`}>
                <kbd className="d-voice-settings__kbd" data-empty={pttKey === null && !capturing ? 'true' : undefined} data-listening={capturing ? 'true' : undefined}>{capturing ? labels.pttListening : pttKey ?? labels.pttNone}</kbd>
                <button ref={captureRef} type="button" className="d-btn d-voice-settings__capture" data-variant="default" data-size="md"
                  aria-describedby={`${id}-prompt`} onKeyDown={onCaptureKeyDown} onBlur={() => { if (capturing) onCaptureKey('cancel'); }}
                  onClick={() => onCaptureKey(capturing ? 'cancel' : 'start')}>{capturing ? labels.pttCancel : labels.pttSet}</button>
              </span>
              {labels.pttHint ? <span className="d-voice-settings__note">{labels.pttHint}</span> : null}
            </div>
            <div className="d-voice-settings__field">
              <label className="d-voice-settings__label" htmlFor={`${id}-release`}>{labels.release}</label>
              <div className="d-voice-settings__volume">
                <input id={`${id}-release`} type="range" className="d-voice-settings__range" min={0} max={1000} step={20} value={pttReleaseMs}
                  aria-valuetext={`${pttReleaseMs} ms`} aria-describedby={`${id}-release-hint`} style={fill(pttReleaseMs / 10)}
                  {...releaseSliding} />
                <span className="d-voice-settings__volume-value d-voice-settings__release-value" aria-hidden="true">{pttReleaseMs} ms</span>
              </div>
              <span id={`${id}-release-hint`} className="d-voice-settings__note">{labels.releaseHint}</span>
            </div>
          </div>
        ) : null}
        <p id={`${id}-prompt`} className="d-voice-settings__prompt" role="status">{capturing && !blank ? (refused ? labels.pttRefused : labels.pttPrompt) : null}</p>
      </section>
      <section className="d-voice-settings__group" aria-labelledby={labels.processing ? `${id}-processing` : undefined}>
        {heading(labels.processing, `${id}-processing`)}
        <div className="d-voice-settings__row" role="group" aria-labelledby={`${id}-noise-label`} aria-describedby={noiseHint ? `${id}-noise-hint` : undefined}>
          <span className="d-voice-settings__text">
            <span id={`${id}-noise-label`} className="d-voice-settings__row-label">{labels.noise}{labels.noiseTag ? <span className="d-voice-settings__tag">{labels.noiseTag}</span> : null}</span>
            {noiseHint ? <span id={`${id}-noise-hint`} className="d-voice-settings__row-hint">{noiseHint}</span> : null}
          </span>
          <Segmented id={`${id}-noise-group`} label={labels.noise} value={noise}
            options={[{ value: 'automatic', label: labels.automatic }, { value: 'lightweight', label: labels.lightweight }, { value: 'off', label: labels.off }]}
            onChange={v => onNoise(v === 'automatic' ? 'automatic' : v === 'off' ? 'off' : 'lightweight')} />
        </div>
        {echo !== undefined ? <SwitchRow id={`${id}-echo`} label={labels.echo} hint={labels.echoHint} checked={echo} onChange={onEcho} /> : null}
        {agc !== undefined ? <SwitchRow id={`${id}-agc`} label={labels.agc} hint={labels.agcHint} checked={agc} onChange={onAgc} /> : null}
      </section>
    </div>
  );
}
