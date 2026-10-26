import {test} from 'node:test';
import assert from 'node:assert/strict';
import worker,{submit,sha256} from '../worker/index.js';
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
async function fixture(){const CONTROL=new MemoryR2(),env={CONTROL,CLUSTER_ID:'test',ADMIN_TOKEN:'a'.repeat(48),ALLOWED_ORIGINS:'null'};
 const control={schema:1,cluster_id:'test',revision:3,epoch:1,owner:'A',phase:'ONLINE',operation:null};
 await CONTROL.set(root+'control.json',control);
 await CONTROL.set(root+'instances/B.json',{schema:1,cluster_id:'test',instance_id:'B',bound_epoch:0,serving:false,reported_at:new Date().toISOString()});
 await CONTROL.set(root+'agents/B.json',{schema:1,cluster_id:'test',instance_id:'B',mailbox_protocol:1,reported_at:new Date().toISOString()});
 CONTROL.writes=[];
 const command={schema:1,id:'cmd-one',cluster_id:'test',target:'B',action:'handover-request',created_at:new Date().toISOString(),expires_at:new Date(Date.now()+300000).toISOString(),expected:{revision:3,epoch:1,owner:'A',phase:'ONLINE'},bound_epoch:0,operation_id:'move-one',source_epoch:1};
 return {env,CONTROL,control,command};
}
function request(env,path,method='GET',body,headers={}){return new Request('https://admin.example'+path,{method,headers:{Authorization:'Bearer '+env.ADMIN_TOKEN,...(body?{'Content-Type':'application/json'}:{}),...headers},body:body?JSON.stringify(body):undefined});}
async function finish(env,command,state='APPLIED'){await env.CONTROL.set(root+'commands/B/results/'+command.id+'.json',{schema:1,id:command.id,target:'B',command_sha256:await sha256(JSON.stringify(command)),state,at:new Date().toISOString(),action:command.action});}

test('unauthorized API never reads or writes R2',async()=>{const {env,CONTROL}=await fixture();env.CONTROL.get=()=>{throw Error('must not read');};const r=await worker.fetch(new Request('https://admin.example/api/status'),env);assert.equal(r.status,401);assert.equal(CONTROL.writes.length,0);});
test('unconfigured admin secret fails closed',async()=>{const {env}=await fixture();env.ADMIN_TOKEN='';assert.equal((await worker.fetch(new Request('https://admin.example/api/status'),env)).status,503);});
test('cross origin rejected; local file preflight allowed',async()=>{const {env}=await fixture();assert.equal((await worker.fetch(request(env,'/api/status','GET',null,{Origin:'https://evil.example'}),env)).status,403);const response=await worker.fetch(new Request('https://admin.example/api/command',{method:'OPTIONS',headers:{Origin:'null'}}),env);assert.equal(response.status,204);assert.equal(response.headers.get('Access-Control-Allow-Origin'),'null');});
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
test('diagnostic excludes admin token and reads no backup credentials',async()=>{const {env,CONTROL}=await fixture();await CONTROL.set('matrix-recovery/keys/secret.json',{password:'PRIVATE'});const response=await worker.fetch(request(env,'/api/diagnostic'),env),text=await response.text();assert(!text.includes(env.ADMIN_TOKEN));assert(!text.includes('PRIVATE'));assert.equal(CONTROL.writes.length,1);});
