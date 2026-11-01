import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, screen, waitFor, within } from '@testing-library/react';
import type { VoiceParticipant } from '@dilla/client-core';
import type { Ring } from '@dilla/client-core';
import { ringAudible } from './useSounds.tsx';
import { ME } from '../../test/fixtures.ts';
import { BOB, BOB_DEV, callState, CAROL, CAROL_DEV, GENV, OWN_DEV, renderVoiceShell, type VoiceHarnessOptions } from '../../test/fake-client.ts';

afterEach(() => { vi.unstubAllGlobals(); });

const main = () => within(screen.getByRole('main'));
const person = (userId: string, deviceId: string, name: string, own = false): VoiceParticipant =>
  ({ userId, deviceId, name, flags: 1, own, web: true, bot: false });
const BOB_IN = person(BOB, BOB_DEV, 'bob');
const ME_IN = person(ME.id, OWN_DEV, 'ada', true);
const CAROL_IN = person(CAROL, CAROL_DEV, 'carol');
const room = (...participants: VoiceParticipant[]) => ({ [GENV]: { callId: GENV, participants } });

async function inCall(o: VoiceHarnessOptions = {}) {
  const app = renderVoiceShell({ voice: room(ME_IN, BOB_IN), ...o });
  await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
  await waitFor(() => expect(app.sessions).toHaveLength(1));
  app.client.publish('call', callState({ phase: 'in', channelId: GENV, callId: GENV }));
  await main().findByRole('toolbar', { name: 'voice channel controls' });
  return app;
}

describe('join and leave sounds (flow 06 § Sounds, R2, R4)', () => {
  it('join and leave sounds play for other users only, never while deafened or with sound.voice off', async () => {
    const app = await inCall();
    // The people already there when this device joined make no sound.
    expect(app.sounds.played).toEqual([]);
    app.client.publish('voice', room(ME_IN, BOB_IN, CAROL_IN));
    expect(app.sounds.played).toEqual(['join']);
    // A second device of the own user: no sound.
    app.client.publish('voice', room(ME_IN, BOB_IN, CAROL_IN, person(ME.id, 'ee'.repeat(16), 'ada')));
    expect(app.sounds.played).toEqual(['join']);
    // Deafened: carol leaves, no sound.
    act(() => app.session.setDeafened(true));
    app.client.publish('voice', room(ME_IN, BOB_IN));
    expect(app.sounds.played).toEqual(['join']);
    // Undeafened, sound.voice off: carol joins, no sound.
    act(() => app.session.setDeafened(false));
    app.client.publish('settings', { 'sound.voice': 'off' });
    app.client.publish('voice', room(ME_IN, BOB_IN, CAROL_IN));
    expect(app.sounds.played).toEqual(['join']);
    // sound.voice on again: bob leaves.
    app.client.publish('settings', {});
    app.client.publish('voice', room(ME_IN, CAROL_IN));
    expect(app.sounds.played).toEqual(['join', 'leave']);
    // Before any user activation: no sound.
    app.activation.hasBeenActive = false;
    app.client.publish('voice', room(ME_IN, CAROL_IN, BOB_IN));
    expect(app.sounds.played).toEqual(['join', 'leave']);
  });

  it('no sound for the own join or leave, nor for a pause (R2, RV-3)', async () => {
    const app = renderVoiceShell({ voice: room(BOB_IN) });
    await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    app.client.publish('voice', room(BOB_IN, ME_IN));
    app.client.publish('call', callState({ phase: 'in', channelId: GENV, callId: GENV }));
    app.client.publish('call', callState({ phase: 'paused', channelId: GENV, callId: GENV, changedUser: BOB }));
    app.client.publish('call', callState({ phase: 'in', channelId: GENV, callId: GENV }));
    await app.user.click(main().getByRole('button', { name: 'Leave' }));
    app.client.publish('voice', room(BOB_IN));
    expect(app.sounds.played).toEqual([]);
  });

  it('plays nothing for voice it is not in', async () => {
    const app = renderVoiceShell({ voice: room(BOB_IN) });
    await main().findByRole('button', { name: 'Join voice' });
    app.client.publish('voice', room(BOB_IN, CAROL_IN));
    expect(app.sounds.played).toEqual([]);
  });

  it('m1: the first snapshot of the room after joining is the baseline, not a join', async () => {
    const app = renderVoiceShell({ voice: {} });
    await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    await main().findByRole('toolbar', { name: 'voice channel controls' });
    app.client.publish('voice', room(BOB_IN, ME_IN));
    expect(app.sounds.played).toEqual([]);
    app.client.publish('voice', room(BOB_IN, ME_IN, CAROL_IN));
    expect(app.sounds.played).toEqual(['join']);
  });
});

describe('ringAudible (web3.5 WA5)', () => {
  const CH = 'ab'.repeat(16);
  const SRV = 'cd'.repeat(16);
  const ring = (over: Partial<Ring> = {}): Ring => ({ callId: CH, channelId: CH, callerUser: BOB, callerName: 'bob', ringTs: 1, expiresAt: 0, busy: false, quiet: false, ...over });
  it('sounds for a plain ring, and not for a busy, quiet, muted-channel or muted-server ring', () => {
    expect(ringAudible([ring()], {}, () => SRV)).toBe(true);
    expect(ringAudible([ring({ busy: true })], {}, () => SRV)).toBe(false);
    expect(ringAudible([ring({ quiet: true })], {}, () => SRV)).toBe(false);
    expect(ringAudible([ring()], { [`mute.channel.${CH}`]: '1' }, () => SRV)).toBe(false);
    expect(ringAudible([ring()], { [`mute.community.${SRV}`]: '1' }, () => SRV)).toBe(false);
    expect(ringAudible([ring()], { [`mute.community.${SRV}`]: '1' }, () => undefined)).toBe(true);
  });
});
