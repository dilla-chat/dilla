import { describe, expect, it } from 'vitest';
import { encode } from '../cbor/index.ts';
import { decodeMusicProviderCapability } from '../index.ts';

const actor = new Uint8Array(16).fill(1);
const overviewV1 = () => encode([1n, actor, 0n, 1n, 0n]);
const overviewV2 = (operations: (number | bigint)[] = [24]) => encode([2n, actor, 0n, 1n, 0n, operations]);

describe('decodeMusicProviderCapability', () => {
  it('treats the legacy five-field overview as not supporting provider status', () => {
    expect(decodeMusicProviderCapability(overviewV1())).toBe(false);
  });

  it('requires explicit operation 24 in a valid version 2 overview', () => {
    expect(decodeMusicProviderCapability(overviewV2([23, 24, 1]))).toBe(true);
    expect(decodeMusicProviderCapability(overviewV2([23, 1]))).toBe(false);
  });

  it('rejects malformed or oversized capability tuples', () => {
    expect(() => decodeMusicProviderCapability(encode([2n, actor, 0n, 1n, 0n]))).toThrow();
    expect(() => decodeMusicProviderCapability(encode([1n, actor, 0n, 2n, 0n]))).toThrow();
    expect(() => decodeMusicProviderCapability(encode([1n, new Uint8Array(16), 0n, 1n, 0n]))).toThrow();
    expect(() => decodeMusicProviderCapability(overviewV2([24, 24]))).toThrow();
    expect(() => decodeMusicProviderCapability(overviewV2(Array.from({ length: 33 }, (_, index) => index)))).toThrow();
    expect(() => decodeMusicProviderCapability(overviewV2([256]))).toThrow();
    expect(() => decodeMusicProviderCapability(new Uint8Array(129))).toThrow();
  });
});
