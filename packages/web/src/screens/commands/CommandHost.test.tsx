import { useState } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient } from '../../test/fake-client.ts';
import { clearCommandDraftMemory, CommandHost } from './CommandHost.tsx';
import { CommandProvider } from './CommandContext.tsx';
import type { CommandProviderHandlers } from './command-types.ts';

beforeEach(() => clearCommandDraftMemory());

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

function moderationHarness(joined = true) {
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
  render(<CoreProvider client={core}><CommandHost communityId={communityId} channelId={'44'.repeat(16)} bits={1 << 12}
    items={[]} selfId={selfId} draft="/kick" onDraft={() => undefined} onSendLiteral={() => undefined}
    encodeMessage={text => text} openGif={() => undefined} gifsEnabled /></CoreProvider>);
  return { core, communityId, access, roles, members, user: userEvent.setup() };
}

describe('CommandHost', () => {
  it('routes a slash draft into the production built-in handler and publishes the resulting message', async () => {
    const { user, fake, channelId } = hostHarness();
    fireEvent.change(screen.getByRole('textbox', { name: 'message' }), { target: { value: '/me' } });
    expect(screen.getByText('command mode')).toBeInTheDocument();
    const field = screen.getByRole('combobox', { name: 'text' });
    await user.type(field, 'waves');
    await user.click(screen.getByRole('button', { name: /run/i }));
    await screen.findByText('Sent.');
    expect(fake.callsOf('send')).toEqual([{ m: 'send', channelId, text: '_waves_' }]);
  });

  it('executes a complete command typed after the first slash opens the picker', async () => {
    const { user, fake, channelId } = hostHarness();
    fireEvent.change(screen.getByRole('textbox', { name: 'message' }), { target: { value: '/' } });
    const picker = await screen.findByRole('combobox', { name: 'Command' });
    await user.type(picker, 'me waves hello');
    await user.keyboard('{Enter}');
    expect(await screen.findByRole('combobox', { name: 'text' })).toHaveValue('waves hello');
    await user.click(screen.getByRole('button', { name: /run/i }));
    await screen.findByText('Sent.');
    expect(fake.callsOf('send')).toEqual([{ m: 'send', channelId, text: '_waves hello_' }]);
  });

  it('keeps keyboard focus in the command picker after the first slash', async () => {
    const { user } = hostHarness();
    fireEvent.change(screen.getByRole('textbox', { name: 'message' }), { target: { value: '/' } });
    const picker = await screen.findByRole('combobox', { name: 'Command' });
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
    const picker = await screen.findByRole('combobox', { name: 'Command' });
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
    expect(await screen.findByRole('option', { name: 'target' })).toBeInTheDocument();
    await user.clear(field);
    await user.type(field, 'owner');
    await waitFor(() => expect(screen.queryByRole('option', { name: 'owner' })).toBeNull());
  });

  it('refreshes an empty moderation suggestion when the member arrives without retyping', async () => {
    const { core, communityId, members, user } = moderationHarness(false);
    const field = await screen.findByRole('combobox', { name: 'user' });
    await user.type(field, 'Bea');
    expect(screen.queryByRole('option', { name: 'Bea' })).not.toBeInTheDocument();
    core.publish(`members:${communityId}`, members);
    expect(await screen.findByRole('option', { name: 'Bea' })).toBeInTheDocument();
    expect(field).toHaveValue('Bea');
  });

  it.each(['permission', 'rank', 'membership'] as const)('removes a moderation suggestion after current %s changes', async change => {
    const { core, communityId, access, roles, members, user } = moderationHarness();
    const field = await screen.findByRole('combobox', { name: 'user' });
    await user.type(field, 'Bea');
    expect(await screen.findByRole('option', { name: 'Bea' })).toBeInTheDocument();
    if (change === 'permission') core.publish(`permissions:${communityId}`, { ...access, kickMembers: false });
    else if (change === 'rank') core.publish(`roles:${communityId}`, roles.map(role => role.id === roles[0].id ? { ...role, position: 5 } : role));
    else core.publish(`members:${communityId}`, members.slice(0, 1));
    await waitFor(() => expect(screen.queryByRole('option', { name: 'Bea' })).not.toBeInTheDocument());
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
    await user.click(await screen.findByRole('button', { name: /run/i }));
    await screen.findByText('uncertain');
    const first = ids[0];
    await user.click(screen.getByRole('button', { name: 'toggle host' }));
    await user.click(screen.getByRole('button', { name: 'toggle host' }));
    await user.click(await screen.findByRole('button', { name: /run/i }));
    await waitFor(() => expect(ids).toHaveLength(2));
    expect(ids[1]).toBe(first);
  });

  it('allocates a fresh request ID for a second intentional successful execution', async () => {
    const ids: string[] = [];
    const channelId = crypto.randomUUID().replaceAll('-', '').slice(0, 32);
    const handlers: CommandProviderHandlers = { me: (_invocation, context) => {
      ids.push(context.requestId); return Promise.resolve({ status: 'success' as const, private: true, message: 'done' });
    } };
    render(<CoreProvider client={new FakeClient()}><CommandProvider handlers={handlers}>
      <CommandHost communityId={null} channelId={channelId} items={[]} selfId={null} draft="/me hello" onDraft={() => undefined}
        onSendLiteral={() => undefined} encodeMessage={text => text} openGif={() => undefined} gifsEnabled />
    </CommandProvider></CoreProvider>);
    const user = userEvent.setup();
    const run = await screen.findByRole('button', { name: /run/i });
    await user.click(run);
    await screen.findByText('done');
    await user.click(screen.getByRole('button', { name: /run/i }));
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
    await user.click(await screen.findByRole('option', { name: 'bob' }));
    await user.click(screen.getByRole('button', { name: /run/i }));
    await waitFor(() => expect(fake.callsOf('openDm')).toHaveLength(1));
    await user.click(screen.getByRole('button', { name: /cancel/i }));
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
  await waitFor(() => expect(screen.queryByRole('option', { name: 'known member' }) !== null).toBe(!handled));
});


it('reopens autocomplete after canceling and clearing the slash draft', async () => {
  const { user } = hostHarness();
  const message = screen.getByRole('textbox', { name: 'message' });
  fireEvent.change(message, { target: { value: '/' } });
  const picker = await screen.findByRole('combobox', { name: 'Command' });
  picker.focus();
  await user.keyboard('{Escape}{Escape}');
  expect(screen.queryByRole('combobox', { name: 'Command' })).not.toBeInTheDocument();
  fireEvent.change(message, { target: { value: '' } });
  fireEvent.change(message, { target: { value: '/' } });
  await screen.findByRole('combobox', { name: 'Command' });
});
