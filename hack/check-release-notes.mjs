// Run by hack/check-release-notes.sh from a directory holding the pinned
// plugins. Feeds a fixed set of commits through the commit analyzer and
// the release-notes generator configured in .releaserc.json and fails
// when a release type or a notes section is missing.
import { readFileSync } from 'node:fs';
import { analyzeCommits } from '@semantic-release/commit-analyzer';
import { generateNotes } from '@semantic-release/release-notes-generator';

const config = JSON.parse(readFileSync(process.argv[2], 'utf8'));
const pluginConfig = (name) => {
  const entry = config.plugins.find((p) => (Array.isArray(p) ? p[0] : p) === name);
  return Array.isArray(entry) ? entry[1] : {};
};

const commits = [
  { hash: 'a'.repeat(40), message: 'feat(installer): add a check feature' },
  { hash: 'b'.repeat(40), message: 'fix(bridge): repair a check bug' },
  { hash: 'c'.repeat(40), message: 'fix(chart): drop a check value\n\nBREAKING CHANGE: the check value is gone.' },
  { hash: 'd'.repeat(40), message: 'chore(deps): update a check dependency' },
];
const logger = { log() {}, error: console.error, warn: console.warn };
const context = {
  cwd: process.cwd(),
  options: { repositoryUrl: 'https://github.com/AetherizeGmbH/harbor-workload-identity-bridge' },
  commits,
  lastRelease: { gitTag: 'v0.0.1', version: '0.0.1' },
  nextRelease: { gitTag: 'v0.1.0', version: '0.1.0' },
  logger,
};

const type = await analyzeCommits(pluginConfig('@semantic-release/commit-analyzer'), context);
const notes = await generateNotes(pluginConfig('@semantic-release/release-notes-generator'), context);
console.log(notes);

const failures = [];
if (type !== 'minor') failures.push(`release type is ${type}, want minor (breaking maps to minor pre-1.0)`);
for (const want of ['### Features', 'add a check feature', '### Bug Fixes', 'repair a check bug', 'BREAKING CHANGES', 'the check value is gone']) {
  if (!notes.includes(want)) failures.push(`notes lack "${want}"`);
}
if (notes.includes('update a check dependency')) failures.push('notes show a chore commit, which .releaserc.json hides');
if (failures.length) {
  console.error(`release notes check failed:\n- ${failures.join('\n- ')}`);
  process.exit(1);
}
console.log('release notes check passed');
