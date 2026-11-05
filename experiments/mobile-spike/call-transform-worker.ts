// WebKit path of the call probe: an RTCRtpScriptTransform worker. The send side XORs every encoded frame's payload,
// the receive side XORs it back, and both count frames. Counts are posted to the page once a second.
const KEY = 0x5a;
const counts = { framesOut: 0, framesIn: 0 };

interface ProbeTransformEvent extends Event {
  transformer: { readable: ReadableStream<{ data: ArrayBuffer }>; writable: WritableStream; options: { side: 'send' | 'receive' } };
}

function xor(frame: { data: ArrayBuffer }): void {
  const bytes = new Uint8Array(frame.data.slice(0));
  for (let i = 0; i < bytes.length; i++) bytes[i] ^= KEY;
  frame.data = bytes.buffer;
}

(self as unknown as { onrtctransform: (e: ProbeTransformEvent) => void }).onrtctransform = (e) => {
  const { readable, writable, options } = e.transformer;
  const send = options.side === 'send';
  readable
    .pipeThrough(new TransformStream({
      transform(frame, ctrl) {
        xor(frame);
        if (send) counts.framesOut++; else counts.framesIn++;
        ctrl.enqueue(frame);
      },
    }))
    .pipeTo(writable)
    .catch((err: unknown) => { self.postMessage({ pipeError: `${options.side}: ${String(err)}` }); });
};

setInterval(() => { self.postMessage({ ...counts }); }, 1000);
