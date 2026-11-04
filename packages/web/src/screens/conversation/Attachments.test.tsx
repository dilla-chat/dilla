import { describe, it, expect, vi } from 'vitest';
import { StrictMode } from 'react';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { CoreProvider } from '../../core/context.tsx';
import { refusal } from '../../test/fake-client.ts';
import { FakeClient } from '../../test/fake-client.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { GEN, O1, P1, P2, file, image, ownRow, peerRow, renderConversation, rowOf, stubBlobUrls, timeline } from '../../test/conversation.tsx';
import { AttachmentList, REVOKE_SAVED_AFTER_MS } from './Attachments.tsx';

const opened = (name: string, type: string) => ({ blob: new Blob([new Uint8Array([1, 2, 3])], { type }), name, mime: type });

describe('image cards (L-TS-37, G5)', () => {
  it('requests a mounted thumbnail once and shows it under StrictMode effect replay (F12)', async () => {
    const urls = stubBlobUrls();
    try {
      const fake = new FakeClient();
      fake.handler = c => Promise.resolve(c.m === 'openAttachment' ? opened('map.png', 'image/webp') : null);
      const view = render(<StrictMode><CoreProvider client={fake}><AttachmentList channelId={GEN}
        item={peerRow(P1, '7', 'look', { attachments: [image()] })} tabbable onOpenImage={() => undefined}
        onError={() => undefined} /></CoreProvider></StrictMode>);
      await waitFor(() => expect(screen.getByRole('img', { name: 'map.png' })).toHaveAttribute('data-thumb', 'ready'));
      expect(fake.callsOf('openAttachment')).toHaveLength(1);
      view.unmount();
    } finally {
      urls.restore();
    }
  });
  it('asks for a thumbnail once, shows it from a page-minted URL and revokes it when the card leaves', async () => {
    const urls = stubBlobUrls();
    try {
      const { fake } = renderConversation({ items: [peerRow(P1, '7', 'look', { attachments: [image()] })],
        handler: c => Promise.resolve(c.m === 'openAttachment' ? opened('map.png', 'image/webp') : null) });
      const pic = await screen.findByRole('img', { name: 'map.png' });
      await waitFor(() => expect(pic).toHaveAttribute('data-thumb', 'ready'));
      expect(pic).toHaveAttribute('src', 'blob:test/1');
      expect(fake.callsOf('openAttachment')).toEqual([{ m: 'openAttachment', channelId: GEN, seq: '7', index: 0, thumb: true }]);
      act(() => fake.set(`timeline:${GEN}`, timeline([peerRow(P1, '7', 'look', { attachments: [image()] }), peerRow(P2, '8', 'more')])));
      expect(fake.callsOf('openAttachment')).toHaveLength(1);
      expect(urls.revoked).toEqual([]);
      act(() => fake.set(`timeline:${GEN}`, timeline([peerRow(P2, '8', 'more')])));
      expect(urls.revoked).toEqual(['blob:test/1']);
    } finally {
      urls.restore();
    }
  });
  it('asks nothing for an image without a thumbnail, one too large, or one not yet sent', () => {
    const { fake } = renderConversation({ items: [
      peerRow(P1, '7', 'a', { attachments: [image({ thumb: false }), image({ index: 1, name: 'big.png', tooLarge: true, size: 26_214_401 })] }),
      peerRow(P2, '8', 'b', { seq: null, attachments: [image()] }),
    ] });
    expect(fake.callsOf('openAttachment')).toEqual([]);
    expect(screen.getByText('too large to open in a browser')).toBeInTheDocument();
  });
  it('gives an unsent message’s and a too-large file’s cards no save button and no busy state (L-UI-49)', () => {
    renderConversation({ items: [
      ownRow(O1, null, 'sending', { state: 'pending', attachments: [file()] }),
      peerRow(P1, '7', 'huge', { attachments: [file({ name: 'huge.bin', size: 26_214_401, tooLarge: true })] }),
    ] });
    expect(screen.queryByRole('button', { name: 'save data.bin' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'save huge.bin' })).toBeNull();
    expect(rowOf(O1).querySelector('[aria-busy]')).toBeNull();
    expect(rowOf(O1)).not.toHaveTextContent('opening…');
    expect(rowOf(P1)).toHaveTextContent('too large to open in a browser');
  });
  it('opens the full image in the lightbox, moves between the message’s images, and closes back to its card', async () => {
    const urls = stubBlobUrls();
    try {
      const { fake, user, view } = renderConversation({
        items: [peerRow(P1, '7', '', { attachments: [image({ name: 'one.png', thumb: false }), image({ index: 1, name: 'two.png', thumb: false }), file({ index: 2 })] })],
        handler: c => Promise.resolve(c.m === 'openAttachment' ? opened(c.index === 0 ? 'one.png' : 'two.png', 'image/png') : null),
      });
      const open1 = screen.getByRole('button', { name: 'open one.png' });
      await user.click(open1);
      const box = () => {
        const el = document.querySelector<HTMLElement>('dialog.d-lightbox');
        if (el === null) throw new Error('no lightbox');
        return el;
      };
      await waitFor(() => expect(box().querySelector('img')).toHaveAttribute('src', 'blob:test/1'));
      expect(box().querySelector('img')).toHaveAttribute('alt', 'one.png');
      expect(fake.callsOf('openAttachment').at(-1)).toEqual({ m: 'openAttachment', channelId: GEN, seq: '7', index: 0, thumb: false });
      expect(box()).toHaveTextContent('one.png, 1.2 KB');
      expect(within(box()).queryByRole('button', { name: 'Previous image' })).toBeNull();
      await expectNoAxeViolations(view.container);
      await user.click(within(box()).getByRole('button', { name: 'Next image' }));
      await waitFor(() => expect(box().querySelector('img')).toHaveAttribute('alt', 'two.png'));
      await waitFor(() => expect(box().querySelector('img')).toHaveAttribute('src', 'blob:test/2'));
      expect(urls.revoked).toContain('blob:test/1');
      expect(within(box()).queryByRole('button', { name: 'Next image' })).toBeNull();
      await user.click(within(box()).getByRole('button', { name: 'Close' }));
      expect(document.querySelector('dialog.d-lightbox')).toBeNull();
      expect(open1).toHaveFocus();
      expect(urls.revoked).toContain('blob:test/2');
    } finally {
      urls.restore();
    }
  });
});

describe('file cards', () => {
  it('saves a file through a link it creates, then revokes the link', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const urls = stubBlobUrls();
    const clicks: { download: string; href: string; inDocument: boolean }[] = [];
    const spy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      clicks.push({ download: this.download, href: this.href, inDocument: this.isConnected });
    });
    try {
      const { fake } = renderConversation({ items: [peerRow(P1, '7', 'here', { attachments: [file()] })],
        handler: c => Promise.resolve(c.m === 'openAttachment' ? opened('data.bin', 'application/octet-stream') : null) });
      fireEvent.click(screen.getByRole('button', { name: 'save data.bin' }));
      await waitFor(() => expect(clicks).toHaveLength(1));
      expect(clicks[0]).toEqual({ download: 'data.bin', href: 'blob:test/1', inDocument: true });
      expect(document.querySelector('a[download]')).toBeNull();
      expect(fake.callsOf('openAttachment')).toEqual([{ m: 'openAttachment', channelId: GEN, seq: '7', index: 0, thumb: false }]);
      expect(urls.revoked).toEqual([]);
      act(() => { vi.advanceTimersByTime(REVOKE_SAVED_AFTER_MS); });
      expect(urls.revoked).toEqual(['blob:test/1']);
    } finally {
      spy.mockRestore();
      urls.restore();
      vi.useRealTimers();
    }
  });
  it('names an unnamed file, and says by code when opening fails', async () => {
    const { user } = renderConversation({ items: [peerRow(P1, '7', '', { attachments: [file({ name: '' })] })],
      handler: c => (c.m === 'openAttachment' ? Promise.reject(refusal({ code: 'E_BLOB_HASH', detail: 'x' })) : Promise.resolve(null)) });
    await user.click(screen.getByRole('button', { name: 'save file' }));
    expect(await screen.findByText('could not open (E_BLOB_HASH)')).toBeInTheDocument();
  });
});

describe('inline players (flow 06, L-UI-71, R9)', () => {
  const media = (kind: 'audio' | 'video', name: string, mime: string) => file({ kind, name, mime, size: 40_000 });
  it('an audio attachment plays on demand, and its blob URL is revoked on unmount', async () => {
    const urls = stubBlobUrls();
    try {
      const fake = new FakeClient();
      fake.handler = c => Promise.resolve(c.m === 'openAttachment' ? opened('voice.ogg', 'audio/ogg') : null);
      const view = render(<CoreProvider client={fake}><AttachmentList channelId={GEN}
        item={peerRow(P1, '7', 'listen', { attachments: [media('audio', 'voice.ogg', 'audio/ogg')] })} tabbable onOpenImage={() => undefined}
        onError={() => undefined} /></CoreProvider>);
      // Nothing is fetched before the click.
      expect(fake.callsOf('openAttachment')).toEqual([]);
      fireEvent.click(screen.getByRole('button', { name: 'play voice.ogg' }));
      await waitFor(() => expect(document.querySelector('[data-media-state]')).toHaveAttribute('data-media-state', 'ready'));
      expect(fake.callsOf('openAttachment')).toEqual([{ m: 'openAttachment', channelId: GEN, seq: '7', index: 0, thumb: false }]);
      const audio = document.querySelector('audio');
      expect(audio).toHaveAttribute('src', 'blob:test/1');
      expect(audio).not.toHaveAttribute('autoplay');
      view.unmount();
      expect(urls.revoked).toEqual(['blob:test/1']);
    } finally {
      urls.restore();
    }
  });
  it('a video that errors shows the error text and save', async () => {
    const urls = stubBlobUrls();
    const unhandled = vi.fn();
    window.addEventListener('unhandledrejection', unhandled);
    try {
      const fake = new FakeClient();
      fake.handler = c => Promise.resolve(c.m === 'openAttachment' ? opened('walk.webm', 'video/webm') : null);
      render(<CoreProvider client={fake}><AttachmentList channelId={GEN}
        item={peerRow(P1, '7', 'watch', { attachments: [media('video', 'walk.webm', 'video/webm')] })} tabbable onOpenImage={() => undefined}
        onError={() => undefined} /></CoreProvider>);
      fireEvent.click(screen.getByRole('button', { name: 'play walk.webm' }));
      const video = await waitFor(() => { const v = document.querySelector('video'); expect(v).not.toBeNull(); return v!; });
      expect(video).toHaveAttribute('playsinline');
      fireEvent.error(video);
      expect(await screen.findByText('This browser cannot play this file.')).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'save walk.webm' })).toHaveTextContent('save');
      expect(document.querySelector('[data-media-state]')).toHaveAttribute('data-media-state', 'error');
      await new Promise(resolve => setTimeout(resolve, 0));
      expect(unhandled).not.toHaveBeenCalled();
    } finally {
      window.removeEventListener('unhandledrejection', unhandled);
      urls.restore();
    }
  });
  it('a blob that does not open shows the error state', async () => {
    const fake = new FakeClient();
    fake.handler = c => (c.m === 'openAttachment' ? Promise.reject(refusal({ code: 'E_BLOB_OPEN' })) : Promise.resolve(null));
    render(<CoreProvider client={fake}><AttachmentList channelId={GEN}
      item={peerRow(P1, '7', 'listen', { attachments: [media('audio', 'voice.ogg', 'audio/ogg')] })} tabbable onOpenImage={() => undefined}
      onError={() => undefined} /></CoreProvider>);
    fireEvent.click(screen.getByRole('button', { name: 'play voice.ogg' }));
    expect(await screen.findByText('This browser cannot play this file.')).toBeInTheDocument();
  });
  it('a too-large audio file stays a file card without a player', () => {
    render(<CoreProvider client={new FakeClient()}><AttachmentList channelId={GEN}
      item={peerRow(P1, '7', 'big', { attachments: [file({ kind: 'audio', name: 'long.flac', tooLarge: true, size: 26_214_401 })] })}
      tabbable onOpenImage={() => undefined} onError={() => undefined} /></CoreProvider>);
    expect(screen.queryByRole('button', { name: 'play long.flac' })).toBeNull();
  });
});

it('shows compact sent-file metadata in prototype size · EXT order (gap52)', () => {
  renderConversation({ items: [peerRow(P1, '7', 'sent', { attachments: [file({ name: 'raid-notes.txt', size: 1234 })] })] });
  expect(screen.getByText('1.2 KB · TXT')).toBeInTheDocument();
});
