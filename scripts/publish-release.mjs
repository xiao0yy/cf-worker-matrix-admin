// Called only by the release job, after the read-only build/test job succeeds.
import {resolve} from 'node:path';
import {fileURLToPath} from 'node:url';
import {project, releaseContext} from './release-context.mjs';
import {verifyAssets} from './release-contract.mjs';
import {generateReleaseNotes, mergeReleaseNotes} from './release-notes.mjs';

function numericID(value) {
  if (!Number.isSafeInteger(value) || value <= 0) throw new Error('Invalid GitHub object ID');
  return value;
}

export function formatReleaseError(error, actions = false) {
  const message = error instanceof Error ? error.message : String(error);
  // Expose the actionable error in the job's annotations as well as its log.
  // Escape workflow-command data so multiline messages remain one annotation.
  return actions ? `::error::${message.replaceAll('%', '%25').replaceAll('\r', '%0D').replaceAll('\n', '%0A')}` : message;
}

export async function publishRelease({context, origin, directory, notes, token, fetchImpl = fetch}) {
  if (!context.tag || !token) throw new Error('A validated release tag and GitHub token are required');
  mergeReleaseNotes('', notes); // Validate generated prose before any remote write.
  const assets = verifyAssets(directory, context, origin);
  const repo = `/repos/${context.repository}`;
  const headers = {Authorization:`Bearer ${token}`, Accept:'application/vnd.github+json',
    'X-GitHub-Api-Version':'2022-11-28', 'User-Agent':'matrix-admin-release'};
  async function request(method, path, body, {absent = false, upload = false} = {}) {
    let response;
    try {
      response = await fetchImpl(`https://${upload ? 'uploads' : 'api'}.github.com${path}`, {
        method, redirect:'error', signal:AbortSignal.timeout(300000),
        headers:{...headers, ...(body === undefined ? {} : {'Content-Type':upload ? 'application/octet-stream' : 'application/json'})},
        body:body === undefined ? undefined : upload ? body : JSON.stringify(body),
      });
    } catch {
      // A lost upload/publish response is ambiguous. Re-run to inspect the exact
      // remote asset set; never delete an uploaded asset to force a fresh upload.
      throw new Error(`GitHub ${method} failed; re-run this release to reconcile its existing state`);
    }
    if (absent && response.status === 404) return null;
    if (!response.ok) throw new Error(`GitHub ${method} failed with HTTP ${response.status}; release was not assumed absent`);
    if (response.status === 204) return null;
    return response.json();
  }
  async function verifyTag() {
    let object = (await request('GET', `${repo}/git/ref/tags/${context.tag}`)).object;
    for (let depth = 0; object?.type === 'tag' && depth < 5; depth++) {
      if (!/^[a-f0-9]{40}$/.test(object.sha)) throw new Error('Invalid annotated tag');
      object = (await request('GET', `${repo}/git/tags/${object.sha}`)).object;
    }
    if (object?.type !== 'commit' || object.sha !== context.commit) throw new Error('Remote tag moved or differs from the tested source commit');
  }
  function verifyAsset(asset) {
    const expected = assets.get(asset.name);
    if (!expected || asset.state !== 'uploaded' || asset.size !== expected.size || asset.digest !== `sha256:${expected.sha256}`) {
      throw new Error(`Remote release asset differs or lacks a verified digest: ${asset.name}`);
    }
  }
  async function remoteAssets(releaseID) {
    const list = await request('GET', `${repo}/releases/${releaseID}/assets?per_page=100`);
    if (!Array.isArray(list) || list.length > assets.size) throw new Error('Unexpected remote asset inventory');
    const result = new Map();
    for (const asset of list) {
      numericID(asset.id);
      if (!assets.has(asset.name) || result.has(asset.name)) throw new Error('Unknown or duplicate remote asset');
      result.set(asset.name, asset);
    }
    return result;
  }
  if ((await request('GET', repo)).private !== false) throw new Error('Recovery requires a public GitHub release repository');
  await verifyTag();
  let release = await request('GET', `${repo}/releases/tags/${context.tag}`, undefined, {absent:true});
  if (!release) {
    // The by-tag endpoint only promises published releases. The authenticated
    // release list also includes drafts, including one left by a failed run.
    const matches = [];
    for (let page = 1; ; page++) {
      if (page > 100) throw new Error('Release lookup limit exceeded; no new draft was created');
      const list = await request('GET', `${repo}/releases?per_page=100&page=${page}`);
      if (!Array.isArray(list)) throw new Error('Invalid GitHub release list');
      matches.push(...list.filter(item => item.tag_name === context.tag));
      if (matches.length > 1) throw new Error('Multiple releases use the same tag; inspect before retrying');
      if (list.length < 100) break;
    }
    release = matches[0];
  }
  if (!release) {
    release = await request('POST', `${repo}/releases`, {
      tag_name:context.tag, target_commitish:context.commit, name:`Matrix Admin ${context.tag}`,
      draft:true, prerelease:context.version.includes('-'),
      body:'',
    });
  }
  const id = numericID(release.id);
  if (release.tag_name !== context.tag) throw new Error('GitHub returned a release for a different tag');
  if (typeof release.draft !== 'boolean') throw new Error('GitHub returned an invalid release state');
  mergeReleaseNotes(release.body, notes);
  async function finishRelease() {
    // A user may edit prose during the upload. Merge into the latest body,
    // and never silently reverse a concurrent publish/unpublish operation.
    const latest = await request('GET', `${repo}/releases/${id}`);
    if (latest.tag_name !== context.tag || latest.draft !== release.draft) throw new Error('Release state changed during upload; re-run to reconcile');
    const body = mergeReleaseNotes(latest.body, notes);
    const patch = {};
    if (body !== latest.body) patch.body = body;
    if (!latest.name?.trim()) patch.name = `Matrix Admin ${context.tag}`;
    if (latest.draft) Object.assign(patch, {draft:false, make_latest:latest.prerelease ? 'false' : 'legacy'});
    if (!Object.keys(patch).length) return;
    const updated = await request('PATCH', `${repo}/releases/${id}`, patch);
    if (updated.draft !== false || updated.tag_name !== context.tag
        || (patch.body !== undefined && updated.body !== patch.body)
        || (patch.name !== undefined && updated.name !== patch.name)) throw new Error('Release update response is ambiguous; re-run to reconcile');
  }
  // A release created in the web UI is a supported publishing target. Its
  // editable title/body are not provenance: use the tag commit and asset hashes.
  let remote = await remoteAssets(id);
  const unfinished = asset => asset.state === 'starter' && asset.size === 0 && asset.digest == null;
  // Check EVERY existing asset before uploading any missing file. A late
  // conflict must not leave new attachments on a mismatched release.
  for (const asset of remote.values()) {
    if (!unfinished(asset)) verifyAsset(asset);
  }
  const complete = remote.size === assets.size && [...remote.values()].every(asset => !unfinished(asset));
  if (!release.draft && complete) {
    await verifyTag();
    await finishRelease();
    return {alreadyPublished:true, tag:context.tag};
  }
  if (release.immutable === true) throw new Error('Immutable release is incomplete; attachments cannot be added. Publish a new version, or upload all assets to a draft before publication.');
  // The recovery descriptor becomes downloadable after the other assets.
  const uploadOrder = [...assets.keys()].filter(name => name !== 'software.json').concat('software.json');
  for (const name of uploadOrder) {
    const local = assets.get(name);
    const existing = remote.get(name);
    // GitHub may leave an empty "starter" after a failed upload, including on
    // a web-created release. Never delete an uploaded asset or unknown file.
    if (existing && unfinished(existing)) {
      await request('DELETE', `${repo}/releases/assets/${existing.id}`);
    } else if (existing) {
      verifyAsset(existing);
      continue;
    }
    verifyAsset(await request('POST', `${repo}/releases/${id}/assets?name=${encodeURIComponent(name)}`, local.bytes, {upload:true}));
  }
  remote = await remoteAssets(id);
  if (remote.size !== assets.size) throw new Error('Release asset upload is incomplete');
  for (const asset of remote.values()) verifyAsset(asset);
  await verifyTag();
  await finishRelease();
  return {alreadyPublished:false, tag:context.tag};
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    if (process.argv.length !== 3) throw new Error('Usage: node scripts/publish-release.mjs ASSET_DIRECTORY');
    const {context, origin} = releaseContext();
    const notes = generateReleaseNotes(context, project);
    const result = await publishRelease({context, origin, notes, directory:resolve(process.argv[2]), token:process.env.GH_TOKEN});
    console.log(`${result.alreadyPublished ? 'Verified existing' : 'Completed'} release: https://github.com/${context.repository}/releases/tag/${result.tag}`);
  } catch (error) {
    console.error(formatReleaseError(error, process.env.GITHUB_ACTIONS === 'true'));
    process.exitCode = 1;
  }
}
