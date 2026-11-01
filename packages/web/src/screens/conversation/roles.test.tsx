// Flow 09 in the conversation: role mentions in the composer, the send hint, pins by right, and role labels in the log.
import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { PERM, type PinnedItem, type RoleSummary } from '@dilla/client-core';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient } from '../../test/fake-client.ts';
import { account, ME } from '../../test/fixtures.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { GEN, MEMBERS, O1, OTHER_ROLE, PEER, P1, ROLE, ownRow, peerRow, timeline } from '../../test/conversation.tsx';
import { mentionCandidates, mentionMembers, nameBook } from '../shell-model.ts';
import { ComposerArea, type OutgoingMessage } from './ComposerArea.tsx';
import { Conversation, INITIAL_UI, type ConversationUi } from './Conversation.tsx';
import { toolbarLabels } from '../../test/toolbar.ts';

const GAME = '71'.repeat(16);
const MODS = '72'.repeat(16);
const EVERYONE = '70'.repeat(16);
const role = (id: string, name: string, over: Partial<RoleSummary> = {}): RoleSummary =>
  ({ id, name, color: 0, position: 3, hoist: false, mentionable: true, allow: 0, deny: 0, members: 0, ...over });
const ROLES = [role(EVERYONE, '@everyone', { position: 0 }), role(GAME, 'Game Night', { color: 0x336699 }), role(MODS, 'Modérateurs', { mentionable: false })];
const SEND = PERM.viewChannel | PERM.sendMessages;

function composer(over: { bits?: number | null; broadcast?: boolean; draft?: string; items?: ReturnType<typeof ownRow>[] } = {}) {
  const fake = new FakeClient();
  fake.set('account', account());
  const sent: OutgoingMessage[] = [];
  const dispatched: { type: string }[] = [];
  const book = nameBook({ id: ME.id, username: 'ada' }, MEMBERS, ROLES);
  function Harness() {
    const [draft, setDraft] = useState(over.draft ?? '');
    return <ComposerArea channelId={GEN} label="message #general" placeholder="message #general" blocked={null} broadcast={over.broadcast ?? true}
      bits={over.bits ?? null} roles={ROLES} channelName="general" encodeWith={mentionMembers(MEMBERS)} candidates={mentionCandidates(MEMBERS, ME.id)}
      items={over.items ?? []} book={book} draft={draft} onDraft={setDraft} ui={INITIAL_UI} dispatch={a => dispatched.push(a)} onSend={m => sent.push(m)} onError={() => undefined} />;
  }
  const user = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}><Harness /></CoreProvider></div>);
  return { user, view, sent, dispatched };
}
const box = () => screen.getByRole('textbox', { name: 'message #general' });
const optionNames = () => within(screen.getByRole('listbox')).getAllByRole('option').map(o => o.textContent);

describe('role mentions in the composer', () => {
  it('encodes a mentionable role handle to its token on send', async () => {
    const { user, sent } = composer({ bits: SEND });
    await user.type(box(), 'tonight @game-night!{Enter}');
    expect(sent.map(m => m.text)).toEqual([`tonight <@${GAME}>!`]);
  });
  it('offers a role as @handle with the secondary "role", and lists only the mentionable ones without the broadcast right', async () => {
    const { user } = composer({ bits: SEND });
    await user.type(box(), '@mod');
    expect(screen.queryByRole('listbox')).toBeNull();
    await user.clear(box());
    await user.type(box(), '@ga');
    expect(optionNames()).toEqual(['@Game Nightrole']);
  });
  it('offers every role, such as Modérateurs, to someone who may mention @everyone', async () => {
    const { user, sent } = composer({ bits: SEND | PERM.mentionEveryone });
    await user.type(box(), '@mod');
    expect(optionNames()).toEqual(['@Modérateursrole']);
    await user.keyboard('{Enter}');
    expect(box()).toHaveValue('@modérateurs ');
    await user.keyboard('x{Enter}');
    expect(sent.map(m => m.text)).toEqual([`<@${MODS}> x`]);
  });
  it('neither offers nor encodes @everyone without the bit, even where today’s rule would', async () => {
    const { user, sent } = composer({ bits: SEND, broadcast: true });
    await user.type(box(), '@every');
    expect(screen.queryByRole('listbox')).toBeNull();
    await user.type(box(), 'one {Enter}');
    expect(sent.map(m => m.text)).toEqual(['@everyone ']);
  });
  it('offers and encodes @everyone with the bit', async () => {
    const { user, sent } = composer({ bits: SEND | PERM.mentionEveryone, broadcast: false });
    await user.type(box(), '@every');
    expect(optionNames()).toEqual(['@everyoneeveryone in this channel']);
    await user.type(box(), 'one {Enter}');
    expect(sent.map(m => m.text)).toEqual(['<@everyone> ']);
  });
  it('keeps today’s broadcast rule while the bits are unknown', async () => {
    const { user, sent } = composer({ bits: null, broadcast: true });
    await user.type(box(), '@everyone {Enter}');
    expect(sent.map(m => m.text)).toEqual(['<@everyone> ']);
  });
  it('has no serious axe violations with the role list open', async () => {
    const { user, view } = composer({ bits: SEND });
    await user.type(box(), '@g');
    expect(screen.getByRole('listbox')).toBeInTheDocument();
    await expectNoAxeViolations(view.container);
  });
});

describe('the send hint', () => {
  it('keeps the composer read-only with chperm.cannotSend as its reason when the bits lack send', () => {
    composer({ bits: PERM.viewChannel });
    expect(box()).toHaveAttribute('readonly');
    expect(box()).toHaveAccessibleDescription('You cannot send messages in #general.');
  });
  it('sends nothing on Enter or the send button while it cannot send', async () => {
    const { user, sent } = composer({ bits: PERM.viewChannel, draft: 'hello' });
    box().focus();
    await user.keyboard('{Enter}');
    await user.click(screen.getByRole('button', { name: 'send' }));
    expect(sent).toEqual([]);
    expect(box()).toHaveValue('hello');
  });
  it('inserts no mention from the list while it cannot send', async () => {
    const { user } = composer({ bits: PERM.viewChannel, draft: '@ga' });
    box().focus();
    await user.keyboard('{End}{Enter}');
    expect(box()).toHaveValue('@ga');
  });
  it('does not open the edit of my last message on ArrowUp while it cannot send, but does when it can', async () => {
    const mine = [ownRow(O1, '3', 'my words')];
    const blocked = composer({ bits: PERM.viewChannel, items: mine });
    blocked.view.container.querySelector('textarea')?.focus();
    await blocked.user.keyboard('{ArrowUp}');
    expect(blocked.dispatched.filter(a => a.type === 'edit')).toEqual([]);
    blocked.view.unmount();
    const open = composer({ bits: SEND, items: mine });
    open.view.container.querySelector('textarea')?.focus();
    await open.user.keyboard('{ArrowUp}');
    expect(open.dispatched.filter(a => a.type === 'edit')).toHaveLength(1);
  });
  it('keeps the composer with send, and with unknown bits', () => {
    composer({ bits: SEND });
    expect(box()).toBeInTheDocument();
  });
  it('keeps the composer with unknown bits', () => {
    composer({ bits: null });
    expect(box()).toBeInTheDocument();
  });
});

function log(over: { bits?: number | null; items?: ReturnType<typeof peerRow>[]; pinsOpen?: boolean; pins?: PinnedItem[]; editing?: ConversationUi['editing']; threads?: boolean } = {}) {
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set(`pins:${GEN}`, over.pins ?? []);
  const book = nameBook({ id: ME.id, username: 'ada' }, MEMBERS, ROLES);
  const items = over.items ?? [peerRow(P1, '1', 'theirs')];
  const view = render(<div className="d-root"><CoreProvider client={fake}>
    <Conversation channelId={GEN} logKey="k" label="log" timeline={timeline(items)} book={book} encodeWith={[]} broadcast bits={over.bits ?? null} roles={ROLES}
      pinsTitle="Pinned in #general" ui={{ ...INITIAL_UI, pinsOpen: over.pinsOpen ?? false, editing: over.editing ?? null }} dispatch={vi.fn()} loadingEarlier={false}
      onLoadEarlier={vi.fn()} onError={vi.fn()} {...(over.threads === true ? { onOpenThread: vi.fn() } : {})} />
  </CoreProvider></div>);
  return { fake, view };
}
const row = (id: string) => document.querySelector<HTMLElement>(`.d-message-row[data-msg-id="${id}"]`) as HTMLElement;
const actions = (id: string) => toolbarLabels(within(row(id)).getByRole('toolbar', { name: 'actions for this message' }));

describe('files dropped on the conversation', () => {
  const drop = () => {
    const zone = document.querySelector<HTMLElement>('.dw-conversation');
    if (zone === null) throw new Error('no .dw-conversation');
    fireEvent.drop(zone, { dataTransfer: { types: ['Files'], files: [new File(['a'], 'a.txt', { type: 'text/plain' })] } });
  };
  it('are attached with the right to send', async () => {
    const { fake } = log({ bits: SEND });
    drop();
    await waitFor(() => expect(fake.callsOf('attachFiles')).toHaveLength(1));
  });
  it('are not attached while sending is not allowed', async () => {
    const { fake } = log({ bits: PERM.viewChannel });
    drop();
    await new Promise(r => setTimeout(r, 20));
    expect(fake.callsOf('attachFiles')).toEqual([]);
  });
});

describe('reply items by right (walk 11)', () => {
  it('offers reply and reply in thread with the right to send', () => {
    log({ bits: SEND, threads: true });
    expect(actions(P1)).toEqual(['react', 'reply', 'reply in thread']);
  });
  it('keeps only the reaction for a reader who cannot send', () => {
    log({ bits: PERM.viewChannel, threads: true });
    expect(actions(P1)).toEqual(['react']);
  });
});

describe('pins by right', () => {
  it('hides the pin item without the pin right, and shows it with the right or with unknown bits', () => {
    log({ bits: SEND });
    expect(actions(P1)).toEqual(['react', 'reply']);
  });
  it('shows the pin item with the pin right', () => {
    log({ bits: SEND | PERM.pinMessages });
    expect(actions(P1)).toEqual(['react', 'reply', 'pin']);
  });
  it('shows the pin item while the bits are unknown', () => {
    log({ bits: null });
    expect(actions(P1)).toEqual(['react', 'reply', 'pin']);
  });
  it('hides the unpin item too without the right', () => {
    log({ bits: SEND, items: [peerRow(P1, '1', 'theirs', { pinned: true })] });
    expect(actions(P1)).toEqual(['react', 'reply']);
  });
  it('shows perm.pinsNote in the pins dialog', () => {
    log({ bits: SEND, pinsOpen: true });
    expect(within(screen.getByRole('dialog', { name: 'Pinned in #general' })).getByText('A pin shows while the person who pinned it may pin in this channel.')).toBeInTheDocument();
  });
});

describe('mention tint in the log', () => {
  // Review 10 (widened): a broadcast or a role token tints as "you" only when the core's mention flag is set for the row.
  const pill = (id: string, text: string) => Array.from(row(id).querySelectorAll<HTMLElement>('.d-mention')).find(p => p.textContent?.includes(text));
  it('a broadcast that did not count is a plain broadcast pill, not one naming me', () => {
    log({ items: [peerRow(P1, '1', 'hey <@everyone> look', { mention: false })] });
    expect(pill(P1, '@everyone')).toHaveAttribute('data-broadcast', 'true');
    expect(pill(P1, '@everyone')).not.toHaveAttribute('data-me');
  });
  it('a broadcast that counted tints as me', () => {
    log({ items: [peerRow(P1, '1', 'hey <@everyone> look', { mention: true })] });
    expect(pill(P1, '@everyone')).toHaveAttribute('data-me', 'true');
  });
  it('a token of my role that did not count is a plain role pill; one that counted tints as me', () => {
    const first = log({ items: [peerRow(P1, '1', `ping <@${ROLE}>`, { mention: false })] });
    expect(pill(P1, '@')).not.toHaveAttribute('data-me');
    first.view.unmount();
    log({ items: [peerRow(P1, '1', `ping <@${ROLE}>`, { mention: true })] });
    expect(pill(P1, '@')).toHaveAttribute('data-me', 'true');
  });
});

describe('role labels in the log', () => {
  it('renders a known role with its name and colour dot, and an unknown one generically', () => {
    log({ items: [peerRow(P1, '1', `see <@${GAME}> and <@${OTHER_ROLE}> from <@${PEER}>`)] });
    const mention = within(row(P1)).getByText('@Game Night');
    expect(mention.querySelector('.d-role-dot')?.getAttribute('style')).toContain('#336699');
    expect(within(row(P1)).getByText('@role').querySelector('.d-role-dot')).toBeNull();
  });
});

describe('unpin in the pins dialog', () => {
  const pin: PinnedItem = { msgId: P1, seq: '1', senderUser: PEER, excerpt: 'pin me', pinnedBy: ME.id, pinnedSeq: '9', ts: 1700000000, threadRoot: null };
  const unpin = () => within(screen.getByRole('dialog', { name: 'Pinned in #general' })).queryByRole('button', { name: 'unpin' });
  it('hides unpin without the pin right, and keeps jump', () => {
    log({ bits: SEND, pinsOpen: true, pins: [pin] });
    expect(unpin()).toBeNull();
    expect(within(screen.getByRole('dialog', { name: 'Pinned in #general' })).getByRole('button', { name: 'go to message' })).toBeInTheDocument();
  });
  it('shows unpin with the right, and while the bits are unknown', () => {
    log({ bits: SEND | PERM.pinMessages, pinsOpen: true, pins: [pin] });
    expect(unpin()).not.toBeNull();
  });
  it('shows unpin while the bits are unknown', () => {
    log({ bits: null, pinsOpen: true, pins: [pin] });
    expect(unpin()).not.toBeNull();
  });
});

describe('editing a message that holds a role mention', () => {
  it('keeps the token of a role the person could not newly mention', async () => {
    const user = userEvent.setup();
    const { fake } = log({ bits: SEND, items: [ownRow(O1, '2', `hi <@${MODS}>`)], editing: { msgId: O1, value: 'hi @modérateurs ok' } });
    await user.click(screen.getByRole('button', { name: 'save' }));
    expect(fake.callsOf('editMessage')).toEqual([{ m: 'editMessage', channelId: GEN, msgId: O1, text: `hi <@${MODS}> ok` }]);
  });
  it('does not encode a role handle that was not in the message and may not be mentioned', async () => {
    const user = userEvent.setup();
    const { fake } = log({ bits: SEND, items: [ownRow(O1, '2', 'hi')], editing: { msgId: O1, value: 'hi @modérateurs' } });
    await user.click(screen.getByRole('button', { name: 'save' }));
    expect(fake.callsOf('editMessage')[0]?.text).toBe('hi @modérateurs');
  });
});
