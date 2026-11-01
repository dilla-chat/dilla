import { describe, expect, it } from 'vitest';
import * as bot from './protocol.ts';
import { decodeMusicProviderStatus, encodeMusicProviderStatus } from './provider-status.ts';
import { decode, encode } from '../cbor/index.ts';

const id = (n: number) => new Uint8Array(16).fill(n);
const request = (operation = 0, args: (bigint | Uint8Array | string | null)[] = []) => ({
  requestId: id(1), communityId: id(2), channelId: null, operation, revision: null, expires: null, args,
});

describe('bot application codecs', () => {
  it('roundtrips a request and checks authenticated community context', () => {
    const body = bot.encodeBotRequest(request());
    const decoded = bot.decodeBotRequest(body);
    expect(decoded.operation).toBe(0);
    expect(bot.validateBotContext(decoded, { community: id(2), now: 100n })).toBeUndefined();
    expect(() => bot.validateBotContext(decoded, { community: id(3), now: 100n })).toThrow(bot.BotError);
  });

  it('encodes provider status as a channel-free read with no arguments', () => {
    const status = request(24);
    expect(bot.decodeBotRequest(bot.encodeBotRequest(status))).toMatchObject({ operation: 24, channelId: null, revision: null, expires: null, args: [] });
    expect(() => bot.encodeBotRequest({ ...status, channelId: id(3) })).toThrow(bot.BotError);
    expect(() => bot.encodeBotRequest({ ...status, args: [1n] })).toThrow(bot.BotError);
  });

  it('preserves uint64 values exactly and rejects wrong operation arguments', () => {
    const max = (1n << 64n) - 1n;
    const seek = { ...request(17, [max]), channelId: id(3), revision: max, expires: 110n };
    const decoded = bot.decodeBotRequest(bot.encodeBotRequest(seek));
    expect(decoded.args[0]).toBe(max);
    expect(() => bot.encodeBotRequest({ ...request(13, [1n]), channelId: id(3), revision: 1n, expires: 110n })).toThrow(bot.BotError);
  });

  it('accepts null first-page and bounded opaque moderation cursors', () => {
    expect(bot.decodeBotRequest(bot.encodeBotRequest(request(60, [null]))).args).toEqual([null]);
    const cursor = new Uint8Array([1, 2, 3]);
    expect(bot.decodeBotRequest(bot.encodeBotRequest(request(60, [cursor]))).args).toEqual([cursor]);
    expect(() => bot.encodeBotRequest(request(60, [new Uint8Array()]))).toThrow(bot.BotError);
    expect(() => bot.encodeBotRequest(request(60, [new Uint8Array(257)]))).toThrow(bot.BotError);
    expect(() => bot.encodeBotRequest(request(60, [0n]))).toThrow(bot.BotError);
  });

  it('accepts bounded manual evidence listing cursors', () => {
    expect(bot.decodeBotRequest(bot.encodeBotRequest(request(64, [null]))).args).toEqual([null]);
    const cursor = new Uint8Array([1, 2, 3]);
    expect(bot.decodeBotRequest(bot.encodeBotRequest(request(64, [cursor]))).args).toEqual([cursor]);
    expect(() => bot.encodeBotRequest(request(64, [new Uint8Array(257)]))).toThrow(bot.BotError);
  });

  it('uses the same three-field manual evidence wire tuple for receipt-validated warnings and removals', () => {
    const remove = { ...request(61, [id(3), 'manual removal', 'excerpt']), revision: 0n, expires: 110n };
    expect(bot.decodeBotRequest(bot.encodeBotRequest(remove)).args).toEqual([id(3), 'manual removal', 'excerpt']);
    const warn = { ...request(61, [id(3), 'manual warning', '']), revision: 0n, expires: 110n };
    expect(bot.decodeBotRequest(bot.encodeBotRequest(warn)).args).toEqual([id(3), 'manual warning', '']);
    expect(() => bot.encodeBotRequest({ ...warn, args: [id(3), 'warn', 'manual warning', ''] })).toThrow(bot.BotError);
  });

  it('accepts a bounded role-panel publish request and rejects invalid mappings', () => {
    const mappings = encode([['🎮', id(3), 'Gamers']]);
    const publish = { ...request(31, [id(4), mappings]), revision: 0n, expires: 110n };
    expect(bot.decodeBotRequest(bot.encodeBotRequest(publish)).args).toEqual([id(4), mappings]);
    expect(() => bot.encodeBotRequest({ ...publish, revision: 1n })).toThrow(bot.BotError);
    expect(() => bot.encodeBotRequest({ ...publish, args: [id(4), encode([])] })).toThrow(bot.BotError);
    expect(() => bot.encodeBotRequest({ ...publish, args: [id(4), encode([['✅', id(3), 'One'], ['✅', id(4), 'Two']])] })).toThrow(bot.BotError);
    expect(() => bot.encodeBotRequest({ ...publish, args: [id(4), encode([['\n', id(3), 'Gamers']])] })).toThrow(bot.BotError);
  });

  it('binds a response to the exact local request and community', () => {
    const response: bot.BotResponse = {
      requestId: id(1), communityId: id(2), status: 0, revision: null, data: new Uint8Array(),
    };
    expect(() => bot.validateBotResponseForRequest(response, id(2), id(1))).not.toThrow();
    expect(() => bot.validateBotResponseForRequest(response, id(3), id(1))).toThrow(bot.BotError);
    expect(() => bot.validateBotResponseForRequest(response, id(2), id(4))).toThrow(bot.BotError);
  });

  it('encodes the strict raw request bytes required by the human interaction core boundary', () => {
    const raw = bot.encodeBotRequestBytes(request(60, [null]));
    expect(decode(raw)).toEqual([1n, id(1), id(2), null, 60n, null, null, [null]]);
    expect(() => bot.encodeBotRequestBytes(request(999, []))).toThrow(bot.BotError);
    const mutation = { ...request(61, [id(3), '', '']), revision: 1n, expires: 110n };
    const decoded = bot.decodeBotRequest(bot.encodeBotRequest(mutation));
    expect(() => bot.validateBotContext(decoded, { community: id(2), now: 110n })).toThrow(bot.BotError);
  });

  it('rejects noncanonical base64url even when it decodes to valid CBOR', () => {
    const good = bot.encodeBotRequest(request());
    expect(() => bot.decodeBotRequest(`${good}=`)).toThrow(bot.BotError);
    expect(decode).toBeTypeOf('function');
  });

  it('binds removal proofs to an expected actor when one is supplied', () => {
    const proof = {
      requestId: id(1), actorUser: id(2), actorDevice: id(3), communityId: id(4), groupId: id(5),
      targetSeq: 1n, expires: 120n,
    };
    expect(() => bot.validateRemovalContext(proof, {
      community: id(4), group: id(5), actorUser: id(9), actorDevice: id(3), now: 100n,
    })).toThrow(bot.BotError);
    expect(() => bot.validateRemovalContext(proof, {
      community: id(4), group: id(5), actorUser: id(2), actorDevice: id(9), now: 100n,
    })).toThrow(bot.BotError);
  });

  it('validates send receipt encoder values and preserves the exact original response', () => {
    const responseBody = encode([1n, new Uint8Array(32), 0n]);
    const receipt: bot.SendReceipt = {
      operationId: id(1), attemptId: id(2), predecessorAttemptId: null,
      communityId: id(3), groupId: id(4), expectedGeneration: 0n,
      ciphertextSha256: new Uint8Array(32), class: 0,
      outcome: bot.SendOutcome.Accepted, epoch: 0n,
      sendResponse: { sequence: 1n, frankingTag: new Uint8Array(32), receiveTimestamp: 0n, body: responseBody },
    };
    const encoded = bot.encodeSendReceipt(receipt);
    const decoded = bot.decodeSendReceipt(encoded);
    expect(decoded.sendResponse?.body).toEqual(responseBody);
    expect(decoded.outcome).toBe(bot.SendOutcome.Accepted);

    expect(() => bot.encodeSendReceipt({ ...receipt, class: 2 })).toThrow(bot.BotError);
    expect(() => bot.encodeSendReceipt({ ...receipt, expectedGeneration: 1n << 63n })).toThrow(bot.BotError);
    expect(() => bot.encodeSendReceipt({ ...receipt, sendResponse: { ...receipt.sendResponse!, sequence: 2n } })).toThrow(bot.BotError);
    expect(() => bot.encodeSendReceipt({ ...receipt, outcome: bot.SendOutcome.UnknownAfterRestore })).toThrow(bot.BotError);
  });

  it('roundtrips managed group registration and binds its receipt to the request', async () => {
    const registrationBody = encode([id(5), new Uint8Array([1]), new Uint8Array([2]), new Uint8Array([3])]);
    const request = { operationId: id(6), expectedGeneration: (1n << 63n) - 1n, registrationBody };
    const encodedRequest = bot.encodeBotGroupRegistrationRequest(request);
    expect(bot.decodeBotGroupRegistrationRequest(encodedRequest)).toEqual(request);

    const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', new Uint8Array([
      ...new TextEncoder().encode('dilla bot group registration v1\0'), ...registrationBody,
    ])));
    const receipt = {
      operationId: id(6), groupId: id(5), registrationSHA256: digest,
      acceptedGeneration: request.expectedGeneration, initialEpoch: 0n, nextSequence: 1n,
    };
    const encodedReceipt = bot.encodeBotGroupRegistrationReceipt(receipt);
    const decodedReceipt = bot.decodeBotGroupRegistrationReceipt(encodedReceipt);
    expect(decodedReceipt).toEqual(receipt);
    await expect(bot.validateBotGroupRegistrationReceipt(decodedReceipt, request)).resolves.toBeUndefined();
    await expect(bot.validateBotGroupRegistrationReceipt({ ...decodedReceipt, registrationSHA256: new Uint8Array(32) }, request)).rejects.toThrow(bot.BotError);
  });

  it('rejects invalid managed group registration bounds and bodies', () => {
    const body = encode([id(5), new Uint8Array(), new Uint8Array(), new Uint8Array()]);
    const request = { operationId: id(6), expectedGeneration: 0n, registrationBody: body };
    expect(() => bot.encodeBotGroupRegistrationRequest({ ...request, operationId: new Uint8Array(16) })).toThrow(bot.BotError);
    expect(() => bot.encodeBotGroupRegistrationRequest({ ...request, expectedGeneration: 1n << 63n })).toThrow(bot.BotError);
    expect(() => bot.encodeBotGroupRegistrationRequest({ ...request, registrationBody: new Uint8Array() })).toThrow(bot.BotError);
    expect(() => bot.encodeBotGroupRegistrationRequest({ ...request, registrationBody: encode([new Uint8Array(16), new Uint8Array(), new Uint8Array(), new Uint8Array()]) })).toThrow(bot.BotError);
    expect(() => bot.encodeBotGroupRegistrationRequest({ ...request, registrationBody: encode([id(5), 1n, new Uint8Array(), new Uint8Array()]) })).toThrow(bot.BotError);
    expect(() => bot.decodeBotGroupRegistrationRequest(new Uint8Array([0x83, 0x50, ...new Uint8Array(16), 0, 0x58, 1, 0x00]))).toThrow(bot.BotError);
    const receipt = { operationId: id(6), groupId: id(5), registrationSHA256: new Uint8Array(31), acceptedGeneration: 0n, initialEpoch: 0n, nextSequence: 1n };
    expect(() => bot.encodeBotGroupRegistrationReceipt(receipt)).toThrow(bot.BotError);
    expect(() => bot.encodeBotGroupRegistrationReceipt({ ...receipt, registrationSHA256: new Uint8Array(32), acceptedGeneration: 1n << 63n })).toThrow(bot.BotError);
    expect(() => bot.encodeBotGroupRegistrationReceipt({ ...receipt, registrationSHA256: new Uint8Array(32), nextSequence: 0n })).toThrow(bot.BotError);
  });
});

describe('music provider status', () => {
  it('roundtrips a bounded versioned snapshot and rejects unknown enums', () => {
    const snapshot = { version: 1 as const, installationId: id(9), observedAt: 1_800_000_000n,
      spotifyAccount: 'linked' as const, spotifyAccountReason: 'none' as const,
      spotifyPlayback: 'configured' as const, spotifyPlaybackReason: 'none' as const,
      youtubePlayback: 'missing_requirements' as const, youtubePlaybackReason: 'dependencies_missing' as const };
    expect(decodeMusicProviderStatus(encodeMusicProviderStatus(snapshot))).toEqual(snapshot);
    expect(() => decodeMusicProviderStatus(encode([1n, id(9), 1n, 99n, 0n, 1n, 0n, 2n, 1n]))).toThrow();
  });
});


it('roundtrips optional expected-current skip arguments and rejects malformed guards', () => {
  for (const args of [[], ['ab'.repeat(16)], ['']]) {
    const input = { ...request(15, args), channelId: id(3), revision: 0n, expires: 110n };
    expect(bot.decodeBotRequest(bot.encodeBotRequest(input)).args).toEqual(args);
  }
  for (const args of [[null], [1n], ['bad-id'], ['ab'.repeat(16), 'cd'.repeat(16)]]) {
    expect(() => bot.encodeBotRequest({ ...request(15, args), channelId: id(3), revision: 0n, expires: 110n })).toThrow(bot.BotError);
  }
});
