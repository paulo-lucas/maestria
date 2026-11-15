/* Maestria account pages: copy and download helpers for recovery codes and keys. No inline handlers (strict CSP). */
(function () {
  'use strict';

  function copyText(text, btn, done) {
    function ok() {
      if (!btn) return;
      var prev = btn.getAttribute('title');
      btn.setAttribute('title', done || 'Copied');
      setTimeout(function () { btn.setAttribute('title', prev || ''); }, 1500);
    }
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(ok).catch(function () {});
    }
  }

  function listText(id) {
    var ul = document.getElementById(id);
    if (!ul) return '';
    return Array.prototype.map.call(ul.querySelectorAll('li'), function (li) { return li.textContent.trim(); }).join('\n');
  }

  document.addEventListener('click', function (ev) {
    var list = ev.target.closest('[data-copy-list]');
    if (list) { copyText(listText(list.getAttribute('data-copy-list')), list, 'Copied'); return; }

    var dl = ev.target.closest('[data-download-list]');
    if (dl) {
      var text = 'Maestria recovery codes\nEach code works once.\n\n' + listText(dl.getAttribute('data-download-list')) + '\n';
      var blob = new Blob([text], { type: 'text/plain' });
      var a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = dl.getAttribute('data-filename') || 'codes.txt';
      document.body.appendChild(a);
      a.click();
      setTimeout(function () { URL.revokeObjectURL(a.href); a.remove(); }, 0);
      return;
    }

    // [data-copy] (a command or a key with a Copy button) is handled by copy.js.

    var gated = ev.target.closest('a[data-requires-link]');
    if (gated) {
      var box = document.getElementById(gated.getAttribute('data-requires-link'));
      if (box && !box.checked) ev.preventDefault();
    }
  });

  function syncGated() {
    document.querySelectorAll('a[data-requires-link]').forEach(function (a) {
      var box = document.getElementById(a.getAttribute('data-requires-link'));
      if (!box) return;
      var sync = function () {
        if (box.checked) { a.removeAttribute('aria-disabled'); a.classList.remove('is-disabled'); }
        else { a.setAttribute('aria-disabled', 'true'); a.classList.add('is-disabled'); }
      };
      box.addEventListener('change', sync);
      sync();
    });
  }
  if (document.readyState !== 'loading') syncGated();
  else document.addEventListener('DOMContentLoaded', syncGated);
})();
