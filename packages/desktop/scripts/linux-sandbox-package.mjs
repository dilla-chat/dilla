// Public-only deterministic Linux sandbox component preparation. No signing keys.
import { createHash, createPublicKey } from 'node:crypto';
import { isMain } from './is-main.mjs';

const MAX_SAFE = Number.MAX_SAFE_INTEGER;
const sha512 = bytes => createHash('sha512').update(bytes).digest('base64');
const keysExactly = (obj, keys) => obj && typeof obj === 'object' && !Array.isArray(obj) && Object.keys(obj).length === keys.length && keys.every(k => Object.hasOwn(obj, k));
function canonicalBase64(value, length) {
  if (typeof value !== 'string') return false;
  const bytes = Buffer.from(value, 'base64');
  return bytes.length === length && bytes.toString('base64') === value;
}
export function validateConfiguration(c) {
  if (!keysExactly(c, ['goVersion', 'generation', 'publishedAt', 'expiresAt', 'publicKeys', 'helper']) || !/^go[0-9]+\.[0-9]+\.[0-9]+$/.test(c.goVersion) || !Number.isSafeInteger(c.generation) || c.generation < 1 || c.generation > MAX_SAFE) throw new Error('Invalid sandbox component configuration');
  const date = s => typeof s === 'string' && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,3})?Z$/.test(s) && Number.isFinite(Date.parse(s)) && new Date(Date.parse(s)).toISOString().replace(/\.000Z$/, 'Z') === s.replace(/\.000Z$/, 'Z');
  if (!date(c.publishedAt) || !date(c.expiresAt) || Date.parse(c.expiresAt) <= Date.parse(c.publishedAt) || Date.parse(c.expiresAt) - Date.parse(c.publishedAt) > 93 * 86400000) throw new Error('Invalid sandbox component dates');
  if (!Array.isArray(c.publicKeys) || c.publicKeys.length < 1 || c.publicKeys.length > 8 || c.publicKeys.some(k => !canonicalBase64(k, 32)) || new Set(c.publicKeys).size !== c.publicKeys.length) throw new Error('Invalid sandbox public keys');
  if (!keysExactly(c.helper, ['version', 'size', 'sha512', 'sha256']) || typeof c.helper.version !== 'string' || !/^[A-Za-z0-9._-]{1,80}$/.test(c.helper.version) || !Number.isSafeInteger(c.helper.size) || c.helper.size < 1 || c.helper.size > 8 * 1024 * 1024 || !canonicalBase64(c.helper.sha512, 64) || !/^[a-f0-9]{64}$/.test(c.helper.sha256)) throw new Error('Invalid pinned sandbox helper');
  return c;
}
export function buildCapsule(config, maintainer, helper) {
  const c = validateConfiguration(config);
  if (!Buffer.isBuffer(maintainer) || !maintainer.length || maintainer.length > 32 * 1024 * 1024 || !Buffer.isBuffer(helper) || helper.length !== c.helper.size || sha512(helper) !== c.helper.sha512 || createHash('sha256').update(helper).digest('hex') !== c.helper.sha256) throw new Error('Invalid sandbox component bytes');
  const manifest = Buffer.from(JSON.stringify({ format: 1, kind: 'dilla-linux-sandbox', generation: c.generation, platform: 'linux', arch: 'x64', compatibilityEpoch: 1, sandboxApi: 1, publishedAt: c.publishedAt, expiresAt: c.expiresAt, maintainer: { size: maintainer.length, sha512: sha512(maintainer) }, helper: { size: helper.length, sha512: sha512(helper) } }));
  if (manifest.length > 65536) throw new Error('Sandbox manifest too large');
  const header = Buffer.alloc(32);
  header.write('DLSBOX1\0', 0, 'binary'); header.writeUInt32LE(manifest.length, 8); header.writeBigUInt64LE(BigInt(maintainer.length), 16); header.writeBigUInt64LE(BigInt(helper.length), 24);
  const capsule = Buffer.concat([header, manifest, maintainer, helper]);
  if (capsule.length > 48 * 1024 * 1024) throw new Error('Sandbox capsule too large');
  return { manifest, capsule, constants: { capsuleSize: capsule.length, capsuleSha512: sha512(capsule), manifestSha512: sha512(manifest), maintainerOffset: 32 + manifest.length, maintainerSize: maintainer.length, maintainerSha512: sha512(maintainer), generation: c.generation, compatibilityEpoch: 1, sandboxApi: 1, publishedAt: c.publishedAt, expiresAt: c.expiresAt } };
}


// JSON.parse alone discards duplicate members. Scan the already syntax-validated
// input recursively, comparing decoded names so escaped aliases are rejected.
export function parsePublicConfiguration(text) {
  const value = JSON.parse(text);
  let i = 0;
  const ws = () => { while (/\s/.test(text[i] ?? '') && i < text.length) i++; };
  function string() {
    const start = i++;
    while (i < text.length) {
      if (text[i] === '\\') { i += 2; continue; }
      if (text[i++] === '"') return JSON.parse(text.slice(start, i));
    }
    throw new Error('Invalid JSON string');
  }
  function scan() {
    ws();
    if (text[i] === '{') {
      i++; ws(); const seen = new Set();
      if (text[i] === '}') { i++; return; }
      for (;;) {
        ws(); const key = string();
        if (seen.has(key)) throw new Error('Duplicate public configuration member');
        seen.add(key); ws(); i++; scan(); ws();
        if (text[i++] === '}') return;
      }
    }
    if (text[i] === '[') {
      i++; ws(); if (text[i] === ']') { i++; return; }
      for (;;) { scan(); ws(); if (text[i++] === ']') return; }
    }
    if (text[i] === '"') { string(); return; }
    while (i < text.length && !/[\s,}\]]/.test(text[i])) i++;
  }
  scan();
  return value;
}

export function assertDedicatedKeys(config, trust) {
  const c = validateConfiguration(config);
  if (!trust || !Array.isArray(trust.metadataPublicKeys) || trust.metadataPublicKeys.length < 1 || !canonicalBase64(trust.sparklePublicKey, 32)) throw new Error('Invalid public release trust');
  const other = new Set([trust.sparklePublicKey]);
  for (const pem of trust.metadataPublicKeys) {
    const key = createPublicKey(pem);
    if (key.asymmetricKeyType !== 'ed25519') throw new Error('Invalid metadata public key');
    other.add(key.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64'));
  }
  if (c.publicKeys.some(k => other.has(k))) throw new Error('Sandbox maintenance keys must be separate from release keys');
}

// Replaces only the deliberately unconfigured launcher/main.go using Go's build
// overlay. Each digest is compiled as a literal, and checked before dispatch.
export function renderLauncher(config, result, compiledVersion) {
  validateAppVersion(compiledVersion);
  const c = validateConfiguration(config), { capsule, constants: k } = result;
  if (!Buffer.isBuffer(capsule) || sha512(capsule) !== k.capsuleSha512) throw new Error('Invalid generated capsule');
  const quote = JSON.stringify;
  return `// Code generated by linux-sandbox-package.mjs; DO NOT EDIT.\n//go:build linux && cgo\n\npackage main\n\nimport (\n "bytes"\n "crypto/ed25519"\n "crypto/sha512"\n "encoding/base64"\n "encoding/binary"\n "encoding/json"\n "errors"\n "fmt"\n "os"\n "github.com/dilla-chat/dilla/packages/desktop/native/linux-sandbox/frontend"\n)\n\nfunc compiledCapsule() (frontend.EmbeddedCapsule, error) {\n bad := errors.New("invalid generated sandbox capsule")\n raw, err := base64.StdEncoding.Strict().DecodeString(${quote(capsule.toString('base64'))})\n if err != nil || len(raw) != ${k.capsuleSize} || len(raw) < 32 { return frontend.EmbeddedCapsule{}, bad }\n digest := func(b []byte) string { h := sha512.Sum512(b); return base64.StdEncoding.EncodeToString(h[:]) }\n if digest(raw) != ${quote(k.capsuleSha512)} || !bytes.Equal(raw[:8], []byte("DLSBOX1\\x00")) || binary.LittleEndian.Uint32(raw[12:16]) != 0 { return frontend.EmbeddedCapsule{}, bad }\n ml := int(binary.LittleEndian.Uint32(raw[8:12])); nl := int(binary.LittleEndian.Uint64(raw[16:24])); hl := int(binary.LittleEndian.Uint64(raw[24:32]))\n if ml < 1 || nl != ${k.maintainerSize} || hl < 1 || 32+ml != ${k.maintainerOffset} || 32+ml+nl+hl != len(raw) { return frontend.EmbeddedCapsule{}, bad }\n manifest := raw[32:32+ml]; native := raw[32+ml:32+ml+nl]\n if digest(manifest) != ${quote(k.manifestSha512)} || digest(native) != ${quote(k.maintainerSha512)} { return frontend.EmbeddedCapsule{}, bad }\n var m struct { Generation int64 \\x60json:"generation"\\x60; CompatibilityEpoch int64 \\x60json:"compatibilityEpoch"\\x60; SandboxAPI int64 \\x60json:"sandboxApi"\\x60; PublishedAt string \\x60json:"publishedAt"\\x60; ExpiresAt string \\x60json:"expiresAt"\\x60 }\n if json.Unmarshal(manifest, &m) != nil || m.Generation != ${k.generation} || m.CompatibilityEpoch != ${k.compatibilityEpoch} || m.SandboxAPI != ${k.sandboxApi} || m.PublishedAt != ${quote(k.publishedAt)} || m.ExpiresAt != ${quote(k.expiresAt)} { return frontend.EmbeddedCapsule{}, bad }\n var keys []ed25519.PublicKey\n for _, encoded := range []string{${c.publicKeys.map(quote).join(',')}} { key, e := base64.StdEncoding.Strict().DecodeString(encoded); if e != nil || len(key) != ed25519.PublicKeySize { return frontend.EmbeddedCapsule{}, bad }; keys = append(keys, ed25519.PublicKey(key)) }\n return frontend.NewEmbeddedCapsule(raw, keys)\n}\nfunc main() {\n capsule, err := compiledCapsule(); if err == nil { err = frontend.DispatchLaunchVersion(os.Args[1:], capsule, ${quote(compiledVersion)}) }; if err != nil { _, _ = fmt.Fprintln(os.Stderr, "Dilla sandbox prerequisites are unavailable."); os.Exit(1) }\n}\n`.replaceAll('\\x60', '`');
}

function validateAppVersion(value) {
 const match = typeof value === 'string' && /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?$/.exec(value);
 if (!match || match[0] !== value || !match.slice(1).filter(x => x !== undefined).every(x => Number.isSafeInteger(Number(x)))) throw new Error('Invalid compiled application version');
}

export function maintainerConfiguration(config) {
  const c = validateConfiguration(config);
  return Buffer.from(JSON.stringify({ publicKeys: c.publicKeys, generation: c.generation, publishedAt: c.publishedAt, expiresAt: c.expiresAt, helper: { size: c.helper.size, sha512: c.helper.sha512 } })).toString('base64');
}

/** Prepare unsigned native inputs only; outputDirectory must not already exist. */
export async function prepareComponents({ config, helper, outputDirectory, compiledVersion }) {
  const { execFileSync } = await import('node:child_process');
  const { mkdir, mkdtemp, readFile, rm, writeFile } = await import('node:fs/promises');
  const { tmpdir } = await import('node:os');
  const { dirname, join, resolve } = await import('node:path');
  const { fileURLToPath } = await import('node:url');
  const root = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');
  const c = validateConfiguration(config);
  assertDedicatedKeys(c, parsePublicConfiguration(await readFile(join(root, 'packages/desktop/release-trust.json'), 'utf8')));
  if (!Buffer.isBuffer(helper) || helper.length !== c.helper.size || sha512(helper) !== c.helper.sha512 || createHash('sha256').update(helper).digest('hex') !== c.helper.sha256) throw new Error('Pinned sandbox helper mismatch');
  validateAppVersion(compiledVersion);
  if (helper.length < 20 || !helper.subarray(0, 4).equals(Buffer.from([0x7f, 0x45, 0x4c, 0x46])) || helper[4] !== 2 || helper[5] !== 1 || helper.readUInt16LE(18) !== 62) throw new Error('Sandbox helper is not a Linux x64 ELF');
  const env = Object.fromEntries(['PATH', 'HOME', 'TMPDIR', 'GOCACHE', 'GOPATH', 'GOMODCACHE'].filter(k => process.env[k] !== undefined).map(k => [k, process.env[k]]));
  Object.assign(env, { GOOS: 'linux', GOARCH: 'amd64', GOAMD64: 'v1', CGO_ENABLED: '0', GOTOOLCHAIN: c.goVersion, GOFLAGS: '', GOEXPERIMENT: '', GOENV: 'off', GOWORK: 'off' });
  const version = execFileSync('go', ['version'], { cwd: root, env, encoding: 'utf8' }).trim().split(/\s+/)[2];
  if (version !== c.goVersion) throw new Error('Sandbox Go toolchain version differs from immutable configuration');
  const temp = await mkdtemp(join(tmpdir(), 'dilla-sandbox-components-'));
  try {
    const goConfig = maintainerConfiguration(c);
    const binaries = [];
    for (const name of ['maintainer-a', 'maintainer-b']) {
      const file = join(temp, name);
      execFileSync('go', ['build', '-mod=readonly', '-trimpath', '-buildvcs=false', '-ldflags', `-buildid= -X main.publicConfiguration=${goConfig}`, '-o', file, './packages/desktop/native/linux-sandbox/maintainer'], { cwd: root, env, stdio: 'inherit' });
      binaries.push(await readFile(file));
    }
    if (!binaries[0].equals(binaries[1])) throw new Error('Maintainer build is not reproducible');
    const result = buildCapsule(c, binaries[0], helper);
    const out = resolve(outputDirectory);
    await mkdir(out); // Never overwrite or silently reuse a candidate.
    await writeFile(join(out, 'sandbox-maintainer'), binaries[0], { mode: 0o755, flag: 'wx' });
    await writeFile(join(out, 'component.sandbox'), result.capsule, { flag: 'wx' });
    await writeFile(join(out, 'component.sandbox.json'), result.manifest, { flag: 'wx' });
    const generated = join(out, 'launcher.generated.go');
    await writeFile(generated, renderLauncher(c, result, compiledVersion), { flag: 'wx' });
    await writeFile(join(out, 'launcher-overlay.json'), JSON.stringify({ Replace: { [join(root, 'packages/desktop/native/linux-sandbox/launcher/main.go')]: generated } }) + '\n', { flag: 'wx' });
    await writeFile(join(out, 'constants.json'), JSON.stringify(result.constants, null, 2) + '\n', { flag: 'wx' });
    return result.constants;
  } finally { await rm(temp, { recursive: true, force: true }); }
}

if (isMain(import.meta.url)) {
  const { readFile } = await import('node:fs/promises');
  const [configFile, helperFile, outputDirectory, compiledVersion] = process.argv.slice(2);
  if (!configFile || !helperFile || !outputDirectory || !compiledVersion || process.argv.length !== 6) throw new Error('Usage: linux-sandbox-package.mjs PUBLIC_CONFIG HELPER NEW_OUTPUT_DIRECTORY VERSION');
  const config = parsePublicConfiguration(await readFile(configFile, 'utf8'));
  const constants = await prepareComponents({ config, helper: await readFile(helperFile), outputDirectory, compiledVersion });
  console.log(`Prepared unsigned sandbox generation ${constants.generation}: ${constants.capsuleSha512}`);
}
