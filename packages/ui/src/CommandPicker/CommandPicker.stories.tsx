import type { ReactNode } from 'react';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { CommandBar, CommandChips, CommandField, CommandList, CommandSuggestions, commandOptionId, type CommandListItem, type CommandSuggestion } from './CommandPicker.tsx';
import { SignalComposer } from '../MessageRow/conversation.fixture.tsx';
import { PhoneLayoutProvider } from '../internal/phoneLayout.ts';
import { CONVERSATION_CONTRAST_NOT } from '../../scripts/prototype-contrast.ts';

/*
 * Wave B Task 2: the command picker as packages/web's CommandPicker hands it to ComposerArea's Composer (`top` holds the
 * popover and the bar, `input` the `/` draft or the chips). The copy is the prototype's (picker-catalog.ts in the web).
 */

const noop = () => {};
const LABELS = { title: 'Commands', hint: '↑↓ move · tab complete · esc close', sendAsText: 'Send as text' };
const HINT = 'tab next field · ↵ run · esc leave';

const ALL: CommandListItem[] = [
  { id: 'help', icon: 'circle-help', name: 'help', args: '[command]', description: 'List commands, or explain one', source: 'built-in' },
  { id: 'me', icon: 'sparkles', name: 'me', args: '<text>', description: 'Post an action in italics', source: 'built-in' },
  { id: 'shrug', icon: 'smile', name: 'shrug', args: '[text]', description: 'Appends ¯\\_(ツ)_/¯ to your message', source: 'built-in' },
  { id: 'play', icon: 'play', name: 'play', args: '<query-or-url>', description: 'Play the first YouTube result, or a link, in your voice channel', source: 'Glenn' },
  { id: 'pause', icon: 'pause', name: 'pause', args: '', description: 'Pause playback', source: 'Glenn' },
  { id: 'resume', icon: 'play', name: 'resume', args: '', description: 'Resume playback', source: 'Glenn' },
  { id: 'skip', icon: 'skip-forward', name: 'skip', args: '', description: 'Skip to the next track', source: 'Glenn' },
  { id: 'stop', icon: 'square', name: 'stop', args: '', description: 'Stop and clear the queue', source: 'Glenn' },
  { id: 'queue', icon: 'list-music', name: 'queue', args: '', description: 'Show what plays next', source: 'Glenn' },
  { id: 'volume', icon: 'volume-2', name: 'volume', args: '<0..100>', description: 'Set playback volume', source: 'Glenn' },
  { id: 'poll create', icon: 'vote', name: 'poll create', args: 'opens a form', description: 'Ask the channel a question', source: 'Glenn' },
  { id: 'remind', icon: 'alarm-clock', name: 'remind', args: '<when> <text>', description: 'A private one-time reminder', source: 'Glenn' },
  { id: 'reminders list', icon: 'bell', name: 'reminders list', args: '', description: 'Open your reminders inbox', source: 'Glenn' },
  { id: 'kick', icon: 'user-x', name: 'kick', args: '<user>', description: 'Remove someone from the server', source: 'built-in' },
  { id: 'ban', icon: 'ban', name: 'ban', args: '<user> [duration] [reason]', description: 'Ban for a while, or for good', source: 'built-in' },
  { id: 'timeout', icon: 'timer', name: 'timeout', args: '<user> <duration>', description: 'Stop someone sending or speaking for a while', source: 'built-in' },
  { id: 'warn', icon: 'triangle-alert', name: 'warn', args: '<user> <reason>', description: 'A private warning, logged in modlog', source: 'built-in' },
];
const FILTERED = ALL.filter(item => item.id.includes('re'));

/** The `/` draft in the composer row while the list is open: the textarea ComposerArea's picker draws. */
function Draft({ value }: { value: string }) {
  return <textarea className="d-composer__input" rows={1} value={value} readOnly aria-label="Command" aria-autocomplete="list" aria-controls="cmd-list" />;
}

function Stage({ children, phone = false }: { children: ReactNode; phone?: boolean }) {
  // The chat column at 1280 × 800 (rail, sidebar and members beside it): the popovers float up into the feed's space.
  return phone
    ? <PhoneLayoutProvider value><div data-layout="phone" style={{ width: 390, paddingTop: 360, background: 'var(--bg)' }}>{children}</div></PhoneLayoutProvider>
    : <div style={{ width: 748, paddingTop: 360, background: 'var(--bg)' }}>{children}</div>;
}

const listStory = (draft: string, items: readonly CommandListItem[], phone = false) => (
  <Stage phone={phone}>
    <SignalComposer value={draft} top={<CommandList id="cmd-list" items={items} activeId={items[0]?.id ?? null} showHint
      labels={{ ...LABELS, noMatch: `No command matches ${draft}` }} query={draft} onActive={noop} onPick={noop} onSendAsText={noop} />}
    input={<Draft value={draft} />} />
  </Stage>
);

interface Chip { label: string; optional?: boolean; value: string; placeholder: string; invalid?: boolean; ghost?: boolean }
const chipsStory = (o: { icon: CommandListItem['icon']; name: string; chips: Chip[]; active: number; bar: { icon: CommandListItem['icon']; text: string; tone: 'info' | 'danger' };
  sugs?: { title: string; items: CommandSuggestion[] }; phone?: boolean }) => (
  <Stage phone={o.phone}>
    <SignalComposer top={<>
      {o.sugs !== undefined ? <CommandSuggestions id="cmd-sugs" title={o.sugs.title} items={o.sugs.items} activeId={o.sugs.items[0]?.id ?? null} onActive={noop} onPick={noop} /> : null}
      <CommandBar icon={o.bar.icon} text={o.bar.text} tone={o.bar.tone} hint={HINT} />
    </>} input={<CommandChips icon={o.icon} name={o.name}>
      {o.chips.map((chip, index) => <CommandField key={chip.label || chip.placeholder} label={chip.label} optional={chip.optional === true} value={chip.value} placeholder={chip.placeholder}
        active={index === o.active} invalid={chip.invalid === true} ghost={chip.ghost} multiline={chip.label === 'text' || chip.label === 'reason'}
        {...(index === o.active && o.sugs !== undefined ? { listId: 'cmd-sugs', activeOptionId: commandOptionId('cmd-sugs', o.sugs.items[0]?.id ?? '') } : {})}
        onChange={noop} onFocus={noop} onKeyDown={noop} />)}
    </CommandChips>} />
  </Stage>
);

const meta = {
  title: 'Conversation/CommandPicker', component: CommandList,
  args: { id: 'cmd-list', items: ALL, activeId: 'help', showHint: true, labels: { ...LABELS, noMatch: '' }, query: '/', onActive: noop, onPick: noop, onSendAsText: noop },
  // Fix round 1: the list's scroll area is out of the tab order on purpose (the field keeps focus; ↑↓ scroll the active row
  // into view), so axe's scrollable-region-focusable is off for it here; the conversation's Ruling Y selectors stay.
  parameters: { layout: 'fullscreen', a11y: { config: { rules: [{ id: 'color-contrast', selector: `*${CONVERSATION_CONTRAST_NOT}` }, { id: 'scrollable-region-focusable', selector: '*:not(.d-cmd-list__scroll)' }] } } },
} satisfies Meta<typeof CommandList>;
export default meta;
type Story = StoryObj<typeof meta>;

/** `/` in an empty composer: every available command, the first one selected. */
export const List: Story = { render: () => listStory('/', ALL) };
/** `/re`: startsWith, then includes. */
export const ListFiltered: Story = { render: () => listStory('/re', FILTERED) };
/** `/xyz`: nothing matches; "Send as text" posts the draft literally. */
export const NoMatch: Story = { render: () => listStory('/xyz', []) };

const WHEN: CommandSuggestion[] = [
  { id: 'in 1 minute', label: 'in 1 minute', sub: 'Fri 30 Oct, 18:01 CET', icon: 'alarm-clock' },
  { id: 'in 10 minutes', label: 'in 10 minutes', sub: 'Fri 30 Oct, 18:10 CET', icon: 'alarm-clock' },
  { id: 'in 1 hour', label: 'in 1 hour', sub: 'Fri 30 Oct, 19:00 CET', icon: 'alarm-clock' },
  { id: 'tonight 20:00', label: 'tonight 20:00', sub: 'Fri 30 Oct, 20:00 CET', icon: 'alarm-clock' },
  { id: 'tomorrow 09:00', label: 'tomorrow 09:00', sub: 'Sat 31 Oct, 09:00 CET', icon: 'alarm-clock' },
];
const MEMBERS: CommandSuggestion[] = [
  { id: 'ada', label: 'Ada', sub: '@ada', avatar: { name: 'Ada' } },
  { id: 'bjorn', label: 'Björn', sub: '@bjorn', avatar: { name: 'Björn' } },
  { id: 'kari', label: 'Kari', sub: '@kari', avatar: { name: 'Kari' } },
  { id: 'ole', label: 'Ole', sub: '@ole', avatar: { name: 'Ole' } },
];
const REMIND = { icon: 'alarm-clock' as const, name: 'remind' };

/** `/remind` with both chips filled, back on the when chip: the bar previews the time ("Reminds you …"). */
export const Fields: Story = { render: () => chipsStory({ ...REMIND, active: 0,
  chips: [{ label: 'when', value: 'tonight 20:00', placeholder: 'in 1 hour' }, { label: 'text', value: 'bring two ropes', placeholder: 'bring two ropes' }],
  bar: { icon: 'alarm-clock', text: 'Reminds you Fri 30 Oct, 20:00 CET', tone: 'info' } }) };
/** `/kick`: the member suggestions above the empty user chip. */
export const FieldSuggestionsMembers: Story = { render: () => chipsStory({ icon: 'user-x', name: 'kick', active: 0,
  chips: [{ label: 'user', value: '', placeholder: '' }],
  bar: { icon: 'user-x', text: 'Remove someone from the server · checked against your roles', tone: 'info' },
  sugs: { title: 'Members', items: MEMBERS } }) };
/** `/remind` with an empty when chip: the five times, each with its preview. */
export const FieldSuggestionsWhen: Story = { render: () => chipsStory({ ...REMIND, active: 0,
  chips: [{ label: 'when', value: '', placeholder: 'in 1 hour' }, { label: 'text', value: '', placeholder: 'bring two ropes' }],
  bar: { icon: 'alarm-clock', text: 'A private one-time reminder', tone: 'info' }, sugs: { title: 'When', items: WHEN } }) };
/** Enter with an empty required chip: the bar says so in --danger and the chip turns red. */
export const FieldError: Story = { render: () => chipsStory({ ...REMIND, active: 0,
  chips: [{ label: 'when', value: '', placeholder: 'in 1 hour', invalid: true }, { label: 'text', value: '', placeholder: 'bring two ropes' }],
  bar: { icon: 'triangle-alert', text: 'when is required', tone: 'danger' } }) };
/** `/pause`: no arguments, one ghost chip. */
export const NoFields: Story = { render: () => chipsStory({ icon: 'pause', name: 'pause', active: 0,
  chips: [{ label: '', value: '', placeholder: 'press ↵ to run', ghost: true }],
  bar: { icon: 'pause', text: 'Pause playback · join voice first', tone: 'info' } }) };
/** A phone: the list without its keyboard hint, inset 10px. */
export const Phone: Story = { render: () => listStory('/', ALL, true), parameters: { phoneViewport: true } };
/** A phone: `/remind`'s chips, no hint. */
export const PhoneChips: Story = { render: () => chipsStory({ ...REMIND, active: 0, phone: true,
  chips: [{ label: 'when', value: '', placeholder: 'in 1 hour' }, { label: 'text', value: '', placeholder: 'bring two ropes' }],
  bar: { icon: 'alarm-clock', text: 'A private one-time reminder', tone: 'info' }, sugs: { title: 'When', items: WHEN } }),
parameters: { phoneViewport: true } };
