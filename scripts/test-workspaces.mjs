import { execFileSync } from 'node:child_process';

// npm's multi-workspace runner reports failures but continues to the next workspace.
// Ask npm for its ordered workspace metadata, then let each individual test retain its lifecycle hooks.
const npmCLI = process.env.npm_execpath;
if (!npmCLI) throw new Error('Run workspace tests through npm test so npm_execpath identifies the active npm CLI');

try {
  const scripts = JSON.parse(execFileSync(process.execPath, [npmCLI, 'pkg', 'get', 'scripts.test', '--workspaces', '--json'],
    { encoding: 'utf8', stdio: ['ignore', 'pipe', 'inherit'] }));
  for (const [workspace, script] of Object.entries(scripts)) {
    // Missing scripts are returned as {}, while an explicitly empty test still has pre/post hooks.
    if (typeof script !== 'string') continue;
    // Invoke npm's JS entrypoint through Node: npm.cmd cannot be execFileSync'ed directly on Windows.
    execFileSync(process.execPath, [npmCLI, 'test', `--workspace=${workspace}`], { stdio: 'inherit' });
  }
} catch (error) {
  if (typeof error.status !== 'number') throw error;
  process.exitCode = error.status || 1;
}
