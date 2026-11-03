/* EVOLV durable execution runs — an interactive inspector backed by /api/runs.
   The backend owns all lifecycle truth; this file only reflects it. */
(function () {
  'use strict';

  const phaseLabels = {
    queued: 'queued', planning: 'planning', running: 'running', waiting: 'waiting',
    verifying: 'verifying', paused: 'paused', completed: 'completed',
    failed: 'failed', cancelled: 'cancelled',
  };

  const esc = (s) => String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
  }[c]));

  let currentRunId = null;

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

  function timeAgo(iso) {
    if (!iso) return '';
    const t = new Date(iso).getTime();
    if (isNaN(t)) return iso;
    const diff = Math.max(0, Math.floor((Date.now() - t) / 1000));
    if (diff < 10) return 'just now';
    if (diff < 60) return diff + 's ago';
    if (diff < 3600) return Math.floor(diff / 60) + 'm ago';
    if (diff < 86400) return Math.floor(diff / 3600) + 'h ago';
    return Math.floor(diff / 86400) + 'd ago';
  }

  function formatDateTime(iso) {
    if (!iso) return '';
    const d = new Date(iso);
    if (isNaN(d.getTime())) return iso;
    return d.toLocaleString([], {
      month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit'
    });
  }

  function render(container, runs) {
    container.replaceChildren();
    if (!runs.length) {
      container.append(el('p', 'runs-empty', 'No durable runs yet. Long assignments create one.'));
      return;
    }
    for (const run of runs) {
      const card = el('article', 'run-card run-' + run.status);
      card.tabIndex = 0;
      card.setAttribute('role', 'button');
      card.setAttribute('aria-label', 'Open details for run ' + (run.title || run.id));

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

      card.addEventListener('click', () => openRun(run.id));
      card.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          openRun(run.id);
        }
      });
      container.append(card);
    }
  }

  async function openRun(runId) {
    if (!runId) return;
    currentRunId = runId;
    const modal = document.getElementById('run-modal');
    if (!modal) return;

    modal.classList.add('open');
    const titleEl = document.getElementById('run-modal-title');
    const statusEl = document.getElementById('run-modal-status');
    const metaEl = document.getElementById('run-modal-meta');
    const tasksList = document.getElementById('run-tasks-list');
    const eventsList = document.getElementById('run-events-list');

    if (titleEl) titleEl.textContent = 'Loading run…';
    if (statusEl) {
      statusEl.className = 'run-badge';
      statusEl.textContent = '…';
    }
    if (metaEl) metaEl.textContent = 'Fetching state from /api/runs/detail…';
    if (tasksList) tasksList.innerHTML = '<div class="run-loading">Loading task graph…</div>';
    if (eventsList) eventsList.innerHTML = '<div class="run-loading">Loading journal events…</div>';

    try {
      const [detailRes, eventsRes] = await Promise.all([
        fetch('/api/runs/detail?id=' + encodeURIComponent(runId), { cache: 'no-store' }),
        fetch('/api/runs/events?id=' + encodeURIComponent(runId), { cache: 'no-store' }),
      ]);

      if (!detailRes.ok) throw new Error('HTTP ' + detailRes.status + ' on detail');
      const detail = await detailRes.json();
      let events = [];
      if (eventsRes.ok) {
        const eventsData = await eventsRes.json();
        events = eventsData.events || [];
      }

      renderRunDetail(detail, events);
    } catch (err) {
      if (tasksList) {
        tasksList.innerHTML = '<div class="run-empty">Failed to load run: ' + esc(err.message) + '</div>';
      }
      if (eventsList) {
        eventsList.innerHTML = '<div class="run-empty">Journal unavailable.</div>';
      }
    }
  }

  function renderRunDetail(detail, events) {
    const run = detail.run || {};
    const progress = detail.progress || {};
    const blockedIds = new Set(detail.blocked || []);
    const readyTasks = new Set((detail.ready || []).map((t) => t.id));

    // Header info
    const titleEl = document.getElementById('run-modal-title');
    const statusEl = document.getElementById('run-modal-status');
    const metaEl = document.getElementById('run-modal-meta');
    if (titleEl) titleEl.textContent = run.title || run.id;
    if (statusEl) {
      statusEl.className = 'run-badge run-' + run.status;
      statusEl.textContent = phaseLabels[run.status] || run.status;
    }
    if (metaEl) {
      const parts = ['ID: ' + run.id];
      if (run.created_at) parts.push('Started ' + formatDateTime(run.created_at));
      if (run.closed_at) {
        parts.push('Finished ' + formatDateTime(run.closed_at));
      } else if (run.updated_at) {
        parts.push('Updated ' + timeAgo(run.updated_at));
      }
      metaEl.textContent = parts.join(' · ');
    }

    // Objective callout
    const objBox = document.getElementById('run-objective-box');
    const objText = document.getElementById('run-objective-text');
    if (objBox && objText) {
      if (run.objective) {
        objBox.hidden = false;
        objText.textContent = run.objective;
      } else {
        objBox.hidden = true;
      }
    }

    // Stats grid
    const statsGrid = document.getElementById('run-stats-grid');
    if (statsGrid) {
      const total = progress.total || (run.tasks || []).length || 0;
      const completed = progress.completed || 0;
      const failed = progress.failed || 0;
      const active = (progress.running || 0) + (progress.verifying || 0) + (progress.planning || 0);
      const blocked = blockedIds.size;
      const ready = readyTasks.size;

      statsGrid.innerHTML = `
        <span class="run-stat-item">Tasks: <b>${total}</b></span>
        <span class="run-stat-item stat-ok">Completed: <b>${completed}</b></span>
        ${failed ? `<span class="run-stat-item stat-err">Failed: <b>${failed}</b></span>` : ''}
        ${active ? `<span class="run-stat-item stat-active">Active: <b>${active}</b></span>` : ''}
        ${ready ? `<span class="run-stat-item">Ready: <b>${ready}</b></span>` : ''}
        ${blocked ? `<span class="run-stat-item stat-err">Blocked: <b>${blocked}</b></span>` : ''}
      `;
    }

    // Tab counts
    const tabTasksCount = document.getElementById('run-tab-tasks-count');
    const tabEventsCount = document.getElementById('run-tab-events-count');
    if (tabTasksCount) tabTasksCount.textContent = String((run.tasks || []).length);
    if (tabEventsCount) tabEventsCount.textContent = String(events.length);

    // Render Tasks list
    const tasksList = document.getElementById('run-tasks-list');
    if (tasksList) {
      const tasks = run.tasks || [];
      if (!tasks.length) {
        tasksList.innerHTML = '<div class="run-empty">No tasks defined in this run.</div>';
      } else {
        tasksList.innerHTML = tasks.map((task) => renderTaskCard(task, blockedIds, readyTasks)).join('');
        // Attach copy buttons for verification output
        tasksList.querySelectorAll('.run-copy-btn').forEach((btn) => {
          btn.addEventListener('click', (e) => {
            e.stopPropagation();
            const text = btn.dataset.copyText || '';
            navigator.clipboard.writeText(text).then(() => {
              const orig = btn.textContent;
              btn.textContent = 'Copied!';
              setTimeout(() => { btn.textContent = orig; }, 1500);
            });
          });
        });
      }
    }

    // Render Journal events
    const eventsList = document.getElementById('run-events-list');
    if (eventsList) {
      if (!events.length) {
        eventsList.innerHTML = '<div class="run-empty">No journal events recorded for this run.</div>';
      } else {
        eventsList.innerHTML = events.map((ev) => renderEventRow(ev)).join('');
      }
    }
  }

  function renderTaskCard(task, blockedIds, readyTasks) {
    const isBlocked = blockedIds.has(task.id);
    const isReady = readyTasks.has(task.id);
    const deps = task.depends_on || [];

    const metaItems = [];
    if (task.priority) metaItems.push('P' + task.priority);
    if (task.phase) metaItems.push(esc(task.phase));
    if (task.attempts && task.attempts > 0) metaItems.push(task.attempts + ' attempt' + (task.attempts === 1 ? '' : 's'));
    if (isBlocked) metaItems.push('<b style="color:var(--danger)">BLOCKED</b>');
    else if (isReady) metaItems.push('<b style="color:var(--ok)">READY</b>');

    let depsHtml = '';
    if (deps.length) {
      depsHtml = `<div class="run-task-dep">Depends on: ${deps.map((d) => `<code>${esc(d)}</code>`).join(' ')}</div>`;
    }

    let errorHtml = '';
    if (task.last_error) {
      errorHtml = `<div class="run-task-error">⚠️ ${esc(task.last_error)}</div>`;
    }

    let verifyHtml = '';
    if (task.verification) {
      const v = task.verification;
      const passedClass = v.passed ? 'passed' : 'failed';
      const statusText = v.passed ? '✓ PASSED' : '✕ FAILED (exit ' + v.exit_code + ')';
      const durText = v.duration_ms ? ' · ' + v.duration_ms + 'ms' : '';
      const cmdText = v.command ? esc(v.command) : esc(v.kind || 'check');

      let outputBlock = '';
      if (v.output) {
        outputBlock = `
          <pre class="run-term-output"><code>${esc(v.output.trim())}</code></pre>
        `;
      }

      verifyHtml = `
        <div class="run-verify-box">
          <div class="run-verify-head">
            <span class="run-verify-tag ${passedClass}">${statusText}${durText}</span>
            <span class="run-verify-cmd">${cmdText}</span>
            ${v.output ? `<button class="run-copy-btn" type="button" data-copy-text="${esc(v.output)}">Copy output</button>` : ''}
          </div>
          ${outputBlock}
        </div>
      `;
    }

    return `
      <article class="run-task-card task-${esc(task.status)}">
        <div class="run-task-head">
          <div class="run-task-title-group">
            <span class="run-task-title">${esc(task.title || task.id)}</span>
            <code class="run-task-id">${esc(task.id)}</code>
          </div>
          <span class="run-badge run-${esc(task.status)}">${esc(phaseLabels[task.status] || task.status)}</span>
        </div>
        ${task.objective ? `<div class="run-task-obj">${esc(task.objective)}</div>` : ''}
        ${depsHtml}
        ${errorHtml}
        ${metaItems.length ? `<div class="run-task-meta-row">${metaItems.join(' · ')}</div>` : ''}
        ${verifyHtml}
      </article>
    `;
  }

  function renderEventRow(ev) {
    const seq = '#' + ev.seq;
    const time = timeAgo(ev.at);
    const kindClass = 'k-' + (ev.kind || '').replace('.', '-');
    const taskTag = ev.task ? `<span class="event-task-tag">${esc(ev.task)}</span>` : '';

    let desc = '';
    const data = ev.data;
    if (data && typeof data === 'object') {
      if (ev.kind === 'task.moved') {
        desc = `${esc(data.from || 'unknown')} → ${esc(data.to || 'unknown')}`;
      } else if (ev.kind === 'verify.added') {
        const passed = data.passed ? 'PASSED' : 'FAILED';
        desc = `${esc(data.kind || 'verify')} ${passed} ${data.command ? `(${esc(data.command)})` : ''}`;
      } else if (ev.kind === 'task.error') {
        desc = esc(data.error || 'task error');
      } else if (ev.kind === 'task.attempt') {
        desc = `attempt #${esc(data.attempt || 1)}`;
      } else if (ev.kind === 'run.created') {
        desc = `Run initialized: ${esc(data.title || '')}`;
      } else if (ev.kind === 'run.closed') {
        desc = `Run closed with status ${esc(data.status || 'closed')}`;
      } else if (ev.kind === 'checkpoint') {
        desc = `Checkpoint snapshot at seq #${esc(data.seq || ev.seq)}`;
      } else {
        desc = esc(JSON.stringify(data));
      }
    } else if (typeof data === 'string') {
      desc = esc(data);
    }

    return `
      <div class="run-event-row">
        <span class="event-seq">${esc(seq)}</span>
        <span class="event-kind ${esc(kindClass)}">${esc(ev.kind || 'event')}</span>
        ${taskTag || '<span class="event-seq">—</span>'}
        <span class="event-desc" title="${esc(desc)}">${desc || esc(time)}</span>
      </div>
    `;
  }

  function closeRun() {
    currentRunId = null;
    const modal = document.getElementById('run-modal');
    if (modal) modal.classList.remove('open');
  }

  async function load(container) {
    try {
      const res = await fetch('/api/runs', { cache: 'no-store' });
      if (!res.ok) throw new Error('HTTP ' + res.status);
      const data = await res.json();
      const runs = (data.runs || []).filter((r) => r.title);
      render(container, runs);

      const count = document.getElementById('runs-count');
      const busy = runs.filter((r) => r.status === 'running' || r.status === 'planning' || r.status === 'verifying').length;
      if (container.closest('.sb-sec')) {
        container.closest('.sb-sec').hidden = runs.length === 0;
      }
      if (count) count.textContent = busy ? busy + ' active' : String(runs.length);
    } catch (e) {
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

    // Modal controls
    const modal = document.getElementById('run-modal');
    const closeBtn = document.getElementById('run-modal-close');
    const refreshBtn = document.getElementById('run-modal-refresh');

    if (closeBtn) closeBtn.addEventListener('click', closeRun);
    if (refreshBtn) {
      refreshBtn.addEventListener('click', () => {
        if (currentRunId) openRun(currentRunId);
      });
    }

    if (modal) {
      modal.addEventListener('click', (e) => {
        if (e.target === modal) closeRun();
      });
    }

    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && modal && modal.classList.contains('open')) {
        closeRun();
      }
    });

    // Tab switching
    const tabBtns = document.querySelectorAll('[data-run-tab]');
    tabBtns.forEach((btn) => {
      btn.addEventListener('click', () => {
        tabBtns.forEach((b) => b.classList.remove('active'));
        btn.classList.add('active');
        const tab = btn.dataset.runTab;
        const tasksView = document.getElementById('run-view-tasks');
        const journalView = document.getElementById('run-view-journal');
        if (tasksView && journalView) {
          tasksView.hidden = tab !== 'tasks';
          journalView.hidden = tab !== 'journal';
        }
      });
    });

    // Allow clicking the runs count badge in sidebar to open the first run if available
    const countBadge = document.getElementById('runs-count');
    if (countBadge) {
      countBadge.style.cursor = 'pointer';
      countBadge.addEventListener('click', (e) => {
        e.stopPropagation();
        const firstCard = list.querySelector('.run-card');
        if (firstCard) firstCard.click();
      });
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', mount);
  } else {
    mount();
  }
})();
