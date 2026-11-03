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
    isAlwaysOnTop: () => false, setAlwaysOnTop: value => calls.push(`setAlwaysOnTop:${value}`), moveTop: () => calls.push('moveTop'),
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

for (const delayedPhase of ['acquisition', 'evaluation']) {
  test(`no stale focus mutation after a delayed ${delayedPhase} outlives the renderer proof`, { timeout: 5000 }, async () => {
    const f = fixture();
    let resolveProof, resolveLate;
    const proof = new Promise(resolve => { resolveProof = resolve; });
    const late = new Promise(resolve => { resolveLate = resolve; });
    let acquisitions = 0, evaluations = 0;
    const mutations = [];
    const target = { isMinimized: () => false, show() { mutations.push('show'); }, focus() { mutations.push('focus'); } };
    f.application.browserWindow = async () => {
      const ordinal = ++acquisitions;
      if (delayedPhase === 'acquisition' && ordinal > 1) await late;
      return {
        async evaluate(fn, argument) {
          evaluations++;
          if (delayedPhase === 'evaluation' && ordinal > 1) await late;
          return fn(target, argument);
        },
        async dispose() {},
      };
    };
    f.page.waitForFunction = async () => proof;
    const pending = focusNativePage(f.application, f.page, facts => f.reports.push(facts), { steal: false });
    // Stay pending past the former default repeat interval, not a test-only override.
    await new Promise(resolve => setTimeout(resolve, 2050));
    resolveProof();
    await pending;
    mutations.push('next-peer-focus');
    resolveLate();
    await new Promise(resolve => setTimeout(resolve, 0));
    assert.deepEqual(mutations, ['show', 'focus', 'next-peer-focus'], 'completed focus work must not steal focus back from the next peer');
    assert.equal(acquisitions, 1, 'there is only one awaited request, not a detached repeat');
    assert.equal(evaluations, 1);
    assert.deepEqual(f.reports, []);
  });
}
