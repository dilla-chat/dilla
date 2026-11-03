/** Only the validation renderer records call sites; PCM never enters this log. */
export function observeAudioLifecycle(track: MediaStreamTrack) {
 const started = performance.now();
 const events: { type: 'stop' | 'ended'; ms: number; state: MediaStreamTrackState; callers?: string[] }[] = [];
 const stopped = track.stop;
 const record = (type: 'stop' | 'ended', callers?: string[]) => {
  if (events.length < 16) events.push({ type, ms: performance.now() - started, state: track.readyState, ...(callers ? { callers } : {}) });
 };
 const ended = () => record('ended');
 track.addEventListener('ended', ended);
 track.stop = function () {
  // Keep function names only. Native actor paths and instance URLs must not be logged.
  const callers = (new Error().stack ?? '').split('\n').slice(2, 6).map(line => /^\s*at ([\w.<>]+)\s/.exec(line)?.[1] ?? '[anonymous]');
  record('stop', callers);
  return stopped.call(this);
 };
 return {
  events,
  dispose() { track.removeEventListener('ended', ended); track.stop = stopped; },
 };
}
