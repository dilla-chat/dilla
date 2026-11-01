import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { COMMANDS, type CommandResult } from '@dilla/client-core';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { CommandComposer, type CommandComposerProps, type CommandSuggestion } from './CommandComposer.tsx';
import { expectNoAxeViolations } from '../../test/setup.ts';

const success: CommandResult = { status: 'success', message: 'Done privately', private: true };
const definitions = COMMANDS;
const suggestion = (id: string, label: string, value = id): CommandSuggestion => ({ id, label, value });

function renderComposer(over: Partial<CommandComposerProps> = {}) {
  const props: CommandComposerProps = {
    definitions,
    initialText: '/',
    onExecute: vi.fn(() => Promise.resolve(success)),
    onCancel: vi.fn(),
    onSendLiteral: vi.fn(),
    suggest: vi.fn(() => Promise.resolve([])),
    contextKey: 'community:general',
    ...over,
  };
  return { ...render(<div className="d-root"><CommandComposer {...props} /></div>), props };
}

async function chooseCommand(user: ReturnType<typeof userEvent.setup>, id: string) {
  if (!screen.queryAllByRole('option').some(option => option.textContent?.includes(`/${id}`))) {
    const picker = screen.getByRole('combobox', { name: 'Command' });
    await user.clear(picker);
    await user.type(picker, id);
  }
  await waitFor(() => expect(screen.getAllByRole('option').some(option => option.textContent?.includes(`/${id}`))).toBe(true));
  await user.click(screen.getAllByRole('option').find(option => option.textContent?.includes(`/${id}`))!);
}

describe('CommandComposer', () => {
  it('routes a fully typed slash command and its argument from the picker into execution', async () => {
    const user = userEvent.setup();
    let executed: unknown;
    renderComposer({ initialText: '/', onExecute: invocation => { executed = invocation; return Promise.resolve({ status: 'success', private: false }); } });
    const picker = screen.getByRole('combobox', { name: 'Command' });
    await user.type(picker, 'me waves hello');
    await user.keyboard('{Enter}');
    expect(await screen.findByRole('combobox', { name: 'text' })).toHaveValue('waves hello');
    await user.click(screen.getByRole('button', { name: /run/i }));
    expect(executed).toEqual({ commandId: 'me', args: { text: 'waves hello' } });
  });
  it('sends the full text of an unknown command typed after the first slash', async () => {
    const user = userEvent.setup();
    const view = renderComposer({ onSendLiteral: vi.fn() });
    await user.type(screen.getByRole('combobox', { name: 'Command' }), 'not-a-command');
    await user.click(screen.getByRole('button', { name: /send as text/i }));
    expect(view.props.onSendLiteral).toHaveBeenCalledWith('/not-a-command');
  });
  beforeEach(() => vi.clearAllMocks());

  it('selects a command, validates named arguments, and explicitly submits a real invocation', async () => {
    const user = userEvent.setup();
    const { props } = renderComposer();
    await chooseCommand(user, 'volume');
    expect(props.onExecute).not.toHaveBeenCalled();
    const level = screen.getByRole('spinbutton', { name: 'level' });
    await user.type(level, '42');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(props.onExecute).toHaveBeenCalledWith({ commandId: 'volume', args: { level: '42' } }));
    expect(await screen.findByRole('status')).toHaveTextContent('Done privately');
  });

  it('uses Enter first to complete a picker choice and then to submit an empty command', async () => {
    const user = userEvent.setup();
    const { props } = renderComposer();
    const picker = screen.getByRole('combobox', { name: 'Command' });
    await user.type(picker, 'leave');
    await user.keyboard('{Enter}');
    expect(screen.getByRole('heading', { name: 'Leave voice' })).toBeInTheDocument();
    expect(props.onExecute).not.toHaveBeenCalled();
    await user.keyboard('{Enter}');
    await waitFor(() => expect(props.onExecute).toHaveBeenCalledWith({ commandId: 'leave', args: {} }));
  });

  it('uses a selected play suggestion URL while allowing a typed query without choosing one', async () => {
    const user = userEvent.setup();
    const url = 'https://music.example/playlist';
    const { props } = renderComposer({ suggest: vi.fn((_id, _option, query) => Promise.resolve(query ? [suggestion('track', 'Found track', url)] : [])) });
    await chooseCommand(user, 'play');
    const field = screen.getByRole('combobox', { name: 'query-or-url' });
    await user.type(field, 'found');
    await user.click(await screen.findByRole('option', { name: /Found track/ }));
    await user.click(screen.getByRole('button', { name: /run command/i }));
    await waitFor(() => expect(props.onExecute).toHaveBeenCalledWith({ commandId: 'play', args: { 'query-or-url': url } }));
  });

  it('exposes optional arguments only when requested and includes the selected value', async () => {
    const user = userEvent.setup();
    const { props } = renderComposer();
    await chooseCommand(user, 'poll create');
    await user.click(screen.getByRole('button', { name: 'Add optional question' }));
    await user.type(screen.getByRole('combobox', { name: 'question' }), 'who');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(props.onExecute).toHaveBeenCalledWith({ commandId: 'poll create', args: { question: 'who' } }));
  });

  it('shows optional values populated by a complete typed command without running it', async () => {
    const { props } = renderComposer({ initialText: '/poll create question:"Lunch?"' });
    await act(() => new Promise(resolve => setTimeout(resolve, 0)));
    expect(screen.getByRole('combobox', { name: 'question' })).toHaveValue('Lunch?');
    expect(screen.queryByRole('button', { name: 'Add optional question' })).not.toBeInTheDocument();
    expect(props.onExecute).not.toHaveBeenCalled();
  });

  it('keeps unknown slash text private and only sends it literally after an explicit choice', async () => {
    const user = userEvent.setup();
    const { props } = renderComposer({ initialText: '/does-not-exist hi' });
    expect(await screen.findByRole('alert')).toHaveTextContent(/unknown command/i);
    await user.click(screen.getByRole('button', { name: /send as text/i }));
    expect(props.onSendLiteral).toHaveBeenCalledTimes(1);
    expect(props.onSendLiteral).toHaveBeenCalledWith('/does-not-exist hi');
  });

  it('waits for explicit send before passing // text and returns ordinary text unchanged', async () => {
    const user = userEvent.setup();
    const literal = renderComposer({ initialText: '//play song' });
    expect(literal.props.onSendLiteral).not.toHaveBeenCalled();
    screen.getByRole('button', { name: /send as text/i }).focus();
    await user.keyboard('{Enter}');
    expect(literal.props.onSendLiteral).toHaveBeenCalledWith('/play song');
    await user.click(screen.getByRole('button', { name: /send as text/i }));
    expect(literal.props.onSendLiteral).toHaveBeenCalledTimes(1);
    literal.unmount();
    const ordinary = renderComposer({ initialText: 'hello\nworld' });
    await waitFor(() => expect(ordinary.props.onCancel).toHaveBeenCalledWith('hello\nworld'));
  });

  it('does not send when a literal draft changes while the component is mounted', () => {
    const literal = renderComposer({ initialText: '//first' });
    literal.rerender(<div className="d-root"><CommandComposer {...literal.props} initialText="//second" /></div>);
    expect(literal.props.onSendLiteral).not.toHaveBeenCalled();
  });

  it('cannot submit a typed command omitted from the available definitions', async () => {
    const user = userEvent.setup();
    const { props } = renderComposer({ initialText: '/kick user:user-1', definitions: definitions.filter(command => command.id !== 'kick') });
    expect(screen.queryByRole('button', { name: /run command/i })).not.toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent(/unavailable|no matching|unknown command/i);
    await user.click(screen.getByRole('combobox', { name: 'Command' }));
    await user.keyboard('{Enter}');
    expect(props.onExecute).not.toHaveBeenCalled();
  });

  it('returns a selected command draft to the host when it becomes unavailable', async () => {
    const { props, rerender } = renderComposer({ initialText: '/leave' });
    rerender(<div className="d-root"><CommandComposer {...props} definitions={definitions.filter(command => command.id !== 'leave')} /></div>);
    await waitFor(() => expect(props.onCancel).toHaveBeenCalledWith('/leave'));
    expect(screen.queryByRole('button', { name: /run command/i })).not.toBeInTheDocument();
  });

  it('keeps positional free text separate from later named options', async () => {
    const user = userEvent.setup();
    const { props } = renderComposer({ initialText: '/search hello scope:all' });
    expect(screen.getByRole('combobox', { name: 'query' })).toHaveValue('hello');
    expect(screen.getByRole('combobox', { name: 'scope' })).toHaveValue('all');
    await user.click(screen.getByRole('button', { name: /run command/i }));
    await waitFor(() => expect(props.onExecute).toHaveBeenCalledWith({ commandId: 'search', args: { query: 'hello', scope: 'all' } }));
  });

  it('preserves a named free-text value containing a quoted newline', async () => {
    const user = userEvent.setup();
    const { props } = renderComposer({ initialText: '/search query:"hello\nworld" scope:all' });
    expect(screen.getByRole('combobox', { name: 'query' })).toHaveValue('hello\nworld');
    expect(screen.getByRole('combobox', { name: 'scope' })).toHaveValue('all');
    await user.click(screen.getByRole('button', { name: /run command/i }));
    await waitFor(() => expect(props.onExecute).toHaveBeenCalledWith({ commandId: 'search', args: { query: 'hello\nworld', scope: 'all' } }));
  });

  it('selects with arrows and Tab, closes suggestions before Escape cancels with the draft', async () => {
    const user = userEvent.setup();
    const { props } = renderComposer({ initialText: '/vo' });
    screen.getByRole('combobox', { name: 'Command' }).focus();
    await user.keyboard('{ArrowDown}{Tab}');
    expect(screen.getByRole('heading', { name: /volume/i })).toBeInTheDocument();
    const level = screen.getByRole('spinbutton', { name: 'level' });
    await user.type(level, '5');
    await user.keyboard('{ArrowDown}{Escape}');
    expect(screen.getByRole('spinbutton', { name: 'level' })).toBeInTheDocument();
    await user.keyboard('{Escape}');
    expect(props.onCancel).toHaveBeenCalledWith(expect.stringContaining('volume'));
  });

  it('cancels from the Run button after a command completes', async () => {
    const user = userEvent.setup();
    const { props } = renderComposer({ initialText: '/help' });
    const run = screen.getByRole('button', { name: /run command/i });
    await user.click(run);
    await screen.findByRole('status');
    run.focus();

    await user.keyboard('{Escape}');

    expect(props.onCancel).toHaveBeenCalledTimes(1);
    expect(props.onCancel).toHaveBeenCalledWith('/help');
  });

  it('cancels from the command Cancel button', async () => {
    const user = userEvent.setup();
    const { props } = renderComposer({ initialText: '/help' });
    screen.getByRole('button', { name: /cancel/i }).focus();

    await user.keyboard('{Escape}');

    expect(props.onCancel).toHaveBeenCalledTimes(1);
    expect(props.onCancel).toHaveBeenCalledWith('/help');
  });

  it('cancels from optional argument buttons and the literal send button', async () => {
    const user = userEvent.setup();
    const { props } = renderComposer({ initialText: '/poll create' });
    const addQuestion = screen.getByRole('button', { name: 'Add optional question' });
    addQuestion.focus();
    await user.keyboard('{Escape}');
    expect(props.onCancel).toHaveBeenCalledTimes(1);
    expect(props.onCancel).toHaveBeenCalledWith('/poll create');

    const literal = renderComposer({ initialText: '//hello' });
    screen.getByRole('button', { name: /send as text/i }).focus();
    await user.keyboard('{Escape}');
    expect(literal.props.onCancel).toHaveBeenCalledTimes(1);
    expect(literal.props.onCancel).toHaveBeenCalledWith('//hello');
    expect(literal.props.onSendLiteral).not.toHaveBeenCalled();
  });

  it('handles input Escape once, dismissing the popup before cancelling', async () => {
    const user = userEvent.setup();
    const { props } = renderComposer({ initialText: '/vo' });
    const picker = screen.getByRole('combobox', { name: 'Command' });
    picker.focus();

    await user.keyboard('{Escape}');
    expect(props.onCancel).not.toHaveBeenCalled();
    await user.keyboard('{Escape}');
    expect(props.onCancel).toHaveBeenCalledTimes(1);
    expect(props.onCancel).toHaveBeenCalledWith('/vo');
  });

  it('ignores composing Escape events', () => {
    const { props } = renderComposer({ initialText: '/help' });
    fireEvent.keyDown(screen.getByRole('button', { name: /run command/i }), { key: 'Escape', isComposing: true, keyCode: 229 });
    expect(props.onCancel).not.toHaveBeenCalled();
  });

  it('keeps native Enter activation on Run to a single execution', async () => {
    const user = userEvent.setup();
    const { props } = renderComposer({ initialText: '/help' });
    screen.getByRole('button', { name: /run command/i }).focus();

    await user.keyboard('{Enter}');

    await waitFor(() => expect(props.onExecute).toHaveBeenCalledTimes(1));
  });

  it('does not treat an IME composition Enter as selection or submission', async () => {
    const user = userEvent.setup();
    const { props } = renderComposer();
    const input = screen.getByRole('combobox', { name: /command/i });
    await user.type(input, 'volume');
    fireEvent.keyDown(input, { key: 'Enter', code: 'Enter', isComposing: true, keyCode: 229 });
    expect(props.onExecute).not.toHaveBeenCalled();
    expect(screen.getByRole('combobox', { name: /command/i })).toBeInTheDocument();
  });

  it('prevents double execution while the handler is pending', async () => {
    const user = userEvent.setup();
    let resolve!: (value: CommandResult) => void;
    const onExecute = vi.fn(() => new Promise<CommandResult>(done => { resolve = done; }));
    const { props } = renderComposer({ onExecute });
    await chooseCommand(user, 'leave');
    const run = screen.getByRole('button', { name: /run command/i });
    fireEvent.click(run);
    fireEvent.click(run);
    expect(props.onExecute).toHaveBeenCalledTimes(1);
    await act(() => { resolve({ status: 'uncertain', private: true, message: 'Still checking' }); return Promise.resolve(); });
    expect(await screen.findByRole('status')).toHaveTextContent(/still checking/i);
    expect(screen.getByRole('button', { name: /run command/i })).toBeInTheDocument();
  });

  it('discards stale suggestions after a context switch', async () => {
    const releases: Array<(value: readonly CommandSuggestion[]) => void> = [];
    const suggest = vi.fn(() => new Promise<readonly CommandSuggestion[]>(done => { releases.push(done); }));
    const view = renderComposer({ initialText: '/dm Alex', suggest });
    await waitFor(() => expect(suggest).toHaveBeenCalled());
    view.rerender(<div className="d-root"><CommandComposer {...view.props} contextKey="community:other" /></div>);
    await act(() => { releases[0]([suggestion('user-1', 'Alex', 'user-1')]); return Promise.resolve(); });
    expect(screen.queryByRole('option', { name: /Alex/ })).not.toBeInTheDocument();
  });

  it('shows a brief notice when suggestions fail and keeps the draft executable', async () => {
    const view = renderComposer({ initialText: '/play harbor', suggest: () => Promise.reject(new Error('private provider response')) });
    expect(await screen.findByRole('note')).toHaveTextContent('Suggestions could not be loaded. Try again.');
    expect(screen.queryByText(/private provider response/)).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: /run command/i }));
    await waitFor(() => expect(view.props.onExecute).toHaveBeenCalledWith({ commandId: 'play', args: { 'query-or-url': 'harbor' } }));
  });

  it('does not show a stale suggestion failure after the query changes', async () => {
    let rejectOld!: (reason: Error) => void;
    const suggest = vi.fn((_command, _option, query: string) => query === 'old'
      ? new Promise<readonly CommandSuggestion[]>((_resolve, reject) => { rejectOld = reject; }) : Promise.resolve([]));
    renderComposer({ initialText: '/play old', suggest });
    await waitFor(() => expect(suggest).toHaveBeenCalled());
    fireEvent.change(screen.getByRole('combobox', { name: 'query-or-url' }), { target: { value: 'new' } });
    await act(() => { rejectOld(new Error('old failure')); return Promise.resolve(); });
    expect(screen.queryByRole('note')).toBeNull();
  });

  it('does not show a stale suggestion failure after a context switch', async () => {
    const rejects: Array<(reason: Error) => void> = [];
    const suggest = () => new Promise<readonly CommandSuggestion[]>((_resolve, reject) => { rejects.push(reject); });
    const view = renderComposer({ initialText: '/play harbor', suggest });
    view.rerender(<div className="d-root"><CommandComposer {...view.props} contextKey="community:other" /></div>);
    await act(() => { rejects[0](new Error('old failure')); return Promise.resolve(); });
    expect(screen.queryByRole('note')).toBeNull();
  });

  it('discards a stale completion after a context switch', async () => {
    let release!: (value: CommandResult) => void;
    const onExecute = vi.fn(() => new Promise<CommandResult>(done => { release = done; }));
    const view = renderComposer({ initialText: '/leave', onExecute });
    fireEvent.click(screen.getByRole('button', { name: /run command/i }));
    expect(onExecute).toHaveBeenCalledTimes(1);
    view.rerender(<div className="d-root"><CommandComposer {...view.props} contextKey="community:other" /></div>);
    await act(() => { release({ status: 'success', private: true, message: 'Old channel completed' }); return Promise.resolve(); });
    expect(screen.getByRole('status')).toBeEmptyDOMElement();
  });

  it('requires choosing a unique entity suggestion and sends its stable ID', async () => {
    const user = userEvent.setup();
    const suggest = vi.fn(() => Promise.resolve([suggestion('u1', 'Jordan'), suggestion('u2', 'Jordan')]));
    const { props } = renderComposer({ initialText: '/dm Jordan', suggest });
    const field = await screen.findByRole('combobox', { name: 'user' });
    await user.click(screen.getByRole('button', { name: /run command/i }));
    expect(props.onExecute).not.toHaveBeenCalled();
    const matches = await screen.findAllByRole('option', { name: /Jordan/ });
    expect(matches).toHaveLength(2);
    await user.click(matches[0]);
    await user.click(screen.getByRole('button', { name: /run command/i }));
    await waitFor(() => expect(props.onExecute).toHaveBeenCalledWith({ commandId: 'dm', args: { user: 'u1' } }));
    expect(field).toHaveValue('Jordan');
  });

  it('invalidates a selected entity ID when its display value is edited', async () => {
    const user = userEvent.setup();
    const suggest = vi.fn(() => Promise.resolve([suggestion('u1', 'Jordan')]));
    const { props } = renderComposer({ initialText: '/dm Jordan', suggest });
    const field = await screen.findByRole('combobox', { name: 'user' });
    await user.click(await screen.findByRole('option', { name: 'Jordan' }));
    await user.type(field, ' Smith');
    await user.click(screen.getByRole('button', { name: /run command/i }));
    expect(props.onExecute).not.toHaveBeenCalled();
    expect(screen.getByRole('alert')).toHaveTextContent(/choose user from the suggestions/i);
  });

  it('preserves multiline free text from a complete initial command', async () => {
    renderComposer({ initialText: '/me first line\nsecond line' });
    await act(async () => { await new Promise(resolve => setTimeout(resolve, 0)); });
    expect(screen.getByRole('combobox', { name: 'text' })).toHaveValue('first line\nsecond line');
    expect(screen.getByRole('button', { name: /run command/i })).toBeInTheDocument();
  });

  it('preserves multiline free text after positional arguments in a complete command', async () => {
    renderComposer({ initialText: '/code js first line\nsecond line' });
    await act(async () => { await new Promise(resolve => setTimeout(resolve, 0)); });
    expect(screen.getByRole('combobox', { name: 'language' })).toHaveValue('js');
    expect(screen.getByRole('combobox', { name: 'text' })).toHaveValue('first line\nsecond line');
  });

  it('retains values when execution rejects or reports uncertainty', async () => {
    const user = userEvent.setup();
    const onExecute = vi.fn().mockRejectedValueOnce(new Error('offline')).mockResolvedValueOnce({ status: 'uncertain', private: true, message: 'Outcome not confirmed' });
    renderComposer({ onExecute });
    await chooseCommand(user, 'play');
    const field = screen.getByRole('combobox', { name: 'query-or-url' });
    await user.type(field, 'song');
    await user.keyboard('{Enter}');
    expect(await screen.findByRole('alert')).toHaveTextContent(/offline/i);
    expect(field).toHaveValue('song');
    await user.keyboard('{Enter}');
    expect(await screen.findByRole('status')).toHaveTextContent(/outcome not confirmed/i);
    expect(field).toHaveValue('song');
  });

  it('runs no-argument commands only after an explicit submit and passes private result status', async () => {
    const user = userEvent.setup();
    const { props, container } = renderComposer({ initialText: '/leave' });
    expect(props.onExecute).not.toHaveBeenCalled();
    await expectNoAxeViolations(container);
    await user.click(screen.getByRole('button', { name: /run command/i }));
    await waitFor(() => expect(props.onExecute).toHaveBeenCalledWith({ commandId: 'leave', args: {} }));
  });
});


it('renders suggestion notices separately from selectable entities', async () => {
  const user = userEvent.setup(); const message = 'Poll browsing reached 1000 records. Enter the exact poll ID to find an older poll.';
  const { props } = renderComposer({ initialText: '/poll results', suggest: () => Promise.resolve(Object.assign([], { notice: message })) });
  await user.type(screen.getByRole('combobox', { name: 'poll' }), 'older');
  expect(await screen.findByRole('note')).toHaveTextContent(message);
  expect(screen.queryByRole('option')).toBeNull();
  await user.click(screen.getByRole('button', { name: /run command/i }));
  expect(props.onExecute).not.toHaveBeenCalled();
});
