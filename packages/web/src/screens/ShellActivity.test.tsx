// FX-core (final review I1): an Activity row of a thread reply opens its thread on the phone, not only its channel.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { ChannelSummary, Notice, TimelineItem } from '@dilla/client-core';
import { CoreProvider } from '../core/context.tsx';
import { FakeClient } from '../test/fake-client.ts';
import { account, timelineItem } from '../test/fixtures.ts';
import { PHONE_QUERY } from '../layout.ts';
import { en } from '../strings/index.ts';
import { Shell } from './Shell.tsx';

const A = 'a1'.repeat(16);
const GEN = 'c3'.repeat(16);
const BOB = '28'.repeat(16);
const ROOT = '71'.repeat(16);
const ch = (id: string, name: string): ChannelSummary =>
  ({ id, communityId: A, kind: 0, mode: 0, name, topic: '', parentId: null, position: 0, slowmode: 0, announcement: false, group: 'none' });
const rootRow = (): TimelineItem =>
  timelineItem({ key: 's1', msgId: ROOT, seq: '1', body: 'the root', senderUser: BOB, senderDevice: BOB, ts: 1_700_000_000, thread: { count: 1, lastTs: 1_700_000_100 } });
const notice = (threadRoot: string | null): Notice => ({ id: 1, channelId: GEN, communityId: A, kind: 'mention', senderUser: BOB,
  senderName: 'bob', body: 'look here', ts: Math.floor(Date.now() / 1000) - 60, threadRoot });

const WIDTH = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'offsetWidth');
afterEach(() => {
  if (WIDTH !== undefined) Object.defineProperty(HTMLElement.prototype, 'offsetWidth', WIDTH);
  Reflect.deleteProperty(window, 'matchMedia');
  // eslint-disable-next-line no-restricted-properties -- clears the tab memory the shell writes
  window.sessionStorage.clear();
  vi.restoreAllMocks();
});

function setup(threadRoot: string | null) {
  Object.defineProperty(HTMLElement.prototype, 'offsetWidth', { configurable: true, get: () => 390 });
  window.matchMedia = vi.fn((query: string) => ({
    matches: query === PHONE_QUERY, media: query, addEventListener: () => undefined, removeEventListener: () => undefined,
  })) as unknown as typeof window.matchMedia;
  window.history.replaceState(null, '', '/activity');
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set('connection', { status: 'online', generation: '7' });
  fake.set('communities', [{ id: A, name: 'Midgard', owner: 'ee'.repeat(16) }]);
  fake.set(`channels:${A}`, [ch(GEN, 'general')]);
  fake.set(`members:${A}`, []);
  fake.set('removals', []);
  fake.set('dms', []);
  fake.set('badges', { [GEN]: { unread: 1, mentions: 1 } });
  fake.set('notices', { nextId: 2, items: [notice(threadRoot)] });
  fake.set(`timeline:${GEN}`, { channelId: GEN, group: 'active', hasEarlier: false, items: [rootRow()] });
  fake.set(`thread:${GEN}`, { channelId: GEN, rootMsgId: ROOT, root: rootRow(), items: [], hasEarlier: false });
  const user = userEvent.setup();
  render(<div className="d-root"><CoreProvider client={fake}><Shell /></CoreProvider></div>);
  return { fake, user };
}

describe('Activity on a phone shell', () => {
  it('a thread reply’s row opens its channel and the thread', async () => {
    const { fake, user } = setup(ROOT);
    await user.click(await screen.findByRole('button', { name: /mentioned you/ }));
    expect(window.location.pathname).toBe(`/c/${A}/${GEN}`);
    expect(await screen.findByRole('complementary', { name: en['thread.title'] })).toBeInTheDocument();
    expect(fake.callsOf('openThread')).toEqual([{ m: 'openThread', channelId: GEN, rootMsgId: ROOT }]);
  });

  it('a channel message’s row opens its channel only', async () => {
    const { fake, user } = setup(null);
    await user.click(await screen.findByRole('button', { name: /mentioned you/ }));
    expect(window.location.pathname).toBe(`/c/${A}/${GEN}`);
    await screen.findByText('the root');
    expect(screen.queryByRole('complementary', { name: en['thread.title'] })).toBeNull();
    expect(fake.callsOf('openThread')).toEqual([]);
  });
});
