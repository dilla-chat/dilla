import { act, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { A, DM, MEMBERS, PEER, renderConversation, stubBlobUrls } from '../../test/conversation.tsx';
import { account, ME } from '../../test/fixtures.ts';
import { replaceHistoryState } from '../../router.ts';

const CONSENT = `privacy.dmGifs.${'aa'.repeat(16)}.${ME.id}.${'cc'.repeat(16)}`;
const B = 'b2'.repeat(16);
const gifButton = () => screen.getByRole('button', { name: 'add a gif' });
const notice = () => screen.getByRole('dialog', { name: 'GIFs in direct messages' });
const picker = () => screen.queryByRole('dialog', { name: 'gifs' });
const gifCalls = (calls: readonly { m: string }[]) => calls.filter(c => ['gifStatus', 'gifSearch', 'gifFetch'].includes(c.m));

function setup(opts: { consent?: string; pendingFull?: boolean; status?: boolean } = {}) {
  const blobs = stubBlobUrls();
  let core: ReturnType<typeof renderConversation>['fake'];
  let release: (result: unknown) => void = () => {};
  const result = renderConversation({ dm: true, before: f => {
    core = f;
    f.set('settings', opts.consent === undefined ? {} : { [CONSENT]: opts.consent });
  }, handler: async c => {
    if (c.m === 'setSetting') {
      const next = { ...core.get('settings') };
      if (c.value === null) delete next[c.key]; else next[c.key] = c.value;
      core.set('settings', next);
    }
    if (c.m === 'gifStatus') return { on: opts.status !== false };
    if (c.m === 'gifSearch') return { items: [{ id: 'shield', title: 'a shield', width: 100, height: 100 }], next: null };
    if (c.m === 'gifFetch') {
      if (c.variant === 'full' && opts.pendingFull) return new Promise(resolve => { release = resolve; });
      return { blob: new Blob(['GIF89a'], { type: 'image/gif' }) };
    }
    if (c.m === 'attachFiles') return { trayIds: ['gif'] };
    return null;
  } });
  return { ...result, release: () => release({ blob: new Blob(['GIF89a'], { type: 'image/gif' }) }), restore: () => blobs.restore() };
}

// Missing guards at either the source resolver or the picker boundary disclose DM activity before the notice.
describe('DM GIF consent', () => {
  it('does no GIF networking on DM open, and pressing GIF calls only status before consent', async () => {
    const r = setup();
    try {
      await screen.findByRole('textbox', { name: /^message / });
      expect(gifCalls(r.fake.calls)).toEqual([]);
      await r.user.click(gifButton());
      expect(notice()).toHaveTextContent('GIF searches and picks in direct messages go through Midgard and Giphy.');
      expect(gifCalls(r.fake.calls)).toEqual([{ m: 'gifStatus', communityId: A }]);
      expect(picker()).toBeNull();
      expect(document.querySelector('.d-gif-picker img')).toBeNull();
      await r.user.click(within(notice()).getByRole('button', { name: 'Not now' }));
      expect(picker()).toBeNull();
      expect(r.fake.callsOf('setSetting')).toEqual([]);
      await r.user.click(gifButton());
      expect(notice()).toBeInTheDocument();
      expect(r.fake.callsOf('gifSearch')).toEqual([]);
    } finally { r.restore(); }
  });

  it('Use GIFs persists local account/device consent and enables trending, previews and picking', async () => {
    const r = setup();
    try {
      await r.user.click(gifButton());
      await r.user.click(within(notice()).getByRole('button', { name: 'Use GIFs' }));
      await waitFor(() => expect(document.querySelector('.d-gif-picker img')).not.toBeNull());
      expect(r.fake.get('settings')).toEqual({ [CONSENT]: A });
      expect(r.fake.callsOf('gifSearch')).toEqual([{ m: 'gifSearch', communityId: A, q: '', offset: 0 }]);
      expect(r.fake.callsOf('gifFetch')).toEqual([{ m: 'gifFetch', communityId: A, gifId: 'shield', variant: 'preview' }]);
      await r.user.click(screen.getByRole('button', { name: 'a shield' }));
      await waitFor(() => expect(r.fake.callsOf('attachFiles')).toHaveLength(1));
      expect(r.fake.callsOf('attachFiles')[0]).toMatchObject({ channelId: DM });
      await r.user.click(gifButton());
      expect(screen.queryByRole('dialog', { name: 'GIFs in direct messages' })).toBeNull();
      expect(picker()).not.toBeNull();
    } finally { r.restore(); }
  });

  it('revoking in Privacy prevents further searches and discards an in-flight DM pick', async () => {
    const r = setup({ consent: A, pendingFull: true });
    try {
      expect(gifCalls(r.fake.calls)).toEqual([]);
      await r.user.click(gifButton());
      await screen.findByRole('button', { name: 'a shield' });
      await r.user.click(screen.getByRole('button', { name: 'a shield' }));
      await waitFor(() => expect(r.fake.callsOf('gifFetch').some(c => c.variant === 'full')).toBe(true));
      await act(async () => { replaceHistoryState(null, '', `/settings/privacy?from=${encodeURIComponent(`/dm/${DM}`)}`); await Promise.resolve(); });
      const toggle = await screen.findByRole('switch', { name: 'GIFs in direct messages' });
      expect(toggle).toHaveAttribute('aria-checked', 'true');
      await r.user.click(toggle);
      expect(toggle).toHaveAttribute('aria-checked', 'false');
      const count = gifCalls(r.fake.calls).length;
      await act(async () => { r.release(); await Promise.resolve(); });
      expect(r.fake.callsOf('attachFiles')).toEqual([]);
      expect(gifCalls(r.fake.calls)).toHaveLength(count);
      await act(async () => { replaceHistoryState(null, '', `/dm/${DM}`); await Promise.resolve(); });
      expect(gifCalls(r.fake.calls)).toHaveLength(count);
      await r.user.click(gifButton());
      expect(notice()).toBeInTheDocument();
      expect(picker()).toBeNull();
    } finally { r.restore(); }
  });

  it('a settings push revokes an open picker and discards its pending full GIF', async () => {
    const r = setup({ consent: A, pendingFull: true });
    try {
      await r.user.click(gifButton());
      await r.user.click(await screen.findByRole('button', { name: 'a shield' }));
      await waitFor(() => expect(r.fake.callsOf('gifFetch').some(c => c.variant === 'full')).toBe(true));
      r.fake.publish('settings', {});
      expect(picker()).toBeNull();
      const count = gifCalls(r.fake.calls).length;
      await act(async () => { r.release(); await Promise.resolve(); });
      expect(r.fake.callsOf('attachFiles')).toEqual([]);
      expect(gifCalls(r.fake.calls)).toHaveLength(count);
    } finally { r.restore(); }
  });

  it('enabling in Privacy still requires the named-server notice before any search', async () => {
    const r = setup();
    try {
      await act(async () => { replaceHistoryState(null, '', `/settings/privacy?from=${encodeURIComponent(`/dm/${DM}`)}`); await Promise.resolve(); });
      const toggle = await screen.findByRole('switch', { name: 'GIFs in direct messages' });
      expect(toggle).toHaveAttribute('aria-checked', 'false');
      await r.user.click(toggle);
      expect(toggle).toHaveAttribute('aria-checked', 'true');
      expect(r.fake.get('settings')).toEqual({ [CONSENT]: 'on' });
      await act(async () => { replaceHistoryState(null, '', `/dm/${DM}`); await Promise.resolve(); });
      expect(gifCalls(r.fake.calls)).toEqual([]);
      await r.user.click(gifButton());
      expect(notice()).toBeInTheDocument();
      expect(r.fake.callsOf('gifSearch')).toEqual([]);
      await r.user.click(within(notice()).getByRole('button', { name: 'Use GIFs' }));
      await waitFor(() => expect(r.fake.callsOf('gifSearch')).toHaveLength(1));
    } finally { r.restore(); }
  });

  it('cannot dismiss the notice while its local consent save is pending', async () => {
    const r = setup();
    let finish = () => {};
    try {
      const original = r.fake.handler;
      r.fake.handler = c => c.m === 'setSetting' ? new Promise(resolve => {
        finish = () => { void original(c).then(resolve); };
      }) : original(c);
      await r.user.click(gifButton());
      await r.user.click(within(notice()).getByRole('button', { name: 'Use GIFs' }));
      expect(within(notice()).getByRole('button', { name: 'Not now' })).toBeDisabled();
      notice().focus();
      await r.user.keyboard('{Escape}');
      expect(notice()).toBeInTheDocument();
      expect(r.fake.callsOf('gifSearch')).toEqual([]);
      await act(async () => { finish(); await Promise.resolve(); });
      await waitFor(() => expect(picker()).not.toBeNull());
    } finally { r.restore(); }
  });

  it('does not load GIFs when persisting consent fails', async () => {
    const r = setup();
    try {
      const original = r.fake.handler;
      r.fake.handler = c => c.m === 'setSetting' ? Promise.reject(new Error('storage unavailable')) : original(c);
      await r.user.click(gifButton());
      await r.user.click(within(notice()).getByRole('button', { name: 'Use GIFs' }));
      expect(await within(notice()).findByRole('alert')).toBeInTheDocument();
      expect(picker()).toBeNull();
      expect(r.fake.callsOf('gifSearch')).toEqual([]);
      expect(r.fake.callsOf('gifFetch')).toEqual([]);
    } finally { r.restore(); }
  });

  it('names the newly selected proxy and asks again when the previous server loses its key', async () => {
    const r = setup({ consent: A });
    try {
      r.fake.publish('communities', [...r.fake.get('communities')!, { id: B, name: 'Asgard', owner: ME.id }]);
      r.fake.publish(`members:${B}`, MEMBERS);
      const original = r.fake.handler;
      r.fake.handler = c => c.m === 'gifStatus' ? Promise.resolve({ on: c.communityId === B }) : original(c);
      await r.user.click(gifButton());
      expect(notice()).toHaveTextContent('through Asgard and Giphy');
      expect(r.fake.callsOf('gifStatus')).toEqual([{ m: 'gifStatus', communityId: A }, { m: 'gifStatus', communityId: B }]);
      expect(r.fake.callsOf('gifSearch')).toEqual([]);
      expect(r.fake.callsOf('gifFetch')).toEqual([]);
      await r.user.click(within(notice()).getByRole('button', { name: 'Use GIFs' }));
      await waitFor(() => expect(document.querySelector('.d-gif-picker img')).not.toBeNull());
      expect(r.fake.get('settings')).toEqual({ [CONSENT]: B });
      expect(r.fake.callsOf('gifSearch')).toEqual([{ m: 'gifSearch', communityId: B, q: '', offset: 0 }]);
      expect(r.fake.callsOf('gifFetch').every(call => call.communityId === B)).toBe(true);
    } finally { r.restore(); }
  });

  it('leaves the picker closed and explains when no shared server has a key', async () => {
    const r = setup({ status: false });
    try {
      await r.user.click(gifButton());
      expect(await screen.findByText("GIFs aren't available here")).toBeInTheDocument();
      expect(gifButton()).toHaveAttribute('aria-expanded', 'false');
      expect(screen.queryByRole('dialog')).toBeNull();
      expect(gifCalls(r.fake.calls)).toEqual([{ m: 'gifStatus', communityId: A }]);
    } finally { r.restore(); }
  });

  it.each(['account', 'device'])('does not reuse consent from a different %s', async kind => {
    const r = setup({ consent: A });
    try {
      const current = account();
      r.fake.publish('account', kind === 'account' ? { ...current, user: { id: PEER, username: 'bob' } } : { ...current, deviceId: 'dd'.repeat(16) });
      expect(gifCalls(r.fake.calls)).toEqual([]);
      await r.user.click(gifButton());
      expect(notice()).toBeInTheDocument();
      expect(r.fake.callsOf('gifSearch')).toEqual([]);
    } finally { r.restore(); }
  });
});
