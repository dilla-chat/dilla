import assert from 'node:assert/strict';
import test from 'node:test';
import { BANNER, installRows, pullRequests, releaseNotes } from './desktop-gpui-release-notes.mjs';

const V = '0.2.0-alpha.1';
const ASSETS = ['AppImage', 'tar.gz', 'deb', 'rpm', 'aur.tar.gz'].map((ext) => `dilla-${V}-linux-x64.${ext}`);

test('the banner comes first, word for word', () => {
  const notes = releaseNotes(V);
  assert.ok(notes.startsWith(`${BANNER}\n`));
  assert.match(BANNER, /^\*\*Alpha of the new native Dilla desktop app\.\*\* Installing the deb, rpm or AUR package replaces Dilla beta\./);
  assert.match(BANNER, /It does not update itself yet; download new alphas from this page\.$/);
});

test('every asset has its own install command', () => {
  const notes = releaseNotes(V);
  assert.deepEqual(installRows(V).map(([, file]) => file).sort(), [...ASSETS].sort());
  for (const asset of ASSETS) {
    const row = notes.split('\n').find((l) => l.includes(`| \`${asset}\` |`));
    assert.ok(row, asset);
    assert.match(row, new RegExp(`\\| \`[^\`]*${asset.replace(/\./g, '\\.')}[^\`]*\` \\|$`), asset);
  }
  assert.match(notes, /sudo apt install \.\/dilla-0\.2\.0-alpha\.1-linux-x64\.deb/);
  assert.match(notes, /sudo dnf install \.\/dilla-0\.2\.0-alpha\.1-linux-x64\.rpm/);
  assert.match(notes, /makepkg -si/);
  assert.match(notes, /not published to the AUR/);
  assert.match(notes, /sha256sum --check --ignore-missing SHA256SUMS/);
  assert.match(notes, /gh attestation verify dilla-0\.2\.0-alpha\.1-linux-x64\.AppImage --repo dilla-chat\/dilla/);
});

test('the change list holds the pull requests since the previous alpha, once each', () => {
  const subjects = ['GPUI: release pipeline (#180)', 'fix a typo', 'GPUI: release pipeline (#180)', 'Bot: x (#179)', ''];
  assert.deepEqual(pullRequests(subjects), ['GPUI: release pipeline (#180)', 'Bot: x (#179)']);
  const notes = releaseNotes(V, { subjects, previousTag: 'desktop-gpui-v0.2.0-alpha.0' });
  assert.match(notes, /## Changes since desktop-gpui-v0\.2\.0-alpha\.0\n\n- GPUI: release pipeline \(#180\)\n- Bot: x \(#179\)\n/);
  assert.doesNotMatch(notes, /fix a typo/);
  assert.match(releaseNotes(V), /## Changes\n\n- The first native desktop alpha\./);
});

test('only alpha versions have notes', () => {
  assert.throws(() => releaseNotes('0.1.0-beta.5'), /unsupported release version/);
});
