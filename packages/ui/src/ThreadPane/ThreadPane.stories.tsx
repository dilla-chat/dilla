import { useState, type CSSProperties, type ReactNode } from 'react';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { ThreadPane } from './ThreadPane.tsx';
import { AppShell } from '../AppShell/AppShell.tsx';
import { MessageRow } from '../MessageRow/MessageRow.tsx';
import { MessageLog } from '../MessageLog/MessageLog.tsx';
import { Composer } from '../Composer/Composer.tsx';
import { ChannelList } from '../ChannelList/ChannelList.tsx';
import { ChannelHeader } from '../ChannelHeader/ChannelHeader.tsx';
import { CommunityRail } from '../CommunityRail/CommunityRail.tsx';

// A public thread: the root, its replies and its own composer. Strings are flow 11's (strings/thread.ts), as en.ts holds
// them. Switch the theme in the toolbar (signal, light, high-contrast).
const meta = {
  title: 'Conversation/ThreadPane', component: ThreadPane, parameters: { layout: 'fullscreen' },
} satisfies Meta<typeof ThreadPane>;
export default meta;
type Story = StoryObj<typeof meta>;

const noop = () => {};

const root = <MessageRow author="ada" time="21:02" body="anyone up for a round tonight? the second boss hits hard, so bring the tank gear" state="ok" thread={{ label: '3 replies', onOpen: noop }} />;
const missingRoot = <p style={{ margin: 0, padding: '0.75rem var(--row-pad-x)', fontSize: 'var(--text-sm)', fontStyle: 'italic', color: 'var(--fg-3)' }}>The message that started this thread cannot be shown here.</p>;
const replies = (
  <MessageLog label="replies" emptyLabel="No replies yet.">
    <MessageRow author="björn" time="21:03" body="after nine, still on the boat" state="ok" tag="web" active />
    <MessageRow author="mira" time="21:04" body="count me in" state="ok" own />
    <MessageRow author="ada" time="21:06" body="bring the tank gear, then" state="ok" />
  </MessageLog>
);
const composer = (
  <Composer label="Reply in thread" placeholder="Reply in thread" maxLength={4000} value="" onChange={noop} onSend={noop} sendLabel="send" counterLabel={n => `${n} left`} />
);

const column: CSSProperties = { width: '20rem', height: 640, background: 'var(--surface)', border: '1px solid var(--hairline)' };
const Column = ({ children }: { children: ReactNode }) => <div style={{ ...column, margin: 16 }}>{children}</div>;
const phoneFrame: CSSProperties = { width: 390, height: 844, margin: 0, position: 'relative' };

export const Aside: Story = {
  args: { title: 'Thread', sub: '#general', repliesLabel: '3 replies', closeLabel: 'close thread', onClose: noop, root, children: replies, composer },
  render: args => <Column><ThreadPane {...args} /></Column>,
};
export const AsideMissingRoot: Story = {
  args: { ...Aside.args, root: missingRoot },
  render: args => <Column><ThreadPane {...args} /></Column>,
};
export const AsideEmpty: Story = {
  args: { ...Aside.args, children: <MessageLog label="replies" emptyLabel="No replies yet.">{null}</MessageLog> },
  render: args => <Column><ThreadPane {...args} /></Column>,
};
export const Sheet: Story = {
  args: { ...Aside.args, phone: true },
  render: args => <div style={{ ...phoneFrame, background: 'var(--surface)' }}><ThreadPane {...args} /></div>,
};
export const SheetMissingRoot: Story = {
  args: { ...Aside.args, phone: true, root: missingRoot },
  render: args => <div style={{ ...phoneFrame, background: 'var(--surface)' }}><ThreadPane {...args} /></div>,
};

const rail = (
  <CommunityRail label="servers" activeId="a1" onSelect={noop} addLabel="add a server" onAdd={noop} settingsLabel="settings" onSettings={noop}
    itemLabel={i => i.name} items={[{ id: 'a1', name: 'Midgard Crew' }, { id: 'b2', name: 'Night Owls' }]} />
);
const sidebar = (
  <ChannelList label="channels" title="Midgard Crew" activeId="g" onSelect={noop} emptyLabel="No channels yet."
    channels={[
      { id: 'n', name: 'news', kind: 'announcement', readable: false },
      { id: 'g', name: 'general', kind: 'text', readable: false, badge: 'slow mode' },
      { id: 'v', name: 'longhouse', kind: 'voice', readable: false },
    ]} />
);
const log = (
  <MessageLog label="messages in #general" emptyLabel="No messages yet.">
    <MessageRow author="ada" time="21:02" body="anyone up for a round tonight? the second boss hits hard, so bring the tank gear" state="ok" thread={{ label: '3 replies', onOpen: noop }} />
    <MessageRow author="mira" time="21:08" body="lfg for the weekend raid" state="ok" own />
  </MessageLog>
);
const mainComposer = (
  <Composer label="message #general" placeholder="message #general" maxLength={4000} value="" onChange={noop} onSend={noop} sendLabel="send" counterLabel={n => `${n} left`} />
);

/** The thread in the desktop shell's aside (a column from 64rem of shell width), in place of the member list. */
export const InDesktopShell: Story = {
  args: Aside.args,
  render: args => {
    const [open, setOpen] = useState(true);
    return (
      <AppShell skipLabel="skip to content" rail={rail} sidebar={sidebar} header={<ChannelHeader name="general" />} composer={mainComposer}
        aside={<ThreadPane {...args} onClose={() => setOpen(false)} />} asideLabel="thread" asideOpen={open} onAsideClose={() => setOpen(false)}>
        {log}
      </AppShell>
    );
  },
};
