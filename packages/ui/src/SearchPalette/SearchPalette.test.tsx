import { fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeAll, describe, it, expect, vi } from 'vitest';
import { useState } from 'react';
import { PhoneLayoutProvider } from '../internal/phoneLayout.ts';
import { expectNoAxeViolations } from '../test/setup.ts';
import { SearchPalette } from './SearchPalette.tsx';

type Hit = { id: string; snippet: string; location: string };
/** Task 9 fix round 1: the palette takes groups; the old message hits become one Messages group. */
const g = (rows: readonly Hit[]) => [{ id: 'messages', label: 'Messages', items: rows.map(h => ({ id: h.id, label: h.snippet, sub: h.location, icon: 'message-square' as const })) }];
const hits = g([{ id: 'hit-1', snippet: 'lamp one', location: 'general' }, { id: 'hit-2', snippet: 'lamp two', location: 'loot' }]);
const labels = { search: 'Search channels, people and messages', empty: 'Nothing in this browser matches “absent”.', close: 'Close' };
/** The DOM element the textbox's aria-activedescendant names. */
const activeOption = (input: HTMLElement) => document.getElementById(input.getAttribute('aria-activedescendant') ?? '');

describe('SearchPalette', () => {
  it('keeps textbox focus while arrows and Enter choose a result', async () => {
    const user = userEvent.setup();
    const onPick = vi.fn();
    const view = render(<SearchPalette open query="lamp" groups={hits} labels={labels} onQuery={vi.fn()} onPick={onPick} onClose={vi.fn()} />);
    const input = screen.getByRole('combobox', { name: labels.search });
    input.focus();
    await user.keyboard('{ArrowDown}');
    expect(input).toHaveFocus();
    expect(activeOption(input)).toHaveTextContent('lamp two');
    await user.keyboard('{Enter}');
    expect(onPick).toHaveBeenCalledExactlyOnceWith('hit-2');
    expect(screen.getByRole('listbox').querySelectorAll('[role="option"]')).toHaveLength(2);
    await expectNoAxeViolations(view.container);
  });
  // Task 9 fix round 1: no visible scope (the prototype's palette has none).
  it('announces one empty status, draws no scope control, and closes on Escape', async () => {
    const user = userEvent.setup();
    const onClose = vi.fn();
    render(<SearchPalette open query="absent" groups={[]} labels={labels} onQuery={vi.fn()} onPick={vi.fn()} onClose={onClose} />);
    expect(screen.getAllByRole('status')).toHaveLength(1);
    expect(screen.getByRole('status')).toHaveTextContent(labels.empty);
    expect(screen.queryByRole('button', { name: /this conversation|everything/ })).toBeNull();
    screen.getByRole('combobox', { name: labels.search }).focus();
    await user.keyboard('{Escape}');
    expect(onClose).toHaveBeenCalledOnce();
  });
  it('keeps selection, active descendant, arrows and Enter aligned after hits shrink', async () => {
    const user = userEvent.setup();
    const onPick = vi.fn();
    const props = { open: true, query: 'lamp', labels, onQuery: vi.fn(), onPick, onClose: vi.fn() };
    const view = render(<SearchPalette {...props} groups={hits} />);
    const input = screen.getByRole('combobox', { name: labels.search });
    input.focus();
    await user.keyboard('{ArrowDown}');
    const replacement = g([{ id: 'fresh', snippet: 'new result', location: 'general' }]);
    view.rerender(<SearchPalette {...props} groups={replacement} />);
    expect(activeOption(input)).toHaveTextContent('new result');
    expect(screen.getByRole('option', { name: /new result/ })).toHaveAttribute('aria-selected', 'true');
    await user.keyboard('{Enter}');
    expect(onPick).toHaveBeenCalledExactlyOnceWith('fresh');
    await user.keyboard('{ArrowDown}');
    expect(screen.getByRole('option', { name: /new result/ })).toHaveAttribute('aria-selected', 'true');
  });
  // W-07: Enter and Escape that commit or cancel an IME candidate belong to the IME; the textbox is a combobox.
  it('leaves Enter and Escape to IME composition, and exposes combobox semantics', () => {
    const onPick = vi.fn();
    const onClose = vi.fn();
    render(<SearchPalette open query="lamp" groups={hits} labels={labels} onQuery={vi.fn()} onPick={onPick} onClose={onClose} />);
    const input = screen.getByRole('combobox', { name: labels.search });
    expect(input).toHaveAttribute('aria-expanded', 'true');
    expect(input).toHaveAttribute('aria-autocomplete', 'list');
    fireEvent.keyDown(input, { key: 'Enter', isComposing: true });
    fireEvent.keyDown(input, { key: 'Enter', keyCode: 229 });
    fireEvent.keyDown(input, { key: 'Escape', isComposing: true });
    expect(onPick).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(onPick).toHaveBeenCalledOnce();
  });
  // W-06: Escape closes from any control of the palette, not only the textbox; Close is the input row's esc key cap (Signal §5.8).
  it('closes on Escape from the esc cap, a Close that is the esc cap', async () => {
    const user = userEvent.setup();
    const onClose = vi.fn();
    render(<SearchPalette open query="" groups={[]} labels={labels} onQuery={vi.fn()} onPick={vi.fn()} onClose={onClose} />);
    screen.getByRole('button', { name: labels.close }).focus();
    await user.keyboard('{Escape}');
    expect(onClose).toHaveBeenCalledOnce();
    expect(screen.getByRole('button', { name: labels.close })).toHaveClass('d-search-palette__esc');
    expect(screen.getByRole('button', { name: labels.close })).toHaveTextContent('esc');
  });
  // M-03: one status node stays mounted and only its text changes, so a new message is announced.
  it('keeps one status mounted, with or without results', () => {
    const props = { open: true, query: 'lamp', labels, onQuery: vi.fn(), onPick: vi.fn(), onClose: vi.fn() };
    const view = render(<SearchPalette {...props} groups={hits} />);
    const status = screen.getByRole('status');
    expect(status).toHaveTextContent('');
    view.rerender(<SearchPalette {...props} groups={hits} status="That message is no longer available" />);
    expect(screen.getByRole('status')).toBe(status);
    expect(status).toHaveTextContent('That message is no longer available');
    view.rerender(<SearchPalette {...props} groups={[]} />);
    expect(screen.getByRole('status')).toBe(status);
    expect(status).toHaveTextContent(labels.empty);
  });

  // J1, R-S1: on a desktop the palette is a modal native dialog (focus trapped by the browser, shell inert, ::backdrop scrim); the shell stays mounted.
  it('opens as a modal dialog over the app: showModal is called and the shell stays mounted', () => {
    const modal = vi.spyOn(HTMLDialogElement.prototype, 'showModal');
    const props = { query: '', groups: [], labels, onQuery: vi.fn(), onPick: vi.fn(), onClose: vi.fn() };
    const shell = <main data-testid="shell"><button type="button">channel</button></main>;
    const view = render(<div>{shell}<SearchPalette modal open {...props} /></div>);
    expect(modal).toHaveBeenCalledTimes(1);
    const dialog = screen.getByRole('dialog', { name: labels.search });
    expect(dialog).toHaveClass('d-search-palette-dialog');
    expect(within(dialog).getByRole('search')).not.toHaveAttribute('data-layout');
    expect(screen.getByTestId('shell')).toBeInTheDocument();
    view.rerender(<div>{shell}<SearchPalette modal open={false} {...props} /></div>);
    expect(screen.getByTestId('shell')).toBeInTheDocument();
    expect(screen.queryByRole('dialog')).toBeNull();
    modal.mockRestore();
  });
  it('closes on the native cancel and on a click on the scrim, but not on a click inside', () => {
    const onClose = vi.fn();
    render(<SearchPalette modal open query="" groups={[]} labels={labels} onQuery={vi.fn()} onPick={vi.fn()} onClose={onClose} />);
    const dialog = screen.getByRole('dialog', { name: labels.search });
    fireEvent.click(screen.getByRole('search'));
    expect(onClose).not.toHaveBeenCalled();
    fireEvent.click(dialog);
    expect(onClose).toHaveBeenCalledTimes(1);
    fireEvent(dialog, new Event('cancel', { cancelable: true }));
    expect(onClose).toHaveBeenCalledTimes(2);
  });
  it('a pick that moves focus deliberately is not overridden by the restore (M4)', async () => {
    const user = userEvent.setup();
    function Host() {
      const [open, setOpen] = useState(false);
      return <><button type="button" onClick={() => setOpen(true)}>open search</button><button type="button">message</button>
        <SearchPalette modal open={open} query="" groups={hits} labels={labels} onQuery={vi.fn()}
          onPick={() => { setOpen(false); setTimeout(() => screen.getByRole('button', { name: 'message' }).focus(), 0); }} onClose={() => setOpen(false)} /></>;
    }
    render(<Host />);
    await user.click(screen.getByRole('button', { name: 'open search' }));
    await user.keyboard('{Enter}');
    await new Promise(r => setTimeout(r, 5));
    expect(screen.getByRole('button', { name: 'message' })).toHaveFocus();
  });
  it('focuses the textbox on open, and Escape returns focus to the opener', async () => {
    const user = userEvent.setup();
    function Host() {
      const [open, setOpen] = useState(false);
      return <><button type="button" onClick={() => setOpen(true)}>open search</button>
        <SearchPalette modal open={open} query="" groups={[]} labels={labels} onQuery={vi.fn()} onPick={vi.fn()} onClose={() => setOpen(false)} /></>;
    }
    render(<Host />);
    const opener = screen.getByRole('button', { name: 'open search' });
    await user.click(opener);
    expect(screen.getByRole('combobox', { name: labels.search })).toHaveFocus();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('search')).toBeNull();
    expect(opener).toHaveFocus();
  });
  it('without modal there is no dialog: the inline section, as before (R-S4)', () => {
    const modal = vi.spyOn(HTMLDialogElement.prototype, 'showModal');
    render(<SearchPalette open query="" groups={[]} labels={labels} onQuery={vi.fn()} onPick={vi.fn()} onClose={vi.fn()} />);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.getByRole('search').parentElement?.tagName).not.toBe('DIALOG');
    expect(modal).not.toHaveBeenCalled();
    modal.mockRestore();
  });
  it('a phone layout keeps today’s inline palette (data-layout=phone)', () => {
    render(<PhoneLayoutProvider value><SearchPalette open query="" groups={[]} labels={labels} onQuery={vi.fn()} onPick={vi.fn()} onClose={vi.fn()} /></PhoneLayoutProvider>);
    expect(screen.getByRole('search')).toHaveAttribute('data-layout', 'phone');
  });
  describe('overlay rules', () => {
    let css = '';
    beforeAll(async () => {
      const fs = await import(/* @vite-ignore */ 'node:fs' as string) as { readFileSync(path: string, encoding: 'utf8'): string };
      const cwd = (globalThis as unknown as { process: { cwd(): string } }).process.cwd();
      css = fs.readFileSync(`${cwd}/src/SearchPalette/SearchPalette.css`, 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
    });
    // Task 9 (Signal §5.8): 14vh down, min(600px, 92%) wide, centred by left/right 0 and margin 0 auto (no transform), over the --scrim.
    it('a desktop palette is a modal dialog box: centred without a transform, min(600px, 92%) wide at 14vh, with the scrim', () => {
      const r = /\.d-search-palette-dialog \{([^}]*)\}/.exec(css)?.[1] ?? '';
      expect(r).toMatch(/inset: 14vh 0 auto 0;/);
      expect(r).toMatch(/width: min\(600px, 92%\);/);
      expect(r).toMatch(/margin: 0 auto;/);
      expect(r).not.toMatch(/transform/);
      expect(css).toMatch(/\.d-search-palette-dialog::backdrop \{[^}]*background: var\(--scrim\)/);
    });
  });
  // J1: a result names its place, its author and its time; the footer is the prototype's "↑↓ move  ↵ open … note" (task 9).
  it('shows #channel, author and time on a result under a group heading, and the prototype footer', async () => {
    const withMeta = g([{ id: 'm1', snippet: 'lamp one', location: '#general · Mira · 14:02' }, { id: 'm2', snippet: 'lamp two', location: 'Ada' }]);
    const hinted = { ...labels, hints: { move: 'move', open: 'open' }, note: 'Searches messages held in this browser' };
    const view = render(<SearchPalette open query="lamp" groups={withMeta} labels={hinted} onQuery={vi.fn()} onPick={vi.fn()} onClose={vi.fn()} />);
    const first = screen.getByRole('option', { name: /lamp one/ });
    expect(first).toHaveTextContent('#general');
    expect(first).toHaveTextContent('Mira');
    expect(first).toHaveTextContent('14:02');
    expect(screen.getByRole('option', { name: /lamp two/ })).not.toHaveTextContent('Mira');
    expect(first).toHaveTextContent('#general · Mira · 14:02');
    expect(screen.getByRole('group', { name: 'Messages' })).toBeInTheDocument();
    const foot = view.container.querySelector('.d-search-palette__hints')!;
    expect(Array.from(foot.children).map(k => k.textContent)).toEqual(['↑↓ move', '↵ open']);
    expect(view.container.querySelector('.d-search-palette__note')).toHaveTextContent('Searches messages held in this browser');
    await expectNoAxeViolations(view.container);
  });
});

describe('SearchPalette groups (task 9 fix round 1)', () => {
  const groups = [
    { id: 'channels', label: 'Channels', items: [{ id: 'ch:g', label: 'general', sub: 'Midgard Crew', icon: 'hash' as const }] },
    { id: 'people', label: 'People', items: [{ id: 'dm:a', label: 'Ada', sub: '@ada', avatar: { name: 'Ada' } }] },
    { id: 'empty', label: 'Nothing', items: [] },
    { id: 'actions', label: 'Actions', items: [{ id: 'act:s', label: 'Open settings', icon: 'settings' as const }] },
  ];
  it('draws each non-empty group under its heading, in order, and the arrows move across groups', async () => {
    const user = userEvent.setup();
    const onPick = vi.fn();
    const view = render(<SearchPalette open query="" groups={groups} labels={labels} onQuery={vi.fn()} onPick={onPick} onClose={vi.fn()} />);
    expect(Array.from(view.container.querySelectorAll('.d-search-palette__group')).map(h => h.textContent)).toEqual(['Channels', 'People', 'Actions']);
    expect(screen.getByRole('option', { name: /Ada/ }).querySelector('.d-avatar')).not.toBeNull();
    const input = screen.getByRole('combobox', { name: labels.search });
    input.focus();
    await user.keyboard('{ArrowDown}{ArrowDown}{Enter}');
    expect(onPick).toHaveBeenCalledExactlyOnceWith('act:s');
    await expectNoAxeViolations(view.container);
  });
});

describe('SearchPalette active row (fix round 2)', () => {
  it('keeps the active row by id when results arrive above it, so Enter opens what was chosen', async () => {
    const user = userEvent.setup();
    const onPick = vi.fn();
    const channels = { id: 'channels', label: 'Channels', items: [{ id: 'ch:a', label: 'alpha', icon: 'hash' as const }, { id: 'ch:b', label: 'beta', icon: 'hash' as const }] };
    const props = { open: true, query: 'ro', labels, onQuery: vi.fn(), onPick, onClose: vi.fn() };
    const view = render(<SearchPalette {...props} groups={[channels]} />);
    const input = screen.getByRole('combobox', { name: labels.search });
    input.focus();
    await user.keyboard('{ArrowDown}');
    expect(activeOption(input)).toHaveTextContent('beta');
    // The message search answers: a group lands before the channels.
    view.rerender(<SearchPalette {...props} groups={[...g([{ id: 'm1', snippet: 'rope', location: '#general' }]), channels]} />);
    expect(activeOption(input)).toHaveTextContent('beta');
    await user.keyboard('{Enter}');
    expect(onPick).toHaveBeenCalledExactlyOnceWith('ch:b');
  });
});
