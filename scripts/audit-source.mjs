// Audit tracked + unignored source only. Never traverse a live instance tree.
import {execFileSync} from 'node:child_process';
import {existsSync,lstatSync,readFileSync} from 'node:fs';
import {basename} from 'node:path';
import {createHash} from 'node:crypto';
const paths=[...new Set(execFileSync('git',['ls-files','-c','-o','--exclude-standard','-z'],{encoding:'utf8'}).split('\0').filter(Boolean))];
const errors=[];
for(const path of paths){
  if(!existsSync(path))continue; // tracked deletion pending commit
  if(lstatSync(path).isSymbolicLink()){errors.push(`${path}: source symlink`);continue;}
  const name=basename(path);
  if(name!=='.dev.vars.example' && name!=='.env.example' && !name.endsWith('.env.example') &&
    (/^(?:\.dev\.vars|\.env)(?:\.|$)/.test(name) || name.endsWith('.env') ||
     name.endsWith('.local.json') || /^\.wrangler-local-/.test(name))) errors.push(`${path}: private deployment configuration`);
  if(/(^|\/)(s3\.env|restic\.env|host\.env|handover\.env|local\.json|journal\.json|registry\.json|pending-update\.json|registration-token)$/.test(path)||/\.(db|sqlite|db-wal|db-shm|pem|key|log|tgz|tar\.gz)$/.test(path))errors.push(`${path}: private/runtime file`);
  if(/(^|\/)(state|data|agent|node_modules|dist|bin|\.wrangler)\//.test(path))errors.push(`${path}: generated/runtime directory`);
  const content=readFileSync(path);
  // The only public binary is our licensed, checksum-pinned heading font.
  if(path==='web/fonts/matrix-admin-display.woff2'){
    const font=JSON.parse(readFileSync('web/fonts/SOURCE.json','utf8'));
    if(content.length>256*1024||content.subarray(0,4).toString()!=='wOF2'||
      createHash('sha256').update(content).digest('hex')!==font.sha256)
      errors.push(`${path}: font differs from its reviewed provenance`);
    continue;
  }
  if(content.length>2*1024*1024||content.includes(0)){errors.push(`${path}: unexpected binary/large file`);continue;}
  if(/-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----/.test(content.toString()))errors.push(`${path}: private key material`);
  if(path==='wrangler.jsonc'){
    const config=JSON.parse(content.toString());
    for(const key of ['vars','r2_buckets','account_id','route','routes','env']){
      if(Object.hasOwn(config,key))errors.push(`${path}: deployment-specific ${key} must stay in Cloudflare`);
    }
  }
}
if(errors.length)throw new Error(errors.join('\n'));
console.log(`Audited ${paths.length} source paths; no private/runtime files or unapproved binary artifacts in the publishable tree.`);
