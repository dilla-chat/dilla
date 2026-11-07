import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { fromHex, toHex } from '../hex';
import {
  CborError, MAX_DEPTH, arr, bin, decode, encode, opt, str, u53, u64,
  type CborErrorCode, type CborInput, type CborValue,
} from './index';

interface FrameVectors {
  cases: { name: string; frame: string }[];
  rejects: { name: string; frame: string; code: string }[];
}
interface EnvelopeVectors {
  cases: { name: string; cbor: string; length: number }[];
  rejects: { name: string; error: string; cbor: string }[];
}

function vectors<T>(file: string): T {
  return JSON.parse(readFileSync(new URL(`../../../../protocol/vectors/${file}`, import.meta.url), 'utf8')) as T;
}
const frames = vectors<FrameVectors>('frames.json');
const envelopes = vectors<EnvelopeVectors>('envelope.json');

function failCode(run: () => unknown): CborErrorCode | 'no error' {
  try {
    run();
  } catch (e) {
    if (e instanceof CborError) return e.code;
    throw e;
  }
  return 'no error';
}

const roundTrip = (hex: string): string => toHex(encode(decode(fromHex(hex))));

function normalise(v: CborInput): CborValue {
  if (v === null || typeof v === 'string' || typeof v === 'bigint' || v instanceof Uint8Array) return v;
  if (typeof v === 'number') return BigInt(v);
  return v.map(normalise);
}

const nest = (levels: number, leaf: CborInput): CborInput => (levels === 0 ? leaf : [nest(levels - 1, leaf)]);

// RFC 8949 Appendix A values plus every head-width boundary.
const HEADS: [CborInput, string][] = [
  [0, '00'], [23, '17'], [24, '1818'], [255, '18ff'], [256, '190100'], [65535, '19ffff'],
  [65536, '1a00010000'], [4294967295, '1affffffff'], [4294967296, '1b0000000100000000'],
  [Number.MAX_SAFE_INTEGER, '1b001fffffffffffff'], [1000000000000n, '1b000000e8d4a51000'],
  [18446744073709551615n, '1bffffffffffffffff'],
  ['', '60'], ['IETF', '6449455446'], ['ü', '62c3bc'], ['水', '63e6b0b4'], ['\u{1d11e}', '64f09d849e'],
  [new Uint8Array(0), '40'], [new Uint8Array([1, 2, 3, 4]), '4401020304'],
  [new Uint8Array(24), '5818' + '00'.repeat(24)],
  [null, 'f6'], [[], '80'], [[1, [2, 3], [4, 5]], '8301820203820405'],
  [Array.from({ length: 25 }, (_, i) => i + 1), '98190102030405060708090a0b0c0d0e0f101112131415161718181819'],
];

describe('protocol/vectors/frames.json', () => {
  it('carries 42 cases and 22 rejects, two of them at the codec layer', () => {
    expect(frames.cases).toHaveLength(42);
    expect(frames.rejects).toHaveLength(22);
    expect(frames.rejects.filter((r) => r.code === 'E_FRAME_CBOR').map((r) => r.name))
      .toEqual(['non-minimal op', 'map instead of an array']);
  });

  it('decodes every case and encodes it back byte for byte', () => {
    for (const c of frames.cases) expect(roundTrip(c.frame), c.name).toBe(c.frame);
  });

  it('refuses the codec-layer rejects with the dilla code', () => {
    const expected: Record<string, CborErrorCode> = {
      'non-minimal op': 'E_CBOR_NONCANONICAL',
      'map instead of an array': 'E_CBOR_TYPE',
    };
    for (const r of frames.rejects.filter((x) => x.code === 'E_FRAME_CBOR')) {
      expect(failCode(() => decode(fromHex(r.frame))), r.name).toBe(expected[r.name]);
    }
  });

  it('round-trips the frame-layer rejects, which are well-formed items', () => {
    const frameLayer = frames.rejects.filter((r) => r.code !== 'E_FRAME_CBOR');
    expect(frameLayer).toHaveLength(20);
    for (const r of frameLayer) expect(roundTrip(r.frame), r.name).toBe(r.frame);
  });
});

describe('protocol/vectors/envelope.json', () => {
  it('round-trips every case at its stated length', () => {
    expect(envelopes.cases).toHaveLength(5);
    for (const c of envelopes.cases) {
      expect(fromHex(c.cbor).length, c.name).toBe(c.length);
      expect(roundTrip(c.cbor), c.name).toBe(c.cbor);
    }
  });

  it('round-trips every reject, which the envelope layer refuses and the codec does not', () => {
    expect(envelopes.rejects).toHaveLength(13);
    for (const r of envelopes.rejects) expect(roundTrip(r.cbor), r.name).toBe(r.cbor);
  });
});

describe('encode', () => {
  it('writes the shortest head at every boundary', () => {
    for (const [value, hex] of HEADS) expect(toHex(encode(value)), hex).toBe(hex);
  });

  it('encodes a number and the same bigint identically', () => {
    expect(encode(1000)).toEqual(encode(1000n));
    expect(encode([0, 24, 65536])).toEqual(encode([0n, 24n, 65536n]));
  });

  it('refuses values outside the subset', () => {
    const bad: [unknown, CborErrorCode][] = [
      [-1, 'E_CBOR_RANGE'], [1.5, 'E_CBOR_RANGE'], [Number.NaN, 'E_CBOR_RANGE'],
      [Number.POSITIVE_INFINITY, 'E_CBOR_RANGE'], [2 ** 53, 'E_CBOR_RANGE'],
      [-1n, 'E_CBOR_RANGE'], [2n ** 64n, 'E_CBOR_RANGE'],
      [undefined, 'E_CBOR_TYPE'], [true, 'E_CBOR_TYPE'], [{}, 'E_CBOR_TYPE'], [new Map(), 'E_CBOR_TYPE'],
      [new Uint16Array(2), 'E_CBOR_TYPE'], [[1, undefined], 'E_CBOR_TYPE'],
      ['\ud800', 'E_CBOR_UTF8'], ['a\udc00b', 'E_CBOR_UTF8'], [['ok', '\udbff'], 'E_CBOR_UTF8'],
    ];
    for (const [i, [value, code]] of bad.entries()) {
      expect(failCode(() => encode(value as CborInput)), `bad[${i}]`).toBe(code);
    }
  });

  it('nests at most MAX_DEPTH levels below the top item', () => {
    expect(MAX_DEPTH).toBe(8);
    expect(toHex(encode(nest(8, [])))).toBe('81'.repeat(8) + '80');
    expect(failCode(() => encode(nest(9, [])))).toBe('E_CBOR_DEPTH');
    expect(failCode(() => encode(nest(9, 0)))).toBe('E_CBOR_DEPTH');
  });
});

describe('decode', () => {
  it('reads every boundary head back, unsigned integers as bigint', () => {
    for (const [value, hex] of HEADS) expect(decode(fromHex(hex)), hex).toEqual(normalise(value));
  });

  it('refuses everything outside the subset with its code', () => {
    const refused: [string, CborErrorCode][] = [
      ['20', 'E_CBOR_TYPE'], ['a0', 'E_CBOR_TYPE'], ['c100', 'E_CBOR_TYPE'],
      ['f4', 'E_CBOR_TYPE'], ['f5', 'E_CBOR_TYPE'], ['f7', 'E_CBOR_TYPE'], ['f820', 'E_CBOR_TYPE'],
      ['f93c00', 'E_CBOR_TYPE'], ['fa47c35000', 'E_CBOR_TYPE'], ['fb3ff199999999999a', 'E_CBOR_TYPE'],
      ['5f4101ff', 'E_CBOR_TYPE'], ['7f6161ff', 'E_CBOR_TYPE'], ['9f01ff', 'E_CBOR_TYPE'],
      ['1c', 'E_CBOR_TYPE'], ['ff', 'E_CBOR_TYPE'],
      ['1817', 'E_CBOR_NONCANONICAL'], ['1900ff', 'E_CBOR_NONCANONICAL'], ['1a0000ffff', 'E_CBOR_NONCANONICAL'],
      ['1b00000000ffffffff', 'E_CBOR_NONCANONICAL'], ['580100', 'E_CBOR_NONCANONICAL'],
      ['780161', 'E_CBOR_NONCANONICAL'], ['9800', 'E_CBOR_NONCANONICAL'],
      ['', 'E_CBOR_TRUNCATED'], ['18', 'E_CBOR_TRUNCATED'], ['1901', 'E_CBOR_TRUNCATED'],
      ['430102', 'E_CBOR_TRUNCATED'], ['8201', 'E_CBOR_TRUNCATED'],
      ['9bffffffffffffffff', 'E_CBOR_TRUNCATED'], ['5bffffffffffffffff', 'E_CBOR_TRUNCATED'],
      ['0000', 'E_CBOR_TRAILING'], ['8000', 'E_CBOR_TRAILING'],
      ['62c328', 'E_CBOR_UTF8'], ['61ff', 'E_CBOR_UTF8'], ['63eda080', 'E_CBOR_UTF8'],
      ['81'.repeat(9) + '80', 'E_CBOR_DEPTH'], ['81'.repeat(8) + '8100', 'E_CBOR_DEPTH'],
    ];
    for (const [hex, code] of refused) expect(failCode(() => decode(fromHex(hex))), hex).toBe(code);
  });

  it('accepts nine nested arrays when the innermost is empty', () => {
    expect(failCode(() => decode(fromHex('81'.repeat(8) + '80')))).toBe('no error');
  });

  it('copies byte strings out of the input', () => {
    const input = fromHex('43010203');
    const value = decode(input);
    input[1] = 9;
    expect(value).toEqual(new Uint8Array([1, 2, 3]));
  });

  it('keeps a leading byte order mark in text', () => {
    expect(decode(encode('﻿x'))).toBe('﻿x');
  });

  it('decodes a view into a larger buffer', () => {
    const buffer = fromHex('ff820102ff');
    expect(decode(buffer.subarray(1, 4))).toEqual([1n, 2n]);
  });
});

describe('CborError', () => {
  it('is an Error carrying its code', () => {
    let caught: unknown = null;
    try {
      decode(fromHex('a0'));
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(Error);
    expect(caught).toBeInstanceOf(CborError);
    expect(caught).toMatchObject({ name: 'CborError', code: 'E_CBOR_TYPE' });
    expect((caught as CborError).message.startsWith('E_CBOR_TYPE: ')).toBe(true);
  });
});

describe('accessors', () => {
  const fields = arr(decode(fromHex('8401' + '43aabbcc' + '6178' + 'f6')));

  it('arr checks the shape and, when asked, the length', () => {
    expect(fields).toHaveLength(4);
    expect(arr(decode(fromHex('80')), 0)).toEqual([]);
    expect(failCode(() => arr(fields, 3))).toBe('E_CBOR_SHAPE');
    expect(failCode(() => arr(1n))).toBe('E_CBOR_SHAPE');
    expect(failCode(() => arr(null))).toBe('E_CBOR_SHAPE');
  });

  it('u64 and u53 read unsigned integers', () => {
    expect(u64(fields[0])).toBe(1n);
    expect(failCode(() => u64('1'))).toBe('E_CBOR_SHAPE');
    expect(u53(9007199254740991n)).toBe(Number.MAX_SAFE_INTEGER);
    expect(failCode(() => u53(9007199254740992n))).toBe('E_CBOR_RANGE');
    expect(failCode(() => u53(null))).toBe('E_CBOR_SHAPE');
  });

  it('bin and str read byte and text strings', () => {
    expect(bin(fields[1], 3)).toEqual(new Uint8Array([0xaa, 0xbb, 0xcc]));
    expect(bin(fields[1])).toHaveLength(3);
    expect(failCode(() => bin(fields[1], 4))).toBe('E_CBOR_SHAPE');
    expect(failCode(() => bin('x'))).toBe('E_CBOR_SHAPE');
    expect(str(fields[2])).toBe('x');
    expect(failCode(() => str(new Uint8Array(0)))).toBe('E_CBOR_SHAPE');
  });

  it('opt maps null to null and reads anything else', () => {
    expect(opt(fields[3], str)).toBeNull();
    expect(opt(fields[2], str)).toBe('x');
    expect(failCode(() => opt(1n, str))).toBe('E_CBOR_SHAPE');
  });
});

describe('property: decode and encode are inverse', () => {
  function xorshift(seed: number): () => number {
    let s = seed | 0;
    return () => {
      s ^= s << 13;
      s ^= s >>> 17;
      s ^= s << 5;
      return s >>> 0;
    };
  }
  const BOUNDARIES = [0n, 1n, 23n, 24n, 255n, 256n, 65535n, 65536n, 4294967295n, 4294967296n,
    9007199254740991n, 9007199254740992n, 18446744073709551615n];
  const TEXTS = ['', 'a', 'ü', '水', '\u{1d11e}', 'loot #general', 'x'.repeat(24), 'y'.repeat(300)];

  function generate(next: () => number, depth: number): CborValue {
    switch (next() % (depth >= 4 ? 4 : 5)) {
      case 0:
        return next() % 2 === 0 ? BOUNDARIES[next() % BOUNDARIES.length] : BigInt(next()) * BigInt(next());
      case 1: {
        const bytes = new Uint8Array(next() % 300);
        for (let i = 0; i < bytes.length; i++) bytes[i] = next() & 0xff;
        return bytes;
      }
      case 2:
        return TEXTS[next() % TEXTS.length];
      case 3:
        return null;
      default: {
        const items: CborValue[] = [];
        const n = next() % 6;
        for (let i = 0; i < n; i++) items.push(generate(next, depth + 1));
        return items;
      }
    }
  }

  it('round-trips 2000 generated values both ways', () => {
    const next = xorshift(0x2545f491);
    for (let i = 0; i < 2000; i++) {
      const value = generate(next, 0);
      const bytes = encode(value);
      expect(decode(bytes)).toEqual(value);
      expect(toHex(encode(decode(bytes)))).toBe(toHex(bytes));
    }
  });
});
