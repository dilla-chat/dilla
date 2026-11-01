import { PCMRing } from './pcm-ring.ts';
declare const AudioWorkletProcessor: { new(): { port: MessagePort } };
declare function registerProcessor(name: string, processor: new() => unknown): void;
class ApplicationAudio extends AudioWorkletProcessor {
  private readonly queue = new PCMRing();
  constructor() { super(); this.port.onmessage = event => { if (event.data instanceof Float32Array) this.queue.push(event.data); }; }
  process(_inputs: Float32Array[][], outputs: Float32Array[][]): boolean {
    const stereo = outputs[0];
    if (stereo?.[0] && stereo[1]) this.queue.read(stereo[0], stereo[1]);
    return true;
  }
}
registerProcessor('dilla-application-audio', ApplicationAudio);
