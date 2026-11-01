import { render, screen, fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect } from 'vitest';
import { MessageLog } from '../MessageLog/MessageLog.tsx';
import { MessageRow } from './MessageRow.tsx';
import { MessageToolbar } from '../MessageToolbar/MessageToolbar.tsx';
import { ReactionBar } from '../ReactionBar/ReactionBar.tsx';
import { AttachmentCard } from '../AttachmentCard/AttachmentCard.tsx';

function Row({ n, active }: { n: number; active: boolean }) {
  return (
    <MessageRow author={`author ${n}`} time="21:0" body={`body ${n}`} state="ok" active={active} seq={String(n)}
      toolbar={<MessageToolbar moreLabel="more actions" label="actions for this message" tabbable={active}
        items={[{ id: 'react', label: 'react', onAction: () => {} }, { id: 'reply', label: 'reply', onAction: () => {} }]} />}
      reply={{ label: 'reply to ada', author: 'ada', excerpt: `quoted ${n}`, state: 'ok', jumpLabel: 'go to the original message', onJump: () => {} }}
      attachments={<AttachmentCard kind="file" name={`f${n}.txt`} size="1 KB" thumbUrl={null} w={null} h={null} openLabel={`open f${n}.txt`}
        saveLabel={`save f${n}.txt`} onSave={() => {}} state="idle" tabbable={active} />}
      reactions={<ReactionBar label="reactions" tabbable={active} items={[{ emoji: '👍', count: 1, mine: false, name: 'thumbs up, 1' }]}
        onToggle={() => {}} addLabel="add a reaction" onAdd={() => {}} />} />
  );
}

describe('the keyboard map of the log (Global Constraints)', () => {
  it('tabs through the active row only: the row, its toolbar, its reply line, its card, its reaction bar', async () => {
    const user = userEvent.setup();
    render(<>
      <button type="button">before</button>
      <MessageLog label="messages in #general" emptyLabel="No messages yet."><Row n={1} active={false} /><Row n={2} active /><Row n={3} active={false} /></MessageLog>
      <button type="button">after</button>
    </>);
    screen.getByRole('button', { name: 'before' }).focus();
    const seen: string[] = [];
    for (let i = 0; i < 6; i += 1) {
      await user.tab();
      const el = document.activeElement as HTMLElement;
      // A button's aria-label, else its visible text without aria-hidden glyphs (the reply button is named by its text).
      const shown = el.getAttribute('aria-label')
        ?? [...el.childNodes].filter((n) => !(n instanceof Element && n.getAttribute('aria-hidden') === 'true')).map((n) => n.textContent).join('');
      seen.push(el.tagName === 'ARTICLE' ? `row ${el.getAttribute('data-seq') ?? ''}` : shown);
    }
    expect(seen).toEqual(['row 2', 'react', 'ada quoted 2', 'save f2.txt', 'thumbs up, 1', 'after']);
  });

  it('Escape from a control of the row brings focus back to the row', () => {
    render(<MessageLog label="messages in #general" emptyLabel="No messages yet."><Row n={2} active /></MessageLog>);
    const chip = screen.getByRole('button', { name: 'thumbs up, 1' });
    chip.focus();
    fireEvent.keyDown(chip, { key: 'Escape' });
    expect(document.activeElement?.tagName).toBe('ARTICLE');
  });
});
