import { EventEmitter } from 'node:events';
import { PassThrough, Writable } from 'node:stream';

/**
 * An in-memory GitHub for release-keys.mjs, reached only through a fake
 * `child_process.spawn` of `gh`. It answers `gh api` and `gh secret set|delete`,
 * records every argv and stdin, and never touches the network.
 */
const R = 'repos/dilla-chat/dilla';
const ok = value => ({ status: 200, body: value });
const missing = { status: 404, body: { message: 'Not Found' } };

export function goodPosture() {
 return {
  user: { login: 'jonasthim' },
  repo: { full_name: 'dilla-chat/dilla', permissions: { admin: true, push: true } },
  rules: [
   { type: 'deletion', ruleset_id: 25709839 }, { type: 'non_fast_forward', ruleset_id: 25709839 },
   { type: 'pull_request', ruleset_id: 25709839, parameters: { required_approving_review_count: 0 } },
   { type: 'required_status_checks', ruleset_id: 25709839, parameters: { required_status_checks: [{ context: 'server-ok', integration_id: 15368 }, { context: 'desktop-ok', integration_id: 15368 }] } },
  ],
  rulesets: { 25709839: { id: 25709839, name: 'main', enforcement: 'active', bypass_actors: [{ actor_id: 5, actor_type: 'RepositoryRole', bypass_mode: 'always' }] } },
  collaborators: [{ login: 'jonasthim', role_name: 'admin', permissions: { admin: true, push: true } }],
  installations: { installations: [{ app_slug: 'claude', repository_selection: 'all' }] },
  workflow: { default_workflow_permissions: 'read', can_approve_pull_request_reviews: false },
  immutable: { enabled: true, enforced_by_owner: false },
  pages: { build_type: 'workflow', html_url: 'https://dilla-chat.github.io/dilla/' },
  pagesEnvironment: { name: 'github-pages', deployment_branch_policy: { protected_branches: false, custom_branch_policies: true } },
  pagesPolicies: [{ id: 9, name: 'main', type: 'branch' }],
  repoSecrets: ['CSC_LINK'],
  orgSecrets: [],
 };
}

export function fakeGh(overrides = {}) {
 const state = {
  ...goodPosture(), ...overrides,
  environment: overrides.environment ?? null,
  policies: overrides.policies ?? [],
  envSecrets: new Map(Object.entries(overrides.envSecrets ?? {})),
  ignorePolicyDelete: overrides.ignorePolicyDelete ?? false,
  failSecretSet: overrides.failSecretSet ?? null,
  // Published releases and tag -> commit, for rotate --retire's check of each channel's rotation release.
  releases: overrides.releases ?? [],
  tags: overrides.tags ?? {},
 };
 const calls = [];
 let nextPolicy = 100;
 function api(method, path, body) {
  const [route, query = ''] = path.split('?');
  if (method === 'GET' && route === `${R}/releases`) return ok(Number(new URLSearchParams(query).get('page') ?? 1) === 1 ? state.releases : []);
  const tagRef = new RegExp(`^${R}/git/ref/tags/(.+)$`).exec(route);
  if (method === 'GET' && tagRef) {
   const tag = decodeURIComponent(tagRef[1]);
   return state.tags[tag] ? ok({ ref: `refs/tags/${tag}`, object: { type: 'commit', sha: state.tags[tag] } }) : missing;
  }
  if (method === 'GET' && route === 'user') return ok(state.user);
  if (method === 'GET' && route === R) return ok(state.repo);
  if (method === 'GET' && route === `${R}/rules/branches/main`) return ok(state.rules);
  const ruleset = new RegExp(`^${R}/rulesets/(\\d+)$`).exec(route);
  if (method === 'GET' && ruleset) return state.rulesets[ruleset[1]] ? ok(state.rulesets[ruleset[1]]) : missing;
  if (method === 'GET' && route === `${R}/collaborators`) return ok(state.collaborators);
  if (method === 'GET' && route === 'orgs/dilla-chat/installations') return state.installations ? ok(state.installations) : { status: 403, body: { message: 'Forbidden' } };
  if (method === 'GET' && route === `${R}/actions/permissions/workflow`) return ok(state.workflow);
  if (method === 'GET' && route === `${R}/immutable-releases`) return state.immutable ? ok(state.immutable) : missing;
  if (method === 'GET' && route === `${R}/pages`) return state.pages ? ok(state.pages) : missing;
  if (method === 'GET' && route === `${R}/environments/github-pages`) return ok(state.pagesEnvironment);
  if (method === 'GET' && route === `${R}/environments/github-pages/deployment-branch-policies`) return ok({ total_count: state.pagesPolicies.length, branch_policies: state.pagesPolicies });
  if (method === 'GET' && route === `${R}/actions/secrets`) return ok({ total_count: state.repoSecrets.length, secrets: state.repoSecrets.map(name => ({ name })) });
  if (method === 'GET' && route === `${R}/actions/organization-secrets`) return state.orgSecrets === null ? { status: 403, body: { message: 'Forbidden' } } : ok({ total_count: state.orgSecrets.length, secrets: state.orgSecrets.map(name => ({ name })) });
  if (route === `${R}/environments/desktop-release`) {
   if (method === 'PUT') { state.environment = { name: 'desktop-release', can_admins_bypass: body.can_admins_bypass, deployment_branch_policy: body.deployment_branch_policy, protection_rules: [] }; return ok(state.environment); }
   if (method === 'GET') return state.environment ? ok(state.environment) : missing;
  }
  if (route === `${R}/environments/desktop-release/deployment-branch-policies`) {
   if (!state.environment) return missing;
   if (method === 'GET') return ok({ total_count: state.policies.length, branch_policies: state.policies });
   if (method === 'POST') { const policy = { id: nextPolicy++, name: body.name, type: body.type }; state.policies.push(policy); return ok(policy); }
  }
  const policy = new RegExp(`^${R}/environments/desktop-release/deployment-branch-policies/(\\d+)$`).exec(route);
  if (method === 'DELETE' && policy) { if (!state.ignorePolicyDelete) state.policies = state.policies.filter(p => String(p.id) !== policy[1]); return { status: 204, body: null }; }
  if (method === 'GET' && route === `${R}/environments/desktop-release/secrets`) return state.environment ? ok({ total_count: state.envSecrets.size, secrets: [...state.envSecrets.keys()].map(name => ({ name })) }) : missing;
  return missing;
 }
 function answer(argv, input) {
  if (argv[0] === 'api') {
   const method = argv[argv.indexOf('--method') + 1];
   const path = argv.find((arg, i) => i > 0 && !arg.startsWith('-') && argv[i - 1] !== '--method' && argv[i - 1] !== '-H' && argv[i - 1] !== '--input');
   const response = api(method, path, input.length ? JSON.parse(input.toString()) : undefined);
   if (response.status >= 400) return { code: 1, stdout: JSON.stringify(response.body), stderr: `gh: ${response.body.message} (HTTP ${response.status})` };
   return { code: 0, stdout: response.body === null ? '' : JSON.stringify(response.body), stderr: '' };
  }
  if (argv[0] === 'secret' && (argv[1] === 'set' || argv[1] === 'delete')) {
   const name = argv[2];
   if (argv[1] === 'set') {
    if (state.failSecretSet === name) return { code: 1, stdout: '', stderr: 'failed to set secret' };
    state.envSecrets.set(name, Buffer.from(input));
   } else state.envSecrets.delete(name);
   return { code: 0, stdout: '', stderr: '' };
  }
  return { code: 1, stdout: '', stderr: 'unknown gh command' };
 }
 function spawn(command, argv, options = {}) {
  const child = new EventEmitter();
  const chunks = [];
  const stdinMode = options.stdio?.[0];
  child.stdout = new PassThrough(); child.stderr = new PassThrough();
  const call = { command, argv: [...argv], stdio: options.stdio, stdin: Buffer.alloc(0) };
  calls.push(call);
  const finish = () => setImmediate(() => {
   call.stdin = Buffer.concat(chunks);
   const result = command === 'gh' ? answer(argv, call.stdin) : { code: 127, stdout: '', stderr: 'not gh' };
   child.stdout.end(result.stdout); child.stderr.end(result.stderr);
   setImmediate(() => child.emit('close', result.code));
  });
  if (stdinMode === 'pipe' || stdinMode === undefined) {
   child.stdin = new Writable({ write(chunk, _encoding, done) { chunks.push(Buffer.from(chunk)); done(); }, final(done) { finish(); done(); } });
  } else { child.stdin = null; finish(); }
  return child;
 }
 return { state, calls, spawn };
}
