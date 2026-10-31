import {test} from 'node:test';
import assert from 'node:assert/strict';
import worker,{submit,sha256} from '../worker/index.js';
import {verifyAccess} from '../worker/access.js';
import {generateKeyPair, exportJWK, SignJWT, createLocalJWKSet} from 'jose';
const issuer='https://test.cloudflareaccess.com',audience='a'.repeat(64);
const pair=await generateKeyPair('RS256');
const key={...await exportJWK(pair.publicKey),kid:'test-key',alg:'RS256',use:'sig'};
const localKeys=createLocalJWKSet({keys:[key]});
async function token(claims={},privateKey=pair.privateKey,header={}){
 return new SignJWT({iss:issuer,aud:[audience],sub:'test-user',email:'admin@example.com',type:'app',iat:Math.floor(Date.now()/1000),exp:Math.floor(Date.now()/1000)+3600,...claims}).setProtectedHeader({alg:'RS256',kid:'test-key',...header}).sign(privateKey);
}
const signedToken=await token();
globalThis.fetch=async(url,...rest)=>{
 if(String(url)===issuer+'/cdn-cgi/access/certs')return Response.json({keys:[key]});
 throw new Error('Unexpected outbound fetch: '+url);
};
class MemoryR2 {
  constructor(){this.objects=new Map();this.writes=[];}
  async get(key){const value=this.objects.get(key);if(!value)return null;const text=value.text;return {key,size:new TextEncoder().encode(text).length,etag:value.etag,text:async()=>text};}
  async put(key,text,options={}){const etag=await sha256(text),condition=options.onlyIf,old=this.objects.get(key);
    if(condition instanceof Headers){if(condition.get('If-None-Match')==='*'&&old)return null;}
    else if(condition?.etagMatches&&old?.etag!==condition.etagMatches)return null;
    this.objects.set(key,{text,etag});this.writes.push(key);return {key,etag};}
  async list({prefix,cursor,limit=100}){const all=[...this.objects.keys()].filter(x=>x.startsWith(prefix)).sort(),offset=Number(cursor||0),keys=all.slice(offset,offset+limit);return {objects:keys.map(key=>({key})),truncated:all.length>offset+limit,cursor:String(offset+limit)};}
  async set(key,value){await this.put(key,JSON.stringify(value));}
  async value(key){const x=await this.get(key);return x?JSON.parse(await x.text()):null;}
}
const root='cluster/test/';
async function fixture(){const CONTROL=new MemoryR2(),env={CONTROL,CLUSTER_ID:'test',TEAM_DOMAIN:issuer,POLICY_AUD:audience,accessJWT:signedToken};
 const control={schema:1,cluster_id:'test',revision:3,epoch:1,owner:'A',phase:'ONLINE',operation:null};
 await CONTROL.set(root+'control.json',control);
 await CONTROL.set(root+'instances/B.json',{schema:1,cluster_id:'test',instance_id:'B',bound_epoch:0,serving:false,reported_at:new Date().toISOString()});
 await CONTROL.set(root+'agents/B.json',{schema:1,cluster_id:'test',instance_id:'B',mailbox_protocol:1,preflight:{ready:true,epoch:1,checked_at:new Date().toISOString()},reported_at:new Date().toISOString()});
 CONTROL.writes=[];
 const commandTime=Date.now();
 const command={schema:1,id:'cmd-one',cluster_id:'test',target:'B',action:'handover-request',created_at:new Date(commandTime).toISOString(),expires_at:new Date(commandTime+300000).toISOString(),expected:{revision:3,epoch:1,owner:'A',phase:'ONLINE'},bound_epoch:0,operation_id:'move-one',source_epoch:1};
 return {env,CONTROL,control,command};
}
function request(env,path,method='GET',body,headers={}){return new Request('https://admin.example'+path,{method,headers:{'Cf-Access-Jwt-Assertion':env.accessJWT,...(body?{'Content-Type':'application/json'}:{}),...headers},body:body?JSON.stringify(body):undefined});}
async function finish(env,command,state='APPLIED'){await env.CONTROL.set(root+'commands/B/results/'+command.id+'.json',{schema:1,id:command.id,target:'B',command_sha256:await sha256(JSON.stringify(command)),state,at:new Date().toISOString(),action:command.action});}

test('unauthorized API never reads or writes R2',async()=>{const {env,CONTROL}=await fixture();env.CONTROL.get=()=>{throw Error('must not read');};const r=await worker.fetch(new Request('https://admin.example/api/status'),env);assert.equal(r.status,401);assert.equal(CONTROL.writes.length,0);});
test('unconfigured Access fails closed',async()=>{const {env}=await fixture();env.POLICY_AUD='';assert.equal((await worker.fetch(new Request('https://admin.example/api/status'),env)).status,503);});
test('cross origin and file origins rejected even with valid identity',async()=>{const {env,CONTROL}=await fixture();for(const Origin of ['https://evil.example','null'])for(const method of ['GET','POST','OPTIONS']){assert.equal((await worker.fetch(request(env,'/api/status',method,null,{Origin}),env)).status,403);}assert.equal(CONTROL.writes.length,0);});
test('same origin mutation is allowed after authentication',async()=>{const {env,command}=await fixture();const response=await worker.fetch(request(env,'/api/command','POST',command,{Origin:'https://admin.example'}),env);assert.equal(response.status,200);});
test('status only reads R2; reports are labelled observations',async()=>{const {env,CONTROL}=await fixture();const response=await worker.fetch(request(env,'/api/status'),env),status=await response.json();assert.equal(status.control.owner,'A');assert.equal(status.instances[0].id,'B');assert.equal(status.instances[0].agent_fresh,true);assert.equal(CONTROL.writes.length,0);});
test('command submits immutable archive and CAS pending, never control.json',async()=>{const {env,CONTROL,command}=await fixture();const result=await submit(env,command);assert.equal(result.submitted,true);assert.equal(CONTROL.writes.length,2);assert(CONTROL.writes.every(x=>x.startsWith(root+'commands/')));assert.equal((await CONTROL.value(root+'control.json')).owner,'A');});
test('lost HTTP response retry retains original id despite changed authority',async()=>{const {env,CONTROL,command,control}=await fixture();await submit(env,command);control.phase='QUIESCING';control.revision++;await CONTROL.set(root+'control.json',control);const result=await submit(env,command);assert.equal(result.duplicate,true);assert.equal(result.command.id,'cmd-one');});
test('reused command id with different body rejected',async()=>{const {env,command}=await fixture();await submit(env,command);await assert.rejects(()=>submit(env,{...command,action:'fence-local'}),/编号/);});
test('stale expected revision rejected without writes',async()=>{const {env,CONTROL,command}=await fixture();command.expected.revision--;await assert.rejects(()=>submit(env,command),/状态已变化/);assert.equal(CONTROL.writes.length,0);});
test('missing poller capability prevents unsafe submission',async()=>{const {env,CONTROL,command}=await fixture();CONTROL.objects.delete(root+'agents/B.json');await assert.rejects(()=>submit(env,command),/轮询报告/);assert.equal(CONTROL.writes.length,0);});
test('aged observation is never online authorization',async()=>{const {env,CONTROL,command}=await fixture();const old=await CONTROL.value(root+'instances/B.json');old.reported_at='2020-01-01T00:00:00Z';await CONTROL.set(root+'instances/B.json',old);await assert.rejects(()=>submit(env,command),/过期/);});
test('concurrent submissions have one pending winner',async()=>{const {env,CONTROL,command}=await fixture();const results=await Promise.allSettled([submit(env,command),submit(env,{...command,id:'cmd-two'})]);assert.equal(results.filter(x=>x.status==='fulfilled').length,1);assert(['cmd-one','cmd-two'].includes((await CONTROL.value(root+'commands/B/pending.json')).id));});
test('unfinished pending cannot be overwritten even after expiry',async()=>{const {env,CONTROL,command}=await fixture();const old={...command,id:'cmd-old',created_at:'2020-01-01T00:00:00Z',expires_at:'2020-01-01T00:05:00Z'};await CONTROL.set(root+'commands/B/pending.json',old);await assert.rejects(()=>submit(env,command),/未结束指令/);});
test('terminal receipt allows next command only if hash matches',async()=>{const {env,CONTROL,command}=await fixture();await submit(env,command);await finish(env,command);const next={...command,id:'cmd-two'};assert.equal((await submit(env,next)).submitted,true);const result=await CONTROL.value(root+'commands/B/results/cmd-one.json');result.command_sha256='bad';await CONTROL.set(root+'commands/B/results/cmd-two.json',result);await assert.rejects(()=>submit(env,{...command,id:'cmd-three'}),/哈希/);});
test('closing expired unclaimed command inserts terminal receipt first',async()=>{const {env,CONTROL,command}=await fixture();const expired={...command,created_at:'2020-01-01T00:00:00Z',expires_at:'2020-01-01T00:05:00Z'};await CONTROL.set(root+'commands/B/pending.json',expired);const response=await worker.fetch(request(env,'/api/abandon-expired','POST',{target:'B',id:command.id}),env);assert.equal(response.status,200);assert.equal((await CONTROL.value(root+'commands/B/results/cmd-one.json')).state,'REJECTED');});
test('already claimed command cannot be abandoned',async()=>{const {env,CONTROL,command}=await fixture();await CONTROL.set(root+'commands/B/pending.json',{...command,created_at:'2020-01-01T00:00:00Z',expires_at:'2020-01-01T00:05:00Z'});await finish(env,command,'CLAIMED');assert.equal((await worker.fetch(request(env,'/api/abandon-expired','POST',{target:'B',id:command.id}),env)).status,409);});
test('internal transitions and force are never exposed',async()=>{const {env,command}=await fixture();for(const action of ['handover-claim','handover-release','force','delete'])await assert.rejects(()=>submit(env,{...command,action}),/不支持/);});
test('expired command cannot be submitted',async()=>{const {env,command}=await fixture();command.created_at='2020-01-01T00:00:00Z';command.expires_at='2020-01-01T00:05:00Z';await assert.rejects(()=>submit(env,command),/已过期/);});
test('unknown nested protocol fields rejected',async()=>{const {env,command}=await fixture();command.expected.force=true;await assert.rejects(()=>submit(env,command),/未知/);});
test('archive exists but pending PUT failed: retry actually claims slot',async()=>{const {env,CONTROL,command}=await fixture();await CONTROL.set(root+'commands/B/requests/cmd-one.json',command);const result=await submit(env,command);assert.equal(result.submitted,true);assert.equal((await CONTROL.value(root+'commands/B/pending.json')).id,'cmd-one');});
test('diagnostic excludes Access assertion and reads no backup credentials',async()=>{const {env,CONTROL}=await fixture();await CONTROL.set('matrix-recovery/keys/secret.json',{password:'PRIVATE'});const response=await worker.fetch(request(env,'/api/diagnostic'),env),text=await response.text();assert(!text.includes(env.accessJWT));assert(!text.includes('PRIVATE'));assert.equal(CONTROL.writes.length,1);});

test('forged identity header and old bearer token cannot access any API',async()=>{
 const {env,CONTROL}=await fixture();CONTROL.get=()=>{throw Error('must not read R2');};
 for(const path of ['/api/status','/api/diagnostic','/api/command','/api/dns','/api/abandon-expired','/api/instance-info']){
  const r=await worker.fetch(new Request('https://admin.example'+path,{headers:{'Cf-Access-Authenticated-User-Email':'admin@example.com',Authorization:'Bearer '+'x'.repeat(64)}}),env);assert.equal(r.status,401);
 }assert.equal(CONTROL.writes.length,0);
});
test('JWT signature, issuer, audience, expiry and human identity are required',async()=>{
 const {env}=await fixture();
 const rogue=await generateKeyPair('RS256');
 const invalid=[
  await token({},rogue.privateKey),await token({iss:'https://evil.cloudflareaccess.com'}),
  await token({aud:['b'.repeat(64)]}),await token({exp:Math.floor(Date.now()/1000)-1}),
  await token({nbf:Math.floor(Date.now()/1000)+1000}),await token({exp:undefined}),
  await token({email:undefined}),await token({email:''}),await token({sub:''}),await token({type:'service'}),
  'not-a-jwt',await token({},pair.privateKey,{kid:'unknown'})
 ];
 for(const value of invalid){const r=await verifyAccess(new Request('https://admin.example',{headers:{'Cf-Access-Jwt-Assertion':value}}),env,localKeys);assert.equal(r.status,401);}
});
test('valid JWT accepts matching audience in an array and trailing slash in team URL',async()=>{
 const {env}=await fixture();env.TEAM_DOMAIN+='/';env.accessJWT=await token({aud:['b'.repeat(64),audience]});
 assert.equal((await verifyAccess(request(env,'/api/status'),env,localKeys)).status,200);
});
test('key rotation accepts both published signing keys',async()=>{
 const rotated=await generateKeyPair('RS256');const second={...await exportJWK(rotated.publicKey),kid:'rotated',alg:'RS256'};
 const keys=createLocalJWKSet({keys:[key,second]});const {env}=await fixture();
 for(const assertion of [signedToken,await token({},rotated.privateKey,{kid:'rotated'})]){
  env.accessJWT=assertion;assert.equal((await verifyAccess(request(env,'/api/status'),env,keys)).status,200);
 }
});
test('invalid config cannot fetch arbitrary JWKS URLs; key service failure fails closed',async()=>{
 const {env}=await fixture();for(const domain of ['http://test.cloudflareaccess.com','https://evil.example','https://test.cloudflareaccess.com@evil.example','https://test.cloudflareaccess.com/path']){
  assert.equal((await verifyAccess(request(env,'/api/status'),{...env,TEAM_DOMAIN:domain})).status,503);
 }
 assert.equal((await verifyAccess(request(env,'/api/status'),{...env,TEAM_DOMAIN:'https://unavailable.cloudflareaccess.com'})).status,503);
});
test('invalid JWT cannot reach R2 even on write endpoints',async()=>{
 const {env,CONTROL,command}=await fixture();CONTROL.get=()=>{throw Error('must not read R2');};env.accessJWT=await token({aud:['b'.repeat(64)]});
 assert.equal((await worker.fetch(request(env,'/api/command','POST',command),env)).status,401);assert.equal(CONTROL.writes.length,0);
});

test('machine identity edits stay in their own R2 namespace and use CAS',async()=>{
 const {env,CONTROL}=await fixture();
 const input={id:'B',name:'备用 VPS',public_ip:'192.0.2.10',expected_etag:null};
 const call=body=>worker.fetch(request(env,'/api/instance-info','POST',body),env);
 assert.equal((await call(input)).status,200);
 assert.deepEqual(CONTROL.writes,[root+'instance-info/B.json']);
 let status=await (await worker.fetch(request(env,'/api/status'),env)).json();
 assert.equal(status.instances[0].info.name,'备用 VPS');
 const etag=status.instances[0].info_etag;
 assert.equal((await call({...input,name:'stale'})).status,409);
 assert.equal((await call({...input,name:'新名称',expected_etag:etag})).status,200);
 assert.equal((await call({...input,name:'other tab',expected_etag:etag})).status,409);
 assert.equal((await CONTROL.value(root+'control.json')).owner,'A');
});
test('machine identity rejects paths, nonexistent instances, invalid IPs and authority fields',async()=>{
 const {env,CONTROL}=await fixture();const input={id:'B',name:'VPS',public_ip:'192.0.2.10',expected_etag:null};
 for(const change of [{id:'../control'},{id:'missing'},{public_ip:'999.1.2.3'},{public_ip:'https://evil.example'},{public_ip:'0192.0.2.11'},{name:'x'.repeat(81)},{name:'a\nb'},{owner:'B'}]){
  const response=await worker.fetch(request(env,'/api/instance-info','POST',{...input,...change}),env);assert([400,404].includes(response.status));
 }assert.equal(CONTROL.writes.length,0);
});

async function managedFixture(action, parameters={}) {
 const f=await fixture();
 const agent=await f.CONTROL.value(root+'agents/B.json');
 agent.management_protocol=1;agent.actions=[action];await f.CONTROL.set(root+'agents/B.json',agent);
 delete f.command.operation_id;delete f.command.source_epoch;
 Object.assign(f.command,{action,parameters});f.CONTROL.writes=[];return f;
}
test('management tasks require node capability and never mutate authority',async()=>{
 const f=await managedFixture('backup-now',{full:true});
 await submit(f.env,f.command);
 assert(f.CONTROL.writes.every(k=>k.startsWith(root+'commands/')));
 const bad=await managedFixture('backup-now',{full:true});
 const agent=await bad.CONTROL.value(root+'agents/B.json');delete agent.management_protocol;await bad.CONTROL.set(root+'agents/B.json',agent);
 await assert.rejects(()=>submit(bad.env,bad.command),/升级/);
});
test('task parameters reject command injection and unsupported fields before writing',async()=>{
 for(const [action,parameters] of [['backup-now',{full:'yes'}],['health-check',{command:'sh'}],['upgrade-runtime',{version:'../latest',manifest_sha256:'a'.repeat(64)}],['storage-plan',{keep_last:1,keep_days:30}],['task-cancel',{task_id:'../x'}],['preflight',{snapshot_id:'a'.repeat(64)}]]){
  const {env,CONTROL,command}=await managedFixture(action,parameters);await assert.rejects(()=>submit(env,command));assert.equal(CONTROL.writes.length,0);
 }
});
test('forced takeover requires loss acknowledgment and isolation of every previous writer',async()=>{
 const parameters={snapshot_id:'a'.repeat(64),point_sha256:'b'.repeat(64),isolated_instances:['A'],isolation_method:'powered-off',note:'provider confirmed power off',accept_data_loss:true};
 for(const change of [{accept_data_loss:false},{isolated_instances:[]},{isolated_instances:['B']},{isolated_instances:['A','A']},{isolation_method:'no-heartbeat'}]){
  const f=await managedFixture('force-takeover',{...parameters,...change});await assert.rejects(()=>submit(f.env,f.command));assert.equal(f.CONTROL.writes.length,0);
 }
 const f=await managedFixture('force-takeover',parameters);
 f.control.operation={source:'A',target:'C'};await f.CONTROL.set(root+'control.json',f.control);f.CONTROL.writes=[];
 await assert.rejects(()=>submit(f.env,f.command),/全部/);
 f.command.parameters.isolated_instances.push('C');await submit(f.env,f.command);
 assert(f.CONTROL.writes.every(k=>k.startsWith(root+'commands/')));
});
test('failed or expired preflight blocks planned handover without stopping source',async()=>{
 for(const change of [{ready:false},{epoch:2},{checked_at:'2020-01-01T00:00:00Z'}]){
 const f=await fixture(),a=await f.CONTROL.value(root+'agents/B.json');Object.assign(a.preflight,change);await f.CONTROL.set(root+'agents/B.json',a);f.CONTROL.writes=[];
 await assert.rejects(()=>submit(f.env,f.command),/预检/);assert.equal(f.CONTROL.writes.length,0);
 }
});
test('catalog excludes retired points and never exposes configuration or repository credentials',async()=>{
 const f=await fixture();
 for(const id of ['a','b'])await f.CONTROL.set(root+'recovery/v2/points/'+id.repeat(64)+'.json',{protocol:2,point_schema:2,snapshot_id:id.repeat(64),instance_control:{cluster_id:'test',source_instance_id:'A',source_epoch:1},data_time:new Date().toISOString(),repository_password:'PRIVATE',configuration:{secret:'PRIVATE'},media_included:false});
 await f.CONTROL.set(root+'recovery/v2/retired/'+ 'b'.repeat(64)+'.json',{});
 const r=await worker.fetch(request(f.env,'/api/catalog'),f.env);assert.equal(r.status,200);
 const text=await r.text();assert(!text.includes('PRIVATE'));const data=JSON.parse(text);assert.equal(data.points.length,1);assert.equal(data.points[0].snapshot_id,'a'.repeat(64));assert.match(data.points[0].point_sha256,/^[a-f0-9]{64}$/);
 assert.equal((await worker.fetch(request(f.env,'/api/catalog?offset=-1'),f.env)).status,400);
});
test('new catalog and release endpoints require Access before any storage reads',async()=>{
 const f=await fixture();f.CONTROL.get=()=>{throw Error('must not read R2');};
 for(const path of ['/api/catalog','/api/releases'])assert.equal((await worker.fetch(new Request('https://admin.example'+path),f.env)).status,401);
});
test('release discovery checks manifest hash and reports explicit installation only',async()=>{
 const f=await fixture(),original=globalThis.fetch;
 const manifest={schema:1,project:'matrix-admin',version:'0.3.0',platform:'linux/amd64',runtime:'native-go-v1',control_protocol:1,mailbox_protocol:1,recovery_readers:[2],commit:'a'.repeat(40)};
 const text=JSON.stringify(manifest),hash=await sha256(text);let calls=0;
 globalThis.fetch=async url=>{const u=String(url);if(u.includes('/cdn-cgi/'))return original(url);calls++;
 if(u.includes('/repos/'))return Response.json([{tag_name:'v0.3.0',assets:['manifest.json','SHA256SUMS','matrix-admin-linux-amd64.tar.gz'].map(name=>({name})),published_at:new Date().toISOString()}]);
 if(u.endsWith('/manifest.json'))return new Response(text);if(u.endsWith('/SHA256SUMS'))return new Response(hash+'  manifest.json\n');throw Error('Unexpected URL');};
 try{const a=await worker.fetch(request(f.env,'/api/releases'),f.env);assert.equal(a.status,200);const v=await a.json();assert.equal(v.automatic_install,false);assert.equal(v.releases[0].manifest_sha256,hash);await worker.fetch(request(f.env,'/api/releases'),f.env);assert.equal(calls,3);assert(f.CONTROL.writes.every(k=>k===root+'observations/releases.json'));}finally{globalThis.fetch=original;}
});
