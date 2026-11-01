export function parsePs(text) {
 return text.trim().split('\n').filter(Boolean).map(line => {
  const match = /^\s*(\d+)\s+(\d+)\s+(?:(\d+)-)?(?:(\d+):)?(\d+):(\d+(?:\.\d+)?)\s*$/.exec(line);
  if (!match) throw new Error('Unavailable OS CPU/memory measurement: ' + line);
  const [, pid, rss, days = '0', hours = '0', minutes, seconds] = match;
  return { pid: Number(pid), rssBytes: Number(rss) * 1024, cpuSeconds: Number(days) * 86400 + Number(hours) * 3600 + Number(minutes) * 60 + Number(seconds) };
 });
}

export function cpuPercent(before, after, milliseconds) {
 if (!Number.isFinite(milliseconds) || milliseconds <= 0) throw new Error('Invalid CPU interval');
 const previous = new Map(before.map(row => [row.pid, row.cpuSeconds]));
 for (const row of before) if (!after.some(next => next.pid === row.pid)) throw new Error('A measured process disappeared during the CPU interval');
 let seconds = 0;
 for (const row of after) {
  const delta = row.cpuSeconds - (previous.get(row.pid) ?? 0);
  if (!Number.isFinite(delta) || delta < 0) throw new Error('Invalid cumulative CPU measurement');
  seconds += delta;
 }
 return seconds / (milliseconds / 1000) * 100;
}

export const PERFORMANCE_UNITS = {
 startup: 'ms process launch to ready login', memory: 'MiB aggregate resident set', idleCPU: 'percent of one CPU core',
 interaction: 'ms search pointerdown to painted dialog', callAudio: 'authenticated remote frames per second',
 callCPU: 'percent of one CPU core', callRMS: 'remote decoded linear RMS',
};
export function combinePerformance(browser, desktop) {
 if (browser.mode !== 'browser' || desktop.mode !== 'desktop') throw new Error('Missing browser/desktop measurement pair');
 if (!/^[a-f0-9]{40}$/.test(browser.sourceCommit) || browser.sourceCommit !== desktop.sourceCommit) throw new Error('Performance source mismatch');
 if (typeof browser.sourceDirty !== 'boolean' || typeof desktop.sourceDirty !== 'boolean') throw new Error('Missing performance source status');
 if (!/^[a-f0-9]{64}$/.test(browser.environment?.manifestSHA) || browser.environment.manifestSHA !== desktop.environment?.manifestSHA) throw new Error('Performance UI bundle mismatch');
 if (!['linux-x64', 'win-x64', 'mac-x64', 'mac-arm64'].includes(browser.target) || browser.target !== desktop.target) throw new Error('Performance target mismatch');
 if (!Array.isArray(browser.samples) || browser.samples.length < 3 || browser.samples.length !== desktop.samples?.length) throw new Error('Need at least three paired samples');
 for (const record of [browser, desktop]) for (const sample of record.samples) {
  for (const metric of Object.keys(PERFORMANCE_UNITS)) if (!Number.isFinite(sample[metric]) || sample[metric] < 0) throw new Error('Missing performance measurement: ' + metric);
  if (sample.passedThrough !== 0) throw new Error('Call performance cannot use plaintext fallback');
  if (sample.callAudio <= 0 || sample.callRMS <= 0.005) throw new Error('Call performance requires authenticated audible audio');
 }
 return { format: 1, target: browser.target, sourceCommit: browser.sourceCommit, status: 'measured', sourceDirty: browser.sourceDirty === true || desktop.sourceDirty === true, units: PERFORMANCE_UNITS,
  methodology: 'Three fresh isolated profiles and processes per client, headed, same host and sealed production UI build; Electron runs as an unpackaged sandboxed shell, so installer startup/update checks are outside this comparison. Public Chromium/Electron process inventories and cumulative CPU seconds; OS aggregate RSS; shared resident pages count in each process. Five-second idle and encrypted-call windows after audible authenticated media and 100 sender frames. Windows in which a process exits cannot have complete cumulative counters: discard at most three, publish every discarded snapshot, and fail if no complete window follows. Fake microphones send distinct steady 1000/600 Hz tones with shipping echo cancellation enabled. Search timing begins at real pointerdown and ends after two animation frames of the open dialog. Startup includes acceptance-fixture attachment and initial login readiness; cold disk caches are not controlled. No assertion that Electron is faster.',
  environment: { browser: browser.environment, desktop: desktop.environment },
  ...Object.fromEntries(Object.keys(PERFORMANCE_UNITS).map(metric => [metric, browser.samples.map((sample, i) => ({ browser: sample[metric], desktop: desktop.samples[i][metric] }))])),
  raw: { browser: browser.samples, desktop: desktop.samples } };
}
