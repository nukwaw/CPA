'use strict';
// Optional fixture maintainer: AST-extract exact lexical nodes from the pinned
// existing upstream build. No upstream writes or dependency installation.
const fs = require('node:fs');
const path = require('node:path');
const checkout = process.argv[2];
const ts = require(path.join(checkout, 'node_modules/typescript/lib/typescript.js'));
const html = fs.readFileSync(path.join(checkout, 'dist/index.html'), 'utf8');
const body = [...html.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script\s*>/gi)].find(match => /type="module"/.test(match[1]))[2];
const ast = ts.createSourceFile('management.js', body, ts.ScriptTarget.Latest, true, ts.ScriptKind.JS);
const wanted = new Set(['Jg', 'Yg', 'u_', 'd_', 'fk', 'gk', 'vk', 'yk', 'pO', 'gO', 'Fk', 'Ik', 'zk', 'HO', 'ck', 'jk', 'kk', 'Jk', 'ak', 'ik', 'Ok', 'e_', 'Pk', 'Rk', 'cv', 'Ag', 'xg', 'wg', 'Mg', 'Rg', '$g', 'zg', 'Lk']);
const nodes = {};
function visit(node) {
  if ((ts.isVariableDeclaration(node) || ts.isFunctionDeclaration(node)) && node.name && wanted.has(node.name.text)) {
    if (nodes[node.name.text]) throw new Error('Duplicate lexical node: ' + node.name.text);
    nodes[node.name.text] = node.getText(ast);
  }
  ts.forEachChild(node, visit);
}
visit(ast);
for (const name of wanted) if (!nodes[name]) throw new Error('Missing ' + name);
process.stdout.write(JSON.stringify({commit: '4530da271ba2e89810d4dccebc57f3091afa590a', order: Object.keys(nodes), nodes}, null, 2) + '\n');
