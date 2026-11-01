import { join } from 'node:path';
import { updateFS } from './update-fs.ts';
import { readWindowsRecord } from './windows-slots.ts';
interface DiagnosticPlan {
    health: {
        directory: string;
        nonce: string;
        version: string;
    };
}
export async function recordWindowsRegistrationWarning(plan: DiagnosticPlan): Promise<void> {
    const file = join(plan.health.directory, 'registration-warning.json'), record = { format: 1, kind: 'uninstall-metadata', nonce: plan.health.nonce, version: plan.health.version };
    try {
        const handle = await updateFS.promises.open(file, 'wx', 0o600);
        try {
            await handle.writeFile(JSON.stringify(record));
            await handle.sync();
        }
        finally {
            await handle.close();
        }
    }
    catch { /* Never undo a committed program for a metadata/diagnostic write failure. */ }
}
export async function windowsRegistrationWarning(plan: DiagnosticPlan): Promise<boolean> {
    try {
        const value = JSON.parse((await readWindowsRecord(join(plan.health.directory, 'registration-warning.json'), 512)).toString()) as Record<string, unknown>;
        return !!value && Object.keys(value).length === 4 && value.format === 1 && value.kind === 'uninstall-metadata' && value.nonce === plan.health.nonce && value.version === plan.health.version;
    }
    catch (error) {
        if ((error as NodeJS.ErrnoException).code === 'ENOENT')
            return false;
        return true;
    }
}
