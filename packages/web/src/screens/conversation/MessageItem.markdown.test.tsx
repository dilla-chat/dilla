// E5: a message body renders its markdown as elements; hostile input never becomes markup or a live link.
import { describe, expect, it } from 'vitest';
import { within } from '@testing-library/react';
import { P1, PEER, ownRow, peerRow, renderConversation, rowOf } from '../../test/conversation.tsx';
import { toolbarButton } from '../../test/toolbar.ts';

const bodyOf = (id: string) => rowOf(id).querySelector<HTMLElement>('.d-message-row__body')!;

describe('markdown in a message body (E5)', () => {
  it('renders formatting, code, a quote and a link', () => {
    renderConversation({ items: [peerRow(P1, '1', '**bold** *it* ~~no~~ `c` https://example.com/x\n> quoted')] });
    const body = bodyOf(P1);
    expect(body.querySelector('strong')?.textContent).toBe('bold');
    expect(body.querySelector('em')?.textContent).toBe('it');
    expect(body.querySelector('s')?.textContent).toBe('no');
    expect(body.querySelector('code')?.textContent).toBe('c');
    expect(body.querySelector('blockquote')?.textContent).toBe('quoted');
    const link = within(body).getByRole('link', { name: 'https://example.com/x' });
    expect(link).toHaveAttribute('rel', 'noopener noreferrer nofollow');
    expect(link).toHaveAttribute('target', '_blank');
  });
  it('keeps a mention a pill inside bold, and shows it raw nowhere', () => {
    renderConversation({ items: [peerRow(P1, '1', `**hi <@${PEER}>**`)] });
    const pill = bodyOf(P1).querySelector('strong .d-mention');
    expect(pill?.textContent).toBe('@bob');
    expect(bodyOf(P1)).not.toHaveTextContent('<@');
  });
  it('does not turn a mention in code into a pill', () => {
    renderConversation({ items: [peerRow(P1, '1', `\`<@${PEER}>\``)] });
    expect(bodyOf(P1).querySelector('.d-mention')).toBeNull();
    expect(bodyOf(P1).querySelector('code')?.textContent).toBe(`<@${PEER}>`);
  });
  it('never renders raw html or a hostile link', () => {
    renderConversation({ items: [peerRow(P1, '1', '<img src=x onerror=alert(1)><script>alert(1)</script> [x](javascript:alert(1)) javascript:alert(2) <a href="https://e.example">z</a>')] });
    const body = bodyOf(P1);
    expect(body.querySelector('img, script, iframe, style')).toBeNull();
    expect(body.querySelectorAll('a[href^="javascript"]')).toHaveLength(0);
    expect(body.textContent).toContain('<img src=x onerror=alert(1)>');
    expect(body.textContent).toContain('javascript:alert(2)');
  });
  it('F1: a bidi override before a link cannot reorder the link: it is gone, and the anchor is a ltr isolate showing its href', () => {
    renderConversation({ items: [peerRow(P1, '1', '‮ https://evil.example/?moc.elgoog//:sptth')] });
    const a = within(bodyOf(P1)).getByRole('link');
    expect(a).toHaveAttribute('dir', 'ltr');
    expect(a.textContent).toBe('https://evil.example/?moc.elgoog//:sptth');
    expect(a).toHaveAttribute('href', a.textContent);
    expect(bodyOf(P1).textContent).not.toMatch(/[‪-‮⁦-⁩]/);
  });
  it('renders the raw text of a message in the editor, markers and all', async () => {
    const { user } = renderConversation({ items: [ownRow('1'.repeat(32), '1', '**bold** https://example.com')] });
    await user.hover(rowOf('1'.repeat(32)));
    await user.click(toolbarButton(within(rowOf('1'.repeat(32))).getByRole('toolbar'), 'edit'));
    expect(within(rowOf('1'.repeat(32))).getByRole('textbox')).toHaveValue('**bold** https://example.com');
  });
});
