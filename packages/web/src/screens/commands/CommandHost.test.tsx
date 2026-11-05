import { readFileSync } from 'node:fs';
import * as ClientCore from '@dilla/client-core';
import { useUtilityCommandProvider } from './UtilityCommandProvider.tsx';
import { useState } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient } from '../../test/fake-client.ts';
import { clearCommandDraftMemory, CommandHost } from './CommandHost.tsx';
import { CommandProvider } from './CommandContext.tsx';
import type { CommandProviderHandlers } from './command-types.ts';
import { privateResults } from "./private-results.ts";

let focusStyle: HTMLStyleElement;
beforeEach(() => {
  clearCommandDraftMemory();
  focusStyle = document.createElement('style');
  // jsdom does not resolve token colors here: supply literal fixture values, then exercise the real CSS selector.
  focusStyle.textContent = '.d-composer__box { border: 1px solid #232831; box-shadow: none; }' +
    readFileSync('src/screens/commands/CommandHost.css', 'utf8')
      .replaceAll('var(--accent)', '#5aa8e6').replaceAll('var(--accent-soft)', 'rgba(90, 168, 230, 0.12)');
  document.head.append(focusStyle);
});
afterEach(() => focusStyle.remove());

function hostHarness(fake = new FakeClient()) {
  const channelId = crypto.randomUUID().replaceAll('-', '').slice(0, 32);
  let ordinary = '';
  const literals: string[] = [];
  function Harness() {
    const [mode, setMode] = useState(false);
    const [, rerender] = useState(0);
    return <>
      <textarea aria-label="message" value={ordinary} onChange={event => {
        ordinary = event.currentTarget.value; rerender(n => n + 1);
      }} />
      <CommandHost communityId={null} channelId={channelId} items={[]} selfId={'22'.repeat(16)}
        draft={ordinary} onDraft={text => { ordinary = text; rerender(n => n + 1); }} onSendLiteral={text => literals.push(text)} encodeMessage={text => text} openGif={() => undefined} gifsEnabled
        onModeChange={setMode} />
      <output>{mode ? 'command mode' : 'chat mode'}</output>
    </>;
  }
  const user = userEvent.setup();
  const view = render(<CoreProvider client={fake}><Harness /></CoreProvider>);
  return { user, view, fake, literals, channelId };
}

function moderationHarness(joined = true, action: 'kick' | 'ban' = 'kick') {
  const core = new FakeClient(); const communityId = '55'.repeat(16); const selfId = '66'.repeat(16);
  const access = { loaded: true, top: 4, owner: false, manageCommunity: false, kickMembers: true, banMembers: true,
    moderateMembers: true, manageMessages: false, manageRoles: true, manageChannels: true, createInvite: true,
    everyoneId: '99'.repeat(16), overwritesLoaded: true };
  const roles = [
    { id: 'a1'.repeat(16), name: 'member', position: 1, allow: 0, deny: 0, color: 0, hoist: false, mentionable: false, members: 1 },
    { id: 'a3'.repeat(16), name: 'moderator', position: 4, allow: 0, deny: 0, color: 0, hoist: false, mentionable: false, members: 1 },
  ];
  const members = [
    { userId: selfId, username: 'ada', display: 'Ada', kind: 0 as const, roleIds: [roles[1].id] },
    { userId: '88'.repeat(16), username: 'bea', display: 'Bea', kind: 0 as const, roleIds: [roles[0].id] },
  ];
  core.set('communities', [{ id: communityId, name: 'Town', owner: '77'.repeat(16) }]);
  core.set(`permissions:${communityId}`, access); core.set(`roles:${communityId}`, roles);
  core.set(`members:${communityId}`, joined ? members : members.slice(0, 1));
  function Harness() {
    const [draft, setDraft] = useState(`/${action}`);
    return <CommandHost communityId={communityId} channelId={'44'.repeat(16)} bits={(1 << 12) | (1 << 13)}
      items={[]} selfId={selfId} draft={draft} onDraft={setDraft} onSendLiteral={() => undefined}
      encodeMessage={text => text} openGif={() => undefined} gifsEnabled />;
  }
  const view = render(<CoreProvider client={core}><Harness /></CoreProvider>);
  return { core, communityId, access, roles, members, view, user: userEvent.setup() };
}

describe('CommandHost', () => {
  it('routes a slash draft into the production built-in handler and publishes the resulting message', async () => {
    const { user, fake, channelId } = hostHarness();
    fireEvent.change(screen.getByRole('textbox', { name: 'message' }), { target: { value: '/me' } });
    expect(screen.getByText('command mode')).toBeInTheDocument();
    const field = screen.getByRole('textbox', { name: 'text' });
    await user.type(field, 'waves');
    await user.click(screen.getByRole('button', { name: 'send' }));
    await waitFor(() => expect(privateResults(fake, ':', `dm/${channelId}/`).rows()[0]?.text).toBe('Sent.'));
    expect(fake.callsOf('send')).toEqual([{ m: 'send', channelId, text: '_waves_' }]);
  });

  it('executes a complete command typed after the first slash opens the picker', async () => {
    const { user, fake, channelId } = hostHarness();
    fireEvent.change(screen.getByRole('textbox', { name: 'message' }), { target: { value: '/' } });
    const picker = await screen.findByRole('textbox', { name: 'Command' });
    await user.type(picker, 'me waves hello');
    await user.keyboard('{Enter}');
    expect(await screen.findByRole('textbox', { name: 'text' })).toHaveValue('waves hello');
    await user.click(screen.getByRole('button', { name: 'send' }));
    await waitFor(() => expect(privateResults(fake, ':', `dm/${channelId}/`).rows()[0]?.text).toBe('Sent.'));
    expect(fake.callsOf('send')).toEqual([{ m: 'send', channelId, text: '_waves hello_' }]);
  });

  it('keeps keyboard focus in the command picker after the first slash', async () => {
    const { user } = hostHarness();
    fireEvent.change(screen.getByRole('textbox', { name: 'message' }), { target: { value: '/' } });
    const picker = await screen.findByRole('textbox', { name: 'Command' });
    expect(picker).toHaveFocus();
    await user.type(picker, 'me');
    expect(screen.getByRole('option', { name: /\/me/ })).toBeInTheDocument();
  });

  it('keeps later eligible catalog entries reachable after the first slash', async () => {
    const core = new FakeClient();
    const user = userEvent.setup();
    render(<CoreProvider client={core}><CommandHost communityId={'33'.repeat(16)} channelId={'44'.repeat(16)} bits={0x1ffffff}
      items={[]} selfId={'22'.repeat(16)} draft="/" onDraft={() => undefined} onSendLiteral={() => undefined}
      encodeMessage={text => text} openGif={() => undefined} gifsEnabled onModeChange={() => undefined} /></CoreProvider>);
    const picker = await screen.findByRole('textbox', { name: 'Command' });
    await user.type(picker, 'kick');
    expect(screen.getByRole('option', { name: /\/kick/ })).toBeInTheDocument();
  });

  it('filters owner and higher-rank targets from moderation suggestions', async () => {
    const core = new FakeClient();
    const communityId = '55'.repeat(16), selfId = '66'.repeat(16), ownerId = '77'.repeat(16), targetId = '88'.repeat(16);
    core.set('communities', [{ id: communityId, name: 'Town', owner: ownerId }]);
    core.set(`permissions:${communityId}`, { loaded: true, top: 4, owner: false, manageCommunity: false, kickMembers: true, banMembers: true, moderateMembers: true, manageRoles: true, manageChannels: true, createInvite: true, everyoneId: '99'.repeat(16), overwritesLoaded: true });
    core.set(`roles:${communityId}`, [
      { id: 'a1'.repeat(16), name: 'lower', position: 1, allow: 0, deny: 0, color: 0, hoist: false, mentionable: false, members: 1 },
      { id: 'a2'.repeat(16), name: 'higher', position: 5, allow: 0, deny: 0, color: 0, hoist: false, mentionable: false, members: 1 },
      { id: 'a3'.repeat(16), name: 'self', position: 4, allow: 0, deny: 0, color: 0, hoist: false, mentionable: false, members: 1 },
    ]);
    core.set(`members:${communityId}`, [
      { userId: selfId, username: 'self', display: 'self', kind: 0, roleIds: ['a3'.repeat(16)] },
      { userId: ownerId, username: 'owner', display: 'owner', kind: 0, roleIds: ['a2'.repeat(16)] },
      { userId: targetId, username: 'target', display: 'target', kind: 0, roleIds: ['a1'.repeat(16)] },
    ]);
    const user = userEvent.setup();
    render(<CoreProvider client={core}><CommandHost communityId={communityId} channelId={'44'.repeat(16)} bits={1 << 12}
      items={[]} selfId={selfId} draft="/kick" onDraft={() => undefined} onSendLiteral={() => undefined}
      encodeMessage={text => text} openGif={() => undefined} gifsEnabled /></CoreProvider>);
    const field = await screen.findByRole('combobox', { name: 'user' });
    await user.type(field, 'target');
    expect(await screen.findByRole('option', { name: /^target,/ })).toBeInTheDocument();
    await user.clear(field);
    await user.type(field, 'owner');
    await waitFor(() => expect(screen.queryByRole('option', { name: /^owner,/ })).toBeNull());
  });

  it('refreshes an empty moderation suggestion when the member arrives without retyping', async () => {
    const { core, communityId, members, user } = moderationHarness(false);
    const field = await screen.findByRole('combobox', { name: 'user' });
    await user.type(field, 'Bea');
    expect(screen.queryByRole('option', { name: /^Bea,/ })).not.toBeInTheDocument();
    core.publish(`members:${communityId}`, members);
    expect(await screen.findByRole('option', { name: /^Bea,/ })).toBeInTheDocument();
    expect(field).toHaveValue('Bea');
  });

  it.each(['permission', 'rank', 'membership'] as const)('removes a moderation suggestion after current %s changes', async change => {
    const { core, communityId, access, roles, members, user } = moderationHarness();
    const field = await screen.findByRole('combobox', { name: 'user' });
    await user.type(field, 'Bea');
    expect(await screen.findByRole('option', { name: /^Bea,/ })).toBeInTheDocument();
    if (change === 'permission') core.publish(`permissions:${communityId}`, { ...access, kickMembers: false });
    else if (change === 'rank') core.publish(`roles:${communityId}`, roles.map(role => role.id === roles[0].id ? { ...role, position: 5 } : role));
    else core.publish(`members:${communityId}`, members.slice(0, 1));
    await waitFor(() => expect(screen.queryByRole('option', { name: /^Bea,/ })).not.toBeInTheDocument());
    expect(field).toHaveValue('Bea');
  });

  it('sources unban and self-role options from their current server-managed slices', async () => {
    const cid = '55'.repeat(16), channel = '44'.repeat(16), self = '66'.repeat(16);
    const bannedId = '77'.repeat(16), roleId = '88'.repeat(16);
    const core = new FakeClient();
    core.set('communities', [{ id: cid, name: 'Town', owner: '99'.repeat(16) }]);
    core.set(`permissions:${cid}`, { loaded: true, top: 4, owner: false, manageCommunity: false, kickMembers: false, banMembers: true, moderateMembers: false,
      manageMessages: false, manageRoles: false, manageChannels: false, createInvite: false, everyoneId: 'aa'.repeat(16), overwritesLoaded: true });
    core.set(`bans:${cid}`, [{ userId: bannedId, name: 'banned target', reason: '', byUser: self, created: 1, expires: null }]);
    core.handler = command => command.m === 'listSelfRoles' ? Promise.resolve([{ id: roleId, name: 'Allowed role', selfSelected: false }]) : Promise.resolve(null);
    const user = userEvent.setup();
    const view = render(<CoreProvider client={core}><CommandHost communityId={cid} channelId={channel} bits={1 << 13}
      items={[]} selfId={self} draft="/unban" onDraft={() => undefined} onSendLiteral={() => undefined}
      encodeMessage={text => text} openGif={() => undefined} gifsEnabled /></CoreProvider>);
    const banned = await screen.findByRole('combobox', { name: 'user' });
    await user.type(banned, 'banned');
    expect(await screen.findByRole('option', { name: /banned target/ })).toBeInTheDocument();
    view.unmount();

    clearCommandDraftMemory();
    render(<CoreProvider client={core}><CommandHost communityId={cid} channelId={channel} bits={1 << 15}
      items={[]} selfId={self} draft="/selfrole add" onDraft={() => undefined} onSendLiteral={() => undefined}
      encodeMessage={text => text} openGif={() => undefined} gifsEnabled /></CoreProvider>);
    const role = await screen.findByRole('combobox', { name: 'role' });
    await user.type(role, 'allowed');
    expect(await screen.findByRole('option', { name: 'Allowed role' })).toBeInTheDocument();
    expect(core.callsOf('listSelfRoles').length).toBeGreaterThan(0);
  });

  it('retains an uncertain request ID across a host remount for safe retry', async () => {
    const ids: string[] = [];
    const fake = new FakeClient();
    const channelId = crypto.randomUUID().replaceAll('-', '').slice(0, 32);
    function Harness() {
      const [mounted, setMounted] = useState(true);
      const handlers: CommandProviderHandlers = { me: (_invocation, context) => {
        ids.push(context.requestId); return Promise.resolve({ status: 'uncertain' as const, private: true, message: 'uncertain' });
      } };
      return <CommandProvider handlers={handlers}>
        <button onClick={() => setMounted(value => !value)}>toggle host</button>
        {mounted ? <CommandHost communityId={null} channelId={channelId} items={[]} selfId={null} draft="/me hello" onDraft={() => undefined}
          onSendLiteral={() => undefined} encodeMessage={text => text} openGif={() => undefined} gifsEnabled /> : null}
      </CommandProvider>;
    }
    const user = userEvent.setup();
    render(<CoreProvider client={fake}><Harness /></CoreProvider>);
    await user.click(await screen.findByRole('button', { name: 'send' }));
    await waitFor(() => expect(privateResults(fake, ':', `dm/${channelId}/`).rows()[0]?.text).toBe('uncertain'));
    const first = ids[0];
    await user.click(screen.getByRole('button', { name: 'toggle host' }));
    await user.click(screen.getByRole('button', { name: 'toggle host' }));
    await user.click(await screen.findByRole('button', { name: 'send' }));
    await waitFor(() => expect(ids).toHaveLength(2));
    expect(ids[1]).toBe(first);
  });

  it('allocates a fresh request ID for a second intentional successful execution', async () => {
    const ids: string[] = [];
    const channelId = crypto.randomUUID().replaceAll('-', '').slice(0, 32);
    const handlers: CommandProviderHandlers = { me: (_invocation, context) => {
      ids.push(context.requestId); return Promise.resolve({ status: 'success' as const, private: true, message: 'done' });
    } };
    const fake = new FakeClient();
    render(<CoreProvider client={fake}><CommandProvider handlers={handlers}>
      <CommandHost communityId={null} channelId={channelId} items={[]} selfId={null} draft="/me hello" onDraft={() => undefined}
        onSendLiteral={() => undefined} encodeMessage={text => text} openGif={() => undefined} gifsEnabled />
    </CommandProvider></CoreProvider>);
    const user = userEvent.setup();
    const run = await screen.findByRole('button', { name: 'send' });
    await user.click(run);
    await waitFor(() => expect(privateResults(fake, ':', `dm/${channelId}/`).rows()[0]?.text).toBe('done'));
    await user.click(screen.getByRole('button', { name: 'send' }));
    await waitFor(() => expect(ids).toHaveLength(2));
    expect(ids[1]).not.toBe(ids[0]);
  });

  it('keeps an unknown slash draft private until the explicit send as text action', async () => {
    const { user, fake, literals } = hostHarness();
    fireEvent.change(screen.getByRole('textbox', { name: 'message' }), { target: { value: '/not-a-command' } });
    expect(fake.calls).toEqual([]);
    await user.click(screen.getByRole('button', { name: /send as text/i }));
    expect(literals).toEqual(['/not-a-command']);
    expect(fake.calls).toEqual([]);
  });

  it('does not navigate when a pending DM command is canceled before openDm resolves', async () => {
    const fake = new FakeClient();
    const communityId = '55'.repeat(16), channelId = '44'.repeat(16), selfId = '66'.repeat(16), bob = '77'.repeat(16);
    fake.set(`members:${communityId}`, [{ userId: bob, username: 'bob', display: '', kind: 0, roleIds: [] }]);
    let resolveDm!: (value: unknown) => void;
    fake.handler = command => command.m === 'openDm'
      ? new Promise(resolve => { resolveDm = resolve; })
      : Promise.resolve(null);
    const navigate = vi.fn();
    const runtime = { navigate, call: {} as never, openThread: () => undefined, openSearch: () => undefined };
    const user = userEvent.setup();
    render(<CoreProvider client={fake}><CommandProvider runtime={runtime}>
      <CommandHost communityId={communityId} channelId={channelId} bits={0x1ffffff} items={[]} selfId={selfId}
        draft="/dm" onDraft={() => undefined} onSendLiteral={() => undefined} encodeMessage={text => text} openGif={() => undefined} gifsEnabled />
    </CommandProvider></CoreProvider>);
    await user.type(await screen.findByRole('combobox', { name: 'user' }), 'bob');
    await user.click(await screen.findByRole('option', { name: /^bob,/ }));
    await user.click(screen.getByRole('button', { name: 'send' }));
    await waitFor(() => expect(fake.callsOf('openDm')).toHaveLength(1));
    await user.click(screen.getByRole('combobox', { name: 'user' }));
    await user.keyboard('{Escape}');
    await act(() => { resolveDm({ channelId: '88'.repeat(16) }); return Promise.resolve(); });
    expect(navigate).not.toHaveBeenCalled();
  });
});


it.each([true, false])('preserves suggestion ownership when a provider handles the command: %s', async handled => {
  const core = new FakeClient(); const community = 'c1'.repeat(16); const target = 'c2'.repeat(16);
  core.set(`members:${community}`, [{ userId: target, username: 'known member', display: '', kind: 0, roleIds: [] }]);
  const user = userEvent.setup();
  render(<CoreProvider client={core}><CommandProvider suggest={() => Promise.resolve(handled ? [] : undefined)}>
    <CommandHost communityId={community} channelId={'c3'.repeat(16)} items={[]} selfId={'c4'.repeat(16)} draft="/dm"
      onDraft={() => undefined} onSendLiteral={() => undefined} encodeMessage={text => text} openGif={() => undefined} gifsEnabled />
  </CommandProvider></CoreProvider>);
  await user.type(await screen.findByRole('combobox', { name: 'user' }), 'known');
  await waitFor(() => expect(screen.queryByRole('option', { name: /^known member,/ }) !== null).toBe(!handled));
});


it('reopens autocomplete after canceling and clearing the slash draft', async () => {
  const { user } = hostHarness();
  const message = screen.getByRole('textbox', { name: 'message' });
  fireEvent.change(message, { target: { value: '/' } });
  const picker = await screen.findByRole('textbox', { name: 'Command' });
  picker.focus();
  await user.keyboard('{Escape}{Escape}');
  expect(screen.queryByRole('textbox', { name: 'Command' })).not.toBeInTheDocument();
  fireEvent.change(message, { target: { value: '' } });
  fireEvent.change(message, { target: { value: '/' } });
  await screen.findByRole('textbox', { name: 'Command' });
});

it.each(['success', 'failure', 'uncertain'] as const)('adds a pending private result then maps %s with the handler message', async status => {
  const core = new FakeClient();
  let finish!: (value: { status: typeof status; private: boolean; message: string }) => void;
  const handlers: CommandProviderHandlers = { me: () => new Promise(resolve => { finish = resolve; }) };
  render(<CoreProvider client={core}><CommandProvider handlers={handlers}>
    <CommandHost communityId={null} channelId="private-test" items={[]} selfId={null} draft="/me hello" onDraft={() => undefined}
      onSendLiteral={() => undefined} encodeMessage={text => text} openGif={() => undefined} gifsEnabled />
  </CommandProvider></CoreProvider>);
  const store = privateResults(core, ':', 'dm/private-test/');
  await userEvent.setup().click(await screen.findByRole('button', { name: 'send' }));
  expect(store.rows()[0]).toMatchObject({ status: 'pending', provider: 'builtin' });
  expect(screen.queryByText('Running command privately…')).not.toBeInTheDocument();
  await act(() => { finish({ status, private: true, message: 'handler message' }); return Promise.resolve(); });
  expect(store.rows()[0]).toMatchObject({ status: status === 'success' ? 'success' : 'failed', text: 'handler message' });
});

it('Enter on an unknown command adds the exact failed private result', async () => {
  const { user, fake, channelId, literals } = hostHarness();
  fireEvent.change(screen.getByRole('textbox', { name: 'message' }), { target: { value: '/xyz' } });
  const field = await screen.findByRole('textbox', { name: 'Command' });
  await user.click(field); await user.keyboard('{Enter}');
  expect(privateResults(fake, ':', `dm/${channelId}/`).rows()[0]).toMatchObject({ status: 'failed', text: '/xyz isn’t a command here. To post it as text, start with //.' });
  expect(fake.calls).toEqual([]);
  expect(literals).toEqual([]);
});

it.each([['kick', false], ['kick', true], ['ban', false], ['ban', true]] as const)('%s confirmation retains the prototype read-only details and accepts=%s exactly once', async (action, accepted) => {
  const { core, user } = moderationHarness(true, action);
  const label = action === 'kick' ? 'Kick' : 'Ban';
  await user.type(await screen.findByRole('combobox', { name: 'user' }), 'Bea');
  await user.click(await screen.findByRole('option', { name: /^Bea,/ }));
  await user.click(screen.getByRole('button', { name: 'send' }));
  const dialog = await screen.findByRole('dialog', { name: `${label} \u2068Bea\u2069 from \u2068Town\u2069?` });
  expect(screen.getByText(action === 'kick' ? '\u2068Bea\u2069 leaves now and can come back only with a new invite.' : '\u2068Bea\u2069 leaves now and can’t rejoin for 1 day.')).toBeInTheDocument();
  expect(screen.getByText('modlog · visible to moderators')).toBeInTheDocument();
  expect(screen.getAllByRole('button', { name: /^(Close|Cancel|Kick|Ban)$/ })).toHaveLength(2);
  expect(within(dialog).getByText('Duration').parentElement).toHaveTextContent(action === 'kick' ? /^Duration—$/ : /^Duration1 day$/);
  expect(within(dialog).getByText('Reason').parentElement).toHaveTextContent(/^Reasonnone given$/);
  expect(within(dialog).getByText('Logged').parentElement).toHaveTextContent(/^Loggedmodlog · visible to moderators$/);
  // Parity sweep 2026-11-05 (Jonas): kick has the prototype's optional "reason?" chip behind the confirm, as ban does.
  expect(screen.getByRole('textbox', { name: 'reason' })).toHaveAttribute('placeholder', 'optional');
  expect(privateResults(core, ':', `${'55'.repeat(16)}/${'44'.repeat(16)}/`).rows()).toHaveLength(0);
  if (accepted) {
    const confirm = screen.getByRole('button', { name: label });
    await user.keyboard('{Enter>3/}');
    expect(core.callsOf(action === 'kick' ? 'kickMember' : 'banMember')).toHaveLength(0);
    await user.dblClick(confirm);
  } else await user.click(screen.getByRole('button', { name: 'Cancel' }));
  await waitFor(() => expect(privateResults(core, ':', `${'55'.repeat(16)}/${'44'.repeat(16)}/`).rows()[0].status).toBe(accepted ? 'success' : 'cancelled'));
  expect(core.callsOf(action === 'kick' ? 'kickMember' : 'banMember')).toHaveLength(accepted ? 1 : 0);
  if (!accepted) expect(privateResults(core, ':', `${'55'.repeat(16)}/${'44'.repeat(16)}/`).rows()[0].text).toBe(`${label} cancelled. Nothing changed.`);
});

it('remind starts with the prototype pending text and records a dropped capability failure', async () => {
  const core = new FakeClient(); const channel = '11'.repeat(16); const community = '22'.repeat(16);
  let resolve!: (value: { status: 'failure'; private: true; message: string }) => void;
  const handler = vi.fn(() => new Promise<{ status: 'failure'; private: true; message: string }>(done => { resolve = done; }));
  render(<CoreProvider client={core}><CommandProvider handlers={{ remind: handler }}>
    <CommandHost communityId={community} channelId={channel} items={[]} selfId={null} draft="/remind" onDraft={() => undefined}
      onSendLiteral={() => undefined} encodeMessage={text => text} openGif={() => undefined} gifsEnabled />
  </CommandProvider></CoreProvider>);
  const user = userEvent.setup();
  await user.type(await screen.findByRole('combobox', { name: 'when' }), 'in 1 hour');
  await user.click(screen.getByRole('textbox', { name: 'text' }));
  await user.type(screen.getByRole('textbox', { name: 'text' }), 'bring two ropes');
  await user.click(screen.getByRole('button', { name: 'send' }));
  const store = privateResults(core, ':', `${community}/${channel}/`);
  expect(store.rows()[0]).toMatchObject({ status: 'pending', text: 'Saving your reminder…', provider: 'bot' });
  await act(() => { resolve({ status: 'failure', private: true, message: 'This command is temporarily unavailable.' }); return Promise.resolve(); });
  expect(store.rows()[0]).toMatchObject({ status: 'failed', text: 'This command is temporarily unavailable.' });
  expect(handler).toHaveBeenCalledOnce();
});


it('a capability disabled with remind chips open becomes the existing unavailable failed result', async () => {
  const core = new FakeClient(); const cid = '31'.repeat(16); const channel = '32'.repeat(16);
  let enabled = true;
  const capabilities = vi.fn(() => Promise.resolve({ installationId: '33'.repeat(16), botUserId: '34'.repeat(16), version: 1 as const, supportedOperations: enabled ? [80] : [] }));
  const spy = vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({ capabilities } as unknown as ClientCore.BotUtilities);
  function Harness() {
    const provider = useUtilityCommandProvider(core, cid);
    return <CommandProvider handlers={provider.handlers}><CommandHost communityId={cid} channelId={channel} bits={ClientCore.PERM.sendMessages}
      items={[]} selfId={null} draft="/remind" onDraft={() => undefined} onSendLiteral={() => undefined} encodeMessage={text => text} openGif={() => undefined} gifsEnabled /></CommandProvider>;
  }
  try {
    render(<CoreProvider client={core}><Harness /></CoreProvider>);
    const user = userEvent.setup();
    await user.type(await screen.findByRole('combobox', { name: 'when' }), 'in 1 hour');
    await user.type(screen.getByRole('textbox', { name: 'text' }), 'bring two ropes');
    enabled = false;
    await user.click(screen.getByRole('button', { name: 'send' }));
    await waitFor(() => expect(privateResults(core, ':', `${cid}/${channel}/`).rows()[0]).toMatchObject({ status: 'failed', text: 'Reminders are unavailable in this channel.', provider: 'bot' }));
    expect(core.calls).toEqual([]);
  } finally { spy.mockRestore(); }
});

it('returns to the empty chat composer while a private command is pending, as in the prototype', async () => {
  const core = new FakeClient();
  let finish!: (value: unknown) => void;
  core.handler = call => call.m === 'send' ? new Promise(resolve => { finish = resolve; }) : Promise.resolve(null);
  const { user } = hostHarness(core);
  fireEvent.change(screen.getByRole('textbox', { name: 'message' }), { target: { value: '/me' } });
  await user.type(await screen.findByRole('textbox', { name: 'text' }), 'hello');
  await user.click(screen.getByRole('button', { name: 'send' }));
  expect(screen.getByText('chat mode')).toBeInTheDocument();
  expect(screen.getByRole('textbox', { name: 'message' })).toHaveValue('');
  expect(screen.queryByRole('textbox', { name: 'text' })).not.toBeInTheDocument();
  await act(() => { finish(null); return Promise.resolve(); });
});

it('does not create a duplicate host live region for a completed private result', async () => {
  const { user, fake, channelId } = hostHarness();
  fireEvent.change(screen.getByRole('textbox', { name: 'message' }), { target: { value: '/me hello' } });
  await user.click(await screen.findByRole('button', { name: 'send' }));
  await waitFor(() => expect(privateResults(fake, ':', `dm/${channelId}/`).rows()[0]?.status).toBe('success'));
  expect(document.querySelector('[data-command-result]')).toBeNull();
});

it('retains the request ID when an uncertain response arrives after the host unmounts', async () => {
  const core = new FakeClient(); const ids: string[] = [];
  let finish!: (value: { status: 'uncertain'; private: true; message: string }) => void;
  const handlers: CommandProviderHandlers = { me: (_invocation, context) => {
    ids.push(context.requestId);
    return ids.length === 1 ? new Promise(resolve => { finish = resolve; }) : Promise.resolve({ status: 'success', private: true, message: 'retried safely' });
  } };
  function Harness() {
    const [mounted, setMounted] = useState(true); const [draft, setDraft] = useState('/me hello');
    return <CommandProvider handlers={handlers}><button onClick={() => setMounted(value => !value)}>toggle pending host</button>
      {mounted ? <CommandHost communityId={null} channelId="late-result" items={[]} selfId={null} draft={draft} onDraft={setDraft}
        onSendLiteral={() => undefined} encodeMessage={text => text} openGif={() => undefined} gifsEnabled /> : null}
    </CommandProvider>;
  }
  render(<CoreProvider client={core}><Harness /></CoreProvider>);
  const user = userEvent.setup();
  await user.click(await screen.findByRole('button', { name: 'send' }));
  await user.click(screen.getByRole('button', { name: 'toggle pending host' }));
  await act(() => { finish({ status: 'uncertain', private: true, message: 'check before retrying' }); return Promise.resolve(); });
  await user.click(screen.getByRole('button', { name: 'toggle pending host' }));
  await user.click(await screen.findByRole('button', { name: 'send' }));
  expect(ids).toHaveLength(2);
  expect(ids[1]).toBe(ids[0]);
});

it('permanent ban passes from the picker through confirmation to banMember with null seconds', async () => {
  const { core, user } = moderationHarness(true, 'ban');
  await user.type(await screen.findByRole('combobox', { name: 'user' }), 'Bea');
  await user.click(await screen.findByRole('option', { name: /^Bea,/ }));
  await user.type(screen.getByRole('combobox', { name: 'duration' }), 'permanent');
  await user.click(screen.getByRole('button', { name: 'send' }));
  const dialog = await screen.findByRole('dialog');
  expect(dialog).toHaveTextContent('\u2068Bea\u2069 leaves now and can’t rejoin permanently.');
  expect(dialog).toHaveTextContent('permanent');
  await user.click(screen.getByRole('button', { name: 'Ban' }));
  await waitFor(() => expect(core.callsOf('banMember')).toEqual([{ m: 'banMember', communityId: '55'.repeat(16), userId: '88'.repeat(16), reason: '', seconds: null }]));
});

it.each(['conversation', 'account', 'unmount', 'backdrop'])('confirmation teardown on %s creates no result and never clears the next draft', async reason => {
  const { core, view, user } = moderationHarness();
  await user.type(await screen.findByRole('combobox', { name: 'user' }), 'Bea');
  await user.click(await screen.findByRole('option', { name: /^Bea,/ }));
  await user.click(screen.getByRole('button', { name: 'send' }));
  await screen.findByRole('dialog');
  const onDraft = vi.fn();
  if (reason === 'unmount') view.unmount();
  else if (reason === 'backdrop') fireEvent.click(screen.getByRole('dialog'));
  else view.rerender(<CoreProvider client={core}><CommandHost communityId={'55'.repeat(16)} channelId={reason === 'conversation' ? 'new-channel' : '44'.repeat(16)} items={[]}
    selfId={reason === 'account' ? 'new-account' : '66'.repeat(16)} draft="Keep this draft" onDraft={onDraft} onSendLiteral={() => undefined}
    encodeMessage={text => text} openGif={() => undefined} gifsEnabled /></CoreProvider>);
  await act(() => Promise.resolve());
  expect(privateResults(core, ':', `${'55'.repeat(16)}/${'44'.repeat(16)}/`).rows()).toEqual([]);
  expect(onDraft).not.toHaveBeenCalled();
  expect(core.callsOf('kickMember')).toEqual([]);
});

it('an explicit non-moderation Cancel closes silently without a private result', async () => {
  const core = new FakeClient();
  const handlers: CommandProviderHandlers = { me: async (_invocation, context) => ({ status: await context.confirm('Proceed?', 'Confirm this action') ? 'success' : 'failure', private: true, message: 'Cancelled.' }) };
  render(<CoreProvider client={core}><CommandProvider handlers={handlers}><CommandHost communityId={null} channelId="other-confirm" items={[]} selfId={null} draft="/me hello"
    onDraft={() => undefined} onSendLiteral={() => undefined} encodeMessage={text => text} openGif={() => undefined} gifsEnabled /></CommandProvider></CoreProvider>);
  const user = userEvent.setup();
  await user.click(await screen.findByRole('button', { name: 'send' }));
  await user.click(await screen.findByRole('button', { name: 'Cancel' }));
  await waitFor(() => expect(privateResults(core, ':', 'dm/other-confirm/').rows()).toEqual([]));
});

it('isolates RTL names in the moderation title and body', async () => {
  const { core, communityId, members, user } = moderationHarness();
  core.publish(`members:${communityId}`, members.map(member => member.username === 'bea' ? { ...member, display: 'ليلى' } : member));
  await user.type(await screen.findByRole('combobox', { name: 'user' }), 'ليلى');
  await user.click(await screen.findByRole('option', { name: /^ليلى,/ }));
  await user.click(screen.getByRole('button', { name: 'send' }));
  const dialog = await screen.findByRole('dialog', { name: 'Kick \u2068ليلى\u2069 from \u2068Town\u2069?' });
  expect(dialog).toHaveTextContent('\u2068ليلى\u2069 leaves now and can come back only with a new invite.');
});

it('a thrown command reports a generic failure and logs the original error', async () => {
  const core = new FakeClient(); const cause = new Error('private transport detail');
  const log = vi.spyOn(console, 'error').mockImplementation(() => undefined);
  try {
    render(<CoreProvider client={core}><CommandProvider handlers={{ me: () => Promise.reject(cause) }}><CommandHost communityId={null} channelId="throws" items={[]} selfId={null} draft="/me hello"
      onDraft={() => undefined} onSendLiteral={() => undefined} encodeMessage={text => text} openGif={() => undefined} gifsEnabled /></CommandProvider></CoreProvider>);
    await userEvent.setup().click(await screen.findByRole('button', { name: 'send' }));
    await waitFor(() => expect(privateResults(core, ':', 'dm/throws/').rows()[0]).toMatchObject({ status: 'failed', text: 'That didn’t work. Try again.' }));
    expect(log).toHaveBeenCalledWith('Command failed', cause);
    expect(screen.queryByText('private transport detail')).not.toBeInTheDocument();
  } finally { log.mockRestore(); }
});

it('a literal send abandons confirmation without running Cancel or clearing the next draft', async () => {
  const core = new FakeClient(); const onDraft = vi.fn(); const literal = vi.fn();
  const handlers: CommandProviderHandlers = { me: async (_invocation, context) => ({ status: await context.confirm('Proceed?', 'Confirm this action') ? 'success' : 'failure', private: true, message: 'Cancelled.' }) };
  function Harness() {
    const [draft, setDraft] = useState('/me hello');
    return <><input aria-label="external draft" value={draft} onChange={e => setDraft(e.currentTarget.value)} />
      <CommandProvider handlers={handlers}><CommandHost communityId={null} channelId="literal-confirm" items={[]} selfId={null} draft={draft}
        onDraft={text => { onDraft(text); setDraft(text); }} onSendLiteral={literal} encodeMessage={text => text} openGif={() => undefined} gifsEnabled /></CommandProvider></>;
  }
  render(<CoreProvider client={core}><Harness /></CoreProvider>);
  const user = userEvent.setup();
  await user.click(await screen.findByRole('button', { name: 'send' }));
  await screen.findByRole('dialog');
  fireEvent.change(screen.getByRole('textbox', { name: 'external draft' }), { target: { value: '/xyz' } });
  const send = await screen.findByRole('button', { name: 'Send as text' });
  onDraft.mockClear(); fireEvent.click(send);
  await act(() => Promise.resolve());
  expect(literal).toHaveBeenCalledWith('/xyz');
  expect(onDraft).not.toHaveBeenCalled();
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  expect(privateResults(core, ':', 'dm/literal-confirm/').rows()).toEqual([]);
});

it('Escape explicitly cancels a kick with the approved capitalised row and no mutation', async () => {
  const { core, user } = moderationHarness();
  await user.type(await screen.findByRole('combobox', { name: 'user' }), 'Bea');
  await user.click(await screen.findByRole('option', { name: /^Bea,/ }));
  await user.click(screen.getByRole('button', { name: 'send' }));
  await screen.findByRole('dialog'); await user.keyboard('{Escape}');
  await waitFor(() => expect(privateResults(core, ':', `${'55'.repeat(16)}/${'44'.repeat(16)}/`).rows()[0]).toMatchObject({ status: 'cancelled', text: 'Kick cancelled. Nothing changed.' }));
  expect(core.callsOf('kickMember')).toEqual([]);
});

it.each([
  { action: 'kick' as const, trigger: 'keyboard' }, { action: 'kick' as const, trigger: 'pointer' },
  { action: 'ban' as const, trigger: 'keyboard' }, { action: 'ban' as const, trigger: 'pointer' },
])('keeps $action suggestions and composer focus paint behind the $trigger confirmation while focus stays in the dialog', async ({ action, trigger }) => {
  const { core, user } = moderationHarness(true, action);
  await user.type(await screen.findByRole('combobox', { name: 'user' }), 'Bea');
  await user.click(await screen.findByRole('option', { name: /^Bea,/ }));
  const field = screen.getByRole('combobox', { name: action === 'kick' ? 'user' : 'duration' });
  await user.click(field);
  const suggestions = await screen.findByRole('listbox', { name: action === 'kick' ? 'Members' : 'Durations' });
  expect(suggestions).toBeVisible();
  if (trigger === 'keyboard') await user.keyboard('{Enter}');
  else await user.click(screen.getByRole('button', { name: 'send' }));
  const dialog = await screen.findByRole('dialog');
  expect(dialog).toContainElement(document.activeElement as HTMLElement);
  const retained = screen.getByRole('listbox', { name: action === 'kick' ? 'Members' : 'Durations' });
  expect(retained).toBeVisible();
  expect(dialog).not.toContainElement(retained);
  const box = document.querySelector('.d-composer__box') as HTMLElement;
  expect(box.closest('.dw-command-picker')).toHaveAttribute('data-confirming', 'true');
  expect(getComputedStyle(box).borderColor).toBe('rgb(90, 168, 230)');
  expect(getComputedStyle(box).boxShadow).toBe('0 0 0 3px rgba(90, 168, 230, 0.12)');
  expect(core.callsOf(action === 'kick' ? 'kickMember' : 'banMember')).toEqual([]);
  await user.keyboard('{Escape}');
  expect(screen.queryByRole('dialog')).toBeNull();
  expect(screen.queryByRole('listbox')).toBeNull();
  expect(core.callsOf(action === 'kick' ? 'kickMember' : 'banMember')).toEqual([]);
});

it.each(['kick', 'ban'] as const)('holding Enter on %s Confirm runs the existing command path once', async action => {
  const { core, user } = moderationHarness(true, action);
  await user.type(await screen.findByRole('combobox', { name: 'user' }), 'Bea');
  await user.click(await screen.findByRole('option', { name: /^Bea,/ }));
  await user.click(screen.getByRole('button', { name: 'send' }));
  const confirm = await screen.findByRole('button', { name: action === 'kick' ? 'Kick' : 'Ban' });
  act(() => confirm.focus());
  await user.keyboard('{Enter>3/}');
  await waitFor(() => expect(core.callsOf(action === 'kick' ? 'kickMember' : 'banMember')).toHaveLength(1));
});

it('double-clicking Kick confirms exactly once', async () => {
  const { core, user } = moderationHarness(true, 'kick');
  await user.type(await screen.findByRole('combobox', { name: 'user' }), 'Bea');
  await user.click(await screen.findByRole('option', { name: /^Bea,/ }));
  await user.click(screen.getByRole('button', { name: 'send' }));
  await user.dblClick(await screen.findByRole('button', { name: 'Kick' }));
  await waitFor(() => expect(core.callsOf('kickMember')).toHaveLength(1));
});

it.each(['post', 'cancel'] as const)('keeps poll dialog out of the feed until Post poll (%s)', async action => {
  const core = new FakeClient(); const cid = 'd1'.repeat(16), channel = 'd2'.repeat(16), selfId = 'd3'.repeat(16), botId = 'd4'.repeat(16), installationId = 'd5'.repeat(16);
  core.set('account', { user: { id: selfId }, instance: { id: 'instance' }, deviceId: 'device' } as never);
  core.set('communities', [{ id: cid, name: 'Town', owner: selfId }]);
  core.set(`channels:${cid}`, [{ id: channel, name: 'general', kind: 0, group: 'active' }] as never);
  let complete!: () => void;
  const pollCreate = vi.fn(() => new Promise<void>(resolve => { complete = resolve; }));
  const spy = vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({ capabilities: () => Promise.resolve({ installationId, botUserId: botId, supportedOperations: [70] }), pollCreate,
    reminderList: () => Promise.resolve({ items: [], nextCursor: null }) } as unknown as ClientCore.BotUtilities);
  function Harness() {
    const provider = useUtilityCommandProvider(core, cid);
    const [draft, setDraft] = useState('/poll create');
    return <CommandProvider handlers={provider.handlers}>{provider.dialogs}<CommandHost communityId={cid} channelId={channel} bits={ClientCore.PERM.sendMessages}
      items={[]} selfId={selfId} draft={draft} onDraft={setDraft} onSendLiteral={() => undefined} encodeMessage={text => text} openGif={() => undefined} gifsEnabled /></CommandProvider>;
  }
  try {
    const view = render(<CoreProvider client={core}><Harness /></CoreProvider>);
    const user = userEvent.setup();
    await screen.findByRole('dialog', { name: 'New poll in #general' });
    const store = privateResults(core, `instance:${selfId}`, `${cid}/${channel}/`);
    expect(store.rows()).toEqual([]);
    if (action === 'cancel') {
      await user.click(screen.getByRole('button', { name: 'Cancel' }));
      expect(store.rows()).toEqual([]); expect(pollCreate).not.toHaveBeenCalled();
    } else {
      await user.type(screen.getByRole('textbox', { name: /Question/ }), 'Lunch?');
      await user.type(screen.getByRole('textbox', { name: 'Answer 1' }), 'Soup');
      await user.type(screen.getByRole('textbox', { name: 'Answer 2' }), 'Salad');
      await user.click(screen.getByRole('button', { name: 'Post poll' }));
      await waitFor(() => expect(store.rows()[0]).toMatchObject({ status: 'pending', provider: 'bot' }));
      expect(pollCreate).toHaveBeenCalledOnce(); await act(() => { complete(); return Promise.resolve(); });
      await waitFor(() => expect(store.rows()[0]).toMatchObject({ status: 'success', text: 'Poll posted' }));
    }
    view.unmount();
  } finally { spy.mockRestore(); }
});

describe('the Bot command list outlives the picker', () => {
  const cid = 'a1'.repeat(16); const channel = 'a2'.repeat(16);
  const caps = (ops: number[]) => ({ installationId: '33'.repeat(16), botUserId: '34'.repeat(16), version: 1 as const, supportedOperations: ops });
  function setup(load: () => Promise<unknown>) {
    vi.useFakeTimers();
    const core = new FakeClient();
    const capabilities = vi.fn(load);
    vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({ capabilities } as unknown as ClientCore.BotUtilities);
    let ordinary = '';
    const run = vi.fn(() => Promise.resolve({ status: 'success' as const, private: true, message: 'ok' }));
    function Harness() {
      const [, rerender] = useState(0);
      return <CommandProvider handlers={{ play: run }}>
        <textarea aria-label="message" value={ordinary} onChange={event => { ordinary = event.currentTarget.value; rerender(n => n + 1); }} />
        <CommandHost communityId={cid} channelId={channel} bits={0x1ffffff} items={[]} selfId={'22'.repeat(16)} draft={ordinary}
          onDraft={text => { ordinary = text; rerender(n => n + 1); }} onSendLiteral={() => undefined} encodeMessage={text => text} openGif={() => undefined} gifsEnabled />
      </CommandProvider>;
    }
    const view = render(<CoreProvider client={core}><Harness /></CoreProvider>);
    // The first slash opens the picker; the rest is typed into it, as a person does.
    const typeSlash = (text: string) => {
      fireEvent.change(screen.getByRole('textbox', { name: 'message' }), { target: { value: '/' } });
      if (text !== '/') fireEvent.change(screen.getByRole('textbox', { name: 'Command' }), { target: { value: text } });
    };
    return { core, capabilities, view, typeSlash, run };
  }
  const slow = (ms: number, ops = [3, 12]) => () => new Promise(resolve => { setTimeout(() => { resolve(caps(ops)); }, ms); });
  afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); });

  it('the list survives the picker closing', async () => {
    const { capabilities, typeSlash } = setup(slow(5_000));
    typeSlash('/');
    await act(() => vi.advanceTimersByTimeAsync(1_000));
    fireEvent.keyDown(screen.getByRole('textbox', { name: 'Command' }), { key: 'Escape' });
    expect(screen.queryByRole('textbox', { name: 'Command' })).not.toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(5_000));
    typeSlash('/');
    expect(screen.getByRole('option', { name: /\/play/ })).toBeInTheDocument();
    expect(capabilities).toHaveBeenCalledTimes(1);
  });

  it('the picker says it is loading while the list is unknown', async () => {
    const { typeSlash } = setup(slow(5_000));
    typeSlash('/play');
    await act(() => vi.advanceTimersByTimeAsync(500));
    expect(screen.getByText('Loading Bot commands…')).toBeInTheDocument();
    expect(screen.queryByText(/No command matches/)).not.toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(5_000));
    expect(screen.queryByText('Loading Bot commands…')).not.toBeInTheDocument();
    expect(screen.getByRole('option', { name: /\/play/ })).toBeInTheDocument();
  });

  it('Enter before the list loads waits for it', async () => {
    const { core, typeSlash } = setup(slow(5_000));
    const refusals = () => privateResults(core, ':', `${cid}/${channel}/`).rows().filter(row => /isn’t a command here/.test(row.text));
    typeSlash('/play x');
    await act(() => vi.advanceTimersByTimeAsync(1_000));
    fireEvent.keyDown(screen.getByRole('textbox', { name: 'Command' }), { key: 'Enter' });
    await act(() => vi.advanceTimersByTimeAsync(500));
    expect(refusals()).toHaveLength(0);
    await act(() => vi.advanceTimersByTimeAsync(5_000));
    expect(refusals()).toHaveLength(0);
    expect(screen.getByLabelText('query-or-url')).toHaveValue('x');
  });

  it('Enter on a name that never loads gives the refusal after at most 10 s', async () => {
    const { core, typeSlash } = setup(() => new Promise(() => undefined));
    const refusals = () => privateResults(core, ':', `${cid}/${channel}/`).rows().filter(row => /isn’t a command here/.test(row.text));
    typeSlash('/play x');
    fireEvent.keyDown(screen.getByRole('textbox', { name: 'Command' }), { key: 'Enter' });
    await act(() => vi.advanceTimersByTimeAsync(9_000));
    expect(refusals()).toHaveLength(0);
    await act(() => vi.advanceTimersByTimeAsync(1_500));
    expect(refusals()).toHaveLength(1);
  });

  it('a failed load keeps the last good list and retries; a failure never replaces a good list', async () => {
    let attempt = 0;
    const { core, capabilities, typeSlash } = setup(() => {
      attempt += 1;
      if (attempt === 1) return Promise.reject(new Error('no active interaction group'));
      if (attempt === 2) return Promise.resolve(caps([3, 12]));
      return Promise.reject(new Error('timed out'));
    });
    typeSlash('/');
    await act(() => vi.advanceTimersByTimeAsync(100));
    expect(screen.queryByRole('option', { name: /\/play/ })).not.toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(2_000)); // the first quick retry
    expect(screen.getByRole('option', { name: /\/play/ })).toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(30_000));
    expect(capabilities).toHaveBeenCalledTimes(3);
    expect(screen.getByRole('option', { name: /\/play/ })).toBeInTheDocument();
    expect(core.calls.filter(call => call.m === 'callNote')).toEqual([{ m: 'callNote', code: 'E_BOT_CAPABILITIES' }, { m: 'callNote', code: 'E_BOT_CAPABILITIES' }]);
  });

  // Wave C task 11 (root cause bug 3): after a reload there is no good list, and a failed first load waited a full 30 s refresh.
  it('a failed load with no good list retries at 2 s, 5 s and 10 s, says "loading" meanwhile, then falls back to the 30 s refresh', async () => {
    const { capabilities, typeSlash } = setup(() => Promise.reject(new Error('no active interaction group')));
    typeSlash('/play');
    await act(() => vi.advanceTimersByTimeAsync(100));
    expect(capabilities).toHaveBeenCalledTimes(1);
    expect(screen.getByText('Loading Bot commands…')).toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(1_800));
    expect(capabilities).toHaveBeenCalledTimes(1);
    await act(() => vi.advanceTimersByTimeAsync(300)); // 2.2 s
    expect(capabilities).toHaveBeenCalledTimes(2);
    await act(() => vi.advanceTimersByTimeAsync(5_000)); // 7.2 s: the second retry came 5 s after the second failure
    expect(capabilities).toHaveBeenCalledTimes(3);
    expect(screen.getByText('Loading Bot commands…')).toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(10_000)); // 17.2 s
    expect(capabilities).toHaveBeenCalledTimes(4);
    expect(screen.queryByText('Loading Bot commands…')).not.toBeInTheDocument(); // the quick retries are spent
    await act(() => vi.advanceTimersByTimeAsync(12_000)); // 29.2 s: nothing yet, the 30 s refresh is the next
    expect(capabilities).toHaveBeenCalledTimes(4);
    await act(() => vi.advanceTimersByTimeAsync(1_000)); // the interval tick at 30 s
    expect(capabilities).toHaveBeenCalledTimes(5);
  });

  it('the first retry lands the list as soon as it loads', async () => {
    let attempt = 0;
    const { typeSlash } = setup(() => { attempt += 1; return attempt === 1 ? Promise.reject(new Error('overran')) : Promise.resolve(caps([3, 12])); });
    typeSlash('/');
    await act(() => vi.advanceTimersByTimeAsync(2_100));
    expect(screen.getByRole('option', { name: /\/play/ })).toBeInTheDocument();
    expect(screen.queryByText('Loading Bot commands…')).not.toBeInTheDocument();
  });

  it('opening the picker after a failed load retries at once (a good list was never replaced)', async () => {
    let attempt = 0;
    const { capabilities, typeSlash } = setup(() => { attempt += 1; return attempt === 2 ? Promise.resolve(caps([3, 12])) : Promise.reject(new Error('timed out')); });
    typeSlash('/');
    await act(() => vi.advanceTimersByTimeAsync(100));
    expect(capabilities).toHaveBeenCalledTimes(1);
    fireEvent.keyDown(screen.getByRole('textbox', { name: 'Command' }), { key: 'Escape' });
    expect(screen.queryByRole('textbox', { name: 'Command' })).not.toBeInTheDocument();
    typeSlash('/'); // opened again, well inside the 2 s retry
    await act(() => vi.advanceTimersByTimeAsync(100));
    expect(capabilities).toHaveBeenCalledTimes(2);
    expect(screen.getByRole('option', { name: /\/play/ })).toBeInTheDocument();
  });

  it('a Bot that is gone clears the list instead of keeping the stale one', async () => {
    let attempt = 0;
    const { capabilities, core, typeSlash } = setup(() => { attempt += 1; return attempt === 1 ? Promise.resolve(caps([3, 12])) : Promise.reject(new ClientCore.BotUtilitiesProviderUnavailableError(3)); });
    typeSlash('/');
    await act(() => vi.advanceTimersByTimeAsync(100));
    expect(screen.getByRole('option', { name: /\/play/ })).toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(30_000));
    expect(capabilities).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole('option', { name: /\/play/ })).not.toBeInTheDocument();
    expect(screen.queryByText('Loading Bot commands…')).not.toBeInTheDocument();
    expect(core.calls.filter(call => call.m === 'callNote')).toEqual([]); // no Bot is no fault
    await act(() => vi.advanceTimersByTimeAsync(10_000));
    expect(capabilities).toHaveBeenCalledTimes(2); // and no quick retry
  });

  it('community B never shows community A\'s list', async () => {
    vi.useFakeTimers();
    const core = new FakeClient();
    const a = 'b1'.repeat(16); const b = 'b2'.repeat(16);
    const capabilities = vi.fn()
      .mockImplementation(() => Promise.resolve(caps([3, 12])));
    vi.spyOn(ClientCore, 'createBotUtilities').mockImplementation((_client, community) => ({
      capabilities: community === a ? capabilities : () => new Promise(() => undefined),
    }) as unknown as ClientCore.BotUtilities);
    let ordinary = '';
    function Harness() {
      const [, rerender] = useState(0);
      const [community, setCommunity] = useState(a);
      return <CommandProvider handlers={{ play: () => Promise.resolve({ status: 'success' as const, private: true, message: 'ok' }) }}>
        <button type="button" onClick={() => { ordinary = ''; setCommunity(b); }}>switch</button>
        <textarea aria-label="message" value={ordinary} onChange={event => { ordinary = event.currentTarget.value; rerender(n => n + 1); }} />
        <CommandHost communityId={community} channelId={channel} bits={0x1ffffff} items={[]} selfId={'22'.repeat(16)} draft={ordinary}
          onDraft={text => { ordinary = text; rerender(n => n + 1); }} onSendLiteral={() => undefined} encodeMessage={text => text} openGif={() => undefined} gifsEnabled />
      </CommandProvider>;
    }
    render(<CoreProvider client={core}><Harness /></CoreProvider>);
    fireEvent.change(screen.getByRole('textbox', { name: 'message' }), { target: { value: '/' } });
    await act(() => vi.advanceTimersByTimeAsync(100));
    expect(screen.getByRole('option', { name: /\/play/ })).toBeInTheDocument();
    fireEvent.keyDown(screen.getByRole('textbox', { name: 'Command' }), { key: 'Escape' });
    fireEvent.click(screen.getByRole('button', { name: 'switch' }));
    await act(() => vi.advanceTimersByTimeAsync(100));
    fireEvent.change(screen.getByRole('textbox', { name: 'message' }), { target: { value: '/' } });
    fireEvent.change(screen.getByRole('textbox', { name: 'Command' }), { target: { value: '/play' } });
    expect(screen.queryByRole('option', { name: /\/play/ })).not.toBeInTheDocument();
    expect(screen.getByText('Loading Bot commands…')).toBeInTheDocument();
  });
});
