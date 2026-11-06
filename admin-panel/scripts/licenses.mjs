// Packaging helper. Input is `go list -m -f '{{.Path}}|{{.Version}}|{{.Dir}}' all`.
import fs from 'node:fs';
import path from 'node:path';
const [input,output]=process.argv.slice(2);
if(!input||!output)throw new Error('Usage: node scripts/licenses.mjs dist/modules.txt dist/THIRD_PARTY_LICENSES.txt');
let text='Third-party Go module licenses bundled with Dozor\n\n';
for(const line of fs.readFileSync(input,'utf8').replace(/^\uFEFF/,'').split(/\r?\n/)){
  const [name,version,dir]=line.split('|');if(!version||!dir)continue;
  text+=`\n${'='.repeat(72)}\n${name} ${version}\n${'='.repeat(72)}\n`;
  const files=fs.readdirSync(dir).filter(name=>/^(LICENSE|COPYING|NOTICE)(\.|$)/i.test(name));
  if(!files.length)throw new Error(`No license found: ${name}`);
  for(const file of files){const filePath=path.join(dir,file);if(fs.statSync(filePath).isFile())text+=`\n--- ${file} ---\n${fs.readFileSync(filePath,'utf8')}\n`;}
}
fs.writeFileSync(output,text);
