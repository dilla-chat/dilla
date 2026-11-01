import { describe, expect, it, vi } from 'vitest';
import type { ChannelSummary, DmSummary } from '@dilla/client-core';
import { actionsProvider, channelsProvider, messagesProvider, paletteGroups, peopleProvider, settingsProvider } from './palette.ts';

const ch = (id: string, communityId: string, name: string, kind: 0 | 1 | 2 = 0): ChannelSummary => ({ id, communityId, kind, mode: 0, name, topic: '', parentId: null, position: 0, slowmode: 0, announcement: false, group: 'active' });
const communities = [{ id: 'a', name: 'Midgard Crew', owner: 'o' }, { id: 'b', name: 'Valhalla Kitchen', owner: 'o' }];
const channels = [ch('g', 'a', 'general'), ch('v', 'a', 'harbour', 1), ch('cat', 'a', 'raids', 2), ch('afk', 'a', 'AFK', 1), ch('r', 'b', 'recipes')];
const dms: DmSummary[] = [{ id: 'd1', kind: 3, members: ['me', 'ada'], name: 'Ada', group: 'active' }, { id: 'd2', kind: 4, members: ['me', 'a', 'b'], name: 'raid crew', group: 'active' }];

describe('palette providers (task 9 fix round 1)', () => {
  const open = vi.fn(() => true);
  const providers = [
    channelsProvider(communities, channels, new Set(['afk']), open),
    peopleProvider(dms, d => (d.kind === 4 ? 'group DM' : '@ada'), open),
    messagesProvider([
      { id: '1', snippet: 'elsewhere rope', location: '#loot', current: false, run: () => Promise.resolve(true) },
      { id: '2', snippet: 'here rope', location: '#general', author: 'Ada', time: '20:31', current: true, run: () => Promise.resolve(true) },
    ]),
    actionsProvider([{ id: 'settings', label: 'Open settings', icon: 'settings', keywords: 'profile', run: () => true }]),
  ];
  it('lists channels (no categories, no AFK), people and actions with nothing typed, in the prototype order, and no messages', () => {
    const groups = paletteGroups(providers, '');
    expect(groups.map(g => g.id)).toEqual(['channels', 'people', 'actions']);
    expect(groups[0].items.map(i => [i.label, i.sub, i.icon])).toEqual([['general', 'Midgard Crew', 'hash'], ['harbour', 'Midgard Crew · voice', 'volume-2'], ['recipes', 'Valhalla Kitchen', 'hash']]);
    expect(groups[1].items.map(i => i.sub)).toEqual(['@ada', 'group DM']);
  });
  it('filters by name, server and keywords, and puts the open conversation\'s messages first', () => {
    const groups = paletteGroups(providers, 'Rope');
    expect(groups.map(g => g.id)).toEqual(['messages']);
    expect(groups[0].items.map(i => [i.label, i.sub])).toEqual([['here rope', '#general · Ada · 20:31'], ['elsewhere rope', '#loot']]);
    expect(paletteGroups(providers, 'valhalla')[0].items.map(i => i.label)).toEqual(['recipes']);
    expect(paletteGroups(providers, 'profile').map(g => g.id).filter(id => id !== 'messages')).toEqual(['actions']);
  });
  it('a channel result runs its opener', () => {
    const item = paletteGroups(providers, 'general')[0].items[0];
    expect(item.run()).toBe(true);
    expect(open).toHaveBeenCalledWith(channels[0]);
  });
});

describe('settings provider (merge of lanes 9 and 10)', () => {
  const run = vi.fn(() => true);
  const rows = Array.from({ length: 10 }, (_, i) => ({ id: `user|s${i}|`, label: `Voice ${i}`, breadcrumb: 'You / Voice', run }));
  const search = vi.fn((q: string) => rows.filter(r => r.label.toLowerCase().includes(q)));
  it('says nothing with nothing typed', () => {
    expect(paletteGroups([settingsProvider(search)], '')).toEqual([]);
    expect(search).not.toHaveBeenCalled();
  });
  it('lists up to 8 matches as "Settings" with the breadcrumb under each, and a pick runs its opener', () => {
    const [group] = paletteGroups([settingsProvider(search)], 'Voice');
    expect(group.label).toBe('Settings');
    expect(group.items).toHaveLength(8);
    expect(group.items[0]).toMatchObject({ id: 'set:user|s0|', label: 'Voice 0', sub: 'You / Voice', icon: 'settings' });
    expect(group.items[0].run()).toBe(true);
    expect(run).toHaveBeenCalled();
  });
});
