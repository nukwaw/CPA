/* Pure presentation helpers; shared by the dashboard and dependency-free tests. */
(function (root) {
  'use strict';
  const number = value => Number.isFinite(Number(value)) ? Math.max(0, Number(value)) : 0;
  const integer = value => new Intl.NumberFormat(undefined, {maximumFractionDigits: 0}).format(number(value));
  const compact = value => new Intl.NumberFormat(undefined, {notation: 'compact', maximumFractionDigits: 1}).format(number(value));
  const money = value => new Intl.NumberFormat(undefined, {style: 'currency', currency: 'USD', minimumFractionDigits: 2, maximumFractionDigits: number(value) > 0 && number(value) < .01 ? 6 : 2}).format(number(value));
  const duration = value => value == null ? '—' : number(value) < 1000 ? `${integer(value)} ms` : `${(number(value) / 1000).toFixed(2)} s`;
  const percent = (part, total) => number(total) ? `${Math.min(100, number(part) / number(total) * 100).toFixed(1)}%` : '—';
  // Decode throughput, matching the keeper dashboard: complete output tokens over
  // total request time. Undefined when either side is unknown.
  const speed = (outputTokens, latencyMs) => {
    const tokens = number(outputTokens), latency = number(latencyMs);
    return latency > 0 && tokens > 0 ? tokens / (latency / 1000) : null;
  };
  const speedText = (outputTokens, latencyMs) => {
    const value = speed(outputTokens, latencyMs);
    return value == null ? '—' : `${value >= 100 ? integer(value) : value.toFixed(1)} tok/s`;
  };
  // Cache read rate, matching the keeper dashboard: share of input served from cache.
  const cacheRate = (cacheReadTokens, inputTokens) => number(inputTokens) > 0 ? number(cacheReadTokens) / number(inputTokens) * 100 : null;
  const cacheRateText = (cacheReadTokens, inputTokens) => {
    const value = cacheRate(cacheReadTokens, inputTokens);
    return value == null ? '—' : `${Math.min(100, value).toFixed(1)}%`;
  };
  // A response model differing from the requested one is a routing signal, not an
  // error; the dashboard flags it so an unexpected upstream substitution is visible.
  const responseModelMismatch = event => Boolean(String(event?.response_model || '').trim()) && String(event.response_model).trim() !== String(event.model || '').trim();
  const date = value => {
    const parsed = new Date(value);
    return value && Number.isFinite(parsed.getTime()) ? parsed.toLocaleString() : '—';
  };
  // One preset list drives every range control the dashboard owns, so the page
  // filters and the credential side window cannot drift apart, and a range is
  // always resolved in one place. `today` is the default: a reader opening the
  // page wants the current day, not a rolling window that reaches into yesterday.
  const RANGE_PRESETS = [
    ['today', 'Today'],
    ['yesterday', 'Yesterday'],
    ['24h', 'Last 24 hours'],
    ['7d', 'Last week'],
    ['30d', 'Last month'],
    ['custom', 'Custom range'],
    ['all', 'All retained history'],
  ];
  const RANGE_DEFAULT = 'today';
  const rolling = {'24h': 86400000, '7d': 604800000, '30d': 2592000000};
  const startOfDay = at => new Date(at.getFullYear(), at.getMonth(), at.getDate());
  // Resolve a preset to an explicit instant pair. The calendar presets use the
  // reader's local day boundaries, which is the frame the page already labels its
  // buckets in; a day boundary is therefore built from calendar parts rather than
  // by subtracting a fixed 24 hours, which a DST shift would move.
  function rangeBounds(range, filters = {}, now = Date.now()) {
    const at = new Date(now);
    if (range === 'custom') {
      const from = new Date(filters.from), to = new Date(filters.to);
      if (!filters.from || !filters.to || !Number.isFinite(from.getTime()) || !Number.isFinite(to.getTime()) || from >= to) throw new Error('Choose a valid custom range with the end after the start.');
      return {from, to};
    }
    if (range === 'today') return {from: startOfDay(at), to: at};
    if (range === 'yesterday') {const to = startOfDay(at); return {from: new Date(to.getFullYear(), to.getMonth(), to.getDate() - 1), to};}
    if (rolling[range]) return {from: new Date(now - rolling[range]), to: at};
    // Anything else, including an unknown value, reads all retained history.
    return {from: new Date(0), to: at};
  }
  function rangeQuery(range, filters = {}, now = Date.now()) {
    const {from, to} = rangeBounds(range, filters, now);
    return {from: from.toISOString(), to: to.toISOString(), bucket: to - from > 3 * 86400000 ? 'day' : 'hour'};
  }
  function query(filters, now = Date.now()) {
    const params = new URLSearchParams();
    const {from, to, bucket} = rangeQuery(filters.range || RANGE_DEFAULT, filters, now);
    params.set('from', from);
    params.set('to', to);
    params.set('bucket', bucket);
    for (const key of ['provider', 'model', 'account', 'key_id', 'status']) if (filters[key]) params.set(key, filters[key]);
    return params;
  }
  function seriesPoints(series, field, width = 740, height = 220) {
    const all = (Array.isArray(series) ? series : []).filter(item => item && Number.isFinite(Date.parse(item.time))).map(item => ({time: item.time, timestamp: Date.parse(item.time), value: number(item[field])})).sort((a, b) => a.timestamp - b.timestamp);
    const max = all.reduce((value, item) => Math.max(value, item.value), 1);
    const values = all.length > 1500 ? Array.from({length: 1500}, (_, i) => all[Math.round(i * (all.length - 1) / 1499)]) : all;
    const left = 48, top = 10, bottom = height - 28, right = width - 14;
    const start = all[0]?.timestamp || 0, span = (all.at(-1)?.timestamp || 0) - start;
    return {max, left, top, bottom, right, sampled: all.length > values.length, points: values.map(item => ({...item, x: left + (span ? (item.timestamp - start) / span : .5) * (right - left), y: bottom - item.value / max * (bottom - top)}))};
  }
  function csvCell(value) {
    let text = String(value ?? '');
    if (/^[\s]*[=+@\-]/.test(text) || /^[\t\r\n]/.test(text)) text = "'" + text;
    return '"' + text.replace(/"/g, '""') + '"';
  }
  function price(input) {
    const model = String(input.model || '').trim();
    if (!model || model.length > 256) throw new Error('Enter an exact model ID (up to 256 characters).');
    const result = {model};
    for (const key of ['input_per_million', 'output_per_million', 'cache_read_per_million', 'cache_write_per_million']) {
      const value = Number(input[key]);
      if (input[key] === '' || input[key] == null || !Number.isFinite(value) || value < 0 || value > 1e9) throw new Error('Rates must be finite, non-negative numbers no greater than 1 billion.');
      result[key] = value;
    }
    return result;
  }
  function arrays(value, key) { return Array.isArray(value?.[key]) ? value[key].filter(item => item && typeof item === 'object') : []; }
  /* ---- Saved provider quota state -------------------------------------------------
     Provider state shapes differ per provider: some ship windows with a display label,
     others rows with an i18n key plus params, others grouped buckets or a billing
     object. Every shape is reduced to the same {id, label, percent, hint} line so one
     card can present any provider honestly, and an unknown value stays null rather
     than being rendered as zero. */
  const percentOf = value => {
    const parsed = Number(value);
    return Number.isFinite(parsed) ? Math.min(100, Math.max(0, parsed)) : null;
  };
  // Untranslated providers ship an i18n key, sometimes with a duration parameter.
  const quotaLabel = (key, params) => {
    const name = String(key || '').split('.').pop();
    if (!name) return '';
    if (params && params.duration) return `${params.duration} limit`;
    const words = name.split('_');
    return words.map((word, index) => index === 0 && word ? word.charAt(0).toUpperCase() + word.slice(1) : word).join(' ');
  };
  // The identities API composes its key from a file name and an internal suffix.
  // The key is presentation only; the account fact carries the grouping.
  // The dashboard groups quota by (provider, account), so a card is titled by the
  // fact that identifies the group. A filename would be wrong here: several
  // credentials can share one account, and the card is not about any single file.
  // A card is one credential file, the same unit the control panel's own quota page
  // uses. The recorded account is shown alongside it as a fact, not as the title.
  const credentialName = credential => {
    const file = credentialFiles(credential);
    return file || accountText(credential) || credential?.provider || 'Credential';
  };
  // The credential's file, which is how a card is identified and addressed. A devin
  // file can expose more than one credential identity, so its suffix is kept.
  const credentialFiles = credential => {
    const raw = String(credential?.key || '').trim();
    if (!raw) return '';
    return raw.split('\u0000').filter(Boolean).join(' \u00b7 ');
  };
  // A credential that exposes no account property is grouped by provider alone, so
  // the card still says which account fact is missing instead of guessing a value.
  const accountText = credential => {
    const account = String(credential?.account || '').trim();
    if (!account) return '';
    const kind = String(credential?.account_kind || '').trim();
    return kind === 'device_id' ? `device ${account}` : kind === 'email' ? account : `${kind ? `${kind} ` : ''}${account}`;
  };
  const describeCredential = credential => {
    const files = credentialFiles(credential);
    return `${credential?.provider || 'Unknown'}${accountText(credential) ? ` \u00b7 ${accountText(credential)}` : ''}${files ? ` \u00b7 ${files}` : ''}${credential?.observed_at ? ` \u00b7 observed ${date(credential.observed_at)}` : ''}`;
  };
  // The dashboard groups every credential by the facts the backend recorded.
  const credentialKey = (provider, key) => `${provider || ''}\u0000${key || ''}`;
  // Grouped providers report what is left as a 0..1 fraction of the window.
  const usedFromRemaining = fraction => {
    const parsed = Number(fraction);
    return Number.isFinite(parsed) ? percentOf(100 - Math.min(1, Math.max(0, parsed)) * 100) : null;
  };
  // Saved display state and normalized observations use different window id
  // namespaces for the same window: the control panel saves display ids
  // ("five-hour", "weekly"), while the backend records its own ids
  // ("five_hour" for Claude, "primary"/"additional:<name>" for Codex). Matching
  // them is what lets a newer observation replace a saved value instead of being
  // appended as a second line for the same window.
  const displayWindowID = id => String(id || '').trim().replaceAll('_', '-');
  function normalizedWindowID(provider, window, items = []) {
    const id = String(window?.id || '');
    if (provider === 'claude') return id === 'iguana_necktie' ? 'seven-day-fable' : displayWindowID(id);
    if (provider !== 'codex') return id;
    const seconds = Number(window?.window_seconds);
    const period = seconds === 18000 ? 'five-hour' : seconds >= 2419200 && seconds <= 2678400 ? 'monthly' : seconds === 604800 ? 'weekly' : null;
    if (!period) return null;
    if (id === 'primary' || id === 'secondary') return period;
    if (id.startsWith('code_review:')) return `code-review-${period}`;
    // Match a uniquely named additional window by name and duration; the UI-only
    // array index in the id is not stable.
    if (id.startsWith('additional:')) {
      const slug = value => String(value || '').trim().toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
      const name = id.split(':')[1];
      const matches = items.filter(item => slug(item.labelParams?.name) === name && Number(item.periodHours) * 3600 === seconds);
      return matches.length === 1 ? displayWindowID(matches[0].id) : null;
    }
    return null;
  }
  // Reduce one credential's saved state and the account's normalized snapshot to
  // lines. The saved state is the control panel's own last observation and the
  // snapshot is a separate timestamped measurement (a provider response or a
  // manual refresh the backend recorded), so the newer one is authoritative per
  // window. A window the newer observation does not carry keeps its saved value,
  // and an older observation never replaces a newer saved value.
  //
  // Each line is `{id, label, percent, hint, source}`. `id` is what the card
  // renders (the control panel's display id when the window maps to one) and
  // `source` is the backend window id the value was recorded under, or empty when
  // the value exists only as saved display state with no recorded observation.
  function quotaLines(entry, snapshot) {
    const lines = [], state = entry?.state || {}, savedIndex = new Map(), rawIndex = new Map();
    const push = (id, label, ratio, hint) => {if (!id && !label) return null; lines.push({id: String(id || label), label: String(label || id), percent: ratio == null ? null : percentOf(ratio), hint: hint ? String(hint) : '', source: ''}); return lines.length - 1;};
    const ratio = (used, limit) => number(limit) > 0 ? number(used) / number(limit) * 100 : null;
    const stateWindows = arrays(state, 'windows');
    for (const window of stateWindows) {
      const used = percentOf(window.usedPercent), remaining = percentOf(window.remainingPercent);
      const index = push(window.id || window.label, window.label || quotaLabel(window.labelKey, window.labelParams) || window.id, used != null ? used : remaining != null ? 100 - remaining : null, window.resetLabel || (number(window.resetAtMs) ? date(window.resetAtMs) : ''));
      if (index == null) continue;
      savedIndex.set(displayWindowID(window.id || window.label), index);
      rawIndex.set(String(window.id || window.label), index);
    }
    for (const row of arrays(state, 'rows')) push(row.id || row.label, row.label || quotaLabel(row.labelKey, row.labelParams) || row.id, ratio(row.used, row.limit), row.resetHint || (number(row.resetAtMs) ? date(row.resetAtMs) : ''));
    for (const group of arrays(state, 'groups')) for (const bucket of arrays(group, 'buckets')) {
      push(bucket.id || bucket.window || group.id, `${group.label || quotaLabel(group.labelKey, group.labelParams) || group.id} \u00b7 ${bucket.label || bucket.window || bucket.id}`, usedFromRemaining(bucket.remainingFraction), bucket.description);
    }
    for (const window of arrays(state.data, 'windows')) push(window.id, window.label || window.id, percentOf(window.usedPercent), null);
    if (state.billing) push('billing', state.billing.planType || 'Billing', percentOf(state.billing.usagePercent ?? state.billing.usedPercent), state.billing.periodEnd);
    if (state.subscription?.plan) push('plan', 'Subscription', null, state.subscription.plan);
    const saved = Date.parse(entry?.observed_at || ''), observed = Date.parse(snapshot?.observed_at || '');
    const newer = !Number.isFinite(saved) || (Number.isFinite(observed) && observed > saved);
    if (newer) for (const window of arrays(snapshot, 'windows')) {
      const used = percentOf(window.used_percent), remaining = percentOf(window.remaining_percent);
      const percent = used != null ? used : remaining != null ? 100 - remaining : null;
      // A newer observation is authoritative about reset metadata too: a saved
      // reset time that the newer window does not report is dropped rather than
      // mixed with a fresh value.
      const hint = window.reset_at ? date(window.reset_at) : '';
      // An unmappable window is still shown rather than hidden, because the fresh
      // value is the point of the card; a raw id already on the card is updated in
      // place so a cache written with backend ids cannot render twice.
      const raw = String(window.id || window.label || '');
      const index = savedIndex.get(normalizedWindowID(entry?.provider, window, stateWindows)) ?? rawIndex.get(raw);
      // `source` records the backend window id this line's value came from. Quota
      // history is only ever recorded under that id, so it is the id a history
      // query must use; the rendered `id` may be the control panel's display id.
      if (index == null) {const at = push(raw, window.label || window.id, null, hint); if (at != null) {lines[at].percent = percent; lines[at].hint = hint; lines[at].source = raw; rawIndex.set(raw, at);}}
      else {if (percent != null) lines[index].percent = percent; lines[index].hint = hint; if (window.id) lines[index].source = raw;}
    }
    // A saved state that carries no recognizable window at all still shows the
    // account's last observation rather than an empty card.
    if (!lines.length) for (const window of arrays(snapshot, 'windows')) {const at = push(window.id || window.label, window.label || window.id, percentOf(window.used_percent), window.reset_at); if (at != null) lines[at].source = String(window.id || '');}
    return lines;
  }
  // Join saved display state, normalized snapshots and the live credential files
  // (the management auth-files list) into cards. One card is one credential file:
  // every file gets its own card, even when two files serve one account, because
  // each is separately refreshable, which is what the control panel's quota page
  // offers. An empty account is grouped under its provider alone.
  function summarizeCredentials(entries, snapshots, authFiles) {
    const credentials = new Map();
    const ensure = (provider, account, accountKind, key) => {
      const id = credentialKey(provider, key);
      if (!credentials.has(id)) credentials.set(id, {provider, account: account || '', account_kind: accountKind || '', key: key || '', indices: [], lines: [], state: null, observed_at: null});
      const credential = credentials.get(id);
      // The account is a fact recorded on the card, never the card's identity, so a
      // card that was created from an account-only source adopts the fact later.
      if (!credential.account && account) credential.account = account;
      if (!credential.account_kind && accountKind) credential.account_kind = accountKind;
      return credential;
    };
    for (const file of authFiles) {
      if (!file || typeof file !== 'object' || file.runtime_only) continue;
      const provider = String(file.provider || file.type || '').trim().toLowerCase();
      const name = String(file.name || '').trim();
      if (!provider || !name) continue;
      // Only the email is an account fact: the entry's `account` field can carry an
      // API key, and a credential that publishes no email keeps empty facts. A
      // saved entry for the same file later fills the fact it recorded.
      const email = String(file.email || '').trim();
      // `auth_index` is transient correlation: it addresses the live credential in the
      // core manager so the original quota handler can be asked to refresh it. It is
      // never rendered, persisted, or uploaded; only the account fact is durable.
      const index = typeof file.auth_index === 'string' ? file.auth_index.trim() : '';
      // A devin file can expose more than one credential identity, so its native
      // composite key keeps the transient suffix, matching its display cache keys.
      const key = provider === 'devin' && index ? `${name} ${index}` : name;
      const credential = ensure(provider, email, email ? 'email' : '', key);
      if (index && !credential.indices.includes(index)) credential.indices.push(index);
    }
    const snapshotFor = (provider, account) => snapshots.find(item => item.provider === provider && String(item.account || '') === String(account || ''));
    for (const entry of entries) {
      const credential = ensure(entry.provider, entry.account, entry.account_kind, entry.key), snapshot = snapshotFor(entry.provider, entry.account);
      credential.state = entry.state || null;
      credential.lines = quotaLines(entry, snapshot);
      // Every source contributes its own observation time; the newest one wins so a
      // fresher normalized snapshot is not hidden by an older saved display state.
      if (entry.observed_at && (!credential.observed_at || Date.parse(entry.observed_at) > Date.parse(credential.observed_at))) credential.observed_at = entry.observed_at;
    }
    // A normalized snapshot records only the account, because that is the quota
    // subject; it cannot name a file. So it enriches every card serving that account
    // and stands alone as an account-only card only when no credential file can
    // carry it, which is how a saved observation outlives its credential file.
    for (const snapshot of snapshots) {
      const account = String(snapshot.account || '');
      let matched = false;
      for (const credential of credentials.values()) {
        if (credential.provider !== snapshot.provider || credential.account !== account) continue;
        matched = true;
        if (!credential.observed_at || Date.parse(snapshot.observed_at) > Date.parse(credential.observed_at)) credential.observed_at = snapshot.observed_at;
      }
      if (!matched) {
        const orphan = ensure(snapshot.provider, snapshot.account, snapshot.account_kind, '');
        orphan.observed_at = snapshot.observed_at;
      }
    }
    for (const credential of credentials.values()) if (!credential.lines.length) credential.lines = quotaLines(null, snapshotFor(credential.provider, credential.account));
    // Ordered by provider then credential file, matching how the control panel lists
    // its own quota cards.
    return [...credentials.values()].sort((a, b) => String(a.provider + '\u0000' + a.key).localeCompare(String(b.provider + '\u0000' + b.key)));
  }
  const api = {number, integer, compact, money, duration, percent, date, query, seriesPoints, csvCell, price, arrays, speed, speedText, cacheRate, cacheRateText, responseModelMismatch, percentOf, quotaLabel, credentialName, credentialFiles, accountText, credentialKey, describeCredential, quotaLines, summarizeCredentials, rangeBounds, rangeQuery, RANGE_PRESETS, RANGE_DEFAULT};
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.CPAStats = api;
})(typeof window === 'undefined' ? globalThis : window);
