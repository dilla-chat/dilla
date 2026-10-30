// Wave B task 3b, security review LOW-1: the saved key's file name takes a slug of the instance's name.
import { describe, expect, it } from 'vitest';
import { instanceSlug, recoveryKeyFile } from './recovery-file.ts';

describe('instanceSlug', () => {
  it('keeps a plain host name', () => {
    expect(instanceSlug('dilla.thim.dev')).toBe('dilla.thim.dev');
    expect(instanceSlug('Midgard Crew')).toBe('midgard-crew');
  });
  it('drops a bidi override, so nothing reorders the name in a download list', () => {
    const rlo = `evil${String.fromCodePoint(0x202e)}txt.exe`;
    expect(instanceSlug(rlo)).toBe('evil-txt.exe');
    expect(instanceSlug(rlo)).not.toMatch(/[‪-‮⁦-⁩]/u);
  });
  it('turns a path into a name, without leading dots or hyphens', () => {
    expect(instanceSlug('../../etc/passwd')).toBe('etc-passwd');
    expect(instanceSlug('...')).toBe('dilla');
  });
  it('caps a long name at 64 characters and trims what the cut leaves at its end', () => {
    expect(instanceSlug('a'.repeat(300))).toHaveLength(64);
    const cut = instanceSlug(`${'b'.repeat(63)}-${'c'.repeat(10)}`);
    expect(cut).toBe('b'.repeat(63));
  });
  it('falls back to dilla when nothing is left', () => {
    expect(instanceSlug('')).toBe('dilla');
    expect(instanceSlug('   ')).toBe('dilla');
    expect(instanceSlug('ąęł🔑')).toBe('dilla');
  });
  it('folds full-width and compatibility characters first (NFKC)', () => {
    expect(instanceSlug('ｄｉｌｌａ．ｔｅｓｔ')).toBe('dilla.test');
  });
});

describe('recoveryKeyFile', () => {
  it('names the file by the slug, keeps the name as written in the text, and carries the key once', () => {
    const groups = ['7K2M', 'QX9D', 'H4TB'];
    const file = recoveryKeyFile(`Mid${String.fromCodePoint(0x202e)}gard`, groups);
    expect(file.name).toBe('dilla-recovery-key-mid-gard.txt');
    expect(file.text.split('7K2M-QX9D-H4TB')).toHaveLength(2);
    expect(file.text).toContain(`Your dilla recovery key for Mid${String.fromCodePoint(0x202e)}gard.`);
    expect(file.text.endsWith('\n\n7K2M-QX9D-H4TB\n')).toBe(true);
  });
});
