import {readFileSync,readdirSync} from 'node:fs';
import vm from 'node:vm';
new vm.Script(readFileSync(new URL('../web/app.js',import.meta.url),'utf8'));
await import('../worker/index.js');
for(const path of ['../web/index.html','../web/style.css'])if(!readFileSync(new URL(path,import.meta.url),'utf8').length)throw new Error('Empty frontend asset');
console.log('Worker module and browser assets checked.');

const allowed=new Set(['index.html','app.js','style.css','_headers']);
for(const name of readdirSync(new URL('../web/',import.meta.url)))if(!allowed.has(name))throw new Error('Unexpected public asset (possibly private config): '+name);
