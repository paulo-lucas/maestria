#!/usr/bin/env node
/*
 * Builds the design mockups: design/src/<name>.html -> design/screens/<name>.html.
 *
 * Each source file starts with a JSON header comment:
 *   <!--page {"title": "Activity", "layout": "app", "scope": "atlas", "nav": "activity",
 *             "group": "Workspace", "order": 20, "label": "2. Workspace / Activity", "desc": "..."}-->
 * The rest of the file is the page body. The script wraps it in the shell
 * (sidebar, top bar, bottom nav) or the auth layout, inlines the icon sprite,
 * and regenerates index.html and the screen list in preview.html.
 *
 * Usage: node design/tools/build.mjs   (no dependencies)
 */
import { readFileSync, writeFileSync, readdirSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const sprite = readFileSync(join(root, 'assets/icons.svg'), 'utf8').trim();
const ICONS = `<!--icons:start (copy of assets/icons.svg)-->\n${sprite}\n<!--icons:end-->`;

const icon = (id, cls = '') => `<svg class="icon${cls ? ' ' + cls : ''}" aria-hidden="true"><use href="#${id}"/></svg>`;
const BRAND_MARK = '<svg class="brand__mark" viewBox="0 0 24 24" aria-hidden="true"><rect class="brand__bg" width="24" height="24" rx="6"/><path class="brand__bars" d="M7 14.5v2.5M10.3 11v6M13.7 7.5V17M17 10v7"/></svg>';

const WORKSPACES = [
  { id: 'atlas', name: 'Atlas', color: 'blue', needs: 2 },
  { id: 'nimbus', name: 'Nimbus', color: 'violet', needs: 1 },
  { id: 'harbor', name: 'Harbor', color: 'teal', needs: 1 },
  { id: 'juniper', name: 'Juniper', color: 'amber', needs: 1 },
  { id: 'quarry', name: 'Quarry', color: 'rose', needs: 0, offline: true },
];
const TOTAL_NEEDS = WORKSPACES.reduce((n, w) => n + w.needs, 0);

const WS_NAV = [
  { id: 'activity', label: 'Activity', short: 'Activity', icon: 'i-activity', href: 'workspace-activity.html' },
  { id: 'tasks', label: 'Human tasks', short: 'Tasks', icon: 'i-inbox', href: 'workspace-tasks.html', count: true },
  { id: 'chat', label: 'Chat', short: 'Chat', icon: 'i-chat', href: 'workspace-chat.html' },
  { id: 'floors', label: 'Floors', short: 'Floors', icon: 'i-floors', href: 'workspace-floors.html' },
  { id: 'docs', label: 'Docs', short: 'Docs', icon: 'i-docs', href: 'workspace-docs.html' },
];
const GLOBAL_NAV = [
  { id: 'overview', label: 'Overview', short: 'Overview', icon: 'i-overview', href: 'overview.html', count: true },
  { id: 'usage', label: 'Usage &amp; standby', short: 'Usage', icon: 'i-gauge', href: 'usage.html' },
  { id: 'connectors', label: 'Connectors', short: 'Connectors', icon: 'i-plug', href: 'connector.html' },
  { id: 'security', label: 'Devices &amp; security', short: 'Security', icon: 'i-shield', href: 'security.html' },
];

const current = (on) => (on ? ' aria-current="page"' : '');

function themeToggle(extra = '') {
  return `<button class="btn btn--ghost btn--icon theme-toggle${extra}" type="button" data-theme-toggle aria-label="Theme: system" title="Theme: system">
          ${icon('i-auto', 'i-auto')}
          ${icon('i-sun', 'i-light')}
          ${icon('i-moon', 'i-dark')}
        </button>`;
}

function sidebar(m) {
  const ws = m.noWorkspaces ? [] : WORKSPACES;
  const needs = m.noWorkspaces ? 0 : TOTAL_NEEDS;
  const g = (id) => GLOBAL_NAV.find((n) => n.id === id);
  const item = (n) => `<a class="nav__item" href="${n.href}"${current(m.nav === n.id)}>${icon(n.icon)} ${n.label}${n.count && needs ? ` <span class="count" data-status="needs" title="Needs you">${needs}</span>` : ''}</a>`;
  const wsItems = ws.map((w) => {
    const trail = w.offline ? '<span class="nav__trail">Offline</span>' : w.needs ? `<span class="count">${w.needs}</span>` : '';
    let html = `<a class="nav__item${w.offline ? ' is-offline' : ''}" href="workspace-activity.html" data-ws="${w.color}"><span class="swatch"></span> ${w.name} ${trail}</a>`;
    if (m.scope === w.id) {
      html += `\n      <div class="nav__sub">\n${WS_NAV.map((n) => `        <a class="nav__item" href="${n.href}"${current(m.nav === n.id)}>${icon(n.icon)} ${n.label}${n.count && w.needs ? ` <span class="count" data-status="needs">${w.needs}</span>` : ''}</a>`).join('\n')}\n      </div>`;
    }
    return '      ' + html;
  }).join('\n');
  const offline = m.conn === 'offline';
  return `<aside class="sidebar" aria-label="Main">
    <a class="brand" href="overview.html">${BRAND_MARK} Maestria</a>
    <nav class="nav">
      ${item(g('overview'))}
      <div class="nav__group">Workspaces</div>
${wsItems || '      <p class="nav__empty">No workspaces yet</p>'}
      <div class="nav__group">System</div>
      ${item(g('usage'))}
      ${item(g('connectors'))}
      ${item(g('security'))}
    </nav>
    <div class="sidebar__foot">
      <a class="conn-card" href="connector.html">
        <span class="dot" data-status="${offline ? 'offline' : 'ok'}"></span>
        <span class="conn-card__name">Workstation</span>
        <span class="conn-card__ver">v2.0.3</span>
        <span class="conn-card__meta">${offline ? 'Offline · last seen 14:02' : 'Connected · Wire OK'}</span>
      </a>
      ${m.noWorkspaces ? '' : `<a class="conn-card" href="connector.html">
        <span class="dot" data-status="offline"></span>
        <span class="conn-card__name">Laptop</span>
        <span class="conn-card__ver">v2.0.1</span>
        <span class="conn-card__meta">Last seen 2h ago</span>
      </a>`}
      <div class="account-row">
        <span class="avatar avatar--sm" data-tone="slate">Y</span>
        <span class="account-row__name truncate">Your account</span>
        ${themeToggle(' btn--sm')}
      </div>
    </div>
  </aside>`;
}

function topbar(m) {
  const ws = WORKSPACES.find((w) => w.id === m.scope);
  const ws_ = m.noWorkspaces ? [] : WORKSPACES;
  const summary = ws
    ? `<span class="swatch swatch--lg" data-ws="${ws.color}"></span><span class="truncate">${ws.name}</span>`
    : `${icon('i-overview', 'scope__all-icon')}<span class="truncate">All workspaces</span>`;
  const items = ws_.map((w) => `        <a class="menu__item" href="workspace-activity.html" data-ws="${w.color}"${current(m.scope === w.id)}><span class="swatch swatch--lg"></span> ${w.name} ${w.offline ? '<span class="status" data-status="offline">Offline</span>' : w.needs ? `<span class="count">${w.needs}</span>` : ''}</a>`).join('\n');
  const offline = m.conn === 'offline';
  const connLabel = offline ? 'Connectors: all offline' : 'Connectors: 1 online, 1 offline';
  return `<header class="topbar">
    <a class="brand" href="overview.html" aria-label="Maestria overview">${BRAND_MARK}</a>
    <details class="menu scope">
      <summary aria-label="Switch workspace">
        ${summary}
        ${icon('i-chevron-down')}
      </summary>
      <div class="menu__panel">
        <a class="menu__item" href="overview.html"${current(m.scope === 'all')}>${icon('i-overview')} All workspaces${TOTAL_NEEDS && !m.noWorkspaces ? ` <span class="count" data-status="needs">${TOTAL_NEEDS}</span>` : ''}</a>
        ${items ? '<div class="menu__sep"></div>\n' + items : ''}
      </div>
    </details>
    <div class="topbar__end">
      <a class="conn-dot" href="connector.html" aria-label="${connLabel}" title="${connLabel}">
        ${icon('i-plug')}${offline ? '<span class="dot" data-status="danger"></span>' : ''}
      </a>
      ${themeToggle()}
    </div>
  </header>`;
}

function bottomnav(m) {
  const ws = WORKSPACES.find((w) => w.id === m.scope);
  const items = ws ? WS_NAV : GLOBAL_NAV;
  const count = ws ? ws.needs : m.noWorkspaces ? 0 : TOTAL_NEEDS;
  return `<nav class="bottomnav" aria-label="${ws ? ws.name + ' sections' : 'Sections'}">
${items.map((n) => `    <a class="bottomnav__item" href="${n.href}"${current(m.nav === n.id)}>${icon(n.icon)}${n.short}${n.count && count ? `<span class="count" data-status="needs">${count}</span>` : ''}</a>`).join('\n')}
  </nav>`;
}

function head(m, prefix) {
  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
  <meta name="color-scheme" content="light dark">
  <title>${m.title} · Maestria</title>
  <link rel="stylesheet" href="${prefix}tokens.css">
  <link rel="stylesheet" href="${prefix}base.css">
  <link rel="stylesheet" href="${prefix}components.css">
  <link rel="stylesheet" href="${prefix}shell.css">
  <script src="${prefix}assets/mock.js"></script>
</head>`;
}

function renderApp(m, body) {
  return `${head(m, '../')}
<body>
${ICONS}
<a class="skip-link" href="#main">Skip to content</a>

<div class="app">
  ${sidebar(m)}

  ${topbar(m)}

  <main class="main" id="main">
${body.trimEnd()}
  </main>

  ${bottomnav(m)}
</div>
</body>
</html>
`;
}

function renderAuth(m, body) {
  return `${head(m, '../')}
<body>
${ICONS}
<main class="auth" id="main">
  <a class="brand auth__brand" href="auth-login.html">${BRAND_MARK} Maestria</a>
${body.trimEnd()}
  <footer class="auth__foot">
    <span>Open source, MIT</span>
    ${themeToggle(' btn--sm')}
  </footer>
</main>
</body>
</html>
`;
}

// ---- Build pages ----
const srcDir = join(root, 'src');
const pages = [];
for (const file of readdirSync(srcDir).filter((f) => f.endsWith('.html')).sort()) {
  const raw = readFileSync(join(srcDir, file), 'utf8');
  const match = raw.match(/^<!--page\s+([\s\S]*?)-->\n?/);
  if (!match) throw new Error(`${file}: missing <!--page {...}--> header`);
  const meta = { layout: 'app', scope: 'all', ...JSON.parse(match[1]) };
  const body = raw.slice(match[0].length);
  const html = meta.layout === 'auth' ? renderAuth(meta, body) : renderApp(meta, body);
  writeFileSync(join(root, 'screens', file), html);
  pages.push({ file, ...meta });
}
pages.sort((a, b) => a.order - b.order);

// ---- Inline icons into hand-written pages ----
for (const file of ['components.html']) {
  const p = join(root, file);
  const s = readFileSync(p, 'utf8');
  writeFileSync(p, s.replace(/<!--icons:start[\s\S]*?<!--icons:end-->/, () => ICONS));
}

// ---- Index ----
const groups = [];
for (const p of pages) {
  let g = groups.find((x) => x.name === p.group);
  if (!g) groups.push((g = { name: p.group, pages: [] }));
  g.pages.push(p);
}
const index = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
  <meta name="color-scheme" content="light dark">
  <title>Maestria v2 design</title>
  <link rel="stylesheet" href="tokens.css">
  <link rel="stylesheet" href="base.css">
  <link rel="stylesheet" href="components.css">
  <link rel="stylesheet" href="shell.css">
  <link rel="stylesheet" href="sheet.css">
  <script src="assets/mock.js"></script>
</head>
<body>
${ICONS}
<div class="sheet">
  <header class="sheet-head">
    <a class="brand" href="index.html">${BRAND_MARK} Maestria v2 design</a>
    <div class="btn-row">
      <a class="btn" href="components.html">${icon('i-sparkle')}Components</a>
      <a class="btn" href="preview.html">Preview at 360 / 768 / 1280</a>
      ${themeToggle()}
    </div>
  </header>
${groups.map((g) => `  <section class="spec">
    <h2 class="spec__title">${g.name}</h2>
    <div class="index-grid">
${g.pages.map((p) => `      <a class="card index-card" href="screens/${p.file}"><span class="index-card__title">${icon(p.icon || 'i-file')}${p.label}</span><p>${p.desc}</p></a>`).join('\n')}
    </div>
  </section>`).join('\n')}
</div>
</body>
</html>
`;
writeFileSync(join(root, 'index.html'), index);

// ---- Preview screen list ----
const previewPath = join(root, 'preview.html');
const preview = readFileSync(previewPath, 'utf8').replace(
  /(<select class="select" id="preview-page">)[\s\S]*?(<\/select>)/,
  (_, a, b) => `${a}\n${pages.map((p) => `        <option value="screens/${p.file}">${p.label}</option>`).join('\n')}\n      ${b}`,
);
writeFileSync(previewPath, preview);

console.log(`built ${pages.length} screens`);
