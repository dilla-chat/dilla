import { join, dirname } from 'node:path';
import { updateFS, syncUpdateDirectory } from './update-fs.ts';
import { canonicalSHA512, windowsProgramID } from './windows-manifest.ts';
import type { WindowsSlot } from './windows-slots.ts';
const { lstat, realpath, readdir, rename } = updateFS.promises;
/** Preserve a partial previous attempt for inspection. Only the authenticated
 * inactive digest namespace is movable; no content is deleted or overwritten. */
export async function quarantineInactiveWindowsProgram(root: string, old: WindowsSlot, targetDigest: string, nonce: string): Promise<void> {
    if (!canonicalSHA512(targetDigest) || !/^[a-f0-9]{64}$/.test(nonce) || windowsProgramID(old.manifestSha512) === windowsProgramID(targetDigest) || await realpath(root) !== root)
        throw new Error('Invalid inactive Windows target');
    const versions = join(root, 'versions'), manifests = join(root, 'manifests');
    for (const directory of [versions, manifests]) {
        const info = await lstat(directory);
        if (!info.isDirectory() || info.isSymbolicLink() || await realpath(directory) !== directory)
            throw new Error('Invalid Windows inactive parent');
    }
    let count = 0;
    const check = async (file: string, depth = 0): Promise<void> => { const info = await lstat(file); if (++count > 32768 || depth > 64 || info.isSymbolicLink() || (!info.isFile() && !info.isDirectory()) || (info.isFile() && info.nlink !== 1) || await realpath(file) !== file)
        throw new Error('Invalid inactive Windows tree'); if (info.isDirectory())
        for (const name of await readdir(file))
            await check(join(file, name), depth + 1); };
    const program = join(versions, windowsProgramID(targetDigest)), manifest = join(manifests, Buffer.from(targetDigest, 'base64').toString('hex') + '.json');
    for (const file of [program, manifest]) {
        try {
            await lstat(file);
        }
        catch (error) {
            if ((error as NodeJS.ErrnoException).code === 'ENOENT')
                continue;
            throw error;
        }
        await check(file);
        const quarantine = file + '.failed-' + nonce;
        try {
            await lstat(quarantine);
            throw new Error('Windows inactive quarantine already exists');
        }
        catch (error) {
            if ((error as NodeJS.ErrnoException).code !== 'ENOENT')
                throw error;
        }
        await rename(file, quarantine);
        await syncUpdateDirectory(dirname(file));
    }
}
