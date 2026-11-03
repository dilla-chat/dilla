import assert from 'node:assert/strict';
import { test } from 'node:test';
import { JSDOM } from 'jsdom';
import { runAxe } from '../web/support/axe.ts';

// Actual DOM selector matching around the shipping filter. Engine findings are
// controlled inputs; a separate real-browser axe reproduction verifies the pair.
const targets = ['#stats', '#stats-visible', '#stats-unmarked', '#stats-deep', '#neighbor', '#gutter-child'];
async function findings(theme = 'signal', rule = 'color-contrast') {
  const dom = new JSDOM(`<!doctype html><html data-theme="${theme}"><body>
    <div id="stats" class="d-voice-dock__stats">
      <span id="stats-visible" aria-hidden="true">24 ms</span><span id="stats-unmarked">Other text</span>
      <span aria-hidden="true"><em id="stats-deep">Different descendant</em></span>
    </div><p id="neighbor">Nearby text</p><div class="d-message-row__gutter"><span id="gutter-child">Time</span></div>
    </body></html>`, { runScripts: 'outside-only' });
  dom.window.axe = { run: async () => ({ violations: [{ id: rule, impact: 'serious', help: 'controlled finding',
    nodes: [...targets.map(target => ({ target: [target] })), { target: ['#stats-visible', '#shadow-descendant'] }] }] }) };
  const page = { async evaluate(fn, arg) {
    dom.window.argument = arg;
    return dom.window.eval(`(${fn.toString()})(window.argument)`);
  } };
  try { return await runAxe(page); } finally { dom.window.close(); }
}
const targetList = results => results.flatMap(result => result.targets);

test('the signal dock exception covers only the approved parent and direct visible stats text', async () => {
  assert.deepEqual(targetList(await findings()), ['#stats-unmarked', '#stats-deep', '#neighbor', '#gutter-child', '#stats-visible #shadow-descendant']);
});
for (const theme of ['light', 'high-contrast']) {
  test(`${theme} keeps all contrast findings`, async () => {
    assert.deepEqual(targetList(await findings(theme)), [...targets, '#stats-visible #shadow-descendant']);
  });
}
test('other rules remain strict even on the exact approved stats elements', async () => {
  assert.deepEqual(targetList(await findings('signal', 'button-name')), [...targets, '#stats-visible #shadow-descendant']);
});
