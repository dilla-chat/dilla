import { test } from 'node:test';
import { strict as assert } from 'node:assert';
import { readFileSync } from 'node:fs';
import { checkNativeStockFeasibility } from './check-native-stock-feasibility.mjs';
const candidate = readFileSync(new URL('../.github/workflows/desktop.yml', import.meta.url), 'utf8');
const stock = /^  native-stock-feasibility:\n[\s\S]*?(?=^  [a-z][\w-]*:)/m.exec(candidate)[0];
test('selected native stock feasibility job retains its required sequencing', () => { assert.deepEqual(checkNativeStockFeasibility(candidate, { workflow: true }), []); });
for (const [name, from, to] of [
 ['runner', 'runs-on: ubuntu-24.04', 'runs-on: self-hosted'],
 ['producer', 'node scripts/e2e-test-tools.mjs verify artifacts/e2e-test-tools', 'echo skipped'],
 ['build', 'build "$RUNNER_TEMP/native-stock" 1.0.0', 'build "$RUNNER_TEMP/native-stock"'],
 ['install', 'sudo apt-get install -y "$RUNNER_TEMP/native-stock/dilla-1.0.0-linux-x64.deb"', 'echo skipped'],
 ['nonroot', 'sudo -H -u dilla-ci env', 'sudo env'],
 ['credentials', 'persist-credentials: false', 'persist-credentials: true'],
 ['Connect test', ' e2e/unit/native-stock-connect.test.mjs', ''],
 ['commented ASAR comparison', '          cmp "$RUNNER_TEMP/native-extracted/opt/Dilla/resources/app.asar" /opt/Dilla/resources/app.asar', '          # cmp "$RUNNER_TEMP/native-extracted/opt/Dilla/resources/app.asar" /opt/Dilla/resources/app.asar'],
 ['sandbox', 'xvfb-run -a bash', 'xvfb-run -a --no-sandbox bash'],
 ['if', 'timeout-minutes: 45', 'timeout-minutes: 45\n    if: false'],
]) test(`candidate check rejects weakened ${name}`, () => { const changed = candidate.replace(stock, stock.replace(from, to)); assert.notEqual(changed, candidate); assert.ok(checkNativeStockFeasibility(changed, { workflow: true }).length); });
