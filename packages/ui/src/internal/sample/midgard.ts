/**
 * The Signal prototype's sample data (docs/design/signal/README.md §11; the prototype's `C`, `PEOPLE`, `SP`, `store`, `threads`,
 * `occ`, `rem` and `pf`), copied verbatim for the whole-screen stories so they draw what the prototype draws. Stories only: nothing
 * in packages/web reads this file.
 */

/** A person of the sample: the prototype's display name, handle, status line, bio and avatar hue (`H`). */
export type SamplePerson = { name: string; handle: string; status: string; bio: string; hue: number; bot?: boolean };

export const PEOPLE = {
  Ada: { name: 'Ada', handle: 'ada', status: 'planning the raid', bio: 'Raid lead. Has the map. Will not explain the stain.', hue: 298 },
  Björn: { name: 'Björn', handle: 'bjorn', status: 'bridge troll survivor', bio: 'Carries shields, loses them.', hue: 211 },
  Mira: { name: 'Mira', handle: 'mira', status: 'packing rope', bio: 'Navigator. Will not carry the snacks.', hue: 234 },
  Sven: { name: 'Sven', handle: 'sven', status: 'stirring', bio: 'Owner of the kitchen.', hue: 28 },
  Liv: { name: 'Liv', handle: 'liv', status: 'on the stove', bio: 'Cardamom maximalist.', hue: 160 },
  Kari: { name: 'Kari', handle: 'kari', status: 'streaming', bio: 'Builds the boats.', hue: 12 },
  Ole: { name: 'Ole', handle: 'ole', status: 'afk-ish', bio: 'Rows.', hue: 48 },
  Glenn: { name: 'Glenn', handle: 'glenn', status: 'playing in harbour', bio: 'Managed bot. Music, polls, reminders and moderation for this server.', hue: 205, bot: true },
} as const satisfies Record<string, SamplePerson>;

/** You, as the prototype's `pf` holds you. */
export const YOU = PEOPLE.Mira;

/** The servers on the rail, in order (Valhalla Kitchen has two mentions for you in #general). */
export const SERVERS = [
  { id: 'mc', name: 'Midgard Crew', initials: 'MC' },
  { id: 'vk', name: 'Valhalla Kitchen', initials: 'VK', unread: 2, mentions: 2 },
] as const;

export const MIDGARD = {
  name: 'Midgard Crew',
  general: { name: 'general', topic: 'raids, plans and the bridge troll' },
  loot: { name: 'loot', topic: 'maps, drops and who owes who' },
  /** The sidebar's stat line: three people online (bots not counted), five in harbour (six once you join). */
  online: 3,
  inVoice: 5,
  members: 6,
  roleColor: 0xe9b6ec,
} as const;

/** Harbour's occupants as the prototype's `occ` and `mediaOf` set them; Glenn plays music there. */
export const HARBOUR = [
  { name: 'Ada', screen: true, video: false },
  { name: 'Björn', screen: false, video: true },
  { name: 'Kari', screen: true, video: true },
  { name: 'Ole', screen: false, video: false },
] as const;

/** #general's thread on Björn's message (`threads.t1`). */
export const THREAD = {
  root: { author: 'Björn', time: '20:27', body: 'in. last time the bridge troll took everything we had' },
  replies: [
    { author: 'Ada', time: '20:31', body: 'bring a rope this time' },
    { author: 'Mira', time: '20:32', body: 'two ropes' },
    { author: 'Björn', time: '20:33', body: 'fine. two ropes.' },
  ],
  label: '3 replies',
  last: 'last 20:33',
} as const;

/** The direct messages (`dm:*`) and the DM list's rows. */
export const DMS = [
  { id: 'ada', name: 'Ada', sub: 'planning the raid', online: true, messages: [{ author: 'Ada', time: '20:34', body: 'two ropes. final answer.' }] },
  { id: 'bjorn', name: 'Björn', sub: 'do you still have my shield', online: true, messages: [
    { author: 'Björn', time: 'yesterday', body: 'do you still have my shield' }, { author: 'Mira', time: 'yesterday', body: 'define “have”' }] },
  { id: 'crew', name: 'raid crew', sub: 'Ada, Björn, you', online: false, messages: [
    { author: 'Ada', time: '17:20', body: 'group photo after the raid' }, { author: 'Björn', time: '17:22', body: 'only if the troll is in it' }] },
] as const;

/** Reminders (`rem`): one due, one upcoming. */
export const REMINDERS = [
  { id: '1', text: 'bring two ropes', at: 'Today, 20:00', where: '#general', due: true },
  { id: '2', text: 'raid at the bridge', at: 'Fri 30 Oct, 20:00 CET', where: '#general', due: false },
] as const;
export const REMINDERS_DUE = REMINDERS.filter(r => r.due).length;

/** The phone's Activity rows, in the prototype's order. */
export const ACTIVITY = [
  { who: 'Glenn', where: 'Reminder · #general', text: 'bring two ropes' },
  { who: 'Liv', where: 'Valhalla Kitchen · #general', text: 'mentioned you: can you bring the cardamom?' },
  { who: 'Sven', where: 'Valhalla Kitchen · #general', text: 'mentioned you: and the big pot' },
  { who: 'Ada', where: 'Direct message', text: 'two ropes. final answer.' },
] as const;

/** This browser and the other one (`devices`), and the connection line of the phone's You tab. */
export const DEVICES = [
  { id: 'd3f9a1', name: 'Firefox on Linux', meta: 'this browser · active now', current: true },
  { id: '8b20ce', name: 'Chrome on macOS', meta: 'active 2 days ago', current: false },
] as const;
export const HOST = 'midgard.gg';
export const PING_MS = 24;
