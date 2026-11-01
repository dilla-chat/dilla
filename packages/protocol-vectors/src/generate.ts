import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { concat, fromHex, hex } from './bytes.ts';
import { encodeEnvelope, frankingCommitment, frankingTag, EnvelopeType, type Envelope, type Attachment, type Preview } from './envelope.ts';
import { kid, deriveFrameKeys, counter, nonce, encodeSframeHeader, SUITE, encryptFrame, prefixLen, protect, rbspEscape, canonicalizeH264, SframeError, type Codec } from './sframe.ts';
import { safetyNumber, sas, recoveryKeyBase32, deriveRecoveryKeys, credentialIdentity, sskMessage, dskMessage, sessionPreimage, accountKindMessage, accountKindAttestation } from './identity.ts';
import { keyFromSeed, sign } from './ed25519.ts';
import { frameVectors } from './frames.ts';
import { encode } from './cbor.ts';
import { attachmentVectors } from './attachment.ts';
import { botVectors } from './bot.ts';

export const VECTORS_DIR = join(dirname(fileURLToPath(import.meta.url)), '..', '..', '..', 'protocol', 'vectors');
const fill = (n: number, b: number) => new Uint8Array(n).fill(b);
const j = (o: unknown) => JSON.stringify(o, (_, v) => v instanceof Uint8Array ? hex(v) : v, 2) + '\n';

export async function envelopeVectors() {
  const att = await attachmentVectors();
  const cases: Array<{ name: string; envelope: Envelope }> = [
    { name: 'text message with a reply and one attachment', envelope: {
      v: 1, msgId: fill(16, 0x01), type: EnvelopeType.Message, threadId: null, replyTo: fill(16, 0x02),
      body: 'On my way. Grab the wolf capes from the chest by the portal.',
      attachments: [{ blobId: fill(32, 0x03), key: fill(32, 0x04), nonce: fill(12, 0x05), size: 2100000, mime: 'image/jpeg', w: 1600, h: 900, thumb: null, name: 'wolf-capes.jpg' }],
      previews: [], kf: fill(32, 0x06) } },
    { name: 'reaction add in a thread', envelope: {
      v: 1, msgId: fill(16, 0x11), type: EnvelopeType.ReactionAdd, threadId: fill(16, 0x12), replyTo: fill(16, 0x13),
      body: '⛏', attachments: [], previews: [], kf: fill(32, 0x16) } },
    { name: 'delete tombstone', envelope: {
      v: 1, msgId: fill(16, 0x21), type: EnvelopeType.Delete, threadId: null, replyTo: fill(16, 0x01),
      body: '', attachments: [], previews: [], kf: fill(32, 0x26) } },
    { name: 'message with a sender-generated link preview', envelope: {
      v: 1, msgId: fill(16, 0x31), type: EnvelopeType.Message, threadId: null, replyTo: null,
      body: 'https://valheim.fandom.com/wiki/Silver', attachments: [],
      previews: [{ url: 'https://valheim.fandom.com/wiki/Silver', title: 'Silver', description: 'Silver is a metal found in the Mountains.', image: null }],
      kf: fill(32, 0x36) } },
    { name: 'image message with a thumbnail and a name', envelope: {
      v: 1, msgId: fill(16, 0x41), type: EnvelopeType.Message, threadId: null, replyTo: null,
      body: '', attachments: [{ blobId: fill(32, 0x43), key: fill(32, 0x44), nonce: fill(12, 0x45), size: 1234,
        mime: 'image/png', w: 640, h: 480, thumb: att.cases[2]!.thumb, name: 'map.png' }],
      previews: [], kf: fill(32, 0x46) } },
  ];
  const out = [];
  for (const c of cases) {
    const bytes = encodeEnvelope(c.envelope);
    out.push({ name: c.name, envelope: c.envelope, cbor: hex(bytes), length: bytes.length, commitment: hex(await frankingCommitment(c.envelope)) });
  }
  // Inputs a conforming decoder must REFUSE, with the error code 04-envelope-and-franking.md
  // names for each. `encodeEnvelope` does not apply the limits, so it can produce the well-formed
  // deterministic CBOR of an envelope that is nonetheless invalid — which is exactly what an
  // implementation under test has to be handed.
  //
  // interfaces.md §2.8: nine cases, not one — a count case per collection (5 attachments, 3
  // previews) plus one case per tightened scalar bound, beside the pre-existing tombstone case.
  // web-2b task 1: four more — three shapes and the name limit (L-CORE-30).
  const sampleAttachment = (): Attachment => ({ blobId: fill(32, 0x03), key: fill(32, 0x04), nonce: fill(12, 0x05), size: 2100000, mime: 'image/jpeg', w: 1600, h: 900, thumb: null, name: 'wolf-capes.jpg' });
  const samplePreview = (): Preview => ({ url: 'https://example.invalid/', title: 't', description: 'd', image: null });
  const rejectBase = { v: 1 as const, msgId: fill(16, 0x21), type: EnvelopeType.Message, threadId: null, replyTo: null, kf: fill(32, 0x26) };
  const withAttachment = (patch: Partial<Attachment>): Envelope => ({
    ...rejectBase, body: '', attachments: [{ ...sampleAttachment(), ...patch }], previews: [],
  });
  const withPreview = (patch: Partial<Preview>): Envelope => ({
    ...rejectBase, body: '', attachments: [], previews: [{ ...samplePreview(), ...patch }],
  });
  const rejectCase = (name: string, envelope: Envelope, error = 'E_ENVELOPE_LIMIT') => ({ name, error, cbor: hex(encodeEnvelope(envelope)) });
  const rejects = [
    rejectCase('delete tombstone with a non-empty body', { ...rejectBase, type: EnvelopeType.Delete, replyTo: fill(16, 0x01), body: 'deleted because', attachments: [], previews: [] }),
    rejectCase('mime one byte over 255', withAttachment({ mime: 'a'.repeat(256) })),
    rejectCase('thumb one byte over 8192', withAttachment({ thumb: new Uint8Array(8193) })),
    rejectCase('five attachments', { ...rejectBase, body: '', attachments: Array.from({ length: 5 }, sampleAttachment), previews: [] }),
    rejectCase('three previews', { ...rejectBase, body: '', attachments: [], previews: Array.from({ length: 3 }, samplePreview) }),
    rejectCase('preview url one byte over 2048', withPreview({ url: 'u'.repeat(2049) })),
    rejectCase('preview title one byte over 256', withPreview({ title: 't'.repeat(257) })),
    rejectCase('preview description one byte over 1024', withPreview({ description: 'd'.repeat(1025) })),
    rejectCase('preview image one byte over 16384', withPreview({ image: new Uint8Array(16385) })),
    rejectCase('an edit with no reply_to', { ...rejectBase, type: EnvelopeType.Edit, body: 'fixed', attachments: [], previews: [] }, 'E_ENVELOPE_SHAPE'),
    rejectCase('a reaction carrying an attachment', { ...rejectBase, type: EnvelopeType.ReactionAdd, replyTo: fill(16, 0x01), body: '👍', attachments: [sampleAttachment()], previews: [] }, 'E_ENVELOPE_SHAPE'),
    rejectCase('an attachment name one byte over 255', withAttachment({ name: 'n'.repeat(256) })),
    { name: 'an attachment of eight elements', error: 'E_ENVELOPE_SHAPE', cbor: hex(encode([1, fill(16, 0x21), 0, null, null, '', [[fill(32, 0x03), fill(32, 0x04), fill(12, 0x05), 2100000, 'image/jpeg', 1600, 900, null]], [], fill(32, 0x26)])) },
  ];
  return { version: 1, description: 'dilla envelope encodings (04-envelope-and-franking.md). cbor = deterministic CBOR of the 9-element array; commitment = HMAC-SHA256(k_f, "dilla frank v1" || CBOR with k_f blanked). rejects = well-formed CBOR that a conforming decoder must still refuse with the named error code.', cases: out, rejects };
}

export async function frankingVectors() {
  const env: Envelope = { v: 1, msgId: fill(16, 0x01), type: EnvelopeType.Message, threadId: null, replyTo: null, body: 'Found a silver vein under the mountain.', attachments: [], previews: [], kf: fill(32, 0x06) };
  const commitment = await frankingCommitment(env);
  const instanceKey = fill(32, 0x09);
  const cases = [];
  for (const [epoch, seq, ts] of [[41, 4127, 1758659640], [41, 4128, 1758659700], [42, 4129, 1758659760]] as const) {
    cases.push({ group_id: hex(fill(16, 0x07)), epoch, seq, uploader_device: hex(fill(16, 0x08)), commitment: hex(commitment), recv_ts: ts,
      tag: hex(await frankingTag(instanceKey, fill(16, 0x07), epoch, seq, fill(16, 0x08), commitment, ts)) });
  }
  return { version: 1, description: 'franking tags: T = HMAC-SHA256(K_frank, "dilla frank tag v1" || group_id || epoch(8) || seq(8) || uploader_device || C || recv_ts(8))', instance_franking_key: hex(instanceKey), envelope_cbor: hex(encodeEnvelope(env)), cases };
}

export async function sframeVectors() {
  const baseKey = fill(16, 0x0a);
  const cases = [];
  for (const [leaf, epoch, slot, layer, seq] of [[0, 41, 0, 0, 0], [3, 41, 0, 0, 1], [3, 297, 1, 2, 1000], [65535, 255, 3, 15, (1 << 30)]] as const) {
    const k = kid(leaf, epoch);
    const { key, salt } = await deriveFrameKeys(baseKey, k);
    const ctr = counter(slot, layer, seq);
    cases.push({ leaf_index: leaf, epoch, kid: k.toString(), key: hex(key), salt: hex(salt), slot, layer, seq, ctr: ctr.toString(), nonce: hex(nonce(salt, ctr)), header: hex(encodeSframeHeader(k, ctr)) });
  }

  // RFC 9605 appendix C.1: every KID value the appendix uses with ctr 0, every CTR value with
  // kid 0, and max/max. The 17 values are the appendix's own (no 7 and no 8 among them).
  const c1Values = [0n, 1n, 0xffn, 0x100n, 0xffffn, 0x10000n, 0xffffffn, 0x1000000n, 0xffffffffn, 0x100000000n,
    0xffffffffffn, 0x10000000000n, 0xffffffffffffn, 0x1000000000000n, 0xffffffffffffffn, 0x100000000000000n, 0xffffffffffffffffn];
  const c1Pairs: Array<[bigint, bigint]> = [...c1Values.map((c): [bigint, bigint] => [0n, c]), ...c1Values.slice(1).map((k): [bigint, bigint] => [k, 0n]),
    [0xffffffffffffffffn, 0xffffffffffffffffn]];
  const rfc9605_c1 = c1Pairs.map(([k, c]) => ({ kid: k.toString(), ctr: c.toString(), header: hex(encodeSframeHeader(k, c)) }));

  // RFC 9605 appendix C.3, suite 0x0004, as a dilla frame: the RFC's metadata is the prefix.
  const c3Base = fromHex('000102030405060708090a0b0c0d0e0f');
  const c3Keys = await deriveFrameKeys(c3Base, 0x123n);
  const c3Prefix = fromHex('4945544620534672616d65205747');
  const c3Plain = fromHex('64726166742d696574662d736672616d652d656e63');
  const rfc9605_c3 = {
    base_key: hex(c3Base), kid: (0x123n).toString(), ctr: (0x4567n).toString(), key: hex(c3Keys.key), salt: hex(c3Keys.salt),
    nonce: hex(nonce(c3Keys.salt, 0x4567n)), prefix: hex(c3Prefix), plaintext: hex(c3Plain),
    frame: hex(encryptFrame(c3Keys.key, c3Keys.salt, 0x123n, 0x4567n, c3Prefix.length, concat(c3Prefix, c3Plain))),
  };

  // One frame per codec rule. Each entry carries its own (leaf, epoch, slot, layer, seq) so the
  // slot always matches the source the codec would come from.
  const frameInputs: Array<{ name: string; codec: Codec; leaf: number; epoch: number; slot: number; layer: number; seq: number; input: string }> = [
    { name: 'opus mic', codec: 'opus', leaf: 0, epoch: 41, slot: 0, layer: 0, seq: 0, input: 'fc0102030405060708' },
    { name: 'vp8 delta camera', codec: 'vp8', leaf: 3, epoch: 41, slot: 1, layer: 0, seq: 1, input: '310102030405060708' },
    { name: 'vp8 key 640x480 layer 2', codec: 'vp8', leaf: 3, epoch: 297, slot: 1, layer: 2, seq: 1000, input: '5002009d012a8002e0010102030405060708' },
    { name: 'h264 sps pps idr escaped', codec: 'h264', leaf: 3, epoch: 41, slot: 1, layer: 0, seq: 5, input: '000000016742c01e95a0501ec80000000168ce3c800000000165888421ff00000312345a5a5a5a' },
  ];
  const media_frames: Array<Record<string, unknown>> = [];
  for (const f of frameInputs) {
    const k = kid(f.leaf, f.epoch);
    const ctr = counter(f.slot, f.layer, f.seq);
    const input = fromHex(f.input);
    media_frames.push({ name: f.name, codec: f.codec, leaf_index: f.leaf, epoch: f.epoch, slot: f.slot, layer: f.layer, seq: f.seq,
      kid: k.toString(), ctr: ctr.toString(), header: hex(encodeSframeHeader(k, ctr)), input: f.input,
      prefix_len: prefixLen(f.codec, input), frame: hex(await protect(baseKey, k, ctr, f.codec, input)) });
  }
  // The H.264 sender's shapes (CRYPTO-2), appended so no earlier row moves. `canonical` is what the
  // receiver opens to (P || plaintext) when the sender rewrote the input; `prefix_len` is the clear
  // prefix of the canonical frame. A prefix ending in two zero bytes cannot occur: the three ue(v)
  // fields read an odd number of bits, so the last prefix byte holds a field bit and the byte before
  // it cannot be all zero with a ue(v) prefix one in it; the seed-2 escape is pinned by `escapes`.
  const h264Inputs: Array<{ name: string; leaf: number; epoch: number; slot: number; seq: number; input: string }> = [
    { name: 'h264 idr alone', leaf: 1, epoch: 41, slot: 1, seq: 6, input: '00000001658884aabbccdd' },
    { name: 'h264 idr in two slices', leaf: 1, epoch: 41, slot: 1, seq: 7, input: '00000001658884aabb00000001658884ccdd' },
    { name: 'h264 three-byte start codes, leading garbage, trailing zeros', leaf: 1, epoch: 41, slot: 1, seq: 8,
      input: 'ffee0000016742c01e95a0501ec80000000168ce3c800000000001658884aabb0000' },
    { name: 'h264 prefix ending in one zero byte', leaf: 1, epoch: 41, slot: 2, seq: 9, input: '0000000165c02000aa55' },
    { name: 'h264 access unit delimiter and filler before the first slice', leaf: 1, epoch: 41, slot: 1, seq: 10,
      input: '0000000109f0000000010cffff0000000168ce3c8000000001658884aabb' },
  ];
  for (const f of h264Inputs) {
    const k = kid(f.leaf, f.epoch);
    const ctr = counter(f.slot, 0, f.seq);
    const c = canonicalizeH264(fromHex(f.input));
    const row: Record<string, unknown> = { name: f.name, codec: 'h264', leaf_index: f.leaf, epoch: f.epoch, slot: f.slot, layer: 0, seq: f.seq,
      kid: k.toString(), ctr: ctr.toString(), header: hex(encodeSframeHeader(k, ctr)), input: f.input,
      prefix_len: c.prefixLen, frame: hex(await protect(baseKey, k, ctr, 'h264', fromHex(f.input))) };
    if (hex(c.frame) !== f.input) row.canonical = hex(c.frame);
    media_frames.push(row);
  }
  // The first sequence number whose sealed H.264 frame ends in a 00 byte (its tag does: about one
  // frame in 256): a hop that trimmed trailing zeros would break exactly these frames.
  {
    const input = '00000001658884aabbccdd';
    const k = kid(1, 41);
    for (let seq = 100; ; seq++) {
      const ctr = counter(1, 0, seq);
      const frame = await protect(baseKey, k, ctr, 'h264', fromHex(input));
      if (frame[frame.length - 1] !== 0) continue;
      media_frames.push({ name: 'h264 sealed tail ending in a zero byte', codec: 'h264', leaf_index: 1, epoch: 41, slot: 1, layer: 0, seq,
        kid: k.toString(), ctr: ctr.toString(), header: hex(encodeSframeHeader(k, ctr)), input,
        prefix_len: prefixLen('h264', fromHex(input)), frame: hex(frame) });
      break;
    }
  }

  // Seeded RBSP escaping: rbsp_escape(seed, in) = WriteRbsp(00^seed || in)[seed..].
  const escapes = ([[0, '00000001000003'], [0, '000000'], [0, '0000'], [1, '0001'], [1, '0100'], [2, '03ff'], [2, '04'], [0, '9f03290100000000000001']] as const)
    .map(([seed, input]) => ({ seed_zeros: seed, in: input, out: hex(rbspEscape(seed, fromHex(input))) }));

  // Inputs a conforming receiver MUST refuse, each with the protocol/05 code it refuses them with.
  const headerRejects: Array<[string, string, string]> = [
    ['truncated header: empty', '', 'E_SFRAME_TRUNCATED_HEADER'],
    ['truncated header: extended kid missing', '80', 'E_SFRAME_TRUNCATED_HEADER'],
    ['truncated header: extended kid and ctr missing', '8f', 'E_SFRAME_TRUNCATED_HEADER'],
    ['truncated header: ctr one byte short', '99010001', 'E_SFRAME_TRUNCATED_HEADER'],
    ['non-minimal kid 5', '8005', 'E_SFRAME_NON_MINIMAL_HEADER'],
    ['non-minimal kid 7', '8007', 'E_SFRAME_NON_MINIMAL_HEADER'],
    ['non-minimal ctr 0', '0800', 'E_SFRAME_NON_MINIMAL_HEADER'],
    ['non-minimal ctr 7', '0807', 'E_SFRAME_NON_MINIMAL_HEADER'],
    ['non-minimal kid with a leading zero byte', '9000ff', 'E_SFRAME_NON_MINIMAL_HEADER'],
    ['non-minimal ctr with a leading zero byte', '0900ff', 'E_SFRAME_NON_MINIMAL_HEADER'],
    ['non-minimal kid 0 in eight bytes', 'f00000000000000000', 'E_SFRAME_NON_MINIMAL_HEADER'],
    ['non-minimal ctr 8 in eight bytes', '0f0000000000000008', 'E_SFRAME_NON_MINIMAL_HEADER'],
    ['non-minimal kid: leaf 1 epoch 41 in three bytes', 'a0000129', 'E_SFRAME_NON_MINIMAL_HEADER'],
    // Valid RFC 9605 headers whose KID is no dilla KID: 2^24 or more (protocol/05 "Frame format").
    ['non-canonical kid 2^24', hex(encodeSframeHeader(1n << 24n, 0n)), 'E_SFRAME_NON_CANONICAL_KID'],
    ['non-canonical kid: leaf 1 epoch 41 with bit 24 set', hex(encodeSframeHeader((1n << 24n) | kid(1, 41), 0n)), 'E_SFRAME_NON_CANONICAL_KID'],
    ['non-canonical kid 2^64 - 1', hex(encodeSframeHeader((1n << 64n) - 1n, 0n)), 'E_SFRAME_NON_CANONICAL_KID'],
  ];
  // The AEAD rows tamper with the VP8 key-frame vector: prefix 10 bytes, header 11 bytes.
  const good = fromHex(media_frames[2].frame as string);
  const flip = (at: number, mask: number) => { const f = good.slice(); f[at] ^= mask; return hex(f); };
  const frameRejects: Array<[string, string, string]> = [
    ['tag bit flipped', flip(good.length - 1, 0x01), 'E_SFRAME_AUTH'],
    ['prefix byte changed', flip(6, 0x01), 'E_SFRAME_AUTH'],
    ['first ciphertext bit flipped', flip(21, 0x01), 'E_SFRAME_AUTH'],
    ['header ctr byte changed', flip(20, 0x01), 'E_SFRAME_AUTH'],
    ['tag truncated to 15 bytes', hex(good.subarray(0, 10 + 11 + 15)), 'E_SFRAME_TRUNCATED_FRAME'],
  ];
  const rejects = [
    ...headerRejects.map(([name, header, error]) => ({ name, header, error })),
    ...frameRejects.map(([name, frame, error]) => ({ name, codec: 'vp8', leaf_index: 3, epoch: 297, frame, error })),
    { name: 'vp8 key frame shorter than 10 bytes', codec: 'vp8', frame: '5002009d012a8002e0', error: 'E_SFRAME_MALFORMED_PREFIX' },
    // The opus vector's frame, but sealed under its KID with bit 24 set and that KID's own key: a
    // receiver that derived a key for it would authenticate it. It must be refused before that.
    { name: 'opus frame sealed under a non-canonical kid', codec: 'opus',
      frame: hex(await protect(baseKey, (1n << 24n) | kid(0, 41), counter(0, 0, 0), 'opus', fromHex(frameInputs[0].input))), error: 'E_SFRAME_NON_CANONICAL_KID' },
  ];
  // The H.264 receiver's prefix refusals (CRYPTO-2), appended: each is refused while computing P,
  // before any header is read, so none needs a key.
  const nal = (header: number) => `00000001${header.toString(16).padStart(2, '0')}888421`;
  const h264Rejects: Array<[string, string, string]> = [
    ['h264 sps and pps with no slice', '000000016742c01e95a0501ec80000000168ce3c80', 'E_SFRAME_NO_VCL_NAL'],
    ...[2, 3, 4, 19, 20, 21].map((t): [string, string, string] => [`h264 first vcl nal of type ${t}`, nal(0x60 | t), 'E_SFRAME_UNSUPPORTED_CODEC']),
    ...[0, 24, 31].map((t): [string, string, string] => [`h264 nal of type ${t}`, nal(t === 0 ? 0 : 0x60 | t), 'E_SFRAME_MALFORMED_PREFIX']),
    ['h264 with no start code', '658884aabb', 'E_SFRAME_MALFORMED_PREFIX'],
    ['h264 with bytes before the first start code', 'ff00000001658884aabb', 'E_SFRAME_MALFORMED_PREFIX'],
    ['h264 slice header ending inside pic_parameter_set_id', '0000000165c020', 'E_SFRAME_MALFORMED_PREFIX'],
    ['h264 pic_parameter_set_id 256', '0000000165c02020aa', 'E_SFRAME_MALFORMED_PREFIX'],
  ];
  for (const [name, frame, error] of h264Rejects) {
    let code = 'accepted';
    try { prefixLen('h264', fromHex(frame)); } catch (e) { code = (e as SframeError).code; }
    if (code !== error) throw new Error(`${name}: the generator's own prefix rule says ${code}, not ${error}`);
    rejects.push({ name, codec: 'h264', frame, error });
  }

  // What a sender refuses (CRYPTO-2): a fresh sender (leaf 1, epoch 41, min epoch 41) encrypting
  // `input` on (slot, layer), or, for a row with `seq`, the counter (slot, layer, seq) itself.
  const sender_rejects = [
    { name: 'h264 sps a libwebrtc receiver would rewrite', codec: 'h264', slot: 1, layer: 0, input: '000000016742c01e95a0501ec80000000165888421', error: 'E_SFRAME_NON_CANONICAL_SPS' },
    { name: 'h264 with no slice', codec: 'h264', slot: 1, layer: 0, input: '000000016742c01e95a0501ec80000000168ce3c80', error: 'E_SFRAME_NO_VCL_NAL' },
    { name: 'vp8 key frame shorter than 10 bytes', codec: 'vp8', slot: 1, layer: 0, input: '5002009d012a8002e0', error: 'E_SFRAME_MALFORMED_PREFIX' },
    { name: 'layer 16', codec: 'opus', slot: 0, layer: 16, input: 'fc01', error: 'E_SFRAME_LAYER_RANGE' },
    { name: 'reserved slot 4', codec: 'opus', slot: 4, layer: 0, input: 'fc01', error: 'E_SFRAME_SLOT_MISMATCH' },
    { name: 'sequence number 2^52', slot: 0, layer: 0, seq: (1n << 52n).toString(), error: 'E_SFRAME_COUNTER_EXHAUSTED' },
    { name: 'h264 with only an AUD and filler', codec: 'h264', slot: 1, layer: 0, input: '0000000109f0000000010cffff', error: 'E_SFRAME_NO_VCL_NAL' },
  ];

  return { version: 1, suite: SUITE, description: 'dilla-sframe/1 (05-media-frames.md): base_key = MLS-Exporter("SFrame 1.0 Base Key", "", 16); key/salt per RFC 9605 §4.4.2; CTR = slot(8)|layer(4)|seq(52); nonce = salt XOR CTR; header per RFC 9605 §4.3. rfc9605_c1/rfc9605_c3 = RFC 9605 appendix C; media_frames = P || H || C || T with AAD H || P after the codec prefix rule (H.264: canonical start codes, seeded RBSP escape; canonical = what the receiver opens to when the sender rewrote the input); escapes = seeded WriteRbsp; rejects = inputs a receiver must refuse with the named code; sender_rejects = what a sender must refuse; receiver = scripted key-ring runs (05 "Rotation", "Receiver rules"), each from an empty ring.',
    base_key: hex(baseKey), cases, rfc9605_c1, rfc9605_c3, media_frames, escapes, rejects, sender_rejects, receiver: await receiverScripts() };
}

/**
 * Scripted receiver runs (CRYPTO-2): every script starts from an empty key ring. `install` installs
 * an epoch (its base key, roster as [leaf, device], this device's own leaf or -1, at `now` ms);
 * `decrypt` hands the ring one Opus frame from `leaf` in `epoch` on counter (`slot`, 0, `seq`),
 * sealed under that epoch's base key, for the track of `device` and `track_slot`, at `now` ms, and
 * names the outcome: `ok` or the E_SFRAME_* code. The expectations are protocol/05's rules; the
 * Rust and Go key rings must both reproduce every one.
 */
async function receiverScripts() {
  const dev = (b: number) => hex(fill(16, b));
  const [A, B, C] = [dev(0xa1), dev(0xb2), dev(0xc3)];
  const keyOf = (epoch: number) => fill(16, epoch % 251);
  const install = (epoch: number, roster: Array<[number, string]>, own: number, now: number) =>
    ({ op: 'install', epoch, base_key: hex(keyOf(epoch)), roster, own_leaf: own, now });
  const decrypt = async (leaf: number, epoch: number, slot: number, seq: number, device: string, trackSlot: number, now: number, expect: string) =>
    ({ op: 'decrypt', codec: 'opus', frame: hex(await protect(keyOf(epoch), kid(leaf, epoch), counter(slot, 0, seq), 'opus', fromHex(`fc${(seq % 256).toString(16).padStart(2, '0')}`))),
      leaf, epoch, slot, seq, device, track_slot: trackSlot, now, expect });
  const AB: Array<[number, string]> = [[0, A], [1, B]];
  const ABC: Array<[number, string]> = [[0, A], [1, B], [2, C]];
  return [
    { name: 'replay window of 128', steps: [
      install(5, AB, 0, 0),
      await decrypt(1, 5, 0, 200, B, 0, 0, 'ok'),
      await decrypt(1, 5, 0, 72, B, 0, 0, 'E_SFRAME_REPLAY'),
      await decrypt(1, 5, 0, 73, B, 0, 0, 'ok'),
      await decrypt(1, 5, 0, 73, B, 0, 0, 'E_SFRAME_REPLAY'),
      await decrypt(1, 5, 0, 200, B, 0, 0, 'E_SFRAME_REPLAY'),
      await decrypt(1, 5, 0, 199, B, 0, 0, 'ok'),
    ] },
    { name: 'own kid, leaf, sender and slot bindings', steps: [
      install(5, ABC, 0, 0),
      await decrypt(0, 5, 0, 0, A, 0, 0, 'E_SFRAME_OWN_KID'),
      await decrypt(9, 5, 0, 0, B, 0, 0, 'E_SFRAME_LEAF_NOT_IN_EPOCH'),
      await decrypt(2, 5, 0, 0, B, 0, 0, 'E_SFRAME_SENDER_MISMATCH'),
      await decrypt(1, 5, 1, 0, B, 0, 0, 'E_SFRAME_SLOT_MISMATCH'),
      await decrypt(1, 5, 1, 0, B, 1, 0, 'ok'),
    ] },
    { name: 'a device holding two leaves', steps: [
      install(5, [[0, A], [1, B], [3, B]], 0, 0),
      await decrypt(1, 5, 0, 0, B, 0, 0, 'E_SFRAME_SENDER_MISMATCH'),
    ] },
    { name: 'unknown, held for 10 s, stale for 20 s, then unknown', steps: [
      install(5, AB, 0, 0),
      await decrypt(1, 6, 0, 0, B, 0, 0, 'E_SFRAME_UNKNOWN_KID'),
      install(6, AB, 0, 1000),
      await decrypt(1, 6, 0, 0, B, 0, 1000, 'ok'),
      await decrypt(1, 5, 0, 0, B, 0, 10999, 'ok'),
      await decrypt(1, 5, 0, 1, B, 0, 11000, 'E_SFRAME_STALE_EPOCH'),
      await decrypt(1, 5, 0, 2, B, 0, 30999, 'E_SFRAME_STALE_EPOCH'),
      await decrypt(1, 5, 0, 3, B, 0, 31000, 'E_SFRAME_UNKNOWN_KID'),
    ] },
    { name: 'an epoch with the same low byte evicts the held one', steps: [
      install(1, AB, 0, 0),
      install(257, AB, 0, 100),
      await decrypt(1, 1, 0, 0, B, 0, 200, 'E_SFRAME_AUTH'),
      await decrypt(1, 257, 0, 1, B, 0, 200, 'ok'),
    ] },
    { name: 'a member the newest epoch removed', steps: [
      install(5, ABC, 0, 0),
      install(6, AB, 0, 0),
      await decrypt(2, 5, 0, 0, C, 0, 100, 'E_SFRAME_SENDER_MISMATCH'),
      await decrypt(1, 5, 0, 0, B, 0, 100, 'ok'),
    ] },
    { name: 'a late install more than 255 epochs old', steps: [
      install(300, AB, 0, 0),
      install(44, AB, 0, 10),
      await decrypt(1, 300, 0, 0, B, 0, 20, 'ok'),
      await decrypt(1, 44, 0, 1, B, 0, 20, 'E_SFRAME_AUTH'),
    ] },
    { name: 'a new epoch drops one more than 255 behind it', steps: [
      install(10, ABC, 0, 0),
      install(300, AB, 0, 100),
      await decrypt(1, 10, 0, 0, B, 0, 200, 'E_SFRAME_STALE_EPOCH'),
      await decrypt(1, 300, 0, 0, B, 0, 200, 'ok'),
    ] },
    { name: 'a dropped epoch installed again stays dropped', steps: [
      install(5, AB, 0, 0),
      install(6, AB, 0, 0),
      await decrypt(1, 6, 0, 0, B, 0, 10000, 'ok'),
      install(5, AB, 0, 11000),
      await decrypt(1, 5, 0, 0, B, 0, 11000, 'E_SFRAME_STALE_EPOCH'),
    ] },
    { name: 'a resync that moves a device keeps both kids', steps: [
      install(5, [[0, A], [3, B]], 0, 0),
      install(6, AB, 0, 100),
      await decrypt(3, 5, 0, 0, B, 0, 200, 'ok'),
      await decrypt(1, 6, 0, 0, B, 0, 200, 'ok'),
    ] },
    { name: 'a dropped epoch cannot be reinstalled 25 seconds after its drop', steps: [
      install(5, AB, 0, 0),
      install(6, AB, 0, 0),
      await decrypt(1, 5, 0, 0, B, 0, 10_000, 'E_SFRAME_STALE_EPOCH'),
      install(5, AB, 0, 35_000),
      await decrypt(1, 5, 0, 1, B, 0, 35_000, 'E_SFRAME_UNKNOWN_KID'),
      await decrypt(1, 6, 0, 0, B, 0, 35_000, 'ok'),
    ] },
  ];
}

export async function identityVectors() {
  const umkA = fill(32, 0xa1), umkB = fill(32, 0xb2);
  const rk = fill(32, 0x0b);
  const keys = await deriveRecoveryKeys(rk);

  // Deterministic seeds. They are published in the vector file so any implementation can
  // reproduce both signatures; they are test material and protect nothing.
  const umkSeed = fill(32, 0x41);
  const sskSeed = fill(32, 0x42);
  const dskSeed = fill(32, 0x43);
  const umk = await keyFromSeed(umkSeed);
  const ssk = await keyFromSeed(sskSeed);
  const dsk = await keyFromSeed(dskSeed);

  const userId = fill(16, 0xd4), deviceId = fill(16, 0xe5);
  const kind = 0, tier = 1, signerTier = 0;
  const sigUmkSsk = await sign(umk.privateKey, sskMessage(ssk.publicKey));
  const sigSskDev = await sign(ssk.privateKey, dskMessage(deviceId, dsk.publicKey, kind, tier, signerTier));

  // The device-session signature preimage (02-delivery-service.md "Device sessions"). The inputs
  // are fixed, not random, so a client in any language can check its own 81 bytes against the
  // instance's without running a handshake.
  const sessionInstanceId = fromHex('00112233445566778899aabbccddeeff');
  const sessionDeviceId = fromHex('0102030405060708090a0b0c0d0e0f10');
  const sessionNonce = fill(32, 0xab);

  // The instance-signed account-kind attestation (wire 4): the instance's external-sender key signs the account's kind
  // over the instance id and the user id (Ruling AW: no key). A human and a Bot account of the credential above.
  const instanceSeed = fill(32, 0x5a);
  const instance = await keyFromSeed(instanceSeed);
  const kindKeyId = fill(16, 0x5b);
  const kindInstanceId = sessionInstanceId;
  const attestations = [];
  for (const k of [0, 1] as const) {
    const message = accountKindMessage(kindInstanceId, userId, k);
    const sig = await sign(instance.privateKey, message);
    attestations.push({ kind: k, message: hex(message), signature: hex(sig), attestation: hex(accountKindAttestation(kindKeyId, k, sig)) });
  }

  return {
    version: 1,
    description: 'safety number (60 digits), SAS (30 digits), recovery key encodings and derived keys, credential identity CBOR with real Ed25519 signatures, the 81-byte device-session signature preimage, and the instance-signed account-kind attestation (03-identity.md, 02-delivery-service.md)',
    safety_number: { umk_a: hex(umkA), umk_b: hex(umkB), digits: await safetyNumber(umkA, umkB) },
    sas: { epoch_authenticator: hex(fill(32, 0xc3)), digits: sas(fill(32, 0xc3)) },
    recovery_key: { rk: hex(rk), base32: recoveryKeyBase32(rk), k_header: hex(keys.header), k_backup: hex(keys.archive) },
    credential_identity: {
      umk_priv: hex(umkSeed),
      ssk_priv: hex(sskSeed),
      dsk_priv: hex(dskSeed),
      dsk_pub: hex(dsk.publicKey),
      fields: {
        umk_pub: hex(umk.publicKey), user_id: hex(userId), device_id: hex(deviceId),
        kind, tier, signer_tier: signerTier, ssk_pub: hex(ssk.publicKey),
        sig_umk_ssk: hex(sigUmkSsk), sig_ssk_dev: hex(sigSskDev),
      },
      cbor: hex(credentialIdentity({
        umkPub: umk.publicKey, userId, deviceId, kind, tier, signerTier,
        sskPub: ssk.publicKey, sigUmkSsk, sigSskDev,
      })),
    },
    session_preimage: {
      instance_id: hex(sessionInstanceId),
      device_id: hex(sessionDeviceId),
      nonce: hex(sessionNonce),
      purpose: 0,
      preimage: hex(sessionPreimage(sessionInstanceId, sessionDeviceId, sessionNonce, 0)),
    },
    account_kind: {
      instance_priv: hex(instanceSeed),
      instance_pub: hex(instance.publicKey),
      key_id: hex(kindKeyId),
      instance_id: hex(kindInstanceId),
      user_id: hex(userId),
      cases: attestations,
      // Wire forms a decoder must refuse: version 2, kind 2, and a well-formed attestation with a trailing byte.
      rejects: [
        { name: 'version 2', attestation: hex(encode([2, kindKeyId, 0, fill(64, 0x01)])) },
        { name: 'kind 2', attestation: hex(encode([1, kindKeyId, 2, fill(64, 0x01)])) },
        { name: 'trailing byte', attestation: hex(new Uint8Array([...encode([1, kindKeyId, 0, fill(64, 0x01)]), 0])) },
      ],
    },
  };
}

export async function main() {
  mkdirSync(VECTORS_DIR, { recursive: true });
  writeFileSync(join(VECTORS_DIR, 'envelope.json'), j(await envelopeVectors()));
  writeFileSync(join(VECTORS_DIR, 'franking.json'), j(await frankingVectors()));
  writeFileSync(join(VECTORS_DIR, 'sframe.json'), j(await sframeVectors()));
  writeFileSync(join(VECTORS_DIR, 'identity.json'), j(await identityVectors()));
  writeFileSync(join(VECTORS_DIR, 'frames.json'), j(frameVectors()));
  writeFileSync(join(VECTORS_DIR, 'attachment.json'), j(await attachmentVectors()));
  writeFileSync(join(VECTORS_DIR, 'bot.json'), j(await botVectors()));
  console.log(`vectors written to ${VECTORS_DIR}`);
}

if (process.argv[1] === fileURLToPath(import.meta.url)) await main();
