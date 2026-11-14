/*
 * Maestria panel behaviour. Vanilla JS, no inline handlers (strict CSP).
 * Loaded in <head> without defer so the saved theme applies before first paint.
 */
(function () {
  'use strict';
  var KEY = 'maestria-theme';
  var root = document.documentElement;

  function readTheme() {
    try { return localStorage.getItem(KEY); } catch (e) { return null; }
  }
  function applyTheme(theme) {
    if (theme === 'light' || theme === 'dark') root.setAttribute('data-theme', theme);
    else root.removeAttribute('data-theme');
  }
  applyTheme(readTheme());

  function onReady(fn) {
    if (document.readyState !== 'loading') fn();
    else document.addEventListener('DOMContentLoaded', fn);
  }

  function labelFor(theme) { return theme ? 'Theme: ' + theme : 'Theme: system'; }

  onReady(function () {
    // Theme toggle: system -> light -> dark -> system.
    document.querySelectorAll('[data-theme-toggle]').forEach(function (btn) {
      var label = labelFor(root.getAttribute('data-theme'));
      btn.setAttribute('aria-label', label);
      btn.setAttribute('title', label);
      btn.addEventListener('click', function () {
        var current = root.getAttribute('data-theme');
        var next = current === null ? 'light' : current === 'light' ? 'dark' : null;
        applyTheme(next);
        try { if (next) localStorage.setItem(KEY, next); else localStorage.removeItem(KEY); } catch (e) {}
        document.querySelectorAll('[data-theme-toggle]').forEach(function (b) {
          b.setAttribute('aria-label', labelFor(next));
          b.setAttribute('title', labelFor(next));
        });
      });
    });

    // Menus built on <details>: close on outside click and on Escape.
    document.addEventListener('click', function (ev) {
      document.querySelectorAll('details.menu[open]').forEach(function (d) {
        if (!d.contains(ev.target)) d.removeAttribute('open');
      });
    });
    document.addEventListener('keydown', function (ev) {
      if (ev.key !== 'Escape') return;
      document.querySelectorAll('details.menu[open]').forEach(function (d) {
        d.removeAttribute('open');
        var s = d.querySelector('summary');
        if (s) s.focus();
      });
    });

    // Dialogs: [data-open-dialog="id"] opens, [data-close-dialog] closes.
    document.addEventListener('click', function (ev) {
      var opener = ev.target.closest('[data-open-dialog]');
      if (opener) {
        var dlg = document.getElementById(opener.getAttribute('data-open-dialog'));
        if (dlg && dlg.showModal) dlg.showModal();
      }
      var closer = ev.target.closest('[data-close-dialog]');
      if (closer) {
        var d = closer.closest('dialog');
        if (d) d.close();
      }
    });

    // A button with data-requires="checkbox-id" is enabled only while that box is checked.
    document.querySelectorAll('[data-requires]').forEach(function (btn) {
      var box = document.getElementById(btn.getAttribute('data-requires'));
      if (!box) return;
      var sync = function () { btn.disabled = !box.checked; };
      box.addEventListener('change', sync);
      sync();
    });
  });
})();
