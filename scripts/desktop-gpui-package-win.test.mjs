// The Windows installer's static facts (GPUI release plan, Task 13), checkable without Windows: beta.5's uninstall key,
// the AppUserModelID against the app's APP_ID, the plan's Inno settings and the script's guards. The install, takeover,
// uninstall and crt-static runs happen on windows-latest in desktop-gpui.yml's gpui-win-package job.
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const read = (rel) => readFileSync(join(root, rel), 'utf8');
const iss = read('clients/desktop/packaging/windows/dilla.iss');
const ps1 = read('scripts/desktop-gpui-package-win.ps1');

/** electron-builder's UUID v5 (app-builder-lib NsisTarget.js: UUID.v5(appInfo.id, ELECTRON_BUILDER_NS_UUID)). */
function uuidV5(name, namespace) {
  const hash = createHash('sha1').update(Buffer.from(namespace.replace(/-/g, ''), 'hex')).update(name).digest();
  hash[6] = (hash[6] & 0x0f) | 0x50;
  hash[8] = (hash[8] & 0x3f) | 0x80;
  const x = hash.subarray(0, 16).toString('hex');
  return `${x.slice(0, 8)}-${x.slice(8, 12)}-${x.slice(12, 16)}-${x.slice(16, 20)}-${x.slice(20, 32)}`;
}

test("beta.5's uninstall key is electron-builder's UUID v5 of dev.dilla.desktop, and dilla.iss uses it", () => {
  // beta.5's electron-builder.cjs: appId 'dev.dilla.desktop', no nsis.guid override.
  const guid = uuidV5('dev.dilla.desktop', '50e065bc-3134-11e6-9bab-38c9862bdaf3');
  assert.equal(guid, 'f6db60e4-10df-5b69-9206-408d5f2cebe9');
  assert.ok(iss.includes(`Beta5Guid = '${guid}'`), 'dilla.iss reads the uninstall entry of that GUID');
  assert.ok(iss.includes("Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\"), 'under the per-user Uninstall key');
  assert.match(iss, /RegQueryStringValue\(HKCU, UninstallRoot \+ Beta5Guid, 'UninstallString'/);
});

test("the shortcut's AppUserModelID is the app's APP_ID", () => {
  const appId = /pub const APP_ID: &str = "([^"]+)";/.exec(read('clients/desktop/dilla-desktop/src/lib.rs'))?.[1];
  assert.equal(appId, 'dev.dilla.desktop');
  assert.ok(iss.includes(`#define AppId "${appId}"`));
  assert.match(iss, /AppUserModelID: "\{#AppId\}"/);
  // notifier::install passes crate::APP_ID to the platform as the Windows AppUserModelID.
  assert.match(read('clients/desktop/dilla-desktop/src/native/notifier.rs'), /set_app_identity\(crate::APP_ID, crate::PRODUCT_NAME\)/);
});

test('dilla.iss is per user, silent-capable, takes over beta.5 and registers the dilla: protocol', () => {
  assert.match(iss, /^PrivilegesRequired=lowest$/m);
  assert.match(iss, /^DefaultDirName=\{localappdata\}\\Programs\\Dilla$/m);
  assert.match(iss, /^OutputBaseFilename=dilla-\{#AppVersion\}-win-x64-setup$/m);
  assert.match(iss, /function PrepareToInstall/);
  assert.match(iss, /'\/S \/KEEP_APP_DATA \/currentuser --updated _\?=' \+ Dir/, "electron-builder's own old-uninstaller call");
  const reg = iss.replace(/^;.*$/gm, '');
  assert.match(reg, /Subkey: "Software\\Classes\\dilla"; ValueType: string; ValueName: ""; ValueData: "URL:Dilla"; Flags: uninsdeletekey/, 'the scheme key is removed on uninstall');
  assert.match(reg, /Subkey: "Software\\Classes\\dilla"; ValueType: string; ValueName: "URL Protocol"; ValueData: ""/);
  assert.match(reg, /Subkey: "Software\\Classes\\dilla\\DefaultIcon"; ValueType: string; ValueName: ""; ValueData: "\{app\}\\\{#Exe\},0"/);
  assert.match(reg, /Subkey: "Software\\Classes\\dilla\\shell\\open\\command"; ValueType: string; ValueName: ""; ValueData: """\{app\}\\\{#Exe\}"" ""%1"""/, 'the link is the one argument');
  assert.ok(!/Root: HKLM/.test(reg), 'per user: nothing under HKLM');
  assert.ok(!/AppData|APPDATA|Roaming/.test(iss.replace(/^;.*$/gm, '').replace(/\/\/.*$/gm, '')), 'user data is never touched');
  assert.match(iss, /ValueName: "Dilla"; Flags: dontcreatekey uninsdeletevalue/, "the app's autostart value goes with the app");
});

test('the signtool hook exists and is off unless DILLA_SIGNTOOL_COMMAND is set', () => {
  assert.match(iss, /#ifdef WithSigning\s*[\s\S]*SignTool=dilla_sign[\s\S]*#endif/);
  assert.match(ps1, /if \(\$env:DILLA_SIGNTOOL_COMMAND\) \{ \$isccArgs \+= @\('\/DWithSigning'/);
});

test('the script guards the version, the release binary, the static CRT, --version and the output', () => {
  assert.match(ps1, /-alpha\\\.\(0\|\[1-9\]\\d\*\)\$'\) \{ Fail "unsupported release version/);
  assert.ok(ps1.includes('check-desktop-release-binary.sh'));
  assert.ok(ps1.includes('/dependents') && ps1.includes('VCRUNTIME'));
  assert.ok(ps1.includes('--version') && ps1.includes('dilla-$Version-win-x64-setup.exe'));
  assert.ok(ps1.includes("-notmatch '^6\\.'"), 'iscc must be Inno Setup 6');
  assert.ok(ps1.includes('x86_64-pc-windows-msvc'), 'the licence bundle lists the Windows crates');
});
