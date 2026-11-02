// Review aid for the non-selected job fragment; selection must additionally
// wire this check into the workflow checker and its tests.
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';
const yaml = createRequire(import.meta.url)('js-yaml');
const requiredRuns = [
 'node scripts/e2e-test-tools.mjs verify artifacts/e2e-test-tools',
 'node --test e2e/unit/native-https-proxy.test.mjs e2e/unit/native-test-cert.test.mjs e2e/unit/native-stock-manager.test.mjs',
 'node packages/desktop/scripts/build-native-linux-packages.mjs build "$RUNNER_TEMP/native-stock" 1.0.0',
 'sudo apt-get install -y "$RUNNER_TEMP/native-stock/dilla-1.0.0-linux-x64.deb"',
];
export function checkNativeStockFeasibility(text, { workflow = false } = {}) {
 let job; try { job = yaml.load(workflow ? text : `jobs:\n${text}`).jobs['native-stock-feasibility']; } catch { return ['invalid job YAML']; }
 const errors = [];
 if (!job || job['runs-on'] !== 'ubuntu-24.04' || job.needs !== 'test-tools' || job['timeout-minutes'] !== 45 || !Array.isArray(job.steps)) return ['invalid fresh-runner job'];
 if (Object.hasOwn(job, 'if') || Object.hasOwn(job, 'continue-on-error')) errors.push('job may not skip or ignore failures');
 const steps = job.steps;
 const checkout = steps.filter(s => s.uses === 'actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1');
 if (checkout.length !== 1 || checkout[0].with?.['persist-credentials'] !== false) errors.push('checkout must not persist credentials');
 if (steps.some(s => Object.hasOwn(s, 'if') || Object.hasOwn(s, 'continue-on-error'))) errors.push('steps may not skip or ignore failures');
 const runs = steps.filter(s => typeof s.run === 'string').map(s => s.run.trim());
 const lines = run => run.split('\n').map(line => line.trim()).filter(line => line && !line.startsWith('#'));
 for (const command of requiredRuns) if (runs.flatMap(lines).filter(s => s === command).length !== 1) errors.push(`missing exact command: ${command}`);
 const pos = part => steps.findIndex(s => (typeof s.run === 'string' && lines(s.run).some(line => line.includes(part))) || (s.with?.name ?? '').includes(part));
 const fetch = pos('dilla-e2e-tools-linux-x64-'), verify = pos('node scripts/e2e-test-tools.mjs verify'), build = pos('build-native-linux-packages.mjs build');
 if (fetch < 0 || verify <= fetch || build <= verify) errors.push('producer verification must precede build');
 for (const artifact of ['dilla-web-dist', 'dilla-desktop-core', 'dilla-core-wasi']) if (pos(artifact) < 0 || pos(artifact) > pos('build-native-linux-packages.mjs build')) errors.push(`missing prior artifact: ${artifact}`);
 const requiredOrder = ['git rev-parse HEAD', 'verifyShippingCoreWasmInDist', 'native-https-proxy.test.mjs', 'build-native-linux-packages.mjs build', 'sha256sum', 'sudo apt-get install -y "$RUNNER_TEMP/native-stock/', 'dpkg-deb -x', 'sudo useradd --create-home', 'sudo -H -u dilla-ci env'];
 let last = -1;
 for (const item of requiredOrder) { const current = pos(item); if (current < last || current < 0) errors.push(`missing or misplaced ${item}`); last = current; }
 const commands = runs.flatMap(lines), combined = commands.join('\n');
 for (const command of ['cmp "$RUNNER_TEMP/native-extracted/opt/Dilla/dilla-desktop" /opt/Dilla/dilla-desktop', 'cmp "$RUNNER_TEMP/native-extracted/opt/Dilla/resources/app.asar" /opt/Dilla/resources/app.asar', 'npm exec -w @dilla/e2e -- playwright test --config playwright.native-stock.config.ts --list']) if (!commands.includes(command)) errors.push(`missing ${command}`);
 for (const fragment of ["dpkg-query -W -f='${Version}' dilla", 'GITHUB_ACTIONS=$GITHUB_ACTIONS', 'GITHUB_SHA=$GITHUB_SHA', 'GITHUB_RUN_ID=$GITHUB_RUN_ID', 'GITHUB_RUN_ATTEMPT=$GITHUB_RUN_ATTEMPT', 'DILLA_E2E_TOOLING_DIR=/home/dilla-ci/workspace/artifacts/e2e-test-tools', 'npm exec -w @dilla/e2e -- playwright test --config playwright.native-stock.config.ts']) if (!combined.includes(fragment)) errors.push(`missing ${fragment}`);
 if (/--no-sandbox|--disable-setuid-sandbox|ignore-certificate-errors|APPIMAGE_EXTRACT_AND_RUN|continue-on-error|\|\|\s*true/.test(text)) errors.push('forbidden bypass');
 return errors;
}
if (process.argv[1] && pathToFileURL(process.argv[1]).href === import.meta.url) {
 const errors = checkNativeStockFeasibility(readFileSync(process.argv[2], 'utf8'));
 if (errors.length) { console.error(errors.join('\n')); process.exitCode = 1; } else console.log('Native stock feasibility candidate checks passed');
}
