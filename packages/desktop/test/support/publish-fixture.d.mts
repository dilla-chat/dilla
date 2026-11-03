import type { KeyObject } from 'node:crypto';
import type { ReleaseFixture } from './release-fixture.d.mts';
import type { FakeGithub, FakeState } from './fake-github.d.mts';

export const SHA: string;
export const TREE: string;
export interface PublishFixture {
 root: string; fixture: ReleaseFixture; version: string; channel: string; tag: string; sha: string; tree: string;
 candidate: string; installers: string; tools: { verifier: any; signer: any }; github: FakeGithub;
 env: Record<string, string>; git: (args: string[]) => string; ancestors: Set<string>; now: number; cleanup(): Promise<void>;
 stagedKeys?: StagedKeys;
}
type Pair = { publicKey: KeyObject; privateKey: KeyObject };
export interface StagedKeys { metadata: Pair; sandbox: Pair; sparkle: Pair }
export function rotationProof(options: { keys: StagedKeys; version: string; channel: string; metadata: Buffer; manifest: Buffer; archive: Buffer; slot?: string }): Buffer;
export function publishFixture(options?: { sha?: string; commit?: string; tree?: string; staged?: boolean; immutable?: boolean; onPublish?: FakeState['onPublish']; releases?: Parameters<FakeGithub['addRelease']>[0][] }): Promise<PublishFixture>;
