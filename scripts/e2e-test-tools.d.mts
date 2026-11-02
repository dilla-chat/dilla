export interface TestToolPaths { testhost: string; testkit: string; bot?: string; botHelper?: string }
export interface TestToolOptions { repoRoot?: string; env?: NodeJS.ProcessEnv; platform?: string; arch?: string }
export function verifyTestTools(root: string, options?: TestToolOptions): TestToolPaths;
export function buildTestTools(root: string, options?: TestToolOptions): TestToolPaths;
