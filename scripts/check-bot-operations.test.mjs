import { test } from 'node:test';
import assert from 'node:assert/strict';
import { catalogOperations, check, missingOperations, undefinedCatalogOperations } from './check-bot-operations.mjs';

const go = 'const (\n\tOpA Operation = 0\n\tOpB   Operation = 25\n\tOpC Operation = 26\n\tOpProbe Operation = 90\n)\n';
const catalog = 'var commands = []Command{\n\t{F, "a", "", protocol.OpA, nil},\n\t{F, "b", "", protocol.OpProbe, nil},\n\t{F, "c", "", protocol.OpA, nil},\n}\n\nfunc x() { protocol.OpZ }\n';

test('names every v1 Go operation absent from the client set', () => {
  assert.deepEqual(missingOperations(go, 'const KNOWN_OPERATIONS = new Set([0, 25,\n 30])'), ['26 (OpC)']);
});

test('passes when the client covers every v1 operation; sealed-only operations are the catalog\'s', () => {
  assert.deepEqual(missingOperations(go, 'const KNOWN_OPERATIONS = new Set([0, 25, 26])'), []);
});

test('reads the catalog table only, once per operation', () => {
  assert.deepEqual(catalogOperations(catalog), ['OpA', 'OpProbe']);
});

test('names a catalog operation the codec does not define', () => {
  assert.deepEqual(undefinedCatalogOperations(go, catalog), []);
  assert.deepEqual(undefinedCatalogOperations(go, catalog.replace('OpProbe', 'OpMissing')), ['OpMissing']);
});

test('the real tree is in sync', () => {
  assert.deepEqual(check(), []);
});
