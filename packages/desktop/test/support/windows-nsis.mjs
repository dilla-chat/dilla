import { createRequire } from 'node:module';
import { readFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';
const require = createRequire(import.meta.url);
require('app-builder-lib');
const { getMakeNsisPath } = require('app-builder-lib/out/toolsets/windows');
const { NsisTarget } = require('app-builder-lib/out/targets/nsis/NsisTarget');
const { nsisEscapeString } = require('app-builder-lib/out/targets/nsis/nsisScriptGenerator');
const { CancellationToken } = require('builder-util-runtime');
const config = require('../../electron-builder.cjs');
const desktop = resolve(import.meta.dirname, '../..');
export async function compileWindowsNSIS({ directory, archive, productName = 'DillaFixture', appId = 'dev.dilla.fixture', version = '1.2.3', uninstallKey = appId, name = 'dilla-fixture.exe', transform = source => source }) {
 let counter = 0;
 const target = { isUnicodeEnabled: true, options: config.nsis, packager: { config,
  info: { cancellationToken: new CancellationToken(), buildResourcesDir: join(desktop, 'assets') }, getResource: async () => null, getTempFile: async () => join(directory, `${counter++}-messages.nsh`) } };
 const header = await NsisTarget.prototype.computeCommonInstallerScriptHeader.call(target);
 const tool = await getMakeNsisPath(config.toolsets?.nsis);
 const file = join(directory, name);
 const source = await readFile(join(desktop, config.nsis.script), 'utf8');
 const definitions = { PRODUCT_NAME: productName, APP_ID: appId, VERSION: version, UNINSTALL_APP_KEY: uninstallKey, ESTIMATED_SIZE: 1, APP_64: archive, DILLA_PROTOCOL_SCHEME: 'dilla-fixture-' + appId.replaceAll('.', '-') };
 const args = ['-WX', ...Object.entries(definitions).map(([key, value]) => `-D${key}=${nsisEscapeString(String(value))}`), '-XUnicode true', `-XOutFile "${file}"`, '-'];
 const output = spawnSync(tool.path, args, { input: header + transform(source), encoding: 'utf8', env: { ...process.env, ...tool.env } });
 if (output.status !== 0) throw new Error(output.stdout + output.stderr);
 return file;
}

/** Compiled acceptance diagnostics only: no shipping flag or logging path.
 * Store a static refusal-site ordinal and the initial guard's numeric status.
 * Never store NSIS registers that may contain paths or account data. */
export function withInitialInstallDiagnostics(source) {
 source = source.replaceAll('\r\n', '\n');
 const sites = [], wrappers = [], conditions = []; let current = '', preflights = 0;
 source = source.split('\n').map(line => {
  if (/^\s*!if/.test(line)) conditions.push(line.trim() === '!if "${Prefix}" != "un."');
  if (/^\s*!endif\s*$/.test(line)) conditions.pop();
  const declaration = /^Function ([^\s]+)/.exec(line); if (declaration) current = declaration[1].replace('${Prefix}', '');
  const refusal = /^(\s+)Call (\$\{Prefix\})?Fail$/.exec(line);
  if (refusal) {
   sites.push(current + ':refusal-' + (sites.length + 1));
   const name = 'FixtureRefusal' + sites.length;
   const prefixes = refusal[2] && !conditions.includes(true) ? ['', 'un.'] : [''];
   for (const prefix of prefixes) wrappers.push(`Function ${prefix}${name}\n  StrCpy $FixtureFailureSite ${sites.length}\n  Call ${prefix}Fail\nFunctionEnd\n`);
   return `${refusal[1]}Call ${refusal[2] ?? ''}${name}`;
  }
  if (/^\s+ExecWait .*--installer-preflight=initial.* \$0$/.test(line)) {
   const name = 'FixtureInitialPreflight' + (++preflights);
   wrappers.push(`Function ${name}\n${line}\n  StrCpy $FixtureNativeExit $0\nFunctionEnd\n`);
   return '  Call ' + name;
  }
  return line;
 }).join('\n');
 if (preflights !== 2 || sites.length < 10 || !source.includes('Function Fail\n') || !source.includes('Function .onInit\n') || !source.includes('Var DataRoot\n')) throw new Error('Initial install diagnostic boundary changed');
 source = source.replace('Var DataRoot\n', 'Var FixtureFailureSite\nVar FixtureNativeExit\nVar DataRoot\n')
  .replace('Function .onInit\n', 'Function .onInit\n  StrCpy $FixtureNativeExit "not-run"\n')
  .replace('Function Fail\n', `Function Fail
  Push $R9
  FileOpen $R9 "$EXEDIR\\fixture-install-diagnostic.json" w
  FileWrite $R9 '{"format":1,"site":$FixtureFailureSite,"nativeExit":"$FixtureNativeExit"}'
  FileClose $R9
  Pop $R9
`);
 // Original instruction positions stay one-for-one: relative +N branches must
 // skip the same operations. Diagnostic instructions live only in wrappers.
 return { source: source + '\n' + wrappers.join('\n'), sites: Object.freeze(sites) };
}
export function initialInstallDiagnostic(value, sites) {
 if (!value || typeof value !== 'object' || Array.isArray(value) || Object.keys(value).length !== 3 || value.format !== 1
  || !Number.isSafeInteger(value.site) || value.site < 1 || value.site > sites.length
  || typeof value.nativeExit !== 'string' || !/^(?:not-run|0|[1-9]\d{0,9})$/.test(value.nativeExit)
  || (value.nativeExit !== 'not-run' && Number(value.nativeExit) > 0xffffffff)) throw new Error('Invalid private initial install diagnostic');
 return { format: 1, phase: sites[value.site - 1], nativeExit: value.nativeExit === 'not-run' ? null : Number(value.nativeExit) };
}

export function withMaintenanceTimingFixture(source) {
 const held = source.replace('  Call un.HoldStableLauncher', [
  '  Call un.HoldStableLauncher',
  '  ReadEnvStr $0 "DILLA_FIXTURE_HOLD_ROOT"',
  '  StrCmp $0 "1" 0 +4',
  '    FileOpen $0 "$APPDATA\\${PRODUCT_NAME}\\fixture-hold-ready" w',
  '    FileClose $0',
  '    Sleep 300000',
 ].join('\n'));
 return held.replace('  Call un.MaintenancePreflight', '  Call un.MaintenancePreflight\n' + publication('DILLA_FIXTURE_PUBLISH_DURING_SCAN', 'd'.repeat(64)))
  .replace('  StrCpy $InitialRetry 1', '  StrCpy $InitialRetry 1\n' + publication('DILLA_FIXTURE_INITIAL_PUBLISH_DURING_SCAN', 'c'.repeat(64)));
}
function publication(flag, nonce) {
 return [
  `  ReadEnvStr $0 "${flag}"`,
  '  ${If} $0 == "1"',
  `    CreateDirectory "$DataRoot\\startup-${nonce}"`,
  `    FileOpen $0 "$DataRoot\\startup-${nonce}\\journal.json" w`,
  `    FileWrite $0 '{"format":1,"nonce":"${nonce}","phase":"prepared"}'`,
  '    FileClose $0',
  '    FileOpen $0 "$DataRoot\\active.json" w',
  `    FileWrite $0 '{"format":1,"nonce":"${nonce}"}'`,
  '    FileClose $0',
  '  ${EndIf}',
 ].join('\n');
}
