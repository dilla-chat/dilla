// Fails early, with the command to run, when a generated (gitignored) wasm the probe bundles is missing.
import { existsSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const repo = resolve(dirname(fileURLToPath(import.meta.url)), '../..');
const coreWasm = resolve(repo, 'packages/core-wasm/pkg/dilla_core_wasm_bg.wasm');
if (!existsSync(coreWasm)) {
  console.error(`missing ${coreWasm}; from the repo root run:\n  wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/core-wasm/pkg`);
  process.exit(1);
}
// The RNNoise bytes the worklet compiles (packages/media/src/audio/generated/rnnoise.wasm).
execFileSync(process.execPath, [resolve(repo, 'packages/media/scripts/extract-rnnoise-wasm.mjs')], { stdio: 'inherit' });
