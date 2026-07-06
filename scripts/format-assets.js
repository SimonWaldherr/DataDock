#!/usr/bin/env node

const fs = require('node:fs');
const path = require('node:path');

const checkOnly = process.argv.includes('--check');
const root = process.cwd();
const targets = ['README.md', 'static', 'templates'];
const extensions = new Set(['.css', '.html', '.js', '.md']);

function walk(entry) {
  const full = path.join(root, entry);
  if (!fs.existsSync(full)) return [];
  const stat = fs.statSync(full);
  if (stat.isFile()) return extensions.has(path.extname(full)) ? [full] : [];
  if (!stat.isDirectory()) return [];
  return fs.readdirSync(full).flatMap((child) => walk(path.join(entry, child)));
}

function formatText(input) {
  const normalized = input.replace(/\r\n?/g, '\n');
  const trimmed = normalized
    .split('\n')
    .map((line) => line.replace(/[ \t]+$/g, ''))
    .join('\n');
  return trimmed.replace(/\n*$/g, '') + '\n';
}

const changed = [];
for (const file of targets.flatMap(walk)) {
  const before = fs.readFileSync(file, 'utf8');
  const after = formatText(before);
  if (after === before) continue;
  changed.push(path.relative(root, file));
  if (!checkOnly) fs.writeFileSync(file, after);
}

if (changed.length > 0) {
  const action = checkOnly ? 'would format' : 'formatted';
  changed.forEach((file) => console.log(`${action}: ${file}`));
  if (checkOnly) process.exit(1);
}
