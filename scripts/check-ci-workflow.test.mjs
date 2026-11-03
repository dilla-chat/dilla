import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { spawnSync } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { gateExpression, JOB_GATES } from './ci-areas.mjs';
import { BROWSER_WEB_JOBS, browserSuiteCommand, checkDesktopReleaseWorkflow, checkDesktopWorkflow, DESKTOP_RELEASE_ACTIONS, DESKTOP_RELEASE_SECRETS, checkReleaseWorkflow, checkWorkflow, jobSteps, MIN_BROWSER_WEB_TIMEOUT, NON_RACE_STEP, raceGatedTests, RELEASE_ASSETS, WASM_CLANG_BLOCK, WASM_CLANG_ENV_BLOCK, WASM_CLANG_STEPS, WASM_LLVM_KEY_FPR, WASM_LLVM_VERSION, WEB_SHARDS } from './check-ci-workflow.mjs';

const NPM_TEST = "      - if: needs.changes.outputs.web != 'false'\n        run: npm test\n";
const SCRIPT_PATH = fileURLToPath(new URL('./check-ci-workflow.mjs', import.meta.url));
const REPO_ROOT = join(dirname(SCRIPT_PATH), '..');
const REAL_CI = readFileSync(join(REPO_ROOT, '.github', 'workflows', 'ci.yml'), 'utf8');

test('whole-module Go jobs provision GTK development files before package loading', () => {
  for (const name of ['go', 'go-lint', 'go-vuln']) {
    const body = new RegExp(`^  ${name}:\\n[\\s\\S]*?(?=^  [a-z][\\w-]*:|$(?![\\s\\S]))`, 'm').exec(REAL_CI)[0];
    const steps = jobSteps(body);
    const gtk = steps.findIndex(step => step.length === 1 && step[0] === 'run: sudo apt-get update && sudo apt-get install -y libgtk-3-dev');
    const load = steps.findIndex(step => step.some(line => /(?:go vet|golangci-lint run|govulncheck@\S+) \.\/\.\.\./.test(line)));
    assert.ok(gtk >= 0 && gtk < load, name + ' must install GTK before loading the Linux cgo frontend');
  }
});

test('GTK provisioning cannot be omitted, postponed or bypassed by excluding cgo', () => {
  for (const name of ['go', 'go-lint', 'go-vuln']) {
    const body = jobBody(name), setup = '      - run: sudo apt-get update && sudo apt-get install -y libgtk-3-dev\n';
    assert.ok(body.includes(setup), name);
    for (const changed of [body.replace(setup, ''), body.replace('libgtk-3-dev', 'libgtk-3-0'),
      body.replace(setup, '      - if: false\n        run: sudo apt-get update && sudo apt-get install -y libgtk-3-dev\n'),
      body.replace(setup, '') + setup]) {
      assert.ok(checkWorkflow(fixture(GOOD.replace(body, changed))).some(problem => problem.includes('GTK')), name);
    }
  }
  const body = jobBody('go-vuln');
  for (const changed of [body.replace('run: go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...', 'run: CGO_ENABLED=0 go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...'),
    body.replace('    steps:', '    env:\n      CGO_ENABLED: 0\n    steps:')]) {
    assert.notEqual(changed, body);
    assert.ok(checkWorkflow(fixture(GOOD.replace(body, changed))).some(problem => problem.includes('GTK')));
  }
});

test('Linux sandbox foundation has independent native Ubuntu lanes ahead of desktop', () => {
  assert.match(REAL_CI, /^  linux-sandbox-foundation:\n    needs: \[changes\]/m);
  const body = /^  linux-sandbox-foundation:\n[\s\S]*?(?=^  [a-z][\w-]*:)/m.exec(REAL_CI)?.[0] ?? '';
  assert.match(body, /os: ubuntu-22\.04, lane: namespace/);
  assert.match(body, /os: ubuntu-24\.04, lane: managed/);
  assert.match(body, /xvfb-run -a dbus-run-session -- node --test packages\/desktop\/test\/native\/linux-sandbox-foundation\.test\.mjs/);
  assert.match(body, /id: sandbox_foundation/);
  assert.match(body, /steps\.sandbox_foundation\.outcome\)\n        uses: actions\/upload-artifact@/);
  assert.match(REAL_CI, /  desktop:\n    needs: \[[^\]\n]*\blinux-sandbox-foundation\]/);
});

test('Linux sandbox foundation rejects omitted lanes, fake proofs and delayed or weakened execution', () => {
  const body = /^  linux-sandbox-foundation:\n[\s\S]*?(?=^  desktop:)/m.exec(GOOD)[0];
  for (const [before, after] of [
    ['needs: [changes]', 'needs: [changes, web-build]'],
    ['timeout-minutes: 15', 'timeout-minutes: 90'],
    ['fail-fast: true', 'fail-fast: false'],
    ['os: ubuntu-22.04, lane: namespace', 'os: ubuntu-24.04, lane: namespace'],
    ['          - { os: ubuntu-24.04, lane: managed }\n', ''],
    ['lane: managed', 'lane: namespace'],
    ['persist-credentials: false', 'persist-credentials: true'],
    ['node-version-file: .nvmrc', 'node-version: latest'],
    ['actions/setup-node@949feb2413d6458794dcd2491c4babbbce0c15c1', 'actions/setup-node@1111111111111111111111111111111111111111'],
    ['run: npm ci', 'run: npm install'],
    ['node --test packages/desktop/test/native/linux-sandbox-foundation.test.mjs', 'node --test --test-name-pattern=kernel packages/desktop/test/native/linux-sandbox-foundation.test.mjs'],
    ['DILLA_LINUX_SANDBOX_FOUNDATION_LANE: ${{ matrix.lane }}', 'DILLA_LINUX_SANDBOX_FOUNDATION_LANE: omitted'],
    ['      - id: sandbox_foundation\n        run: xvfb-run', '      - id: sandbox_foundation\n        if: false\n        run: xvfb-run'],
    ['    steps:', '    env:\n      APPIMAGE_EXTRACT_AND_RUN: 1\n    steps:'],
    ['          sudo apt-get update', '          sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0\n          sudo apt-get update'],
    ['          if-no-files-found: error', '          if-no-files-found: ignore'],
    ["      - if: always() && contains(fromJSON('[\"success\",\"failure\",\"cancelled\"]'), steps.sandbox_foundation.outcome)\n        uses: actions/upload-artifact@", '      - uses: actions/upload-artifact@'],
    ["steps.sandbox_foundation.outcome)","steps.sandbox_maintenance.outcome)"],
    ["always() && contains(fromJSON('[\"success\",\"failure\",\"cancelled\"]'), steps.sandbox_foundation.outcome)","always()"],
  ]) {
    const changed = body.replace(before, after); assert.notEqual(changed, body, before);
    assert.ok(checkWorkflow(fixture(GOOD.replace(body, changed))).some(problem => problem.includes('Linux sandbox foundation')), before);
  }
  assert.ok(checkWorkflow(fixture(GOOD.replace(body, ''))).some(problem => problem.includes('missing job "linux-sandbox-foundation"')));
  const changed = GOOD.replace(', linux-sandbox-foundation]', ']');
  assert.notEqual(changed, GOOD);
  assert.ok(checkWorkflow(fixture(changed)).some(problem => problem.includes('desktop') && problem.includes('Linux sandbox foundation')));
});

const MAINTENANCE_CLEANUP_IF = `always() && matrix.lane == 'managed' && contains(fromJSON('["success","failure","cancelled"]'), steps.sandbox_maintenance.outcome)`;
const MAINTENANCE_PORTABLE = 'node --test packages/desktop/test/linux-sandbox-maintenance.test.mjs';
const MAINTENANCE_NATIVE = 'node --test packages/desktop/test/native/linux-sandbox-maintenance.test.mjs';
const MAINTENANCE_CLEANUP = 'node packages/desktop/test/native/linux-sandbox-maintenance.test.mjs --cleanup-only';

test('managed maintenance CI runs portable controls then pinned Go/native/always cleanup before unchanged foundation',()=>{
 const body=jobBody('linux-sandbox-foundation'),steps=jobSteps(body);
 const index=command=>steps.findIndex(step=>step.includes('run: '+command));
 const portable=index(MAINTENANCE_PORTABLE),go=steps.findIndex(step=>step.includes('uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7'));
 const native=index(MAINTENANCE_NATIVE),cleanup=index(MAINTENANCE_CLEANUP),foundation=index('xvfb-run -a dbus-run-session -- node --test packages/desktop/test/native/linux-sandbox-foundation.test.mjs');
 assert.ok(portable>=0&&go>portable&&native>go&&cleanup>native&&foundation>cleanup);
 for(const step of [steps[portable],steps[go],steps[native]])assert.ok(step.includes("if: matrix.lane == 'managed'"));
 assert.ok(steps[native].includes('id: sandbox_maintenance'));assert.ok(steps[native].includes('DILLA_LINUX_SANDBOX_FOUNDATION_LANE: managed'));
 assert.ok(steps[cleanup].includes('if: '+MAINTENANCE_CLEANUP_IF));assert.ok(steps[cleanup].includes('timeout-minutes: 1'));
 const proof=steps.find(step=>step.includes('path: packages/desktop/test-results/linux-sandbox-maintenance-managed.json'));
 assert.ok(steps.indexOf(proof)>cleanup&&steps.indexOf(proof)<foundation);
 assert.ok(proof.includes('if-no-files-found: error'));assert.ok(proof.includes('if: '+MAINTENANCE_CLEANUP_IF));assert.ok(proof.includes('retention-days: 3'));
});

test('managed maintenance CI refuses omissions, broad guards, late cleanup, setup drift and filtered native execution',()=>{
 const body=jobBody('linux-sandbox-foundation');const steps=jobSteps(body);
 for(const [before,after]of [
  [MAINTENANCE_PORTABLE,MAINTENANCE_PORTABLE+' --test-name-pattern=partial'],
  [MAINTENANCE_NATIVE,MAINTENANCE_NATIVE.replace('node --test','node --test --test-name-pattern=kernel')],
  [MAINTENANCE_CLEANUP,MAINTENANCE_CLEANUP.replace(' --cleanup-only','')],
  ['timeout-minutes: 1\n        run: '+MAINTENANCE_CLEANUP,'timeout-minutes: 15\n        run: '+MAINTENANCE_CLEANUP],
  [MAINTENANCE_CLEANUP_IF,'always()'],
  [MAINTENANCE_CLEANUP_IF,MAINTENANCE_CLEANUP_IF.replace("matrix.lane == 'managed' && ",'')],
  [MAINTENANCE_CLEANUP_IF,MAINTENANCE_CLEANUP_IF.replace('"cancelled"','"skipped"')],
  [MAINTENANCE_CLEANUP_IF,MAINTENANCE_CLEANUP_IF.replace('always()','success()')],
  ['actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e','actions/setup-go@1111111111111111111111111111111111111111'],
  ['path: packages/desktop/test-results/linux-sandbox-maintenance-managed.json','path: packages/desktop/test-results/**'],
  ['name: linux-sandbox-maintenance-managed\n          path:', 'name: linux-sandbox-maintenance-managed\n          if-no-files-found: ignore\n          path:'],
 ]){
  const changed=body.replace(before,after);assert.notEqual(changed,body,before);
  assert.ok(checkWorkflow(fixture(GOOD.replace(body,changed))).some(problem=>problem.includes('Linux sandbox foundation')),before);
 }
 for(const command of [MAINTENANCE_PORTABLE,MAINTENANCE_NATIVE,MAINTENANCE_CLEANUP]){
  const step=steps.find(step=>step.includes('run: '+command));assert.ok(step);
  // Drop the complete YAML step, not merely a token in a comment.
  const lines=body.split('\n'),start=lines.findIndex(line=>line.trim()==='run: '+command||line.trim()==='- run: '+command);assert.ok(start>=0);
  let first=start;while(first>0&&!lines[first].startsWith('      - '))first--;
  let last=first+1;while(last<lines.length&&!lines[last].startsWith('      - '))last++;
  const changed=[...lines.slice(0,first),...lines.slice(last)].join('\n');
  assert.ok(checkWorkflow(fixture(GOOD.replace(body,changed))).some(problem=>problem.includes('Linux sandbox foundation')));
 }
 // Moving the cleanup behind the mounted foundation could leave privileged
 // leftovers throughout unrelated Electron execution; order is part of proof.
 const cleanupStart=body.indexOf('      - name: Cleanup root sandbox maintenance fixture');assert.ok(cleanupStart>=0);
 const cleanupEnd=body.indexOf('      - ',cleanupStart+8);const block=body.slice(cleanupStart,cleanupEnd);
 const changed=body.slice(0,cleanupStart)+body.slice(cleanupEnd)+block;
 assert.ok(checkWorkflow(fixture(GOOD.replace(body,changed))).some(problem=>problem.includes('Linux sandbox foundation')));
});

test('managed maintenance cleanup allowance is confined to exact job and finalizer blocks',()=>{
 const body=jobBody('linux-sandbox-foundation');const lines=body.split('\n');
 function blockContaining(text){const at=lines.findIndex(line=>line.includes(text));assert.ok(at>=0,text);let start=at;while(start>0&&!lines[start].startsWith('      - '))start--;let end=start+1;while(end<lines.length&&!lines[end].startsWith('      - '))end++;return lines.slice(start,end).join('\n').replace(/\n*$/,'')+'\n';}
 const setup=blockContaining('uses: actions/setup-go@'),cleanup=blockContaining('name: Cleanup root sandbox maintenance fixture'),artifact=blockContaining('name: Preserve root sandbox maintenance proof');
 for(const changed of [body.replace(setup,''),body.replace(setup,setup.replace("        if: matrix.lane == 'managed'\n",'')),body.replace(setup,'')+setup,body.replace(artifact,''),
  body.replace(cleanup,cleanup.replace('        run:', '        continue-on-error: true\n        run:')),
  body.replace(cleanup,cleanup.replace(MAINTENANCE_CLEANUP,MAINTENANCE_CLEANUP+' || true')),
  body.replace(artifact,artifact.replace('if-no-files-found: error','if-no-files-found: ignore')),
  body.replace(artifact,artifact.replace('retention-days: 3','retention-days: 90'))]){
  assert.notEqual(changed,body);assert.ok(checkWorkflow(fixture(GOOD.replace(body,changed))).some(problem=>problem.includes('Linux sandbox foundation')));
 }
 const otherJob=GOOD.replace(NPM_TEST,cleanup+NPM_TEST);assert.notEqual(otherJob,GOOD);
 assert.ok(checkWorkflow(fixture(otherJob)).some(problem=>problem.includes('R22')&&problem.includes('always()')));
 const rebuilt=body.replace(cleanup,cleanup.replace(MAINTENANCE_CLEANUP,MAINTENANCE_NATIVE));
 assert.ok(checkWorkflow(fixture(GOOD.replace(body,rebuilt))).some(problem=>problem.includes('R22')&&problem.includes('always()')));
});

test('Linux sandbox foundation is required exactly once by completion and final gates', () => {
  for (const name of ['ci-work-complete', 'ci-ok']) {
    const body = jobBody(name);
    for (const replacement of ['', 'linux-sandbox-foundation, linux-sandbox-foundation, ']) {
      const changed = body.replace('linux-sandbox-foundation, ', replacement); assert.notEqual(changed, body);
      assert.ok(checkWorkflow(fixture(GOOD.replace(body, changed))).some(problem => name === 'ci-ok' ? problem.includes('"ci-ok"') : problem.includes('completion barrier')));
    }
  }
});
const REAL_RELEASE = readFileSync(join(REPO_ROOT, '.github', 'workflows', 'release.yml'), 'utf8');

const REAL_DESKTOP = readFileSync(join(REPO_ROOT, '.github', 'workflows', 'desktop.yml'), 'utf8');
test('desktop checker refuses missing installed native package acceptance gates', () => {
 const block = REAL_DESKTOP.split('\n  native-linux-package-acceptance:\n')[1]?.split(/\n  [a-z][a-z0-9-]*:\n/)[0] ?? '';
 const root = fixture(GOOD);
 writeFileSync(join(root, '.github/workflows/desktop.yml'), REAL_DESKTOP.replace('\n  native-linux-package-acceptance:\n' + block, ''));
 assert.ok(checkDesktopWorkflow(root).some(problem => problem.includes('native package acceptance')));
});
test('native format acceptance rejects skipped/commented runs, omitted formats and foreign artifacts', () => {
 const block = REAL_DESKTOP.split('\n  native-linux-package-acceptance:\n')[1]?.split(/\n  [a-z][a-z0-9-]*:\n/)[0] ?? '';
 assert.ok(block);
 for (const [before, after] of [
  ['format: aur, label: AUR', 'format: aur, label: RPM'],
  ['needs: [test-tools, native-linux-packages]', 'needs: test-tools'],
  ['persist-credentials: false', 'persist-credentials: true'],
  ['run: bash e2e/web/support/native-linux-package-run.sh', '# run: bash e2e/web/support/native-linux-package-run.sh'],
  ['name: Installed ${{ matrix.label }} package acceptance', 'name: Installed ${{ matrix.label }} package acceptance\n        if: false'],
  ['name: desktop-installers-linux-packages-x64', 'run-id: 123\n          name: desktop-installers-linux-packages-x64'],
 ]) {
  assert.ok(block.includes(before));
  const root = fixture(GOOD); writeFileSync(join(root, '.github/workflows/desktop.yml'), REAL_DESKTOP.replace(block, block.replace(before, after)));
  assert.ok(checkDesktopWorkflow(root).some(problem => problem.includes('native package acceptance')), before);
 }
});
test('desktop workflow checker requires the native feasibility job and its installed ASAR comparison', () => {
 const stock = /^  native-stock-feasibility:\n[\s\S]*?(?=^  [a-z][\w-]*:)/m.exec(REAL_DESKTOP)[0];
 for (const [before, after] of [
  ['  native-stock-feasibility:', '  removed-stock-feasibility:'],
  ['          cmp "$RUNNER_TEMP/native-extracted/opt/Dilla/resources/app.asar" /opt/Dilla/resources/app.asar', '          # cmp "$RUNNER_TEMP/native-extracted/opt/Dilla/resources/app.asar" /opt/Dilla/resources/app.asar'],
  ['          persist-credentials: false', '          persist-credentials: true'],
 ]) {
  const root = fixture(GOOD), changed = REAL_DESKTOP.replace(stock, stock.replace(before, after));
  assert.notEqual(changed, REAL_DESKTOP);
  writeFileSync(join(root, '.github/workflows/desktop.yml'), changed);
  assert.ok(checkDesktopWorkflow(root).some(problem => problem.includes('native stock feasibility')));
 }
});
function addE2ePackage(root) {
  mkdirSync(join(root, 'e2e'), { recursive: true });
  writeFileSync(join(root, 'e2e/package.json'), readFileSync(join(REPO_ROOT, 'e2e/package.json')));
}

test('desktop-only CI keeps shared core security and removes unsupported browser client lanes', () => {
  assert.doesNotMatch(REAL_CI, /^  (?:firefox-[^:]+|webkit-[^:]+|chromium-(?:web-[123]|voice|core)):/m);
  assert.match(REAL_CI, /^  core-integration:/m);
  assert.match(REAL_CI, /uses: \.\/.github\/workflows\/desktop.yml/);
  const scripts = JSON.parse(readFileSync(join(REPO_ROOT, 'e2e/package.json'), 'utf8')).scripts;
  for (const name of ['test:e2e:matrix', 'test:e2e:media', 'test:e2e:spike', 'test:e2e:core']) assert.doesNotMatch(scripts[name], /firefox|webkit/);
});

test('CI refuses reinstalling an unsupported compatibility browser in shared harness jobs', () => {
  const changed = GOOD.replace('npx playwright install --with-deps chromium', 'npx playwright install --with-deps chromium firefox');
  assert.notEqual(changed, GOOD);
  assert.ok(checkWorkflow(fixture(changed)).some(problem => problem.includes('unsupported browser engine')));
});

test('desktop acceptance requires every native target, Electron tests, packaging and fuse verification', () => {
  assert.deepEqual(checkDesktopWorkflow(REPO_ROOT), []);
  for (const removed of ['node packages/desktop/scripts/prepare-browser-sandbox.mjs', 'node packages/desktop/scripts/run-performance.mjs', 'name: Desktop performance comparison', 'name: desktop-performance-', 'playwright.desktop-voice.config.ts', 'DILLA_E2E_NOTIFICATIONS', 'dunst', 'with-window-manager.sh', 'prepare-windows-audio.ps1', '--grep-invert "application-audio.spec.ts"', 'Application audio encryption and exclusion', 'application-audio.spec.ts', 'os: macos-15,', 'npm run typecheck -w @dilla/desktop', 'npm run build:validation -w @dilla/desktop', 'node packages/desktop/scripts/check-fuses.mjs', 'node --test packages/desktop/test/native/mac-upgrade.test.mjs', 'node --test packages/desktop/test/native/installed-upgrade.test.mjs', 'libfuse2t64', 'name: desktop-installed-upgrade-', 'npm run dist -w @dilla/desktop', 'arch: x64, shard: 3', "specs: 'web/installed-upgrade-account.spec.ts", 'os: [ubuntu-24.04, windows-latest, macos-15]', 'playwright.desktop.config.ts']) {
    const root = mkdtempSync(join(tmpdir(), 'dilla-desktop-ci-'));
    mkdirSync(join(root, '.github/workflows'), { recursive: true });
    addE2ePackage(root);
    writeFileSync(join(root, '.github/workflows/desktop.yml'), REAL_DESKTOP.replaceAll(removed, 'removed'));
    assert.ok(checkDesktopWorkflow(root).length > 0, removed);
  }
});

test('installed Linux sandbox preparation cannot lose build prerequisites, controls or exact proof', () => {
 for(const [before,after] of [
  ['libgtk-3-dev pkg-config','libgtk-3-0'],
  ['node packages/desktop/scripts/test-linux-sandbox-package.mjs packages/desktop/out','true'],
  ["name: Linux AppImage final package inspection mutations\n        if: runner.os == 'Linux'","name: Linux AppImage final package inspection mutations\n        if: false"],
  ['packages/desktop/out/*.sandbox.json','packages/desktop/out/*.other'],
  ['CGO_ENABLED=0 go test ./packages/desktop/test/fixtures/linux-installed-sandbox-bootstrap','true'],
  ['node --test packages/desktop/test/linux-installed-sandbox-bootstrap.test.mjs','node --test --test-name-pattern=none packages/desktop/test/linux-installed-sandbox-bootstrap.test.mjs'],
  ["name: Linux installed sandbox prerequisite controls\n        if: runner.os == 'Linux'","name: Linux installed sandbox prerequisite controls\n        if: false"],
  ['path: ${{ runner.temp }}/dilla-installed-sandbox-initial.json','path: unknown.json'],
  ["name: Initial installed sandbox prerequisite proof\n        if: runner.os == 'Linux'","name: Initial installed sandbox prerequisite proof\n        if: always()"],
 ]){
  const root=fixture(GOOD),changed=REAL_DESKTOP.replaceAll(before,after);assert.notEqual(changed,REAL_DESKTOP,before);
  writeFileSync(join(root,'.github/workflows/desktop.yml'),changed);
  assert.ok(checkDesktopWorkflow(root).some(p=>p.includes('installed sandbox')),before);
 }
});

test('native desktop Go setup remains unconditional for every platform', () => {
 const before = 'cache: npm\n      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7';
 assert.ok(REAL_DESKTOP.includes(before)); assert.deepEqual(checkDesktopWorkflow(REPO_ROOT), []);
 for (const guard of ["runner.os == 'Windows'", "runner.os == 'Linux'", 'false', 'success()']) {
  const root=fixture(GOOD), changed=REAL_DESKTOP.replace(before, `cache: npm\n      - if: ${guard}\n        uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7`);
  assert.notEqual(changed,REAL_DESKTOP); writeFileSync(join(root,'.github/workflows/desktop.yml'),changed);
  assert.ok(checkDesktopWorkflow(root).some(problem=>problem.includes('pinned native Go')),guard);
 }
 const block='      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7\n        with:\n          go-version-file: go.mod\n          cache-dependency-path: go.sum\n';
 const nativeStart=REAL_DESKTOP.indexOf('  native:\n'), at=REAL_DESKTOP.indexOf(block,nativeStart); assert.ok(at>=0);
 const without=REAL_DESKTOP.slice(0,at)+REAL_DESKTOP.slice(at+block.length), unit='      - run: npm test -w @dilla/desktop\n', unitAt=without.indexOf(unit,nativeStart); assert.ok(unitAt>=0);
 const reordered=without.slice(0,unitAt+unit.length)+block+without.slice(unitAt+unit.length), root=fixture(GOOD);
 writeFileSync(join(root,'.github/workflows/desktop.yml'),reordered); assert.ok(checkDesktopWorkflow(root).some(problem=>problem.includes('pinned native Go')));
});

test('every macOS Electron consumer seals its npm runtime immediately after installation', () => {
 assert.deepEqual(checkDesktopWorkflow(REPO_ROOT), []);
 const seal = "      - name: Seal macOS npm Electron runtime for native notifications\n        if: runner.os == 'macOS'\n        run: node e2e/web/support/prepare-mac-electron-runtime.ts\n";
 const install = '      - run: node node_modules/electron/install.js\n';
 for (const job of ['native', 'client', 'voice']) {
  const start = REAL_DESKTOP.indexOf(`\n  ${job}:\n`);
  const tail = REAL_DESKTOP.slice(start), next = /\n  [a-z][a-z0-9-]*:\n/.exec(tail.slice(1));
  const end = next ? start + 1 + next.index : REAL_DESKTOP.length;
  const body = REAL_DESKTOP.slice(start, end);
  assert.ok(body.includes(install + seal), job);
  for (const changed of [body.replace(seal, ''), body.replace(seal, seal.replace("runner.os == 'macOS'", "runner.os == 'Linux'")),
   body.replace(install + seal, seal + install), body.replace(seal, '') + seal]) {
   const root = fixture(GOOD);
   writeFileSync(join(root, '.github/workflows/desktop.yml'), REAL_DESKTOP.slice(0, start) + changed + REAL_DESKTOP.slice(end));
   assert.ok(checkDesktopWorkflow(root).some(problem => problem.includes('macOS npm Electron runtime') && problem.includes(job)), job);
  }
 }
});

test('native failure diagnostics run only after a successful desktop build', () => {
 const guard="if: failure() && steps.desktop-build.outcome == 'success'";
 const before=`${guard}\n        uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7\n        with:\n          name: desktop-failure-`;
 assert.ok(REAL_DESKTOP.includes(before)); assert.deepEqual(checkDesktopWorkflow(REPO_ROOT),[]);
 for(const replacement of ['failure()', "failure() && steps.desktop-build.outcome != 'success'", 'always()']) {
  const root=fixture(GOOD), changed=REAL_DESKTOP.replace(before,before.replace(guard,`if: ${replacement}`)); assert.notEqual(changed,REAL_DESKTOP);
  writeFileSync(join(root,'.github/workflows/desktop.yml'),changed); assert.ok(checkDesktopWorkflow(root).some(p=>p.includes('native failure diagnostics')),replacement);
 }
 const block=`${before}\${{ matrix.platform }}-\${{ matrix.arch }}\n          path: |\n            packages/desktop/test-results\n            \${{ runner.temp }}/dilla-windows-audio/*.log\n            \${{ runner.temp }}/dilla-windows-audio/*.out\n            \${{ runner.temp }}/dilla-windows-audio/*.err\n          if-no-files-found: error`;
 assert.ok(REAL_DESKTOP.includes(block));
 for(const changed of [REAL_DESKTOP.replace(block,block.replace('if-no-files-found: error','if-no-files-found: warn')),REAL_DESKTOP.replace(block,block.replace('packages/desktop/test-results','packages/desktop/missing')),REAL_DESKTOP.replace('id: desktop-build','id: other-build')]) {
  const root=fixture(GOOD);writeFileSync(join(root,'.github/workflows/desktop.yml'),changed);assert.ok(checkDesktopWorkflow(root).some(p=>p.includes('native failure diagnostics')));
 }
});

test('installed account coverage retains OS credential protection, normal FUSE and owning-shard proof collection', () => {
  for (const [before, after] of [
    ['e2e/web/support/installed-keyring.sh', 'removed-keyring.sh'],
    ['libasound2t64 gnome-keyring libfuse2t64', 'libasound2t64'],
    ['node packages/desktop/scripts/installed-account-proof.mjs e2e/desktop-client-results', 'echo skipped-account-proof'],
    ["if: steps.installed-account.outputs.present == 'true'", 'if: false'],
    ['name: desktop-installed-account-', 'name: unrelated-proof-'],
  ]) {
    const root = mkdtempSync(join(tmpdir(), 'dilla-account-ci-'));
    mkdirSync(join(root, '.github/workflows'), { recursive: true });
    addE2ePackage(root);
    const changed = REAL_DESKTOP.replaceAll(before, after); assert.notEqual(changed, REAL_DESKTOP, before);
    writeFileSync(join(root, '.github/workflows/desktop.yml'), changed);
    assert.ok(checkDesktopWorkflow(root).some(problem => /installed account/i.test(problem)), before);
  }
});

test('native build provenance authenticates final fused installers and is uploaded with their bytes', () => {
  const record = '      - name: Record native desktop build provenance\n        run: node packages/desktop/scripts/build-provenance.mjs packages/desktop/out ${{ matrix.platform }}-${{ matrix.arch }}\n';
  const fuse = '      - run: node packages/desktop/scripts/check-fuses.mjs\n';
  for (const changed of [
    REAL_DESKTOP.replace(record, ''),
    REAL_DESKTOP.replace(record, record.replace('        run:', '        if: false\n        run:')),
    REAL_DESKTOP.replace('            packages/desktop/out/build-provenance-*.json\n', ''),
    REAL_DESKTOP.replace(fuse, '').replace('      - name: Build installable desktop artifacts\n', fuse + '      - name: Build installable desktop artifacts\n'),
  ]) {
    assert.notEqual(changed, REAL_DESKTOP);
    const root = mkdtempSync(join(tmpdir(), 'dilla-build-proof-ci-'));
    mkdirSync(join(root, '.github/workflows'), { recursive: true });
    addE2ePackage(root);
    writeFileSync(join(root, '.github/workflows/desktop.yml'), changed);
    assert.ok(checkDesktopWorkflow(root).some(problem => /build provenance/i.test(problem)));
  }
});

test('desktop voice cannot shard its frozen-clock suite', () => {
  const root = mkdtempSync(join(tmpdir(), 'dilla-desktop-voice-ci-'));
  mkdirSync(join(root, '.github/workflows'), { recursive: true });
    addE2ePackage(root);
  writeFileSync(join(root, '.github/workflows/desktop.yml'), REAL_DESKTOP.replaceAll('playwright.desktop-voice.config.ts --project=electron-voice --workers=1', 'playwright.desktop-voice.config.ts --project=electron-voice --workers=1 --shard=1/2'));
  assert.ok(checkDesktopWorkflow(root).some(problem => problem.includes('single frozen-clock worker')));
});

test('Electron voice runs before downloading the browser performance baseline', () => {
  const root = mkdtempSync(join(tmpdir(), 'dilla-voice-baseline-order-'));
  mkdirSync(join(root, '.github/workflows'), { recursive: true });
  addE2ePackage(root);
  const start = REAL_DESKTOP.indexOf('\n  voice:\n'), end = REAL_DESKTOP.indexOf('\n  client:\n', start);
  const command = '      - run: npm exec -w @dilla/e2e -- playwright install --with-deps chromium\n';
  let voice = REAL_DESKTOP.slice(start, end);
  assert.equal(voice.split(command).length, 2);
  voice = voice.replace(command, '').replace('      - name: Desktop voice end-to-end coverage in Electron\n', command + '      - name: Desktop voice end-to-end coverage in Electron\n');
  writeFileSync(join(root, '.github/workflows/desktop.yml'), REAL_DESKTOP.slice(0, start) + voice + REAL_DESKTOP.slice(end));
  assert.ok(checkDesktopWorkflow(root).some(problem => /browser baseline.*after Electron voice/.test(problem)));
});

test('installed upgrade acceptance cannot omit either NSIS or normal FUSE launch', () => {
  for (const [before, after] of [
    ["if: runner.os == 'Windows'\n        run: node --test packages/desktop/test/native/installed-upgrade.test.mjs", "if: runner.os == 'macOS'\n        run: node --test packages/desktop/test/native/installed-upgrade.test.mjs"],
    ["if: runner.os == 'Linux'\n        run: xvfb-run -a dbus-run-session -- bash packages/desktop/scripts/with-window-manager.sh node --test packages/desktop/test/native/installed-upgrade.test.mjs", "if: runner.os == 'macOS'\n        run: xvfb-run -a dbus-run-session -- bash packages/desktop/scripts/with-window-manager.sh node --test packages/desktop/test/native/installed-upgrade.test.mjs"],
    ["name: Installed upgrade acceptance\n        if: runner.os == 'Linux'", "name: Installed upgrade acceptance\n        if: runner.os == 'Linux'\n        env:\n          APPIMAGE_EXTRACT_AND_RUN: 1"],
  ]) {
    const root = mkdtempSync(join(tmpdir(), 'dilla-installed-ci-'));
    mkdirSync(join(root, '.github/workflows'), { recursive: true });
    addE2ePackage(root);
    const changed = REAL_DESKTOP.replace(before, after);
    assert.notEqual(changed, REAL_DESKTOP);
    writeFileSync(join(root, '.github/workflows/desktop.yml'), changed);
    assert.ok(checkDesktopWorkflow(root).some(problem => /installed.*acceptance|FUSE/.test(problem)));
  }
});

test('installed proof is produced after Playwright clears output and uploaded afterwards', () => {
  const sparkle = "      - name: Installed unsigned Sparkle upgrade and signed-feed rejection\n        if: runner.os == 'macOS'\n        run: node --test packages/desktop/test/native/mac-upgrade.test.mjs\n";
  const moved = REAL_DESKTOP.replace(sparkle, '').replace('      - name: Linux display and Chromium sandbox helper', sparkle + '      - name: Linux display and Chromium sandbox helper');
  assert.notEqual(moved, REAL_DESKTOP);
  const root = mkdtempSync(join(tmpdir(), 'dilla-installed-proof-'));
  mkdirSync(join(root, '.github/workflows'), { recursive: true });
    addE2ePackage(root);
  writeFileSync(join(root, '.github/workflows/desktop.yml'), moved);
  assert.ok(checkDesktopWorkflow(root).some(problem => problem.includes('Playwright cleanup')));
});

test('desktop CI refuses continuing matrix jobs or tests after the first failure', () => {
  const good = REAL_DESKTOP.replaceAll('fail-fast: false', 'fail-fast: true').replaceAll('--max-failures=3', '--max-failures=1');
  for (const [before, after] of [['fail-fast: true', 'fail-fast: false'], ['--max-failures=1', '--max-failures=3']]) {
    const root = mkdtempSync(join(tmpdir(), 'dilla-desktop-first-failure-'));
    mkdirSync(join(root, '.github/workflows'), { recursive: true });
    addE2ePackage(root);
    writeFileSync(join(root, '.github/workflows/desktop.yml'), good.replace(before, after));
    assert.ok(checkDesktopWorkflow(root).some(problem => problem.includes('first failure')), after);
  }
});

test('CI rejects removing the whole-run watcher or its completion barrier', () => {
  for (const name of ['ci-watchdog', 'ci-work-complete']) {
    const changed = REAL_CI.replace(new RegExp(`^  ${name}:\\n[\\s\\S]*?(?=^  [a-z][\\w-]*:|$(?![\\s\\S]))`, 'm'), '');
    assert.notEqual(changed, REAL_CI);
    const problems = checkWorkflow(fixture(changed));
    assert.ok(problems.some(problem => problem.includes(`missing job "${name}"`)), problems.join('\n'));
  }
});

test('CI rejects an unsafe, delayed or unbounded whole-run watcher', () => {
  for (const [before, after] of [
    ["github.event.pull_request.head.repo.full_name == github.repository)", 'true)'],
    ['timeout-minutes: 90', 'timeout-minutes: 360'],
    ['persist-credentials: false', 'persist-credentials: true'],
    ['actions: write', 'actions: read'],
    ['actions: write', 'actions: write\n      contents: write'],
    ['  ci-watchdog:\n', '  ci-watchdog:\n    name: renamed-watcher\n'],
    ['persist-credentials: false', 'persist-credentials: false\n          repository: untrusted/fork'],
    ['GH_TOKEN: ${{ github.token }}', 'GH_TOKEN: ${{ secrets.PUBLISH_TOKEN }}'],
    ['  ci-watchdog:\n', '  ci-watchdog:\n    needs: [changes]\n'],
    ['run: python3 scripts/ci-watchdog.py', 'run: echo done'],
  ]) {
    const changed = REAL_CI.replace(before, after);
    assert.notEqual(changed, REAL_CI, before);
    assert.ok(checkWorkflow(fixture(changed)).some(problem => problem.includes('unsafe watchdog')), before);
  }
});

test('CI rejects a completion barrier that misses jobs or waits for the watcher', () => {
  for (const changed of [
    REAL_CI.replace(/(  ci-work-complete:\n    needs: \[)changes, /, '$1'),
    REAL_CI.replace(/(  ci-work-complete:\n    needs: \[)/, '$1ci-watchdog, '),
    REAL_CI.replace(/(  ci-work-complete:\n    needs: \[[^\]]*\]\n)    if: always\(\)\n/, '$1'),
  ]) {
    assert.notEqual(changed, REAL_CI);
    assert.ok(checkWorkflow(fixture(changed)).some(problem => problem.includes('completion barrier')));
  }
});

test('CI accepts a skipped watcher only for fork pull requests', () => {
  const changed = REAL_CI.replace("      - if: needs.ci-watchdog.result == 'skipped' && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository)\n        run: exit 1\n", '');
  assert.notEqual(changed, REAL_CI);
  assert.ok(checkWorkflow(fixture(changed)).some(problem => problem.includes('skipped watcher')));
});

test('desktop tooling must be built once per native target and verified before every consumer', () => {
  for (const [before, after] of [
    ['node scripts/e2e-test-tools.mjs build artifacts/e2e-test-tools', 'echo skipped build'],
    ['needs: test-tools', 'needs: rust-native'],
    ['DILLA_E2E_TOOLING_DIR:', 'UNUSED_TOOLING_DIR:'],
    ['node scripts/e2e-test-tools.mjs verify artifacts/e2e-test-tools', 'echo skipped verification'],
    ['name: dilla-e2e-tools-${{ matrix.platform }}-${{ matrix.arch }}-${{ github.sha }}', 'name: stale-tools'],
    ['overwrite: true', 'overwrite: false'],
    ["go test ./internal/ops -run 'Lock|BackupAndServe|ServeAdmits'", 'echo skipped locks'],
  ]) {
    const root = mkdtempSync(join(tmpdir(), 'dilla-tooling-ci-'));
    mkdirSync(join(root, '.github/workflows'), { recursive: true });
    addE2ePackage(root);
    const changed = REAL_DESKTOP.replaceAll(before, after);
    assert.notEqual(changed, REAL_DESKTOP, before);
    writeFileSync(join(root, '.github/workflows/desktop.yml'), changed);
    assert.ok(checkDesktopWorkflow(root).some(problem => /tooling|directory locks/i.test(problem)), before);
  }
});

test('desktop native acceptance stays independent and tooling never comes from another run', () => {
  for (const changed of [
    REAL_DESKTOP.replace('  native:\n', '  native:\n    needs: test-tools\n'),
    REAL_DESKTOP.replaceAll('path: artifacts/e2e-test-tools\n', 'path: artifacts/e2e-test-tools\n          run-id: 123\n'),
    REAL_DESKTOP.replace('  client:\n', '  client:\n    unrelated:\n      - run: cargo build -p dilla-testkit --release --locked\n'),
  ]) {
    const root = mkdtempSync(join(tmpdir(), 'dilla-tooling-scope-'));
    mkdirSync(join(root, '.github/workflows'), { recursive: true });
    addE2ePackage(root);
    writeFileSync(join(root, '.github/workflows/desktop.yml'), changed);
    assert.ok(checkDesktopWorkflow(root).some(problem => /tooling|independent/i.test(problem)));
  }
});


test('compiled helper recovery tests receive the same-commit helper and cannot silently skip', () => {
  const native = REAL_CI.split('\n  rust-native:\n')[1].split('\n  rust-wasi:\n')[0];
  const unit = REAL_CI.split('\n  go:\n')[1].split('\n  go-native:\n')[0];
  const go = REAL_CI.split('\n  go-native:\n')[1]?.split('\n  go-ds:\n')[0] ?? '';
  assert.ok(go, 'native integrations need a separate bounded job instead of consuming the unit job budget');
  assert.match(go, /timeout-minutes: 15/);
  assert.match(unit, /timeout-minutes: 30/);
  assert.doesNotMatch(unit, /DILLA_BOT_HELPER_INTEGRATION/);
  assert.match(native, /cargo build -p dilla-bot-core --release --locked/);
  assert.match(native, /name: dilla-bot-core\n\s+path: target\/release\/dilla-bot-core/);
  assert.match(go, /needs:.*rust-native/);
  assert.match(go, /name: dilla-bot-core\n\s+path: target\/release/);
  const run = jobSteps(go).find(step => step.some(line => line.includes('go test -race ./internal/bot/native ./cmd/dillabot')));
  assert.ok(run, 'the native adapter and production dependency tests must run with the compiled helper');
  assert.ok(run.some(line => line.includes("DILLA_BOT_HELPER_INTEGRATION: '1'")));
  assert.ok(run.some(line => line.includes("DILLA_CONTROL_E2E: '1'")));
  assert.ok(run.some(line => line.includes('DILLA_BOT_HELPER_BIN: ${{ github.workspace }}/target/release/dilla-bot-core')));
  assert.ok(run.some(line => line.includes('./internal/bot/music/persistence')), 'encrypted queue upgrades must run against the compiled helper');
  const steps = jobSteps(go);
  const fixtureBuild = steps.findIndex(step => step.some(line => line.includes('cargo test -p dilla-core --test native_bot_group_peer_fixture --no-run --locked')));
  const integration = steps.findIndex(step => step.some(line => line.includes('go test -race ./internal/bot/native ./cmd/dillabot')));
  const npmInstall = steps.findIndex(step => step.some(line => line.includes('run: npm ci')));
  assert.ok(npmInstall >= 0 && npmInstall < integration, 'install esbuild before opting into the real client control tests');
  const botRelease = REAL_RELEASE.split('\n  bot-release:\n')[1].split(/^  \S/m)[0];
  assert.doesNotMatch(botRelease, /DILLA_CONTROL_E2E|run: npm ci/);
  assert.ok(fixtureBuild >= 0 && fixtureBuild < integration, 'compile the Rust peer before the integration test starts its bounded readiness wait');
});

test('workflow steps execute only commit-pinned GitHub actions', () => {
  for (const workflow of [REAL_CI, REAL_RELEASE]) {
    for (const match of workflow.matchAll(/^\s*-?\s*uses:\s*(\S+)/gm)) {
      if (match[1] === './.github/workflows/desktop.yml') continue;
      assert.match(match[1], /^actions\/[\w-]+(?:\/[\w-]+)?@[0-9a-f]{40}$/);
    }
  }
});

test('clang 23 build jobs stay on the Ubuntu 24.04 image they install packages for', () => {
  for (const job of ['rust-wasm-node', 'browser-spike', 'browser-media', 'web-build']) {
    assert.match(REAL_CI, new RegExp(`^  ${job}:\\n(?:    (?:name|needs|if):.*\\n)*    runs-on: ubuntu-24\\.04$`, 'm'));
  }
  assert.match(REAL_RELEASE, /^  web-build:\n    runs-on: ubuntu-24\.04$/m);
});

test('the CI checker rejects a third-party action even when pinned', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(
    'actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7',
    'taiki-e/install-action@1168f6810a3cb30a3514bf5756a3b7f0422c6df8 # v2',
  )));
  assert.ok(problems.some((problem) => problem.includes('GitHub-owned action')), problems.join('\n'));
});

test('the CI checker rejects a movable GitHub action tag', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(
    'actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7',
    'actions/checkout@v7',
  )));
  assert.ok(problems.some((problem) => problem.includes('40-hex commit')), problems.join('\n'));
});

test('the CI checker rejects a flow-style action step', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(
    NPM_TEST,
    '      - { name: unsafe, uses: taiki-e/install-action@1168f6810a3cb30a3514bf5756a3b7f0422c6df8 }\n' + NPM_TEST,
  )));
  assert.ok(problems.some((problem) => problem.includes('flow-style action')), problems.join('\n'));
});

test('Rust CI and release setup use the pinned rustup toolchain without third-party helper actions', () => {
  const toolchain = readFileSync(join(REPO_ROOT, 'rust-toolchain.toml'), 'utf8');
  assert.match(toolchain, /channel = "1\.98\.1"/);
  for (const workflow of [REAL_CI, REAL_RELEASE]) {
    assert.doesNotMatch(workflow, /uses:\s*(?:dtolnay\/rust-toolchain|Swatinem\/rust-cache)@/);
    assert.match(workflow, /- run: rustup show active-toolchain/);
  }
});

// The go job's test step: every package but internal/ds and internal/api, which go-ds and go-api run (CI budget, fix wave).
const GO_TEST = 'go test -race -shuffle=on -timeout 20m $(go list ./... | grep -vx -e github.com/dilla-chat/dilla/internal/ds -e github.com/dilla-chat/dilla/internal/api)';

/** The browser jobs of GOOD, generated from BROWSER_WEB_JOBS: one job per engine, suite and web shard. */
function browserJobsFixture() {
  return Object.entries(BROWSER_WEB_JOBS).map(([job, spec]) => `  ${job}:
    runs-on: ubuntu-latest
    timeout-minutes: ${spec.timeout}
    needs: [rust-native, rust-wasi, web-build]
    steps:
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-core-wasi
          path: internal/mlswasi/testdata
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-testkit
          path: artifacts
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-web-dist
          path: packages/web/dist
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-core-harness
          path: packages/client-core/harness-dist
      - run: npx playwright install --with-deps ${spec.engine}
${spec.engine === 'firefox' && spec.suite !== 'core' ? '      - run: sh scripts/ci-audio-server.sh\n' : ''}      - run: ${browserSuiteCommand(spec)}
        env:
          DILLA_TESTKIT: \${{ github.workspace }}/artifacts/dilla-testkit
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        if: failure()
        with:
          name: browser-web-results-${job}
          path: e2e/test-results
          if-no-files-found: error
`).join('\n');
}

const GOOD_BASE = `name: ci
on:
  push:
    branches: [main]
  pull_request:
permissions:
  contents: read
concurrency:
  group: \${{ github.workflow }}-\${{ github.ref }}
  cancel-in-progress: true
env:
  CARGO_TERM_COLOR: always
jobs:
  node:
    runs-on: ubuntu-latest
    steps:
      - if: needs.changes.outputs.web != 'false'
        run: npm test
      - name: vectors are up to date
        if: needs.changes.outputs.web != 'false'
        run: npm run vectors
      - run: npm run test:css-check
      - run: npm run check:css
      - run: npm run test:ci-check
      - run: npm run test:wasm-size-check
      - run: npm run test:manifest-writer
      - run: npm run test:install
      - uses: actions/cache@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0
        if: needs.changes.outputs.text != 'false'
        with:
          path: \${{ runner.temp }}/ucd
          key: ucd-\${{ hashFiles('scripts/gen-profile-text-tables.mjs') }}
      - name: profile-text tables are current
        if: needs.changes.outputs.text != 'false'
        run: node scripts/gen-profile-text-tables.mjs --download --check "$RUNNER_TEMP/ucd"
  ui:
    runs-on: ubuntu-latest
    steps:
      - run: npm test -w packages/ui
      - run: node packages/ui/scripts/shoot-stories.mjs 6016 target/ui-screens/web-2b form- ceremony- shell- conversation- settings- screens- primitives-channelrow-- conversation-messagetoolbar-- conversation-attachmentcard-- screens-conversation--
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with:
          name: ui-screens
          path: target/ui-screens/web-2b
          if-no-files-found: error
      - run: node packages/ui/scripts/shoot-stories.mjs 6027 target/ui-screens/web-2c/desktop trust-safetycompare-- trust-trustalert-- search-searchpalette-- status-typingindicator-- settings-diagnosticspanel-- shell-tabhand-over-- screens-trustandpolish--
      - run: node packages/ui/scripts/shoot-stories.mjs 6027 target/ui-screens/web-2c/phone trust-safetycompare-- trust-trustalert-- search-searchpalette-- status-typingindicator-- settings-diagnosticspanel-- shell-tabhand-over-- screens-trustandpolish-- --viewport=360x740
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with:
          name: ui-screens-web-2c
          path: |
            target/ui-screens/web-2c/desktop/**
            target/ui-screens/web-2c/phone/**
          if-no-files-found: error
      - run: node packages/ui/scripts/shoot-stories.mjs 6038 target/ui-screens/web-3/desktop voice- media-mediaplayer-- screens-voice--
      - run: node packages/ui/scripts/shoot-stories.mjs 6038 target/ui-screens/web-3/phone voice- media-mediaplayer-- screens-voice-- --viewport=360x740
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with:
          name: ui-screens-web-3
          path: target/ui-screens/web-3/**
          if-no-files-found: error
  rust-native:
    runs-on: ubuntu-latest
    steps:
      - if: needs.changes.outputs.core != 'false'
        run: cargo fmt --all --check
      - if: needs.changes.outputs.core != 'false'
        run: cargo clippy --workspace --all-targets --all-features --locked -- -D warnings
      - if: needs.changes.outputs.core != 'false'
        run: |
          sudo prlimit --pid "$$" --memlock=268435456:268435456
          cargo test --workspace --all-features --locked
      - run: cargo build -p dilla-testkit --release --locked
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with:
          name: dilla-testkit
          path: target/release/dilla-testkit
          if-no-files-found: error
  rust-wasm-node:
    runs-on: ubuntu-latest
    steps:
      - run: rustup show active-toolchain
      - run: bash scripts/install-ci-tool.sh wasm-bindgen
      - run: bash scripts/install-ci-tool.sh wasm-pack
      - run: cargo test -p dilla-core-wasm --target wasm32-unknown-unknown --locked --features vectors
      - run: cargo clippy -p dilla-core-wasm --target wasm32-unknown-unknown --lib --locked -- -D warnings
      - run: wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/media/wasm
      - run: wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/core-wasm/pkg
      - run: node scripts/check-wasm-size.mjs packages/core-wasm/pkg/dilla_core_wasm_bg.wasm
      - run: npm ci
      - run: npm run typecheck -w @dilla/media
      - run: npm run typecheck -w @dilla/core-wasm
      - run: npm run test:wasm -w @dilla/media
      - run: npm run check:schema -w @dilla/media
  rust-wasi:
    runs-on: ubuntu-latest
    steps:
      - run: cargo build -p dilla-core-wasi --target wasm32-wasip1 --release --locked
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with:
          name: dilla-core-wasi
          path: target/wasm32-wasip1/release/dilla_core_wasi.wasm
          if-no-files-found: error
  vectors:
    runs-on: ubuntu-latest
    steps:
      - run: npm run vectors
      - run: git diff --exit-code -- protocol/vectors
      - run: cargo run -p dilla-testkit --bin dilla-testkit --locked -- vectors
  browser-spike:
    runs-on: ubuntu-latest
    timeout-minutes: 20
    steps:
      - run: bash scripts/install-ci-tool.sh wasm-bindgen
      - run: bash scripts/install-ci-tool.sh wasm-pack
      - run: wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir spike/pkg
      - run: npm run test:e2e:matrix -w @dilla/e2e
  deny:
    runs-on: ubuntu-latest
    steps:
      - run: bash scripts/install-ci-tool.sh cargo-deny
      - run: cargo deny --all-features check advisories bans licenses sources
  go:
    runs-on: ubuntu-latest
    timeout-minutes: 30
    needs: [rust-wasi]
    steps:
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-core-wasi
          path: internal/mlswasi/testdata
      - run: sudo apt-get update && sudo apt-get install -y libgtk-3-dev
      - run: go vet ./...
      - run: CGO_ENABLED=0 go build -tags dillapins ./internal/deps
      - run: go test -race -shuffle=on -timeout 20m $(go list ./... | grep -vx -e github.com/dilla-chat/dilla/internal/ds -e github.com/dilla-chat/dilla/internal/api)
      - run: ${NON_RACE_STEP}
      - run: CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' ./cmd/dillad

  browser-media:
    runs-on: ubuntu-latest
    timeout-minutes: 25
    needs: [rust-native, rust-wasi]
    steps:
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-core-wasi
          path: internal/mlswasi/testdata
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-testkit
          path: artifacts
      - run: bash scripts/install-ci-tool.sh wasm-bindgen
      - run: bash scripts/install-ci-tool.sh wasm-pack
      - run: wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/media/wasm
      - run: go build -o target/dilla-mediabot ./cmd/dilla-mediabot
      - run: node packages/media/scripts/extract-rnnoise-wasm.mjs
      - run: npx playwright install --with-deps chromium
      - run: sh scripts/ci-audio-server.sh
      - run: npm run test:e2e:media -w @dilla/e2e
        env:
          DILLA_TESTKIT: \${{ github.workspace }}/artifacts/dilla-testkit
      - run: npx playwright test --config e2e/playwright.media.config.ts --project=chromium-media unsupported-sfu-codec.spec.ts
        env:
          DILLA_MEDIA_SFU_AV1: '1'
          DILLA_TESTKIT: \${{ github.workspace }}/artifacts/dilla-testkit
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        if: failure()
        with:
          name: browser-media-results
          path: e2e/test-results
          if-no-files-found: error

  web-build:
    runs-on: ubuntu-latest
    timeout-minutes: 15
    steps:
      - run: bash scripts/install-ci-tool.sh wasm-bindgen
      - run: bash scripts/install-ci-tool.sh wasm-pack
      - run: wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/core-wasm/pkg
      - run: node scripts/check-wasm-size.mjs packages/core-wasm/pkg/dilla_core_wasm_bg.wasm
      - run: npm run typecheck -w @dilla/client-core
      - run: npm run typecheck -w @dilla/web
      - run: npm run typecheck:e2e
      - run: npm run build -w @dilla/web
      - run: npm run -w @dilla/web test:dist
      - run: npm run build:harness -w @dilla/client-core
      - run: go test -tags webdist ./internal/web/...
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with:
          name: dilla-web-dist
          path: packages/web/dist/
          if-no-files-found: error
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with:
          name: dilla-core-harness
          path: packages/client-core/harness-dist/
          if-no-files-found: error

${browserJobsFixture()}
  go-native:
    runs-on: ubuntu-latest
    timeout-minutes: 15
    needs: [rust-wasi, rust-native]
    steps:
      - run: cargo test -p dilla-core --test native_bot_group_peer_fixture --no-run --locked
      - run: go test -race ./internal/bot/native ./cmd/dillabot
        env:
          DILLA_BOT_HELPER_INTEGRATION: '1'
          DILLA_BOT_HELPER_BIN: \${{ github.workspace }}/target/release/dilla-bot-core

  go-ds:
    runs-on: ubuntu-latest
    timeout-minutes: 55
    needs: [rust-wasi]
    steps:
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-core-wasi
          path: internal/mlswasi/testdata
      - run: go test -race -shuffle=on -timeout 45m ./internal/ds/...

  go-api:
    runs-on: ubuntu-latest
    timeout-minutes: 55
    needs: [rust-wasi]
    steps:
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-core-wasi
          path: internal/mlswasi/testdata
      - run: go test -race -shuffle=on -timeout 50m ./internal/api/...

  go-harness:
    runs-on: ubuntu-latest
    timeout-minutes: 45
    needs: [rust-wasi, rust-native]
    steps:
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-core-wasi
          path: internal/mlswasi/testdata
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-testkit
          path: artifacts
      - run: go test -race -shuffle=on -timeout 38m ./internal/testkit/...
        env:
          DILLA_TESTKIT: \${{ github.workspace }}/artifacts/dilla-testkit
          DILLA_TESTKIT_REQUIRED: '1'

  go-fts5-arm64:
    runs-on: ubuntu-24.04-arm
    steps:
      - run: go test ./internal/store/sqlite/...

  go-lint:
    runs-on: ubuntu-latest
    steps:
      - run: bash scripts/install-ci-tool.sh golangci-lint
      - run: sudo apt-get update && sudo apt-get install -y libgtk-3-dev
      - run: golangci-lint run ./...

  go-vuln:
    runs-on: ubuntu-latest
    steps:
      - run: sudo apt-get update && sudo apt-get install -y libgtk-3-dev
      - run: go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
      - run: go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 -scan package -tags dillapins ./internal/deps

  go-sqlc:
    runs-on: ubuntu-latest
    steps:
      - run: bash scripts/install-ci-tool.sh sqlc
      - run: sqlc diff
      - run: git diff --exit-code

  go-postgres:
    runs-on: ubuntu-latest
    needs: [rust-wasi]
    services:
      postgres:
        image: postgres:18.6-alpine3.24
        env:
          POSTGRES_INITDB_ARGS: --locale-provider=builtin --builtin-locale=C.UTF-8
    steps:
      - run: go test -race -shuffle=on -timeout 20m ./internal/store/... ./internal/ops/...
        env:
          DILLA_TEST_PG: postgres://dilla:dilla@127.0.0.1:5432/dilla?sslmode=disable
      - run: go test -race -shuffle=on -timeout 20m -run 'AFK|Afk|HiddenCategory|HiddenParent|ParentAnywhere|Marker|ConcurrentEnsures|ReadsThePointer' ./internal/api/...
        env:
          DILLA_TEST_PG: postgres://dilla:dilla@127.0.0.1:5432/dilla?sslmode=disable

  go-release:
    runs-on: ubuntu-latest
    needs: [web-build]
    steps:
      - run: rm -rf internal/web/dist
      - name: Fetch the web client built by the web-build job
        uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-web-dist
          path: internal/web/dist
      - run: |
          test -f internal/web/dist/index.html
          if grep -q 'content="placeholder"' internal/web/dist/index.html; then echo "internal/web/dist still holds the placeholder client"; exit 1; fi
      - name: the downloaded client passes the manifest check
        run: go test -tags webdist ./internal/web/...
        env:
          DILLA_WEB_DIST: \${{ github.workspace }}/internal/web/dist
      - run: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dist/dillad-linux-amd64 ./cmd/dillad
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with:
          name: dillad-binaries
          path: dist/
          if-no-files-found: error

`;

// The four jobs that build for wasm32-unknown-unknown, as ci.yml writes them: the canonical clang 23 job env and,
// right after checkout, the canonical pinned install steps (ruling of 2026-10-13; hardened after the d3429ce lead).
const WASM_JOBS = ['rust-wasm-node', 'browser-spike', 'browser-media', 'web-build'];

function withWasmClang(text) {
  const out = [];
  let job = null;
  for (const line of text.split('\n')) {
    const key = /^ {2}([A-Za-z0-9_-]+):\s*$/.exec(line);
    if (key) job = key[1];
    if (WASM_JOBS.includes(job) && line === '    steps:') {
      out.push(WASM_CLANG_ENV_BLOCK, line, '      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7', WASM_CLANG_BLOCK);
      continue;
    }
    out.push(line);
  }
  return out.join('\n');
}

// The shape ci.yml gives the change filter: a `changes` job, a `docs` job that always runs, and every other job gated
// on `changes` as JOB_GATES says (its own needs merged in).
const CHANGES_JOB = `  changes:
    runs-on: ubuntu-latest
    outputs:
      go: \${{ steps.areas.outputs.go }}
      core: \${{ steps.areas.outputs.core }}
      web: \${{ steps.areas.outputs.web }}
      e2e: \${{ steps.areas.outputs.e2e }}
      text: \${{ steps.areas.outputs.text }}
      code: \${{ steps.areas.outputs.code }}
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7
        with:
          fetch-depth: 0
      - id: areas
        run: node scripts/ci-areas.mjs
        env:
          EVENT_NAME: \${{ github.event_name }}
          BASE_SHA: \${{ github.event.pull_request.base.sha }}
  docs:
    runs-on: ubuntu-latest
    steps:
      - run: npm run test:docs-check
      - run: npm run test:brief-check
      - run: npm run test:copy-check
      - run: npm run check:docs
      - run: npm run check:brief
      - run: npm run check:copy
      - run: npm run check:ci
`;

const CI_OK_JOB = (jobs) => `  ci-ok:
    needs: [${jobs.join(', ')}]
    if: always()
    runs-on: ubuntu-latest
    steps:
      - if: contains(needs.*.result, 'failure') || contains(needs.*.result, 'cancelled')
        run: exit 1
      - if: needs.ci-watchdog.result == 'skipped' && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository)
        run: exit 1
      - run: echo ok
`;

function withChanges(text) {
  const watcher = /^  ci-watchdog:\n[\s\S]*?(?=^  changes:)/m.exec(REAL_CI)[0];
  const lines = text.replace('jobs:\n', `jobs:\n${watcher}${CHANGES_JOB}`).split('\n');
  const out = [];
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const key = /^ {2}([A-Za-z0-9_-]+):\s*$/.exec(line);
    out.push(line);
    if (key === null || !(key[1] in JOB_GATES) || !lines.slice(0, i).includes('jobs:')) continue;
    let needs = [];
    for (let j = i + 1; j < lines.length && !/^ {2}\S/.test(lines[j]); j++) {
      const m = /^ {4}needs: \[(.*)\]$/.exec(lines[j]);
      if (m) { needs = m[1].split(',').map((s) => s.trim()); lines.splice(j, 1); break; }
    }
    out.push(`    needs: [${['changes', ...needs].join(', ')}]`, `    if: ${gateExpression(key[1])}`);
  }
  const jobs = out.filter((line) => /^ {2}[A-Za-z0-9_-]+:\s*$/.test(line)).map((line) => line.trim().slice(0, -1)).filter((name, i, all) => all.indexOf(name) === i && out.indexOf(`  ${name}:`) > out.indexOf('jobs:'));
  const barrier = `  ci-work-complete:
    needs: [${jobs.filter(name => name !== 'ci-watchdog').join(', ')}]
    if: always()
    runs-on: ubuntu-latest
    timeout-minutes: 2
    steps:
      - run: echo "All CI work reached a terminal result"
`;
  return `${out.join('\n').replace(/\n$/, '')}\n${barrier}${CI_OK_JOB([...jobs, 'ci-work-complete'])}`;
}

const LINUX_FOUNDATION_JOB = /^  linux-sandbox-foundation:\n[\s\S]*?(?=^  desktop:)/m.exec(REAL_CI)[0].replace(/^    needs:.*\n/m, '').replace(/^    if:.*\n/m, '');
const GOOD = withChanges(withWasmClang(GOOD_BASE + LINUX_FOUNDATION_JOB + '  desktop:\n    needs: [web-build, linux-sandbox-foundation]\n    uses: ./.github/workflows/desktop.yml\n'));

test('desktop units retain all native platforms and cannot be conditionally narrowed', () => {
  const step = '      - run: npm test -w @dilla/desktop\n';
  assert.ok(REAL_DESKTOP.includes(step));
  for (const replacement of ['', "      - if: runner.os == 'Linux'\n        run: npm test -w @dilla/desktop\n", "      - if: runner.os != 'Windows'\n        run: npm test -w @dilla/desktop\n"]) {
    const root = mkdtempSync(join(tmpdir(), 'dilla-unit-platform-ci-'));
    mkdirSync(join(root, '.github/workflows'), { recursive: true });
    addE2ePackage(root);
    writeFileSync(join(root, '.github/workflows/desktop.yml'), REAL_DESKTOP.replace(step, replacement));
    assert.ok(checkDesktopWorkflow(root).some(problem => problem.includes('desktop unit')));
  }
});

const BUDGET = MIN_BROWSER_WEB_TIMEOUT;

// The race-gated Go tests of the repository (ruling I8), as GOOD's non-race step names them.
const GATED = [
  ['cmd/dilla-loadrig', 'TestALoopbackCellDecryptsEverything'],
  ['cmd/dilla-mediabot', 'TestTwoBotsDecryptEachOtherThroughTheSFU'],
  ['internal/media', 'TestGoPublisherToGoSubscriberDecryptsThroughTheSFU'],
  ['internal/sfu', 'TestTheSFUOfferCarriesOnlyTheDillaCodecs'],
  ['internal/sfu', 'TestPromotionAddsTheVideoSourcesAndDemotionRemovesThem'],
];

function gatedTest(name) {
  return `package x\n\nimport "testing"\n\nfunc ${name}(t *testing.T) {\n\tif raceEnabled {\n\t\tt.Skip("upstream livekit-server data race in updateRidsFromSDP; runs in the non-race step")\n\t}\n}\n`;
}

// A repository root holding ci.yml and, as Go test files, the race-gated tests in `gated`.
function fixture(body, gated = GATED) {
  const dir = mkdtempSync(join(tmpdir(), 'dilla-ci-'));
  mkdirSync(join(dir, '.github', 'workflows'), { recursive: true });
  addE2ePackage(dir);
  writeFileSync(join(dir, '.github', 'workflows', 'ci.yml'), body);
  // The CLI also checks release.yml.
  writeFileSync(join(dir, '.github', 'workflows', 'release.yml'), REAL_RELEASE);
  writeFileSync(join(dir, '.github', 'workflows', 'desktop.yml'), REAL_DESKTOP);
  // ... and the desktop release trampoline with the key scripts it pins.
  for (const file of ['.github/workflows/desktop-release.yml', '.github/workflows/desktop-release-request.yml', 'packages/desktop/scripts/release-keys.mjs', 'packages/desktop/scripts/release-sign-job.mjs']) {
    mkdirSync(dirname(join(dir, file)), { recursive: true });
    writeFileSync(join(dir, file), readFileSync(join(REPO_ROOT, file), 'utf8'));
  }
  for (const [pkg, name] of gated) {
    mkdirSync(join(dir, pkg), { recursive: true });
    writeFileSync(join(dir, pkg, `${name}_test.go`), gatedTest(name));
  }
  return dir;
}

test('the real workflow passes every rule', () => {
  const root = join(dirname(fileURLToPath(import.meta.url)), '..');
  assert.deepEqual(checkWorkflow(root), []);
});

test('a well-formed workflow passes', () => {
  assert.deepEqual(checkWorkflow(fixture(GOOD)), []);
});

test('a missing job is reported by name', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/  deny:[\s\S]*$/, '')));
  assert.ok(problems.some((p) => p.includes('deny')), problems.join('\n'));
});

test('|| true is reported with its line number', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('run: npm test\n', 'run: npm test || true\n')));
  assert.ok(problems.some((p) => /\|\| true/.test(p)), problems.join('\n'));
});

test('continue-on-error is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('  deny:\n', '  deny:\n    continue-on-error: true\n')));
  assert.ok(problems.some((p) => p.includes('continue-on-error')), problems.join('\n'));
});

test('if: always() is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(NPM_TEST, '      - if: always()\n        run: npm test\n')));
  assert.ok(problems.some((p) => p.includes('always()')), problems.join('\n'));
});

// Fix round 1, finding 1 (important): the explicit-expression spelling `if: ${{ always() }}` is just
// as valid as `if: always()` and just as capable of making a job non-blocking, so the R22 gate must
// catch it too.
test('if: ${{ always() }} is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace(NPM_TEST, '      - if: ${{ always() }}\n        run: npm test\n')),
  );
  assert.ok(problems.some((p) => p.includes('always()')), problems.join('\n'));
});

test('a job that lost a required step is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace("      - if: needs.changes.outputs.core != 'false'\n        run: cargo fmt --all --check\n", '')));
  assert.ok(problems.some((p) => p.includes('cargo fmt')), problems.join('\n'));
});

test('a missing concurrency block is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/concurrency:[\s\S]*?cancel-in-progress: true\n/, '')));
  assert.ok(problems.some((p) => p.includes('concurrency')), problems.join('\n'));
});

test('commit-specific concurrency groups cannot leave obsolete pull-request runs alive', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('group: ${{ github.workflow }}-${{ github.ref }}', 'group: ${{ github.workflow }}-${{ github.ref }}-${{ github.sha }}')));
  assert.ok(problems.some((p) => p.includes('concurrency group')), problems.join('\n'));
});

test('a workflow token without read-only permissions is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('permissions:\n  contents: read\n', '')));
  assert.ok(problems.some((p) => p.includes('permissions: contents: read')), problems.join('\n'));
});

test('a workflow-level RUSTFLAGS is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('  CARGO_TERM_COLOR: always\n', '  CARGO_TERM_COLOR: always\n  RUSTFLAGS: -D warnings\n')));
  assert.ok(problems.some((p) => p.includes('RUSTFLAGS')), problems.join('\n'));
});

test('a rust-wasi job that lost if-no-files-found is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('          if-no-files-found: error\n', '')));
  assert.ok(problems.some((p) => p.includes('if-no-files-found')), problems.join('\n'));
});

test('a rust-wasi job that lost the artifact path is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace('          path: target/wasm32-wasip1/release/dilla_core_wasi.wasm\n', '')),
  );
  assert.ok(problems.some((p) => p.includes('dilla_core_wasi.wasm')), problems.join('\n'));
});

test('a go job without needs: rust-wasi is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('    needs: [changes, rust-wasi]\n', '    needs: [changes, deny]\n')));
  assert.ok(problems.some((p) => p.includes('"go"') && p.includes('needs: rust-wasi')), problems.join('\n'));
});

test('a go-harness job without needs: rust-wasi is reported', () => {
  const problems = checkWorkflow(fixture(editJob('go-harness', b => b.replace('    needs: [changes, rust-wasi, rust-native]\n', '    needs: [changes, rust-native]\n'))));
  assert.ok(problems.some((p) => p.includes('"go-harness"') && p.includes('needs: rust-wasi')), problems.join('\n'));
});

// The edges are order-independent to GitHub, so the ordering rule must accept either order.
test('a go-harness job whose needs: lists the two edges in the other order passes', () => {
  assert.deepEqual(
    checkWorkflow(fixture(editJob('go-harness', b => b.replace('needs: [changes, rust-wasi, rust-native]', 'needs: [changes, rust-native, rust-wasi]')))),
    [],
  );
});

// The go job has no testkit binary: requiring one there would fail every run.
test('a go job that requires the testkit it does not download is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace(`      - run: ${GO_TEST}\n`,
      `      - run: ${GO_TEST}\n        env:\n          DILLA_TESTKIT_REQUIRED: '1'\n`)),
  );
  assert.ok(problems.some((p) => p.includes('go-harness\'s')), problems.join('\n'));
});

test('a workflow without the go-harness job is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/  go-harness:[\s\S]*?\n\n/, '')));
  assert.ok(problems.some((p) => p.includes('missing job "go-harness"')), problems.join('\n'));
});

test('a go-harness job that lost its own timeout is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/(  go-harness:\n(?:    (?:name|needs|if):.*\n)*    runs-on: ubuntu-latest\n)    timeout-minutes: 45\n/, '$1')));
  assert.ok(problems.some((p) => p.includes('timeout-minutes: 45')), problems.join('\n'));
});

test('a node job that stopped running the checker\'s own tests is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('      - run: npm run test:ci-check\n', '')));
  assert.ok(problems.some((p) => p.includes('test:ci-check')), problems.join('\n'));
});

// Fix round 1, finding 3 (important): gap-31 §4 item 2 / NV-14 resolved the pairing as
// `upload-artifact@v7` with `download-artifact@v8` — pairing v7 with v4 (facts/plan-B.md's now-stale
// deviation B15 text) is the untested combination B15 existed to prevent. The checker owns the
// cross-plan ordering rule already, so it must own this half of the hand-off too.
test('a go-harness job without needs: rust-native is reported', () => {
  const problems = checkWorkflow(fixture(editJob('go-harness', b => b.replace('needs: [changes, rust-wasi, rust-native]', 'needs: [changes, rust-wasi]'))));
  assert.ok(problems.some((p) => p.includes('needs: rust-native')), problems.join('\n'));
});

test('a rust-native job that stopped building the testkit is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace('      - run: cargo build -p dilla-testkit --release --locked\n', '')),
  );
  assert.ok(problems.some((p) => p.includes('cargo build -p dilla-testkit')), problems.join('\n'));
});

test('a rust-native job that lost the testkit artifact name is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('          name: dilla-testkit\n          path: target', '          path: target')));
  assert.ok(problems.some((p) => p.includes('name: dilla-testkit')), problems.join('\n'));
});

test('a rust-native test step without the SQLCipher memory-lock allowance is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('          sudo prlimit --pid "$$" --memlock=268435456:268435456\n', '')));
  assert.ok(problems.some((p) => p.includes('sudo prlimit')), problems.join('\n'));
});

test('a go-harness job that stopped receiving DILLA_TESTKIT is reported', () => {
  const problems = checkWorkflow(
    fixture(
      GOOD.replace(
        "        env:\n          DILLA_TESTKIT: \${{ github.workspace }}/artifacts/dilla-testkit\n          DILLA_TESTKIT_REQUIRED: '1'\n",
        '',
      ),
    ),
  );
  assert.ok(problems.some((p) => p.endsWith('is missing: DILLA_TESTKIT')), problems.join('\n'));
});

// `DILLA_TESTKIT` is a prefix of `DILLA_TESTKIT_REQUIRED`, so the first check alone would pass a
// go-harness job that dropped the second: a missing binary would then skip every scenario test, job
// still green.
test('a go-harness job that stopped requiring the testkit is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace("          DILLA_TESTKIT_REQUIRED: '1'\n", '')));
  assert.ok(problems.some((p) => p.endsWith('is missing: DILLA_TESTKIT_REQUIRED')), problems.join('\n'));
});

// The wildcard is what picks up every new internal/... package without a workflow edit.
test('a go job whose test step is narrowed to a package list is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace(GO_TEST, 'go test -race -shuffle=on -timeout 15m ./internal/store/...')),
  );
  assert.ok(problems.some((p) => p.includes('must be exactly')), problems.join('\n'));
});

// CI budget (fix wave): the go job leaves out internal/ds and nothing else, and go-ds runs it.
test('a go job that leaves out more than internal/ds and internal/api is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace('-e github.com/dilla-chat/dilla/internal/api)', '-e github.com/dilla-chat/dilla/internal/api -e github.com/dilla-chat/dilla/internal/store)')),
  );
  assert.ok(problems.some((p) => p.includes('must be exactly')), problems.join('\n'));
});

// web3.9: the css checker's own test and the check itself, and the AFK / channel-parent tests on Postgres.
test('a node job without the css checker steps is reported', () => {
  for (const step of ['npm run test:css-check', 'npm run check:css']) {
    const problems = checkWorkflow(fixture(GOOD.replace(`      - run: ${step}\n`, '')));
    assert.ok(problems.some((p) => p.includes('"node"') && p.includes(step)), problems.join('\n'));
  }
});

test('a go-postgres job that stopped running the api AFK tests is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/      - run: go test -race -shuffle=on -timeout 20m -run 'AFK[^\n]*\n        env:\n[^\n]*\n/, '')));
  assert.ok(problems.some((p) => p.includes('"go-postgres"') && p.includes('./internal/api/...')), problems.join('\n'));
});

function withAPITests(body) {
  const dir = fixture(GOOD);
  mkdirSync(join(dir, 'internal', 'api'), { recursive: true });
  writeFileSync(join(dir, 'internal', 'api', 'afk_x_test.go'), body);
  return dir;
}

test('an AFK test the go-postgres -run regex misses is reported', () => {
  const problems = checkWorkflow(withAPITests('package api\n\nfunc TestAFKFine(t *testing.T) {}\nfunc TestSomethingUnmatched(t *testing.T) {}\n'));
  assert.ok(problems.some((p) => p.includes('TestSomethingUnmatched') && p.includes('afk_x_test.go')), problems.join('\n'));
  assert.ok(!problems.some((p) => p.includes('TestAFKFine')), problems.join('\n'));
});

test('AFK tests the go-postgres -run regex matches pass', () => {
  const dir = withAPITests('package api\n\nfunc TestAFKFine(t *testing.T) {}\nfunc TestTheBuiltInCarriesTheMarker(t *testing.T) {}\n');
  assert.deepEqual(checkWorkflow(dir), []);
});

test('a workflow without the go-ds job is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/  go-ds:[\s\S]*?\n\n/, '')));
  assert.ok(problems.some((p) => p.includes('missing job "go-ds"')), problems.join('\n'));
});

test('a go-ds job that stopped running internal/ds is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace('      - run: go test -race -shuffle=on -timeout 45m ./internal/ds/...\n', '')),
  );
  assert.ok(problems.some((p) => p.includes('"go-ds"') && p.includes('./internal/ds/...')), problems.join('\n'));
});

test('a go-ds job with a shorter test budget is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace('      - run: go test -race -shuffle=on -timeout 45m ./internal/ds/...', '      - run: go test -race -shuffle=on -timeout 20m ./internal/ds/...')),
  );
  assert.ok(problems.some((p) => p.includes('"go-ds"') && p.includes('-timeout 45m')), problems.join('\n'));
});

test('a go-ds job with too little wall-clock margin is reported', () => {
  const start = GOOD.indexOf('  go-ds:\n');
  const body = GOOD.slice(start, GOOD.indexOf('\n\n', start));
  const problems = checkWorkflow(fixture(GOOD.replace(body, () => body.replace('timeout-minutes: 55', 'timeout-minutes: 25'))));
  assert.ok(problems.some((p) => p.includes('"go-ds"') && p.includes('timeout-minutes: 55')), problems.join('\n'));
});

test('a workflow without the go-api job is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/  go-api:[\s\S]*?\n\n/, '')));
  assert.ok(problems.some((p) => p.includes('missing job "go-api"')), problems.join('\n'));
});

test('a go-api job that stopped running internal/api is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace('      - run: go test -race -shuffle=on -timeout 50m ./internal/api/...\n', '')),
  );
  assert.ok(problems.some((p) => p.includes('"go-api"') && p.includes('./internal/api/...')), problems.join('\n'));
});

test('a go-api job with a shorter budget is reported', () => {
  const start = GOOD.indexOf('  go-api:\n');
  const body = GOOD.slice(start, GOOD.indexOf('\n\n', start));
  const problems = checkWorkflow(fixture(GOOD.replace(body, () => body.replace('timeout-minutes: 55', 'timeout-minutes: 30'))));
  assert.ok(problems.some((p) => p.includes('"go-api"') && p.includes('timeout-minutes: 55')), problems.join('\n'));
});

test('a go job that stopped leaving out internal/api is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(' -e github.com/dilla-chat/dilla/internal/api', '')));
  assert.ok(problems.some((p) => p.includes('must be exactly')), problems.join('\n'));
});

test('a go-api job without needs: rust-wasi is reported', () => {
  const start = GOOD.indexOf('  go-api:\n');
  const body = GOOD.slice(start, GOOD.indexOf('\n\n', start));
  const problems = checkWorkflow(fixture(GOOD.replace(body, () => body.replace('    needs: [changes, rust-wasi]\n', ''))));
  assert.ok(problems.some((p) => p.includes('"go-api"') && p.includes('rust-wasi')), problems.join('\n'));
});

test('a go-ds job without needs: rust-wasi is reported', () => {
  const start = GOOD.indexOf('  go-ds:\n');
  const body = GOOD.slice(start, GOOD.indexOf('\n\n', start));
  const problems = checkWorkflow(fixture(GOOD.replace(body, () => body.replace('    needs: [changes, rust-wasi]\n', ''))));
  assert.ok(problems.some((p) => p.includes('"go-ds"') && p.includes('rust-wasi')), problems.join('\n'));
});

for (const [job, needle] of [
  ['rust-wasm-node', 'wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/media/wasm'],
  ['rust-wasm-node', 'wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/core-wasm/pkg'],
  ['rust-wasm-node', 'node scripts/check-wasm-size.mjs packages/core-wasm/pkg/dilla_core_wasm_bg.wasm'],
  ['rust-wasm-node', 'npm run typecheck -w @dilla/media'],
  ['rust-wasm-node', 'npm run test:wasm -w @dilla/media'],
  ['rust-wasm-node', 'npm run check:schema -w @dilla/media'],
  ['go-harness', 'go test -race -shuffle=on -timeout 38m ./internal/testkit/...'],
  ['go-harness', 'name: dilla-testkit'],
  ['go-lint', 'golangci-lint run ./...'],
  ['go-vuln', '-scan package -tags dillapins ./internal/deps'],
  ['go-sqlc', 'sqlc diff'],
  ['go-postgres', 'postgres:18.6-alpine3.24'],
  ['go-postgres', '--locale-provider=builtin --builtin-locale=C.UTF-8'],
  ['go-postgres', 'DILLA_TEST_PG'],
  // I7 (fix wave): internal/ops holds the only Postgres backup, restore and heal tests; a step that
  // drops it skips them in every job. The -timeout is explicit so a slow package fails by name.
  ['go-postgres', 'go test -race -shuffle=on -timeout 20m ./internal/store/... ./internal/ops/...'],
  ['go-release', 'if-no-files-found: error'],
]) {
  test(`a ${job} job that lost "${needle}" is reported`, () => {
    const start = GOOD.indexOf(`  ${job}:\n`);
    const end = GOOD.indexOf('\n\n', start);
    const body = GOOD.slice(start, end === -1 ? undefined : end);
    assert.ok(body.includes(needle), 'fixture sanity: ' + needle);
    const gutted = body.split('\n').filter((l) => !l.includes(needle)).join('\n');
    const problems = checkWorkflow(fixture(GOOD.replace(body, () => gutted)));
    assert.ok(problems.some((p) => p.includes(`"${job}"`) && p.includes(needle)), problems.join('\n'));
  });
}

test('a go job with download-artifact@v4 instead of v8 is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replaceAll('actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8', 'actions/download-artifact@v4')),
  );
  assert.ok(problems.some((p) => p.includes('download-artifact@v4')), problems.join('\n'));
});

// Ruling L: the Go half of the tree is gated by this checker too, so a workflow that simply never
// declares the `go` job — the state this repository was in before Plan B task 8 — must be reported
// rather than silently accepted. Before this rule the checker passed such a workflow.
test('a workflow with no go job at all is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/  go:[\s\S]*$/, '')));
  assert.ok(problems.some((p) => p.includes('missing job "go"')), problems.join('\n'));
  assert.ok(problems.some((p) => p.includes('missing job "go-fts5-arm64"')), problems.join('\n'));
  for (const job of ['go-lint', 'go-vuln', 'go-sqlc', 'go-postgres', 'go-release']) {
    assert.ok(problems.some((p) => p.includes(`missing job "${job}"`)), problems.join('\n'));
  }
});

// Ruling M: `internal/deps` is the only thing that compiles the pinned LiveKit/wazero/sqlite graph
// with the three pion `replace` directives, and it is behind the `dillapins` tag, so no other step
// in the workflow would notice the graph breaking.
test('a go job that lost the pinned-module-graph build is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace('      - run: CGO_ENABLED=0 go build -tags dillapins ./internal/deps\n', '')),
  );
  assert.ok(problems.some((p) => p.includes('dillapins')), problems.join('\n'));
});

test('a go job that stopped running the race-detector tests is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace(`      - run: ${GO_TEST}\n`, '')),
  );
  assert.ok(problems.some((p) => p.includes('-race')), problems.join('\n'));
});

test('a go-fts5-arm64 job that stopped running on arm is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('    runs-on: ubuntu-24.04-arm\n', '    runs-on: ubuntu-latest\n')));
  assert.ok(problems.some((p) => p.includes('ubuntu-24.04-arm')), problems.join('\n'));
});

// The CLI entry point at the bottom of check-ci-workflow.mjs, not `checkWorkflow` itself: this must
// be run as a subprocess from a cwd that is NOT the fixture root and has no .github/workflows of its
// own, so a script that silently fell back to process.cwd() (ignoring argv[2]) cannot pass by
// accident — it would fail to find ci.yml at all rather than happening to validate the right file.
function runCli(root, cwd) {
  return spawnSync(process.execPath, [SCRIPT_PATH, root], { cwd, encoding: 'utf8' });
}

test('the CLI honours a root argument passed from a different cwd', () => {
  // The real ci.yml: the CLI also checks release.yml, whose actions must be ci.yml's at the same versions.
  const root = fixture(REAL_CI);
  const cwd = mkdtempSync(join(tmpdir(), 'dilla-ci-cwd-'));
  const result = runCli(root, cwd);
  assert.equal(result.status, 0, `stdout: ${result.stdout}\nstderr: ${result.stderr}`);
});

test('the CLI reports problems in a workflow at the given root, run from elsewhere', () => {
  const root = fixture(GOOD.replace(/  deny:[\s\S]*$/, ''));
  const cwd = mkdtempSync(join(tmpdir(), 'dilla-ci-cwd-'));
  const result = runCli(root, cwd);
  assert.equal(result.status, 1, `stdout: ${result.stdout}\nstderr: ${result.stderr}`);
  assert.match(result.stderr, /deny/);
});

// Jonas, 2026-10-21: the container image is dropped.
test('a workflow that builds or pushes a container image is reported', () => {
  for (const addition of [
    '      - run: docker login ghcr.io -u me\n',
    '      - uses: docker/setup-buildx-action@v4\n',
    '      - run: bash scripts/build-ci-image.sh\n',
  ]) {
    const problems = checkWorkflow(fixture(GOOD.replace('  ci-ok:', () => `  image:\n    runs-on: ubuntu-latest\n    steps:\n${addition}  ci-ok:`)));
    assert.ok(problems.some((p) => p.includes('container image was dropped')), `${addition}: ${problems.join('\n')}`);
  }
});

test('the real workflow has no image job', () => {
  assert.doesNotMatch(REAL_CI, /^ {2}image:/m);
});

// The rule that covers a job added tomorrow: no job is named in REQUIRED_STEPS, yet its upload-artifact
// step is still held to the fail-closed setting.
test('an upload-artifact step in any job without if-no-files-found: error is reported', () => {
  const extra = '\n  extra:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7\n        with:\n          name: x\n          path: y\n      - run: echo after\n';
  const problems = checkWorkflow(fixture(GOOD + extra));
  assert.deepEqual(
    problems.map((p) => p.replace(/^ci\.yml:\d+: /, '')).filter((p) => !p.includes('JOB_GATES') && !p.includes('"ci-ok"') && !p.includes('completion barrier')),
    ['actions/upload-artifact without "if-no-files-found: error" — CI is fail-closed'],
  );
  // The same step with the setting is clean.
  assert.deepEqual(checkWorkflow(fixture(GOOD + extra.replace('          path: y\n', '          path: y\n          if-no-files-found: error\n'))).filter((p) => !p.includes('JOB_GATES') && !p.includes('"ci-ok"') && !p.includes('completion barrier')), []);
});

// Task 21: the browser E2EE suite.
test('a workflow without the browser-media job is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/  browser-media:[\s\S]*?\n\n/, '')));
  assert.ok(problems.some((p) => p.includes('missing job "browser-media"')), problems.join('\n'));
});

test('browser-media must explicitly extract RNNoise wasm before Playwright', () => {
  const step = '      - run: node packages/media/scripts/extract-rnnoise-wasm.mjs\n';
  assert.deepEqual(checkWorkflow(fixture(GOOD)), []);
  const problems = checkWorkflow(fixture(GOOD.replace(step, '')));
  assert.ok(problems.some((p) => p.includes('extract-rnnoise-wasm.mjs')), problems.join('\n'));
});

test('browser-media extraction must precede the media suite', () => {
  const step = '      - run: node packages/media/scripts/extract-rnnoise-wasm.mjs\n';
  const moved = GOOD.replace(step, '').replace('      - run: npm run test:e2e:media -w @dilla/e2e\n', '      - run: npm run test:e2e:media -w @dilla/e2e\n' + step);
  const problems = checkWorkflow(fixture(moved));
  assert.ok(problems.some((p) => p.includes('RNNoise extraction') && p.includes('before')), problems.join('\n'));
});

test('browser-media must start an audio server before the media suite', () => {
  const step = '      - run: sh scripts/ci-audio-server.sh\n';
  const missing = checkWorkflow(fixture(GOOD.replace(step, '')));
  assert.ok(missing.some((p) => p.includes('ci-audio-server.sh')), missing.join('\n'));
  const moved = GOOD.replace(step, '').replace('      - run: npm run test:e2e:media -w @dilla/e2e\n', '      - run: npm run test:e2e:media -w @dilla/e2e\n' + step);
  const late = checkWorkflow(fixture(moved));
  assert.ok(late.some((p) => p.includes('audio server') && p.includes('before')), late.join('\n'));
});

test('the browser-media job requires the real-SFU AV1 leg', () => {
  const stripped = GOOD.replace(/      - run: npx playwright test --config e2e\/playwright\.media\.config\.ts --project=chromium-media unsupported-sfu-codec\.spec\.ts\n        env:\n          DILLA_MEDIA_SFU_AV1: '1'\n          DILLA_TESTKIT: \$\{\{ github\.workspace \}\}\/artifacts\/dilla-testkit\n/, '');
  const problems = checkWorkflow(fixture(stripped));
  assert.ok(problems.some((p) => p.includes('unsupported-sfu-codec.spec.ts')), problems.join('\n'));
});

for (const [need, kept] of [['rust-wasi', 'rust-native'], ['rust-native', 'rust-wasi']]) {
  test(`a browser-media job without needs: ${need} is reported`, () => {
    const problems = checkWorkflow(fixture(GOOD.replace('    needs: [changes, rust-native, rust-wasi]\n', `    needs: [changes, ${kept}]\n`)));
    assert.ok(problems.some((p) => p.includes('"browser-media"') && p.includes(`needs: ${need}`)), problems.join('\n'));
  });
}

for (const needle of [
  'timeout-minutes: 25',
  'name: dilla-core-wasi',
  'path: internal/mlswasi/testdata',
  'name: dilla-testkit',
  'wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/media/wasm',
  'go build -o target/dilla-mediabot ./cmd/dilla-mediabot',
  'npx playwright install --with-deps chromium',
  'npm run test:e2e:media -w @dilla/e2e',
  'DILLA_TESTKIT: ${{ github.workspace }}/artifacts/dilla-testkit',
]) {
  test(`a browser-media job that lost "${needle}" is reported`, () => {
    const start = GOOD.indexOf('  browser-media:\n');
    const body = GOOD.slice(start, GOOD.indexOf('\n\n', start));
    assert.ok(body.includes(needle), 'fixture sanity: ' + needle);
    const gutted = body.split('\n').filter((l) => !l.includes(needle)).join('\n');
    const problems = checkWorkflow(fixture(GOOD.replace(body, () => gutted)));
    assert.ok(problems.some((p) => p.includes('"browser-media"') && p.includes(needle)), problems.join('\n'));
  });
}

test('a browser-spike job without its timeout is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('    timeout-minutes: 20\n', '')));
  assert.ok(problems.some((p) => p.includes('"browser-spike"') && p.includes('timeout-minutes: 20')), problems.join('\n'));
});

test('the video publishing tests retain a non-race CI step', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(NON_RACE_STEP, 'go test ./cmd/dilla-mediabot')));
  assert.ok(problems.some((p) => p.includes('job "go"') && p.includes(NON_RACE_STEP)), problems.join('\n'));
  assert.ok(problems.some((p) => p.includes('no non-race') && p.includes('5 race-gated')), problems.join('\n'));
});

// Ruling I8, the CI run on main after PR #7: TestALoopbackCellDecryptsEverything published VP8 under
// -race. The repository's gated tests are exactly the ones the non-race step names.
test('the repository gates exactly the tests the non-race step names', () => {
  const root = join(dirname(fileURLToPath(import.meta.url)), '..');
  const found = raceGatedTests(root).map((g) => [g.pkg.slice(2), g.name]);
  assert.deepEqual(found.toSorted(), GATED.toSorted());
});

test('a race-gated test the non-race step does not name is reported', () => {
  const extra = [...GATED, ['internal/api', 'TestANewVideoPublisher']];
  const problems = checkWorkflow(fixture(GOOD, extra));
  assert.ok(problems.some((p) => p.includes('does not name race-gated TestANewVideoPublisher')), problems.join('\n'));
  assert.ok(problems.some((p) => p.includes('does not run ./internal/api')), problems.join('\n'));
});

test('a non-race step that lost a gated package is reported', () => {
  const step = NON_RACE_STEP.replace(' ./cmd/dilla-loadrig', '');
  const problems = checkWorkflow(fixture(GOOD.replace(NON_RACE_STEP, step)));
  assert.ok(
    problems.some((p) => p.includes('does not run ./cmd/dilla-loadrig') && p.includes('TestALoopbackCellDecryptsEverything')),
    problems.join('\n'),
  );
});

test('a non-race step that lost a gated test name is reported', () => {
  const step = NON_RACE_STEP.replace('TestALoopbackCellDecryptsEverything|', '');
  const problems = checkWorkflow(fixture(GOOD.replace(NON_RACE_STEP, step)));
  assert.ok(problems.some((p) => p.includes('does not name race-gated TestALoopbackCellDecryptsEverything')), problems.join('\n'));
});

// A renamed or deleted test leaves an alternative that matches nothing, and go test passes that.
test('a non-race step naming a test that is not gated is reported', () => {
  const problems = checkWorkflow(fixture(GOOD, GATED.slice(1)));
  assert.ok(problems.some((p) => p.includes('names TestALoopbackCellDecryptsEverything, which is no race-gated test')), problems.join('\n'));
});

test('a non-race step that gained -race is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(NON_RACE_STEP, NON_RACE_STEP.replace('go test ', 'go test -race '))));
  assert.ok(problems.some((p) => p.includes('no non-race')), problems.join('\n'));
});

test('a raceEnabled gate outside a Test function is reported', () => {
  const root = fixture(GOOD);
  writeFileSync(join(root, 'internal', 'sfu', 'helper_test.go'), 'package x\n\nfunc helper() {\n\tif raceEnabled {\n\t\treturn\n\t}\n}\n');
  const problems = checkWorkflow(root);
  assert.ok(problems.some((p) => p.includes('internal/sfu/helper_test.go') && p.includes('outside a Test function')), problems.join('\n'));
});

test('race gates under node_modules and dot-directories are not the module\'s', () => {
  const root = fixture(GOOD);
  for (const dir of ['node_modules/x', '.claude/worktrees/other']) {
    mkdirSync(join(root, dir), { recursive: true });
    writeFileSync(join(root, dir, 'gate_test.go'), gatedTest('TestElsewhere'));
  }
  assert.deepEqual(checkWorkflow(root), []);
});

test('browser-media must upload failure traces and host logs', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('name: browser-media-results', 'name: removed-results')));
  assert.ok(problems.some((p) => p.includes('browser-media') && p.includes('browser-media-results')), problems.join('\n'));
});

test('a node job that stopped running the wasm size checker\'s own tests is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('      - run: npm run test:wasm-size-check\n', '')));
  assert.ok(problems.some((p) => p.includes('"node"') && p.includes('npm run test:wasm-size-check')), problems.join('\n'));
});

test('a browser-spike job that lost its size-profile wasm-pack line is reported', () => {
  const line = 'wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir spike/pkg';
  const problems = checkWorkflow(fixture(GOOD.replace(`      - run: ${line}\n`, '')));
  assert.ok(problems.some((p) => p.includes('"browser-spike"') && p.includes(line)), problems.join('\n'));
});

test('a wasm-pack build with --release instead of the size profile is reported with its line', () => {
  const body = GOOD.replace(
    'wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir spike/pkg',
    'wasm-pack build core/dilla-core-wasm --target web --release --mode no-install --out-dir spike/pkg',
  );
  assert.notEqual(body, GOOD, 'fixture sanity');
  const lineNo = body.split('\n').findIndex((l) => l.includes('--release --mode no-install --out-dir spike/pkg')) + 1;
  const problems = checkWorkflow(fixture(body));
  assert.ok(
    problems.includes(`ci.yml:${lineNo}: wasm-pack build must use --profile wasm-release, not --release (C16)`),
    problems.join('\n'),
  );
});

test('a wasm-pack build with no profile at all is reported', () => {
  const body = GOOD.replace(
    'wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/core-wasm/pkg',
    'wasm-pack build core/dilla-core-wasm --target web --mode no-install --out-dir ../../packages/core-wasm/pkg',
  );
  assert.notEqual(body, GOOD, 'fixture sanity');
  const problems = checkWorkflow(fixture(body));
  assert.ok(problems.some((p) => p.endsWith('wasm-pack build must use --profile wasm-release, not --release (C16)')), problems.join('\n'));
});

// Task 26 (web-1): the web client's build, its browser suites, and its hand-off to release.

/** One job of GOOD, from its key line to the line before the next job key. */
function jobBody(job) {
  const lines = GOOD.split('\n');
  const from = lines.indexOf(`  ${job}:`);
  assert.ok(from !== -1, `fixture sanity: job ${job}`);
  let to = from + 1;
  while (to < lines.length && !/^ {2}[A-Za-z0-9_-]+:\s*$/.test(lines[to])) to++;
  return lines.slice(from, to).join('\n');
}

for (const job of ['web-build', ...Object.keys(BROWSER_WEB_JOBS)]) {
  test(`a workflow without the ${job} job is reported`, () => {
    const problems = checkWorkflow(fixture(GOOD.replace(jobBody(job), '')));
    assert.ok(problems.some((p) => p.includes(`missing job "${job}"`)), problems.join('\n'));
  });
}

for (const [job, needle] of [
  ['web-build', 'timeout-minutes: 15'],
  ['web-build', 'wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/core-wasm/pkg'],
  ['web-build', 'node scripts/check-wasm-size.mjs packages/core-wasm/pkg/dilla_core_wasm_bg.wasm'],
  ['web-build', 'npm run typecheck -w @dilla/client-core'],
  ['web-build', 'npm run typecheck -w @dilla/web'],
  ['web-build', 'npm run typecheck:e2e'],
  ['web-build', 'npm run build -w @dilla/web'],
  ['web-build', 'npm run build:harness -w @dilla/client-core'],
  ['web-build', 'go test -tags webdist ./internal/web/...'],
  ['web-build', 'name: dilla-web-dist'],
  ['web-build', 'path: packages/web/dist/'],
  ['web-build', 'name: dilla-core-harness'],
  ['web-build', 'path: packages/client-core/harness-dist/'],
  ...Object.entries(BROWSER_WEB_JOBS).flatMap(([job, spec]) => [
    [job, `timeout-minutes: ${spec.timeout}`],
    ...(spec.engine === 'firefox' && spec.suite !== 'core' ? [[job, 'sh scripts/ci-audio-server.sh']] : []),
    [job, 'name: dilla-core-wasi'],
    [job, 'path: internal/mlswasi/testdata'],
    [job, 'name: dilla-testkit'],
    [job, 'name: dilla-web-dist'],
    [job, 'path: packages/web/dist'],
    [job, 'name: dilla-core-harness'],
    [job, 'path: packages/client-core/harness-dist'],
    [job, `npx playwright install --with-deps ${spec.engine}`],
    [job, browserSuiteCommand(spec)],
    [job, 'DILLA_TESTKIT: ${{ github.workspace }}/artifacts/dilla-testkit'],
    [job, 'if: failure()'],
    [job, `name: browser-web-results-${job}`],
    [job, 'path: e2e/test-results'],
  ]),
  ['go-release', 'name: dilla-web-dist'],
  ['go-release', 'path: internal/web/dist'],
  ['go-release', 'content="placeholder"'],
  ['go-release', 'go test -tags webdist ./internal/web/...'],
  ['node', 'npm run test:manifest-writer'],
  ['node', 'npm run test:install'],
  ['rust-wasm-node', 'npm run typecheck -w @dilla/core-wasm'],
  ['rust-wasm-node', 'cargo clippy -p dilla-core-wasm --target wasm32-unknown-unknown --lib --locked -- -D warnings'],
  ['rust-wasm-node', '- run: rustup show active-toolchain'],
]) {
  test(`a ${job} job that lost "${needle}" is reported (web-1)`, () => {
    const body = jobBody(job);
    assert.ok(body.includes(needle), 'fixture sanity: ' + needle);
    const gutted = body.split('\n').filter((l) => !l.includes(needle)).join('\n');
    const problems = checkWorkflow(fixture(GOOD.replace(body, () => gutted)));
    assert.ok(problems.some((p) => p.includes(`"${job}"`) && p.includes(needle)), problems.join('\n'));
  });
}

for (const job of Object.keys(BROWSER_WEB_JOBS)) {
  for (const need of ['rust-native', 'rust-wasi', 'web-build']) {
    test(`a ${job} job without needs: ${need} is reported`, () => {
      const body = jobBody(job);
      const kept = ['rust-native', 'rust-wasi', 'web-build'].filter((n) => n !== need).join(', ');
      const cut = body.replace('    needs: [changes, rust-native, rust-wasi, web-build]\n', `    needs: [changes, ${kept}]\n`);
      assert.notEqual(cut, body, 'fixture sanity: needs');
      const problems = checkWorkflow(fixture(GOOD.replace(body, () => cut)));
      assert.ok(problems.some((p) => p.includes(`"${job}"`) && p.includes(`needs: ${need}`)), problems.join('\n'));
    });
  }
}

test('shared core integration uses only the Electron-compatible engine', () => {
  assert.deepEqual(BROWSER_WEB_JOBS, { 'core-integration': { engine: 'chromium', suite: 'core', shard: null, timeout: 20 } });
  assert.equal(browserSuiteCommand(BROWSER_WEB_JOBS['core-integration']), 'npm run test:e2e:core:chromium -w @dilla/e2e');
  assert.equal(WEB_SHARDS, 3, 'desktop client acceptance still has three shards');
});

test('the core security job cannot be deleted, folded into another suite or given too little time', () => {
  const body = jobBody('core-integration');
  assert.ok(checkWorkflow(fixture(GOOD.replace(body, ''))).some(p => p.includes('missing job "core-integration"')));
  const both = body.replace('    steps:\n', '    steps:\n      - run: npm run test:e2e:voice:chromium -w @dilla/e2e\n');
  assert.ok(checkWorkflow(fixture(GOOD.replace(body, () => both))).some(p => p.includes('exactly one Playwright suite')));
  assert.ok(BROWSER_WEB_JOBS['core-integration'].timeout >= MIN_BROWSER_WEB_TIMEOUT);
});

test('unsupported browser client jobs are rejected when reintroduced', () => {
  const old = '  firefox-web-1:\n    needs: [changes]\n    if: needs.changes.outputs.e2e != \'false\'\n    steps:\n      - run: npm run test:e2e:web:firefox -w @dilla/e2e\n';
  const problems = checkWorkflow(fixture(GOOD.replace('  deny:', () => old + '  deny:')));
  assert.ok(problems.some(p => p.includes('firefox-web-1') && p.includes('JOB_GATES')));
});

test('every browser job has its own artifact name in the real ci.yml', () => {
  const names = Object.keys(BROWSER_WEB_JOBS).map((job) => `name: browser-web-results-${job}`);
  assert.equal(new Set(names).size, names.length);
  for (const name of names) assert.equal(REAL_CI.split('\n').filter((l) => l.trim() === name).length, 1, name);
});

test('the real ci-ok job needs every browser job', () => {
  const body = REAL_CI.slice(REAL_CI.indexOf('\n  ci-ok:'));
  for (const job of Object.keys(BROWSER_WEB_JOBS)) assert.ok(new RegExp(`needs:[^\\n]*[ \\[]${job}[,\\]]`).test(body), job);
});

test('a ci-ok job that forgot core security integration is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/(  ci-ok:\n    needs: \[[^\]]*), core-integration/, '$1')));
  assert.ok(problems.some((p) => p.includes('"ci-ok"') && p.includes('"core-integration"')), problems.join('\n'));
});

test('a browser job without the e2e gate is reported', () => {
  for (const job of Object.keys(BROWSER_WEB_JOBS)) {
    const body = jobBody(job);
    const ungated = body.replace("    if: needs.changes.outputs.e2e != 'false'\n", '');
    assert.notEqual(ungated, body, `fixture sanity: ${job} gate`);
    const problems = checkWorkflow(fixture(GOOD.replace(body, () => ungated)));
    assert.ok(problems.some((p) => p.includes(`"${job}"`) && p.includes("needs.changes.outputs.e2e != 'false'")), `${job}: ${problems.join('\n')}`);
  }
});

test('go-release must point the webdist test at the downloaded client', () => {
  const line = '          DILLA_WEB_DIST: ${{ github.workspace }}/internal/web/dist\n';
  const body = jobBody('go-release');
  assert.ok(body.includes(line), 'fixture sanity: the DILLA_WEB_DIST line');
  const problems = checkWorkflow(fixture(GOOD.replace(body, () => body.replace(line, ''))));
  assert.ok(
    problems.some((p) => p.includes('"go-release"') && p.includes('DILLA_WEB_DIST: ${{ github.workspace }}/internal/web/dist')),
    problems.join('\n'),
  );
});

test('a go-release job without needs: web-build is reported', () => {
  const body = jobBody('go-release');
  const problems = checkWorkflow(fixture(GOOD.replace(body, () => body.replace('    needs: [changes, web-build]\n', ''))));
  assert.ok(problems.some((p) => p.includes('"go-release"') && p.includes('needs: web-build')), problems.join('\n'));
});

test('a go-release job that builds before it fetches the web client is reported', () => {
  const build = '      - run: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dist/dillad-linux-amd64 ./cmd/dillad\n';
  const body = jobBody('go-release');
  assert.ok(body.includes(build), 'fixture sanity: the amd64 build');
  const moved = body.replace(build, '').replace('    steps:\n', `    steps:\n${build}`);
  const problems = checkWorkflow(fixture(GOOD.replace(body, () => moved)));
  assert.ok(problems.some((p) => p.includes('"go-release"') && p.includes('before it builds')), problems.join('\n'));
});

// web-2b task 11 (L-CI-20, L-CI-21): the screenshots of the web-2b stories and the re-derived browser budget.
const UI_SHOTS_2B = 'node packages/ui/scripts/shoot-stories.mjs 6016 target/ui-screens/web-2b form- ceremony- shell- conversation- settings- screens- primitives-channelrow-- conversation-messagetoolbar-- conversation-attachmentcard-- screens-conversation--';

for (const needle of [UI_SHOTS_2B, 'name: ui-screens', 'path: target/ui-screens/web-2b']) {
  test(`a ui job that lost "${needle}" is reported (web-2b)`, () => {
    const body = jobBody('ui');
    assert.ok(body.includes(needle), 'fixture sanity: ' + needle);
    const gutted = body.split('\n').filter((l) => !l.includes(needle)).join('\n');
    const problems = checkWorkflow(fixture(GOOD.replace(body, () => gutted)));
    assert.ok(problems.some((p) => p.includes('"ui"') && p.includes(needle)), problems.join('\n'));
  });
}

for (const prefix of ['form-', 'ceremony-', 'shell-', 'conversation-', 'settings-', 'screens-', 'primitives-channelrow--', 'conversation-messagetoolbar--', 'conversation-attachmentcard--', 'screens-conversation--']) {
  test(`a ui screenshot step that dropped the ${prefix} prefix is reported (web-2b)`, () => {
    const body = jobBody('ui');
    const narrowed = body.replace(` ${prefix}`, '');
    assert.notEqual(narrowed, body, `fixture sanity: the ${prefix} prefix`);
    const problems = checkWorkflow(fixture(GOOD.replace(body, () => narrowed)));
    assert.ok(problems.some((p) => p.includes('"ui"') && p.includes(UI_SHOTS_2B)), problems.join('\n'));
  });
}

test('a ui job still capturing into an earlier directory is reported (web-2b)', () => {
  for (const earlier of ['web-1', 'web-2a']) {
    const body = jobBody('ui');
    const old = body.replaceAll('target/ui-screens/web-2b', `target/ui-screens/${earlier}`);
    assert.notEqual(old, body, 'fixture sanity: the web-2b directory');
    const problems = checkWorkflow(fixture(GOOD.replace(body, () => old)));
    assert.ok(
      problems.some((p) => p.includes('"ui"') && p.includes('path: target/ui-screens/web-2b')),
      `${earlier}: ${problems.join('\n')}`,
    );
  }
});

test('neither the checker nor the fixture names an earlier screenshot directory (web-2b)', () => {
  const checker = readFileSync(SCRIPT_PATH, 'utf8');
  for (const earlier of ['target/ui-screens/web-1', 'target/ui-screens/web-2a']) {
    assert.ok(!checker.includes(earlier), `check-ci-workflow.mjs still names ${earlier}`);
    assert.ok(!GOOD.includes(earlier), `GOOD still names ${earlier}`);
  }
});

for (const [job, { timeout }] of Object.entries(BROWSER_WEB_JOBS)) {
  test(`a ${job} job off its derived budget is reported (web-2c)`, () => {
    const body = jobBody(job);
    const off = body.replace(`    timeout-minutes: ${timeout}\n`, `    timeout-minutes: ${timeout - 5}\n`);
    assert.notEqual(off, body, 'fixture sanity: the derived budget');
    const problems = checkWorkflow(fixture(GOOD.replace(body, () => off)));
    assert.ok(problems.some((p) => p.includes(`"${job}"`) && p.includes(`timeout-minutes: ${timeout}`)), problems.join('\n'));
  });
}

// web-2c task 11 (L-CI-22): the seven new story families at both viewports, both upload paths and the budget.
test('web-2c captures, both paths and timeout are required independently', () => {
  const desktop = 'node packages/ui/scripts/shoot-stories.mjs 6027 target/ui-screens/web-2c/desktop trust-safetycompare-- trust-trustalert-- search-searchpalette-- status-typingindicator-- settings-diagnosticspanel-- shell-tabhand-over-- screens-trustandpolish--';
  const phone = 'node packages/ui/scripts/shoot-stories.mjs 6027 target/ui-screens/web-2c/phone trust-safetycompare-- trust-trustalert-- search-searchpalette-- status-typingindicator-- settings-diagnosticspanel-- shell-tabhand-over-- screens-trustandpolish-- --viewport=360x740';
  const required = [
    ['desktop step', GOOD.replace(`      - run: ${desktop}\n`, '')],
    ['phone step', GOOD.replace(`      - run: ${phone}\n`, '')],
    ['desktop path', GOOD.replace('target/ui-screens/web-2c/desktop/**', 'target/ui-screens/web-2b/desktop/**')],
    ['phone path', GOOD.replace('target/ui-screens/web-2c/phone/**', 'target/ui-screens/web-2b/phone/**')],
    ['timeout', GOOD.replace(`timeout-minutes: ${BUDGET}`, `timeout-minutes: ${BUDGET - 1}`)],
  ];
  for (const [name, workflow] of required) {
    const problems = checkWorkflow(fixture(workflow));
    assert.ok(problems.length > 0, `${name}: checker accepted a missing gate`);
    assert.match(problems.join('\n'), /web-2c|screenshot|browser-web|timeout/i, name);
  }
});

// web-3 task 11: the voice, ring and media-player families at both viewports, uploaded from target/ui-screens/web-3.
test('web-3 captures and the web-3 upload path are required independently', () => {
  const desktop = 'node packages/ui/scripts/shoot-stories.mjs 6038 target/ui-screens/web-3/desktop voice- media-mediaplayer-- screens-voice--';
  const phone = 'node packages/ui/scripts/shoot-stories.mjs 6038 target/ui-screens/web-3/phone voice- media-mediaplayer-- screens-voice-- --viewport=360x740';
  assert.deepEqual(checkWorkflow(fixture(GOOD)), [], 'fixture sanity: GOOD passes');
  const required = [
    ['desktop step', GOOD.replace(`      - run: ${desktop}\n`, ''), desktop],
    ['phone step', GOOD.replace(`      - run: ${phone}\n`, ''), phone],
    ['narrowed prefixes', GOOD.replace(` voice- media-mediaplayer-- screens-voice--\n`, ' voice-\n'), desktop],
    ['upload path', GOOD.replace('path: target/ui-screens/web-3/**', 'path: target/ui-screens/web-2c/**'), 'path: target/ui-screens/web-3/**'],
    ['artifact name', GOOD.replace('name: ui-screens-web-3\n', 'name: ui-screens-web-2d\n'), 'name: ui-screens-web-3'],
  ];
  for (const [name, workflow, needle] of required) {
    assert.notEqual(workflow, GOOD, `fixture sanity: ${name}`);
    const problems = checkWorkflow(fixture(workflow));
    assert.ok(problems.some((p) => p.includes('"ui"') && p.includes(needle)), `${name}: ${problems.join('\n')}`);
  }
});

// Ruling of 2026-10-13 (web-2c D5 in CI), hardened after the d3429ce scanner lead: every wasm32-unknown-unknown
// build compiles SQLite with one exact clang 23 build, and the checker compares whole blocks, not substrings.
/** GOOD with one job's body passed through `edit`. */
function editJob(job, edit) {
  const body = jobBody(job);
  const edited = edit(body);
  assert.notEqual(edited, body, `fixture sanity: the edit of ${job} changed nothing`);
  return GOOD.replace(body, edited);
}

const STEP_RULE = (job) =>
  `ci.yml: job "${job}" builds for wasm32-unknown-unknown; its second to fourth steps, right after actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7, must be exactly the pinned clang 23 steps (WASM_CLANG_STEPS)`;
const ENV_RULE = (job) =>
  `ci.yml: job "${job}" builds for wasm32-unknown-unknown; its job-level env must be exactly:\n${WASM_CLANG_ENV_BLOCK}`;

const LLVM_PACKAGES = ['clang-23', 'llvm-23', 'libllvm23', 'libclang-cpp23', 'libclang1-23', 'libclang-common-23-dev', 'llvm-23-linker-tools', 'llvm-23-runtime'];

/** The install script the canonical steps run: the pin, the fingerprint check and the read-back live there now. */
const CLANG_SCRIPT = readFileSync(new URL('./ci-llvm.sh', import.meta.url), 'utf8');

test('the canonical steps restore the cached build, run the install script and save a build the cache lacked', () => {
  assert.equal(WASM_CLANG_STEPS.length, 3);
  assert.equal(WASM_CLANG_STEPS[1], '      - id: clang23\n        name: clang 23 for the wasm C build\n        run: bash scripts/ci-llvm.sh install');
  assert.match(WASM_CLANG_STEPS[0], /^      - id: clang23-cache\n        uses: actions\/cache\/restore@[0-9a-f]{40} # v[\d.]+\n/);
  assert.ok(WASM_CLANG_STEPS[0].includes('          path: ~/.cache/llvm-23-debs\n'));
  assert.ok(WASM_CLANG_STEPS[0].includes('          restore-keys: llvm-23-debs-${{ runner.arch }}-ubuntu-24.04-'));
  assert.match(WASM_CLANG_STEPS[2], /^      - if: steps\.clang23\.outputs\.changed == 'true'\n        uses: actions\/cache\/save@[0-9a-f]{40} # v[\d.]+\n/);
  assert.ok(WASM_CLANG_STEPS[2].includes('          key: llvm-23-debs-${{ runner.arch }}-ubuntu-24.04-${{ steps.clang23.outputs.build }}'));
  assert.equal(WASM_CLANG_BLOCK, WASM_CLANG_STEPS.join('\n'));
});

test('the install script pins every llvm-toolchain-23 package at one version and reads each back', () => {
  assert.ok(CLANG_SCRIPT.includes('v=$(apt-cache policy clang-23 | awk \'/Candidate:/ { print $2 }\')'));
  assert.match(WASM_LLVM_VERSION, /^23\.\d+$/);
  assert.ok(CLANG_SCRIPT.includes(`case "$v" in 1:${WASM_LLVM_VERSION}.*~++*) ;; *) echo "expected clang ${WASM_LLVM_VERSION}.x candidate, got $v" >&2; exit 1 ;; esac`), 'the signed apt source candidate must stay on LLVM 23.1');
  const install = CLANG_SCRIPT.split('\n').find((l) => l.includes('apt-get install') && l.includes('clang-23='));
  for (const p of LLVM_PACKAGES) {
    assert.ok(install.includes(` "${p}=$v"`), `${p} is not installed at the pinned version`);
  }
  assert.ok(!/\s(clang-23|llvm-23)(\s|$)/.test(install), 'an unpinned package name in the install line');
  assert.ok(CLANG_SCRIPT.includes(`for p in ${LLVM_PACKAGES.join(' ')}; do test "$(dpkg-query -W -f='\${Version}' "$p")" = "$v"; done`));
  assert.ok(CLANG_SCRIPT.includes(`case "$v" in 1:${WASM_LLVM_VERSION}.*~++*) ;; *) echo "expected an installed clang ${WASM_LLVM_VERSION}.x build, got $v" >&2; exit 1 ;; esac`), 'the cached build is read back against the same pin');
  assert.ok(CLANG_SCRIPT.includes(`"$bin/clang-23" --version | grep -F "clang version ${WASM_LLVM_VERSION}."`));
  assert.ok(CLANG_SCRIPT.includes('bin="${LLVM_BIN:-/usr/bin}"'), 'the compiler is read back by its absolute path');
  assert.ok(CLANG_SCRIPT.includes('apt-cache policy clang-23 llvm-23'));
  assert.ok(CLANG_SCRIPT.includes('packages=(' + LLVM_PACKAGES.join(' ') + ')'), 'the cached build is the same eight packages');
});

test('the install script checks the fingerprint on the keyring apt is pointed at', () => {
  const lines = CLANG_SCRIPT.split('\n').map((l) => l.trim());
  assert.ok(lines.includes(`fingerprint=${WASM_LLVM_KEY_FPR}`));
  assert.ok(lines.includes('keyring="${LLVM_KEYRING:-/usr/share/keyrings/apt-llvm.gpg}"'));
  const dearmor = lines.indexOf('gpg --dearmor -o "$temp/apt-llvm.gpg" "$temp/llvm-snapshot.asc"');
  const check = lines.indexOf('test "$primaries" = "$fingerprint"');
  const install = lines.indexOf('sudo install -m 0644 "$temp/apt-llvm.gpg" "$keyring"');
  const same = lines.indexOf('cmp "$temp/apt-llvm.gpg" "$keyring"');
  const source = lines.findIndex((l) => l.includes('[signed-by=$keyring]'));
  assert.ok(dearmor !== -1 && dearmor < check && check < install && install < same && same < source, lines.join('\n'));
  assert.ok(lines[check - 1].startsWith('primaries=$(gpg --show-keys --with-colons "$temp/apt-llvm.gpg"'));
  assert.ok(WASM_CLANG_ENV_BLOCK.includes('CC_wasm32_unknown_unknown: /usr/bin/clang-23'));
  assert.ok(WASM_CLANG_ENV_BLOCK.includes('AR_wasm32_unknown_unknown: /usr/bin/llvm-ar-23'));
});

for (const job of WASM_JOBS) {
  test(`a ${job} job without the clang 23 job env is reported`, () => {
    const problems = checkWorkflow(fixture(editJob(job, (b) => b.replace(`${WASM_CLANG_ENV_BLOCK}\n`, ''))));
    assert.ok(problems.includes(ENV_RULE(job)), problems.join('\n'));
  });
  test(`a ${job} job whose CC is a bare clang-23 that PATH resolves is reported`, () => {
    const problems = checkWorkflow(fixture(editJob(job, (b) => b.replace(': /usr/bin/clang-23', ': clang-23'))));
    assert.ok(problems.includes(ENV_RULE(job)), problems.join('\n'));
  });
  test(`a ${job} job env with an extra compiler flag is reported`, () => {
    const problems = checkWorkflow(fixture(editJob(job, (b) => b.replace(WASM_CLANG_ENV_BLOCK, `${WASM_CLANG_ENV_BLOCK}\n      CFLAGS_wasm32_unknown_unknown: -O0`))));
    assert.ok(problems.includes(ENV_RULE(job)), problems.join('\n'));
  });
  test(`a ${job} job running the install script with another argument is reported`, () => {
    const problems = checkWorkflow(fixture(editJob(job, (b) => b.replace('bash scripts/ci-llvm.sh install', 'bash scripts/ci-llvm.sh install --any'))));
    assert.ok(problems.includes(STEP_RULE(job)), problems.join('\n'));
  });
  test(`a ${job} job restoring another cache is reported`, () => {
    const problems = checkWorkflow(fixture(editJob(job, (b) => b.replace('~/.cache/llvm-23-debs', '~/.cache/llvm-24-debs'))));
    assert.ok(problems.includes(STEP_RULE(job)), problems.join('\n'));
  });
  test(`a ${job} job saving the cache unconditionally is reported`, () => {
    const problems = checkWorkflow(fixture(editJob(job, (b) => b.replace("      - if: steps.clang23.outputs.changed == 'true'\n        uses: actions/cache/save", '      - uses: actions/cache/save'))));
    assert.ok(problems.includes(STEP_RULE(job)), problems.join('\n'));
  });
  test(`a ${job} job that skips the clang 23 step with an if: is reported`, () => {
    const problems = checkWorkflow(fixture(editJob(job, (b) => b.replace('        name: clang 23 for the wasm C build\n', '        name: clang 23 for the wasm C build\n        if: false\n'))));
    assert.ok(problems.includes(STEP_RULE(job)), problems.join('\n'));
  });
  test(`a ${job} job that installs clang 23 after other steps is reported`, () => {
    const moved = editJob(job, (b) => {
      const without = b.replace(`${WASM_CLANG_BLOCK}\n`, '');
      return `${without.replace(/\n*$/, '')}\n${WASM_CLANG_BLOCK}\n`;
    });
    const problems = checkWorkflow(fixture(moved));
    assert.ok(problems.includes(STEP_RULE(job)), problems.join('\n'));
  });
}

// The validator differential of the d3429ce lead: the earlier rule matched substrings, so this job passed it while
// running no install at all, with the required strings in a comment and in an echo.
test('an evading job that only names the pinned strings in a comment and an echo is reported', () => {
  const evading = editJob('web-build', (b) =>
    b.replace(
      `${WASM_CLANG_BLOCK}\n`,
      `      # ${WASM_CLANG_BLOCK.split('\n').map((l) => l.trim()).join(' ; ')}\n` +
        `      - run: echo '${WASM_CLANG_BLOCK.split('\n').filter((l) => l.includes('ci-llvm.sh')).join('').trim()}'\n`,
    ),
  );
  const problems = checkWorkflow(fixture(evading));
  assert.ok(problems.includes(STEP_RULE('web-build')), problems.join('\n'));
});

for (const [what, edit, expected] of [
  [
    'the hyphenated target variable on a step, which cc reads before the underscored one',
    (b) => b.replace('      - run: wasm-pack build', '      - env:\n          CC_wasm32-unknown-unknown: /usr/bin/gcc\n        run: wasm-pack build'),
    /names a C toolchain variable outside the canonical clang 23 job env/,
  ],
  [
    'a TARGET_CC in a flow-style step env',
    (b) => b.replace('      - run: wasm-pack build', '      - env: { TARGET_CC: gcc }\n        run: wasm-pack build'),
    /names a C toolchain variable outside the canonical clang 23 job env/,
  ],
  [
    'a quoted CC key on a step',
    (b) => b.replace('      - run: wasm-pack build', '      - env:\n          "CC": gcc\n        run: wasm-pack build'),
    /names a C toolchain variable outside the canonical clang 23 job env/,
  ],
  [
    'a step that exports the compiler through GITHUB_ENV',
    (b) => b.replace('      - run: wasm-pack build', '      - run: echo "CC_wasm32_unknown_unknown=/usr/bin/gcc" >> "$GITHUB_ENV"\n      - run: wasm-pack build'),
    /writes GITHUB_ENV or GITHUB_PATH/,
  ],
  [
    'a step that puts another clang first on PATH',
    (b) => b.replace('      - run: wasm-pack build', '      - run: echo "$RUNNER_TEMP/fake" >> "$GITHUB_PATH"\n      - run: wasm-pack build'),
    /writes GITHUB_ENV or GITHUB_PATH/,
  ],
]) {
  test(`web-build with ${what} is reported`, () => {
    const problems = checkWorkflow(fixture(editJob('web-build', edit)));
    assert.ok(problems.some((p) => expected.test(p)), problems.join('\n'));
  });
}

test('a compiler variable in the workflow-level env is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('env:\n  CARGO_TERM_COLOR: always\n', 'env:\n  CARGO_TERM_COLOR: always\n  CC: gcc\n')));
  assert.ok(problems.some((p) => /names a C toolchain variable outside the canonical clang 23 job env/.test(p)), problems.join('\n'));
});

test('a YAML anchor and alias that would reuse an env block is reported', () => {
  const anchored = GOOD.replace('env:\n  CARGO_TERM_COLOR: always\n', 'env: &shared\n  CARGO_TERM_COLOR: always\n').replace(
    '  deny:\n    runs-on: ubuntu-latest\n',
    '  deny:\n    runs-on: ubuntu-latest\n    env: *shared\n',
  );
  const problems = checkWorkflow(fixture(anchored));
  assert.ok(problems.some((p) => p.includes('YAML anchors, aliases and merge keys are not allowed')), problems.join('\n'));
});

test('a wasm job added tomorrow without the pinned clang 23 is reported', () => {
  const extra =
    '  wasm-extra:\n' +
    '    runs-on: ubuntu-latest\n' +
    '    steps:\n' +
    '      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7\n' +
    '      - run: cargo build -p dilla-core --target wasm32-unknown-unknown --locked\n';
  const problems = checkWorkflow(fixture(GOOD.replace('  deny:\n', `${extra}  deny:\n`)));
  assert.ok(problems.includes(ENV_RULE('wasm-extra')), problems.join('\n'));
  assert.ok(problems.includes(STEP_RULE('wasm-extra')), problems.join('\n'));
});

test('jobs that build no wasm need no clang 23', () => {
  assert.deepEqual(checkWorkflow(fixture(GOOD)), []);
});

// install-release (2026-10-14): the release workflow, checked against the real ci.yml it must match.
function releaseFixture(release) {
  const dir = mkdtempSync(join(tmpdir(), 'dilla-release-'));
  mkdirSync(join(dir, '.github', 'workflows'), { recursive: true });
  addE2ePackage(dir);
  writeFileSync(join(dir, '.github', 'workflows', 'ci.yml'), REAL_CI);
  writeFileSync(join(dir, '.github', 'workflows', 'desktop.yml'), REAL_DESKTOP);
  writeFileSync(join(dir, '.github', 'workflows', 'release.yml'), release);
  return dir;
}

function releaseProblems(edit) {
  const edited = edit(REAL_RELEASE);
  assert.notEqual(edited, REAL_RELEASE, 'the edit changed nothing; the fixture drifted from release.yml');
  return checkReleaseWorkflow(releaseFixture(edited));
}

function assertReported(problems, needle) {
  assert.ok(problems.some((p) => p.includes(needle)), `want a problem naming ${needle}:\n${problems.join('\n')}`);
}

test('the real release workflow passes every rule', () => {
  assert.deepEqual(checkReleaseWorkflow(REPO_ROOT), []);
});


test('a release workflow without the release job is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/\n {2}release:\n[\s\S]*$/, '\n')), 'missing job "release"');
});

test('a release without the native Bot build is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/\n {2}bot-release:\n[\s\S]*?\n {2}go-release:/, '\n  go-release:')), 'bot-release');
});

test('a Bot release without a stamped and checked version is reported', () => {
  const unstamped = REAL_RELEASE.replace(' -X main.Version=${VERSION}', '');
  assertReported(releaseProblems(() => unstamped), 'Bot binary says its version');
});

test('a release checksum that omits Bot binaries is reported', () => {
  assertReported(
    releaseProblems((t) => t.replace(`sha256sum ${RELEASE_ASSETS.join(' ')} > SHA256SUMS`,
      'sha256sum dillad-linux-amd64 dillad-linux-arm64 dilla_core_wasi.wasm install.sh > SHA256SUMS')),
    '> SHA256SUMS',
  );
});

test('a release that no longer attaches install.sh is reported', () => {
  assertReported(releaseProblems((t) => t.replace('release/install.sh release/install-bot.sh release/SHA256SUMS', 'release/install-bot.sh release/SHA256SUMS')), 'release/install.sh');
});

test('a release that no longer attaches install-bot.sh is reported', () => {
  assertReported(releaseProblems((t) => t.replace('release/install-bot.sh release/SHA256SUMS', 'release/SHA256SUMS')), 'release/install-bot.sh');
});

test('SHA256SUMS that leaves an asset out is reported', () => {
  assertReported(
    releaseProblems((t) => t.replace(`sha256sum ${RELEASE_ASSETS.join(' ')} > SHA256SUMS`, 'sha256sum dillad-linux-amd64 dillad-linux-arm64 > SHA256SUMS')),
    '> SHA256SUMS',
  );
});

test('a release job without contents: write is reported', () => {
  assertReported(releaseProblems((t) => t.replace('    permissions:\n      contents: write\n', '')), 'contents: write');
});

test('contents: write on a build job is reported', () => {
  assertReported(releaseProblems((t) => t.replace('  rust-wasi:\n    runs-on: ubuntu-latest\n', '  rust-wasi:\n    runs-on: ubuntu-latest\n    permissions:\n      contents: write\n')), 'only the release job writes');
});

test('a container image in the release workflow is reported', () => {
  assertReported(releaseProblems((t) => `${t}\n  image:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: docker/setup-buildx-action@f87e5991a6d7451dcb8d9637bfbc97413f497069 # v4.4.1\n`), 'container image');
});

test('a dispatch input expanded inside a script is reported', () => {
  assertReported(releaseProblems((t) => t.replace('tag="${INPUT_TAG:-$GITHUB_REF_NAME}"', 'tag="${{ inputs.tag }}"')), 'outside an env: key');
});

test('an action at a version ci.yml does not use is reported', () => {
  assertReported(releaseProblems((t) => t.replace('  rust-wasi:\n    runs-on: ubuntu-latest\n    needs: [tag]\n    steps:\n      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7', '  rust-wasi:\n    runs-on: ubuntu-latest\n    needs: [tag]\n    steps:\n      - uses: actions/checkout@main')), 'actions/checkout@main');
});

test('a tag job that no longer checks the ref is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/ {10}test "\$GITHUB_REF" = .*\n/, '')), 'refs/tags/$tag');
});

test('a release web-build without the clang 23 pin is reported', () => {
  assertReported(releaseProblems((t) => t.replace('        name: clang 23 for the wasm C build\n', '        name: clang for the wasm C build\n')), 'clang 23');
});

test('a release go-release that builds before the placeholder guard is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/ {6}- name: The binaries embed the real client[^\n]*\n {8}run: \|\n(?: {10}[^\n]*\n)+/, '')), 'refuse the placeholder');
});

test('a release go-release that does not wait for web-build is reported', () => {
  assertReported(releaseProblems((t) => t.replace('needs: [tag, web-build]', 'needs: [tag]')), '"web-build" in its needs');
});

test('a release Bot build that does not wait for the WASI core is reported', () => {
  assertReported(releaseProblems((t) => t.replace('needs: [tag, rust-wasi]', 'needs: [tag]')), '"rust-wasi" in its needs');
});

test('a release Bot build that omits the WASI core download is reported', () => {
  assertReported(releaseProblems((t) => t.replace('          path: internal/mlswasi/testdata\n', '')), 'path: internal/mlswasi/testdata');
});

test('|| true in the release workflow is reported', () => {
  assertReported(releaseProblems((t) => t.replace('sha256sum -c --strict SHA256SUMS', 'sha256sum -c --strict SHA256SUMS || true')), '|| true');
});

test('a release workflow that no longer runs on v* tags is reported', () => {
  assertReported(releaseProblems((t) => t.replace("tags: ['v*']", "tags: ['*']")), "tags 'v*'");
});

for (const action of ['actions/cache', 'actions/cache/restore']) {
  test(`a release build that restores ${action} is rejected as a build cache`, () => {
    const problems = releaseProblems((t) => t.replace(
      '      - run: rustup show active-toolchain',
      `      - uses: ${action}@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0\n      - run: rustup show active-toolchain`,
    ));
    assertReported(problems, 'a build cache in the release workflow');
  });
}

test('a third-party action pinned by SHA is still rejected in release', () => {
  const problems = releaseProblems((t) => t.replace(
    '      - run: rustup show active-toolchain',
    '      - uses: taiki-e/install-action@1168f6810a3cb30a3514bf5756a3b7f0422c6df8 # v2\n      - run: rustup show active-toolchain',
  ));
  assertReported(problems, 'not a GitHub-owned action');
});

test('a movable GitHub action tag is rejected in release', () => {
  const problems = releaseProblems((t) => t.replace(
    'actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7',
    'actions/checkout@v7',
  ));
  assertReported(problems, '40-hex commit SHA');
});

test('a flow-style action step is rejected in release', () => {
  const problems = releaseProblems((t) => t.replace(
    '      - run: rustup show active-toolchain',
    '      - { uses: taiki-e/install-action@1168f6810a3cb30a3514bf5756a3b7f0422c6df8 }\n      - run: rustup show active-toolchain',
  ));
  assertReported(problems, 'flow-style action');
});

test('every release action is owned by GitHub and pinned to a commit', () => {
  const actions = REAL_RELEASE.split('\n').filter((line) => /uses:/.test(line));
  assert.ok(actions.length > 0);
  for (const line of actions) assert.match(line, /uses:\s*actions\/[\w-]+@[0-9a-f]{40} # \S+$/, line);
});

test('a release that attaches an unstamped install.sh is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/ {10}sed -i "s\/\^DILLA_RELEASE_TAG=.*\n/, '')), 'DILLA_RELEASE_TAG');
});

test('a release that does not check the stamp landed is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/ {10}test "\$\(grep -c "\^DILLA_RELEASE_TAG=.*\n/, '')), 'grep -c');
});

test('a release that does not refuse an existing release by name is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/ {10}if gh release view "\$TAG".*\n/, '')), 'gh release view');
});

test('a tag job that releases a commit off main is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/ {10}git merge-base --is-ancestor .*\n/, '')), 'origin/main');
});

test('a build cache in the release workflow is reported', () => {
  assertReported(releaseProblems((t) => t.replace('          node-version-file: .nvmrc\n', '          node-version-file: .nvmrc\n          cache: npm\n')), 'build cache');
});

test('a release setup-go without cache: false is reported', () => {
  assertReported(releaseProblems((t) => t.replace('          cache: false\n', '')), 'setup-go caches by default');
});

// v0.1.0-web3.1: the release stamps its tag (without the leading v) into dillad at link time; ci.yml's builds never do.
test('a release build without the version stamp is reported', () => {
  assertReported(releaseProblems((t) => t.replace(' -X main.Version=${VERSION}" -o dist/dillad-linux-arm64', '" -o dist/dillad-linux-arm64')),
    'go build without -X main.Version=${VERSION}');
});

test('an extra release go build without the version stamp is reported', () => {
  const extra = "      - name: linux/riscv64\n        run: CGO_ENABLED=0 GOOS=linux GOARCH=riscv64 go build -trimpath -ldflags='-s -w' -o dist/dillad-linux-riscv64 ./cmd/dillad\n";
  assertReported(releaseProblems((t) => t.replace('      - name: linux/arm64\n', `${extra}      - name: linux/arm64\n`)),
    'go build without -X main.Version=${VERSION}');
});

test('a release that does not check the stamped version is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/ {10}test "\$\(dist\/dillad-linux-amd64 version .*\n/, '')), 'dillad-linux-amd64 version');
});

test('a release build whose version does not come from the tag job is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/( {6}- name: linux\/arm64\n {8}env:\n {10}VERSION: )\$\{\{ needs\.tag\.outputs\.version \}\}/, '$1${{ github.ref_name }}')),
    'uses the version without its own env VERSION: ${{ needs.tag.outputs.version }}');
});

test('a release whose tag job does not output the version is reported', () => {
  assertReported(releaseProblems((t) => t.replace('      version: ${{ steps.tag.outputs.version }}\n', '')), 'must output version');
  assertReported(releaseProblems((t) => t.replace('          echo "version=${tag#v}" >> "$GITHUB_OUTPUT"\n', '')), 'is missing: echo "version=${tag#v}"');
});

test('a ci.yml build that stamps a version is reported', () => {
  const stamped = GOOD.replace("go build -trimpath -ldflags='-s -w' ./cmd/dillad", "go build -trimpath -ldflags='-s -w -X main.Version=1.2.3' ./cmd/dillad");
  assert.notEqual(stamped, GOOD);
  const problems = checkWorkflow(fixture(stamped));
  assert.ok(problems.some((p) => p.includes('main.Version')), problems.join('\n'));
});

// Task 3 m2: a quoted symbol after -X, and the -X= spelling, stamp a version all the same.
test('a ci.yml build that stamps a quoted version is reported', () => {
  for (const flag of ["-X 'main.Version=1.2.3'", '-X "main.Version=1.2.3"', '-X=main.Version=1.2.3', "-X='main.Version=1.2.3'"]) {
    const stamped = GOOD.replace("go build -trimpath -ldflags='-s -w' ./cmd/dillad", `go build -trimpath -ldflags="-s -w ${flag}" ./cmd/dillad`);
    assert.notEqual(stamped, GOOD);
    const problems = checkWorkflow(fixture(stamped));
    assert.ok(problems.some((p) => p.includes('stamps main.Version')), `${flag}\n${problems.join('\n')}`);
  }
});

test('the CLI checks release.yml too', () => {
  const root = releaseFixture(REAL_RELEASE.replace('release/install.sh release/install-bot.sh release/SHA256SUMS', 'release/install-bot.sh release/SHA256SUMS'));
  const cwd = mkdtempSync(join(tmpdir(), 'dilla-ci-cwd-'));
  const result = runCli(root, cwd);
  assert.equal(result.status, 1, `stdout: ${result.stdout}\nstderr: ${result.stderr}`);
  assert.match(result.stderr, /release\.yml/);
});

test('a job that lost its change gate is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace("    if: needs.changes.outputs.go != 'false'\n    runs-on: ubuntu-latest\n    timeout-minutes: 55\n", '    runs-on: ubuntu-latest\n    timeout-minutes: 55\n')));
  assert.ok(problems.some((p) => p.includes('must carry the job-level if:')), problems.join('\n'));
});

test('a job that does not need changes is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/(  deny:\n)    needs: \[changes\]\n/, '$1')));
  assert.ok(problems.some((p) => p.includes('"deny" must list "changes"')), problems.join('\n'));
});

test('the docs job may not be gated: it is the one that runs for a docs-only change', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('  docs:\n', "  docs:\n    needs: [changes]\n    if: needs.changes.outputs.code != 'false'\n")));
  assert.ok(problems.some((p) => p.includes('"docs" must run for every change')), problems.join('\n'));
});

test('a job added without a gate is reported', () => {
  const problems = checkWorkflow(fixture(`${GOOD}\n  extra:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n`));
  assert.ok(problems.some((p) => p.includes('"extra" has no gate')), problems.join('\n'));
});

test('a gate that can run while a needed job is skipped is reported', () => {
  // go-release (go or web) needs web-build (e2e): fine. Narrow web-build to core and a UI change would skip go-release's dependency.
  const problems = checkWorkflow(fixture(GOOD.replace(/(  web-build:\n(?:    needs:.*\n)?)    if: .*\n/, "$1    if: needs.changes.outputs.core != 'false'\n")));
  assert.ok(problems.some((p) => p.includes('"web-build" must carry')), problems.join('\n'));
});

test('the real ci-ok job needs every other job', () => {
  const body = REAL_CI.slice(REAL_CI.indexOf('\n  ci-ok:'));
  const needs = /needs: \[([^\]]*)\]/.exec(body)[1].split(',').map((s) => s.trim());
  const jobs = [...REAL_CI.slice(REAL_CI.indexOf('\njobs:')).matchAll(/^ {2}([A-Za-z0-9_-]+):\s*$/gm)].map((m) => m[1]).filter((job) => job !== 'ci-ok');
  assert.deepEqual(needs.sort(), jobs.sort());
});

test('a ci-ok job that forgot a job is reported', () => {
  const changed = GOOD.replace(/(  ci-ok:\n    needs: \[[^\]]*)changes, /, '$1');
  assert.notEqual(changed, GOOD);
  const problems = checkWorkflow(fixture(changed));
  assert.ok(problems.some((p) => p.includes('"ci-ok"') && p.includes('"changes"')), problems.join('\n'));
});

test('a ci-ok job without the failure check, or without always(), is reported', () => {
  let problems = checkWorkflow(fixture(GOOD.replace("contains(needs.*.result, 'cancelled')", 'false')));
  assert.ok(problems.some((p) => p.includes('"ci-ok"') && p.includes('cancelled')), problems.join('\n'));
  problems = checkWorkflow(fixture(GOOD.replace('    if: always()\n    runs-on: ubuntu-latest\n    steps:\n      - if: contains', '    runs-on: ubuntu-latest\n    steps:\n      - if: contains')));
  assert.ok(problems.some((p) => p.includes('"ci-ok"') && p.includes('always()')), problems.join('\n'));
});

test('if: always() stays forbidden in every job but ci-ok, and in ci-ok steps', () => {
  let problems = checkWorkflow(fixture(GOOD.replace("  deny:\n    needs: [changes]\n    if: needs.changes.outputs.core != 'false'\n", '  deny:\n    needs: [changes]\n    if: always()\n')));
  assert.ok(problems.some((p) => p.includes('if: always()') && p.includes('R22')), problems.join('\n'));
  problems = checkWorkflow(fixture(GOOD.replace('      - run: echo ok\n', '      - if: always()\n        run: echo ok\n')));
  assert.ok(problems.some((p) => p.includes('if: always()') && p.includes('R22')), problems.join('\n'));
});

test('a gate that tests == true instead of != false is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace("needs.changes.outputs.go != 'false'", "needs.changes.outputs.go == 'true'")));
  assert.ok(problems.some((p) => p.includes('must carry the job-level if:')), problems.join('\n'));
});

test('the docs job must run the real checks, not only their unit tests', () => {
  for (const step of ['check:brief', 'check:copy', 'check:docs', 'check:ci']) {
    const problems = checkWorkflow(fixture(GOOD.replace(`      - run: npm run ${step}\n`, '')));
    assert.ok(problems.some((p) => p.includes('"docs"') && p.includes(step)), `${step}: ${problems.join('\n')}`);
  }
});

test('installed AppImage sandbox gate requires exact verification after actual upgrade and before upload', () => {
 const step = `      - name: Linux AppImage sandbox native acceptance\n        if: runner.os == 'Linux'\n        run: node packages/desktop/test/support/linux-installed-runtime-proof.mjs packages/desktop/test-results/linux-installed-upgrade.json\n`;
 assert.ok(REAL_DESKTOP.includes(step), 'missing actual installed AppImage kernel proof gate');
 for(const changed of [
  REAL_DESKTOP.replace(step,''),REAL_DESKTOP.replace(step,step.replace("runner.os == 'Linux'",'false')),
  REAL_DESKTOP.replace(step,step.replace('        run:','        continue-on-error: true\n        run:')),
  REAL_DESKTOP.replace(step,step.replace('run: node ','run: echo ')),
  REAL_DESKTOP.replace(step,step.replace('linux-installed-upgrade.json','other.json')),
  REAL_DESKTOP.replace(step,step.replace('native acceptance','diagnostic acceptance')),
  REAL_DESKTOP.replace(step,'').replace('      - name: Installed upgrade acceptance\n',step+'      - name: Installed upgrade acceptance\n'),
  REAL_DESKTOP.replace(step,step.replace('        run:','        env:\n          NODE_OPTIONS: --import ./fake-proof.mjs\n        run:')),
 ]){
  assert.notEqual(changed,REAL_DESKTOP);const root=fixture(GOOD);writeFileSync(join(root,'.github/workflows/desktop.yml'),changed);
  assert.ok(checkDesktopWorkflow(root).some(p=>p.includes('installed AppImage sandbox')),changed);
 }
});


test('native Linux release producer is separate, source-versioned, non-bypassable and uploads only its own outputs', () => {
 const block = REAL_DESKTOP.split('\n  native-linux-packages:\n')[1]?.split(/\n  [a-z][a-z0-9-]*:\n/)[0] ?? '';
 const mutations = [
  REAL_DESKTOP.replace('\n  native-linux-packages:\n' + block, ''),
  ...[
   ['name: desktop-installers-linux-packages-x64', 'name: unrelated-native-outputs'],
   ['linux-packages-x64.json', 'linux-x64.json'],
   ['"$version"', '1.0.0'],
   ['name: Record native Linux package build provenance', 'name: Record native Linux package build provenance\n        if: false'],
   ['name: Build native Linux release packages', 'name: Build native Linux release packages\n        if: false'],
   ['persist-credentials: false', 'persist-credentials: true'],
  ].map(([before, after]) => REAL_DESKTOP.replace('\n  native-linux-packages:\n' + block, '\n  native-linux-packages:\n' + block.replaceAll(before, after))),
 ];
 for (const changed of mutations) {
  const root = fixture(GOOD); writeFileSync(join(root, '.github/workflows/desktop.yml'), changed);
  assert.ok(checkDesktopWorkflow(root).some(problem => problem.includes('native Linux package producer')));
 }
});

// ---- Desktop release trampoline (checkDesktopReleaseWorkflow) ----
// Every test mutates the real committed files in a temporary copy and expects at least one problem.

const yaml = createRequire(import.meta.url)('js-yaml');
const RELEASE_FILES = [
  ...readdirSync(join(REPO_ROOT, '.github', 'workflows')).map(name => `.github/workflows/${name}`),
  'packages/desktop/scripts/release-keys.mjs', 'packages/desktop/scripts/release-sign-job.mjs',
];
const KEYED = '.github/workflows/desktop-release.yml', REQUEST = '.github/workflows/desktop-release-request.yml';
/** A temporary root holding the real files with `changes` (path -> text => text) applied. */
function releaseRoot(changes = {}) {
  const root = mkdtempSync(join(tmpdir(), 'dilla-desktop-release-ci-'));
  for (const file of RELEASE_FILES) {
    const text = readFileSync(join(REPO_ROOT, file), 'utf8');
    mkdirSync(dirname(join(root, file)), { recursive: true });
    writeFileSync(join(root, file), changes[file] ? changes[file](text) : text);
  }
  for (const [file, change] of Object.entries(changes)) if (!RELEASE_FILES.includes(file)) {
    mkdirSync(dirname(join(root, file)), { recursive: true }); writeFileSync(join(root, file), change(''));
  }
  return root;
}
/** A structural change: parse, mutate, dump. */
const edit = mutate => text => { const doc = yaml.load(text); mutate(doc); return yaml.dump(doc, { lineWidth: -1 }); };
const step = (doc, job, match) => doc.jobs[job].steps.find(item => (item.name ?? item.uses ?? item.run ?? '').includes(match));
function reported(changes, label) {
  const problems = checkDesktopReleaseWorkflow(releaseRoot(changes));
  assert.ok(problems.length > 0, `not reported: ${label}`);
  return problems;
}

test('desktop release workflows as committed have no problems', () => {
  assert.deepEqual(checkDesktopReleaseWorkflow(REPO_ROOT), []);
  assert.deepEqual(checkDesktopReleaseWorkflow(releaseRoot({ [KEYED]: edit(() => {}), [REQUEST]: edit(() => {}) })), [], 'a parse and re-dump is not a change');
  assert.equal(DESKTOP_RELEASE_SECRETS.length, 6);
  assert.ok(DESKTOP_RELEASE_ACTIONS.every(action => /^actions\/[a-z-]+@[0-9a-f]{40}$/.test(action)));
  const cli = spawnSync(process.execPath, [SCRIPT_PATH, releaseRoot({ [KEYED]: text => text.replace('cancel-in-progress: false', 'cancel-in-progress: true') })], { encoding: 'utf8' });
  assert.notEqual(cli.status, 0, 'wired into the CLI');
  assert.match(cli.stderr, /desktop-release\.yml/);
});

test('request workflow gaining secrets, an environment, contents: write, any uses or a checkout is reported', () => {
  for (const [label, change] of [
    ['secrets', edit(doc => { doc.jobs.request.steps[0].env = { K: '${{ secrets.DESKTOP_METADATA_KEY_A }}' }; })],
    ['environment', edit(doc => { doc.jobs.request.environment = 'desktop-release'; })],
    ['contents: write', edit(doc => { doc.permissions.contents = 'write'; })],
    ['job permissions', edit(doc => { doc.jobs.request.permissions = { contents: 'read' }; })],
    ['uses', edit(doc => { doc.jobs.request.steps.push({ uses: 'actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1' }); })],
    ['job env', edit(doc => { doc.jobs.request.env = { A: 'b' }; })],
    ['second job', edit(doc => { doc.jobs.other = doc.jobs.request; })],
    ['workflow_dispatch', edit(doc => { doc.on.workflow_dispatch = null; })],
    ['runs-on', edit(doc => { doc.jobs.request['runs-on'] = 'self-hosted'; })],
    ['name', edit(doc => { doc.name = 'desktop release request 2'; })],
  ]) reported({ [REQUEST]: change }, label);
});

test('keyed workflow gaining push, pull_request, workflow_dispatch, a branches filter, a different upstream name or missing types is reported', () => {
  for (const [label, change] of [
    ['push', edit(doc => { doc.on.push = { tags: ['desktop-v*'] }; })],
    ['pull_request', edit(doc => { doc.on.pull_request = null; })],
    ['workflow_dispatch', edit(doc => { doc.on.workflow_dispatch = null; })],
    ['branches', edit(doc => { doc.on.workflow_run.branches = ['main']; })],
    ['upstream name', edit(doc => { doc.on.workflow_run.workflows = ['ci']; })],
    ['types', edit(doc => { delete doc.on.workflow_run.types; })],
    ['concurrency', edit(doc => { doc.concurrency['cancel-in-progress'] = true; })],
  ]) reported({ [KEYED]: change }, label);
});

test('changing the sign if is reported', () => {
  // The event check sits on the first job (gate); every later job needs it.
  reported({ [KEYED]: edit(doc => { doc.jobs.gate.if = "github.event.workflow_run.conclusion == 'success'"; }) }, 'no event check');
  reported({ [KEYED]: edit(doc => { delete doc.jobs.gate.if; }) }, 'no if');
  reported({ [KEYED]: edit(doc => { doc.jobs.sign.if = 'true'; }) }, 'an if on sign');
  reported({ [KEYED]: edit(doc => { doc.jobs.publish.if = 'true'; }) }, 'an if on publish');
  reported({ [KEYED]: edit(doc => { step(doc, 'publish', 'Publish the versioned').if = 'false'; }) }, 'a step if');
});

test('removing environment from sign, an environment on publish or verify-published, environment as a mapping or expression elsewhere, or the environment named in another workflow is reported', () => {
  for (const [label, change] of [
    ['sign without environment', { [KEYED]: edit(doc => { delete doc.jobs.sign.environment; }) }],
    ['publish environment', { [KEYED]: edit(doc => { doc.jobs.publish.environment = 'desktop-release'; }) }],
    ['verify-published environment', { [KEYED]: edit(doc => { doc.jobs['verify-published'].environment = 'other'; }) }],
    ['mapping on sign', { [KEYED]: edit(doc => { doc.jobs.sign.environment = { name: 'desktop-release' }; }) }],
    ['feed environment changed', { [KEYED]: edit(doc => { doc.jobs.feed.environment = 'desktop-release'; }) }],
    ['collect environment', { [KEYED]: edit(doc => { doc.jobs.collect.environment = 'desktop-release'; }) }],
    ['gate environment', { [KEYED]: edit(doc => { doc.jobs.gate.environment = 'desktop-release'; }) }],
    ['mapping in ci.yml', { '.github/workflows/release.yml': edit(doc => { Object.values(doc.jobs)[0].environment = { name: 'desktop-release' }; }) }],
    ['expression in ci.yml', { '.github/workflows/release.yml': edit(doc => { Object.values(doc.jobs)[0].environment = "${{ format('desktop-{0}', 'release') }}"; }) }],
    ['plain name in another workflow', { '.github/workflows/release.yml': edit(doc => { Object.values(doc.jobs)[0].environment = 'desktop-release'; }) }],
    ['a new workflow', { '.github/workflows/sneaky.yml': () => 'name: sneaky\non: push\npermissions: {}\njobs:\n  x:\n    runs-on: ubuntu-24.04\n    environment: desktop-release\n    steps:\n      - run: echo\n' }],
  ]) reported(change, label);
});

test('any permission added to sign, contents: write missing from publish, contents: write on verify-published are reported', () => {
  for (const [label, change] of [
    ['sign write', edit(doc => { doc.jobs.sign.permissions.contents = 'write'; })],
    ['sign id-token', edit(doc => { doc.jobs.sign.permissions['id-token'] = 'write'; })],
    ['publish read', edit(doc => { doc.jobs.publish.permissions.contents = 'read'; })],
    ['verify write', edit(doc => { doc.jobs['verify-published'].permissions.contents = 'write'; })],
    ['top-level', edit(doc => { doc.permissions = { contents: 'read' }; })],
    ['feed actions', edit(doc => { doc.jobs.feed.permissions.actions = 'write'; })],
    ['sign actions', edit(doc => { doc.jobs.sign.permissions.actions = 'read'; })],
    ['collect write', edit(doc => { doc.jobs.collect.permissions.contents = 'write'; })],
    ['gate pull-requests write', edit(doc => { doc.jobs.gate.permissions['pull-requests'] = 'write'; })],
    ['collect needs', edit(doc => { delete doc.jobs.collect.needs; })],
    ['sign needs', edit(doc => { doc.jobs.sign.needs = ['gate']; })],
    ['publish needs', edit(doc => { doc.jobs.publish.needs = []; })],
    ['verify needs', edit(doc => { doc.jobs['verify-published'].needs = ['gate', 'sign']; })],
    ['feed needs', edit(doc => { doc.jobs.feed.needs = ['gate', 'sign', 'publish']; })],
    ['extra job', edit(doc => { doc.jobs.extra = { 'runs-on': 'ubuntu-24.04', steps: [{ run: 'echo' }] }; })],
  ]) reported({ [KEYED]: change }, label);
});

test('a seventh secrets reference, a secrets reference outside the Sign step, secrets[ indexing, secrets: inherit and toJSON(secrets) are reported', () => {
  for (const [label, change] of [
    ['seventh', { [KEYED]: edit(doc => { step(doc, 'sign', 'Sign (').env.DESKTOP_EXTRA = '${{ secrets.DESKTOP_EXTRA }}'; }) }],
    ['outside', { [KEYED]: edit(doc => { step(doc, 'sign', 'Verify the candidate').env.K = '${{ secrets.DESKTOP_METADATA_KEY_A }}'; }) }],
    ['other job', { [KEYED]: edit(doc => { step(doc, 'publish', 'Publish the versioned').env.K = '${{ secrets.DESKTOP_SANDBOX_KEY_B }}'; }) }],
    ['collect job', { [KEYED]: edit(doc => { step(doc, 'collect', 'Collect').env.K = '${{ secrets.GITHUB_TOKEN }}'; }) }],
    ['indexing', { [KEYED]: edit(doc => { step(doc, 'sign', 'Sign (').env.DESKTOP_METADATA_KEY_A = "${{ secrets['DESKTOP_METADATA_KEY_A'] }}"; }) }],
    ['toJSON', { [KEYED]: edit(doc => { step(doc, 'gate', 'Gate').env.ALL = '${{ toJSON(secrets) }}'; }) }],
    ['inherit elsewhere', { '.github/workflows/ci.yml': text => text.replace('    uses: ./.github/workflows/desktop.yml\n', '    uses: ./.github/workflows/desktop.yml\n    secrets: inherit\n') }],
    ['DESKTOP_ secret in another workflow', { '.github/workflows/release.yml': text => text + '\n# ${{ secrets.DESKTOP_METADATA_KEY_A }}\n' }],
    ['swapped value', { [KEYED]: edit(doc => { step(doc, 'sign', 'Sign (').env.DESKTOP_METADATA_KEY_A = '${{ secrets.DESKTOP_METADATA_KEY_B }}'; }) }],
  ]) reported(change, label);
});

test('an extra or missing key in the Sign step env is reported', () => {
  reported({ [KEYED]: edit(doc => { step(doc, 'sign', 'Sign (').env.GH_TOKEN = '${{ github.token }}'; }) }, 'extra');
  reported({ [KEYED]: edit(doc => { delete step(doc, 'sign', 'Sign (').env.DESKTOP_SANDBOX_KEY_B; }) }, 'missing');
  reported({ [KEYED]: edit(doc => { step(doc, 'sign', 'Sign (').name = 'Sign'; }) }, 'renamed');
  reported({ [KEYED]: edit(doc => { step(doc, 'sign', 'Sign (').run += ' --debug'; }) }, 'run changed');
});

test('the notes step takes previous from the gate output, not from tags or another value', () => {
  reported({ [KEYED]: edit(doc => { delete step(doc, 'collect', 'Generate release notes').env.PREVIOUS; }) }, 'missing PREVIOUS');
  reported({ [KEYED]: edit(doc => { step(doc, 'collect', 'Generate release notes').env.PREVIOUS = ''; }) }, 'empty PREVIOUS');
  reported({ [KEYED]: edit(doc => { step(doc, 'collect', 'Generate release notes').env.EXTRA = 'x'; }) }, 'extra env');
});

test('defaults, shell, working-directory, container, services, NODE_OPTIONS in sign are reported', () => {
  for (const [label, change] of [
    ['defaults', doc => { doc.jobs.sign.defaults = { run: { shell: 'bash' } }; }],
    ['workflow defaults', doc => { doc.defaults = { run: { shell: 'bash' } }; }],
    ['shell', doc => { step(doc, 'sign', 'Verify the candidate').shell = 'bash'; }],
    ['working-directory', doc => { step(doc, 'sign', 'Sign (')['working-directory'] = 'packages'; }],
    ['container', doc => { doc.jobs.sign.container = 'node:24'; }],
    ['services', doc => { doc.jobs.sign.services = { x: { image: 'redis' } }; }],
    ['NODE_OPTIONS', doc => { step(doc, 'sign', 'Verify the candidate').env.NODE_OPTIONS = '--require ./x.js'; }],
    ['NODE_OPTIONS in collect', doc => { step(doc, 'collect', 'Collect').env.NODE_OPTIONS = '--require ./x.js'; }],
    ['git hardening', doc => { delete step(doc, 'sign', 'Verify the candidate').env.GIT_CONFIG_GLOBAL; }],
    ['git in sign', doc => { doc.jobs.sign.steps.splice(3, 0, { run: 'git status' }); }],
    ['NODE_PATH in publish', doc => { step(doc, 'publish', 'Publish the versioned').env.NODE_PATH = '/tmp'; }],
    ['job env', doc => { doc.jobs.sign.env = { A: 'b' }; }],
    ['workflow env', doc => { doc.env = { A: 'b' }; }],
    ['continue-on-error', doc => { doc.jobs.sign['continue-on-error'] = true; }],
    ['runs-on', doc => { doc.jobs.sign['runs-on'] = 'ubuntu-latest'; }],
  ]) reported({ [KEYED]: edit(change) }, label);
});

test('checkout ref/repository/path or workflow_run.head_sha is reported', () => {
  for (const [label, change] of [
    ['ref', doc => { step(doc, 'sign', 'actions/checkout').with.ref = '${{ github.event.workflow_run.head_branch }}'; }],
    ['repository', doc => { step(doc, 'publish', 'actions/checkout').with.repository = 'other/dilla'; }],
    ['path', doc => { step(doc, 'sign', 'actions/checkout').with.path = 'src'; }],
    ['head_sha', doc => { step(doc, 'gate', 'Gate').env.HINT_SHA = '${{ github.event.workflow_run.head_sha }}'; }],
    ['head_repository', doc => { step(doc, 'gate', 'Gate').env.HINT_REPO = '${{ github.event.workflow_run.head_repository.full_name }}'; }],
    ['gate fetch-depth', doc => { delete step(doc, 'gate', 'actions/checkout').with['fetch-depth']; }],
    ['persist-credentials', doc => { delete step(doc, 'verify-published', 'actions/checkout').with['persist-credentials']; }],
    ['sign fetch-depth', doc => { delete step(doc, 'sign', 'actions/checkout').with['fetch-depth']; }],
    ['sign fetch-depth 1', doc => { step(doc, 'sign', 'actions/checkout').with['fetch-depth'] = 1; }],
  ]) reported({ [KEYED]: edit(change) }, label);
});

test('download-artifact with pattern, merge-multiple, the triggering run id or no artifact-ids is reported', () => {
  for (const [label, change] of [
    ['pattern', doc => { step(doc, 'collect', 'Fetch the accepted').with.pattern = 'desktop-*'; }],
    ['merge-multiple', doc => { step(doc, 'collect', 'Fetch the accepted').with['merge-multiple'] = true; }],
    ['triggering run', doc => { step(doc, 'collect', 'Fetch the accepted').with['run-id'] = '${{ github.event.workflow_run.id }}'; }],
    ['no artifact-ids', doc => { delete step(doc, 'collect', 'Fetch the accepted').with['artifact-ids']; }],
    ['ids from elsewhere', doc => { step(doc, 'collect', 'Fetch the accepted').with['artifact-ids'] = '${{ needs.gate.outputs.run }}'; }],
    ['digest-mismatch', doc => { step(doc, 'collect', 'Fetch the accepted').with['digest-mismatch'] = 'warn'; }],
    ['signed release from another run', doc => { step(doc, 'publish', 'Fetch the signed').with['run-id'] = '${{ needs.gate.outputs.run }}'; }],
    ['raw CI artifacts in sign', doc => { step(doc, 'sign', 'Fetch the signing candidate').with = { ...step(doc, 'collect', 'Fetch the accepted').with }; }],
    ['candidate by name in sign', doc => { const w = step(doc, 'sign', 'Fetch the signing candidate').with; delete w['artifact-ids']; w.name = 'desktop-release-candidate'; }],
    ['candidate from another run', doc => { step(doc, 'sign', 'Fetch the signing candidate').with['run-id'] = '${{ needs.gate.outputs.run }}'; }],
    ['second download in sign', doc => { doc.jobs.sign.steps.splice(3, 0, { uses: step(doc, 'sign', 'Fetch the signing candidate').uses, with: { name: 'desktop-signed-release', path: '/tmp/x', 'digest-mismatch': 'error' } }); }],
    ['raw CI artifacts in publish', doc => { step(doc, 'publish', 'Fetch the signing candidate').with = { ...step(doc, 'collect', 'Fetch the accepted').with }; }],
  ]) reported({ [KEYED]: edit(change) }, label);
});

test('workflow_run.head_branch outside the gate env is reported', () => {
  reported({ [KEYED]: edit(doc => { step(doc, 'collect', 'Collect').env.TAG = '${{ github.event.workflow_run.head_branch }}'; }) }, 'collect env');
  reported({ [KEYED]: edit(doc => { doc.jobs.gate.outputs.tag = '${{ github.event.workflow_run.head_branch }}'; }) }, 'outputs');
  reported({ [KEYED]: edit(doc => { step(doc, 'publish', 'Publish the versioned').env.RUN = '${{ github.event.workflow_run.id }}'; }) }, 'run id');
  reported({ [KEYED]: edit(doc => { step(doc, 'gate', 'Gate').env.HINT_TAG = '${{ github.event.workflow_run.display_title }}'; }) }, 'other event field');
});

test('a ${{ }} in a run line, including a block scalar, is reported', () => {
  reported({ [KEYED]: edit(doc => { step(doc, 'collect', 'Collect').run = 'node x.mjs "${{ needs.gate.outputs.version }}"'; }) }, 'inline');
  reported({ [KEYED]: text => text.replace('        run: node packages/desktop/scripts/release-candidate.mjs verify "$RUNNER_TEMP/candidate"\n',
    '        run: |\n          node packages/desktop/scripts/release-candidate.mjs verify "$RUNNER_TEMP/candidate"\n          echo ${{ needs.gate.outputs.tag }}\n') }, 'block scalar');
  reported({ [REQUEST]: edit(doc => { doc.jobs.request.steps[0].run = 'echo "${{ github.ref_name }}"'; }) }, 'request');
});

test('an unpinned or third-party action, an action between download and sign, setup-node without package-manager-cache false, or npm in publish is reported', () => {
  for (const [label, change] of [
    ['unpinned', doc => { step(doc, 'sign', 'actions/checkout').uses = 'actions/checkout@v7'; }],
    ['other sha', doc => { step(doc, 'sign', 'actions/checkout').uses = 'actions/checkout@' + 'a'.repeat(40); }],
    ['third party', doc => { step(doc, 'publish', 'actions/setup-node').uses = 'someone/setup-node@' + 'a'.repeat(40); }],
    ['action between download and sign', doc => { doc.jobs.sign.steps.splice(3, 0, { uses: step(doc, 'sign', 'actions/setup-node').uses, with: { 'node-version-file': '.nvmrc', 'package-manager-cache': false } }); }],
    ['pages action in sign', doc => { doc.jobs.sign.steps.push({ uses: step(doc, 'feed', 'upload-pages-artifact').uses, with: { path: 'x' } }); }],
    ['package-manager-cache', doc => { delete step(doc, 'feed', 'actions/setup-node').with['package-manager-cache']; }],
    ['npm in publish', doc => { doc.jobs.publish.steps.splice(2, 0, { run: 'npm ci --ignore-scripts' }); }],
    ['npx in verify', doc => { step(doc, 'verify-published', 'Re-download').run = 'npx tsx packages/desktop/scripts/verify-published.mjs'; }],
    ['always()', doc => { doc.jobs.feed.if = 'always()'; }],
    ['|| true', doc => { step(doc, 'publish', 'Re-verify').run += ' || true'; }],
    ['candidate verify after sign', doc => { const s = doc.jobs.sign.steps; [s[3], s[4]] = [s[4], s[3]]; }],
    ['step order in collect', doc => { const s = doc.jobs.collect.steps; [s[5], s[6]] = [s[6], s[5]]; }],
    ['download check skipped', doc => { doc.jobs.collect.steps.splice(4, 1); }],
    ['digest verify after plan', doc => { const s = doc.jobs.publish.steps; s.push(s.splice(3, 1)[0]); }],
    ['candidate verify after plan', doc => { const s = doc.jobs.publish.steps; s.push(s.splice(5, 1)[0]); }],
  ]) reported({ [KEYED]: edit(change) }, label);
});

test('upload path other than runner.temp/signed is reported', () => {
  reported({ [KEYED]: edit(doc => { step(doc, 'sign', 'upload-artifact').with.path = '${{ runner.temp }}'; }) }, 'parent');
  reported({ [KEYED]: edit(doc => { step(doc, 'sign', 'upload-artifact').with.path = '${{ runner.temp }}/candidate'; }) }, 'candidate');
  reported({ [KEYED]: edit(doc => { step(doc, 'sign', 'upload-artifact').with.name = 'other'; }) }, 'name');
  reported({ [KEYED]: edit(doc => { step(doc, 'feed', 'upload-pages-artifact').with.path = '${{ runner.temp }}/signed'; }) }, 'pages path');
  reported({ [KEYED]: edit(doc => { step(doc, 'collect', 'Upload the signing candidate').with.path = '${{ runner.temp }}/downloaded'; }) }, 'raw downloads as the candidate');
  reported({ [KEYED]: edit(doc => { step(doc, 'gate', 'upload-artifact').with.path = '${{ runner.temp }}'; }) }, 'gate records path');
});

test('release-keys.mjs importing a non-node module or adding a tag policy is reported', () => {
  const keys = 'packages/desktop/scripts/release-keys.mjs', signer = 'packages/desktop/scripts/release-sign-job.mjs';
  reported({ [keys]: text => "import yaml from 'js-yaml';\n" + text }, 'package import');
  reported({ [keys]: text => text + "\nawait import('left-pad');\n" }, 'dynamic import');
  reported({ [keys]: text => text + "\nconst policy = { name: 'desktop-v*', type: 'tag' };\n" }, 'tag policy');
  reported({ [keys]: text => text + "\n// deployment-branch-policies\nconst policies = [{ name: 'main', type: 'branch' }, { name: 'dev', type: 'branch' }];\n" }, 'two branch policies');
  // Unconditional: removing the environment setup altogether (and with it the policy literal) is reported too.
  reported({ [keys]: text => text.replaceAll('deployment-branch-policies', 'policies').replace(/\{ name: 'main', type: 'branch' \}/g, '{}') }, 'no main policy');
  reported({ [signer]: text => "import { x } from 'https://example.com/x.mjs';\n" + text }, 'remote import');
  reported({ [signer]: text => "import { x } from '@dilla/other';\n" + text }, 'package import in signer');
});

test('other workflows naming the sign job script or chaining on desktop release request are reported', () => {
  reported({ '.github/workflows/release.yml': text => text + '\n# node packages/desktop/scripts/release-sign-job.mjs\n' }, 'sign script');
  reported({ '.github/workflows/sneaky.yml': () => "name: sneaky\non:\n  workflow_run:\n    workflows: ['desktop release request']\n    types: [completed]\npermissions: {}\njobs:\n  x:\n    runs-on: ubuntu-24.04\n    steps:\n      - run: echo\n" }, 'workflow_run chain');
});
