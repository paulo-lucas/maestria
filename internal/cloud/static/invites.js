/* The invitations page (administrators only): create an invitation, show its link once with a Copy
   button, revoke one. Listeners are on the document so the page works whether it was loaded
   directly or swapped in by the app's navigation. */
(function () {
  'use strict';
  var csrf = document.body.dataset.csrf;

  function $(s) { return document.querySelector(s); }

  async function call(path, payload) {
    var res = await fetch(path, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Accept': 'application/json', 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
      body: JSON.stringify(payload || {})
    });
    var data = null;
    try { data = await res.json(); } catch (e) { /* no body */ }
    if (res.status === 401) { location.href = '/login'; throw new Error('unauthenticated'); }
    if (!res.ok) throw new Error((data && data.error && data.error !== 'not_found' && data.error !== 'csrf' && data.error !== 'internal' ? data.error : 'That did not work. Reload the page and try again.'));
    return data;
  }

  /* Only the list is replaced, so the link card stays. */
  async function refreshList() {
    var res = await fetch('/admin/invites', { headers: { 'X-Fragment': '1' }, credentials: 'same-origin' });
    if (!res.ok) return;
    var doc = new DOMParser().parseFromString(await res.text(), 'text/html');
    var fresh = doc.querySelector('#invite-list'), cur = $('#invite-list');
    if (fresh && cur) cur.innerHTML = fresh.innerHTML;
  }

  function showError(msg) {
    var e = $('#invite-error');
    if (!e) return;
    e.textContent = msg || '';
    e.hidden = !msg;
  }

  document.addEventListener('input', function (ev) {
    if (ev.target.id !== 'invite-email') return;
    var send = $('#invite-send');
    if (!send || send.hasAttribute('data-no-mail')) return;
    var has = ev.target.value.trim() !== '';
    send.disabled = !has;
    if (!has) send.checked = false;
  });

  document.addEventListener('submit', async function (ev) {
    if (ev.target.id !== 'invite-form') return;
    ev.preventDefault();
    var btn = $('#invite-submit');
    showError('');
    btn.disabled = true;
    try {
      var r = await call('/api/invites', {
        email: $('#invite-email').value.trim(),
        note: $('#invite-note').value.trim(),
        expires: $('#invite-expires').value,
        sendEmail: $('#invite-send').checked
      });
      var link = r.link.charAt(0) === '/' ? location.origin + r.link : r.link;
      var code = $('#invite-link');
      code.textContent = link;
      $('#invite-result-note').textContent = r.emailed
        ? 'We also emailed it. Copy the link now if you want it too: it is shown only once.'
        : 'Copy the link now: it is shown only once.';
      var card = $('#invite-result');
      card.hidden = false;
      card.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
      $('#invite-form').reset();
      var send = $('#invite-send');
      if (send) send.disabled = true;
      await refreshList();
    } catch (e) {
      showError(e.message);
    } finally {
      btn.disabled = false;
    }
  });

  document.addEventListener('click', async function (ev) {
    var b = ev.target.closest('[data-action="revoke-invite"]');
    if (!b) return;
    b.disabled = true;
    try {
      await call('/api/invites/' + encodeURIComponent(b.getAttribute('data-id')) + '/revoke');
      await refreshList();
    } catch (e) {
      b.disabled = false;
      showError(e.message);
    }
  });
})();
