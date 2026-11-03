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
  await focusNativePage(f.application, f.page, facts => f.reports.push(facts), { steal: false });
  assert.deepEqual(f.calls, ['isMinimized', 'show', 'focus', 'dispose', 'renderer-focus-proof']);
  assert.deepEqual(f.reports, []);
});

test('a stolen focus raises the window on top only for the request and puts the previous state back', async () => {
  const f = fixture({ focus: true });
  const { application, page, calls } = f;
  const window = await application.browserWindow(page);
  // Reach the fake window through the fixture's own evaluate so the steal path sees the extra methods.
  application.browserWindow = async () => ({
    async evaluate(fn, argument) {
      const target = {
        isMinimized: () => { calls.push('isMinimized'); return true; }, restore: () => calls.push('restore'),
        show: () => calls.push('show'), focus: () => calls.push('focus'), moveTop: () => calls.push('moveTop'),
        isAlwaysOnTop: () => { calls.push('isAlwaysOnTop'); return false; },
        setAlwaysOnTop: value => calls.push(`setAlwaysOnTop:${value}`),
      };
      return fn(target, argument);
    },
    async dispose() { calls.push('dispose'); },
  });
  await window.dispose(); calls.length = 0;
  await focusNativePage(application, page, facts => f.reports.push(facts), { steal: true });
  assert.deepEqual(calls, ['isMinimized', 'restore', 'isAlwaysOnTop', 'setAlwaysOnTop:true', 'moveTop', 'show', 'focus',
    'setAlwaysOnTop:false', 'dispose', 'renderer-focus-proof']);
  assert.deepEqual(f.reports, []);
});

test('the request is repeated while the renderer proof is pending and stops once it is settled', async () => {
  const f = fixture();
  let focusRequests = 0;
  const { application, page } = f;
  application.browserWindow = async () => ({
    async evaluate(fn, argument) { return fn({ isMinimized: () => false, show() {}, focus() { focusRequests++; } }, argument); },
    async dispose() {},
  });
  // The OS grants the third request: the renderer proof polls until document focus arrives.
  page.waitForFunction = async () => {
    const started = Date.now();
    while (focusRequests < 3) {
      if (Date.now() - started > 1500) throw new Error('renderer proof timed out');
      await new Promise(resolve => setTimeout(resolve, 5));
    }
  };
  await focusNativePage(application, page, facts => f.reports.push(facts), { steal: false, repeatMs: 20 });
  assert.ok(focusRequests >= 3, `repeated requests: ${focusRequests}`);
  const after = focusRequests;
  await new Promise(resolve => setTimeout(resolve, 100));
  assert.equal(focusRequests, after, 'no request may follow a settled proof');
  assert.deepEqual(f.reports, []);
});

test('a refused or hung repeat never delays or replaces the failed renderer proof', async () => {
  const f = fixture();
  const { application, page, failure } = f;
  let acquisitions = 0;
  application.browserWindow = async () => {
    acquisitions++;
    if (acquisitions === 2) throw new Error('PRIVATE_REFUSED');
    if (acquisitions > 2) return new Promise(() => {});
    return { async evaluate(fn, argument) { return fn({ isMinimized: () => false, show() {}, focus() {} }, argument); }, async dispose() {} };
  };
  page.waitForFunction = async () => { await new Promise(resolve => setTimeout(resolve, 120)); throw failure; };
  const started = Date.now();
  await assert.rejects(focusNativePage(application, page, facts => f.reports.push(facts), { steal: false, repeatMs: 20 }), error => error === failure);
  assert.ok(Date.now() - started < 1000, 'the proof failure must not wait on a hung repeat');
  assert.ok(acquisitions >= 3, `repeats attempted: ${acquisitions}`);
  assert.ok(!JSON.stringify(f.reports).includes('PRIVATE'));
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
