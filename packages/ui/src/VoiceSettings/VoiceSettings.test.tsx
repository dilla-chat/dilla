import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, it, expect, vi } from 'vitest';
import { axe } from 'vitest-axe';
import { typesOrOperates, VoiceSettings, type VoiceSettingsProps } from './VoiceSettings.tsx';
import meta from './VoiceSettings.stories.tsx';
import { VOICE_HANDLERS, VOICE_LABELS } from './VoiceSettings.fixture.ts';

const labels: VoiceSettingsProps['labels'] = {
  input: 'input device', output: 'output device', systemDefault: 'system default',
  outputUnsupported: 'This browser plays voice on the system default device.', mode: 'input mode', vad: 'voice activity', ptt: 'push to talk',
  pttKey: 'push-to-talk key', pttNone: 'no key set', pttSet: 'set key', pttListening: 'press a key…', pttCancel: 'cancel',
  pttPrompt: 'Press the key to use. Escape cancels. It works only while dilla is the active tab.',
  pttRefused: 'That key types text, moves the focus or needs another key. Use one key on its own, such as Pause, Scroll Lock or F13 to F24.',
  noise: 'noise suppression', automatic: 'automatic', lightweight: 'lightweight', off: 'off',
  volume: VOICE_LABELS.volume, inputVolume: 'input volume', outputVolume: 'output volume', testStart: 'Let’s check', testStop: 'Stop testing',
  testHint: 'hint', testUnavailable: 'unavailable in a call', testEchoOff: 'use headphones', heard: 'You’re being heard', tooQuiet: 'Too quiet to send', notTesting: 'Not testing',
  levelValue: 'level {value}', thresholdValue: 'threshold {value}', noLevel: '—', threshold: 'sensitivity threshold',
  auto: 'set sensitivity automatically', autoHintOn: 'auto on', autoHintOff: 'drag the line', release: 'release delay', releaseHint: 'keeps sending',
  echo: 'echo cancellation', echoHint: 'no feedback', agc: 'automatic gain control', agcHint: 'evens out',
};
const prompt = () => document.querySelector('.d-voice-settings__prompt') as HTMLElement;
const base: VoiceSettingsProps = {
  inputs: [{ id: 'mic-1', label: 'USB microphone' }, { id: 'mic-2', label: 'Headset' }], outputs: [{ id: 'out-1', label: 'Speakers' }],
  input: '', output: '', canSelectOutput: true, inputVolume: 100, outputVolume: 100, mode: 'vad', pttKey: null, capturing: false, pttReleaseMs: 200,
  noise: 'lightweight', echo: true, agc: true, micTest: { testing: false, available: true, levelDb: -100, thresholdDb: -48, auto: true, open: false }, labels,
  ...VOICE_HANDLERS,
};

afterEach(() => { delete document.documentElement.dataset.theme; });

describe('VoiceSettings', () => {
  it('chooses devices with native selects that start at the system default', () => {
    const onInput = vi.fn();
    const onOutput = vi.fn();
    render(<VoiceSettings {...base} onInput={onInput} onOutput={onOutput} />);
    const input = screen.getByRole('combobox', { name: 'input device' });
    expect(input).toHaveValue('');
    expect(screen.getAllByRole('option', { name: 'system default' })).toHaveLength(2);
    fireEvent.change(input, { target: { value: 'mic-2' } });
    expect(onInput).toHaveBeenCalledWith('mic-2');
    fireEvent.change(screen.getByRole('combobox', { name: 'output device' }), { target: { value: 'out-1' } });
    expect(onOutput).toHaveBeenCalledWith('out-1');
  });

  it('says so when the browser cannot choose the output', () => {
    render(<VoiceSettings {...base} canSelectOutput={false} />);
    expect(screen.queryByRole('combobox', { name: 'output device' })).toBeNull();
    expect(screen.getByText('This browser plays voice on the system default device.')).toBeInTheDocument();
  });

  it('voice activity is the default; push to talk shows its key row; noise suppression has three choices', async () => {
    const onMode = vi.fn();
    const onNoise = vi.fn();
    const { rerender } = render(<VoiceSettings {...base} onMode={onMode} onNoise={onNoise} />);
    expect(screen.getByRole('radio', { name: 'voice activity' })).toHaveAttribute('aria-checked', 'true');
    expect(screen.queryByRole('button', { name: /set key/ })).toBeNull();
    await userEvent.click(screen.getByRole('radio', { name: 'push to talk' }));
    expect(onMode).toHaveBeenCalledWith('ptt');
    await userEvent.click(screen.getByRole('radio', { name: 'off' }));
    expect(onNoise).toHaveBeenCalledWith('off');
    rerender(<VoiceSettings {...base} mode="ptt" />);
    expect(screen.getByText('no key set')).toBeInTheDocument();
    rerender(<VoiceSettings {...base} mode="ptt" pttKey="F8" />);
    expect(screen.getByText('F8')).toBeInTheDocument();
  });

  it('captures the push-to-talk key: a non-text key is saved without typing, Escape cancels', async () => {
    const onCaptureKey = vi.fn();
    const { rerender } = render(<VoiceSettings {...base} mode="ptt" onCaptureKey={onCaptureKey} />);
    await userEvent.click(screen.getByRole('button', { name: 'set key' }));
    expect(onCaptureKey).toHaveBeenLastCalledWith('start');
    rerender(<VoiceSettings {...base} mode="ptt" capturing onCaptureKey={onCaptureKey} />);
    // Ruling AX: while listening the key box says "Press a key…" and the button is Cancel.
    const capture = screen.getByRole('button', { name: 'cancel' });
    expect(capture).toHaveFocus();
    expect(document.querySelector('.d-voice-settings__kbd')).toHaveTextContent('press a key…');
    expect(prompt()).toHaveTextContent('Press the key to use. Escape cancels.');
    const typed = fireEvent.keyDown(capture, { key: 'F13', code: 'F13' });
    expect(typed).toBe(false);
    expect(onCaptureKey).toHaveBeenLastCalledWith({ code: 'F13', ctrl: false, alt: false, meta: false, shift: false });
    fireEvent.keyDown(capture, { key: 'Escape', code: 'Escape' });
    expect(onCaptureKey).toHaveBeenLastCalledWith('cancel');
    onCaptureKey.mockClear();
    fireEvent.click(capture);
    expect(onCaptureKey).toHaveBeenLastCalledWith('cancel');
    // Fix round 2: Enter and Space press Cancel while it listens.
    for (const key of ['Enter', ' ']) {
      onCaptureKey.mockClear();
      fireEvent.keyDown(capture, { key, code: key === ' ' ? 'Space' : 'Enter' });
      expect(onCaptureKey).toHaveBeenLastCalledWith('cancel');
    }
  });

  it('accepts only a non-text key on its own: F13 to F24, Pause, ScrollLock, Insert (task-8b review I2, supersedes M9)', async () => {
    const onCaptureKey = vi.fn();
    render(<VoiceSettings {...base} mode="ptt" capturing onCaptureKey={onCaptureKey} />);
    const capture = screen.getByRole('button', { name: 'cancel' });
    const refused: Array<[string, string, Record<string, boolean>]> = [
      // Enter and Space press Cancel while it listens (fix round 2), so they are not in this list.
      ['a', 'KeyA', {}], ['A', 'KeyA', { shiftKey: true }], ['7', 'Digit7', {}], [',', 'Comma', {}],
      ['ArrowDown', 'ArrowDown', {}], ['Dead', 'Quote', {}],
      // A text or operating key stays refused with Control, Alt or Meta held (the setting stores the code alone).
      ['m', 'KeyM', { ctrlKey: true, shiftKey: true }], ['v', 'KeyV', { metaKey: true }], ['x', 'KeyX', { altKey: true }],
      // Function keys the browser uses (help, find, reload, caret browsing, fullscreen, devtools) are refused.
      ['F1', 'F1', {}], ['F5', 'F5', {}], ['F8', 'F8', {}], ['F12', 'F12', {}],
      // An allowed key held with a modifier is a combination, which the setting cannot store.
      ['Pause', 'Pause', { ctrlKey: true }], ['F13', 'F13', { shiftKey: true }],
    ];
    for (const [key, code, mods] of refused) {
      const typed = fireEvent.keyDown(capture, { key, code, ...mods });
      expect(typed).toBe(false);
    }
    expect(onCaptureKey).not.toHaveBeenCalled();
    await waitFor(() => expect(prompt()).toHaveTextContent('That key types text, moves the focus or needs another key.'));
    fireEvent.keyDown(capture, { key: 'Control', code: 'ControlLeft', ctrlKey: true });
    expect(onCaptureKey).not.toHaveBeenCalled();
    for (const code of ['F13', 'F24', 'Pause', 'ScrollLock', 'Insert']) {
      fireEvent.keyDown(capture, { key: code, code });
      expect(onCaptureKey).toHaveBeenLastCalledWith({ code, ctrl: false, alt: false, meta: false, shift: false });
    }
    expect(onCaptureKey).toHaveBeenCalledTimes(5);
  });

  // WB a11y m7: a second refused key says the refusal again (the region is emptied, then set), so the prompt never looks
  // frozen to a screen reader user.
  it('says the refusal again for a second refused key', async () => {
    render(<VoiceSettings {...base} mode="ptt" capturing onCaptureKey={vi.fn()} />);
    const capture = screen.getByRole('button', { name: 'cancel' });
    const status = prompt();
    fireEvent.keyDown(capture, { key: 'a', code: 'KeyA' });
    expect(status).toHaveTextContent('That key types text');
    fireEvent.keyDown(capture, { key: 'b', code: 'KeyB' });
    expect(status).toHaveTextContent('');
    await waitFor(() => expect(status).toHaveTextContent('That key types text'));
  });

  it('refuses a key typed with AltGr, which browsers report as Control plus Alt (task 8b ruling)', () => {
    const onCaptureKey = vi.fn();
    render(<VoiceSettings {...base} mode="ptt" capturing onCaptureKey={onCaptureKey} />);
    const capture = screen.getByRole('button', { name: 'cancel' });
    fireEvent.keyDown(capture, { key: '@', code: 'Digit2', ctrlKey: true, altKey: true, modifierAltGraph: true });
    fireEvent.keyDown(capture, { key: '€', code: 'KeyE', altKey: true, modifierAltGraph: true });
    fireEvent.keyDown(capture, { key: 'Insert', code: 'Insert', modifierAltGraph: true });
    expect(onCaptureKey).not.toHaveBeenCalled();
    expect(typesOrOperates({ key: '@', code: 'Digit2', ctrlKey: true, altKey: true, metaKey: false, getModifierState: (k) => k === 'AltGraph' })).toBe(true);
    expect(typesOrOperates({ key: 'Pause', code: 'Pause', ctrlKey: false, altKey: false, metaKey: false, getModifierState: () => false })).toBe(false);
  });

  // Task 10b
  it('input and output volume are named ranges 0–200 % with their value', () => {
    const onInputVolume = vi.fn();
    const onOutputVolume = vi.fn();
    render(<VoiceSettings {...base} inputVolume={57} onInputVolume={onInputVolume} onOutputVolume={onOutputVolume} />);
    const input = screen.getByRole('slider', { name: 'input volume' });
    expect(input).toHaveAttribute('max', '200');
    expect(input).toHaveAttribute('aria-valuetext', '57%');
    expect(input.style.getPropertyValue('--d-fill')).toBe('28.5%');
    fireEvent.input(input, { target: { value: '140' } });
    expect(onInputVolume).toHaveBeenLastCalledWith(140, false);
    fireEvent.change(input);
    expect(onInputVolume).toHaveBeenLastCalledWith(140, true);
    // An assistive technology sets the value with input + change only (no key or pointer events): it is committed.
    const output = screen.getByRole('slider', { name: 'output volume' });
    fireEvent.input(output, { target: { value: '0' } });
    fireEvent.change(output);
    expect(onOutputVolume).toHaveBeenLastCalledWith(0, true);
    // A drag cancelled by the browser still commits where it was.
    fireEvent.input(output, { target: { value: '30' } });
    fireEvent.pointerCancel(output);
    expect(onOutputVolume).toHaveBeenLastCalledWith(30, true);
  });

  it('the output volume stays where the browser cannot choose the output device', () => {
    render(<VoiceSettings {...base} canSelectOutput={false} />);
    expect(screen.getByRole('slider', { name: 'output volume' })).toBeInTheDocument();
  });

  it('Mic test: Let’s check / Stop testing, the pill as live text, the readouts, and automatic sensitivity', async () => {
    const onMicTest = vi.fn();
    const onAuto = vi.fn();
    const onThreshold = vi.fn();
    const { rerender } = render(<VoiceSettings {...base} onMicTest={onMicTest} onAuto={onAuto} onThreshold={onThreshold} />);
    const pill = () => document.querySelector('.d-voice-settings__pill') as HTMLElement;
    expect(pill()).not.toHaveAttribute('role');
    expect(pill()).toHaveTextContent('Not testing');
    expect(screen.getByText('level —')).toBeInTheDocument();
    expect(screen.getByText('threshold -48 dB')).toBeInTheDocument();
    expect(screen.getByRole('slider', { name: 'sensitivity threshold' })).toBeDisabled();
    await userEvent.click(screen.getByRole('button', { name: 'Let’s check' }));
    expect(onMicTest).toHaveBeenCalledTimes(1);
    rerender(<VoiceSettings {...base} micTest={{ testing: true, available: true, levelDb: -30.4, thresholdDb: -48, auto: true, open: true }} onAuto={onAuto} />);
    expect(screen.getByRole('button', { name: 'Stop testing' })).toHaveAttribute('data-testing', 'true');
    expect(pill()).toHaveTextContent('You’re being heard');
    expect(screen.getByText('level -30 dB')).toBeInTheDocument();
    rerender(<VoiceSettings {...base} micTest={{ testing: true, available: true, levelDb: -60, thresholdDb: -48, auto: true, open: false }} onAuto={onAuto} />);
    expect(pill()).toHaveTextContent('Too quiet to send');
    await userEvent.click(screen.getByRole('switch', { name: 'set sensitivity automatically' }));
    expect(onAuto).toHaveBeenCalledWith(false);
    rerender(<VoiceSettings {...base} micTest={{ testing: false, available: true, levelDb: -100, thresholdDb: -35, auto: false, open: false }} onThreshold={onThreshold} />);
    expect(screen.getByText('drag the line')).toBeInTheDocument();
    const line = screen.getByRole('slider', { name: 'sensitivity threshold' });
    fireEvent.input(line, { target: { value: '-20' } });
    expect(onThreshold).toHaveBeenLastCalledWith(-20, false);
    fireEvent.change(line);
    expect(onThreshold).toHaveBeenLastCalledWith(-20, true);
  });

  // Fix round 1: the pill follows the gate (open), not the raw level; the live region says a state only after it held 1 s.
  it('the pill shows the gate at once; its live text waits until a state held for a second', () => {
    vi.useFakeTimers();
    try {
      const testing = (open: boolean, levelDb: number) => ({ testing: true, available: true, levelDb, thresholdDb: -48, auto: true, open });
      const { rerender } = render(<VoiceSettings {...base} micTest={testing(true, -70)} />);
      const pill = document.querySelector('.d-voice-settings__pill') as HTMLElement;
      const live = document.querySelector('.d-voice-settings__readout .d-sr-only') as HTMLElement;
      // Under the line but still held open: heard.
      expect(pill).toHaveTextContent('You’re being heard');
      expect(live).toHaveAttribute('role', 'status');
      expect(live).toHaveTextContent('');
      act(() => { vi.advanceTimersByTime(1000); });
      expect(live).toHaveTextContent('You’re being heard');
      rerender(<VoiceSettings {...base} micTest={testing(false, -70)} />);
      act(() => { vi.advanceTimersByTime(500); });
      rerender(<VoiceSettings {...base} micTest={testing(true, -20)} />);
      act(() => { vi.advanceTimersByTime(900); });
      expect(live).toHaveTextContent('You’re being heard');
      rerender(<VoiceSettings {...base} micTest={testing(false, -70)} />);
      act(() => { vi.advanceTimersByTime(1000); });
      expect(live).toHaveTextContent('Too quiet to send');
      // Fix round 2: a new Let's check starts with an empty live region.
      rerender(<VoiceSettings {...base} />);
      rerender(<VoiceSettings {...base} micTest={testing(true, -20)} />);
      expect(live).toHaveTextContent('');
    } finally { vi.useRealTimers(); }
  });

  it('says to use headphones while echo cancellation is off', () => {
    const { rerender } = render(<VoiceSettings {...base} echo />);
    expect(screen.queryByText('use headphones')).toBeNull();
    rerender(<VoiceSettings {...base} echo={false} />);
    expect(screen.getByText('use headphones')).toBeInTheDocument();
  });

  it('Let’s check is disabled while a call holds the microphone, and says so', () => {
    render(<VoiceSettings {...base} micTest={{ testing: false, available: false, levelDb: -100, thresholdDb: -48, auto: true, open: false }} />);
    expect(screen.getByRole('button', { name: 'Let’s check' })).toBeDisabled();
    expect(screen.getByText('unavailable in a call')).toBeInTheDocument();
  });

  it('push to talk shows the release delay: 0–1000 ms in steps of 20', () => {
    const onPttRelease = vi.fn();
    // The row mounts when push to talk is picked, after the component (fix round 2: its listeners follow it).
    const { rerender } = render(<VoiceSettings {...base} pttReleaseMs={200} onPttRelease={onPttRelease} />);
    rerender(<VoiceSettings {...base} mode="ptt" pttReleaseMs={200} onPttRelease={onPttRelease} />);
    const range = screen.getByRole('slider', { name: 'release delay' });
    expect(range).toHaveAttribute('step', '20');
    expect(range).toHaveAttribute('max', '1000');
    expect(range).toHaveAttribute('aria-valuetext', '200 ms');
    expect(range).toHaveAccessibleDescription('keeps sending');
    fireEvent.input(range, { target: { value: '340' } });
    expect(onPttRelease).toHaveBeenLastCalledWith(340, false);
    fireEvent.change(range);
    expect(onPttRelease).toHaveBeenLastCalledWith(340, true);
  });

  it('echo cancellation and automatic gain are switches; a switch the browser cannot apply is not shown (Ruling AR)', async () => {
    const onEcho = vi.fn();
    const onAgc = vi.fn();
    const { rerender } = render(<VoiceSettings {...base} onEcho={onEcho} onAgc={onAgc} />);
    await userEvent.click(screen.getByRole('switch', { name: 'echo cancellation' }));
    expect(onEcho).toHaveBeenCalledWith(false);
    await userEvent.click(screen.getByRole('switch', { name: 'automatic gain control' }));
    expect(onAgc).toHaveBeenCalledWith(false);
    expect(screen.getByRole('switch', { name: 'echo cancellation' })).toHaveAccessibleDescription('no feedback');
    rerender(<VoiceSettings {...base} agc={undefined} echo={undefined} />);
    expect(screen.queryByRole('switch', { name: 'automatic gain control' })).toBeNull();
    expect(screen.queryByRole('switch', { name: 'echo cancellation' })).toBeNull();
  });

  it.each(['signal', 'light', 'high-contrast'])('passes axe at its default story args in %s', async theme => {
    document.documentElement.dataset.theme = theme;
    const { container } = render(<div className="d-root"><VoiceSettings {...(meta.args as VoiceSettingsProps)} /></div>);
    // jsdom has no layout or canvas, so contrast is left to the Storybook axe run in a real browser.
    expect((await axe(container, { rules: { 'color-contrast': { enabled: false } } })).violations).toEqual([]);
  });
});
