/*
 * Mock interactions for the Maestria design files. Not production code.
 * Loaded in <head> (no defer) so the forced theme applies before first paint.
 */
(function () {
  var KEY = 'maestria-theme';
  var root = document.documentElement;

  function readTheme() {
    var fromUrl = new URLSearchParams(location.search).get('theme');
    if (fromUrl === 'light' || fromUrl === 'dark') return fromUrl;
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

  onReady(function () {
    // Component sheet: copy each light specimen into its dark twin.
    document.querySelectorAll('[data-clone-from]').forEach(function (target) {
      var src = document.getElementById(target.getAttribute('data-clone-from'));
      if (!src) return;
      var copy = src.cloneNode(true);
      copy.removeAttribute('id');
      copy.querySelectorAll('[id]').forEach(function (el) { el.id = el.id + '-dark'; });
      copy.querySelectorAll('[aria-controls]').forEach(function (el) { el.setAttribute('aria-controls', el.getAttribute('aria-controls') + '-dark'); });
      copy.querySelectorAll('[data-filter]').forEach(function (el) { el.setAttribute('data-filter', el.getAttribute('data-filter') + '-dark'); });
      copy.querySelectorAll('input[name]').forEach(function (el) { el.name = el.name + '-dark'; });
      copy.querySelectorAll('label[for]').forEach(function (el) { el.htmlFor = el.htmlFor + '-dark'; });
      while (copy.firstChild) target.appendChild(copy.firstChild);
    });

    // Theme toggle: system -> light -> dark -> system.
    document.querySelectorAll('[data-theme-toggle]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var current = root.getAttribute('data-theme');
        var next = current === null ? 'light' : current === 'light' ? 'dark' : null;
        applyTheme(next);
        try { next ? localStorage.setItem(KEY, next) : localStorage.removeItem(KEY); } catch (e) {}
        var label = next ? 'Theme: ' + next : 'Theme: system';
        btn.setAttribute('aria-label', label);
        btn.setAttribute('title', label);
      });
    });

    // Filter chips: <div data-filter="list-id"> with chips [data-value]; items [data-key] in #list-id.
    document.querySelectorAll('[data-filter]').forEach(function (group) {
      var list = document.getElementById(group.getAttribute('data-filter'));
      if (!list) return;
      group.addEventListener('click', function (ev) {
        var chip = ev.target.closest('.chip');
        if (!chip) return;
        group.querySelectorAll('.chip').forEach(function (c) { c.setAttribute('aria-pressed', String(c === chip)); });
        var value = chip.getAttribute('data-value');
        list.querySelectorAll('[data-key]').forEach(function (item) {
          item.hidden = value !== 'all' && item.getAttribute('data-key') !== value;
        });
      });
    });

    // Tabs: [role=tablist] > [role=tab][aria-controls].
    document.querySelectorAll('[role="tablist"]').forEach(function (list) {
      list.addEventListener('click', function (ev) {
        var tab = ev.target.closest('[role="tab"]');
        if (!tab) return;
        list.querySelectorAll('[role="tab"]').forEach(function (t) {
          var on = t === tab;
          t.setAttribute('aria-selected', String(on));
          var panel = document.getElementById(t.getAttribute('aria-controls'));
          if (panel) panel.hidden = !on;
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

    // Copy buttons: show feedback only.
    document.addEventListener('click', function (ev) {
      var btn = ev.target.closest('[data-copy]');
      if (!btn) return;
      var label = btn.getAttribute('aria-label') || '';
      btn.setAttribute('aria-label', 'Copied');
      btn.setAttribute('title', 'Copied');
      setTimeout(function () { btn.setAttribute('aria-label', label); btn.setAttribute('title', label); }, 1500);
    });

    // Docs tree starts collapsed on narrow screens.
    if (window.matchMedia('(max-width: 1023px)').matches) {
      document.querySelectorAll('.docs-nav[open]').forEach(function (d) { d.removeAttribute('open'); });
    }

    // Mock forms never submit.
    document.addEventListener('submit', function (ev) { ev.preventDefault(); });

    // Mock task actions: resolve the card in place.
    document.addEventListener('click', function (ev) {
      var btn = ev.target.closest('[data-resolve]');
      if (!btn) return;
      var card = btn.closest('[data-key], .task');
      if (!card) return;
      var note = document.createElement('p');
      note.className = 'task__response';
      note.textContent = btn.getAttribute('data-resolve') + ' · just now';
      card.querySelectorAll('.task__actions, .choice-list, .inline-form').forEach(function (el) { el.remove(); });
      card.removeAttribute('data-status');
      card.appendChild(note);
    });

    // Preview page: load the chosen screen into every frame.
    var picker = document.getElementById('preview-page');
    if (picker) {
      var themePicker = document.getElementById('preview-theme');
      var update = function () {
        var url = picker.value + (themePicker.value ? '?theme=' + themePicker.value : '');
        document.querySelectorAll('.preview-frame iframe').forEach(function (f) { f.src = url; });
      };
      picker.addEventListener('change', update);
      themePicker.addEventListener('change', update);
      var initial = new URLSearchParams(location.search).get('page');
      if (initial) picker.value = initial;
      update();
    }
  });
})();
