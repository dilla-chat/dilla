import { useEffect, useState, useSyncExternalStore } from 'react';
import { SETTINGS } from '@dilla/client-core';
import { Banner, isPushToTalkKey, VoiceSettings, type CaptureKeyAction } from '@dilla/ui';
import { useCore } from '../../core/context.tsx';
import { errorOf, type UiError } from '../../core/errors.ts';
import { useSlice } from '../../core/use-slice.ts';
import { getProcessorStatus, getStageStatus, subscribeProcessorStatus, subscribeStageStatus } from '../../call/processor-status.ts';
import { processingSupport, voicePrefs } from '../../call/voice-prefs.ts';
import { noiseBuildMode, noisePreference } from '../../../../media/src/audio/noise-preference';
import { useMicTest } from './MicrophoneTest.tsx';
import { t } from '../../strings/index.ts';
import type { ProcessorStatus } from '@dilla/media';

const TITLE_ID = 'voice-settings-title';
// L-TS-54: a device id the browser gave, as the settings key accepts it.
const DEVICE_ID = /^[A-Za-z0-9+/=_-]{1,128}$/;

interface Device { id: string; label: string }

/**
 * The devices to choose from, read only once the microphone permission was granted: before that the browser lists no
 * labels, and asking for the list must not open a permission prompt from Settings. '' (the system default) is always there.
 * A browser that cannot query the `microphone` permission (Firefox throws) is read by its list: labelled devices are
 * listed only after a grant (review m2).
 */
function useDevices(): { inputs: Device[]; outputs: Device[]; granted: boolean } {
  const [lists, setLists] = useState<{ inputs: Device[]; outputs: Device[]; granted: boolean }>({ inputs: [], outputs: [], granted: false });
  useEffect(() => {
    let live = true;
    const read = async () => {
      const permissions = (navigator as { permissions?: Permissions }).permissions;
      const media = (navigator as { mediaDevices?: MediaDevices }).mediaDevices;
      if (media === undefined) return;
      let state: PermissionState | 'unknown' = 'unknown';
      try {
        if (permissions !== undefined) state = (await permissions.query({ name: 'microphone' })).state;
      } catch {
        state = 'unknown';
      }
      if (state !== 'granted' && state !== 'unknown') return;
      const all = await media.enumerateDevices();
      if (state === 'unknown' && !all.some(d => d.kind === 'audioinput' && d.label !== '')) return;
      const pick = (kind: MediaDeviceKind) => all.filter(d => d.kind === kind && d.deviceId !== 'default' && DEVICE_ID.test(d.deviceId))
        .map(d => ({ id: d.deviceId, label: d.label !== '' ? d.label : d.deviceId.slice(0, 8) }));
      if (live) setLists({ inputs: pick('audioinput'), outputs: pick('audiooutput'), granted: true });
    };
    read().catch(() => undefined);
    return () => { live = false; };
  }, []);
  return lists;
}

/**
 * A slider's value while it is dragged: shown at once (and in Let's check), written only when it is let go (pointer up or
 * key up; fix round 1), and handed back to the stored value once the worker republished it.
 */
function useDraft(stored: number, write: (v: number) => void): [number, (v: number, commit: boolean) => void] {
  const [draft, setDraft] = useState<number | null>(null);
  // Merge of Task 10b: hand back only once the stored value caught up with the draft. Clearing on any republish lost keyboard
  // steps: the write of 199 landing while the draft was already 198 showed 199 again, and the next key moved from there.
  useEffect(() => { setDraft(d => (d === stored ? null : d)); }, [stored]);
  return [draft ?? stored, (v: number, commit: boolean) => { setDraft(v); if (commit && v !== stored) write(v); }];
}

const processorText = (status: ProcessorStatus, noise: string): string => `${t('settings.voice.processor', {processor: (status.effective === 'off' || (status.effective === 'browser' && status.browserNoise === 'off')) ? t('settings.voice.processor.off') : status.effective === 'browser' ? t('settings.voice.processor.browser') : status.effective === 'failed' ? t('settings.voice.processor.failed') : status.effective === 'initializing' ? t('settings.voice.processor.initializing') : status.engineName ?? t('settings.voice.lightweight')})} ${status.reason === null ? '' : t(`settings.voice.fallback.${status.reason}`)} ${status.browserNoise === 'unknown' ? t('settings.voice.browserUnknown') : noise === 'off' && status.browserNoise === 'on' ? t('settings.voice.browserStillOn') : ''}`.replace(/\s+/g, ' ').trim();

/**
 * Settings → Voice (flow 06, README §5.9): devices and their volumes, the Mic test (Let's check, the sensitivity meter and
 * automatic sensitivity), the input mode with push to talk's key and release delay, and processing (noise suppression,
 * echo cancellation, automatic gain), each written with setSetting. The section keeps no copy of a setting and renders what
 * the worker republishes (a slider shows its draft while it moves). A stored push-to-talk key that is not one non-text key
 * on its own shows as unset (task-8b ruling I2). A processing switch the browser cannot apply is not shown (Ruling AR).
 */
export function Voice(): React.JSX.Element {
  const client = useCore();
  const settings = useSlice('settings') ?? {};
  const { inputs, outputs } = useDevices();
  const call = useSlice('call');
  const inCall = call !== undefined && ['joining', 'in', 'paused', 'leaving'].includes(call.phase);
  const callStatus = useSyncExternalStore(subscribeProcessorStatus, getProcessorStatus);
  const stageStatus = useSyncExternalStore(subscribeStageStatus, getStageStatus);
  const noise = noisePreference(settings[SETTINGS.voiceNoise]);
  const prefs = voicePrefs(settings);
  const [support] = useState(processingSupport);
  const [capturing, setCapturing] = useState(false);
  const [error, setError] = useState<UiError | null>(null);
  const write = (key: string, value: string | null) => {
    setError(null);
    client.call({ m: 'setSetting', key, value }).catch((e: unknown) => setError(errorOf(e)));
  };
  const [inputVolume, setInputVolume] = useDraft(prefs.inputVolume, v => write(SETTINGS.voiceInputVolume, String(v)));
  const [outputVolume, setOutputVolume] = useDraft(prefs.outputVolume, v => write(SETTINGS.voiceOutputVolume, String(v)));
  const [threshold, setThreshold] = useDraft(prefs.sensitivity.thresholdDb, v => write(SETTINGS.voiceSensitivity, String(v)));
  const [release, setRelease] = useDraft(prefs.pttReleaseMs, v => write(SETTINGS.voicePttRelease, String(v)));
  const input = settings[SETTINGS.voiceInput] ?? '';
  const output = settings[SETTINGS.voiceOutput] ?? '';
  const mode = settings[SETTINGS.voiceMode] === 'ptt' ? 'ptt' : 'vad';
  const mic = useMicTest({ input, output, noise, processing: prefs.processing, inputVolume, outputVolume,
    sensitivity: { auto: prefs.sensitivity.auto, thresholdDb: threshold }, mode }, inCall);
  // Fix round 1 (M1): the call says when its input stage is not applied, and whether the microphone then passes or is held.
  const stageNote = !inCall || stageStatus === null || stageStatus === 'active' ? null
    : stageStatus === 'unsupported' ? t('signalSettings.voice.stageUnsupported')
      : mode === 'vad' && !prefs.sensitivity.auto ? t('signalSettings.voice.stageFailedClosed') : t('signalSettings.voice.stageFailedOpen');
  const stored = settings[SETTINGS.voicePttKey];
  const pttKey = stored !== undefined && isPushToTalkKey(stored) ? stored : null;
  const onCaptureKey = (action: CaptureKeyAction) => {
    if (action === 'start') { setCapturing(true); return; }
    setCapturing(false);
    if (action !== 'cancel' && isPushToTalkKey(action.code)) write(SETTINGS.voicePttKey, action.code);
  };
  // web3.3 fix round 1 (I1): call audio plays through the call’s AudioContext (webAudioMix), so the output is chosen
  // there; a browser whose AudioContext cannot (Firefox) plays on the system default whatever an element could do.
  const canSelectOutput = typeof AudioContext !== 'undefined' && 'setSinkId' in AudioContext.prototype;
  const status = mic.status ?? callStatus;
  return (
    <section className="dw-settings-section" aria-labelledby={TITLE_ID} data-doc-state="voice-settings">
      <h2 id={TITLE_ID} tabIndex={-1}>{t('settings.voice.section')}</h2>
      <p>{t('signalSettings.voice.body')}</p>
      {error === null ? null : <Banner tone="danger">{t('settings.error.other', { code: error.code })}</Banner>}
      <VoiceSettings inputs={inputs} outputs={outputs} input={input} output={output}
        canSelectOutput={canSelectOutput} inputVolume={inputVolume} outputVolume={outputVolume}
        mode={mode} pttKey={pttKey} capturing={capturing} pttReleaseMs={release} noise={noise}
        micTest={{ testing: mic.testing, available: !inCall, levelDb: mic.levelDb, open: mic.open, thresholdDb: prefs.sensitivity.auto ? mic.autoDb : threshold, auto: prefs.sensitivity.auto }}
        echo={support.echo ? prefs.processing.echo : undefined} agc={support.agc ? prefs.processing.agc : undefined}
        labels={{ input: t('settings.voice.input'), output: t('settings.voice.output'), systemDefault: t('settings.voice.systemDefault'),
          outputUnsupported: t('settings.voice.outputUnsupported'), mode: t('settings.voice.mode'), vad: t('settings.voice.vad'),
          ptt: t('settings.voice.ptt'), pttKey: t('settings.voice.pttKey'), pttNone: t('settings.voice.pttNone'), pttSet: t('settings.voice.pttSet'),
          pttListening: t('signalSettings.voice.pttListening'), pttCancel: t('signalSettings.voice.pttCancel'),
          pttPrompt: t('settings.voice.pttPrompt'), pttRefused: t('settings.voice.pttRefused'), noise: t('settings.voice.noise'), automatic: t('settings.voice.automatic'), lightweight: t('settings.voice.lightweight'), off: t('settings.voice.off'),
          devices: t('signalSettings.voice.devices'), test: t('signalSettings.voice.test'), inputMode: t('signalSettings.voice.inputMode'),
          processing: t('signalSettings.voice.processing'), pttHint: t('signalSettings.voice.pttHint'),
          noiseHints: { automatic: t('signalSettings.voice.noiseHint.automatic'), lightweight: t('signalSettings.voice.noiseHint.lightweight'), off: t('signalSettings.voice.noiseHint.off') },
          // README §5.9: while Automatic is a beta, the row's label carries the prototype's BETA tag when it is picked.
          noiseTag: noiseBuildMode === 'beta' && noise === 'automatic' ? t('signalSettings.voice.noiseTag') : undefined,
          volume: t('signalSettings.voice.volume'), inputVolume: t('signalSettings.voice.inputVolume'), outputVolume: t('signalSettings.voice.outputVolume'),
          testStart: t('signalSettings.voice.testStart'), testStop: t('signalSettings.voice.testStop'),
          testHint: mic.failed ? t('settings.voice.testFailed') : t('signalSettings.voice.testHint'), testUnavailable: t('settings.voice.testCall'), testEchoOff: t('signalSettings.voice.testEchoOff'),
          heard: t('signalSettings.voice.heard'), tooQuiet: t('signalSettings.voice.tooQuiet'), notTesting: t('signalSettings.voice.notTesting'),
          levelValue: t('signalSettings.voice.levelValue', { value: '{value}' }), thresholdValue: t('signalSettings.voice.thresholdValue', { value: '{value}' }), noLevel: t('signalSettings.voice.noLevel'),
          threshold: t('signalSettings.voice.threshold'), auto: t('signalSettings.voice.auto'),
          autoHintOn: t('signalSettings.voice.autoHintOn'), autoHintOff: t('signalSettings.voice.autoHintOff'),
          release: t('signalSettings.voice.release'), releaseHint: t('signalSettings.voice.releaseHint'),
          echo: t('signalSettings.voice.echo'), echoHint: t('signalSettings.voice.echoHint'), agc: t('signalSettings.voice.agc'), agcHint: t('signalSettings.voice.agcHint') }}
        onInput={id => write(SETTINGS.voiceInput, id === '' ? null : id)}
        onOutput={id => write(SETTINGS.voiceOutput, id === '' ? null : id)}
        onInputVolume={setInputVolume}
        onOutputVolume={setOutputVolume}
        onMode={mode => write(SETTINGS.voiceMode, mode)}
        onCaptureKey={onCaptureKey}
        onPttRelease={setRelease}
        onNoise={mode => write(SETTINGS.voiceNoise, mode)}
        onMicTest={() => mic.toggle()}
        onThreshold={setThreshold}
        onAuto={on => write(SETTINGS.voiceSensitivityAuto, on ? 'on' : 'off')}
        onEcho={on => write(SETTINGS.voiceEcho, on ? 'on' : 'off')}
        onAgc={on => write(SETTINGS.voiceAgc, on ? 'on' : 'off')} />
      {stageNote === null ? null : <p role="status">{stageNote}</p>}
      {noiseBuildMode === 'beta' ? <p>{t('settings.voice.noiseBeta')}</p> : null}
      {status === null ? null : <p role="status">{processorText(status, noise)}</p>}
    </section>
  );
}
