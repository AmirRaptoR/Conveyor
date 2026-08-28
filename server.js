#!/usr/bin/env node
'use strict';

/**
 * Agent Team — a local dashboard over ~/codes/reports.
 *
 * Reads only; the ship-issues loop owns every write. Zero dependencies, binds to
 * loopback, no auth: this is a single-user view of your own machine.
 *
 *   node server.js [--port N] [--root DIR] [--host ADDR] [--open]
 */

const http = require('http');
const fs = require('fs');
const fsp = require('fs/promises');
const path = require('path');
const os = require('os');
const { execFile } = require('child_process');

const args = process.argv.slice(2);
const argValue = (name, fallback) => {
  const i = args.indexOf(name);
  return i >= 0 && args[i + 1] ? args[i + 1] : fallback;
};

const ROOT = path.resolve(
  argValue('--root', process.env.SHIP_REPORTS_ROOT || path.join(os.homedir(), 'codes', 'reports'))
);
const PUBLIC_DIR = path.join(__dirname, 'public');
// Loopback by default; containers set 0.0.0.0 and publish to loopback on the host.
const HOST = argValue('--host', process.env.SHIP_REPORTS_HOST || '127.0.0.1');
// Tried in order; 0 lets the OS pick any free port, so startup never fails.
const PORT_CANDIDATES = [Number(argValue('--port', 0)) || 7788, 7799, 8123, 8899, 0];
const ISSUE_DETAIL_LIMIT = 60; // newest N issues ship with full detail; older ones as summaries

// ---------------------------------------------------------------- reading

const readJson = async (file, fallback = null) => {
  try {
    return JSON.parse(await fsp.readFile(file, 'utf8'));
  } catch {
    return fallback;
  }
};

const readTail = async (file, lines) => {
  try {
    const text = await fsp.readFile(file, 'utf8');
    return text.split('\n').filter(Boolean).slice(-lines).map((l) => {
      try { return JSON.parse(l); } catch { return null; }
    }).filter(Boolean);
  } catch {
    return [];
  }
};

const byDateDesc = (key) => (a, b) => String(b[key] || '').localeCompare(String(a[key] || ''));

/** Latest deployment per environment, derived from what actually shipped. */
function deriveLive(issues, releases) {
  const live = {};
  const consider = (env, record) => {
    if (!env || !record.at) return;
    if (!live[env] || String(record.at) > String(live[env].at)) live[env] = { ...record, environment: env };
  };

  for (const issue of issues) {
    const d = issue.deploy || {};
    if (d.status !== 'deployed') continue;
    consider(d.environment, {
      at: d.at || issue.issue?.closedAt,
      url: d.url,
      via: 'merge',
      issue: issue.issue?.number,
      title: issue.issue?.title,
      release: issue.release || null,
    });
  }
  for (const rel of releases) {
    const d = rel.deploy || {};
    if (d.status !== 'deployed') continue;
    consider(d.environment, {
      at: d.at || rel.date,
      url: d.url,
      via: 'release',
      release: rel.version,
      issueCount: rel.issueCount,
    });
  }
  return live;
}

function deriveStats(issues, releases, state) {
  const now = Date.now();
  const closedAt = (i) => Date.parse(i.issue?.closedAt || '') || 0;
  const week = issues.filter((i) => now - closedAt(i) < 7 * 864e5).length;
  const cycles = issues.map((i) => i.metrics?.cycleMinutes).filter((n) => typeof n === 'number');
  const rounds = issues.map((i) => i.review?.rounds).filter((n) => typeof n === 'number');
  const firstPass = rounds.filter((n) => n === 1).length;
  const avg = (xs) => (xs.length ? Math.round(xs.reduce((a, b) => a + b, 0) / xs.length) : null);

  return {
    shipped: issues.length,
    shippedThisWeek: week,
    releases: releases.length,
    unreleased: state?.issuesSinceLastRelease || 0,
    unreleasedNumbers: state?.issuesSinceLastReleaseNumbers || [],
    latestVersion: releases[0]?.version || null,
    latestReleaseAt: releases[0]?.date || null,
    avgCycleMinutes: avg(cycles),
    avgReviewRounds: rounds.length ? Number((rounds.reduce((a, b) => a + b, 0) / rounds.length).toFixed(1)) : null,
    firstPassRate: rounds.length ? Math.round((firstPass / rounds.length) * 100) : null,
    userVisible: issues.filter((i) => i.change?.userVisible).length,
    withMedia: issues.filter((i) => (i.media || []).length > 0).length,
    reviewers: issues.reduce((acc, i) => {
      const r = i.review?.reviewer;
      if (r) acc[r] = (acc[r] || 0) + 1;
      return acc;
    }, {}),
  };
}

async function readProject(dir) {
  const projectPath = path.join(ROOT, dir);
  const meta = await readJson(path.join(projectPath, 'project.json'));
  if (!meta) return null;

  const [state, releasesFile, activity, log] = await Promise.all([
    readJson(path.join(projectPath, 'state.json'), {}),
    readJson(path.join(projectPath, 'releases.json'), { releases: [] }),
    readJson(path.join(projectPath, 'activity.json'), null),
    readTail(path.join(projectPath, 'activity-log.jsonl'), 40),
  ]);

  // The report being accumulated for the issue in flight, so the dashboard can
  // show what has been agreed and done so far, not just the state name.
  let draft = null;
  const activeIssue = activity && activity.issue && activity.issue.number;
  if (activeIssue) draft = await readJson(path.join(projectPath, 'drafts', `${activeIssue}.json`), null);

  let issues = [];
  try {
    const files = (await fsp.readdir(path.join(projectPath, 'issues'))).filter((f) => f.endsWith('.json'));
    issues = (await Promise.all(files.map((f) => readJson(path.join(projectPath, 'issues', f))))).filter(Boolean);
  } catch { /* no issues yet */ }

  issues.sort((a, b) => String(b.issue?.closedAt || '').localeCompare(String(a.issue?.closedAt || '')));
  const releases = (releasesFile.releases || []).slice().sort(byDateDesc('date'));

  // Trim the tail so a long-lived project does not send megabytes on every update.
  const detailed = issues.slice(0, ISSUE_DETAIL_LIMIT);
  const summarised = issues.slice(ISSUE_DETAIL_LIMIT).map((i) => ({
    truncated: true,
    issue: i.issue,
    change: { userVisible: i.change?.userVisible },
    release: i.release,
    summary: i.summary,
  }));

  return {
    name: meta.name || dir,
    dir,
    repo: meta.repo,
    repoUrl: meta.repoUrl,
    defaultBranch: meta.defaultBranch,
    stack: meta.stack,
    deployPolicy: meta.deployPolicy || 'ask',
    environments: meta.environments || {},
    activity: activity || { state: 'idle' },
    activityLog: log.reverse(),
    draft,
    issues: [...detailed, ...summarised],
    releases,
    live: deriveLive(issues, releases),
    stats: deriveStats(issues, releases, state),
  };
}

async function readState() {
  let dirs = [];
  try {
    dirs = (await fsp.readdir(ROOT, { withFileTypes: true }))
      .filter((d) => d.isDirectory() && !d.name.startsWith('.'))
      .map((d) => d.name);
  } catch {
    return { ok: false, error: `reports root not found: ${ROOT}`, root: ROOT, projects: [] };
  }

  const projects = (await Promise.all(dirs.map(readProject))).filter(Boolean);
  projects.sort((a, b) => {
    const busy = (p) => (p.activity?.state && p.activity.state !== 'idle' ? 0 : 1);
    return busy(a) - busy(b) || a.name.localeCompare(b.name);
  });

  const totals = projects.reduce((acc, p) => ({
    shipped: acc.shipped + p.stats.shipped,
    releases: acc.releases + p.stats.releases,
    unreleased: acc.unreleased + p.stats.unreleased,
    thisWeek: acc.thisWeek + p.stats.shippedThisWeek,
    active: acc.active + (p.activity?.state && p.activity.state !== 'idle' ? 1 : 0),
  }), { shipped: 0, releases: 0, unreleased: 0, thisWeek: 0, active: 0 });

  return { ok: true, root: ROOT, generatedAt: new Date().toISOString(), projects, totals };
}

// ---------------------------------------------------------------- watching

const clients = new Set();
let cache = null;
let pending = null;

async function refresh(reason) {
  cache = await readState();
  const payload = `event: state\ndata: ${JSON.stringify({ ...cache, reason })}\n\n`;
  for (const res of clients) res.write(payload);
}

function scheduleRefresh(reason) {
  clearTimeout(pending);
  pending = setTimeout(() => refresh(reason).catch(() => {}), 200);
}

function startWatching() {
  try {
    fs.watch(ROOT, { recursive: true }, (_event, file) => {
      if (file && (file.endsWith('.tmp') || file.includes('/.'))) return; // atomic-write temp files
      scheduleRefresh(file || 'change');
    });
    return 'fs.watch (recursive)';
  } catch {
    // Recursive watch is not available everywhere; a cheap signature poll is enough.
    let last = '';
    setInterval(async () => {
      const sig = await signature();
      if (sig !== last) { last = sig; scheduleRefresh('poll'); }
    }, 3000);
    return 'polling every 3s';
  }
}

async function signature() {
  const parts = [];
  const walk = async (dir, depth = 0) => {
    if (depth > 3) return;
    let entries = [];
    try { entries = await fsp.readdir(dir, { withFileTypes: true }); } catch { return; }
    for (const e of entries) {
      const full = path.join(dir, e.name);
      if (e.isDirectory()) await walk(full, depth + 1);
      else {
        try { const s = await fsp.stat(full); parts.push(`${full}:${s.mtimeMs}`); } catch { /* gone */ }
      }
    }
  };
  await walk(ROOT);
  return parts.join('|');
}

// ---------------------------------------------------------------- serving

const MIME = {
  '.html': 'text/html; charset=utf-8', '.css': 'text/css; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8', '.json': 'application/json; charset=utf-8',
  '.png': 'image/png', '.jpg': 'image/jpeg', '.jpeg': 'image/jpeg', '.gif': 'image/gif',
  '.svg': 'image/svg+xml', '.webp': 'image/webp', '.avif': 'image/avif', '.md': 'text/markdown; charset=utf-8',
};

const send = (res, code, body, type = 'application/json; charset=utf-8', extra = {}) => {
  res.writeHead(code, { 'content-type': type, 'cache-control': 'no-store', ...extra });
  res.end(body);
};

/** Resolve a request path inside a base directory, refusing to escape it. */
function safeJoin(base, target) {
  const resolved = path.resolve(base, '.' + path.posix.normalize('/' + target));
  return resolved === base || resolved.startsWith(base + path.sep) ? resolved : null;
}

async function serveFile(res, file) {
  try {
    const data = await fsp.readFile(file);
    send(res, 200, data, MIME[path.extname(file).toLowerCase()] || 'application/octet-stream');
  } catch {
    send(res, 404, JSON.stringify({ error: 'not found' }));
  }
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, `http://${req.headers.host}`);
  const pathname = decodeURIComponent(url.pathname);

  if (pathname === '/' || pathname === '/index.html') {
    return serveFile(res, path.join(PUBLIC_DIR, 'index.html'));
  }

  if (pathname === '/api/state') {
    if (!cache) cache = await readState();
    return send(res, 200, JSON.stringify(cache));
  }

  if (pathname === '/events') {
    res.writeHead(200, {
      'content-type': 'text/event-stream',
      'cache-control': 'no-store',
      connection: 'keep-alive',
    });
    res.write('retry: 2000\n\n');
    if (!cache) cache = await readState();
    res.write(`event: state\ndata: ${JSON.stringify({ ...cache, reason: 'connect' })}\n\n`);
    clients.add(res);
    const beat = setInterval(() => res.write(': ping\n\n'), 25000);
    req.on('close', () => { clearInterval(beat); clients.delete(res); });
    return undefined;
  }

  // /api/issue/<project>/<number> — full record for an issue trimmed out of /api/state
  const issueMatch = pathname.match(/^\/api\/issue\/([^/]+)\/(\d+)$/);
  if (issueMatch) {
    const file = safeJoin(ROOT, `${issueMatch[1]}/issues/${issueMatch[2]}.json`);
    if (!file) return send(res, 400, JSON.stringify({ error: 'bad path' }));
    return serveFile(res, file);
  }

  // /files/<project>/media/... — evidence referenced by a report
  if (pathname.startsWith('/files/')) {
    const file = safeJoin(ROOT, pathname.slice('/files/'.length));
    if (!file) return send(res, 400, JSON.stringify({ error: 'bad path' }));
    return serveFile(res, file);
  }

  if (pathname.startsWith('/static/')) {
    const file = safeJoin(PUBLIC_DIR, pathname.slice('/static/'.length));
    if (!file) return send(res, 400, JSON.stringify({ error: 'bad path' }));
    return serveFile(res, file);
  }

  return send(res, 404, JSON.stringify({ error: 'not found' }));
});

// ---------------------------------------------------------------- startup

function listen(candidates) {
  const [port, ...rest] = candidates;
  server.once('error', (err) => {
    if (err.code === 'EADDRINUSE' && rest.length) {
      console.log(`  port ${port} busy, trying ${rest[0] || 'a free port'}…`);
      return listen(rest);
    }
    console.error(err.message);
    process.exit(1);
  });
  server.listen(port, HOST, async () => {
    const actual = server.address().port;
    cache = await readState();
    const how = startWatching();
    const projects = cache.projects.map((p) => p.name).join(', ') || 'none yet';
    console.log('');
    console.log('  Agent Team');
    console.log(`  → http://${HOST}:${actual}`);
    console.log('');
    console.log(`  reports   ${ROOT}`);
    console.log(`  projects  ${projects}`);
    console.log(`  live      ${how}`);
    console.log('');
    if (args.includes('--open')) execFile('xdg-open', [`http://${HOST}:${actual}`], () => {});
  });
}

listen(PORT_CANDIDATES);
