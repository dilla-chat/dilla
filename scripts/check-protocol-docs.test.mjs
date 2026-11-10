import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync, mkdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { checkDocs, checkErrorVocabulary } from './check-protocol-docs.mjs';

function fixture(files) {
  const dir = mkdtempSync(join(tmpdir(), 'dilla-docs-'));
  mkdirSync(join(dir, 'protocol'), { recursive: true });
  for (const [name, body] of Object.entries(files)) writeFileSync(join(dir, 'protocol', name), body);
  return dir;
}

test('passes when every required document exists with its required headings and no placeholders', () => {
  const dir = fixture({
    'README.md': '# dilla protocol\n\n## Documents\n\n## Conformance\n',
    '00-overview.md': '# Overview\n\n## Terminology\n\n## Precedence\n\n## Conformance language\n',
    '01-groups.md': '# Groups\n\n## Group kinds\n\n## dilla_binding\n\n## External senders\n\n## Joining\n\n## Cadence\n',
    '02-delivery-service.md': '# Delivery service\n\n## Roles\n\n## API\n\n## Invariants\n\n## Errors\n\n## Retention\n',
    '03-identity.md': '# Identity\n\n## Keys\n\n## Credential\n\n## Device list\n\n## Custody by tier\n\n## Pairing\n\n## Recovery\n\n## Safety number\n\n## Vectors\n',
    '04-envelope-and-franking.md': '# Envelope\n\n## Envelope\n\n## Deterministic CBOR\n\n## Franking\n\n## Vectors\n',
    '05-media-frames.md': '# Media\n\n## Frame format\n\n## Codec prefixes\n\n## Key schedule\n\n## Counter partition\n\n## Rotation\n',
    '06-backup-archive.md': '# Backup\n\n## Keys\n\n## Header\n\n## Archive\n\n## Restore\n',
    '07-versioning.md': '# Versioning\n\n## Versions\n\n## Negotiation\n\n## Change process\n',
    '08-threat-model.md': '# Threat model\n\n## Adversaries\n\n## Guarantees\n\n## Residual trust\n\n## Out of scope\n',
    '09-http-api.md': '# HTTP API\n\n## Scope\n\n## Encoding\n\n## Socket requests\n\n## Identifiers\n\n## Sessions\n\n## Accounts and devices\n\n## Auth ceremonies\n\n## Instance\n\n## Rate limits\n\n## Flags\n',
    '10-community-bot.md': '# Community bot\n\n## Scope\n\n## Encoding\n\n## Command catalog\n\n## Command key\n\n## Sealed request\n\n## Reply\n\n## State hints\n\n## Bot channels\n\n## Webhooks\n\n## Bot connection and feed\n\n## Health\n\n## Vectors\n',
  });
  assert.deepEqual(checkDocs(dir), []);
});

test('reports a missing document, a missing heading and a placeholder', () => {
  const dir = fixture({
    'README.md': '# dilla protocol\n\n## Documents\n\n## Conformance\n',
    '00-overview.md': '# Overview\n\n## Terminology\n\nTBD\n',
  });
  const problems = checkDocs(dir);
  assert.ok(problems.some(p => p.includes('01-groups.md') && p.includes('missing')));
  assert.ok(problems.some(p => p.includes('00-overview.md') && p.includes('## Precedence')));
  assert.ok(problems.some(p => p.includes('00-overview.md') && p.includes('placeholder')));
});

test('recursively detects placeholders in all files under protocol/', () => {
  const tmpDir = mkdtempSync(join(tmpdir(), 'dilla-docs-'));
  try {
    const protocolDir = join(tmpDir, 'protocol');
    mkdirSync(protocolDir, { recursive: true });

    // Create required documents
    writeFileSync(join(protocolDir, 'README.md'), '# dilla protocol\n\n## Documents\n\n## Conformance\n');
    writeFileSync(join(protocolDir, '00-overview.md'), '# Overview\n\n## Terminology\n\n## Precedence\n\n## Conformance language\n');
    writeFileSync(join(protocolDir, '01-groups.md'), '# Groups\n\n## Group kinds\n\n## dilla_binding\n\n## External senders\n\n## Joining\n\n## Cadence\n');
    writeFileSync(join(protocolDir, '02-delivery-service.md'), '# Delivery service\n\n## Roles\n\n## API\n\n## Invariants\n\n## Errors\n\n## Retention\n');
    writeFileSync(join(protocolDir, '03-identity.md'), '# Identity\n\n## Keys\n\n## Credential\n\n## Device list\n\n## Custody by tier\n\n## Pairing\n\n## Recovery\n\n## Safety number\n\n## Vectors\n');
    writeFileSync(join(protocolDir, '04-envelope-and-franking.md'), '# Envelope\n\n## Envelope\n\n## Deterministic CBOR\n\n## Franking\n\n## Vectors\n');
    writeFileSync(join(protocolDir, '05-media-frames.md'), '# Media\n\n## Frame format\n\n## Codec prefixes\n\n## Key schedule\n\n## Counter partition\n\n## Rotation\n');
    writeFileSync(join(protocolDir, '06-backup-archive.md'), '# Backup\n\n## Keys\n\n## Header\n\n## Archive\n\n## Restore\n');
    writeFileSync(join(protocolDir, '07-versioning.md'), '# Versioning\n\n## Versions\n\n## Negotiation\n\n## Change process\n');
    writeFileSync(join(protocolDir, '08-threat-model.md'), '# Threat model\n\n## Adversaries\n\n## Guarantees\n\n## Residual trust\n\n## Out of scope\n');
    writeFileSync(join(protocolDir, '09-http-api.md'), '# HTTP API\n\n## Scope\n\n## Encoding\n\n## Socket requests\n\n## Identifiers\n\n## Sessions\n\n## Accounts and devices\n\n## Auth ceremonies\n\n## Instance\n\n## Rate limits\n\n## Flags\n');

    // Create extra files with placeholders in nested directories
    mkdirSync(join(protocolDir, 'vectors'), { recursive: true });
    writeFileSync(join(protocolDir, 'vectors', 'test-vectors.json'), '{"vectors": "TODO"}');

    mkdirSync(join(protocolDir, 'appendix'), { recursive: true });
    writeFileSync(join(protocolDir, 'appendix', 'reference.md'), '# Reference\n\nTBD: to be filled in');

    // Check should detect placeholders in nested files
    const problems = checkDocs(tmpDir);
    assert.ok(problems.some(p => p.includes('vectors/test-vectors.json') && p.includes('placeholder')), 'Should detect TODO in vectors/test-vectors.json');
    assert.ok(problems.some(p => p.includes('appendix/reference.md') && p.includes('placeholder')), 'Should detect TBD in appendix/reference.md');
  } finally {
    rmSync(tmpDir, { recursive: true });
  }
});

test('checkErrorVocabulary reports a code present in state.rs but absent from 02', () => {
  const dir = mkdtempSync(join(tmpdir(), 'dilla-vocab-'));
  mkdirSync(join(dir, 'protocol'), { recursive: true });
  mkdirSync(join(dir, 'testkit', 'src', 'ds'), { recursive: true });
  writeFileSync(join(dir, 'protocol', '02-delivery-service.md'), '| 404 | `E_NOT_FOUND` | x | y |\n');
  writeFileSync(join(dir, 'testkit', 'src', 'ds', 'state.rs'), '"E_NOT_FOUND" "E_TOO_LARGE"\n');
  const problems = checkErrorVocabulary(dir);
  assert.ok(problems.some(p => p.includes('E_TOO_LARGE')));
  rmSync(dir, { recursive: true, force: true });
});

test('checkErrorVocabulary scopes doc codes to the Errors status table, not every backticked E_* token in the file', () => {
  // 02 also has a "## Gateway frames" section that mentions E_FRAME_* codes in prose, never as a
  // status-table row. Those are not HTTP error codes and Go has nothing to declare for them.
  const dir = mkdtempSync(join(tmpdir(), 'dilla-vocab-'));
  mkdirSync(join(dir, 'protocol'), { recursive: true });
  mkdirSync(join(dir, 'testkit', 'src', 'ds'), { recursive: true });
  mkdirSync(join(dir, 'internal', 'server'), { recursive: true });
  writeFileSync(join(dir, 'protocol', '02-delivery-service.md'),
    '## Gateway frames\n\n' +
    'An opcode outside the negotiated `wire_version` is a hard error (`E_FRAME_TYPE`), never ignored.\n' +
    'A frame of the wrong element count for its opcode is `E_FRAME_SHAPE`.\n\n' +
    '## Errors\n\n' +
    '| HTTP | code | meaning |\n|---|---|---|\n| 404 | `E_NOT_FOUND` | x |\n');
  writeFileSync(join(dir, 'testkit', 'src', 'ds', 'state.rs'), '"E_NOT_FOUND"\n');
  writeFileSync(join(dir, 'internal', 'server', 'errors.go'), 'const CodeNotFound Code = "E_NOT_FOUND"\n');
  const problems = checkErrorVocabulary(dir);
  assert.deepEqual(problems.filter(p => p.includes('E_FRAME_')), [],
    'gateway-frame codes are not status-table rows and must not be demanded of internal/server/errors.go');
  rmSync(dir, { recursive: true, force: true });
});

test('checkErrorVocabulary reports a code present in errors.go but absent from 02\'s Errors table (the Go direction)', () => {
  const dir = mkdtempSync(join(tmpdir(), 'dilla-vocab-'));
  mkdirSync(join(dir, 'protocol'), { recursive: true });
  mkdirSync(join(dir, 'testkit', 'src', 'ds'), { recursive: true });
  mkdirSync(join(dir, 'internal', 'server'), { recursive: true });
  writeFileSync(join(dir, 'protocol', '02-delivery-service.md'),
    '## Errors\n\n| HTTP | code | meaning |\n|---|---|---|\n| 404 | `E_NOT_FOUND` | x |\n');
  writeFileSync(join(dir, 'testkit', 'src', 'ds', 'state.rs'), '"E_NOT_FOUND"\n');
  writeFileSync(join(dir, 'internal', 'server', 'errors.go'),
    'const CodeNotFound Code = "E_NOT_FOUND"\nconst CodeMystery Code = "E_MYSTERY"\n');
  const problems = checkErrorVocabulary(dir);
  assert.ok(problems.includes('02-delivery-service.md: missing code E_MYSTERY present in internal/server/errors.go'));
  rmSync(dir, { recursive: true, force: true });
});

test('checkErrorVocabulary skips both Go legs entirely when internal/server/errors.go does not exist yet', () => {
  const dir = mkdtempSync(join(tmpdir(), 'dilla-vocab-'));
  mkdirSync(join(dir, 'protocol'), { recursive: true });
  mkdirSync(join(dir, 'testkit', 'src', 'ds'), { recursive: true });
  writeFileSync(join(dir, 'protocol', '02-delivery-service.md'),
    '## Errors\n\n| HTTP | code | meaning |\n|---|---|---|\n| 404 | `E_NOT_FOUND` | x |\n');
  writeFileSync(join(dir, 'testkit', 'src', 'ds', 'state.rs'), '"E_NOT_FOUND"\n');
  const problems = checkErrorVocabulary(dir);
  assert.deepEqual(problems.filter(p => p.includes('errors.go')), []);
  rmSync(dir, { recursive: true, force: true });
});
