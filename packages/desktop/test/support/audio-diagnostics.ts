/** Test-only observer: never consumes PCM or changes capture/termination policy.
 * Self-contained because Playwright evaluates it in Electron's main process. */
export function installNativeAudioDiagnostics(_electron: unknown, { binary }: { binary: string }): void {
 const childProcess = process.getBuiltinModule('node:child_process');
 const original = childProcess.spawn;
 const started = performance.now();
 const records: { pid?: number; startedMs: number; firstPcmMs?: number; pcmChunks: number; pcmBytes: number; stderr: string; sourcePID: number; stops: { ms: number; signal: string | number; callers: string[] }[]; exit?: { code: number | null; signal: string | null; ms: number }; error?: string }[] = [];
 const scrub = (text: string) => text.replace(/https?:\/\/\S+/gi, '[url]').replace(/(?:[A-Za-z]:[\\/]|\/)[^\s"'<>]*/g, '[path]').slice(0, 4096);
 (globalThis as any).__nativeAudioDiagnostics = records;
 childProcess.spawn = ((file: string, args: string[], options: unknown) => {
  const child = (original as any)(file, args, options);
  if (file !== binary || args?.[0] !== 'capture' || records.length >= 16) return child;
  let stderr = '';
  const record: typeof records[number] = { pid: child.pid, startedMs: performance.now() - started, sourcePID: Number(args[1]?.split(':')[1]), stops: [], pcmChunks: 0, pcmBytes: 0, get stderr() { return scrub(stderr); } };
  records.push(record);
  const kill = child.kill;
  child.kill = function (signal: string | number = 'SIGTERM') {
   if (record.stops.length < 16) record.stops.push({ ms: performance.now() - started, signal, callers: (new Error().stack ?? '').split('\n').slice(2, 6).map(line => /^\s*at ([\w.<>]+)\s/.exec(line)?.[1] ?? '[anonymous]') });
   return kill.call(this, signal);
  };
  child.stdout?.on('data', (data: Buffer) => {
   record.firstPcmMs ??= performance.now() - started;
   record.pcmChunks++; record.pcmBytes += data.length;
  });
  child.stderr?.on('data', (data: Buffer) => { if (stderr.length < 8192) stderr += data.toString('utf8').slice(0, 8192 - stderr.length); });
  child.once('error', (error: Error) => { record.error = scrub(error.message); });
  child.once('exit', (code: number | null, signal: string | null) => { record.exit = { code, signal, ms: performance.now() - started }; });
  return child;
 }) as typeof childProcess.spawn;
 (globalThis as any).__stopNativeAudioDiagnostics = () => { childProcess.spawn = original; };
}
