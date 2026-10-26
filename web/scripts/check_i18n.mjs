// @ts-nocheck
import { readFileSync, readdirSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { parse } from 'svelte/compiler';
import ts from 'typescript';

const webRoot = resolve(fileURLToPath(new URL('..', import.meta.url)));
const visibleAttributes = new Set(['aria-label', 'alt', 'placeholder', 'title']);

export function catalogIssues(source) {
  const ast = ts.createSourceFile('i18n.ts', source, ts.ScriptTarget.Latest, true);
  const catalogs = new Map();
  for (const statement of ast.statements) {
    if (!ts.isVariableStatement(statement)) continue;
    for (const decl of statement.declarationList.declarations) {
      if (!ts.isIdentifier(decl.name) || !['en', 'de'].includes(decl.name.text)) continue;
      const initializer = ts.isAsExpression(decl.initializer) ? decl.initializer.expression : decl.initializer;
      if (!initializer || !ts.isObjectLiteralExpression(initializer)) continue;
      const entries = new Map();
      for (const property of initializer.properties) {
        if (!ts.isPropertyAssignment(property)) continue;
        const key = ts.isIdentifier(property.name) || ts.isStringLiteral(property.name) ? property.name.text : undefined;
        if (!key || !ts.isStringLiteralLike(property.initializer)) continue;
        if (entries.has(key)) return [`Duplicate ${decl.name.text}.${key}`];
        entries.set(key, property.initializer.text);
      }
      catalogs.set(decl.name.text, entries);
    }
  }
  const en = catalogs.get('en'), de = catalogs.get('de');
  if (!en || !de) return ['English or German catalog missing'];
  const issues = [];
  for (const [locale, own, other] of [['en', en, de], ['de', de, en]]) {
    for (const [key, value] of own) {
      if (!value.trim()) issues.push(`Empty ${locale}.${key}`);
      if (!other.has(key)) issues.push(`Missing ${locale === 'en' ? 'de' : 'en'}.${key}`);
    }
  }
  for (const [key, value] of en) {
    if (!de.has(key)) continue;
    const placeholders = (text) => [...text.matchAll(/\{([A-Za-z]\w*)\}/g)].map((m) => m[1]).sort().join(',');
    if (placeholders(value) !== placeholders(de.get(key))) issues.push(`Placeholder mismatch ${key}`);
  }
  return issues;
}

export function svelteIssues(source) {
  const ast = parse(source, { modern: true });
  const issues = [];
  const line = (offset) => source.slice(0, offset).split('\n').length;
  function literalText(expression) {
    if (expression?.type === 'Literal' && typeof expression.value === 'string') return [expression.value];
    if (expression?.type === 'ConditionalExpression') return [
      ...literalText(expression.consequent), ...literalText(expression.alternate)
    ];
    return [];
  }
  function visit(node) {
    if (!node || typeof node !== 'object') return;
    if (node.type === 'Text') {
      const value = node.data?.trim();
      if (value && /\p{L}/u.test(value)) issues.push(`line ${line(node.start)}: hard-coded text ${JSON.stringify(value)}`);
      return;
    }
    if (node.type === 'RegularElement' && node.attributes) {
      for (const attr of node.attributes) {
        if (attr.type !== 'Attribute' || !visibleAttributes.has(attr.name)) continue;
        if (attr.value?.length === 1 && attr.value[0].type === 'Text') {
          const value = attr.value[0].data.trim();
          if (value && /\p{L}/u.test(value)) issues.push(`line ${line(attr.start)}: hard-coded ${attr.name} ${JSON.stringify(value)}`);
        }
        const parts = Array.isArray(attr.value) ? attr.value : [attr.value];
        for (const part of parts) {
          for (const value of literalText(part.expression)) {
            if (/\p{L}/u.test(value)) issues.push(`line ${line(attr.start)}: hard-coded ${attr.name} ${JSON.stringify(value)}`);
          }
        }
      }
    }
    for (const [key, value] of Object.entries(node)) {
      if (['attributes', 'expression', 'metadata', 'name_loc', 'script', 'css', 'instance', 'module'].includes(key)) continue;
      if (Array.isArray(value)) value.forEach(visit);
      else if (value && typeof value === 'object' && typeof value.type === 'string') visit(value);
    }
  }
  visit(ast.fragment);
  return issues;
}

function svelteFiles(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name);
    return entry.isDirectory() ? svelteFiles(path) : entry.name.endsWith('.svelte') ? [path] : [];
  });
}

export function audit(root = webRoot) {
  const issues = catalogIssues(readFileSync(join(root, 'src/lib/i18n.ts'), 'utf8'));
  for (const path of svelteFiles(join(root, 'src'))) {
    for (const issue of svelteIssues(readFileSync(path, 'utf8'))) issues.push(`${relative(root, path)}: ${issue}`);
  }
  return issues;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const issues = audit();
  if (issues.length) {
    console.error(issues.join('\n'));
    process.exitCode = 1;
  } else console.log('i18n catalogs and Svelte text check passed');
}
