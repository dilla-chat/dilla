import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, screen, waitFor, within } from '@testing-library/react';
import type { ChannelSummary } from '@dilla/client-core';
import { A, BOB, BOB_DEV, callState, OWN_DEV, refusal, renderVoiceShell } from '../test/fake-client.ts';
import { ME } from '../test/fixtures.ts';

// Web3.8 (WS5, item 12): one channel list. The voice channels stand in position among the text channels, with their people
// nested under them; there is no "active voice" and no "voice" heading.
const TA = '51'.repeat(16);
const VV = '52'.repeat(16);
const TB = '53'.repeat(16);
const VW = '54'.repeat(16);
const ch = (id: string, name: string, kind: 0 | 1 | 2, position: number): ChannelSummary => ({
  id, communityId: A, kind, mode: 0, name, topic: '', parentId: null, position, slowmode: 0, announcement: false,
  group: kind === 1 ? 'unsupported' : 'active' });
const CHANNELS = [ch(TA, 'alpha', 0, 0), ch(VV, 'lounge', 1, 1), ch(TB, 'beta', 0, 2), ch(VW, 'studio', 1, 3)];

afterEach(() => { vi.restoreAllMocks(); });

function mount() {
  const app = renderVoiceShell({ path: `/c/${A}/${TA}`, voice: {
    [VV]: { callId: VV, participants: [
      { userId: ME.id, deviceId: OWN_DEV, name: 'ada', flags: 0, own: true, web: true, bot: false },
      { userId: BOB, deviceId: BOB_DEV, name: 'bob', flags: 1, own: false, web: true, bot: false },
    ] } } });
  act(() => app.client.publish(`channels:${A}`, CHANNELS));
  return app;
}
const list = () => screen.getByRole('navigation', { name: 'channels' });

describe('one channel list (web3.8 item 12)', () => {
  it('lists text and voice channels in position order, in one list', async () => {
    mount();
    await within(list()).findByRole('button', { name: 'voice lounge' });
    const text = list().textContent ?? '';
    const order = ['alpha', 'lounge', 'beta', 'studio'].map(n => text.indexOf(n));
    expect(order.every(i => i >= 0)).toBe(true);
    expect(order).toEqual([...order].sort((a, b) => a - b));
    expect(list().querySelectorAll('ul[role="list"]:not(.d-voice-row__people)').length).toBe(1);
  });

  it('puts the server\'s AFK channel last, below every other row, whatever its position (MP6)', async () => {
    const app = mount();
    act(() => app.client.publish(`afk:${A}`, { channelId: VV, timeoutSeconds: 300, pending: false }));
    await within(list()).findByRole('button', { name: 'voice lounge' });
    const text = list().textContent ?? '';
    const order = ['alpha', 'beta', 'studio', 'lounge'].map(n => text.indexOf(n));
    expect(order).toEqual([...order].sort((a, b) => a - b));
    expect(order.every(i => i >= 0)).toBe(true);
  });

  it('opens the AFK page from its row without joining; only Go AFK joins (wave B task 4)', async () => {
    const app = mount();
    act(() => app.client.publish(`afk:${A}`, { channelId: VV, timeoutSeconds: 300, pending: false }));
    const row = await within(list()).findByRole('button', { name: 'voice lounge' });
    await app.user.click(row);
    expect(await screen.findByText('Nobody is away')).toBeInTheDocument();
    expect(app.client.callsOf('afkAway')).toEqual([]);
    expect(app.sessions).toHaveLength(0);
    await app.user.click(screen.getByRole('button', { name: 'Go AFK' }));
    await waitFor(() => expect(app.client.callsOf('afkAway')).toHaveLength(1));
    expect(app.sessions).toHaveLength(0);
  });

  it('has no "active voice" and no "voice" heading, and gives the voice row its speaker icon', async () => {
    mount();
    const lounge = await within(list()).findByRole('button', { name: 'voice lounge' });
    expect(screen.queryByRole('heading', { name: 'active voice' })).toBeNull();
    expect(screen.queryByRole('heading', { name: 'voice' })).toBeNull();
    expect(lounge.querySelector('[data-icon="volume-2"]')).not.toBeNull();
    expect(document.querySelector('.dw-voice-section, .d-active-voice')).toBeNull();
  });

  it('nests the people under their voice row, speaking from the call', async () => {
    const app = mount();
    const lounge = await within(list()).findByRole('button', { name: 'voice lounge' });
    const item = lounge.closest('li')!;
    const names = [...item.querySelectorAll('.d-voice-row__person-name')].map(n => n.textContent);
    expect(names).toEqual(['ada', 'bob']);
    expect(item.querySelector('[data-speaking="true"]')).toBeNull();
    await app.user.click(lounge);
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    app.client.publish('call', callState({ phase: 'in', channelId: VV, callId: VV }));
    act(() => app.session.update({ speaking: true }));
    await waitFor(() => expect(item.querySelector('[data-speaking="true"] .d-voice-row__person-name')).toHaveTextContent('ada'));
  });

  it('shows no unread badge on a voice row', async () => {
    const app = mount();
    act(() => app.client.publish('badges', { [VV]: { unread: 4, mentions: 0 }, [TB]: { unread: 2, mentions: 0 } }));
    const lounge = await within(list()).findByRole('button', { name: 'voice lounge' });
    expect(lounge).toHaveAccessibleName('voice lounge');
    expect(lounge.closest('li')!.querySelector('.d-pill')).toBeNull();
    // The control: the text row beside it does show its count.
    expect(within(list()).getByRole('button', { name: /^beta/ })).toHaveAccessibleName(/2/);
  });

  it('moves with ArrowDown from the text row to the voice row to the next text row', async () => {
    const app = mount();
    await within(list()).findByRole('button', { name: 'voice lounge' });
    within(list()).getByRole('button', { name: /^alpha/ }).focus();
    await app.user.keyboard('{ArrowDown}');
    expect(document.activeElement).toBe(within(list()).getByRole('button', { name: 'voice lounge' }));
    await app.user.keyboard('{ArrowDown}');
    expect(document.activeElement).toBe(within(list()).getByRole('button', { name: /^beta/ }));
  });

  it('opens a voice room from its join button', async () => {
    const app = mount();
    await within(list()).findByRole('button', { name: 'voice lounge' });
    await app.user.click(within(list()).getAllByRole('button', { name: 'join voice' })[0]);
    await waitFor(() => expect(app.log).toContain(`callJoin:${VV}`));
    expect(window.location.pathname).toBe(`/c/${A}/${VV}`);
  });
});

// web3.9 (WEB-LIST): categories that fold, a "+" that moves with them, one-click join.
const CA = '61'.repeat(16);
const CB = '62'.repeat(16);
const RP = '63'.repeat(16);
const RL = '64'.repeat(16);
const RV = '65'.repeat(16);
const LC = '66'.repeat(16);
const kid = (id: string, name: string, kind: 0 | 1, position: number, parentId: string): ChannelSummary => ({ ...ch(id, name, kind, position), parentId });
const CATEGORISED = [ch(TA, 'alpha', 0, 0), ch(CA, 'raids', 2, 10), kid(RP, 'plans', 0, 0, CA), kid(RL, 'loot', 0, 1, CA),
  kid(RV, 'war room', 1, 2, CA), ch(CB, 'lounge', 2, 20), kid(LC, 'chat', 0, 0, CB)];
const FOLDED = `channels.collapsed.${CA}`;
function mountCategories(settings: Record<string, string> = {}, path = `/c/${A}/${RP}`) {
  const app = renderVoiceShell({ path: `/c/${A}/${TA}`, settings });
  act(() => app.client.publish(`channels:${A}`, CATEGORISED));
  // The route follows the list, which the harness seeds without these channels.
  act(() => { window.history.pushState(null, '', path); window.dispatchEvent(new PopStateEvent('popstate')); });
  return app;
}
const manage = (app: ReturnType<typeof mount>) => act(() => app.client.publish(`permissions:${A}`, { loaded: true, manageChannels: true } as never));
const head = (name: string) => within(list()).getByRole('button', { name });
const has = (name: string | RegExp) => within(list()).queryByRole('button', { name }) !== null;

describe('categories (web3.9)', () => {
  it('folding a category writes channels.collapsed.<id> = 1 and keeps its active and unread rows', async () => {
    const app = mountCategories();
    act(() => app.client.publish('badges', { [RL]: { unread: 3, mentions: 0 } }));
    await within(list()).findByRole('button', { name: 'raids' });
    expect(head('raids')).toHaveAttribute('aria-expanded', 'true');
    expect(has('voice war room')).toBe(true);
    await app.user.click(head('raids'));
    expect(app.client.callsOf('setSetting')).toEqual([{ m: 'setSetting', key: FOLDED, value: '1' }]);
    act(() => app.client.publish('settings', { [FOLDED]: '1' }));
    expect(head('raids')).toHaveAttribute('aria-expanded', 'false');
    expect(has(/^plans/)).toBe(true);
    expect(has(/^loot/)).toBe(true);
    expect(has('voice war room')).toBe(false);
    expect(has(/^chat/)).toBe(true);
  });

  it('unfolding writes null', async () => {
    const app = mountCategories({ [FOLDED]: '1' });
    expect(await within(list()).findByRole('button', { name: 'raids' })).toHaveAttribute('aria-expanded', 'false');
    await app.user.click(head('raids'));
    expect(app.client.callsOf('setSetting')).toEqual([{ m: 'setSetting', key: FOLDED, value: null }]);
  });

  it('a remount reads the setting back', async () => {
    mountCategories({ [FOLDED]: '1' });
    expect(await within(list()).findByRole('button', { name: 'raids' })).toHaveAttribute('aria-expanded', 'false');
    cleanup();
    mountCategories({ [FOLDED]: '1' });
    expect(await within(list()).findByRole('button', { name: 'raids' })).toHaveAttribute('aria-expanded', 'false');
    expect(has('voice war room')).toBe(false);
  });

  it("a manager's category \"+\" opens the create flow with that parent", async () => {
    const app = mountCategories();
    app.client.handler = c => Promise.resolve(c.m === 'createChannel' ? { channelId: '77'.repeat(16) } : null);
    await within(list()).findByRole('button', { name: 'raids' });
    expect(screen.queryByRole('button', { name: 'new channel in raids' })).toBeNull();
    manage(app);
    await app.user.click(screen.getByRole('button', { name: 'new channel in raids' }));
    await app.user.type(await screen.findByRole('textbox', { name: 'Channel name' }), 'Bosses{Enter}');
    await waitFor(() => expect(app.client.callsOf('createChannel')).toHaveLength(1));
    expect(app.client.callsOf('createChannel')[0]).toMatchObject({ communityId: A, kind: 'text', name: 'bosses', parentId: CA });
    expect(app.client.callsOf('updateChannel')).toHaveLength(0);
  });

  it('a refused create under a category shows the existing create error and keeps the dialog', async () => {
    const app = mountCategories();
    app.client.handler = c => (c.m === 'createChannel' ? Promise.reject(refusal({ code: 'E_NOT_FOUND', status: 404 })) : Promise.resolve(null));
    manage(app);
    await app.user.click(screen.getByRole('button', { name: 'new channel in raids' }));
    await app.user.type(await screen.findByRole('textbox', { name: 'Channel name' }), 'Bosses{Enter}');
    expect(await screen.findByRole('alert')).toHaveTextContent('E_NOT_FOUND');
    expect(screen.getByRole('dialog', { name: /New channel/ })).toBeInTheDocument();
  });

  it('the new-channel "+" sits in the sidebar head with no categories, and moves to the category headers with them', async () => {
    const app = mount();
    manage(app);
    const heads = () => document.querySelector('.d-channel-list__head') as HTMLElement;
    await within(list()).findByRole('button', { name: 'voice lounge' });
    expect(within(heads()).getByRole('button', { name: 'new channel' })).toBeInTheDocument();
    act(() => app.client.publish(`channels:${A}`, CATEGORISED));
    expect(within(heads()).queryByRole('button', { name: 'new channel' })).toBeNull();
    expect(screen.getByRole('button', { name: 'new channel in lounge' })).toBeInTheDocument();
  });
});

describe('one-click voice join on desktop (web3.9)', () => {
  it('a click on a voice row opens it and joins', async () => {
    const app = mount();
    await app.user.click(await within(list()).findByRole('button', { name: 'voice studio' }));
    await waitFor(() => expect(app.log).toContain(`callJoin:${VW}`));
    expect(window.location.pathname).toBe(`/c/${A}/${VW}`);
  });

  it('moving keyboard focus over voice rows never joins; only activating one does', async () => {
    const app = mount();
    const studio = await within(list()).findByRole('button', { name: 'voice studio' });
    act(() => studio.focus());
    await app.user.keyboard('{ArrowUp}{ArrowDown}{ArrowUp}{ArrowDown}');
    await app.user.tab();
    await app.user.tab({ shift: true });
    expect(app.log.filter(l => l.startsWith('callJoin'))).toEqual([]);
    expect(app.client.callsOf('callJoin')).toHaveLength(0);
    act(() => within(list()).getByRole('button', { name: 'voice studio' }).focus());
    await app.user.keyboard('{Enter}');
    await waitFor(() => expect(app.log).toContain(`callJoin:${VW}`));
  });

  it('a click on the row of the voice channel already joined only opens it', async () => {
    const app = mount();
    await app.user.click(await within(list()).findByRole('button', { name: 'voice studio' }));
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    await app.user.click(within(list()).getByRole('button', { name: /^alpha/ }));
    await app.user.click(within(list()).getByRole('button', { name: 'voice studio' }));
    expect(app.log.filter(l => l === `callJoin:${VW}`)).toHaveLength(1);
  });
});
