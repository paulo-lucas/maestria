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

    // Floating menus: <div data-menu> holds a [data-menu-toggle] button and a popover panel. The
    // panel is in the top layer (no ancestor can clip it); it is anchored to the button here, opens
    // upward when there is no room below, and is a bottom sheet on a phone (see components.css).
    // The browser closes it on Escape and on an outside click; focus goes back to the button.
    var SHEET_QUERY = '(max-width: 640px)';
    var MENU_GAP = 6;
    var MENU_EDGE = 8;

    function menuToggleOf(panel) {
      return panel.parentElement && panel.parentElement.querySelector('[data-menu-toggle]');
    }

    function placeMenu(panel) {
      var btn = menuToggleOf(panel);
      if (!btn) return;
      if (window.matchMedia(SHEET_QUERY).matches) {
        panel.style.removeProperty('top');
        panel.style.removeProperty('left');
        panel.style.removeProperty('max-height');
        panel.setAttribute('data-placed', '');
        return;
      }
      var vw = document.documentElement.clientWidth;
      var vh = window.innerHeight;
      panel.style.removeProperty('max-height');
      var r = btn.getBoundingClientRect();
      var w = panel.offsetWidth;
      var h = panel.offsetHeight;
      var left = r.left + w > vw - MENU_EDGE ? r.right - w : r.left;
      left = Math.max(MENU_EDGE, Math.min(left, vw - w - MENU_EDGE));
      var below = vh - r.bottom - MENU_GAP - MENU_EDGE;
      var above = r.top - MENU_GAP - MENU_EDGE;
      var top;
      if (h <= below) {
        top = r.bottom + MENU_GAP;
      } else if (h <= above) {
        top = r.top - MENU_GAP - h;
      } else if (below >= above) {
        top = r.bottom + MENU_GAP;
        panel.style.maxHeight = Math.max(below, 80) + 'px';
      } else {
        var room = Math.max(above, 80);
        panel.style.maxHeight = room + 'px';
        top = r.top - MENU_GAP - Math.min(h, room);
      }
      panel.style.top = Math.max(MENU_EDGE, top) + 'px';
      panel.style.left = left + 'px';
      panel.setAttribute('data-placed', '');
    }

    function openMenus() {
      return document.querySelectorAll('[data-menu] > .menu__panel:popover-open');
    }

    function replaceOpenMenus() {
      openMenus().forEach(placeMenu);
    }

    document.addEventListener('pointerdown', function (ev) {
      var btn = ev.target.closest && ev.target.closest('[data-menu-toggle]');
      if (!btn) return;
      // The browser's outside-click close runs before the click: remember what it closed.
      var panel = btn.parentElement.querySelector('.menu__panel');
      btn._menuWasOpen = !!panel && panel.matches(':popover-open');
    });
    document.addEventListener('click', function (ev) {
      var btn = ev.target.closest && ev.target.closest('[data-menu-toggle]');
      if (!btn) return;
      var panel = btn.parentElement.querySelector('.menu__panel');
      if (!panel || typeof panel.showPopover !== 'function') return;
      var wasOpen = btn._menuWasOpen !== undefined ? btn._menuWasOpen : panel.matches(':popover-open');
      btn._menuWasOpen = undefined;
      if (wasOpen) {
        if (panel.matches(':popover-open')) panel.hidePopover();
        return;
      }
      panel.showPopover();
      if (ev.detail === 0) {
        var first = panel.querySelector('.menu__item:not([disabled])');
        if (first) first.focus();
      }
    });
    document.addEventListener('toggle', function (ev) {
      var panel = ev.target;
      if (!panel.matches || !panel.matches('[data-menu] > .menu__panel')) return;
      var btn = menuToggleOf(panel);
      var open = ev.newState === 'open';
      if (btn) btn.setAttribute('aria-expanded', open ? 'true' : 'false');
      if (open) {
        placeMenu(panel);
        return;
      }
      panel.removeAttribute('data-placed');
      var active = document.activeElement;
      if (btn && (!active || active === document.body || panel.contains(active))) btn.focus();
    }, true);
    document.addEventListener('keydown', function (ev) {
      if (ev.key !== 'ArrowDown' && ev.key !== 'ArrowUp') return;
      var toggle = ev.target.closest && ev.target.closest('[data-menu-toggle]');
      if (toggle) {
        // A menu button: the arrow keys open the menu and land on its first (or last) item.
        var menuPanel = toggle.parentElement.querySelector('.menu__panel');
        if (!menuPanel || typeof menuPanel.showPopover !== 'function') return;
        ev.preventDefault();
        if (!menuPanel.matches(':popover-open')) menuPanel.showPopover();
        var entries = menuPanel.querySelectorAll('.menu__item:not([disabled])');
        var entry = ev.key === 'ArrowDown' ? entries[0] : entries[entries.length - 1];
        if (entry) entry.focus();
        return;
      }
      var item = ev.target.closest && ev.target.closest('[data-menu] > .menu__panel .menu__item');
      if (!item) return;
      var items = Array.prototype.slice.call(item.parentElement.querySelectorAll('.menu__item:not([disabled])'));
      var i = items.indexOf(item);
      var next = items[(i + (ev.key === 'ArrowDown' ? 1 : items.length - 1)) % items.length];
      if (next) { ev.preventDefault(); next.focus(); }
    });
    window.addEventListener('resize', replaceOpenMenus);
    window.addEventListener('scroll', replaceOpenMenus, true);

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
