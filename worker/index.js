import {verifyAccess} from './access.js';
const encoder = new TextEncoder();
const actions = new Set(['probe', 'handover-request', 'handover-resume', 'handover-cancel', 'fence-local', 'resume-local', 'source-resume']);
const terminal = new Set(['APPLIED', 'FAILED', 'UNKNOWN', 'REJECTED']);
const safe = value => typeof value === 'string' && /^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,99}$/.test(value) && value !== '..';
const stableExpected = value => ({revision: value.revision, epoch: value.epoch, owner: value.owner, phase: value.phase});
const sameExpected = (a,b) => JSON.stringify(stableExpected(a)) === JSON.stringify(stableExpected(b));
class Fault extends Error { constructor(status,message) { super(message); this.status=status; } }
export async function sha256(value) { return [...new Uint8Array(await crypto.subtle.digest('SHA-256',encoder.encode(value)))].map(x=>x.toString(16).padStart(2,'0')).join(''); }
function prefix(env) { if (!safe(env.CLUSTER_ID)) throw new Fault(503,'Worker CLUSTER_ID 未配置'); return `cluster/${env.CLUSTER_ID}/`; }
async function jsonObject(bucket,key,required=false) {
  const object=await bucket.get(key);
  if(!object){if(required)throw new Fault(503,`缺少控制对象：${key}`);return null;}
  if(object.size>1024*1024)throw new Fault(502,'控制对象过大');
  let value;try{value=JSON.parse(await object.text());}catch{throw new Fault(502,'控制对象 JSON 无效');}
  return {value,etag:object.etag};
}
async function allObjects(bucket,keyPrefix) {
  const objects=[];let cursor;
  do {const page=await bucket.list({prefix:keyPrefix,cursor,limit:100});objects.push(...page.objects);if(objects.length>200)throw new Fault(503,'实例或诊断对象超过本工具上限');cursor=page.truncated?page.cursor:undefined;}while(cursor);
  return objects;
}
async function authority(env) {
  const state=(await jsonObject(env.CONTROL,prefix(env)+'control.json',true)).value;
  if(state.schema!==1||state.cluster_id!==env.CLUSTER_ID||!Number.isSafeInteger(state.epoch)||state.epoch<1||!Number.isSafeInteger(state.revision)||state.revision<1)throw new Fault(502,'控制协议不兼容');
  return state;
}
function isFresh(value,field='reported_at',seconds=90) {const age=Date.now()-Date.parse(value?.[field]);return Number.isFinite(age)&&age>=-30000&&age<=seconds*1000;}
async function readInstance(env,id) {
  if(!safe(id))throw new Fault(400,'实例 ID 无效');
  const base=prefix(env), report=await jsonObject(env.CONTROL,base+'instances/'+id+'.json'), agent=await jsonObject(env.CONTROL,base+'agents/'+id+'.json');
  const info=await jsonObject(env.CONTROL,base+'instance-info/'+id+'.json');
  const pending=await jsonObject(env.CONTROL,base+'commands/'+id+'/pending.json');
  const result=pending&&safe(pending.value.id)?await jsonObject(env.CONTROL,base+'commands/'+id+'/results/'+pending.value.id+'.json'):null;
  return {id,info:info?.value??null,info_etag:info?.etag??null,report:report?.value??null,report_fresh:isFresh(report?.value),agent:agent?.value??null,agent_fresh:isFresh(agent?.value),pending:pending?.value??null,result:result?.value??null};
}
async function status(env) {
  const control=await authority(env), keys=await allObjects(env.CONTROL,prefix(env)+'instances/');
  const ids=keys.filter(x=>x.key.endsWith('.json')).map(x=>x.key.slice((prefix(env)+'instances/').length,-5)).filter(safe);
  const instances=await Promise.all(ids.map(id=>readInstance(env,id)));
  return {schema:1,version:'0.1.1',at:new Date().toISOString(),control,instances,capabilities:{mailbox:1,planned_handover:true,force_takeover:false,automatic_cleanup:false,dns:!!(env.CF_DNS_TOKEN&&env.CF_ZONE_ID&&env.CF_RECORD_ID&&env.CF_RECORD_NAME)}};
}
async function saveInstanceInfo(env,input) {
  if(!input||!safe(input.id)||!Object.hasOwn(input,'expected_etag')||(input.expected_etag!==null&&typeof input.expected_etag!=='string')||typeof input.name!=='string'||typeof input.public_ip!=='string'||Object.keys(input).some(k=>!['id','name','public_ip','expected_etag'].includes(k)))throw new Fault(400,'实例信息字段无效');
  const name=input.name.trim(),ip=input.public_ip.trim();
  if(name.length>80||/[\x00-\x1f\x7f]/.test(name)||ip&&!/^(?:0|[1-9]\d{0,2})(?:\.(?:0|[1-9]\d{0,2})){3}$/.test(ip)||ip&&ip.split('.').some(x=>Number(x)>255))throw new Fault(400,'名称最多 80 字，公网地址请填写有效 IPv4');
  if(!await jsonObject(env.CONTROL,prefix(env)+'instances/'+input.id+'.json'))throw new Fault(404,'实例不存在');
  const info={schema:1,instance_id:input.id,name,public_ip:ip,updated_at:new Date().toISOString()};
  const saved=await env.CONTROL.put(prefix(env)+'instance-info/'+input.id+'.json',JSON.stringify(info),{onlyIf:input.expected_etag===null?new Headers({'If-None-Match':'*'}):{etagMatches:input.expected_etag},httpMetadata:{contentType:'application/json',cacheControl:'no-store'}});
  if(!saved)throw new Fault(409,'实例名称已被其他页面修改，请刷新后重试');
  return {saved:true,info};
}
async function putImmutable(bucket,key,text) {
  const made=await bucket.put(key,text,{onlyIf:new Headers({'If-None-Match':'*'}),httpMetadata:{contentType:'application/json',cacheControl:'no-store'}});
  if(made)return;
  const previous=await bucket.get(key);
  if(!previous||await previous.text()!==text)throw new Fault(409,'同一指令编号已有不同内容，请刷新，不要覆盖');
}
function validCommand(body,env) {
  if(body.schema!==1||!safe(body.id)||body.cluster_id!==env.CLUSTER_ID||!safe(body.target)||!actions.has(body.action))throw new Fault(400,'不支持的指令或身份');
  if(!body.expected||!Number.isSafeInteger(body.expected.epoch)||!Number.isSafeInteger(body.expected.revision)||!Number.isSafeInteger(body.bound_epoch)||body.bound_epoch<0)throw new Fault(400,'缺少精确 epoch/revision');
  const created=Date.parse(body.created_at),expires=Date.parse(body.expires_at),now=Date.now();
  if(!Number.isFinite(created)||!Number.isFinite(expires)||expires<=created||expires-created>300000||created>now+30000||expires<=now)throw new Fault(400,'指令时间无效或已过期，请校准电脑时间并刷新');
  if(['handover-request','handover-resume','handover-cancel','source-resume'].includes(body.action)&&(!safe(body.operation_id)||body.operation_id.includes('.')||!Number.isSafeInteger(body.source_epoch)||body.source_epoch<1))throw new Fault(400,'缺少切换操作编号及源 epoch');
  const expectedKeys=['schema','id','cluster_id','target','action','created_at','expires_at','expected','bound_epoch','operation_id','source_epoch'];
  if(Object.keys(body.expected).some(k=>!['revision','epoch','owner','phase'].includes(k))||Object.keys(body).some(k=>!expectedKeys.includes(k)))throw new Fault(400,'未知指令字段');
  return Object.fromEntries(expectedKeys.map(k=>[k,body[k]]));
}
export async function submit(env,input) {
  const c=validCommand(input,env),base=prefix(env),state=await authority(env),instance=await readInstance(env,c.target);
  // Immutable command archives are the idempotency key, including retry after a
  // lost HTTP response. Read them before validating the changing live revision.
  const text=JSON.stringify(c),archive=base+'commands/'+c.target+'/requests/'+c.id+'.json';
  const previous=await env.CONTROL.get(archive);
  if(previous){if(await previous.text()!==text)throw new Fault(409,'指令编号已用于其他内容');
    if(instance.pending?.id===c.id)return {submitted:true,duplicate:true,command:c,result:instance.result};
    // A successful archive write followed by a lost/failed slot write must be
    // recoverable. Do not claim submission merely because the archive exists.
  }
  if(!sameExpected(c.expected,state))throw new Fault(409,'权威状态已变化，请刷新后重新确认');
  if(!instance.report_fresh||!instance.agent_fresh||instance.agent?.schema!==1||instance.agent?.cluster_id!==env.CLUSTER_ID||instance.agent?.mailbox_protocol!==1||instance.agent?.instance_id!==c.target||instance.report?.schema!==1||instance.report?.instance_id!==c.target||instance.report?.cluster_id!==env.CLUSTER_ID||instance.report?.bound_epoch!==c.bound_epoch)throw new Fault(409,'实例或指令轮询报告缺失、过期或身份不符；不能提交');
  if(c.action==='handover-request'&&(state.phase!=='ONLINE'||state.owner===c.target||c.source_epoch!==state.epoch))throw new Fault(409,'当前不满足正常切换条件');
  const slotKey=base+'commands/'+c.target+'/pending.json',slot=await jsonObject(env.CONTROL,slotKey);
  if(slot){
    const receipt=await jsonObject(env.CONTROL,base+'commands/'+c.target+'/results/'+slot.value.id+'.json');
    // TTL alone never frees a slot: an old process could have already claimed
    // it. A poller must publish a terminal receipt, or explicit audited abandon.
    if(!receipt||!terminal.has(receipt.value.state))throw new Fault(409,'此实例已有未结束指令；查看回执，不能覆盖或按超时自动清空');
    if(receipt.value.schema!==1||receipt.value.id!==slot.value.id||receipt.value.target!==c.target||receipt.value.command_sha256!==await sha256(JSON.stringify(slot.value)))throw new Fault(409,'旧指令回执哈希不匹配');
  }
  await putImmutable(env.CONTROL,archive,text);
  const saved=await env.CONTROL.put(slotKey,text,{onlyIf:slot?{etagMatches:slot.etag}:new Headers({'If-None-Match':'*'}),httpMetadata:{contentType:'application/json',cacheControl:'no-store'}});
  if(!saved)throw new Fault(409,'并发提交冲突；刷新核对指令编号，不自动重试');
  const confirmed=await env.CONTROL.get(slotKey);
  if(!confirmed||await confirmed.text()!==text)throw new Fault(503,'提交回读不一致；结果未知，请按原指令编号查询');
  return {submitted:true,command:c,note:'仅表示指令已存入 R2，尚不代表已执行或切换完成'};
}
async function abandon(env,body) {
  // Abandon only an expired, NEVER-CLAIMED slot. A conditional cancellation
  // marker is inserted into the same receipt key before releasing the slot, so
  // an in-flight poller can no longer obtain CLAIMED authorization.
  const {target,id}=body;if(!safe(target)||!safe(id))throw new Fault(400,'无效实例/指令 ID');
  const base=prefix(env),slot=await jsonObject(env.CONTROL,base+'commands/'+target+'/pending.json',true);
  if(slot.value.id!==id||Date.now()<=Date.parse(slot.value.expires_at))throw new Fault(409,'只允许关闭已过期的同一指令');
  const receipt={schema:1,id,target,command_sha256:await sha256(JSON.stringify(slot.value)),state:'REJECTED',at:new Date().toISOString(),action:slot.value.action,operation_id:slot.value.operation_id,error:'operator cancelled expired unclaimed command; no ownership change'};
  const key=base+'commands/'+target+'/results/'+id+'.json';
  if(!await env.CONTROL.put(key,JSON.stringify(receipt),{onlyIf:new Headers({'If-None-Match':'*'})}))throw new Fault(409,'已有执行回执；不得清除执行中的指令');
  await putImmutable(env.CONTROL,base+'audit/abandon-'+target+'-'+id+'.json',JSON.stringify(receipt));
  return {cancelled:true,receipt};
}
async function diagnostics(env) {
  const current=await status(env),events=[];
  for(const instance of current.instances){
    const keys=await allObjects(env.CONTROL,prefix(env)+'commands/'+instance.id+'/results/');
    for(const entry of keys.slice(-10)){const value=await jsonObject(env.CONTROL,entry.key);if(value)events.push(value.value);}
  }
  return {schema:1,generated_at:new Date().toISOString(),status:current,command_results:events,note:'报告只含 R2 运维状态，不读取 S3 密钥、消息数据库或原始日志。APPLIED 表示管理命令返回，不代表业务切换已完成。'};
}
async function dns(env,body) {
  if(!env.CF_DNS_TOKEN||!env.CF_ZONE_ID||!env.CF_RECORD_ID||!env.CF_RECORD_NAME)throw new Fault(409,'Cloudflare DNS API 未启用，可手动修改 IP');
  if(!/^[a-f0-9]{32}$/i.test(env.CF_ZONE_ID)||!/^[a-f0-9]{32}$/i.test(env.CF_RECORD_ID))throw new Fault(503,'Cloudflare 记录配置无效');
  const url=`https://api.cloudflare.com/client/v4/zones/${env.CF_ZONE_ID}/dns_records/${env.CF_RECORD_ID}`;
  const call=async(method,payload)=>{const response=await fetch(url,{method,redirect:'error',headers:{Authorization:'Bearer '+env.CF_DNS_TOKEN,'Content-Type':'application/json'},body:payload?JSON.stringify(payload):undefined,signal:AbortSignal.timeout(10000)});if(!response.ok)throw new Fault(502,'Cloudflare DNS HTTP '+response.status);const result=await response.json();if(!result.success)throw new Fault(502,'Cloudflare DNS API 失败');return result.result;};
  const old=await call('GET');if(old.name!==env.CF_RECORD_NAME||old.type!=='A')throw new Fault(409,'Cloudflare 记录名称或类型不符');
  if(!body)return {record:old};
  if(!safe(body.id)||!/^\d{1,3}(\.\d{1,3}){3}$/.test(body.ip)||body.ip.split('.').some(x=>Number(x)>255)||body.expected_ip!==old.content)throw new Fault(409,'IP 无效或 DNS 已变化，请刷新');
  const audit={schema:1,id:body.id,at:new Date().toISOString(),record_name:old.name,old_ip:old.content,new_ip:body.ip};
  await putImmutable(env.CONTROL,prefix(env)+'audit/dns-'+body.id+'.json',JSON.stringify(audit));
  const updated=await call('PATCH',{content:body.ip});
  return {record:updated,note:'DNS 更新不证明旧连接已排空；仍需要正常切换的停写屏障'};
}
function originAllowed(request){const origin=request.headers.get('Origin');return !origin||origin===new URL(request.url).origin;}
function response(request,data,code=200){const headers={'Content-Type':'application/json; charset=utf-8','Cache-Control':'no-store','X-Content-Type-Options':'nosniff','Referrer-Policy':'no-referrer'};return new Response(JSON.stringify(data),{status:code,headers});}
async function body(request){if(!(request.headers.get('Content-Type')||'').startsWith('application/json'))throw new Fault(415,'需要 application/json');const text=await request.text();if(encoder.encode(text).length>16384)throw new Fault(413,'请求过大');try{return JSON.parse(text);}catch{throw new Fault(400,'JSON 无效');}}
export default {async fetch(request,env){
  const url=new URL(request.url);
  if(!url.pathname.startsWith('/api/'))return env.ASSETS?env.ASSETS.fetch(request):new Response('Not found',{status:404});
  if(!originAllowed(request))return new Response('Origin denied',{status:403});
  try{
    const auth=await verifyAccess(request,env);
    if(auth.status!==200)return response(request,{error:auth.error},auth.status);
    if(request.method==='OPTIONS')return new Response(null,{status:204,headers:{'Cache-Control':'no-store'}});
    let result;
    if(request.method==='GET'&&url.pathname==='/api/status')result=await status(env);
    else if(request.method==='GET'&&url.pathname==='/api/diagnostic')result=await diagnostics(env);
    else if(request.method==='POST'&&url.pathname==='/api/instance-info')result=await saveInstanceInfo(env,await body(request));
    else if(request.method==='POST'&&url.pathname==='/api/command')result=await submit(env,await body(request));
    else if(request.method==='POST'&&url.pathname==='/api/abandon-expired')result=await abandon(env,await body(request));
    else if(request.method==='GET'&&url.pathname==='/api/dns')result=await dns(env);
    else if(request.method==='POST'&&url.pathname==='/api/dns')result=await dns(env,await body(request));
    else throw new Fault(404,'未知 API');
    return response(request,result);
  }catch(error){return response(request,{error:error instanceof Fault?error.message:'存储或服务异常；刷新核对原指令编号，勿自动重试'},error instanceof Fault?error.status:503);}
}};
