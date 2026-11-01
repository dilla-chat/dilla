import { describe, expect, it } from 'vitest';
import { SETTINGS, VOICE_DEFAULTS, effectiveNotifyMode, isMuted, isSettingKey, isSettingValue, pageSettings, PENDING_ROLE_PANEL_ACTION_PREFIX, suppressesEveryone } from './settings';

it('hides durable role-panel action retries from the user settings slice', () => {
  const rows = { [SETTINGS.density]: 'regular', [`${PENDING_ROLE_PANEL_ACTION_PREFIX}${'a'.repeat(32)}`]: '{"eventSeq":"3"}' };
  expect(pageSettings(rows)).toEqual({ [SETTINGS.density]: 'regular' });
});

it('accepts only bounded device-local appearance and search choices', () => {
  const choices = [
    [SETTINGS.density, ['regular', 'compact']],
    [SETTINGS.reduceMotion, ['system', 'on', 'off']],
    [SETTINGS.searchScope, ['channel', 'community', 'all']],
  ] as const;
  for (const [key, values] of choices) {
    expect(isSettingKey(key)).toBe(true);
    for (const value of values) expect(isSettingValue(key, value)).toBe(true);
    expect(isSettingValue(key, 'x'.repeat(129))).toBe(false);
  }
  expect(isSettingKey('theme')).toBe(false);
  expect(isSettingKey('density.extra')).toBe(false);
  expect(isSettingValue(SETTINGS.searchScope, 'everywhere')).toBe(false);
});

const HEX = 'ab'.repeat(16);

describe('settings keys (L-TS-25)', () => {
  it('names the three key shapes', () => {
    expect(SETTINGS.notifyDefault).toBe('notify.default');
    expect(SETTINGS.notifyChannel(HEX)).toBe(`notify.channel.${HEX}`);
    expect(SETTINGS.muteChannel(HEX)).toBe(`mute.channel.${HEX}`);
  });

  it('accepts exactly those shapes', () => {
    expect(isSettingKey('notify.default')).toBe(true);
    expect(isSettingKey(`notify.channel.${HEX}`)).toBe(true);
    expect(isSettingKey(`mute.channel.${HEX}`)).toBe(true);
    for (const bad of ['', 'theme', 'notify', 'notify.default.x', ` notify.default`, `notify.channel.${HEX.toUpperCase()}`,
      `notify.channel.${HEX}0`, `mute.channel.${HEX.slice(1)}`, `mute.channel.${HEX} `, `notify.channel.`, `mute.channel.${'g'.repeat(32)}`]) {
      expect(isSettingKey(bad), JSON.stringify(bad)).toBe(false);
    }
  });

  it('admits each shape only its values', () => {
    for (const v of ['dms-mentions', 'everything', 'nothing']) expect(isSettingValue('notify.default', v)).toBe(true);
    for (const v of ['all', 'mentions', 'nothing']) expect(isSettingValue(`notify.channel.${HEX}`, v)).toBe(true);
    expect(isSettingValue(`mute.channel.${HEX}`, '1')).toBe(true);
    expect(isSettingValue('notify.default', 'all')).toBe(false);
    expect(isSettingValue(`notify.channel.${HEX}`, 'everything')).toBe(false);
    expect(isSettingValue(`mute.channel.${HEX}`, '0')).toBe(false);
    expect(isSettingValue(`mute.channel.${HEX}`, '')).toBe(false);
    expect(isSettingValue('theme', 'mesh')).toBe(false);
  });
});

describe('effectiveNotifyMode (Q09)', () => {
  it('defaults to DMs and mentions', () => {
    expect(effectiveNotifyMode({}, HEX, 0)).toBe('mentions');
    expect(effectiveNotifyMode({}, HEX, 3)).toBe('all');
    expect(effectiveNotifyMode({}, HEX, 4)).toBe('all');
    expect(effectiveNotifyMode({ 'notify.default': 'dms-mentions' }, HEX, 0)).toBe('mentions');
  });

  it('follows the default', () => {
    expect(effectiveNotifyMode({ 'notify.default': 'everything' }, HEX, 0)).toBe('all');
    expect(effectiveNotifyMode({ 'notify.default': 'everything' }, HEX, 3)).toBe('all');
    expect(effectiveNotifyMode({ 'notify.default': 'nothing' }, HEX, 0)).toBe('nothing');
    expect(effectiveNotifyMode({ 'notify.default': 'nothing' }, HEX, 4)).toBe('nothing');
  });

  it('lets a channel override the default', () => {
    expect(effectiveNotifyMode({ 'notify.default': 'everything', [`notify.channel.${HEX}`]: 'nothing' }, HEX, 0)).toBe('nothing');
    expect(effectiveNotifyMode({ 'notify.default': 'nothing', [`notify.channel.${HEX}`]: 'all' }, HEX, 0)).toBe('all');
    expect(effectiveNotifyMode({ [`notify.channel.${HEX}`]: 'mentions' }, HEX, 3)).toBe('mentions');
    expect(effectiveNotifyMode({ [`notify.channel.${'cd'.repeat(16)}`]: 'nothing' }, HEX, 0)).toBe('mentions');
  });

  it('ignores a value it does not admit', () => {
    expect(effectiveNotifyMode({ [`notify.channel.${HEX}`]: 'loud' }, HEX, 0)).toBe('mentions');
    expect(effectiveNotifyMode({ 'notify.default': 'loud' }, HEX, 0)).toBe('mentions');
    expect(effectiveNotifyMode({ 'notify.default': 'loud' }, HEX, 3)).toBe('all');
  });
});

describe('isMuted', () => {
  it('is true only for the value 1', () => {
    expect(isMuted({ [`mute.channel.${HEX}`]: '1' }, HEX)).toBe(true);
    expect(isMuted({ [`mute.channel.${HEX}`]: '0' }, HEX)).toBe(false);
    expect(isMuted({}, HEX)).toBe(false);
    expect(isMuted({ [`mute.channel.${'cd'.repeat(16)}`]: '1' }, HEX)).toBe(false);
  });
});

// L-TS-54: the voice and sound keys web-3 adds. Each admits only its own values, so a page cannot turn the
// settings table into a general store (architect ruling 14).
describe('voice and sound settings (L-TS-54)', () => {
  const ID = 'cd'.repeat(16);
  it('accepts one value and refuses another for every key', () => {
    const cases: [string, string[], string[]][] = [
      [SETTINGS.voiceInput, ['default', 'Ab0+/=_-', 'x'.repeat(128)], ['', 'x'.repeat(129), 'a b', 'a.b']],
      [SETTINGS.voiceOutput, ['f00d', 'x'.repeat(128)], ['', 'x'.repeat(129), 'é']],
      [SETTINGS.voiceMode, ['vad', 'ptt'], ['', 'VAD', 'push']],
      [SETTINGS.voicePttKey, ['KeyV', 'F13', 'Space', 'a' + '1'.repeat(31)], ['', '1Key', 'Key V', 'a' + '1'.repeat(32)]],
      [SETTINGS.voiceNoise, ['on', 'off', 'automatic', 'lightweight'], ['1', 'true']],
      [SETTINGS.voiceVolume(ID), ['0', '10', '100', '110', '200'], ['05', '15', '210', '-10', '100.0', '']],
      // #37 T5: the volume of a person's shared screen sound; 0 is the stream mute.
      [SETTINGS.voiceStreamVolume(ID), ['0', '10', '100', '110', '200'], ['05', '15', '210', '-10', '100.0', '']],
      [SETTINGS.soundVoice, ['on', 'off'], ['0', '1']],
      [SETTINGS.soundRing, ['on', 'off'], ['0', 'quiet']],
      // Product walk M5: the own mute and deafen kept across a reload.
      [SETTINGS.voiceSelf, ['muted', 'deafened'], ['', 'on', 'live', '1']],
      // Signal task 10b: input and output volume (whole %, 0..200), automatic sensitivity, the threshold (whole dB, −100..0),
      // the push-to-talk release delay (0..1000 ms in steps of 20), echo cancellation and automatic gain.
      [SETTINGS.voiceInputVolume, ['0', '1', '57', '100', '199', '200'], ['', '-1', '201', '007', '50.5', '1e2', ' 100']],
      [SETTINGS.voiceOutputVolume, ['0', '100', '200'], ['', '300', '01', 'loud']],
      [SETTINGS.voiceSensitivityAuto, ['on', 'off'], ['', '1', 'true']],
      [SETTINGS.voiceSensitivity, ['0', '-1', '-48', '-50', '-99', '-100'], ['', '1', '-0', '-101', '-050', '-48.5', '+0', '-1000']],
      [SETTINGS.voicePttRelease, ['0', '20', '200', '980', '1000'], ['', '10', '210', '1020', '-20', '020', '200.0']],
      [SETTINGS.voiceEcho, ['on', 'off'], ['', '0', 'yes']],
      [SETTINGS.voiceAgc, ['on', 'off'], ['', '1', 'auto']],
    ];
    for (const [key, good, bad] of cases) {
      expect(isSettingKey(key), key).toBe(true);
      for (const v of good) expect(isSettingValue(key, v), `${key}=${v}`).toBe(true);
      for (const v of bad) expect(isSettingValue(key, v), `${key}=${v}`).toBe(false);
    }
  });

  // Signal task 10 fix round 1: the Desktop notifications switch of Settings → Notifications.
  it('admits notify.desktop as on or off', () => {
    expect(SETTINGS.notifyDesktop).toBe('notify.desktop');
    expect(isSettingValue('notify.desktop', 'off')).toBe(true);
    expect(isSettingValue('notify.desktop', 'on')).toBe(true);
    expect(isSettingValue('notify.desktop', '1')).toBe(false);
  });

  it('names the keys and refuses near misses', () => {
    expect([SETTINGS.voiceInput, SETTINGS.voiceOutput, SETTINGS.voiceMode, SETTINGS.voicePttKey, SETTINGS.voiceNoise,
      SETTINGS.voiceVolume(ID), SETTINGS.soundVoice, SETTINGS.soundRing]).toEqual(['voice.input', 'voice.output', 'voice.mode',
      'voice.pttKey', 'voice.noise', `voice.volume.${ID}`, 'sound.voice', 'sound.ring']);
    for (const bad of ['voice', 'voice.volume.', 'voice.streamVolume.', `voice.streamVolume.${ID.toUpperCase()}`, `voice.streamVolume.${ID}0`, `voice.volume.${ID.toUpperCase()}`, `voice.volume.${ID}0`, 'sound', 'sound.mention', 'voice.ptt', 'voice.inputvolume', 'voice.sensitivity.auto', 'voice.echo.on']) {
      expect(isSettingKey(bad), bad).toBe(false);
    }
  });

  it('defaults to voice activity, noise suppression on, full volume and both sounds on', () => {
    expect(VOICE_DEFAULTS).toEqual({ mode: 'vad', pttKey: null, noise: 'on', volume: '100', soundVoice: 'on', soundRing: 'on',
      inputVolume: '100', outputVolume: '100', sensitivityAuto: 'on', sensitivity: '-50', pttRelease: '200', echo: 'on', agc: 'on' });
    // Every default is a value its key admits.
    expect(isSettingValue(SETTINGS.voiceInputVolume, VOICE_DEFAULTS.inputVolume)).toBe(true);
    expect(isSettingValue(SETTINGS.voiceSensitivity, VOICE_DEFAULTS.sensitivity)).toBe(true);
    expect(isSettingValue(SETTINGS.voicePttRelease, VOICE_DEFAULTS.pttRelease)).toBe(true);
  });
});

// web3.5: per-server notification, mute and @everyone switches.
describe('community settings (web3.5)', () => {
  const COM = 'cd'.repeat(16);
  const OTHER = 'ef'.repeat(16);

  it('names the three keys and validates keys and values', () => {
    expect(SETTINGS.notifyCommunity(COM)).toBe(`notify.community.${COM}`);
    expect(SETTINGS.muteCommunity(COM)).toBe(`mute.community.${COM}`);
    expect(SETTINGS.everyoneCommunity(COM)).toBe(`everyone.community.${COM}`);
    for (const key of [SETTINGS.notifyCommunity(COM), SETTINGS.muteCommunity(COM), SETTINGS.everyoneCommunity(COM)]) {
      expect(isSettingKey(key)).toBe(true);
      expect(isSettingKey(`${key}0`)).toBe(false);
      expect(isSettingKey(key.slice(0, -1))).toBe(false);
      expect(isSettingKey(key.toUpperCase())).toBe(false);
    }
    for (const v of ['all', 'mentions', 'nothing']) expect(isSettingValue(SETTINGS.notifyCommunity(COM), v)).toBe(true);
    expect(isSettingValue(SETTINGS.notifyCommunity(COM), 'everything')).toBe(false);
    expect(isSettingValue(SETTINGS.notifyCommunity(COM), '1')).toBe(false);
    for (const key of [SETTINGS.muteCommunity(COM), SETTINGS.everyoneCommunity(COM)]) {
      expect(isSettingValue(key, '1')).toBe(true);
      expect(isSettingValue(key, '0')).toBe(false);
      expect(isSettingValue(key, 'all')).toBe(false);
    }
  });

  it('resolves channel over community over default', () => {
    const s = { 'notify.default': 'everything', [SETTINGS.notifyCommunity(COM)]: 'mentions', [SETTINGS.notifyChannel(HEX)]: 'nothing' };
    expect(effectiveNotifyMode(s, HEX, 0, COM)).toBe('nothing');
    const noChannel = { 'notify.default': 'everything', [SETTINGS.notifyCommunity(COM)]: 'mentions' };
    expect(effectiveNotifyMode(noChannel, HEX, 0, COM)).toBe('mentions');
    expect(effectiveNotifyMode(noChannel, HEX, 0)).toBe('all');
    expect(effectiveNotifyMode(noChannel, HEX, 0, OTHER)).toBe('all');
    expect(effectiveNotifyMode(noChannel, HEX, 0, 'zz')).toBe('all');
    expect(effectiveNotifyMode({ [SETTINGS.notifyCommunity(COM)]: 'loud' }, HEX, 0, COM)).toBe('mentions');
    expect(effectiveNotifyMode({ [SETTINGS.notifyCommunity(COM)]: 'loud' }, HEX, 3, COM)).toBe('all');
  });

  it('mutes by community alone', () => {
    const s = { [SETTINGS.muteCommunity(COM)]: '1' };
    expect(isMuted(s, HEX, COM)).toBe(true);
    expect(isMuted(s, HEX)).toBe(false);
    expect(isMuted(s, HEX, OTHER)).toBe(false);
    expect(isMuted({ [SETTINGS.muteChannel(HEX)]: '1' }, HEX, COM)).toBe(true);
    expect(isMuted({ [SETTINGS.muteCommunity(COM)]: '0' }, HEX, COM)).toBe(false);
  });

  it('suppresses @everyone per community', () => {
    expect(suppressesEveryone({ [SETTINGS.everyoneCommunity(COM)]: '1' }, COM)).toBe(true);
    expect(suppressesEveryone({ [SETTINGS.everyoneCommunity(COM)]: '1' }, OTHER)).toBe(false);
    expect(suppressesEveryone({}, COM)).toBe(false);
    expect(suppressesEveryone({ [SETTINGS.everyoneCommunity(COM)]: '0' }, COM)).toBe(false);
  });
});

describe('channels.collapsed.<categoryHex> (R36)', () => {
  it('is a key with the value 1 and nothing else', () => {
    const key = SETTINGS.channelCollapsed(HEX);
    expect(key).toBe(`channels.collapsed.${HEX}`);
    expect(isSettingKey(key)).toBe(true);
    expect(isSettingValue(key, '1')).toBe(true);
    expect(isSettingValue(key, '2')).toBe(false);
    expect(isSettingKey('channels.collapsed.x')).toBe(false);
    expect(isSettingKey(`channels.collapsed.${HEX}0`)).toBe(false);
  });
});
