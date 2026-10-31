import {test} from 'node:test';
import assert from 'node:assert/strict';
import {createHash} from 'node:crypto';
import {existsSync, readFileSync, writeFileSync} from 'node:fs';
import {mirrorOptions, mirrorTelegramImage} from '../scripts/mirror-telegram.mjs';

const options = {version:'v0.2610.0', repository:'fixture/matrix-admin', actor:'fixture', token:'fixture-secret-token'};
const raw = Buffer.from(JSON.stringify({schemaVersion:2, mediaType:'application/vnd.oci.image.index.v1+json',
  manifests:[{digest:'sha256:'+'a'.repeat(64), platform:{os:'linux', architecture:'amd64'}},
    {digest:'sha256:'+'b'.repeat(64), platform:{os:'linux', architecture:'arm64'}}]})+'\n');
const digest = 'sha256:'+createHash('sha256').update(raw).digest('hex');
const config = Buffer.from(JSON.stringify({os:'linux', architecture:'amd64'}));

function fixture(update = {}) {
  const state = {calls:[], reports:[], authFile:null, ...update};
  const response = (stdout = '', status = 0, stderr = '') => ({stdout:Buffer.from(stdout), status, stderr:Buffer.from(stderr)});
  const run = async (args, input) => {
    state.calls.push({args, input});
    assert.ok(!args.some(arg=>arg.includes(options.token)), 'token must not be in process arguments');
    if (args[0] === 'login') {
      state.authFile = args[args.indexOf('--authfile')+1];
      writeFileSync(state.authFile, options.token);
      assert.equal(input, options.token+'\n');
      return state.loginError ? response('', 1, options.token+' login failed') : response();
    }
    if (args[0] === 'copy') return state.copyError ? response('', 1, 'destination denied') : response();
    if (args.includes('--config')) return response(state.wrongPlatform ? '{"os":"linux","architecture":"arm64"}' : config);
    if (args.at(-1).startsWith('docker://dock.mau.dev/')) return state.upstreamError ? response('', 1, 'HTTP 451: upstream refused') : response(raw);
    if (args.includes('--no-creds')) {
      assert.deepEqual(JSON.parse(readFileSync(args[args.indexOf('--authfile')+1])), {auths:{}});
      if (state.private) return response('', 1, 'unauthorized: authentication required');
      if (state.networkError) return response('', 1, 'TLS handshake timeout');
      return response(state.publicMismatch ? '{}\n' : raw);
    }
    return response(state.destinationMismatch ? '{}\n' : raw);
  };
  const invoke = extra => mirrorTelegramImage({...options, ...extra}, {run, record:value=>state.reports.push(value)});
  return {state, invoke};
}

test('mirror accepts only version tags and a valid expected digest and repository', () => {
  assert.equal(mirrorOptions(options).destination, 'ghcr.io/fixture/matrix-telegram');
  for (const change of [{version:'latest'}, {version:'main'}, {version:'v1.2\nanything'}, {version:'v1.2; echo bad'},
    {expectedDigest:'not-a-digest'}, {repository:'../elsewhere'}, {repository:'fixture/repo\n'}, {actor:'bad actor'}, {token:''}]) {
    assert.throws(()=>mirrorOptions({...options, ...change}));
  }
});

test('upstream refusal stops before login or publication', async () => {
  const f = fixture({upstreamError:true});
  await assert.rejects(f.invoke(), /HTTP 451/);
  assert.equal(f.state.calls.length, 1);
  assert.deepEqual(f.state.reports, []);
});

test('a different expected digest stops before any authenticated request', async () => {
  const f = fixture();
  await assert.rejects(f.invoke({expectedDigest:'sha256:'+'f'.repeat(64)}), /expected digest/);
  assert.equal(f.state.calls.length, 1);
});

test('an image without the supported architecture cannot be published', async () => {
  const f = fixture({wrongPlatform:true});
  await assert.rejects(f.invoke(), /linux\/amd64/);
  assert.ok(!f.state.calls.some(c=>c.args[0]==='copy' || c.args[0]==='login'));
});

test('copy uses the frozen digest, preserves all platforms, and verifies anonymous access', async () => {
  const f = fixture();
  const result = await f.invoke({expectedDigest:digest});
  assert.equal(result.pull_reference, `ghcr.io/fixture/matrix-telegram@${digest}`);
  assert.equal(result.copied, true);
  assert.equal(result.anonymous_pull_verified, true);
  const copy = f.state.calls.find(c=>c.args[0]==='copy').args;
  assert.ok(copy.includes('--all') && copy.includes('--preserve-digests'));
  assert.equal(copy.at(-2), `docker://dock.mau.dev/mautrix/telegram@${digest}`);
  assert.equal(copy.at(-1), `docker://ghcr.io/fixture/matrix-telegram:v0.2610.0-sha256-${digest.slice(7)}`);
  assert.equal(f.state.calls.filter(c=>c.args.at(-1).endsWith(':v0.2610.0')).length, 1);
  assert.equal(existsSync(f.state.authFile), false);
  assert.ok(!JSON.stringify(f.state.reports).includes(options.token));
});

test('a private new package is copied but not reported as ready for anonymous recovery', async () => {
  const f = fixture({private:true});
  const result = await f.invoke();
  assert.equal(result.copied, true);
  assert.equal(result.anonymous_pull_verified, false);
  assert.match(result.next_action, /Public/);
  assert.equal(existsSync(f.state.authFile), false);
});

test('failed writes never record a completed mirror', async () => {
  const f = fixture({copyError:true});
  await assert.rejects(f.invoke(), /Copy official image/);
  assert.ok(f.state.reports.every(r=>!r.copied));
  assert.equal(existsSync(f.state.authFile), false);
});

test('a changed destination manifest is rejected', async () => {
  const f = fixture({destinationMismatch:true});
  await assert.rejects(f.invoke(), /GHCR manifest digest mismatch/);
  assert.ok(f.state.reports.every(r=>!r.copied));
});

test('an anonymous digest mismatch is never accepted', async () => {
  const f = fixture({publicMismatch:true});
  await assert.rejects(f.invoke(), /Anonymous GHCR digest mismatch/);
  assert.ok(f.state.reports.every(r=>!r.anonymous_pull_verified));
});

test('anonymous network failures are not mislabeled as a private package', async () => {
  const f = fixture({networkError:true});
  await assert.rejects(f.invoke(), /TLS handshake timeout/);
  assert.ok(f.state.reports.every(r=>!r.next_action));
});

test('login errors redact the token and clean up the credential file', async () => {
  const f = fixture({loginError:true});
  await assert.rejects(f.invoke(), error=>!error.message.includes(options.token) && error.message.includes('[REDACTED]'));
  assert.equal(existsSync(f.state.authFile), false);
  assert.ok(!f.state.calls.some(c=>c.args[0]==='copy'));
});
