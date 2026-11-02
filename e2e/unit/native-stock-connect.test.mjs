import { test } from 'node:test';
import { strict as assert } from 'node:assert';
import { selectConnectDialog } from '../web/support/native-stock-connect.mjs';
const title = '_NET_WM_NAME(UTF8_STRING) = "Connect to a Dilla instance"\n_NET_WM_PID(CARDINAL) = 1234\n';
test('native Connect helper targets only the exact dialog owned by the installed app', () => {
 assert.equal(selectConnectDialog([{ id: '0x123', properties: title }], 1234), '0x123');
 assert.equal(selectConnectDialog([{ id: '0x123', properties: title }], 1235), undefined);
 assert.equal(selectConnectDialog([{ id: '0x123', properties: title.replace('Connect to a Dilla instance', 'Dilla permission') }], 1234), undefined);
 assert.equal(selectConnectDialog([{ id: '0x123', properties: title.replace('1234', '12345') }], 1234), undefined);
 assert.throws(() => selectConnectDialog([{ id: '0x123', properties: title }, { id: '0x456', properties: title }], 1234));
 assert.throws(() => selectConnectDialog([{ id: '0x123;evil', properties: title }], 1234));
});
