import type { KeyObject } from 'node:crypto';
import type { sandboxReleaseFixture } from './linux-sandbox-release-fixture.ts';

export interface FixtureJob { name: string; conclusion: string; steps?: { name: string; conclusion: string }[] }
export interface FixtureAsset { name: string; size: number; sha512: string }
export interface FixtureTrust { metadataPublicKeys: string[]; sparklePublicKey: string }
export interface FixtureSource {
 format: number; ciRun: number; commit: string; head: string; tree: string;
 review: { status: string; ciRun: number; tree: string; inventory: { sha256: string } };
 builds: Record<string, { sha256: string }>; performance: Record<string, { sha256: string }>; installedAccounts: Record<string, { sha256: string }>; notes: { sha256: string };
}
export interface FixtureRelease {
 format: number; version: string; channel: string; publishedAt: string; expiresAt: string; source: FixtureSource; assets: FixtureAsset[];
 artifacts: Array<FixtureAsset & { platform: string; arch: string; url: string; sparkleSignature?: string; linuxSandbox?: { compatibilityEpoch: number; minimumGeneration: number; bundle: { url: string; size: number; sha512: string } } }>;
}
export interface FixtureContext { metadata: Buffer; signature: string; trust: FixtureTrust; directory: string; run: Record<string, unknown> & { id: number; head_sha: string }; jobs: FixtureJob[]; version: string; now: number }
export interface ReleaseFixture {
 directory: string; version: string; channel: string; sandbox: ReturnType<typeof sandboxReleaseFixture>;
 key: { publicKey: KeyObject; privateKey: KeyObject }; sparkle: { publicKey: KeyObject; privateKey: KeyObject };
 source: FixtureSource; notes: Buffer; jobs: FixtureJob[]; assets: FixtureAsset[]; recipeDirectory: string; recipeName: string;
 release: FixtureRelease; metadata: Buffer; signature: string; trust: FixtureTrust; run: FixtureContext['run']; context: FixtureContext;
 resign(changed: unknown): FixtureContext;
 files: Record<'installers' | 'performance' | 'installed-account', Record<string, string[]>>;
 writeArtifactLayout(downloadRoot: string): Promise<void>;
}
export const RELEASE_TOOL_FILES: readonly string[];
export function isolatedReleaseTools(directory: string, options: { trust: FixtureTrust; sandboxConfig: unknown; files?: string[] }): Promise<{ verifier: any; signer: any }>;
export function acceptedJobs(): FixtureJob[];
export function buildReleaseFixture(options?: { version?: string; directory?: string; now?: number; commit?: string; head?: string; tree?: string; sandboxDates?: { publishedAt?: string; expiresAt?: string } }): Promise<ReleaseFixture>;
export function currentSandboxDates(now?: number): { publishedAt: string; expiresAt: string };
