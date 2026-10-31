import {readFileSync, appendFileSync} from 'node:fs';
import {join, resolve} from 'node:path';
import {fileURLToPath} from 'node:url';
import {execFileSync} from 'node:child_process';
import {validateContext} from './release-contract.mjs';

export const project = fileURLToPath(new URL('../', import.meta.url));
export function releaseContext(tag = process.env.RELEASE_TAG || '', repository = process.env.GITHUB_REPOSITORY || '') {
  const git = (...args) => execFileSync('git', args, {cwd:project, encoding:'utf8'}).trim();
  const version = readFileSync(join(project, 'VERSION'), 'utf8').trim();
  const origin = JSON.parse(readFileSync(join(project, 'release.config.json'), 'utf8'));
  const lockfile = JSON.parse(readFileSync(join(project, 'package-lock.json'), 'utf8'));
  if (lockfile.version !== version || lockfile.packages?.['']?.version !== version) throw new Error('Package lock version must match VERSION');
  // Validate before giving a ref to Git. Only an existing version tag is valid.
  if (tag && tag !== `v${version}`) throw new Error('Release tag must equal v<VERSION>');
  const context = validateContext({version, packageVersion:JSON.parse(readFileSync(join(project, 'package.json'), 'utf8')).version,
    tag, repository, origin, commit:git('rev-parse', 'HEAD'), tagCommit:tag ? git('rev-parse', '--verify', `refs/tags/${tag}^{commit}`) : undefined});
  return {context, origin};
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const {context} = releaseContext();
  if (process.env.GITHUB_OUTPUT) appendFileSync(process.env.GITHUB_OUTPUT, `version=${context.version}\ncommit=${context.commit}\n`);
  console.log(`Validated source ${context.commit} for ${context.tag || context.version}`);
}
