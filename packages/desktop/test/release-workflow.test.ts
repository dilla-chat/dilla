import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
// @ts-expect-error publishing tooling is JavaScript
import { checkPublisherWorkflow, checkCheckedInPublisher } from '../scripts/check-release-workflow.mjs';

test('release workflow preserves authenticated source/artifact gates and never receives signing keys', () => {
 assert.deepEqual(checkCheckedInPublisher(), []);
 const text = readFileSync(resolve(import.meta.dirname, '../../../.github/workflows/desktop-release.yml'), 'utf8');
 for (const removed of ['publish-release.mjs plan', 'run-id: ${{ steps.plan.outputs.run }}', 'repository: dilla-chat/dilla', 'fetch-depth: 0']) assert.ok(checkPublisherWorkflow(text.replace(removed, 'removed')).length > 0);
 assert.ok(checkPublisherWorkflow(text.replace('contents: read', 'contents: read\n  pull_request:')).length > 0);
 assert.ok(checkPublisherWorkflow(text + '\nenv:\n  PRIVATE_KEY: secret\n').length > 0);
 assert.ok(checkPublisherWorkflow(text.replace(/actions\/checkout@[a-f0-9]+/, 'actions/checkout@main')).length > 0);
});
