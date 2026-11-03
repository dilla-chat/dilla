export interface FakeAsset { id: number; name: string; size: number; digest: string; state: string; bytes: Buffer; browser_download_url: string }
export interface FakeRelease { id: number; tag_name: string; name: string; body: string; draft: boolean; prerelease: boolean; target_commitish: string; immutable: boolean; upload_url: string; assets: FakeAsset[] }
export interface FakeLogEntry { method: string; url: string; redirect?: string; body?: any }
export interface FakeState { tags: Record<string, string>; runs: Record<string, unknown>; jobs: Record<string, unknown[]>; releases: FakeRelease[]; immutable: boolean; log: FakeLogEntry[]; onPublish?: (state: FakeState, release: FakeRelease) => unknown }
export interface FakeGithub {
 fetch: typeof fetch;
 state: FakeState;
 addRelease(fields: { tag_name: string; name?: string; body?: string; draft?: boolean; prerelease?: boolean; target_commitish?: string; immutable?: boolean; files?: Record<string, string | Buffer> }): FakeRelease;
 addAsset(release: FakeRelease, name: string, bytes: Buffer): FakeAsset;
 sha256(bytes: Uint8Array): string;
}
export function fakeGithub(options?: { tags?: Record<string, string>; runs?: Record<string, unknown>; jobs?: Record<string, unknown[]>; releases?: Parameters<FakeGithub['addRelease']>[0][]; immutable?: boolean; onPublish?: FakeState['onPublish'] }): FakeGithub;
