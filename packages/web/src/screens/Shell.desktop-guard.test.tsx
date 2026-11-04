import { afterEach, expect, it, vi } from 'vitest';
import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { ChannelSummary, SliceTypes, TimelineItem } from '@dilla/client-core';
import { SliceStore } from '../../../client-core/src/state/store.ts';
import type { DesktopBridge, DesktopCommand, DesktopEvent } from '../../../desktop/src/contract.ts';
import { CoreProvider } from '../core/context.tsx';
import { startDesktopLifecycle } from '../desktop.ts';
import { pushHistoryState, replaceHistoryState } from '../router.ts';
import { FakeClient, refusal } from '../test/fake-client.ts';
import { account, timelineItem } from '../test/fixtures.ts';
import { en } from '../strings/index.ts';
import { Shell } from './Shell.tsx';

const COMMUNITY = 'a1'.repeat(16), A = 'c3'.repeat(16), B = 'd4'.repeat(16);
const ROOT_A = '71'.repeat(16), ROOT_B = '72'.repeat(16);
const WIDTH = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'offsetWidth');
let stopLifecycle = () => {};
afterEach(() => {
  cleanup(); stopLifecycle();
  replaceHistoryState(null, '', '/');
  vi.unstubAllGlobals();
  Reflect.deleteProperty(window, 'matchMedia');
  if (WIDTH) Object.defineProperty(HTMLElement.prototype, 'offsetWidth', WIDTH);
  // eslint-disable-next-line no-restricted-properties -- clears shell navigation memory
  window.sessionStorage.clear();
});

function setup() {
  Object.defineProperty(HTMLElement.prototype, 'offsetWidth', { configurable: true, get: () => 1600 });
  window.matchMedia = vi.fn((query: string) => ({ matches: query === '(min-width: 64rem)', media: query,
    addEventListener() {}, removeEventListener() {} })) as unknown as typeof window.matchMedia;
  const commands: DesktopCommand[] = [];
  const listeners = new Set<(event: DesktopEvent) => void>();
  const bridge: DesktopBridge = {
    info: () => Promise.resolve({ version: '0.1.0', instance: 'https://chat.test/',
      capabilities: { applicationAudio: false, browserAuth: false, notifications: true, screenCapture: true } }),
    send: command => { commands.push(command); return Promise.resolve(); },
    onEvent: listener => { listeners.add(listener); return () => { listeners.delete(listener); }; },
  };
  vi.stubGlobal('dillaDesktop', bridge);
  stopLifecycle = startDesktopLifecycle(bridge);
  replaceHistoryState(null, '', `/c/${COMMUNITY}/${A}`);
  const client = new FakeClient();
  // Use the real worker publisher for tray derivation; the fake supplies only the worker transport.
  const slices = new SliceStore((name, _rev, value) => client.set(name, value as SliceTypes[typeof name]));
  client.handler = command => {
    if (command.m === 'attachFiles') slices.set(`tray:${command.channelId}`, command.files.map(file => ({
      id: file.name, name: file.name, size: file.size, mime: file.type, image: false, phase: 'ready', reason: '',
    })));
    if (command.m === 'discardAttachment' || (command.m === 'send' && command.attachments?.length)) {
      slices.set(`tray:${command.channelId}`, []);
    }
    return Promise.resolve(null);
  };
  client.set('account', account());
  client.set('connection', { status: 'online', generation: '1' });
  client.set('communities', [{ id: COMMUNITY, name: 'Midgard', owner: 'ee'.repeat(16) }]);
  client.set(`channels:${COMMUNITY}`, [[A, 'alpha'], [B, 'beta']].map(([id, name], position): ChannelSummary => ({
    id, name, communityId: COMMUNITY, kind: 0, mode: 0, topic: '', parentId: null, position,
    slowmode: 0, announcement: false, group: 'none',
  })));
  for (const [id, root] of [[A, ROOT_A], [B, ROOT_B]]) {
    const row: TimelineItem = timelineItem({ key: 's1', seq: '1', msgId: root, body: `root ${id}`,
      thread: { count: 2, lastTs: 1_700_000_100 } });
    client.set(`timeline:${id}`, { channelId: id, group: 'active', items: [row], hasEarlier: false });
    client.set(`thread:${id}`, { channelId: id, rootMsgId: root, root: row, items: [], hasEarlier: false });
  }
  const view = render(<div className="d-root"><CoreProvider client={client}><Shell /></CoreProvider></div>);
  return { client, view, user: userEvent.setup(), commands,
    navigate(id: string) { act(() => pushHistoryState(null, '', `/c/${COMMUNITY}/${id}`)); },
    probe(id: string) { act(() => { for (const listener of listeners) listener({ type: 'prepare-quit', id }); });
      return commands.at(-1); },
  };
}
const composer = (name: string) => screen.getByRole('textbox', { name: `message #${name}` });
const guard = (app: ReturnType<typeof setup>) => app.commands.filter(command => command.type === 'guard').at(-1);
function expectProtected(app: ReturnType<typeof setup>, expected: boolean) {
  expect(guard(app)).toEqual({ type: 'guard', activeCall: false, unsavedDraft: expected });
  // Actual Quit and standard update restart share this native prepare-quit handshake.
  for (const id of ['actual-quit', 'restart-to-update']) {
    expect(app.probe(id)).toEqual({ type: 'quit-ack', id, activeCall: false, unsavedDraft: expected });
  }
}

it('protects an ordinary draft in A after sending B, and releases after sending the final draft', async () => {
  const app = setup();
  await app.user.type(composer('alpha'), 'keep A');
  app.navigate(B);
  await app.user.type(composer('beta'), 'send B');
  await app.user.click(within(screen.getByRole('main')).getByRole('button', { name: 'send' }));
  await waitFor(() => expect(composer('beta')).toHaveValue(''));
  expectProtected(app, true);
  app.navigate(A);
  expect(composer('alpha')).toHaveValue('keep A');
  await app.user.click(within(screen.getByRole('main')).getByRole('button', { name: 'send' }));
  await waitFor(() => expect(composer('alpha')).toHaveValue(''));
  expectProtected(app, false);
});

it('protects an A draft restored by a late failed send while the empty B composer is current', async () => {
  const app = setup();
  let rejectSend!: (reason: unknown) => void;
  const handler = app.client.handler;
  app.client.handler = command => command.m === 'send' && command.channelId === A
    ? new Promise((_resolve, reject) => { rejectSend = reject; }) : handler(command);
  await app.user.type(composer('alpha'), 'retry A');
  await app.user.click(within(screen.getByRole('main')).getByRole('button', { name: 'send' }));
  app.navigate(B);
  await app.user.type(composer('beta'), 'send B');
  await app.user.click(within(screen.getByRole('main')).getByRole('button', { name: 'send' }));
  expectProtected(app, false);
  await act(async () => { rejectSend(refusal({ code: 'E_OFFLINE' })); await Promise.resolve(); });
  expect(composer('beta')).toHaveValue('');
  expectProtected(app, true);
  app.navigate(A);
  expect(composer('alpha')).toHaveValue('retry A');
  await app.user.clear(composer('alpha'));
  expectProtected(app, false);
});

it('protects a retained thread draft after another thread sends, then releases on discard', async () => {
  const app = setup();
  const openThread = async () => {
    await app.user.click(screen.getByRole('button', { name: '2 replies' }));
    return screen.findByRole('complementary', { name: en['thread.title'] });
  };
  let thread = await openThread();
  await app.user.type(within(thread).getByRole('textbox'), 'keep thread A');
  app.navigate(B);
  thread = await openThread();
  // Wave B: the thread composer has no Send button (the prototype's); Enter sends.
  await app.user.type(within(thread).getByRole('textbox'), 'send thread B{Enter}');
  await waitFor(() => expect(within(thread).getByRole('textbox')).toHaveValue(''));
  expectProtected(app, true);
  app.navigate(A);
  thread = await openThread();
  expect(within(thread).getByRole('textbox')).toHaveValue('keep thread A');
  await app.user.clear(within(thread).getByRole('textbox'));
  expectProtected(app, false);
});

it.each(['discard', 'send'] as const)('protects a file-only tray away from A, then releases on final %s', async finish => {
  const app = setup();
  const file = new File(['hello'], 'keep.txt', { type: 'text/plain' });
  await app.user.upload(document.querySelector<HTMLInputElement>('main input[type=file]')!, file);
  await screen.findByText('keep.txt');
  app.navigate(B);
  expect(composer('beta')).toHaveValue('');
  expectProtected(app, true);
  app.navigate(A);
  await screen.findByText('keep.txt');
  await app.user.click(within(screen.getByRole('main')).getByRole('button', {
    name: finish === 'send' ? 'send' : /remove.*keep.txt/i,
  }));
  await waitFor(() => expect(screen.queryByText('keep.txt')).not.toBeInTheDocument());
  expectProtected(app, false);
});

it('keeps the other file tray protected after sending B and releases when the noncurrent A tray clears', async () => {
  const app = setup();
  const upload = (name: string) => app.user.upload(document.querySelector<HTMLInputElement>('main input[type=file]')!,
    new File(['hello'], name, { type: 'text/plain' }));
  await upload('alpha.txt');
  app.navigate(B);
  await upload('beta.txt');
  await app.user.click(within(screen.getByRole('main')).getByRole('button', { name: 'send' }));
  await waitFor(() => expect(screen.queryByText('beta.txt')).not.toBeInTheDocument());
  expectProtected(app, true);
  await act(async () => { await app.client.call({ m: 'discardAttachment', channelId: A, trayId: 'alpha.txt' }); });
  expect(composer('beta')).toHaveValue('');
  expectProtected(app, false);
});
