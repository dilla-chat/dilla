/** Test-only: Windows cannot provide Linux uid/mode evidence for synthetic
 * Linux update plans. Keep all other cache and authentication checks intact. */
export function fixtureCacheSource(source: string, host: NodeJS.Platform): string {
 if (host !== 'win32') return source;
 const boundary = 's.uid !== process.getuid?.() || (s.mode & 0o077) !== 0';
 if (source.split(boundary).length !== 3) throw new Error('Fixture cache ownership boundary changed');
 return source.replaceAll(boundary, 'false');
}
