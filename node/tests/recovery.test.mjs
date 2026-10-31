import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync,rmSync,readFileSync,existsSync,cpSync,mkdirSync,appendFileSync,symlinkSync,readdirSync} from 'node:fs';
import {join} from 'node:path';
import {tmpdir} from 'node:os';
import {execFileSync} from 'node:child_process';
import {release,sealRelease,bundle,host,point,run,successful,write,json,hashFile,sealBundle,shell} from './fixtures.mjs';

function setup(t){
  const work=mkdtempSync(join(tmpdir(),'matrix-node-test-'));t.after(()=>rmSync(work,{recursive:true,force:true}));
  const code=join(work,'release'),software=release(code),data=bundle(work,software),h=host(work),snapshot=point(work,software);
  return {work,code,software,data,snapshot,...h};
}
const parse=p=>JSON.parse(readFileSync(p,'utf8'));
const trace=work=>existsSync(join(work,'trace'))?readFileSync(join(work,'trace'),'utf8'):'';
const reader=(f,...args)=>{
  const result=run(join(f.code,'backup/recover.sh'),['--target',f.root,...args],f.env);
  if(result.status!==0){
    const work=result.stdout.match(/Recovery workspace: (\S+)/)?.[1];
    if(work?.startsWith(f.root+'/')&&existsSync(join(work,'validation.log')))result.stderr+='\n'+readFileSync(join(work,'validation.log'),'utf8');
    result.stderr+='\nFixture trace:\n'+trace(f.work);
  }
  return result;
};

test('fresh standby uses a public release and private config only; rerun preserves identity',t=>{
  const f=setup(t);const input=readFileSync(join(f.root,'config/s3.env'),'utf8');
  successful(reader(f,'--prepare-standby'));
  const cfg=join(f.root,'deploy/control/local.json');const first=hashFile(cfg);
  assert.notEqual(parse(cfg).instance_id,'source');
  assert.equal(readFileSync(join(f.root,'config/s3.env'),'utf8'),input);
  assert.equal(parse(join(f.root,'agent/registry.json')).s3_config,join(f.root,'config/s3.env'));
  assert.ok(existsSync(join(f.root,'deploy/control/bin/matrix-mailbox')));
  assert.deepEqual(readdirSync(join(f.root,'generations')),[]);
  assert.match(trace(f.work),/matrix-mailbox.timer/);assert.match(trace(f.work),/matrix-handover.timer/);
  assert.doesNotMatch(trace(f.work),/docker (run|pull|load|start)|tuwunel-restic.timer|restic (cat|snapshots|restore)/);
  assert.equal(readFileSync(join(f.root,'deploy/control/s3.env'),'utf8'),input);
  assert.doesNotMatch(readFileSync(join(f.work,'s3-trace'),'utf8'),/repositories\//);
  assert.doesNotMatch(readFileSync(join(f.work,'s3-trace'),'utf8'),/releases\/|artifacts\/|\.sh/);
  const count=trace(f.work).split('\n').length;
  successful(reader(f,'--prepare-standby'));
  assert.equal(hashFile(cfg),first);assert.equal(trace(f.work).split('\n').length,count+2); // Docker readiness checks only
  successful(reader(f,'--check-foundation'));
});

test('point corruption and unsafe destination fail before installing services',t=>{
  const f=setup(t);
  appendFileSync(join(f.work,'bucket/cluster/fixture/recovery/v2/points',`${f.snapshot}.json`),' ');
  assert.notEqual(reader(f,'--prepare-standby').status,0);
  assert.ok(!existsSync(join(f.root,'deploy')));assert.doesNotMatch(trace(f.work),/systemctl/);
  point(f.work,f.software);mkdirSync(join(f.root,'generations','existing'));
  assert.notEqual(reader(f,'--prepare-standby').status,0);
  assert.ok(existsSync(join(f.root,'generations','existing')));
});

test('R2 configuration hash and payload schema fail closed before installing services',t=>{
  const f=setup(t);const key=join(f.work,'bucket/cluster/fixture/recovery/v2/points',`${f.snapshot}.json`);
  const value=parse(key),capsule=join(f.work,'bucket',value.configuration_object);
  appendFileSync(capsule,' ');
  assert.notEqual(reader(f,'--prepare-standby').status,0);
  assert.ok(!existsSync(join(f.root,'deploy')));assert.doesNotMatch(trace(f.work),/systemctl/);
  point(f.work,f.software);
  const current=parse(key),bad=parse(join(f.work,'bucket',current.configuration_object));
  bad.files[0].path='control/bin/evil';
  const replacement=join(f.work,'replacement.json');json(replacement,bad);
  current.configuration_sha256=hashFile(replacement);current.configuration_object=`cluster/fixture/recovery/v2/configurations/${current.configuration_sha256}.json`;
  write(join(f.work,'bucket',current.configuration_object),readFileSync(replacement));json(key,current);
  json(join(f.work,'bucket/cluster/fixture/recovery/v2/latest.json'),{protocol:2,point_schema:2,point_key:`cluster/fixture/recovery/v2/points/${f.snapshot}.json`,point_sha256:hashFile(key)});
  assert.notEqual(reader(f,'--prepare-standby').status,0);assert.ok(!existsSync(join(f.root,'deploy')));
});

test('release installation rejects destination links before replacing any public file',t=>{
  const f=setup(t),destination=join(f.work,'destination'),outside=join(f.work,'outside');
  cpSync(f.code,destination,{recursive:true});mkdirSync(outside);
  write(join(outside,'keep'),'existing unrelated data');
  appendFileSync(join(destination,'backup/AGENT.md'),'keep installed file until every destination is checked');
  const before=hashFile(join(destination,'backup/AGENT.md'));
  rmSync(join(destination,'control'),{recursive:true});symlinkSync(outside,join(destination,'control'));
  const wrapper=join(f.work,'copy-release.sh');shell(wrapper,'source "$1/backup/release-common.sh"\nrelease_copy "$1" "$2"\n');
  assert.notEqual(run(wrapper,[f.code,destination],f.env).status,0);
  assert.deepEqual(readdirSync(outside),['keep']);
  assert.equal(hashFile(join(destination,'backup/AGENT.md')),before);
});

test('code update refuses a generation change while acquiring locks',t=>{
  const f=setup(t);successful(reader(f,'--prepare-standby'));
  const control=join(f.root,'deploy/control'),cfg=parse(join(control,'local.json'));
  const state={instance_id:cfg.instance_id,bound_epoch:1,blocked:false,serving:true,control:{phase:'ONLINE',owner:cfg.instance_id}};
  json(join(f.work,'status.json'),state);json(join(f.work,'next-status.json'),{...state,bound_epoch:2});
  appendFileSync(join(f.code,'backup/tuwunel-restic.sh'),'\n# changed public release\n');
  const software=sealRelease(f.code,'0.3.1');bundle(f.work,software);point(f.work,software);
  const installed=hashFile(join(f.root,'deploy/release-manifest.json')),before=trace(f.work).length;
  const result=reader(f,'--update-foundation');assert.notEqual(result.status,0);
  assert.match(result.stderr,/state changed while acquiring update locks/);
  assert.equal(hashFile(join(f.root,'deploy/release-manifest.json')),installed);
  assert.ok(!existsSync(join(f.root,'agent/pending-update.json')));
  assert.doesNotMatch(trace(f.work).slice(before),/systemctl/);
});

test('code update propagates to active generation, preserves private state and survives interruption',t=>{
  const f=setup(t);successful(reader(f,'--prepare-standby'));
  const control=join(f.root,'deploy/control'),active=join(f.root,'generations/active');
  mkdirSync(join(active,'deploy'),{recursive:true});cpSync(join(f.root,'deploy'),join(active,'deploy'),{recursive:true});
  const cfg=parse(join(control,'local.json'));
  const saved=[join(control,'local.json'),join(control,'s3.env'),join(control,'state/journal.json')].map(p=>[p,hashFile(p)]);
  json(join(f.work,'status.json'),{instance_id:cfg.instance_id,cluster_id:'fixture',bound_epoch:1,deployment_root:active,blocked:false,serving:true,control:{phase:'ONLINE',owner:cfg.instance_id}});
  // Create another public code generation; only the backup producer changes.
  appendFileSync(join(f.code,'backup/tuwunel-restic.sh'),'\n# next release fixture\n');
  let software=sealRelease(f.code,'0.3.1');bundle(f.work,software);point(f.work,software);
  assert.notEqual(reader(f,'--check-foundation').status,0);
  const before=trace(f.work).length;
  successful(reader(f,'--update-foundation'));
  successful(reader(f,'--check-foundation'));
  assert.equal(hashFile(join(f.root,'deploy/backup/tuwunel-restic.sh')),hashFile(join(active,'deploy/backup/tuwunel-restic.sh')));
  assert.doesNotMatch(trace(f.work).slice(before),/(stop|start) matrix-control.service/);
  for(const [p,h]of saved)assert.equal(hashFile(p),h);
  // A changed controller must never replace the running controller online.
  appendFileSync(join(f.code,'control/bin/matrix-control'),'\n# new controller\n');
  software=sealRelease(f.code,'0.3.2');bundle(f.work,software);point(f.work,software);
  assert.notEqual(reader(f,'--update-foundation').status,0);
  assert.ok(!existsSync(join(f.root,'agent/pending-update.json')));
  let status=parse(join(f.work,'status.json'));status.blocked=true;status.serving=false;status.control.phase='STARTING';status.handover={may_have_written:true};json(join(f.work,'status.json'),status);
  assert.notEqual(reader(f,'--update-foundation').status,0);
  assert.ok(!existsSync(join(f.root,'agent/pending-update.json')));
  status.control.phase='RELEASED';status.handover={phase:'RELEASED',may_have_written:false};json(join(f.work,'status.json'),status);
  write(join(f.work,'fail-start'),'');
  assert.notEqual(reader(f,'--update-foundation').status,0);
  assert.ok(existsSync(join(f.root,'agent/pending-update.json')));
  json(join(control,'state/journal.json'),{blocked:true,bound_epoch:1,deployment_root:active,handover:{phase:'RELEASED',may_have_written:false}});
  const journalHash=hashFile(join(control,'state/journal.json'));
  rmSync(join(f.work,'fail-start'));
  // Even with the controller reachable, an old update cannot resume at a new epoch.
  rmSync(join(f.work,'stopped'));
  json(join(f.work,'status.json'),{...status,bound_epoch:2});
  const stale=reader(f,'--update-foundation');assert.notEqual(stale.status,0);
  assert.match(stale.stderr,/state changed since the interrupted update/);
  assert.ok(existsSync(join(f.root,'agent/pending-update.json')));
  json(join(f.work,'status.json'),status);write(join(f.work,'stopped'),'');
  // A partially replaced generation is repaired from the same pinned intent.
  appendFileSync(join(active,'deploy/backup/tuwunel-restic.sh'),'partial');
  successful(reader(f,'--update-foundation'));
  successful(reader(f,'--check-foundation'));
  assert.ok(!existsSync(join(f.root,'agent/pending-update.json')));
  assert.equal(hashFile(join(control,'state/journal.json')),journalHash);
  assert.equal(hashFile(saved[0][0]),saved[0][1]);assert.equal(hashFile(saved[1][0]),saved[1][1]);
});

test('publisher commits immutable R2 config/repository records; final publication never advances latest',t=>{
  const f=setup(t);const state=join(f.work,'backup-state');mkdirSync(join(state,'public-releases'),{recursive:true});
  rmSync(join(f.work,'bucket/cluster/fixture/recovery/v2/points'),{recursive:true});
  // This is a previously downloaded, verified public-release cache.
  cpSync(f.code,join(state,'public-releases',f.software.manifest_sha256),{recursive:true});
  cpSync(join(f.root,'config/s3.env'),join(f.code,'control/s3.env'));
  json(join(f.code,'control/local.json'),{cluster_id:'fixture',instance_id:'source'});write(join(f.code,'control/managed'),'{}');
  let metadata=parse(join(f.data,'metadata.json'));
  Object.assign(metadata,{server_name:'matrix.example',media_included:false,media:{backup:'external-s3'},telegram_included:true,telegram:{schema:1},image_id:'sha256:'+'b'.repeat(64),images:{},data_time:'2026-10-29T00:00:00Z',instance_control:{schema:1,cluster_id:'fixture',source_instance_id:'source',source_epoch:1}});
  json(join(f.data,'metadata.json'),metadata);
  json(join(state,'last-success.json'),{snapshot_id:f.snapshot,repository_id:'fixture-repository',data_time:metadata.data_time,verification:'full-restore',database_startup_verified:true});
  const config=join(f.work,'publish.env');write(config,`S3_ENDPOINT=https://s3.test.backblazeb2.com\nS3_REGION=test\nS3_BUCKET=test\nAWS_ACCESS_KEY_ID=b2-fixture\nAWS_SECRET_ACCESS_KEY=b2-fixture\nSTATE_DIR=${state}\nRESTIC_PASSWORD=fixture-private-password\nRESTIC_REPOSITORY=s3:https://s3.test.backblazeb2.com/test/repository\n`);
  json(join(f.work,'status.json'),{serving:true,blocked:false,cluster_id:'fixture',instance_id:'source',bound_epoch:1});
  const capsule=join(f.work,'publish-configuration.json');
  const pack=()=>{rmSync(capsule,{force:true});execFileSync(process.env.MATRIX_RELEASE_BINARY,['config-pack','--bundle',f.data,'--output',capsule]);};pack();
  const env={...f.env,MATRIX_BACKUP_CONFIG:config,RECOVERY_BUNDLE:f.data,RECOVERY_CONFIG_FILE:capsule};
  successful(run(join(f.code,'backup/publish-recovery.sh'),[],env));
  let writes=readFileSync(join(f.work,'s3-trace'),'utf8').split('\n').filter(l=>l.startsWith('PUT '));
  assert.equal(writes.length,4);for(const line of writes)assert.match(line,/^PUT cluster\/fixture\/recovery\/v2\/(repositories\/[^/]+\.json|configurations\/[a-f0-9]{64}\.json|points\/[a-f0-9]{64}\.json|latest\.json)$/);
  const latest=hashFile(join(f.work,'bucket/cluster/fixture/recovery/v2/latest.json'));
  const published=hashFile(join(state,'last-published.json'));
  write(join(f.work,'conflict-latest'),'');
  const conflict=run(join(f.code,'backup/publish-recovery.sh'),[],env);
  assert.notEqual(conflict.status,0);assert.match(conflict.stderr,/R2 latest changed/);
  assert.equal(hashFile(join(state,'last-published.json')),published);
  assert.equal(hashFile(join(f.work,'bucket/cluster/fixture/recovery/v2/latest.json')),latest);
  rmSync(join(f.work,'conflict-latest'));
  successful(run(join(f.code,'backup/publish-recovery.sh'),[],env)); // immutable retry
  const catalog=parse(join(f.work,'bucket/cluster/fixture/recovery/v2/points',`${f.snapshot}.json`));
  const configObject=join(f.work,'bucket',catalog.configuration_object),original=readFileSync(configObject);
  appendFileSync(configObject,'corrupt');
  const corrupt=run(join(f.code,'backup/publish-recovery.sh'),[],env);
  assert.notEqual(corrupt.status,0);assert.match(corrupt.stderr,/Immutable R2 object differs/);
  assert.equal(hashFile(join(f.work,'bucket/cluster/fixture/recovery/v2/latest.json')),latest);
  write(configObject,original);

  metadata.consistency='globally-quiesced';metadata.operation_id='final-test';json(join(f.data,'metadata.json'),metadata);pack();
  json(join(state,'last-success.json'),{snapshot_id:'b'.repeat(64),repository_id:'fixture-repository',data_time:metadata.data_time,verification:'full-restore',database_startup_verified:true});
  json(join(f.work,'status.json'),{blocked:true,serving:false,instance_id:'source',bound_epoch:1,final_backup:{id:'final-test',phase:'SNAPSHOTTING'},control:{phase:'SNAPSHOTTING',operation:{id:'final-test'}}});
  successful(run(join(f.code,'backup/publish-recovery.sh'),[],{...env,RECOVERY_COMMIT_MODE:'final'}));
  assert.equal(hashFile(join(f.work,'bucket/cluster/fixture/recovery/v2/latest.json')),latest);
  assert.equal(parse(join(state,'final-published-final-test.json')).snapshot_id,'b'.repeat(64));
});

test('isolated restore wires real SQLite and public tools without restoring run permission',t=>{
  const f=setup(t),p=join(f.data,'payload');
  const image='sha256:'+'b'.repeat(64),ref='registry.example/matrix@'+image;
  write(join(p,'database/CURRENT'),'MANIFEST-1\n');write(join(p,'database/MANIFEST-1'),'fixture\n');
  write(join(p,'config/registration-token'),'fixture');write(join(p,'config/s3/media.env'),'MEDIA_FIXTURE=true\n');
  json(join(p,'config/docker-compose.yml'),{services:{tuwunel:{image:ref,restart:'always',environment:{TUWUNEL_SERVER_NAME:'matrix.example'}}},networks:{default:{ipam:{}}}});
  json(join(p,'telegram/config/docker-compose.yml'),{services:{telegram:{image:ref,restart:'always'}}});
  write(join(p,'telegram/data/config.yaml'),'fixture');write(join(p,'telegram/data/registration.yaml'),'fixture');
  const tables=['user_login','crypto_account','crypto_olm_session','crypto_megolm_inbound_session','crypto_megolm_outbound_session','portal','message'];
  execFileSync('sqlite3',[join(p,'telegram/data/telegram.db'),tables.map(n=>`CREATE TABLE ${n}(id INTEGER); INSERT INTO ${n} VALUES(1);`).join('\n')]);
  const telegram={schema:1,method:'sqlite-cold-backup',image_id:image,state_counts:Object.fromEntries(tables.map(n=>[n,1])),network:{name:'matrix-fixture',subnet:'172.20.0.0/16',gateway:'172.20.0.1'}};
  json(join(p,'telegram/metadata.json'),telegram);
  const key=join(f.work,'bucket/cluster/fixture/recovery/v2/points',`${f.snapshot}.json`),value=parse(key);
  Object.assign(value,{server_name:'matrix.example',image_id:image,images:{tuwunel:{ref,id:image},telegram:{ref,id:image}},telegram,consistency:'globally-quiesced',operation_id:'fixture-op'});
  json(key,value);json(join(f.work,'bucket/cluster/fixture/recovery/v2/latest.json'),{protocol:2,point_schema:2,point_key:`cluster/fixture/recovery/v2/points/${f.snapshot}.json`,point_sha256:hashFile(key)});
  json(join(f.data,'metadata.json'),{...value,format:value.backup_format,media_included:false,telegram_included:true});sealBundle(f.data);point(f.work,f.software);
  shell(join(f.work,'mock/docker'),`
echo "docker $*" >>"$TEST_ROOT/trace"
case "$1" in
 info) exit 0;;
 image) case "\${4:-}" in '{{.Id}}') printf 'sha256:%064s\\n' '' | tr ' ' b;;'{{.Os}}/{{.Architecture}}') echo linux/amd64;;*) echo '[]';;esac;;
 compose)
  shift;file=
  while (($#));do case "$1" in version) exit 0;;-f) file=$2;shift 2;;--project-directory) shift 2;;--quiet) exit 0;;config|--format) shift;;json) shift;;*) exit 99;;esac;done
  [[ -n "$file" ]];cat "$file";;
 run) [[ " $* " == *' --network none '* && " $* " != *' -p '* ]];echo fixture-container;;
 logs) printf '%s\\n' '{"message":"Starting Matrix connector"}' '{"message":"Database is up to date","db_section":"matrix_state"}' '{"message":"Starting HTTP listener"}';;
 inspect) [[ "$3" == '{{.State.ExitCode}}' ]];echo 0;;
 stop|rm) exit 0;;
 *) exit 99;;
esac
`);
  const result=join(f.work,'result.json');successful(reader(f,'--result-file',result));
  const report=parse(result),root=report.deployment_root;
  assert.equal(report.mode,'isolated');assert.ok(report.telegram_database_startup_verified);
  assert.ok(!existsSync(join(root,'deploy/control/local.json')));assert.ok(!existsSync(join(root,'deploy/control/managed')));
  assert.equal(parse(join(root,'deploy/tuwunel/docker-compose.yml')).services.tuwunel.restart,'no');
  assert.equal(parse(join(root,'deploy/telegram/docker-compose.yml')).services.telegram.restart,'no');
  assert.equal(hashFile(join(root,'deploy/release-manifest.json')),f.software.manifest_sha256);
  assert.doesNotMatch(trace(f.work),/systemctl|docker start |compose .* up/);
});

test('explicit software upgrade needs no newer data point and defaults to check-only',t=>{
  const f=setup(t);successful(reader(f,'--prepare-standby'));
  appendFileSync(join(f.code,'backup/tuwunel-restic.sh'),'\n# explicit upgrade\n');sealRelease(f.code,'0.3.1');
  const upgrade=(apply=false)=>run(join(f.code,'backup/upgrade.sh'),['--release',f.code,'--target',f.root,...(apply?['--apply']:[])],f.env);
  const before=readFileSync(join(f.work,'s3-trace'),'utf8');
  assert.notEqual(upgrade().status,0);
  successful(upgrade(true));successful(upgrade());
  assert.equal(readFileSync(join(f.work,'s3-trace'),'utf8'),before);
  assert.equal(hashFile(join(f.root,'deploy/release-manifest.json')),hashFile(join(f.code,'release-manifest.json')));
});

test('R2 cluster mismatch stops before B2 access or service installation',t=>{
 const f=setup(t),key=join(f.work,'bucket/cluster/fixture/recovery/v2/points',`${f.snapshot}.json`),value=parse(key);
 value.instance_control.cluster_id='other';json(key,value);
 json(join(f.work,'bucket/cluster/fixture/recovery/v2/latest.json'),{protocol:2,point_schema:2,point_key:`cluster/fixture/recovery/v2/points/${f.snapshot}.json`,point_sha256:hashFile(key)});
 const result=reader(f,'--prepare-standby');assert.notEqual(result.status,0);
 assert.ok(!existsSync(join(f.root,'deploy')));assert.doesNotMatch(trace(f.work),/restic (cat|restore)|systemctl/);
});
