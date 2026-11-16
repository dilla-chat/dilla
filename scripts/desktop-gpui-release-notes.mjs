#!/usr/bin/env node
// The release notes of a desktop-gpui-v* release (GPUI release plan, Task 6): the alpha banner first, then how to
// install each asset, how to check them, and the pull requests merged since the previous desktop-gpui-v* tag.
//
// Usage: desktop-gpui-release-notes.mjs <version> <commit>   (in a full clone: it reads the tags and the log)
import { execFileSync } from 'node:child_process';
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { linuxPackageVersion } from './desktop-gpui-aur.mjs';

export const REPO = 'dilla-chat/dilla';
export const TAG_PREFIX = 'desktop-gpui-v';
// Task 7's banner (owner-approved wording): first in every alpha's notes.
export const BANNER = '**Alpha of the new native Dilla desktop app.** Installing the deb, rpm or AUR package replaces Dilla beta. '
  + 'Your old app data stays on disk but is not used. Dilla signs in as a new device, so keep another signed-in device at hand '
  + 'to link it. It does not update itself yet; download new alphas from this page.';

/** The five Linux assets with the command that installs each; every asset of the release has a row. */
export function installRows(version) {
  linuxPackageVersion(version);
  const f = (ext) => `dilla-${version}-linux-x64.${ext}`;
  return [
    ['Ubuntu 24.04+, Debian 13', f('deb'), `sudo apt install ./${f('deb')}`],
    ['Fedora 40+', f('rpm'), `sudo dnf install ./${f('rpm')}`],
    ['Arch (AUR recipe, built locally)', f('aur.tar.gz'), `mkdir dilla-bin && tar -xzf ${f('aur.tar.gz')} -C dilla-bin && cd dilla-bin && makepkg -si`],
    ['Any distribution (AppImage)', f('AppImage'), `chmod +x ${f('AppImage')} && ./${f('AppImage')}`],
    ['Any distribution (archive)', f('tar.gz'), `tar -xzf ${f('tar.gz')} && ./dilla-${version}/dilla-desktop`],
  ];
}

/** Pull-request subjects (`Title (#123)`) from the commit subjects, newest first, deduplicated. */
export function pullRequests(subjects) {
  const seen = new Set();
  return subjects.map((s) => s.trim()).filter((s) => /\(#\d+\)$/.test(s) && !seen.has(s) && seen.add(s));
}

export function releaseNotes(version, { subjects = [], previousTag = null } = {}) {
  const rows = installRows(version);
  const prs = pullRequests(subjects);
  return [
    BANNER,
    '',
    '## Install',
    '',
    '| System | File | Install |',
    '|---|---|---|',
    ...rows.map(([system, file, command]) => `| ${system} | \`${file}\` | \`${command}\` |`),
    '',
    'The AUR recipe is a release asset only: it is not published to the AUR. The packages need glibc 2.39 or newer, Vulkan '
      + 'or OpenGL ES 3, PipeWire or PulseAudio for calls, and a running Secret Service (GNOME Keyring, KWallet or KeePassXC): '
      + 'without one, Dilla cannot store its database key and cannot sign in. See [the desktop guide]'
      + `(https://github.com/${REPO}/blob/main/docs/user/desktop.md).`,
    '',
    '## Check the download',
    '',
    '```sh',
    'sha256sum --check --ignore-missing SHA256SUMS',
    `gh attestation verify dilla-${version}-linux-x64.AppImage --repo ${REPO}`,
    '```',
    '',
    `## Changes${previousTag ? ` since ${previousTag}` : ''}`,
    '',
    ...(prs.length ? prs.map((p) => `- ${p}`) : ['- The first native desktop alpha.']),
    '',
  ].join('\n');
}

/** The newest desktop-gpui-v* tag (other than `tag`) whose commit is an ancestor of `commit`, or null. */
function previousTag(tag, commit) {
  const tags = execFileSync('git', ['tag', '--list', `${TAG_PREFIX}*`, '--sort=-v:refname'], { encoding: 'utf8' }).split('\n').filter(Boolean);
  for (const t of tags) {
    if (t === tag) continue;
    try { execFileSync('git', ['merge-base', '--is-ancestor', t, commit]); return t; } catch { /* not an ancestor */ }
  }
  return null;
}

// Run directly, on every OS: a Windows path (D:\a\...) or one with a space never equals `file://` + argv[1].
if (process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url) {
  const [version, commit] = process.argv.slice(2);
  if (!version || !/^[0-9a-f]{40}$/.test(commit ?? '')) {
    console.error('usage: desktop-gpui-release-notes.mjs <version> <commit sha>');
    process.exit(1);
  }
  const prev = previousTag(`${TAG_PREFIX}${version}`, commit);
  const range = prev ? `${prev}..${commit}` : commit;
  // Without a previous alpha, only this commit's subject: the whole history is not this release's change list.
  const subjects = execFileSync('git', ['log', '--format=%s', ...(prev ? [] : ['-1']), range], { encoding: 'utf8' }).split('\n');
  process.stdout.write(releaseNotes(version, { subjects, previousTag: prev }));
}
