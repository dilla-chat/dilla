import { decodeFrame } from '../../packages/client-core/src/gateway/frames';
import { expect, test } from './support/peer-browser';
import { watchContexts } from './support/leaks';
import { VoicePeer, WAIT, copy, joinVoice, noiseOff, onboard, tile } from './support/voice';

watchContexts(test);

test('a live call sends coarse reports over the existing socket and defers an update until the call ends', async ({ peerBrowser }) => {
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  const bob = await peerBrowser.open('bob');
  try {
    await peer.setup();
    await onboard(alice, peer, 'alice'); await onboard(bob, peer, 'bob'); await peer.seat(2);
    await noiseOff(alice.page); await noiseOff(bob.page);
    await expect.poll(() => alice.page.workers().some(w => /\/assets\/entry-[\w-]+\.js$/.test(w.url())), { timeout: WAIT }).toBe(true);
    const worker = alice.page.workers().find(w => /\/assets\/entry-[\w-]+\.js$/.test(w.url()));
    if (!worker) throw new Error(`the shipping core worker did not start: ${alice.page.workers().map(w => w.url()).join(', ')}`);
    await worker.evaluate(() => {
      const root = globalThis as unknown as { __reports: number[][] };
      root.__reports = [];
      const send = WebSocket.prototype.send;
      WebSocket.prototype.send = function(data) {
        if (data instanceof ArrayBuffer) root.__reports.push(Array.from(new Uint8Array(data)));
        else if (ArrayBuffer.isView(data)) root.__reports.push(Array.from(new Uint8Array(data.buffer, data.byteOffset, data.byteLength)));
        send.call(this, data);
      };
    });
    const http = { stats: 0, activity: 0, version: 0 };
    alice.context.on('request', request => {
      const path = new URL(request.url()).pathname;
      if (/^\/v1\/calls\/[^/]+\/stats$/.test(path)) http.stats++;
      if (path === '/v1/accounts/me/presence/activity') http.activity++;
      if (path === '/app-version.json') http.version++;
    });
    await joinVoice(alice.page, peer.communityName, peer.voiceName);
    await joinVoice(bob.page, peer.communityName, peer.voiceName);
    await expect(tile(alice.page, 'bob')).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
    const reports = async () => (await worker.evaluate(() => (globalThis as unknown as { __reports: number[][] }).__reports))
      .map(bytes => ({ bytes, frame: decodeFrame(new Uint8Array(bytes)) })).filter(row => row.frame.op === 58);
    await expect.poll(async () => (await reports()).length, { timeout: WAIT }).toBeGreaterThanOrEqual(2);
    for (const { bytes, frame } of await reports()) {
      expect(bytes.length).toBeLessThanOrEqual(1024);
      expect(frame.n).toBe(0n);
    }
    expect(http.stats).toBe(0); expect(http.activity).toBe(0);
    await alice.context.route('**/app-version.json', route => route.fulfill({ json: { entry: '/assets/next-call-build.js' } }));
    const versions = http.version;
    await alice.page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
    await expect.poll(() => http.version).toBe(versions + 1);
    await expect(alice.page.getByText(copy('update.finishCall'), { exact: true })).toBeVisible();
    await expect(tile(alice.page, 'bob')).toHaveAttribute('data-voice-state', 'ok');
    await expect(tile(bob.page, 'alice')).toHaveAttribute('data-voice-state', 'ok');
  } finally {
    await peerBrowser.close(bob); await peerBrowser.close(alice); await peer.close();
  }
});
