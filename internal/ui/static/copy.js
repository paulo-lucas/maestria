/* Command blocks (.code-inline): the Copy button next to a command copies exactly that line.
   One handler for the local panel and the hosted app, in a file of its own because the content
   security policy forbids inline scripts. Also builds the same markup for scripts that render
   pages in the browser (window.MaestriaCmd.html). */
(function () {
  'use strict';

  var RESET_MS = 1600;

  function esc(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  /* The markup of one command block. Keep it identical to the Go helper (ui "cmd"). */
  /* The words of a command, each in a span so a line never breaks inside "--link". */
  function words(command) {
    return String(command).split(' ').map(function (w) { return '<span class="cmd-t">' + esc(w) + '</span>'; }).join(' ');
  }

  function html(command) {
    return '<div class="code-inline"><code>' + words(command) + '</code>' +
      '<button class="btn btn--ghost btn--sm" type="button" data-copy aria-label="Copy command" title="Copy command">' +
      '<svg class="icon" aria-hidden="true"><use href="#i-copy"/></svg><span data-copy-label>Copy</span></button></div>';
  }

  /* A browser without the async clipboard (or on plain http) still gets a working button. */
  function writeText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text).catch(function () { return legacyCopy(text); });
    }
    return legacyCopy(text);
  }

  function legacyCopy(text) {
    return new Promise(function (resolve, reject) {
      var ta = document.createElement('textarea');
      ta.value = text;
      ta.setAttribute('readonly', '');
      ta.style.position = 'fixed';
      ta.style.top = '-1000px';
      document.body.appendChild(ta);
      ta.select();
      var ok = false;
      try { ok = document.execCommand('copy'); } catch (e) { ok = false; }
      ta.remove();
      if (ok) resolve(); else reject(new Error('copy failed'));
    });
  }

  function commandOf(btn) {
    var block = btn.closest('.code-inline');
    var code = block && block.querySelector('code');
    if (!code) return '';
    var text = code.textContent.trim();
    return btn.hasAttribute('data-copy-nospace') ? text.replace(/\s+/g, '') : text;
  }

  function flash(btn, ok) {
    var label = btn.querySelector('[data-copy-label]');
    var prevTitle = btn.getAttribute('data-copy-title') || btn.getAttribute('title') || 'Copy command';
    btn.setAttribute('data-copy-title', prevTitle);
    btn.setAttribute('title', ok ? 'Copied' : 'Select the command and copy it');
    if (label) {
      if (!label.hasAttribute('data-copy-text')) label.setAttribute('data-copy-text', label.textContent);
      label.textContent = ok ? 'Copied' : 'Press Ctrl+C';
    }
    btn.classList.toggle('is-copied', ok);
    clearTimeout(btn._copyTimer);
    btn._copyTimer = setTimeout(function () {
      btn.setAttribute('title', prevTitle);
      if (label) label.textContent = label.getAttribute('data-copy-text');
      btn.classList.remove('is-copied');
    }, RESET_MS);
  }

  document.addEventListener('click', function (ev) {
    var btn = ev.target.closest && ev.target.closest('[data-copy]');
    if (!btn) return;
    var text = commandOf(btn);
    if (!text) return;
    ev.preventDefault();
    writeText(text).then(function () { flash(btn, true); }, function () {
      // Nothing could be copied: select the command so Ctrl+C / Cmd+C works.
      var code = btn.closest('.code-inline').querySelector('code');
      var sel = window.getSelection && window.getSelection();
      if (code && sel) { var r = document.createRange(); r.selectNodeContents(code); sel.removeAllRanges(); sel.addRange(r); }
      flash(btn, false);
    });
  });

  window.MaestriaCmd = { html: html, words: words };
})();
