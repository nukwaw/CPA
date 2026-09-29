'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const C = require('./stats-core.js');
const bridge = require('./management-bridge.js');
const {statusNotice, httpErrorMessage} = require('./stats.js');

test('disabled builtin collection is informational and preserves history/pricing/quota access', () => {
  const notice = statusNotice({collection_enabled: false, write_failures: 0});
  assert.equal(notice.error, false);
  assert.match(notice.text, /Built-in usage collection is off/);
  assert.match(notice.text, /observability\.usage\.usage-statistics-enabled/);
  assert.match(notice.text, /Previously stored history, pricing, and quota remain available/);
  assert.doesNotMatch(notice.text, /separate collection toggle/);
});
test('enabled or absent builtin status does not display a disabled-collection notice', () => {
  for (const status of [{collection_enabled: true, write_failures: 0}, {write_failures: 0}, {}, null]) assert.deepEqual(statusNotice(status), {text: '', error: false});
});
test('persistence warnings remain visible alongside disabled collection and clear on resume', () => {
  const failures = {write_failures: 3, last_write_failure_at: '2026-01-02T03:04:05Z'};
  const disabled = statusNotice({collection_enabled: false, ...failures});
  assert.equal(disabled.error, true); assert.match(disabled.text, /collection is off/); assert.match(disabled.text, /3 persistence writes failed/);
  const resumed = statusNotice({collection_enabled: true, ...failures});
  assert.equal(resumed.error, true); assert.doesNotMatch(resumed.text, /collection is off/); assert.match(resumed.text, /3 persistence writes failed/);
});

test('bounded ingestion drops warn alongside write failures and collection state without adding a toggle', () => {
  const notice = statusNotice({collection_enabled: false, write_failures: 2, dropped_events: 7, queue_overflows: 5, validation_failures: 1, last_drop_at: '2026-01-02T03:04:05Z', pending_events: 3, queue_capacity: 256});
  assert.equal(notice.error, true);
  assert.match(notice.text, /collection is off/);
  assert.match(notice.text, /2 persistence writes failed/);
  assert.match(notice.text, /7 usage\/quota records were dropped before persistence/);
  assert.match(notice.text, /5 queue overflows; 1 validation failures/);
  assert.match(notice.text, /Historical data may have gaps/);
  assert.match(notice.text, /3 admitted records are pending/);
  assert.match(notice.text, /Queue capacity: 256, plus one in-flight record/);
  assert.doesNotMatch(notice.text, /enable.*persistence|enable.*queue/);
});
test('pending ingestion is informational and drains without a stale warning', () => {
  const pending = statusNotice({pending_events: 257, queue_capacity: 256, dropped_events: 0, write_failures: 0});
  assert.equal(pending.error, false);
  assert.match(pending.text, /257 admitted records/);
  assert.deepEqual(statusNotice({pending_events: 0, dropped_events: 0, queue_overflows: 0, validation_failures: 0}), {text: '', error: false});
});
test('local capacity warns at either supported boundary or after a rejected write without suggesting pruning', () => {
  const capacity = {retained_events: 10, retained_event_limit: 20, journal_bytes: 100, journal_byte_limit: 200, rejected_writes: 0};
  for (const reached of [{retained_events: 20}, {retained_events: 21}, {journal_bytes: 200}, {journal_bytes: 201}, {rejected_writes: 1}]) {
    const notice = statusNotice({local_capacity: {...capacity, ...reached}});
    assert.equal(notice.error, true);
    assert.match(notice.text, /local persistence reached a supported capacity boundary/);
    assert.match(notice.text, /Existing history remains readable/);
    assert.match(notice.text, /rejected, never silently pruned/);
    assert.match(notice.text, /backed-up move to CPA's existing PostgreSQL storage/);
  }
  for (const local_capacity of [undefined, null, {}, capacity, {retained_events: 10, retained_event_limit: 0, journal_bytes: 100, journal_byte_limit: 0}]) assert.deepEqual(statusNotice({local_capacity}), {text: '', error: false});
});
test('unattributed quota headers are informational and keep manual refresh and usage accounting available', () => {
  const notice = statusNotice({skipped_quota_headers: 3});
  assert.equal(notice.error, false);
  assert.match(notice.text, /3 quota-header observations were not attributed/);
  assert.match(notice.text, /credential\/account continuity could not be proven/);
  assert.match(notice.text, /Usage accounting is independent/);
  assert.match(notice.text, /verified manual quota refreshes remain available/);
  assert.deepEqual(statusNotice({skipped_quota_headers: 0}), {text: '', error: false});
  const combined = statusNotice({collection_enabled: false, write_failures: 1, dropped_events: 2, pending_events: 3, local_capacity: {rejected_writes: 1}, skipped_quota_headers: 4});
  assert.equal(combined.error, true);
  for (const pattern of [/collection is off/, /1 persistence writes failed/, /2 usage\/quota records/, /3 admitted records/, /capacity boundary/, /4 quota-header observations/]) assert.match(combined.text, pattern);
});
test('unavailable storage status is explicit without suggesting original management is unavailable', () => {
  const notice = statusNotice({available: false, collection_enabled: false});
  assert.equal(notice.error, true);
  assert.match(notice.text, /Usage persistence is unavailable/);
  assert.match(notice.text, /original management page and manual quota refresh remain available/);
  assert.doesNotMatch(notice.text, /Previously stored history, pricing, and quota remain available/);
  assert.equal(httpErrorMessage(503), statusNotice({available: false}).text);
  assert.match(httpErrorMessage(429), /Too many requests/);
  assert.match(httpErrorMessage(500), /HTTP 500/);
});

test('all-time range supplies an explicit epoch instead of server last24h default', () => {
  const params = C.query({range: 'all'}, Date.parse('2026-01-05T00:00:00Z'));
  assert.equal(params.get('from'), '1970-01-01T00:00:00.000Z');
  assert.equal(params.get('to'), '2026-01-05T00:00:00.000Z');
});
test('filters encode untrusted model IDs and never introduce auth query parameters', () => {
  const params = C.query({range: '24h', model: '<model>&token=secret', provider: 'codex', auth_index: 'a'}, 86400000);
  assert.equal(params.get('model'), '<model>&token=secret');
  assert.equal(params.has('token'), false);
  assert.equal(params.get('from'), '1970-01-01T00:00:00.000Z');
});
test('invalid custom ranges fail before a request', () => {
  for (const range of [{from: '', to: ''}, {from: '2026-02-01', to: '2026-01-01'}, {from: 'bad', to: '2026-01-01'}]) assert.throws(() => C.query({range: 'custom', ...range}), /valid custom range/);
});
test('sparse chart points reflect elapsed time and downsampling retains newest', () => {
  const series = [0, 3600000, 86400000].map((offset, index) => ({time: new Date(1767225600000 + offset).toISOString(), requests: index + 1}));
  const plot = C.seriesPoints(series, 'requests');
  assert.equal(plot.points.length, 3);
  assert.ok(Math.abs((plot.points[1].x - plot.points[0].x) / (plot.points[2].x - plot.points[0].x) - 1 / 24) < .001);
  const long = Array.from({length: 1800}, (_, index) => ({time: new Date(1767225600000 + index * 3600000).toISOString(), requests: index}));
  const sampled = C.seriesPoints(long, 'requests');
  assert.equal(sampled.sampled, true); assert.equal(sampled.points.length, 1500); assert.equal(sampled.points.at(-1).value, 1799);
});
test('prices require all nonnegative finite fields including explicit cache rates', () => {
  const valid = {model: ' test ', input_per_million: 2, output_per_million: 8, cache_read_per_million: 0, cache_write_per_million: 3};
  assert.equal(C.price(valid).model, 'test');
  for (const bad of [-1, Infinity, NaN, '', 1e10]) assert.throws(() => C.price({...valid, input_per_million: bad}));
  assert.throws(() => C.price({...valid, model: ''}));
});
test('CSV exports neutralize spreadsheet formulas and escape quotes', () => {
  assert.equal(C.csvCell('=HYPERLINK("evil")'), '"\'=HYPERLINK(""evil"")"');
  assert.equal(C.csvCell(' +2'), '"\' +2"'); assert.equal(C.csvCell('ordinary'), '"ordinary"');
});
test('quota sanitizer drops all unknown credential-bearing fields recursively', () => {
  const clean = bridge.sanitize('codex', {status: 'success', access_token: 'SECRET', error: 'secret error', windows: [{id: 'five-hour', usedPercent: 25, token: 'SECRET', labelParams: {name: 'GPT', access_token: 'SECRET'}}]});
  assert.deepEqual(clean, {status: 'success', windows: [{id: 'five-hour', usedPercent: 25, labelParams: {name: 'GPT'}}]});
  assert.equal(JSON.stringify(clean).includes('SECRET'), false);
  assert.equal(bridge.sanitize('codex', {status: 'loading', windows: []}), null);
  assert.equal(bridge.sanitize('__proto__', {status: 'success'}), null);
});
test('provider schemas preserve only the actual quota display fields', () => {
  const states = {
    antigravity: {status: 'success', groups: [{id: 'g', buckets: [{id: 'b', remainingFraction: .6}]}]},
    claude: {status: 'success', windows: [], extraUsage: {is_enabled: true, monthly_limit: 50, used_credits: 2, utilization: null}},
    devin: {status: 'success', windows: [], observedAtMs: 123, plan: 'pro'},
    kimi: {status: 'success', rows: [{id: 'summary', used: 1, limit: 5}]},
    meta: {status: 'success', data: {windows: [{id: 'weekly', usedPercent: 10}], planName: 'pro'}},
    xai: {status: 'success', billing: {mode: 'billing', periodType: 'weekly', usagePercent: 30, productUsage: []}}
  };
  for (const [provider, state] of Object.entries(states)) assert.deepEqual(bridge.sanitize(provider, {...state, refresh_token: 'secret'}), state);
  const xai = bridge.sanitize('xai', {...states.xai, billing: {...states.xai.billing, userId: 'private', teamId: 'private'}});
  assert.equal(JSON.stringify(xai).includes('private'), false);
});
test('Codex window identity uses observed duration, never primary-order guess', () => {
  assert.equal(bridge.normalizedWindowID('codex', {id: 'primary'}), null);
  assert.equal(bridge.normalizedWindowID('codex', {id: 'primary', window_seconds: 604800}), 'weekly');
  assert.equal(bridge.normalizedWindowID('codex', {id: 'secondary', window_seconds: 18000}), 'five-hour');
  assert.equal(bridge.normalizedWindowID('claude', {id: 'iguana_necktie'}), 'seven-day-fable');
});
test('additional Codex windows match unique observed name/duration, never index', () => {
  const windows = [{id: 'gpt-five-hour-3', labelParams: {name: 'GPT'}, periodHours: 5}];
  assert.equal(bridge.normalizedWindowID('codex', {id: 'additional:gpt:primary', window_seconds: 18000}, windows), 'gpt-five-hour-3');
  assert.equal(bridge.normalizedWindowID('codex', {id: 'additional:gpt:primary', window_seconds: 18000}, windows.concat(windows)), null);
});
test('quota overlay only updates newer windows and preserves unknown prior data', () => {
  const now = Date.now(), old = now - 60000;
  const current = {status: 'success', planType: 'Pro', windows: [{id: 'five-hour', usedPercent: 5, label: 'Existing', resetLabel: '-', periodHours: 5}, {id: 'weekly', usedPercent: 30, label: 'Weekly', resetLabel: '-', periodHours: 168}]};
  const times = new Map();
  const snapshot = {windows: [{id: 'primary', used_percent: 50, window_seconds: 18000, observed_at: new Date(now).toISOString()}, {id: 'secondary', used_percent: 99, window_seconds: 604800, observed_at: new Date(old - 1000).toISOString()}]};
  const result = bridge.overlay('codex', current, snapshot, old, times);
  assert.equal(result.changed, true); assert.equal(result.state.planType, 'Pro'); assert.equal(result.state.windows[0].usedPercent, 50); assert.equal(result.state.windows[1].usedPercent, 30); assert.equal(current.windows[0].usedPercent, 5);
  assert.equal(bridge.overlay('codex', result.state, snapshot, old, times).changed, false);
  assert.equal(bridge.overlay('codex', {status: 'loading', windows: []}, snapshot, old).changed, false);
  assert.equal(bridge.overlay('codex', {status: 'error', windows: []}, snapshot, old).changed, false);
});
test('newer 10% quota clears an expired reset from old 90% state for every mapped provider', () => {
  const now = Date.now(), old = now - 120000, expired = now - 60000;
  const cases = [
    {provider: 'codex', id: 'five-hour', incoming: {id: 'primary', window_seconds: 18000, used_percent: 10}, oldUsage: {usedPercent: 90}, usage: item => item.usedPercent, expected: 10, resets: {resetAtMs: expired, resetLabel: 'expired reset'}, wrap: windows => ({status: 'success', windows}), items: state => state.windows},
    {provider: 'claude', id: 'five-hour', incoming: {id: 'five_hour', used_percent: 10}, oldUsage: {usedPercent: 90}, usage: item => item.usedPercent, expected: 10, resets: {resetAtMs: expired, resetLabel: 'expired reset'}, wrap: windows => ({status: 'success', windows}), items: state => state.windows},
    {provider: 'devin', id: 'monthly', incoming: {id: 'monthly', remaining_percent: 90}, oldUsage: {remainingPercent: 10}, usage: item => item.remainingPercent, expected: 90, resets: {resetAtMs: expired, resetLabel: 'expired reset'}, wrap: windows => ({status: 'success', windows}), items: state => state.windows},
    {provider: 'antigravity', id: 'model', incoming: {id: 'model', used_percent: 10}, oldUsage: {remainingFraction: .1}, usage: item => item.remainingFraction, expected: .9, resets: {resetAtMs: expired, resetTime: new Date(expired).toISOString()}, wrap: buckets => ({status: 'success', groups: [{id: 'group', buckets}]}), items: state => state.groups[0].buckets},
    {provider: 'kimi', id: 'weekly', incoming: {id: 'weekly', used: 10, limit: 100}, oldUsage: {used: 90, limit: 100}, usage: item => item.used, expected: 10, resets: {resetAtMs: expired, resetHint: 'expired reset'}, wrap: rows => ({status: 'success', rows}), items: state => state.rows},
    {provider: 'meta', id: 'weekly', incoming: {id: 'weekly', used_percent: 10}, oldUsage: {usedPercent: 90}, usage: item => item.usedPercent, expected: 10, resets: {resetAt: expired / 1000}, wrap: windows => ({status: 'success', data: {windows}}), items: state => state.data.windows}
  ];
  for (const c of cases) {
    const current = c.wrap([{id: c.id, ...c.oldUsage, ...c.resets}, {id: 'unrelated', ...c.oldUsage, ...c.resets}]);
    const original = structuredClone(current), times = new Map();
    const snapshot = {windows: [{...c.incoming, observed_at: new Date(now).toISOString()}]};
    const result = bridge.overlay(c.provider, current, snapshot, old, times);
    assert.equal(result.changed, true, c.provider);
    const [updated, unrelated] = c.items(result.state);
    assert.equal(c.usage(updated), c.expected, c.provider);
    for (const field of Object.keys(c.resets)) assert.equal(Object.hasOwn(updated, field), false, `${c.provider}.${field}`);
    assert.deepEqual(unrelated, c.items(original)[1], `${c.provider} unrelated window`);
    assert.deepEqual(current, original, `${c.provider} source state must not mutate`);
    for (const at of [old - 1, old]) {
      const stale = bridge.overlay(c.provider, current, {windows: [{...c.incoming, observed_at: new Date(at).toISOString()}]}, old);
      assert.equal(stale.changed, false, `${c.provider} stale reset-less observation`);
      assert.equal(stale.state, current);
    }
    const staleReset = bridge.overlay(c.provider, result.state, {windows: [{...c.incoming, reset_at: new Date(expired).toISOString(), observed_at: new Date(now - 1).toISOString()}]}, old, times);
    assert.equal(staleReset.changed, false, `${c.provider} stale reset must not reappear`);
    assert.equal(staleReset.state, result.state);
  }
});
test('overlay uses a normalized future reset but never invents or restores one from display cache', () => {
  const now = Date.now(), future = now + 3600000;
  const current = {status: 'success', windows: [{id: 'five-hour', usedPercent: 90, resetAtMs: future, resetLabel: 'old future label'}]};
  const window = {id: 'primary', used_percent: 10, window_seconds: 18000, observed_at: new Date(now).toISOString()};
  const withReset = bridge.overlay('codex', current, {windows: [{...window, reset_at: new Date(future).toISOString()}]}, now - 1000);
  assert.equal(withReset.state.windows[0].resetAtMs, future);
  assert.equal(withReset.state.windows[0].resetLabel, new Date(future).toLocaleString());
  for (const reset_at of [undefined, null, '', 'invalid']) {
    const withoutReset = bridge.overlay('codex', current, {windows: [{...window, reset_at}]}, now - 1000);
    assert.equal(withoutReset.state.windows[0].usedPercent, 10);
    assert.equal(Object.hasOwn(withoutReset.state.windows[0], 'resetAtMs'), false);
    assert.equal(Object.hasOwn(withoutReset.state.windows[0], 'resetLabel'), false);
  }
});
test('partial normalized state creates only supported observed windows', () => {
  const snapshot = {plan: 'Pro', windows: [{id: 'five_hour', used_percent: 75, observed_at: new Date().toISOString()}]};
  const result = bridge.overlay('claude', null, snapshot, 0);
  assert.equal(result.changed, true); assert.equal(result.state.windows.length, 1); assert.equal(result.state.windows[0].id, 'five-hour'); assert.equal(result.state.planType, 'Pro');
});

function store(initial) {let state = initial; const listeners = []; return {getState: () => state, subscribe(fn) {listeners.push(fn); return () => {};}, setState(patch) {const old = state; if (typeof patch === 'function') patch = patch(state); if (patch === state) return; state = {...state, ...patch}; listeners.forEach(fn => fn(state, old));}};}
const settle = () => new Promise(resolve => setImmediate(resolve));
const binding = (credential_generation = 'generation-A', revision = 'revision-A', extra = {}) => ({provider: 'codex', key: 'test.json', auth_index: 'index1', credential_generation, revision, ...extra});
const success = (usedPercent = 42) => ({status: 'success', windows: [{id: 'five-hour', usedPercent, resetLabel: '-'}]});
const cachedEntry = (who = binding()) => ({...who, observed_at: new Date(Date.now() - 60000).toISOString(), state: {status: 'success', windows: [{id: 'five-hour', usedPercent: 12, label: 'Five hours', resetLabel: '-', periodHours: 5}]}});
const providers = ['antigravity', 'claude', 'codex', 'devin', 'kimi', 'meta', 'xai'];
function harness({cached = [], snapshots = [], origin = 'http://localhost:18317', delayFiles = false, quotaStatus = 200, bindings = [binding()], now = Date.now()} = {}) {
  const calls = [], events = [], timers = new Map(), listeners = new Map(), holds = []; let nextTimer = 0;
  const maps = {cacheGeneration: 0, fileGenerations: {}, ...Object.fromEntries(providers.map(provider => [`${provider}Quota`, {}]))};
  const quota = store(maps), auth = store({isAuthenticated: true, connectionStatus: 'connected', apiBase: origin, managementKey: 'secret-key'});
  const window = {dispatchEvent(event) {events.push(event);}, addEventListener(type, fn) {listeners.set(type, fn);}};
  const h = {window, quota, auth, calls, events, timers, bindings, cached, snapshots, quotaStatus, now, putStatus: 200, clearCalls: [], hold(path, method = 'GET') {holds.push({path, method});}, refresh() {listeners.get('online')();}, async timersAt(ms) {for (const [id, timer] of [...timers]) if (timer.ms === ms) {timers.delete(id); timer.fn();} await settle();}, puts() {return calls.filter(call => call.options.method === 'PUT');}, resolveFiles() {calls.find(call => call.url.endsWith('/identities') && call.release)?.release();}};
  if (delayFiles) h.hold('/identities');
  class Clock extends Date {constructor(...args) {super(...(args.length ? args : [h.now]));} static now() {return h.now;}}
  const context = vm.createContext({window, location: {origin: 'http://localhost:18317', href: 'http://localhost:18317/management.html'}, document: {hidden: false}, navigator: {onLine: true}, URL, Date: Clock, Map, Set, Object, Number, String, Array, JSON, Promise, TextEncoder, AbortController, DOMException, CustomEvent: class {constructor(type, options) {this.type = type; this.detail = options.detail;}}, console: {warn() {}}, setTimeout(fn, ms) {const id = ++nextTimer; timers.set(id, {fn, ms}); return id;}, clearTimeout(id) {timers.delete(id);}, fetch: async (url, options) => {
    const call = {url, options}; calls.push(call);
    let body = {}; if (url.endsWith('/identities')) body = {bindings: h.bindings}; else if (url.endsWith('/quota/cache')) body = {entries: h.cached}; else if (url.endsWith('/quota')) body = {snapshots: h.snapshots};
    let status = options.method === 'PUT' ? h.putStatus : h.quotaStatus;
    body = structuredClone(body);
    const held = holds.findIndex(item => url.endsWith(item.path) && options.method === item.method);
    if (held >= 0) {holds.splice(held, 1); const override = await new Promise(resolve => {call.release = value => {delete call.release; resolve(value);};}); if (override?.body) body = override.body; if (override?.status) status = override.status;}
    return {ok: status >= 200 && status < 300, status, json: async () => body};
  }});
  vm.runInContext(fs.readFileSync(require.resolve('./management-bridge.js'), 'utf8'), context);
  const api = window.CPAQuotaPersistence;
  // Exact semantics of the verified native helpers. Compiled-fixture Go tests
  // additionally execute the ORIGINAL generated expressions through this bridge.
  const nativeCapture = name => {const {cacheGeneration, fileGenerations} = quota.getState(); return {cacheGeneration, fileGenerations, name};};
  const nativeCommit = (generation, commit, name = generation.name) => {const current = quota.getState(); if (current.cacheGeneration !== generation.cacheGeneration) return false; if (name !== undefined) {if ((current.fileGenerations[name] ?? 0) !== (generation.fileGenerations[name] ?? 0)) return false;} else if (current.fileGenerations !== generation.fileGenerations) return false; commit(); return true;};
  const resolveUpdater = api.wrapUpdater((updater, previous) => typeof updater === 'function' ? updater(previous) : updater);
  h.capture = api.wrapCapture(nativeCapture); h.commit = api.wrapCommit(nativeCommit);
  h.update = (provider, updater) => quota.setState(state => ({[`${provider}Quota`]: resolveUpdater(updater, state[`${provider}Quota`])}));
  h.clear = names => {
    h.clearCalls.push(names ? [...names] : undefined);
    quota.setState(state => {
      if (names) {
        if (!names.length) return state;
        const fileGenerations = {...state.fileGenerations}; names.forEach(name => {fileGenerations[name] = (fileGenerations[name] ?? 0) + 1;});
        return {fileGenerations, ...Object.fromEntries(providers.map(provider => {const map = `${provider}Quota`, cache = state[map], keys = Object.keys(cache).filter(key => names.includes(key.split('\0')[0])); if (!keys.length) return [map, cache]; const next = {...cache}; keys.forEach(key => delete next[key]); return [map, next];}))};
      }
      return {cacheGeneration: state.cacheGeneration + 1, fileGenerations: {}, ...Object.fromEntries(providers.map(provider => [`${provider}Quota`, {}]))};
    });
  };
  quota.setState({clearQuotaCache: h.clear});
  h.succeed = (generation, value = success(), provider = 'codex', key = 'test.json') => h.commit(generation, () => h.update(provider, previous => ({...previous, [key]: value})));
  h.batch = (generation, results) => {for (const provider of new Set(results.map(result => result.provider))) h.update(provider, previous => {const next = {...previous}; results.filter(result => result.provider === provider).forEach(result => h.commit(generation, () => {next[result.key] = result.state;}, result.key.split('\0')[0])); return next;});};
  api.attach({quotaStore: quota, authStore: auth});
  return h;
}

test('bridge hydrates same stable generation across runtime restart without uploading old revision', async () => {
  const entry = cachedEntry(), h = harness({cached: [entry], bindings: [binding('generation-A', 'new-runtime-revision')]}); await settle();
  assert.equal(h.quota.getState().codexQuota['test.json'].windows[0].usedPercent, 12);
  assert.equal(h.window.CPAQuotaPersistence.status.cache_observed_at, entry.observed_at);
  await h.timersAt(500); assert.equal(h.puts().length, 0);
  assert.ok(h.calls.every(call => !call.url.includes('secret-key') && call.options.headers.Authorization === 'Bearer secret-key'));
  assert.equal(h.calls.some(call => call.url.includes('auth-files')), false);
});
test('bridge reports unavailable storage on canonical event without replacing quota UI state', async () => {
  const h = harness({quotaStatus: 503, delayFiles: true}), original = success(90);
  h.quota.setState({codexQuota: {'test.json': original}}); h.resolveFiles(); await settle();
  assert.equal(h.window.CPAQuotaPersistence.status.state, 'unavailable');
  assert.match(h.window.CPAQuotaPersistence.status.message, /original management page and manual quota refresh remain available/);
  assert.equal(h.quota.getState().codexQuota['test.json'], original);
  assert.ok(h.events.every(event => event.type === 'cpa-quota-persistence-status'));
  assert.equal(JSON.stringify(h.window.CPAQuotaPersistence.status).includes('secret-key'), false);
});
test('bridge refuses cross-origin or URL-carried credentials without leaking key', async () => {
  for (const origin of ['https://other.example', 'http://user:secret@localhost:18317', 'http://localhost:18317?token=secret', 'http://localhost:18317#secret']) {const h = harness({origin}); await settle(); assert.equal(h.calls.length, 0); assert.equal(h.window.CPAQuotaPersistence.status.state, 'incompatible');}
});
test('bridge preserves loading/live states and rejects absent or mismatched generation/index', async () => {
  const entry = cachedEntry(), h = harness({cached: [entry], delayFiles: true});
  h.quota.setState({codexQuota: {'test.json': {status: 'loading', windows: []}}}); h.resolveFiles(); await settle();
  assert.equal(h.quota.getState().codexQuota['test.json'].status, 'loading');
  for (const change of [{auth_index: 'replaced-index'}, {credential_generation: 'B'}, {credential_generation: undefined}]) {const other = harness({cached: [{...entry, ...change}]}); await settle(); assert.equal(other.quota.getState().codexQuota['test.json'], undefined);}
});
test('success before first binding is never retroactively promoted', async () => {
  const h = harness({delayFiles: true}), operation = h.capture('test.json');
  assert.equal(h.succeed(operation, {...success(), access_token: 'DO_NOT_PERSIST'}), true);
  h.resolveFiles(); await settle(); await h.timersAt(500); await h.timersAt(15000);
  assert.equal(h.puts().length, 0);
  // A subsequent genuinely bound operation persists normally.
  const bound = h.capture('test.json'); h.succeed(bound, {...success(), access_token: 'DO_NOT_PERSIST'}); await h.timersAt(500);
  const body = JSON.parse(h.puts()[0].options.body); assert.equal(body.entries[0].credential_generation, 'generation-A'); assert.equal(body.entries[0].revision, 'revision-A'); assert.equal(h.puts()[0].options.body.includes('DO_NOT_PERSIST'), false);
});
test('operation starting before identities remains unknown even if its success arrives after lookup', async () => {
  const h = harness({delayFiles: true}), old = h.capture('test.json'); h.resolveFiles(); await settle();
  assert.equal(h.succeed(old), false); await h.timersAt(500); assert.equal(h.puts().length, 0);
});
test('normal unchanged OAuth quota refresh saves and reload hydrates without another upload', async () => {
  const h = harness(); await settle(); const operation = h.capture('test.json');
  h.succeed(operation, success(25)); await h.timersAt(500); const entries = JSON.parse(h.puts()[0].options.body).entries;
  assert.equal(entries[0].revision, 'revision-A');
  const reloaded = harness({cached: entries}); await settle(); assert.equal(reloaded.quota.getState().codexQuota['test.json'].windows[0].usedPercent, 25);
  await reloaded.timersAt(500); assert.equal(reloaded.puts().length, 0);
});
test('A starts, B replaces same filename/index/email, identity learns B, then A succeeds', async () => {
  const h = harness({bindings: [{...binding(), email: 'same@example.test'}]}); await settle();
  const A = h.capture('test.json'); h.bindings = [{...binding('B', 'rB'), email: 'same@example.test'}]; h.refresh(); await settle();
  assert.equal(h.succeed(A, success(91)), false); assert.equal(h.quota.getState().codexQuota['test.json'], undefined);
  h.succeed(h.capture('test.json'), success(10)); await h.timersAt(500);
  const entry = JSON.parse(h.puts()[0].options.body).entries[0]; assert.equal(entry.credential_generation, 'B'); assert.equal(entry.revision, 'rB'); assert.equal(entry.state.windows[0].usedPercent, 10); assert.equal(JSON.stringify(entry).includes('same@example'), false);
});
test('reverse A/B completions and ABA revisions never resurrect old quota', async () => {
  for (const reverse of [false, true]) {
    const h = harness(); await settle(); const A = h.capture('test.json');
    h.bindings = [binding('B', 'rB')]; h.refresh(); await settle(); const B = h.capture('test.json');
    if (reverse) {h.succeed(B, success(20)); h.succeed(A, success(90));} else {h.succeed(A, success(90)); h.succeed(B, success(20));}
    assert.equal(h.quota.getState().codexQuota['test.json'].windows[0].usedPercent, 20);
    h.bindings = [binding('generation-A', 'revision-A2')]; h.refresh(); await settle();
    assert.equal(h.succeed(A, success(90)), false); assert.equal(h.succeed(B, success(80)), false);
    h.succeed(h.capture('test.json'), success(5)); await h.timersAt(500);
    assert.equal(h.puts().length, 1); assert.equal(JSON.parse(h.puts()[0].options.body).entries[0].revision, 'revision-A2');
  }
});
test('ABA cannot rehydrate pre-replacement A snapshots in the same tab', async () => {
  const h = harness({cached: [cachedEntry()]}); await settle();
  h.bindings = [binding('B', 'rB')]; h.refresh(); await settle(); assert.equal(h.quota.getState().codexQuota['test.json'], undefined);
  h.bindings = [binding('generation-A', 'revision-A2')]; h.refresh(); await settle(); assert.equal(h.quota.getState().codexQuota['test.json'], undefined);
});
test('unknown batch files remain unknown across unchanged polls and a later identity addition', async () => {
  const h = harness(); await settle(); const batch = h.capture(); h.refresh(); await settle();
  h.bindings.push(binding('new', 'rNew', {key: 'new.json'})); h.refresh(); await settle();
  h.batch(batch, [{provider: 'codex', key: 'new.json', state: success(90)}]); assert.equal(h.quota.getState().codexQuota['new.json'], undefined);
  await h.timersAt(500); assert.equal(h.puts().length, 0);
});
test('unknown direct writes and unknown-at-start operations refuse persistence', async () => {
  const h = harness(); await settle(); h.quota.setState({codexQuota: {'test.json': success()}}); await h.timersAt(500); assert.equal(h.puts().length, 0);
  const unknown = h.capture('unknown.json'); h.bindings.push(binding('new', 'rNew', {key: 'unknown.json'})); h.refresh(); await settle();
  assert.equal(h.succeed(unknown, success(), 'codex', 'unknown.json'), false); await h.timersAt(500); assert.equal(h.puts().length, 0);
});
test('unchanged polls preserve native generations and all-seven batch refresh persists with exact provenance', async () => {
  const bindings = providers.map(provider => binding(`gen-${provider}`, `rev-${provider}`, {provider, key: provider === 'devin' ? 'devin.json\0index1' : `${provider}.json`}));
  const h = harness({bindings}); await settle(); const capture = h.capture();
  const states = {antigravity: {status: 'success', groups: []}, claude: success(), codex: success(), devin: success(), kimi: {status: 'success', rows: []}, meta: {status: 'success', data: {windows: []}}, xai: {status: 'success', billing: {usagePercent: 42}}};
  h.refresh(); await settle(); assert.equal(h.clearCalls.length, 0);
  h.batch(capture, bindings.map(who => ({provider: who.provider, key: who.key, state: states[who.provider]})));
  await h.timersAt(500); const entries = JSON.parse(h.puts()[0].options.body).entries;
  assert.equal(entries.length, 7); for (const entry of entries) {assert.equal(entry.credential_generation, `gen-${entry.provider}`); assert.equal(entry.revision, `rev-${entry.provider}`); assert.equal(Object.hasOwn(entry.state, 'credential_generation'), false);}
  assert.deepEqual(Object.keys(capture).sort(), ['cacheGeneration', 'fileGenerations', 'name']);
});
test('batch filename guards retain unrelated results when another credential is replaced', async () => {
  const other = binding('other', 'rOther', {key: 'other.json'}), h = harness({bindings: [binding(), other]}); await settle(); const batch = h.capture();
  h.bindings = [binding('B', 'rB'), other]; h.refresh(); await settle();
  h.batch(batch, [{provider: 'codex', key: 'test.json', state: success(90)}, {provider: 'codex', key: 'other.json', state: success(10)}]);
  assert.equal(h.quota.getState().codexQuota['test.json'], undefined); assert.equal(h.quota.getState().codexQuota['other.json'].windows[0].usedPercent, 10);
  await h.timersAt(500); assert.deepEqual(JSON.parse(h.puts()[0].options.body).entries.map(entry => entry.key), ['other.json']);
});
test('identity refresh responses cannot install in reverse completion order', async () => {
  const h = harness(); await settle(); h.hold('/identities'); h.refresh(); const stale = h.calls.at(-1);
  h.bindings = [binding('B', 'rB')]; h.refresh(); await settle(); stale.release(); await settle();
  h.succeed(h.capture('test.json')); await h.timersAt(500); assert.equal(JSON.parse(h.puts()[0].options.body).entries[0].credential_generation, 'B');
});
test('cache and normalized payloads recheck bindings after delayed responses', async () => {
  const h = harness(); await settle(); h.cached = [cachedEntry()]; h.snapshots = [{provider: 'codex', auth_index: 'index1', credential_generation: 'generation-A', windows: [{id: 'primary', window_seconds: 18000, used_percent: 99, observed_at: new Date().toISOString()}]}];
  h.hold('/quota/cache'); h.refresh(); await settle(); const delayed = h.calls.find(call => call.release);
  h.bindings = [binding('B', 'rB')]; delayed.release(); await settle(); assert.equal(h.quota.getState().codexQuota['test.json'], undefined);
});
test('revision-only changes fence operations but stable-generation stored history still hydrates', async () => {
  const h = harness({cached: [cachedEntry()]}); await settle(); const old = h.capture('test.json');
  h.bindings = [binding('generation-A', 'rA2')]; h.refresh(); await settle();
  assert.equal(h.succeed(old, success(90)), false); assert.equal(h.quota.getState().codexQuota['test.json'].windows[0].usedPercent, 12);
  await h.timersAt(500); assert.equal(h.puts().length, 0);
  h.succeed(h.capture('test.json'), success(33)); await h.timersAt(500); assert.equal(JSON.parse(h.puts()[0].options.body).entries[0].revision, 'rA2');
});
test('revision churn preserves fresh settled display and observation/window freshness without uploading history', async () => {
  const now = Date.parse('2026-01-02T03:04:05Z'), iso = offset => new Date(now + offset).toISOString();
  const entry = {...cachedEntry(), observed_at: iso(-60000)};
  const snapshot = (used, offset) => ({provider: 'codex', auth_index: 'index1', credential_generation: 'generation-A', windows: [{id: 'primary', window_seconds: 18000, used_percent: used, observed_at: iso(offset)}]});
  const h = harness({now, cached: [entry], snapshots: [snapshot(20, -30000)]}); await settle();
  const live = success(77); h.succeed(h.capture('test.json'), live); const inflight = h.capture('test.json');
  h.bindings = [binding('generation-A', 'rA2')]; h.refresh(); await settle();
  assert.equal(h.quota.getState().codexQuota['test.json'], live, 'older cached and normalized data must not replace fresh live state');
  assert.equal(h.quota.getState().fileGenerations['test.json'], 1);
  assert.equal(h.commit(inflight, () => assert.fail('obsolete callback ran')), false);
  await h.timersAt(500); await h.timersAt(15000); assert.equal(h.puts().length, 0, 'old queued success is not restamped');
  const current = h.capture('test.json'); h.refresh(); await settle();
  assert.equal(h.clearCalls.length, 1, 'unchanged polls must not reset native operations');
  assert.equal(h.commit(current, () => {}), true);
  h.now += 2000; h.snapshots = [snapshot(88, 1000)]; h.refresh(); await settle();
  assert.equal(h.quota.getState().codexQuota['test.json'].windows[0].usedPercent, 88, 'newer normalized data proves the observation sidecar survived');
  h.bindings = [binding('generation-A', 'rA3')]; h.snapshots = [snapshot(80, 500)]; h.refresh(); await settle();
  assert.equal(h.quota.getState().codexQuota['test.json'].windows[0].usedPercent, 88, 'window timestamp survives the next revision');
  h.snapshots = [snapshot(89, 1500)]; h.refresh(); await settle();
  assert.equal(h.quota.getState().codexQuota['test.json'].windows[0].usedPercent, 89);
  await h.timersAt(500); await h.timersAt(15000); assert.equal(h.puts().length, 0);
});
test('revision-only fencing preserves original settled errors and unknown successes but never obsolete loading', async () => {
  for (const state of [{status: 'error', error: 'original provider failure', errorCode: 429, windows: []}, {status: 'idle', windows: []}, {...success(77), originalDisplayField: 'keep locally'}, {status: 'loading', windows: []}]) {
    const h = harness(); await settle(); const old = h.capture('test.json');
    h.quota.setState({codexQuota: {'test.json': state}});
    h.bindings = [binding('generation-A', 'rA2')]; h.refresh(); await settle();
    assert.equal(h.quota.getState().codexQuota['test.json'], state.status === 'loading' ? undefined : state, state.status);
    assert.equal(h.commit(old, () => assert.fail('old callback ran')), false);
    await h.timersAt(500); await h.timersAt(15000); assert.equal(h.puts().length, 0, 'restoring local state must not promote unknown operations');
  }
});
test('revision-only native filename fencing preserves settled unchanged siblings as well', async () => {
  const sibling = binding('claude-A', 'claude-rA', {provider: 'claude'}), h = harness({bindings: [binding(), sibling]}); await settle();
  const codex = success(77), claude = success(88), old = h.capture('test.json');
  h.succeed(old, codex); h.succeed(old, claude, 'claude');
  h.bindings = [binding('generation-A', 'rA2'), sibling]; h.refresh(); await settle();
  assert.equal(h.quota.getState().codexQuota['test.json'], codex);
  assert.equal(h.quota.getState().claudeQuota['test.json'], claude);
  assert.equal(h.succeed(old), false);
  await h.timersAt(500); assert.equal(h.puts().length, 0);
});
test('a true identity change in any same-filename sibling forbids revision-only preservation for that file', async () => {
  const sibling = binding('claude-A', 'claude-rA', {provider: 'claude'});
  for (const replacement of [{...sibling, credential_generation: 'claude-B', revision: 'claude-rB'}, {...sibling, auth_index: 'index2', revision: 'claude-rB'}, null]) for (const reverse of [false, true]) {
    const h = harness({bindings: reverse ? [sibling, binding()] : [binding(), sibling], cached: [cachedEntry(), cachedEntry(sibling)]}); await settle();
    const old = h.capture('test.json'); h.succeed(old, success(77)); h.succeed(old, success(88), 'claude');
    h.bindings = [binding('generation-A', 'rA2'), ...(replacement ? [replacement] : [])]; h.refresh(); await settle();
    assert.equal(h.quota.getState().codexQuota['test.json'], undefined);
    assert.equal(h.quota.getState().claudeQuota['test.json'], undefined);
    assert.equal(h.succeed(old), false);
    await h.timersAt(500); assert.equal(h.puts().length, 0);
  }
});
test('failed PUT retries preserve original binding only while current', async () => {
  const h = harness(); await settle(); h.putStatus = 500; h.succeed(h.capture('test.json')); await h.timersAt(500);
  assert.equal(h.puts().length, 1); h.putStatus = 200; await h.timersAt(15000); assert.equal(h.puts().length, 2);
  assert.equal(h.puts()[0].options.body, h.puts()[1].options.body);
  h.putStatus = 500; h.succeed(h.capture('test.json')); await h.timersAt(500);
  h.bindings = [binding('B', 'rB')]; h.refresh(); await settle(); h.putStatus = 200; await h.timersAt(15000); assert.equal(h.puts().length, 3);
});
test('failed PUT completion after replacement cannot requeue old batch', async () => {
  const h = harness(); await settle(); h.hold('/quota/cache', 'PUT'); h.succeed(h.capture('test.json')); await h.timersAt(500); const put = h.puts()[0];
  h.bindings = [binding('B', 'rB')]; h.refresh(); await settle(); put.release({status: 500}); await settle(); await h.timersAt(15000); assert.equal(h.puts().length, 1);
});
test('late A PUT 409 cannot erase replacement B display or discard its queued upload', async () => {
  const h = harness(); await settle(); h.hold('/quota/cache', 'PUT');
  h.succeed(h.capture('test.json'), success(90)); await h.timersAt(500); const failed = h.puts()[0];
  h.bindings = [binding('B', 'rB')]; h.refresh(); await settle();
  const B = h.capture('test.json'), live = success(10); h.succeed(B, live);
  const clears = h.clearCalls.length; failed.release({status: 409}); await settle();
  assert.equal(h.quota.getState().codexQuota['test.json'], live);
  assert.equal(h.clearCalls.length, clears, 'stale A failure must not reset B native generation');
  assert.equal(h.commit(B, () => {}), true);
  await h.timersAt(500); await h.timersAt(15000);
  assert.equal(h.puts().length, 2, 'B must upload, A must never retry');
  const entries = h.puts().map(call => JSON.parse(call.options.body).entries[0]);
  assert.deepEqual(entries.map(entry => [entry.credential_generation, entry.revision, entry.state.windows[0].usedPercent]), [['generation-A', 'revision-A', 90], ['B', 'rB', 10]]);
});
test('late 409 never drops newer valid same-filename pending work even when the binding is unchanged', async () => {
  const h = harness(); await settle(); h.hold('/quota/cache', 'PUT');
  h.succeed(h.capture('test.json'), success(90)); await h.timersAt(500); const failed = h.puts()[0];
  const newer = h.capture('test.json'), live = success(10); h.succeed(newer, live); await h.timersAt(500);
  failed.release({status: 409}); await settle();
  assert.equal(h.quota.getState().codexQuota['test.json'], live);
  assert.equal(h.clearCalls.length, 0);
  assert.equal(h.commit(newer, () => {}), true);
  await h.timersAt(15000); assert.equal(h.puts().length, 2);
  const entries = h.puts().map(call => JSON.parse(call.options.body).entries[0]);
  assert.deepEqual(entries.map(entry => [entry.revision, entry.state.windows[0].usedPercent]), [['revision-A', 90], ['revision-A', 10]]);
  await h.timersAt(15000); assert.equal(h.puts().length, 2);
});
test('late mixed-batch 409 clears only still-owned filenames while replacement work waits behind the PUT', async () => {
  const other = binding('other', 'rOther', {key: 'other.json'}), h = harness({bindings: [binding(), other]}); await settle();
  h.hold('/quota/cache', 'PUT'); h.batch(h.capture(), [{provider: 'codex', key: 'test.json', state: success(90)}, {provider: 'codex', key: 'other.json', state: success(80)}]);
  await h.timersAt(500); const failed = h.puts()[0];
  h.bindings = [binding('B', 'rB'), other]; h.refresh(); await settle();
  const live = success(10); h.succeed(h.capture('test.json'), live); await h.timersAt(500);
  assert.equal(h.puts().length, 1, 'new debounce cannot flush during the held request');
  failed.release({status: 409}); await settle();
  assert.equal(h.quota.getState().codexQuota['test.json'], live);
  assert.equal(h.quota.getState().codexQuota['other.json'], undefined, 'still-owned conflict retains the original terminal clear');
  await h.timersAt(15000); assert.equal(h.puts().length, 2);
  const entries = JSON.parse(h.puts()[1].options.body).entries;
  assert.deepEqual(entries.map(entry => [entry.key, entry.credential_generation, entry.state.windows[0].usedPercent]), [['test.json', 'B', 10]]);
  await h.timersAt(15000); assert.equal(h.puts().length, 2, 'neither failed batch entry is retried');
});
test('late 409 is terminal across revision-only, ABA and native reset fences without dropping newer work', async () => {
  for (const transition of ['revision', 'ABA', 'reset']) {
    const h = harness(); await settle(); h.hold('/quota/cache', 'PUT');
    const A = h.capture('test.json'); h.succeed(A, success(90)); await h.timersAt(500); const failed = h.puts()[0];
    if (transition === 'reset') h.clear(['test.json']);
    else {
      h.bindings = [transition === 'revision' ? binding('generation-A', 'rA2') : binding('B', 'rB')]; h.refresh(); await settle();
      if (transition === 'ABA') {h.bindings = [binding()]; h.refresh(); await settle();}
    }
    const current = h.capture('test.json'), live = success(10); h.succeed(current, live);
    const clears = h.clearCalls.length; failed.release({status: 409}); await settle();
    assert.equal(h.quota.getState().codexQuota['test.json'], live, transition);
    assert.equal(h.clearCalls.length, clears, transition);
    assert.equal(h.succeed(A, success(99)), false, transition);
    await h.timersAt(500); await h.timersAt(15000); assert.equal(h.puts().length, 2, transition);
    const entry = JSON.parse(h.puts()[1].options.body).entries[0];
    assert.equal(entry.revision, transition === 'revision' ? 'rA2' : 'revision-A');
    assert.equal(entry.state.windows[0].usedPercent, 10);
  }
});
test('409 drops entire stale batch, refreshes identities, never re-stamps or retries', async () => {
  const h = harness(); await settle(); h.putStatus = 409; h.succeed(h.capture('test.json')); h.bindings = [binding('B', 'rB')]; await h.timersAt(500);
  h.putStatus = 200; await h.timersAt(15000); assert.equal(h.puts().length, 1); assert.equal(h.quota.getState().codexQuota['test.json'], undefined);
  h.succeed(h.capture('test.json'), success(10)); await h.timersAt(500); assert.equal(h.puts().length, 2); assert.equal(JSON.parse(h.puts()[1].options.body).entries[0].credential_generation, 'B');
});
test('409 does not permanently disable new operations for unchanged valid batch siblings', async () => {
  const h = harness(); await settle(); const old = h.capture('test.json'); h.putStatus = 409; h.succeed(old); await h.timersAt(500);
  assert.equal(h.succeed(old), false); h.putStatus = 200; h.succeed(h.capture('test.json'), success(12)); await h.timersAt(500);
  assert.equal(h.puts().length, 2); assert.equal(JSON.parse(h.puts()[1].options.body).entries[0].state.windows[0].usedPercent, 12);
});
test('per-file reset while PUT fails prevents requeue without clearing unrelated pending quota', async () => {
  const other = binding('other', 'rOther', {key: 'other.json'}), h = harness({bindings: [binding(), other]}); await settle();
  h.hold('/quota/cache', 'PUT'); h.succeed(h.capture('test.json')); await h.timersAt(500); const failed = h.puts()[0];
  h.succeed(h.capture('other.json'), success(12), 'codex', 'other.json'); h.clear(['test.json']); failed.release({status: 500}); await settle();
  await h.timersAt(15000); assert.equal(h.puts().length, 2); assert.deepEqual(JSON.parse(h.puts()[1].options.body).entries.map(entry => entry.key), ['other.json']);
});
test('global and file resets discard retry/window state and preserve native generation scopes', async () => {
  const other = binding('other', 'rOther', {key: 'other.json'}), h = harness({bindings: [binding(), other], cached: [cachedEntry(), cachedEntry(other)]}); await settle();
  const A = h.capture('test.json'), B = h.capture('other.json'), global = h.capture();
  h.succeed(A); h.clear(['test.json']); assert.equal(h.succeed(A), false); assert.equal(h.commit(global, () => {}), false); assert.equal(h.succeed(B, success(), 'codex', 'other.json'), true);
  h.refresh(); await settle(); assert.equal(h.quota.getState().codexQuota['test.json'], undefined);
  h.clear(); assert.equal(h.succeed(B, success(), 'codex', 'other.json'), false); await h.timersAt(500); assert.equal(h.puts().length, 0);
  h.refresh(); await settle(); assert.deepEqual(h.quota.getState().codexQuota, {});
});
test('deletion revokes pending, hydrated and native inflight state', async () => {
  const h = harness({cached: [cachedEntry()]}); await settle(); const operation = h.capture('test.json'); h.succeed(operation);
  h.bindings = []; h.refresh(); await settle(); assert.equal(h.quota.getState().codexQuota['test.json'], undefined); assert.equal(h.succeed(operation), false);
  await h.timersAt(500); assert.equal(h.puts().length, 0);
});
test('unknown or duplicate binding fields fail closed without client fingerprinting', async () => {
  for (const bindings of [[{...binding(), credential_generation: undefined}], [{...binding(), revision: undefined}], [binding(), binding('B', 'rB')]]) {
    const h = harness({bindings, cached: [cachedEntry()]}); await settle(); h.succeed(h.capture('test.json')); await h.timersAt(500); assert.equal(h.puts().length, 0);
  }
});
test('logout aborts synchronization and late callbacks cannot enter a new auth session', async () => {
  const h = harness({cached: [cachedEntry()]}); await settle(); const old = h.capture('test.json');
  h.hold('/quota/cache'); h.refresh(); await settle(); const delayed = h.calls.find(call => call.release);
  h.auth.setState({isAuthenticated: false, connectionStatus: 'disconnected', managementKey: ''}); assert.equal(delayed.options.signal.aborted, true);
  delayed.release(); await settle(); assert.equal(h.window.CPAQuotaPersistence.status.state, 'waiting-for-login'); assert.equal(h.succeed(old), false);
  h.auth.setState({isAuthenticated: true, connectionStatus: 'connected', managementKey: 'new-key'}); await settle(); assert.equal(h.succeed(old), false); await h.timersAt(500); assert.equal(h.puts().length, 0);
});
