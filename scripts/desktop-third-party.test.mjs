import assert from 'node:assert/strict';
import { mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test, { after } from 'node:test';
import { shippedPackages, thirdPartyMarkdown, writeBundle } from './desktop-third-party.mjs';

const dir = mkdtempSync(join(tmpdir(), 'dilla-third-party-'));
after(() => rmSync(dir, { recursive: true, force: true }));

const pkg = (name, extra = {}) => ({ id: `${name}-id`, name, version: '1.0.0', license: 'MIT', repository: `https://example.invalid/${name}`, ...extra });
const normal = (to) => ({ pkg: `${to}-id`, dep_kinds: [{ kind: null }] });
const build = (to) => ({ pkg: `${to}-id`, dep_kinds: [{ kind: 'build' }] });
const dev = (to) => ({ pkg: `${to}-id`, dep_kinds: [{ kind: 'dev' }] });
function metadata(overrides = {}) {
  return {
    workspace_members: ['dilla-desktop-id', 'dilla-client-id'],
    packages: [
      pkg('dilla-desktop', { license: 'Apache-2.0' }), pkg('dilla-client', { license: 'Apache-2.0' }),
      pkg('gpui', { license: 'Apache-2.0' }), pkg('serde', { license: 'MIT OR Apache-2.0' }), pkg('cc'), pkg('insta'),
      pkg('odd|name', { license: undefined, license_file: 'LICENSE.txt', repository: undefined }),
      ...(overrides.packages ?? []),
    ],
    resolve: {
      nodes: [
        { id: 'dilla-desktop-id', deps: [normal('dilla-client'), normal('gpui'), dev('insta'), build('cc')] },
        { id: 'dilla-client-id', deps: [normal('serde'), normal('odd|name')] },
        { id: 'gpui-id', deps: [normal('serde')] },
        { id: 'serde-id', deps: [] }, { id: 'cc-id', deps: [] }, { id: 'insta-id', deps: [] }, { id: 'odd|name-id', deps: [] },
        ...(overrides.nodes ?? []),
      ],
    },
  };
}

test('only crates reached through normal dependencies ship', () => {
  assert.deepEqual(shippedPackages(metadata()).map((p) => p.name), ['dilla-client', 'gpui', 'odd|name', 'serde']);
});

test('THIRD-PARTY.md lists each shipped third-party crate once, with licence and repository', () => {
  const md = thirdPartyMarkdown(metadata());
  assert.match(md, /^\| gpui \| 1\.0\.0 \| Apache-2\.0 \| https:\/\/example\.invalid\/gpui \|$/m);
  assert.match(md, /^\| serde \| 1\.0\.0 \| MIT OR Apache-2\.0 \|/m);
  assert.match(md, /^\| odd\\\|name \| 1\.0\.0 \| see LICENSE\.txt in the crate \| https:\/\/crates\.io\/crates\/odd\\\|name \|$/m);
  assert.doesNotMatch(md, /^\| (dilla-client|dilla-desktop|cc|insta) \|/m);
  assert.equal(md.match(/^\| serde \|/gm).length, 1);
});

test('a shipped crate with no licence fails the bundle', () => {
  const m = metadata({ packages: [pkg('mystery', { license: undefined })], nodes: [{ id: 'mystery-id', deps: [] }] });
  m.resolve.nodes[0].deps.push(normal('mystery'));
  assert.throws(() => thirdPartyMarkdown(m), /mystery 1\.0\.0 declares no licence/);
});

test('the bundle holds Dilla\'s licence, the fonts\' OFL, libwebrtc\'s licence and THIRD-PARTY.md', () => {
  const webrtc = join(dir, 'webrtc-LICENSE.md');
  writeFileSync(webrtc, 'libwebrtc licence text');
  const out = join(dir, 'licenses');
  writeBundle(out, { metadata: metadata(), webrtcLicense: webrtc });
  assert.deepEqual(readdirSync(out).sort(), ['LICENSE', 'OFL-DMSerifDisplay.txt', 'OFL-JetBrainsMono.txt', 'THIRD-PARTY.md', 'libwebrtc-LICENSE.md']);
  assert.match(readFileSync(join(out, 'LICENSE'), 'utf8'), /GNU AFFERO GENERAL PUBLIC LICENSE/);
  assert.match(readFileSync(join(out, 'OFL-JetBrainsMono.txt'), 'utf8'), /SIL OPEN FONT LICENSE/i);
  assert.equal(readFileSync(join(out, 'libwebrtc-LICENSE.md'), 'utf8'), 'libwebrtc licence text');
});
