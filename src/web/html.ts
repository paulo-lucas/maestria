const ESCAPES: Record<string, string> = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };

export function esc(s: string): string {
  return s.replace(/[&<>"']/g, (c) => ESCAPES[c]!);
}

export function layout(title: string, body: string): string {
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<meta name="robots" content="noindex, nofollow">
<title>${esc(title)} · Maestria</title>
<link rel="stylesheet" href="/static/app.css">
</head>
<body>
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
  <strong>Maestria</strong>
  <form method="post" action="/logout">
    <input type="hidden" name="_csrf" value="${esc(opts.csrf)}">
    <button type="submit" class="ghost">Sign out</button>
  </form>
</header>
<main class="card">
  <h1>Welcome</h1>
  <p class="muted">You are signed in. Workspaces, tasks, notices, roadmap, usage and chat arrive in the next steps.</p>
  <form method="post" action="/logout-all">
    <input type="hidden" name="_csrf" value="${esc(opts.csrf)}">
    <button type="submit" class="ghost">Sign out everywhere</button>
  </form>
</main>`,
  );
}
