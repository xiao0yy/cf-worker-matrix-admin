import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync, rmSync, readFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {fileURLToPath} from 'node:url';
import {shell, run} from './fixtures.mjs';

const id = 'sha256:' + 'a'.repeat(64);
const reference = 'ghcr.io/example/matrix:v1@' + id;
const helper = fileURLToPath(new URL('../backup/image-reference.sh', import.meta.url));

function check(t, ref = reference, containerID = id, resolvedID = id) {
  const work = mkdtempSync(join(tmpdir(), 'matrix-image-reference-'));
  t.after(() => rmSync(work, {recursive:true, force:true}));
  shell(join(work, 'docker'), `
if [[ "$1" == inspect ]]; then
  printf '%s\\n' "$CONTAINER_JSON"
elif [[ "$1 $2 $3" == 'image inspect --format' ]]; then
  printf '%s\\n' "$5" > "$TEST_ROOT/resolved-ref"
  printf '%s\\n' "$RESOLVED_ID"
else
  # The old RepoDigests lookup would pick this non-downloadable alias first.
  printf '[{"RepoDigests":["matrix-recovery/tuwunel@%s","%s"]}]\\n' "$EXPECTED_ID" "$CONFIGURED_REF"
fi
`);
  shell(join(work, 'check'), 'source "$1"\nimage_reference tuwunel "$EXPECTED_ID"\n');
  const result = run(join(work, 'check'), [helper], {
    ...process.env, DOCKER_BIN:join(work, 'docker'), TEST_ROOT:work,
    EXPECTED_ID:id, CONFIGURED_REF:ref, RESOLVED_ID:resolvedID,
    CONTAINER_JSON:JSON.stringify({Image:containerID, Config:{Image:ref}}),
  });
  return {result, work};
}

test('backup pins the configured registry digest despite local aliases', t => {
  const {result, work} = check(t);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout.trim(), reference);
  assert.equal(readFileSync(join(work, 'resolved-ref'), 'utf8').trim(), reference);
});

for (const ref of ['ghcr.io/example/matrix:latest', 'matrix-recovery/tuwunel@'+id, id, 'ghcr.io/example/matrix@sha256:nope']) {
  test(`rejects non-recoverable container reference ${ref}`, t => {
    assert.notEqual(check(t, ref).result.status, 0);
  });
}

test('refuses a container changed after the snapshot', t => {
  assert.notEqual(check(t, reference, 'sha256:'+'b'.repeat(64)).result.status, 0);
});

test('refuses a registry reference resolving to a different image', t => {
  assert.notEqual(check(t, reference, id, 'sha256:'+'c'.repeat(64)).result.status, 0);
});
