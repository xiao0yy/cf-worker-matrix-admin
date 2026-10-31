import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import vm from 'node:vm';

function browser(fetch) {
  const nodes=new Map();
  const context=vm.createContext({
    document:{getElementById(id){if(!nodes.has(id))nodes.set(id,{value:id==='action'?'probe':'',textContent:''});return nodes.get(id);}},
    location:{protocol:'file:',origin:'null'}, crypto, AbortSignal, fetch,
    setInterval(){throw Error('file pages must not poll authenticated API');},
  });
  vm.runInContext(readFileSync(new URL('../web/app.js',import.meta.url),'utf8'),context);
  return context;
}

test('browser API uses the Access session on the same origin without a bearer token',async()=>{
  let captured;
  const c=browser(async(path,options)=>{captured={path,options};return Response.json({ok:true});});
  await vm.runInContext("api('/api/status')",c);
  assert.equal(captured.path,'/api/status');
  assert.equal(captured.options.credentials,'same-origin');
  assert.equal(captured.options.redirect,'manual');
  assert.equal(captured.options.headers.Authorization,undefined);
});

test('expired Access login stops polling and never replays a write',async()=>{
  for(const response of [{type:'opaqueredirect',status:0},new Response('',{status:401}),new Response('',{status:403})]){
    let calls=0;
    const c=browser(async()=>{calls++;return response;});
    await assert.rejects(vm.runInContext("api('/api/command','POST',{id:'original-command'})",c),/重新登录/);
    await vm.runInContext('refresh()',c);
    assert.equal(calls,1);
  }
});

test('human machine labels include IP and stable ID, with hostname fallback',()=>{
 const c=browser(async()=>Response.json({}));
 const label=vm.runInContext("instanceLabel({id:'node-1',info:{name:'primary-vps',public_ip:'192.0.2.10'}})",c);
 assert.equal(label,'primary-vps · 192.0.2.10 [node-1]');
 assert.equal(vm.runInContext("instanceName({id:'node-2',agent:{host:{hostname:'backup-vps'}}})",c),'backup-vps');
 assert.equal(vm.runInContext("instanceName({id:'node-3'})",c),'node-3');
});

test('background refresh preserves typed identity and its original concurrency token',()=>{
 const c=browser(async()=>Response.json({}));
 vm.runInContext("snapshot={instances:[{id:'node-1',info:{name:'old',public_ip:'192.0.2.11'},info_etag:'old-etag'},{id:'node-2',info:{name:'backup'},info_etag:'other-etag'}]};document.getElementById('target').value='node-1';fillInstanceInfo();document.getElementById('instance-name').value='my edit';document.getElementById('instance-name').oninput();snapshot.instances[0].info.name='other browser edit';snapshot.instances[0].info_etag='new-etag';fillInstanceInfo();",c);
 assert.equal(vm.runInContext("document.getElementById('instance-name').value",c),'my edit');
 assert.equal(vm.runInContext('infoETag',c),'old-etag');
 vm.runInContext("document.getElementById('target').value='node-2';document.getElementById('target').onchange();",c);
 assert.equal(vm.runInContext("document.getElementById('instance-name').value",c),'backup');
 assert.equal(vm.runInContext('infoETag',c),'other-etag');
});
