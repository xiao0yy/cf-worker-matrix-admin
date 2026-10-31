// Build public runtime assets from explicit source paths, never a live deploy.
import {readFileSync, writeFileSync, mkdirSync, mkdtempSync, rmSync, cpSync, chmodSync, existsSync} from 'node:fs';
import {join, resolve} from 'node:path';
import {fileURLToPath} from 'node:url';
import {tmpdir} from 'node:os';
import {execFileSync} from 'node:child_process';
import {sha256, inventory, packRelease} from './release-lib.mjs';
import {assetNames} from './release-contract.mjs';

const root=fileURLToPath(new URL('../',import.meta.url));
const run=(cmd,args,options={})=>execFileSync(cmd,args,{cwd:root,encoding:'utf8',stdio:['pipe','pipe','pipe'],...options});
const dev=process.argv.includes('--development');
if(process.argv.slice(2).some(a=>a!=='--development'))throw new Error('Only --development is supported; publishing requires a clean commit');
if(!dev && run('git',['status','--porcelain']).trim())throw new Error('Commit reviewed source before building a release');
const origin=JSON.parse(readFileSync(join(root,'release.config.json'),'utf8'));
const version=readFileSync(join(root,'VERSION'),'utf8').trim();
if(!/^\d+\.\d+\.\d+(?:-[a-zA-Z0-9.-]+)?$/.test(version))throw new Error('Invalid VERSION');
const commit=run('git',['rev-parse','HEAD']).trim();
const timestamp=run('git',['show','-s','--format=%ct','HEAD']).trim();
const output=join(root,'dist',dev?'development':`v${version}`);
if(existsSync(output))throw new Error(`Output already exists: ${output}; select or remove your own previous build first`);
const work=mkdtempSync(join(tmpdir(),'matrix-release-build-'));
const stage=join(work,'stage');
mkdirSync(stage);
const backupFiles = [
  'payload-verify.sh','recover.sh','recover-main.sh','s3-common.sh','release-common.sh','local-config.sh','inspect-host.sh','host-state.schema.json',
  'prepare-standby.sh','foundation-software.sh','upgrade.sh','tuwunel-restic.sh','publish-recovery.sh','final-backup.sh',
  'restore-tuwunel.sh','telegram-backup.sh','verify-telegram-runtime.sh','image-reference.sh',
  'tuwunel-restic.service','tuwunel-restic.timer','s3.env.example','local.env.example','AGENT.md','S3-RECOVERY.md','THIRD-PARTY.md'
];
const controlFiles=['handover-worker.sh','mailbox-install.sh','matrix-control.service','matrix-handover.service','matrix-handover.timer','matrix-mailbox.service','matrix-mailbox.timer','nginx-server.conf','nginx-location.conf'];
try {
  for(const [part,names] of [['backup',backupFiles],['control',controlFiles]]){
    mkdirSync(join(stage,part,'bin'),{recursive:true});
    for(const name of names){cpSync(join(root,'node',part,name),join(stage,part,name));chmodSync(join(stage,part,name),name.endsWith('.sh')?0o700:0o600);}
  }
  cpSync(join(root,'LICENSE'),join(stage,'backup/LICENSE.txt'));chmodSync(join(stage,'backup/LICENSE.txt'),0o600);
  writeFileSync(join(stage,'release.json'),JSON.stringify(origin,null,2)+'\n',{mode:0o600});
  const env={...process.env,CGO_ENABLED:'0',GOOS:'linux',GOARCH:'amd64',GOAMD64:'v1'};
  for(const [module,bin] of [['node/control','control/bin/matrix-control'],['poller','control/bin/matrix-mailbox'],['node/release','backup/bin/matrix-release']]){
    run(process.env.GO_BINARY||'go',['build','-buildvcs=false','-trimpath','-ldflags=-s -w','-o',join(stage,bin),'.'],{cwd:join(root,module),env});
    chmodSync(join(stage,bin),0o700);
  }
  const dep=JSON.parse(readFileSync(join(root,'dependencies.lock.json'),'utf8')).restic;
  if(dep.version!==origin.restic_version)throw new Error('Restic lock/version mismatch');
  let binary;
  if(process.env.RESTIC_BINARY)binary=readFileSync(resolve(process.env.RESTIC_BINARY));
  else {
    const file=join(work,'restic.bz2');
    run('curl',['--fail','--silent','--show-error','--location','--proto','=https','--proto-redir','=https','--max-time','300','--output',file,dep.url]);
    binary=execFileSync('bzip2',['-dc',file],{maxBuffer:128*1024*1024});
  }
  if(sha256(binary)!==dep.binary_sha256)throw new Error('Pinned restic binary checksum mismatch');
  writeFileSync(join(stage,'backup/bin/restic'),binary,{mode:0o700});
  writeFileSync(join(stage,'control/software.sha256'),inventory(join(stage,'control')).map(f=>`${f.sha256}  ${f.path}\n`).join(''),{mode:0o600});
  const manifest=packRelease(stage,output,{version,commit,origin,timestamp});
  writeFileSync(join(stage,'release-manifest.json'),JSON.stringify(manifest,null,2)+'\n',{mode:0o600});
  run(join(stage,'backup/bin/matrix-release'),['verify','--root',stage,'--manifest',join(stage,'release-manifest.json')]);
  cpSync(join(stage,'backup/bin/matrix-release'),join(output,'matrix-release-linux-amd64'));
  if(!dev)run('git',['archive','--format=tar.gz',`--prefix=matrix-admin-${version}/`,`--output=${join(output,'source.tar.gz')}`,commit]);
  writeFileSync(join(output,'BUILD.json'),JSON.stringify({version,commit,publishable:!dev,release_base_url:origin.release_base_url,source:'explicit source inventory; no live instance files'},null,2)+'\n');
  writeFileSync(join(output,'SHA256SUMS'),assetNames.filter(name=>name!=='SHA256SUMS' && (name!=='source.tar.gz'||!dev)).map(name=>`${sha256(readFileSync(join(output,name)))}  ${name}\n`).join(''));
  console.log(`Verified ${dev?'DEVELOPMENT (do not publish)':'release'} artifacts: ${output}`);
} catch(error) {
  // The output was created by this invocation and cannot contain user data.
  rmSync(output,{recursive:true,force:true});throw error;
} finally {rmSync(work,{recursive:true,force:true});}
