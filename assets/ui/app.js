/* EVOLV front brain (v6 shell) — connects to the organism over WebSocket and
   hot-swaps whatever the agent rewrites. No refresh. Ever. */
'use strict';

const $ = (s) => document.querySelector(s);
const state = {
  ws: null, connected: false, busy: false,
  evolutionCount: 0, mounted: new Map(), // name -> el
  shellReloadTimer: null, thinkingEl: null,
  evoAll: [], evoRevealed: 0, // ledger source of truth + revealed-beyond-cap count
  creations: [], creationHistory: [], candidates: [], libraryTab: 'library',
};

/* ---------------- update banner ---------------- */
function showUpdateBanner(u) {
  const b = $('#update-banner');
  if (!b || !u.version) return;
  $('#update-version').textContent = u.version;
  b.classList.add('show');
  toast(`🧬 <b>evolv v${u.version}</b> is out — a newer organism exists. <b>evolv update</b> to morph.`, 8000);
}
async function runUpdate() {
  toast('⬇️ morphing to the latest genome…');
  try {
    const r = await fetch('/api/update/run', { method: 'POST' });
    const d = await r.json();
    if (d.ok) toast(`✅ updated to <b>v${d.version}</b> — restart evolv to wake the new body`);
    else toast(`⚠️ update failed: ${d.error || 'unknown'}`);
  } catch { toast('⚠️ update failed — check your connection'); }
}

/* ---------------- toasts ---------------- */
function toast(msg, ms = 3800) {
  const t = document.createElement('div');
  t.className = 'toast'; t.innerHTML = msg;
  $('#toasts').appendChild(t);
  setTimeout(() => { t.classList.add('out'); setTimeout(() => t.remove(), 450); }, ms);
}
window.evolvToast = toast; // for grown components & plugins

/* ---------------- sound & soul: tiny WebAudio blips (default off) ---------------- */
let audioCtx = null;
function blip(freq = 660, dur = 0.07, type = 'sine', gain = 0.04) {
  if (localStorage.getItem('evolv.sound') !== '1') return;
  try {
    audioCtx = audioCtx || new (window.AudioContext || window.webkitAudioContext)();
    const o = audioCtx.createOscillator(), g = audioCtx.createGain();
    o.type = type; o.frequency.value = freq;
    g.gain.setValueAtTime(gain, audioCtx.currentTime);
    g.gain.exponentialRampToValueAtTime(0.0001, audioCtx.currentTime + dur);
    o.connect(g).connect(audioCtx.destination);
    o.start(); o.stop(audioCtx.currentTime + dur);
  } catch { /* silence is also a sound */ }
}
function setSound(on) {
  localStorage.setItem('evolv.sound', on ? '1' : '');
  $('#btn-sound').textContent = on ? '🔊' : '🔇';
  if (on) blip(880, 0.09, 'triangle', 0.05);
}

/* ---------------- websocket: the spine ---------------- */
function connect() {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  const ws = new WebSocket(`${proto}://${location.host}/ws`);
  state.ws = ws;
  ws.onopen = () => { state.connected = true; $('#provider-pill .dot').classList.remove('busy'); };
  ws.onclose = () => { state.connected = false; setTimeout(connect, 1200); };
  ws.onerror = () => ws.close();
  ws.onmessage = (e) => {
    let ev; try { ev = JSON.parse(e.data); } catch { return; }
    handle(ev);
  };
}

function handle(ev) {
  switch (ev.type) {
    case 'hello': {
      const h = ev.payload || {};
      if (h.agent) {
        $('#prov-name').textContent = h.agent.provider || 'kilo';
        $('#model-name').textContent = h.agent.model || '';
      }
      break;
    }
    case 'fs:change': onFsChange(ev.payload || {}, ev.time); blip(520, 0.05, 'sine', 0.03); break;
    case 'evolution': onEvolution(ev.payload || {}); blip(740, 0.06, 'triangle', 0.035); break;
    case 'plan': onPlan(ev.payload || {}); blip(620, 0.05, 'sine', 0.03); break;
    case 'agent:token': onToken((ev.payload || {}).text || ''); break;
    case 'agent:reasoning': onReasoning((ev.payload || {}).text || ''); break;
    case 'agent:phase': {
      const p = (ev.payload || {}).phase;
      const pill = $('#provider-pill');
      pill.classList.toggle('busy', p === 'thinking' || p === 'evolving');
      if (p === 'thinking' && !state.thinkingEl && !currentAssistant) showThinking();
      if (p === 'idle') finishAssistant();
      break;
    }
    case 'tool:call': onToolCall(ev.payload || {}); break;
    case 'agent:provider': {
      const p = ev.payload || {};
      $('#prov-name').textContent = p.provider; $('#model-name').textContent = p.model;
      toast(`🧠 mind swapped → <b>${p.provider} / ${p.model}</b>`);
      break;
    }
    case 'plugin:toast': toast((ev.payload || {}).text || ''); break;
    case 'security:lint': {
      const p = ev.payload || {};
      toast(`🛡️ <b>Security Alert</b>: ${escapeHtml(p.target || 'component')} ${p.status || 'flagged'}`);
      break;
    }
    case 'dream': {
      const st = (ev.payload || {}).state;
      const pill = $('#provider-pill');
      pill.classList.toggle('busy', st === 'start');
      if (st === 'start') toast('💭 <b>evolv is dreaming</b> — autonomous evolution in progress…', 7000);
      else toast('🌅 dream over — see what it grew in the sidebar', 6000);
      break;
    }
    case 'plugin:log': console.log('[plugin]', (ev.payload || {}).text); break;
    case 'shell:reload': {
      // LOOP GUARD — a storm of reload events (post-update seeding, a dream
      // rewriting the shell, rollback) must recompile the page ONCE, not
      // bounce it forever. Suppress anything within 10s of the last reload.
      if (shellReloadGuarded()) { console.log('[evolv] shell reload suppressed by loop guard'); break; }
      $('#recompile-path').textContent = (ev.payload || {}).reason || 'the organism is rewriting its own shell…';
      $('#recompile').classList.add('show');
      clearTimeout(state.shellReloadTimer);
      state.shellReloadTimer = setTimeout(() => { markShellReloaded(); location.reload(); }, 900);
      break;
    }
    case 'update:available': {
      const u = ev.payload || {};
      showUpdateBanner(u);
      break;
    }
    case 'config:updated': {
      const p = ev.payload || {};
      if (p.provider) $('#prov-name').textContent = p.provider;
      if (p.model) $('#model-name').textContent = p.model;
      break;
    }
    case 'canvas:request_capture': {
      const p = ev.payload || {};
      if (p.timestamp && Math.abs(Date.now() - p.timestamp) > 5000) return;
      captureOrganOrCanvas(p.target || 'canvas').then((dataUrl) => {
        if (!dataUrl) return;
        fetch('/api/canvas/screenshot', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ id: p.id || p.request_id || '', target: p.target || 'canvas', data: dataUrl })
        }).catch((err) => console.warn('[vision] upload error:', err));
      });
      break;
    }
  }
}

/* ---------------- shell reload loop guard ----------------
A recompile reloads the page; if the source of the reload (a seed, a dream,
an update storm) keeps firing after the reload, the page would bounce
forever on "recompiling shell". The guard: a sessionStorage timestamp set
the instant we decide to reload — survives the reload itself — suppresses
any further recompile overlay for 10 seconds. One morph, one reload. */
function shellReloadGuarded() {
  const last = +(sessionStorage.getItem('evolv.reloadAt') || 0);
  return Date.now() - last < 10000;
}
function markShellReloaded() { sessionStorage.setItem('evolv.reloadAt', String(Date.now())); }

/* ---------------- shell hot-reload (genome recompile) ---------------- */
const SHELL_FILES = new Set(['workspace/ui/index.html', 'workspace/ui/app.js']);
function onFsChange(ch, evTime) {
  const rel = ch.path.replace(/\\/g, '/');
  onWbFsChange(ch); // the workbench watches the project live
  // replayed/stale events must not trigger recompiles: only react to fresh
  // writes and renames (boot-time 'create' storms are how the genome seeds)
  const age = Date.now() - new Date(evTime).getTime();
  const fresh = (ch.kind === 'write' || ch.kind === 'rename') && age < 15000;
  if (rel === 'workspace/ui/style.css') {
    // SKIN HOT-SWAP — no reload, no flash. The stylesheet is swapped under
    // the running app; you literally watch the organism change clothes.
    if (!fresh) return;
    const link = document.querySelector('link[rel="stylesheet"]');
    if (link) {
      link.href = '/style.css?v=' + Date.now();
      toast('🎨 skin hot-swapped live — zero refresh', 2500);
    }
    return;
  }
  if (genomeOpen) refreshGenomeList(rel); // the inspector watches the genome live
  if (ch.kind === 'remove') {
    if (ch.zone === 'components') {
      const name = (ch.path.split('/').pop() || '').replace(/\.html$/, '');
      unmountComponent(name);
    }
    return;
  }
  if (fresh && SHELL_FILES.has(rel)) {
    // the organism rewrote its own shell — recompile visibly, then hot-swap.
    // Same loop guard: shell writes can arrive in bursts; reload once.
    if (shellReloadGuarded()) return;
    $('#recompile-path').textContent = ch.path;
    $('#recompile').classList.add('show');
    clearTimeout(state.shellReloadTimer);
    state.shellReloadTimer = setTimeout(() => { markShellReloaded(); location.reload(); }, 900);
    return;
  }
  if (ch.zone === 'components' && ch.path.endsWith('.html')) {
    const name = (ch.path.split('/').pop() || '').replace(/\.html$/, '');
    mountComponent(name, { fresh: true });
  }
}

/* ---------------- organs: the WHOLE PAGE is the canvas ----------------
Every component the organism grows mounts inline in the conversation —
full width, right where it was born — and stays reachable in the sidebar
Organs list. Click any organ there to open it across the entire window. */

async function fetchOrganHTML(name) {
  try {
    const r = await fetch(`/ui/components/${encodeURIComponent(name)}.html?${Date.now()}`);
    if (!r.ok) return null;
    return await r.text();
  } catch { return null; }
}

// graft <style> + markup + scripts from a component file into a container
function graftOrgan(container, html) {
  const doc = new DOMParser().parseFromString(html, 'text/html');
  container.innerHTML = '';
  for (const st of Array.from(doc.querySelectorAll('style'))) {
    const s = document.createElement('style');
    s.textContent = st.textContent;
    container.appendChild(s);
  }
  const scripts = Array.from(doc.querySelectorAll('script')).map((sc) => sc.textContent);
  const frag = document.createDocumentFragment();
  for (const node of Array.from(doc.body.childNodes)) {
    if (node.nodeName === 'SCRIPT' || node.nodeName === 'STYLE') continue;
    frag.appendChild(document.importNode(node, true));
  }
  container.appendChild(frag);
  for (const code of scripts) {
    const s = document.createElement('script');
    s.textContent = code;
    container.appendChild(s); // appending executes it
  }
}

// register an organ (sidebar chip + inline card in the chat flow)
async function mountComponent(name, opts) {
  const { fresh = false, inline = true } = opts || {};
  const html = await fetchOrganHTML(name);
  if (html == null) return;
  state.mounted.set(name, html); // registry: name -> raw html
  renderOrganList();
  if (inline) mountInline(name, html, fresh);
}

function organCard(name) {
  const card = document.createElement('div');
  card.className = 'comp-card';
  card.dataset.name = name;
  card.innerHTML = `<div class="comp-head"><span class="ico">🧩</span><span><div class="name">${escapeHtml(name)}</div><div class="desc">self-authored organ — click to open full-page</div></span><span class="full" title="open across the whole window">⤢</span><span class="x" title="ask evolv to remove it">✕</span></div><div class="comp-body"></div>`;
  card.querySelector('.x').addEventListener('click', () => send(`remove_component ${name} via remove_component, confirm in one line`));
  card.querySelector('.full').addEventListener('click', () => openOrgan(name));
  card.querySelector('.comp-head .name').addEventListener('click', () => openOrgan(name));
  return card;
}

function mountInline(name, html, fresh) {
  const scroll = $('#chat-scroll');
  let card = scroll.querySelector(`.comp-card[data-name="${CSS.escape(name)}"]`);
  if (card) {
    card.classList.remove('mutating'); void card.offsetWidth; card.classList.add('mutating');
  } else {
    card = organCard(name);
    scroll.appendChild(card);
  }
  graftOrgan(card.querySelector('.comp-body'), html);
  if (fresh) toast(`⚡ <b>${name}</b> grew into the page — live, no refresh`);
  scrollChat();
}

function unmountComponent(name) {
  state.mounted.delete(name);
  const card = $(`#chat-scroll .comp-card[data-name="${CSS.escape(name)}"]`);
  if (card) card.remove();
  if (state.organOpen === name) closeOrgan();
  renderOrganList();
}

function renderOrganList() {
  const list = $('#organ-list');
  if (!list) return;
  list.innerHTML = '';
  for (const name of state.mounted.keys()) {
    const chip = document.createElement('button');
    chip.className = 'organ-chip';
    chip.innerHTML = `<span>🧩</span> ${escapeHtml(name)}`;
    chip.addEventListener('click', () => openOrgan(name));
    list.appendChild(chip);
  }
  $('#organ-count').textContent = state.mounted.size;
  $('#organs-sec').style.display = state.mounted.size ? '' : 'none';
}

function openOrgan(name) {
  const html = state.mounted.get(name);
  if (html == null) return;
  state.organOpen = name;
  $('#organ-title').textContent = name;
  $('#organ-modal').classList.add('open');
  graftOrgan($('#organ-body'), html);
}
function closeOrgan() {
  state.organOpen = null;
  $('#organ-modal').classList.remove('open');
}

/* ---------------- vision: canvas & organ screenshots ---------------- */

async function captureOrganOrCanvas(target) {
  let el = null;
  if (!target || target === 'canvas' || target === 'all') {
    el = $('#chat-scroll');
  } else {
    el = document.querySelector(`.comp-card[data-name="${CSS.escape(target)}"] .comp-body`) ||
         document.querySelector(`.comp-card[data-name="${CSS.escape(target)}"]`);
  }
  if (!el) {
    el = $('#chat-scroll') || document.body;
  }
  return await rasterizeElementToDataURL(el, target);
}

async function rasterizeElementToDataURL(el, title) {
  if (!el) return null;
  // If the element has an HTML5 canvas inside, grab its frame directly
  const c = el.querySelector('canvas');
  if (c && c.width > 0 && c.height > 0) {
    try {
      return c.toDataURL('image/png');
    } catch (e) {
      // tainted canvas or other issue: continue to SVG rasterizer
    }
  }

  const w = Math.min(Math.max(el.offsetWidth || 640, 320), 1280);
  const h = Math.min(Math.max(el.offsetHeight || 400, 240), 960);

  try {
    const clone = el.cloneNode(true);
    clone.querySelectorAll('script').forEach((s) => s.remove());
    const serializer = new XMLSerializer();
    const xhtml = serializer.serializeToString(clone);

    const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="${w}" height="${h}">
      <style>
        * { box-sizing: border-box; }
        body { margin: 0; padding: 12px; font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; background: #121715; color: #e1ebe6; }
      </style>
      <foreignObject width="100%" height="100%">
        <div xmlns="http://www.w3.org/1999/xhtml">
          ${xhtml}
        </div>
      </foreignObject>
    </svg>`;

    const canvas = document.createElement('canvas');
    canvas.width = w;
    canvas.height = h;
    const ctx = canvas.getContext('2d');
    ctx.fillStyle = '#121715';
    ctx.fillRect(0, 0, w, h);

    const blob = new Blob([svg], { type: 'image/svg+xml;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const img = new Image();

    return await new Promise((resolve) => {
      img.onload = () => {
        try {
          ctx.drawImage(img, 0, 0);
          URL.revokeObjectURL(url);
          resolve(canvas.toDataURL('image/png'));
        } catch (e) {
          URL.revokeObjectURL(url);
          resolve(makeFallbackDataURL(w, h, title));
        }
      };
      img.onerror = () => {
        URL.revokeObjectURL(url);
        resolve(makeFallbackDataURL(w, h, title));
      };
      img.src = url;
    });
  } catch (err) {
    return makeFallbackDataURL(w, h, title);
  }
}

function makeFallbackDataURL(w, h, title) {
  const c = document.createElement('canvas');
  c.width = w || 640;
  c.height = h || 400;
  const ctx = c.getContext('2d');
  ctx.fillStyle = '#121715';
  ctx.fillRect(0, 0, c.width, c.height);
  ctx.strokeStyle = '#273831';
  ctx.lineWidth = 2;
  ctx.strokeRect(12, 12, c.width - 24, c.height - 24);
  ctx.fillStyle = '#1a2320';
  ctx.fillRect(12, 12, c.width - 24, 48);
  ctx.fillStyle = '#c9ef72';
  ctx.font = 'bold 16px -apple-system, BlinkMacSystemFont, sans-serif';
  ctx.fillText('EVOLV VISION · ' + (title || 'Organ'), 28, 42);
  ctx.fillStyle = '#8ca398';
  ctx.font = '12px monospace';
  ctx.fillText(`Snapshot ${c.width}x${c.height} · ${new Date().toLocaleTimeString()}`, 28, 90);
  return c.toDataURL('image/png');
}

/* ---------------- evolution stream (sidebar) ---------------- */
const KIND_META = {
  component: ['🧩'], tool: ['🔧'], skill: ['🧠'],
  prompt: ['✍️'], plugin: ['🔌'], file: ['📄'],
  edit: ['✏️'], delete: ['💥'], move: ['📦'],
  project: ['🎯'], remove_component: ['🗑'],
  snapshot: ['🧊'], rollback: ['⏪'], self_update: ['🦋'],
};
function evoItem(kind, name, desc, time, dream) {
  const [ico] = KIND_META[kind] || ['✨'];
  const t = time || new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
  const item = document.createElement('div');
  item.className = 'evo-item' + (dream ? ' evo-dream' : '');
  item.innerHTML = `<span class="ico">${dream ? '🌙' : ico}</span><span><div><span class="k">${escapeHtml(kind)}</span> <span class="n">${escapeHtml(name || '')}</span></div>${desc ? `<div class="d">${escapeHtml(desc)}</div>` : ''}</span><span class="t">${t}</span>`;
  return item;
}
// a dream run starts when a dream entry follows a non-dream one — mark it
// with a quiet divider so consecutive dream mutations read as one sleep
function dreamDivider() {
  const d = document.createElement('div');
  d.className = 'evo-dream-div';
  d.innerHTML = '<span>🌙 dream — nobody watching</span>';
  return d;
}
function bumpCounter() {
  for (const id of ['#evo-count', '#evo-count2']) {
    const c = $(id); if (c) c.textContent = state.evolutionCount;
  }
  const box = $('#evo-counter');
  box.classList.remove('bump'); void box.offsetWidth; box.classList.add('bump');
}
/* The ledger is virtualized: the DOM only ever holds the newest EVO_CAP
   entries (plus any you explicitly reveal). The full history lives in
   state.evoAll — a long-running organism that mutates all night stays smooth. */
const EVO_CAP = 200, EVO_PAGE = 100;

function renderEvoList() {
  const list = $('#stream-list');
  if (!list) return;
  const visible = state.evoAll.slice(0, EVO_CAP + state.evoRevealed);
  const hidden = state.evoAll.length - visible.length;
  list.textContent = '';
  const frag = document.createDocumentFragment();
  let prevDream = false;
  // walk newest -> oldest (top of list = latest mutation); a dream run opens
  // with a divider above its newest entry, exactly like the live prepend did
  for (const h of visible) {
    const isDream = !!h.dream;
    if (isDream && !prevDream) frag.appendChild(dreamDivider());
    frag.appendChild(evoItem(h.kind, h.name, h.description, h.t || (h.time || '').slice(11, 19), isDream));
    prevDream = isDream;
  }
  if (hidden > 0) {
    const b = el('button', 'evo-more');
    b.textContent = `⏵ show ${Math.min(EVO_PAGE, hidden)} older mutations — ${hidden} hidden`;
    b.onclick = () => { state.evoRevealed += EVO_PAGE; renderEvoList(); };
    frag.appendChild(b);
  }
  list.appendChild(frag);
}

function onEvolution(e) {
  state.evolutionCount = Math.max(state.evolutionCount, e.n || 0);
  loadMind(); // the mind panel updates the instant something grows
  bumpCounter();
  // stamp the clock once, at ingestion — re-renders must not let old events drift
  state.evoAll.unshift({
    kind: e.kind, name: e.name, description: e.description, dream: e.dream,
    t: new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' }),
  });
  renderEvoList();
}

async function loadEvolutionHistory() {
  try {
    const r = await fetch('/api/evolution');
    const d = await r.json();
    state.evolutionCount = d.count || 0;
    bumpCounter();
    state.evoAll = (d.history || []).slice().reverse().map((h) => ({
      kind: h.kind, name: h.name, description: h.description,
      time: h.time, dream: h.dream,
    }));
    renderEvoList();
    (d.components || []).forEach((n) => mountComponent(n, { fresh: false, inline: false }));
    countDreamsWhileAway(d);
  } catch {}
}

// "it dreamt N times while you were away" — count dream mutations newer than
// the last visit stamp; runs of ledger entries = one sleep each
function countDreamsWhileAway(d) {
  try {
    const last = +(localStorage.getItem('evolv.lastSeen') || 0);
    const now = Date.now();
    localStorage.setItem('evolv.lastSeen', String(now));
    if (!last) return; // first visit — nothing was "away"
    const dreams = (d.history || []).filter((h) => h.dream && (Date.parse(h.time) || 0) > last);
    if (!dreams.length) return;
    const txt = `🌙 it dreamt <b>${dreams.length}</b> time${dreams.length === 1 ? '' : 's'} while you were away — ${dreams.length} mutation${dreams.length === 1 ? '' : 's'}, zero prompts`;
    const w = $('#welcome');
    if (w) {
      const line = document.createElement('button');
      line.className = 'w-dreams';
      line.innerHTML = txt + '. open the replay →';
      line.addEventListener('click', openReplay);
      w.querySelector('.w-sub').after(line);
      return;
    }
    // returning to an existing conversation: a dismissible banner at the top
    const strip = document.createElement('div');
    strip.className = 'dream-return';
    strip.innerHTML = `<span>${txt} · <u>see what it made</u></span><button class="dr-x" title="dismiss">✕</button>`;
    strip.addEventListener('click', (e) => {
      if (e.target.classList.contains('dr-x')) { strip.remove(); return; }
      strip.remove(); openReplay();
    });
    $('#chat-scroll').prepend(strip);
  } catch {}
}

/* ---------------- snapshot diff ---------------- */
function fmtSnap(id) {
  // 20261031-160201 → Oct 31 16:02
  const m = /^((\d{4})(\d{2})(\d{2}))-(\d{2})(\d{2})(\d{2})$/.exec(id || '');
  if (!m) return id || '?';
  const months = ['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'];
  return `${months[+m[3] - 1]} ${+m[4]} ${m[5]}:${m[6]}`;
}
async function openDiff() {
  $('#diff-modal').classList.add('open');
  let ids = [];
  try {
    const r = await fetch('/api/snapshots');
    const d = await r.json();
    ids = d.snapshots || [];
  } catch {}
  const from = $('#diff-from'), to = $('#diff-to');
  from.innerHTML = ''; to.innerHTML = '';
  if (!ids.length) {
    $('#diff-body').innerHTML = '<div class="dm-empty">no snapshots yet — they are taken before every mutation</div>';
    return;
  }
  ids.forEach((id) => {
    from.appendChild(new Option(`${fmtSnap(id)}  (${id})`, id));
    to.appendChild(new Option(`${fmtSnap(id)}  (${id})`, id));
  });
  to.selectedIndex = 0;
  from.selectedIndex = Math.min(1, ids.length - 1);
  runDiff();
}
async function runDiff() {
  const from = $('#diff-from').value, to = $('#diff-to').value;
  if (!from || !to) return;
  $('#diff-body').innerHTML = '<div class="dm-empty">comparing genomes…</div>';
  let diffs = [];
  try {
    const r = await fetch(`/api/snapshots/diff?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}`);
    const d = await r.json();
    if (d.error) { $('#diff-body').innerHTML = `<div class="dm-empty">${escapeHtml(d.error)}</div>`; return; }
    diffs = d.diffs || [];
  } catch {}
  if (!diffs.length) { $('#diff-body').innerHTML = '<div class="dm-empty">these two genome states are identical</div>'; return; }
  const body = $('#diff-body');
  body.innerHTML = '';
  diffs.forEach((f) => {
    const box = el('div', 'dm-file');
    const head = el('div', 'dm-path');
    head.innerHTML = `<span>${escapeHtml(f.path)}</span><span class="dm-status ${f.status}">${f.status}</span>`;
    box.appendChild(head);
    (f.lines || []).forEach((l) => {
      const row = el('div', 'dm-line ' + (l.tag === '+' ? 'add' : l.tag === '-' ? 'del' : 'ctx'));
      row.innerHTML = `<span class="tag">${l.tag}</span><span>${escapeHtml(l.text) || '&nbsp;'}</span>`;
      box.appendChild(row);
    });
    body.appendChild(box);
  });
}
function closeDiff() { $('#diff-modal').classList.remove('open'); }

/* ---------------- replay timeline ---------------- */
// map a ledger entry to the genome file it most likely touched
function replayFilePath(kind, name) {
  if (!name) return null;
  if (kind === 'component') return `ui/components/${name}.html`;
  if (kind === 'tool') return `tools/${name}.js`;
  if (kind === 'skill') return `skills/${name}.md`;
  if (kind === 'plugin') return `plugins/${name}/plugin.js`;
  return null;
}
async function openReplay() {
  $('#replay-modal').classList.add('open');
  const body = $('#replay-body');
  body.innerHTML = '<div class="rp-empty">digging up the fossil record…</div>';
  let hist = [];
  try {
    const r = await fetch('/api/replay');
    const d = await r.json();
    hist = (d.history || []).slice().reverse(); // oldest first: watch it grow
    $('#replay-count').textContent = d.count || hist.length;
  } catch {}
  if (!hist.length) { body.innerHTML = '<div class="rp-empty">no mutations yet — ask it to evolve something</div>'; return; }
  body.innerHTML = '<div class="rp-spine"></div>';
  const spine = body.querySelector('.rp-spine');
  hist.forEach((h) => {
    const [ico] = KIND_META[h.kind] || ['✨'];
    const e = el('div', `rp-entry k-${h.kind || 'other'}`);
    const fp = replayFilePath(h.kind, h.name);
    e.innerHTML = `
      <div class="rp-time">${escapeHtml((h.time || '').slice(0, 19).replace('T', ' '))}</div>
      <div class="rp-line">${ico} <span class="rp-kind">${escapeHtml(h.kind || 'mutation')}</span> <span class="rp-name">${escapeHtml(h.name || '')}</span></div>
      ${h.description ? `<div class="rp-desc">${escapeHtml(h.description)}</div>` : ''}
      ${fp ? `<div class="rp-file">open in genome → ${escapeHtml(fp)}</div>` : ''}`;
    if (fp) e.addEventListener('click', () => { closeReplay(); openGenome(); openGenomeFile(fp, true); });
    spine.appendChild(e);
  });
}
function closeReplay() { $('#replay-modal').classList.remove('open'); }

/* ---------------- chat ---------------- */
function el(tag, cls, html) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (html !== undefined) e.innerHTML = html;
  return e;
}
function escapeHtml(s) {
  return (s || '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

let currentAssistant = null; // {m, body, raw, reason}
function showThinking() {
  if (currentAssistant) return;
  const m = el('div', 'msg evolv');
  m.innerHTML = `<div class="avatar">E</div><div class="body"><div class="bubble thinking"><i></i><i></i><i></i></div></div>`;
  $('#chat-scroll').appendChild(m);
  state.thinkingEl = m;
  scrollChat();
}
function hideThinking() {
  if (state.thinkingEl) { state.thinkingEl.remove(); state.thinkingEl = null; }
}

/* One assistant bubble per turn. Reasoning can arrive before any visible
   token, so both handlers need to be able to create it. */
function ensureAssistant() {
  if (currentAssistant) return currentAssistant;
  hideThinking();
  const m = el('div', 'msg evolv');
  m.innerHTML = `<div class="avatar">E</div><div class="body"></div>`;
  $('#chat-scroll').appendChild(m);
  currentAssistant = { m, body: m.querySelector('.body'), raw: '', reason: null };
  return currentAssistant;
}

function onToken(text) {
  const a = ensureAssistant();
  a.raw += text;
  scheduleRender();
}

/* reasoning / thinking tokens: streamed into a collapsible block kept out of
   the markdown HTML, so renderAssistant() cannot wipe it. */
function onReasoning(text) {
  const a = ensureAssistant();
  if (!a.reason) {
    a.reason = el('details', 'reason');
    a.reason.open = true;
    a.reason.innerHTML = '<summary>🤔 thinking…</summary><pre></pre>';
    a.body.prepend(a.reason);
  }
  a.reason.querySelector('pre').textContent += text;
  scrollChat();
}

/* rAF batching: tokens can arrive in bursts of dozens per second, but the
   full markdown parse + DOM rebuild only runs once per animation frame.
   finishAssistant() does the final synchronous render, so nothing is lost. */
let renderQueued = false;
function scheduleRender() {
  if (renderQueued) return;
  renderQueued = true;
  requestAnimationFrame(() => {
    renderQueued = false;
    if (!currentAssistant) return; // turn finished; final render already ran
    renderAssistant();
    scrollChat();
  });
}

// the streaming caret: a blinking block at the end of the last paragraph
function addCaret() {
  if (!currentAssistant) return;
  const body = currentAssistant.body;
  const old = body.querySelector('.caret'); if (old) old.remove();
  const c = el('span', 'caret');
  let last = body.lastElementChild;
  if (!last) { body.appendChild(c); return; }
  if (last.classList.contains('tool-group') || last.classList.contains('codeblock')) {
    body.appendChild(c);
  } else {
    last.appendChild(c);
  }
}

/* Render streamed text: visible prose + tool call cards for evolv blocks.
   Incomplete trailing evolv block shows a "running…" card. */
const ACTION_META = {
  evolve_component: ['🧩', 'grow component'], remove_component: ['🗑️', 'remove component'],
  evolve_tool: ['🔧', 'grow tool'], run_tool: ['⚙️', 'run tool'],
  evolve_skill: ['🧠', 'learn skill'], evolve_prompt: ['✍️', 'rewrite prompt'],
  evolve_plugin: ['🔌', 'install plugin'], write_file: ['📝', 'write file'],
  edit_file: ['✏️', 'edit file'], read_file: ['📖', 'read file'],
  list_dir: ['📂', 'list dir'], tree: ['🌲', 'tree'],
  delete_file: ['💥', 'delete'], move: ['📦', 'move'],
  set_project: ['🎯', 'bind project'], reload_shell: ['🔄', 'recompile shell'],
  set_plan: ['🗺️', 'plan'],
  snapshot: ['🧊', 'freeze genome'], rollback: ['⏪', 'rewind genome'],
  shell: ['❖', 'shell'], self_update: ['🦋', 'self update'],
};
function toolCard(act, pending) {
  const [ico, verb] = ACTION_META[act.action] || ['✨', act.action];
  const target = act.name || act.path || act.to || act.command || '';
  const shortTarget = String(target).length > 64 ? String(target).slice(0, 61) + '…' : target;
  return `<div class="tool-card${pending ? ' pending' : ''}" data-action="${escapeHtml(act.action || '')}" data-target="${escapeHtml(String(target))}">`
    + `<div class="tc-line"><span class="tc-dot"></span><span class="tc-verb">${escapeHtml(verb)}</span>`
    + (shortTarget ? `<span class="tc-target">${escapeHtml(shortTarget)}</span>` : '')
    + `<span class="tc-status">${pending ? '◌' : '✓'}</span></div></div>`;
}
function evolvBlockCards(body) {
  const acts = [];
  const dec = evolvLines(body);
  for (const line of dec) {
    try { acts.push(JSON.parse(line)); } catch {}
  }
  return acts.map((a) => toolCard(a, false)).join('');
}
// tolerant line-splitter: JSON objects may span lines, so use a brace scanner
function evolvLines(body) {
  const out = []; let depth = 0, cur = '', inStr = false, esc = false;
  for (const ch of body) {
    if (depth === 0 && !/\s/.test(ch) && ch !== '{') { continue; }
    cur += ch;
    if (esc) { esc = false; continue; }
    if (ch === '\\') { esc = true; continue; }
    if (ch === '"') inStr = !inStr;
    if (inStr) continue;
    if (ch === '{') depth++;
    else if (ch === '}') { depth--; if (depth === 0) { out.push(cur); cur = ''; } }
  }
  return out;
}
function renderAssistant() {
  const { raw, body } = currentAssistant;
  const re = /```evolv\s*\n([\s\S]*?)```/g;
  let htmlOut = '', last = 0, mch;
  while ((mch = re.exec(raw))) {
    htmlOut += mdLite(raw.slice(last, mch.index));
    htmlOut += `<div class="tool-group">` + evolvBlockCards(mch[1]) + `</div>`;
    last = re.lastIndex;
  }
  const tail = raw.slice(last);
  const openIdx = tail.lastIndexOf('```evolv');
  if (openIdx >= 0) {
    htmlOut += proseWithSalvage(tail.slice(0, openIdx));
    htmlOut += `<div class="tool-group">` + evolvLines(tail.slice(openIdx + 8)).map((l) => {
      try { return toolCard(JSON.parse(l), true); } catch { return `<div class="tool-card pending"><div class="tc-line"><span class="tc-dot"></span><span class="tc-verb">mutating genome…</span></div></div>`; }
    }).join('') + `</div>`;
  } else {
    htmlOut += proseWithSalvage(tail);
  }
  body.innerHTML = htmlOut;
  // the reasoning block is kept outside the rendered markdown: re-attach it
  // after every re-render so thoughts do not vanish when content arrives
  if (currentAssistant.reason) body.prepend(currentAssistant.reason);
  if (state.busy) addCaret();
}

/* Prose render that salvages action-JSON the model leaked without a fence:
   the kernel executes it anyway, so the UI shows it as a tool card instead of
   raw garbage. */
function proseWithSalvage(bodyText) {
  if (!bodyText) return '';
  let out = '', last = 0, depth = 0, start = -1, inStr = false, esc = false;
  const flush = (from, to) => { const seg = bodyText.slice(from, to); if (seg.trim()) out += mdLite(seg); };
  for (let i = 0; i < bodyText.length; i++) {
    const ch = bodyText[i];
    if (esc) { esc = false; continue; }
    if (ch === '\\' && inStr) { esc = true; continue; }
    if (ch === '"') { inStr = !inStr; continue; }
    if (inStr) continue;
    if (ch === '{') { if (depth === 0) start = i; depth++; }
    else if (ch === '}') {
      depth--;
      if (depth === 0 && start >= 0) {
        const obj = bodyText.slice(start, i + 1);
        if (obj.includes('"action"')) {
          try {
            flush(last, start);
            out += toolCard(JSON.parse(obj), false);
            last = i + 1;
          } catch { /* not valid JSON — leave it in the prose */ }
        }
        start = -1;
      } else if (depth < 0) depth = 0;
    }
  }
  flush(last, bodyText.length);
  return out;
}

/* Live tool status: the kernel publishes tool:call (running/ok/error) around
   every execution. Match the newest pending card with the same action+target. */
function onToolCall(p) {
  const cards = document.querySelectorAll(`.tool-card.pending[data-action="${CSS.escape(p.action || '')}"]`);
  for (let i = cards.length - 1; i >= 0; i--) {
    const c = cards[i];
    if (!p.target || !p.path || String(c.dataset.target) === String(p.target) || String(c.dataset.target) === String(p.path)) {
      c.classList.remove('pending');
      if (p.state === 'running') c.classList.add('running');
      else if (p.state === 'error') c.classList.add('error');
      const st = c.querySelector('.tc-status');
      if (st) st.textContent = p.state === 'running' ? '…' : (p.state === 'error' ? '✗' : '✓');
      return;
    }
  }
}

function finishAssistant() {
  if (!currentAssistant) return;
  hideThinking();
  renderAssistant();
  if (currentAssistant.reason) {
    currentAssistant.reason.open = false;
    const s = currentAssistant.reason.querySelector('summary');
    if (s) s.textContent = '🤔 thoughts';
  }
  const c = currentAssistant.body.querySelector('.caret'); if (c) c.remove();
  addCopyBtn(currentAssistant.m, currentAssistant.raw);
  currentAssistant = null;
  state.busy = false;
  setSendMode('send');
  $('#send').disabled = false;
}

// quiet copy button under assistant messages, GPT-style
function addCopyBtn(m, raw) {
  if (m.querySelector('.msg-actions')) return;
  m.dataset.raw = raw || '';
  const bar = el('div', 'msg-actions');
  const b = el('button', 'msg-copy', '⧉ copy');
  b.addEventListener('click', () => window.copyText(b));
  bar.appendChild(b);
  m.querySelector('.body').appendChild(bar);
}

// markdown lite v2: headings, lists, links, blockquotes, bold/italic/code,
// fenced code with copy buttons. Escape first, then build tags.
function mdLite(s) {
  let out = escapeHtml(s);
  // fenced code blocks first (grab before inline transforms touch them)
  out = out.replace(/```(\w*)\n([\s\S]*?)```/g, (_, l, c) =>
    `<div class="codeblock"><div class="cb-bar"><span>${l || 'code'}</span><button class="cb-copy" onclick="copyText(this)" data-code="${encodeURIComponent(c)}">copy</button></div><pre><code>${c}</code></pre></div>`);
  out = out.replace(/`([^`\n]+)`/g, '<code>$1</code>');
  out = out.replace(/\*\*([^*\n]+)\*\*/g, '<strong>$1</strong>');
  out = out.replace(/\*([^*\n]+)\*/g, '<em>$1</em>');
  out = out.replace(/\[([^\]\n]+)\]\((https?:[^)\s]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>');

  // line-based blocks: headings, lists, quotes, paragraphs
  const lines = out.split('\n');
  const html = [];
  let list = null, para = [];
  const flushPara = () => { if (para.length) { html.push(`<p>${para.join('<br>')}</p>`); para = []; } };
  const flushList = () => { if (list) { html.push(`<${list}>` + listItems.join('') + `</${list}>`); list = null; } };
  let listItems = [];
  for (const raw of lines) {
    const line = raw.trimEnd();
    const h = line.match(/^(#{1,4})\s+(.*)/);
    const ul = line.match(/^[-*]\s+(.*)/);
    const ol = line.match(/^\d+[.)]\s+(.*)/);
    const q = line.match(/^&gt;\s?(.*)/);
    if (!line.trim()) { flushPara(); flushList(); continue; }
    if (h) { flushPara(); flushList(); html.push(`<h${h[1].length + 2}>${h[2]}</h${h[1].length + 2}>`); continue; }
    if (ul || ol) {
      flushPara();
      const want = ul ? 'ul' : 'ol';
      if (list !== want) { flushList(); list = want; listItems = []; }
      listItems.push(`<li>${(ul || ol)[1]}</li>`);
      continue;
    }
    if (q) { flushPara(); flushList(); html.push(`<blockquote>${q[1]}</blockquote>`); continue; }
    flushList();
    para.push(line);
  }
  flushPara(); flushList();
  return html.join('');
}
// copy-to-clipboard for code blocks and messages (wired via inline onclick)
window.copyText = async function (btn) {
  const raw = btn.dataset.code !== undefined
    ? decodeURIComponent(btn.dataset.code)
    : btn.closest('.msg').dataset.raw || btn.closest('.msg').innerText;
  try {
    await navigator.clipboard.writeText(raw);
    const old = btn.textContent;
    btn.textContent = 'copied ✓';
    setTimeout(() => { btn.textContent = old; }, 1400);
  } catch { /* clipboard blocked; ignore */ }
};

function scrollChat() { const sc = $('#chat-scroll'); sc.scrollTop = sc.scrollHeight; }

function addMsg(role, text) {
  const m = el('div', 'msg ' + (role === 'user' ? 'user' : 'evolv'));
  if (role === 'user') {
    m.innerHTML = `<div class="bubble">${escapeHtml(text)}</div>`;
  } else {
    m.innerHTML = `<div class="avatar">E</div><div class="body">${mdLite(text)}</div>`;
    addCopyBtn(m, text);
  }
  $('#chat-scroll').appendChild(m);
  scrollChat();
  return m;
}

async function send(text) {
  text = (text !== undefined ? text : $('#input').value).trim();
  if (!text || state.busy) return;
  hideWelcome();
  $('#input').value = '';
  $('#input').style.height = 'auto';
  state.busy = true;
  setSendMode('stop');
  addMsg('user', text);
  showThinking();
  try {
    await fetch('/api/chat', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ message: text }),
    });
  } catch {
    hideThinking(); finishAssistant();
    toast('⚠️ kernel unreachable — is evolv still running?');
  }
}

/* the send button doubles as a stop button while the organism thinks */
function setSendMode(mode) {
  const b = $('#send');
  if (mode === 'stop') { b.textContent = '■'; b.title = 'stop the organism mid-thought'; b.classList.add('stop'); b.disabled = false; }
  else { b.textContent = '↑'; b.title = 'send'; b.classList.remove('stop'); b.disabled = false; }
}
async function stopGeneration() {
  try { await fetch('/api/stop', { method: 'POST' }); } catch {}
  toast('⏹ stop signal sent — the organism pauses');
}

/* ---------------- settings ---------------- */
function openDrawer() { $('#drawer').classList.add('open'); loadSettings(); }
async function loadSettings() {
  try {
    const [cfg, models, upd] = await Promise.all([
      fetch('/api/settings').then((r) => r.json()),
      fetch('/api/models').then((r) => r.json()).catch(() => ({})),
      fetch('/api/update').then((r) => r.json()).catch(() => ({})),
    ]);
    const c = cfg.config || {};
    $('#set-provider').value = c.provider || 'kilo';
    $('#set-model').value = c.model || '';
    $('#set-username').value = c.user_name || '';
    $('#set-project').value = c.project_dir || '';
    $('#set-dream').checked = !!c.auto_evolve;
    $('#set-dream-mins').value = c.heartbeat_mins || 15;
    $('#genome-path').textContent = '~/.evolv';
    const dl = $('#model-list'); dl.innerHTML = '';
    (models.models || []).forEach((m) => {
      const o = document.createElement('option'); o.value = m; dl.appendChild(o);
    });
    $('#genome-path').textContent = `~/.evolv  ·  v${upd.current || '?'}`;
    if (upd.available) showUpdateBanner(upd.available);
  } catch {}
}

async function saveSettings() {
  const body = {
    provider: $('#set-provider').value,
    model: $('#set-model').value.trim(),
    user_name: $('#set-username').value.trim(),
    project_dir: $('#set-project').value.trim(),
    auto_evolve: $('#set-dream').checked,
    heartbeat_mins: Math.max(1, parseInt($('#set-dream-mins').value, 10) || 15),
    api_keys: {},
    base_urls: {},
  };
  const key = $('#set-key').value.trim();
  if (key) body.api_keys[body.provider] = key;
  const base = $('#set-base').value.trim();
  if (base) body.base_urls[body.provider] = base;
  try {
    await fetch('/api/settings', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
    });
    toast('🧬 genome updated — new mind active');
    $('#drawer').classList.remove('open');
  } catch { toast('⚠️ could not save settings'); }
}

/* ---------------- plan panel (the agent's live task checklist) ---------------- */
const PLAN_ICON = { todo: '○', doing: '◐', done: '☑' };
function renderPlan(p) {
  const sec = $('#plan-sec'), list = $('#plan-list'), stats = $('#plan-stats');
  if (!sec || !p || !p.steps || !p.steps.length) { if (sec) sec.hidden = true; return; }
  const done = p.steps.filter((s) => s.status === 'done').length;
  sec.hidden = false;
  stats.textContent = `${done}/${p.steps.length}`;
  list.innerHTML = p.steps.map((s) => {
    const st = PLAN_ICON[s.status] || '○';
    return `<div class="plan-step ${escapeHtml(s.status || 'todo')}"><span class="pi">${st}</span><span class="pt">${escapeHtml(s.title)}</span></div>`;
  }).join('') + (p.title ? `<div class="plan-title">${escapeHtml(p.title)}</div>` : '');
}
function onPlan(e) { renderPlan(e); }
async function loadPlan() {
  try {
    const r = await fetch('/api/plan');
    const d = await r.json();
    if (d && d.steps) renderPlan(d);
  } catch {}
}

/* ---------------- mind panel (what the organism knows, live) ---------------- */
async function loadMind() {
  try {
    const r = await fetch('/api/mind');
    const d = await r.json();
    const box = $('#mind-list');
    if (!box) return;
    const row = (ico, cls, name) => `<span class="mind-item ${cls || ''}">${ico} ${escapeHtml(name)}</span>`;
    box.innerHTML =
      (d.skills || []).map((s) => row('🧠', 'm-skill', s)).join('') +
      (d.tools || []).map((t) => row('🔧', 'm-tool', t)).join('') +
      (d.plugins || []).map((p) => row('🔌', 'm-plugin', p)).join('') +
      (d.prompts || []).map((p) => row('✍️', 'm-prompt', p)).join('') || '<span class="mind-empty">nothing grown yet — ask it to evolve</span>';
    const c = $('#mind-stats');
    if (c) c.textContent = `${(d.skills || []).length}·${(d.tools || []).length}·${(d.plugins || []).length}`;
  } catch {}
}

/* ---------------- creation library + live pinned widgets ---------------- */
const CREATION_ICON = { widget:'🧩', tool:'🔧', skill:'🧠', plugin:'🔌', prompt:'✍️' };
function seenCreations(){try{return new Set(JSON.parse(localStorage.getItem('evolv.creations.seen')||'[]'))}catch{return new Set()}}
function markCreationsSeen(){localStorage.setItem('evolv.creations.seen',JSON.stringify(state.creations.map(c=>c.id)));$('#library-badge').hidden=true;$('#inbox-count').textContent='0'}
async function loadCreationLibrary(){try{const [d,c]=await Promise.all([fetch('/api/creations').then(r=>r.json()),fetch('/api/candidates').then(r=>r.json())]);state.creations=d.creations||[];state.creationHistory=d.evolution||[];state.candidates=c.candidates||[];const unseen=state.creations.filter(item=>!seenCreations().has(item.id));$('#library-count').textContent=state.creations.length;$('#inbox-count').textContent=unseen.length;$('#candidate-count').textContent=state.candidates.length;$('#library-badge').hidden=!unseen.length;$('#library-badge').textContent=unseen.length||'';await renderPinnedCreations();renderCreationLibrary()}catch{toast('⚠️ creation library could not be loaded')}}
async function renderPinnedCreations(){const panel=$('#pinned-sec'),list=$('#pinned-widget-list');if(!panel||!list)return;const pinned=state.creations.filter(c=>c.pinned&&!c.archived);panel.hidden=!pinned.length;$('#pinned-count').textContent=pinned.length;list.innerHTML='';for(const c of pinned){const item=document.createElement('article');item.className=`pinned-sidebar-card${c.enabled?'':' disabled'}`;item.dataset.id=c.id;item.innerHTML=`<header><button class="pinned-open"><span>${CREATION_ICON[c.type]||'◇'}</span><b>${escapeHtml(c.name)}</b><small>${escapeHtml(c.type)}</small></button><button class="pinned-remove" title="Unpin">×</button></header><div class="pinned-widget-body"></div>`;item.querySelector('.pinned-open').onclick=()=>{openGenome();setTimeout(()=>openGenomeFile(c.path.replace(/^workspace\//,'')),80)};item.querySelector('.pinned-remove').onclick=()=>creationAction(c,'pin');list.appendChild(item);const body=item.querySelector('.pinned-widget-body');if(c.type==='widget'&&c.enabled){const html=state.mounted.get(c.name)||await fetchOrganHTML(c.name);if(html!=null)graftOrgan(body,html)}else if(c.type==='widget'){body.innerHTML='<button class="pinned-enable">Enable live widget</button>';body.querySelector('button').onclick=()=>creationAction(c,'enable')}else body.innerHTML='<p class="pinned-widget-note">Pinned creation · click its title to inspect source.</p>'}}
function creationCard(c){const el=document.createElement('article');el.className=`creation-card${c.pinned?' pinned':''}${c.archived?' archived':''}`;const age=new Date(c.updated).toLocaleDateString([],{month:'short',day:'numeric'});const secBadge=c.security_status==='error'?'<b style="color:var(--danger)" title="'+escapeHtml(c.security_summary||'')+'">🛡️ INSECURE</b>':(c.security_status==='warning'?'<b style="color:var(--accent)" title="'+escapeHtml(c.security_summary||'')+'">🛡️ WARN</b>':'<b style="color:var(--ok)" title="Passed security checks">🛡️ SAFE</b>');el.innerHTML=`<div class="creation-card-head"><span class="creation-icon">${CREATION_ICON[c.type]||'◇'}</span><div><h3>${escapeHtml(c.name)}</h3><span>${escapeHtml(c.type)} · ${age}</span></div><i class="creation-health ${c.status}"></i></div><div class="creation-meta"><code>${escapeHtml(c.hash.slice(7,19))}</code><span>${Math.max(1,Math.round(c.bytes/1024))} KB</span>${secBadge}${c.pinned?'<b>PINNED</b>':''}${!c.enabled?'<b>OFF</b>':''}</div><div class="creation-actions">${c.type==='widget'?`<button data-act="enable">${c.enabled?'Disable':'Enable'}</button>`:''}<button data-act="pin">${c.pinned?'Unpin':'Pin'}</button><button data-act="source">Source</button><button data-act="archive">${c.archived?'Restore':'Archive'}</button></div>`;el.querySelectorAll('[data-act]').forEach(b=>b.onclick=()=>creationAction(c,b.dataset.act));return el}
async function creationAction(c,action){if(action==='source'){closeCreationLibrary();openGenome();setTimeout(()=>openGenomeFile(c.path.replace(/^workspace\//,'')),80);return}const value=action==='enable'?!c.enabled:action==='pin'?!c.pinned:!c.archived;const r=await fetch('/api/creations',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({id:c.id,action,value})});if(!r.ok){toast('⚠️ creation state could not be saved');return}await loadCreationLibrary()}
function renderCreationLibrary(){if(!$('#library-grid'))return;const q=$('#library-search').value.trim().toLowerCase(),type=$('#library-type').value,filter=$('#library-state').value;let items=state.creations.filter(c=>(!q||`${c.name} ${c.type} ${c.path}`.toLowerCase().includes(q))&&(type==='all'||c.type===type));items=items.filter(c=>filter==='all'||filter==='active'&&!c.archived||filter==='pinned'&&c.pinned||filter==='disabled'&&!c.enabled||filter==='archived'&&c.archived);items.sort((a,b)=>Number(b.pinned)-Number(a.pinned)||new Date(b.updated)-new Date(a.updated));const grid=$('#library-grid');grid.innerHTML='';items.forEach(c=>grid.appendChild(creationCard(c)));if(!items.length)grid.innerHTML='<div class="library-empty">No creations match this view.</div>';const seen=seenCreations(),inbox=$('#library-inbox');inbox.innerHTML='';state.creations.filter(c=>!seen.has(c.id)).forEach(c=>inbox.appendChild(creationCard(c)));if(!inbox.children.length)inbox.innerHTML='<div class="library-empty">Inbox zero. New autonomous creations will wait here for review.</div>';renderCandidates();renderCreationHistory()}
function renderCandidates(){const host=$('#library-candidates');host.innerHTML='';for(const c of state.candidates){const card=document.createElement('article');card.className=`candidate-card ${escapeHtml(c.status)}`;const checks=(c.checks||[]).map(x=>`<li class="${x.ok?'pass':'fail'}"><span>${x.ok?'✓':'×'}</span><code>${escapeHtml(x.command)}</code><small>${Math.round((x.duration_ms||0)/100)/10}s</small></li>`).join('');card.innerHTML=`<header><div><span class="candidate-status">${escapeHtml(c.status)}</span><h3>${escapeHtml(c.name||c.id)}</h3><small>${escapeHtml(c.id)} · ${(c.changes||[]).length} file${(c.changes||[]).length===1?'':'s'}</small></div>${c.status==='ready'?'<button data-promote>Promote candidate</button>':''}</header><ul>${checks||'<li>No validation evidence recorded.</li>'}</ul><details><summary>Changed files</summary>${(c.changes||[]).map(x=>`<p><code>${escapeHtml(x.Path||x.path)}</code><small>${escapeHtml((x.BeforeHash||x.before_hash||'new').slice(0,19))} → ${escapeHtml((x.AfterHash||x.after_hash||'').slice(0,19))}</small></p>`).join('')}</details>`;const promote=card.querySelector('[data-promote]');if(promote)promote.onclick=()=>promoteCandidate(c);host.appendChild(card)}if(!host.children.length)host.innerHTML='<div class="library-empty">No candidates yet. EVOLV will stage self-modifications here before touching its living source.</div>'}
async function promoteCandidate(c){if(!confirm(`Promote ${c.name||c.id} into the living source?\n\nAll source hashes will be checked again before any file is written.`))return;const r=await fetch('/api/candidates',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({action:'promote',id:c.id})}),text=await r.text();let out={};try{out=JSON.parse(text)}catch{}if(!r.ok){toast(`⚠️ promotion blocked: ${escapeHtml(out.error||text||'source changed')}`);return}toast('✅ candidate promoted after hash verification');await loadCreationLibrary()}
function renderCreationHistory(){const host=$('#library-history');host.innerHTML='';const groups=[];for(const e of [...state.creationHistory].reverse()){const key=`${e.kind||'mutation'}:${e.name||''}`;let g=groups.find(x=>x.key===key&&Math.abs(new Date(x.time)-new Date(e.time))<120000);if(!g){g={key,kind:e.kind||'mutation',name:e.name||'',time:e.time,entries:[]};groups.push(g)}g.entries.push(e)}groups.slice(0,100).forEach(g=>{const row=document.createElement('details');row.className='evolution-group';row.innerHTML=`<summary><span>${CREATION_ICON[g.kind]||'◇'}</span><div><b>${escapeHtml(g.name||g.kind)}</b><small>${g.entries.length} change${g.entries.length===1?'':'s'} · ${new Date(g.time).toLocaleString()}</small></div><em>${escapeHtml(g.kind)}</em></summary><div>${g.entries.map(e=>`<p>${escapeHtml(e.description||e.kind||'mutation')}</p>`).join('')}</div>`;host.appendChild(row)})}
function setLibraryTab(tab){state.libraryTab=tab;document.querySelectorAll('[data-library-tab]').forEach(b=>b.classList.toggle('active',b.dataset.libraryTab===tab));$('#library-grid').hidden=tab!=='library';$('#library-inbox').hidden=tab!=='inbox';$('#library-candidates').hidden=tab!=='candidates';$('#library-history').hidden=tab!=='history';$('[data-library-toolbar]').hidden=tab!=='library';if(tab==='inbox')markCreationsSeen()}
async function openCreationLibrary(){$('#library-modal').classList.add('open');$('#library-modal').setAttribute('aria-hidden','false');await loadCreationLibrary();$('#library-search').focus()}
function closeCreationLibrary(){$('#library-modal').classList.remove('open');$('#library-modal').setAttribute('aria-hidden','true')}

function openHologram(){const h=$('#hologram'),frame=$('#hologram-frame');h.hidden=false;if(!frame.src)frame.src=frame.dataset.src;document.body.classList.add('hologram-open')}
function closeHologram(){$('#hologram').hidden=true;document.body.classList.remove('hologram-open');$('#btn-dna').focus()}

/* ---------------- genome inspector (watch the body, live) ---------------- */
let genomeOpen = false, genomeFile = null, genomeDirty = false;

// file-type icon: the genome is all plain text, but each organ has a shape
function gIcon(path) {
  const ext = (path.split('.').pop() || '').toLowerCase();
  if (path.endsWith('plugin.js')) return '🔌';
  if (path.includes('/')) return { html: '🧩', js: '⚙️', md: '📝', css: '🎨', json: '🧬', jsonl: '📖', txt: '📄' }[ext] || '📄';
  return '🧬';
}

// fuzzy subsequence match → matched indices (for glyph highlighting), or null
function fuzzyMatch(q, s) {
  if (!q) return [];
  const idx = [];
  let i = 0;
  for (let j = 0; j < s.length && i < q.length; j++) if (s[j].toLowerCase() === q[i].toLowerCase()) idx.push(j++), i++;
  return i === q.length ? idx : null;
}

async function openGenome() {
  genomeOpen = true;
  $('#genome-modal').classList.add('show');
  $('#genome-filter').value = '';
  await refreshGenomeList();
}
function closeGenome() {
  if (genomeDirty && !confirm('unsaved edits to ' + genomeFile + ' — discard them?')) return;
  genomeOpen = false;
  $('#genome-modal').classList.remove('show');
}
async function refreshGenomeList(changedPath) {
  try {
    const r = await fetch('/api/genome');
    const d = await r.json();
    const list = $('#genome-list');
    const q = $('#genome-filter').value.trim();
    list.innerHTML = '';
    let shown = 0;
    (d.files || []).forEach((f) => {
      const hits = fuzzyMatch(q.replace(/\s+/g, ''), f);
      if (q && !hits) return; // doesn't match the filter
      shown++;
      const item = document.createElement('div');
      item.className = 'g-file' + (f === genomeFile ? ' active' : '');
      item.dataset.path = f;
      // render with icon + fuzzy-highlighted matched glyphs
      let name = '';
      for (let i = 0; i < f.length; i++) name += hits.includes(i) ? '<b>' + escapeHtml(f[i]) + '</b>' : escapeHtml(f[i]);
      item.innerHTML = '<span class="g-ico">' + gIcon(f) + '</span>' + name;
      item.addEventListener('click', () => switchGenomeFile(f));
      list.appendChild(item);
    });
    $('#genome-count').textContent = q
      ? `${shown} of ${(d.files || []).length} files · live`
      : `${(d.files || []).length} files · live`;
    if (changedPath) {
      const elx = [...list.children].find((x) => x.dataset.path === changedPath);
      if (elx) { elx.classList.add('g-flash'); setTimeout(() => elx.classList.remove('g-flash'), 1200); }
      // external change to the file being edited → reload it (unless typing)
      if (changedPath === genomeFile && !genomeDirty) openGenomeFile(genomeFile, true);
    }
  } catch {}
}

// switching files with unsaved edits → ask before throwing them away
async function switchGenomeFile(path) {
  if (genomeDirty && genomeFile !== path && !confirm('unsaved edits to ' + genomeFile + ' — discard and open ' + path + '?')) return;
  openGenomeFile(path);
}
async function openGenomeFile(path, silent) {
  genomeFile = path; genomeDirty = false;
  $('#btn-genome-revert').hidden = true;
  try {
    const r = await fetch('/api/files?path=' + encodeURIComponent(path));
    if (!r.ok) return;
    $('#genome-editor').value = await r.text();
    $('#genome-open-path').textContent = 'workspace/' + path;
    document.querySelectorAll('.g-file').forEach((x) => x.classList.toggle('active', x.dataset.path === path));
    if (!silent) $('#genome-editor').focus();
  } catch {}
}
function markGenomeDirty() {
  if (genomeDirty) return;
  genomeDirty = true;
  $('#btn-genome-revert').hidden = false;
  $('#genome-open-path').textContent = 'workspace/' + genomeFile + '  ● unsaved';
}
async function saveGenomeFile() {
  if (!genomeFile) return;
  try {
    const r = await fetch('/api/files', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ path: genomeFile, content: $('#genome-editor').value }),
    });
    if (r.ok) { genomeDirty = false; $('#btn-genome-revert').hidden = true; $('#genome-open-path').textContent = 'workspace/' + genomeFile; toast('⚡ written to the genome — the organism feels it live'); }
    else toast('⚠️ write failed');
  } catch { toast('⚠️ write failed'); }
}

/* ---------------- layout toggles ---------------- */
function setSidebar(hidden) {
  const willHide = hidden !== undefined ? hidden : !document.body.classList.contains('sb-hidden');
  document.body.classList.toggle('sb-hidden', willHide);
  localStorage.setItem('evolv.sb', willHide ? '1' : '');
  const toggle = document.getElementById('btn-sidebar');
  if (toggle) {
    toggle.setAttribute('aria-expanded', String(!willHide));
    toggle.setAttribute('aria-label', willHide ? 'Show sidebar' : 'Hide sidebar');
    toggle.textContent = willHide ? '›' : '‹';
  }
}
function isNarrow() { return window.matchMedia('(max-width: 820px)').matches; }

/* ---------------- share this conversation (⇪) ---------------- */
async function exportTranscript() {
  const msgs = [...document.querySelectorAll('#chat-scroll .msg')].map((m) => ({
    role: m.classList.contains('user') ? 'user' : 'evolv',
    text: (m.querySelector('.bubble') || m.querySelector('.body') || m).innerText || '',
  })).filter((m) => m.text.trim());
  if (!msgs.length) { toast('nothing to share yet — talk to the organism first'); return; }
  try {
    const r = await fetch('/api/export', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ title: 'EVOLV conversation — ' + new Date().toLocaleString(), messages: msgs }),
    });
    if (!r.ok) throw new Error('export failed');
    const blob = await r.blob();
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = 'evolv-transcript-' + new Date().toISOString().slice(0, 10) + '.html';
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 4000);
    toast(`⇪ transcript saved — ${msgs.length} messages, one standalone file`);
  } catch { toast('⚠️ export failed'); }
}

/* ---------------- conversation search (ctrl+f) ---------------- */
let searchRanges = [], searchCur = -1;
function searchTextNodes(root) {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, {
    acceptNode: (n) => (n.parentElement && n.parentElement.closest('#search-bar')) ? NodeFilter.FILTER_REJECT : NodeFilter.FILTER_ACCEPT,
  });
  return walker;
}
function clearSearchHighlights() {
  if (window.CSS && CSS.highlights) { CSS.highlights.delete('evolv-hit'); CSS.highlights.delete('evolv-hit-cur'); }
  searchRanges = []; searchCur = -1;
}
function runSearch(q) {
  clearSearchHighlights();
  if (!q || !window.CSS || !CSS.highlights) { $('#search-count').textContent = '0/0'; return; }
  const needle = q.toLowerCase();
  const walker = searchTextNodes(document.getElementById('chat-scroll'));
  let node;
  while ((node = walker.nextNode())) {
    const text = node.nodeValue.toLowerCase();
    let idx = text.indexOf(needle);
    while (idx !== -1) {
      const r = document.createRange();
      r.setStart(node, idx); r.setEnd(node, idx + needle.length);
      searchRanges.push(r);
      idx = text.indexOf(needle, idx + needle.length);
    }
  }
  searchCur = searchRanges.length ? 0 : -1;
  paintSearch();
}
function paintSearch() {
  const n = searchRanges.length;
  $('#search-count').textContent = n ? `${searchCur + 1}/${n}` : '0/0';
  if (!n) { CSS.highlights.delete('evolv-hit'); CSS.highlights.delete('evolv-hit-cur'); return; }
  CSS.highlights.set('evolv-hit', new Highlight(...searchRanges.filter((_, i) => i !== searchCur)));
  CSS.highlights.set('evolv-hit-cur', new Highlight(searchRanges[searchCur]));
  searchRanges[searchCur].startContainer.parentElement?.scrollIntoView({ block: 'center', behavior: 'smooth' });
}
function stepSearch(dir) {
  if (!searchRanges.length) return;
  searchCur = (searchCur + dir + searchRanges.length) % searchRanges.length;
  paintSearch();
}
function openSearch() {
  $('#search-bar').hidden = false;
  $('#search-input').focus(); $('#search-input').select();
  runSearch($('#search-input').value);
}
function closeSearch() {
  $('#search-bar').hidden = true;
  clearSearchHighlights();
  $('#input').focus();
}

/* ---------------- wiring ---------------- */
window.addEventListener('DOMContentLoaded', () => {
  connect();
  loadEvolutionHistory();
  loadMind();
  loadPlan();
  loadCreationLibrary();

  // restore panel state — phones start with the sidebar folded away
  setSidebar(localStorage.getItem('evolv.sb') === '1' || isNarrow());
  // tap the dark veil to close the mobile sidebar
  $('#sb-backdrop').addEventListener('click', () => setSidebar(true));
  // conversation search
  $('#search-input').addEventListener('input', (e) => runSearch(e.target.value));
  $('#search-input').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); stepSearch(e.shiftKey ? -1 : 1); }
  });
  $('#search-next').addEventListener('click', () => stepSearch(1));
  $('#search-prev').addEventListener('click', () => stepSearch(-1));
  $('#search-close').addEventListener('click', closeSearch);
  // share this conversation
  $('#btn-export').addEventListener('click', exportTranscript);
  // sound & soul — off until you ask for it
  $('#btn-sound').addEventListener('click', () => setSound(localStorage.getItem('evolv.sound') !== '1'));
  $('#btn-sound').textContent = localStorage.getItem('evolv.sound') === '1' ? '🔊' : '🔇';

  $('#send').addEventListener('click', () => { if (state.busy) stopGeneration(); else { blip(660, 0.06, 'sine', 0.04); send(); } });
  $('#input').addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send(); }
  });
  $('#input').addEventListener('input', function () {
    this.style.height = 'auto'; this.style.height = Math.min(this.scrollHeight, 140) + 'px';
  });
  document.querySelectorAll('.quick').forEach((b) => b.addEventListener('click', () => send(b.dataset.q)));
  $('#btn-settings').addEventListener('click', openDrawer);
  $('#provider-pill').addEventListener('click', openDrawer);
  $('#btn-close-settings').addEventListener('click', () => $('#drawer').classList.remove('open'));
  $('#btn-update').addEventListener('click', runUpdate);
  $('#btn-save').addEventListener('click', saveSettings);
  $('#btn-dream').addEventListener('click', async () => {
    toast('💭 tucking the organism in — dream starting…');
    try { await fetch('/api/dream', { method: 'POST' }); } catch { toast('⚠️ could not start dream'); }
  });
  $('#btn-files').addEventListener('click', openGenome);
  $('#btn-library').addEventListener('click', openCreationLibrary);
  $('#btn-library-close').addEventListener('click', closeCreationLibrary);
  $('#btn-pack-export').addEventListener('click', () => {
    toast('📦 Packaging genome into .evolvpack…', 2000);
    window.location.href = '/api/pack/export?ledger=1';
  });
  const packFileInput = $('#pack-file-input');
  $('#btn-pack-import').addEventListener('click', () => {
    packFileInput.value = '';
    packFileInput.click();
  });
  packFileInput.addEventListener('change', async (e) => {
    const file = e.target.files && e.target.files[0];
    if (!file) return;
    toast(`🔍 Inspecting <b>${file.name}</b>…`, 2500);
    try {
      const inspectRes = await fetch('/api/pack/inspect', {
        method: 'POST',
        headers: { 'Content-Type': 'application/octet-stream' },
        body: file
      });
      const report = await inspectRes.json();
      if (!inspectRes.ok) {
        toast(`⚠️ ${report.error || 'Failed to inspect package'}`);
        return;
      }
      const m = report.manifest || {};
      const fileCount = (m.files || []).length;
      const confCount = (report.conflicts || []).filter(c => c.exists && !c.identical).length;
      let msg = `Install .evolvpack (${fileCount} files by ${m.author || 'unknown'})?`;
      if (confCount > 0) msg += `\n\nNotice: ${confCount} local files will be updated.`;
      if (!confirm(msg)) return;

      toast('📥 Installing package…', 3000);
      const importRes = await fetch('/api/pack/import?force=1&quarantine=1', {
        method: 'POST',
        headers: { 'Content-Type': 'application/octet-stream' },
        body: file
      });
      const importReport = await importRes.json();
      if (importRes.ok) {
        const added = (importReport.created || []).length + (importReport.overwritten || []).length;
        toast(`✨ <b>Package installed!</b> ${added} files imported.`);
        await loadCreationLibrary();
      } else {
        toast(`⚠️ ${importReport.error || 'Failed to import package'}`);
      }
    } catch (err) {
      toast(`⚠️ Import error: ${err.message}`);
    }
  });
  $('#library-modal').addEventListener('click',e=>{if(e.target.id==='library-modal')closeCreationLibrary()});
  document.querySelectorAll('[data-library-tab]').forEach(b=>b.addEventListener('click',()=>setLibraryTab(b.dataset.libraryTab)));
  $('#library-search').addEventListener('input',renderCreationLibrary);
  $('#library-type').addEventListener('change',renderCreationLibrary);
  $('#library-state').addEventListener('change',renderCreationLibrary);
  $('#btn-dna').addEventListener('click',openHologram);
  $('#btn-dna-close').addEventListener('click',closeHologram);
  $('#hologram').addEventListener('click',e=>{if(e.target.id==='hologram')closeHologram()});
  $('#btn-snap-diff').addEventListener('click', openDiff);
  $('#btn-diff-close').addEventListener('click', closeDiff);
  $('#btn-diff-run').addEventListener('click', runDiff);
  $('#diff-modal').addEventListener('click', (e) => { if (e.target.id === 'diff-modal') closeDiff(); });
  $('#btn-replay').addEventListener('click', openReplay);
  $('#btn-replay-close').addEventListener('click', closeReplay);
  $('#replay-modal').addEventListener('click', (e) => { if (e.target.id === 'replay-modal') closeReplay(); });
  $('#btn-genome-close').addEventListener('click', closeGenome);
  $('#btn-genome-save').addEventListener('click', saveGenomeFile);
  $('#btn-genome-del').addEventListener('click', async () => {
    if (!genomeFile || !confirm(`delete ${genomeFile} from the genome? This cannot be undone (but a snapshot exists).`)) return;
    try {
      const r = await fetch('/api/files?path=' + encodeURIComponent(genomeFile), { method: 'DELETE' });
      if (r.ok) {
        toast(`💥 <b>${genomeFile}</b> pruned from the genome`);
        genomeFile = null;
        genomeDirty = false;
        $('#btn-genome-revert').hidden = true;
        $('#genome-open-path').textContent = 'pick a file — the organism feels every save live';
        $('#genome-editor').value = '';
        refreshGenomeList();
      } else toast('⚠️ delete failed');
    } catch { toast('⚠️ delete failed'); }
  });
  $('#genome-editor').addEventListener('input', () => { if (!genomeDirty) markGenomeDirty(); });
  $('#genome-editor').addEventListener('keydown', (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key === 's') { e.preventDefault(); saveGenomeFile(); }
  });
  // fuzzy filter: type to shrink the file tree, Esc clears
  $('#genome-filter').addEventListener('input', () => refreshGenomeList());
  $('#genome-filter').addEventListener('keydown', (e) => {
    if (e.key === 'Escape') { e.stopPropagation(); e.target.value = ''; refreshGenomeList(); }
    e.stopPropagation(); // don't fire global shortcuts while typing
  });
  // revert: throw away local edits, reload the genome's own version
  $('#btn-genome-revert').addEventListener('click', () => {
    if (!genomeFile) return;
    openGenomeFile(genomeFile);
    toast('↺ reverted — back to the genome\'s version');
  });

  // layout
  $('#btn-sidebar').addEventListener('click', () => setSidebar());
  $('#btn-sidebar-open').addEventListener('click', () => setSidebar(false));

  // theme (dark obsidian <-> light paper), persisted
  applyTheme(localStorage.getItem('evolv.theme') || 'dark');
  $('#btn-theme').addEventListener('click', () =>
    applyTheme(document.body.classList.contains('paper') ? 'dark' : 'paper'));

  // draggable sidebar width, persisted
  initSidebarResize();

  // keyboard shortcuts
  document.addEventListener('keydown', (e) => {
    const mod = e.ctrlKey || e.metaKey;
    if (mod && e.key.toLowerCase() === 'k') { e.preventDefault(); $('#btn-new').click(); return; }
    if (mod && e.key.toLowerCase() === 'b') { e.preventDefault(); setSidebar(!document.body.classList.contains('sb-hidden')); return; }
    if (mod && e.shiftKey && e.key.toLowerCase() === 'l') {
      e.preventDefault(); $('#btn-theme').click(); return;
    }
    if (mod && e.key.toLowerCase() === 'f' && !(document.activeElement && document.activeElement.closest('#genome-modal'))) {
      e.preventDefault(); openSearch(); return;
    }
    if (e.key === 'Escape') {
      if (!$('#search-bar').hidden) { closeSearch(); return; }
      if ($('#organ-modal').classList.contains('open')) closeOrgan();
      if ($('#genome-modal').classList.contains('show')) closeGenome();
      if ($('#replay-modal').classList.contains('open')) closeReplay();
      if ($('#diff-modal').classList.contains('open')) closeDiff();
      $('#drawer').classList.remove('open');
      return;
    }
    const tag = document.activeElement && document.activeElement.tagName;
    if (e.key === '/' && tag !== 'TEXTAREA' && tag !== 'INPUT') {
      e.preventDefault(); $('#input').focus();
    }
  });

  $('#btn-organ-close').addEventListener('click', closeOrgan);
  $('#organ-modal').addEventListener('click', (e) => { if (e.target.id === 'organ-modal') closeOrgan(); });
  $('#btn-organ-expand').addEventListener('click', () => $('#organ-modal').classList.toggle('wide'));
  $('#btn-new').addEventListener('click', async () => {
    $('#chat-scroll').innerHTML = '';
    currentAssistant = null; state.thinkingEl = null;
    try { await fetch('/api/reset', { method: 'POST' }); } catch {}
    showWelcome();
  });

  showWelcome();
  document.querySelectorAll('.w-card').forEach((b) => b.addEventListener('click', () => send(b.dataset.q)));
});

// theme: light "paper" flips every CSS variable through a warm daylight palette
function applyTheme(t) {
  document.body.classList.toggle('paper', t === 'paper');
  localStorage.setItem('evolv.theme', t);
  const b = $('#btn-theme');
  if (b) { b.textContent = t === 'paper' ? '🌙' : '☀️'; b.title = t === 'paper' ? 'back to the dark (ctrl+shift+l)' : 'daylight mode (ctrl+shift+l)'; }
}

// drag the sidebar edge to resize; width persists across boots
function initSidebarResize() {
  const rz = $('#sb-resizer');
  const saved = parseInt(localStorage.getItem('evolv.sbw') || '', 10);
  if (saved >= 200 && saved <= 480) document.documentElement.style.setProperty('--sb-w', saved + 'px');
  let startX = 0, startW = 0, dragging = false;
  rz.addEventListener('mousedown', (e) => { dragging = true; startX = e.clientX; startW = $('#sidebar').offsetWidth; document.body.style.cursor = 'col-resize'; e.preventDefault(); });
  window.addEventListener('mousemove', (e) => {
    if (!dragging) return;
    const w = Math.max(200, Math.min(480, startW + e.clientX - startX));
    document.documentElement.style.setProperty('--sb-w', w + 'px');
  });
  window.addEventListener('mouseup', () => {
    if (!dragging) return;
    dragging = false; document.body.style.cursor = '';
    const w = parseInt(getComputedStyle(document.documentElement).getPropertyValue('--sb-w'), 10);
    if (w) localStorage.setItem('evolv.sbw', String(w));
  });
}

function showWelcome() {
  if ($('#welcome')) return;
  const w = el('div', '');
  w.id = 'welcome';
  w.innerHTML = `
    <div class="w-mark evolv-mark" role="img" aria-label="EVOLV evolving emblem"><svg viewBox="0 0 64 64" aria-hidden="true"><circle class="em-orbit em-orbit-outer" cx="32" cy="32" r="25"/><circle class="em-orbit em-orbit-inner" cx="32" cy="32" r="19"/><path class="em-core em-core-one" d="M32 11 49 32 32 53 15 32Z"/><path class="em-core em-core-two" d="M32 14 46 22 46 42 32 50 18 42 18 22Z"/><path class="em-core em-core-three" d="M32 13 38 26 51 32 38 38 32 51 26 38 13 32 26 26Z"/><circle class="em-seed" cx="32" cy="32" r="5"/><circle class="em-satellite" cx="32" cy="7" r="3"/></svg></div>
    <h1>What should evolve today?</h1>
    <p class="w-sub">EVOLV is a workspace that rewrites itself — its UI, its tools, its mind — and works on your code anywhere on this machine.</p>
    <div class="w-grid">
      <button class="w-card" data-q="evolve a pomodoro focus timer onto the canvas"><span class="w-ico">⏱</span><b>Pomodoro timer</b><span>a live focus widget, grown in seconds</span></button>
      <button class="w-card" data-q="evolve a live CPU/memory dashboard component that uses the shell tool"><span class="w-ico">📊</span><b>System monitor</b><span>real CPU / memory, polled live</span></button>
      <button class="w-card" data-q="evolve yourself: grow a new tool, skill, or component of your own choosing — surprise me"><span class="w-ico">🧬</span><b>Evolve freely</b><span>it picks its own next mutation</span></button>
      <button class="w-card" data-q="summarize what you are and what you can do, then show me your genome"><span class="w-ico">🗂</span><b>Show your genome</b><span>a guided tour of its own body</span></button>
    </div>`;
  $('#chat-scroll').appendChild(w);
  w.querySelectorAll('.w-card').forEach((b) => b.addEventListener('click', () => send(b.dataset.q)));
}
function hideWelcome() {
  const w = $('#welcome');
  if (w) w.remove();
}

// (the empty-state welcome replaced the injected greeting — no fake first message)

/* ================= WORKBENCH — a real dev environment for the user's project =================
The genome inspector edits the organism's own body; the workbench opens the
user's bound project (config.project_dir, or organism home when unbound).
Lazy file tree, in-place text editor with ctrl+s, new file/dir, rename,
delete — every write lands on real disk and publishes fs:change, so the
organism FEELS the workbench editing its host project live. */

let wbOpenFlag = false;
let wbExpanded = new Set();   // dir paths currently expanded
let wbSelected = null;        // selected tree row path (dir or file)
let wbFile = null;            // file open in the editor
let wbDirty = false;
let wbRootName = '';

async function openWorkbench() {
  wbOpenFlag = true;
  $('#wb-modal').classList.add('show');
  try {
    const r = await fetch('/api/wb/root');
    const j = await r.json();
    wbRootName = j.name || 'project';
    $('#wb-root-name').textContent = wbRootName;
    $('#wb-root-files').textContent = j.files != null ? ` · ${j.files} files` : '';
  } catch { $('#wb-root-name').textContent = 'unreachable'; }
  refreshWbTree();
}
function closeWorkbench() {
  if (wbDirty && !confirm('unsaved edits will be lost — close anyway?')) return;
  wbOpenFlag = false;
  wbDirty = false;
  $('#wb-dirty').hidden = true;
  $('#wb-modal').classList.remove('show');
}

async function refreshWbTree() {
  const box = $('#wb-tree');
  box.innerHTML = '<div class="wb-loading">loading project…</div>';
  try {
    const r = await fetch('/api/wb/tree?open=1&depth=2');
    const j = await r.json();
    box.innerHTML = '';
    if (!j.entries || !j.entries.length) {
      box.innerHTML = '<div class="wb-loading">empty project — hit "＋ file" to create the first one</div>';
      return;
    }
    box.appendChild(buildWbLevel(j.entries));
  } catch { box.innerHTML = '<div class="wb-loading">⚠️ could not list the project</div>'; }
}

// build one nested <ul> of entries; dirs already carrying kids render open
function buildWbLevel(entries) {
  const ul = document.createElement('ul');
  ul.className = 'wb-ul';
  for (const e of entries) {
    const li = document.createElement('li');
    li.className = 'wb-row' + (e.dir ? ' is-dir' : ' is-file') + (wbSelected === e.path ? ' sel' : '');
    li.dataset.path = e.path;
    const icon = wbIcon(e.name, e.dir);
    const arrow = e.dir ? '<span class="wb-arrow">' + (e.open || wbExpanded.has(e.path) ? '▾' : '▸') + '</span>' : '<span class="wb-arrow"></span>';
    li.innerHTML = `${arrow}<span class="wb-ico">${icon}</span><span class="wb-name">${escapeHtml(e.name)}</span>` +
      (e.dir ? '' : `<span class="wb-size">${fmtSize(e.size)}</span>`);
    li.title = e.path;
    if (e.dir) {
      li.addEventListener('click', (ev) => { ev.stopPropagation(); toggleWbDir(li, e); });
      li.addEventListener('dblclick', (ev) => { ev.stopPropagation(); toggleWbDir(li, e); });
      li.addEventListener('contextmenu', (ev) => { ev.preventDefault(); ev.stopPropagation(); wbSelected = e.path; wbDirMenu(e); });
    } else {
      li.addEventListener('click', (ev) => { ev.stopPropagation(); wbSelected = e.path; openWbFile(e.path); });
      li.addEventListener('contextmenu', (ev) => { ev.preventDefault(); ev.stopPropagation(); wbSelected = e.path; wbFileMenu(e); });
    }
    ul.appendChild(li);
    // nested kids: either preloaded from the server or expanded on demand
    const kidBox = document.createElement('li');
    kidBox.className = 'wb-kids';
    kidBox.dataset.kidsof = e.path;
    ul.appendChild(kidBox);
    if (e.dir && (e.open || wbExpanded.has(e.path))) {
      wbExpanded.add(e.path);
      li.querySelector('.wb-arrow').textContent = '▾';
      if (e.kids && e.kids.length) kidBox.appendChild(buildWbLevel(e.kids));
      else if (e.kids) kidBox.innerHTML = '<div class="wb-empty">(empty)</div>';
      else loadWbKids(kidBox, e.path);
    }
  }
  return ul;
}

async function loadWbKids(box, dirPath) {
  box.innerHTML = '<div class="wb-loading">…</div>';
  try {
    const r = await fetch('/api/wb/tree?path=' + encodeURIComponent(dirPath) + '&open=1&depth=1');
    const j = await r.json();
    box.innerHTML = '';
    if (!j.entries || !j.entries.length) { box.innerHTML = '<div class="wb-empty">(empty)</div>'; return; }
    box.appendChild(buildWbLevel(j.entries));
  } catch { box.innerHTML = '<div class="wb-empty">⚠️</div>'; }
}

function toggleWbDir(li, e) {
  const box = li.parentElement.querySelector(`[data-kidsof="${CSS.escape(e.path)}"]`);
  if (!box) return;
  if (wbExpanded.has(e.path)) {
    wbExpanded.delete(e.path);
    box.innerHTML = '';
    li.querySelector('.wb-arrow').textContent = '▸';
    li.classList.remove('sel');
  } else {
    wbExpanded.add(e.path);
    wbSelected = e.path;
    li.querySelector('.wb-arrow').textContent = '▾';
    loadWbKids(box, e.path);
  }
}

function wbIcon(name, isDir) {
  if (isDir) return '📁';
  const ext = (name.split('.').pop() || '').toLowerCase();
  const map = { js:'🟨', ts:'🟦', go:'🐹', py:'🐍', rs:'🦀', html:'🌐', css:'🎨', json:'🧾', md:'📝',
    txt:'📄', sh:'🐚', yml:'⚙️', yaml:'⚙️', toml:'⚙️', mod:'🐹', sum:'🐹', png:'🖼', jpg:'🖼',
    svg:'🖼', gif:'🖼', lock:'🔒', env:'🔑', gitignore:'🙈', csv:'📊', sql:'🗄' };
  return map[ext] || '📄';
}
function fmtSize(n) {
  if (n == null) return '';
  if (n < 1024) return n + ' B';
  if (n < 1048576) return (n / 1024).toFixed(1) + ' K';
  return (n / 1048576).toFixed(1) + ' M';
}

async function openWbFile(path) {
  try {
    const r = await fetch('/api/wb/read?path=' + encodeURIComponent(path));
    const j = await r.json();
    if (!j.ok) { toast('⚠️ ' + escapeHtml(j.error || 'could not open')); return; }
    wbFile = path;
    wbDirty = false;
    $('#wb-editor').value = j.content;
    $('#wb-open-path').textContent = path;
    $('#wb-dirty').hidden = true;
    $('#btn-wb-del').hidden = false;
    $('#btn-wb-rename').hidden = false;
    updateWbStats();
    document.querySelectorAll('.wb-row.sel').forEach((n) => n.classList.remove('sel'));
    const row = [...document.querySelectorAll('.wb-row')].find((n) => n.dataset.path === path);
    if (row) row.classList.add('sel');
  } catch { toast('⚠️ could not open file'); }
}

function updateWbStats() {
  const v = $('#wb-editor').value;
  const lines = v ? v.split('\n').length : 0;
  $('#wb-stats').textContent = `${fmtSize(v.length)} · ${lines} lines`;
}

function markWbDirty() {
  if (!wbDirty) { wbDirty = true; $('#wb-dirty').hidden = false; }
  updateWbStats();
}

async function saveWbFile() {
  if (!wbFile) { toast('open a file first — pick one from the tree'); return; }
  try {
    const r = await fetch('/api/wb/write', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ path: wbFile, content: $('#wb-editor').value }),
    });
    const j = await r.json();
    if (j.ok) {
      wbDirty = false;
      $('#wb-dirty').hidden = true;
      toast(`💾 <b>${escapeHtml(wbFile)}</b> written to disk`);
    } else toast('⚠️ ' + escapeHtml(j.error || 'write failed'));
  } catch { toast('⚠️ write failed'); }
}

function wbUniqueName(base, siblings) {
  if (!siblings.has(base)) return base;
  const dot = base.lastIndexOf('.');
  const stem = dot > 0 ? base.slice(0, dot) : base;
  const ext = dot > 0 ? base.slice(dot) : '';
  for (let i = 2; ; i++) if (!siblings.has(stem + '-' + i + ext)) return stem + '-' + i + ext;
}

async function wbSiblingNames(dirPath) {
  try {
    const r = await fetch('/api/wb/tree?path=' + encodeURIComponent(dirPath));
    const j = await r.json();
    return new Set((j.entries || []).map((e) => e.name));
  } catch { return new Set(); }
}

$('#btn-wb-newfile').addEventListener('click', async () => {
  const dir = wbSelected && !wbSelected.includes('.') ? wbSelected : '';
  const names = await wbSiblingNames(dir);
  const name = wbUniqueName('new-file.txt', names);
  const path = dir ? dir + '/' + name : name;
  await fetch('/api/wb/write', { method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ path, content: '' }) });
  wbExpanded.add(dir);
  refreshWbTree();
  openWbFile(path);
  $('#wb-editor').focus();
  toast(`📄 <b>${escapeHtml(path)}</b> created — type away`);
});

$('#btn-wb-newdir').addEventListener('click', async () => {
  const dir = wbSelected && !wbSelected.includes('.') ? wbSelected : '';
  const names = await wbSiblingNames(dir);
  const name = wbUniqueName('new-dir', names);
  const path = dir ? dir + '/' + name : name;
  await fetch('/api/wb/mkdir', { method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ path }) });
  wbExpanded.add(dir);
  refreshWbTree();
  toast(`📁 <b>${escapeHtml(path)}</b> created`);
});

$('#btn-wb-refresh').addEventListener('click', refreshWbTree);
$('#btn-wb-close').addEventListener('click', closeWorkbench);
$('#wb-modal').addEventListener('click', (e) => { if (e.target.id === 'wb-modal') closeWorkbench(); });
$('#btn-wb-save').addEventListener('click', saveWbFile);
$('#wb-editor').addEventListener('input', markWbDirty);
$('#wb-editor').addEventListener('keydown', (e) => {
  if ((e.ctrlKey || e.metaKey) && e.key === 's') { e.preventDefault(); saveWbFile(); }
  if (e.key === 'Tab') { // tab inserts two spaces, never steals focus
    e.preventDefault();
    const ta = e.target, s0 = ta.selectionStart, s1 = ta.selectionEnd;
    ta.value = ta.value.slice(0, s0) + '  ' + ta.value.slice(s1);
    ta.selectionStart = ta.selectionEnd = s0 + 2;
    markWbDirty();
  }
  e.stopPropagation(); // don't fire global shortcuts while typing
});

$('#btn-wb-del').addEventListener('click', async () => {
  if (!wbFile || !confirm(`delete ${wbFile} from your project? This cannot be undone.`)) return;
  const r = await fetch('/api/wb/delete', { method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ path: wbFile }) });
  const j = await r.json();
  if (j.ok) {
    toast(`💥 <b>${escapeHtml(wbFile)}</b> deleted`);
    wbFile = null; wbDirty = false;
    $('#wb-editor').value = '';
    $('#wb-open-path').textContent = 'pick a file — the workbench edits your project in place';
    $('#wb-dirty').hidden = true;
    $('#btn-wb-del').hidden = true;
    $('#btn-wb-rename').hidden = true;
    refreshWbTree();
  } else toast('⚠️ ' + escapeHtml(j.error || 'delete failed'));
});

$('#btn-wb-rename').addEventListener('click', async () => {
  if (!wbFile) return;
  const to = prompt('rename / move to (project-relative path):', wbFile);
  if (!to || to === wbFile) return;
  const r = await fetch('/api/wb/rename', { method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ from: wbFile, to }) });
  const j = await r.json();
  if (j.ok) { toast(`✎ <b>${escapeHtml(wbFile)}</b> → <b>${escapeHtml(to)}</b>`); wbFile = to; refreshWbTree(); openWbFile(to); }
  else toast('⚠️ ' + escapeHtml(j.error || 'rename failed'));
});

function wbFileMenu(e) {
  wbSelected = e.path;
  if (confirm(`file: ${e.path}\n\nOK = open · Cancel = stay`)) openWbFile(e.path);
}
function wbDirMenu(e) {
  wbSelected = e.path;
  const act = prompt(`folder: ${e.path}\n\nType: open / collapse / newfile / newdir / delete`, 'open');
  if (!act) return;
  if (act === 'open') { if (!wbExpanded.has(e.path)) toggleWbDir([...document.querySelectorAll('.wb-row')].find((n) => n.dataset.path === e.path), e); }
  else if (act === 'collapse') { if (wbExpanded.has(e.path)) toggleWbDir([...document.querySelectorAll('.wb-row')].find((n) => n.dataset.path === e.path), e); }
  else if (act === 'newfile') $('#btn-wb-newfile').click();
  else if (act === 'newdir') $('#btn-wb-newdir').click();
  else if (act === 'delete') {
    if (!confirm(`delete folder ${e.path}? It must be empty.`)) return;
    fetch('/api/wb/delete', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ path: e.path }) })
      .then((r) => r.json()).then((j) => { if (j.ok) { toast(`💥 <b>${escapeHtml(e.path)}</b> deleted`); refreshWbTree(); } else toast('⚠️ ' + escapeHtml(j.error || 'delete failed')); });
  }
}

// the workbench watches the project live too: any fs:change under the root
// refreshes the open tree (unless the change came from our own save, which
// the editor already reflects)
let wbRefreshTimer = null;
function onWbFsChange(ch) {
  if (!wbOpenFlag) return;
  const src = (ch || {}).source;
  if (src === 'workbench') return; // our own writes are already on screen
  clearTimeout(wbRefreshTimer);
  wbRefreshTimer = setTimeout(refreshWbTree, 600); // coalesce bursts
}

$('#btn-workbench').addEventListener('click', openWorkbench);

/* ---- workbench project-wide search ---- */
let wbSearchTimer = null;
$('#wb-search').addEventListener('input', () => {
  clearTimeout(wbSearchTimer);
  wbSearchTimer = setTimeout(runWbSearch, 250);
});
$('#wb-search').addEventListener('keydown', (e) => {
  e.stopPropagation();
  if (e.key === 'Escape') { e.target.value = ''; $('#wb-results').hidden = true; $('#wb-results').innerHTML = ''; }
});

function wbSearchQuery() {
  const params = new URLSearchParams();
  params.set('q', $('#wb-search').value.trim());
  if ($('#wb-regex').getAttribute('aria-pressed') === 'true') params.set('regex', '1');
  if ($('#wb-case').getAttribute('aria-pressed') === 'true') params.set('case', '1');
  const ext = $('#wb-ext').value.trim();
  if (ext) params.set('ext', ext);
  const pathf = $('#wb-pathf').value.trim();
  if (pathf) params.set('path', pathf);
  params.set('limit', '100');
  return params;
}

// toggle buttons reflect state in the DOM so the request is always truthful
['#wb-regex', '#wb-case'].forEach((sel) => {
  const btn = $(sel);
  if (!btn) return;
  btn.addEventListener('click', () => {
    const on = btn.getAttribute('aria-pressed') !== 'true';
    btn.setAttribute('aria-pressed', String(on));
    btn.classList.toggle('on', on);
    runWbSearch();
  });
});
['#wb-ext', '#wb-pathf'].forEach((sel) => {
  const input = $(sel);
  if (!input) return;
  input.addEventListener('input', () => { clearTimeout(wbSearchTimer); wbSearchTimer = setTimeout(runWbSearch, 250); });
  input.addEventListener('keydown', (e) => e.stopPropagation());
});

async function runWbSearch() {
  const q = $('#wb-search').value.trim();
  const box = $('#wb-results');
  if (!q) { box.hidden = true; box.innerHTML = ''; return; }
  try {
    const r = await fetch('/api/wb/search?' + wbSearchQuery().toString());
    const j = await r.json();
    if (!j.ok) { box.hidden = false; box.innerHTML = '<div class="wb-empty">⚠️ ' + escapeHtml(j.error || 'search failed') + '</div>'; return; }
    box.hidden = false;
    if (!j.hits.length) { box.innerHTML = `<div class="wb-empty">no matches for "${escapeHtml(q)}" in ${j.files} files</div>`; return; }
    const head = `<div class="wb-res-head">${j.hits.length}${j.truncated ? '+' : ''} matches in ${j.files} files${j.truncated ? ' (showing the best matches)' : ''}</div>`;
    box.innerHTML = head + j.hits.map((h) =>
      `<button class="wb-res" data-path="${escapeHtml(h.path)}" data-line="${h.line}">` +
      `<span class="wb-res-path">${escapeHtml(h.path)}:${h.line}</span> — ${escapeHtml(h.text.trim().slice(0, 120))}</button>`).join('');
    box.querySelectorAll('.wb-res').forEach((b) => b.addEventListener('click', () => jumpWb(b.dataset.path, +b.dataset.line)));
  } catch { box.hidden = false; box.innerHTML = '<div class="wb-empty">⚠️ search failed</div>'; }
}

// open a file and put the caret on the matching line (and flash it)
async function jumpWb(path, line) {
  await openWbFile(path);
  const ta = $('#wb-editor');
  const lines = ta.value.split('\n');
  if (line < 1 || line > lines.length) return;
  let pos = 0;
  for (let i = 0; i < line - 1; i++) pos += lines[i].length + 1;
  ta.focus();
  ta.selectionStart = pos;
  ta.selectionEnd = pos + lines[line - 1].length;
  // scroll the caret into view
  const lh = parseFloat(getComputedStyle(ta).lineHeight) || 20;
  ta.scrollTop = Math.max(0, (line - 6) * lh);
  ta.style.background = 'var(--accent-soft)';
  setTimeout(() => { ta.style.background = ''; }, 500);
}
