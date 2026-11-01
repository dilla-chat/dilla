import { test } from 'node:test';
import assert from 'node:assert/strict';
import { check, missingOperations } from './check-bot-operations.mjs';

const go = 'const (\n\tOpA Operation = 0\n\tOpB   Operation = 25\n\tOpC Operation = 26\n)\n';

test('names every Go operation absent from the client set', () => {
  assert.deepEqual(missingOperations(go, 'const KNOWN_OPERATIONS = new Set([0, 25,\n 30])'), ['26 (OpC)']);
});

test('passes when the client covers every operation', () => {
  assert.deepEqual(missingOperations(go, 'const KNOWN_OPERATIONS = new Set([0, 25, 26])'), []);
});

test('the real tree is in sync', () => {
  assert.deepEqual(check(), []);
});
