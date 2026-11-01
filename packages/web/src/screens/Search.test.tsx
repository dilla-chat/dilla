import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { it, expect, vi } from 'vitest';
import { Search } from './Search.tsx';

const messages = (run: () => boolean | Promise<boolean>, label = 'lamp') => [{ id: 'messages', label: 'Messages',
  items: [{ id: 'msg:1', label, sub: '#general', icon: 'message-square' as const, run }] }];

it('keeps focus in the textbox and runs the picked result, closing when it went somewhere', async () => {
  const user = userEvent.setup();
  const run = vi.fn(() => true);
  const onClose = vi.fn();
  render(<Search open query="lamp" groups={messages(run)} onQuery={vi.fn()} onClose={onClose} />);
  const input = screen.getByRole('combobox', { name: 'Search channels, people and messages' });
  input.focus();
  await user.keyboard('{ArrowDown}');
  expect(input).toHaveFocus();
  await user.keyboard('{Enter}');
  expect(run).toHaveBeenCalledOnce();
  expect(onClose).toHaveBeenCalledOnce();
});

it('names its close button for the search, not for Settings', async () => {
  const onClose = vi.fn();
  render(<Search open query="" groups={[]} onQuery={vi.fn()} onClose={onClose} />);
  expect(screen.queryByRole('button', { name: 'Close settings' })).toBeNull();
  await userEvent.setup().click(screen.getByRole('button', { name: 'Close' }));
  expect(onClose).toHaveBeenCalledOnce();
});

it('announces a missing row and focuses a surviving textbox', async () => {
  const user = userEvent.setup();
  render(<Search open query="gone" groups={messages(() => Promise.resolve(false), 'gone')} onQuery={vi.fn()} onClose={vi.fn()} />);
  await user.click(screen.getByRole('option', { name: /gone/ }));
  expect(screen.getByRole('status')).toHaveTextContent('That message is no longer available');
  expect(screen.getByRole('combobox', { name: 'Search channels, people and messages' })).toHaveFocus();
});

// J1, R-S4: on a desktop the palette is the one modal dialog (no wrapper of its own): the shell stays in the DOM and Escape gives focus back.
it('is the palette’s own modal dialog on a desktop, over a shell that stays mounted, and Escape returns focus', async () => {
  const user = userEvent.setup();
  const onClose = vi.fn();
  const Host = ({ open }: { open: boolean }) => <>
    <main><button type="button">opener</button></main>
    <Search open={open} query="" groups={[]} onQuery={vi.fn()} onClose={onClose} />
  </>;
  const view = render(<Host open={false} />);
  screen.getByRole('button', { name: 'opener' }).focus();
  view.rerender(<Host open />);
  const dialog = screen.getByRole('dialog', { name: 'Search channels, people and messages' });
  expect(dialog).toHaveClass('d-search-palette-dialog');
  expect(document.querySelectorAll('dialog')).toHaveLength(1);
  expect(document.querySelector('.dw-search')).toBeNull();
  expect(screen.getByRole('button', { name: 'opener', hidden: true })).toBeInTheDocument();
  expect(screen.getByRole('combobox', { name: 'Search channels, people and messages' })).toHaveFocus();
  await user.keyboard('{Escape}');
  expect(onClose).toHaveBeenCalledOnce();
  view.rerender(<Host open={false} />);
  expect(screen.getByRole('button', { name: 'opener' })).toHaveFocus();
});
