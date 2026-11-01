import { useEffect, useRef, type ReactNode } from 'react';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { expect, userEvent } from 'storybook/test';
import { Composer } from './Composer.tsx';
import { ReplyChip } from '../ReplyChip/ReplyChip.tsx';
import { AttachmentTray } from '../AttachmentTray/AttachmentTray.tsx';
import { MentionList } from '../MentionList/MentionList.tsx';
import { SignalComposer } from '../MessageRow/conversation.fixture.tsx';
import { TypingIndicator } from '../TypingIndicator/TypingIndicator.tsx';
import { PhoneLayoutProvider } from '../internal/phoneLayout.ts';

// The Composer is controlled (L-UI-13): each story passes the text as `value`. 69 bytes per line, so 53
// lines (less the last space) leave 344 of the 4000-byte budget and 59 lines are 70 bytes over it.
// The counter words a negative remainder as the shell does (shell.composer.over): never a negative "left".
const LINE = 'meet at the harbour after nine, bring the rope and the good lantern. ';
const meta = {
  title: 'Conversation/Composer', component: Composer,
  args: { label: 'message #general', placeholder: 'message #general', maxLength: 4000, value: '', onChange: () => {}, onSend: () => {},
    sendLabel: 'send', counterLabel: (n: number) => (n < 0 ? `${-n} bytes over the limit` : `${n} left`) },
  decorators: [Story => <div style={{ maxWidth: 720 }}><Story /></div>],
} satisfies Meta<typeof Composer>;
export default meta;
type Story = StoryObj<typeof meta>;

// The label shows only while the composer holds focus: this story focuses the field on mount so the
// label (its label part cased as a label, the channel name as given) is visible in the screenshots.
function FocusField({ children }: { children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => { ref.current?.querySelector('textarea')?.focus(); }, []);
  return <div ref={ref}>{children}</div>;
}

export const Empty: Story = {};
export const Disabled: Story = { args: { disabled: true, disabledReason: 'joining this channel' } };
export const NearLimit: Story = { args: { value: LINE.repeat(53).trimEnd() } };
export const OverBudget: Story = { args: { value: LINE.repeat(59).trimEnd() } };
export const Focused: Story = {
  args: { label: 'message #Harbour-Crew', placeholder: 'message #Harbour-Crew' },
  decorators: [Story => <FocusField><Story /></FocusField>],
};
// Product walk I2: at 360 px a 100-byte channel name in one word, in the shown label, is cut with an ellipsis; the
// composer never widens.
export const LongChannelPhone: Story = {
  args: { label: `message #${'x'.repeat(100)}`, placeholder: `message #${'x'.repeat(100)}` },
  decorators: [Story => <div style={{ width: 328 }}><FocusField><Story /></FocusField></div>],
  play: ({ canvasElement }) => {
    const form = canvasElement.querySelector<HTMLElement>('.d-composer');
    const label = canvasElement.querySelector<HTMLElement>('.d-composer__label');
    if (form === null || label === null) throw new Error('LongChannelPhone: no composer');
    form.querySelector('textarea')?.focus();
    const f = form.getBoundingClientRect();
    const l = label.getBoundingClientRect();
    if (form.scrollWidth > form.clientWidth + 1 || l.right > f.right + 1 || document.documentElement.scrollWidth > window.innerWidth + 1) {
      throw new Error(`LongChannelPhone: the composer overflows (form ${String(f.width)}, scroll ${String(form.scrollWidth)}, label ends at ${String(l.right)})`);
    }
  },
};

// ---- web-2b (L-UI-53): every string is L-COPY-03's, quoted (en.ts gains them in task 9). ----
const ATTACH = { attachLabel: 'attach files', onAttach: () => {} };
export const WithReplyChip: Story = {
  args: { ...ATTACH, value: 'see you there',
    top: <ReplyChip label="replying to björn" excerpt="after nine, still on the boat" cancelLabel="cancel reply" onCancel={() => {}} /> },
};
export const WithTray: Story = {
  args: { ...ATTACH, canSendEmpty: true,
    top: <AttachmentTray label="files to send" items={[
      { id: 't0', name: 'drawn.png', size: '48 KB', step: 'uploading', failed: false, phase: 'uploading', removeLabel: 'remove drawn.png', onRemove: () => {} },
      { id: 't1', name: 'notes.txt', size: '2 KB', step: 'ready', failed: false, phase: 'ready', removeLabel: 'remove notes.txt', onRemove: () => {} },
    ]} /> },
};
// The list open while typing `@mi`: the textarea stays a textbox and points at the list (pre-flight ruling F10).
export const WithMentionList: Story = {
  args: { ...ATTACH, value: '@mi',
    combobox: { expanded: true, controls: 'mentions', activeDescendant: 'mention-mira', onKey: () => false },
    top: <MentionList id="mentions" label="people to mention" activeId="mention-mira" onPick={() => {}} options={[
      { id: 'mention-mira', primary: 'mira', secondary: '@mira' }, { id: 'mention-mike', primary: 'Mike Dahl', secondary: '@mike' },
    ]} /> },
};
export const WithAttach: Story = { args: { ...ATTACH } };

// web3.7 (flow 11): the notice sits above the box and never blocks it. The slow-mode line counts down (the caller redraws it);
// an announcement channel tells people who may not post why, while reactions stay open.
export const SlowModeNotice: Story = {
  args: { value: 'one more thing', notice: <span>Slow mode is on. You can send again in 8 seconds.</span> },
};
export const AnnouncementReadOnly: Story = {
  args: { notice: <span>Only some people can post in #news.</span>, label: 'message #news', placeholder: 'message #news' },
};

// web3.9 (G3): with the shell's band set to the user panel's height (Signal's 64px panel, --panel-h), the resting composer is
// exactly that tall, so its top meets the panel's top; focus changes nothing; three lines grow it upward. Measured, not read from CSS.
const BAND = '64px';
const bandDecorator = (Story: () => ReactNode) => <div style={{ maxWidth: 720, ['--d-composer-band' as string]: BAND }}><Story /></div>;
const formHeight = (root: HTMLElement) => (root.querySelector('form') as HTMLElement).getBoundingClientRect().height;
export const BandAtRest: Story = {
  decorators: [bandDecorator],
  play: async ({ canvasElement }) => {
    const rest = formHeight(canvasElement);
    await expect(Math.round(rest)).toBe(64);
    await userEvent.click(canvasElement.querySelector('textarea') as HTMLElement);
    await expect(formHeight(canvasElement)).toBe(rest);
  },
};
export const BandThreeLines: Story = {
  args: { value: 'one\ntwo\nthree' },
  decorators: [bandDecorator],
  play: async ({ canvasElement }) => {
    await expect(formHeight(canvasElement)).toBeGreaterThan(64);
  },
};

// ---- Task 7 (Signal §8): the prototype's composer states, with the props ComposerArea passes (conversation.fixture.tsx).
const signalBox = (Story: () => ReactNode) => <div style={{ maxWidth: 900, paddingTop: 140, background: 'var(--bg)' }}><Story /></div>;
const signalArgs = { globals: { theme: 'signal' }, decorators: [signalBox] };
const GIF = { label: 'add a gif', open: false, onToggle: () => {} };
export const SignalEmpty: Story = { ...signalArgs, render: () => <SignalComposer gif={GIF} /> };
export const SignalFocused: Story = { ...signalArgs, render: () => <FocusField><SignalComposer gif={GIF} /></FocusField> };
export const SignalWithText: Story = { ...signalArgs, render: () => <SignalComposer gif={GIF} value="see you at the bridge" /> };
export const SignalTyping: Story = { ...signalArgs, render: () => <SignalComposer gif={GIF}
  typing={<TypingIndicator names={['Björn']} labels={{ one: '{name} is typing…', many: 'Several people are typing…' }} />} /> };
export const SignalReply: Story = { ...signalArgs, render: () => <FocusField><SignalComposer gif={GIF} replying
  top={<ReplyChip label="Replying to Ada" name="Ada" excerpt="raid tonight at 20:00? bring the good shields" cancelLabel="cancel reply" onCancel={() => {}} />} /></FocusField> };
export const SignalAttachmentsUploading: Story = { ...signalArgs, render: () => <SignalComposer gif={GIF} canSendEmpty
  top={<AttachmentTray label="files to send" items={[
    { id: 't0', name: 'map.png', size: '1.2 MB', step: 'uploading', failed: false, phase: 'uploading', removeLabel: 'remove map.png', onRemove: () => {} },
    { id: 't1', name: 'packing-list.txt', size: '2 KB', step: 'ready', failed: false, phase: 'ready', removeLabel: 'remove packing-list.txt', onRemove: () => {} },
  ]} />} /> };
export const SignalAttachmentTooLarge: Story = { ...signalArgs, render: () => <SignalComposer gif={GIF}
  top={<p className="d-composer__alert" role="alert">raid-video.mkv is over 25 MB and cannot be sent from a browser.</p>} /> };
export const SignalMentioning: Story = { ...signalArgs, render: () => <SignalComposer gif={GIF} value="@b"
  combobox={{ expanded: true, controls: 'mentions', activeDescendant: 'm-bjorn', onKey: () => false }}
  top={<MentionList id="mentions" label="people to mention" title="Members" activeId="m-bjorn" onPick={() => {}} options={[
    { id: 'm-bjorn', primary: 'Björn', secondary: '@bjorn', person: true }, { id: 'b-everyone', primary: '@everyone', secondary: 'everyone in this channel' },
  ]} />} /> };
export const SignalPhone: Story = { globals: { theme: 'signal' }, render: () => (
  <PhoneLayoutProvider value><div data-layout="phone" style={{ width: 390, paddingTop: 40, background: 'var(--bg)' }}><SignalComposer gif={GIF} /></div></PhoneLayoutProvider>
) };
