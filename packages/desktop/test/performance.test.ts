import { test } from 'node:test';
import assert from 'node:assert/strict';
// @ts-expect-error release tooling uses JavaScript
import { cpuPercent, parsePs, combinePerformance } from '../scripts/performance-report.mjs';

test('process samples retain OS CPU units and reject missing processes rather than reporting zero', () => {
 const rows = parsePs('  12 1024 01:02.34\n  13 2048 1-02:03:04.50\n');
 assert.deepEqual(rows, [{ pid: 12, rssBytes: 1048576, cpuSeconds: 62.34 }, { pid: 13, rssBytes: 2097152, cpuSeconds: 93784.5 }]);
 assert.equal(cpuPercent([{ pid: 1, cpuSeconds: 10 }], [{ pid: 1, cpuSeconds: 12 }, { pid: 2, cpuSeconds: 1 }], 6000), 50);
 assert.throws(() => cpuPercent([{ pid: 1, cpuSeconds: 10 }], [], 6000), /disappeared/);
 assert.throws(() => parsePs('12 1024 unavailable'), /CPU/);
});

test('performance publication requires paired measurements from the same source and target', () => {
 const sample = { startup: 100, memory: 200, idleCPU: 1, interaction: 5, callAudio: 49, callCPU: 10, callRMS: 0.1, passedThrough: 0 };
 const raw = { format: 1, sourceCommit: 'a'.repeat(40), sourceDirty: false, target: 'linux-x64', samples: [sample, sample, sample], environment: { os: 'test', manifestSHA: '0'.repeat(64) } };
 const browser = { ...raw, mode: 'browser' }, desktop = { ...raw, mode: 'desktop' };
 const report = combinePerformance(browser, desktop);
 assert.deepEqual(report.startup[0], { browser: 100, desktop: 100 });
 assert.equal(report.units.memory, 'MiB aggregate resident set');
 assert.throws(() => combinePerformance(browser, { ...desktop, sourceCommit: 'b'.repeat(40) }), /source/);
 assert.throws(() => combinePerformance(browser, { ...desktop, environment: { manifestSHA: '1'.repeat(64) } }), /bundle/);
 assert.throws(() => combinePerformance(browser, { ...desktop, samples: [] }), /samples/);
 assert.throws(() => combinePerformance(browser, { ...desktop, samples: [{ ...sample, passedThrough: 1 }, sample, sample] }), /plaintext/);
 assert.throws(() => combinePerformance(browser, { ...desktop, samples: [{ ...sample, memory: NaN }, sample, sample] }), /measurement/);
});
