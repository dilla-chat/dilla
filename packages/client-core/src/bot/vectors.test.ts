import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { fromHex, toHex } from '../hex.ts';
import * as bot from './protocol.ts';
import * as kp from './keypackage.ts';
import { encode } from '../cbor/index.ts';

type VectorKind = 'request' | 'response' | 'removal' | 'event' | 'receipt' | 'helper_request' | 'helper_response' | 'keypackage_publication' | 'keypackage_publication_receipt' | 'keypackage_intent' | 'keypackage_intent_result' | 'keypackage_resolution' | 'group_registration_request' | 'group_registration_receipt' | 'device_list_push';
type Context = { community: string; group?: string; actor_user?: string; actor_device?: string; now: string };
type Vector = { name: string; kind: VectorKind; hex?: string; body?: string; preimage_hex?: string; context?: Context; registration_body_hex?: string; registration_sha256?: string };
type Vectors = { cases: Vector[]; rejects: Vector[] };
type Decoded =
  | { kind: 'request'; value: bot.BotRequest }
  | { kind: 'response'; value: bot.BotResponse }
  | { kind: 'removal'; value: bot.RemovalProof }
  | { kind: 'event'; value: bot.DurableEvent }
  | { kind: 'receipt'; value: bot.SendReceipt }
  | { kind: 'helper_request'; value: bot.HelperEnvelope }
  | { kind: 'helper_response'; value: bot.HelperResponse }
  | { kind: 'keypackage_publication'; value: kp.KeyPackagePublication }
  | { kind: 'keypackage_publication_receipt'; value: kp.KeyPackagePublicationReceipt }
  | { kind: 'keypackage_intent'; value: kp.KeyPackageIntent }
  | { kind: 'keypackage_intent_result'; value: kp.KeyPackageIntentResult }
  | { kind: 'keypackage_resolution'; value: kp.KeyPackageResolution }
  | { kind: 'group_registration_request'; value: bot.BotGroupRegistrationRequest }
  | { kind: 'group_registration_receipt'; value: bot.BotGroupRegistrationReceipt }
  | { kind: 'device_list_push'; value: bot.DeviceListPush };

const vectors = JSON.parse(readFileSync(new URL('../../../../protocol/vectors/bot.json', import.meta.url), 'utf8')) as Vectors;

function context(c: Context): bot.BotContext {
  return {
    community: fromHex(c.community),
    ...(c.group === undefined ? {} : { group: fromHex(c.group) }),
    ...(c.actor_user === undefined ? {} : { actorUser: fromHex(c.actor_user) }),
    ...(c.actor_device === undefined ? {} : { actorDevice: fromHex(c.actor_device) }),
    now: BigInt(c.now),
  };
}

function bodyPayload(body: string): Uint8Array {
  expect(body.startsWith('dilla.bot/1:')).toBe(true);
  const encoded = body.slice('dilla.bot/1:'.length);
  expect(/^[A-Za-z0-9_-]+$/u.test(encoded)).toBe(true);
  const padded = encoded.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - encoded.length % 4) % 4);
  const binary = atob(padded);
  return Uint8Array.from(binary, (character) => character.charCodeAt(0));
}

function bodyFromHex(hex: string): string {
  const binary = String.fromCharCode(...fromHex(hex));
  const encoded = btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/u, '');
  return `dilla.bot/1:${encoded}`;
}

function decodeVector(kind: VectorKind, hex: string, body?: string): Decoded {
  switch (kind) {
    case 'request': return { kind, value: bot.decodeBotRequest(body ?? bodyFromHex(hex)) };
    case 'response': return { kind, value: bot.decodeBotResponse(body ?? bodyFromHex(hex)) };
    case 'removal': return { kind, value: bot.decodeRemovalProof(fromHex(hex)) };
    case 'event': return { kind, value: bot.decodeDurableEvent(fromHex(hex)) };
    case 'receipt': return { kind, value: bot.decodeSendReceipt(fromHex(hex)) };
    case 'helper_request': return { kind, value: bot.decodeHelperRequest(fromHex(hex)) };
    case 'helper_response': return { kind, value: bot.decodeHelperResponse(fromHex(hex)) };
    case 'keypackage_publication': return { kind, value: kp.decodeKeyPackagePublication(fromHex(hex)) };
    case 'keypackage_publication_receipt': return { kind, value: kp.decodeKeyPackagePublicationReceipt(fromHex(hex)) };
    case 'keypackage_intent': return { kind, value: kp.decodeKeyPackageIntent(fromHex(hex)) };
    case 'keypackage_intent_result': return { kind, value: kp.decodeKeyPackageIntentResult(fromHex(hex)) };
    case 'keypackage_resolution': return { kind, value: kp.decodeKeyPackageResolution(fromHex(hex)) };
    case 'group_registration_request': return { kind, value: bot.decodeBotGroupRegistrationRequest(fromHex(hex)) };
    case 'group_registration_receipt': return { kind, value: bot.decodeBotGroupRegistrationReceipt(fromHex(hex)) };
    case 'device_list_push': return { kind, value: bot.decodeDeviceListPush(fromHex(hex)) };
  }
}

function encodeVector(decoded: Decoded): Uint8Array | string {
  switch (decoded.kind) {
    case 'request': return bot.encodeBotRequest(decoded.value);
    case 'response': return bot.encodeBotResponse(decoded.value);
    case 'removal': return bot.encodeRemovalProof(decoded.value);
    case 'event': return bot.encodeDurableEvent(decoded.value);
    case 'receipt': return bot.encodeSendReceipt(decoded.value);
    case 'helper_request': return bot.encodeHelperRequest(decoded.value);
    case 'helper_response': return bot.encodeHelperResponse(decoded.value);
    case 'keypackage_publication': return kp.encodeKeyPackagePublication(decoded.value);
    case 'keypackage_publication_receipt': return kp.encodeKeyPackagePublicationReceipt(decoded.value);
    case 'keypackage_intent': return kp.encodeKeyPackageIntent(decoded.value);
    case 'keypackage_intent_result': return kp.encodeKeyPackageIntentResult(decoded.value);
    case 'keypackage_resolution': return kp.encodeKeyPackageResolution(decoded.value);
    case 'group_registration_request': return bot.encodeBotGroupRegistrationRequest(decoded.value);
    case 'group_registration_receipt': return bot.encodeBotGroupRegistrationReceipt(decoded.value);
    case 'device_list_push': return bot.encodeDeviceListPush(decoded.value);
  }
}

function checkContext(decoded: Decoded, c: Context): void {
  const ctx = context(c);
  switch (decoded.kind) {
    case 'request': bot.validateBotContext(decoded.value, ctx); break;
    case 'response': bot.validateBotResponseContext(decoded.value, ctx.community); break;
    case 'removal': bot.validateRemovalContext(decoded.value, ctx); break;
    case 'event': bot.validateEventContext(decoded.value, ctx.community); break;
    case 'receipt':
      if (ctx.group === undefined) throw new Error('receipt vector is missing its group context');
      bot.validateReceiptContext(decoded.value, ctx.community, ctx.group);
      break;
    case 'helper_request':
    case 'helper_response': break;
  }
}

describe('shared bot protocol vectors', () => {
  it('keeps the maximum nested upload plus 33 refs within the helper payload cap', () => {
    const resultBytes = 1 + 5 + kp.MAX_KEYPACKAGE_UPLOAD_BODY + 2 + 33 * 34 + 9;
    expect(resultBytes).toBe(1_048_435);
    expect(kp.HELPER_PAYLOAD_MAX - resultBytes).toBe(45);
  });

  it('roundtrips actual maximum KeyPackage payloads and rejects upload cap plus one', () => {
    const makeUpload = (targetLength: number): Uint8Array => {
      const packages = [new Uint8Array(targetLength - 72), ...Array.from({ length: 31 }, () => new Uint8Array([0x22]))];
      const body = encode([packages, new Uint8Array([0x33])]);
      expect(body.length).toBe(targetLength);
      return body;
    };
    const refs = Array.from({ length: 33 }, (_, index) => new Uint8Array(32).fill(index + 1));
    const maximumUpload = makeUpload(kp.MAX_KEYPACKAGE_UPLOAD_BODY);
    const result = kp.encodeKeyPackageIntentResult({
      uploadBody: maximumUpload,
      packageRefs: refs,
      greatestExpiry: (1n << 63n) - 1n,
    });
    expect(result.length).toBe(1_048_435);
    expect(result.length).toBeLessThanOrEqual(kp.HELPER_PAYLOAD_MAX);
    const decodedResult = kp.decodeKeyPackageIntentResult(result);
    expect(decodedResult.uploadBody).toEqual(maximumUpload);
    expect(decodedResult.packageRefs).toEqual(refs);
    expect(decodedResult.greatestExpiry).toBe((1n << 63n) - 1n);
    expect(kp.encodeKeyPackageIntentResult(decodedResult)).toEqual(result);

    const operationId = new Uint8Array(16).fill(0x96);
    const publication = kp.encodeKeyPackagePublication({
      operationId,
      expectedGeneration: (1n << 63n) - 1n,
      uploadBody: maximumUpload,
    });
    const decodedPublication = kp.decodeKeyPackagePublication(publication);
    expect(decodedPublication.uploadBody).toEqual(maximumUpload);
    expect(kp.encodeKeyPackagePublication(decodedPublication)).toEqual(publication);

    const helper = bot.encodeHelperResponse({ requestId: operationId, status: 0, payload: result });
    expect(helper.length).toBe(1_048_460);
    expect(helper.length).toBeLessThanOrEqual(1 << 20);
    expect(bot.decodeHelperResponse(helper).payload).toEqual(result);

    const oversizedUpload = makeUpload(kp.MAX_KEYPACKAGE_UPLOAD_BODY + 1);
    expect(() => kp.encodeKeyPackagePublication({ operationId, expectedGeneration: 0n, uploadBody: oversizedUpload })).toThrow(kp.KeyPackageCodecError);
    expect(() => kp.decodeKeyPackagePublication(encode([operationId, 0n, oversizedUpload]))).toThrow();
    expect(() => kp.encodeKeyPackageIntentResult({ uploadBody: oversizedUpload, packageRefs: refs, greatestExpiry: 0n })).toThrow(kp.KeyPackageCodecError);
    expect(() => kp.decodeKeyPackageIntentResult(encode([oversizedUpload, refs, 0n]))).toThrow();
  }, 30_000);

  it('validates typed encoder inputs', () => {
    expect(() => kp.encodeKeyPackagePublication({ operationId: new Uint8Array(16), expectedGeneration: 0n, uploadBody: new Uint8Array([0x82,0x81,0x41,0x01,0xf6]) })).toThrow(kp.KeyPackageCodecError);
    expect(() => kp.encodeKeyPackageIntent({ operationId: new Uint8Array(16).fill(1), count: 33n, lastResort: 0n, expectedGeneration: 0n })).toThrow(kp.KeyPackageCodecError);
  });

  it('roundtrips every valid canonical encoding and checks contexts', async () => {
    expect(vectors.cases.length).toBeGreaterThanOrEqual(37);
    const kinds = new Set(vectors.cases.map((vector) => vector.kind));
    expect([...kinds].sort()).toEqual(['device_list_push', 'event', 'group_registration_receipt', 'group_registration_request', 'helper_request', 'helper_response', 'keypackage_intent', 'keypackage_intent_result', 'keypackage_publication', 'keypackage_publication_receipt', 'keypackage_resolution', 'receipt', 'removal', 'request', 'response']);

    for (const vector of vectors.cases) {
      const hex = vector.hex;
      if (hex === undefined) throw new Error(`valid vector ${vector.name} is missing hex`);
      const decoded = decodeVector(vector.kind, hex);
      const encoded = encodeVector(decoded);
      expect(typeof encoded === 'string' ? toHex(bodyPayload(encoded)) : toHex(encoded), vector.name).toBe(hex);

      if (vector.body !== undefined && typeof encoded === 'string') {
        expect(toHex(bodyPayload(vector.body)), vector.name).toBe(hex);
        const fromBody = decodeVector(vector.kind, '', vector.body);
        expect(encodeVector(fromBody), vector.name).toBe(vector.body);
        expect(toHex(bodyPayload(encoded)), vector.name).toBe(hex);
      }
      if (vector.context !== undefined) checkContext(decoded, vector.context);
      if (decoded.kind === 'group_registration_request') {
        if (vector.registration_body_hex === undefined) throw new Error(`registration vector ${vector.name} is missing its body`);
        expect(toHex(decoded.value.registrationBody), vector.name).toBe(vector.registration_body_hex);
        if (vector.registration_sha256 === undefined) throw new Error(`registration vector ${vector.name} is missing its hash`);
        const domain = new TextEncoder().encode('dilla bot group registration v1\0');
        const preimage = new Uint8Array(domain.length + decoded.value.registrationBody.length);
        preimage.set(domain);
        preimage.set(decoded.value.registrationBody, domain.length);
        const hash = new Uint8Array(await crypto.subtle.digest('SHA-256', preimage));
        expect(toHex(hash), vector.name).toBe(vector.registration_sha256);
      }
      if (decoded.kind === 'group_registration_receipt' && vector.registration_sha256 !== undefined) {
        expect(toHex(decoded.value.registrationSHA256), vector.name).toBe(vector.registration_sha256);
      }
      if (vector.preimage_hex !== undefined) {
        if (decoded.kind !== 'removal') throw new Error(`non-removal vector ${vector.name} has a preimage`);
        expect(toHex(bot.removalPreimage(decoded.value)), vector.name).toBe(vector.preimage_hex);
      }
    }
  });

  it('refuses every structural and context reject vector', () => {
    for (const vector of vectors.rejects) {
      const run = () => {
        if (vector.kind === 'helper_request' && vector.name.includes('length')) {
          if (vector.hex === undefined) throw new Error(`reject vector ${vector.name} is missing hex`);
          bot.parseHelperFrame(fromHex(vector.hex));
          return;
        }
        const decoded = decodeVector(vector.kind, vector.hex ?? '', vector.body);
        if (vector.context !== undefined) checkContext(decoded, vector.context);
      };
      expect(run, vector.name).toThrow(vector.kind.startsWith('keypackage_') ? undefined : bot.BotError);
    }
  });
});
