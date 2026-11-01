import type { Meta, StoryObj } from '@storybook/react-vite';
import { VoiceSettings } from './VoiceSettings.tsx';
import { VOICE_HANDLERS, VOICE_LABELS } from './VoiceSettings.fixture.ts';

const meta = {
  title: 'Voice/VoiceSettings', component: VoiceSettings,
  args: {
    inputs: [{ id: 'mic-1', label: 'USB microphone' }, { id: 'mic-2', label: 'Headset microphone' }],
    outputs: [{ id: 'out-1', label: 'Speakers' }, { id: 'out-2', label: 'Headset' }],
    input: '', output: '', canSelectOutput: true, inputVolume: 100, outputVolume: 100, mode: 'vad', pttKey: null, capturing: false, pttReleaseMs: 200,
    noise: 'automatic', echo: true, agc: true,
    micTest: { testing: false, available: true, levelDb: -100, thresholdDb: -48, auto: true, open: false },
    labels: VOICE_LABELS,
    ...VOICE_HANDLERS,
  },
  decorators: [Story => <div style={{ maxWidth: 720 }}><Story /></div>],
} satisfies Meta<typeof VoiceSettings>;
export default meta;
type Story = StoryObj<typeof meta>;

/** Voice activity, the default. */
export const VoiceActivity: Story = {};
/** Push to talk with no key yet: there is no default key. */
export const PushToTalkNoKey: Story = { args: { mode: 'ptt' } };
/** README §5.9: push to talk with its shortcut and release delay, as the prototype opens it. */
export const PushToTalk: Story = { args: { mode: 'ptt', pttKey: 'F13', input: 'mic-2' } };
/** Listening for the next key: the key box takes the accent and its ring. */
export const PushToTalkListening: Story = { args: { mode: 'ptt', pttKey: 'F13', capturing: true } };
export const CapturingKey: Story = { args: { mode: 'ptt', capturing: true } };
/** A plain letter is refused while capturing: the prompt says why and the capture goes on (M9). */
export const CapturingRefused: Story = {
  args: { mode: 'ptt', capturing: true },
  play: ({ canvasElement }) => {
    const button = canvasElement.querySelector('.d-voice-settings__capture');
    if (!button) throw new Error('CapturingRefused: no capture button');
    button.dispatchEvent(new KeyboardEvent('keydown', { key: 'a', code: 'KeyA', bubbles: true, cancelable: true }));
  },
};
/** Firefox: the output follows the system default. */
export const OutputUnsupported: Story = { args: { canSelectOutput: false, noise: 'off' } };
/** Automatic while it is a beta: the row's BETA tag (the app's beta build, Automatic picked). */
export const NoiseBeta: Story = { args: { labels: { ...VOICE_LABELS, noiseTag: 'BETA' } } };
/** Let's check running: you are heard (the pill green, Stop testing in the danger outline). */
export const TestingHeard: Story = { args: { micTest: { testing: true, available: true, levelDb: -24, thresholdDb: -48, auto: true, open: true } } };
/** Let's check running under the line: "Too quiet to send". */
export const TestingTooQuiet: Story = { args: { micTest: { testing: true, available: true, levelDb: -64, thresholdDb: -48, auto: true, open: false } } };
/** Automatic sensitivity off: the line can be dragged. */
export const ManualSensitivity: Story = { args: { micTest: { testing: false, available: true, levelDb: -100, thresholdDb: -50, auto: false, open: false } } };
/** In a call the microphone is the call's: Let's check waits. */
export const TestUnavailable: Story = { args: { micTest: { testing: false, available: false, levelDb: -100, thresholdDb: -48, auto: true, open: false } } };
/** Ruling AR: Safari has no automatic gain constraint, so that row is not shown. */
export const NoAutomaticGain: Story = { args: { agc: undefined } };
/** Echo cancellation off: the Mic test says to use headphones (fix round 1, L3). */
export const EchoOff: Story = { args: { echo: false } };
