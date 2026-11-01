import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { act, cleanup, screen, waitFor, within } from '@testing-library/react';
import type { Command } from '@dilla/client-core';
import { refusal } from '../../test/fake-client.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { A, GEN, renderConversation, stubBlobUrls } from '../../test/conversation.tsx';

const GIF_BYTES = new Uint8Array([0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0, 0]);
const gifBlob = () => new Blob([GIF_BYTES], { type: 'image/gif' });
const page = (ids: string[], next: number | null = null) => ({
  items: ids.map(id => ({ id, title: `title ${id}`, width: 200, height: 113 })), next,
});

let blobs: ReturnType<typeof stubBlobUrls>;
beforeEach(() => { blobs = stubBlobUrls(); });
afterEach(() => { cleanup(); blobs.restore(); });
const bytesOf = (f: Blob): Promise<Uint8Array> => new Promise((resolve, reject) => {
  const r = new FileReader();
  r.onload = () => { resolve(new Uint8Array(r.result as ArrayBuffer)); };
  r.onerror = () => { reject(new Error('read')); };
  r.readAsArrayBuffer(f);
});

/** The shell on a channel of a server whose Giphy status is `gifs` (default: a key is set); the button shows once the status is read. */
async function open(over: { gifs?: boolean; dm?: boolean; handler?: (c: Command) => Promise<unknown> } = {}) {
  const searches: { q: string; offset: number }[] = [];
  const base = over.handler ?? ((c: Command): Promise<unknown> => {
    if (c.m === 'gifSearch') {
      searches.push({ q: c.q, offset: c.offset });
      return Promise.resolve(page(c.q === 'cat' ? ['c1', 'c2'] : c.offset > 0 ? ['m1', 'm2', 'm3'] : ['t1', 't2', 't3'], c.offset === 0 && c.q === '' ? 3 : null));
    }
    if (c.m === 'gifFetch') return Promise.resolve({ blob: gifBlob() });
    if (c.m === 'attachFiles') return Promise.resolve({ trayIds: ['tray-1'] });
    return Promise.resolve(null);
  });
  const handler = (c: Command): Promise<unknown> =>
    c.m === 'gifStatus' ? Promise.resolve({ on: over.gifs ?? true, rating: 'pg-13' }) : base(c);
  const r = renderConversation({ handler, ...(over.dm === true ? { dm: true } : {}) });
  if ((over.gifs ?? true) && over.dm !== true) await screen.findByRole('button', { name: 'add a gif' });
  else await waitFor(() => expect(screen.getByRole('textbox', { name: /^message / })).toBeInTheDocument());
  return { ...r, searches };
}

const button = () => screen.getByRole('button', { name: 'add a gif' });
const dialog = () => screen.getByRole('dialog', { name: 'gifs' });
const cells = () => Array.from(document.querySelectorAll<HTMLButtonElement>('button[data-gif-id]'));

describe('the gif button (E8)', () => {
  it('is absent when the server has no Giphy key', async () => {
    await open({ gifs: false });
    expect(screen.queryByRole('button', { name: 'add a gif' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Attach files (25 MB each)' })).toBeInTheDocument();
  });

  it('asks the worker about this server, and about no other', async () => {
    const { fake } = await open();
    expect(fake.callsOf('gifStatus')).toEqual([{ m: 'gifStatus', communityId: A }]);
  });

  it('is absent in a direct message and asks nothing: there are no GIFs outside a server', async () => {
    const { fake } = await open({ dm: true });
    expect(screen.queryByRole('button', { name: 'add a gif' })).toBeNull();
    expect(fake.callsOf('gifStatus')).toEqual([]);
  });

  it('is absent when reading the status fails', async () => {
    const r = renderConversation({ handler: c => c.m === 'gifStatus' ? Promise.reject(refusal({ code: 'E_UNAVAILABLE', status: 503 })) : Promise.resolve(null) });
    await waitFor(() => expect(r.fake.callsOf('gifStatus')).toHaveLength(1));
    expect(screen.queryByRole('button', { name: 'add a gif' })).toBeNull();
  });

  it('is present, closed, when the server has a key; opening it asks for trending and focuses the search box', async () => {
    const { user, fake, searches } = await open();
    expect(button()).toHaveAttribute('aria-expanded', 'false');
    await user.click(button());
    expect(button()).toHaveAttribute('aria-expanded', 'true');
    expect(within(dialog()).getByRole('searchbox', { name: 'search gifs' })).toHaveFocus();
    await waitFor(() => expect(cells()).toHaveLength(3));
    expect(searches).toEqual([{ q: '', offset: 0 }]);
    expect(fake.callsOf('gifSearch')).toEqual([{ m: 'gifSearch', communityId: A, q: '', offset: 0 }]);
    expect(screen.getByText('Powered by GIPHY')).toBeInTheDocument();
    expect(cells().map(c => c.getAttribute('aria-label'))).toEqual(['title t1', 'title t2', 'title t3']);
  });
});

describe('previews and privacy', () => {
  it('shows every preview from a page-minted blob URL fetched through the worker, and never a giphy.com address', async () => {
    const { user, fake } = await open();
    await user.click(button());
    await waitFor(() => expect(document.querySelectorAll('.d-gif-picker img')).toHaveLength(3));
    const srcs = Array.from(document.querySelectorAll('.d-gif-picker img')).map(i => i.getAttribute('src'));
    expect(srcs.every(s => s !== null && s.startsWith('blob:test/'))).toBe(true);
    expect(fake.callsOf('gifFetch').map(c => [c.communityId, c.gifId, c.variant])).toEqual([[A, 't1', 'preview'], [A, 't2', 'preview'], [A, 't3', 'preview']]);
    expect(document.body.innerHTML).not.toContain('giphy.com');
  });

  it('revokes the preview URLs when the picker closes', async () => {
    const { user } = await open();
    await user.click(button());
    await waitFor(() => expect(blobs.created).toHaveLength(3));
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog', { name: 'gifs' })).toBeNull();
    await waitFor(() => expect([...blobs.revoked].sort()).toEqual([...blobs.created].sort()));
  });
});

describe('searching', () => {
  it('searches what is typed after a short pause, and shows only the newest answer', async () => {
    const { user, searches } = await open();
    await user.click(button());
    await waitFor(() => expect(cells()).toHaveLength(3));
    await user.type(within(dialog()).getByRole('searchbox'), 'cat');
    await waitFor(() => expect(cells().map(c => c.getAttribute('data-gif-id'))).toEqual(['c1', 'c2']));
    // One search for the whole word, not one per key.
    expect(searches.map(s => s.q)).toEqual(['', 'cat']);
  });

  it('ignores a slow answer to an older search', async () => {
    const pending = new Map<string, (v: unknown) => void>();
    const { user } = await open({
      handler: c => {
        if (c.m === 'gifSearch') return new Promise(resolve => { pending.set(c.q, resolve); });
        if (c.m === 'gifFetch') return Promise.resolve({ blob: gifBlob() });
        return Promise.resolve(null);
      },
    });
    await user.click(button());
    await waitFor(() => expect(pending.has('')).toBe(true));
    await user.type(within(dialog()).getByRole('searchbox'), 'cat');
    await waitFor(() => expect(pending.has('cat')).toBe(true));
    await act(async () => { pending.get('cat')?.(page(['c1'])); await Promise.resolve(); });
    await act(async () => { pending.get('')?.(page(['stale'])); await Promise.resolve(); });
    await waitFor(() => expect(cells().map(c => c.getAttribute('data-gif-id'))).toEqual(['c1']));
  });

  it('loads the next page under "more" and appends it', async () => {
    const { user, searches } = await open();
    await user.click(button());
    await waitFor(() => expect(cells()).toHaveLength(3));
    await user.click(within(dialog()).getByRole('button', { name: 'more' }));
    await waitFor(() => expect(cells()).toHaveLength(3 + 3));
    expect(searches).toEqual([{ q: '', offset: 0 }, { q: '', offset: 3 }]);
    expect(within(dialog()).queryByRole('button', { name: 'more' })).toBeNull();
  });

  it('says so when nothing matches', async () => {
    const { user } = await open({ handler: c => Promise.resolve(c.m === 'gifSearch' ? page([]) : null) });
    await user.click(button());
    await waitFor(() => expect(within(dialog()).getByRole('status')).toHaveTextContent('no gifs for that'));
  });

  it('says so when the instance refuses, and keeps the picker open', async () => {
    const { user } = await open({ handler: c => c.m === 'gifSearch' ? Promise.reject(refusal({ code: 'E_UNAVAILABLE', status: 503 })) : Promise.resolve(null) });
    await user.click(button());
    await waitFor(() => expect(within(dialog()).getByText('could not reach the gif service')).toBeInTheDocument());
    expect(dialog()).toBeInTheDocument();
  });
});

describe('picking', () => {
  it('downloads the full GIF through the worker and adds it to the tray as image/gif, without sending anything', async () => {
    const { user, fake } = await open();
    await user.click(button());
    await waitFor(() => expect(cells()).toHaveLength(3));
    await user.click(cells()[1]);
    await waitFor(() => expect(fake.callsOf('attachFiles')).toHaveLength(1));
    expect(fake.callsOf('gifFetch').filter(c => c.variant === 'full').map(c => c.gifId)).toEqual(['t2']);
    const [call] = fake.callsOf('attachFiles');
    expect(call).toMatchObject({ m: 'attachFiles', channelId: GEN });
    expect(call?.files).toHaveLength(1);
    const f = call.files[0];
    expect(f).toBeInstanceOf(File);
    expect(f.name).toBe('giphy-t2.gif');
    expect(f.type).toBe('image/gif');
    expect(await bytesOf(f)).toEqual(GIF_BYTES);
    expect(fake.callsOf('send')).toEqual([]);
    // The picker closes and the message box takes focus, so Enter sends the tray.
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'gifs' })).toBeNull());
    expect(screen.getByRole('textbox', { name: 'message #general' })).toHaveFocus();
  });

  it('keeps the picker open and says so when the download fails', async () => {
    const { user, fake } = await open({
      handler: c => {
        if (c.m === 'gifSearch') return Promise.resolve(page(['t1']));
        if (c.m === 'gifFetch') return c.variant === 'full' ? Promise.reject(refusal({ code: 'E_UNAVAILABLE', status: 503 })) : Promise.resolve({ blob: gifBlob() });
        return Promise.resolve(null);
      },
    });
    await user.click(button());
    await waitFor(() => expect(cells()).toHaveLength(1));
    await user.click(cells()[0]);
    await waitFor(() => expect(within(dialog()).getByText('could not add that gif')).toBeInTheDocument());
    expect(fake.callsOf('attachFiles')).toEqual([]);
  });

  it('ignores a second pick while one is downloading', async () => {
    let release: (v: unknown) => void = () => undefined;
    const { user, fake } = await open({
      handler: c => {
        if (c.m === 'gifSearch') return Promise.resolve(page(['t1', 't2']));
        if (c.m === 'gifFetch' && c.variant === 'full') return new Promise(resolve => { release = resolve; });
        if (c.m === 'gifFetch') return Promise.resolve({ blob: gifBlob() });
        if (c.m === 'attachFiles') return Promise.resolve({ trayIds: ['x'] });
        return Promise.resolve(null);
      },
    });
    await user.click(button());
    await waitFor(() => expect(cells()).toHaveLength(2));
    await user.click(cells()[0]);
    await user.click(cells()[1]);
    expect(fake.callsOf('gifFetch').filter(c => c.variant === 'full')).toHaveLength(1);
    await act(async () => { release({ blob: gifBlob() }); await Promise.resolve(); });
    await waitFor(() => expect(fake.callsOf('attachFiles')).toHaveLength(1));
  });
});

describe('keyboard and focus', () => {
  it('Escape closes the picker and returns focus to the gif button', async () => {
    const { user } = await open();
    await user.click(button());
    await waitFor(() => expect(cells()).toHaveLength(3));
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog', { name: 'gifs' })).toBeNull();
    expect(button()).toHaveFocus();
    expect(button()).toHaveAttribute('aria-expanded', 'false');
  });

  it('the button toggles: a second activation closes it', async () => {
    const { user } = await open();
    await user.click(button());
    expect(dialog()).toBeInTheDocument();
    await user.click(button());
    expect(screen.queryByRole('dialog', { name: 'gifs' })).toBeNull();
  });

  it('has no serious axe violations open', async () => {
    const { user } = await open();
    await user.click(button());
    await waitFor(() => expect(cells()).toHaveLength(3));
    await expectNoAxeViolations(document.body);
  });
});

describe('previews under pressure (review M1)', () => {
  it('keeps at most two previews in flight, matching the share the instance gives one member', async () => {
    let live = 0;
    let most = 0;
    const { user } = await open({
      handler: async c => {
        if (c.m === 'gifSearch') return page(['p1', 'p2', 'p3', 'p4', 'p5', 'p6']);
        if (c.m === 'gifFetch') {
          live += 1;
          most = Math.max(most, live);
          await new Promise(resolve => setTimeout(resolve, 20));
          live -= 1;
          return { blob: gifBlob() };
        }
        return null;
      },
    });
    await user.click(button());
    await waitFor(() => expect(document.querySelectorAll('.d-gif-picker img')).toHaveLength(6));
    expect(most).toBeLessThanOrEqual(2);
  });

  it('asks again, after a pause, for a preview the instance was too busy to give, and shows the others meanwhile', async () => {
    const tries = new Map<string, number>();
    const { user, fake } = await open({
      handler: c => {
        if (c.m === 'gifSearch') return Promise.resolve(page(['busy1', 'ok2']));
        if (c.m === 'gifFetch') {
          const n = (tries.get(c.gifId) ?? 0) + 1;
          tries.set(c.gifId, n);
          if (c.gifId === 'busy1' && n < 3) return Promise.reject(refusal({ code: 'E_UNAVAILABLE', status: 503, retryAfterMs: 1000 }));
          return Promise.resolve({ blob: gifBlob() });
        }
        return Promise.resolve(null);
      },
    });
    await user.click(button());
    // The ready one shows at once; the busy one fills in on a later try.
    await waitFor(() => expect(document.querySelector('[data-gif-id="ok2"] img')).not.toBeNull());
    expect(document.querySelector('[data-gif-id="busy1"] img')).toBeNull();
    await waitFor(() => expect(document.querySelector('[data-gif-id="busy1"] img')).not.toBeNull(), { timeout: 5000 });
    expect(tries.get('busy1')).toBe(3);
    expect(fake.callsOf('gifFetch').filter(c => c.gifId === 'ok2')).toHaveLength(1);
  });

  it('gives up on a preview that keeps failing, without blocking the rest', async () => {
    const { user } = await open({
      handler: c => {
        if (c.m === 'gifSearch') return Promise.resolve(page(['dead', 'ok2']));
        if (c.m === 'gifFetch') return c.gifId === 'dead' ? Promise.reject(refusal({ code: 'E_NOT_FOUND', status: 404 })) : Promise.resolve({ blob: gifBlob() });
        return Promise.resolve(null);
      },
    });
    await user.click(button());
    await waitFor(() => expect(document.querySelector('[data-gif-id="ok2"] img')).not.toBeNull());
    expect(document.querySelector('[data-gif-id="dead"] img')).toBeNull();
  });
});
