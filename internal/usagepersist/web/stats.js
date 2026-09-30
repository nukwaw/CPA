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
    return {text: notices.join('\n\n'), error: failed || dropped || unavailable || capacityReached};
  }
  // A management handler usually explains its own failure. Prefer that explanation
  // (for example "no quota provider available for credential") to a bare status.
  async function serverError(response) {
    try {
      const body = await response.json();
      const detail = typeof body?.error === 'string' ? body.error.trim() : typeof body?.message === 'string' ? body.message.trim() : '';
      if (detail && detail.length <= 300) return detail;
    } catch { /* Non-JSON error bodies fall back to the status text. */ }
    return httpErrorMessage(response.status);
  }
  function httpErrorMessage(status) {
    return status === 503 ? unavailableNotice : status === 404 ? 'Statistics API is unavailable. Check the server version and management route; usage collection uses the existing usage-statistics-enabled setting.' : status === 429 ? 'Too many requests. Wait a moment, then retry.' : `The server could not complete this request (HTTP ${status}). Try again or inspect the server logs.`;
  }
  if (nodeModule) {module.exports = {statusNotice, httpErrorMessage}; return;}
  const $ = id => document.getElementById(id);
  const sessionKey = 'cpa-stats-management-key';
  const apiBase = new URL('./v0/management/stats/', location.href);
  const state = {key: '', tab: 'overview', chart: 'requests', paused: false, offset: 0, limit: 50, total: 0, data: null, prices: [], quota: null, credentials: [], dialog: null, dialogTab: 'quota', dialogPaused: false, dialogTimer: 0, connected: false, stopped: false, busy: false, epoch: 0, requests: new Set(), timer: 0, failures: 0};
  const storage = {get(store, key) {try {return store.getItem(key);} catch {return null;}}, set(store, key, value) {try {if (value == null) store.removeItem(key); else store.setItem(key, value);} catch { /* Storage is optional. */ }}};
  state.key = storage.get(sessionStorage, sessionKey) || '';
  $('remember-key').checked = Boolean(state.key);
  let theme = storage.get(localStorage, 'cpa-stats-theme') || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
  document.documentElement.dataset.theme = theme;
  function node(tag, text, className) {const element = document.createElement(tag); if (text != null) element.textContent = String(text); if (className) element.className = className; return element;}
  function message(id, text, error = false) {const target = $(id); target.textContent = text; target.hidden = !text; target.classList.toggle('error', error);}
  function connection(text, status = 'ready') {$('connection').dataset.status = status; $('connection').lastElementChild.textContent = text;}
  function invalidate() {state.epoch++; for (const controller of state.requests) controller.abort(); state.requests.clear(); state.busy = false; clearTimeout(state.timer);}
  function clearData() {state.data = null; state.prices = []; state.quota = null; state.credentials = []; state.total = 0; state.offset = 0; message('persistence-warning', ''); closeQuotaDialog(); renderTotals(null); renderOverview({}); renderTables({}); renderEvents({events: [], total: 0}); renderQuota(); renderPrices(); $('updated').textContent = 'No authenticated snapshot';}
  function authRequired() {state.connected = false; state.stopped = true; state.key = ''; storage.set(sessionStorage, sessionKey, null); invalidate(); clearData(); $('refresh').disabled = false; connection('Authentication required', 'error'); message('banner', 'Connect with a valid management key to view statistics.', true); openAuth();}
  async function api(path, options = {}) {
    const controller = new AbortController(); state.requests.add(controller);
    const headers = new Headers({'Accept': 'application/json'});
    if (state.key) headers.set('Authorization', `Bearer ${state.key}`);
    if (options.body !== undefined) headers.set('Content-Type', 'application/json');
    let response;
    try {
      // `root` addresses an existing management endpoint outside the statistics
      // group, e.g. the original credential quota refresh. Same origin, same key.
      response = await fetch(options.root ? new URL(path, location.href) : new URL(path, apiBase), {method: options.method || 'GET', headers, credentials: 'same-origin', cache: 'no-store', redirect: 'error', referrerPolicy: 'no-referrer', signal: controller.signal, ...(options.body !== undefined ? {body: JSON.stringify(options.body)} : {})});
      if (response.status === 401 || response.status === 403) {authRequired(); throw new Error('Management authentication failed. Check your key and remote-management permissions.');}
      if (!response.ok) throw new Error(await serverError(response));
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
  function filters() {return {range: $('range').value, from: $('from').value, to: $('to').value, model: $('model').value, provider: $('provider').value, status: $('status').value};}
  // Every range control on the page is filled from the one preset list, so the page
  // filters and the side window always offer the same choices in the same order.
  function fillRangeSelects() {
    for (const id of ['range', 'quota-range']) {
      const select = $(id), previous = select.value;
      select.replaceChildren();
      for (const [value, label] of C.RANGE_PRESETS) select.add(new Option(label, value));
      select.value = C.RANGE_PRESETS.some(([value]) => value === previous) ? previous : C.RANGE_DEFAULT;
    }
  }
  // A custom range only makes sense once both ends are given, so the inputs stay
  // hidden until the preset asks for them.
  function syncRangeInputs() {
    const custom = $('range').value === 'custom';
    $('from-label').hidden = !custom;
    $('to-label').hidden = !custom;
    const dialogCustom = $('quota-range').value === 'custom';
    $('quota-from-label').hidden = !dialogCustom;
    $('quota-to-label').hidden = !dialogCustom;
  }
  function empty(target, title, description) {target.replaceChildren(); const box = node('div', null, 'empty'); box.append(node('strong', title), node('span', description)); target.append(box);}
  function emptyTable(id, columns, description) {const row = node('tr'); const cell = node('td', description, 'empty'); cell.colSpan = columns; row.append(cell); $(id).replaceChildren(row);}
  // The summary is one compact line inside the overview tab; the request stream
  // keeps its own space and no longer shares a header statistics strip.
  function renderTotals(summary) {
    const s = summary;
    if (!s) { $('overview-totals').textContent = 'Waiting for your first snapshot'; return; }
    const cost = C.number(s.priced_requests) ? C.money(s.cost_usd) : 'unpriced';
    $('overview-totals').textContent = `${C.integer(s.requests)} requests · ${C.percent(s.successes, s.requests)} success · ${C.compact(s.total_tokens)} tokens (${C.compact(s.input_tokens)} in / ${C.compact(s.output_tokens)} out) · ${cost} · ${C.compact(s.cache_read_tokens)} cache read · avg ${s.requests ? C.duration(s.average_latency_ms) : '—'}${C.number(s.unpriced_requests) ? ` · ${C.integer(s.unpriced_requests)} unpriced` : ''}`;
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
  // Categorical palette, defined in stats.css. The first series is the same green
  // the control panel uses for its own success marks, so a series colour means the
  // same thing in both surfaces.
  const colors = ['var(--series-1)', 'var(--series-2)', 'var(--series-3)', 'var(--series-4)', 'var(--series-5)'];
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
  function renderTables(data) {
    const models = C.arrays(data, 'models').sort((a, b) => C.number(b.requests) - C.number(a.requests));
    $('models-table').replaceChildren();
    if (!models.length) emptyTable('models-table', 9, 'No model usage matches these filters.');
    for (const model of models) {const row = node('tr'); cells(row, [model.name, C.integer(model.requests), C.percent(model.successes, model.requests), C.compact(model.input_tokens), C.compact(model.output_tokens), C.compact(model.cache_read_tokens), C.compact(model.total_tokens), C.duration(model.average_latency_ms), cost(model)]); $('models-table').append(row);}
    $('providers-table').replaceChildren();
    const providers = C.arrays(data, 'providers');
    if (!providers.length) emptyTable('providers-table', 6, 'No provider usage matches these filters.');
    for (const provider of providers) {const row = node('tr'); cells(row, [provider.name, C.integer(provider.requests), C.integer(provider.failures), C.compact(provider.total_tokens), C.duration(provider.average_latency_ms), cost(provider)]); $('providers-table').append(row);}
  }
  // The result cell carries the sanitized provider message on hover; the model cell
  // flags a response model that differs from the requested one.
  function resultBadge(event) {
    const label = event.failed ? `Failed${event.status_code ? ` · ${event.status_code}` : ''}` : 'Success';
    const badge = node('span', label, `badge${event.failed ? ' error' : ''}`);
    if (event.error_text) { badge.title = event.error_text; badge.classList.add('has-detail'); badge.tabIndex = 0; badge.setAttribute('aria-label', `${label}. ${event.error_text}`); }
    return badge;
  }
  function modelCell(event) {
    const cell = node('div', null, 'model-cell');
    const model = node('span', event.model || 'Unknown');
    model.append(node('small', `${event.provider || 'Unknown'}${event.stream ? ' · stream' : ''}`));
    cell.append(model);
    if (C.responseModelMismatch(event)) {
      const warning = node('span', `Resp: ${event.response_model}`, 'badge warn');
      warning.title = `The upstream response reported ${event.response_model} while ${event.model} was requested. Model aliases and provider routing can explain this.`;
      cell.append(warning);
    }
    return cell;
  }
  function tierCell(event) {
    const requested = String(event.service_tier || '').trim(), actual = String(event.response_service_tier || '').trim();
    if (!requested && !actual) return '—';
    const tier = actual || requested;
    const cell = node('div', null, 'tier-cell');
    const badge = node('span', tier, `badge${actual && requested && actual !== requested ? ' warn' : ' neutral'}`);
    if (actual && requested && actual !== requested) badge.title = `Requested ${requested}, upstream reported ${actual}.`;
    cell.append(badge);
    if (actual && requested && actual !== requested) cell.append(node('small', `req. ${requested}`));
    return cell;
  }
  function cacheRateCell(event) {
    const text = C.cacheRateText(event.cache_read_tokens, event.input_tokens);
    const cell = node('div', null, 'cache-cell');
    cell.append(node('span', text));
    cell.append(node('small', `${C.compact(event.cache_read_tokens)} read / ${C.compact(event.input_tokens)} in`));
    return cell;
  }
  function renderEvents(data) {
    const events = C.arrays(data, 'events'); state.total = C.number(data.total); $('events-table').replaceChildren();
    if (!events.length) emptyTable('events-table', 10, 'No completed requests match these filters. Live updates will appear automatically.');
    for (const event of events) {const row = node('tr'); if (C.responseModelMismatch(event)) row.classList.add('mismatch-row'); cells(row, [C.date(event.requested_at), modelCell(event), resultBadge(event), tierCell(event), C.compact(event.total_tokens), C.speedText(event.output_tokens, event.latency_ms), cacheRateCell(event), C.duration(event.latency_ms), event.ttft_ms > 0 ? C.duration(event.ttft_ms) : '—', event.priced ? C.money(event.cost_usd) : 'Unpriced']); $('events-table').append(row);}
    $('events-count').textContent = events.length ? `${C.integer(state.offset + 1)}–${C.integer(state.offset + events.length)} of ${C.integer(state.total)} requests${state.offset ? ' · older-page updates paused' : ''}` : '0 requests';
    $('events-prev').disabled = state.offset === 0; $('events-next').disabled = state.offset + state.limit >= state.total;
  }
  // Saved provider state is provider-specific. The pure shaping helpers live in
  // stats-core.js; this view only turns their output into DOM.
  function renderQuota() {
    const grid = $('quota-grid'); grid.replaceChildren();
    const credentials = state.credentials;
    if (!credentials.length) {empty(grid, 'No provider quota state yet', 'Quota appears after a manual refresh, a verified provider response, or an automatic header observation. Open Management › Quota to refresh a credential.'); return;}
    for (const credential of credentials) {
      const card = node('article', null, 'card quota-card');
      const heading = node('div', null, 'card-heading');
      const title = node('div');
      title.append(node('h2', C.credentialName(credential)));
      title.append(node('p', C.describeCredential(credential)));
      heading.append(title);
      // The two card actions travel together so the heading keeps exactly two
      // columns: the credential on the left, its actions on the right.
      const actions = node('div', null, 'quota-card-actions');
      const open = credential.lines.length ? () => openQuotaDialog(credential) : null;
      // The whole card is the target, matching the control panel's own quota cards,
      // where the card is the control. A click that lands on one of the card's own
      // buttons belongs to that button and must not also open the window. Keyboard
      // users keep using the labelled button inside the card, which stays the
      // accessible control for this action.
      if (open) {
        card.classList.add('openable');
        card.addEventListener('click', event => {if (event.target.closest('button')) return; open();});
      }
      if (credential.indices.length) {const refresh = node('button', 'Refresh', 'text-button'); refresh.type = 'button'; refresh.setAttribute('aria-label', `Refresh ${C.credentialName(credential)} from the provider`); refresh.addEventListener('click', () => void refreshFromProvider(credential, refresh)); actions.append(refresh);}
      if (open) {const label = node('button', 'Window history →', 'text-button'); label.type = 'button'; label.setAttribute('aria-label', `Open window history for ${C.credentialName(credential)}`); label.addEventListener('click', open); actions.append(label);}
      if (actions.childElementCount) heading.append(actions);
      card.append(heading);
      const lines = node('div', null, 'quota-lines');
      if (!credential.lines.length) {
        // The control panel's own quota page invites a refresh from a dashed
        // placeholder. Mirroring it keeps the empty card actionable instead of
        // explaining the feature in prose.
        const placeholder = node('button', null, 'quota-placeholder');
        placeholder.type = 'button';
        placeholder.append(node('span', '↻', 'quota-placeholder-icon'));
        placeholder.append(node('span', credential.indices.length ? 'Click here to refresh quota' : 'No saved quota yet'));
        if (credential.indices.length) {placeholder.setAttribute('aria-label', `Refresh ${C.credentialName(credential)} from the provider`); placeholder.addEventListener('click', () => void refreshFromProvider(credential, placeholder));}
        else {placeholder.disabled = true; placeholder.title = 'Refresh this credential in Management › Quota to populate it; verified provider responses and header observations fill it automatically.';}
        lines.append(placeholder);
      }
      for (const line of credential.lines) {
        const wrapper = node('div', null, 'quota-line');
        const head = node('div', null, 'quota-line-head');
        head.append(node('span', line.label), node('strong', line.percent == null ? '—' : `${line.percent.toFixed(0)}%`));
        const track = node('div', null, 'quota-track'), fill = node('i');
        fill.style.width = `${line.percent == null ? 0 : line.percent}%`;
        fill.classList.toggle('high', line.percent != null && line.percent >= 80);
        fill.classList.toggle('medium', line.percent != null && line.percent >= 50 && line.percent < 80);
        track.append(fill);
        wrapper.append(head, track);
        if (line.hint) wrapper.append(node('small', line.hint));
        lines.append(wrapper);
      }
      card.append(lines);
      grid.append(card);
    }
  }
  // Ask the original credential-quota handler to refresh one card's credentials from
  // the provider. This add-on performs no provider traffic itself: the existing
  // handler owns that call, and the passive middleware records the response under
  // the (provider, account) facts. A card can hold several credentials, and one may
  // legitimately fail (no quota plugin covers it), so every attempt is reported
  // without discarding the ones that succeeded.
  async function refreshFromProvider(credentials, button) {
    const targets = (Array.isArray(credentials) ? credentials : [credentials]).filter(credential => credential && credential.indices.length);
    if (!targets.length) {message('quota-message', 'No credential here can be refreshed: the add-on publishes no live credential for this group.', true); return;}
    if (button) button.disabled = true;
    message('quota-message', `Refreshing ${targets.length === 1 ? C.credentialName(targets[0]) : `${targets.length} credentials`} from the provider…`);
    const failures = [];
    let requested = 0;
    try {
      for (const credential of targets) {
        for (const index of credential.indices) {
          try {
            await api('./v0/management/quota/fetch', {root: true, method: 'POST', body: {auth_index: index}});
            requested++;
          } catch (error) {
            if (error.name === 'AbortError') return;
            failures.push(`${C.credentialName(credential)}: ${error.message}`);
          }
        }
      }
      // The observation is recorded by an isolated worker after the handler returns,
      // so give it a moment before re-reading the saved state it just produced.
      if (requested) await new Promise(resolve => setTimeout(resolve, 900));
      await refresh(false);
      if (failures.length) message('quota-message', `Some credentials could not be refreshed — ${failures.join(' · ')}`, true);
      else if (requested) message('quota-message', `Requested a provider refresh for ${requested} credential${requested === 1 ? '' : 's'}. Refreshed values appear as they are recorded.`);
      else message('quota-message', 'The provider refresh could not be requested.', true);
    } finally {
      if (button) button.disabled = false;
    }
  }

  async function loadQuota(epoch) {
    try {
      const [cache, snapshots, identities] = await Promise.all([api('quota/cache'), api('quota'), api('quota/identities')]);
      if (epoch !== state.epoch) return;
      state.quota = {entries: C.arrays(cache, 'entries'), snapshots: C.arrays(snapshots, 'snapshots')};
      state.credentials = C.summarizeCredentials(state.quota.entries, state.quota.snapshots, C.arrays(identities, 'bindings'));
      // Quota history is only recorded under the backend's own window id, so only
      // windows a line can name a backend id for are offered. A display id that
      // reached the card from saved state alone has no recorded history and would
      // chart nothing, so offering it would only produce an empty window.
      for (const credential of state.credentials) {
        const windows = new Map();
        for (const line of credential.lines) if (line.source) windows.set(line.source, line.label || line.source);
        credential.windows = [...windows].map(([id, label]) => ({id, label}));
      }
      renderQuota();
      message('quota-message', '');
    } catch (error) {
      if (error.name === 'AbortError' || epoch !== state.epoch) return;
      state.credentials = [];
      renderQuota();
      message('quota-message', `${error.message} Saved quota state needs a reachable statistics API and a management route that is not write-blocked.`, true);
    }
  }
  // Quota history is keyed by the backend's window id, which is not the label the
  // card shows ("five_hour" against "5h limit"). Present the label the reader
  // already has on the card, and fall back to the id only when nothing maps it.
  function windowLabel(id) {
    if (!id) return '';
    const line = (state.dialog?.lines || []).find(item => item.source === id);
    return line?.label || String(id);
  }
  function renderQuotaHistory(summary) {
    const target = $('quota-history'), points = C.arrays(summary, 'observations');
    const note = $('quota-history-note');
    if (!points.length) {note.textContent = summary?.window ? `No history recorded for ${windowLabel(summary.window)}` : 'Recorded observations of this window'; empty(target, 'No recorded observations', 'History starts with the first verified quota observation and is capped per credential; nothing is backfilled.'); return;}
    note.textContent = `${windowLabel(summary.window) || 'Window'} · ${C.integer(points.length)} observations · ${C.date(points[0].observed_at)} → ${C.date(points.at(-1).observed_at)}`;
    const width = 520, height = 160, values = points.map(point => ({time: Date.parse(point.observed_at), percent: C.number(point.used_percent)}));
    const first = values[0].time, span = (values.at(-1).time - first) || 1, svgNS = 'http://www.w3.org/2000/svg';
    function shape(tag, attrs, text) {const element = document.createElementNS(svgNS, tag); for (const [key, value] of Object.entries(attrs)) element.setAttribute(key, String(value)); if (text != null) element.textContent = text; return element;}
    const svg = shape('svg', {viewBox: `0 0 ${width} ${height}`, role: 'img', 'aria-label': 'Recorded used percentage of the selected quota window'});
    svg.append(shape('title', {}, 'Window history'), shape('desc', {}, 'Each point is one recorded observation of how much of the window was used.'));
    for (let i = 0; i <= 2; i++) {const y = 12 + (height - 34) * i / 2; svg.append(shape('line', {x1: 34, x2: width - 8, y1: y, y2: y, class: 'chart-grid'}), shape('text', {x: 28, y: y + 4, 'text-anchor': 'end', class: 'chart-axis'}, `${100 - i * 50}%`));}
    const x = point => 34 + (point.time - first) / span * (width - 42), y = point => height - 22 - Math.min(100, point.percent) / 100 * (height - 34);
    svg.append(shape('path', {d: values.map((point, i) => `${i ? 'L' : 'M'}${x(point).toFixed(2)} ${y(point).toFixed(2)}`).join(' '), class: 'chart-line'}));
    for (const point of values) {const dot = shape('circle', {cx: x(point), cy: y(point), r: values.length > 120 ? 1.5 : 3, class: 'chart-point'}); dot.append(shape('title', {}, `${C.date(point.time)} · ${point.percent.toFixed(1)}% used`)); svg.append(dot);}
    target.replaceChildren(svg, node('small', `Oldest ${C.date(values[0].time)} · newest ${C.date(values.at(-1).time)} · local time`));
  }
  function renderQuotaValue(summary) {
    const target = $('quota-value'), usage = summary?.usage || {}, estimate = summary?.estimate || null;
    const rows = [
      ['Recorded requests', C.integer(usage.requests)],
      ['Tokens', `${C.compact(usage.total_tokens)} (${C.compact(usage.input_tokens)} in / ${C.compact(usage.output_tokens)} out)`],
      ['Estimated spend', C.number(usage.priced_requests) ? `${C.money(usage.cost_usd)}${C.number(usage.unpriced_requests) ? ` · ${C.integer(usage.unpriced_requests)} unpriced` : ''}` : 'Unpriced'],
      ['Window used', estimate ? `${C.number(estimate.used_percent).toFixed(1)}%${summary?.window ? ` of ${windowLabel(summary.window)}` : ''} at the latest observation` : '—'],
      ['Full-window estimate', estimate && C.number(estimate.full_usd) ? C.money(estimate.full_usd) : 'Not derivable'],
      ['Estimated unused', estimate && C.number(estimate.unused_usd) ? C.money(estimate.unused_usd) : 'Not derivable'],
    ];
    const list = node('div', null, 'quota-value-list');
    for (const [label, value] of rows) {const row = node('div', null, 'quota-value-row'); row.append(node('span', label), node('strong', value)); list.append(row);}
    target.replaceChildren(list, node('small', 'The estimate applies your stored rates to the requests attributed to this credential inside the selected range, then scales that spend to a fully used window. It is not a provider invoice and is unavailable while spend or window usage is unknown.', 'muted'));
  }
  function renderQuotaRequests(page) {
    const events = C.arrays(page, 'events'), table = $('quota-requests');
    table.replaceChildren();
    if (!events.length) emptyTable('quota-requests', 8, 'No requests from this credential in the selected range.');
    for (const event of events) {const row = node('tr'); if (C.responseModelMismatch(event)) row.classList.add('mismatch-row'); cells(row, [C.date(event.requested_at), modelCell(event), resultBadge(event), tierCell(event), C.compact(event.total_tokens), C.speedText(event.output_tokens, event.latency_ms), C.duration(event.latency_ms), event.priced ? C.money(event.cost_usd) : 'Unpriced']); table.append(row);}
    $('quota-requests-count').textContent = events.length ? `${C.integer(events.length)} of ${C.integer(C.number(page.total) || events.length)} requests in range · newest first${state.dialogPaused ? ' · updates paused' : ' · refreshes every 5 seconds'}` : 'No requests in range';
  }
  // The side window shows one credential from two independent sources: the recorded
  // quota summary and the credential's own request stream. They are separate reads
  // with separate refresh needs, so they live on separate tabs and only the visible
  // one is read. The window and range selectors stay shared because both drive them.
  function renderDialogTab() {
    for (const button of document.querySelectorAll('[data-dialog-tab]')) {
      if (button.dataset.dialogTab === state.dialogTab) button.setAttribute('aria-current', 'page'); else button.removeAttribute('aria-current');
    }
    $('dialog-panel-quota').hidden = state.dialogTab !== 'quota';
    $('dialog-panel-requests').hidden = state.dialogTab !== 'requests';
  }
  function selectDialogTab(name) {if (!['quota', 'requests'].includes(name)) return; state.dialogTab = name; renderDialogTab(); void refreshQuotaDialog();}
  async function refreshQuotaDialog(force = true) {
    const credential = state.dialog;
    if (!credential) return;
    const epoch = state.epoch, window = $('quota-window').value;
    const stream = state.dialogTab === 'requests';
    // A custom range is validated before anything is read, so an unfinished range
    // reports itself on the surface instead of rejecting unnoticed.
    let range;
    try {
      range = C.rangeQuery($('quota-range').value, {from: $('quota-from').value, to: $('quota-to').value});
    } catch (error) {
      message('quota-dialog-message', error.message, true);
      return;
    }
    // The credential-scoped stream is filtered by the recorded facts: the events API
    // no longer accepts a credential index, only `provider` and `account`.
    const params = new URLSearchParams({provider: credential.provider, from: range.from, to: range.to});
    if (credential.account) params.set('account', credential.account);
    if (window) params.set('window', window);
    // A paused stream is not read. The poll it owns stops scheduling itself, so
    // pausing the stream must not also stop the quota tab refreshing.
    if (stream && state.dialogPaused) return;
    try {
      if (stream) {
        const page = await api(`events?${params}&limit=25`);
        if (epoch !== state.epoch || state.dialog !== credential) return;
        renderQuotaRequests(page);
        message('quota-requests-error', '');
      } else {
        const summary = await api(`quota/summary?${params}`);
        if (epoch !== state.epoch || state.dialog !== credential) return;
        renderQuotaHistory(summary);
        renderQuotaValue(summary);
      }
      message('quota-dialog-message', '');
    } catch (error) {
      if (error.name === 'AbortError' || epoch !== state.epoch) return;
      message('quota-dialog-message', error.message, true);
    } finally {
      if (epoch === state.epoch && state.dialog === credential) scheduleQuotaDialog(force);
    }
  }
  function scheduleQuotaDialog(force = false) {clearTimeout(state.dialogTimer); if (!state.dialog || state.stopped) return; if (state.dialogPaused && state.dialogTab === 'requests') return; state.dialogTimer = setTimeout(() => {if (!document.hidden && navigator.onLine !== false) void refreshQuotaDialog(); else scheduleQuotaDialog();}, 5000);}
  function openQuotaDialog(credential) {
    state.dialog = credential; state.dialogPaused = false; state.dialogTab = 'quota'; renderDialogTab(); syncRangeInputs();
    $('quota-pause').setAttribute('aria-pressed', 'false'); $('quota-pause').textContent = 'Pause stream';
    $('quota-dialog-title').textContent = C.credentialName(credential);
    $('quota-dialog-subtitle').textContent = C.describeCredential(credential).replace('observed', 'last saved');
    const select = $('quota-window'), previous = select.value;
    select.replaceChildren();
    // Automatic is the default: it charts whichever window has the most recorded
    // observations, which the card cannot know on its own. The explicit choices are
    // the backend window ids the card's own lines recorded, each labelled the way
    // the card labels it, so a choice always has history to chart.
    const options = [['', 'Automatic · most recorded']];
    for (const window of credential.windows || []) options.push([window.id, window.label || window.id]);
    for (const [value, label] of options) select.add(new Option(label, value));
    if (options.some(([value]) => value === previous)) select.value = previous;
    renderQuotaHistory(null); renderQuotaValue(null); renderQuotaRequests({events: [], total: 0});
    message('quota-dialog-message', '');
    if (!$('quota-dialog').open) $('quota-dialog').showModal();
    // A modal focuses its first focusable element, which here is the dismiss button.
    // Start on the window selector instead: it is the first real control on the
    // surface, and it keeps the ring off the control that means "leave".
    $('quota-window').focus();
    void refreshQuotaDialog();
  }
  function closeQuotaDialog() {state.dialog = null; clearTimeout(state.dialogTimer); if ($('quota-dialog').open) $('quota-dialog').close();}
  function schedule() {clearTimeout(state.timer); if (!state.stopped) state.timer = setTimeout(() => {const live = state.tab === 'requests' && !state.paused && state.offset === 0; const quota = state.tab === 'quota'; if (!document.hidden && navigator.onLine !== false && (live || quota || (state.tab !== 'requests' && state.tab !== 'quota'))) void refresh(false); else schedule();}, Math.min(60000, (state.tab === 'requests' ? 5000 : state.tab === 'quota' ? 15000 : 15000) * Math.pow(2, Math.min(3, state.failures))));}
  async function loadFilters(epoch) {
    try {const params = C.query(filters()); for (const key of ['model', 'provider', 'account', 'key_id', 'status', 'bucket']) params.delete(key); const result = await api(`filters?${params}`); if (epoch !== state.epoch) return; for (const [id, key, label] of [['model', 'models', 'All models'], ['provider', 'providers', 'All providers']]) {const select = $(id), selected = select.value; const values = [...new Set((Array.isArray(result[key]) ? result[key] : []).map(String))].sort(); select.replaceChildren(new Option(label, '')); if (selected && !values.includes(selected)) values.unshift(selected); for (const value of values) select.add(new Option(value, value)); select.value = selected;}} catch (error) {if (error.name !== 'AbortError' && epoch === state.epoch && state.connected) message('banner', 'Statistics loaded, but filter options could not refresh. Your current filters are retained.', true);}
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
      else if (tab === 'quota') {await loadQuota(epoch); if (epoch !== state.epoch) return;}
      else {const response = await api(`overview?${params}`); if (epoch !== state.epoch) return; state.data = response; renderTotals(response.summary); renderOverview(response); renderTables(response);
        if (tab === 'requests') {params.set('limit', state.limit); params.set('offset', state.offset); try {const events = await api(`events?${params}`); if (epoch !== state.epoch) return; renderEvents(events); message('live-error', '');} catch (error) {if (error.name === 'AbortError' || epoch !== state.epoch) return; message('live-error', error.message, true); throw error;}}
      }
      if (epoch !== state.epoch) return;
      const firstConnection = !state.connected; state.connected = true; state.failures = 0; connection('Connected'); $('updated').textContent = `Updated ${new Date().toLocaleTimeString()}`; message('banner', ''); message('auth-error', ''); if ($('auth-dialog').open) $('auth-dialog').close(); $('management-key').value = ''; if (firstConnection || force) void loadFilters(epoch);
      void api('status').then(status => {if (epoch === state.epoch) {const notice = statusNotice(status); message('persistence-warning', notice.text, notice.error);}}).catch(error => {if (epoch === state.epoch && error.name !== 'AbortError') message('persistence-warning', `Usage persistence status could not be checked. ${error.message} Statistics may be incomplete.`, true);});
    } catch (error) {if (error.name !== 'AbortError' && epoch === state.epoch) {state.failures++; connection(navigator.onLine === false ? 'Offline' : 'Update failed', 'error'); message('banner', `${error.message}${state.data ? ' Showing the last successful snapshot.' : ''}`, true); if ($('auth-dialog').open) message('auth-error', error.message, true);}}
    finally {if (epoch === state.epoch) {state.busy = false; $('refresh').disabled = false; schedule();}}
  }
  function selectTab(tab) {if (!['overview', 'requests', 'quota', 'pricing'].includes(tab)) return; invalidate(); closeQuotaDialog(); state.tab = tab; state.offset = 0; for (const button of document.querySelectorAll('[data-tab]')) {if (button.dataset.tab === tab) button.setAttribute('aria-current', 'page'); else button.removeAttribute('aria-current');} for (const name of ['overview', 'requests', 'quota']) $(`panel-${name}`).hidden = tab !== name; $('panel-pricing').hidden = tab !== 'pricing'; $('statistics-content').hidden = tab === 'pricing'; $('filters').hidden = tab === 'pricing' || tab === 'quota'; history.replaceState(null, '', `#${tab}`); void refresh();}
  function renderPrices() {
    const query = $('price-search').value.trim().toLowerCase(), rows = state.prices.filter(price => String(price.model || '').toLowerCase().includes(query)).sort((a, b) => String(a.model).localeCompare(String(b.model)));
    $('prices-table').replaceChildren(); if (!rows.length) emptyTable('prices-table', 7, query ? 'No rates match your search.' : 'No rates yet. Sync the catalog or add a manual override.');
    for (const price of rows.slice(0, 200)) {const row = node('tr'), badge = node('span', price.manual ? 'Manual override' : price.source || 'Catalog', `badge${price.manual ? '' : ' neutral'}`), actions = node('div', null, 'row-actions'); const edit = node('button', 'Edit', 'text-button'); edit.type = 'button'; edit.addEventListener('click', () => editPrice(price)); actions.append(edit); if (price.manual) {const reset = node('button', 'Remove override', 'text-button'); reset.type = 'button'; reset.addEventListener('click', () => void removePrice(price.model, reset)); actions.append(reset);} cells(row, [price.model, C.money(price.input_per_million), C.money(price.output_per_million), price.cache_read_available === false ? 'Unknown' : C.money(price.cache_read_per_million), price.cache_write_available === false ? 'Unknown' : C.money(price.cache_write_per_million), badge, actions]); $('prices-table').append(row);}
    $('prices-count').textContent = `${rows.length > 200 ? `Showing first 200 of ${C.integer(rows.length)} matches — narrow your search. ` : `${C.integer(rows.length)} matches. `}${C.integer(state.prices.length)} total model rates · USD per million tokens · * partial pricing coverage in the overview`;
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
  $('filters').addEventListener('submit', event => event.preventDefault());
  $('filters').addEventListener('change', () => {syncRangeInputs(); state.offset = 0; invalidate(); void refresh();});
  $('reset-filters').addEventListener('click', () => {$('filters').reset(); $('range').value = C.RANGE_DEFAULT; syncRangeInputs(); state.offset = 0; invalidate(); void refresh();});
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
  $('quota-refresh').addEventListener('click', () => void refresh());
  $('quota-provider-refresh').addEventListener('click', () => void refreshFromProvider(state.credentials, $('quota-provider-refresh')));
  $('quota-dialog-close').addEventListener('click', closeQuotaDialog);
  for (const button of document.querySelectorAll('[data-dialog-tab]')) button.addEventListener('click', () => selectDialogTab(button.dataset.dialogTab));
  $('quota-dialog').addEventListener('close', () => {state.dialog = null; clearTimeout(state.dialogTimer);});
  $('quota-window').addEventListener('change', () => void refreshQuotaDialog());
  $('quota-range').addEventListener('change', () => {syncRangeInputs(); void refreshQuotaDialog();});
  for (const id of ['quota-from', 'quota-to']) $(id).addEventListener('change', () => void refreshQuotaDialog());
  $('quota-pause').addEventListener('click', () => {state.dialogPaused = !state.dialogPaused; $('quota-pause').setAttribute('aria-pressed', String(state.dialogPaused)); $('quota-pause').textContent = state.dialogPaused ? 'Resume stream' : 'Pause stream'; if (!state.dialogPaused) void refreshQuotaDialog();});
  window.addEventListener('offline', () => {connection('Offline', 'error'); message('banner', 'You are offline. The last successful snapshot remains visible.', true);});
  window.addEventListener('online', () => {if (!state.stopped) void refresh();});
  document.addEventListener('visibilitychange', () => {if (!document.hidden && !state.stopped && !(state.tab === 'requests' && (state.paused || state.offset))) {void refresh(false); if (state.dialog) void refreshQuotaDialog();}});
  window.addEventListener('pagehide', () => {state.stopped = true; invalidate();});
  window.addEventListener('pageshow', event => {if (event.persisted) {state.stopped = false; void refresh();}});
  fillRangeSelects();
  syncRangeInputs();
  const initialTab = location.hash.slice(1); if (['overview', 'requests', 'quota', 'pricing'].includes(initialTab)) selectTab(initialTab); else void refresh();
})();
