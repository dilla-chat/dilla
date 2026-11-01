import type { VoiceSettingsProps } from './VoiceSettings.tsx';

/** The shipped copy (packages/web/src/strings/en.ts, the prototype's Voice page), as Settings → Voice passes it. */
export const VOICE_LABELS: VoiceSettingsProps['labels'] = {
  input: 'Input device', output: 'Output device', systemDefault: 'System default',
  outputUnsupported: 'This browser plays voice on the system default device.', mode: 'Input mode', vad: 'Voice activity', ptt: 'Push to talk',
  pttKey: 'Shortcut', pttNone: 'No key set', pttSet: 'Change', pttListening: 'Press a key…', pttCancel: 'Cancel',
  pttPrompt: 'Press the key to use. Escape cancels. It works only while dilla is the active tab.',
  pttRefused: 'That key types text, moves the focus or needs another key. Use one key on its own, such as Pause, Scroll Lock or F13 to F24.',
  noise: 'Noise suppression', automatic: 'Automatic', lightweight: 'Lightweight', off: 'Off',
  devices: 'Devices', test: 'Mic test', inputMode: 'Input mode', processing: 'Processing',
  pttHint: 'Works only while the dilla tab is active. F13–F24, Pause, Scroll Lock or Insert.',
  noiseHints: { automatic: 'Picks the best processor this device can run. Falls back to RNNoise.', lightweight: 'Lower CPU, removes less', off: 'Raw microphone, nothing removed' },
  volume: 'Volume', inputVolume: 'Input volume', outputVolume: 'Output volume',
  testStart: 'Let’s check', testStop: 'Stop testing',
  testHint: 'Talk normally. The bar is your level; the white line is where dilla starts sending your voice. Your voice plays back so you hear what others hear, so wear headphones.',
  testUnavailable: 'Microphone testing is unavailable during a call.',
  testEchoOff: 'Echo cancellation is off: use headphones, or your speakers will feed back into the mic.',
  heard: 'You’re being heard', tooQuiet: 'Too quiet to send', notTesting: 'Not testing', levelValue: 'level {value}', thresholdValue: 'threshold {value}', noLevel: '—',
  threshold: 'Sensitivity threshold', auto: 'Set sensitivity automatically',
  autoHintOn: 'dilla picks the threshold from your room noise. Turn off to drag the line yourself.',
  autoHintOff: 'Drag the white line on the meter to set the threshold.',
  release: 'Release delay', releaseHint: 'Keeps sending this long after you let go, so word endings aren’t cut.',
  echo: 'Echo cancellation', echoHint: 'Stops your speakers from feeding back into the mic',
  agc: 'Automatic gain control', agcHint: 'Evens out loud and quiet speech',
};
const noop = () => {};
export const VOICE_HANDLERS = {
  onInput: noop, onOutput: noop, onInputVolume: noop, onOutputVolume: noop, onMode: noop, onCaptureKey: noop, onPttRelease: noop, onNoise: noop,
  onMicTest: noop, onThreshold: noop, onAuto: noop, onEcho: noop, onAgc: noop,
};

