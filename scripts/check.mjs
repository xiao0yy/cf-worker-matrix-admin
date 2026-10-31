import {readFileSync,readdirSync} from 'node:fs';
import vm from 'node:vm';
for (const file of ['app.js','handover.js']) new vm.Script(readFileSync(new URL('../web/'+file,import.meta.url),'utf8'));
await import('../worker/index.js');
for(const path of ['../web/index.html','../web/style.css','../web/handover.css'])if(!readFileSync(new URL(path,import.meta.url),'utf8').length)throw new Error('Empty frontend asset');
console.log('Worker module and browser assets checked.');

const allowed=new Set(['index.html','app.js','handover.js','style.css','handover.css','_headers','fonts']);
for(const name of readdirSync(new URL('../web/',import.meta.url)))if(!allowed.has(name))throw new Error('Unexpected public asset (possibly private config): '+name);
const fonts=new Set(['matrix-admin-display.woff2','OFL.txt','SOURCE.json']);
for(const name of readdirSync(new URL('../web/fonts/',import.meta.url)))if(!fonts.has(name))throw new Error('Unexpected font asset: '+name);
