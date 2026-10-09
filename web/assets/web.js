// Datastar v1.0.4 reports fetch lifecycle and patch events on the document.
// Keep status outside the live panel so journal patches cannot replace it.
document.addEventListener('datastar-fetch', (event) => {
  const el = event.detail.el;
  if (!el?.id.startsWith('live-connection')) return;
  if (!el.isConnected) return;
  if (event.detail.type === 'datastar-patch-elements') document.dispatchEvent(new CustomEvent('bonnie-live-ready', {detail: {url: el.dataset.url}}));
  const status = document.getElementById('connection-status');
  if (!status) return;
  const type = event.detail.type;
  const connected = type === 'datastar-patch-elements';
  status.dataset.state = connected ? 'connected' : type === 'started' ? 'connecting' : 'disconnected';
  status.textContent = connected ? 'Live · durable updates' : type === 'started' ? 'Connecting…' : type === 'retrying' ? 'Reconnecting…' : 'Disconnected · updates paused';
});

// Datastar morphs the application and replaces its live subscription. Browser
// history keeps real URLs for bookmarks and Back, without a document reload.
let navigating = false;
function navigate(url, push = true) {
  if (navigating) return Promise.reject(new Error('A view is still loading.'));
  const target = new URL(url, location.href);
  const expected = `/web/live?${new URLSearchParams({filter: target.searchParams.get('filter') || '', run: target.searchParams.get('run') || '', view: target.searchParams.get('view') || 'chat'})}`;
  if (document.getElementById('application')?.dataset.location === expected) {
    if (push) history.pushState(null, '', url);
    return Promise.resolve();
  }
  navigating = true;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => { cleanup(); reject(new Error('Cannot connect to live updates.')); }, 15000);
    const cleanup = () => { clearTimeout(timer); navigating = false; document.removeEventListener('bonnie-live-ready', ready); };
    const ready = (event) => { if (event.detail.url !== expected) return; cleanup(); if (push) history.pushState(null, '', url); resolve(); };
    document.addEventListener('bonnie-live-ready', ready);
    document.getElementById('navigation').dispatchEvent(new CustomEvent('bonnie-navigate', {detail: {url}}));
  });
}
document.addEventListener('click', (event) => {
  const link = event.target.closest?.('a');
  if (!link || event.button !== 0 || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey || link.target || link.hasAttribute('download')) return;
  const url = new URL(link.href);
  if (url.origin !== location.origin || !['/web', '/web/'].includes(url.pathname)) return;
  event.preventDefault();
  navigate(url.pathname + url.search).catch(error => { document.getElementById('action-status').textContent = error.message; });
});
window.addEventListener('popstate', () => { navigate(location.pathname + location.search, false).catch(error => { document.getElementById('action-status').textContent = error.message; }); });

// Actions use the channel through a same-origin, CSRF-protected form adapter.
document.addEventListener('submit', async (event) => {
  const form = event.target;
  if (!(form instanceof HTMLFormElement)) return;
  if (form.classList.contains('trace-filter')) {
    event.preventDefault();
    navigate(`/web/?${new URLSearchParams(new FormData(form))}`).catch(error => { document.getElementById('action-status').textContent = error.message; });
    return;
  }
  if (!form.classList.contains('action')) return;
  event.preventDefault();
  if (form.dataset.busy) return;
  const action = form.dataset.confirm;
  if ((action === 'clear' || action === 'reset') && !confirm(`${action === 'clear' ? 'Clear model context' : 'Retire this run'}? The journal stays available.`)) return;
  const status = document.getElementById('action-status');
  form.dataset.busy = 'true';
  const buttons = [...form.querySelectorAll('button')];
  buttons.forEach(button => { button.disabled = true; });
  status.textContent = 'Working… You can cancel an active turn with the run controls.';
  const message = form.querySelector('textarea[name="text"]');
  const submittedText = message?.value;
  try {
    let actionURL = form.action;
    const body = new URLSearchParams(new FormData(form));
    if (form.id === 'start-form') {
      // Create the run before sending, so no first-turn deltas are missed.
      const ensured = await fetch('/web/actions/ensure', {
        method: 'POST', credentials: 'same-origin',
        body: new URLSearchParams({csrf: body.get('csrf'), address: crypto.randomUUID()}),
        headers: {'Accept': 'application/json'}
      });
      const result = await ensured.json();
      if (!ensured.ok) throw new Error(result.error || 'Cannot create a run.');
      await navigate(`/web/?${new URLSearchParams({run: result.run_id})}`);
      body.set('run', result.run_id);
      actionURL = '/web/actions/send';
    }
    const response = await fetch(actionURL, {
      method: 'POST',
      credentials: 'same-origin',
      body,
      headers: { 'Accept': 'application/json' }
    });
    const text = await response.text();
    let result;
    try { result = JSON.parse(text); } catch { result = { error: text }; }
    if (!response.ok) throw new Error(result.error || `Request failed (${response.status})`);
    document.getElementById('action-status').textContent = 'Action completed.';
    if (result.run_id) {
      const current = new URL(location.href);
      if (current.searchParams.get('run') !== result.run_id) {
        await navigate(`/web/?${new URLSearchParams({run: result.run_id})}`);
      }
    }
    // Do not clear a new draft typed while the request was in progress.
    if (message && message.value === submittedText) { message.value = ''; fitComposer(message); }
  } catch (error) {
    document.getElementById('action-status').textContent = error.message || 'Request failed. Check the journal before you retry.';
  } finally {
    delete form.dataset.busy;
    buttons.forEach(button => { button.disabled = false; });
  }
});

// The composer grows with its draft up to the CSS max-height. The textarea
// has data-ignore-morph, so a live patch does not reset the inline height.
const composerIDs = ['start-text', 'send-text'];
function fitComposer(input) {
  input.style.height = 'auto';
  input.style.height = `${input.scrollHeight}px`;
}
document.addEventListener('input', (event) => {
  if (event.target instanceof HTMLTextAreaElement && composerIDs.includes(event.target.id)) fitComposer(event.target);
});

// Keep the transcript at the newest message while the reader is at the end.
// A reader who scrolls up to read history stays where they are.
let followTranscript = true;
let autoScrollUntil = 0;
const toEnd = (behavior) => {
  const transcript = document.getElementById('transcript');
  if (!transcript) return;
  autoScrollUntil = Date.now() + 600;
  transcript.scrollTo({ top: transcript.scrollHeight, behavior });
};
document.addEventListener('scroll', (event) => {
  const el = event.target;
  if (!(el instanceof HTMLElement) || el.id !== 'transcript' || Date.now() < autoScrollUntil) return;
  followTranscript = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
}, true);
new MutationObserver(() => { if (followTranscript) toEnd('smooth'); })
  .observe(document.querySelector('main') || document.body, { childList: true, subtree: true });
toEnd('instant');

// Plain Enter keeps a newline. Ctrl+Enter uses native form validation and Send.
document.addEventListener('keydown', (event) => {
  if (event.key !== 'Enter' || !event.ctrlKey || event.isComposing) return;
  const input = event.target;
  if (!(input instanceof HTMLTextAreaElement) || !['start-text', 'send-text'].includes(input.id)) return;
  const form = input.form;
  const send = form?.querySelector('button[type="submit"]');
  if (!send || send.disabled || form.dataset.busy) return;
  event.preventDefault();
  form.requestSubmit(send);
});
