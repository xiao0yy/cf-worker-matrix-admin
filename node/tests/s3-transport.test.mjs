import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync, rmSync, readFileSync} from 'node:fs';
import {join} from 'node:path';
import {tmpdir} from 'node:os';
import {repository, host, run, successful, shell, write} from './fixtures.mjs';

test('R2 transport signs empty GET/HEAD bodies and PUT payloads while preserving conditional writes', t => {
  const work = mkdtempSync(join(tmpdir(), 'matrix-s3-transport-'));
  t.after(() => rmSync(work, {recursive:true, force:true}));
  const h = host(work);
  write(join(work, 'payload'), 'fixture recovery point\n');
  const script = join(work, 'transport.sh');
  shell(script, `
source "$1/node/backup/s3-common.sh"
WORK="$TEST_ROOT"
AUTH=
trap '[[ -z "$AUTH" ]] || rm -f -- "$AUTH"' EXIT
load_s3 "$TEST_ROOT/node/config/s3.env"
key="$RECOVERY_PREFIX/latest.json"
status=0
s3 GET "$key" "$WORK/missing" || status=$?
[[ "$status" == 44 ]]
s3 PUT "$key" "$WORK/payload" 'If-None-Match: *'
s3 GET "$key" "$WORK/read-back"
cmp "$WORK/payload" "$WORK/read-back"
etag=$(object_etag)
s3 HEAD "$key" "$WORK/head"
status=0
s3 PUT "$key" "$WORK/payload" 'If-None-Match: *' || status=$?
[[ "$status" == 42 ]]
printf 'next point\\n' >"$WORK/payload"
s3 PUT "$key" "$WORK/payload" "If-Match: $etag"
status=0
s3 PUT "$key" "$WORK/payload" "If-Match: $etag" || status=$?
[[ "$status" == 42 ]]
s3 GET "$key" "$WORK/final"
cmp "$WORK/payload" "$WORK/final"
`);
  successful(run(script, [repository], h.env));
  assert.equal(readFileSync(join(work, 'final'), 'utf8'), 'next point\n');
  assert.match(readFileSync(join(work, 'head'), 'utf8'), /^ETag: "[a-f0-9]{64}"/);
});
