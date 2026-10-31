import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync, rmSync, writeFileSync, readFileSync, symlinkSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {assetNames, hash, validateContext, verifyAssets} from '../scripts/release-contract.mjs';
import {formatReleaseError, publishRelease} from '../scripts/publish-release.mjs';

function fixture(t) {
  const directory = mkdtempSync(join(tmpdir(), 'matrix-publish-test-'));
  t.after(() => rmSync(directory, {recursive:true, force:true}));
  const context = {version:'0.3.0', tag:'v0.3.0', commit:'a'.repeat(40), repository:'fixture/matrix-admin'};
  const origin = {release_base_url:'https://github.com/fixture/matrix-admin/releases/download'};
  const write = (name, data) => writeFileSync(join(directory, name), data);
  const json = (name, data) => write(name, JSON.stringify(data, null, 2)+'\n');
  const checksums = () => write('SHA256SUMS', assetNames.filter(n => n !== 'SHA256SUMS').map(n => `${hash(readFileSync(join(directory, n)))}  ${n}\n`).join(''));
  write('matrix-admin-linux-amd64.tar.gz', 'runtime archive fixture');
  write('matrix-release-linux-amd64', 'verifier fixture');
  write('source.tar.gz', 'source fixture');
  const archive = readFileSync(join(directory, 'matrix-admin-linux-amd64.tar.gz'));
  const manifest = {schema:1, project:'matrix-admin', version:context.version, commit:context.commit,
    archive:{name:'matrix-admin-linux-amd64.tar.gz', sha256:hash(archive), size:archive.length},
    files:[{path:'release.json', sha256:hash(JSON.stringify(origin, null, 2)+'\n')}]};
  json('matrix-admin-linux-amd64.manifest.json', manifest);
  json('software.json', {project:'matrix-admin', version:context.version, manifest_sha256:hash(readFileSync(join(directory, 'matrix-admin-linux-amd64.manifest.json')))});
  const build = {version:context.version, commit:context.commit, publishable:true, release_base_url:origin.release_base_url};
  json('BUILD.json', build); checksums();
  return {directory, context, origin, write, json, checksums, build, notes:'## 更新内容\n\n- Fixture change'};
}

function github(f) {
  const state = {release:null, assets:[], calls:[], tagReads:0, uploadCount:0, annotated:false};
  const response = (body, status = 200) => new Response(status === 204 ? null : JSON.stringify(body), {status});
  const fetchImpl = async (url, options) => {
    const parsed = new URL(url), path = parsed.pathname, method = options.method;
    assert.equal(options.headers.Authorization, 'Bearer fixture-token');
    assert.equal(options.redirect, 'error');
    state.calls.push({method, path, name:parsed.searchParams.get('name'), body:method === 'PATCH' ? JSON.parse(options.body) : undefined});
    if (path === '/repos/fixture/matrix-admin') return response({private:false});
    if (path.endsWith(`/git/ref/tags/${f.context.tag}`)) {
      state.tagReads++;
      return response({object:{type:state.annotated ? 'tag' : 'commit', sha:state.moveTag && state.tagReads > 1 ? 'b'.repeat(40) : f.context.commit}});
    }
    if (path.endsWith(`/git/tags/${f.context.commit}`)) return response({object:{type:'commit', sha:f.context.commit}});
    if (path.endsWith(`/releases/tags/${f.context.tag}`)) return state.denyRead ? response({}, 401) : response(state.release || {}, state.release && !state.release.draft ? 200 : 404);
    if (method === 'GET' && path.endsWith('/releases')) {
      if (state.draftOnSecondPage && parsed.searchParams.get('page') === '1') return response(Array.from({length:100}, (_, n) => ({tag_name:`other-${n}`})));
      return response(state.release ? [state.release] : []);
    }
    if (method === 'POST' && path.endsWith('/releases')) {
      const body = JSON.parse(options.body); assert.equal(body.draft, true);
      state.release = {...body, id:100, immutable:false}; return response(state.release, 201);
    }
    if (method === 'GET' && path.endsWith('/releases/100')) {
      if (state.editBeforeFinish) {
        Object.assign(state.release, state.editBeforeFinish);
        state.editBeforeFinish = null;
      }
      return response(state.release);
    }
    if (path.endsWith('/releases/100/assets') && method === 'GET') return response(state.assets);
    if (path.endsWith('/releases/100/assets') && method === 'POST') {
      assert.equal(parsed.hostname, 'uploads.github.com');
      state.uploadCount++;
      const asset = {id:state.uploadCount, name:parsed.searchParams.get('name'), state:'uploaded', size:options.body.length, digest:`sha256:${hash(options.body)}`};
      state.assets.push(asset);
      if (state.failUpload === state.uploadCount) throw new Error('simulated lost upload response');
      return response(asset, 201);
    }
    if (method === 'DELETE') {
      const id = Number(path.split('/').at(-1));
      const asset = state.assets.find(a => a.id === id);
      assert.ok(!state.release.immutable && assetNames.includes(asset.name) && asset.state === 'starter' && asset.size === 0 && asset.digest == null);
      state.assets = state.assets.filter(a => a.id !== id); return response(null, 204);
    }
    if (method === 'PATCH' && path.endsWith('/releases/100')) {
      assert.equal(state.assets.length, assetNames.length);
      state.release = {...state.release, ...JSON.parse(options.body)};
      if (state.failUpdateResponse) throw new Error('simulated lost metadata update response');
      return response(state.release);
    }
    throw new Error(`Unexpected fixture request: ${method} ${url}`);
  };
  const publish = () => publishRelease({...f, token:'fixture-token', fetchImpl});
  return {state, publish};
}

test('release tag, source, version and publishing repository must agree', () => {
  const args = {version:'0.3.0', packageVersion:'0.3.0', tag:'v0.3.0', commit:'a'.repeat(40), tagCommit:'a'.repeat(40), repository:'fixture/matrix-admin', origin:{release_base_url:'https://github.com/fixture/matrix-admin/releases/download'}};
  assert.equal(validateContext(args).tag, 'v0.3.0');
  for (const update of [{tag:'v0.4.0'}, {packageVersion:'0.2.0'}, {tagCommit:'b'.repeat(40)}, {repository:'other/repo'}, {origin:{release_base_url:'https://gitea.example/releases/download'}}]) {
    assert.throws(() => validateContext({...args, ...update}));
  }
});

test('only the complete verified asset set can reach the publishing API', async t => {
  const f = fixture(t), g = github(f);
  assert.equal(verifyAssets(f.directory, f.context, f.origin).size, 7);
  f.write('source.tar.gz', 'corrupted source');
  await assert.rejects(g.publish(), /checksum mismatch/);
  assert.equal(g.state.calls.length, 0);
  f.checksums(); f.json('BUILD.json', {...f.build, publishable:false}); f.checksums();
  await assert.rejects(g.publish(), /Build identity/);
  f.json('BUILD.json', f.build); f.checksums(); f.write('private.env', 'must not upload');
  await assert.rejects(g.publish(), /asset set/);
  rmSync(join(f.directory, 'private.env')); rmSync(join(f.directory, 'source.tar.gz'));
  symlinkSync(join(f.directory, 'BUILD.json'), join(f.directory, 'source.tar.gz'));
  await assert.rejects(g.publish(), /Unsafe release asset/);
  assert.equal(g.state.calls.length, 0);
});

test('publishes only after all assets are verified and annotated tag resolves correctly', async t => {
  const f = fixture(t), g = github(f); g.state.annotated = true;
  assert.deepEqual(await g.publish(), {alreadyPublished:false, tag:'v0.3.0'});
  assert.equal(g.state.release.draft, false);
  assert.equal(g.state.uploadCount, 7);
  assert.equal(g.state.calls.at(-1).method, 'PATCH');
  assert.equal(g.state.tagReads, 2);
});

test('lost upload response keeps a draft and retries without overwriting uploaded assets', async t => {
  const f = fixture(t), g = github(f); g.state.failUpload = 3;
  await assert.rejects(g.publish(), /reconcile/);
  assert.equal(g.state.release.draft, true);
  assert.equal(g.state.assets.length, 3);
  assert.ok(!g.state.calls.some(c => c.method === 'PATCH'));
  g.state.failUpload = null;
  g.state.draftOnSecondPage = true;
  assert.equal((await g.publish()).alreadyPublished, false);
  assert.equal(g.state.uploadCount, 7);
  assert.equal(g.state.calls.filter(c => c.method === 'POST' && c.path.endsWith('/releases')).length, 1);
  assert.ok(!g.state.calls.some(c => c.method === 'DELETE'));
});

test('rerunning a published release verifies it without mutation', async t => {
  const f = fixture(t), g = github(f); await g.publish();
  const before = g.state.calls.length;
  assert.equal((await g.publish()).alreadyPublished, true);
  assert.ok(g.state.calls.slice(before).every(c => c.method === 'GET'));
});

test('digest conflicts stop both draft retries and published-release retries', async t => {
  const f = fixture(t), g = github(f); await g.publish();
  g.state.assets[0].digest = `sha256:${'0'.repeat(64)}`;
  for (const draft of [true, false]) {
    g.state.release.draft = draft; const before = g.state.calls.length;
    await assert.rejects(g.publish(), /Remote release asset differs/);
    assert.ok(g.state.calls.slice(before).every(c => c.method === 'GET'));
  }
});

test('an authentication error is never interpreted as an absent release', async t => {
  const f = fixture(t), g = github(f); g.state.denyRead = true;
  await assert.rejects(g.publish(), /HTTP 401/);
  assert.ok(g.state.calls.every(c => c.method === 'GET'));
});

test('only empty starter uploads may be cleared on a mutable release', async t => {
  for (const draft of [true, false]) {
    const f = fixture(t), g = github(f); g.state.failUpload = 1;
    await assert.rejects(g.publish());
    g.state.release.draft = draft;
    Object.assign(g.state.assets[0], {state:'starter', size:0, digest:null});
    g.state.failUpload = null;
    await g.publish();
    assert.equal(g.state.calls.filter(c => c.method === 'DELETE').length, 1);
    assert.equal(g.state.assets.length, 7);
  }
});

test('a tag changed during upload cannot publish the draft', async t => {
  const f = fixture(t), g = github(f); g.state.moveTag = true;
  await assert.rejects(g.publish(), /Remote tag moved/);
  assert.equal(g.state.release.draft, true);
  assert.ok(!g.state.calls.some(c => c.method === 'PATCH'));
});

test('an incomplete mutable release is completed without replacing existing assets', async t => {
  const f = fixture(t), g = github(f); await g.publish(); g.state.assets.pop();
  const before = g.state.calls.length, previous = structuredClone(g.state.assets);
  assert.equal((await g.publish()).alreadyPublished, false);
  assert.equal(g.state.assets.length, 7);
  assert.deepEqual(g.state.assets.slice(0, 6), previous);
  assert.equal(g.state.calls.slice(before).filter(c => c.method === 'POST').length, 1);
  assert.ok(g.state.calls.slice(before).every(c => ['GET', 'POST'].includes(c.method)));
});

test('web-created draft and published releases accept assets and preserve user metadata', async t => {
  for (const draft of [true, false]) {
    const f = fixture(t), g = github(f);
    g.state.release = {id:100, tag_name:f.context.tag, target_commitish:'main', draft, immutable:false, name:'My release', body:'My release notes', prerelease:true};
    assert.equal((await g.publish()).alreadyPublished, false);
    assert.equal(g.state.assets.length, assetNames.length);
    assert.equal(g.state.release.name, 'My release');
    assert.ok(g.state.release.body.startsWith('My release notes\n\n'));
    assert.ok(g.state.release.body.includes(f.notes));
    assert.equal(g.state.release.prerelease, true);
    assert.equal(g.state.release.draft, false);
    assert.ok(!g.state.calls.some(c => c.method === 'POST' && c.path.endsWith('/releases')));
    assert.equal(g.state.calls.filter(c => c.method === 'PATCH').length, 1);
    if (!draft) assert.ok(!('draft' in g.state.calls.find(c => c.method === 'PATCH').body));
    assert.equal(g.state.calls.filter(c => c.method === 'POST').at(-1).name, 'software.json');
  }
});

test('completed release verification does not depend on editable release notes', async t => {
  const f = fixture(t), g = github(f); await g.publish();
  for (const body of [null, '', 'Edited release notes', `<!-- matrix-admin-release:${'b'.repeat(40)} -->`]) {
    g.state.release.body = body;
    const before = g.state.calls.length, assets = structuredClone(g.state.assets);
    assert.equal((await g.publish()).alreadyPublished, true);
    assert.deepEqual(g.state.assets, assets);
    assert.ok(g.state.calls.slice(before).every(c => ['GET', 'PATCH'].includes(c.method)));
    assert.ok(g.state.release.body.includes(f.notes));
  }
});

test('web-created empty release can resume after a lost upload response', async t => {
  const f = fixture(t), g = github(f);
  g.state.release = {id:100, tag_name:f.context.tag, draft:false, body:''};
  g.state.failUpload = 3;
  await assert.rejects(g.publish(), /reconcile/);
  const previous = structuredClone(g.state.assets);
  assert.equal(g.state.release.draft, false);
  g.state.failUpload = null;
  await g.publish();
  assert.deepEqual(g.state.assets.slice(0, 3), previous);
  assert.equal(g.state.uploadCount, 7);
  assert.ok(g.state.calls.every(c => ['GET', 'POST', 'PATCH'].includes(c.method)));
});

test('blank title and description are filled only after every attachment is verified', async t => {
  const f = fixture(t), g = github(f);
  g.state.release = {id:100, tag_name:f.context.tag, draft:false, name:'', body:''};
  await g.publish();
  assert.equal(g.state.release.name, 'Matrix Admin v0.3.0');
  assert.ok(g.state.release.body.includes(f.notes));
  assert.equal(g.state.calls.at(-1).method, 'PATCH');
  const before = g.state.calls.length;
  await g.publish();
  assert.ok(g.state.calls.slice(before).every(c => c.method === 'GET'));
});

test('lost notes update response resumes without duplicate text or asset uploads', async t => {
  const f = fixture(t), g = github(f); g.state.failUpdateResponse = true;
  await assert.rejects(g.publish(), /reconcile/);
  const body = g.state.release.body, before = g.state.calls.length;
  g.state.failUpdateResponse = false;
  assert.equal((await g.publish()).alreadyPublished, true);
  assert.equal(g.state.release.body, body);
  assert.ok(g.state.calls.slice(before).every(c => c.method === 'GET'));
});

test('notes preserve prose edited during the upload', async t => {
  const f = fixture(t), g = github(f);
  g.state.editBeforeFinish = {name:'Updated title', body:'A note added while uploading'};
  await g.publish();
  assert.equal(g.state.release.name, 'Updated title');
  assert.ok(g.state.release.body.startsWith('A note added while uploading\n\n'));
  assert.ok(g.state.release.body.includes(f.notes));
});

test('a concurrent unpublish is not reversed by the notes update', async t => {
  const f = fixture(t), g = github(f);
  g.state.release = {id:100, tag_name:f.context.tag, draft:false, body:''};
  g.state.editBeforeFinish = {draft:true};
  await assert.rejects(g.publish(), /Release state changed/);
  assert.ok(!g.state.calls.some(c => c.method === 'PATCH'));
  assert.equal(g.state.release.draft, true);
});

test('a later asset conflict stops before uploading an earlier missing asset', async t => {
  const f = fixture(t), g = github(f); await g.publish();
  g.state.assets.shift();
  g.state.assets.at(-1).digest = `sha256:${'0'.repeat(64)}`;
  const before = g.state.calls.length;
  await assert.rejects(g.publish(), /Remote release asset differs/);
  assert.ok(g.state.calls.slice(before).every(c => c.method === 'GET'));
});

test('immutable releases are verified when complete and never repaired when incomplete', async t => {
  const f = fixture(t), g = github(f); await g.publish(); g.state.release.immutable = true;
  const before = g.state.calls.length;
  assert.equal((await g.publish()).alreadyPublished, true);
  g.state.assets.pop();
  await assert.rejects(g.publish(), /Immutable release is incomplete/);
  assert.ok(g.state.calls.slice(before).every(c => c.method === 'GET'));
});

test('unknown attachments are preserved and block publication', async t => {
  const f = fixture(t), g = github(f);
  g.state.release = {id:100, tag_name:f.context.tag, draft:false, body:''};
  g.state.assets = [{id:1, name:'unexpected.zip', state:'uploaded', size:20}];
  await assert.rejects(g.publish(), /Unknown or duplicate remote asset/);
  assert.equal(g.state.assets.length, 1);
  assert.ok(g.state.calls.every(c => c.method === 'GET'));
});

test('empty starter with data or a digest is not deleted', async t => {
  for (const update of [{size:1}, {digest:`sha256:${'0'.repeat(64)}`}]) {
    const f = fixture(t), g = github(f);
    g.state.release = {id:100, tag_name:f.context.tag, draft:false, body:''};
    g.state.assets = [{id:1, name:'BUILD.json', state:'starter', size:0, digest:null, ...update}];
    await assert.rejects(g.publish(), /Remote release asset differs/);
    assert.ok(g.state.calls.every(c => c.method === 'GET'));
  }
});

test('a release returned for the wrong tag is never modified', async t => {
  const f = fixture(t), g = github(f);
  g.state.release = {id:100, tag_name:'v9.9.9', draft:false};
  await assert.rejects(g.publish(), /different tag/);
  assert.ok(g.state.calls.every(c => c.method === 'GET'));
});

test('release errors are visible as escaped Actions annotations', () => {
  const error = new Error('Upload failed: 100%\r\nKeep the draft.\n::warning::untrusted line');
  assert.equal(formatReleaseError(error), error.message);
  assert.equal(formatReleaseError(error, true), '::error::Upload failed: 100%25%0D%0AKeep the draft.%0A::warning::untrusted line');
});
