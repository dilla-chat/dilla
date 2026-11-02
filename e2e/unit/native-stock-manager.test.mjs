import { test } from 'node:test';
import { strict as assert } from 'node:assert';
import { readFileSync } from 'node:fs';
import { stockManagerPage } from '../web/support/native-stock-manager.mjs';
const page = url => ({ url: () => url });
const browser = pages => ({ contexts: () => [{ pages: () => pages }] });
test('stock probe recognizes the exact shipped manager protocol in the owned browser context', () => {
 const main = readFileSync(new URL('../../packages/desktop/src/main.ts', import.meta.url),'utf8');
 assert.match(main, /manager\.loadURL\('dilla-internal:\/\/setup\/'\)/);
 const manager = page('dilla-internal://setup/');
 assert.equal(stockManagerPage(browser([page('file:///setup/'), page('dilla-internal://setup/other'), manager])), manager);
 assert.equal(stockManagerPage(browser([page('file:///setup/')])), undefined);
 assert.throws(() => stockManagerPage({ contexts: () => [{ pages: () => [manager] }, { pages: () => [] }] }));
 assert.throws(() => stockManagerPage(browser([manager, manager])));
});
