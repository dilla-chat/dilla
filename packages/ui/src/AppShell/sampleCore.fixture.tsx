// Stories only (Task 11, Jonas Q3, rule 3d): a read-only CoreClient holding the prototype's Midgard sample as slices, so a
// whole-screen story renders the web's own screens (screens/Activity.tsx, screens/You.tsx) instead of a copy of them.
import type { ReactNode } from 'react';
import type { CoreClient, SliceName, SliceTypes } from '@dilla/client-core';
import { CoreProvider } from '../../../web/src/core/context.tsx';
import { PEOPLE, YOU, HOST, PING_MS } from '../internal/sample/midgard.ts';

const MC = 'a1'.repeat(16);
const VK = 'b2'.repeat(16);
const VK_GENERAL = 'c3'.repeat(16);
const DM_ADA = 'd4'.repeat(16);
const ME = 'e5'.repeat(16);
const NOW = Math.floor(Date.now() / 1000);
const member = (userId: string, p: { name: string; handle: string; status: string; bot?: boolean }) =>
  ({ userId, username: p.handle, display: p.name, kind: p.bot === true ? 1 as const : 0 as const, roleIds: [], statusMsg: p.status });

const SLICES: Record<string, unknown> = {
  account: {
    presenceChoice: 'online', phase: 'ready',
    instance: { id: 'f0'.repeat(16), name: HOST, registrationMode: 1, passwordSignup: true, totp: true, passkeys: true },
    user: { id: ME, username: YOU.handle, display: YOU.name, statusMsg: YOU.status, bio: YOU.bio },
    deviceId: 'd3f9a1'.padEnd(32, '0'),
  } as SliceTypes['account'],
  connection: { status: 'online', generation: '7', pingMs: PING_MS },
  communities: [{ id: MC, name: 'Midgard Crew', owner: 'aa'.repeat(16) }, { id: VK, name: 'Valhalla Kitchen', owner: 'bb'.repeat(16) }],
  [`members:${MC}`]: [member(ME, YOU), member('aa'.repeat(16), PEOPLE.Ada), member('ab'.repeat(16), PEOPLE.Glenn)],
  [`members:${VK}`]: [member(ME, YOU), member('ba'.repeat(16), PEOPLE.Liv), member('bb'.repeat(16), PEOPLE.Sven)],
  [`channels:${VK}`]: [{ id: VK_GENERAL, communityId: VK, kind: 0, mode: 0, name: 'general', topic: '', parentId: null, position: 0, group: 'none', slowmode: 0, announcement: false }],
  dms: [],
  // Settings → Sign-in (Task 11, Q7): the prototype's four factors.
  factors: { owed: false, totp: { confirmedAt: Date.UTC(2026, 9, 12, 12) / 1000 }, backupCodesLeft: 8, hasPassword: true,
    passwordChanged: NOW - 21 * 86_400, passkeys: [{ id: 'ab'.repeat(16), name: 'MacBook Touch ID', created: Date.UTC(2026, 9, 12, 12) / 1000, lastUsed: null }] },
  // Settings → Devices (fix round 2): the prototype's two browsers, this one named "Firefox on Linux".
  devices: [
    { id: 'd3f9a1'.padEnd(32, '0'), tier: 1, signerTier: 1, lastSeen: NOW - 30, revokedAt: null, listed: true, own: true, label: 'Firefox on Linux' },
    { id: '8b20ce'.padEnd(32, '0'), tier: 1, signerTier: 1, lastSeen: NOW - 2 * 86_400, revokedAt: null, listed: true, own: false, label: 'Chrome on macOS' },
  ],
  badges: {},
  settings: {},
  // The prototype's Activity, oldest first (the tab lists newest first): Ada's DM, Sven's and Liv's mentions in Valhalla Kitchen.
  notices: { nextId: 4, items: [
    { id: 1, channelId: DM_ADA, communityId: null, kind: 'dm', senderUser: 'aa'.repeat(16), senderName: 'Ada', body: 'two ropes. final answer.', ts: NOW - 300, threadRoot: null },
    { id: 2, channelId: VK_GENERAL, communityId: VK, kind: 'mention', senderUser: 'bb'.repeat(16), senderName: 'Sven', body: 'and the big pot', ts: NOW - 200, threadRoot: null },
    { id: 3, channelId: VK_GENERAL, communityId: VK, kind: 'mention', senderUser: 'ba'.repeat(16), senderName: 'Liv', body: 'can you bring the cardamom?', ts: NOW - 100, threadRoot: null },
  ] },
};

const client = {
  get: (name: SliceName) => SLICES[name],
  subscribe: () => () => undefined,
  // The sample's answers to the reads a screen makes on mount (Overview's inactive timeout); anything else answers null.
  call: (command: { m: string }) => Promise.resolve(command.m === 'getAFKSettings' ? { channelId: null, timeoutSeconds: 900 } : null),
} as unknown as CoreClient;

/** Midgard Crew's id in the sample. */
export const SAMPLE_MC = MC;
/** The Midgard sample as the worker's slices, for the web screens a story renders. */
export function SampleCore({ children }: { children: ReactNode }) {
  return <CoreProvider client={client}>{children}</CoreProvider>;
}
/** The due reminder of the sample ("bring two ropes" in #general), as Shell.tsx hands it to Activity. */
export const SAMPLE_DUE = [{ key: 'r:1', text: 'bring two ropes', where: '#general' }] as const;
