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
