// Apply the saved choice before the page is drawn. Only a display preference
// is stored. Storage can be unavailable in a private or restricted browser.
(() => {
  const media = matchMedia('(prefers-color-scheme: dark)');
  const valid = value => ['system', 'light', 'dark'].includes(value);
  let choice = 'system';
  try {
    const saved = localStorage.getItem('bonnie_web_theme');
    if (valid(saved)) choice = saved;
  } catch { /* Use the system setting when storage is unavailable. */ }
  function apply() {
    document.documentElement.dataset.theme = choice === 'system' ? (media.matches ? 'dark' : 'light') : choice;
    const control = document.getElementById('theme-toggle');
    if (control) {
      const label = `Theme: ${choice[0].toUpperCase()}${choice.slice(1)}`;
      if (control.textContent !== label) control.textContent = label;
      control.setAttribute('aria-label', `Theme: ${choice}. Switch to ${choice === 'system' ? 'light' : choice === 'light' ? 'dark' : 'system'}.`);
    }
  }
  apply();
  media.addEventListener('change', apply);
  document.addEventListener('DOMContentLoaded', () => {
    apply();
    // The header can be replaced by a Datastar view patch.
    new MutationObserver(apply).observe(document.body, {childList: true, subtree: true});
  });
  document.addEventListener('datastar-fetch', event => {
    if (event.detail.type === 'datastar-patch-elements') apply();
  });
  document.addEventListener('click', event => {
    if (!event.target.closest?.('#theme-toggle')) return;
    choice = choice === 'system' ? 'light' : choice === 'light' ? 'dark' : 'system';
    try { localStorage.setItem('bonnie_web_theme', choice); } catch { /* Keep the choice for this page. */ }
    apply();
  });
  window.addEventListener('storage', event => {
    if (event.key !== 'bonnie_web_theme' && event.key !== null) return;
    choice = valid(event.newValue) ? event.newValue : 'system';
    apply();
  });
})();
