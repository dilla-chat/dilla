// @vitest-environment node
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
// @ts-expect-error the copy lint is a plain .mjs script without types
import { FORBIDDEN } from '../../../../scripts/check-ui-copy.mjs';
import { en } from './en.ts';
import { t, formatTime, formatDay } from './index.ts';

describe('t', () => {
  it('fills placeholders and ignores extra variables', () => {
    expect(t('boot.error.detail', { code: 'E_NETWORK', unused: 1 })).toBe('Error E_NETWORK. Reload the page to try again.');
  });
  it('throws when a placeholder has no variable', () => {
    expect(() => t('boot.error.detail')).toThrow('strings: boot.error.detail needs {code}');
  });
  it('returns a value without placeholders unchanged', () => {
    expect(t('boot.loading.status')).toBe(en['boot.loading.status']);
  });
});

describe('the copy rules', () => {
  const values = Object.entries<string>(en);
  it('has no empty value and only one-word placeholders', () => {
    for (const [key, value] of values) {
      expect(value.trim(), key).not.toBe('');
      for (const m of value.matchAll(/\{([^}]*)\}/g)) expect(m[1], key).toMatch(/^[a-zA-Z]+$/);
    }
  });
  it('never exclaims and never reassures', () => {
    for (const [key, value] of values) {
      expect(value, key).not.toMatch(/!|\bsecure\b|\bsafe\b|\bprotected\b|\u{1F512}/iu);
    }
  });
  it('keeps the boot truth rule of L-COPY-01 and names the dialog close button', () => {
    expect(en['boot.revoked.detail']).toBe('Your account no longer accepts this browser. Ask the host if you did not expect this. To start over here, clear this site’s data in the browser’s settings.');
    expect(en['boot.storeLost.detail']).not.toMatch(/recovery/i);
    expect(en['dialog.close']).toBe('Close');
  });
  it('says what the recovery key does now, and promises no history on a second browser (L-COPY-02)', () => {
    expect(en['onboarding.keys.loss']).toBe('This key is the only way to get your account back or to add another browser. If every browser you use loses its data and you do not have the key, the account and its history are gone, and the host cannot bring them back.');
    expect(en['onboarding.browser.oneBrowser']).toBe('To use this account in another browser, sign in there with your password and the second factor you add next, then this recovery key. Messages sent before that browser joins are not shown in it.');
    expect(en['signin.done.body']).toBe('This browser is now a device of {username} on {instance}. Messages sent before now are not shown here.');
    expect(en['signin.step']).toBe('Sign in · step {n} of {total}');
    expect(en['signin.error.totpFailed']).toBe('That code did not work. Sign in again with a fresh code.');
    expect(en['signin.error.required']).toBe('Fill in this field.');
    expect(en['signin.error.evicted']).toBe('Someone else is signing in to this account. Change your password from a device you still have, or ask the operator.');
    expect(Object.keys(en).filter(k => k.startsWith('signin.'))).toHaveLength(34);
  });
  // Flow 03 as task 15 froze it (L-COPY-02): the count line has two forms, `devices.cap.one` and `devices.cap.other`, in
  // place of the one `devices.cap`, so the settings rows are 62; the two buttons under the list are lower-case chrome.
  it('names settings in lower case in the chrome and sentence case on its surfaces (Q14)', () => {
    for (const k of ['settings.nav.devices', 'settings.nav.notifications', 'settings.nav.appearance', 'devices.revoke', 'devices.rename', 'devices.refresh',
      'devices.signOut', 'devices.forget', 'notify.channel.default', 'notify.mute', 'appearance.theme.contrast',
      'notify.permission.label', 'notify.permission.ask', 'notify.default.label', 'appearance.theme.label'] as const) expect(en[k], k).toMatch(/^[A-Z#]/); // Signal task 10: sentence case
    for (const k of ['settings.title', 'devices.title', 'devices.revokeTitle', 'devices.removeUnlistedTitle', 'devices.signOutTitle',
      'devices.forgetTitle', 'devices.keyLabel', 'notify.title', 'appearance.title', 'notify.channels.title'] as const) expect(en[k], k).toMatch(/^[A-Z]/);
    expect(en['notify.title.channel']).toBe('#{channel} · {server}');
    expect(en['devices.cap.other']).toBe('{n} devices');
    expect(en['devices.cap.one']).toBe('One device');
    // EOD E7: a device can be named; the body says who can read the name.
    expect(en['devices.renameBody']).toBe('The server can change it, so check a device by its id before you remove it.');
    expect(en['devices.renameSuggestion']).toBe('{browser} on {os}');
    expect(en['settings.error.other']).toBe('That did not work ({code}). Try again.');
    // BACKUPS-RECOVERY-04 adds devices.rootMismatch.
    expect(en['devices.rootMismatch']).toBe('The recovery data stored for this account on {instance} is not this account’s. The recovery key will not work until the operator resets it.');
    // Flow 15 (change password) adds 23: settings.password.*.
    // Flow 06 (web-3) adds 19: settings.nav.voice, settings.voice.* (15), settings.notify.sounds and settings.sound.voice / .ring.
    expect(Object.keys(en).filter(k => /^(settings|devices|notify|appearance)\./.test(k))).toHaveLength(154); // fix round 2 adds devices.subOwn and devices.subOther; flow 17 adds settings.nav.profile, EOD E7 adds six devices.rename*, web3.12.1 devices.renameServerReads; Signal task 10 fix round 1 adds the twelve notify.* of the prototype's rows and drops settings.voice.automaticBeta; task 10b drops the nine strings of the record-and-play test and the input meter (Let's check is signalSettings.voice.*)
    expect(Object.keys(en)).not.toContain('devices.error.other');
    expect(Object.keys(en)).not.toContain('devices.cap');
  });
  it('keeps the shell chrome in lower case and names badges by count (L-COPY-02)', () => {
    for (const k of ['shell.rail.settings', 'shell.tabs.label', 'shell.tabs.channels', 'shell.tabs.dms', 'shell.dms.label', 'shell.dms.empty',
      'shell.dms.new', 'shell.dms.open', 'shell.status.devices', 'shell.dm.log', 'shell.dm.composer'] as const) expect(en[k], k).toMatch(/^[a-z]/);
    expect(en['shell.channels.rowLabel']).toBe('{name}, unread {unread}, mentions {mentions}');
    expect(en['shell.channels.rowLabelMuted']).toBe('{name}, muted, mentions {mentions}');
    expect(en['shell.rail.itemLabel']).toBe('{name}, unread {unread}, mentions {mentions}');
    expect(en['shell.dm.log']).toBe('messages with {name}');
    // Pre-flight ruling (rows 1.10, loop note 15): `@` before the name, so the composer keeps the name's case (web-1's label rule).
    expect(en['shell.dm.composer']).toBe('message @{name}');
    // Flow 07 and 04 add the start-server, empty-DM, empty-log and GIF copy; Bot moderation adds recovery copy.
    // The later GIF attachment label adds one more shell key.
    // Task 6b fix round 1 (Ruling Z): the DM header topic adds shell.dm.topic, topicHandle, topicGroup and you.
    // Task 7 (Signal §5.5, §6.4, §6.8): the toolbar's "more", the code block's four, the composer's five, the welcome's three
    // and the tray's remove tooltip. shell.composer.attach is the prototype's sentence-case tooltip (fix round 1).
    // Task 11: the DM composer's placeholder, the prototype's 'Message {name}' (the label keeps 'message @{name}').
    expect(en['shell.dm.placeholder']).toBe('Message {name}');
    // Task 11 (Jonas, Q2): the DM list's head: shell.dms.title, section, online and count.
    expect(Object.keys(en).filter(k => k.startsWith('shell.'))).toHaveLength(218);
  });
  it('holds the conversation copy of L-COPY-03: chrome lower case, dialogs and refusals sentence case', () => {
    expect(Object.keys(en).filter(k => k.startsWith('shell.emoji.'))).toHaveLength(33);
    for (const k of ['shell.message.toolbar', 'shell.message.react', 'shell.message.edited', 'shell.message.replyJump', 'shell.message.saving',
      'shell.emoji.label', 'shell.edit.label', 'shell.edit.hint', 'shell.composer.mentions', 'shell.pins.open',
      'shell.pins.by', 'shell.tray.label', 'shell.tray.ready', 'shell.attachment.open', 'shell.attachment.unnamed'] as const) {
      expect(en[k], k).toMatch(/^[a-z]/);
    }
    for (const k of ['shell.delete.title', 'shell.delete.body', 'shell.delete.confirm', 'shell.delete.cancel', 'shell.pins.title', 'shell.pins.titleDm',
      'shell.pins.empty', 'shell.pins.close', 'shell.drop.title', 'shell.drop.body', 'shell.tray.tooMany', 'shell.tray.notReady',
      'shell.lightbox.close', 'shell.lightbox.save', 'shell.lightbox.previous', 'shell.lightbox.next', 'shell.message.replyNotLoaded', 'shell.tray.failedHint', 'shell.message.attachmentGone'] as const) {
      expect(en[k], k).toMatch(/^[A-Z]/);
    }
    expect(en['shell.message.reaction']).toBe('{name}, {count}');
    expect(en['shell.delete.body']).toBe('It is removed for everyone in this conversation, with its reactions and files. Copies someone already saved stay with them. This cannot be undone.');
    expect(en['shell.message.replyMissing']).toBe('the original message cannot be shown here');
    expect(en['shell.tray.failedHint']).toBe('Remove it and attach it again.');
    expect(en['shell.message.attachmentGone']).toBe('A file of this message is no longer on the server. Discard it and attach the file again.');
    expect(Object.keys(en)).not.toContain('shell.composer.mentionHere');
    expect(en['shell.drop.body']).toBe('Up to 4 files, 25 MB each.');
    expect(en['shell.tray.tooLarge']).toBe('{name} is over 25 MB and cannot be sent from a browser.');
    expect(en['shell.pins.by']).toBe('pinned by {name}');
  });

});

describe('flow 06 (L-COPY-05)', () => {
  const doc = readFileSync(fileURLToPath(new URL('../../../../docs/design/flows/06-voice.md', import.meta.url)), 'utf8');
  const copy = doc.slice(doc.indexOf('## Copy'));
  const pairs = [...copy.matchAll(/`([a-zA-Z][\w.]*)='([^`]*)'`/g)].map(m => [m[1], m[2]] as const);
  it('strings: every flow-06 copy key equals en.ts byte for byte', () => {
    expect(pairs.length).toBeGreaterThanOrEqual(98);
    const table = en as Readonly<Record<string, string>>;
    for (const [key, value] of pairs) expect(table[key], key).toBe(value);
    for (const [key, value] of pairs) {
      for (const f of FORBIDDEN as { word: string; re: RegExp }[]) expect(f.re.test(value), `${key}: ${f.word}`).toBe(false);
    }
  });
  it('late rejoin (2026-10-21): a device gone longer than the AFK timeout is told it left, and the flow lists the line', () => {
    expect(en['voice.leftWhileAway']).toBe('You left the call while away.');
    expect(pairs.find(([k]) => k === 'voice.leftWhileAway')?.[1]).toBe('You left the call while away.');
  });
  it('names the joined voice row for assistive technology (MP6)', () => {
    expect(en['voice.row.joined']).toBe('connected');
  });
  it('fills each placeholder the flow names and keeps chrome lower case', () => {
    expect(en['voice.paused'].match(/\{name\}/g)).toHaveLength(2);
    for (const k of ['voice.join', 'voice.mute', 'voice.leave', 'call.ring.answer', 'call.ring.decline', 'media.save', 'voice.audioStart'] as const) expect(en[k], k).toMatch(/^[a-z]/);
    // Signal task 10: the settings chrome is sentence case.
    for (const k of ['settings.nav.voice', 'settings.notify.sounds', 'settings.sound.voice', 'settings.sound.ring'] as const) expect(en[k], k).toMatch(/^[A-Z]/);
  });
  it('MP5: the dock names the channel and shows ping and rates; the flow lists the eight keys, and "voice connected" is gone', () => {
    const table = en as Readonly<Record<string, string>>;
    const expected = {
      'voice.dock.ping': '{ms} ms', 'voice.dock.pingSub': '<1 ms', 'voice.dock.pingNone': '— ms', 'voice.dock.rate': '↑ {up} ↓ {down} kb/s',
      'voice.dock.stats': 'Ping {ms} milliseconds, {quality}. Sending {up}, receiving {down} kilobits per second.',
      'voice.dock.quality.good': 'good', 'voice.dock.quality.poor': 'poor', 'voice.dock.quality.bad': 'bad',
    };
    for (const [key, value] of Object.entries(expected)) {
      expect(table[key], key).toBe(value);
      expect(pairs.find(([k]) => k === key)?.[1], `${key} in flow 06`).toBe(value);
    }
    expect(table['layout.dock.connected']).toBeUndefined();
    expect(table['voice.dock.connected']).toBeUndefined();
    expect(Object.values(table)).not.toContain('voice connected');
  });
});

describe('flow 07 (v0.1.0-web3.1, the create flow)', () => {
  const doc = readFileSync(fileURLToPath(new URL('../../../../docs/design/flows/07-create.md', import.meta.url)), 'utf8');
  const copy = doc.slice(doc.indexOf('## Copy'));
  const pairs = [...copy.matchAll(/`([a-zA-Z][\w.]*)='([^`]*)'`/g)].map(m => [m[1], m[2]] as const);
  it('strings: every flow-07 copy key equals en.ts byte for byte, and every new key is in the flow', () => {
    const table = en as Readonly<Record<string, string>>;
    for (const [key, value] of pairs) expect(table[key], key).toBe(value);
    for (const [key, value] of pairs) {
      for (const f of FORBIDDEN as { word: string; re: RegExp }[]) expect(f.re.test(value), `${key}: ${f.word}`).toBe(false);
    }
    const listed = new Set(pairs.map(([key]) => key));
    const added = Object.keys(en).filter(k => /^(create|invite)\./.test(k) || ['shell.noServer.create', 'shell.noServer.body',
      'shell.noServer.action', 'shell.dms.noServer', 'shell.dms.alone'].includes(k));
    expect(added.filter(k => !listed.has(k))).toEqual([]);
  });
  it('names the invite constants in the Copy table and keeps the chrome lower case', () => {
    expect(copy).toContain('`INVITE_MAX_USES=10`');
    expect(copy).toContain('`INVITE_TTL_DAYS=7`');
    expect(en['invite.copied']).toBe('Invite link copied.');
    for (const k of ['create.rail.new', 'create.rail.newLabel', 'create.rail.join', 'create.channel.open', 'create.channel.button',
      'invite.open', 'invite.copy', 'create.channel.text', 'create.channel.voice', 'create.actions'] as const) expect(en[k], k).toMatch(/^[a-z]/);
  });
});

describe('flow 08 (v0.1.0-web3.4, server admin and moderation)', () => {
  it('keeps the admin and members chrome lower case', () => {
    // Task 9 (Rulings T/AA): the members head and the card's Message follow the prototype's case.
    for (const k of ['members.menu.compare', 'members.menu.kick', 'members.menu.ban',
      'admin.open', 'admin.nav.overview', 'admin.nav.channels', 'admin.nav.invites', 'admin.nav.bans', 'admin.nav.delete', 'admin.nav.leave', 'admin.nav.transfer',
      'admin.channels.edit', 'admin.channels.up', 'admin.channels.down', 'admin.channels.delete', 'admin.channels.newCategory',
      'admin.invites.new', 'admin.invites.revoke',
      'admin.bans.unban',
      'admin.leave.open', 'admin.channel.open',
      'invite.options'] as const) expect(en[k], k).toMatch(/^[a-z]/);
  });
});

describe('dates', () => {
  it('formats unix seconds, not milliseconds', () => {
    const time = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit' });
    const day = new Intl.DateTimeFormat(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
    expect(formatTime(18_420)).toBe(time.format(new Date(18_420_000)));
    expect(formatTime(18_420)).not.toBe(time.format(new Date(18_420)));
    expect(formatDay(1_790_000_000)).toBe(day.format(new Date(1_790_000_000_000)));
  });
});
