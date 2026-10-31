import { createElement, useEffect } from 'react';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { COMMANDS, decode, encode as importEncode, fromHex, PERM, type BotUtilities, type CoreClient, type CommandContext as Binding, type CommandResult, type PollSummary, type ReminderSummary } from '@dilla/client-core';
import * as ClientCore from '@dilla/client-core';
import { FakeClient } from '../../test/fake-client.ts';
import type { CommandExecutionContext } from './command-types.ts';
import { finalPollMarker, PollCard } from './PollCard.tsx';
import { CommandPicker } from './CommandPicker.tsx';
import { notifyReminder, reminderFromArgs, useUtilityCommandProvider } from './UtilityCommandProvider.tsx';

const COMMUNITY = '11'.repeat(16); const CHANNEL = '22'.repeat(16); const USER = '33'.repeat(16);
const INSTALLATION = '44'.repeat(16); const BOT = '55'.repeat(16); const POLL = '77'.repeat(16);
const wire = (value: unknown): Uint8Array => importEncode(value as never);
const utf8 = (value: string): Uint8Array => new Uint8Array(new TextEncoder().encode(value));
const asBytes = (id: string) => fromHex(id); const toHex = (id: Uint8Array) => Array.from(id, byte => byte.toString(16).padStart(2, '0')).join('');
const supported = [3, 70, 71, 72, 73, 74, 75, 80, 81, 82, 83, 84].map(BigInt);
function pollSummary(over: Partial<PollSummary> = {}): PollSummary { return { id: POLL, channelId: CHANNEL, threadId: null, creatorId: USER, question: 'Lunch?', answers: ['Soup', 'Salad'], multi: true, createdAt: 1_800_000_000n, dueAt: 1_800_003_600n, closed: false, counts: [0n, 0n], myChoices: [], revision: 1n, ...over }; }
function pollWire(p: PollSummary): unknown[] { return [asBytes(p.id), asBytes(p.channelId), p.threadId === null ? null : asBytes(p.threadId), asBytes(p.creatorId), p.question, p.answers, p.multi ? 1n : 0n, p.createdAt, p.dueAt, p.closed ? 1n : 0n, p.counts, p.myChoices?.map(BigInt) ?? null, p.revision]; }
function utilityFixture() {
  const core = new FakeClient();
  core.set('account', ({ phase: 'ready', user: { id: USER, username: 'ada' }, deviceId: '88'.repeat(16), instance: { id: '99'.repeat(16), name: 'test', registrationMode: 0, passwordSignup: true, totp: false, passkeys: false }, recoveryKey: null, error: null, signIn: null, factorSetup: null, rootMismatch: null, ownTrust: [] } as never));
  core.set('communities', [{ id: COMMUNITY, name: 'Test', owner: USER }]);
  const everyone = '66'.repeat(16);
  core.set(`permissions:${COMMUNITY}`, { loaded: true, everyoneId: everyone, overwritesLoaded: true } as never);
  core.set(`roles:${COMMUNITY}`, [{ id: everyone, position: 0, allow: PERM.viewChannel | PERM.sendMessages, deny: 0 }] as never);
  core.set(`overwrites:${COMMUNITY}`, {});
  core.set(`channels:${COMMUNITY}`, [{ id: CHANNEL, communityId: COMMUNITY, kind: 0, mode: 0, name: 'lobby', topic: '', parentId: null, position: 0, slowmode: 0, announcement: false, group: 'active' }]);
  core.set(`members:${COMMUNITY}`, [{ userId: USER, username: 'ada', display: 'Ada', kind: 0, roleIds: [] }]);
  const requests: { operation: number; requestId: string; args: readonly unknown[] }[] = [];
  const responses: { deliveryId: string; requestId: string; communityId: string; operation: number; channelId: null; status: number; revision: string | null; data: Uint8Array }[] = [];
  let row = pollSummary(); let reminder: unknown[] | null = null; let delivery = 0; let failOperation: number | null = null; let failApplies = true;
  core.handler = command => Promise.resolve().then(() => {
    if (command.m === 'getBotCommandStatus') return { installationId: INSTALLATION, botUserId: BOT, state: 3, revision: '1' };
    if (command.m === 'humanInteractionRequest') {
      const operation = command.operation; const id = command.requestId; const args = command.args; requests.push({ operation, requestId: id, args });
      let data = wire([]); let revision: string | null = null; let status = 0;
      if (operation === 3) data = wire([1n, asBytes(INSTALLATION), asBytes(BOT), 1n, supported]);
      if (operation === 70) { const payload = decode(args[0] as Uint8Array) as unknown[]; const answers = payload[4] as string[];
        row = pollSummary({ id, channelId: toHex(payload[1] as Uint8Array), threadId: payload[2] === null ? null : toHex(payload[2] as Uint8Array), question: String(payload[3]), answers, multi: payload[5] === 1n, dueAt: 1_800_000_000n + BigInt(payload[6] as bigint) });
        data = wire([1n, pollWire(row)]); revision = '1'; }
      if (operation === 71) { data = wire([1n, pollWire(row)]); revision = row.revision.toString(); }
      if (operation === 72) data = wire([1n, [pollWire(row)], null]);
      if (operation === 73 && !(failOperation === operation && !failApplies)) { const choices = decode(args[1] as Uint8Array) as bigint[]; const next = row.revision + 1n;
        row = { ...row, revision: next, myChoices: choices.map(Number), counts: row.answers.map((_, index) => BigInt(choices.includes(BigInt(index)) ? 1 : 0)) };
        data = wire([1n, pollWire(row)]); revision = next.toString(); }
      if (operation === 74 && !(failOperation === operation && !failApplies)) { row = { ...row, closed: true, revision: row.revision + 1n }; data = wire([1n, pollWire(row)]); revision = row.revision.toString(); }
      if (operation === 75) data = wire([1n, [asBytes(USER)], null]);
      if (operation === 80) { const payload = decode(args[0] as Uint8Array) as unknown[];
        reminder = [asBytes(id), payload[3], BigInt(Math.floor(Date.now() / 1000)) + BigInt(payload[2] as bigint), payload[4], payload[5], payload[6], 0n, 1n]; data = wire([1n, reminder]); revision = '1'; }
      if (operation === 81) data = wire([1n, reminder === null ? [] : [reminder], null]);
      if (operation === 82 || operation === 83) if (reminder !== null && !(failOperation === operation && !failApplies)) { const next = BigInt(reminder[7] as bigint) + 1n; reminder = [...reminder.slice(0, 6), operation === 82 ? 2n : 3n, next]; data = wire([1n, reminder]); revision = String(next); }
      if (failOperation === operation) { status = 5; failOperation = null; failApplies = true; }
      responses.push({ deliveryId: String(++delivery), requestId: id, communityId: COMMUNITY, operation, channelId: null, status, revision, data });
      return { requestId: id, upload: 'accepted' };
    }
    if (command.m === 'readBotInteractionResponses') { const operations = command.operations as number[]; const ids = command.requestIds as string[] | undefined;
      return { responses: responses.filter(response => operations.includes(response.operation) && (ids === undefined || ids.includes(response.requestId))), through: null, blocked: false }; }
    if (command.m === 'ackBotInteractionResponses') return null; return null;
  });
  return { client: core, requests, responses, get poll() { return row; }, setPoll(over: Partial<PollSummary>) { row = { ...row, ...over }; }, get reminder() { return reminder; }, failNext(operation: number, apply = true) { failOperation = operation; failApplies = apply; },
    markReminderDue() { if (reminder !== null) reminder = [...reminder.slice(0, 2), BigInt(Math.floor(Date.now() / 1000)) - 60n, ...reminder.slice(3, 6), 1n, BigInt(reminder[7] as bigint) + 1n]; } };
}
function commandContext(client: CoreClient, requestId: string, over: Partial<CommandExecutionContext> = {}): CommandExecutionContext {
  const binding: Binding = { communityId: COMMUNITY, channelId: CHANNEL, bits: PERM.manageMessages | PERM.sendMessages };
  return { requestId, binding, getCurrentBinding: () => binding, isCurrent: () => true, client, getCall: () => ({} as never), call: {} as never,
    navigate: vi.fn(), openThread: vi.fn(), openSearch: vi.fn(), items: [], selfId: USER, encodeMessage: value => value, confirm: () => Promise.resolve(true), openInvite: () => Promise.resolve(true), openGif: vi.fn(), openModerationLog: vi.fn(), ...over };
}
function mount(client: CoreClient, community: string | null = COMMUNITY, onNavigate?: (communityId: string, channelId: string, threadId: string | null) => void) { const captured: { value?: ReturnType<typeof useUtilityCommandProvider> } = {};
  function Probe() { const value = useUtilityCommandProvider(client, community, onNavigate); useEffect(() => { captured.value = value; }); return createElement('div', null, value.dialogs, value.inbox); }
  const view = render(createElement(Probe)); return { captured, ...view, rerenderProbe: () => view.rerender(createElement(Probe)) }; }
afterEach(() => { vi.restoreAllMocks(); });

describe('utility command provider', () => {
  it('/poll create from the picker opens the New poll dialog', async () => {
    const f = utilityFixture();
    function Picker() {
      const provider = useUtilityCommandProvider(f.client, COMMUNITY);
      return <>{provider.dialogs}<CommandPicker definitions={COMMANDS} initialText="/" contextKey="poll-picker-test"
        suggest={() => Promise.resolve([])} onExecute={() => Promise.resolve({ status: 'success', private: true, message: '' })} onCancel={() => {}} onSendLiteral={() => {}}
        onRunDetached={invocation => { void provider.handlers['poll create']!(invocation, commandContext(f.client, 'af'.repeat(16))); }} /> </>;
    }
    render(<Picker />);
    await userEvent.click(screen.getByRole('option', { name: /^\/poll create,/ }));
    expect(await screen.findByRole('dialog', { name: 'New poll in #lobby' })).toBeInTheDocument();
    expect(screen.getByPlaceholderText('the bridge')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
  });
  it('publishes a poll through the adapter from the focused create dialog', async () => {
    const user = userEvent.setup(); const f = utilityFixture(); const mounted = mount(f.client); await waitFor(() => expect(mounted.captured.value).toBeDefined());
    let run!: Promise<unknown>; act(() => { run = mounted.captured.value!.handlers['poll create']!({ commandId: 'poll create', args: { question: 'Lunch?', answers: 'Soup, Salad', multiple: 'on' } }, commandContext(f.client, 'aa'.repeat(16))); });
    await screen.findByRole('heading', { name: 'New poll in #lobby' }); await user.click(screen.getByRole('button', { name: 'Post poll' }));
    const result = await run; expect(result).toMatchObject({ status: 'success', private: true, message: 'Poll posted' });
    expect(f.requests.filter(row => row.operation === 70)).toHaveLength(1);
    const created = f.requests.find(row => row.operation === 70)!; expect(created.requestId).toBe('aa'.repeat(16));
    expect(decode(created.args[0] as Uint8Array)).toMatchObject([1n, asBytes(CHANNEL), null, 'Lunch?', ['Soup', 'Salad'], 1n, 86_400n]);
    expect(f.poll.id).toBe('aa'.repeat(16)); mounted.unmount();
  });
  it('keeps the edited poll body and request id through an uncertain retry', async () => {
    const user = userEvent.setup(); const f = utilityFixture(); const mounted = mount(f.client); await waitFor(() => expect(mounted.captured.value).toBeDefined());
    const invocation = { commandId: 'poll create', args: { question: 'Original?', answers: 'A, B' } };
    const context = commandContext(f.client, 'ac'.repeat(16)); let run!: Promise<unknown>;
    act(() => { run = mounted.captured.value!.handlers['poll create']!(invocation, context); });
    await screen.findByRole('heading', { name: 'New poll in #lobby' });
    await user.clear(screen.getByLabelText('Question')); await user.type(screen.getByLabelText('Question'), 'Edited?');
    f.failNext(70); await user.click(screen.getByRole('button', { name: 'Post poll' })); expect(await run).toMatchObject({ status: 'uncertain' });
    const botPost = { key: 's9', state: 'ok' as const, reason: '', senderUser: BOT, senderDevice: null, own: false, web: false, bot: true,
      ts: 1_800_000_000, body: `Edited?\n[dilla-poll:v1:${context.requestId}]`, msgId: 'bc'.repeat(16), seq: '9', edited: false, reply: null,
      reactions: [], pinned: false, attachments: [], mention: false, actions: [], thread: null };
    const retryContext = commandContext(f.client, context.requestId, { items: [botPost] });
    act(() => { run = mounted.captured.value!.handlers['poll create']!(invocation, context); });
    expect(await run).toMatchObject({ status: 'uncertain' });
    act(() => { run = mounted.captured.value!.handlers['poll create']!(invocation, retryContext); });
    expect(await run).toMatchObject({ status: 'success', message: 'Poll posted' });
    const writes = f.requests.filter(row => row.operation === 70); expect(writes).toHaveLength(1);
    expect(new Set(writes.map(row => row.requestId))).toEqual(new Set(['ac'.repeat(16)]));
    const payloads = writes.map(row => decode(row.args[0] as Uint8Array) as unknown[]);
    expect(payloads.every(payload => JSON.stringify(payload, (_key, value: unknown) => typeof value === 'bigint' ? value.toString() : value) === JSON.stringify(payloads[0], (_key, value: unknown) => typeof value === 'bigint' ? value.toString() : value))).toBe(true);
    expect(payloads[0]?.[3]).toBe('Edited?'); mounted.unmount();
  });
  it('keeps a pending poll create uncertain when send permission disappears', async () => {
    const user = userEvent.setup(); const f = utilityFixture(); const mounted = mount(f.client); await waitFor(() => expect(mounted.captured.value).toBeDefined());
    let bits = PERM.sendMessages; const context = commandContext(f.client, 'b1'.repeat(16), { getCurrentBinding: () => ({ communityId: COMMUNITY, channelId: CHANNEL, bits }) });
    const invoke = () => mounted.captured.value!.handlers['poll create']!({ commandId: 'poll create', args: { question: 'Q?', answers: 'A, B' } }, context);
    let run!: Promise<unknown>; act(() => { run = invoke(); }); await screen.findByRole('heading', { name: 'New poll in #lobby' });
    f.failNext(70); await user.click(screen.getByRole('button', { name: 'Post poll' })); expect(await run).toMatchObject({ status: 'uncertain' });
    bits = 0; expect(await invoke()).toMatchObject({ status: 'uncertain' });
    expect(f.requests.filter(row => row.operation === 70).map(row => row.requestId)).toEqual(['b1'.repeat(16)]); mounted.unmount();
  });
  it('reconciles an accepted reminder from its owner list before opening a retry dialog', async () => {
    const f = utilityFixture(); const mounted = mount(f.client); await waitFor(() => expect(mounted.captured.value).toBeDefined());
    const context = commandContext(f.client, 'b2'.repeat(16)); const invoke = () => mounted.captured.value!.handlers.remind!({ commandId: 'remind', args: { when: 'in 1h', text: 'Keep one copy' } }, context);
    let run!: Promise<unknown>; f.failNext(80); act(() => { run = invoke(); });
    expect(await run).toMatchObject({ status: 'uncertain' }); expect(f.reminder).not.toBeNull();
    act(() => { run = invoke(); }); expect(await run).toMatchObject({ status: 'success' });
    expect(screen.queryByRole('heading', { name: 'Set a reminder' })).not.toBeInTheDocument();
    expect(f.requests.filter(row => row.operation === 80).map(row => row.requestId)).toEqual(['b2'.repeat(16)]); mounted.unmount();
  });
  it('returns an explicit reminder list only after its owned snapshot completes', async () => {
    const f = utilityFixture(); const mounted = mount(f.client); await waitFor(() => expect(mounted.captured.value).toBeDefined());
    let created!: Promise<unknown>;
    act(() => { created = mounted.captured.value!.handlers.remind!({ commandId: 'remind', args: { when: 'in 1h', text: 'Listed reminder' } }, commandContext(f.client, 'b4'.repeat(16))); });
    expect(await created).toMatchObject({ status: 'success' });
    let result: CommandResult | undefined; act(() => { void mounted.captured.value!.handlers['reminders list']!({ commandId: 'reminders list', args: {} }, commandContext(f.client, 'b5'.repeat(16))).then(value => { result = value; }); });
    await waitFor(() => { expect(result?.status).toBe('success'); expect(result?.message).toContain('Listed reminder'); }); mounted.unmount();
  });
  it.each(['reminders cancel', 'reminders dismiss'] as const)('%s resolves its first target lookup before dispatch', async commandId => {
    const f = utilityFixture(); const mounted = mount(f.client); await waitFor(() => expect(mounted.captured.value).toBeDefined());
    let created!: Promise<unknown>;
    act(() => { created = mounted.captured.value!.handlers.remind!({ commandId: 'remind', args: { when: 'in 1h', text: `Target for ${commandId}` } }, commandContext(f.client, 'b6'.repeat(16))); });
    await created;
    let result: CommandResult | undefined; act(() => { void mounted.captured.value!.handlers[commandId]!({ commandId, args: { reminder: 'b6'.repeat(16) } }, commandContext(f.client, 'b7'.repeat(16))).then(value => { result = value; }); });
    await waitFor(() => expect(result).toMatchObject({ status: 'success' })); expect(f.requests.some(row => row.operation === (commandId.endsWith('cancel') ? 82 : 83))).toBe(true); mounted.unmount();
  });
  it('reconciles timezone aliases using the adapter canonical zone', async () => {
    const f = utilityFixture(); const mounted = mount(f.client); await waitFor(() => expect(mounted.captured.value).toBeDefined());
    const invocation = { commandId: 'remind', args: { when: 'in 1h', text: 'Alias zone', timezone: 'US/Eastern' } }; const context = commandContext(f.client, 'b8'.repeat(16)); let run!: Promise<unknown>;
    f.failNext(80); act(() => { run = mounted.captured.value!.handlers.remind!(invocation, context); });
    expect(await run).toMatchObject({ status: 'uncertain' });
    act(() => { run = mounted.captured.value!.handlers.remind!(invocation, context); }); expect(await run).toMatchObject({ status: 'success' });
    expect(screen.queryByRole('heading', { name: 'Set a reminder' })).not.toBeInTheDocument(); mounted.unmount();
  });
  it('keeps the poll question limit in Unicode code points', async () => {
    const user = userEvent.setup(); const f = utilityFixture(); const mounted = mount(f.client); await waitFor(() => expect(mounted.captured.value).toBeDefined());
    let run!: Promise<unknown>; act(() => { run = mounted.captured.value!.handlers['poll create']!({ commandId: 'poll create', args: { question: 'Q', answers: 'A, B' } }, commandContext(f.client, 'b0'.repeat(16))); });
    await screen.findByRole('heading', { name: 'New poll in #lobby' }); const question = screen.getByLabelText('Question');
    fireEvent.change(question, { target: { value: '😀'.repeat(300) } }); expect(question).toHaveValue('😀'.repeat(300));
    expect(screen.getByText('300 / 300')).toBeInTheDocument();
    fireEvent.change(question, { target: { value: '😀'.repeat(301) } }); expect(question).toHaveValue('😀'.repeat(301)); expect(screen.getByText('301 / 300')).toHaveAttribute('data-danger', 'true');
    await user.click(screen.getByRole('button', { name: 'Cancel' })); await run; mounted.unmount();
  });
  it('closes a creation dialog when live send permission is revoked', async () => {
    const f = utilityFixture(); const mounted = mount(f.client); await waitFor(() => expect(mounted.captured.value).toBeDefined());
    let bits = PERM.sendMessages | PERM.manageMessages; const context = commandContext(f.client, 'b1'.repeat(16), { getCurrentBinding: () => ({ communityId: COMMUNITY, channelId: CHANNEL, bits }) });
    let run!: Promise<unknown>; act(() => { run = mounted.captured.value!.handlers['poll create']!({ commandId: 'poll create', args: { question: 'Q?', answers: 'A, B' } }, context); });
    await screen.findByRole('heading', { name: 'New poll in #lobby' }); bits = PERM.manageMessages;
    await waitFor(() => expect(screen.queryByRole('heading', { name: 'New poll in #lobby' })).not.toBeInTheDocument());
    expect(await run).toMatchObject({ status: 'failure' }); expect(f.requests.some(row => row.operation === 70)).toBe(false); mounted.unmount();
  });
  it('replaces a vote with the selected answer and reports the returned count', async () => {
    const f = utilityFixture(); const mounted = mount(f.client); await waitFor(() => expect(mounted.captured.value).toBeDefined());
    const result = await mounted.captured.value!.handlers['poll vote']!({ commandId: 'poll vote', args: { poll: POLL, choice: '2' } }, commandContext(f.client, 'bb'.repeat(16)));
    expect(result).toMatchObject({ status: 'success' }); const mutation = f.requests.find(row => row.operation === 73)!;
    expect(mutation.requestId).toBe('bb'.repeat(16)); expect(decode(mutation.args[1] as Uint8Array)).toEqual([1n]);
    expect(f.poll.myChoices).toEqual([1]); expect(f.poll.counts).toEqual([0n, 1n]); mounted.unmount();
  });
  it('requires an explicit DST fold choice before creating an ambiguous reminder', async () => {
    const f = utilityFixture(); const mounted = mount(f.client); await waitFor(() => expect(mounted.captured.value).toBeDefined());
    const args = { when: '2026-11-01 01:30', text: 'Call home', timezone: 'America/New_York' };
    expect(await mounted.captured.value!.handlers.remind!({ commandId: 'remind', args }, commandContext(f.client, 'cc'.repeat(16))))
      .toMatchObject({ status: 'failure', message: 'This local time occurs twice. Choose which time you mean.' });
    // The picker sends the later candidate the person chose from the two (Wave B Task 2: the chips, not a dialog).
    const result = await mounted.captured.value!.handlers.remind!({ commandId: 'remind', args: { ...args, chosen: '1793514600' } }, commandContext(f.client, 'cc'.repeat(16))); expect(result).toMatchObject({ status: 'success' }); const mutation = f.requests.find(row => row.operation === 80)!;
    expect(mutation.requestId).toBe('cc'.repeat(16)); const payload = decode(mutation.args[0] as Uint8Array) as unknown[];
    expect(payload[1]).toBe(1n); expect(payload[4]).toBe('America/New_York'); expect(payload[2]).toBe(1_793_514_600n); mounted.unmount();
  });

  it('shows due reminders in the inbox and reuses an uncertain dismissal attempt', async () => {
    const user = userEvent.setup(); const f = utilityFixture(); const mounted = mount(f.client, COMMUNITY, vi.fn());
    await waitFor(() => expect(mounted.captured.value).toBeDefined());
    await waitFor(() => expect(f.requests.some(row => row.operation === 81)).toBe(true));
    let create!: Promise<unknown>;
    act(() => { create = mounted.captured.value!.handlers.remind!({ commandId: 'remind', args: { when: 'in 1h', text: 'Call home' } }, commandContext(f.client, 'dd'.repeat(16))); });
    expect(await create).toMatchObject({ status: 'success' });
    f.markReminderDue();
    await act(async () => { await mounted.captured.value!.handlers['reminders list']!({ commandId: 'reminders list', args: {} }, commandContext(f.client, 'ee'.repeat(16))); });
    await waitFor(() => expect(screen.getByRole('button', { name: /1 due/ })).toBeInTheDocument());
    await user.click(await screen.findByRole('tab', { name: /^Due/ }));
    expect(await screen.findByText('Call home')).toBeInTheDocument();
    f.failNext(83, false); await user.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(await screen.findByText('The change may have been saved. Refresh reminders before trying again.')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: 'Dismiss' })).toBeInTheDocument());
    await user.click(screen.getByRole('button', { name: 'Dismiss' }));
    await waitFor(() => expect(f.requests.filter(row => row.operation === 83)).toHaveLength(1));
    expect(f.requests.find(row => row.operation === 83)?.requestId).toMatch(/^[0-9a-f]{32}$/u);
    mounted.unmount();
  });
  // README §6.7 / the prototype: a due reminder offers Dismiss (op 83), an upcoming one Cancel (op 82), each on its tab.
  it('cancels an upcoming reminder from the Upcoming tab; a due one offers Dismiss only', async () => {
    const user = userEvent.setup(); const f = utilityFixture(); const mounted = mount(f.client); await waitFor(() => expect(mounted.captured.value).toBeDefined());
    let create!: Promise<unknown>; act(() => { create = mounted.captured.value!.handlers.remind!({ commandId: 'remind', args: { when: 'in 1h', text: 'Cancel me' } }, commandContext(f.client, 'ad'.repeat(16))); });
    await create;
    await act(async () => { await mounted.captured.value!.handlers['reminders list']!({ commandId: 'reminders list', args: {} }, commandContext(f.client, 'ae'.repeat(16))); });
    await screen.findByText('Cancel me');
    expect(screen.getByRole('tab', { name: /^Upcoming/ })).toHaveAttribute('aria-selected', 'true');
    expect(screen.queryByRole('button', { name: 'Dismiss' })).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Cancel' })); await waitFor(() => expect(f.requests.some(row => row.operation === 82)).toBe(true));
    mounted.unmount();
  });
  it('does not dispatch an inbox mutation after the live account changes during capability lookup', async () => {
    const f = utilityFixture(); const real = ClientCore.createBotUtilities(f.client, COMMUNITY);
    await real.reminderCreate({ requestId: 'b3'.repeat(16), whenKind: 0, when: 3600, text: 'Owner scoped', timezone: 'UTC', sourceChannelId: CHANNEL });
    let hold = false; let release!: (value: Awaited<ReturnType<typeof real.capabilities>>) => void; const capProbe = vi.fn(); const cancel = vi.fn((input: Parameters<typeof real.reminderCancel>[0]) => real.reminderCancel(input));
    const delayed = new Promise<Awaited<ReturnType<typeof real.capabilities>>>(resolve => { release = resolve; });
    vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({ ...real, capabilities: () => { capProbe(); return hold ? delayed : real.capabilities(); }, reminderCancel: cancel });
    const user = userEvent.setup(); const mounted = mount(f.client); await waitFor(() => expect(f.requests.some(row => row.operation === 81)).toBe(true));
    await user.click(screen.getByRole('button', { name: /Reminders/ })); await screen.findByText('Owner scoped'); hold = true;
    const before = capProbe.mock.calls.length; await user.click(screen.getByRole('button', { name: 'Cancel' })); await waitFor(() => expect(capProbe.mock.calls.length).toBeGreaterThan(before));
    const changed = { ...f.client.get('account')!, user: { ...f.client.get('account')!.user!, id: 'ac'.repeat(16) } };
    act(() => f.client.set('account', changed)); release({ version: 1, installationId: INSTALLATION, botUserId: BOT, supportedOperations: [3, 82] });
    await waitFor(() => expect(cancel).not.toHaveBeenCalled()); mounted.unmount();
  });
  it('redacts a source link immediately when the source channel is no longer active', async () => {
    const user = userEvent.setup(); const f = utilityFixture(); const mounted = mount(f.client, COMMUNITY, vi.fn());
    let create!: Promise<unknown>; act(() => { create = mounted.captured.value!.handlers.remind!({ commandId: 'remind', args: { when: 'in 1h', text: 'Still private' } }, commandContext(f.client, 'af'.repeat(16))); });
    await create;
    // The text is the link to its source message (a button drawn as text); a source that went away leaves plain text.
    await user.click(screen.getByRole('button', { name: 'Reminders' })); await screen.findByRole('button', { name: 'Still private' });
    act(() => f.client.set(`channels:${COMMUNITY}`, f.client.get(`channels:${COMMUNITY}`)!.map(row => ({ ...row, group: 'removed' })) as never)); mounted.rerenderProbe();
    expect(screen.queryByRole('button', { name: 'Still private' })).not.toBeInTheDocument(); expect(screen.getByText('Still private')).toBeInTheDocument();
    mounted.unmount();
  });

  it('keeps the reminders inbox available when no community composer is active', async () => {
    const f = utilityFixture(); const mounted = mount(f.client, null);
    await waitFor(() => expect(mounted.captured.value).toBeDefined());
    expect(screen.getByRole('button', { name: 'Reminders' })).toBeInTheDocument();
    expect(mounted.captured.value!.handlers.remind).toBeUndefined();
    mounted.unmount();
  });
  // e2e slash-commands (reminders sync): opened while the list still loads, the popover's tab follows the list once it arrives.
  it('opened while loading, the popover lands on Due once a due reminder arrives', async () => {
    const user = userEvent.setup(); const f = utilityFixture();
    let release!: (page: { items: ReminderSummary[]; nextCursor: Uint8Array | null }) => void;
    const pending = new Promise<{ items: ReminderSummary[]; nextCursor: Uint8Array | null }>(resolve => { release = resolve; });
    vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({ capabilities: () => Promise.resolve({ version: 1, installationId: INSTALLATION, botUserId: BOT, supportedOperations: [3, 80, 81, 82, 83, 84] }),
      reminderList: () => pending } as unknown as BotUtilities);
    const mounted = mount(f.client, null);
    await user.click(screen.getByRole('button', { name: /Reminders/ }));
    expect(screen.getByRole('tab', { name: /^Upcoming/ })).toHaveAttribute('aria-selected', 'true');
    release({ items: [{ id: '01'.padStart(32, '0'), text: 'Due soon', dueUnixSeconds: 1_800_000_001n,
      timezone: 'UTC', sourceChannelId: CHANNEL, sourceThreadId: null, state: 'due', revision: 1n }], nextCursor: null });
    expect(await screen.findByText('Due soon')).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: /^Due/ })).toHaveAttribute('aria-selected', 'true');
    mounted.unmount();
  });
  it('loads every page at the 100 reminder quota, including the last records', async () => {
    const user = userEvent.setup(); const f = utilityFixture(); const pages: ReminderSummary[][] = Array.from({ length: 5 }, (_, page) => Array.from({ length: 20 }, (_unused, index) => {
      const n = page * 20 + index + 1;
      return { id: n.toString(16).padStart(32, '0'), communityId: COMMUNITY, text: `Reminder ${n}`, createdAt: 1_800_000_000n,
        dueUnixSeconds: 1_800_000_000n + BigInt(n), timezone: 'UTC', sourceChannelId: CHANNEL, sourceThreadId: null,
        state: n % 3 === 0 ? 'due' : 'scheduled', revision: 1n };
    }));
    const listPage = vi.fn((cursor?: Uint8Array | null) => { const index = cursor?.[0] ?? 0; return Promise.resolve({ items: pages[index] ?? [], nextCursor: index < 4 ? new Uint8Array([index + 1]) : null }); });
    vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({ capabilities: () => Promise.resolve({ version: 1, installationId: INSTALLATION, botUserId: BOT, supportedOperations: [3, 80, 81, 82, 83, 84] }),
      reminderList: listPage } as unknown as BotUtilities);
    const mounted = mount(f.client, null);
    await waitFor(() => expect(listPage).toHaveBeenCalledTimes(5));
    await user.click(screen.getByRole('button', { name: /Reminders/ }));
    // 33 of them are due (the Due tab, picked first); the other 67 are on Upcoming.
    expect(await screen.findByText('Reminder 81')).toBeInTheDocument(); expect(screen.getAllByRole('listitem')).toHaveLength(33);
    expect(screen.getByRole('button', { name: /33 due/ })).toBeInTheDocument();
    await user.click(screen.getByRole('tab', { name: /^Upcoming/ }));
    expect(screen.getByText('Reminder 100')).toBeInTheDocument(); expect(screen.getAllByRole('listitem')).toHaveLength(67); mounted.unmount();
  });
  it('pages 100 long single-record responses through the real BotUtilities adapter', async () => {
    const f = utilityFixture(); const base = f.client.handler; const pageRequests: Uint8Array[] = [];
    f.client.handler = command => {
      if (command.m !== 'humanInteractionRequest' || command.operation !== 81) return base(command);
      const cursor = command.args[0] as Uint8Array | null;
      const dueCursor = cursor === null ? 0 : Number(BigInt(new TextDecoder().decode(cursor).split('/')[1] ?? '0') - 1_800_000_100n);
      const pageIndex = cursor === null ? 0 : dueCursor;
      pageRequests.push(cursor?.slice() ?? new Uint8Array());
      const rows = Array.from({ length: 1 }, (_unused, i) => {
        const n = pageIndex + i + 1; const id = n.toString(16).padStart(32, '0');
        return [asBytes(id), 'x'.repeat(1450) + ` ${n}`, 1_800_000_100n + BigInt(n), 'UTC', null, null, 1n, 1n];
      });
      const final = pageIndex === 99 ? null : new Uint8Array(new TextEncoder().encode(`${USER}/${(1_800_000_100n + BigInt((pageIndex + 1))).toString().padStart(20, '0')}/${((pageIndex + 1)).toString(16).padStart(32, '0')}`));
      f.responses.push({ deliveryId: String(1000 + pageRequests.length), requestId: command.requestId, communityId: COMMUNITY, operation: 81,
        channelId: null, status: 0, revision: null, data: wire([1n, rows, final]) });
      return Promise.resolve({ requestId: command.requestId, upload: 'accepted' });
    };
    const mounted = mount(f.client, null); await waitFor(() => expect(pageRequests).toHaveLength(100));
    expect(pageRequests[0]).toEqual(new Uint8Array()); expect(pageRequests.slice(1).every(cursor => cursor.length > 0)).toBe(true);
    expect(screen.getByRole('button', { name: /100 due/ })).toBeInTheDocument(); mounted.unmount();
  });
  it('hides fetched reminder data immediately when the owner changes', async () => {
    const user = userEvent.setup(); const f = utilityFixture(); const mounted = mount(f.client);
    await waitFor(() => expect(mounted.captured.value).toBeDefined());
    let create!: Promise<unknown>;
    act(() => { create = mounted.captured.value!.handlers.remind!({ commandId: 'remind', args: { when: 'in 1h', text: 'Private account reminder' } }, commandContext(f.client, 'ff'.repeat(16))); });
    expect(await create).toMatchObject({ status: 'success' }); f.markReminderDue();
    await act(async () => { await mounted.captured.value!.handlers['reminders list']!({ commandId: 'reminders list', args: {} }, commandContext(f.client, '01'.repeat(16))); });
    await user.click(await screen.findByRole('tab', { name: /^Due/ }));
    expect(await screen.findByText('Private account reminder')).toBeInTheDocument();
    const nextAccount = { ...f.client.get('account')!, user: { ...f.client.get('account')!.user!, id: 'ab'.repeat(16) } };
    act(() => f.client.set('account', nextAccount)); mounted.rerenderProbe();
    expect(screen.queryByText('Private account reminder')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /1 due/ })).not.toBeInTheDocument();
    mounted.unmount();
  });
});

it('notifies a newly due reminder once, while honoring DND and visibility', () => {
  const f = utilityFixture(); const entry = { communityId: COMMUNITY, reminder: {
    id: 'ab'.repeat(16), communityId: COMMUNITY, text: 'Check the oven', createdAt: 1n, dueUnixSeconds: 2n,
    timezone: 'UTC', sourceChannelId: CHANNEL, sourceThreadId: null, state: 'due' as const, revision: 1n,
  } };
  const constructor = vi.fn(function NotificationMock() { return { onclick: null, close: vi.fn() }; });
  Object.assign(constructor, { permission: 'granted' }); vi.stubGlobal('Notification', constructor);
  f.client.set('settings', { 'notify.default': 'everything' });
  let visible: DocumentVisibilityState = 'hidden';
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visible });
  const openInbox = vi.fn();
  const account = f.client.get('account')!;
  act(() => f.client.set('account', { ...account, presenceChoice: 'dnd' }));
  notifyReminder(entry, f.client, openInbox); expect(constructor).not.toHaveBeenCalled();
  act(() => f.client.set('account', { ...account, presenceChoice: 'online' }));
  visible = 'visible'; notifyReminder(entry, f.client, openInbox); expect(constructor).not.toHaveBeenCalled();
  visible = 'hidden'; notifyReminder(entry, f.client, openInbox); notifyReminder(entry, f.client, openInbox);
  expect(constructor).toHaveBeenCalledOnce();
  delete (document as Partial<Record<'visibilityState', unknown>>).visibilityState;
});

describe('poll card binding', () => {
  it('accepts only the exact final marker line', () => { const marker = `[dilla-poll:v1:${POLL}]`;
    expect(finalPollMarker(`note ${marker}\ntext`)).toBeNull(); expect(finalPollMarker(`${marker}\nanswer`)).toBeNull(); expect(finalPollMarker(`Question\n${marker}`)).toBe(POLL);
    expect(finalPollMarker(`Question\n${marker}\n`)).toBeNull(); expect(finalPollMarker(`Question\n[dilla-poll:v1:${'AB'.repeat(16)}]`)).toBeNull(); });
  it('rejects a human or edited marker before asking the Bot for any poll record', () => { const f = utilityFixture();
    const props = { client: f.client, communityId: COMMUNITY, channelId: CHANNEL, threadId: null, body: `[dilla-poll:v1:${POLL}]`, authorId: BOT, bot: false, edited: false, deleted: false, selfId: USER, bits: 0 };
    expect(render(createElement(PollCard, props)).container.firstChild).toBeNull(); expect(render(createElement(PollCard, { ...props, bot: true, edited: true })).container.firstChild).toBeNull(); expect(f.requests).toHaveLength(0); });
  it('rejects a bot marker when the authenticated author or backing thread does not match', async () => {
    const f = utilityFixture(); const props = { client: f.client, communityId: COMMUNITY, channelId: CHANNEL, threadId: null, body: `Lunch?\n[dilla-poll:v1:${POLL}]`, authorId: 'aa'.repeat(16), bot: true, edited: false, deleted: false, selfId: USER, bits: 0 };
    const { container } = render(createElement(PollCard, props)); expect(container.querySelector('h3')).toBeNull(); await waitFor(() => expect(f.requests.some(row => row.operation === 3)).toBe(true)); expect(f.requests.some(row => row.operation === 71)).toBe(false);
    render(createElement(PollCard, { ...props, authorId: BOT, threadId: 'ee'.repeat(16) }));
    await waitFor(() => expect(f.requests.some(row => row.operation === 71)).toBe(true));
    expect(screen.queryByRole('heading', { name: 'Lunch?' })).not.toBeInTheDocument();
  });
  it('loads a bot-authored record and supports multivote replacement, withdrawal, voters and creator close', async () => {
    const user = userEvent.setup(); const f = utilityFixture(); f.setPoll({ counts: [1n, 1n] });
    render(createElement(PollCard, { client: f.client, communityId: COMMUNITY, channelId: CHANNEL, threadId: null, body: `Lunch?\n[dilla-poll:v1:${POLL}]`, authorId: BOT, bot: true, edited: false, deleted: false, selfId: USER, bits: 0 }));
    expect(await screen.findByRole('heading', { name: 'Lunch?' })).toBeInTheDocument(); expect(f.requests.some(row => row.operation === 71)).toBe(true);
    await waitFor(() => expect(screen.getAllByRole('img', { name: 'Ada' })).toHaveLength(2));
    const choices = screen.getAllByRole('button', { name: /^(Soup|Salad)/ }); await user.click(choices[0]); await user.click(choices[1]);
    await waitFor(() => expect(f.poll.myChoices).toEqual([0, 1]));
    await user.click(choices[0]); await user.click(choices[1]); await waitFor(() => expect(f.poll.myChoices).toEqual([]));
    await user.click(screen.getByRole('button', { name: 'Close poll' })); await waitFor(() => expect(f.poll.closed).toBe(true));
    expect(f.requests.filter(row => row.operation === 73).map(row => row.requestId)).toHaveLength(4); expect(f.requests.some(row => row.operation === 74)).toBe(true);
  });
  it('finishes an authenticated poll read when its next refresh tick arrives before the response', async () => {
    const f = utilityFixture(); const base = f.client.handler;
    const real = ClientCore.createBotUtilities(f.client, COMMUNITY);
    vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({ ...real, get binding() { return real.binding; }, pollVoters: () => Promise.resolve({ voters: [], nextCursor: null }) });
    let tick!: () => void; let firstRead: (() => void) | undefined; let firstRequest: string | undefined;
    const setInterval = window.setInterval.bind(window);
    vi.spyOn(window, 'setInterval').mockImplementation(((fn: () => void, ms: number) => {
      if (ms === 30_000) { tick = fn; return 900001; }
      return setInterval(fn, ms);
    }) as never);
    f.client.handler = command => {
      if (command.m === 'humanInteractionRequest' && command.operation === 71) firstRequest ??= command.requestId;
      if (command.m === 'readBotInteractionResponses' && command.operations.includes(71)) {
        return new Promise(resolve => {
          if (command.requestIds?.includes(firstRequest!)) firstRead = () => { void base(command).then(resolve); };
        });
      }
      return base(command);
    };
    render(createElement(PollCard, { client: f.client, communityId: COMMUNITY, channelId: CHANNEL, threadId: null,
      body: `Lunch?\n[dilla-poll:v1:${POLL}]`, authorId: BOT, bot: true, edited: false, deleted: false, selfId: USER, bits: 0 }));
    await waitFor(() => expect(firstRead).toBeDefined());
    // The real Bot can need several gateway rounds, exceeding the 30-second refresh cadence.
    act(() => { tick(); });
    act(() => { firstRead!(); });
    expect(await screen.findByRole('heading', { name: 'Lunch?' })).toBeInTheDocument();
    expect(f.requests.filter(row => row.operation === 71)).toHaveLength(1);
    act(() => { tick(); });
    await waitFor(() => expect(f.requests.filter(row => row.operation === 71)).toHaveLength(2));
  });
  it.each([false, true])('keeps a pending walk and refreshes changed revisions on the next cadence (%s)', async changed => {
    const f = utilityFixture(); f.setPoll({ counts: [1n, 1n] }); let clock = performance.now(); vi.spyOn(performance, 'now').mockImplementation(() => clock); const real = ClientCore.createBotUtilities(f.client, COMMUNITY);
    let tick!: () => void; let release!: () => void; let started = false;
    const setInterval = window.setInterval.bind(window);
    vi.spyOn(window, 'setInterval').mockImplementation(((fn: () => void, ms: number) => {
      if (ms === 30_000) { tick = fn; return 900001; }
      return setInterval(fn, ms);
    }) as never);
    vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({ ...real, get binding() { return real.binding; }, pollVoters: async () => {
      if (!started) { started = true; return new Promise(resolve => { release = () => resolve({ voters: [USER], nextCursor: null }); }); }
      return { voters: [], nextCursor: null };
    } });
    render(createElement(PollCard, { client: f.client, communityId: COMMUNITY, channelId: CHANNEL, threadId: null,
      body: `Lunch?\n[dilla-poll:v1:${POLL}]`, authorId: BOT, bot: true, edited: false, deleted: false, selfId: USER, bits: 0 }));
    await waitFor(() => expect(release).toBeDefined());
    await act(async () => { await Promise.resolve(); });
    if (changed) f.setPoll({ revision: 2n });
    act(() => { clock += 30_000; tick(); });
    await waitFor(() => expect(f.requests.filter(row => row.operation === 71)).toHaveLength(2));
    await act(async () => { release(); await Promise.resolve(); });
    await screen.findByText('Open for 1 hour · pick any · 1 voter');
    if (changed) {
      act(() => { clock += 30_000; tick(); });
      await screen.findByText('Open for 1 hour · pick any · 0 voters');
      expect(screen.queryByRole('img', { name: 'Ada' })).not.toBeInTheDocument();
    } else expect(await screen.findByRole('img', { name: 'Ada' })).toBeInTheDocument();
  });
  it('loads an independent first-page preview for every answer', async () => {
    const f = utilityFixture(); f.setPoll({ counts: [1n, 1n] }); const real = ClientCore.createBotUtilities(f.client, COMMUNITY);
    vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({ ...real, get binding() { return real.binding; }, pollVoters: async input => {
      await real.pollVoters(input); return { voters: input.option === 0 ? [USER] : [], nextCursor: null };
    } });
    render(createElement(PollCard, { client: f.client, communityId: COMMUNITY, channelId: CHANNEL, threadId: null,
      body: `Lunch?\n[dilla-poll:v1:${POLL}]`, authorId: BOT, bot: true, edited: false, deleted: false, selfId: USER, bits: 0 }));
    expect(await screen.findByRole('img', { name: 'Ada' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^Soup/ }).querySelector('[role="img"]')).not.toBeNull();
    expect(screen.getByRole('button', { name: /^Salad/ }).querySelector('[role="img"]')).toBeNull();
  });
  it.each(['access', 'owner', 'installation'] as const)('rejects a pending voter page after its %s context changes', async change => {
    const f = utilityFixture(); f.setPoll({ counts: [1n, 1n] }); const base = f.client.handler; let replaced = false;
    f.client.handler = command => replaced && command.m === 'getBotCommandStatus'
      ? Promise.resolve({ installationId: 'ab'.repeat(16), botUserId: BOT, state: 3, revision: '1' }) : base(command);
    const real = ClientCore.createBotUtilities(f.client, COMMUNITY); let release: (() => void) | undefined;
    vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({ ...real, get binding() { return real.binding; }, pollVoters: async input => {
      const page = await real.pollVoters(input); if (input.option !== 0) return page; return new Promise(resolve => { release = () => resolve(page); });
    } });
    render(createElement(PollCard, { client: f.client, communityId: COMMUNITY, channelId: CHANNEL, threadId: null,
      body: `Lunch?\n[dilla-poll:v1:${POLL}]`, authorId: BOT, bot: true, edited: false, deleted: false, selfId: USER, bits: 0 }));
    await screen.findByRole('heading', { name: 'Lunch?' });
     await waitFor(() => expect(release).toBeDefined());
    if (change === 'access') {
      const access = f.client.get(`permissions:${COMMUNITY}`)!;
      f.client.publish(`permissions:${COMMUNITY}`, { ...access, loaded: false });
      f.client.publish(`permissions:${COMMUNITY}`, access);
    } else if (change === 'owner') f.client.publish('account', { ...f.client.get('account')!, deviceId: 'ac'.repeat(16) });
    else replaced = true;
    await act(async () => { release!(); await Promise.resolve(); });
    expect(screen.queryByRole('img', { name: 'Ada' })).not.toBeInTheDocument();
  });
  it('retries an uncertain poll vote with the same request id', async () => {
    const user = userEvent.setup(); const f = utilityFixture();
    const props = { client: f.client, communityId: COMMUNITY, channelId: CHANNEL, threadId: null, body: `Lunch?\n[dilla-poll:v1:${POLL}]`, authorId: BOT, bot: true, edited: false, deleted: false, selfId: USER, bits: PERM.viewChannel | PERM.sendMessages | PERM.manageMessages } as const;
    const view = render(createElement(PollCard, props));
    expect(await screen.findByRole('heading', { name: 'Lunch?' })).toBeInTheDocument();
    f.failNext(73, false);
    await user.click(screen.getAllByRole('button', { name: /^(Soup|Salad)/ })[0]);
    await screen.findByText('Your vote may not have counted. Tap it again to retry.');
    const first = f.requests.find(row => row.operation === 73)!;
    view.rerender(createElement(PollCard, { ...props, bits: PERM.viewChannel | PERM.sendMessages }));
    expect(screen.getAllByRole('button', { name: /^(Soup|Salad)/ })[0]).toHaveAttribute('aria-pressed', 'true'); view.unmount();
    render(createElement(PollCard, { ...props, bits: PERM.viewChannel | PERM.sendMessages }));
    expect(await screen.findByRole('button', { name: /^Soup/ })).toBeEnabled(); expect(screen.getAllByRole('button', { name: /^(Soup|Salad)/ })[0]).toHaveAttribute('aria-pressed', 'true');
    await user.click(screen.getByRole('button', { name: /^Soup/ }));
    await screen.findByText('Your vote may not have counted. Tap it again to retry.');
    const mutations = f.requests.filter(row => row.operation === 73);
    expect(mutations.length).toBeGreaterThan(0); expect(mutations.every(row => row.requestId === first.requestId)).toBe(true);
    const selectedMutation = mutations[0]; expect(selectedMutation).toBeDefined();
    expect(decode((selectedMutation?.args[1] as Uint8Array | undefined) ?? new Uint8Array())).toEqual([0n]);
  });
  it('keeps an uncertain vote ID when capability recovery temporarily fails', async () => {
    const f = utilityFixture(); const base = f.client.handler; let ready = true; let refresh!: () => void;
    vi.spyOn(window, 'setInterval').mockImplementation(((fn: () => void, ms: number) => { if (ms === 30_000) refresh = fn; return 9; }) as never);
    f.client.handler = command => command.m === 'getBotCommandStatus'
      ? Promise.resolve({ installationId: INSTALLATION, botUserId: BOT, state: ready ? 3 : 4, revision: '1' }) : base(command);
    const user = userEvent.setup(); const props = { client: f.client, communityId: COMMUNITY, channelId: CHANNEL, threadId: null,
      body: `Lunch?\n[dilla-poll:v1:${POLL}]`, authorId: BOT, bot: true, edited: false, deleted: false, selfId: USER, bits: PERM.viewChannel | PERM.sendMessages };
    const view = render(createElement(PollCard, props)); await screen.findByRole('heading', { name: 'Lunch?' }); f.failNext(73, false);
    await user.click(screen.getAllByRole('button', { name: /^(Soup|Salad)/ })[0]);
    await screen.findByText('Your vote may not have counted. Tap it again to retry.');
    const originalId = f.requests.find(row => row.operation === 73)!.requestId; ready = false; await user.click(screen.getByRole('button', { name: /^Soup/ }));
    await waitFor(() => expect(screen.queryByRole('heading', { name: 'Lunch?' })).not.toBeInTheDocument());
    ready = true; act(() => { refresh(); }); await screen.findByRole('heading', { name: 'Lunch?' });
    await user.click(screen.getByRole('button', { name: /^Soup/ }));
    expect(f.requests.filter(row => row.operation === 73).every(row => row.requestId === originalId)).toBe(true); view.unmount();
  });
  it('reconciles a threaded poll from its authoritative host items, independent of quote metadata', async () => {
    const f = utilityFixture(); const mounted = mount(f.client); await waitFor(() => expect(mounted.captured.value).toBeDefined()); const user = userEvent.setup();
    const rootId = 'bc'.repeat(16); const requestId = 'bd'.repeat(16);
    const binding = { communityId: COMMUNITY, channelId: CHANNEL, threadId: rootId, bits: PERM.sendMessages | PERM.viewChannel };
    const context = commandContext(f.client, requestId, { binding, getCurrentBinding: () => binding }); const invocation = { commandId: 'poll create', args: { question: 'Thread poll?', answers: 'A, B' } };
    let run!: Promise<unknown>; act(() => { run = mounted.captured.value!.handlers['poll create']!(invocation, context); });
    await screen.findByRole('heading', { name: 'New poll in #lobby' }); f.failNext(70); await user.click(screen.getByRole('button', { name: 'Post poll' })); expect(await run).toMatchObject({ status: 'uncertain' });
    const message = { key: 's10', state: 'ok' as const, reason: '', senderUser: BOT, senderDevice: null, own: false, web: false, bot: true, ts: 1_800_000_000,
      body: `Thread poll?\n[dilla-poll:v1:${requestId}]`, msgId: 'be'.repeat(16), seq: '10', edited: false,
      reply: { msgId: 'bf'.repeat(16), senderUser: BOT, seq: null, excerpt: 'quoted from another thread', state: 'ok' as const, threadRoot: 'c0'.repeat(16) },
      reactions: [], pinned: false, attachments: [], mention: false, actions: [], thread: null };
    const retryContext = commandContext(f.client, requestId, { binding, getCurrentBinding: () => binding, items: [message] });
    expect(f.poll.threadId).toBe(rootId); expect(await mounted.captured.value!.handlers['poll create']!(invocation, retryContext)).toMatchObject({ status: 'success' }); mounted.unmount();
  });
});

describe('utility outcome and privacy regressions', () => {
  const badReceipts = ([73, 74] as const).flatMap(operation =>
    (['malformed', 'poll', 'channel', 'thread', 'revision', 'schema', 'state'] as const).map(defect => ({ operation, defect })));
  it.each(badReceipts)('retains operation $operation after a $defect accepted receipt and failed recovery', async ({ operation, defect }) => {
    const f = utilityFixture(); const base = f.client.handler; const attemptedIds: string[] = []; let corrupt = true; let uncertain = false; let failRead = false; let failedRead = false; let refresh!: () => void;
    const canonical = pollSummary({ revision: 2n, myChoices: operation === 73 ? [0] : [], counts: [1n, 0n], closed: operation === 74 });
    const bad = { ...canonical, ...(defect === 'poll' ? { id: 'ab'.repeat(16) } : {}), ...(defect === 'channel' ? { channelId: 'ac'.repeat(16) } : {}),
      ...(defect === 'thread' ? { threadId: 'ad'.repeat(16) } : {}), ...(defect === 'revision' ? { revision: 3n } : {}),
      ...(defect === 'state' ? operation === 73 ? { myChoices: [] } : { closed: false } : {}) };
    f.client.handler = async command => {
      const value = await base(command);
      if (command.m !== 'readBotInteractionResponses') return value;
      const page = value as { responses: typeof f.responses };
      return { ...page, responses: page.responses.map(row => row.operation !== operation ? row : { ...row, status: 0, revision: '2',
        data: corrupt ? defect === 'malformed' ? wire([]) : wire([defect === 'schema' ? 2n : 1n, pollWire(bad)]) : wire([1n, pollWire(canonical)]) }) };
    };
    const real = ClientCore.createBotUtilities(f.client, COMMUNITY);
    const observe = async (run: () => Promise<PollSummary>) => { try { return await run(); } catch (cause) { uncertain = cause instanceof ClientCore.BotUtilitiesUncertainMutationError; throw cause; } };
    vi.spyOn(window, 'setInterval').mockImplementation(((fn: () => void, ms: number) => { if (ms === 30_000) refresh = fn; return 9; }) as never);
    vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({ ...real, get binding() { return real.binding; },
      capabilities: () => { if (failRead && defect !== 'malformed') { failRead = false; failedRead = true; return Promise.reject(new Error('capability unavailable')); } return real.capabilities(); },
      pollGet: (...args) => { if (failRead && defect === 'malformed') { failRead = false; failedRead = true; return Promise.reject(new Error('backing read unavailable')); } return real.pollGet(...args); },
      pollVote: input => { attemptedIds.push(input.requestId); return observe(() => real.pollVote(input)); },
      pollClose: input => { attemptedIds.push(input.requestId); return observe(() => real.pollClose(input)); },
      read: async (id, op) => { const result = await real.read(id, op); if (op === operation && corrupt) failRead = true; return result; } });
    const user = userEvent.setup(); const view = render(createElement(PollCard, { client: f.client, communityId: COMMUNITY, channelId: CHANNEL, threadId: null,
      body: `Lunch?\n[dilla-poll:v1:${POLL}]`, authorId: BOT, bot: true, edited: false, deleted: false, selfId: USER, bits: PERM.viewChannel | PERM.sendMessages }));
    await screen.findByRole('heading', { name: 'Lunch?' }); f.failNext(operation, false);
    await user.click(screen.getByRole('button', { name: operation === 73 ? /^Soup/ : 'Close poll' }));
    await waitFor(() => expect(uncertain && failedRead).toBe(true));
    const originalId = f.requests.find(row => row.operation === operation)!.requestId;
    act(() => { refresh(); });
    const retry = await screen.findByRole('button', { name: operation === 73 ? /^Soup/ : 'Close poll' });
    expect(retry).toBeEnabled(); expect(screen.getAllByRole('button', { name: /^(Soup|Salad)/ })[0])[operation === 73 ? 'toBeEnabled' : 'toBeDisabled']();
    if (operation === 73) expect(screen.getAllByRole('button', { name: /^(Soup|Salad)/ })[0]).toHaveAttribute('aria-pressed', 'true');
    corrupt = false; await user.click(retry);
    // The transport retains its original undecodable receipt even after the wire recovers.
    await waitFor(() => expect(retry).toBeEnabled());
    expect(attemptedIds).toEqual([originalId, originalId]);
    expect(screen.getAllByRole('button', { name: /^(Soup|Salad)/ })[0])[operation === 73 ? 'toBeEnabled' : 'toBeDisabled']();
    view.unmount();
  });

  it.each([73, 74] as const)('settles a validated operation %s receipt despite an unavailable refresh', async operation => {
    const f = utilityFixture(); const real = ClientCore.createBotUtilities(f.client, COMMUNITY); let failRead = false; let failedRead = false; let refresh!: () => void;
    const accepted = async (input: { requestId: string }, run: () => Promise<PollSummary>) => { await run(); throw new ClientCore.BotUtilitiesUncertainMutationError(operation, 0, input.requestId, new Error('lost outcome')); };
    vi.spyOn(window, 'setInterval').mockImplementation(((fn: () => void, ms: number) => { if (ms === 30_000) refresh = fn; return 9; }) as never);
    vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({ ...real, get binding() { return real.binding; },
      capabilities: () => { if (failRead && operation === 73) { failRead = false; failedRead = true; return Promise.reject(new Error('capability unavailable')); } return real.capabilities(); },
      pollGet: (...args) => { if (failRead && operation === 74) { failRead = false; failedRead = true; return Promise.reject(new Error('backing read unavailable')); } return real.pollGet(...args); },
      pollVote: input => accepted(input, () => real.pollVote(input)), pollClose: input => accepted(input, () => real.pollClose(input)),
      read: async (id, op) => { const result = await real.read(id, op); if (op === operation) failRead = true; return result; } });
    const user = userEvent.setup(); const view = render(createElement(PollCard, { client: f.client, communityId: COMMUNITY, channelId: CHANNEL, threadId: null,
      body: `Lunch?\n[dilla-poll:v1:${POLL}]`, authorId: BOT, bot: true, edited: false, deleted: false, selfId: USER, bits: PERM.viewChannel | PERM.sendMessages }));
    await screen.findByRole('heading', { name: 'Lunch?' });
    await user.click(screen.getByRole('button', { name: operation === 73 ? /^Soup/ : 'Close poll' }));
    await waitFor(() => expect(failedRead).toBe(true));
    await waitFor(() => expect(screen.getAllByRole('button', { name: /^(Soup|Salad)/ })[0]).toBeEnabled());
    expect(screen.queryByRole('button', { name: /Retry/ })).not.toBeInTheDocument();
    act(() => { refresh(); });
    if (operation === 74) await screen.findByText('CLOSED');
    else await waitFor(() => expect(screen.getByRole('button', { name: /^Soup/ })).toBeInTheDocument());
    expect(f.requests.filter(row => row.operation === operation)).toHaveLength(1); view.unmount();
  });
  it('keeps a typed accepted close definitive when the following capability check fails', async () => {
    const f = utilityFixture(); const real = ClientCore.createBotUtilities(f.client, COMMUNITY); let failCaps = false;
    vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({ ...real, get binding() { return real.binding; },
      capabilities: () => { if (failCaps) { failCaps = false; return Promise.reject(new Error('capability unavailable')); } return real.capabilities(); },
      pollClose: async input => { const result = await real.pollClose(input); failCaps = true; return result; } });
    const user = userEvent.setup(); const view = render(createElement(PollCard, { client: f.client, communityId: COMMUNITY, channelId: CHANNEL, threadId: null,
      body: `Lunch?\n[dilla-poll:v1:${POLL}]`, authorId: BOT, bot: true, edited: false, deleted: false, selfId: USER, bits: PERM.viewChannel | PERM.sendMessages }));
    await screen.findByRole('heading', { name: 'Lunch?' }); await user.click(screen.getByRole('button', { name: 'Close poll' }));
    await screen.findByText('CLOSED'); expect(screen.queryByRole('button', { name: /Retry/ })).not.toBeInTheDocument();
    expect(f.requests.filter(row => row.operation === 74)).toHaveLength(1); view.unmount();
  });
  it.each([73, 74] as const)('keeps operation %s uncertain across unrelated typed capability statuses', async operation => {
    const f = utilityFixture(); const real = ClientCore.createBotUtilities(f.client, COMMUNITY); let rejectedStatus: number | null = null; let refresh!: () => void;
    vi.spyOn(window, 'setInterval').mockImplementation(((fn: () => void, ms: number) => { if (ms === 30_000) refresh = fn; return 9; }) as never);
    vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({ ...real, get binding() { return real.binding; },
      capabilities: () => rejectedStatus === null ? real.capabilities() : Promise.reject(new ClientCore.BotUtilitiesStatusError(3, rejectedStatus, 'e1'.repeat(16), null)) });
    const user = userEvent.setup(); const view = render(createElement(PollCard, { client: f.client, communityId: COMMUNITY, channelId: CHANNEL, threadId: null,
      body: `Lunch?\n[dilla-poll:v1:${POLL}]`, authorId: BOT, bot: true, edited: false, deleted: false, selfId: USER, bits: PERM.viewChannel | PERM.sendMessages }));
    await screen.findByRole('heading', { name: 'Lunch?' }); f.failNext(operation, false);
    await user.click(screen.getByRole('button', { name: operation === 73 ? /^Soup/ : 'Close poll' }));
    await screen.findByText(operation === 73 ? 'Your vote may not have counted. Tap it again to retry.' : 'The vote result is uncertain. Refresh the poll before trying again.');
    const original = f.requests.find(row => row.operation === operation)!.requestId;
    for (const status of [2, 3, 4]) {
      rejectedStatus = status; await user.click(screen.getByRole('button', { name: operation === 73 ? /^Soup/ : 'Close poll' }));
      await waitFor(() => expect(screen.queryByRole('heading', { name: 'Lunch?' })).not.toBeInTheDocument());
      rejectedStatus = null; act(() => { refresh(); }); await screen.findByRole('heading', { name: 'Lunch?' });
      expect(screen.getByRole('button', { name: operation === 73 ? /^Soup/ : 'Close poll' })).toBeEnabled();
      if (operation === 73) expect(screen.getAllByRole('button', { name: /^(Soup|Salad)/ })[0]).toHaveAttribute('aria-pressed', 'true');
    }
    await user.click(screen.getByRole('button', { name: operation === 73 ? /^Soup/ : 'Close poll' }));
    expect(f.requests.filter(row => row.operation === operation).every(row => row.requestId === original)).toBe(true); view.unmount();
  });
  it.each(['reminders list', 'reminders cancel', 'reminders dismiss', 'partial list'] as const)('%s validates a superseded snapshot after community revocation', async action => {
    const commandId = action === 'partial list' ? 'reminders list' : action;
    const f = utilityFixture(); const other = 'ce'.repeat(16); const revokedId = 'cf'.repeat(16); let hold = false; let held = false; let release!: (value: { items: ReminderSummary[]; nextCursor: null }) => void; let currentReads = 0;
    f.client.set('communities', [{ id: other, name: 'Revoked', owner: USER }, { id: COMMUNITY, name: 'Current', owner: USER }]);
    const reminder = (id: string, text: string): ReminderSummary => ({ id, text, dueUnixSeconds: 1_800_000_001n, timezone: 'UTC', sourceChannelId: null, sourceThreadId: null, state: 'scheduled', revision: 1n });
    const mutations: string[] = [];
    vi.spyOn(ClientCore, 'createBotUtilities').mockImplementation((_client, id) => ({ capabilities: () => Promise.resolve({ version: 1, installationId: INSTALLATION, botUserId: BOT, supportedOperations: [3, 81, 82, 83] }),
      reminderList: () => {
        if (id === other && hold && action === 'partial list') return Promise.reject(new Error('incomplete scan'));
        if (id === COMMUNITY) { currentReads++; if (hold && !held) { held = true; return new Promise(resolve => { release = resolve; }); } }
        return Promise.resolve({ items: [reminder(id === other ? revokedId : 'cd'.repeat(16), id === other ? 'Revoked private reminder' : 'Current private reminder')], nextCursor: null });
      }, reminderCancel: () => { mutations.push(id); return Promise.resolve(reminder(revokedId, 'Revoked private reminder')); },
      reminderDismiss: () => { mutations.push(id); return Promise.resolve(reminder(revokedId, 'Revoked private reminder')); } } as unknown as BotUtilities));
    const mounted = mount(f.client); await waitFor(() => expect(currentReads).toBe(1)); hold = true;
    let result: CommandResult | undefined;
    act(() => { void mounted.captured.value!.handlers[commandId]!({ commandId, args: { reminder: revokedId } }, commandContext(f.client, 'cc'.repeat(16))).then(value => { result = value; }); });
    await waitFor(() => expect(held).toBe(true));
    act(() => { f.client.set('communities', [{ id: COMMUNITY, name: 'Current', owner: USER }]); });
    // Observe the replacement scan in a separate event before releasing the old scan.
    await waitFor(() => expect(currentReads).toBeGreaterThanOrEqual(3));
    act(() => { release({ items: [reminder('cd'.repeat(16), 'Current private reminder')], nextCursor: null }); });
    await waitFor(() => expect(result).toBeDefined());
    if (action === 'reminders list') { expect(result).toMatchObject({ status: 'success' }); expect(result?.message).toContain('Current private reminder'); }
    else expect(result).toMatchObject({ status: 'failure' });
    expect(result?.message).not.toContain('Revoked private reminder'); expect(mutations).toEqual([]); mounted.unmount();
  });

  it.each([73, 74] as const)('does not release operation %s for a terminal status with another request ID', async operation => {
    const f = utilityFixture(); const real = ClientCore.createBotUtilities(f.client, COMMUNITY);
    vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({ ...real, get binding() { return real.binding; },
      read: () => Promise.reject(new ClientCore.BotUtilitiesStatusError(operation, 4, 'e1'.repeat(16), null)) });
    const user = userEvent.setup(); const view = render(createElement(PollCard, { client: f.client, communityId: COMMUNITY, channelId: CHANNEL, threadId: null,
      body: `Lunch?\n[dilla-poll:v1:${POLL}]`, authorId: BOT, bot: true, edited: false, deleted: false, selfId: USER, bits: PERM.viewChannel | PERM.sendMessages }));
    await screen.findByRole('heading', { name: 'Lunch?' }); f.failNext(operation, false);
    await user.click(screen.getByRole('button', { name: operation === 73 ? /^Soup/ : 'Close poll' }));
    await screen.findByText(operation === 73 ? 'Your vote may not have counted. Tap it again to retry.' : 'The vote result is uncertain. Refresh the poll before trying again.');
    expect(screen.getByRole('button', { name: operation === 73 ? /^Soup/ : 'Close poll' })).toBeEnabled(); view.unmount();
  });
  it.each([73, 74] as const)('releases a fresh operation %s when its preflight proves nothing was dispatched', async operation => {
    const f = utilityFixture(); const real = ClientCore.createBotUtilities(f.client, COMMUNITY); let reject = false;
    vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({ ...real, get binding() { return real.binding; }, capabilities: () => {
      if (reject) { reject = false; return Promise.reject(new ClientCore.BotUtilitiesStatusError(3, 3, 'e1'.repeat(16), null)); }
      return real.capabilities();
    } });
    const user = userEvent.setup(); const view = render(createElement(PollCard, { client: f.client, communityId: COMMUNITY, channelId: CHANNEL, threadId: null,
      body: `Lunch?\n[dilla-poll:v1:${POLL}]`, authorId: BOT, bot: true, edited: false, deleted: false, selfId: USER, bits: PERM.viewChannel | PERM.sendMessages }));
    await screen.findByRole('heading', { name: 'Lunch?' }); reject = true;
    await user.click(screen.getByRole('button', { name: operation === 73 ? /^Soup/ : 'Close poll' }));
    await waitFor(() => expect(screen.getAllByRole('button', { name: /^(Soup|Salad)/ })[0]).toBeEnabled());
    expect(screen.queryByRole('button', { name: /Retry/ })).not.toBeInTheDocument(); expect(f.requests.some(row => row.operation === operation)).toBe(false); view.unmount();
  });
  it('keeps an accepted vote definitive when its following capability check fails', async () => {
    const f = utilityFixture(); const real = ClientCore.createBotUtilities(f.client, COMMUNITY); let reject = false;
    vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({ ...real, get binding() { return real.binding; },
      capabilities: () => { if (reject) { reject = false; return Promise.reject(new ClientCore.BotUtilitiesStatusError(3, 3, 'e1'.repeat(16), null)); } return real.capabilities(); },
      pollVote: async input => { const result = await real.pollVote(input); reject = true; return result; } });
    const user = userEvent.setup(); const view = render(createElement(PollCard, { client: f.client, communityId: COMMUNITY, channelId: CHANNEL, threadId: null,
      body: `Lunch?\n[dilla-poll:v1:${POLL}]`, authorId: BOT, bot: true, edited: false, deleted: false, selfId: USER, bits: PERM.viewChannel | PERM.sendMessages }));
    await screen.findByRole('heading', { name: 'Lunch?' }); await user.click(screen.getAllByRole('button', { name: /^(Soup|Salad)/ })[0]);
    await waitFor(() => expect(screen.getAllByRole('button', { name: /^(Soup|Salad)/ })[0]).toBeEnabled()); expect(screen.getAllByRole('button', { name: /^(Soup|Salad)/ })[0]).toHaveAttribute('aria-pressed', 'true');
    await user.click(screen.getAllByRole('button', { name: /^(Soup|Salad)/ })[1]);
    await waitFor(() => expect(f.poll.myChoices).toEqual([0, 1])); expect(new Set(f.requests.filter(row => row.operation === 73).map(row => row.requestId)).size).toBe(2); view.unmount();
  });
  it.each(['disabled', 'reinstalled', 'device'] as const)('revalidates %s before returning an earlier community reminder page', async change => {
    const f = utilityFixture(); const other = 'ce'.repeat(16); let hold = false; let held = false; let changed = false; let release!: (value: { items: ReminderSummary[]; nextCursor: null }) => void; let reads = 0;
    f.client.set('communities', [{ id: other, name: 'Earlier', owner: USER }, { id: COMMUNITY, name: 'Current', owner: USER }]);
    const reminder = (id: string, text: string): ReminderSummary => ({ id, text, dueUnixSeconds: 1_800_000_001n, timezone: 'UTC', sourceChannelId: null, sourceThreadId: null, state: 'scheduled', revision: 1n });
    vi.spyOn(ClientCore, 'createBotUtilities').mockImplementation((_client, id) => ({ capabilities: () => {
      if (changed && id === other && change === 'disabled') return Promise.reject(new ClientCore.BotUtilitiesProviderUnavailableError(81));
      return Promise.resolve({ version: 1, installationId: changed && id === other && change === 'reinstalled' ? 'e2'.repeat(16) : INSTALLATION, botUserId: BOT, supportedOperations: [3, 81] });
    }, reminderList: () => {
      if (id === COMMUNITY) { reads++; if (hold && !held) { held = true; return new Promise(resolve => { release = resolve; }); } }
      return Promise.resolve({ items: [reminder(id === other ? 'cf'.repeat(16) : 'cd'.repeat(16), id === other ? 'Earlier private reminder' : 'Current private reminder')], nextCursor: null });
    } } as unknown as BotUtilities));
    const mounted = mount(f.client); await waitFor(() => expect(reads).toBe(1)); hold = true; let result: CommandResult | undefined;
    act(() => { void mounted.captured.value!.handlers['reminders list']!({ commandId: 'reminders list', args: {} }, commandContext(f.client, 'cc'.repeat(16))).then(value => { result = value; }); });
    await waitFor(() => expect(held).toBe(true)); changed = true;
    if (change === 'device') act(() => { f.client.set('account', { ...f.client.get('account')!, deviceId: 'ef'.repeat(16) }); });
    act(() => { release({ items: [reminder('cd'.repeat(16), 'Current private reminder')], nextCursor: null }); });
    await waitFor(() => expect(result).toBeDefined()); expect(result?.message).not.toContain('Earlier private reminder');
    if (change === 'device') expect(result).toMatchObject({ status: 'failure' });
    else { expect(result).toMatchObject({ status: 'success' }); expect(result?.message).toContain('Current private reminder'); }
    mounted.unmount();
  });

  it.each([73, 74] as const)('releases operation %s only for its original terminal mutation status', async operation => {
    const f = utilityFixture(); const real = ClientCore.createBotUtilities(f.client, COMMUNITY); const ids: string[] = []; let status: number | null = 2;
    const reject = (input: { requestId: string }) => { ids.push(input.requestId); return Promise.reject(new ClientCore.BotUtilitiesStatusError(operation, status!, input.requestId, null)); };
    vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({ ...real, get binding() { return real.binding; },
      pollVote: input => status === null ? real.pollVote(input) : reject(input), pollClose: input => status === null ? real.pollClose(input) : reject(input) });
    const user = userEvent.setup(); const view = render(createElement(PollCard, { client: f.client, communityId: COMMUNITY, channelId: CHANNEL, threadId: null,
      body: `Lunch?\n[dilla-poll:v1:${POLL}]`, authorId: BOT, bot: true, edited: false, deleted: false, selfId: USER, bits: PERM.viewChannel | PERM.sendMessages }));
    await screen.findByRole('heading', { name: 'Lunch?' });
    for (const terminal of [2, 3, 4]) {
      status = terminal; await user.click(screen.getByRole('button', { name: operation === 73 ? /^Soup/ : 'Close poll' }));
      await waitFor(() => expect(screen.getAllByRole('button', { name: /^(Soup|Salad)/ })[0]).toBeEnabled());
      expect(screen.queryByRole('button', { name: /Retry/ })).not.toBeInTheDocument();
    }
    status = null; await user.click(screen.getByRole('button', { name: operation === 73 ? /^Soup/ : 'Close poll' }));
    await waitFor(() => expect(f.requests.some(row => row.operation === operation)).toBe(true));
    expect(new Set(ids).size).toBe(3); expect(ids).not.toContain(f.requests.find(row => row.operation === operation)!.requestId); view.unmount();
  });

});


describe('byte-bounded utility discovery and reconciliation', () => {
  it('recovers a created reminder after six other long one-record pages', async () => {
    const f = utilityFixture(); const mounted = mount(f.client); await waitFor(() => expect(mounted.captured.value).toBeDefined());
    const context = commandContext(f.client, 'c1'.repeat(16)); const invoke = { commandId: 'remind', args: { when: 'in 1h', text: 'Recovered seventh reminder' } };
    let first!: Promise<CommandResult>;
    f.failNext(80); act(() => { first = mounted.captured.value!.handlers.remind!(invoke, context); });
    expect(await first).toMatchObject({ status: 'uncertain' });
    const saved = f.reminder!; const base = f.client.handler; let pages = 0;
    f.client.handler = command => {
      if (command.m !== 'humanInteractionRequest' || command.operation !== 81) return base(command);
      const input = command.args[0] as Uint8Array | null;
      const index = input === null ? 0 : Number(new TextDecoder().decode(input).split('/')[2]?.slice(-2));
      const id = (index + 1).toString().padStart(32, '0');
      const row = index === 6 ? saved : [asBytes(id), 'x'.repeat(1500), 1_800_000_000n + BigInt(index), 'UTC', null, null, 1n, 1n];
      const cursor = index === 6 ? null : utf8(`${USER}/${(1_800_000_000n + BigInt(index)).toString().padStart(20, '0')}/${id}`);
      f.responses.push({ deliveryId: String(1000 + ++pages), requestId: command.requestId, communityId: COMMUNITY, operation: 81, channelId: null, status: 0, revision: null, data: wire([1n, [row], cursor]) });
      return Promise.resolve({ requestId: command.requestId, upload: 'accepted' });
    };
    const recovered = await mounted.captured.value!.handlers.remind!(invoke, context);
    expect(pages).toBe(7); expect(recovered).toMatchObject({ status: 'success' });
    expect(f.requests.filter(row => row.operation === 80)).toHaveLength(1); mounted.unmount();
  });

  it('finds matching polls beyond eight byte-limited pages through the real adapter', async () => {
    const f = utilityFixture(); const base = f.client.handler; let pages = 0;
    f.client.handler = command => {
      if (command.m !== 'humanInteractionRequest' || command.operation !== 72) return base(command);
      const cursor = command.args[2] as Uint8Array | null;
      const priorId = cursor === null ? 0 : parseInt(new TextDecoder().decode((decode(cursor) as unknown[])[4] as Uint8Array).slice(-32), 16);
      const n = priorId + 1; const id = n.toString(16).padStart(32, '0');
      const row = pollSummary({ id, question: n === 10 ? 'Wanted tenth poll' : 'Other poll', answers: Array.from({ length: 10 }, (_, index) => String(index) + 'x'.repeat(99)), counts: Array(10).fill(0n) as bigint[] });
      const next = n === 10 ? null : wire([1n, asBytes(COMMUNITY), asBytes(CHANNEL), null, utf8(`dilla.bot.poll.v1/${id}`)]);
      f.responses.push({ deliveryId: String(1000 + ++pages), requestId: command.requestId, communityId: COMMUNITY, operation: 72, channelId: null, status: 0, revision: null, data: wire([1n, [pollWire(row)], next]) });
      return Promise.resolve({ requestId: command.requestId, upload: 'accepted' });
    };
    const mounted = mount(f.client); await waitFor(() => expect(mounted.captured.value).toBeDefined());
    const suggestions = await mounted.captured.value!.suggest('poll results', { name: 'poll', kind: 'poll', description: '', required: true }, 'Wanted', commandContext(f.client, 'c2'.repeat(16)).binding);
    expect(suggestions).toMatchObject([{ value: 'a'.padStart(32, '0'), label: 'Wanted tenth poll' }]); expect(pages).toBe(10); mounted.unmount();
  });

  it('bounds unbounded poll history explicitly and uses authenticated exact-ID and choice lookup', async () => {
    const f = utilityFixture(); const real = ClientCore.createBotUtilities(f.client, COMMUNITY); let n = 0;
    const pollList = vi.fn(() => { const start = n; n += 20; return Promise.resolve({ items: Array.from({ length: 20 }, (_, i) => pollSummary({ id: (start + i + 1).toString(16).padStart(32, '0') })), nextCursor: utf8(String(n)) }); });
    const pollGet = vi.fn((id: string, options: Parameters<BotUtilities['pollGet']>[1]) => real.pollGet(id, options));
    vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({ ...real, pollList, pollGet });
    const mounted = mount(f.client); await waitFor(() => expect(mounted.captured.value).toBeDefined());
    const binding = commandContext(f.client, 'c3'.repeat(16)).binding; const suggest = mounted.captured.value!.suggest;
    const list = await suggest('poll results', { name: 'poll', kind: 'poll', description: '', required: true }, 'Lunch', binding);
    expect(pollList).toHaveBeenCalledTimes(50); expect(list?.notice).toMatch(/1000 records.*exact poll ID/u);
    const exact = await suggest('poll results', { name: 'poll', kind: 'poll', description: '', required: true }, POLL, binding);
    expect(exact?.map(row => row.value)).toEqual([POLL]); expect(pollGet).toHaveBeenCalledWith(POLL, { channelId: CHANNEL, threadId: null });
    const choices = await suggest('poll vote', { name: 'choice', kind: 'text', description: '', required: true }, 'Soup', binding, { poll: POLL });
    expect(choices?.map(row => row.value)).toEqual(['1']); expect(pollList).toHaveBeenCalledTimes(50); mounted.unmount();
  });

  it.each(['reminder', 'poll'] as const)('stops %s discovery on repeated cursors', async kind => {
    const f = utilityFixture(); const real = ClientCore.createBotUtilities(f.client, COMMUNITY);
    const reminderList = vi.fn(() => Promise.resolve({ items: [{ id: 'ab'.repeat(16), text: 'loop', state: 'due' } as ReminderSummary], nextCursor: new Uint8Array([1]) }));
    const pollList = vi.fn(() => Promise.resolve({ items: [pollSummary()], nextCursor: new Uint8Array([1]) }));
    vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({ ...real, reminderList, pollList });
    const mounted = mount(f.client); await waitFor(() => expect(mounted.captured.value).toBeDefined());
    if (kind === 'poll') {
      const result = await mounted.captured.value!.suggest('poll results', { name: 'poll', kind: 'poll', description: '', required: true }, 'Lunch', commandContext(f.client, 'c4'.repeat(16)).binding);
      expect(result?.notice).toMatch(/could not be loaded/u); expect(pollList).toHaveBeenCalledTimes(2);
    } else { await waitFor(() => expect(reminderList).toHaveBeenCalledTimes(2)); expect(mounted.captured.value!.dueCount).toBe(0); }
    mounted.unmount();
  });
});

it('keeps the reminders popover open, once, when its trigger moves between action slots', async () => {
  const f = utilityFixture(); const user = userEvent.setup();
  function Relocating({ sidebar }: { sidebar: boolean }) {
    const provider = useUtilityCommandProvider(f.client, sidebar ? null : COMMUNITY);
    return <>{provider.dialogs}<header>{sidebar ? null : provider.inbox}</header><aside>{sidebar ? provider.inbox : null}</aside></>;
  }
  const mounted = render(<Relocating sidebar={false} />);
  await user.click(screen.getByRole('button', { name: 'Reminders' }));
  expect(screen.getByRole('dialog', { name: 'Reminders' })).toBeInTheDocument();
  mounted.rerender(<Relocating sidebar />);
  expect(screen.getAllByRole('dialog', { name: 'Reminders' })).toHaveLength(1);
  act(() => { screen.getByRole('dialog', { name: 'Reminders' }).querySelector<HTMLElement>('[role="tab"]')?.focus(); });
  await user.keyboard('{Escape}');
  expect(screen.queryByRole('dialog', { name: 'Reminders' })).toBeNull();
});

// Wave B Task 2 fix round 1: a reminder's text over 1500 bytes has its own message.
it('says a reminder text over 1500 bytes is too long', () => {
  const now = new Date('2026-10-30T17:00:00Z');
  expect(reminderFromArgs({ when: '1h', text: 'x'.repeat(1501), timezone: 'Europe/Stockholm' }, now)).toEqual({ error: 'A reminder is at most 1500 bytes. Shorten the text.' });
  expect(reminderFromArgs({ when: 'soon', text: 'ok', timezone: 'Europe/Stockholm' }, now)).toEqual({ error: 'Choose a valid time from 1 minute to 365 days away and enter reminder text.' });
  expect(reminderFromArgs({ when: '1h', text: 'ok', timezone: 'Europe/Stockholm' }, now)).toMatchObject({ value: { text: 'ok', timezone: 'Europe/Stockholm' } });
});
