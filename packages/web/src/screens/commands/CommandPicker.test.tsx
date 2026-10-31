import { readFileSync } from 'node:fs';
import { useState } from 'react';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { COMMANDS, type CommandResult } from '@dilla/client-core';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { CommandPicker, type CommandComposerProps, type CommandSuggestion } from './CommandPicker.tsx';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient } from '../../test/fake-client.ts';
import { clearCommandDraftMemory, CommandHost } from './CommandHost.tsx';
import { privateResults } from './private-results.ts';
import { CommandProvider } from './CommandContext.tsx';
import type { CommandProviderHandlers } from './command-types.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';

const success: CommandResult = { status: 'success', message: 'Done privately', private: true };
const definitions = COMMANDS;
const suggestion = (id: string, label: string, value = id, description?: string): CommandSuggestion => ({ id, label, value, ...(description !== undefined ? { description } : {}) });
// Friday 30 October 2026, 18:00 in Stockholm (CET, UTC+1).
const NOW = new Date('2026-10-30T17:00:00Z');

function renderPicker(over: Partial<CommandComposerProps> = {}) {
  const props: CommandComposerProps = {
    definitions,
    initialText: '/',
    onExecute: vi.fn(() => Promise.resolve(success)),
    onCancel: vi.fn(),
    onSendLiteral: vi.fn(),
    suggest: vi.fn(() => Promise.resolve([])),
    contextKey: 'community:general',
    timeZone: 'Europe/Stockholm',
    now: () => NOW,
    ...over,
  };
  return { ...render(<div className="d-root"><CommandPicker {...props} /></div>), props };
}
const draftField = () => screen.getByRole('textbox', { name: 'Command' });
const send = () => screen.getByRole('button', { name: 'send' });

beforeEach(() => { vi.clearAllMocks(); clearCommandDraftMemory(); });

describe('CommandPicker: the / list', () => {
  it('lists every command for "/", filters by startsWith then includes, and names rows "/name, description"', async () => {
    const user = userEvent.setup();
    renderPicker();
    expect(screen.getByText('Commands')).toBeInTheDocument();
    expect(screen.getByRole('option', { name: '/remind, A private one-time reminder' })).toBeInTheDocument();
    await user.type(draftField(), 're');
    const names = screen.getAllByRole('option').map(option => option.getAttribute('aria-label')!.split(',')[0]);
    expect(names.slice(0, 7)).toEqual(['/reply', '/resume', '/repeat', '/remind', '/reminders list', '/reminders cancel', '/reminders dismiss']);
    expect(names.indexOf('/announcement create')).toBeGreaterThan(names.indexOf('/reminders dismiss'));
  });

  it('shows the prototype args: <required>, [optional], a bounded number, "opens a form"', () => {
    renderPicker();
    const row = (id: string) => screen.getByRole('option', { name: new RegExp(`^/${id},`) });
    expect(row('ban')).toHaveTextContent('/ban <user> [duration] [reason]');
    expect(row('volume')).toHaveTextContent('/volume <0..100>');
    expect(row('poll create')).toHaveTextContent('/poll create opens a form');
  });

  it('picks with Tab; Escape in the list clears the draft', async () => {
    const user = userEvent.setup();
    const first = renderPicker({ initialText: '/vo' });
    draftField().focus();
    await user.keyboard('{Tab}');
    expect(screen.getByRole('combobox', { name: '0..100' })).toHaveFocus();
    first.unmount();
    const second = renderPicker({ initialText: '/re' });
    draftField().focus();
    await user.keyboard('{ArrowDown}{Escape}');
    expect(second.props.onCancel).toHaveBeenCalledWith('');
  });

  it('says nothing matches and posts the draft literally on "Send as text"', async () => {
    const user = userEvent.setup();
    const { props } = renderPicker();
    await user.type(draftField(), 'xyz');
    expect(screen.getByText('No command matches /xyz')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Send as text' }));
    expect(props.onSendLiteral).toHaveBeenCalledWith('/xyz');
  });

  it('double slash posts literal', async () => {
    const user = userEvent.setup();
    const { props } = renderPicker();
    await user.type(draftField(), '/shrug');
    expect(screen.queryByRole('listbox')).toBeNull();
    await user.keyboard('{Enter}');
    expect(props.onSendLiteral).toHaveBeenCalledWith('/shrug');
  });

  it('routes a fully typed command and its argument into chips', async () => {
    const user = userEvent.setup();
    const { props } = renderPicker();
    await user.type(draftField(), 'me waves hello');
    await user.keyboard('{Enter}');
    expect(await screen.findByRole('textbox', { name: 'text' })).toHaveValue('waves hello');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(props.onExecute).toHaveBeenCalledWith({ commandId: 'me', args: { text: 'waves hello' } }));
    expect(props.onCancel).toHaveBeenCalledWith('');
  });

  it('hands the draft back when the slash is deleted', async () => {
    const user = userEvent.setup();
    const { props } = renderPicker();
    await user.type(draftField(), '{Backspace}');
    expect(props.onCancel).toHaveBeenCalledWith('');
  });

  it('capability dropped while open: the command leaves the list', () => {
    const view = renderPicker({ initialText: '/poll', listed: new Set(definitions.map(command => command.id)) });
    expect(screen.getByRole('option', { name: /^\/poll create,/ })).toBeInTheDocument();
    view.rerender(<div className="d-root"><CommandPicker {...view.props} listed={new Set(definitions.filter(command => command.id !== 'poll create').map(command => command.id))} /></div>);
    expect(screen.queryByRole('option', { name: /^\/poll create,/ })).toBeNull();
    expect(screen.getByRole('option', { name: /^\/poll vote,/ })).toBeInTheDocument();
  });

  it('runs poll create detached: it opens its form instead of chips', async () => {
    const user = userEvent.setup();
    const onRunDetached = vi.fn();
    renderPicker({ initialText: '/poll c', onRunDetached });
    await user.click(screen.getByRole('option', { name: /^\/poll create,/ }));
    expect(onRunDetached).toHaveBeenCalledWith({ commandId: 'poll create', args: {} });
    expect(screen.queryByRole('combobox', { name: 'question' })).toBeNull();
  });
});

describe('CommandPicker: chips', () => {
  it('shows one chip per argument, optional labels with "?", the description in the bar', () => {
    renderPicker({ initialText: '/ban' });
    expect(screen.getByRole('combobox', { name: 'user' })).toHaveFocus();
    expect(screen.getByText('duration?')).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'reason' })).toHaveAttribute('placeholder', 'optional');
    expect(screen.getByText('Ban for a while, or for good · checked against your roles')).toBeInTheDocument();
    expect(screen.getByText('tab next field · ↵ run · esc leave')).toBeInTheDocument();
  });

  it('a command without arguments shows one ghost chip and runs on Enter', async () => {
    const user = userEvent.setup();
    const { props } = renderPicker({ initialText: '/pause', room: 'Harbour' });
    expect(screen.getByRole('textbox', { name: 'press ↵ to run' })).toHaveFocus();
    expect(screen.getByText('Pause playback · Harbour')).toBeInTheDocument();
    await user.keyboard('{Enter}');
    await waitFor(() => expect(props.onExecute).toHaveBeenCalledWith({ commandId: 'pause', args: {} }));
  });

  it('says to join voice first for a music command outside voice', () => {
    renderPicker({ initialText: '/skip', room: null });
    expect(screen.getByText('Skip to the next track · join voice first')).toBeInTheDocument();
  });

  it('marks the first missing field and says so in the bar', async () => {
    const user = userEvent.setup();
    const { props } = renderPicker({ initialText: '/remind' });
    await user.keyboard('{Enter}');
    expect(screen.getByRole('alert')).toHaveTextContent('when is required');
    expect(screen.getByRole('combobox', { name: 'when' })).toHaveAttribute('aria-invalid', 'true');
    expect(props.onExecute).not.toHaveBeenCalled();
  });

  it('validates a bounded number with the prototype copy', async () => {
    const user = userEvent.setup();
    const { props } = renderPicker({ initialText: '/volume' });
    await user.type(screen.getByRole('combobox', { name: '0..100' }), '150{Enter}');
    expect(screen.getByRole('alert')).toHaveTextContent('A whole number from 0 to 100');
    await user.clear(screen.getByRole('combobox', { name: '0..100' }));
    await user.type(screen.getByRole('combobox', { name: '0..100' }), '42{Enter}');
    await waitFor(() => expect(props.onExecute).toHaveBeenCalledWith({ commandId: 'volume', args: { level: '42' } }));
  });

  it('lists the when phrases with their previews; tab accepts suggestion; the bar previews the time', async () => {
    const user = userEvent.setup();
    const { props } = renderPicker({ initialText: '/remind' });
    expect(screen.getByRole('listbox', { name: 'When' })).toBeInTheDocument();
    expect(screen.getByRole('option', { name: 'tonight 20:00, Fri 30 Oct, 20:00 CET' })).toBeInTheDocument();
    expect(screen.getByRole('option', { name: 'tomorrow 09:00, Sat 31 Oct, 09:00 CET' })).toBeInTheDocument();
    await user.type(screen.getByRole('combobox', { name: 'when' }), 'tonig');
    expect(screen.getByText('Try “in 1 hour” or “tomorrow 09:00”')).toBeInTheDocument();
    await user.keyboard('{Tab}');
    expect(screen.getByRole('combobox', { name: 'when' })).toHaveValue('tonight 20:00');
    expect(screen.getByRole('textbox', { name: 'text' })).toHaveFocus();
    await user.type(screen.getByRole('textbox', { name: 'text' }), 'bring two ropes');
    await user.click(screen.getByRole('combobox', { name: 'when' }));
    expect(screen.getByText('Reminds you Fri 30 Oct, 20:00 CET')).toBeInTheDocument();
    await user.click(send());
    await waitFor(() => expect(props.onExecute).toHaveBeenCalledWith({ commandId: 'remind', args: { when: '2026-10-30T20:00', text: 'bring two ropes', timezone: 'Europe/Stockholm' } }));
  });

  it('refuses a time outside the window', async () => {
    const user = userEvent.setup();
    renderPicker({ initialText: '/remind' });
    await user.type(screen.getByRole('combobox', { name: 'when' }), 'in 400 days');
    expect(screen.getByText('Between 1 minute and 365 days from now')).toBeInTheDocument();
    await user.keyboard('{Tab}');
    await user.type(screen.getByRole('textbox', { name: 'text' }), 'x{Enter}');
    expect(screen.getByRole('alert')).toHaveTextContent('Between 1 minute and 365 days from now');
  });

  it('ambiguous time lists both candidates', async () => {
    const user = userEvent.setup();
    // Saturday 24 October 2026 12:00 UTC: Stockholm's clocks go back at 03:00 the next night, so 02:30 happens twice.
    const { props } = renderPicker({ initialText: '/remind', now: () => new Date('2026-10-24T12:00:00Z') });
    await user.type(screen.getByRole('combobox', { name: 'when' }), 'tomorrow 02:30');
    expect(screen.getByText('This local time occurs twice. Choose which time you mean.')).toBeInTheDocument();
    expect(screen.getAllByRole('option').map(option => option.getAttribute('aria-label'))).toEqual([
      'Earlier · UTC+02:00 · Sun 25 Oct, 02:30 CEST',
      'Later · UTC+01:00 · Sun 25 Oct, 02:30 CET',
    ]);
    await user.keyboard('{ArrowDown}{Enter}');
    await user.type(screen.getByRole('textbox', { name: 'text' }), 'call home{Enter}');
    await waitFor(() => expect(props.onExecute).toHaveBeenCalledWith({ commandId: 'remind',
      args: { when: '2026-10-25T02:30', text: 'call home', timezone: 'Europe/Stockholm', chosen: String(Date.UTC(2026, 9, 25, 1, 30) / 1000) } }));
  });

  it('esc keeps filled draft', async () => {
    const user = userEvent.setup();
    const { props } = renderPicker({ initialText: '/remind' });
    await user.type(screen.getByRole('combobox', { name: 'when' }), 'in 1 hour');
    await user.keyboard('{Tab}');
    await user.type(screen.getByRole('textbox', { name: 'text' }), 'bring two ropes');
    await user.keyboard('{Escape}');
    expect(props.onCancel).toHaveBeenCalledWith('/remind in 1 hour bring two ropes');
  });

  it('backspace leaves from first chip', async () => {
    const user = userEvent.setup();
    renderPicker({ initialText: '/remind' });
    await user.click(screen.getByRole('textbox', { name: 'text' }));
    await user.keyboard('{Backspace}');
    expect(screen.getByRole('combobox', { name: 'when' })).toHaveFocus();
    await user.keyboard('{Backspace}');
    expect(draftField()).toHaveValue('/remind');
    expect(draftField()).toHaveFocus();
    expect(screen.getByRole('option', { name: /^\/remind,/ })).toBeInTheDocument();
  });

  it('a typed /remind splits its time from its text', () => {
    renderPicker({ initialText: '/remind in 1 hour bring two ropes' });
    expect(screen.getByRole('combobox', { name: 'when' })).toHaveValue('in 1 hour');
    expect(screen.getByRole('textbox', { name: 'text' })).toHaveValue('bring two ropes');
  });

  it('tab accepts suggestion: a member by name, then runs with the stable id', async () => {
    const user = userEvent.setup();
    const suggest = vi.fn((_id: string, _option: unknown, query: string) => Promise.resolve([suggestion('u1', 'Björn', 'u1', '@bjorn'), suggestion('u2', 'Kari', 'u2', '@kari')]
      .filter(row => row.label.toLowerCase().includes(query.toLowerCase()))));
    const { props } = renderPicker({ initialText: '/kick', suggest });
    expect(await screen.findByRole('listbox', { name: 'Members' })).toBeInTheDocument();
    expect(screen.getByRole('option', { name: 'Björn, @bjorn' })).toBeInTheDocument();
    await user.type(screen.getByRole('combobox', { name: 'user' }), 'bj');
    await screen.findByRole('option', { name: 'Björn, @bjorn' });
    await user.keyboard('{Tab}');
    expect(screen.getByRole('combobox', { name: 'user' })).toHaveValue('Björn');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(props.onExecute).toHaveBeenCalledWith({ commandId: 'kick', args: { user: 'u1' } }));
  });

  it('says no member is called that', async () => {
    const user = userEvent.setup();
    renderPicker({ initialText: '/kick', suggest: () => Promise.resolve([]) });
    await user.type(screen.getByRole('combobox', { name: 'user' }), 'Nobody{Enter}');
    expect(screen.getByRole('alert')).toHaveTextContent('No member called Nobody here');
  });

  it('turns a spoken duration into the registry form', async () => {
    const user = userEvent.setup();
    const suggest = vi.fn(() => Promise.resolve([suggestion('u1', 'Kari', 'u1', '@kari')]));
    const { props } = renderPicker({ initialText: '/timeout', suggest });
    await user.type(screen.getByRole('combobox', { name: 'user' }), 'Kari');
    await screen.findByRole('option', { name: 'Kari, @kari' });
    await user.keyboard('{Tab}');
    expect(screen.getByRole('listbox', { name: 'Durations' })).toBeInTheDocument();
    await user.type(screen.getByRole('combobox', { name: 'duration' }), '10 min{Tab}');
    expect(screen.getByRole('combobox', { name: 'duration' })).toHaveValue('10 minutes');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(props.onExecute).toHaveBeenCalledWith({ commandId: 'timeout', args: { user: 'u1', duration: '10m' } }));
  });

  it('uses a selected play suggestion URL', async () => {
    const user = userEvent.setup();
    const url = 'https://music.example/playlist';
    const { props } = renderPicker({ initialText: '/play', suggest: vi.fn((_id, _option, query) => Promise.resolve(query ? [suggestion('track', 'Found track', url)] : [])) });
    await user.type(screen.getByRole('combobox', { name: 'query-or-url' }), 'found');
    await user.click(await screen.findByRole('option', { name: 'Found track' }));
    await user.click(send());
    await waitFor(() => expect(props.onExecute).toHaveBeenCalledWith({ commandId: 'play', args: { 'query-or-url': url } }));
  });

  it('enter twice runs once', async () => {
    let resolve!: (value: CommandResult) => void;
    const onExecute = vi.fn(() => new Promise<CommandResult>(done => { resolve = done; }));
    renderPicker({ initialText: '/leave', onExecute });
    const ghost = screen.getByRole('textbox', { name: 'press ↵ to run' });
    fireEvent.keyDown(ghost, { key: 'Enter' });
    fireEvent.keyDown(ghost, { key: 'Enter' });
    fireEvent.click(send());
    expect(onExecute).toHaveBeenCalledTimes(1);
    await act(() => { resolve({ status: 'uncertain', private: true, message: 'Still checking' }); return Promise.resolve(); });
    expect(screen.getByRole('alert')).toHaveTextContent('Still checking');
  });

  it('keeps the chips and their values when a run throws or fails', async () => {
    const user = userEvent.setup();
    const onExecute = vi.fn().mockRejectedValueOnce(new Error('offline')).mockResolvedValueOnce({ status: 'failure', private: true, message: 'Join a voice channel first.' });
    renderPicker({ initialText: '/play', onExecute });
    const field = screen.getByRole('combobox', { name: 'query-or-url' });
    await user.type(field, 'song{Enter}');
    expect(await screen.findByRole('alert')).toHaveTextContent('offline');
    expect(field).toHaveValue('song');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('Join a voice channel first.'));
    expect(field).toHaveValue('song');
  });

  it('returns a selected command draft to the host when it becomes unavailable', async () => {
    const { props, rerender } = renderPicker({ initialText: '/leave' });
    rerender(<div className="d-root"><CommandPicker {...props} definitions={definitions.filter(command => command.id !== 'leave')} /></div>);
    await waitFor(() => expect(props.onCancel).toHaveBeenCalledWith('/leave'));
  });

  it('cannot open a typed command omitted from the available definitions', async () => {
    const user = userEvent.setup();
    const { props } = renderPicker({ initialText: '/kick user:user-1', definitions: definitions.filter(command => command.id !== 'kick') });
    expect(screen.getByText('No command matches /kick user:user-1')).toBeInTheDocument();
    draftField().focus();
    await user.keyboard('{Enter}');
    expect(props.onExecute).not.toHaveBeenCalled();
  });

  it('keeps positional free text separate from later named options', async () => {
    const user = userEvent.setup();
    const { props } = renderPicker({ initialText: '/search hello scope:all' });
    expect(screen.getByRole('textbox', { name: 'query' })).toHaveValue('hello');
    expect(screen.getByRole('combobox', { name: 'scope' })).toHaveValue('all');
    await user.click(send());
    await waitFor(() => expect(props.onExecute).toHaveBeenCalledWith({ commandId: 'search', args: { query: 'hello', scope: 'all' } }));
  });

  it('removes confirmation focus paint when the presentation override ends while the chip is blurred', async () => {
    const user = userEvent.setup();
    const style = document.createElement('style');
    style.textContent = '.d-composer__box { border: 1px solid #232831; box-shadow: none; }' +
      readFileSync('src/screens/commands/CommandHost.css', 'utf8')
        .replaceAll('var(--accent)', '#5aa8e6').replaceAll('var(--accent-soft)', 'rgba(90, 168, 230, 0.12)');
    document.head.append(style);
    try {
      const view = renderPicker({ initialText: '/remind' });
      render(<button type="button">elsewhere</button>);
      await user.click(screen.getByRole('button', { name: 'elsewhere' }));
      view.rerender(<div className="d-root"><CommandPicker {...view.props} moderationConfirmOpen /></div>);
      const box = document.querySelector('.d-composer__box') as HTMLElement;
      expect(getComputedStyle(box).borderColor).toBe('rgb(90, 168, 230)');
      expect(getComputedStyle(box).boxShadow).toBe('0 0 0 3px rgba(90, 168, 230, 0.12)');
      view.rerender(<div className="d-root"><CommandPicker {...view.props} moderationConfirmOpen={false} /></div>);
      expect(box.closest('.dw-command-picker')).not.toHaveAttribute('data-confirming');
      expect(getComputedStyle(box).borderColor).toBe('rgb(35, 40, 49)');
      expect(getComputedStyle(box).boxShadow).toBe('none');
      expect(screen.queryByRole('listbox', { name: 'When' })).toBeNull();
    } finally { style.remove(); }
  });

  it('hides argument suggestions on ordinary blur and restores them when the chip regains focus', async () => {
    const user = userEvent.setup();
    renderPicker({ initialText: '/remind' });
    render(<button type="button">elsewhere</button>);
    const field = screen.getByRole('combobox', { name: 'when' });
    await user.click(field);
    expect(screen.getByRole('listbox', { name: 'When' })).toBeVisible();
    await user.click(screen.getByRole('button', { name: 'elsewhere' }));
    expect(screen.queryByRole('listbox', { name: 'When' })).toBeNull();
    await user.click(field);
    expect(screen.getByRole('listbox', { name: 'When' })).toBeVisible();
  });

  it('discards stale suggestions after a context switch', async () => {
    const releases: Array<(value: readonly CommandSuggestion[]) => void> = [];
    const suggest = vi.fn(() => new Promise<readonly CommandSuggestion[]>(done => { releases.push(done); }));
    const view = renderPicker({ initialText: '/dm Alex', suggest });
    await waitFor(() => expect(suggest).toHaveBeenCalled());
    view.rerender(<div className="d-root"><CommandPicker {...view.props} contextKey="community:other" /></div>);
    await act(() => { releases[0]([suggestion('user-1', 'Alex', 'user-1')]); return Promise.resolve(); });
    expect(screen.queryByRole('option', { name: /Alex/ })).not.toBeInTheDocument();
  });

  it('requires choosing one of two same-named members and sends its stable id', async () => {
    const user = userEvent.setup();
    const suggest = vi.fn(() => Promise.resolve([suggestion('u1', 'Jordan', 'u1', '@jordan1'), suggestion('u2', 'Jordan', 'u2', '@jordan2')]));
    const { props } = renderPicker({ initialText: '/dm Jordan', suggest });
    await screen.findAllByRole('option', { name: /Jordan/ });
    await user.click(send());
    expect(props.onExecute).not.toHaveBeenCalled();
    expect(screen.getByRole('alert')).toHaveTextContent('No member called Jordan here');
    await user.click(screen.getByRole('combobox', { name: 'user' }));
    await user.click(screen.getAllByRole('option', { name: /Jordan/ })[0]);
    await user.click(send());
    await waitFor(() => expect(props.onExecute).toHaveBeenCalledWith({ commandId: 'dm', args: { user: 'u1' } }));
  });

  it('renders suggestion notices in the bar, never as an option', async () => {
    const user = userEvent.setup(); const message = 'Poll browsing reached 1000 records. Enter the exact poll ID to find an older poll.';
    const { props } = renderPicker({ initialText: '/poll results', suggest: () => Promise.resolve(Object.assign([], { notice: message })) });
    await user.type(screen.getByRole('combobox', { name: 'poll' }), 'older');
    expect(await screen.findByText(message)).toBeInTheDocument();
    expect(screen.queryByRole('option')).toBeNull();
    await user.click(send());
    expect(props.onExecute).not.toHaveBeenCalled();
  });

  it('ignores composing keys', () => {
    const { props } = renderPicker({ initialText: '/help' });
    fireEvent.keyDown(screen.getByRole('combobox', { name: 'command' }), { key: 'Escape', isComposing: true, keyCode: 229 });
    fireEvent.keyDown(screen.getByRole('combobox', { name: 'command' }), { key: 'Enter', isComposing: true, keyCode: 229 });
    expect(props.onCancel).not.toHaveBeenCalled();
    expect(props.onExecute).not.toHaveBeenCalled();
  });

  it('has no axe violations in the list and in the chips', async () => {
    const list = renderPicker();
    await expectNoAxeViolations(list.container);
    list.unmount();
    const chips = renderPicker({ initialText: '/remind' });
    await expectNoAxeViolations(chips.container);
  });
});

describe('CommandPicker: fix round 1', () => {
  const fill = (name: string, value: string, role: 'combobox' | 'textbox' = 'combobox') => fireEvent.change(screen.getByRole(role, { name }), { target: { value } });
  const escape = (name: string, role: 'combobox' | 'textbox' = 'combobox') => fireEvent.keyDown(screen.getByRole(role, { name }), { key: 'Escape' });

  it.each([
    ['timeout', [['user', 'Bea Smith'], ['duration', '10 minutes']]],
    ['ban', [['user', 'Björn Ek'], ['duration', '7 days'], ['reason', 'spam and more']]],
    ['ban', [['user', 'Bea'], ['duration', '7 days'], ['reason', 'spam']]],
    ['warn', [['user', 'Ada "the map" Lind'], ['reason', 'bridge troll']]],
  ] as const)('Esc on filled /%s chips round-trips into the same chips', (command, values) => {
    const first = renderPicker({ initialText: `/${command}` });
    for (const [name, value] of values) fill(name, value, name === 'reason' ? 'textbox' : 'combobox');
    escape('user');
    const text = vi.mocked(first.props.onCancel).mock.calls.at(-1)![0];
    first.unmount();
    renderPicker({ initialText: text });
    for (const [name, value] of values) expect(screen.getByRole(name === 'reason' ? 'textbox' : 'combobox', { name })).toHaveValue(value);
  });

  it('a typed /poll create opens its form', () => {
    const onRunDetached = vi.fn();
    renderPicker({ initialText: '/poll create', onRunDetached });
    expect(onRunDetached).toHaveBeenCalledWith({ commandId: 'poll create', args: {} });
  });

  it('Esc on a multi-line /code value round-trips with its newlines', async () => {
    const user = userEvent.setup();
    const first = renderPicker({ initialText: '/code yaml' });
    await user.click(screen.getByRole('textbox', { name: 'text' }));
    await user.keyboard('name: harbour{Shift>}{Enter}{/Shift}size: 6{Shift>}{Enter}{/Shift}crew: 4');
    await user.keyboard('{Escape}');
    const text = vi.mocked(first.props.onCancel).mock.calls.at(-1)![0];
    first.unmount();
    renderPicker({ initialText: text });
    expect(screen.getByRole('combobox', { name: 'language' })).toHaveValue('yaml');
    expect(screen.getByRole('textbox', { name: 'text' })).toHaveValue('name: harbour\nsize: 6\ncrew: 4');
  });

  it('a multi-line /code value grows its chip, survives edits and runs with its newlines', async () => {
    const user = userEvent.setup();
    const { props } = renderPicker({ initialText: '/code yaml' });
    const text = screen.getByRole('textbox', { name: 'text' });
    expect(text.tagName).toBe('TEXTAREA');
    expect(text).toHaveAttribute('rows', '1');
    await user.click(text);
    await user.keyboard('name: harbour{Shift>}{Enter}{/Shift}size: 6');
    expect(text).toHaveValue('name: harbour\nsize: 6');
    expect(text).toHaveAttribute('rows', '2');
    await user.keyboard('{Shift>}{Enter}{/Shift}crew: 4');
    expect(text).toHaveValue('name: harbour\nsize: 6\ncrew: 4');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(props.onExecute).toHaveBeenCalledWith({ commandId: 'code', args: { language: 'yaml', text: 'name: harbour\nsize: 6\ncrew: 4' } }));
  });

  it('a failed run reddens no chip, and the ghost chip is never invalid', async () => {
    const onExecute = vi.fn(() => Promise.resolve({ status: 'failure' as const, private: true, message: 'Join a voice channel first.' }));
    const play = renderPicker({ initialText: '/play song', onExecute });
    fireEvent.keyDown(screen.getByRole('combobox', { name: 'query-or-url' }), { key: 'Enter' });
    expect(await screen.findByRole('alert')).toHaveTextContent('Join a voice channel first.');
    expect(screen.getByRole('combobox', { name: 'query-or-url' })).not.toHaveAttribute('aria-invalid');
    play.unmount();
    renderPicker({ initialText: '/pause', onExecute });
    fireEvent.keyDown(screen.getByRole('textbox', { name: 'press ↵ to run' }), { key: 'Enter' });
    expect(await screen.findByRole('alert')).toHaveTextContent('Join a voice channel first.');
    expect(screen.getByRole('textbox', { name: 'press ↵ to run' })).not.toHaveAttribute('aria-invalid');
  });

  it('Enter on an unknown command says it is not a command here', async () => {
    const user = userEvent.setup();
    renderPicker();
    await user.type(draftField(), 'xyz{Enter}');
    expect(screen.getByRole('alert')).toHaveTextContent('/xyz isn’t a command here. To post it as text, start with //.');
    await user.type(draftField(), 'a');
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('Enter on a typed command its Bot does not offer opens no chips', async () => {
    const user = userEvent.setup();
    renderPicker({ listed: new Set(definitions.filter(command => command.id !== 'play').map(command => command.id)) });
    await user.type(draftField(), 'play song{Enter}');
    expect(screen.queryByRole('combobox', { name: 'query-or-url' })).toBeNull();
    expect(screen.getByRole('alert')).toHaveTextContent('/play isn’t a command here.');
  });
});

describe('CommandPicker in CommandHost', () => {
  it('retry keeps requestId', async () => {
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
    await waitFor(() => expect(privateResults(fake, ':', `dm/${channelId}/`).rows().at(-1)?.text).toBe('uncertain'));
    await user.click(screen.getByRole('button', { name: 'send' }));
    await waitFor(() => expect(ids).toHaveLength(2));
    await user.click(screen.getByRole('button', { name: 'toggle host' }));
    await user.click(screen.getByRole('button', { name: 'toggle host' }));
    await user.click(await screen.findByRole('button', { name: 'send' }));
    await waitFor(() => expect(ids).toHaveLength(3));
    expect(new Set(ids).size).toBe(1);
  });
});
