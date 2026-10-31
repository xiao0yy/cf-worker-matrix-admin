import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync,mkdirSync,writeFileSync,readFileSync,rmSync,symlinkSync,utimesSync} from 'node:fs';
import {join} from 'node:path';
import {tmpdir} from 'node:os';
import {inventory,packRelease} from '../../scripts/release-lib.mjs';

test('release packaging is deterministic despite source file timestamps',t=>{
  const root=mkdtempSync(join(tmpdir(),'matrix-package-'));t.after(()=>rmSync(root,{recursive:true,force:true}));
  const stage=join(root,'stage');mkdirSync(stage);writeFileSync(join(stage,'file.txt'),'fixture',{mode:0o600});
  const metadata={version:'0.3.0',commit:'a'.repeat(40),timestamp:1,origin:{platform:'linux/amd64',protocols:{control:1,mailbox:1,recovery:2},restic_version:'0.19.1'}};
  const first=packRelease(stage,join(root,'first'),metadata);
  utimesSync(join(stage,'file.txt'),10000,10000);
  const second=packRelease(stage,join(root,'second'),metadata);
  assert.deepEqual(second,first);
  assert.deepEqual(readFileSync(join(root,'first/matrix-admin-linux-amd64.tar.gz')),readFileSync(join(root,'second/matrix-admin-linux-amd64.tar.gz')));
  symlinkSync('/etc/passwd',join(stage,'unexpected-link'));
  assert.throws(()=>inventory(stage),/Symlink/);
});
