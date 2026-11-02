import test from 'node:test';
import assert from 'node:assert/strict';
import { resolveLinuxInstallation, assertLinuxInstallationRole } from '../src/linux-installation.ts';

const native = { installKind: 'system-package' as const, version: '0.1.0', channel: 'stable' as const, arch: 'x64', executable: '/opt/Dilla/dilla-desktop', resourcesPath: '/opt/Dilla/resources' };
test('stock native layout starts without a custom receipt and remains check-only', async () => {
 const installation = await resolveLinuxInstallation(native);
 assert.deepEqual(installation, { installKind: 'system-package' });
 assertLinuxInstallationRole(installation, 'check');
 for (const role of ['download', 'install', 'recovery', 'watchdog', 'sandbox-setup'] as const) assert.throws(() => assertLinuxInstallationRole(installation, role), /package manager/);
});
test('compiled installation kind rejects conflicting layout and unknown authority', async () => {
 for (const context of [{ ...native, installKind: 'unknown' as const }, { ...native, executable: '/tmp/.mount-dilla/dilla-desktop' }, { ...native, resourcesPath: '/tmp/resources' }, { ...native, installKind: 'appimage' as const }]) await assert.rejects(resolveLinuxInstallation(context), /installation|layout/);
 assert.deepEqual(await resolveLinuxInstallation({ ...native, installKind: 'appimage', executable: '/tmp/.mount-dilla/dilla-desktop', resourcesPath: '/tmp/.mount-dilla/resources' }), { installKind: 'appimage' });
});
