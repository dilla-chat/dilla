import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

export function checkPublisherWorkflow(text) {
 const issues = [];
 for (const item of ["tags: ['desktop-v*']", 'contents: read', 'actions: read', 'contents: write', 'fetch-depth: 0',
   'node-version-file: .nvmrc', 'node packages/desktop/scripts/publish-release.mjs plan',
   'run-id: ${{ steps.plan.outputs.run }}', 'repository: dilla-chat/dilla', 'pattern: desktop-installers-*',
   'node packages/desktop/scripts/publish-release.mjs publish', 'GH_TOKEN: ${{ github.token }}']) {
  if (!text.includes(item)) issues.push('Missing desktop publication gate: ' + item);
 }
 if (/pull_request:|workflow_dispatch:|continue-on-error|always\(\)|secrets\.|PRIVATE_KEY|SIGNING_KEY|\|\|\s*true/.test(text)) issues.push('Publication must use authenticated tag artifacts without private signing keys or skipped gates');
 const allowed = new Set(['actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1', 'actions/setup-node@949feb2413d6458794dcd2491c4babbbce0c15c1', 'actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333']);
 for (const match of text.matchAll(/uses:\s*(\S+)/g)) if (!allowed.has(match[1])) issues.push('Unpinned/unrecognized desktop publisher action');
 return issues;
}
export function checkCheckedInPublisher() {
 return checkPublisherWorkflow(readFileSync(resolve(import.meta.dirname, '../../../.github/workflows/desktop-release.yml'), 'utf8'));
}
