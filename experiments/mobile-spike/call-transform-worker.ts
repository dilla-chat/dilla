// WebKit path of the call probe: an RTCRtpScriptTransform worker. The send side prepends a 2-byte magic and XORs the
// whole frame; the receive side XORs it back, checks and strips the magic (framesOk / framesBad). Counts are posted to
// the page once a second. Same frame format as the Chromium path in call-probe.ts.
const KEY = 0x5a;
const MAGIC = [0xd1, 0x1a];
const counts = { framesOut: 0, framesIn: 0, framesOk: 0, framesBad: 0 };

interface ProbeTransformEvent extends Event {
  transformer: { readable: ReadableStream<{ data: ArrayBuffer }>; writable: WritableStream; options: { side: 'send' | 'receive' } };
}

function seal(frame: { data: ArrayBuffer }): void {
  const src = new Uint8Array(frame.data);
  const out = new Uint8Array(src.length + 2);
  out[0] = MAGIC[0];
  out[1] = MAGIC[1];
  out.set(src, 2);
  for (let i = 0; i < out.length; i++) out[i] ^= KEY;
  frame.data = out.buffer;
}

/** True when the frame carried the magic (then it is stripped); false leaves the frame as it arrived. */
function open(frame: { data: ArrayBuffer }): boolean {
  const src = new Uint8Array(frame.data);
  if (src.length < 2 || (src[0] ^ KEY) !== MAGIC[0] || (src[1] ^ KEY) !== MAGIC[1]) return false;
  const out = new Uint8Array(src.length - 2);
  for (let i = 0; i < out.length; i++) out[i] = src[i + 2] ^ KEY;
  frame.data = out.buffer;
  return true;
}

(self as unknown as { onrtctransform: (e: ProbeTransformEvent) => void }).onrtctransform = (e) => {
  const { readable, writable, options } = e.transformer;
  const send = options.side === 'send';
  readable
    .pipeThrough(new TransformStream({
      transform(frame, ctrl) {
        if (send) {
          seal(frame);
          counts.framesOut++;
        } else {
          counts.framesIn++;
          if (open(frame)) counts.framesOk++; else counts.framesBad++;
        }
        ctrl.enqueue(frame);
      },
    }))
    .pipeTo(writable)
    .catch((err: unknown) => { self.postMessage({ pipeError: `${options.side}: ${String(err)}` }); });
};

setInterval(() => { self.postMessage({ ...counts }); }, 1000);
