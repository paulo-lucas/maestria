# Maestria v2 design

Static, clickable mockups of the v2 panel. Open `index.html` in a browser (no server needed), or `preview.html` to see a screen at 360, 768 and 1280px at once.

## Files

| File | Role |
| --- | --- |
| `tokens.css` | Design tokens: type, space, radius, neutrals, tones. Production. |
| `base.css` | Reset, typography, focus, icons, utilities. Production. |
| `components.css` | Every component (buttons, tags, status, gauges, tasks, agents, floors, chat, forms...). Production. |
| `shell.css` | App shell (sidebar, top bar, bottom nav) and page layouts. Production. |
| `sheet.css` | Only for the review pages (`index`, `components`, `preview`). |
| `assets/icons.svg` | Icon sprite. Each page inlines a copy so `<use href="#i-...">` works from `file://`. |
| `assets/sidebar.js` | The resizable sidebar (desktop). Production; synced to the panel like the CSS. |
| `assets/favicon.svg`, `favicon-32.png`, `apple-touch-icon.png`, `icon-192.png`, `icon-512.png`, `manifest.webmanifest` | The favicon set and web manifest, from the logo mark (`docs/images/logo.svg`) in its literal colours. The PNGs are rasterised from the same geometry (the 32px one with a slightly heavier stroke, 2.1 like the in-app mark, so the bars stay legible). `theme_color` is the light `--bg` as hex. Production. |
| `assets/mock.js` | Mock interactions only (theme toggle, filters, tabs, menus, dialogs). Not production. |
| `src/*.html` | Page bodies with a JSON header (title, scope, active nav, index group). Edit these. |
| `screens/*.html` | Generated pages: body wrapped in the shell (sidebar, top bar, bottom nav) or the auth layout. Don't edit by hand. |
| `tools/build.mjs` | The generator. Also regenerates `index.html` and the screen list in `preview.html`. |

## Editing

Edit `src/<page>.html` (or the shell in `tools/build.mjs`), then run `node design/tools/build.mjs`. No dependencies.

## Conventions

- **Themes**: `color-scheme` + `light-dark()`. The OS decides; `data-theme="light|dark"` on `<html>` forces one; `.theme-light` / `.theme-dark` force a subtree.
- **Tones**: `data-ws="<color>"` (workspace palette: blue, sky, teal, green, amber, orange, rose, pink, violet, slate) and `data-status="<state>"` (working, idle, needs, blocked, review, done, offline, ok, warn, danger, info) set `--tone`, `--tone-ink`, `--tone-tint`, `--tone-line`, which components read.
- **Strict CSP**: no inline `style` or scripts. Values that would need inline styles use native elements (`<progress>`) or classes (`.mark-90` for the standby threshold marker).
- **Sidebar width**: `--sidebar-w` (default `--sidebar-w-min`, 248px; max `--sidebar-w-max`, 2x). `sidebar.js` overrides it on `<html>` through the CSSOM and remembers it in `localStorage`. Long names use `.nav__label`, `.truncate`, `.account-row__id`; the sidebar never scrolls sideways.
- **Responsive**: mobile first. Sidebar from 1024px; below it, top bar with scope switcher + bottom nav. Page content uses container queries, so it adapts to the space left by the sidebar.
