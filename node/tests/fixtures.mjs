import {readFileSync,writeFileSync,mkdirSync,cpSync,rmSync,existsSync,chmodSync,readdirSync} from 'node:fs';
import {join,dirname} from 'node:path';
import {fileURLToPath} from 'node:url';
import {execFileSync,spawnSync} from 'node:child_process';
import {inventory,sha256} from '../../scripts/release-lib.mjs';

export const repository=fileURLToPath(new URL('../../',import.meta.url));
export function write(file,text,mode=0o600){mkdirSync(dirname(file),{recursive:true});writeFileSync(file,text,{mode});chmodSync(file,mode);}
export function shell(file,text){write(file,'#!/usr/bin/env bash\nset -Eeuo pipefail\n'+text,0o700);}
export function json(file,value){write(file,JSON.stringify(value,null,2)+'\n');}
export const hashFile=p=>sha256(readFileSync(p));
export function sealRelease(root,version='0.3.0'){
  rmSync(join(root,'release-manifest.json'),{force:true});
  rmSync(join(root,'control/software.sha256'),{force:true});
  write(join(root,'control/software.sha256'),inventory(join(root,'control')).map(f=>`${f.sha256}  ${f.path}\n`).join(''));
  const files=inventory(root).filter(f=>f.path==='release.json'||f.path.startsWith('backup/')||f.path.startsWith('control/'));
  const manifest={schema:1,project:'matrix-admin',version,commit:'a'.repeat(40),platform:'linux/amd64',protocols:{control:1,mailbox:1,recovery:2},restic_version:'0.19.1',archive:{name:'matrix-admin-linux-amd64.tar.gz',sha256:'a'.repeat(64),size:1},files};
  json(join(root,'release-manifest.json'),manifest);
  return {project:'matrix-admin',version,manifest_sha256:hashFile(join(root,'release-manifest.json'))};
}
export function release(root){
  for(const part of ['backup','control']){
    mkdirSync(join(root,part),{recursive:true});
    for(const file of readdirSync(join(repository,'node',part))){
      if(part==='control'&&!/\.(sh|service|timer|conf)$/.test(file))continue;
      if(part==='backup'&&!/\.(sh|md|json|example|service|timer)$/.test(file))continue;
      cpSync(join(repository,'node',part,file),join(root,part,file),{recursive:true});
      chmodSync(join(root,part,file),file.endsWith('.sh')?0o700:0o600);
    }
  }
  json(join(root,'release.json'),JSON.parse(readFileSync(join(repository,'release.config.json'),'utf8')));
  const binary=process.env.MATRIX_RELEASE_BINARY;
  if(!binary||!existsSync(binary))throw new Error('Run npm run test:node to build the release verifier first');
  mkdirSync(join(root,'backup/bin'),{recursive:true});cpSync(binary,join(root,'backup/bin/matrix-release'));chmodSync(join(root,'backup/bin/matrix-release'),0o700);
  shell(join(root,'control/bin/matrix-control'),`
case "$1" in
 capabilities) echo '{"schema":1,"planned_handover":true}';;
 prepare-standby)
  echo prepare-standby >>"$TEST_ROOT/trace"
  cfg=$3;state=$(jq -er .state_dir "$cfg");mkdir -p "$state"
  jq '{schema:1,instance_id,cluster_id,bound_epoch:0,blocked:true}' "$cfg" >"$state/journal.json" ;;
 status)
  [[ ! -e "$TEST_ROOT/stopped" ]]
  if [[ -f "$TEST_ROOT/status.json" ]];then cat "$TEST_ROOT/status.json";else
   jq '{schema:1,instance_id,cluster_id,bound_epoch:0,blocked:true,serving:false,control:{phase:"ONLINE",owner:"source"}}' "$3"
  fi
  if [[ -f "$TEST_ROOT/next-status.json" ]];then mv "$TEST_ROOT/next-status.json" "$TEST_ROOT/status.json";fi ;;
 final-verify) exit 0;;
 *) exit 99;;
esac
`);
  shell(join(root,'control/bin/matrix-mailbox'),'exit 0\n');
  shell(join(root,'backup/bin/restic'),`
echo "restic $*" >>"$TEST_ROOT/trace"
[[ \${1:-} != -o ]] || shift 2
case "$1" in
 version) echo 'restic 0.19.1 fixture';;
 cat) echo '{"id":"fixture-repository"}';;
 snapshots) printf '[{"id":"%s","tags":["verified","recovery-v2","tuwunel-checkpoint"]}]\\n' "$FIXTURE_SNAPSHOT";;
 restore)
  shift;target=;partial=false
  while (($#));do case "$1" in --target) target=$2;shift 2;;--include) partial=true;shift 2;;*) shift;;esac;done
  [[ -n "$target" ]];mkdir -p "$target/bundle/payload"
  mkdir -p "$target/bundle/payload/telegram/data"
  cp -a "$TEST_ROOT/bundle/payload/database" "$target/bundle/payload/"
  cp -a "$TEST_ROOT/bundle/payload/telegram/data/telegram.db" "$target/bundle/payload/telegram/data/";;
 *) exit 99;;
esac
`);
  return sealRelease(root);
}
export function sealBundle(root){
  const files=inventory(join(root,'payload'));
  write(join(root,'manifest.sha256'),files.map(f=>`${f.sha256}  ./${f.path}\n`).join(''));
}
export function bundle(work,software){
  const root=join(work,'bundle');
  json(join(root,'metadata.json'),{format:'tuwunel-checkpoint-v2',recovery_version:2,data_layout:'database-only-v1',software,instance_control:{schema:1,cluster_id:'fixture'}});
  json(join(root,'payload/control/source-local.json'),{schema:1,cluster_id:'fixture',instance_id:'source',s3_config:'/source/deploy/control/s3.env',state_dir:'/source/deploy/control/state',runtime_dir:'/run/matrix-control',generations_dir:'/source/generations',services:[{name:'tuwunel',data_path:'/source/db'},{name:'telegram-bridge',data_path:'/source/tg'}]});
  write(join(root,'payload/nginx/matrix.conf'),'include /source/deploy/control/nginx-server/guard.conf;\n');
  write(join(root,'payload/nginx/00-matrix-common.conf'),'# fixture\n');
  sealBundle(root);return root;
}
export function host(work){
  for(const path of ['mock','nginx','units','node/config','node/agent','node/generations'])mkdirSync(join(work,path),{recursive:true});
  write(join(work,'nginx-main.conf'),'# fixture\n');
  shell(join(work,'mock/docker'),`
echo "docker $*" >>"$TEST_ROOT/trace"
[[ "$1" == info || "$1 $2" == 'compose version' ]] && exit 0
[[ "$1 $2" == 'container inspect' ]] && exit 1
echo 'Forbidden application Docker command' >&2;exit 99
`);
  shell(join(work,'mock/nginx'),'echo "nginx $*" >>"$TEST_ROOT/trace"\n');
  shell(join(work,'mock/systemctl'),`
echo "systemctl $*" >>"$TEST_ROOT/trace"
case "$*" in
 'stop matrix-control.service') touch "$TEST_ROOT/stopped";;
 'start matrix-control.service') [[ ! -e "$TEST_ROOT/fail-start" ]];rm -f "$TEST_ROOT/stopped";;
esac
`);
  shell(join(work,'mock/curl'),`
method=GET;output=;headers=;upload=;url=;condition=
while (($#));do case "$1" in
 --output) output=$2;shift 2;;--dump-header) headers=$2;shift 2;;--upload-file) upload=$2;shift 2;;--request) method=$2;shift 2;;--head) method=HEAD;shift;;
 --header) case "$2" in If-*) condition=$2;;esac;shift 2;;
 --config|--proto|--connect-timeout|--max-time|--retry|--retry-delay|--write-out|--max-filesize) shift 2;;
 https://*) url=$1;shift;;*) shift;;esac;done
[[ "$url" == 'https://fixture.r2.cloudflarestorage.com/control/'* ]] || { echo 'Unexpected non-R2 network request' >&2;exit 99; }
key=\${url#https://fixture.r2.cloudflarestorage.com/control/};file="$TEST_ROOT/bucket/$key"
echo "$method $key" >>"$TEST_ROOT/s3-trace"
if [[ "$method" == PUT ]];then
 if [[ "$condition" == 'If-None-Match: *' && -f "$file" ]];then printf 412;exit;fi
 if [[ "$condition" == 'If-Match: '* ]];then
  [[ -f "$file" ]] || { printf 412;exit; }
  current=$(sha256sum "$file"|cut -d' ' -f1)
  [[ "$condition" == "If-Match: \\"$current\\"" ]] || { printf 412;exit; }
 fi
 if [[ -f "$TEST_ROOT/conflict-latest" && "$key" == */latest.json ]];then printf 412;exit;fi
 mkdir -p "$(dirname "$file")";cp "$upload" "$file"
fi
if [[ ! -f "$file" ]];then printf 404;exit;fi
hash=$(sha256sum "$file"|cut -d' ' -f1)
[[ -z "$headers" ]] || printf 'ETag: "%s"\\r\\n' "$hash" >"$headers"
if [[ "$method" == HEAD ]];then printf 'ETag: "%s"\\r\\n' "$hash" >"$output";elif [[ "$method" == GET ]];then cp "$file" "$output";fi
printf 200
`);
  const root=join(work,'node');
  write(join(root,'config/s3.env'),"S3_ENDPOINT=https://fixture.r2.cloudflarestorage.com\nS3_REGION=auto\nS3_BUCKET=control\nCLUSTER_ID=fixture\nAWS_ACCESS_KEY_ID=r2-fixture\nAWS_SECRET_ACCESS_KEY=r2-fixture\n");
  const settings=`LOCAL_CONFIG_SCHEMA=1\nRESTORE_ROOT=${root}\nWORK_ROOT=${root}/generations\nLOCK_FILE=${root}/agent/recovery.lock\nNGINX_MAIN_CONF=${work}/nginx-main.conf\nNGINX_HTTP_CONF_DIR=${work}/nginx\nSYSTEMD_UNIT_DIR=${work}/units\n`;
  write(join(root,'agent/local.env'),settings);
  write(join(work,'internal.env'),readFileSync(join(root,'config/s3.env'),'utf8')+settings);
  return {root,env:{...process.env,TEST_ROOT:work,PATH:`${work}/mock:${process.env.PATH}`,FIXTURE_SNAPSHOT:'a'.repeat(64)}};
}
export function run(script,args,env){return spawnSync('bash',[script,...args],{env,encoding:'utf8',timeout:60000,maxBuffer:4*1024*1024});}
export function successful(result){if(result.status!==0)throw new Error(`Exit ${result.status}: ${result.stdout}\n${result.stderr}`);return result;}
export function point(work,software){
  const snapshot='a'.repeat(64),prefix='cluster/fixture/recovery/v2',key=`${prefix}/points/${snapshot}.json`;
  const configuration=join(work,'configuration.json');rmSync(configuration,{force:true});
  execFileSync(process.env.MATRIX_RELEASE_BINARY,['config-pack','--bundle',join(work,'bundle'),'--output',configuration]);
  const ch=hashFile(configuration),configuration_object=`${prefix}/configurations/${ch}.json`;
  write(join(work,'bucket',configuration_object),readFileSync(configuration));
  const repo=join(work,'repository.json');
  json(repo,{schema:1,repository_id:'fixture-repository',password:'fixture-private-password',endpoint:'https://s3.test.backblazeb2.com',region:'test',bucket:'test',path:'repository',access_key_id:'b2-fixture',secret_access_key:'b2-fixture'});
  const rh=hashFile(repo),repository_object=`${prefix}/repositories/fixture-repository-${rh}.json`;
  write(join(work,'bucket',repository_object),readFileSync(repo));
  const metadata=JSON.parse(readFileSync(join(work,'bundle/metadata.json'),'utf8'));
  json(join(work,'bucket',key),{...metadata,protocol:2,point_schema:2,catalog_schema:1,data_layout:'database-only-v1',backup_format:'tuwunel-checkpoint-v2',recovery_version:2,platform:'linux/amd64',scope:{media:false,telegram:true},media:{backup:'external-s3'},snapshot_id:snapshot,software,configuration_object,configuration_sha256:ch,repository_object,repository_sha256:rh,repository_id:'fixture-repository'});
  json(join(work,'bucket',prefix,'latest.json'),{protocol:2,point_schema:2,point_key:key,point_sha256:hashFile(join(work,'bucket',key))});
  return snapshot;
}
