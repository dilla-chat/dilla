import { desktopBridge } from '../desktop.ts';
export const APPLICATION_SOURCE = 'dilla:application';
export async function applicationAudioAvailable(): Promise<boolean> {
  try { const bridge = desktopBridge(); return !!bridge?.requestAppAudio && !!bridge.onAppAudio && (await bridge.info()).capabilities.applicationAudio; }
  catch { return false; }
}
export async function acquireApplicationAudio(): Promise<MediaStreamTrack> {
  const bridge = desktopBridge();
  if (!bridge?.requestAppAudio || !bridge.onAppAudio) throw new DOMException('Application capture unavailable', 'NotSupportedError');
  const source = await bridge.requestAppAudio();
  if (!source) throw new DOMException('Application selection cancelled', 'NotAllowedError');
  let context: AudioContext | null = null;
  let stopFrames = () => {}, stopEvents = () => {};
  let track: MediaStreamTrack | undefined;
  let closed = false;
  const stop = () => {
    if (closed) return; closed = true; stopFrames(); stopEvents();
    void bridge.send({ type: 'stop-app-audio', id: source.id }).catch(() => {});
    void context?.close().catch(() => {});
  };
  try {
    context = new AudioContext({ sampleRate: 48000, latencyHint: 'interactive' });
    await context.audioWorklet.addModule((await import('./app-audio-worklet.ts?worker&url')).default);
    const node = new AudioWorkletNode(context, 'dilla-application-audio', { numberOfInputs: 0, numberOfOutputs: 1, outputChannelCount: [2] });
    const destination = context.createMediaStreamDestination(); node.connect(destination);
    track = destination.stream.getAudioTracks()[0];
    if (!track) throw new Error('No application audio track');
    const stopTrack = track.stop.bind(track);
    track.stop = () => { stop(); stopTrack(); };
    stopFrames = bridge.onAppAudio(source.id, samples => { if (!closed) node.port.postMessage(samples, [samples.buffer]); });
    stopEvents = bridge.onEvent(event => {
      if (event.type === 'app-audio-ended' && event.id === source.id) { track!.stop(); track!.dispatchEvent(new Event('ended')); }
    });
    await context.resume();
    return track;
  } catch (error) { stop(); track?.stop(); throw error; }
}
