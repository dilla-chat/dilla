import { test } from 'node:test';
import assert from 'node:assert/strict';
import { pendingStartupCommand } from '../src/update-startup-ui.ts';
test('pending installed startup permits only trusted local bootstrap commands, never user/call/draft entry capabilities', () => {
 for (const command of ['boot-ready', 'window-mode', 'desktop-theme', 'guard']) assert.equal(pendingStartupCommand(command), true);
 for (const command of ['badge', 'quit-ack', 'autostart', 'notify', 'notify-ring', 'dismiss-notification', 'stop-app-audio', 'window-menu', 'window-control', 'check-update', 'restart-update', 'browser-auth', 'app-audio', 'manager-connect', 'unknown']) assert.equal(pendingStartupCommand(command), false);
});
