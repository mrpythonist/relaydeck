async function api(path, opts) {
  const r = await fetch(path, opts);
  if (!r.ok) { const t = await r.text(); throw new Error(t || r.statusText); }
  return r.status === 204 ? null : r.json();
}
const $ = (s) => document.querySelector(s);
function badge(s){ return `<span class="badge ${s}">${s}</span>`; }
function esc(s){ return String(s ?? '').replace(/[&<>"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c])); }

async function loadOverview() {
  try {
    const st = await api('/api/v1/stats');
    $('#statCards').innerHTML = [
      ['Total events', st.total_events], ['Successful', st.successful_deliveries],
      ['Failed', st.failed_deliveries], ['Pending', st.pending_deliveries],
      ['Dead', st.dead_deliveries], ['Retries', st.retry_count],
      ['Success rate', (st.delivery_success_rate || 0).toFixed(1) + '%'],
    ].map(([l, v]) => `<div class="card"><div class="num">${v}</div><div class="lbl">${l}</div></div>`).join('');
    const evs = (await api('/api/v1/events?limit=10')).events;
    $('#recentEvents').innerHTML = evTable(evs);
  } catch (e) { $('#statCards').innerHTML = '<p>Failed to load stats: ' + esc(e.message) + '</p>'; }
}
function evTable(evs) {
  if (!evs.length) return '<p class="hint">No events yet. Send a webhook to POST /api/v1/webhooks/{source}.</p>';
  return '<table><tr><th>Event ID</th><th>Source</th><th>Content-Type</th><th>Created</th><th></th></tr>' +
    evs.map(e => `<tr><td><code>${esc(e.id)}</code></td><td>${esc(e.source)}</td><td>${esc(e.content_type)}</td><td>${esc(e.created_at)}</td><td><button class="secondary" onclick="showEvent('${esc(e.id)}')">Inspect</button></td></tr>`).join('') + '</table>';
}
async function showEvent(id) {
  const d = await api('/api/v1/events/' + encodeURIComponent(id));
  const ev = d.event;
  $('#eventDetail').innerHTML = `<h3>${esc(ev.id)}</h3>
    <p>Source: <code>${esc(ev.source)}</code> · ${esc(ev.created_at)}</p>
    <h4>Payload</h4><pre>${esc(ev.payload)}</pre>
    <h4>Headers (sensitive values redacted)</h4><pre>${esc(JSON.stringify(ev.headers, null, 2))}</pre>
    <h4>Deliveries</h4>` + (d.deliveries || []).map(x => `<p>${badge(x.status)} → ${esc(x.destination_id)} · attempts ${x.attempts} ${x.last_error ? '· ' + esc(x.last_error) : ''}</p>`).join('') +
    `<p><button onclick="replayEvent('${esc(ev.id)}')">Replay event</button></p>`;
  $('#eventDetail').scrollIntoView();
}
async function replayEvent(id) {
  const r = await api('/api/v1/events/' + encodeURIComponent(id) + '/replay', { method: 'POST' });
  alert('Replay queued: ' + r.deliveries_created + ' deliveries created.');
  loadAll();
}
async function loadEvents() {
  const q = new URLSearchParams({ limit: 50 });
  const src = $('#filterSource').value.trim(), st = $('#filterStatus').value, s = $('#searchEvent').value.trim();
  if (src) q.set('source', src); if (st) q.set('status', st); if (s) q.set('search', s);
  const evs = (await api('/api/v1/events?' + q)).events;
  $('#eventsTable').innerHTML = evTable(evs);
}
function delTable(dels, withRetry) {
  if (!dels.length) return '<p class="hint">No deliveries.</p>';
  return '<table><tr><th>ID</th><th>Event</th><th>Destination</th><th>Status</th><th>Attempts</th><th>Last error</th><th></th></tr>' +
    dels.map(d => `<tr><td><code>${esc(d.id)}</code></td><td><code>${esc(d.event_id)}</code></td><td>${esc(d.destination_name || d.destination_id)}</td><td>${badge(d.status)}</td><td>${d.attempts}</td><td>${esc(d.last_error || '')}</td><td>${withRetry && (d.status === 'dead' || d.status === 'failed' || d.status === 'pending') ? `<button class="secondary" onclick="retryDelivery('${esc(d.id)}')">Retry</button>` : ''}</td></tr>`).join('') + '</table>';
}
async function retryDelivery(id) {
  await api('/api/v1/deliveries/' + encodeURIComponent(id) + '/retry', { method: 'POST' });
  loadAll();
}
async function loadDeliveries() {
  const dels = (await api('/api/v1/deliveries?limit=100')).deliveries;
  $('#deliveriesTable').innerHTML = delTable(dels, true);
  const failed = dels.filter(d => d.status === 'dead' || d.status === 'failed');
  $('#failedTable').innerHTML = delTable(failed, true);
}
async function loadDestinations() {
  const dests = (await api('/api/v1/destinations')).destinations;
  $('#destinationsList').innerHTML = dests.length ? '<table><tr><th>Name</th><th>URL</th><th>Source</th><th>Method</th><th>Retries</th><th>Enabled</th><th></th></tr>' +
    dests.map(d => `<tr><td>${esc(d.name)}</td><td><code>${esc(d.url)}</code></td><td>${esc(d.source_filter || 'all')}</td><td>${esc(d.method)}</td><td>${d.max_retries}</td><td>${d.enabled ? 'yes' : 'no'}</td><td><button class="secondary" onclick="toggleDest('${esc(d.id)}', ${!d.enabled}, ${JSON.stringify(d).replace(/"/g, '&quot;')})">${d.enabled ? 'Disable' : 'Enable'}</button></td></tr>`).join('') + '</table>'
    : '<p class="hint">No destinations yet — add one below.</p>';
}
async function toggleDest(id, enabled, d) {
  const body = { name: d.name, url: d.url, source_filter: d.source_filter, method: d.method, max_retries: d.max_retries, timeout_seconds: d.timeout_seconds, backoff_base_ms: d.backoff_base_ms, backoff_max_ms: d.backoff_max_ms, filter_substring: d.filter_substring, headers: d.headers, enabled };
  await api('/api/v1/destinations/' + encodeURIComponent(id), { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  loadDestinations();
}
async function loadSources() {
  const srcs = (await api('/api/v1/sources')).sources;
  $('#sourcesList').innerHTML = srcs.length ? '<ul>' + srcs.map(s => `<li><code>${esc(s)}</code> — <code>POST /api/v1/webhooks/${esc(s)}</code></li>`).join('') + '</ul>' : '<p class="hint">No sources registered yet.</p>';
  $('#settingsBox').innerHTML = '<p>API authentication: ' + (await api('/health').then(() => 'server reachable')) + '. Configure listen address, API key, workers and retention in <code>relaydeck.yaml</code> or <code>RELAYDECK_*</code> environment variables. See README for the full reference.</p><p>Delivery semantics: <strong>at-least-once</strong>. Events are stored durably before acknowledgement; retries use exponential backoff with jitter.</p>';
}
function loadAll() { loadOverview(); loadEvents(); loadDeliveries(); loadDestinations(); loadSources(); }
document.querySelectorAll('#tabs button').forEach(b => b.addEventListener('click', () => {
  document.querySelectorAll('#tabs button').forEach(x => x.classList.remove('active'));
  document.querySelectorAll('.tab').forEach(x => x.classList.remove('active'));
  b.classList.add('active'); $('#tab-' + b.dataset.tab).classList.add('active');
}));
$('#themeBtn').addEventListener('click', () => {
  const cur = document.documentElement.getAttribute('data-theme');
  document.documentElement.setAttribute('data-theme', cur === 'dark' ? 'light' : 'dark');
});
$('#refreshBtn').addEventListener('click', loadAll);
$('#applyFilters').addEventListener('click', loadEvents);
$('#destForm').addEventListener('submit', async (e) => {
  e.preventDefault();
  const fd = new FormData(e.target);
  const body = { name: fd.get('name'), url: fd.get('url'), source_filter: fd.get('source_filter') || '', method: fd.get('method') || 'POST', max_retries: parseInt(fd.get('max_retries') || '5', 10), filter_substring: fd.get('filter_substring') || '' };
  try { await api('/api/v1/destinations', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }); e.target.reset(); loadDestinations(); }
  catch (err) { alert('Failed to add destination: ' + err.message); }
});
loadAll();
