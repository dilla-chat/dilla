import { open, rm } from 'node:fs/promises';
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { REPOSITORY, createGithubApi } from './ci-run.mjs';

/** Release notes are generated from merged PR numbers and titles only. Titles
 * stay editable by their authors after merge, so each is rendered as an inert
 * code span: GitHub renders no links, mentions, references or HTML inside one. */
const REPO = `/repos/${REPOSITORY}`;
const COMMIT = /^[a-f0-9]{40}$/;
export const DESKTOP_TAG = /^desktop-v((0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?)$/;
const MAX_LISTED = 250;
// C0/C1 controls, line/paragraph separators and bidirectional overrides become spaces.
const CONTROL = /[\u0000-\u001f\u007f-\u009f\u2028\u2029\u200e\u200f\u202a-\u202e\u2066-\u2069]/g;

/** [major, minor, patch, beta] with beta = Infinity for a stable version. */
export function parseDesktopVersion(version) {
 const match = typeof version === 'string' && DESKTOP_TAG.exec('desktop-v' + version);
 const parts = match ? [match[2], match[3], match[4], match[5]].map(part => part === undefined ? Infinity : Number(part)) : null;
 if (!parts || parts.some(part => part !== Infinity && !Number.isSafeInteger(part))) throw new Error('Invalid desktop version');
 return parts;
}
export function compareDesktopVersions(a, b) {
 const left = parseDesktopVersion(a), right = parseDesktopVersion(b);
 for (let i = 0; i < 4; i++) if (left[i] !== right[i]) return left[i] < right[i] ? -1 : 1;
 return 0;
}

export function inertTitle(text, max = 200) {
 if (!Number.isSafeInteger(max) || max < 2) throw new Error('Invalid title limit');
 let flat = String(text ?? '').replace(CONTROL, ' ').replace(/\s+/g, ' ').trim();
 const points = [...flat];
 if (points.length > max) {
  // Cut on code points (never inside a surrogate pair) and never inside an entity such as &amp; or &#64;.
  let cut = points.slice(0, max - 1).join('');
  const partial = /&#?[A-Za-z0-9]*$/.exec(cut);
  if (partial && /^&#?[A-Za-z0-9]{1,32};/.test(flat.slice(partial.index))) cut = cut.slice(0, partial.index);
  flat = cut.trimEnd() + '…';
 }
 if (!flat) return '` `';
 const longest = Math.max(0, ...(flat.match(/`+/g) ?? []).map(run => run.length));
 const fence = '`'.repeat(longest + 1);
 const pad = flat.startsWith('`') || flat.endsWith('`') ? ' ' : '';
 return fence + pad + flat + pad + fence;
}

/** The tag of the greatest desktop version strictly below `version`, or null. */
export function previousDesktopVersion(tagNames, version) {
 parseDesktopVersion(version);
 let best = null;
 for (const tag of tagNames) {
  const match = typeof tag === 'string' && DESKTOP_TAG.exec(tag);
  if (!match) continue;
  try { if (compareDesktopVersions(match[1], version) >= 0) continue; } catch { continue; }
  if (best === null || compareDesktopVersions(match[1], best[1]) > 0) best = match;
 }
 return best ? best[0] : null;
}

export async function generateNotes({ api, version, sha, previous }) {
 parseDesktopVersion(version);
 if (typeof sha !== 'string' || !COMMIT.test(sha)) throw new Error('Invalid commit sha');
 if (previous !== null && (typeof previous !== 'string' || !DESKTOP_TAG.test(previous))) throw new Error('Invalid previous desktop tag');
 const header = `# Dilla desktop ${version}\n\n`;
 if (previous === null) return header + 'First desktop release.\n';
 const commits = [];
 for (let page = 1; ; page++) {
  const list = (await api(`${REPO}/compare/${previous}...${sha}?per_page=100&page=${page}`))?.commits;
  if (!Array.isArray(list)) throw new Error('Malformed compare response');
  for (const entry of list) { if (typeof entry?.sha !== 'string' || !COMMIT.test(entry.sha)) throw new Error('Malformed compare commit'); commits.push(entry.sha); }
  if (list.length < 100) break;
  if (page >= 100) throw new Error('Too many commits since the previous desktop release');
 }
 const titles = new Map();
 for (const commit of commits) {
  const pulls = await api(`${REPO}/commits/${commit}/pulls?per_page=100`);
  if (!Array.isArray(pulls)) throw new Error('Malformed pull request list');
  for (const pull of pulls) {
   // Only number, merge state, base branch and title are ever read.
   const number = pull.number;
   if (!Number.isSafeInteger(number) || number < 1 || !pull.merged_at || pull.base?.ref !== 'main' || titles.has(number)) continue;
   const title = pull.title;
   titles.set(number, typeof title === 'string' ? title : '');
  }
 }
 const numbers = [...titles.keys()].sort((a, b) => a - b);
 if (!numbers.length) return header + 'No pull requests were merged since the previous desktop release.\n';
 let body = numbers.slice(0, MAX_LISTED).map(number => `- #${number} ${inertTitle(titles.get(number))}\n`).join('');
 if (numbers.length > MAX_LISTED) body += `- and ${numbers.length - MAX_LISTED} more merged pull requests\n`;
 return header + body;
}

export async function cli(argv, env, { fetchImpl = fetch } = {}) {
 const [command, version, sha, output] = argv;
 if (command !== 'generate' || argv.length !== 4) throw new Error('Usage: release-notes.mjs generate VERSION SHA OUTPUT_FILE');
 parseDesktopVersion(version);
 if (!COMMIT.test(sha)) throw new Error('Invalid commit sha');
 // Exclusive create before any network use: an existing file is never overwritten.
 const file = resolve(output), handle = await open(file, 'wx', 0o644);
 try {
  const api = createGithubApi({ token: env.GH_TOKEN, fetchImpl });
  const refs = await api(`${REPO}/git/matching-refs/tags/desktop-v`);
  if (!Array.isArray(refs)) throw new Error('Malformed tag list');
  const tags = refs.map(ref => typeof ref?.ref === 'string' && ref.ref.startsWith('refs/tags/') ? ref.ref.slice('refs/tags/'.length) : null).filter(Boolean);
  await handle.writeFile(await generateNotes({ api, version, sha, previous: previousDesktopVersion(tags, version) }));
  await handle.close();
 } catch (error) {
  await handle.close().catch(() => {});
  await rm(file, { force: true });
  throw error;
 }
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
 await cli(process.argv.slice(2), process.env);
 console.log('Generated desktop release notes from merged pull request titles.');
}
