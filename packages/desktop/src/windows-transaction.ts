import { updateUninstallVersion } from './update-registry.ts';
import { recordWindowsRegistrationWarning } from './windows-diagnostic.ts';
import { updateFS } from './update-fs.ts';
import { parseWindowsSlot } from './windows-slots.ts';
import { readWindowsRecord } from './windows-slots.ts';
import { verifyWindowsProgramManifest } from './windows-manifest.ts';
import { join } from 'node:path';
import { verifyUpdatedWindowsProgram, type InstalledUpdatePlan } from './update-plan.ts';
import { publishWindowsSlot, verifyWindowsSlot, sameWindowsSlot, type WindowsSlot } from './windows-slots.ts';
import { readWindowsSlots, verifyWindowsProgram } from './windows-installation.ts';
function releaseSlot(plan: InstalledUpdatePlan): WindowsSlot {
    const artifact = plan.release.artifacts.find(file => file.platform === 'win32' && file.arch === plan.arch);
    if (plan.platform !== 'win32' || !plan.windows || !artifact?.programManifest || plan.windows.oldSlot.generation >= Number.MAX_SAFE_INTEGER)
        throw new Error('Invalid Windows commit authority');
    return { format: 1, kind: 'release', generation: plan.windows.oldSlot.generation + 1, version: plan.health.version, manifestSha512: artifact.programManifest.sha512, nonce: plan.health.nonce, acceptedAt: plan.verifiedAt };
}
export async function commitWindowsUpdate(plan: InstalledUpdatePlan, publicKeys: readonly string[]): Promise<void> {
    const slot = releaseSlot(plan);
    await verifyUpdatedWindowsProgram(plan);
    await verifyWindowsSlot(slot, { publicKeys, channel: plan.release.channel, arch: plan.arch, initialVersion: plan.windows!.initialVersion, initialManifestSha512: plan.windows!.initialManifestSha512 }, join(plan.userData, 'updates'));
    await publishWindowsSlot(join(plan.userData, 'updates'), slot);
    // Past the one-way slot publication: no throwing metadata work may request rollback.
    try {
        if (!plan.uninstall)
            throw new Error('Missing owned Windows registration');
        const manifest = verifyWindowsProgramManifest(await readWindowsRecord(join(plan.health.directory, 'program-manifest.json'), 1048576), slot.manifestSha512, { version: slot.version, channel: plan.release.channel, arch: 'x64' });
        await updateUninstallVersion(plan.windows!.root, plan.uninstall, slot.version, manifest.files.reduce((sum, file) => sum + file.size, 0));
    }
    catch {
        await recordWindowsRegistrationWarning(plan);
    }
}
export async function committedWindowsUpdate(plan: InstalledUpdatePlan, publicKeys: readonly string[]): Promise<boolean> {
    if (!plan.windows)
        throw new Error('Missing Windows stable program authority');
    const trust = { publicKeys, channel: plan.release.channel, arch: plan.arch, initialVersion: plan.windows.initialVersion, initialManifestSha512: plan.windows.initialManifestSha512 }, provenance = join(plan.userData, 'updates'), next = releaseSlot(plan), file = join(provenance, `windows-active-${next.generation % 2}.json`);
    // Slot metadata visibility is the one-way commit point. A present expected
    // release that later fails tree/read verification is uncertainty after commit,
    // never evidence that publishing did not happen or permission to open old stores.
    const entry = await updateFS.promises.lstat(file).catch((error: unknown) => { if ((error as NodeJS.ErrnoException).code === 'ENOENT')
        return null; throw error; });
    if (entry !== null) {
        const visible = parseWindowsSlot(await readWindowsRecord(file, 1024));
        if (sameWindowsSlot(visible, next)) {
            await verifyWindowsSlot(visible, trust, provenance);
            await verifyUpdatedWindowsProgram(plan);
            return true;
        }
        if (visible.generation >= next.generation)
            throw new Error('Windows publication is present but uncertain; do not restore old');
        await verifyWindowsSlot(visible, trust, provenance);
        await verifyWindowsProgram(plan.windows.root, visible, trust);
    }
    const selected = await readWindowsSlots(plan.windows.root, trust, provenance);
    if (!sameWindowsSlot(selected, plan.windows.oldSlot))
        throw new Error('Windows committed program changed during update');
    return false;
}
