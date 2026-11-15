/*
 * Resizable sidebar (desktop). Vanilla JS, no inline anything (strict CSP): the width is a CSS
 * custom property set on <html> through the CSSOM, which a style-src 'self' policy allows.
 *
 * Loaded in <head> without defer so a saved width applies before first paint. The handle is
 * <div class="sidebar-resizer" data-sidebar-resizer hidden>; this script fills in its ARIA state
 * and shows it. Below 1024px the handle is not displayed and the sidebar is not shown at all.
 *
 *   drag, or arrow keys (Shift = bigger steps), Home / End = min / max, double-click or Enter = reset
 *   min and max come from --sidebar-w-min and --sidebar-w-max in tokens.css
 *   the width is remembered per browser in localStorage (every access is in try/catch)
 */
(function () {
  'use strict';
  var KEY = 'maestria-sidebar-w';
  var VAR = '--sidebar-w';
  var STEP = 16, BIG_STEP = 64;
  var root = document.documentElement;

  function token(name, fallback) {
    var n = parseFloat(getComputedStyle(root).getPropertyValue(name));
    return isFinite(n) && n > 0 ? n : fallback;
  }
  function minW() { return token('--sidebar-w-min', 248); }
  function maxW() { return token('--sidebar-w-max', 496); }
  // The sidebar only exists from 1024px, where the maximum still leaves the page 528px.
  function ceiling() { return maxW(); }
  // Anything that is not a finite number (garbage in storage) means "default".
  function clamp(v) {
    v = Number(v);
    if (!isFinite(v)) return minW();
    return Math.round(Math.max(minW(), Math.min(ceiling(), v)));
  }

  function load() {
    try {
      var raw = localStorage.getItem(KEY);
      if (raw === null) return null;
      var n = Number(raw);
      return isFinite(n) ? n : null;
    } catch (e) { return null; }
  }
  function save(v) {
    try { if (v === null) localStorage.removeItem(KEY); else localStorage.setItem(KEY, String(v)); } catch (e) { /* private mode, blocked storage */ }
  }

  var pref = load();      // the width the person chose (null = default); applied clamped
  var handle = null;

  function apply() {
    var w = pref === null ? minW() : clamp(pref);
    if (pref === null || w === minW()) root.style.removeProperty(VAR);
    else root.style.setProperty(VAR, w + 'px');
    if (handle) {
      handle.setAttribute('aria-valuemin', String(minW()));
      handle.setAttribute('aria-valuemax', String(ceiling()));
      handle.setAttribute('aria-valuenow', String(w));
      handle.setAttribute('aria-valuetext', 'Sidebar ' + w + ' pixels wide');
    }
    return w;
  }
  function set(v, persist) {
    pref = clamp(v);
    var w = apply();
    if (persist) save(pref === minW() ? null : w);
    return w;
  }
  function reset() { pref = null; apply(); save(null); }

  apply(); // before first paint

  function ready() {
    handle = document.querySelector('[data-sidebar-resizer]');
    if (!handle) return;
    var sidebar = document.getElementById(handle.getAttribute('aria-controls') || 'sidebar') || document.querySelector('.sidebar');
    handle.hidden = false;
    handle.setAttribute('aria-valuenow', String(minW())); // overwritten by apply() just below
    apply();

    var drag = null;
    function current() { return sidebar ? Math.round(sidebar.getBoundingClientRect().width) : minW(); }

    handle.addEventListener('pointerdown', function (ev) {
      if (ev.button !== 0) return;
      drag = { id: ev.pointerId, x: ev.clientX, w: current() };
      try { handle.setPointerCapture(ev.pointerId); } catch (e) { /* synthetic events */ }
      root.classList.add('is-resizing');
      ev.preventDefault();
    });
    handle.addEventListener('pointermove', function (ev) {
      if (!drag || ev.pointerId !== drag.id) return;
      set(drag.w + (ev.clientX - drag.x), false);
    });
    function end(ev) {
      if (!drag || ev.pointerId !== drag.id) return;
      try { handle.releasePointerCapture(ev.pointerId); } catch (e) { /* already released */ }
      drag = null;
      root.classList.remove('is-resizing');
      set(pref === null ? minW() : pref, true);
    }
    handle.addEventListener('pointerup', end);
    handle.addEventListener('pointercancel', end);
    handle.addEventListener('lostpointercapture', end);

    handle.addEventListener('dblclick', reset);
    handle.addEventListener('keydown', function (ev) {
      var step = ev.shiftKey ? BIG_STEP : STEP;
      var w = current();
      switch (ev.key) {
        case 'ArrowLeft': case 'ArrowDown': set(w - step, true); break;
        case 'ArrowRight': case 'ArrowUp': set(w + step, true); break;
        case 'Home': set(minW(), true); break;
        case 'End': set(ceiling(), true); break;
        case 'Enter': reset(); break;
        default: return;
      }
      ev.preventDefault();
    });

    // A smaller window can no longer fit the chosen width: show it clamped, remember the choice.
    window.addEventListener('resize', apply);
    // Another tab changed it.
    window.addEventListener('storage', function (ev) {
      if (ev.key !== KEY) return;
      pref = load();
      apply();
    });
  }

  if (document.readyState !== 'loading') ready();
  else document.addEventListener('DOMContentLoaded', ready);
})();
