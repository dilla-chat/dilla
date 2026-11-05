import { test } from 'node:test';
import assert from 'node:assert/strict';
import { nativeNotificationId, notificationPermission } from '../src/native-ids.ts';

test('the OS notification permission maps onto the web NotificationPermission', () => {
  assert.equal(notificationPermission('granted'), 'granted');
  assert.equal(notificationPermission('denied'), 'denied');
  assert.equal(notificationPermission('prompt'), 'default');
  assert.equal(notificationPermission('prompt-with-rationale'), 'default');
});

test('a notification tag maps to a stable positive 31-bit native id', () => {
  const tags = ['dilla:message:abc', 'dilla:call:1', 'x', '', 'dilla:' + 'z'.repeat(250)];
  const ids = tags.map(nativeNotificationId);
  for (const id of ids) assert.ok(Number.isInteger(id) && id > 0 && id <= 0x7fffffff, String(id));
  assert.equal(new Set(ids).size, tags.length);
  assert.equal(nativeNotificationId('dilla:message:abc'), ids[0]);
});
