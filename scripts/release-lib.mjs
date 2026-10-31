import {createHash} from 'node:crypto';
import {readFileSync, readdirSync, lstatSync, writeFileSync, mkdirSync} from 'node:fs';
import {join, relative} from 'node:path';
import {execFileSync} from 'node:child_process';

export const sha256 = data => createHash('sha256').update(data).digest('hex');
export function inventory(root) {
  const files = [];
  function walk(dir) {
    for (const entry of readdirSync(dir).sort()) {
      const path = join(dir, entry), stat = lstatSync(path);
      if (stat.isSymbolicLink()) throw new Error(`Symlink in release: ${path}`);
      if (stat.isDirectory()) walk(path);
      else if (stat.isFile()) files.push({path: relative(root, path), sha256: sha256(readFileSync(path)), size: stat.size, executable: (stat.mode & 0o111) !== 0});
      else throw new Error(`Non-regular release member: ${path}`);
    }
  }
  walk(root);
  return files.sort((a,b) => a.path < b.path ? -1 : 1);
}

export function packRelease(stage, output, {version, commit, origin, timestamp}) {
  mkdirSync(output, {recursive:true});
  const files = inventory(stage);
  const archiveName = 'matrix-admin-linux-amd64.tar.gz';
  const archive = join(output, archiveName);
  // Files only; no directory entries, links, pax headers or implicit recursion.
  const tar = execFileSync('tar', ['--format=ustar', '--owner=0', '--group=0', '--numeric-owner', `--mtime=@${timestamp}`, '--no-recursion', '-cf', '-', '--', ...files.map(f => f.path)], {cwd:stage, maxBuffer:512*1024*1024});
  const gzip = execFileSync('gzip', ['-n','-9'], {input:tar, maxBuffer:256*1024*1024});
  writeFileSync(archive, gzip, {mode:0o600});
  const manifest = {schema:1, project:'matrix-admin', version, commit, platform:origin.platform, protocols:origin.protocols, restic_version:origin.restic_version,
    archive:{name:archiveName, sha256:sha256(gzip), size:gzip.length}, files};
  const text = JSON.stringify(manifest,null,2)+'\n';
  writeFileSync(join(output,'matrix-admin-linux-amd64.manifest.json'),text);
  writeFileSync(join(output,'software.json'),JSON.stringify({project:manifest.project,version,manifest_sha256:sha256(text)},null,2)+'\n');
  writeFileSync(join(output,'SHA256SUMS'),`${sha256(gzip)}  ${archiveName}\n${sha256(text)}  matrix-admin-linux-amd64.manifest.json\n`);
  return manifest;
}
