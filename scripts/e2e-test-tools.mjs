import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { chmodSync, copyFileSync, lstatSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const digest = bytes => createHash('sha256').update(bytes).digest('hex');

function context({ repoRoot = REPO_ROOT, env = process.env, platform = process.platform, arch = process.arch } = {}) {
  if (!/^[0-9a-f]{40}$/.test(env.GITHUB_SHA ?? '')) throw new Error('Tooling source requires GITHUB_SHA');
  const sourceSha = execFileSync('git', ['-C', repoRoot, 'rev-parse', 'HEAD'], { encoding: 'utf8' }).trim();
  if (sourceSha !== env.GITHUB_SHA) throw new Error('Tooling source does not match the checked-out commit');
  if (!/^\d+$/.test(env.GITHUB_RUN_ID ?? '')) throw new Error('Tooling workflow run requires GITHUB_RUN_ID');
  if (!(['linux', 'win32', 'darwin'].includes(platform)) || !(['x64', 'arm64'].includes(arch)) || (platform !== 'darwin' && arch !== 'x64')) {
    throw new Error(`Unsupported tooling platform/architecture: ${platform}/${arch}`);
  }
  return { repoRoot, env, sourceSha, runId: env.GITHUB_RUN_ID, platform, arch };
}

function toolNames(platform) {
  const suffix = platform === 'win32' ? '.exe' : '';
  return { testhost: `dilla-testhost${suffix}`, testkit: `dilla-testkit${suffix}`,
    ...(platform === 'linux' ? { bot: 'dillabot', botHelper: 'dilla-bot-core' } : {}) };
}

function regularFile(path) {
  const info = lstatSync(path);
  if (!info.isFile() || info.isSymbolicLink()) throw new Error(`Tooling file must be regular, without symlinks: ${path}`);
  return readFileSync(path);
}

function directory(path) {
  const info = lstatSync(path);
  if (!info.isDirectory() || info.isSymbolicLink()) throw new Error(`Tooling directory must not be a symlink: ${path}`);
}

/** A prebuilt bundle is opt-in and fail-closed: no missing or stale file triggers a fallback build. */
export function verifyTestTools(root, options) {
  const expected = context(options);
  root = resolve(root);
  directory(root);
  const manifest = JSON.parse(regularFile(join(root, 'manifest.json')).toString('utf8'));
  if (manifest.v !== 1) throw new Error('Unsupported tooling manifest schema');
  for (const field of ['sourceSha', 'runId', 'platform', 'arch']) {
    if (manifest[field] !== expected[field]) throw new Error(`Tooling ${field} does not match this checkout/run/platform`);
  }
  // Failed-job reruns can reuse a producer from an earlier attempt of this same run and SHA.
  if (!/^\d+$/.test(manifest.producerAttempt ?? '')) throw new Error('Tooling producer attempt is invalid');
  const names = toolNames(expected.platform);
  const filenames = Object.values(names).sort();
  if (!manifest.files || Array.isArray(manifest.files) || JSON.stringify(Object.keys(manifest.files).sort()) !== JSON.stringify(filenames)) {
    throw new Error('Tooling manifest does not name exactly the required executable files');
  }
  if (JSON.stringify(readdirSync(root).sort()) !== JSON.stringify([...filenames, 'manifest.json'].sort())) {
    throw new Error('Tooling directory contains unexpected or missing files');
  }
  const paths = {};
  for (const [role, name] of Object.entries(names)) {
    const path = join(root, name);
    const bytes = regularFile(path);
    const file = manifest.files[name];
    if (!file || file.size !== bytes.length || file.sha256 !== digest(bytes)) throw new Error(`Tooling size/hash mismatch: ${name}`);
    paths[role] = path;
  }
  // Artifact ZIP extraction loses Unix executable modes. Restore only files whose bytes were just verified.
  if (expected.platform !== 'win32') for (const path of Object.values(paths)) chmodSync(path, 0o755);
  return paths;
}

/** Build every required executable from this checkout even when compilation intermediates came from a cache. */
export function buildTestTools(root, options) {
  const c = context(options);
  if (c.platform !== process.platform || c.arch !== process.arch) throw new Error('Tooling must be built on its native platform/architecture');
  root = resolve(root);
  mkdirSync(root, { recursive: true });
  directory(root);
  const names = toolNames(c.platform);
  const run = (command, args) => execFileSync(command, args, { cwd: c.repoRoot, env: c.env, stdio: 'inherit' });
  const goBuild = (role, pkg) => run(c.env.GO ?? 'go', ['-C', c.repoRoot, 'build', '-o', join(root, names[role]), pkg]);
  const cargoBuild = (role, pkg) => {
    run(c.env.CARGO ?? 'cargo', ['build', '-p', pkg, '--release', '--locked']);
    copyFileSync(join(resolve(c.repoRoot, c.env.CARGO_TARGET_DIR ?? 'target'), 'release', names[role]), join(root, names[role]));
  };
  goBuild('testhost', './cmd/dilla-testhost');
  cargoBuild('testkit', 'dilla-testkit');
  if (c.platform === 'linux') {
    goBuild('bot', './cmd/dillabot');
    // Keep separate invocations: testkit's dilla-core feature set must not leak into the production Bot helper.
    cargoBuild('botHelper', 'dilla-bot-core');
  }
  const files = Object.fromEntries(Object.values(names).map(name => {
    const bytes = regularFile(join(root, name));
    return [name, { size: bytes.length, sha256: digest(bytes) }];
  }));
  writeFileSync(join(root, 'manifest.json'), JSON.stringify({ v: 1, sourceSha: c.sourceSha, runId: c.runId,
    producerAttempt: c.env.GITHUB_RUN_ATTEMPT ?? '1', platform: c.platform, arch: c.arch, files }, null, 2) + '\n');
  return verifyTestTools(root, options);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const [command, root] = process.argv.slice(2);
  if (!root || !['build', 'verify'].includes(command)) throw new Error('Usage: node scripts/e2e-test-tools.mjs build|verify <bundle-directory>');
  const tools = command === 'build' ? buildTestTools(root) : verifyTestTools(root);
  console.log(`Verified ${Object.keys(tools).length} native E2E tools for ${process.platform}/${process.arch} at ${resolve(root)}`);
}
