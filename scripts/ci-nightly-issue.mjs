import { realpathSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

/**
 * desktop-gpui's nightly report: a red nightly opens (or comments on) one tracking issue, a green one comments and closes
 * it. The release gate (CI speed plan, Task 5) refuses while an open issue with this title and label exists.
 *
 * Usage: GH_TOKEN=... GITHUB_REPOSITORY=owner/name node scripts/ci-nightly-issue.mjs <success|failure|cancelled> <run url>
 *        GH_TOKEN=... GITHUB_REPOSITORY=owner/name node scripts/ci-nightly-issue.mjs --needs <result>... -- <run url>
 */
export const ISSUE_TITLE = 'nightly desktop-gpui red';
export const ISSUE_LABEL = 'ci-nightly';
const API_ORIGIN = 'https://api.github.com';

/**
 * The workflow's conclusion from its jobs' `needs.*.result` values: any failure or cancellation is a failure, and so is a
 * run where nothing succeeded (all skipped, or no jobs): a nightly that proved nothing is not green.
 */
export const conclusionOf = (results) => (results.some((r) => r === 'failure' || r === 'cancelled') || !results.includes('success') ? 'failure' : 'success');

/** One GitHub API call. Redirects are refused and the token is never logged. A 4xx/5xx is an error unless its status is in `tolerate`. */
async function call({ token, fetchImpl, method = 'GET', path, body, tolerate = [] }) {
  const response = await fetchImpl(new URL(path, API_ORIGIN), {
    method,
    headers: { Authorization: `Bearer ${token}`, Accept: 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28', ...(body ? { 'Content-Type': 'application/json' } : {}) },
    body: body ? JSON.stringify(body) : undefined,
    redirect: 'error',
    signal: AbortSignal.timeout(30000),
  });
  if (!response.ok && !tolerate.includes(response.status)) throw new Error(`GitHub API ${method} ${path.split('?')[0]} failed (${response.status})`);
  return response.ok ? response.json() : null;
}

/** Opens, comments on or closes the tracking issue for this nightly's conclusion; resolves to what it did. */
export async function report({ conclusion, runUrl, token, repo, fetchImpl = fetch }) {
  const api = (options) => call({ token, fetchImpl, ...options });
  const issues = await api({ path: `/repos/${repo}/issues?labels=${ISSUE_LABEL}&state=open&per_page=100` });
  const existing = issues.find((i) => i.title === ISSUE_TITLE && !i.pull_request);
  if (conclusion === 'success') {
    if (!existing) return 'none';
    await api({ method: 'POST', path: `/repos/${repo}/issues/${existing.number}/comments`, body: { body: `The nightly is green again: ${runUrl}` } });
    await api({ method: 'PATCH', path: `/repos/${repo}/issues/${existing.number}`, body: { state: 'closed', state_reason: 'completed' } });
    return 'closed';
  }
  if (existing) {
    await api({ method: 'POST', path: `/repos/${repo}/issues/${existing.number}/comments`, body: { body: `The nightly is still red: ${runUrl}` } });
    return 'commented';
  }
  // The first red nightly must not fail on a missing label; 422 is "already_exists".
  await api({ method: 'POST', path: `/repos/${repo}/labels`, body: { name: ISSUE_LABEL, color: 'b60205', description: 'desktop-gpui nightly failed' }, tolerate: [422] });
  await api({ method: 'POST', path: `/repos/${repo}/issues`, body: { title: ISSUE_TITLE, labels: [ISSUE_LABEL], body: `The desktop-gpui nightly is red: ${runUrl}\n\nThis issue closes itself when a nightly goes green. While it is open, the desktop-gpui release gate refuses.` } });
  return 'opened';
}

const isMain = (() => {
  try { return realpathSync(process.argv[1]) === realpathSync(fileURLToPath(import.meta.url)); } catch { return false; }
})();

if (isMain) {
  const args = process.argv.slice(2);
  let conclusion;
  let runUrl;
  if (args[0] === '--needs') {
    const dash = args.indexOf('--');
    conclusion = conclusionOf(args.slice(1, dash));
    runUrl = args[dash + 1];
  } else [conclusion, runUrl] = args;
  const { GH_TOKEN: token, GITHUB_REPOSITORY: repo } = process.env;
  if (!['success', 'failure', 'cancelled'].includes(conclusion) || !runUrl || !token || !repo) {
    console.error('usage: GH_TOKEN=... GITHUB_REPOSITORY=owner/name node scripts/ci-nightly-issue.mjs <success|failure|cancelled> <run url>');
    process.exit(2);
  }
  try {
    console.log(await report({ conclusion, runUrl, token, repo, fetchImpl: fetch }));
  } catch (error) {
    console.error(error.message);
    process.exit(1);
  }
}
