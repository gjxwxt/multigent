#!/usr/bin/env node
/*
 * docs-check: deterministic drift gate between docs/api-spec.md and the
 * actual Spring controllers.
 *
 * Extracts `HTTP METHOD /path` rows from docs/api-spec.md tables and
 * `@XxxMapping` annotations from server/src controllers, then fails when the
 * two sets differ. Deterministic on purpose (no AI judgment) — same idea as
 * the platform's ci_ready gate: if documentation can rot silently, it will.
 *
 * Exit 0 = in sync; exit 1 = drift detected (print the diff).
 */
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const specPath = path.join(repoRoot, 'docs', 'api-spec.md')
const serverSrc = path.join(repoRoot, 'server', 'src', 'main', 'java')

const HTTP_VERBS = ['GET', 'POST', 'PUT', 'PATCH', 'DELETE']

function fail(message) {
  console.error(`docs-check: FAIL — ${message}`)
  process.exit(1)
}

function extractSpecRoutes(markdown) {
  const routes = new Set()
  // Matches "| GET | `/api/v1/items` | ..." rows, "### `GET /api/health`"
  // headers, and the "- **路径**：`GET /api/v1/items`" bullet style.
  const patterns = [
    /\|\s*([A-Z]+)\s*\|\s*`([^`]+)`/g,
    /###\s*`([A-Z]+)\s+([^`]+)`/g,
    /路径\*{2}[：:]\s*`([A-Z]+)\s+([^`]+)`/g,
  ]
  for (const pattern of patterns) {
    for (const match of markdown.matchAll(pattern)) {
      const verb = match[1].toUpperCase()
      const route = match[2].trim()
      if (HTTP_VERBS.includes(verb) && route.startsWith('/')) {
        routes.add(`${verb} ${route}`)
      }
    }
  }
  return routes
}

function walkJavaFiles(dir) {
  const out = []
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name)
    if (entry.isDirectory()) out.push(...walkJavaFiles(full))
    else if (entry.name.endsWith('.java')) out.push(full)
  }
  return out
}

function extractControllerRoutes() {
  if (!fs.existsSync(serverSrc)) fail(`server source directory not found: ${serverSrc}`)
  const routes = new Set()
  for (const file of walkJavaFiles(serverSrc)) {
    // Only @RestController classes define the HTTP surface we document.
    const source = fs.readFileSync(file, 'utf-8')
    if (!/@RestController/.test(source)) continue

    const classPrefix = source.match(/@RequestMapping\(\s*"([^"]+)"\s*\)/)?.[1] ?? ''
    const mappingRe = /@(GetMapping|PostMapping|PutMapping|PatchMapping|DeleteMapping)(?:\(\s*(?:value\s*=\s*)?"([^"]*)"[^)]*\))?/g
    for (const match of source.matchAll(mappingRe)) {
      const verb = match[1].replace(/Mapping$/, '').toUpperCase()
      const sub = match[2] ?? ''
      // Normalize {} placeholders: docs use {id}, Spring accepts both {id} — keep as-is.
      routes.add(`${verb} ${joinPath(classPrefix, sub)}`)
    }
    // @RequestMapping(method = RequestMethod.X) style
    const methodRe = /@RequestMapping\([^)]*method\s*=\s*RequestMethod\.([A-Z_]+)[^)]*value\s*=\s*"([^"]*)"/g
    for (const match of source.matchAll(methodRe)) {
      routes.add(`${match[1]} ${joinPath(classPrefix, match[2])}`)
    }
  }
  return routes
}

function joinPath(prefix, sub) {
  const joined = `${prefix.replace(/\/$/, '')}/${sub.replace(/^\//, '')}`
  return joined.replace(/\/{2,}/g, '/').replace(/\/$/, '') || '/'
}

const specRoutes = extractSpecRoutes(fs.readFileSync(specPath, 'utf-8'))
if (specRoutes.size === 0) fail('no routes parsed from docs/api-spec.md — check the doc format')

const codeRoutes = extractControllerRoutes()
if (codeRoutes.size === 0) fail('no routes parsed from controllers — check @RestController mappings')

const missingInSpec = [...codeRoutes].filter((r) => !specRoutes.has(r)).sort()
const missingInCode = [...specRoutes].filter((r) => !codeRoutes.has(r)).sort()

if (missingInSpec.length > 0 || missingInCode.length > 0) {
  console.error('docs-check: FAIL — api-spec.md and controllers diverged')
  for (const r of missingInSpec) console.error(`  in code but not in docs/api-spec.md: ${r}`)
  for (const r of missingInCode) console.error(`  in docs/api-spec.md but not in code: ${r}`)
  process.exit(1)
}

console.log(`docs-check: ok — ${specRoutes.size} documented routes match controllers`)
