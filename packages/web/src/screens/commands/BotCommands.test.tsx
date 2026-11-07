// Bot v2 task 20: the picker reads the Bot's commands from the catalog on this device, and a Bot command is one sealed
// request whose answers stream in over the socket (the `botAnswers` slice the worker publishes).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { toHex, type BotAnswerView, type BotCatalogView } from '@dilla/client-core';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient } from '../../test/fake-client.ts';
import { clearCommandDraftMemory, CommandHost } from './CommandHost.tsx';
import { privateResults } from './private-results.ts';
import { botSince } from './bot-run.ts';
import { FIXTURE_BOT_USER, fixtureCatalog } from './bot-catalog.fixture.ts';

const COMMUNITY = '55'.repeat(16);
const CHANNEL = '44'.repeat(16);
const SELF = '66'.repeat(16);
const text = (s: string): string => toHex(new TextEncoder().encode(s));

function harness(catalog: BotCatalogView | null = fixtureCatalog(), user = userEvent.setup()) {
  const fake = new FakeClient();
  fake.set('communities', [{ id: COMMUNITY, name: 'Town', owner: '77'.repeat(16) }] as never);
  fake.set(`members:${COMMUNITY}`, [{ userId: SELF, username: 'ada', display: 'Ada', kind: 0, roleIds: [] },
    { userId: FIXTURE_BOT_USER, username: 'glenn', display: 'Glenn', kind: 1, roleIds: [] }] as never);
  fake.set(`botCatalog:${COMMUNITY}`, catalog);
  let draft = '';
  const setDraft = (value: string) => { draft = value; view.rerender(tree()); };
  const tree = () => <CoreProvider client={fake}>
    <CommandHost communityId={COMMUNITY} channelId={CHANNEL} bits={0} items={[]} selfId={SELF} draft={draft} onDraft={setDraft}
      onSendLiteral={() => undefined} encodeMessage={s => s} openGif={() => undefined} gifsEnabled />
  </CoreProvider>;
  const view = render(tree());
  const open = (value: string) => { act(() => { setDraft(value); }); };
  const results = () => privateResults(fake, ':', `${COMMUNITY}/${CHANNEL}/`).rows();
  let n = 0;
  const answer = (a: Omit<BotAnswerView, 'n' | 'requestId'> & { requestId?: string }) => {
    const requestId = a.requestId ?? fake.callsOf('sendBotCommand').at(-1)!.requestId;
    n += 1;
    fake.publish('botAnswers', [...(fake.get('botAnswers') ?? []), { ...a, n, requestId }]);
  };
  return { fake, open, results, answer, user, unmount: () => { view.unmount(); } };
}

beforeEach(() => { clearCommandDraftMemory(); });
afterEach(() => { vi.useRealTimers(); });

describe('Bot commands from the catalog on this device', () => {
  it('the picker lists Bot commands from the local catalog with no request at open', async () => {
    const h = harness();
    h.open('/');
    expect(await screen.findByRole('option', { name: /^\/remind,/u })).toBeInTheDocument();
    expect(screen.getByRole('option', { name: /^\/play,/u })).toBeInTheDocument();
    // The list is read from the slice: opening it sent nothing at all (no status read, no capability round trip).
    expect(h.fake.calls).toEqual([]);
  });

  it('a pushed catalog replaces the list without a fetch', async () => {
    const h = harness();
    h.open('/');
    await screen.findByRole('option', { name: /^\/remind,/u });
    h.fake.publish(`botCatalog:${COMMUNITY}`, fixtureCatalog({ revision: '4', commands: [{ name: 'weather', description: 'Today’s weather',
      operation: 99, args: [], permissionBit: 0, needsBotChannel: false, handlerKind: 0 }] }));
    expect(await screen.findByRole('option', { name: /^\/weather,/u })).toBeInTheDocument();
    expect(screen.queryByRole('option', { name: /^\/remind,/u })).toBeNull();
    expect(h.fake.calls).toEqual([]);
  });

  it('an offline or degraded Bot greys commands with the reason and the time', async () => {
    const since = 1_758_659_040;
    const h = harness(fixtureCatalog({ status: { state: 'offline', since } }));
    h.open('/');
    const reason = `The Bot is offline since ${botSince(since)}`;
    // Greyed, not hidden: the row stays and says why.
    expect(await screen.findByRole('option', { name: `/play, ${reason}` })).toBeInTheDocument();
    h.fake.publish(`botCatalog:${COMMUNITY}`, fixtureCatalog({ status: { state: 'degraded', since } }));
    expect(await screen.findByRole('option', { name: `/play, The Bot is having trouble since ${botSince(since)}` })).toBeInTheDocument();
  });

  it('a greyed command does not run: its bar says why and Enter sends nothing', async () => {
    const since = 1_758_659_040;
    const h = harness(fixtureCatalog({ status: { state: 'offline', since } }));
    h.open('/pause');
    expect(await screen.findByText(`The Bot is offline since ${botSince(since)}`)).toBeInTheDocument();
    await h.user.click(screen.getByRole('button', { name: 'send' }));
    fireEvent.keyDown(document.activeElement ?? document.body, { key: 'Enter' });
    await act(async () => { await Promise.resolve(); });
    expect(h.fake.callsOf('sendBotCommand')).toEqual([]);
  });

  it('posting commands with no Bot channel are greyed with Mark a channel as a Bot channel first', async () => {
    const h = harness(fixtureCatalog({ botChannels: [] }));
    h.open('/');
    expect(await screen.findByRole('option', { name: '/rolepanel create, Mark a channel as a Bot channel first' })).toBeInTheDocument();
    // A command that posts nowhere is not greyed.
    expect(screen.getByRole('option', { name: /^\/remind, A private one-time reminder/u })).toBeInTheDocument();
  });

  it('Enter sends one sealed request and shows the first answer', async () => {
    const h = harness();
    h.open('/queue');
    await h.user.click(await screen.findByRole('button', { name: 'send' }));
    await waitFor(() => expect(h.fake.callsOf('sendBotCommand')).toHaveLength(1));
    expect(h.fake.callsOf('sendBotCommand')[0]).toMatchObject({ m: 'sendBotCommand', communityId: COMMUNITY, channelId: CHANNEL, operation: 10, args: [] });
    h.answer({ seq: 0, status: 200, body: text('1. Song — Artist'), final: true });
    await waitFor(() => expect(h.results()[0]?.text).toBe('1. Song — Artist'));
    expect(h.results()[0]?.status).toBe('success');
    expect(h.fake.callsOf('sendBotCommand')).toHaveLength(1);
  });

  it('sends each argument in the catalog’s shape', async () => {
    const h = harness();
    h.open('/volume 40');
    await h.user.click(await screen.findByRole('button', { name: 'send' }));
    await waitFor(() => expect(h.fake.callsOf('sendBotCommand')).toHaveLength(1));
    expect(h.fake.callsOf('sendBotCommand')[0]).toMatchObject({ operation: 18, args: [40] });
  });

  it('two streamed answers render in order', async () => {
    const h = harness();
    h.open('/nowplaying');
    await h.user.click(await screen.findByRole('button', { name: 'send' }));
    await waitFor(() => expect(h.fake.callsOf('sendBotCommand')).toHaveLength(1));
    h.answer({ seq: 0, status: 200, body: text('Searching…'), final: false });
    await waitFor(() => expect(h.results()[0]?.text).toBe('Searching…'));
    expect(h.results()[0]?.status).toBe('pending');
    h.answer({ seq: 1, status: 200, body: text('Now playing: Song'), final: true });
    await waitFor(() => expect(h.results()[0]?.text).toBe('Now playing: Song'));
    expect(h.results()[0]?.status).toBe('success');
  });

  it('no answer in 5 s shows The Bot didn\'t answer with Retry; Retry reuses the request id', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const h = harness(fixtureCatalog(), userEvent.setup({ advanceTimers: vi.advanceTimersByTime }));
    h.open('/queue');
    await h.user.click(await screen.findByRole('button', { name: 'send' }));
    await waitFor(() => expect(h.fake.callsOf('sendBotCommand')).toHaveLength(1));
    await act(async () => { await vi.advanceTimersByTimeAsync(5_000); });
    await waitFor(() => expect(h.results()[0]?.text).toBe("The Bot didn't answer"));
    const retry = await screen.findByRole('button', { name: 'Retry' });
    await h.user.click(retry);
    await waitFor(() => expect(h.fake.callsOf('sendBotCommand')).toHaveLength(2));
    const [first, second] = h.fake.callsOf('sendBotCommand');
    expect(second.requestId).toBe(first.requestId);
    h.answer({ seq: 0, status: 200, body: text('The queue is empty.'), final: true });
    await waitFor(() => expect(h.results().at(-1)?.text).toBe('The queue is empty.'));
  });

  it('greyed rows are aria-disabled, with no inline style (I1), and a key this device cannot check or a changed Bot say so (M10, A)', async () => {
    const h = harness(fixtureCatalog({ status: { state: 'unverified', since: null } }));
    h.open('/');
    const row = await screen.findByRole('option', { name: "/play, This device can't check the Bot yet" });
    expect(row).toHaveAttribute('aria-disabled', 'true');
    expect(document.querySelector('style')).toBeNull();
    h.fake.publish(`botCatalog:${COMMUNITY}`, fixtureCatalog({ status: { state: 'changed', since: null } }));
    expect(await screen.findByRole('option', { name: "/play, This server's Bot account changed, so its commands are paused" })).toHaveAttribute('aria-disabled', 'true');
  });

  it('a refusal says why: a disabled Bot, no Bot channel, no permission (M3)', async () => {
    const h = harness();
    h.fake.handler = command => command.m === 'sendBotCommand'
      ? Promise.reject(Object.assign(new Error('E_FORBIDDEN'), { code: 'E_FORBIDDEN', status: 403 })) : Promise.resolve(null);
    h.open('/queue');
    await h.user.click(await screen.findByRole('button', { name: 'send' }));
    await waitFor(() => expect(h.results()[0]?.text).toBe('The Bot is turned off in this server'));
    h.fake.handler = () => Promise.resolve(null);
    h.open('/pause');
    await h.user.click(await screen.findByRole('button', { name: 'send' }));
    await waitFor(() => expect(h.fake.callsOf('sendBotCommand')).toHaveLength(2));
    h.answer({ seq: 0, status: 409, body: text('E_NOT_BOT_CHANNEL'), final: true });
    await waitFor(() => expect(h.results().at(-1)?.text).toBe('Mark a channel as a Bot channel first'));
    h.open('/skip');
    await h.user.click(await screen.findByRole('button', { name: 'send' }));
    await waitFor(() => expect(h.fake.callsOf('sendBotCommand')).toHaveLength(3));
    h.answer({ seq: 0, status: 403, body: '', final: true });
    await waitFor(() => expect(h.results().at(-1)?.text).toBe("You can't use this command here"));
  });

  it('an answer that comes after the timeout, before Retry, is shown and the Retry offer goes (M4)', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const h = harness(fixtureCatalog(), userEvent.setup({ advanceTimers: vi.advanceTimersByTime }));
    h.open('/queue');
    await h.user.click(await screen.findByRole('button', { name: 'send' }));
    await waitFor(() => expect(h.fake.callsOf('sendBotCommand')).toHaveLength(1));
    await act(async () => { await vi.advanceTimersByTimeAsync(5_000); });
    expect(await screen.findByRole('button', { name: 'Retry' })).toBeInTheDocument();
    h.answer({ seq: 0, status: 200, body: text('Late but here'), final: true });
    await waitFor(() => expect(h.results()[0]).toMatchObject({ status: 'success', text: 'Late but here' }));
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull());
    expect(h.fake.callsOf('sendBotCommand')).toHaveLength(1);
  });

  it('closing the composer ends a pending run and forgets its reply secret (M5)', async () => {
    const h = harness();
    h.open('/queue');
    await h.user.click(await screen.findByRole('button', { name: 'send' }));
    await waitFor(() => expect(h.fake.callsOf('sendBotCommand')).toHaveLength(1));
    const [sent] = h.fake.callsOf('sendBotCommand');
    h.unmount();
    await waitFor(() => expect(h.fake.callsOf('forgetBotCommand')).toEqual([{ m: 'forgetBotCommand', requestId: sent.requestId }]));
  });

  it('nothing polls: 60 s after a command, no further request', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const h = harness(fixtureCatalog(), userEvent.setup({ advanceTimers: vi.advanceTimersByTime }));
    h.open('/queue');
    await h.user.click(await screen.findByRole('button', { name: 'send' }));
    await waitFor(() => expect(h.fake.calls).toHaveLength(1));
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(h.fake.calls.map(c => c.m)).toEqual(['sendBotCommand']);
  });
});
