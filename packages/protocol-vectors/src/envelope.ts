import { encode, decode, type CborValue } from './cbor.ts';
import { hmacSha256 } from './hmac.ts';
import { concat, utf8, be64 } from './bytes.ts';

// A const object, not a TypeScript enum: Node's type stripping (used by `npm run vectors`) cannot execute enums.
export const EnvelopeType = { Message: 0, Edit: 1, Delete: 2, ReactionAdd: 3, ReactionRemove: 4, Pin: 5, Unpin: 6 } as const;
export type EnvelopeType = (typeof EnvelopeType)[keyof typeof EnvelopeType];
const ENVELOPE_TYPES = new Set<number>(Object.values(EnvelopeType));

export type Attachment = { blobId: Uint8Array; key: Uint8Array; nonce: Uint8Array; size: number; mime: string; w: number | null; h: number | null; thumb: Uint8Array | null; name: string };
export type Preview = { url: string; title: string; description: string; image: Uint8Array | null };
export type Envelope = {
  v: 1; msgId: Uint8Array; type: EnvelopeType; threadId: Uint8Array | null; replyTo: Uint8Array | null;
  body: string; attachments: Attachment[]; previews: Preview[]; kf: Uint8Array;
};

const FRANK_DOMAIN = utf8('dilla frank v1');
const TAG_DOMAIN = utf8('dilla frank tag v1');

function assertLen(b: Uint8Array, n: number, what: string) { if (b.length !== n) throw new Error(`${what}: expected ${n} bytes, got ${b.length}`); }

// Limits (04-envelope-and-franking.md, "Envelope").
const MAX_BODY_LONG = 4000; // type 0/1, UTF-8 bytes
const MAX_BODY_SHORT = 32; // type 3/4, UTF-8 bytes
const MAX_BODY_NONE = 0; // type 2/5/6: a tombstone, pin or unpin carries no body at all
const MAX_ATTACHMENTS = 10;
const MAX_PREVIEWS = 2;
const MAX_PREVIEW_IMAGE = 16384;
const MAX_THUMB = 8192;
const MAX_MIME = 255;
const MAX_NAME = 255; // attachment name, UTF-8 bytes
const MAX_URL = 2048;
const MAX_TITLE = 256;
const MAX_DESCRIPTION = 1024;

function assertLimit(cond: boolean, what: string) { if (!cond) throw new Error(`envelope: limit exceeded: ${what}`); }

function toArray(e: Envelope, kf: Uint8Array): CborValue {
  assertLen(e.msgId, 16, 'msg_id');
  if (e.threadId) assertLen(e.threadId, 16, 'thread_id');
  if (e.replyTo) assertLen(e.replyTo, 16, 'reply_to');
  return [
    e.v, e.msgId, e.type, e.threadId, e.replyTo, e.body,
    e.attachments.map(a => { assertLen(a.blobId, 32, 'blob_id'); assertLen(a.key, 32, 'key'); assertLen(a.nonce, 12, 'nonce');
      return [a.blobId, a.key, a.nonce, a.size, a.mime, a.w, a.h, a.thumb, a.name]; }),
    e.previews.map(p => [p.url, p.title, p.description, p.image]),
    kf,
  ];
}

export function encodeEnvelope(e: Envelope): Uint8Array {
  assertLen(e.kf, 32, 'k_f');
  return encode(toArray(e, e.kf));
}

export function decodeEnvelope(bytes: Uint8Array): Envelope {
  const a = decode(bytes);
  if (!Array.isArray(a) || a.length !== 9) throw new Error('envelope: expected a 9-element array');
  const [v, msgId, type, threadId, replyTo, body, attachments, previews, kf] = a as [number, Uint8Array, number, Uint8Array | null, Uint8Array | null, string, CborValue[], CborValue[], Uint8Array];
  if (v !== 1) throw new Error(`envelope: unsupported version ${v}`);
  if (!ENVELOPE_TYPES.has(type)) throw new Error(`envelope: unknown type ${type}`);
  assertLen(msgId, 16, 'msg_id'); assertLen(kf, 32, 'k_f');

  const bodyLen = utf8(body).length;
  if (type === EnvelopeType.Message || type === EnvelopeType.Edit) assertLimit(bodyLen <= MAX_BODY_LONG, 'body over 4000 bytes for type 0/1');
  if (type === EnvelopeType.ReactionAdd || type === EnvelopeType.ReactionRemove) assertLimit(bodyLen <= MAX_BODY_SHORT, 'body over 32 bytes for type 3/4');
  // A delete, pin or unpin is contentless: any body there is unrenderable by a conforming client
  // and is a covert channel, so the limit is 0 and the envelope is rejected rather than trimmed.
  if (type === EnvelopeType.Delete || type === EnvelopeType.Pin || type === EnvelopeType.Unpin) assertLimit(bodyLen <= MAX_BODY_NONE, 'body on type 2/5/6');
  assertLimit(attachments.length <= MAX_ATTACHMENTS, 'more than 10 attachments');
  assertLimit(previews.length <= MAX_PREVIEWS, 'more than 2 previews');

  const decodedAttachments = attachments.map(x => {
    if (!Array.isArray(x) || x.length !== 9) throw new Error('envelope: shape: an attachment is not a 9-element array');
    const [blobId, key, nonce, size, mime, w, h, thumb, name] = x as [Uint8Array, Uint8Array, Uint8Array, number, string, number | null, number | null, Uint8Array | null, string];
    assertLimit(utf8(mime).length <= MAX_MIME, 'mime over 255 bytes');
    if (thumb) assertLimit(thumb.length <= MAX_THUMB, 'thumb over 8192 bytes');
    assertLimit(utf8(name).length <= MAX_NAME, 'attachment name over 255 bytes');
    return { blobId, key, nonce, size, mime, w, h, thumb, name };
  });
  const decodedPreviews = previews.map(x => {
    const [url, title, description, image] = x as [string, string, string, Uint8Array | null];
    assertLimit(utf8(url).length <= MAX_URL, 'preview url over 2048 bytes');
    assertLimit(utf8(title).length <= MAX_TITLE, 'preview title over 256 bytes');
    assertLimit(utf8(description).length <= MAX_DESCRIPTION, 'preview description over 1024 bytes');
    if (image) assertLimit(image.length <= MAX_PREVIEW_IMAGE, 'preview image over 16384 bytes');
    return { url, title, description, image };
  });
  if (type !== EnvelopeType.Message) {
    if (replyTo === null) throw new Error(`envelope: shape: type ${type} names its target in reply_to`);
    if (decodedAttachments.length > 0 || decodedPreviews.length > 0) throw new Error(`envelope: shape: type ${type} carries no attachments or previews`);
  }
  return {
    v: 1, msgId, type: type as EnvelopeType, threadId, replyTo, body,
    attachments: decodedAttachments,
    previews: decodedPreviews,
    kf,
  };
}

/** C = HMAC-SHA256(K_f, "dilla frank v1" || CBOR(envelope with k_f = empty bstr)) */
export async function frankingCommitment(e: Envelope): Promise<Uint8Array> {
  const blanked = encode(toArray(e, new Uint8Array(0)));
  return hmacSha256(e.kf, concat(FRANK_DOMAIN, blanked));
}

/** T = HMAC-SHA256(K_frank, "dilla frank tag v1" || group_id || epoch(8) || seq(8) || uploader_device || C || recv_ts(8)) */
export async function frankingTag(instanceKey: Uint8Array, groupId: Uint8Array, epoch: number, seq: number, uploaderDevice: Uint8Array, commitment: Uint8Array, recvTs: number): Promise<Uint8Array> {
  assertLen(groupId, 16, 'group_id'); assertLen(uploaderDevice, 16, 'uploader_device'); assertLen(commitment, 32, 'commitment');
  return hmacSha256(instanceKey, concat(TAG_DOMAIN, groupId, be64(epoch), be64(seq), uploaderDevice, commitment, be64(recvTs)));
}
