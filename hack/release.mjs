// Runs semantic-release from the versions pinned in package-lock.json (the
// release job installs them with `npm ci`) and publishes the result as step
// outputs, the same two that cycjimmy/semantic-release-action set before:
//   new_release_published  "true" when a release was created, else "false"
//   new_release_version    x.y.z of that release (only when published)
// release.yml gates the chart and image dispatches on them. The bare
// `npx semantic-release` CLI sets no outputs, which once silently skipped
// both dispatches (v0.0.0 to v0.0.5).
import { appendFileSync } from 'node:fs';
import semanticRelease from 'semantic-release';

const result = await semanticRelease();
const version = result && result.nextRelease ? result.nextRelease.version : '';
const lines = [`new_release_published=${version ? 'true' : 'false'}`];
if (version) lines.push(`new_release_version=${version}`);

if (process.env.GITHUB_OUTPUT) {
  appendFileSync(process.env.GITHUB_OUTPUT, `${lines.join('\n')}\n`);
} else {
  console.log(lines.join('\n'));
}
