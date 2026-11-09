import { existsSync, readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

export const REQUIRED = {
  'README.md': ['## Documents', '## Conformance'],
  '00-overview.md': ['## Terminology', '## Precedence', '## Conformance language'],
  '01-groups.md': ['## Group kinds', '## dilla_binding', '## External senders', '## Joining', '## Cadence'],
  '02-delivery-service.md': ['## Roles', '## API', '## Invariants', '## Errors', '## Retention'],
  '03-identity.md': ['## Keys', '## Credential', '## Device list', '## Custody by tier', '## Pairing', '## Recovery', '## Safety number', '## Vectors'],
  '04-envelope-and-franking.md': ['## Envelope', '## Deterministic CBOR', '## Franking', '## Vectors'],
  '05-media-frames.md': ['## Frame format', '## Codec prefixes', '## Key schedule', '## Counter partition', '## Rotation'],
  '06-backup-archive.md': ['## Keys', '## Header', '## Archive', '## Restore'],
  '07-versioning.md': ['## Versions', '## Negotiation', '## Change process'],
  '08-threat-model.md': ['## Adversaries', '## Guarantees', '## Residual trust', '## Out of scope'],
  '09-http-api.md': ['## Scope', '## Encoding', '## Socket requests', '## Identifiers', '## Sessions', '## Accounts and devices', '## Auth ceremonies', '## Instance', '## Rate limits', '## Flags'],
  '10-community-bot.md': ['## Scope', '## Encoding', '## Command catalog', '## Command key', '## Sealed request', '## Reply', '## State hints', '## Bot channels', '## Webhooks', '## Bot connection and feed', '## Health', '## Vectors'],
};

const PLACEHOLDER = /\b(TBD|TODO|FIXME|XXX)\b|lorem ipsum|fill in later/i;

export function checkDocs(root) {
  const problems = [];
  for (const [name, headings] of Object.entries(REQUIRED)) {
    const path = join(root, 'protocol', name);
    if (!existsSync(path)) { problems.push(`${name}: missing`); continue; }
    const text = readFileSync(path, 'utf8');
    for (const h of headings) {
      if (!text.split('\n').some(line => line.trim() === h)) problems.push(`${name}: missing heading "${h}"`);
    }
    const bad = text.split('\n').findIndex(line => PLACEHOLDER.test(line));
    if (bad >= 0) problems.push(`${name}: placeholder on line ${bad + 1}`);
  }

  // Recursively walk protocol/ and check all files for placeholders
  const protocolDir = join(root, 'protocol');
  if (existsSync(protocolDir)) {
    const walkDir = (currentDir, prefix) => {
      const entries = readdirSync(currentDir, { withFileTypes: true });
      for (const entry of entries) {
        const fullPath = join(currentDir, entry.name);
        const relPath = prefix ? join(prefix, entry.name) : entry.name;
        if (entry.isFile()) {
          try {
            const text = readFileSync(fullPath, 'utf8');
            const bad = text.split('\n').findIndex(line => PLACEHOLDER.test(line));
            if (bad >= 0) problems.push(`${relPath}: placeholder on line ${bad + 1}`);
          } catch (e) {
            // Skip files that can't be read
          }
        } else if (entry.isDirectory()) {
          walkDir(fullPath, relPath);
        }
      }
    };
    walkDir(protocolDir, '');
  }

  return problems;
}

/// Checks that the `E_*` vocabulary in `02`, the Rust `DsError` and the Go `server.Code` constants
/// all agree. Three sources, not two: the Go constants are where `E_INTERNAL`, `E_VERSION` and
/// `E_PROVISIONAL_OUTSIDE_PAIRING` live, and a check that never reads them is how those three went
/// missing from the document in the first place (ID12).
export function checkErrorVocabulary(root) {
  const problems = [];
  const doc = readFileSync(join(root, 'protocol', '02-delivery-service.md'), 'utf8');
  const rust = readFileSync(join(root, 'testkit', 'src', 'ds', 'state.rs'), 'utf8');
  let go = '';
  let goExists = true;
  try {
    go = readFileSync(join(root, 'internal', 'server', 'errors.go'), 'utf8');
  } catch {
    // `internal/server/errors.go` does not exist until task 6: this leg is skipped, not failed,
    // until then — task 0 runs before task 6.
    goExists = false;
  }
  // Scoped to the "## Errors" status table's rows, not every backticked `E_*` token in the file:
  // sections like "## Gateway frames" also mention `E_*` codes in prose (e.g. `E_FRAME_TYPE`) that
  // are not HTTP error codes and have no row in the table, so Go has nothing to declare for them.
  const inDoc = new Set([...doc.matchAll(/^\|\s*\d{3}\s*\|\s*`(E_[A-Z_]+)`/gm)].map(m => m[1]));
  const inRust = new Set([...rust.matchAll(/"(E_[A-Z_]+)"/g)].map(m => m[1]));
  const inGo = new Set([...go.matchAll(/Code\s*=\s*"(E_[A-Z_]+)"/g)].map(m => m[1]));
  for (const code of inRust) {
    if (!inDoc.has(code)) problems.push(`02-delivery-service.md: missing code ${code} present in state.rs`);
  }
  if (goExists) {
    for (const code of inGo) {
      if (!inDoc.has(code)) problems.push(`02-delivery-service.md: missing code ${code} present in internal/server/errors.go`);
    }
    for (const code of inDoc) {
      if (!inGo.has(code)) problems.push(`internal/server/errors.go: missing code ${code} listed in 02-delivery-service.md`);
    }
  }
  const dsOnly = new Set(['E_COMMIT_CONFLICT', 'E_COMMIT_REQUIRED', 'E_COMMIT_INVALID', 'E_BINDING_INVALID',
    'E_MODE_READABLE', 'E_LEAF_NOT_CURRENT', 'E_COMMITMENT_INVALID', 'E_TOO_LARGE', 'E_PRUNED',
    'E_GROUP_EXISTS', 'E_NOT_FOUND', 'E_RATE_LIMITED']);
  for (const code of dsOnly) {
    if (!inRust.has(code)) problems.push(`state.rs: missing DS code ${code} listed in 02-delivery-service.md`);
  }
  return problems;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const root = process.argv[2] ?? process.cwd();
  const problems = [...checkDocs(root), ...checkErrorVocabulary(root)];
  if (problems.length) { console.error(problems.join('\n')); process.exit(1); }
  console.log('protocol docs: ok');
}
