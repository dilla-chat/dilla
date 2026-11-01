import type { ScreenCaptureOptions } from './contract.ts';

function record(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === 'object' && !Array.isArray(value);
}
function integer(value: unknown, min: number, max: number): value is number {
  return typeof value === 'number' && Number.isInteger(value) && value >= min && value <= max;
}
export function captureId(value: unknown): value is string {
  return typeof value === 'string' && /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(value);
}
export function captureOptions(value: unknown): ScreenCaptureOptions {
  if (!record(value) || Object.keys(value).sort().join(',') !== 'frameRate,height,width'
    || !integer(value.width, 16, 3840) || !integer(value.height, 16, 2160) || !integer(value.frameRate, 1, 60)) {
    throw new Error('Invalid screen capture options');
  }
  return { width: value.width, height: value.height, frameRate: value.frameRate };
}
export function frameBytes(width: unknown, height: unknown): number {
  if (!integer(width, 16, 3840) || !integer(height, 16, 2160)) throw new Error('Invalid screen capture frame');
  return width * height * 4;
}
/** Called only after the caller separately verifies the exact owning WebContents. */
export function videoOnlyPermission(permission: string, details: unknown, url: string): boolean {
  return permission === 'display-capture' && record(details) && details.isMainFrame === true && details.requestingUrl === url
    && Array.isArray(details.mediaTypes) && details.mediaTypes.length === 1 && details.mediaTypes[0] === 'video';
}

/** At most two transferable buffers can await consumer acknowledgement. No retained pixel buffers. */
export class FrameWindow {
  readonly id: string;
  private sequence = 0;
  private pending: { sequence: number; at: number }[] = [];
  constructor(id: string) { if (!captureId(id)) throw new Error('Invalid capture id'); this.id = id; }
  reserve(now: number): number | null {
    if (this.pending.length >= 2) return null;
    // Fail rather than reusing a sequence after a very long uninterrupted capture.
    if (this.sequence > 0xffffffff) throw new Error('Screen capture sequence exhausted');
    const sequence = this.sequence++;
    this.pending.push({ sequence, at: now });
    return sequence;
  }
  ack(value: unknown): boolean {
    if (!record(value) || Object.keys(value).sort().join(',') !== 'id,sequence,type' || value.type !== 'ack'
      || value.id !== this.id || !integer(value.sequence, 0, 0xffffffff) || this.pending[0]?.sequence !== value.sequence) return false;
    this.pending.shift();
    return true;
  }
  expired(now: number): boolean { return !!this.pending.length && now - this.pending[0].at >= 5000; }
}
