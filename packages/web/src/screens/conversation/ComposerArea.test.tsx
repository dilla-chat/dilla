import { beforeEach, describe, it, expect, vi } from 'vitest';
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { BROWSER_ATTACHMENT_CAP, type TrayItem } from '@dilla/client-core';
import { refusal } from '../../test/fake-client.ts';
import { PHONE_QUERY } from '../../layout.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { clearCommandDraftMemory } from '../commands/CommandHost.tsx';
import { GEN, O1, O2, P1, PEER, ownRow, peerRow, renderConversation, rowOf } from '../../test/conversation.tsx';

const box = (name = 'message #general') => screen.getByRole('textbox', { name });
const chip = () => document.querySelector('.d-reply-chip');
const input = () => {
  const el = document.querySelector<HTMLInputElement>('input.dw-attach-input');
  if (el === null) throw new Error('no attach input');
  return el;
};
const entry = (id: string, phase: TrayItem['phase'], over: Partial<TrayItem> = {}): TrayItem =>
  ({ id, name: `${id}.txt`, size: 5, mime: 'text/plain', image: false, phase, reason: '', ...over });

beforeEach(() => clearCommandDraftMemory());

describe('mentions in the composer (L-TS-37)', () => {
  it('renders the GIF search picker while a slash command is active', async () => {
    const { fake, user } = renderConversation({ handler: command => {
      if (command.m === 'gifStatus') return Promise.resolve({ on: true });
      if (command.m === 'gifSearch') return Promise.resolve({ items: [], next: null });
      return Promise.resolve(null);
    } });
    await waitFor(() => expect(fake.callsOf('gifStatus')).toHaveLength(1));
    await waitFor(() => expect(screen.getByRole('button', { name: 'add a gif' })).toBeInTheDocument());
    fireEvent.change(box(), { target: { value: '/giphy cats' } });
    await user.click(await screen.findByRole('button', { name: /run/i }));
    expect(await screen.findByRole('dialog', { name: 'gifs' })).toBeInTheDocument();
    expect(fake.callsOf('gifSearch')).toContainEqual(expect.objectContaining({ m: 'gifSearch', q: 'cats' }));
  });
  it('does not publish a canceled slash draft through ordinary Send', async () => {
    const { fake, user } = renderConversation();
    fireEvent.change(box(), { target: { value: '/secret-command' } });
    await user.keyboard('{Escape}{Escape}');
    expect(box()).toHaveValue('/secret-command');
    await user.click(screen.getByRole('button', { name: 'send' }));
    expect(await screen.findByRole('button', { name: /send as text/i })).toBeInTheDocument();
    expect(fake.callsOf('send')).toEqual([]);
    await user.click(screen.getByRole('button', { name: /send as text/i }));
    await waitFor(() => expect(fake.callsOf('send')).toEqual([{ m: 'send', channelId: GEN, text: '/secret-command' }]));
  });
  it('reopens command mode after a canceled slash draft is edited to a new slash command', async () => {
    const { user } = renderConversation();
    fireEvent.change(box(), { target: { value: '/me first' } });
    await user.click(await screen.findByRole('button', { name: /cancel/i }));
    fireEvent.change(box(), { target: { value: '/search second' } });
    expect(await screen.findByRole('combobox', { name: 'query' })).toHaveValue('second');
  });
  it('opens a fresh @ query at the same position after Escape and deletion', async () => {
    const { user } = renderConversation();
    await user.type(box(), '@b');
    expect(screen.getByRole('listbox', { name: 'people to mention' })).toBeInTheDocument();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('listbox')).toBeNull();
    await user.keyboard('{Backspace}{Backspace}');
    expect(box()).toHaveValue('');
    await user.keyboard('@');
    expect(screen.getByRole('listbox', { name: 'people to mention' })).toBeInTheDocument();
  });
  it('opens the list on @ with members and @everyone, moves with the arrows and inserts the pick', async () => {
    const { fake, user } = renderConversation();
    await user.type(box(), '@');
    const combo = screen.getByRole('textbox', { name: 'message #general' });
    const list = screen.getByRole('listbox', { name: 'people to mention' });
    const options = within(list).getAllByRole('option');
    expect(options).toHaveLength(3);
    expect(options[0]).toHaveTextContent('@bob');
    expect(options[1]).toHaveTextContent('Mo');
    expect(options[1]).toHaveTextContent('@mo');
    expect(options[2]).toHaveTextContent('@everyone');
    expect(options[2]).toHaveTextContent('everyone in this channel');
    expect(combo).toHaveAttribute('aria-autocomplete', 'list');
    expect(combo).toHaveAttribute('aria-controls', list.id);
    expect(combo).toHaveAttribute('aria-activedescendant', options[0].id);
    expect(options[0]).toHaveAttribute('aria-selected', 'true');
    await user.keyboard('{ArrowDown}');
    expect(combo).toHaveAttribute('aria-activedescendant', options[1].id);
    await user.keyboard('{Enter}');
    expect(screen.queryByRole('listbox')).toBeNull();
    expect(box()).toHaveValue('@mo ');
    expect(fake.callsOf('send')).toEqual([]);
  });
  it('filters by prefix and picks with Tab', async () => {
    const { user } = renderConversation();
    await user.type(box(), 'hey @M');
    expect(within(screen.getByRole('listbox', { name: 'people to mention' })).getAllByRole('option')).toHaveLength(1);
    await user.keyboard('{Tab}');
    expect(box()).toHaveValue('hey @mo ');
  });
  it('offers the DM’s other participant and no broadcast', async () => {
    const { user } = renderConversation({ dm: true });
    await user.type(box('message @bob'), '@');
    const options = within(screen.getByRole('listbox', { name: 'people to mention' })).getAllByRole('option');
    expect(options).toHaveLength(1);
    expect(options[0]).toHaveTextContent('@bob');
  });
  it('a picked mention reaches the worker as a token', async () => {
    const { fake, user } = renderConversation();
    await user.type(box(), '@bo');
    await user.keyboard('{Enter}look{Enter}');
    expect(fake.callsOf('send')).toEqual([{ m: 'send', channelId: GEN, text: `<@${PEER}> look` }]);
  });
  it('converts a typed @username of a member, and @everyone in a channel only', async () => {
    const channel = renderConversation();
    await channel.user.type(box(), 'hey @bob and @everyone now{Enter}');
    expect(channel.fake.callsOf('send')).toEqual([{ m: 'send', channelId: GEN, text: `hey <@${PEER}> and <@everyone> now` }]);
    channel.view.unmount();
    const dm = renderConversation({ dm: true });
    await dm.user.type(box('message @bob'), 'hey @bob and @everyone now{Enter}');
    expect(dm.fake.callsOf('send').map(c => c.text)).toEqual([`hey <@${PEER}> and @everyone now`]);
  });
  it('counts the budget on the text as it will be sent', () => {
    renderConversation();
    fireEvent.change(box(), { target: { value: '@bob '.repeat(800) } });
    expect(screen.getByText('24800 bytes over the limit', { selector: '.d-composer__counter' })).toBeInTheDocument();
  });
});

describe('replies (L-UI-45)', () => {
  it('starts from the toolbar and focuses the composer; Escape closes the list before it cancels the reply', async () => {
    const { user } = renderConversation({ items: [peerRow(P1, '1', 'the question')] });
    await user.click(within(within(rowOf(P1)).getByRole('toolbar')).getByRole('button', { name: 'reply' }));
    expect(box()).toHaveFocus();
    expect(chip()).toHaveTextContent(/replying to bob/i);
    expect(chip()).toHaveTextContent('the question');
    await user.keyboard('@b');
    expect(screen.getByRole('listbox', { name: 'people to mention' })).toBeInTheDocument();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('listbox')).toBeNull();
    expect(chip()).not.toBeNull();
    await user.keyboard('o');
    expect(screen.queryByRole('listbox')).toBeNull();
    await user.keyboard('{Escape}');
    expect(chip()).toBeNull();
    expect(box()).toHaveFocus();
  });
  it('sends the reply with its target, keeps the chip while refused and clears it once a send resolves', async () => {
    let refuse = true;
    const { fake, user } = renderConversation({ items: [peerRow(P1, '1', 'q')], handler: c => (
      c.m !== 'send' ? Promise.resolve(null) : refuse ? Promise.reject(refusal({ code: 'E_NETWORK' })) : Promise.resolve(null)) });
    await user.click(within(within(rowOf(P1)).getByRole('toolbar')).getByRole('button', { name: 'reply' }));
    await user.keyboard('answer{Enter}');
    expect(await screen.findByRole('alert')).toHaveTextContent('That did not work (E_NETWORK).');
    expect(chip()).not.toBeNull();
    refuse = false;
    await user.click(screen.getByRole('button', { name: 'send' }));
    await waitFor(() => expect(chip()).toBeNull());
    expect(fake.callsOf('send')).toEqual([
      { m: 'send', channelId: GEN, text: 'answer', replyTo: P1 }, { m: 'send', channelId: GEN, text: 'answer', replyTo: P1 },
    ]);
    expect(box()).toHaveValue('');
  });
  it('cancels from the chip’s button', async () => {
    const { user } = renderConversation({ items: [peerRow(P1, '1', 'q')] });
    await user.click(within(within(rowOf(P1)).getByRole('toolbar')).getByRole('button', { name: 'reply' }));
    await user.click(screen.getByRole('button', { name: 'cancel reply' }));
    expect(chip()).toBeNull();
  });
  it('returns focus to the textarea after cancelling a reply (F4)', async () => {
    const { user } = renderConversation({ items: [peerRow(P1, '1', 'q')] });
    await user.click(within(within(rowOf(P1)).getByRole('toolbar')).getByRole('button', { name: 'reply' }));
    await user.click(screen.getByRole('button', { name: 'cancel reply' }));
    expect(box()).toHaveFocus();
  });
});

describe('ArrowUp in the empty composer', () => {
  it('edits the newest own sent message, and does nothing while there is text', async () => {
    const { user } = renderConversation({ items: [
      ownRow(O1, '1', 'first'), ownRow(O2, '2', `second <@${PEER}>`), peerRow(P1, '3', 'theirs'), ownRow('ab'.repeat(16), null, 'sending', { state: 'pending' }),
    ] });
    await user.click(box());
    await user.keyboard('x{ArrowUp}');
    expect(screen.queryByRole('textbox', { name: 'edit your message' })).toBeNull();
    await user.keyboard('{Backspace}{ArrowUp}');
    const editor = screen.getByRole('textbox', { name: 'edit your message' });
    expect(editor).toHaveFocus();
    expect(editor).toHaveValue('second @bob');
    expect(rowOf(O2)).toContainElement(editor);
  });
});

describe('attaching (L-TS-35, Q16)', () => {
  it('attaches from the button’s hidden input and from a paste', async () => {
    const { fake, user } = renderConversation();
    expect(input()).toHaveAttribute('type', 'file');
    expect(input().multiple).toBe(true);
    expect(input()).toHaveAttribute('hidden');
    const click = vi.spyOn(input(), 'click');
    await user.click(screen.getByRole('button', { name: 'Attach files (25 MB each)' }));
    expect(click).toHaveBeenCalledTimes(1);
    const a = new File(['abc'], 'a.txt', { type: 'text/plain' });
    fireEvent.change(input(), { target: { files: [a] } });
    await waitFor(() => expect(fake.callsOf('attachFiles')).toEqual([{ m: 'attachFiles', channelId: GEN, files: [a] }]));
    const b = new File(['x'], 'b.png', { type: 'image/png' });
    fireEvent.paste(box(), { clipboardData: { files: [b], types: ['Files'], getData: () => '' } });
    await waitFor(() => expect(fake.callsOf('attachFiles')).toHaveLength(2));
    expect(fake.callsOf('attachFiles')[1]).toEqual({ m: 'attachFiles', channelId: GEN, files: [b] });
  });
  it.each([
    ['E_ATTACHMENT_TOO_LARGE', 'huge.bin is over 25 MB and cannot be sent from a browser.'],
    ['E_ATTACHMENT_COUNT', 'A message carries at most 4 files.'],
    ['E_NETWORK', 'That did not work (E_NETWORK). Try again, or reload the page.'],
  ])('shows the refusal %s by its code, never its detail', async (code, text) => {
    renderConversation({ handler: c => (c.m === 'attachFiles' ? Promise.reject(refusal({ code, detail: 'ignored detail' })) : Promise.resolve(null)) });
    const small = new File(['a'], 'small.txt');
    const huge = new File(['b'], 'huge.bin');
    Object.defineProperty(huge, 'size', { value: BROWSER_ATTACHMENT_CAP + 1 });
    fireEvent.change(input(), { target: { files: [small, huge] } });
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(text);
    expect(alert).toHaveClass('dw-tray-error');
    expect(screen.queryByText(/ignored detail/)).toBeNull();
  });
  it('shows the channel’s tray with each phase and a failed entry’s hint, and removes an entry', async () => {
    const { fake, user } = renderConversation({ before: f => f.set(`tray:${GEN}`, [
      entry('t1', 'reading', { name: 'a.png', size: 1234, mime: 'image/png', image: true }),
      entry('t2', 'uploading', { name: 'b.bin', size: 70_000 }),
      entry('t3', 'ready'),
      entry('t4', 'failed', { reason: 'E_QUOTA' }),
    ]) });
    const items = within(screen.getByRole('list', { name: 'files to send' })).getAllByRole('listitem');
    expect(items.map(e => e.getAttribute('data-phase'))).toEqual(['reading', 'uploading', 'ready', 'failed']);
    expect(items[0]).toHaveTextContent('a.png');
    expect(items[0]).toHaveTextContent('1.2 KB');
    expect(items[0]).toHaveTextContent('reading');
    expect(items[1]).toHaveTextContent('68 KB');
    expect(items[1]).toHaveTextContent('uploading');
    expect(items[2]).toHaveTextContent('ready');
    expect(items[3]).toHaveTextContent('failed (E_QUOTA)');
    expect(within(items[3]).getByText('Remove it and attach it again.')).toBeInTheDocument();
    expect(within(items[2]).queryByText('Remove it and attach it again.')).toBeNull();
    await user.click(within(items[1]).getByRole('button', { name: 'remove b.bin' }));
    expect(fake.callsOf('discardAttachment')).toEqual([{ m: 'discardAttachment', channelId: GEN, trayId: 't2' }]);
  });
  // Task 7 fix round 1 (Signal §6.8): an attached still shows its own preview, keyed to its tray id in the order of the files;
  // a GIF is the striped tile with its name; the URL goes back when the entry leaves the tray.
  it('previews an attached image and a GIF, and revokes the image URL when its entry leaves', async () => {
    const created: string[] = [];
    const revoked: string[] = [];
    const urlApi = URL as unknown as { createObjectURL?: (b: Blob) => string; revokeObjectURL?: (u: string) => void };
    const saved = { create: urlApi.createObjectURL, revoke: urlApi.revokeObjectURL };
    urlApi.createObjectURL = () => { const u = `blob:http://127.0.0.1/${String(created.length + 1)}`; created.push(u); return u; };
    urlApi.revokeObjectURL = (u: string) => { revoked.push(u); };
    try {
      const { fake } = renderConversation();
      const files = [new File(['x'], 'map.png', { type: 'image/png' }), new File(['y'], 'victory.gif', { type: 'image/gif' }), new File(['z'], 'notes.txt', { type: 'text/plain' })];
      act(() => { fireEvent.change(input(), { target: { files } }); });
      await waitFor(() => expect(fake.callsOf('attachFiles')).toHaveLength(1));
      act(() => { fake.set(`tray:${GEN}`, [
        entry('7', 'uploading', { name: 'map.png', mime: 'image/png', image: true }), entry('8', 'reading', { name: 'victory.gif', mime: 'image/gif', image: true }),
        entry('9', 'reading', { name: 'notes.txt' }),
      ]); });
      const cards = () => Array.from(document.querySelectorAll<HTMLElement>('.d-attachment-tray__item'));
      await waitFor(() => expect(cards().map(c => c.dataset.kind)).toEqual(['image', 'gif', 'file']));
      expect(cards()[0].querySelector<HTMLElement>('.d-attachment-tray__preview')!.style.backgroundImage).toContain(created[0]);
      act(() => { fake.set(`tray:${GEN}`, [entry('8', 'ready', { name: 'victory.gif', mime: 'image/gif', image: true }), entry('9', 'ready', { name: 'notes.txt' })]); });
      await waitFor(() => expect(revoked).toContain(created[0]));
      // An earlier test's queued previews may go back meanwhile (their tray unmounted); of this test's URLs only the removed still's does.
      expect(revoked.filter(u => created.includes(u))).toEqual([created[0]]);
    } finally {
      urlApi.createObjectURL = saved.create;
      urlApi.revokeObjectURL = saved.revoke;
    }
  });
  it('waits for every file, then sends them with the text, or alone', async () => {
    const { fake, user } = renderConversation({ before: f => f.set(`tray:${GEN}`, [entry('t1', 'ready'), entry('t2', 'uploading')]) });
    await user.type(box(), 'x{Enter}');
    expect(fake.callsOf('send')).toEqual([]);
    expect(screen.getByRole('alert')).toHaveTextContent('Wait until every file is ready, or remove it.');
    act(() => fake.set(`tray:${GEN}`, [entry('t1', 'ready'), entry('t2', 'ready')]));
    await user.keyboard('{Enter}');
    expect(fake.callsOf('send')).toEqual([{ m: 'send', channelId: GEN, text: 'x', attachments: ['t1', 't2'] }]);
    expect(screen.queryByRole('alert')).toBeNull();
    act(() => fake.set(`tray:${GEN}`, [entry('t3', 'ready')]));
    await waitFor(() => expect(box()).toHaveValue(''));
    // Composer.canSendEmpty (L-UI-53) lets files go out with no text (DEV-W2-60).
    await user.click(screen.getByRole('button', { name: 'send' }));
    expect(fake.callsOf('send').at(-1)).toEqual({ m: 'send', channelId: GEN, text: '', attachments: ['t3'] });
  });
  it('drops files on the conversation and says so while they are dragged over it', async () => {
    const { fake } = renderConversation({ items: [peerRow(P1, '1', 'x')] });
    const zone = document.querySelector<HTMLElement>('.dw-conversation');
    if (zone === null) throw new Error('no .dw-conversation');
    const status = () => document.querySelector('.dw-drop-status')?.textContent;
    expect(status()).toBe('');
    fireEvent.dragEnter(zone, { dataTransfer: { types: ['Files'], files: [] } });
    expect(status()).toBe('Drop to attach');
    expect(document.querySelector('.d-drop-overlay')).toHaveTextContent('Up to 4 files, 25 MB each.');
    const f = new File(['a'], 'a.txt', { type: 'text/plain' });
    fireEvent.drop(zone, { dataTransfer: { types: ['Files'], files: [f] } });
    await waitFor(() => expect(fake.callsOf('attachFiles')).toEqual([{ m: 'attachFiles', channelId: GEN, files: [f] }]));
    expect(status()).toBe('');
  });
  it('passes axe with the mention list, a reply chip and a tray open', async () => {
    const { user, view } = renderConversation({ items: [peerRow(P1, '1', 'q')], before: f => f.set(`tray:${GEN}`, [entry('t1', 'ready')]) });
    await user.click(within(within(rowOf(P1)).getByRole('toolbar')).getByRole('button', { name: 'reply' }));
    await user.keyboard('@');
    expect(screen.getByRole('listbox', { name: 'people to mention' })).toBeInTheDocument();
    await expectNoAxeViolations(view.container);
  });
});

describe('tray focus handoff (F4)', () => {
  it('on a phone the last removed entry puts no focus in the composer', async () => {
    window.matchMedia = vi.fn((query: string) => ({
      matches: query === PHONE_QUERY, media: query, addEventListener: () => undefined, removeEventListener: () => undefined,
    })) as unknown as typeof window.matchMedia;
    try {
      const { fake, user } = renderConversation({ before: f => f.set(`tray:${GEN}`, [entry('t1', 'ready')]) });
      await user.click(screen.getByRole('button', { name: 'remove t1.txt' }));
      act(() => fake.set(`tray:${GEN}`, []));
      expect(box()).not.toHaveFocus();
    } finally {
      Reflect.deleteProperty(window, 'matchMedia');
    }
  });

  it('moves a removed entry to the next remove button, then to the textarea', async () => {
    const { fake, user } = renderConversation({ before: f => f.set(`tray:${GEN}`, [entry('t1', 'ready'), entry('t2', 'ready'), entry('t3', 'ready')]) });
    await user.click(screen.getByRole('button', { name: 'remove t1.txt' }));
    act(() => fake.set(`tray:${GEN}`, [entry('t2', 'ready'), entry('t3', 'ready')]));
    expect(screen.getByRole('button', { name: 'remove t2.txt' })).toHaveFocus();
    await user.click(screen.getByRole('button', { name: 'remove t3.txt' }));
    act(() => fake.set(`tray:${GEN}`, [entry('t2', 'ready')]));
    expect(box()).toHaveFocus();
  });
});
