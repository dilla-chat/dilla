import { describe, it, expect } from 'vitest';
import { deriveKeyPair, sealBase, openBase } from './hpke.ts';
import { fromHex, hex } from './bytes.ts';

// RFC 9180 appendix A.1.1: DHKEM(X25519, HKDF-SHA256), HKDF-SHA256, AES-128-GCM, base mode.
const A1 = {
  info: '4f6465206f6e2061204772656369616e2055726e',
  ikmE: '7268600d403fce431561aef583ee1613527cff655c1343f29812e66706df3234',
  pkEm: '37fda3567bdbd628e88668c3c8d7e97d1d1253b6d4ea6d44c150f741f1bf4431',
  skEm: '52c4a758a802cd8b936eceea314432798d5baf2d7e9235dc084ab1b9cfa2f736',
  ikmR: '6db9df30aa07dd42ee5e8181afdb977e538f5e1fec8a06223f33f7013e525037',
  pkRm: '3948cfe0ad1ddb695d780e59077195da6c56506b027329794ab02bca80815c4d',
  skRm: '4612c550263fc8ad58375df3f557aac531d26850903e55a9f23f21d8534e8ac8',
  pt: '4265617574792069732074727574682c20747275746820626561757479',
  aad: '436f756e742d30',
  ct: 'f938558b5d72f1a23810b4be2ab4f84331acc02fc97babc53a52ae8218a355a96d8770ac83d07bea87e13c512a',
};

describe('hpke', () => {
  it('RFC 9180 A.1', async () => {
    const e = await deriveKeyPair(fromHex(A1.ikmE));
    const r = await deriveKeyPair(fromHex(A1.ikmR));
    expect(hex(e.sk)).toBe(A1.skEm);
    expect(hex(e.pk)).toBe(A1.pkEm);
    expect(hex(r.sk)).toBe(A1.skRm);
    expect(hex(r.pk)).toBe(A1.pkRm);
    const { enc, ct } = await sealBase(r.pk, fromHex(A1.info), fromHex(A1.aad), fromHex(A1.pt), fromHex(A1.ikmE));
    expect(hex(enc)).toBe(A1.pkEm);
    expect(hex(ct)).toBe(A1.ct);
    expect(hex(await openBase(r.sk, enc, fromHex(A1.info), fromHex(A1.aad), ct))).toBe(A1.pt);
    const flipped = ct.slice(); flipped[0]! ^= 1;
    await expect(openBase(r.sk, enc, fromHex(A1.info), fromHex(A1.aad), flipped)).rejects.toThrow();
  });
});
