#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

node <<'NODE'
const fs = require('node:fs');
const path = require('node:path');

const documents = ['README.md', 'docs/DEPLOYMENT.md'];
const required = [
  ['README.md', 'docs/DEPLOYMENT.md'],
  ['README.md', 'LICENSE'],
  ['docs/DEPLOYMENT.md', 'compose.yml'],
];

for (const file of documents) {
  if (!fs.existsSync(file)) throw new Error(`缺少文档：${file}`);
}

for (const [file, fragment] of required) {
  const source = fs.readFileSync(file, 'utf8');
  if (!source.includes(fragment)) throw new Error(`${file} 缺少引用：${fragment}`);
}

for (const file of documents) {
  const source = fs.readFileSync(file, 'utf8');
  const links = [...source.matchAll(/\]\(([^)]+)\)/g)].map((match) => match[1]);
  for (const link of links) {
    if (/^(?:https?:|mailto:|#)/.test(link)) continue;
    const target = link.split('#', 1)[0].split('?', 1)[0];
    if (!target) continue;
    const resolved = path.resolve(path.dirname(file), target);
    if (!fs.existsSync(resolved)) throw new Error(`${file} 链接目标不存在：${link}`);
  }
}

const deployment = fs.readFileSync('docs/DEPLOYMENT.md', 'utf8');
for (const phrase of ['docker compose up -d', 'SLA_IMAGE', '/healthz', 'ADMIN_TOKEN']) {
  if (!deployment.includes(phrase)) throw new Error(`部署文档缺少关键内容：${phrase}`);
}
console.log('✅ README 与部署文档链接及关键入口校验通过');
NODE
