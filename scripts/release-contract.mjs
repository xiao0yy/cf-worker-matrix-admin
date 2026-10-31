import {readFileSync, readdirSync, lstatSync} from 'node:fs';
import {join} from 'node:path';
import {createHash} from 'node:crypto';

export const assetNames = Object.freeze([
  'BUILD.json', 'SHA256SUMS', 'matrix-admin-linux-amd64.manifest.json',
  'matrix-admin-linux-amd64.tar.gz', 'matrix-release-linux-amd64', 'software.json', 'source.tar.gz',
]);
export const hash = bytes => createHash('sha256').update(bytes).digest('hex');
const versionPattern = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[a-zA-Z0-9]+(?:[.-][a-zA-Z0-9]+)*)?$/;

export function validateContext({version, packageVersion, tag, repository, origin, commit, tagCommit}) {
  if (!versionPattern.test(version) || packageVersion !== version) throw new Error('VERSION and package version must agree');
  if (!/^[a-f0-9]{40}$/.test(commit)) throw new Error('Invalid source commit');
  if (!tag) return {version, commit};
  if (tag !== `v${version}` || tagCommit !== commit) throw new Error('Release tag, VERSION and checked-out commit must agree');
  if (!/^[a-zA-Z0-9_-]+\/[a-zA-Z0-9_.-]+$/.test(repository || '')) throw new Error('Invalid GitHub repository');
  const expected = `https://github.com/${repository}/releases/download`;
  if (origin.release_base_url !== expected) throw new Error('Release download origin must match the publishing GitHub repository');
  return {version, commit, tag, repository};
}

// Verify the handoff between the read-only build job and the publishing job.
// No wildcard upload: each byte belongs to one reviewed, fixed asset name.
export function verifyAssets(directory, context, origin) {
  const names = readdirSync(directory).sort();
  if (JSON.stringify(names) !== JSON.stringify([...assetNames].sort())) throw new Error('Release asset set is incomplete or contains unknown files');
  const assets = new Map();
  for (const name of names) {
    const path = join(directory, name), stat = lstatSync(path);
    if (!stat.isFile() || stat.isSymbolicLink() || stat.size > 256 * 1024 * 1024) throw new Error(`Unsafe release asset: ${name}`);
    const bytes = readFileSync(path);
    assets.set(name, {name, bytes, size:bytes.length, sha256:hash(bytes)});
  }
  const sums = new Map();
  for (const line of assets.get('SHA256SUMS').bytes.toString().trimEnd().split('\n')) {
    const match = /^([a-f0-9]{64})  ([a-zA-Z0-9_.-]+)$/.exec(line);
    if (!match || sums.has(match[2]) || match[2] === 'SHA256SUMS' || !assets.has(match[2])) throw new Error('Invalid release checksum inventory');
    sums.set(match[2], match[1]);
  }
  for (const [name, asset] of assets) {
    if (name !== 'SHA256SUMS' && sums.get(name) !== asset.sha256) throw new Error(`Release checksum mismatch: ${name}`);
  }
  const json = name => JSON.parse(assets.get(name).bytes);
  const build = json('BUILD.json'), manifest = json('matrix-admin-linux-amd64.manifest.json'), descriptor = json('software.json');
  if (build.publishable !== true || build.commit !== context.commit || build.version !== context.version || build.release_base_url !== origin.release_base_url) throw new Error('Build identity differs from the selected release');
  if (manifest.schema !== 1 || manifest.project !== 'matrix-admin' || manifest.version !== context.version || manifest.commit !== context.commit) throw new Error('Runtime manifest identity differs from the selected release');
  if (descriptor.project !== manifest.project || descriptor.version !== context.version || descriptor.manifest_sha256 !== assets.get('matrix-admin-linux-amd64.manifest.json').sha256) throw new Error('Software descriptor differs from the runtime manifest');
  const archive = assets.get('matrix-admin-linux-amd64.tar.gz');
  if (manifest.archive?.name !== archive.name || manifest.archive.sha256 !== archive.sha256 || manifest.archive.size !== archive.size) throw new Error('Runtime archive differs from its manifest');
  const embeddedOrigin = manifest.files?.find(file => file.path === 'release.json');
  if (embeddedOrigin?.sha256 !== hash(JSON.stringify(origin, null, 2)+'\n')) throw new Error('Packaged download origin differs from the publishing repository');
  return assets;
}
