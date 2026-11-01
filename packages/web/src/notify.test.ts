import { describe, it, expect, vi } from 'vitest';
import type { ChannelSummary, Notice } from '@dilla/client-core';
import { FakeClient } from './test/fake-client.ts';
import { account, ME } from './test/fixtures.ts';
import { noticeTitle, ringNotification, shouldNotify, startNotifier, type NotificationCtor, type NotifyContext } from './notify.ts';
import type { Ring } from '@dilla/client-core';

const A = 'a1'.repeat(16);
const GEN = 'c3'.repeat(16);
const ROLE = 'f7'.repeat(16);
const RAND = 'd4'.repeat(16);
const DM = '9a'.repeat(16);
const ch = (id: string, name: string): ChannelSummary =>
  ({ id, communityId: A, kind: 0, mode: 0, name, topic: '', parentId: null, position: 0, slowmode: 0, announcement: false, group: 'active' });
const notice = (over: Partial<Notice> = {}): Notice => ({
  id: 0, channelId: RAND, communityId: A, kind: 'message', senderUser: '28'.repeat(16), senderName: 'bob', body: 'hi', ts: 1_790_000_000, threadRoot: null, ...over,
});
const dmNotice = (over: Partial<Notice> = {}): Notice => notice({ channelId: DM, communityId: null, kind: 'dm', ...over });
const ctx = (over: Partial<NotifyContext> = {}): NotifyContext => ({ permission: 'granted', settings: {}, visible: true, onScreen: GEN, presenceChoice: 'online', ...over });

describe('shouldNotify (L-TS-25)', () => {
  const rows: [string, Notice, 0 | 3 | 4, NotifyContext, boolean][] = [
    ['a DM by default', dmNotice(), 3, ctx(), true],
    ['a DM in Do Not Disturb', dmNotice(), 3, ctx({ presenceChoice: 'dnd' }), false],
    ['a DM before the saved choice loads', dmNotice(), 3, ctx({ presenceChoice: undefined }), false],
    ['a mention in Do Not Disturb', notice({ kind: 'mention' }), 0, ctx({ presenceChoice: 'dnd' }), false],
    ['a group DM by default', dmNotice(), 4, ctx(), true],
    ['a plain message by default', notice(), 0, ctx(), false],
    ['a mention by default', notice({ kind: 'mention' }), 0, ctx(), true],
    ['a mention in the channel on screen', notice({ channelId: GEN, kind: 'mention' }), 0, ctx(), false],
    ['a DM on screen', dmNotice(), 3, ctx({ onScreen: DM }), false],
    ['a mention in the channel on screen while the page is hidden', notice({ channelId: GEN, kind: 'mention' }), 0, ctx({ visible: false }), true],
    ['without permission', notice({ kind: 'mention' }), 0, ctx({ permission: 'default' }), false],
    ['when denied', notice({ kind: 'mention' }), 0, ctx({ permission: 'denied' }), false],
    ['without the API', notice({ kind: 'mention' }), 0, ctx({ permission: 'unsupported' }), false],
    ['in a muted channel', notice({ kind: 'mention' }), 0, ctx({ settings: { [`mute.channel.${RAND}`]: '1' } }), false],
    ['in a muted DM', dmNotice(), 3, ctx({ settings: { [`mute.channel.${DM}`]: '1' } }), false],
    ['a message when the default is everything', notice(), 0, ctx({ settings: { 'notify.default': 'everything' } }), true],
    ['a mention when the default is nothing', notice({ kind: 'mention' }), 0, ctx({ settings: { 'notify.default': 'nothing' } }), false],
    ['a DM when the default is nothing', dmNotice(), 3, ctx({ settings: { 'notify.default': 'nothing' } }), false],
    ['a channel set to all under nothing', notice(), 0, ctx({ settings: { 'notify.default': 'nothing', [`notify.channel.${RAND}`]: 'all' } }), true],
    ['a DM set to nothing', dmNotice(), 3, ctx({ settings: { [`notify.channel.${DM}`]: 'nothing' } }), false],
    ['a DM set to mentions', dmNotice(), 3, ctx({ settings: { [`notify.channel.${DM}`]: 'mentions' } }), true],
    ['a plain message in a channel set to mentions', notice(), 0, ctx({ settings: { 'notify.default': 'everything', [`notify.channel.${RAND}`]: 'mentions' } }), false],
  ];
  it.each(rows)('%s', (_, n, kind, c, want) => {
    expect(shouldNotify(n, kind, c)).toBe(want);
  });
});

// Signal task 10 fix round 1: Settings → Notifications' Desktop notifications switch.
describe('shouldNotify and the desktop switch', () => {
  it('shows nothing on this device while notify.desktop is off', () => {
    const n = notice({ kind: 'mention' });
    expect(shouldNotify(n, 0, ctx({ visible: false }))).toBe(true);
    expect(shouldNotify(n, 0, ctx({ visible: false, settings: { 'notify.desktop': 'off' } }))).toBe(false);
    expect(shouldNotify(n, 0, ctx({ visible: false, settings: { 'notify.desktop': 'on' } }))).toBe(true);
  });
});

describe('shouldNotify per server (web3.5 WA5)', () => {
  const server = (v: string) => ({ [`notify.community.${A}`]: v });
  it('a server set to nothing silences a channel with no override; a channel override still wins', () => {
    expect(shouldNotify(notice({ kind: 'mention' }), 0, ctx({ settings: server('nothing') }))).toBe(false);
    expect(shouldNotify(notice({ kind: 'mention' }), 0, ctx({ settings: { ...server('nothing'), [`notify.channel.${RAND}`]: 'all' } }))).toBe(true);
  });
  it('a server set to all notifies a plain message', () => {
    expect(shouldNotify(notice(), 0, ctx({ settings: server('all') }))).toBe(true);
  });
  it('a muted server notifies nothing', () => {
    expect(shouldNotify(notice({ kind: 'mention' }), 0, ctx({ settings: { [`mute.community.${A}`]: '1' } }))).toBe(false);
  });
  it('a server that ignores @everyone turns a broadcast-only mention into a message, and keeps a direct one', () => {
    const settings = { [`everyone.community.${A}`]: '1' };
    const c = ctx({ settings, me: ME.id, roles: [] });
    expect(shouldNotify(notice({ kind: 'mention', body: 'hey <@everyone>' }), 0, c)).toBe(false);
    expect(shouldNotify(notice({ kind: 'mention', body: 'hey <@here>' }), 0, c)).toBe(false);
    expect(shouldNotify(notice({ kind: 'mention', body: `hey <@everyone> <@${ME.id}>` }), 0, c)).toBe(true);
    expect(shouldNotify(notice({ kind: 'mention', body: 'hey <@everyone>' }), 0, ctx({ settings: {}, me: ME.id, roles: [] }))).toBe(true);
    // a direct role mention is kept
    expect(shouldNotify(notice({ kind: 'mention', body: `<@everyone> <@${ROLE}>` }), 0, ctx({ settings, me: ME.id, roles: [ROLE] }))).toBe(true);
    expect(shouldNotify(notice({ kind: 'mention', body: `<@everyone> <@${ROLE}>` }), 0, c)).toBe(false);
    // as a plain message it follows the message rules: all notifies it
    expect(shouldNotify(notice({ kind: 'mention', body: 'hey <@everyone>' }), 0, ctx({ settings: { ...settings, ...server('all') }, me: ME.id, roles: [] }))).toBe(true);
  });
});

describe('startNotifier under @everyone suppression (web3.5 WA5 fix)', () => {
  const suppress = { [`everyone.community.${A}`]: '1' };
  const run = (setup: (h: ReturnType<typeof harness>) => void, bodies: string[]) => {
    const h = harness();
    h.fake.set('settings', suppress);
    setup(h);
    h.fake.set('notices', { nextId: 0, items: [] });
    const stop = h.start();
    h.fake.set('notices', { nextId: bodies.length, items: bodies.map((body, id) => notice({ id, kind: 'mention', body })) });
    stop();
    return Note.made.length;
  };
  const withRole = (h: ReturnType<typeof harness>) => {
    h.fake.set('account', account());
    h.fake.set(`members:${A}`, [{ userId: ME.id, username: 'ada', display: '', kind: 0, roleIds: [ROLE] }]);
  };
  it('notifies a role mention and a user mention, not a broadcast alone', () => {
    expect(run(withRole, [`<@${ROLE}>`, `<@${ME.id}>`, '<@everyone>'])).toBe(2);
  });
  it('fails open when the own user is unknown', () => {
    expect(run(h => { h.fake.set('account', account({ user: null })); h.fake.set(`members:${A}`, []); }, ['<@everyone>'])).toBe(1);
  });
  it('fails open when the community members are not loaded', () => {
    expect(run(h => h.fake.set('account', account()), ['<@everyone>'])).toBe(1);
  });
});

describe('noticeTitle', () => {
  it('names the channel and server, or the sender of a DM', () => {
    expect(noticeTitle(notice(), { channel: 'random', server: 'Midgard' })).toBe('#random · Midgard');
    expect(noticeTitle(dmNotice({ senderName: 'bob' }), { channel: null, server: null })).toBe('bob');
    expect(noticeTitle(notice(), { channel: null, server: null })).toBe('#d4d4d4d4 · a1a1a1a1');
  });
});

class Note {
  static permission: NotificationPermission = 'granted';
  static made: Note[] = [];
  onclick: ((ev: Event) => unknown) | null = null;
  closed = false;
  constructor(readonly title: string, readonly options: NotificationOptions) { Note.made.push(this); }
  close(): void { this.closed = true; }
}

function harness(over: { visible?: boolean; onScreen?: string | null } = {}) {
  Note.made = [];
  Note.permission = 'granted';
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set('communities', [{ id: A, name: 'Midgard', owner: 'ee'.repeat(16) }]);
  fake.set(`channels:${A}`, [ch(GEN, 'general'), ch(RAND, 'random')]);
  fake.set('dms', [{ id: DM, kind: 3, members: [], name: 'bob', group: 'active' }]);
  fake.set('settings', {});
  const focus = vi.fn();
  const open = vi.fn();
  const start = () => startNotifier({
    client: fake, Notification: Note, visible: () => over.visible ?? true, onScreen: () => over.onScreen ?? GEN, focus, open,
  });
  return { fake, focus, open, start };
}

describe('startNotifier', () => {
  it('never replays what the first slice held, then shows each new item once', () => {
    const h = harness();
    const old = [0, 1, 2].map(id => notice({ id, kind: 'mention' }));
    h.fake.set('notices', { nextId: 3, items: old });
    const stop = h.start();
    expect(Note.made).toEqual([]);
    h.fake.set('notices', { nextId: 4, items: [...old, notice({ id: 3, kind: 'mention', body: 'look' })] });
    expect(Note.made.map(n => [n.title, n.options])).toEqual([['#random · Midgard', { body: 'look', tag: `dilla:${RAND}`, silent: false }]]);
    expect('icon' in Note.made[0].options).toBe(false);
    h.fake.set('notices', { nextId: 4, items: [...old, notice({ id: 3, kind: 'mention', body: 'look' })] });
    expect(Note.made).toHaveLength(1);
    stop();
    h.fake.set('notices', { nextId: 5, items: [notice({ id: 4, kind: 'mention' })] });
    expect(Note.made).toHaveLength(1);
  });
  it('starts counting at the first slice that arrives after it started', () => {
    const h = harness();
    const stop = h.start();
    h.fake.set('notices', { nextId: 0, items: [] });
    h.fake.set('notices', { nextId: 1, items: [dmNotice({ id: 0, body: 'yo' })] });
    expect(Note.made.map(n => [n.title, n.options.tag])).toEqual([['bob', `dilla:${DM}`]]);
    stop();
  });
  it('applies the rules to each item', () => {
    const h = harness();
    h.fake.set('notices', { nextId: 0, items: [] });
    const stop = h.start();
    h.fake.set('notices', { nextId: 3, items: [notice({ id: 0 }), notice({ id: 1, kind: 'mention', body: 'm' }), notice({ id: 2, channelId: GEN, kind: 'mention' })] });
    expect(Note.made.map(n => n.options.body)).toEqual(['m']);
    stop();
  });
  it('focuses the tab, opens the channel and closes on click', () => {
    const h = harness();
    h.fake.set('notices', { nextId: 0, items: [] });
    const stop = h.start();
    const n = notice({ id: 0, kind: 'mention' });
    h.fake.set('notices', { nextId: 1, items: [n] });
    Note.made[0].onclick?.(new Event('click'));
    expect(h.focus).toHaveBeenCalledTimes(1);
    expect(h.open).toHaveBeenCalledWith(n);
    expect(Note.made[0].closed).toBe(true);
    stop();
  });
  it('shows nothing without permission, and survives a constructor that throws', () => {
    const h = harness();
    Note.permission = 'default';
    h.fake.set('notices', { nextId: 0, items: [] });
    const stop = h.start();
    h.fake.set('notices', { nextId: 1, items: [notice({ id: 0, kind: 'mention' })] });
    expect(Note.made).toEqual([]);
    stop();
    class Broken {
      static permission: NotificationPermission = 'granted';
      onclick: ((ev: Event) => unknown) | null = null;
      constructor() { throw new TypeError('no notifications here'); }
      close(): void {}
    }
    const again = startNotifier({ client: h.fake, Notification: Broken, visible: () => true, onScreen: () => null, focus: h.focus, open: h.open });
    expect(() => h.fake.set('notices', { nextId: 2, items: [notice({ id: 1, kind: 'mention' })] })).not.toThrow();
    again();
  });
  it('shows a mention as the name it stands for, never the raw token (web-2a card 48)', () => {
    const h = harness();
    h.fake.set('account', account());
    // roleIds on MemberSummary (L-TS-36, published by task 7; ruled: web-e2e 6).
    h.fake.set(`members:${A}`, [
      { userId: ME.id, username: 'ada', display: 'Ada L', kind: 0, roleIds: [] },
      { userId: '28'.repeat(16), username: 'bob', display: '', kind: 0, roleIds: [] },
    ]);
    h.fake.set('notices', { nextId: 0, items: [] });
    const stop = h.start();
    h.fake.set('notices', { nextId: 2, items: [
      notice({ id: 0, kind: 'mention', body: `<@${ME.id}> look, <@${'28'.repeat(16)}> and <@everyone>` }),
      dmNotice({ id: 1, body: `ping <@${ME.id}> <@${'ee'.repeat(16)}>` }),
    ] });
    expect(Note.made.map(n => n.options.body)).toEqual(['@Ada L look, @bob and @everyone', 'ping @Ada L @eeeeeeee']);
    expect(Note.made.some(n => String(n.options.body).includes('<@'))).toBe(false);
    stop();
  });
});

describe('ringNotification (flow 06, R1, R8)', () => {
  const ring = (over: Partial<Ring> = {}): Ring => ({ callId: DM, channelId: DM, callerUser: '28'.repeat(16), callerName: 'bob',
    ringTs: 1_900_000_030, expiresAt: 0, busy: false, quiet: false, ...over });
  function ctor(permission: NotificationPermission) {
    const made: { title: string; options: NotificationOptions; closed: boolean; onclick: ((ev: Event) => unknown) | null }[] = [];
    const requestPermission = vi.fn();
    class N {
      static readonly permission = permission;
      static readonly requestPermission = requestPermission;
      closed = false;
      onclick: ((ev: Event) => unknown) | null = null;
      constructor(readonly title: string, readonly options: NotificationOptions) { made.push(this); }
      close() { this.closed = true; }
    }
    return { Ctor: N as unknown as NotificationCtor, made, requestPermission };
  }
  const deps = (Ctor: NotificationCtor | undefined, settings: Record<string, string> = {}) =>
    ({ Notification: Ctor, settings, kind: 3 as const, visible: false, presenceChoice: 'online', focus: vi.fn(), open: vi.fn() });

  it('raises one tagged notification for a ring that is neither busy nor quiet, with permission granted', () => {
    const { Ctor, made } = ctor('granted');
    const d = deps(Ctor);
    const handle = ringNotification(ring(), d);
    expect(made).toHaveLength(1);
    expect(made[0]).toMatchObject({ title: 'bob is calling', options: { body: 'Open dilla to answer.', tag: `dilla-ring-${DM}` } });
    made[0].onclick?.(new Event('click'));
    expect(d.focus).toHaveBeenCalled();
    expect(d.open).toHaveBeenCalledWith(DM);
    expect(made[0].closed).toBe(true);
    handle?.close();
  });
  it('waits for the saved status before notifying about a ring', () => {
    const { Ctor, made } = ctor('granted');
    expect(ringNotification(ring(), { ...deps(Ctor), presenceChoice: undefined })).toBeNull();
    expect(made).toHaveLength(0);
  });
  it('raises nothing for a busy ring, a quiet (muted) ring or a DM set to nothing', () => {
    for (const [r, settings] of [[ring({ busy: true }), {}], [ring({ quiet: true }), {}], [ring(), { [`notify.channel.${DM}`]: 'nothing' }]] as const) {
      const { Ctor, made } = ctor('granted');
      expect(ringNotification(r, deps(Ctor, settings))).toBeNull();
      expect(made).toEqual([]);
    }
  });
  it('raises nothing for a ring in Do Not Disturb', () => {
    const { Ctor, made } = ctor('granted');
    expect(ringNotification(ring(), { ...deps(Ctor), presenceChoice: 'dnd' })).toBeNull();
    expect(made).toEqual([]);
  });
  it('raises nothing for a ring in a muted or silenced server', () => {
    for (const settings of [{ [`mute.community.${A}`]: '1' }, { [`notify.community.${A}`]: 'nothing' }]) {
      const { Ctor, made } = ctor('granted');
      expect(ringNotification(ring(), { ...deps(Ctor, settings), communityId: A })).toBeNull();
      expect(made).toEqual([]);
    }
  });
  it('raises nothing while the page is visible (flow 06: the banner covers a visible tab)', () => {
    const { Ctor, made } = ctor('granted');
    expect(ringNotification(ring(), { ...deps(Ctor), visible: true })).toBeNull();
    expect(made).toEqual([]);
  });
  it('raises nothing without permission and never asks for it', () => {
    for (const permission of ['default', 'denied'] as const) {
      const { Ctor, made, requestPermission } = ctor(permission);
      expect(ringNotification(ring(), deps(Ctor))).toBeNull();
      expect(made).toEqual([]);
      expect(requestPermission).not.toHaveBeenCalled();
    }
    expect(ringNotification(ring(), deps(undefined))).toBeNull();
  });
});
