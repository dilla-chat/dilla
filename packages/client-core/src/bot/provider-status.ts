import { arr, decode, encode, u64, type CborInput, type CborValue } from '../cbor/index.ts';

export type MusicProviderAccount = 'unknown' | 'not_linked' | 'linked' | 'needs_attention';
export type MusicProviderSetup = 'unknown' | 'configured' | 'missing_requirements';
export type MusicProviderReason = 'none' | 'missing_credentials' | 'invalid_credentials' | 'binding_mismatch'
  | 'account_mismatch' | 'helper_missing' | 'helper_invalid' | 'state_directory_unavailable'
  | 'dependencies_missing' | 'unsupported_bot' | 'unavailable';

/** Fresh, bounded operation 24 response. `observedAt` is a Unix timestamp in seconds. */
export interface MusicProviderStatusSnapshot {
  version: 1;
  installationId: Uint8Array;
  observedAt: bigint;
  spotifyAccount: MusicProviderAccount;
  spotifyAccountReason: MusicProviderReason;
  spotifyPlayback: MusicProviderSetup;
  spotifyPlaybackReason: MusicProviderReason;
  youtubePlayback: MusicProviderSetup;
  youtubePlaybackReason: MusicProviderReason;
}

const accounts: MusicProviderAccount[] = ['unknown', 'not_linked', 'linked', 'needs_attention'];
const setup: MusicProviderSetup[] = ['unknown', 'configured', 'missing_requirements'];
const reasons: MusicProviderReason[] = ['none', 'missing_credentials', 'invalid_credentials', 'binding_mismatch',
  'account_mismatch', 'helper_missing', 'helper_invalid', 'state_directory_unavailable',
  'dependencies_missing', 'unsupported_bot', 'unavailable'];
const enumValue = <T extends string>(value: unknown, values: T[]): T => {
  const n = u64(value as CborValue);
  if (n >= BigInt(values.length)) throw new TypeError('invalid music provider status enum');
  return values[Number(n)];
};
const idBytes = (value: unknown): Uint8Array => {
  if (!(value instanceof Uint8Array) || value.length !== 16 || value.every(byte => byte === 0)) throw new TypeError('invalid installation id');
  return value;
};

/** Decode the bounded overview capability tuple without assuming a Bot protocol version. */
export function decodeMusicProviderCapability(data: Uint8Array): boolean {
  if (!(data instanceof Uint8Array) || data.length > 128) throw new TypeError('invalid overview capability payload');
  const fields = arr(decode(data));
  if (fields.length < 1) throw new TypeError('invalid overview capability shape');
  const version = u64(fields[0]);
  if (version !== 1n && version !== 2n) throw new TypeError('unsupported overview capability version');
  if (fields.length !== (version === 1n ? 5 : 6)) throw new TypeError('invalid overview capability shape');
  idBytes(fields[1]);
  u64(fields[2]);
  if ((u64(fields[3]) !== 0n && u64(fields[3]) !== 1n) || (u64(fields[4]) !== 0n && u64(fields[4]) !== 1n)) {
    throw new TypeError('invalid overview capability flags');
  }
  if (version === 1n) return false;
  const operations = arr(fields[5]);
  if (operations.length > 32) throw new TypeError('overview capability operation list exceeds limit');
  const seen = new Set<bigint>();
  let supportsMusicProviderStatus = false;
  for (const field of operations) {
    const operation = u64(field);
    if (operation > 255n || seen.has(operation)) throw new TypeError('invalid overview capability operation');
    seen.add(operation);
    if (operation === 24n) supportsMusicProviderStatus = true;
  }
  return supportsMusicProviderStatus;
}

/** Decode operation 24's `[version, installation, observedAt, ...fixed enums]` CBOR tuple. */
export function decodeMusicProviderStatus(data: Uint8Array): MusicProviderStatusSnapshot {
  if (!(data instanceof Uint8Array) || data.length > 512) throw new TypeError('invalid music provider status payload');
  const fields = arr(decode(data), 9);
  if (u64(fields[0]) !== 1n) throw new TypeError('unsupported music provider status version');
  return { version: 1, installationId: idBytes(fields[1]), observedAt: u64(fields[2]),
    spotifyAccount: enumValue(fields[3], accounts), spotifyAccountReason: enumValue(fields[4], reasons),
    spotifyPlayback: enumValue(fields[5], setup), spotifyPlaybackReason: enumValue(fields[6], reasons),
    youtubePlayback: enumValue(fields[7], setup), youtubePlaybackReason: enumValue(fields[8], reasons) };
}

/** Encode operation 24's bounded response tuple; exported for vector and adapter tests. */
export function encodeMusicProviderStatus(snapshot: MusicProviderStatusSnapshot): Uint8Array {
  const field = <T extends string>(value: T, values: T[]): bigint => {
    const index = values.indexOf(value);
    if (index < 0) throw new TypeError('invalid music provider status enum');
    return BigInt(index);
  };
  const value: CborInput[] = [1n, idBytes(snapshot.installationId), u64(snapshot.observedAt),
    field(snapshot.spotifyAccount, accounts), field(snapshot.spotifyAccountReason, reasons),
    field(snapshot.spotifyPlayback, setup), field(snapshot.spotifyPlaybackReason, reasons),
    field(snapshot.youtubePlayback, setup), field(snapshot.youtubePlaybackReason, reasons)];
  const encoded = encode(value);
  if (encoded.length > 512) throw new TypeError('music provider status payload exceeds limit');
  return encoded;
}
