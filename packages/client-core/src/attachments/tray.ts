// Worker-memory attachment preparation and pending-reference cleanup.
import type { AttachmentDescriptor, Id } from '../core-port';
import type { Routes } from '../http/routes';
import { AttachmentError, BROWSER_ATTACHMENT_CAP, MAX_ATTACHMENTS, newKeyAndNonce, sealBlob, sealThumb } from './crypto';
import { safeName } from './name';
import { makeThumbnail, RENDERABLE_IMAGE, type ThumbDeps } from './thumb';

export type TrayPhase = 'reading' | 'preparing' | 'uploading' | 'ready' | 'failed';
export interface TrayEntry { id: string; name: string; size: number; mime: string; image: boolean; phase: TrayPhase;
  reason: string; descriptor: AttachmentDescriptor | null; }
type Internal = TrayEntry & { file: File; uploaded: boolean; removed: boolean; blobId: Uint8Array | null };

export class Tray {
  private readonly list: Internal[] = [];
  private next = 0;
  private chain: Promise<void> = Promise.resolve();
  constructor(private readonly deps: { channelId: Id; routes: Pick<Routes, 'putBlob' | 'deleteBlob'>; thumb: ThumbDeps | null;
    random(n: number): Uint8Array; onChange(entries: readonly TrayEntry[]): void }) {}

  entries(): readonly TrayEntry[] {
    return this.list.map(({ id, name, size, mime, image, phase, reason, descriptor }) =>
      ({ id, name, size, mime, image, phase, reason, descriptor }));
  }
  /** Export the original File handles for a one-time, encrypted local update checkpoint. */
  files(): File[] { return this.list.filter(entry => entry.phase !== 'failed').map(({ file }) => file); }
  private changed(): void { this.deps.onChange(this.entries()); }
  async add(files: readonly File[]): Promise<string[]> {
    if (this.list.length + files.length > MAX_ATTACHMENTS) throw new AttachmentError('E_ATTACHMENT_COUNT');
    const fresh = files.map((file): Internal => {
      const mime = file.type && new TextEncoder().encode(file.type).length <= 255 ? file.type : 'application/octet-stream';
      return { file, id: String(++this.next), name: safeName(file.name, ''), size: file.size, mime,
        image: file.size <= BROWSER_ATTACHMENT_CAP && RENDERABLE_IMAGE.includes(mime),
        phase: file.size > BROWSER_ATTACHMENT_CAP ? 'failed' : 'reading',
        reason: file.size > BROWSER_ATTACHMENT_CAP ? 'E_ATTACHMENT_TOO_LARGE' : '', descriptor: null,
        uploaded: false, removed: false, blobId: null };
    });
    this.list.push(...fresh);
    this.changed();
    this.chain = this.chain.then(async () => { for (const entry of fresh) await this.process(entry); });
    await this.chain;
    return fresh.map((e) => e.id);
  }
  private async cleanup(blobId: Uint8Array): Promise<void> {
    try { await this.deps.routes.deleteBlob(this.deps.channelId, blobId); } catch { /* pending_ttl expires a failed cleanup */ }
  }
  private async process(e: Internal): Promise<void> {
    if (e.removed || e.phase === 'failed') return;
    try {
      const bytes = new Uint8Array(await e.file.arrayBuffer());
      if (e.removed) return;
      e.phase = 'preparing'; this.changed();
      const { key, nonce } = newKeyAndNonce((n) => this.deps.random(n));
      const { stored, blobId } = await sealBlob(key, nonce, bytes);
      if (e.removed) return;
      let w: number | null = null;
      let h: number | null = null;
      let thumb: Uint8Array | null = null;
      if (e.image && this.deps.thumb) {
        const preview = await makeThumbnail(e.file, this.deps.thumb);
        if (e.removed) return;
        if (preview) {
          w = preview.w; h = preview.h;
          if (preview.bytes) thumb = await sealThumb(key, nonce, preview.bytes);
        }
      }
      if (e.removed) return;
      e.phase = 'uploading'; e.blobId = blobId; this.changed();
      await this.deps.routes.putBlob(this.deps.channelId, blobId, stored);
      e.uploaded = true;
      if (e.removed) { await this.cleanup(blobId); return; }
      e.descriptor = { blobId, key, nonce, size: bytes.length, mime: e.mime, w, h, thumb, name: e.name };
      e.phase = 'ready'; this.changed();
    } catch (error) {
      if (e.removed) return;
      e.phase = 'failed';
      e.reason = typeof error === 'object' && error !== null && 'code' in error && typeof error.code === 'string'
        ? error.code : 'E_ATTACHMENT_FAILED';
      e.descriptor = null; this.changed();
    }
  }
  async discard(id: string): Promise<void> {
    const index = this.list.findIndex((e) => e.id === id);
    if (index < 0) return;
    const [entry] = this.list.splice(index, 1);
    entry.removed = true;
    this.changed();
    if (entry.uploaded && entry.blobId) await this.cleanup(entry.blobId);
  }
  take(ids: readonly string[]): AttachmentDescriptor[] {
    if (new Set(ids).size !== ids.length) throw new Error('E_TRAY_NOT_READY');
    const entries = ids.map((id) => this.list.find((e) => e.id === id));
    if (entries.some((e) => !e || e.phase !== 'ready' || !e.descriptor)) throw new Error('E_TRAY_NOT_READY');
    const descriptors = entries.map((e) => e!.descriptor!);
    for (const entry of entries) { const index = this.list.indexOf(entry!); this.list.splice(index, 1); }
    this.changed();
    return descriptors;
  }
}
