import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { CoreProvider } from '../../core/context.tsx';
import { callState, FakeClient } from '../../test/fake-client.ts';
import { account } from '../../test/fixtures.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { Voice } from './Voice.tsx';
import { reportStageStatus } from '../../call/processor-status.ts';

afterEach(() => { vi.unstubAllGlobals(); });

/** The worker republishes what setSetting stored, as the real one does. */
function setup(settings: Record<string, string> = {}, media?: { permission: PermissionState | 'throws'; devices: MediaDeviceInfo[];
  getUserMedia?: () => Promise<MediaStream>; supported?: MediaTrackSupportedConstraints }) {
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set('settings', settings);
  fake.handler = command => {
    if (command.m === 'setSetting') {
      const next = { ...fake.get('settings') };
      if (command.value === null) delete next[command.key]; else next[command.key] = command.value;
      fake.set('settings', next);
    }
    return Promise.resolve(null);
  };
  const enumerateDevices = vi.fn(() => Promise.resolve(media?.devices ?? []));
  const getUserMedia = vi.fn(media?.getUserMedia ?? (() => Promise.reject(new Error('no'))));
  const supported = media?.supported ?? { echoCancellation: true, autoGainControl: true };
  Object.defineProperty(navigator, 'mediaDevices', { configurable: true, value: { enumerateDevices, getUserMedia, getSupportedConstraints: () => supported } });
  Object.defineProperty(navigator, 'permissions', { configurable: true,
    value: { query: vi.fn(() => (media?.permission === 'throws' ? Promise.reject(new TypeError('microphone is not a permission name'))
      : Promise.resolve({ state: media?.permission ?? 'prompt' }))) } });
  const user = userEvent.setup();
  const view = render(<CoreProvider client={fake}><Voice /></CoreProvider>);
  return { fake, user, view, enumerateDevices, getUserMedia };
}
const settingCalls = (fake: FakeClient) => fake.callsOf('setSetting').map(c => [c.key, c.value]);

describe('Settings → Voice', () => {
  it('Settings → Voice and → Notifications store their keys through setSetting (the Voice half)', async () => {
    const { fake, user } = setup();
    await user.click(screen.getByRole('radio', { name: 'Push to talk' }));
    expect(settingCalls(fake)).toEqual([['voice.mode', 'ptt']]);
    // The capture prompt; F13 is the key.
    await user.click(await screen.findByRole('button', { name: 'Change' }));
    expect(screen.getByText('Press the key to use. Escape cancels. It works only while dilla is the active tab.')).toBeInTheDocument();
    // Ruling AX: while listening, the key box says "Press a key…" and the button is Cancel.
    expect(screen.getByText('Press a key…')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus();
    fireEvent.keyDown(document.activeElement!, { key: 'F13', code: 'F13' });
    expect(settingCalls(fake)).toEqual([['voice.mode', 'ptt'], ['voice.pttKey', 'F13']]);
    expect(await screen.findByText('F13')).toBeInTheDocument();
    // Escape in the prompt stores nothing.
    await user.click(screen.getByRole('button', { name: 'Change' }));
    await user.keyboard('{Escape}');
    expect(settingCalls(fake)).toHaveLength(2);
    expect(screen.queryByText(/Press the key to use/)).toBeNull();
    await user.click(screen.getByRole('radio', { name: 'Off' }));
    expect(settingCalls(fake).at(-1)).toEqual(['voice.noise', 'off']);
  });

  it('shows a stored key that is not one key on its own as unset', () => {
    setup({ 'voice.mode': 'ptt', 'voice.pttKey': 'KeyV' });
    expect(screen.getByText('No key set')).toBeInTheDocument();
    expect(screen.queryByText('KeyV')).toBeNull();
  });

  it('lists devices only after the microphone permission was granted', async () => {
    const mic = { deviceId: 'mic-1', kind: 'audioinput', label: 'Desk mic', groupId: 'g' } as MediaDeviceInfo;
    const first = setup({}, { permission: 'prompt', devices: [mic] });
    await waitFor(() => expect(screen.getByRole('combobox', { name: 'Input device' })).toBeInTheDocument());
    expect(first.enumerateDevices).not.toHaveBeenCalled();
    expect(screen.queryByRole('option', { name: 'Desk mic' })).toBeNull();
    first.view.unmount();
    const second = setup({}, { permission: 'granted', devices: [mic] });
    expect(await screen.findByRole('option', { name: 'Desk mic' })).toBeInTheDocument();
    expect(second.enumerateDevices).toHaveBeenCalled();
    await second.user.selectOptions(screen.getByRole('combobox', { name: 'Input device' }), 'mic-1');
    expect(settingCalls(second.fake).at(-1)).toEqual(['voice.input', 'mic-1']);
  });

  it('has no axe violations', async () => {
    const { view } = setup({ 'voice.mode': 'ptt', 'voice.pttKey': 'F13' });
    await expectNoAxeViolations(view.container);
  });

  it('m2: where the browser cannot query the microphone permission, labelled devices stand for the grant', async () => {
    const mic = { deviceId: 'mic-1', kind: 'audioinput', label: 'Desk mic', groupId: 'g' } as MediaDeviceInfo;
    setup({}, { permission: 'throws', devices: [mic] });
    expect(await screen.findByRole('option', { name: 'Desk mic' })).toBeInTheDocument();
  });

  // Task 10b: the meter is the Mic test's; it reads the microphone only while Let's check runs, never on its own.
  it('never opens the microphone for the meter: at rest the pill says Not testing and the level is a dash', async () => {
    const mic = { deviceId: 'mic-1', kind: 'audioinput', label: 'Desk mic', groupId: 'g' } as MediaDeviceInfo;
    const { getUserMedia } = setup({}, { permission: 'granted', devices: [mic] });
    expect(await screen.findByRole('option', { name: 'Desk mic' })).toBeInTheDocument();
    expect(screen.getByText('Not testing')).toBeInTheDocument();
    expect(screen.getByText('level —')).toBeInTheDocument();
    expect(screen.getByText('threshold -48 dB')).toBeInTheDocument();
    await new Promise(resolve => setTimeout(resolve, 20));
    expect(getUserMedia).not.toHaveBeenCalled();
  });

  it('in a call Let’s check is disabled and says why', () => {
    const { fake } = setup();
    act(() => fake.publish('call', callState({ phase: 'in' })));
    expect(screen.getByRole('button', { name: 'Let’s check' })).toBeDisabled();
    expect(screen.getByText('Microphone testing is unavailable during a call.')).toBeVisible();
  });
});

describe('Settings → Voice: task 10b levels and switches', () => {
  it('writes the volumes, the sensitivity, the release delay and the processing switches, each validated by client-core', async () => {
    const { fake, user } = setup({ 'voice.mode': 'ptt' });
    // Fix round 2: a slider writes on the native change (a pointer let go, a key step, or an assistive technology setting the
    // value with only input + change), not on every move.
    const slide = (name: string, value: string) => {
      const range = screen.getByRole('slider', { name });
      fireEvent.input(range, { target: { value } });
      expect(settingCalls(fake).some(([k, v]) => v === value && k !== 'voice.mode')).toBe(false);
      fireEvent.change(range);
    };
    slide('Input volume', '150');
    slide('Output volume', '40');
    slide('Release delay', '400');
    await user.click(screen.getByRole('switch', { name: 'Set sensitivity automatically' }));
    expect(fake.get('settings')?.['voice.sensitivityAuto']).toBe('off');
    expect(screen.getByRole('slider', { name: 'Sensitivity threshold' })).not.toBeDisabled();
    expect(screen.getByText('threshold -50 dB')).toBeInTheDocument();
    slide('Sensitivity threshold', '-30');
    await user.click(screen.getByRole('switch', { name: 'Echo cancellation' }));
    await user.click(screen.getByRole('switch', { name: 'Automatic gain control' }));
    expect(settingCalls(fake)).toEqual(expect.arrayContaining([['voice.inputVolume', '150'], ['voice.outputVolume', '40'], ['voice.pttRelease', '400'],
      ['voice.sensitivityAuto', 'off'], ['voice.sensitivity', '-30'], ['voice.echo', 'off'], ['voice.agc', 'off']]));
    expect(screen.getByRole('slider', { name: 'Input volume' })).toHaveAttribute('aria-valuetext', '150%');
    expect(screen.getByText('threshold -30 dB')).toBeInTheDocument();
  });

  it('shows the stored values', () => {
    setup({ 'voice.inputVolume': '57', 'voice.outputVolume': '200', 'voice.sensitivityAuto': 'off', 'voice.sensitivity': '-35', 'voice.mode': 'ptt',
      'voice.pttRelease': '0', 'voice.echo': 'off', 'voice.agc': 'on' });
    expect(screen.getByRole('slider', { name: 'Input volume' })).toHaveValue('57');
    expect(screen.getByRole('slider', { name: 'Output volume' })).toHaveValue('200');
    expect(screen.getByRole('slider', { name: 'Sensitivity threshold' })).toHaveValue('-35');
    expect(screen.getByRole('slider', { name: 'Release delay' })).toHaveValue('0');
    expect(screen.getByRole('switch', { name: 'Echo cancellation' })).toHaveAttribute('aria-checked', 'false');
    expect(screen.getByRole('switch', { name: 'Automatic gain control' })).toHaveAttribute('aria-checked', 'true');
  });

  it('M1: in a call whose input stage is not applied, says whether the microphone passes or is held', () => {
    const owner = Symbol('call');
    const { fake } = setup({ 'voice.sensitivityAuto': 'off' });
    act(() => fake.publish('call', callState({ phase: 'in' })));
    act(() => reportStageStatus(owner, 'failed'));
    expect(screen.getByText(/with a manual threshold nothing is sent/)).toBeInTheDocument();
    act(() => reportStageStatus(owner, 'unsupported'));
    expect(screen.getByText('This browser cannot apply the input volume or sensitivity: your microphone is sent as it is.')).toBeInTheDocument();
    act(() => reportStageStatus(owner, 'active'));
    expect(screen.queryByText(/your microphone is sent as it is/)).toBeNull();
    act(() => reportStageStatus(owner, null));
  });

  it('Ruling AR: a processing switch the browser cannot apply is not shown', () => {
    setup({}, { permission: 'prompt', devices: [], supported: { echoCancellation: true } });
    expect(screen.getByRole('switch', { name: 'Echo cancellation' })).toBeInTheDocument();
    expect(screen.queryByRole('switch', { name: 'Automatic gain control' })).toBeNull();
  });
});

describe('Settings → Voice: the output device (web3.3 fix round 1, I1)', () => {
  // Remote call audio plays through an AudioContext (webAudioMix), so the output is chosen on the context, not an element.
  it('offers the output picker where AudioContext can choose an output', () => {
    vi.stubGlobal('AudioContext', class { setSinkId() { return Promise.resolve(); } });
    setup();
    expect(screen.getByRole('combobox', { name: 'Output device' })).toBeInTheDocument();
    expect(screen.queryByText('This browser plays voice on the system default device.')).toBeNull();
  });

  it('says the system default plays where only a media element could choose (Firefox)', () => {
    vi.stubGlobal('AudioContext', class {});
    const proto = HTMLMediaElement.prototype as unknown as { setSinkId?: unknown };
    const had = Object.prototype.hasOwnProperty.call(proto, 'setSinkId');
    proto.setSinkId = () => Promise.resolve();
    try {
      setup();
      expect(screen.queryByRole('combobox', { name: 'Output device' })).toBeNull();
      expect(screen.getByText('This browser plays voice on the system default device.')).toBeInTheDocument();
    } finally {
      if (!had) delete proto.setSinkId;
    }
  });
});

it('keeps legacy On on Lightweight and offers Automatic explicitly', async () => {
  const {fake,user}=setup({'voice.noise':'on'});
  expect(screen.getByRole('radio',{name:'Lightweight'})).toHaveAttribute('aria-checked','true');
  expect(settingCalls(fake)).toHaveLength(0);
  await user.click(screen.getByRole('radio',{name:'Automatic'}));
  expect(settingCalls(fake).at(-1)).toEqual(['voice.noise','automatic']);
});

// Task 10 (README §5.9): the microphone test sits in the Voice page's Mic test group with the input level, under its heading.
describe('Settings → Voice grammar', () => {
  it('puts Let’s check and the sensitivity meter in the Mic test group', () => {
    const { view } = setup();
    const group = screen.getByRole('region', { name: 'Mic test' });
    expect(group).toContainElement(screen.getByRole('button', { name: 'Let’s check' }));
    expect(group).toContainElement(screen.getByRole('slider', { name: 'Sensitivity threshold' }));
    expect(group).toHaveAttribute('data-setting', 'voice-test');
    expect(view.container.querySelectorAll('.d-voice-settings__group')).toHaveLength(4);
  });
});
