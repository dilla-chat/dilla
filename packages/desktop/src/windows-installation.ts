import { createHash } from 'node:crypto';
import { dirname, join, basename } from 'node:path';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { updateFS } from './update-fs.ts';
import { strictWindowsJSON, canonicalSHA512, verifyWindowsProgramManifest, verifyWindowsProgramTree, windowsProgramID } from './windows-manifest.ts';
import { parseWindowsSlot, verifyWindowsSlot, sameWindowsSlot, selectWindowsSlot, readWindowsRecord, type WindowsSlot, type WindowsSlotTrust } from './windows-slots.ts';
const execute = promisify(execFile);
export interface WindowsInstallation {
    root: string;
    launcher: string;
    oldSlot: WindowsSlot;
    initialVersion: string;
    initialManifestSha512: string;
}
interface BootstrapDescription {
    format: 1;
    appId: string;
    productName: string;
    channel: 'stable' | 'beta';
    arch: 'x64';
    initialVersion: string;
    initialManifestSha512: string;
    trustHash: string;
}
export async function verifyWindowsProgram(root: string, slot: WindowsSlot, context: {
    channel: 'stable' | 'beta';
    arch: string;
}): Promise<string> {
    if (context.arch !== 'x64')
        throw new Error('Unsupported Windows program architecture');
    const file = join(root, 'manifests', Buffer.from(slot.manifestSha512, 'base64').toString('hex') + '.json'), raw = await readWindowsRecord(file, 1048576);
    const manifest = verifyWindowsProgramManifest(raw, slot.manifestSha512, { version: slot.version, channel: context.channel, arch: 'x64' }), directory = join(root, 'versions', windowsProgramID(slot.manifestSha512));
    if (!manifest.files.some(file => file.path === 'dilla-desktop.exe' && file.size > 0) || !manifest.files.some(file => file.path === 'resources/app.asar' && file.size > 0))
        throw new Error('Missing Windows executable/ASAR authority');
    await verifyWindowsProgramTree(directory, manifest);
    return join(directory, 'dilla-desktop.exe');
}
export async function readWindowsSlots(root: string, trust: WindowsSlotTrust, provenance: string): Promise<WindowsSlot> {
    const slots: WindowsSlot[] = [];
    for (const index of [0, 1]) {
        const file = join(provenance, `windows-active-${index}.json`), entry = await updateFS.promises.lstat(file).catch((error: unknown) => { if ((error as NodeJS.ErrnoException).code === 'ENOENT')
            return null; throw error; });
        if (entry === null)
            continue;
        // Once a slot is present, every read/authentication uncertainty holds startup.
        // Silently dropping it could open older code after accounts already migrated.
        const slot = parseWindowsSlot(await readWindowsRecord(file, 1024));
        await verifyWindowsSlot(slot, trust, provenance);
        slots.push(slot);
    }
    if (!slots.length)
        throw new Error('No authenticated Windows program slot; retry or reinstall Dilla');
    const selected = selectWindowsSlot(slots);
    await verifyWindowsProgram(root, selected, trust);
    return selected;
}
/** Only the fixed compiled native launcher supplies its initial trust point.
 * It independently pins owner/DACL/canonical roots before exposing --state. */
export async function currentWindowsInstallation(userData: string, executable: string, context: {
    publicKeys: readonly string[];
    version: string;
    channel: 'stable' | 'beta';
    arch: string;
    productName: string;
}): Promise<WindowsInstallation> {
    if (process.platform !== 'win32' || basename(executable) !== 'dilla-desktop.exe')
        throw new Error('Windows updates require the stable installed launcher');
    const program = dirname(executable), versions = dirname(program), root = dirname(versions), launcher = join(root, 'Dilla.exe');
    if (basename(versions) !== 'versions' || !/^v-[a-f0-9]{32}$/.test(basename(program)) || await updateFS.promises.realpath(root) !== root || await updateFS.promises.realpath(launcher) !== launcher)
        throw new Error('Invalid Windows stable program root');
    const description = await describeWindowsLauncher(launcher, context);
    if (!description || Object.keys(description).length !== 8 || description.format !== 1 || description.appId !== 'dev.dilla.desktop' || description.productName !== context.productName || description.channel !== context.channel || description.arch !== context.arch
        || !canonicalSHA512(description.initialManifestSha512) || description.trustHash !== createHash('sha256').update(JSON.stringify(context.publicKeys)).digest('hex'))
        throw new Error('Windows bootstrap identity or pinned trust differs');
    const trust = { ...context, initialVersion: description.initialVersion, initialManifestSha512: description.initialManifestSha512 }, oldSlot = await readWindowsSlots(root, trust, join(userData, 'updates'));
    const reported = strictWindowsJSON(Buffer.from((await execute(launcher, ['--state'], { encoding: 'utf8', timeout: 30000, maxBuffer: 4096, windowsHide: true })).stdout)) as Record<string, unknown>;
    const status = reported.status;
    delete reported.status;
    const reportedSlot = parseWindowsSlot(Buffer.from(JSON.stringify(reported)));
    if (!['committed', 'pending'].includes(String(status)) || !sameWindowsSlot(reportedSlot, oldSlot))
        throw new Error('Native Windows selected slot differs');
    if (oldSlot.version !== context.version || await verifyWindowsProgram(root, oldSlot, trust) !== executable)
        throw new Error('Running Windows version differs from authenticated active slot');
    return { root, launcher, oldSlot, initialVersion: description.initialVersion, initialManifestSha512: description.initialManifestSha512 };
}
async function describeWindowsLauncher(launcher: string, context: {
    publicKeys: readonly string[];
    channel: 'stable' | 'beta';
    arch: string;
}): Promise<BootstrapDescription> {
    const result = await execute(launcher, ['--describe'], { encoding: 'utf8', timeout: 10000, maxBuffer: 4096, windowsHide: true }), description = strictWindowsJSON(Buffer.from(result.stdout)) as BootstrapDescription;
    if (!description || Object.keys(description).length !== 8 || description.format !== 1 || description.appId !== 'dev.dilla.desktop' || description.productName !== basename(dirname(launcher)) || description.channel !== context.channel || description.arch !== context.arch
        || !canonicalSHA512(description.initialManifestSha512) || description.trustHash !== createHash('sha256').update(JSON.stringify(context.publicKeys)).digest('hex'))
        throw new Error('Windows bootstrap identity or pinned trust differs');
    return description;
}
export async function verifyWindowsInstallationSnapshot(windows: WindowsInstallation, userData: string, installed: string, context: {
    publicKeys: readonly string[];
    channel: 'stable' | 'beta';
    arch: string;
}): Promise<void> {
    const root = dirname(dirname(dirname(installed)));
    if (windows.root !== root || windows.launcher !== join(root, 'Dilla.exe') || dirname(dirname(installed)) !== join(root, 'versions'))
        throw new Error('Invalid Windows snapshot root');
    const description = await describeWindowsLauncher(windows.launcher, context);
    if (description.initialVersion !== windows.initialVersion || description.initialManifestSha512 !== windows.initialManifestSha512 || basename(userData) !== description.productName)
        throw new Error('Windows snapshot differs from compiled initial trust');
    await verifyWindowsSlot(windows.oldSlot, { ...context, initialVersion: windows.initialVersion, initialManifestSha512: windows.initialManifestSha512 }, join(userData, 'updates'));
    if (await verifyWindowsProgram(root, windows.oldSlot, context) !== installed)
        throw new Error('Windows old slot executable differs');
}
