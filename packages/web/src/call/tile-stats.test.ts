import { describe, expect, it } from 'vitest';
import { audioCodecOf, readTileStats, videoFrameOf } from './tile-stats.ts';

const report = (entries: Array<Record<string, unknown>>) => {
  const m = new Map(entries.map((e) => [e.id as string, e]));
  return m as unknown as RTCStatsReport;
};

describe('tile stats', () => {
  it('the sent audio codec comes from the outbound-rtp entry’s codec', () => {
    const r = report([
      { id: 'c1', type: 'codec', mimeType: 'audio/opus', clockRate: 48000 },
      { id: 'o1', type: 'outbound-rtp', kind: 'audio', codecId: 'c1' },
    ]);
    expect(audioCodecOf(r)).toEqual({ codec: 'opus', clockRate: 48000 });
    expect(audioCodecOf(report([{ id: 'o1', type: 'outbound-rtp', kind: 'audio' }]))).toBeNull();
    expect(audioCodecOf(undefined)).toBeNull();
  });

  it('a sent video’s frame is its largest layer’s; nothing until the browser reports both height and rate', () => {
    const r = report([
      { id: 'l', type: 'outbound-rtp', kind: 'video', frameHeight: 360, framesPerSecond: 15 },
      { id: 'h', type: 'outbound-rtp', kind: 'video', frameHeight: 1440, framesPerSecond: 29.6 },
      { id: 'i', type: 'inbound-rtp', kind: 'video', frameHeight: 2160, framesPerSecond: 60 },
    ]);
    expect(videoFrameOf(r, 'outbound-rtp')).toEqual({ height: 1440, fps: 30 });
    expect(videoFrameOf(r, 'inbound-rtp')).toEqual({ height: 2160, fps: 60 });
    expect(videoFrameOf(report([{ id: 'x', type: 'inbound-rtp', kind: 'video', frameHeight: 720 }]), 'inbound-rtp')).toBeNull();
  });

  it('one read covers the own microphone and screen and every remote screen; a failed read is absent', async () => {
    const track = (r: RTCStatsReport | Error) => ({ getRTCStatsReport: () => (r instanceof Error ? Promise.reject(r) : Promise.resolve(r)) });
    const mic = report([{ id: 'c', type: 'codec', mimeType: 'audio/opus', clockRate: 48000 }, { id: 'o', type: 'outbound-rtp', kind: 'audio', codecId: 'c' }]);
    const own = report([{ id: 'o', type: 'outbound-rtp', kind: 'video', frameHeight: 1080, framesPerSecond: 30 }]);
    const bob = report([{ id: 'i', type: 'inbound-rtp', kind: 'video', frameHeight: 1440, framesPerSecond: 30 }]);
    const pub = (t: unknown) => ({ getTrackPublication: () => (t === undefined ? undefined : { track: t }) });
    const stats = await readTileStats({
      ended: () => false, micPub: () => ({ track: track(mic) }), screenPub: () => ({ track: track(own) }), shareAudio: () => true,
      remotes: () => [{ identity: 'bob', ...pub(track(bob)) }, { identity: 'carl', ...pub(track(new Error('gone'))) }, { identity: 'dana', ...pub(undefined) }],
      screenSource: 'screen_share',
    });
    expect(stats).toEqual({ ownAudio: { codec: 'opus', clockRate: 48000 }, ownScreen: { height: 1080, fps: 30, audio: true }, screens: { bob: { height: 1440, fps: 30 } } });
  });

  it('an ended session reads nothing', async () => {
    const stats = await readTileStats({ ended: () => true, micPub: () => { throw new Error('not read'); }, screenPub: () => null, shareAudio: () => false, remotes: () => [], screenSource: 's' });
    expect(stats).toEqual({ ownAudio: null, ownScreen: null, screens: {} });
  });
});
