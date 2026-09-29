'use strict';
// Maintainer-only generator using the pinned checkout's existing TypeScript AST.
// Runtime and tests consume generated Go patterns / JSON, with no npm dependency.
const fs = require('node:fs');
const path = require('node:path');
const ts = require(path.join(process.argv[2], 'node_modules/typescript/lib/typescript.js'));
const fixture = require('./manual_sources_fixture.json');
const aliases = {Jg:'normalize', Yg:'stringvalue', e_:'tokenparse', $g:'decode', u_:'extractaccount', d_:'account', fk:'header', gk:'fetch', vk:'consume', yk:'codexconfig', pO:'project', gO:'agconfig', HO:'claudeconfig', ck:'devinconfig', jk:'kimiconfig', kk:'metaconfig', Fk:'user', Ik:'xaiheader', zk:'xaiconfig', Jk:'registry', ak:'devinfetch', ik:'devinfactory', Ok:'metafetch', wm:'quota', Pk:'record', Rk:'paidfetch', cv:'paid', Ag:'codexheaders', xg:'agheaders', wg:'claudeheaders', Mg:'kimiheaders', Rg:'xaiheaders', zg:'paidheaders', Lk:'billingfetch'};
const globals = new Set(['Number','Array','Object','JSON','Math','Map','Set','Promise','Error','window','globalThis','atob','undefined']);
const output = ['// Code generated from pinned actual compiled AST by generate_source_templates.cjs; DO NOT EDIT.', '// Every @slot is a lexical identifier, never a minified-name ABI.', 'package web', '', 'var sourceTemplates = map[string]string{'];
for (const [name, text] of Object.entries(fixture.nodes)) {
  const key = aliases[name]; if (!key) throw new Error('Missing semantic alias: ' + name);
  const prefix = text.startsWith('function ') ? '' : 'var ';
  const ast = ts.createSourceFile('source.js', prefix + text + ';', ts.ScriptTarget.Latest, true, ts.ScriptKind.JS);
  const locals = new Set(), edits = [];
  function binding(node) {
    if (ts.isIdentifier(node)) {if (node.text !== name) locals.add(node.text);}
    else if (ts.isObjectBindingPattern(node) || ts.isArrayBindingPattern(node)) for (const element of node.elements) if (ts.isBindingElement(element)) binding(element.name);
  }
  function local(node) {
    if (ts.isParameter(node) || ts.isVariableDeclaration(node)) binding(node.name);
    ts.forEachChild(node, local);
  }
  local(ast);
  function visit(node) {
    if (ts.isIdentifier(node)) {
      const p = node.parent;
      const property = (ts.isPropertyAccessExpression(p) && p.name === node) || ((ts.isPropertyAssignment(p) || ts.isBindingElement(p)) && p.propertyName === node) || (ts.isPropertyAssignment(p) && p.name === node);
      if (!property && !globals.has(node.text)) {
        const slot = aliases[node.text] || (locals.has(node.text) ? key + 'local' + Buffer.from(node.text).toString('hex') : 'ref' + Buffer.from(node.text).toString('hex'));
        edits.push({start:node.getStart(ast)-prefix.length,end:node.end-prefix.length,text:'@'+slot});
      }
    }
    ts.forEachChild(node, visit);
  }
  visit(ast);
  let result = text;
  for (const edit of edits.sort((a,b)=>b.start-a.start)) result = result.slice(0,edit.start)+edit.text+result.slice(edit.end);
  output.push('\t' + JSON.stringify(key) + ': ' + JSON.stringify(result) + ',');
}
output.push('}', '');
process.stdout.write(output.join('\n'));
