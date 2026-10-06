#!/usr/bin/env python3
"""endpoint-crosswalk: license-remediation B2 evidence extractor.

Cross-references backend endpoint registrations (internal/api/server.go
mux.HandleFunc/Handle calls, all muxes) against the frontend's actual API
call literals (web/src, every fetch/api-helper string). Produces the
four-tier crosswalk worksheet that the STOP-gated trimming decision will
consume.

Tiers (worksheet labels only — NO keep/drop verdicts are made here):
  zero-call          registered, no frontend literal found anywhere
  frontend-called    registered AND reachable from >= 1 frontend call site
  platform-internal  health/version/preview proxy/IM callbacks/runtime
                     runtime-node/context-ingest/deploy-verify — machine or
                     subsystem callers, not interactive UI
  websocket-surface  WS/SSE/streaming/event surfaces, listed separately
                     per v3 §7.2 (upstream-concentrated realtime plane)

Matching semantics (conservative, evidenced):
  - frontend literals are cut at the first `${...}` interpolation; the
    static prefix is matched against registered routes segment-wise
  - `{param}` route segments match any literal segment
  - a literal ending in '/' prefix-matches routes whose segments continue it
  - query strings are stripped for matching but kept in the evidence
  - agent_channel_ws is an OUTBOUND websocket bridge (refreshAgentIMBridges
    dials IM providers), not a registered HTTP route — recorded in the
    worksheet's WS notes instead of the endpoint table.

Usage:
  python3 main.py [--repo DIR] [--out-dir DIR]
Outputs (in --out-dir):
  endpoint-crosswalk.json — machine-exact, every endpoint + match evidence
  endpoint-crosswalk.md   — human worksheet
Exit: 0 always (evidence tool, no gate).
"""
import json
import os
import re
import sys
from collections import defaultdict

MUX_HANDLE = re.compile(r'(\w*Mux|\bmux)\.(?:HandleFunc|Handle)\(\s*"([^"]+)"')
# Frontend literals: a quote/backtick, then optionally a ${...} preamble
# (apiBase()), then /api/v1/... Interpolation bodies may contain quotes
# (encodeURIComponent(project || 'current')) and must not terminate the
# match — only a bare quote/backtick outside ${...} does.
FE_LITERAL = re.compile(r'[\'"`](?:\$\{[^}]*\})?(/api/v1/(?:\$\{[^}`]*\}|[^\'"`])*)')

# Platform-internal surfaces: machine or subsystem callers, no interactive
# UI caller expected. Worksheet heuristic — every entry user-verifiable.
INTERNAL_PREFIXES = (
    '/api/v1/runtime/',
    '/api/v1/runtime-node/',
    '/api/v1/im/',
    '/api/v1/context/ingest',
    '/api/v1/deploy-verify',
    '/api/v1/oauth/callback',  # browser redirect target (provider -> server)
)
INTERNAL_EXACT = {
    '/api/v1/health', '/api/v1/version',
    '/preview/', '/_next/', '/fonts/', '/design-systems/',
    '/api/', '/projects/', '/',
}


def backend_endpoints(repo):
    """Extract every (mux, method, route, handler, line) registration from
    internal/api/server.go."""
    path = os.path.join(repo, 'internal/api/server.go')
    src = open(path, encoding='utf-8', errors='replace').read()
    eps = {}
    for m in MUX_HANDLE.finditer(src):
        mux, pattern = m.group(1), m.group(2)
        line = src[:m.start()].count('\n') + 1
        key = (mux, pattern)
        eps.setdefault(key, {'lines': [], 'handler': ''})
        eps[key]['lines'].append(line)
        tail = src[m.end():m.end() + 120]
        hm = re.match(r'\s*,?\s*(?:s\.)?(\w+)', tail)
        if hm and not eps[key]['handler']:
            eps[key]['handler'] = hm.group(1)
    out = []
    for (mux, pattern), meta in eps.items():
        method, route = ('*', pattern) if ' ' not in pattern else pattern.split(' ', 1)
        out.append({
            'mux': mux, 'method': method, 'pattern': pattern, 'route': route,
            'handler': meta['handler'], 'lines': meta['lines'],
        })
    out.sort(key=lambda e: (e['mux'], e['route'], e['method']))
    return out


# Variable-composed call sites: `const designBase = \`/api/v1/...\`` followed
# by `${designBase}/start` produces API literals with no /api/v1 prefix.
# One-level expansion: capture the const assignment, then rewrite
# ${varName} at literal start.
FE_VARDEF = re.compile(r'const\s+(\w+)\s*=\s*[\'"`](/api/v1/[^\'"`]*)[\'"`]')
FE_VARREF = re.compile(r'[\'"`]\$\{(\w+)\}(/[^\'"`]*)')


def frontend_calls(repo):
    """Every /api/v1... string literal in web/src (ts/tsx), mapped to the
    files that use it. Literals are kept raw — matching happens later.
    One-level variable composition is expanded: a `${var}/suffix` literal
    where var is a file-local const with an /api/v1 template becomes
    var-template + suffix."""
    calls = defaultdict(set)
    base = os.path.join(repo, 'web/src')
    for root, dirs, fs in os.walk(base):
        dirs[:] = [d for d in dirs if d not in ('node_modules', 'dist')]
        for f in fs:
            if not f.endswith(('.ts', '.tsx')):
                continue
            p = os.path.join(root, f)
            rel = os.path.relpath(p, repo)
            try:
                src = open(p, encoding='utf-8', errors='replace').read()
            except OSError:
                continue
            for m in FE_LITERAL.finditer(src):
                calls[m.group(1)].add(rel)
            var_defs = {m.group(1): m.group(2) for m in FE_VARDEF.finditer(src)}
            for m in FE_VARREF.finditer(src):
                name, suffix = m.group(1), m.group(2)
                if name in var_defs:
                    calls[var_defs[name] + '{VARSEP}' + suffix].add(rel)
    return calls


def normalize_literal(literal):
    """Normalize a frontend literal into segment form: each ${...}
    interpolation becomes one '{P}' param segment. Query strings are
    stripped but reported. Variable-composition separators ({VARSEP}) are
    dropped."""
    literal = literal.replace('{VARSEP}', '')
    body = literal.split('?')[0]
    has_query = '?' in literal
    body = re.sub(r'\$\{[^}]*\}', '{P}', body)
    return body, has_query


def route_segments(route):
    return [s for s in route.split('/') if s != '']


def literal_segments(body):
    return [s for s in body.split('/') if s != '']


def route_matches(route, literal):
    """Match a registered Go route pattern against a frontend literal's
    normalized segments. Returns the match kind or None.

    Rules (segment-exact; each ${...} = one param segment):
      exact            normalized literal equals the route
      query            same path, literal carries a query string
      template         all segments match ({param} and {P} match anything)
      template-prefix  literal ends with '/' — caller builds deeper paths
                       under this route (e.g. '/api/v1/workflows/${id}/')
      prefix           route itself is a prefix registration ('/preview/')

    A shallower literal ('/api/v1/projects/{P}/agents') no longer lights up
    every deeper route: segment counts must align or the literal must end
    with a trailing slash.
    """
    body, has_query = normalize_literal(literal)
    static = body

    if route == static:
        return 'query' if has_query else 'exact'
    if route.endswith('/') and static.startswith(route):
        return 'prefix'

    rs = route_segments(route)
    ls = literal_segments(static)
    if not ls:
        return None
    trailing_slash = static.endswith('/')
    if len(rs) < len(ls):
        return None
    dynamic_verb = False
    for i, l in enumerate(ls):
        r = rs[i]
        r_is_param = r.startswith('{') and r.endswith('}')
        l_is_param = l == '{P}'
        if r_is_param or l_is_param:
            # A literal param against a route VERB segment: the call site
            # dispatches dynamically (action = 'approve'|'cancel'|...).
            # Count as evidence but flag it — the exact endpoint set is the
            # sibling routes with that verb segment.
            if l_is_param and not r_is_param:
                dynamic_verb = True
            continue
        if r != l:
            return None
    if len(rs) == len(ls) and not trailing_slash:
        if dynamic_verb:
            return 'dynamic-verb'
        return 'query' if has_query else 'exact'
    if trailing_slash:
        return 'template-prefix'
    return None


def classify(rows):
    tiers = defaultdict(list)
    for r in rows:
        if r['ws_like']:
            tiers['websocket-surface'].append(r)
        elif r['internal']:
            tiers['platform-internal'].append(r)
        elif r['called']:
            tiers['frontend-called'].append(r)
        else:
            tiers['zero-call'].append(r)
    return tiers


def main():
    repo = '.'
    out_dir = '.'
    args = sys.argv[1:]
    if '--repo' in args:
        repo = args[args.index('--repo') + 1]
    if '--out-dir' in args:
        out_dir = args[args.index('--out-dir') + 1]

    eps = backend_endpoints(repo)
    fe = frontend_calls(repo)
    fe_literals = sorted(fe.keys())

    rows = []
    for e in eps:
        route = e['route']
        matched = []
        for lit in fe_literals:
            kind = route_matches(route, lit)
            if kind:
                matched.append({'literal': lit, 'match': kind, 'files': sorted(fe[lit])})
        is_internal = route in INTERNAL_EXACT or route.startswith(INTERNAL_PREFIXES)
        # WS/SSE/streaming heuristic: event/stream/live routes and the audit
        # event feed. agent_channel_ws is outbound (see module docstring).
        ws_like = (
            '/events' in route or route.endswith('/stream') or
            route.endswith('/live') or
            e['handler'] in ('handleAuditEvents', 'handleAgentLiveLog',
                             'handleGetTaskPreviewLive', 'handleIMEvent')
        )
        rows.append({
            **e,
            'frontend_literals': matched,
            'called': bool(matched),
            'internal': is_internal,
            'ws_like': bool(ws_like),
        })

    tiers = classify(rows)

    matched_lits = set()
    for r in rows:
        for m in r['frontend_literals']:
            matched_lits.add(m['literal'])
    unmatched = sorted(set(fe_literals) - matched_lits)

    report = {
        'repo': os.path.abspath(repo),
        'backend_registered': len(rows),
        'frontend_distinct_literals': len(fe_literals),
        'tiers': {k: len(v) for k, v in tiers.items()},
        'unmatched_frontend_literals': unmatched,
        'notes': [
            'agent_channel_ws (internal/api/agent_channel_ws.go) is an OUTBOUND '
            'websocket bridge: refreshAgentIMBridges dials IM providers (Lark '
            'larkws, Mattermost gorilla dialer). It registers no HTTP route and '
            'belongs to the WS plane evidence, not the endpoint table.',
            'The 130-endpoint figure cited in v3 §7.2 corresponds to the '
            'frontend-called tier under a stricter matcher; this worksheet '
            'reports every tier mechanically and defers all verdicts.',
        ],
        'endpoints': rows,
    }

    os.makedirs(out_dir, exist_ok=True)
    jpath = os.path.join(out_dir, 'endpoint-crosswalk.json')
    with open(jpath, 'w') as fh:
        json.dump(report, fh, indent=1, ensure_ascii=False)
        fh.write('\n')

    md = []
    md.append('# 端点交叉底稿（B2 机械部分）')
    md.append('')
    md.append('> **STOP 提示**：本底稿只做机械交叉，不做任何保留/延后/废弃裁决。')
    md.append('> 130→45 的裁剪是产品级决策（v3 §7.2），由用户逐项拍板。')
    md.append('')
    md.append(f"- 后端注册（internal/api/server.go，全部 mux）：**{len(rows)}**")
    md.append(f"- 前端字面量（web/src 全部 ts/tsx，含模板串静态前缀）：**{len(fe_literals)}**")
    for k in ('frontend-called', 'zero-call', 'platform-internal', 'websocket-surface'):
        md.append(f"- {k}：**{len(tiers.get(k, []))}**")
    md.append(f"- 前端字面量未命中注册：{len(unmatched)}（见文末，多为 UI 路由或带完整查询串的拼接）")
    md.append('')
    md.append('**WS 面说明**：`agent_channel_ws.go` 是出站 WebSocket 桥（refreshAgentIMBridges '
              '拨向 Lark/Mattermost），不注册 HTTP 路由；入站实时面为 SSE（agent chat / preview '
              'live / assistant chat，已按 handler 归入 websocket-surface 档）。v3 §7.2 的 '
              '"agent_channel_ws + events" 与此对齐。')
    md.append('')
    for tier in ('zero-call', 'frontend-called', 'platform-internal', 'websocket-surface'):
        items = tiers.get(tier, [])
        md.append(f'## {tier}（{len(items)}）')
        md.append('')
        md.append('| method | pattern | mux | handler | 命中字面量数 | 示例 |')
        md.append('|---|---|---|---|---|---|')
        for r in items:
            lits = r['frontend_literals']
            sample = ''
            if lits:
                first = lits[0]['literal']
                sample = first if len(lits) == 1 else f"{first} (+{len(lits)-1})"
                sample = sample.replace('|', '\\|')
            md.append(f"| {r['method']} | `{r['pattern']}` | {r['mux']} | {r['handler']} | {len(lits)} | {sample} |")
        md.append('')
    md.append('## 前端字面量未命中任何注册（证据完备性）')
    md.append('')
    md.append('这些是前端出现的 /api/v1 字符串，但按本脚本的保守匹配规则没有对应注册端点。')
    md.append('裁剪裁决前应人工过目（多为完整查询串拼接或跨段拼接）。')
    md.append('')
    for l in unmatched:
        files = ', '.join(sorted(fe[l])[:2])
        md.append(f'- `{l}` ← {files}')
    md.append('')

    mpath = os.path.join(out_dir, 'endpoint-crosswalk.md')
    with open(mpath, 'w') as fh:
        fh.write('\n'.join(md) + '\n')

    print(f"backend={len(rows)} frontend_literals={len(fe_literals)}")
    print(f"tiers: {dict(report['tiers'])}")
    print(f"unmatched literals: {len(unmatched)}")
    print(f"written: {jpath}")
    print(f"written: {mpath}")


if __name__ == '__main__':
    main()
