import * as nodePath from 'node:path';
import { readdir } from 'node:fs/promises';

/** Converts one OS-native relative path to the '/'-separated form that site layouts and repo paths use. `pathModule` is injectable so a Linux run can simulate win32. */
export const toPosix = (native, pathModule = nodePath) => native.split(pathModule.sep).join('/');

/** `file` relative to `root`, always '/'-separated. */
export const posixRelative = (root, file, pathModule = nodePath) => toPosix(pathModule.relative(root, file), pathModule);

/** Sorted recursive listing of a directory with '/'-separated entries (readdir's recursive entries use the OS separator). */
export async function posixTree(directory, { pathModule = nodePath, list = dir => readdir(dir, { recursive: true }) } = {}) {
 return (await list(directory)).map(entry => toPosix(entry, pathModule)).sort();
}
