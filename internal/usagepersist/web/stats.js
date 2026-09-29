/* Embedded statistics dashboard. All server-controlled values use textContent. */
(function () {
  'use strict';
  const nodeModule = typeof module !== 'undefined' && module.exports;
  const C = nodeModule ? require('./stats-core.js') : window.CPAStats;
  const unavailableNotice = 'Usage persistence is unavailable. Check persistence storage and server logs. The original management page and manual quota refresh remain available.';
  function statusNotice(status) {
    const notices = [], unavailable = status?.available === false;
    if (unavailable) notices.push(unavailableNotice);
    if (status?.collection_enabled === false) notices.push(`Built-in usage collection is off. New request usage will not be recorded. To resume, enable the existing usage-statistics-enabled setting (v8: observability.usage.usage-statistics-enabled).${unavailable ? '' : ' Previously stored history, pricing, and quota remain available.'}`);
    const failed = C.number(status?.write_failures) > 0;
    if (failed) notices.push(`Warning: ${C.integer(status.write_failures)} persistence writes failed since startup. Historical data may have gaps. Last failure: ${C.date(status.last_write_failure_at)}. Inspect server logs.`);
    const dropped = C.number(status?.dropped_events) > 0;
    if (dropped) notices.push(`Warning: ${C.integer(status.dropped_events)} usage/quota records were dropped before persistence since startup (${C.integer(status.queue_overflows)} queue overflows; ${C.integer(status.validation_failures)} validation failures). Historical data may have gaps. Last drop: ${C.date(status.last_drop_at)}. Inspect server logs.`);
    if (C.number(status?.pending_events) > 0) notices.push(`${C.integer(status.pending_events)} admitted records are pending persistence. Latest statistics may lag until they are written.${C.number(status.queue_capacity) > 0 ? ` Queue capacity: ${C.integer(status.queue_capacity)}, plus one in-flight record.` : ''}`);
    const capacity = status?.local_capacity;
    const capacityReached = !!capacity && (C.number(capacity.rejected_writes) > 0 || C.number(capacity.journal_byte_limit) > 0 && C.number(capacity.journal_bytes) >= C.number(capacity.journal_byte_limit) || C.number(capacity.retained_event_limit) > 0 && C.number(capacity.retained_events) >= C.number(capacity.retained_event_limit));
    if (capacityReached) notices.push(`Warning: local persistence reached a supported capacity boundary (${C.integer(capacity.retained_events)} / ${C.integer(capacity.retained_event_limit)} events; ${C.integer(capacity.journal_bytes)} / ${C.integer(capacity.journal_byte_limit)} journal bytes). Existing history remains readable; records that do not fit are rejected, never silently pruned. Plan a backed-up move to CPA's existing PostgreSQL storage for larger history.`);
    if (C.number(status?.skipped_quota_headers) > 0) notices.push(`${C.integer(status.skipped_quota_headers)} quota-header observations were not attributed because credential/account continuity could not be proven. Usage accounting is independent; verified manual quota refreshes remain available.`);
    return {text: notices.join('\n\n'), error: failed || dropped || unavailable || capacityReached};
  }
  function httpErrorMessage(status) {
    return status === 503 ? unavailableNotice : status === 404 ? 'Statistics API is unavailable. Check the server version and management route; usage collection uses the existing usage-statistics-enabled setting.' : status === 429 ? 'Too many requests. Wait a moment, then retry.' : `The server could not complete this request (HTTP ${status}). Try again or inspect the server logs.`;
  }
  if (nodeModule) {module.exports = {statusNotice, httpErrorMessage}; return;}
  const $ = id => document.getElementById(id);
  const sessionKey = 'cpa-stats-management-key';
  const apiBase = new URL('./v0/management/stats/', location.href);
  const state = {key: '', tab: 'overview', chart: 'requests', paused: false, offset: 0, limit: 50, total: 0, data: null, prices: [], connected: false, stopped: false, busy: false, epoch: 0, requests: new Set(), timer: 0, failures: 0};
  const storage = {get(store, key) {try {return store.getItem(key);} catch {return null;}}, set(store, key, value) {try {if (value == null) store.removeItem(key); else store.setItem(key, value);} catch { /* Storage is optional. */ }}};
  state.key = storage.get(sessionStorage, sessionKey) || '';
  $('remember-key').checked = Boolean(state.key);
  let theme = storage.get(localStorage, 'cpa-stats-theme') || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
  document.documentElement.dataset.theme = theme;
  function node(tag, text, className) {const element = document.createElement(tag); if (text != null) element.textContent = String(text); if (className) element.className = className; return element;}
  function message(id, text, error = false) {const target = $(id); target.textContent = text; target.hidden = !text; target.classList.toggle('error', error);}
  function connection(text, status = 'ready') {$('connection').dataset.status = status; $('connection').lastElementChild.textContent = text;}
  function invalidate() {state.epoch++; for (const controller of state.requests) controller.abort(); state.requests.clear(); state.busy = false; clearTimeout(state.timer);}
  function clearData() {state.data = null; state.prices = []; state.total = 0; state.offset = 0; message('persistence-warning', ''); renderSummary(null); renderOverview({}); renderAnalysis({}); renderEvents({events: [], total: 0}); renderPrices(); $('updated').textContent = 'No authenticated snapshot';}
  function authRequired() {state.connected = false; state.stopped = true; state.key = ''; storage.set(sessionStorage, sessionKey, null); invalidate(); clearData(); $('refresh').disabled = false; connection('Authentication required', 'error'); message('banner', 'Connect with a valid management key to view statistics.', true); openAuth();}
  async function api(path, options = {}) {
    const controller = new AbortController(); state.requests.add(controller);
    const headers = new Headers({'Accept': 'application/json'});
    if (state.key) headers.set('Authorization', `Bearer ${state.key}`);
    if (options.body !== undefined) headers.set('Content-Type', 'application/json');
    let response;
    try {
      response = await fetch(new URL(path, apiBase), {method: options.method || 'GET', headers, credentials: 'same-origin', cache: 'no-store', redirect: 'error', referrerPolicy: 'no-referrer', signal: controller.signal, ...(options.body !== undefined ? {body: JSON.stringify(options.body)} : {})});
      if (response.status === 401 || response.status === 403) {authRequired(); throw new Error('Management authentication failed. Check your key and remote-management permissions.');}
      if (!response.ok) throw new Error(httpErrorMessage(response.status));
      if (response.status === 204) return {};
      const type = response.headers.get('content-type') || '';
      if (!type.includes('json')) throw new Error('The server returned an unexpected response. Check the proxy route and server version.');
      const result = await response.json();
      if (!result || typeof result !== 'object') throw new Error('The server returned an invalid statistics response.');
      return result;
    } catch (error) {
      if (error.name === 'AbortError') throw error;
      if (error instanceof SyntaxError) throw new Error('The server returned malformed JSON. Check the proxy route and server logs.');
      if (error instanceof TypeError) throw new Error(navigator.onLine === false ? 'You are offline. The last successful snapshot is kept on screen.' : 'Cannot reach the proxy. Check your network and server, then retry.');
      throw error;
    } finally {state.requests.delete(controller);}
  }
  function filters() {return {range: $('range').value, from: $('from').value, to: $('to').value, model: $('model').value, provider: $('provider').value, auth_index: $('auth-index').value, key_id: $('key-id').value, status: $('status').value};}
  function empty(target, title, description) {target.replaceChildren(); const box = node('div', null, 'empty'); box.append(node('strong', title), node('span', description)); target.append(box);}
  function emptyTable(id, columns, description) {const row = node('tr'); const cell = node('td', description, 'empty'); cell.colSpan = columns; row.append(cell); $(id).replaceChildren(row);}
  function renderSummary(summary) {
    const s = summary;
    $('metric-requests').textContent = s ? C.compact(s.requests) : '—';
    $('metric-results').textContent = s ? `${C.integer(s.successes)} successful · ${C.integer(s.failures)} failed` : 'No snapshot yet';
    $('metric-tokens').textContent = s ? C.compact(s.total_tokens) : '—';
    $('metric-token-detail').textContent = s ? `${C.compact(s.input_tokens)} input · ${C.compact(s.output_tokens)} output` : 'Input + output + reported extras';
    $('metric-success').textContent = s ? C.percent(s.successes, s.requests) : '—';
    $('metric-cost').textContent = s && C.number(s.priced_requests) ? C.money(s.cost_usd) : '—';
    $('metric-priced').textContent = s ? `${C.integer(s.priced_requests)} priced · ${C.integer(s.unpriced_requests)} unpriced` : 'Pricing coverage unavailable';
    $('metric-latency').textContent = s && C.number(s.requests) ? C.duration(s.average_latency_ms) : '—';
    $('metric-cache').textContent = s ? C.compact(s.cache_read_tokens) : '—';
    $('metric-cache-detail').textContent = s ? `${C.compact(s.cache_write_tokens)} cache write · ${C.compact(s.reasoning_tokens)} reasoning` : 'Reported by upstream providers';
  }
  function renderChart(data) {
    const target = $('traffic-chart'), rows = C.arrays(data, 'series');
    if (!rows.length || !C.number(data.summary?.requests)) {empty(target, 'No traffic in this range', 'Send a request through the proxy or choose another time range.'); $('chart-caption').replaceChildren(); return;}
    const width = Math.max(320, Math.min(1100, target.clientWidth || 740));
    const plot = C.seriesPoints(rows, state.chart, width), svgNS = 'http://www.w3.org/2000/svg';
    function shape(tag, attrs, text) {const element = document.createElementNS(svgNS, tag); for (const [key, value] of Object.entries(attrs)) element.setAttribute(key, String(value)); if (text != null) element.textContent = text; return element;}
    const svg = shape('svg', {viewBox: `0 0 ${width} 220`, role: 'img', 'aria-label': `${state.chart.replaceAll('_', ' ')} over the selected time range`});
    svg.append(shape('title', {}, 'Traffic over time'), shape('desc', {}, 'Each point represents one server aggregation bucket. Hover or focus a point for its value.'));
    for (let i = 0; i <= 4; i++) {const y = plot.top + (plot.bottom - plot.top) * i / 4; svg.append(shape('line', {x1: plot.left, x2: plot.right, y1: y, y2: y, class: 'chart-grid'}), shape('text', {x: plot.left - 10, y: y + 4, 'text-anchor': 'end', class: 'chart-axis'}, state.chart === 'cost_usd' ? C.money(plot.max * (1 - i / 4)) : C.compact(plot.max * (1 - i / 4))));}
    const path = plot.points.map((p, i) => `${i ? 'L' : 'M'}${p.x.toFixed(2)} ${p.y.toFixed(2)}`).join(' ');
    svg.append(shape('path', {d: `${path} L${plot.points.at(-1).x} ${plot.bottom} L${plot.points[0].x} ${plot.bottom} Z`, class: 'chart-area'}), shape('path', {d: path, class: 'chart-line'}));
    for (const p of plot.points) {const point = shape('circle', {cx: p.x, cy: p.y, r: plot.points.length > 70 ? 1.5 : 3, class: 'chart-point', tabindex: plot.points.length <= 48 ? 0 : -1}); point.append(shape('title', {}, `${C.date(p.time)} · ${state.chart === 'cost_usd' ? C.money(p.value) : C.integer(p.value)}`)); svg.append(point);}
    target.replaceChildren(svg);
    $('chart-caption').replaceChildren(node('span', C.date(rows[0].time)), node('span', `${rows.length} buckets${plot.sampled ? ' · sampled' : ''} · local time`), node('span', C.date(rows.at(-1).time)));
  }
  const colors = ['var(--accent)', 'var(--blue)', 'var(--amber)', 'var(--purple)', 'var(--muted)'];
  function shareList(id, rows, count) {
    const target = $(id); target.replaceChildren();
    const sorted = [...rows].sort((a, b) => C.number(b.requests) - C.number(a.requests));
    if (!sorted.length) {empty(target, 'Nothing to compare yet', 'Recorded requests will appear here.'); return;}
    const total = sorted.reduce((sum, row) => sum + C.number(row.requests), 0);
    sorted.slice(0, count).forEach((row, i) => {const line = node('div', null, 'share-row'), text = node('div'), name = node('div', row.name || 'Unknown', 'share-name'), track = node('div', null, 'share-track'), fill = node('div', null, 'share-fill'); name.title = row.name || 'Unknown'; fill.style.width = `${total ? C.number(row.requests) / total * 100 : 0}%`; fill.style.background = colors[i % colors.length]; track.append(fill); text.append(name, track); const value = node('div', C.integer(row.requests), 'share-value'); value.append(node('div', C.percent(row.requests, total), 'share-detail')); line.append(node('span', String(i + 1).padStart(2, '0'), 'share-rank'), text, value); target.append(line);});
  }
  function renderComposition(summary = {}) {
    const target = $('token-composition'), parts = [['Uncached input', 'uncached_input'], ['Output', 'output_tokens'], ['Cache read', 'cache_read_tokens'], ['Cache write', 'cache_write_tokens']];
    summary = {...summary, uncached_input: Math.max(0, C.number(summary.input_tokens) - C.number(summary.cache_read_tokens) - C.number(summary.cache_write_tokens))};
    const total = parts.reduce((sum, [, key]) => sum + C.number(summary[key]), 0), bar = node('div', null, 'composition-bar'), legend = node('div', null, 'composition-legend');
    parts.forEach(([label, key], i) => {const value = C.number(summary[key]), segment = node('span', null, 'composition-segment'); segment.style.width = `${total ? value / total * 100 : 0}%`; segment.style.background = colors[i]; segment.title = `${label}: ${C.integer(value)}`; if (value) bar.append(segment); const item = node('div', null, 'legend-item'), swatch = node('i'); swatch.style.background = colors[i]; item.append(swatch, node('span', label), node('strong', C.compact(value))); legend.append(item);});
    target.replaceChildren(bar, legend);
  }
  function renderOverview(data) {renderChart(data); shareList('provider-mix', C.arrays(data, 'providers'), 6); shareList('model-leaders', C.arrays(data, 'models'), 5); renderComposition(data.summary);}
  function cells(row, values) {for (const value of values) {const cell = node('td'); if (value instanceof Node) cell.append(value); else cell.textContent = String(value ?? '—'); row.append(cell);}}
  function cost(row) {return C.number(row.priced_requests) ? `${C.money(row.cost_usd)}${C.number(row.unpriced_requests) ? ' *' : ''}` : 'Unpriced';}
  function renderAnalysis(data) {
    const models = C.arrays(data, 'models').sort((a, b) => C.number(b.requests) - C.number(a.requests));
    $('models-table').replaceChildren();
    if (!models.length) emptyTable('models-table', 9, 'No model usage matches these filters.');
    for (const model of models) {const row = node('tr'); cells(row, [model.name, C.integer(model.requests), C.percent(model.successes, model.requests), C.compact(model.input_tokens), C.compact(model.output_tokens), C.compact(model.cache_read_tokens), C.compact(model.total_tokens), C.duration(model.average_latency_ms), cost(model)]); $('models-table').append(row);}
    $('providers-table').replaceChildren();
    const providers = C.arrays(data, 'providers');
    if (!providers.length) emptyTable('providers-table', 6, 'No provider usage matches these filters.');
    for (const provider of providers) {const row = node('tr'); cells(row, [provider.name, C.integer(provider.requests), C.integer(provider.failures), C.compact(provider.total_tokens), C.duration(provider.average_latency_ms), cost(provider)]); $('providers-table').append(row);}
  }
  function renderEvents(data) {
    const events = C.arrays(data, 'events'); state.total = C.number(data.total); $('events-table').replaceChildren();
    if (!events.length) emptyTable('events-table', 8, 'No completed requests match these filters. Live updates will appear automatically.');
    for (const event of events) {const row = node('tr'), model = node('span', event.model || 'Unknown'); model.append(node('small', `${event.provider || 'Unknown'}${event.stream ? ' · stream' : ''}`)); const badge = node('span', event.failed ? `Failed${event.status_code ? ` · ${event.status_code}` : ''}` : 'Success', `badge${event.failed ? ' error' : ''}`); cells(row, [C.date(event.requested_at), model, badge, C.compact(event.total_tokens), C.duration(event.latency_ms), event.ttft_ms > 0 ? C.duration(event.ttft_ms) : '—', event.priced ? C.money(event.cost_usd) : 'Unpriced', event.auth_index || '—']); $('events-table').append(row);}
    $('events-count').textContent = events.length ? `${C.integer(state.offset + 1)}–${C.integer(state.offset + events.length)} of ${C.integer(state.total)} requests${state.offset ? ' · older-page updates paused' : ''}` : '0 requests';
    $('events-prev').disabled = state.offset === 0; $('events-next').disabled = state.offset + state.limit >= state.total;
  }
  function schedule() {clearTimeout(state.timer); if (!state.stopped) state.timer = setTimeout(() => {if (!document.hidden && navigator.onLine !== false && !(state.tab === 'realtime' && (state.paused || state.offset > 0))) void refresh(false); else schedule();}, Math.min(60000, (state.tab === 'realtime' ? 5000 : 15000) * Math.pow(2, Math.min(3, state.failures))));}
  async function loadFilters(epoch) {
    try {const params = C.query(filters()); for (const key of ['model', 'provider', 'auth_index', 'key_id', 'status', 'bucket']) params.delete(key); const result = await api(`filters?${params}`); if (epoch !== state.epoch) return; for (const [id, key, label] of [['model', 'models', 'All models'], ['provider', 'providers', 'All providers'], ['auth-index', 'auth_indexes', 'All credentials'], ['key-id', 'key_ids', 'All key IDs']]) {const select = $(id), selected = select.value; const values = [...new Set((Array.isArray(result[key]) ? result[key] : []).map(String))].sort(); select.replaceChildren(new Option(label, '')); if (selected && !values.includes(selected)) values.unshift(selected); for (const value of values) select.add(new Option(value, value)); select.value = selected;}} catch (error) {if (error.name !== 'AbortError' && epoch === state.epoch && state.connected) message('banner', 'Statistics loaded, but filter options could not refresh. Your current filters are retained.', true);}
  }
  async function refresh(force = true) {
    if (state.mutating) return;
    if (state.busy) {if (!force) return; invalidate();}
    if (state.stopped && !force) return;
    state.stopped = false; const epoch = state.epoch, tab = state.tab; let params;
    try {params = C.query(filters());} catch (error) {message('banner', error.message, true); return;}
    state.busy = true; $('refresh').disabled = true; connection('Refreshing', 'loading');
    try {
      if (tab === 'pricing') {const response = await api('pricing'); if (epoch !== state.epoch) return; state.prices = C.arrays(response, 'prices'); renderPrices();}
      else {const response = await api(`${tab === 'analysis' ? 'analysis' : 'overview'}?${params}`); if (epoch !== state.epoch) return; state.data = response; renderSummary(response.summary); renderOverview(response); renderAnalysis(response);
        if (tab === 'realtime') {params.set('limit', state.limit); params.set('offset', state.offset); try {const events = await api(`events?${params}`); if (epoch !== state.epoch) return; renderEvents(events); message('live-error', '');} catch (error) {if (error.name === 'AbortError' || epoch !== state.epoch) return; message('live-error', error.message, true); throw error;}}
      }
      if (epoch !== state.epoch) return;
      const firstConnection = !state.connected; state.connected = true; state.failures = 0; connection('Connected'); $('updated').textContent = `Updated ${new Date().toLocaleTimeString()}`; message('banner', ''); message('auth-error', ''); if ($('auth-dialog').open) $('auth-dialog').close(); $('management-key').value = ''; if (firstConnection || force) void loadFilters(epoch);
      void api('status').then(status => {if (epoch === state.epoch) {const notice = statusNotice(status); message('persistence-warning', notice.text, notice.error);}}).catch(error => {if (epoch === state.epoch && error.name !== 'AbortError') message('persistence-warning', `Usage persistence status could not be checked. ${error.message} Statistics may be incomplete.`, true);});
    } catch (error) {if (error.name !== 'AbortError' && epoch === state.epoch) {state.failures++; connection(navigator.onLine === false ? 'Offline' : 'Update failed', 'error'); message('banner', `${error.message}${state.data ? ' Showing the last successful snapshot.' : ''}`, true); if ($('auth-dialog').open) message('auth-error', error.message, true);}}
    finally {if (epoch === state.epoch) {state.busy = false; $('refresh').disabled = false; schedule();}}
  }
  function selectTab(tab) {if (!['overview', 'analysis', 'realtime', 'pricing'].includes(tab)) return; invalidate(); state.tab = tab; state.offset = 0; for (const button of document.querySelectorAll('[data-tab]')) {if (button.dataset.tab === tab) button.setAttribute('aria-current', 'page'); else button.removeAttribute('aria-current');} for (const name of ['overview', 'analysis', 'realtime', 'pricing']) $(`panel-${name}`).hidden = tab !== name; $('statistics-content').hidden = tab === 'pricing'; $('filters').hidden = tab === 'pricing'; history.replaceState(null, '', `#${tab}`); void refresh();}
  function renderPrices() {
    const query = $('price-search').value.trim().toLowerCase(), rows = state.prices.filter(price => String(price.model || '').toLowerCase().includes(query)).sort((a, b) => String(a.model).localeCompare(String(b.model)));
    $('prices-table').replaceChildren(); if (!rows.length) emptyTable('prices-table', 7, query ? 'No rates match your search.' : 'No rates yet. Sync the catalog or add a manual override.');
    for (const price of rows.slice(0, 200)) {const row = node('tr'), badge = node('span', price.manual ? 'Manual override' : price.source || 'Catalog', `badge${price.manual ? '' : ' neutral'}`), actions = node('div', null, 'row-actions'); const edit = node('button', 'Edit', 'text-button'); edit.type = 'button'; edit.addEventListener('click', () => editPrice(price)); actions.append(edit); if (price.manual) {const reset = node('button', 'Remove override', 'text-button'); reset.type = 'button'; reset.addEventListener('click', () => void removePrice(price.model, reset)); actions.append(reset);} cells(row, [price.model, C.money(price.input_per_million), C.money(price.output_per_million), price.cache_read_available === false ? 'Unknown' : C.money(price.cache_read_per_million), price.cache_write_available === false ? 'Unknown' : C.money(price.cache_write_per_million), badge, actions]); $('prices-table').append(row);}
    $('prices-count').textContent = `${rows.length > 200 ? `Showing first 200 of ${C.integer(rows.length)} matches — narrow your search. ` : `${C.integer(rows.length)} matches. `}${C.integer(state.prices.length)} total model rates · USD per million tokens · * partial pricing coverage in analysis`;
  }
  const priceFields = [['price-model', 'model'], ['price-input', 'input_per_million'], ['price-output', 'output_per_million'], ['price-cache-read', 'cache_read_per_million'], ['price-cache-write', 'cache_write_per_million']];
  function editPrice(price) {for (const [id, key] of priceFields) $(id).value = price[key] ?? ''; $('price-model').focus(); $('price-editor').scrollIntoView({block: 'center', behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth'});}
  async function priceAction(button, action) {if (state.mutating) return; state.mutating = true; clearTimeout(state.timer); button.disabled = true; message('pricing-message', ''); const epoch = state.epoch; try {await action(); state.mutating = false; if (epoch === state.epoch) await refresh();} catch (error) {if (error.name !== 'AbortError' && epoch === state.epoch) message('pricing-message', error.message, true);} finally {state.mutating = false; button.disabled = false; schedule();}}
  async function removePrice(model, button) {if (!confirm(`Remove the manual override for ${model}? A stored catalog rate will become active when available.`)) return; await priceAction(button, async () => {await api(`pricing?${new URLSearchParams({model})}`, {method: 'DELETE'}); message('pricing-message', 'Override removed. Any stored catalog price is now active.');});}
  function openAuth() {$('management-key').value = ''; if (!$('auth-dialog').open) $('auth-dialog').showModal(); $('management-key').focus();}
  $('theme').addEventListener('click', () => {theme = theme === 'dark' ? 'light' : 'dark'; document.documentElement.dataset.theme = theme; storage.set(localStorage, 'cpa-stats-theme', theme);});
  $('refresh').addEventListener('click', () => void refresh());
  for (const button of document.querySelectorAll('[data-tab]')) button.addEventListener('click', () => selectTab(button.dataset.tab));
  for (const button of document.querySelectorAll('[data-chart]')) button.addEventListener('click', () => {state.chart = button.dataset.chart; for (const other of document.querySelectorAll('[data-chart]')) other.setAttribute('aria-pressed', String(other === button)); renderChart(state.data || {});});
  $('see-analysis').addEventListener('click', () => selectTab('analysis'));
  $('filters').addEventListener('submit', event => event.preventDefault());
  $('filters').addEventListener('change', () => {const custom = $('range').value === 'custom'; $('from-label').hidden = !custom; $('to-label').hidden = !custom; state.offset = 0; invalidate(); void refresh();});
  $('reset-filters').addEventListener('click', () => {$('filters').reset(); $('from-label').hidden = true; $('to-label').hidden = true; state.offset = 0; invalidate(); void refresh();});
  $('pause-live').addEventListener('click', () => {state.paused = !state.paused; $('pause-live').setAttribute('aria-pressed', String(state.paused)); $('pause-live').textContent = state.paused ? 'Resume updates' : 'Pause updates'; if (!state.paused) {state.offset = 0; void refresh();}});
  $('events-prev').addEventListener('click', () => {state.offset = Math.max(0, state.offset - state.limit); invalidate(); void refresh();});
  $('events-next').addEventListener('click', () => {state.offset += state.limit; invalidate(); void refresh();});
  $('price-search').addEventListener('input', renderPrices);
  $('clear-price').addEventListener('click', () => $('price-editor').reset());
  $('price-editor').addEventListener('submit', event => {event.preventDefault(); let price; try {price = C.price(Object.fromEntries(priceFields.map(([id, key]) => [key, $(id).value])));} catch (error) {message('pricing-message', error.message, true); return;} void priceAction($('save-price'), async () => {await api('pricing', {method: 'PUT', body: price}); message('pricing-message', 'Manual override saved on the server.'); $('price-editor').reset();});});
  $('sync-pricing').addEventListener('click', () => void priceAction($('sync-pricing'), async () => {$('sync-result').textContent = 'Synchronizing…'; try {const result = await api('pricing/sync', {method: 'POST', body: {}}); $('sync-result').textContent = `${C.integer(result.updated)} rates updated · ${C.integer(result.skipped_manual)} manual overrides preserved · ${C.date(result.synced_at)}`;} catch (error) {$('sync-result').textContent = 'Sync failed. Existing rates are unchanged unless the server reports otherwise.'; throw error;}}));
  $('auth-open').addEventListener('click', openAuth); $('auth-close').addEventListener('click', () => $('auth-dialog').close());
  $('auth-form').addEventListener('submit', event => {event.preventDefault(); invalidate(); clearData(); state.key = $('management-key').value.trim(); state.connected = false; state.stopped = false; storage.set(sessionStorage, sessionKey, $('remember-key').checked && state.key ? state.key : null); message('auth-error', ''); void refresh();});
  $('auth-forget').addEventListener('click', () => {invalidate(); state.key = ''; state.connected = false; state.stopped = true; storage.set(sessionStorage, sessionKey, null); $('management-key').value = ''; $('remember-key').checked = false; clearData(); connection('Disconnected', 'error'); message('auth-error', 'Key forgotten. Existing server-side cookies, if any, are managed by the server.'); message('banner', 'Disconnected. Connect again to load statistics.'); $('refresh').disabled = false;});
  $('export-models').addEventListener('click', () => {const columns = ['name', 'requests', 'successes', 'failures', 'input_tokens', 'output_tokens', 'cache_read_tokens', 'total_tokens', 'average_latency_ms', 'cost_usd', 'priced_requests', 'unpriced_requests']; const lines = [columns, ...C.arrays(state.data, 'models').map(row => columns.map(key => row[key]))]; const blob = new Blob(['\uFEFF', lines.map(row => row.map(C.csvCell).join(',')).join('\r\n')], {type: 'text/csv;charset=utf-8'}), url = URL.createObjectURL(blob), anchor = node('a'); anchor.href = url; anchor.download = 'cpa-model-statistics.csv'; anchor.click(); setTimeout(() => URL.revokeObjectURL(url), 1000);});
  let resizeFrame = 0;
  window.addEventListener('resize', () => {cancelAnimationFrame(resizeFrame); resizeFrame = requestAnimationFrame(() => {if (state.tab === 'overview') renderChart(state.data || {});});});
  window.addEventListener('offline', () => {connection('Offline', 'error'); message('banner', 'You are offline. The last successful snapshot remains visible.', true);});
  window.addEventListener('online', () => {if (!state.stopped) void refresh();});
  document.addEventListener('visibilitychange', () => {if (!document.hidden && !state.stopped && !(state.tab === 'realtime' && (state.paused || state.offset))) void refresh(false);});
  window.addEventListener('pagehide', () => {state.stopped = true; invalidate();});
  window.addEventListener('pageshow', event => {if (event.persisted) {state.stopped = false; void refresh();}});
  const initialTab = location.hash.slice(1); if (['overview', 'analysis', 'realtime', 'pricing'].includes(initialTab)) selectTab(initialTab); else void refresh();
})();
