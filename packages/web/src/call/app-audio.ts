import { desktopBridge } from '../desktop.ts';
export const APPLICATION_SOURCE = 'dilla:application';
export async function applicationAudioAvailable(): Promise<boolean> {
  try { const bridge = desktopBridge(); return !!bridge?.requestAppAudio && !!bridge.onAppAudio && (await bridge.info()).capabilities.applicationAudio; }
  catch { return false; }
}
export async function acquireApplicationAudio(): Promise<MediaStreamTrack> {
  const bridge = desktopBridge();
  if (!bridge?.requestAppAudio || !bridge.onAppAudio) throw new DOMException('Application capture unavailable', 'NotSupportedError');
  let source: { id: string; label: string } | null = null;
  const earlyEnded = new Set<string>();
  let context: AudioContext | null = null;
  let stopFrames = () => {}, stopEvents = () => {};
  let track: MediaStreamTrack | undefined;
  let closed = false;
  const stop = () => {
    if (closed) return; closed = true; stopFrames(); stopEvents();
    if (source) void bridge.send({ type: 'stop-app-audio', id: source.id }).catch(() => {});
    void context?.close().catch(() => {});
  };
  try {
    // Native helpers can exit before the request reply or while addModule is
    // loading. Subscribe first so those failures cannot become silent tracks.
    stopEvents = bridge.onEvent(event => {
      if (event.type !== 'app-audio-ended') return;
      if (!source) { if (earlyEnded.size < 16) earlyEnded.add(event.id); return; }
      if (event.id === source.id) { stop(); track?.stop(); track?.dispatchEvent(new Event('ended')); }
    });
    source = await bridge.requestAppAudio();
    if (!source) throw new DOMException('Application selection cancelled', 'NotAllowedError');
    if (earlyEnded.has(source.id)) throw new DOMException('Selected application ended', 'AbortError');
    earlyEnded.clear();
    context = new AudioContext({ sampleRate: 48000, latencyHint: 'interactive' });
    await context.audioWorklet.addModule((await import('./app-audio-worklet.ts?worker&url')).default);
    if (closed) throw new DOMException('Selected application ended', 'AbortError');
    const node = new AudioWorkletNode(context, 'dilla-application-audio', { numberOfInputs: 0, numberOfOutputs: 1, outputChannelCount: [2] });
    const destination = context.createMediaStreamDestination(); node.connect(destination);
    track = destination.stream.getAudioTracks()[0];
    if (!track) throw new Error('No application audio track');
    const stopTrack = track.stop.bind(track);
    track.stop = () => { stop(); stopTrack(); };
    stopFrames = bridge.onAppAudio(source.id, samples => { if (!closed) node.port.postMessage(samples, [samples.buffer]); });
    await context.resume();
    if (closed) throw new DOMException('Selected application ended', 'AbortError');
    return track;
  } catch (error) { stop(); track?.stop(); throw error; }
}
