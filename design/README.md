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
| `assets/mock.js` | Mock interactions only (theme toggle, filters, tabs, menus). Not production. |

## Conventions

- **Themes**: `color-scheme` + `light-dark()`. The OS decides; `data-theme="light|dark"` on `<html>` forces one; `.theme-light` / `.theme-dark` force a subtree.
- **Tones**: `data-ws="<color>"` (workspace palette: blue, sky, teal, green, amber, orange, rose, pink, violet, slate) and `data-status="<state>"` (working, idle, needs, blocked, review, done, offline, ok, warn, danger, info) set `--tone`, `--tone-ink`, `--tone-tint`, `--tone-line`, which components read.
- **Strict CSP**: no inline `style` or scripts. Values that would need inline styles use native elements (`<progress>`) or classes (`.mark-90` for the standby threshold marker).
- **Responsive**: mobile first. Sidebar from 1024px; below it, top bar with scope switcher + bottom nav. Page content uses container queries, so it adapts to the space left by the sidebar.
