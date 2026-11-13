const ESCAPES: Record<string, string> = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };

export function esc(s: string): string {
  return s.replace(/[&<>"']/g, (c) => ESCAPES[c]!);
}

export function layout(title: string, body: string, opts: { headExtra?: string; bodyAttrs?: string } = {}): string {
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<meta name="robots" content="noindex, nofollow">
<title>${esc(title)} · Maestria</title>
<link rel="stylesheet" href="/static/app.css">
${opts.headExtra ?? ''}</head>
<body${opts.bodyAttrs ? ' ' + opts.bodyAttrs : ''}>
${body}
</body>
</html>
`;
}

export function loginPage(opts: { csrf: string; error?: string; retryAfter?: number }): string {
  const error = opts.error ? `<p class="error" role="alert">${esc(opts.error)}</p>` : '';
  return layout(
    'Sign in',
    `<main class="card narrow">
  <h1>Maestria</h1>
  <p class="muted">Sign in to the control panel.</p>
  ${error}
  <form method="post" action="/login" autocomplete="off">
    <input type="hidden" name="_csrf" value="${esc(opts.csrf)}">
    <label>Password
      <input type="password" name="password" autocomplete="current-password" required maxlength="256" autofocus>
    </label>
    <label>Authenticator code
      <input type="text" name="code" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9]{6}" maxlength="6" required>
    </label>
    <button type="submit">Sign in</button>
  </form>
</main>`,
  );
}

export function homePage(opts: { csrf: string }): string {
  return layout(
    'Panel',
    `<header class="bar">
  <div class="bar-left">
    <strong class="brand">Maestria</strong>
    <nav id="ws-switcher" class="ws-switcher" aria-label="Workspaces"></nav>
  </div>
  <div class="bar-actions">
    <span id="sync-status" class="muted small"></span>
    <form method="post" action="/logout">
      <input type="hidden" name="_csrf" value="${esc(opts.csrf)}">
      <button type="submit" class="ghost small-btn">Sign out</button>
    </form>
    <form method="post" action="/logout-all">
      <input type="hidden" name="_csrf" value="${esc(opts.csrf)}">
      <button type="submit" class="ghost small-btn">Sign out everywhere</button>
    </form>
  </div>
</header>
<main class="panel">
  <div id="banner" class="banner" role="alert" hidden></div>

  <div id="view-header" class="view-header">
    <div>
      <h1 id="view-title">Home</h1>
      <p id="view-meta" class="muted small"></p>
    </div>
    <button id="back-home" type="button" class="ghost small-btn" hidden>All workspaces</button>
  </div>

  <div id="filter-bar" class="filter-bar" hidden></div>

  <p id="no-workspaces" class="section muted" hidden>No workspaces configured yet. Add entries to <code>~/.config/maestria/workspaces.json</code> or run <code>maestria workspace add</code>.</p>

  <section id="needs-section" class="section attention clear">
    <div class="section-head">
      <div class="section-title">
        <h2>Needs you</h2>
        <span id="needs-badge" class="section-badge" hidden>0</span>
      </div>
      <button id="toggle-history" type="button" class="ghost small-btn" hidden></button>
    </div>
    <div id="needs-list" class="inbox-list"></div>
    <p id="needs-empty" class="muted" hidden>Nothing waiting on you.</p>
  </section>

  <section id="updates-section" class="section">
    <div class="section-head">
      <div class="section-title">
        <h2>Updates</h2>
        <span id="updates-badge" class="count-pill"></span>
      </div>
    </div>
    <div id="updates-list" class="inbox-list"></div>
    <p id="updates-empty" class="muted" hidden>No updates.</p>
  </section>

  <section id="history-section" class="section" hidden>
    <div class="section-head">
      <h2>History</h2>
    </div>
    <div id="history-list" class="history-list"></div>
    <p id="history-empty" class="muted small" hidden>No responses or dismissed updates yet.</p>
  </section>
</main>`,
    {
      headExtra: '<link rel="stylesheet" href="/workspaces.css">\n<script src="/static/app.js" defer></script>\n',
      bodyAttrs: `data-csrf="${esc(opts.csrf)}"`,
    },
  );
}
