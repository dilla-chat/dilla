// MP7: a user mention and a message's author open the same profile card as the member list.
import { afterEach, describe, expect, it } from 'vitest';
import { act, screen, within } from '@testing-library/react';
import { PHONE_QUERY } from '../../layout.ts';
import { ME } from '../../test/fixtures.ts';
import { A, GEN, MOD, NOW, OTHER_ROLE, P1, P2, PEER, STRANGER, peerRow, renderConversation, rowOf, timeline } from '../../test/conversation.tsx';

afterEach(() => { Reflect.deleteProperty(window, 'matchMedia'); });

const card = () => screen.getByRole('dialog');
const actionNames = () => within(card()).queryAllByRole('button').filter(b => b.matches('.d-profile-card__action, .d-profile-card__icon-action')).map(b => b.getAttribute('aria-label') ?? b.textContent);

describe('a mention and an author open the profile card (MP7)', () => {
  it('opens the card of the mentioned member from a click on the pill', async () => {
    const { user } = renderConversation({ items: [peerRow(P1, '1', `hello <@${PEER}>`)] });
    await user.click(within(rowOf(P1)).getByRole('button', { name: '@bob' }));
    expect(card()).toHaveAccessibleName('bob');
    expect(actionNames()).toContain('Message');
  });
  it('leaves role and @everyone pills plain', () => {
    renderConversation({ items: [peerRow(P1, '1', `<@everyone> <@${OTHER_ROLE}>`, { mention: true })] });
    expect(within(rowOf(P1).querySelector<HTMLElement>('.d-message-row__body')!).queryAllByRole('button')).toEqual([]);
    expect(rowOf(P1).querySelectorAll('span.d-mention')).toHaveLength(2);
  });
  it('keeps an id that is no member, no sender and no known role a plain pill', () => {
    renderConversation({ items: [peerRow(P1, '1', `bye <@${STRANGER}>`)] });
    expect(within(rowOf(P1).querySelector<HTMLElement>('.d-message-row__body')!).queryAllByRole('button')).toEqual([]);
    expect(rowOf(P1).querySelectorAll('span.d-mention')).toHaveLength(1);
  });
  it('L4: a former member whose name we do not know gets a card titled "former member", never the raw id', async () => {
    const { user } = renderConversation({ items: [peerRow(P2, '1', 'i was here', { senderUser: STRANGER }), peerRow(P1, '2', `bye <@${STRANGER}>`)] });
    await user.click(within(rowOf(P1)).getByRole('button', { name: '@eeeeeeee' }));
    expect(card()).toHaveAccessibleName('former member');
    expect(card()).not.toHaveTextContent('eeeeeeee');
    expect(actionNames()).toEqual([]);
    expect(within(card()).queryByRole('list')).toBeNull();
  });
  it('opens on Enter from the focused pill, and Escape closes it and returns focus to the pill', async () => {
    const { user } = renderConversation({ items: [peerRow(P1, '1', `hello <@${PEER}>`)] });
    const pill = within(rowOf(P1)).getByRole('button', { name: '@bob' });
    act(() => pill.focus());
    await user.keyboard('{Enter}');
    expect(card()).toHaveAccessibleName('bob');
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(pill).toHaveFocus();
  });
  it('opens the author\'s card from the author name', async () => {
    const { user } = renderConversation({ items: [peerRow(P1, '1', 'hi')] });
    await user.click(within(rowOf(P1)).getByRole('button', { name: 'bob' }));
    expect(card()).toHaveAccessibleName('bob');
  });
  it('a click on the avatar of the author whose card is open closes it, like the name', async () => {
    const { user } = renderConversation({ items: [peerRow(P1, '1', 'hi')] });
    await user.click(within(rowOf(P1)).getByRole('button', { name: 'bob' }));
    expect(card()).toHaveAccessibleName('bob');
    await user.click(rowOf(P1).querySelector<HTMLElement>('.d-message-row__avatar')!);
    expect(screen.queryByRole('dialog')).toBeNull();
  });
  it('when the anchor disappears the card closes and focus goes to the message row', async () => {
    const { user, fake } = renderConversation({ items: [peerRow(P1, '1', 'hi')] });
    await user.click(within(rowOf(P1)).getByRole('button', { name: 'bob' }));
    expect(card()).toHaveAccessibleName('bob');
    // An earlier message from the same sender makes P1 a grouped row: it has no author button any more.
    act(() => fake.set(`timeline:${GEN}`, timeline([peerRow(P2, '0', 'before', { ts: NOW - 5 }), peerRow(P1, '1', 'hi')])));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(rowOf(P1)).toHaveFocus();
  });
  it('opens one card at a time: a mention replaces a card open from the member list', async () => {
    window.matchMedia = ((query: string) => ({ matches: query !== PHONE_QUERY, media: query, addEventListener: () => undefined, removeEventListener: () => undefined,
      addListener: () => undefined, removeListener: () => undefined, onchange: null, dispatchEvent: () => false }));
    const { user } = renderConversation({ items: [peerRow(P1, '1', `hello <@${PEER}>`)],
      before: fake => fake.set(`presence:${A}`, { [ME.id]: 'online', [PEER]: 'online', [MOD]: 'online' }) });
    const listRow = document.querySelector<HTMLElement>('.d-member-list__open[data-focus-key^="member:"]');
    expect(listRow).not.toBeNull();
    await user.click(listRow!);
    expect(card()).not.toHaveAccessibleName('bob');
    await user.click(within(rowOf(P1)).getByRole('button', { name: '@bob' }));
    expect(screen.getAllByRole('dialog')).toHaveLength(1);
    expect(card()).toHaveAccessibleName('bob');
    // And the other way: a row of the list replaces the mention's card.
    await user.click(document.querySelector<HTMLElement>(`[data-focus-key="member:${MOD}"]`)!);
    expect(screen.getAllByRole('dialog')).toHaveLength(1);
    expect(card()).toHaveAccessibleName('Mo');
  });
  it('Escape returns focus to the pill even after a member row held focus and was re-mounted', async () => {
    window.matchMedia = ((query: string) => ({ matches: query !== PHONE_QUERY, media: query, addEventListener: () => undefined, removeEventListener: () => undefined,
      addListener: () => undefined, removeListener: () => undefined, onchange: null, dispatchEvent: () => false }));
    const online: Record<string, 'online' | 'offline'> = { [ME.id]: 'online', [PEER]: 'online', [MOD]: 'online' };
    const { user, fake } = renderConversation({ items: [peerRow(P1, '1', `hello <@${PEER}>`)], before: f => f.set(`presence:${A}`, online) });
    await user.click(document.querySelector<HTMLElement>(`[data-focus-key="member:${PEER}"]`)!);
    await user.keyboard('{Escape}');
    await user.click(screen.getByRole('textbox', { name: /message/i }));
    act(() => fake.set(`presence:${A}`, { ...online, [PEER]: 'offline' }));
    const pill = within(rowOf(P1)).getByRole('button', { name: '@bob' });
    await user.click(pill);
    expect(card()).toHaveAccessibleName('bob');
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(pill).toHaveFocus();
  });
});
