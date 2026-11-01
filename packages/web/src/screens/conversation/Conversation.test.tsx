import { afterEach, beforeEach, describe, it, expect, vi } from 'vitest';
import { act, waitFor, fireEvent, screen, within } from '@testing-library/react';
import { en } from '../../strings/en.ts';
import { formatDay, formatTime } from '../../strings/index.ts';
import { REACTION_EMOJI } from '../../emoji.ts';
import { ME } from '../../test/fixtures.ts';
import { PHONE_QUERY } from '../../layout.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import {
  A, GEN, MEMBERS, MOD, O1, O2, P1, P2, PEER, ROLE, file, image, ownRow, peerRow, renderConversation, reply, rowOf,
} from '../../test/conversation.tsx';
import { INITIAL_UI, conversationReducer } from './Conversation.tsx';
import { queryToolbarButton, toolbarButton, toolbarLabels } from '../../test/toolbar.ts';

const composer = () => screen.getByRole('textbox', { name: 'message #general' });
const toolbarOf = (row: HTMLElement) => within(row).getByRole('toolbar', { name: 'actions for this message' });
const tabbable = (row: HTMLElement) => Array.from(row.querySelectorAll<HTMLElement>('button, textarea, a[href]')).filter(e => e.tabIndex >= 0);
/** The reply line's jump button: named by its visible text (L-UI-40, ruling 39), so it is found by its place. */
const replyButton = (row: HTMLElement) => within(row.querySelector('.d-message-row__reply') as HTMLElement).getByRole('button');

describe('conversationReducer', () => {
  it('restores an unsent reply and edit when a tab updates', () => {
    const saved = { ...INITIAL_UI, reply: { msgId: P1, author: 'Ada', excerpt: 'hello' }, editing: { msgId: O1, value: 'changed' } };
    expect(conversationReducer(INITIAL_UI, { type: 'restore', ui: saved })).toEqual(saved);
  });
  it('keeps a reply and an edit apart, counts jumps, and a jump closes the pins', () => {
    const r = { msgId: P1, author: 'bob', excerpt: 'x' };
    let s = conversationReducer(INITIAL_UI, { type: 'reply', reply: r });
    s = conversationReducer(s, { type: 'edit', msgId: O1, value: 'v' });
    expect(s.reply).toEqual(r);
    expect(s.editing).toEqual({ msgId: O1, value: 'v' });
    expect(conversationReducer(s, { type: 'editChange', value: 'w' }).editing).toEqual({ msgId: O1, value: 'w' });
    expect(conversationReducer(INITIAL_UI, { type: 'editChange', value: 'w' })).toBe(INITIAL_UI);
    expect(conversationReducer(s, { type: 'editDone' }).editing).toBeNull();
    expect(conversationReducer(s, { type: 'replySent', msgId: P2 }).reply).toEqual(r);
    expect(conversationReducer(s, { type: 'replySent', msgId: P1 }).reply).toBeNull();
    s = conversationReducer(s, { type: 'pins', open: true });
    expect(s.pinsOpen).toBe(true);
    s = conversationReducer(s, { type: 'jump', msgId: P1 });
    expect(s.pinsOpen).toBe(false);
    expect(s.jump).toEqual({ msgId: P1, n: 1 });
    expect(conversationReducer(s, { type: 'jump', msgId: P1 }).jump).toEqual({ msgId: P1, n: 2 });
    expect(conversationReducer(s, { type: 'trayError', text: 'no' }).trayError).toBe('no');
    expect(conversationReducer(s, { type: 'notice', notice: 'shell.message.replyNotLoaded' }).notice).toBe('shell.message.replyNotLoaded');
    expect(conversationReducer(s, { type: 'reset' })).toEqual(INITIAL_UI);
    expect(INITIAL_UI).toEqual({ reply: null, editing: null, pinsOpen: false, jump: null, notice: null, trayError: null });
  });
});

describe('the roving log (keyboard map, ruling 21)', () => {
  it('moves the active row with the arrows and Home/End, and only the active row’s controls are tabbable', async () => {
    const { user } = renderConversation({ items: [peerRow(P1, '1', 'first'), peerRow(P2, '2', 'second'), ownRow(O1, '3', 'third')] });
    expect([rowOf(P1), rowOf(P2), rowOf(O1)].map(r => r.tabIndex)).toEqual([-1, -1, 0]);
    expect(tabbable(rowOf(P1))).toEqual([]);
    expect(tabbable(rowOf(O1))).toHaveLength(1);
    act(() => rowOf(O1).focus());
    await user.keyboard('{ArrowUp}');
    expect(rowOf(P2)).toHaveFocus();
    expect([rowOf(P1), rowOf(P2), rowOf(O1)].map(r => r.tabIndex)).toEqual([-1, 0, -1]);
    expect(tabbable(rowOf(O1))).toEqual([]);
    expect(tabbable(rowOf(P2))).toHaveLength(1);
    await user.keyboard('{Home}');
    expect(rowOf(P1)).toHaveFocus();
    await user.keyboard('{ArrowUp}');
    expect(rowOf(P1)).toHaveFocus();
    await user.keyboard('{End}');
    expect(rowOf(O1)).toHaveFocus();
    await user.keyboard('{ArrowDown}');
    expect(rowOf(O1)).toHaveFocus();
  });
  it('hands the arrows from the log container to its active row', async () => {
    const { user } = renderConversation({ items: [peerRow(P1, '1', 'first'), peerRow(P2, '2', 'second')] });
    act(() => screen.getByRole('log').focus());
    await user.keyboard('{ArrowDown}');
    expect(rowOf(P2)).toHaveFocus();
    act(() => screen.getByRole('log').focus());
    await user.keyboard('{Home}');
    expect(rowOf(P1)).toHaveFocus();
  });
  it('makes a row active when focus enters it by pointer', async () => {
    const { user } = renderConversation({ items: [peerRow(P1, '1', 'first'), peerRow(P2, '2', 'second')] });
    await user.click(toolbarButton(toolbarOf(rowOf(P1)), 'pin'));
    expect(rowOf(P1).tabIndex).toBe(0);
    expect(rowOf(P2).tabIndex).toBe(-1);
  });
  it('Escape in a row control returns to the row, and on the row to the composer', async () => {
    const { user } = renderConversation({ items: [ownRow(O1, '1', 'mine')] });
    act(() => toolbarButton(toolbarOf(rowOf(O1)), 'react').focus());
    await user.keyboard('{Escape}');
    expect(rowOf(O1)).toHaveFocus();
    await user.keyboard('{Escape}');
    expect(composer()).toHaveFocus();
  });
  it('on a phone Escape on the row puts no script focus in the composer', async () => {
    window.matchMedia = vi.fn((query: string) => ({
      matches: query === PHONE_QUERY, media: query, addEventListener: () => undefined, removeEventListener: () => undefined,
    })) as unknown as typeof window.matchMedia;
    try {
      const { user } = renderConversation({ items: [ownRow(O1, '1', 'mine')] });
      act(() => rowOf(O1).focus());
      await user.keyboard('{Escape}');
      expect(composer()).not.toHaveFocus();
    } finally {
      Reflect.deleteProperty(window, 'matchMedia');
    }
  });
});

// Fix round 2 (Jonas, the prototype's Keyboard): Ctrl+End jumps to the present from anywhere in the tab.
describe('jump to present (Ctrl+End)', () => {
  const scrollAway = (log: HTMLElement) => {
    Object.defineProperty(log, 'scrollHeight', { configurable: true, value: 2000 });
    Object.defineProperty(log, 'clientHeight', { configurable: true, value: 400 });
    log.scrollTop = 100;
    fireEvent.scroll(log);
  };
  const ctrlEnd = (target: Element) => fireEvent.keyDown(target, { key: 'End', ctrlKey: true });
  it('jumps from outside the log once the reader scrolled away, to the newest row', () => {
    renderConversation({ items: [peerRow(P1, '1', 'first'), ownRow(O1, '2', 'newest')] });
    const log = screen.getByRole('log');
    scrollAway(log);
    expect(ctrlEnd(document.body)).toBe(false);
    expect(log.scrollTop).toBe(2000);
    expect(rowOf(O1)).toHaveFocus();
  });
  it('leaves the chord to a text field (the caret moves to the end there), and does nothing at the present', () => {
    renderConversation({ items: [peerRow(P1, '1', 'first'), ownRow(O1, '2', 'newest')] });
    const log = screen.getByRole('log');
    expect(ctrlEnd(document.body), 'at the present the key keeps its default').toBe(true);
    scrollAway(log);
    expect(ctrlEnd(composer()), 'in the composer').toBe(true);
    expect(log.scrollTop).toBe(100);
    expect(fireEvent.keyDown(document.body, { key: 'End', ctrlKey: true, shiftKey: true }), 'Ctrl+Shift+End is not the chord').toBe(true);
    // Fix round 3: not under a modal layer (an aria-modal overlay such as the search palette).
    const modal = document.createElement('div');
    modal.setAttribute('aria-modal', 'true');
    document.body.append(modal);
    expect(ctrlEnd(document.body), 'under a modal overlay').toBe(true);
    expect(log.scrollTop).toBe(100);
    modal.remove();
  });
});

describe('the toolbar (L-UI-41)', () => {
  it('offers react, reply and pin on others’ rows, edit and delete on own rows, and nothing on rows that are not ok', () => {
    renderConversation({ items: [
      peerRow(P1, '1', 'theirs'),
      ownRow(O1, '2', 'mine', { pinned: true }),
      ownRow(O2, null, 'sending', { state: 'pending' }),
      peerRow(P2, '4', '', { state: 'deleted' }),
    ] });
    const labels = (row: HTMLElement) => toolbarLabels(toolbarOf(row));
    expect(labels(rowOf(P1))).toEqual(['react', 'reply', 'reply in thread', 'pin']);
    expect(labels(rowOf(O1))).toEqual(['react', 'reply', 'reply in thread', 'edit', 'unpin', 'delete']);
    expect(within(rowOf(O2)).queryByRole('toolbar')).toBeNull();
    expect(within(rowOf(P2)).queryByRole('toolbar')).toBeNull();
    // The rows' toolbars only: the user panel at the bottom left (web3.8) is a toolbar of its own.
    expect(screen.getAllByRole('toolbar', { name: en['shell.message.toolbar'] })).toHaveLength(2);
  });
  it('pins and unpins by the row’s pinned flag', async () => {
    const { fake, user } = renderConversation({ items: [peerRow(P1, '1', 'theirs'), ownRow(O1, '2', 'mine', { pinned: true })] });
    expect(rowOf(O1)).toHaveAttribute('data-pinned', 'true');
    expect(rowOf(O1)).toHaveTextContent('pinned');
    await user.click(toolbarButton(toolbarOf(rowOf(P1)), 'pin'));
    await user.click(toolbarButton(toolbarOf(rowOf(O1)), 'unpin'));
    expect(fake.callsOf('pin')).toEqual([
      { m: 'pin', channelId: GEN, msgId: P1, on: true }, { m: 'pin', channelId: GEN, msgId: O1, on: false },
    ]);
  });
  it('offers edit and delete to the author user on another device even when own is false (F3)', () => {
    renderConversation({ items: [ownRow(O1, '1', 'other device', { own: false })] });
    expect(toolbarButton(toolbarOf(rowOf(O1)), 'edit')).toBeInTheDocument();
    expect(toolbarButton(toolbarOf(rowOf(O1)), 'delete')).toBeInTheDocument();
    expect(rowOf(O1)).not.toHaveAttribute('data-own');
  });
});

describe('moderator message removal', () => {
  it('shows a separate control only with loaded ManageMessages authority and confirms with the target sequence', async () => {
    const { fake, user } = renderConversation({ items: [peerRow(P1, '7', 'theirs')] });
    fake.handler = command => command.m === 'moderateRemoveMessage' ? Promise.resolve({ removal: 'unknown',
      proofRequestId: '11'.repeat(16), evidenceRequestId: '22'.repeat(16), evidenceUpload: 'unknown' })
      : command.m === 'resolveManualRemoval' ? Promise.resolve({ removal: 'accepted', evidenceUpload: 'accepted' }) : Promise.resolve(null);
    expect(queryToolbarButton(toolbarOf(rowOf(P1)), 'remove for everyone')).toBeNull();
    const everyone = '01'.repeat(16);
    const access = { top: 0, owner: false, manageCommunity: false, manageChannels: false, kickMembers: false, banMembers: false,
      moderateMembers: false, createInvite: false, manageRoles: false, loaded: true, everyoneId: everyone, overwritesLoaded: true };
    fake.set(`permissions:${A}`, access);
    fake.set(`roles:${A}`, [
      { id: everyone, name: '@everyone', color: 0, position: 0, hoist: false, mentionable: false, allow: 1, deny: 0, members: 3 },
      { id: ROLE, name: 'Moderators', color: 0, position: 1, hoist: false, mentionable: false, allow: 1, deny: 0, members: 1 },
    ]);
    fake.set(`overwrites:${A}`, {});
    expect(queryToolbarButton(toolbarOf(rowOf(P1)), 'remove for everyone')).toBeNull();
    fake.set(`roles:${A}`, [
      { id: everyone, name: '@everyone', color: 0, position: 0, hoist: false, mentionable: false, allow: 1, deny: 0, members: 3 },
      { id: ROLE, name: 'Moderators', color: 0, position: 1, hoist: false, mentionable: false, allow: 5, deny: 0, members: 1 },
    ]);
    const action = await waitFor(() => toolbarButton(toolbarOf(rowOf(P1)), 'remove for everyone'));
    await user.click(action);
    const dialog = screen.getByRole('dialog', { name: 'Remove this message for everyone?' });
    expect(fake.callsOf('moderateRemoveMessage')).toEqual([]);
    const reason = within(dialog).getByLabelText(en['shell.moderation.reason']);
    expect(within(dialog).getByRole('button', { name: 'Remove' })).toHaveAttribute('aria-disabled', 'true');
    await user.type(reason, 'reviewed by moderator');
    expect(within(dialog).getByRole('button', { name: 'Remove' })).not.toHaveAttribute('aria-disabled', 'true');
    await user.click(within(dialog).getByRole('button', { name: 'Remove' }));
    expect(fake.callsOf('moderateRemoveMessage')).toEqual([{ m: 'moderateRemoveMessage', channelId: GEN, msgId: P1, seq: '7',
      reason: 'reviewed by moderator', excerpt: 'theirs' }]);
    expect(await screen.findByText(en['shell.moderation.removalUnknown'])).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: en['shell.moderation.retryRemoval'] }));
    expect(await screen.findByText(en['shell.moderation.evidenceSent'])).toBeInTheDocument();
    expect(fake.callsOf('resolveManualRemoval')).toEqual([{ m: 'resolveManualRemoval', proofRequestId: '11'.repeat(16) }]);
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: en['shell.moderation.done'] }));
    expect(screen.queryByRole('dialog')).toBeNull();
  });
});

describe('delete (Q19, ruling 24)', () => {
  it('deletes only after the dialog is confirmed, then hands focus to the row', async () => {
    const { fake, user } = renderConversation({ items: [ownRow(O1, '1', 'mine')] });
    await user.click(toolbarButton(toolbarOf(rowOf(O1)), 'delete'));
    const dialog = screen.getByRole('dialog', { name: 'Delete this message?' });
    expect(dialog).toHaveTextContent('It is removed for everyone in this conversation, with its reactions and files. Copies someone already saved stay with them. This cannot be undone.');
    expect(fake.callsOf('deleteMessage')).toEqual([]);
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(fake.callsOf('deleteMessage')).toEqual([]);
    await user.click(toolbarButton(toolbarOf(rowOf(O1)), 'delete'));
    await user.click(within(screen.getByRole('dialog', { name: 'Delete this message?' })).getByRole('button', { name: 'Delete' }));
    expect(fake.callsOf('deleteMessage')).toEqual([{ m: 'deleteMessage', channelId: GEN, msgId: O1, seq: '1' }]);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(rowOf(O1)).toHaveFocus();
  });
});

describe('edit in place (L-UI-44)', () => {
  it('saves the text with its mentions encoded, cancels on Escape, and sends nothing for an unchanged or empty text', async () => {
    const { fake, user } = renderConversation({ items: [ownRow(O1, '1', `hi <@${PEER}>`)] });
    const edit = () => user.click(toolbarButton(toolbarOf(rowOf(O1)), 'edit'));
    await edit();
    const editor = screen.getByRole('textbox', { name: 'edit your message' });
    expect(editor).toHaveFocus();
    expect(editor).toHaveValue('hi @bob');
    expect(rowOf(O1)).toContainElement(editor);
    await user.keyboard('{Enter}');
    expect(screen.queryByRole('textbox', { name: 'edit your message' })).toBeNull();
    expect(rowOf(O1)).toHaveFocus();
    await edit();
    await user.keyboard(' and @mo{Escape}');
    expect(screen.queryByRole('textbox', { name: 'edit your message' })).toBeNull();
    expect(rowOf(O1)).toHaveFocus();
    await edit();
    await user.clear(screen.getByRole('textbox', { name: 'edit your message' }));
    await user.keyboard('{Enter}');
    expect(screen.getByRole('textbox', { name: 'edit your message' })).toBeInTheDocument();
    expect(fake.callsOf('editMessage')).toEqual([]);
    await user.keyboard('{Escape}');
    await edit();
    await user.keyboard(' and @mo{Enter}');
    expect(fake.callsOf('editMessage')).toEqual([{ m: 'editMessage', channelId: GEN, msgId: O1, text: `hi <@${PEER}> and <@${MOD}>` }]);
    expect(rowOf(O1)).toHaveFocus();
  });
  it('marks an edited row, and shows the edit only as the core folded it', () => {
    renderConversation({ items: [ownRow(O1, '1', 'folded text', { edited: true })] });
    expect(rowOf(O1)).toHaveAttribute('data-edited', 'true');
    expect(rowOf(O1)).toHaveTextContent('edited');
    expect(rowOf(O1)).toHaveTextContent('folded text');
  });
});

describe('reactions (Q18, ruling 28)', () => {
  it('toggles a chip by whether it is mine, and adds from the grid of 32 with focus back on the opener', async () => {
    const { fake, user } = renderConversation({ items: [peerRow(P1, '1', 'theirs', { reactions: [
      { emoji: '\u{1F44D}', count: 2, mine: true }, { emoji: '\u{1F602}', count: 1, mine: false }, { emoji: '\u{1F984}', count: 1, mine: false },
    ] })] });
    const bar = within(rowOf(P1)).getByRole('group', { name: 'reactions' });
    expect(within(bar).getByRole('button', { name: 'thumbs up, 2' })).toHaveAttribute('aria-pressed', 'true');
    expect(within(bar).getByRole('button', { name: 'tears of joy, 1' })).toHaveAttribute('aria-pressed', 'false');
    expect(within(bar).getByRole('button', { name: '\u{1F984}, 1' })).toBeInTheDocument();
    await user.click(within(bar).getByRole('button', { name: 'thumbs up, 2' }));
    await user.click(within(bar).getByRole('button', { name: 'tears of joy, 1' }));
    const react = toolbarButton(toolbarOf(rowOf(P1)), 'react');
    await user.click(react);
    const grid = screen.getByRole('dialog', { name: 'pick a reaction' });
    expect(within(grid).getAllByRole('button').map(b => b.getAttribute('aria-label'))).toEqual(REACTION_EMOJI.map(e => en[e.key]));
    await user.click(within(grid).getByRole('button', { name: 'rocket' }));
    expect(screen.queryByRole('dialog', { name: 'pick a reaction' })).toBeNull();
    expect(react).toHaveFocus();
    const add = within(bar).getByRole('button', { name: 'add a reaction' });
    await user.click(add);
    // The grid lives in the conversation's overlay layer, never inside the log (SLICE-UX-08, ruling 39).
    const fromAdd = screen.getByRole('dialog', { name: 'pick a reaction' });
    expect(screen.getByRole('log').contains(fromAdd)).toBe(false);
    expect(fromAdd.closest('.dw-overlay')).not.toBeNull();
    await user.click(within(fromAdd).getByRole('button', { name: 'thumbs up' }));
    await user.click(add);
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog', { name: 'pick a reaction' })).toBeNull();
    expect(add).toHaveFocus();
    expect(fake.callsOf('react')).toEqual([
      { m: 'react', channelId: GEN, msgId: P1, emoji: '\u{1F44D}', on: false },
      { m: 'react', channelId: GEN, msgId: P1, emoji: '\u{1F602}', on: true },
      { m: 'react', channelId: GEN, msgId: P1, emoji: '\u{1F680}', on: true },
      { m: 'react', channelId: GEN, msgId: P1, emoji: '\u{1F44D}', on: false },
    ]);
  });
  it('draws no reaction bar on a row without reactions', () => {
    renderConversation({ items: [peerRow(P1, '1', 'quiet')] });
    expect(within(rowOf(P1)).queryByRole('group', { name: 'reactions' })).toBeNull();
  });
});

describe('the reply line and the jump (brief:186)', () => {
  let scroll: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    scroll = vi.fn();
    Object.defineProperty(Element.prototype, 'scrollIntoView', { configurable: true, writable: true, value: scroll });
  });
  afterEach(() => {
    vi.useRealTimers();
    delete (Element.prototype as unknown as Record<string, unknown>).scrollIntoView;
    delete (window as unknown as Record<string, unknown>).matchMedia;
  });
  it('keeps a pointer press on the reply jump from activating and moving the log before release', () => {
    renderConversation({ items: [peerRow(P1, '1', 'the original'), ownRow(O1, '2', 'my answer', { reply: reply() })] });
    const jump = replyButton(rowOf(O1));
    composer().focus();
    const down = new Event('pointerdown', { bubbles: true, cancelable: true });
    jump.dispatchEvent(down);
    expect(down.defaultPrevented).toBe(true);
    expect(composer()).toHaveFocus();
    fireEvent.click(jump);
    expect(rowOf(P1)).toHaveFocus();
  });
  it('jumps to a loaded original, focuses it and flashes it for 1.4 s', () => {
    vi.useFakeTimers();
    renderConversation({ items: [peerRow(P1, '1', 'the original'), ownRow(O1, '2', 'my answer', { reply: reply() })] });
    const jump = replyButton(rowOf(O1));
    expect(jump).toHaveTextContent('bob');
    expect(jump).toHaveTextContent('the original');
    expect(jump).toHaveAccessibleName('bob the original');
    expect(jump).toHaveAccessibleDescription('reply to bob go to the original message');
    fireEvent.click(jump);
    expect(rowOf(P1)).toHaveFocus();
    expect(rowOf(P1)).toHaveAttribute('data-flash', 'true');
    expect(scroll).toHaveBeenCalledWith({ block: 'center', behavior: 'smooth' });
    act(() => { vi.advanceTimersByTime(1399); });
    expect(rowOf(P1)).toHaveAttribute('data-flash', 'true');
    act(() => { vi.advanceTimersByTime(1); });
    expect(rowOf(P1)).not.toHaveAttribute('data-flash');
  });
  it('does not animate the scroll under reduced motion', () => {
    Object.defineProperty(window, 'matchMedia', { configurable: true, writable: true,
      value: (q: string) => ({ matches: q === '(prefers-reduced-motion: reduce)', media: q, onchange: null,
        addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
    renderConversation({ items: [peerRow(P1, '1', 'the original'), ownRow(O1, '2', 'my answer', { reply: reply() })] });
    fireEvent.click(replyButton(rowOf(O1)));
    expect(scroll).toHaveBeenCalledWith({ block: 'center', behavior: 'auto' });
    expect(rowOf(P1)).toHaveAttribute('data-flash', 'true');
  });
  it('says the original is further back when it is not loaded, and the notice can be dismissed', async () => {
    const { user } = renderConversation({ items: [ownRow(O1, '5', 'answer', { reply: reply() })] });
    await user.click(replyButton(rowOf(O1)));
    expect(screen.getByText('The original message is further back. Load earlier messages to reach it.')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'dismiss' }));
    expect(screen.queryByText('The original message is further back. Load earlier messages to reach it.')).toBeNull();
  });
  it('says when the original cannot be shown or was deleted', () => {
    renderConversation({ items: [
      ownRow(O1, '5', 'a', { reply: reply({ state: 'missing', senderUser: null, excerpt: '', seq: null }) }),
      ownRow(O2, '6', 'b', { reply: reply({ state: 'deleted', excerpt: '' }) }),
    ] });
    expect(rowOf(O1)).toHaveTextContent('the original message cannot be shown here');
    expect(rowOf(O1).querySelector('.d-message-row__reply button')).toBeNull();
    expect(rowOf(O2)).toHaveTextContent('the original message was deleted');
    expect(replyButton(rowOf(O2))).toHaveAccessibleName('bob the original message was deleted');
  });
});

describe('bodies and pending actions', () => {
  it('renders mentions as names, marks the viewer’s, and never shows a raw token', () => {
    renderConversation({ items: [peerRow(P1, '1', `<@${ME.id}> and <@${MOD}>, <@everyone>, <@${ROLE}>`, { mention: true })] });
    const row = rowOf(P1);
    expect(row).toHaveAttribute('data-mention', 'true');
    expect(Array.from(row.querySelectorAll('.d-mention')).map(p => [p.textContent, p.hasAttribute('data-me'), p.hasAttribute('data-broadcast')])).toEqual([
      ['@Ada L', true, false], ['@Mo', false, false], ['@everyone', true, true], ['@role', true, false],
    ]);
    expect(row.textContent).not.toContain('<@');
  });
  it('shows a pending action as saving… and a failed one with retry and discard', async () => {
    const E1 = 'e1'.repeat(16);
    const E2 = 'e2'.repeat(16);
    const { fake, user } = renderConversation({ items: [
      peerRow(P1, '1', 'a', { actions: [{ msgId: E1, type: 3, state: 'pending', reason: '' }] }),
      ownRow(O1, '2', 'b', { actions: [{ msgId: E1, type: 1, state: 'pending', reason: '' }, { msgId: E2, type: 1, state: 'failed', reason: 'E_NETWORK' }] }),
    ] });
    expect(rowOf(P1)).toHaveTextContent('saving…');
    expect(rowOf(O1)).toHaveTextContent('not saved');
    expect(rowOf(O1)).not.toHaveTextContent('saving…');
    await user.click(within(rowOf(O1)).getByRole('button', { name: 'retry' }));
    expect(rowOf(O1)).toHaveFocus();
    await user.click(within(rowOf(O1)).getByRole('button', { name: 'discard' }));
    expect(fake.callsOf('retrySend')).toEqual([{ m: 'retrySend', msgId: E2 }]);
    expect(fake.callsOf('discardSend')).toEqual([{ m: 'discardSend', msgId: E2 }]);
  });
  it('offers only discard for a sent message whose file is no longer on the server', async () => {
    const { fake, user } = renderConversation({ items: [
      ownRow(O1, null, 'with a file', { state: 'failed', reason: 'E_ATTACHMENT_MISSING', attachments: [file()] }),
    ] });
    const row = rowOf(O1);
    expect(row).toHaveTextContent('A file of this message is no longer on the server. Discard it and attach the file again.');
    expect(within(row).queryByRole('button', { name: 'retry' })).toBeNull();
    expect(within(row).getAllByRole('button', { name: 'discard' })).toHaveLength(1);
    await user.click(within(row).getByRole('button', { name: 'discard' }));
    expect(fake.callsOf('discardSend')).toEqual([{ m: 'discardSend', msgId: O1 }]);
    expect(fake.callsOf('retrySend')).toEqual([]);
  });
  it('passes axe with every decoration on one row', async () => {
    const { view } = renderConversation({ items: [
      peerRow(P1, '1', 'the original'),
      ownRow(O1, '2', `<@${PEER}> look`, { edited: true, pinned: true, reply: reply(), mention: false,
        reactions: [{ emoji: '\u{1F44D}', count: 3, mine: true }], attachments: [image({ thumb: false }), file({ index: 1 })],
        actions: [{ msgId: 'e3'.repeat(16), type: 3, state: 'pending', reason: '' }] }),
    ] });
    await expectNoAxeViolations(view.container);
  });
});

describe('focus handoffs (F4)', () => {
  it('returns to the row when unreacting removes its last chip', async () => {
    const { fake, user } = renderConversation({ items: [peerRow(P1, '1', 'x', { reactions: [{ emoji: '👍', count: 1, mine: true }] })] });
    await user.click(within(rowOf(P1)).getByRole('button', { name: 'thumbs up, 1' }));
    act(() => fake.set(`timeline:${GEN}`, { channelId: GEN, group: 'active', hasEarlier: false, items: [peerRow(P1, '1', 'x')] }));
    expect(rowOf(P1)).toHaveFocus();
  });
  it('returns to the row when a focused chip disappears but another reaction remains', async () => {
    const reactions = [{ emoji: '👍', count: 1, mine: true }, { emoji: '🦀', count: 2, mine: false }];
    const { fake, user } = renderConversation({ items: [peerRow(P1, '1', 'x', { reactions })] });
    await user.click(within(rowOf(P1)).getByRole('button', { name: 'thumbs up, 1' }));
    act(() => fake.set(`timeline:${GEN}`, { channelId: GEN, group: 'active', hasEarlier: false,
      items: [peerRow(P1, '1', 'x', { reactions: [reactions[1]] })] }));
    expect(within(rowOf(P1)).getByRole('button', { name: 'crab, 2' })).toBeInTheDocument();
    expect(rowOf(P1)).toHaveFocus();
  });
  it('dismisses the reply not loaded banner to the active log row', async () => {
    const { user } = renderConversation({ items: [ownRow(O1, '5', 'answer', { reply: reply() })] });
    await user.click(replyButton(rowOf(O1)));
    await user.click(screen.getByRole('button', { name: 'dismiss' }));
    expect(rowOf(O1)).toHaveFocus();
  });
  it('hands attachment gone discard to the next failed row’s discard', async () => {
    const { user } = renderConversation({ items: [
      ownRow(O1, null, 'gone', { state: 'failed', reason: 'E_ATTACHMENT_MISSING', attachments: [file()] }),
      ownRow(O2, null, 'failed', { state: 'failed', reason: 'E_NETWORK' }),
    ] });
    await user.click(within(rowOf(O1)).getByRole('button', { name: 'discard' }));
    expect(within(rowOf(O2)).getByRole('button', { name: 'discard' })).toHaveFocus();
  });
  it('closes the emoji grid when focus leaves it without moving focus', async () => {
    const { user } = renderConversation({ items: [peerRow(P1, '1', 'x')] });
    await user.click(toolbarButton(toolbarOf(rowOf(P1)), 'react'));
    const grid = screen.getByRole('dialog', { name: 'pick a reaction' });
    act(() => composer().focus());
    fireEvent.blur(grid.querySelector('button') as HTMLElement, { relatedTarget: composer() });
    expect(grid).not.toBeInTheDocument();
    expect(composer()).toHaveFocus();
  });
});

describe('grouping, day dividers and the present bar (R29)', () => {
  const MIN = 60;
  const DAY = 24 * 60 * MIN;
  // Noon today, so that a few minutes either way never cross midnight.
  const NOON = new Date().setHours(12, 0, 0, 0) / 1000;
  const authors = () => Array.from(document.querySelectorAll('.d-message-row__author:not(.d-sr-only)')).map(e => e.textContent);
  it('folds three messages of one author within 7 minutes into one author line', () => {
    renderConversation({ items: [
      peerRow(P1, '1', 'one', { ts: NOON }), peerRow(P2, '2', 'two', { ts: NOON + 3 * MIN }), peerRow(O1, '3', 'three', { ts: NOON + 9 * MIN }),
    ] });
    // The gap is measured to the message above, not to the first of the run: 3 min, then 6 min.
    expect(authors()).toEqual(['bob']);
    expect(rowOf(P1)).not.toHaveAttribute('data-grouped');
    expect(rowOf(P2)).toHaveAttribute('data-grouped', 'true');
    expect(rowOf(O1)).toHaveAttribute('data-grouped', 'true');
  });
  it('shows a grouped row’s full time in its gutter, not a shortened one that does not fit', () => {
    renderConversation({ items: [peerRow(P1, '1', 'one', { ts: NOON }), peerRow(P2, '2', 'two', { ts: NOON + 3 * MIN })] });
    const row = rowOf(P2);
    const full = row.querySelector('.d-message-row__head .d-message-row__time')?.textContent ?? '';
    expect(full).not.toBe('');
    expect(row.querySelector('.d-message-row__gutter')).toHaveTextContent(full);
  });
  it('shows only the time of day in the gutter of a grouped row from an earlier day, with the full date as its title', () => {
    renderConversation({ items: [peerRow(P1, '1', 'a', { ts: NOON - DAY }), peerRow(P2, '2', 'b', { ts: NOON - DAY + 60 })] });
    const row = rowOf(P2);
    const gutter = row.querySelector('.d-message-row__gutter') as HTMLElement;
    expect(gutter.textContent).toBe(formatTime(NOON - DAY + 60));
    expect(gutter.textContent).not.toMatch(/\d{4}/);
    expect(gutter.textContent).not.toContain(formatDay(NOON - DAY + 60));
    expect(gutter).toHaveAttribute('aria-hidden', 'true');
    // The head keeps the day for a screen reader, and the title carries it for a pointer.
    const head = row.querySelector('.d-message-row__head .d-message-row__time')?.textContent ?? '';
    expect(head).toContain(formatDay(NOON - DAY + 60));
    expect(gutter).toHaveAttribute('title', head);
  });
  it('leaves a grouped row from today as it was: the time of day, and no extra title text', () => {
    renderConversation({ items: [peerRow(P1, '1', 'one', { ts: NOON }), peerRow(P2, '2', 'two', { ts: NOON + 3 * MIN })] });
    const gutter = rowOf(P2).querySelector('.d-message-row__gutter') as HTMLElement;
    expect(gutter.textContent).toBe(formatTime(NOON + 3 * MIN));
    expect(gutter.getAttribute('title')).toBe(gutter.textContent);
  });
  it('starts a new group after a gap of more than 7 minutes, and at another author', () => {
    renderConversation({ items: [
      peerRow(P1, '1', 'one', { ts: NOON }), peerRow(P2, '2', 'two', { ts: NOON + 8 * MIN }), ownRow(O1, '3', 'three', { ts: NOON + 8 * MIN + 1 }),
    ] });
    expect(authors()).toEqual(['bob', 'bob', 'Ada L']);
    expect(rowOf(P2)).not.toHaveAttribute('data-grouped');
    expect(rowOf(O1)).not.toHaveAttribute('data-grouped');
  });
  it('does not fold a reply, nor a row that cannot be read, into the run above it', () => {
    renderConversation({ items: [
      peerRow(P1, '1', 'one'), peerRow(P2, '2', 'two', { reply: reply() }), peerRow(O1, '3', '', { state: 'cannot-read' }),
    ] });
    expect([P1, P2, O1].map(id => rowOf(id).getAttribute('data-grouped'))).toEqual([null, null, null]);
  });
  it('divides the days: one divider before each day, named by its date, yesterday or today', () => {
    renderConversation({ items: [
      peerRow(P1, '1', 'old', { ts: NOON - 5 * DAY }), peerRow(P2, '2', 'yesterday', { ts: NOON - DAY }), ownRow(O1, '3', 'today', { ts: NOON }),
    ] });
    expect(within(screen.getByRole('log')).getAllByRole('separator').map(s => s.getAttribute('aria-label'))).toEqual([
      formatDay(NOON - 5 * DAY), en['layout.log.yesterday'], en['layout.log.today'],
    ]);
  });
  it('never folds two messages across a day divider', () => {
    renderConversation({ items: [peerRow(P1, '1', 'a', { ts: NOON - DAY }), peerRow(P2, '2', 'b', { ts: NOON - DAY + 60 }), peerRow(O1, '3', 'c', { ts: NOON })] });
    expect(rowOf(P2)).toHaveAttribute('data-grouped', 'true');
    expect(rowOf(O1)).not.toHaveAttribute('data-grouped');
  });
  it('shows the present bar once the log is scrolled away from the newest message, and Ctrl+End goes to it', async () => {
    const { user } = renderConversation({ items: [peerRow(P1, '1', 'one'), peerRow(P2, '2', 'two')] });
    const log = screen.getByRole('log');
    expect(screen.queryByRole('button', { name: en['layout.log.jump'] })).toBeNull();
    Object.defineProperty(log, 'scrollHeight', { configurable: true, value: 2000 });
    Object.defineProperty(log, 'clientHeight', { configurable: true, value: 500 });
    log.scrollTop = 200;
    fireEvent.scroll(log);
    expect(screen.getByRole('button', { name: en['layout.log.jump'] })).toBeInTheDocument();
    expect(screen.getByText(en['layout.log.older'], { selector: 'span' })).toBeInTheDocument();
    act(() => rowOf(P2).focus());
    await user.keyboard('{Control>}{End}{/Control}');
    expect(log.scrollTop).toBe(2000);
    // The scroll event of that move is what a browser sends; at the end the bar goes.
    fireEvent.scroll(log);
    expect(screen.queryByRole('button', { name: en['layout.log.jump'] })).toBeNull();
  });
  it('the jump button scrolls to the newest message too', async () => {
    const { user } = renderConversation({ items: [peerRow(P1, '1', 'one')] });
    const log = screen.getByRole('log');
    Object.defineProperty(log, 'scrollHeight', { configurable: true, value: 1500 });
    Object.defineProperty(log, 'clientHeight', { configurable: true, value: 500 });
    log.scrollTop = 0;
    fireEvent.scroll(log);
    await user.click(screen.getByRole('button', { name: en['layout.log.jump'] }));
    expect(log.scrollTop).toBe(1500);
  });
  it('tags a web client and a bot always', () => {
    const glenn = '9f'.repeat(16);
    renderConversation({
      items: [ownRow(O1, '1', 'sent here'), peerRow(P1, '2', 'sent by a bot', { bot: true, senderUser: glenn, ts: NOON + 3600 })],
      // Task S1: the BOT tag needs the server's member row of account kind 1, not only the credential's kind.
      before: fake => fake.set(`members:${A}`, [...MEMBERS, { userId: glenn, username: 'glenn', display: 'Glenn', kind: 1, roleIds: [] }]),
    });
    expect(rowOf(O1).querySelector('[data-reveal]')).toBeNull();
    expect(rowOf(O1).querySelector('.d-tag')).toHaveAttribute('data-kind', 'web');
    expect(rowOf(P1).querySelector('[data-reveal]')).toBeNull();
    expect(rowOf(P1).querySelector('.d-tag')).toHaveAttribute('data-kind', 'bot');
  });
});

describe('received reactions', () => {
  it('shows sanitized chip text and name while toggling the original reaction', async () => {
    const emoji = '\u202egpj.exe1';
    const { fake, user } = renderConversation({ items: [peerRow(P1, '1', 'x', { reactions: [{ emoji, count: 1, mine: false }] })] });
    const chip = within(rowOf(P1)).getByRole('button', { name: 'gpj.exe1, 1' });
    expect(chip).toHaveTextContent('gpj.exe1');
    expect(chip.textContent).not.toMatch(/[\u0000-\u001f\u007f-\u009f\u202a-\u202e\u2066-\u2069]/u);
    expect(chip.getAttribute('aria-label')).not.toMatch(/[\u0000-\u001f\u007f-\u009f\u202a-\u202e\u2066-\u2069]/u);
    await user.click(chip);
    expect(fake.callsOf('react')).toEqual([{ m: 'react', channelId: GEN, msgId: P1, emoji, on: true }]);
  });
});
