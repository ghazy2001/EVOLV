/* EVOLV durable execution runs — a small, quiet panel backed by /api/runs.
   The backend owns all lifecycle truth; this file only reflects it. */
(function () {
  'use strict';

  const phaseLabels = {
    queued: 'queued', planning: 'planning', running: 'running', waiting: 'waiting',
    verifying: 'verifying', paused: 'paused', completed: 'completed',
    failed: 'failed', cancelled: 'cancelled',
  };

  function el(tag, cls, text) {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text != null) n.textContent = text;
    return n;
  }

  function badge(status) {
    const b = el('span', 'run-badge run-' + status, phaseLabels[status] || status);
    return b;
  }

  function render(container, runs) {
    container.replaceChildren();
    if (!runs.length) {
      container.append(el('p', 'runs-empty', 'No durable runs yet. Long assignments create one.'));
      return;
    }
    for (const run of runs) {
      const card = el('article', 'run-card run-' + run.status);
      const head = el('div', 'run-head');
      head.append(el('b', null, run.title || run.id));
      head.append(badge(run.status));
      card.append(head);
      if (run.objective) card.append(el('p', 'run-objective', run.objective));
      const p = run.progress || {};
      const total = p.total || 0;
      const done = p.completed || 0;
      const pct = total ? Math.round((done / total) * 100) : 0;
      const bar = el('div', 'run-bar');
      const fill = el('i');
      fill.style.width = pct + '%';
      bar.append(fill);
      card.append(bar);
      const meta = el('div', 'run-meta');
      meta.append(el('span', null, done + '/' + total + ' tasks'));
      if (p.failed) meta.append(el('span', null, p.failed + ' failed'));
      if (run.ready) meta.append(el('span', null, run.ready + ' ready'));
      if (run.worktree && run.worktree.active) {
        const wt = run.worktree;
        const tag = el('span', 'run-worktree-tag', '🌿 ' + (wt.branch || 'isolated'));
        if (wt.merged) tag.textContent += ' (merged)';
        meta.append(tag);
      }
      card.append(meta);
      const actions = el('div', 'run-actions');
      if (run.status === 'running' || run.status === 'planning' || run.status === 'queued') {
        const cancelBtn = el('button', 'run-btn run-btn-danger', 'Cancel');
        cancelBtn.type = 'button';
        cancelBtn.onclick = async (ev) => {
          ev.stopPropagation();
          cancelBtn.disabled = true;
          try {
            await fetch('/api/runs/cancel', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ id: run.id }),
            });
            load(container);
          } catch (e) {
            cancelBtn.disabled = false;
          }
        };
        actions.append(cancelBtn);
      } else if (run.status === 'failed' || run.status === 'cancelled' || (p.failed && p.failed > 0)) {
        const retryBtn = el('button', 'run-btn', 'Retry');
        retryBtn.type = 'button';
        retryBtn.onclick = async (ev) => {
          ev.stopPropagation();
          retryBtn.disabled = true;
          try {
            await fetch('/api/runs/retry', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ id: run.id }),
            });
            load(container);
          } catch (e) {
            retryBtn.disabled = false;
          }
        };
        actions.append(retryBtn);
      }
      if (actions.children.length) card.append(actions);
      container.append(card);
    }
  }

  async function load(container) {
    try {
      const res = await fetch('/api/runs', { cache: 'no-store' });
      if (!res.ok) throw new Error('HTTP ' + res.status);
      const data = await res.json();
      const runs = data.runs.filter(r => r.title);
      render(container, runs);
      const count = document.getElementById('runs-count');
      const busy = runs.filter(r => r.status === 'running' || r.status === 'planning').length;
      // Only claim space when there is real work to show. Placement belongs to
      // the layout manager, so this never moves the panel between rails.
      if (container.closest('.sb-sec')) {
        container.closest('.sb-sec').hidden = runs.length === 0;
      }
      if (count) count.textContent = busy ? busy + ' active' : String(runs.length);
    } catch (e) {
      // A transient failure should not leave a stale panel shouting at the user.
      const panel = container.closest('.sb-sec');
      if (panel) panel.hidden = true;
      container.replaceChildren();
    }
  }

  function mount() {
    const host = document.getElementById('runs-panel');
    if (!host) return;
    const list = host.querySelector('.runs-list');
    if (!list) return;
    load(list);
    setInterval(() => {
      if (document.hidden) return;
      load(list);
    }, 15000);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', mount);
  } else {
    mount();
  }
})();
