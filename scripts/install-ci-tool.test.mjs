import assert from 'node:assert/strict';
import { existsSync, mkdtempSync, readFileSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';
import test from 'node:test';

const script = join(dirname(fileURLToPath(import.meta.url)), 'install-ci-tool.sh');

test('an unapproved tool is refused before any download', () => {
  const result = spawnSync('bash', [script, 'unreviewed-installer'], { encoding: 'utf8' });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /unapproved CI tool/);
});

test('a changed publisher archive is refused before extraction', () => {
  const dir = mkdtempSync(join(tmpdir(), 'dilla-tool-test-'));
  const archive = join(dir, 'wrong.tar.gz');
  writeFileSync(archive, 'wrong archive bytes');
  try {
    const result = spawnSync('bash', [script, 'binaryen', '--archive', archive], {
      encoding: 'utf8', env: { ...process.env, RUNNER_TEMP: dir },
    });
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /SHA-256 mismatch/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

// The GPUI desktop release's packaging tools (desktop-gpui-release.yml): each refuses a changed asset.
for (const tool of ['appimagetool', 'appimage-runtime', 'nfpm']) {
  test(`a changed ${tool} asset is refused before it is installed`, () => {
    const dir = mkdtempSync(join(tmpdir(), 'dilla-tool-test-'));
    const archive = join(dir, 'wrong');
    writeFileSync(archive, 'wrong asset bytes');
    try {
      const result = spawnSync('bash', [script, tool, '--archive', archive], {
        encoding: 'utf8', env: { ...process.env, RUNNER_TEMP: dir },
      });
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, /SHA-256 mismatch/);
      assert.equal(existsSync(join(dir, 'dilla-ci-tools', tool === 'appimage-runtime' ? 'appimage-runtime-x86_64' : tool)), false);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
}

test('appimagetool and its runtime come from dated releases, never continuous', () => {
  const source = readFileSync(script, 'utf8');
  for (const tool of ['appimagetool', 'appimage-runtime']) {
    const block = new RegExp(`^  ${tool}\\)\\n([\\s\\S]*?)\\n    ;;`, 'm').exec(source)?.[1];
    assert.ok(block, tool);
    assert.doesNotMatch(block, /continuous/, tool);
    assert.match(block, /^    sha256=[0-9a-f]{64}$/m, tool);
  }
});
