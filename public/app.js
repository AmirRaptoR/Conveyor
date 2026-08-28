'use strict';

/* Agent Team — renders the dashboard from /api/state and keeps it live over SSE. */

const app = document.getElementById('app');
const totalsEl = document.getElementById('totals');
const navEl = document.getElementById('projnav');
const connEl = document.getElementById('conn');

let state = null;
const open = new Set();      // "project#number" of expanded reports
const details = new Map();   // lazily fetched full records for trimmed issues

// ---------------------------------------------------------------- helpers

const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

const STATE_TONE = {
  selecting: 'accent', refining: 'amber', implementing: 'violet', verifying: 'violet',
  reviewing: 'amber', merging: 'accent', deploying: 'accent', releasing: 'violet',
  reporting: 'accent', idle: '', blocked: 'amber', failed: 'red',
};
const STEPS = ['selecting', 'refining', 'implementing', 'verifying', 'reviewing', 'merging', 'deploying', 'reporting'];
const STEP_SHORT = { selecting: 'pick', refining: 'refine', implementing: 'build', verifying: 'test', reviewing: 'review', merging: 'merge', deploying: 'ship', reporting: 'report' };

function relTime(iso) {
  if (!iso) return '';
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return '';
  const s = Math.round((Date.now() - then) / 1000);
  if (s < 45) return 'just now';
  if (s < 5400) return `${Math.round(s / 60)}m ago`;
  if (s < 86400) return `${Math.round(s / 3600)}h ago`;
  if (s < 7 * 86400) return `${Math.round(s / 86400)}d ago`;
  return new Date(then).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}

function elapsed(iso) {
  const then = Date.parse(iso || '');
  if (Number.isNaN(then)) return '';
  const s = Math.max(0, Math.round((Date.now() - then) / 1000));
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
  return h ? `${h}h ${String(m).padStart(2, '0')}m` : `${m}m ${String(sec).padStart(2, '0')}s`;
}

const badge = (text, tone = '', mono = false) =>
  `<span class="badge ${tone} ${mono ? 'mono' : ''}">${esc(text)}</span>`;

const mediaUrl = (project, p) => `/files/${encodeURIComponent(project)}/${p.split('/').map(encodeURIComponent).join('/')}`;

// ---------------------------------------------------------------- header

function renderHeader(s) {
  const t = s.totals || {};
  totalsEl.innerHTML = [
    ['shipped', t.shipped], ['this week', t.thisWeek],
    ['releases', t.releases], ['unreleased', t.unreleased],
  ].map(([label, n]) => `<div class="total"><b>${n ?? 0}</b><span>${label}</span></div>`).join('');

  navEl.innerHTML = (s.projects || []).map((p) => {
    const busy = p.activity && p.activity.state !== 'idle';
    return `<a class="pill ${busy ? 'busy' : ''}" href="#p-${esc(p.dir)}">
      <span class="pill-dot"></span>${esc(p.name)} <b>${p.stats.shipped}</b></a>`;
  }).join('');
}

// ---------------------------------------------------------------- now panel

function renderNow(p) {
  const a = p.activity || { state: 'idle' };
  const idle = !a.state || a.state === 'idle';
  const tone = STATE_TONE[a.state] || '';
  const issue = a.issue;

  const stepIndex = STEPS.indexOf(a.state === 'releasing' ? 'deploying' : a.state);
  const bar = STEPS.map((step, i) => {
    let cls = '';
    if (a.state === 'failed') cls = i <= (stepIndex < 0 ? STEPS.length : stepIndex) ? 'fail' : '';
    else if (a.state === 'blocked') cls = i < stepIndex ? 'done' : i === stepIndex ? 'warn' : '';
    else if (stepIndex >= 0) cls = i < stepIndex ? 'done' : i === stepIndex ? 'at' : '';
    return `<div class="step ${cls}" title="${STEP_SHORT[step]}"></div>`;
  }).join('');

  if (idle) {
    const last = p.activityLog && p.activityLog[0];
    // An idle loop that is waiting for work says so itself; otherwise fall back
    // to the last thing that happened.
    const waiting = a.detail && /wait/i.test(a.detail);
    const note = a.detail
      ? esc(a.detail)
      : last ? `last: ${esc(last.state)}${last.detail ? ` — ${esc(last.detail)}` : ''}`
             : 'no cycle has run yet';
    const when = a.updatedAt || (last && last.at);
    return `<div class="now idle ${waiting ? 'waiting' : ''}">
      <div class="now-head">
        <span class="now-dot"></span>
        <span class="now-state">${waiting ? 'Waiting' : 'Idle'}</span>
        <span class="now-detail">${note}</span>
        ${when ? `<span class="now-elapsed">${esc(relTime(when))}</span>` : ''}
      </div>
    </div>`;
  }

  return `<div class="now">
    <div class="now-head">
      <span class="now-dot"></span>
      <span class="now-state">${badge(a.state, tone)}</span>
      ${issue ? `<span class="now-issue">
          <a href="${esc(issue.url || '#')}" target="_blank" rel="noreferrer">#${esc(issue.number)}</a>
          ${esc(issue.title || '')}
        </span>` : ''}
      ${a.reviewRound ? badge(`round ${a.reviewRound}`, 'amber') : ''}
      ${a.pr ? badge(`PR #${a.pr}`, '', true) : ''}
      <span class="now-elapsed" data-since="${esc(a.cycleStartedAt || a.updatedAt)}">${esc(elapsed(a.cycleStartedAt || a.updatedAt))}</span>
    </div>
    ${a.detail ? `<div class="now-detail">${esc(a.detail)}</div>` : ''}
    ${renderSpec(p)}
    <div class="steps">${bar}</div>
    <div class="step-labels"><span>${STEP_SHORT[STEPS[0]]}</span><span>${STEP_SHORT[STEPS[STEPS.length - 1]]}</span></div>
  </div>`;
}

/** The spec agreed during refinement, shown while the issue is still in flight. */
function renderSpec(p) {
  const r = p.draft && p.draft.refinement;
  const criteria = (r && r.acceptance_criteria) || [];
  if (!criteria.length) return '';
  const done = (p.draft.steps || []).some((s) => s.state === 'verifying');
  return `<div class="spec">
    <div class="spec-head">
      Agreed spec
      <span class="count">${criteria.length} criteria</span>
      ${r.estimated_size ? badge(`size ${r.estimated_size}`) : ''}
      ${r.refinedBy ? badge(`refined by ${[].concat(r.refinedBy).join(' + ')}`, 'amber') : ''}
    </div>
    <ul class="spec-list">${criteria.map((ac) => `
      <li><span class="tick ${done ? 'on' : ''}">${done ? '✓' : '○'}</span>
        <b>${esc(ac.id || '')}</b> ${esc(ac.criterion || '')}</li>`).join('')}</ul>
  </div>`;
}

// ---------------------------------------------------------------- stats & envs

function renderStats(p) {
  const s = p.stats;
  const cells = [
    { b: s.shipped, l: 'issues shipped' },
    { b: s.shippedThisWeek, l: 'this week' },
    { b: s.latestVersion || '—', l: 'latest release', muted: !s.latestVersion },
    { b: s.unreleased, l: 'awaiting release' },
    { b: s.avgCycleMinutes != null ? `${s.avgCycleMinutes}m` : '—', l: 'avg cycle', muted: s.avgCycleMinutes == null },
    { b: s.firstPassRate != null ? `${s.firstPassRate}%` : '—', l: 'approved first pass', muted: s.firstPassRate == null },
  ];
  return `<div class="stat-grid">${cells.map((c) =>
    `<div class="stat ${c.muted ? 'muted' : ''}"><b>${esc(c.b)}</b><span>${c.l}</span></div>`).join('')}</div>`;
}

function renderEnvs(p) {
  const names = new Set([...Object.keys(p.live || {}), ...Object.keys(p.environments || {})]);
  if (p.deployPolicy === 'production-on-merge') names.add('production');
  if (p.deployPolicy === 'test-on-merge') { names.add('test'); names.add('production'); }
  if (!names.size) return '';

  const cards = [...names].map((name) => {
    const live = (p.live || {})[name];
    const url = live?.url || (p.environments || {})[name];
    const tone = live ? 'green' : '';
    let what = '—', meta = 'nothing deployed yet';
    if (live) {
      what = live.release || (live.issue ? `#${live.issue}` : 'deployed');
      const how = live.via === 'release' ? 'via release' : 'on merge';
      meta = `${relTime(live.at)} · ${how}${live.issueCount ? ` · ${live.issueCount} issues` : ''}`;
    }
    return `<div class="env">
      <div class="env-top">
        <span class="env-name">${esc(name)}</span>
        ${badge(live ? 'live' : 'idle', tone)}
        ${url ? `<a href="${esc(url)}" target="_blank" rel="noreferrer" style="margin-left:auto;font-size:11.5px">open ↗</a>` : ''}
      </div>
      <div class="env-what">${esc(what)}</div>
      <div class="env-meta">${esc(meta)}</div>
    </div>`;
  }).join('');

  return `<div class="section">
    <div class="section-title">Environments</div>
    <div class="envs">${cards}</div>
  </div>`;
}

// ---------------------------------------------------------------- issue detail

function renderDetail(p, rec) {
  if (!rec) return `<div class="detail"><span class="row-when">loading…</span></div>`;
  const c = rec.change || {};
  const t = rec.tests || {};
  const r = rec.review || {};
  const pr = rec.pr || {};
  const d = rec.deploy || {};
  const media = rec.media || [];

  const shots = media.length ? `<div><h4>Evidence</h4><div class="shots">${media.map((m) => `
    <figure class="shot">
      <img src="${esc(mediaUrl(p.dir, m.path))}" alt="${esc(m.caption || m.kind)}" loading="lazy">
      <figcaption><span class="k">${esc(m.kind)}</span>${esc(m.caption || '')}</figcaption>
    </figure>`).join('')}</div></div>` : '';

  const howto = (rec.howToSee || []).length ? `<div><h4>How to see it</h4><ol class="howto">${
    rec.howToSee.map((s) => {
      const where = s.where || '';
      const link = /^https?:\/\//.test(where)
        ? ` — <a href="${esc(where)}" target="_blank" rel="noreferrer">${esc(where)}</a>`
        : where ? ` — <code>${esc(where)}</code>` : '';
      return `<li>${esc(s.step || '')}${link}</li>`;
    }).join('')}</ol></div>` : '';

  const testBits = [
    t.result ? `tests ${t.result}` : null,
    t.uiTested ? `UI covered${t.uiFramework ? ` (${t.uiFramework})` : ''}` : null,
  ].filter(Boolean);

  return `<div class="detail">
    <p class="lede">${esc(rec.summary || '')}</p>
    ${rec.why ? `<div><h4>Why it mattered</h4><p>${esc(rec.why)}</p></div>` : ''}
    <div class="ba">
      <div class="before"><div class="k">Before</div>${esc(c.before || '')}</div>
      <div class="after"><div class="k">After</div>${esc(c.after || '')}</div>
    </div>
    ${howto}
    ${shots}
    <div class="chips">
      ${c.userVisible ? badge('user visible', 'violet') : badge('internal')}
      ${c.breaking ? badge('breaking', 'red') : ''}
      ${testBits.map((b) => badge(b, 'green')).join('')}
      ${r.verdict ? badge(`${r.reviewer || 'review'}: ${r.verdict}${r.rounds ? ` · ${r.rounds} round${r.rounds > 1 ? 's' : ''}` : ''}`, r.verdict === 'approve' ? 'green' : 'amber') : ''}
      ${pr.url ? `<a class="badge mono" href="${esc(pr.url)}" target="_blank" rel="noreferrer">PR #${esc(pr.number)} ↗</a>` : ''}
      ${rec.release ? badge(rec.release, 'accent', true) : badge('unreleased')}
      ${d.status ? badge(`${d.environment || 'deploy'}: ${d.status}`, d.status === 'deployed' ? 'green' : d.status === 'failed' ? 'red' : 'amber') : ''}
    </div>
    ${(t.commands || []).length ? `<div><h4>Verified with</h4><pre class="cmd">${esc((t.commands || []).join('\n'))}</pre></div>` : ''}
    ${r.notes ? `<div><h4>Reviewer</h4><p>${esc(r.notes)}</p></div>` : ''}
    ${(c.areas || []).length ? `<div><h4>Touched</h4><div class="chips">${c.areas.map((a) => badge(a, '', true)).join('')}</div></div>` : ''}
  </div>`;
}

function renderIssues(p) {
  if (!p.issues.length) {
    return `<div class="section">
      <div class="section-title">Shipped work</div>
      <div class="empty"><b>Nothing shipped yet</b>Reports land here as the loop closes issues.</div>
    </div>`;
  }
  const rows = p.issues.map((rec) => {
    const n = rec.issue?.number;
    const key = `${p.dir}#${n}`;
    const isOpen = open.has(key);
    const full = rec.truncated ? details.get(key) : rec;
    return `<div class="issue" data-key="${esc(key)}">
      <div class="row ${isOpen ? 'open' : ''}" data-toggle="${esc(key)}">
        <span class="row-chev">▶</span>
        <span class="row-num">#${esc(n)}</span>
        <span class="row-title">${esc(rec.issue?.title || '')}</span>
        ${rec.change?.userVisible ? badge('UI', 'violet') : ''}
        ${rec.release ? badge(rec.release, 'accent', true) : badge('unreleased')}
        <span class="row-when">${esc(relTime(rec.issue?.closedAt))}</span>
      </div>
      ${isOpen ? renderDetail(p, full) : ''}
    </div>`;
  }).join('');

  return `<div class="section">
    <div class="section-title">Shipped work <span class="count">${p.issues.length}</span></div>
    <div class="rows">${rows}</div>
  </div>`;
}

function renderReleases(p) {
  if (!p.releases.length) return '';
  return `<div class="section">
    <div class="section-title">Releases <span class="count">${p.releases.length}</span></div>
    <div class="rels">${p.releases.slice(0, 8).map((r) => `
      <div class="rel">
        <div class="rel-v">${r.releaseUrl ? `<a href="${esc(r.releaseUrl)}" target="_blank" rel="noreferrer">${esc(r.version)}</a>` : esc(r.version)}</div>
        <div class="rel-body">
          <div class="rel-meta">${esc(relTime(r.date))} · ${esc(r.trigger || 'manual')} · ${r.issueCount || 0} issue${r.issueCount === 1 ? '' : 's'}${r.deploy?.status ? ` · ${esc(r.deploy.environment || '')} ${esc(r.deploy.status)}` : ''}</div>
          <ul class="rel-issues">${(r.issues || []).slice(0, 6).map((i) =>
            `<li><span class="n">#${esc(i.number)}</span>${esc(i.title || '')}</li>`).join('')}</ul>
        </div>
      </div>`).join('')}</div>
  </div>`;
}

function renderLog(p) {
  if (!p.activityLog || !p.activityLog.length) return '';
  return `<div class="section">
    <div class="section-title">Recent activity</div>
    <div class="log">${p.activityLog.slice(0, 12).map((e) => `
      <div class="log-row">
        <span class="log-when">${esc(relTime(e.at))}</span>
        <span class="log-state">${badge(e.state, STATE_TONE[e.state] || '')}</span>
        <span class="log-detail">${esc(e.detail || '')}${e.issue ? ` · #${esc(e.issue)}` : ''}</span>
      </div>`).join('')}</div>
  </div>`;
}

function renderProject(p) {
  const policyTone = { 'production-on-merge': 'green', 'test-on-merge': 'accent', ask: 'amber' }[p.deployPolicy] || '';
  return `<section class="project" id="p-${esc(p.dir)}">
    <div class="project-head">
      <span class="project-name">${esc(p.name)}</span>
      ${p.repoUrl ? `<a href="${esc(p.repoUrl)}" target="_blank" rel="noreferrer" class="badge mono">${esc(p.repo)} ↗</a>` : ''}
      <span class="spacer"></span>
      ${p.stack ? badge(p.stack) : ''}
      ${p.defaultBranch ? badge(p.defaultBranch, '', true) : ''}
      ${badge(p.deployPolicy, policyTone)}
    </div>
    ${renderNow(p)}
    ${renderStats(p)}
    ${renderEnvs(p)}
    ${renderIssues(p)}
    ${renderReleases(p)}
    ${renderLog(p)}
  </section>`;
}

// ---------------------------------------------------------------- render

function render(s) {
  state = s;
  renderHeader(s);

  if (!s.ok) {
    app.innerHTML = `<div class="empty"><b>No reports directory</b>${esc(s.error || '')}</div>`;
  } else if (!s.projects.length) {
    app.innerHTML = `<div class="empty"><b>No projects yet</b>Run <code>reports.py init</code> for a project to start.</div>`;
  } else {
    const y = window.scrollY;
    app.innerHTML = s.projects.map(renderProject).join('');
    window.scrollTo(0, y);
  }

  document.getElementById('foot-root').textContent = s.root || '';
  document.getElementById('foot-time').textContent = `updated ${new Date().toLocaleTimeString()}`;
  tick();
}

function tick() {
  document.querySelectorAll('[data-since]').forEach((el) => {
    el.textContent = elapsed(el.dataset.since);
  });
}
setInterval(tick, 1000);

// ---------------------------------------------------------------- events

document.addEventListener('click', async (e) => {
  const img = e.target.closest('.shot img');
  if (img) {
    const box = document.createElement('div');
    box.className = 'lightbox';
    box.innerHTML = `<img src="${img.src}" alt="">`;
    box.addEventListener('click', () => box.remove());
    document.body.appendChild(box);
    return;
  }

  const row = e.target.closest('[data-toggle]');
  if (!row) return;
  const key = row.dataset.toggle;
  if (open.has(key)) open.delete(key);
  else {
    open.add(key);
    // Older reports arrive trimmed; pull the full record on demand.
    const [dir, num] = key.split('#');
    const p = state.projects.find((x) => x.dir === dir);
    const rec = p?.issues.find((i) => String(i.issue?.number) === num);
    if (rec?.truncated && !details.has(key)) {
      try {
        const full = await (await fetch(`/api/issue/${encodeURIComponent(dir)}/${num}`)).json();
        details.set(key, full);
      } catch { /* leave it loading */ }
    }
  }
  render(state);
});

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') document.querySelector('.lightbox')?.remove();
});

// ---------------------------------------------------------------- live

function setConn(cls, label) {
  connEl.className = `conn ${cls}`;
  connEl.querySelector('.conn-label').textContent = label;
}

function connect() {
  const es = new EventSource('/events');
  es.addEventListener('state', (e) => {
    setConn('live', 'live');
    try { render(JSON.parse(e.data)); } catch { /* malformed frame, wait for the next */ }
  });
  es.onopen = () => setConn('live', 'live');
  es.onerror = () => {
    setConn('lost', 'reconnecting');
    // EventSource retries on its own; a manual refetch keeps the page warm meanwhile.
    fetch('/api/state').then((r) => r.json()).then(render).catch(() => {});
  };
}

fetch('/api/state').then((r) => r.json()).then(render).catch(() => {
  app.innerHTML = `<div class="empty"><b>Server unreachable</b>Is <code>node server.js</code> still running?</div>`;
});
connect();
