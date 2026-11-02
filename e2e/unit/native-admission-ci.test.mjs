import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import assert from 'node:assert/strict';

const workflow = readFileSync(new URL('../../.github/workflows/desktop.yml', import.meta.url), 'utf8');
const pkg = JSON.parse(readFileSync(new URL('../package.json', import.meta.url), 'utf8'));
import { checkNativeAdmissionWiring, run, upload } from '../../scripts/check-native-admission-ci.mjs';
const check = text => checkNativeAdmissionWiring(text, pkg);
const command = 'xvfb-run -a bash e2e/web/support/installed-keyring.sh bash packages/desktop/scripts/with-window-manager.sh npm exec -w @dilla/e2e -- playwright test --config playwright.native-admission.config.ts --project=native-admission --workers=1 --max-failures=1';
const condition = "runner.os == 'Linux' && matrix.os == 'ubuntu-24.04' && matrix.arch == 'x64' && matrix.shard == 1";
test('dedicated full-client admission runs once after setup and retains only bounded outcomes', () => {
  assert.ok(check(workflow));
  for (const name of ['actor', 'fixture', 'process', 'ci']) assert.equal(pkg.scripts.test.split(/\s+/).filter(s => s === `unit/native-admission-${name}.test.mjs`).length, 1);
});
test('rejects omitted, duplicated, wrong-platform, early, filtered and unsafe wiring', () => {
  assert.equal(check(workflow.replace(run, '')), false);
  assert.equal(check(workflow.replace(run, `${run}\n${run}`)), false);
  assert.equal(check(workflow.replace(condition, "runner.os == 'Windows'")), false);
  assert.equal(check(workflow.replace(run, '').replace('  client:\n', `  client:\n${run}\n`)), false);
  assert.equal(check(workflow.replace(command, `${command} --grep=account`)), false);
  assert.equal(check(workflow.replace('e2e/native-admission-results/**/attachments/native-admission-*.json', 'e2e/native-admission-results')), false);
  assert.equal(check(workflow.replace(upload, '')), false);
});

test('retained attachments contain only the reviewed outcome fields and disable visual capture', () => {
  const spec = readFileSync(new URL('../web/support/native-admission-account.e2e.ts', import.meta.url), 'utf8');
  const config = readFileSync(new URL('../playwright.native-admission.config.ts', import.meta.url), 'utf8');
  assert.deepEqual([...spec.matchAll(/info\.attach\([^\n]+/g)].map(m => m[0].trim()), [
    "info.attach('native-admission-proof',{body:JSON.stringify({format:1,status:'passed',checks,descendantCount,survivorCount,transforms:fixture.manifest,cppFixture:fixture.cppFixture,sandboxSha512:fixture.sandboxDigest}),contentType:'application/json'});",
    "info.attach('native-admission-failure',{body:JSON.stringify({format:1,phase,checks}),contentType:'application/json'});throw error;}",
  ]);
  assert.match(config, /trace:'off',screenshot:'off',video:'off'/);
});
