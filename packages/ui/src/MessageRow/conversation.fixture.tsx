// Stories only: the Signal conversation as packages/web composes it (screens/conversation/MessageItem.tsx and
// ComposerArea.tsx): the row's props, its toolbar (react, reply, thread, then "more" with edit, pin, delete), its reaction
// bar, the code labels, and the composer's footer (attach, emoji, GIF, </>, the hint) with the reply bar and the tray.
// The words are en.ts's; the people and messages are the prototype's #general (sample data, Task 11).
import { useState, type ReactNode } from 'react';
import { MessageRow, type BodyPart, type MessageRowProps } from './MessageRow.tsx';
import { MessageToolbar } from '../MessageToolbar/MessageToolbar.tsx';
import { ReactionBar } from '../ReactionBar/ReactionBar.tsx';
import { Composer, type ComposerProps } from '../Composer/Composer.tsx';
import { DateDivider, MessageLog } from '../MessageLog/MessageLog.tsx';
import { NowPlaying } from '../NowPlaying/NowPlaying.tsx';
import { npHarbour } from '../NowPlaying/np.fixture.tsx';
import { ThreadPane } from '../ThreadPane/ThreadPane.tsx';
import { THREAD } from '../internal/sample/midgard.ts';

const noop = () => {};

/** en.ts: shell.code.* */
export const CODE_LABELS = { copy: 'Copy', copied: 'Copied', lines: (n: number) => (n === 1 ? `${String(n)} line` : `${String(n)} lines`) };

/** The toolbar MessageItem passes: react, reply, reply in thread, and (own rows) edit, pin and delete under "more". */
export function rowToolbar(own = false): ReactNode {
  return (
    <MessageToolbar label="actions for this message" moreLabel="more actions" tabbable={false} items={[
      { id: 'react', label: 'react', onAction: noop }, { id: 'reply', label: 'reply', onAction: noop },
      { id: 'thread', label: 'reply in thread', onAction: noop },
      ...(own ? [{ id: 'edit' as const, label: 'edit', onAction: noop }] : []),
      { id: 'pin', label: 'pin', onAction: noop },
      ...(own ? [{ id: 'delete' as const, label: 'delete', danger: true, onAction: noop }] : []),
    ]} />
  );
}

/** The reaction bar MessageItem passes. */
export function rowReactions(items: readonly { emoji: string; count: number; mine: boolean; name: string }[]): ReactNode {
  return <ReactionBar label="reactions" tabbable={false} items={items.map(i => ({ ...i, name: `${i.name}, ${String(i.count)}` }))}
    onToggle={noop} addLabel="add a reaction" onAdd={noop} />;
}

/** One row with MessageItem's fixed props. */
export function Row(p: Partial<MessageRowProps> & { author: string; time: string; body: MessageRowProps['body'] }) {
  return <MessageRow state="ok" gutterTime={p.time} toolbar={rowToolbar(p.own)} codeLabels={CODE_LABELS} {...p} />;
}

export const RAID_YAML = 'raid:\n  when: "2026-10-30T20:00"\n  where: the bridge\n  bring:\n    - rope      # two, Mira insists\n    - torches\n    - snacks\n  troll: true\n  max_party: 6';
export const RAID_JSON = '{\n  "raid": {\n    "when": "2026-10-30T20:00",\n    "party": ["Ada", "Björn", "Mira"],\n    "troll": true,\n    "max_party": 6\n  }\n}';
export const yamlBody: BodyPart[] = [{ kind: 'text', text: 'put it in the raid file so nobody argues later:' }, { kind: 'codeblock', lang: 'yaml', text: RAID_YAML }];

/** The prototype's #general (mc:general), as rows of the log, ending with Glenn's "started music" message and its card (Task 8b
 *  fix round 1: MessageItem renders the Bot's marker as that line and MusicMessageCard; Mira is not in harbour, so no control). */
export function GeneralLog({ welcome = true }: { welcome?: boolean }) {
  return (
    <MessageLog label="messages in #general" emptyLabel="No messages yet."
      {...(welcome ? { welcome: { icon: 'hash' as const, title: 'Welcome to #general', body: 'This is the start of #general.' } } : {})}>
      <DateDivider label="today" />
      <Row author="Ada" time="20:27" body="raid tonight at 20:00? bring the good shields" nameColor={0xE9B6EC} />
      <Row author="Björn" time="20:27" body="in. last time the bridge troll took everything we had"
        reactions={rowReactions([{ emoji: '🛡️', count: 2, mine: false, name: 'shield' }, { emoji: '🔥', count: 1, mine: false, name: 'fire' }])}
        thread={{ label: '3 replies', last: 'last 20:33', onOpen: noop }} />
      <Row author="Mira" time="20:28" own body={'I’m in too. Packing list:\nrope\ntorches\nsnacks for Björn'} />
      <Row author="Ada" time="20:29" body="the map from saturday is in #loot, the one with the coffee stain" nameColor={0xE9B6EC} />
      <Row author="Ada" time="20:29" grouped body="see you at the bridge" nameColor={0xE9B6EC} />
      <Row author="Björn" time="20:29" body={yamlBody} />
      <Row author="Glenn" time="20:30" tag="bot" body="Ada started music in harbour" card={<NowPlaying {...npHarbour} canControl={false} />} />
    </MessageLog>
  );
}

/** The composer as ComposerArea passes it (no GIF button: the server has no Giphy key unless `gif`). */
export function SignalComposer(p: Partial<ComposerProps> & { gif?: ComposerProps['gif'] }) {
  const [value, setValue] = useState(p.value ?? '');
  return (
    <Composer label="message #general" placeholder="Message #general" maxLength={4000} sendLabel="send" counterLabel={n => `${String(n)} left`}
      attachLabel="attach files" onAttach={noop} emoji={{ label: 'Emoji', open: false, onToggle: noop }} codeLabel="Code block" hint="⇧↵ new line"
      {...p} value={value} onChange={v => { setValue(v); p.onChange?.(v); }} onSend={() => setValue('')} />
  );
}

/** Task 11: #general's thread on Björn's message (the prototype's t1) as ThreadView.tsx composes it: "Thread", "#general", the
 *  root, "3 replies", the replies and the thread's own composer (ComposerArea with the thread's label). */
export function GeneralThread({ phone = false, onClose = noop }: { phone?: boolean; onClose?: () => void }) {
  return (
    <ThreadPane title="Thread" sub="#general" repliesLabel={THREAD.label} closeLabel="close thread" onClose={onClose} phone={phone}
      root={<Row author={THREAD.root.author} time={THREAD.root.time} body={THREAD.root.body} />}
      composer={<SignalComposer label="Reply in thread" placeholder="Reply in thread" />}>
      <MessageLog label="Thread" emptyLabel="No replies yet.">
        {THREAD.replies.map(r => <Row key={r.time} author={r.author} time={r.time} body={r.body} own={r.author === 'Mira'}
          {...(r.author === 'Ada' ? { nameColor: 0xE9B6EC } : {})} />)}
      </MessageLog>
    </ThreadPane>
  );
}
