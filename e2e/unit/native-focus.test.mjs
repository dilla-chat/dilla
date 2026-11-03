import assert from 'node:assert/strict';
import test from 'node:test';
import { runInNewContext } from 'node:vm';
import { focusNativePage } from '../web/support/native-focus.ts';

function fixture({ focus = false, unavailable = false, hanging = false } = {}) {
  const calls = [], reports = [];
  const failure = new Error('original focus timeout: PRIVATE_URL_TOKEN');
  const window = {
    privatePath: 'PRIVATE_PATH', title: 'PRIVATE_TITLE',
    isMinimized: () => { calls.push('isMinimized'); return false; },
    restore: () => calls.push('restore'), show: () => calls.push('show'), focus: () => calls.push('focus'),
    isDestroyed: () => false, isFocused: () => true, isVisible: () => true, isFullScreen: () => false,
    webContents: { isDestroyed: () => false, isFocused: () => false, getURL: () => 'PRIVATE_URL_TOKEN' },
  };
  let acquired = 0;
  const application = { async browserWindow() {
    acquired++;
    if (acquired > 1 && hanging) return new Promise(() => {});
    if (acquired > 1 && unavailable) throw new Error('PRIVATE_DIAGNOSTIC_ERROR');
    return { async evaluate(fn) { return fn(window); }, async dispose() { calls.push('dispose'); } };
  } };
  const document = { hasFocus: () => focus, visibilityState: 'visible' };
  const page = {
    async waitForFunction(fn) {
      calls.push('renderer-focus-proof');
      if (!runInNewContext(`(${fn.toString()})()`, { document })) throw failure;
    },
    async evaluate(fn) {
      if (unavailable) throw new Error('PRIVATE_RENDERER_ERROR');
      return runInNewContext(`(${fn.toString()})()`, { document });
    },
  };
  return { application, page, failure, calls, reports };
}

test('native focus still requests the real window and requires document focus without diagnostics on success', async () => {
  const f = fixture({ focus: true });
  await focusNativePage(f.application, f.page, facts => f.reports.push(facts));
  assert.deepEqual(f.calls, ['isMinimized', 'show', 'focus', 'dispose', 'renderer-focus-proof']);
  assert.deepEqual(f.reports, []);
});

test('a failed renderer focus proof reports only allowlisted native facts and rethrows the exact original error', async () => {
  const f = fixture();
  await assert.rejects(focusNativePage(f.application, f.page, facts => f.reports.push(facts)), error => error === f.failure);
  assert.equal(f.reports.length, 1, "the failed focus proof must produce bounded evidence");
  const facts = JSON.parse(JSON.stringify(f.reports[0]));
  assert.deepEqual(facts, { native: { available: true, destroyed: false, focused: true, visible: true,
    minimized: false, fullScreen: false, webContentsDestroyed: false, webContentsFocused: false },
    renderer: { available: true, hasFocus: false, visibility: 'visible' }, timedOut: false });
  assert.ok(!JSON.stringify(f.reports).includes('PRIVATE'));
});

test('unavailable diagnostics and a throwing reporter cannot replace the original failed focus proof', async () => {
  const f = fixture({ unavailable: true });
  await assert.rejects(focusNativePage(f.application, f.page, facts => {
    f.reports.push(facts); throw new Error('PRIVATE_REPORTER_ERROR');
  }), error => error === f.failure);
  assert.deepEqual(f.reports, [{ native: { available: false }, renderer: { available: false }, timedOut: false }]);
});

test('a hung native diagnostic cannot hold the original focus failure indefinitely', { timeout: 2000 }, async () => {
  const f = fixture({ hanging: true });
  await assert.rejects(focusNativePage(f.application, f.page, facts => f.reports.push(facts)), error => error === f.failure);
  assert.equal(f.reports.length, 1, "the bounded probe must report before rethrowing");
  assert.equal(f.reports[0].timedOut, true);
  assert.deepEqual(f.reports[0].native, { available: false });
  assert.equal(f.reports[0].renderer.available, true);
});
