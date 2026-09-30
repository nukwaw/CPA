'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const C = require('./stats-core.js');
const bridge = require('./management-bridge.js');
const {statusNotice, httpErrorMessage} = require('./stats.js');
const read = name => fs.readFileSync(require('node:path').join(__dirname, name), 'utf8');
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
test('legacy unattributed-header status is ignored, not fabricated', () => {
  // The backend no longer tracks a skipped-header counter: every admitted header
  // sample carries producer-stamped account facts, so there is nothing to count.
  // A stale/legacy status field must not manufacture a diagnostic.
  assert.deepEqual(statusNotice({skipped_quota_headers: 3}), {text: '', error: false});
  const combined = statusNotice({collection_enabled: false, write_failures: 1, dropped_events: 2, pending_events: 3, local_capacity: {rejected_writes: 1}, skipped_quota_headers: 4});
  assert.equal(combined.error, true);
  for (const pattern of [/collection is off/, /1 persistence writes failed/, /2 usage\/quota records/, /3 admitted records/, /capacity boundary/]) assert.match(combined.text, pattern);
  assert.doesNotMatch(combined.text, /quota-header observations/);
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
test('filters encode untrusted model IDs and never introduce a removed credential parameter', () => {
  const params = C.query({range: '24h', model: '<model>&token=secret', provider: 'codex', account: 'a@example.test'}, 86400000);
  assert.equal(params.get('model'), '<model>&token=secret');
  assert.equal(params.get('account'), 'a@example.test');
  assert.equal(params.has('token'), false);
  assert.equal(params.has('auth_index'), false);
  assert.equal(params.get('from'), '1970-01-01T00:00:00.000Z');
});
test('invalid custom ranges fail before a request', () => {
  for (const range of [{from: '', to: ''}, {from: '2026-02-01', to: '2026-01-01'}, {from: 'bad', to: '2026-01-01'}]) assert.throws(() => C.query({range: 'custom', ...range}), /valid custom range/);
});
test('throughput and cache rate follow the keeper dashboard and stay unknown without a base', () => {
  // Keeper: complete output tokens over total request time, not decode-only speed.
  assert.equal(C.speed(100, 2000), 50);
  assert.equal(C.speedText(100, 2000), '50.0 tok/s');
  assert.equal(C.speedText(5000, 1000), '5,000 tok/s');
  for (const [tokens, latency] of [[0, 1000], [100, 0], [null, 1000], [100, null], [100, -1]]) assert.equal(C.speedText(tokens, latency), '—');
  // Keeper: cache reads over input tokens.
  assert.equal(C.cacheRate(200, 1000), 20);
  assert.equal(C.cacheRateText(200, 1000), '20.0%');
  for (const [read, input] of [[10, 0], [10, null]]) assert.equal(C.cacheRateText(read, input), '—');
  // A reported zero is a real measurement: no input was served from cache.
  assert.equal(C.cacheRateText(0, 1000), '0.0%');
  assert.equal(C.cacheRateText(2000, 1000), '100.0%');
});
test('a card is titled by its credential file, never by the composite identity key', () => {
  // One card is one credential file, which is the unit the control panel's own quota
  // page uses. The recorded account is shown alongside it as a fact, not as the title.
  assert.equal(C.credentialName({provider: 'kimi', key: 'kimi.json', account: 'device-42', account_kind: 'device_id'}), 'kimi.json');
  assert.equal(C.credentialName({provider: 'claude', key: 'claude.json', account: 'person@example.test', account_kind: 'email'}), 'claude.json');
  // One devin file can expose more than one credential identity, so the suffix that
  // tells those identities apart is shown with the file.
  assert.equal(C.credentialName({provider: 'devin', key: 'devin.json\u000075c2e8f8a9535aea'}), 'devin.json \u00b7 75c2e8f8a9535aea');
  // A card with no file falls back to its recorded facts rather than inventing one.
  assert.equal(C.credentialName({provider: 'kimi', account: 'device-42', account_kind: 'device_id'}), 'device device-42');
  assert.equal(C.credentialName({provider: 'kimi'}), 'kimi');
  assert.equal(C.credentialName(undefined), 'Credential');
  // The composite key's NUL separator must never surface raw.
  for (const title of [C.credentialName({provider: 'devin', key: 'devin.json\u0000i'}), C.credentialName({provider: 'devin', key: 'devin.json\u0000i', account: 'p@example.test'}), C.credentialName(undefined)]) {
    assert.ok(!title.includes('\u0000'), `title leaked the composite key: ${JSON.stringify(title)}`);
  }
});
test('a card names the one credential file it represents', () => {
  assert.equal(C.credentialFiles({provider: 'claude', key: 'claude.json'}), 'claude.json');
  // The devin suffix is a credential identity, not a separator artifact, so it is
  // reported with the file instead of being stripped away.
  assert.equal(C.credentialFiles({provider: 'devin', key: 'devin.json\u0000suffix'}), 'devin.json \u00b7 suffix');
  assert.equal(C.credentialFiles({provider: 'claude'}), '');
});
test('the account fact, not a credential index, is what a card describes', () => {
  assert.equal(C.describeCredential({provider: 'claude', account: 'person@example.test', account_kind: 'email'}), 'claude · person@example.test');
  assert.equal(C.describeCredential({provider: 'kimi', account: 'device-42', account_kind: 'device_id'}), 'kimi · device device-42');
  assert.equal(C.describeCredential({provider: 'codex', account: 'group-1', account_kind: 'team'}), 'codex · team group-1');
  // A credential that exposes no account property says so instead of inventing one.
  assert.equal(C.describeCredential({provider: 'codex', account: '', account_kind: ''}), 'codex');
  assert.equal(C.describeCredential({provider: 'codex', account: '', account_kind: '', observed_at: '2026-01-02T05:00:00Z'}), `codex · observed ${new Date('2026-01-02T05:00:00Z').toLocaleString()}`);
  assert.equal(C.describeCredential(undefined), 'Unknown');
});
test('provider quota rows become labelled percentage lines for every supported shape', () => {
  // Claude publishes windows with explicit labels and reset labels.
  const claude = C.quotaLines({state: {status: 'success', windows: [{id: 'five-hour', label: '5-hour limit', usedPercent: 42.5, resetLabel: '12/31, 21:00'}]}});
  assert.deepEqual(claude, [{id: 'five-hour', label: '5-hour limit', percent: 42.5, hint: '12/31, 21:00', source: ''}]);
  // Kimi publishes rows with an untranslated key, params and used/limit counters.
  const kimi = C.quotaLines({state: {status: 'success', rows: [
    {id: 'limit-0', labelKey: 'kimi_quota.limit_window', labelParams: {duration: '5h'}, used: 42500, limit: 100000},
    {id: 'summary', labelKey: 'kimi_quota.weekly_limit', used: 250000, limit: 1000000, resetHint: '1193d 5h'},
  ]}});
  assert.deepEqual(kimi.map(line => [line.label, line.percent]), [['5h limit', 42.5], ['Weekly limit', 25]]);
  assert.equal(kimi[1].hint, '1193d 5h');
  // A remaining fraction is inverted, a zero limit or missing counter is unknown, and
  // a negative reported value is clamped rather than shown as a negative bar.
  const derived = C.quotaLines({state: {groups: [{id: 'g', label: 'Group', buckets: [{id: 'b1', remainingFraction: 0.25}, {id: 'b2', remainingFraction: 1.5}]},], rows: [{id: 'r1', used: 1, limit: 0}, {id: 'r2', used: -5, limit: 100}]}});
  assert.deepEqual(derived.map(line => [line.label, line.percent]), [['r1', null], ['r2', 0], ['Group · b1', 75], ['Group · b2', 0]]);
});
test('two bindings sharing one account keep one card EACH, as the control panel does', () => {
  const bindings = [
    {provider: 'claude', key: 'claude-a.json', account: 'shared@example.test', account_kind: 'email', auth_index: 'index-a'},
    {provider: 'claude', key: 'claude-b.json', account: 'shared@example.test', account_kind: 'email', auth_index: 'index-b'},
    {provider: 'kimi', key: 'kimi.json\u0000device-42', account: 'device-42', account_kind: 'device_id', auth_index: 'device-42'},
  ];
  const cards = C.summarizeCredentials([], [], bindings);
  assert.equal(cards.length, 3, 'one card per credential file');
  const claude = cards.filter(card => card.provider === 'claude');
  assert.equal(claude.length, 2, 'two files serving one account stay two cards');
  assert.deepEqual(claude.map(C.credentialName), ['claude-a.json', 'claude-b.json']);
  // The shared account stays a recorded fact on both cards, so (provider, account)
  // remains usable for filtering and grouping without collapsing the cards.
  for (const card of claude) {
    assert.equal(card.account, 'shared@example.test');
    assert.equal(card.account_kind, 'email');
  }
  assert.equal(C.describeCredential(claude[0]), 'claude · shared@example.test · claude-a.json');
  // Each card is addressed by its own credential, so each refreshes on its own.
  assert.deepEqual(claude.map(card => card.indices[0]), ['index-a', 'index-b']);
  assert.equal(cards.find(card => card.provider === 'kimi').account_kind, 'device_id');
});
test('saved states land on their own credential file, and an unknown account never merges', () => {
  const entries = [
    {provider: 'codex', key: 'codex-a.json', account: 'shared@example.test', account_kind: 'email', observed_at: '2026-01-02T03:00:00Z', state: {windows: [{id: 'five-hour', label: '5-hour limit', usedPercent: 11}]}},
    {provider: 'codex', key: 'codex-b.json', account: 'shared@example.test', account_kind: 'email', observed_at: '2026-01-02T04:00:00Z', state: {windows: [{id: 'five-hour', label: '5-hour limit', usedPercent: 22}]}},
    {provider: 'codex', key: 'codex-anon-a.json', account: '', account_kind: '', observed_at: '2026-01-02T05:00:00Z', state: {windows: [{id: 'weekly', label: 'Weekly limit', usedPercent: 33}]}},
    {provider: 'codex', key: 'codex-anon-b.json', account: '', account_kind: '', state: {windows: [{id: 'weekly', label: 'Weekly limit', usedPercent: 44}]}},
    {provider: 'claude', key: 'claude.json', account: '', account_kind: '', state: {}},
  ];
  const cards = C.summarizeCredentials(entries, [], []);
  const codex = cards.filter(card => card.provider === 'codex');
  assert.equal(codex.length, 4, 'one card per credential file');
  // Two files serving one account are two credentials, so neither hides the other.
  const shared = codex.filter(card => card.account === 'shared@example.test');
  assert.equal(shared.length, 2);
  assert.deepEqual(shared.map(card => card.lines[0].percent), [11, 22], 'each file keeps its own saved state');
  assert.equal(shared.find(card => card.key === 'codex-b.json').observed_at, '2026-01-02T04:00:00Z');
  assert.ok(shared.every(card => card.account_kind === 'email'));
  // Two credentials that expose NO account property are not known to be the same
  // account, so their values are never merged into one card. Keying them by provider
  // alone would present one credential's numbers as if they covered the other.
  const unknown = codex.filter(card => card.account === '');
  assert.equal(unknown.length, 2);
  assert.deepEqual(unknown.map(card => card.lines[0].percent), [33, 44]);
  assert.ok(unknown.every(card => card.account_kind === ''));
  // Grouping by provider alone must not merge different providers.
  assert.equal(cards.filter(card => card.provider === 'claude').length, 1);
  assert.deepEqual(C.summarizeCredentials([], [], []), []);
});
test('a normalized snapshot merges into the card carrying the same (provider, account)', () => {
  const entries = [{provider: 'codex', key: 'codex.json', account: 'person@example.test', account_kind: 'email', observed_at: '2026-01-02T03:00:00Z', state: {}}];
  const snapshots = [
    {provider: 'codex', account: 'person@example.test', account_kind: 'email', observed_at: '2026-01-02T05:00:00Z', windows: [{id: 'primary', label: 'Primary window', used_percent: 61.5}]},
    {provider: 'codex', account: 'other@example.test', account_kind: 'email', observed_at: '2026-01-02T06:00:00Z', windows: [{id: 'primary', label: 'Primary window', used_percent: 90}]},
  ];
  const cards = C.summarizeCredentials(entries, snapshots, []);
  assert.equal(cards.length, 2);
  const matched = cards.find(card => card.account === 'person@example.test');
  assert.equal(matched.observed_at, '2026-01-02T05:00:00Z');
  assert.deepEqual(matched.lines, [{id: 'primary', label: 'Primary window', percent: 61.5, hint: '', source: 'primary'}]);
  // The unmatched snapshot still becomes its own card instead of polluting the first.
  assert.deepEqual(cards.find(card => card.account === 'other@example.test').lines.map(line => line.percent), [90]);
});
test('a newer normalized observation replaces a saved window value, an older one never does', () => {
  // A manual refresh (or any earlier observation) fills the credential's saved
  // display state. A later provider response records backend window ids for the
  // same windows, so the newer observation must win on the window it carries,
  // must leave a saved-only window alone, and must not appear twice.
  const entry = {provider: 'claude', key: 'claude.json', account: 'person@example.test', account_kind: 'email', observed_at: '2026-01-02T03:00:00Z', state: {status: 'success', windows: [
    {id: 'five-hour', label: '5h', usedPercent: 10},
    {id: 'saved-only', label: 'Saved only', usedPercent: 42},
  ]}};
  const snapshot = {provider: 'claude', account: 'person@example.test', account_kind: 'email', observed_at: '2026-01-02T05:00:00Z', windows: [{id: 'five_hour', label: '5h', used_percent: 91}]};
  const fresh = C.summarizeCredentials([entry], [snapshot], [])[0];
  assert.deepEqual(fresh.lines.map(line => [line.id, line.percent]), [['five-hour', 91], ['saved-only', 42]]);
  assert.equal(fresh.observed_at, '2026-01-02T05:00:00Z');
  // An observation older than the saved state leaves every saved value in place.
  const older = C.summarizeCredentials([entry], [{...snapshot, observed_at: '2026-01-02T01:00:00Z'}], [])[0];
  assert.deepEqual(older.lines.map(line => [line.id, line.percent]), [['five-hour', 10], ['saved-only', 42]]);
  assert.equal(older.observed_at, '2026-01-02T03:00:00Z');
});
test('codex backend window ids map onto the saved display windows', () => {
  const entry = {provider: 'codex', key: 'codex.json', account: '', observed_at: '2026-01-02T03:00:00Z', state: {status: 'success', windows: [
    {id: 'five-hour', label: '5-hour limit', usedPercent: 10},
    {id: 'weekly', label: 'Weekly limit', usedPercent: 20},
  ]}};
  const snapshot = {provider: 'codex', account: '', observed_at: '2026-01-02T05:00:00Z', windows: [
    {id: 'primary', label: 'Primary', used_percent: 42, window_seconds: 18000},
    {id: 'secondary', label: 'Secondary', used_percent: 17, window_seconds: 604800},
  ]};
  const card = C.summarizeCredentials([entry], [snapshot], [])[0];
  assert.deepEqual(card.lines.map(line => [line.id, line.percent]), [['five-hour', 42], ['weekly', 17]]);
  // History is recorded under the backend id, so each line must carry it; a line
  // whose value only exists as saved display state has no backend id to query.
  assert.deepEqual(card.lines.map(line => line.source), ['primary', 'secondary']);
  const savedOnly = C.summarizeCredentials([{provider: 'codex', key: 'a.json', state: {windows: [{id: 'weekly', usedPercent: 5}]}}], [], [])[0];
  assert.deepEqual(savedOnly.lines.map(line => [line.id, line.source]), [['weekly', '']]);
});
test('lines that legitimately share an id are never collapsed by the snapshot merge', () => {
  // A grouped provider can repeat the same bucket id inside different groups; those
  // are two separate lines and must both survive. Only a Window snapshot line is
  // updated in place, and only against the window line it maps to.
  const saved = {provider: 'antigravity', observed_at: '2026-01-01T00:00:00Z', state: {status: 'success', groups: [
    {id: 'g1', label: 'G1', buckets: [{id: 'shared', remainingFraction: 0.5}]},
    {id: 'g2', label: 'G2', buckets: [{id: 'shared', remainingFraction: 0.25}]},
  ]}};
  assert.deepEqual(C.quotaLines(saved, null).map(line => [line.id, line.percent]), [['shared', 50], ['shared', 75]]);
  assert.deepEqual(C.quotaLines(saved, {observed_at: '2025-12-31T00:00:00Z', windows: [{id: 'shared', used_percent: 1}]}).map(line => [line.id, line.percent]), [['shared', 50], ['shared', 75]]);
  // A cache already written with backend window ids is updated in place rather than
  // rendered twice, for both a mapped and an unmapped window.
  const claude = {provider: 'claude', observed_at: '2026-01-01T00:00:00Z', state: {status: 'success', windows: [{id: 'five_hour', usedPercent: 1}]}};
  assert.deepEqual(C.quotaLines(claude, {observed_at: '2026-01-02T00:00:00Z', windows: [{id: 'five_hour', used_percent: 88}]}).map(line => [line.id, line.percent]), [['five_hour', 88]]);
  const durationless = {provider: 'codex', observed_at: '2026-01-01T00:00:00Z', state: {status: 'success', windows: [{id: 'primary', usedPercent: 1}]}};
  assert.deepEqual(C.quotaLines(durationless, {observed_at: '2026-01-02T00:00:00Z', windows: [{id: 'primary', used_percent: 77}]}).map(line => [line.id, line.percent]), [['primary', 77]]);
});
test('a card offers only windows it can name a backend id for, once each', () => {
  // Quota history is recorded under the backend window id, so a window offered in
  // the side window must carry that id. The card displays the control panel's
  // display id, so the two namespaces must not both reach the dropdown: doing that
  // listed every window twice, and the display-id copy charted nothing.
  const entry = {provider: 'claude', key: 'claude.json', account: '', observed_at: '2026-01-02T03:00:00Z', state: {status: 'success', windows: [
    {id: 'five-hour', label: '5h limit', usedPercent: 10},
    {id: 'seven-day', label: 'Weekly limit', usedPercent: 20},
  ]}};
  const snapshot = {provider: 'claude', account: '', observed_at: '2026-01-02T05:00:00Z', windows: [
    {id: 'five_hour', label: '5h', used_percent: 91},
    {id: 'seven_day', label: 'Weekly', used_percent: 93},
  ]};
  const card = C.summarizeCredentials([entry], [snapshot], [])[0];
  const options = new Map();
  for (const line of card.lines) if (line.source) options.set(line.source, line.label || line.source);
  assert.deepEqual([...options], [['five_hour', '5h limit'], ['seven_day', 'Weekly limit']], 'one option per recorded window, labelled as the card labels it');
  // A window known only from saved display state has no recorded history, so it is
  // offered nowhere and cannot produce a misleading empty chart.
  const savedOnly = C.summarizeCredentials([{provider: 'claude', key: 'claude.json', account: '', state: {status: 'success', windows: [{id: 'five-hour', label: '5h limit', usedPercent: 10}]}}], [], [])[0];
  assert.deepEqual(savedOnly.lines.map(line => line.source), [''], 'saved-state-only lines carry no backend id');
  assert.equal([...savedOnly.lines].filter(line => line.source).length, 0);
});
test('the side window splits its two sources across two tabs and reads only the visible one', () => {
  const html = read('stats.html'), source = read('stats.js');
  // The markup must carry exactly the two tabs and one panel each, with the stream
  // tab's panel hidden so the quota tab is what a reader sees first.
  assert.deepEqual([...html.matchAll(/data-dialog-tab="(\w+)"/g)].map(match => match[1]), ['quota', 'requests']);
  assert.match(html, /id="dialog-panel-quota" class="dialog-panel"/);
  assert.match(html, /id="dialog-panel-requests" class="dialog-panel" hidden/);
  // The pause control belongs to the stream, so it lives on the stream tab.
  const requestsPanel = html.slice(html.indexOf('id="dialog-panel-requests"'));
  assert.match(requestsPanel, /id="quota-pause"/, 'the stream pause control must sit on the stream tab');
  const quotaPanel = html.slice(html.indexOf('id="dialog-panel-quota"'), html.indexOf('id="dialog-panel-requests"'));
  assert.doesNotMatch(quotaPanel, /id="quota-pause"/, 'the quota tab must not carry the stream control');
  // Each tab is backed by one endpoint, and only the visible tab is read: the
  // summary and the event stream must not share a request.
  assert.match(source, /if \(stream\) \{\n\s*const page = await api\(`events\?\$\{params\}&limit=25`\)/);
  assert.match(source, /const summary = await api\(`quota\/summary\?\$\{params\}`\)/);
  assert.doesNotMatch(source, /Promise\.all\(\[api\(`quota\/summary/, 'the two sources must not be fetched together');
  // Opening always starts on the quota tab, whatever was selected before.
  assert.match(source, /state\.dialogPaused = false; state\.dialogTab = 'quota'; renderDialogTab\(\)/);
  // Pausing the stream must not stop the quota tab refreshing.
  assert.match(source, /if \(state\.dialogPaused && state\.dialogTab === 'requests'\) return;/);
});
test('the panel refreshes a settled card from a newer snapshot instead of skipping it', () => {
  const saved = Date.parse('2026-01-02T03:00:00Z'), window = {id: 'five_hour', used_percent: 91};
  const current = {status: 'success', windows: [{id: 'five-hour', label: '5h', usedPercent: 10, resetLabel: 'saved reset'}]};
  // The stored state's own observation time is the floor, so an older snapshot
  // changes nothing while a newer one updates the window it reports.
  assert.equal(bridge.overlay('claude', current, {observed_at: '2026-01-02T02:00:00Z', windows: [window]}, saved).changed, false);
  const result = bridge.overlay('claude', current, {observed_at: '2026-01-02T05:00:00Z', windows: [window]}, saved);
  assert.equal(result.changed, true);
  assert.equal(result.state.windows.find(item => item.id === 'five-hour').usedPercent, 91);
  // A settled card must no longer be skipped outright, and the floor must come
  // from the stored observation rather than from this session alone.
  const source = read('management-bridge.js');
  assert.doesNotMatch(source, /existing\?\.status === 'success' && !observation\.has\(id\)\) continue/);
  assert.match(source, /storedObservation\.get\(id\)/);
});
test('the dashboard groups by account facts and never reads a removed credential identity', () => {
  const source = read('stats.js'), core = read('stats-core.js'), html = read('stats.html'), bridgeSource = read('management-bridge.js');
  // Cards are identified by the credential file, matching the control panel's own
  // quota page. The account is recorded as a fact on each card, not used as the key.
  assert.match(core, /const credentialKey = \(provider, key\) =>/);
  assert.match(source, /C\.summarizeCredentials\(state\.quota\.entries, state\.quota\.snapshots, C\.arrays\(identities, 'bindings'\)\)/);
  // A snapshot reaches a card through the account facts, matched in the core. The
  // window choices a card offers come from the backend ids its own lines recorded,
  // so the dropdown can never offer a window the card does not display.
  assert.match(core, /const snapshotFor = \(provider, account\) => snapshots\.find\(item => item\.provider === provider && String\(item\.account \|\| ''\) === String\(account \|\| ''\)\)/);
  assert.match(source, /if \(line\.source\) windows\.set\(line\.source, line\.label \|\| line\.source\)/);
  assert.doesNotMatch(source, /credential\.windows = C\.arrays\(snapshot/, 'window choices must not come from a separate snapshot lookup');
  // The consumer must use only that prepared list. Re-merging the rendered line ids
  // here is what listed every window twice, with the display-id copy charting
  // nothing, so the dialog must not consult the lines again.
  assert.match(source, /for \(const window of credential\.windows \|\| \[\]\) options\.push/);
  assert.doesNotMatch(source, /options\.push\(\[window\.id[^\n]*credential\.lines/, 'the dialog must not derive options from rendered line ids');
  for (const [name, text] of [['stats.js', source], ['stats-core.js', core], ['stats.html', html]]) {
    assert.doesNotMatch(text, /credential_generation/, `${name} must not use a credential generation`);
    assert.doesNotMatch(text, /revision/, `${name} must not use a revision fence`);
  }
  // A live credential may be ADDRESSED by its transient index so the original quota
  // handler can be asked to refresh it. That is correlation, not identity: the index
  // must never enter grouping, the markup, a rendered label, or storage.
  assert.doesNotMatch(html, /auth_index/, 'the markup must not carry a credential index');
  assert.match(core, /const credentialKey = \(provider, key\) =>/);
  assert.doesNotMatch(core, /credentialKey\([^)]*index/, 'a credential index must not participate in grouping');
  assert.doesNotMatch(core, /ensure\([^)]*index/, 'a credential index must not participate in grouping');
  assert.doesNotMatch(core, /describeCredential = credential => `\$\{credential\?\.[^`]*indices/, 'the index is never rendered');
  assert.match(core, /credential\.indices\.push\(index\)/, 'the index is collected for addressing only');
  // The bridge may correlate a native request with the transient index, but it must
  // never persist it, key a map by it, or upload it.
  assert.doesNotMatch(bridgeSource, /credential_generation|revision/, 'the bridge must not use a removed credential identity');
  assert.doesNotMatch(bridgeSource, /slot\([^)]*auth_index/, 'the bridge must not key a map by the credential index');
  assert.match(bridgeSource, /const uploadEntry = entry => \(\{provider: entry\.provider, key: entry\.key, account: entry\.account, account_kind: entry\.account_kind/);
  assert.match(bridgeSource, /body: \{entries: batch\.map\(\(\[, value\]\) => uploadEntry\(value\.entry\)\)\}/);
  // The account stays a recorded fact and still scopes the side window, so
  // (provider, account) remains usable for grouping without keying the cards.
  assert.match(core, /if \(!credential\.account && account\) credential\.account = account/);
  // The side window scopes its stream and its value estimate by the recorded facts.
  assert.match(source, /const params = new URLSearchParams\(\{provider: credential\.provider, from: range\.from, to: range\.to\}\)/);
  assert.match(source, /if \(credential\.account\) params\.set\('account', credential\.account\)/);
  // Both sources read the same scoping parameters, so every tab stays on the one
  // credential the side window was opened for.
  assert.match(source, /api\(`events\?\$\{params\}&limit=25`\)/);
  assert.match(source, /api\(`quota\/summary\?\$\{params\}`\)/);
});
test('one preset list drives every range control, and today is the default', () => {
  // The page filters and the side window must offer the same choices in the same
  // order; the markup is empty and filled from this list, so a drift between two
  // hardcoded <select> blocks is not possible.
  assert.deepEqual(C.RANGE_PRESETS.map(([value]) => value), ['today', 'yesterday', '24h', '7d', '30d', 'custom', 'all']);
  assert.deepEqual(C.RANGE_PRESETS.map(([, label]) => label), ['Today', 'Yesterday', 'Last 24 hours', 'Last week', 'Last month', 'Custom range', 'All retained history']);
  assert.equal(C.RANGE_DEFAULT, 'today');
  const html = read('stats.html'), source = read('stats.js');
  for (const id of ['range', 'quota-range']) assert.match(html, new RegExp(`<select id="${id}"></select>`), `${id} must be filled from the shared preset list`);
  assert.doesNotMatch(html, /<select id="(range|quota-range)"[^>]*>\s*<option/, 'a range control must not hardcode its own options');
  assert.match(source, /for \(const id of \['range', 'quota-range'\]\)/);
  assert.match(source, /for \(const \[value, label\] of C\.RANGE_PRESETS\) select\.add\(new Option\(label, value\)\)/);
  assert.match(source, /fillRangeSelects\(\);\n\s*syncRangeInputs\(\);/);
  // An empty filters form resets to the default preset, not to whatever option
  // happens to be first in the markup.
  assert.match(source, /\$\('range'\)\.value = C\.RANGE_DEFAULT/);
});
test('the range presets resolve to explicit instants, with local day boundaries', () => {
  const now = Date.parse('2026-02-10T12:00:00Z');
  const bounds = (range, filters) => {const {from, to} = C.rangeBounds(range, filters, now); return {from: from.toISOString(), to: to.toISOString()};};
  // Today starts at local midnight and ends now, so the window grows through the day.
  const midnight = new Date(now); midnight.setHours(0, 0, 0, 0);
  assert.deepEqual(bounds('today', {}), {from: midnight.toISOString(), to: '2026-02-10T12:00:00.000Z'});
  // Yesterday is the previous local day, ending exactly where today begins.
  const yesterdayStart = new Date(midnight.getTime()); yesterdayStart.setDate(yesterdayStart.getDate() - 1);
  assert.deepEqual(bounds('yesterday', {}), {from: yesterdayStart.toISOString(), to: midnight.toISOString()});
  // The rolling presets measure back from now.
  assert.deepEqual(bounds('24h', {}), {from: '2026-02-09T12:00:00.000Z', to: '2026-02-10T12:00:00.000Z'});
  assert.deepEqual(bounds('7d', {}), {from: '2026-02-03T12:00:00.000Z', to: '2026-02-10T12:00:00.000Z'});
  assert.deepEqual(bounds('30d', {}), {from: '2026-01-11T12:00:00.000Z', to: '2026-02-10T12:00:00.000Z'});
  // All retained history is bounded at the epoch rather than left open, and an
  // unknown preset reads all history rather than silently narrowing the range.
  assert.deepEqual(bounds('all', {}), {from: '1970-01-01T00:00:00.000Z', to: '2026-02-10T12:00:00.000Z'});
  assert.deepEqual(bounds('nonsense', {}), bounds('all', {}));
  // A custom range is passed through and must be complete and ordered.
  assert.deepEqual(bounds('custom', {from: '2026-02-01T00:00:00Z', to: '2026-02-02T00:00:00Z'}), {from: '2026-02-01T00:00:00.000Z', to: '2026-02-02T00:00:00.000Z'});
  assert.throws(() => bounds('custom', {from: '2026-02-02T00:00:00Z', to: '2026-02-01T00:00:00Z'}), /end after the start/);
  assert.throws(() => bounds('custom', {from: '', to: ''}), /end after the start/);
  // An hour bucket is used up to three days and a day bucket beyond it.
  assert.equal(C.rangeQuery('today', {}, now).bucket, 'hour');
  assert.equal(C.rangeQuery('yesterday', {}, now).bucket, 'hour');
  assert.equal(C.rangeQuery('24h', {}, now).bucket, 'hour');
  assert.equal(C.rangeQuery('7d', {}, now).bucket, 'day');
  assert.equal(C.rangeQuery('30d', {}, now).bucket, 'day');
  assert.equal(C.rangeQuery('all', {}, now).bucket, 'day');
});
test('the page filters default to today and carry the resolved range', () => {
  const now = Date.parse('2026-02-10T12:00:00Z');
  const params = C.query({range: '', provider: 'claude', model: 'gpt-5', status: 'success'}, now);
  // An absent range is the default preset, so a form that never set one still reads today.
  const midnight = new Date(now); midnight.setHours(0, 0, 0, 0);
  assert.equal(params.get('from'), midnight.toISOString());
  assert.equal(params.get('to'), '2026-02-10T12:00:00.000Z');
  assert.equal(params.get('bucket'), 'hour');
  assert.equal(params.get('provider'), 'claude');
  assert.equal(params.get('model'), 'gpt-5');
  assert.equal(params.get('status'), 'success');
  assert.equal(params.get('account'), null, 'the page filters must not scope to a credential');
});
test('a response model differing from the requested model is flagged, an absent one is not', () => {
  assert.equal(C.responseModelMismatch({model: 'gpt-5', response_model: 'gpt-5-mini'}), true);
  assert.equal(C.responseModelMismatch({model: 'gpt-5', response_model: 'gpt-5'}), false);
  assert.equal(C.responseModelMismatch({model: 'gpt-5'}), false);
  assert.equal(C.responseModelMismatch({model: 'gpt-5', response_model: '   '}), false);
  assert.equal(C.responseModelMismatch(null), false);
});
test('statistics document keeps one overview, a requests tab, a quota tab and no header metric strip', () => {
  const html = read('stats.html');
  for (const id of ['panel-overview', 'panel-requests', 'panel-quota', 'panel-pricing', 'quota-grid', 'quota-dialog', 'quota-history', 'quota-value', 'quota-requests', 'dialog-panel-quota', 'dialog-panel-requests', 'overview-totals']) assert.match(html, new RegExp(`id="${id}"`));
  for (const gone of ['id="panel-analysis"', 'id="panel-realtime"', 'class="metrics"', 'id="auth-index"', 'id="key-id"', 'see-analysis']) assert.doesNotMatch(html, new RegExp(gone.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')));
  for (const tab of ['overview', 'requests', 'quota', 'pricing']) assert.match(html, new RegExp(`data-tab="${tab}"`));
  // Every tab must have a panel the dashboard can unhide; pricing lives outside the
  // shared statistics container, so it needs its own visibility toggle.
  const source = read('stats.js');
  assert.match(source, /panel-pricing'\)\.hidden/);
  // Every quota reload re-renders the same grid: it must clear before appending, or
  // each refresh would stack another set of cards.
  assert.match(source, /grid\.replaceChildren\(\)/);
  assert.match(html, /id="panel-pricing"[^>]*hidden/);
  // The request stream no longer shows a credential column, and does show the new ones.
  assert.doesNotMatch(html, /<th>Credential<\/th>/);
  for (const header of ['Tier', 'Tok/s', 'Cache read']) assert.match(html, new RegExp(`<th>${header}<\/th>`));
});
test('the quota tab can ask the original handler for a provider refresh', () => {
  const source = read('stats.js'), html = read('stats.html');
  // The add-on performs no provider traffic itself: the page calls the EXISTING
  // credential-quota handler, which owns that outbound call. It addresses the
  // credential by its transient index and sends nothing else.
  assert.match(source, /api\('\.\/v0\/management\/quota\/fetch', \{root: true, method: 'POST', body: \{auth_index: index\}\}\)/);
  // A refresh is distinct from re-reading our own saved state.
  assert.match(html, /id="quota-provider-refresh"[^>]*>↻ Refresh from provider</);
  assert.match(html, /id="quota-refresh"[^>]*>↻ Reload saved state</);
  // Both entry points: one fact group, or every group.
  assert.match(source, /\$\('quota-provider-refresh'\)\.addEventListener\('click', \(\) => void refreshFromProvider\(state\.credentials/);
  assert.match(source, /refreshFromProvider\(credential, refresh\)/);
  // A group can hold several credentials and one may legitimately have no provider
  // plugin, so every index is attempted and failures are reported, never swallowed.
  assert.match(source, /for \(const index of credential\.indices\)/);
  assert.match(source, /failures\.push\(`\$\{C\.credentialName\(credential\)\}: \$\{error\.message\}`\)/);
  // A handler rarely fails with a useful status alone, so its own explanation wins.
  assert.match(source, /if \(!response\.ok\) throw new Error\(await serverError\(response\)\)/);
  assert.match(source, /async function serverError\(response\)/);
});
test('every element the dashboard reads exists in the document', () => {
  const html = read('stats.html');
  const source = read('stats.js');
  const ids = [...source.matchAll(/\$\('([^']+)'\)/g)].map(match => match[1]);
  assert.ok(ids.length > 40, `expected the dashboard to read many elements, saw ${ids.length}`);
  for (const id of new Set(ids)) assert.match(html, new RegExp(`id="${id}"`), `stats.html is missing #${id}`);
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
// Records carry plain account facts: provider + account + account_kind. The
// credential index survives only as transient correlation data for the native page.
const binding = (account = 'person@example.test', account_kind = 'email', extra = {}) => ({provider: 'codex', key: 'test.json', account, account_kind, auth_index: 'index1', ...extra});
const success = (usedPercent = 42) => ({status: 'success', windows: [{id: 'five-hour', usedPercent, resetLabel: '-'}]});
const cachedEntry = (who = binding(), at = Date.now() - 60000) => ({provider: who.provider, key: who.key, account: who.account, account_kind: who.account_kind, observed_at: new Date(at).toISOString(), state: {status: 'success', windows: [{id: 'five-hour', usedPercent: 12, label: 'Five hours', resetLabel: '-', periodHours: 5}]}});
const providers = ['antigravity', 'claude', 'codex', 'devin', 'kimi', 'meta', 'xai'];
function harness({cached = [], snapshots = [], origin = 'http://localhost:18317', delayFiles = false, quotaStatus = 200, bindings = [binding()], now = Date.now()} = {}) {
  const calls = [], events = [], timers = new Map(), listeners = new Map(), holds = []; let nextTimer = 0;
  const maps = {cacheGeneration: 0, fileGenerations: {}, ...Object.fromEntries(providers.map(provider => [`${provider}Quota`, {}]))};
  const quota = store(maps), auth = store({isAuthenticated: true, connectionStatus: 'connected', apiBase: origin, managementKey: 'secret-key'});
  const session = new Map();
  const window = {dispatchEvent(event) {events.push(event);}, addEventListener(type, fn) {listeners.set(type, fn);}, sessionStorage: {getItem: key => session.has(key) ? session.get(key) : null, setItem: (key, value) => session.set(key, value), removeItem: key => session.delete(key)}};
  const h = {window, quota, auth, calls, events, timers, bindings, cached, snapshots, quotaStatus, now, putStatus: 200, clearCalls: [], session, hold(path, method = 'GET') {holds.push({path, method});}, refresh() {listeners.get('online')();}, async timersAt(ms) {for (const [id, timer] of [...timers]) if (timer.ms === ms) {timers.delete(id); timer.fn();} await settle();}, puts() {return calls.filter(call => call.options.method === 'PUT');}, resolveFiles() {calls.find(call => call.url.endsWith('/identities') && call.release)?.release();}};
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
  vm.runInContext(read('management-bridge.js'), context);
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

test('bridge hydrates stored history for the same account facts without uploading it', async () => {
  const entry = cachedEntry(), h = harness({cached: [entry]}); await settle();
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
test('bridge hydrates only cache entries whose account facts still match the binding', async () => {
  const entry = cachedEntry(), h = harness({cached: [entry], delayFiles: true});
  h.quota.setState({codexQuota: {'test.json': {status: 'loading', windows: []}}}); h.resolveFiles(); await settle();
  assert.equal(h.quota.getState().codexQuota['test.json'].status, 'loading');
  for (const change of [{account: 'other@example.test'}, {account_kind: 'device_id'}, {account: undefined}, {account_kind: undefined}]) {
    const other = harness({cached: [{...entry, ...change}]}); await settle(); assert.equal(other.quota.getState().codexQuota['test.json'], undefined);
  }
});
test('success before first binding is never retroactively promoted', async () => {
  const h = harness({delayFiles: true}), operation = h.capture('test.json');
  assert.equal(h.succeed(operation, {...success(), access_token: 'DO_NOT_PERSIST'}), true);
  h.resolveFiles(); await settle(); await h.timersAt(500); await h.timersAt(15000);
  assert.equal(h.puts().length, 0);
  // A subsequent genuinely bound operation persists normally.
  const bound = h.capture('test.json'); h.succeed(bound, {...success(), access_token: 'DO_NOT_PERSIST'}); await h.timersAt(500);
  const body = JSON.parse(h.puts()[0].options.body); assert.equal(body.entries[0].account, 'person@example.test'); assert.equal(body.entries[0].account_kind, 'email'); assert.equal(h.puts()[0].options.body.includes('DO_NOT_PERSIST'), false);
});
test('operation starting before identities remains unknown even if its success arrives after lookup', async () => {
  const h = harness({delayFiles: true}), old = h.capture('test.json'); h.resolveFiles(); await settle();
  assert.equal(h.succeed(old), false); await h.timersAt(500); assert.equal(h.puts().length, 0);
});
test('a normal quota refresh saves only account facts and reloads them without another upload', async () => {
  const h = harness(); await settle(); const operation = h.capture('test.json');
  h.succeed(operation, success(25)); await h.timersAt(500); const entries = JSON.parse(h.puts()[0].options.body).entries;
  assert.deepEqual(Object.keys(entries[0]).sort(), ['account', 'account_kind', 'key', 'observed_at', 'provider', 'state']);
  assert.equal(entries[0].account, 'person@example.test'); assert.equal(entries[0].account_kind, 'email');
  const reloaded = harness({cached: entries}); await settle(); assert.equal(reloaded.quota.getState().codexQuota['test.json'].windows[0].usedPercent, 25);
  await reloaded.timersAt(500); assert.equal(reloaded.puts().length, 0);
});
test('an all-provider batch uploads account facts and never a credential index', async () => {
  const bindings = providers.map(provider => binding(`account-${provider}`, 'email', {provider, key: provider === 'devin' ? 'devin.json\0index1' : `${provider}.json`}));
  const h = harness({bindings}); await settle(); const capture = h.capture();
  const states = {antigravity: {status: 'success', groups: []}, claude: success(), codex: success(), devin: success(), kimi: {status: 'success', rows: []}, meta: {status: 'success', data: {windows: []}}, xai: {status: 'success', billing: {usagePercent: 42}}};
  h.refresh(); await settle(); assert.equal(h.clearCalls.length, 0);
  h.batch(capture, bindings.map(who => ({provider: who.provider, key: who.key, state: states[who.provider]})));
  await h.timersAt(500); const body = h.puts()[0].options.body, entries = JSON.parse(body).entries;
  assert.equal(entries.length, 7);
  for (const entry of entries) {assert.equal(entry.account, `account-${entry.provider}`); assert.equal(entry.account_kind, 'email'); assert.equal(Object.hasOwn(entry, 'auth_index'), false); assert.equal(Object.hasOwn(entry.state, 'auth_index'), false);}
  assert.equal(body.includes('"auth_index"'), false, 'the transient credential index never leaves the page');
  assert.deepEqual(Object.keys(capture).sort(), ['cacheGeneration', 'fileGenerations', 'name']);
});
test('A starts, B replaces the same provider and account, identity learns B, then A succeeds', async () => {
  const h = harness(); await settle();
  const A = h.capture('test.json'); h.bindings = [{...binding(), auth_index: 'index2'}]; h.refresh(); await settle();
  assert.equal(h.succeed(A, success(91)), false); assert.equal(h.quota.getState().codexQuota['test.json'], undefined);
  h.succeed(h.capture('test.json'), success(10)); await h.timersAt(500);
  const entry = JSON.parse(h.puts()[0].options.body).entries[0];
  assert.equal(entry.account, 'person@example.test'); assert.equal(entry.state.windows[0].usedPercent, 10);
  assert.equal(Object.hasOwn(entry, 'auth_index'), false); assert.equal(JSON.stringify(entry).includes('index2'), false);
});
test('a replaced binding fences old operations and reloads stored history for the new one', async () => {
  const h = harness({cached: [cachedEntry()]}); await settle(); const old = h.capture('test.json');
  h.bindings = [binding('moved@example.test')]; h.refresh(); await settle();
  assert.equal(h.succeed(old, success(90)), false);
  assert.equal(h.quota.getState().codexQuota['test.json'], undefined, 'a genuine account change clears the replaced display state');
  await h.timersAt(500); assert.equal(h.puts().length, 0);
  const refreshed = harness({cached: [cachedEntry(binding('moved@example.test'))], bindings: [binding('moved@example.test')]}); await settle();
  assert.equal(refreshed.quota.getState().codexQuota['test.json'].windows[0].usedPercent, 12);
});
test('an unchanged binding poll never resets native operations', async () => {
  const h = harness({cached: [cachedEntry()]}); await settle();
  const current = h.capture('test.json'); h.refresh(); await settle();
  assert.equal(h.clearCalls.length, 0);
  assert.equal(h.commit(current, () => {}), true);
  h.succeed(h.capture('test.json'), success(33)); await h.timersAt(500);
  assert.equal(JSON.parse(h.puts()[0].options.body).entries[0].state.windows[0].usedPercent, 33);
});
test('unknown batch files remain unknown across unchanged polls and a later binding addition', async () => {
  const h = harness(); await settle(); const batch = h.capture(); h.refresh(); await settle();
  h.bindings.push(binding('new@example.test', 'email', {key: 'new.json'})); h.refresh(); await settle();
  h.batch(batch, [{provider: 'codex', key: 'new.json', state: success(90)}]); assert.equal(h.quota.getState().codexQuota['new.json'], undefined);
  await h.timersAt(500); assert.equal(h.puts().length, 0);
});
test('unknown direct writes and unknown-at-start operations refuse persistence', async () => {
  const h = harness(); await settle(); h.quota.setState({codexQuota: {'test.json': success()}}); await h.timersAt(500); assert.equal(h.puts().length, 0);
  const unknown = h.capture('unknown.json'); h.bindings.push(binding('new@example.test', 'email', {key: 'unknown.json'})); h.refresh(); await settle();
  assert.equal(h.succeed(unknown, success(), 'codex', 'unknown.json'), false); await h.timersAt(500); assert.equal(h.puts().length, 0);
});
test('a credential with no account property persists its facts and groups under its provider', async () => {
  const h = harness({bindings: [binding('', '')]}); await settle(); const capture = h.capture('test.json');
  h.succeed(capture, success(64)); await h.timersAt(500);
  const entry = JSON.parse(h.puts()[0].options.body).entries[0];
  assert.equal(entry.provider, 'codex'); assert.equal(entry.account, ''); assert.equal(entry.account_kind, '');
  // The dashboard shows one card per credential file even when neither credential
  // exposes an account property: they are not known to be the same account, so
  // merging them would present one credential's quota as if it covered the other.
  const cards = C.summarizeCredentials([], [], [binding('', ''), binding('', '', {key: 'other.json'})]);
  assert.equal(cards.length, 2); assert.ok(cards.every(card => card.provider === 'codex')); assert.ok(cards.every(card => card.account === ''));
  assert.deepEqual(cards.map(C.credentialName), ['other.json', 'test.json']);
});
test('two live bindings for one account share the single persisted row the pair owns', async () => {
  const h = harness({bindings: [binding(), binding('person@example.test', 'email', {key: 'other.json', auth_index: 'index2'})]});
  await settle();
  // Both credentials resolve to one (provider, account) pair, so both stay legally
  // correlated; the second one never renames the pair or duplicates the row.
  h.succeed(h.capture('test.json'), success(41)); await h.timersAt(500);
  h.succeed(h.capture('other.json'), success(42), 'codex', 'other.json'); await h.timersAt(500);
  const entries = h.puts().map(call => JSON.parse(call.options.body).entries[0]);
  assert.equal(entries.length, 2);
  for (const entry of entries) {assert.equal(entry.provider, 'codex'); assert.equal(entry.account, 'person@example.test'); assert.equal(entry.account_kind, 'email'); assert.equal(Object.hasOwn(entry, 'auth_index'), false);}
});
test('a malformed binding is dropped instead of trusted', async () => {
  for (const bindings of [[{...binding(), key: ''}], [{...binding(), account: 42}], [{...binding(), account_kind: undefined}], [{...binding(), auth_index: 7}], [{...binding(), provider: '__proto__'}]]) {
    const h = harness({bindings, cached: [cachedEntry()]}); await settle(); h.succeed(h.capture('test.json')); await h.timersAt(500); assert.equal(h.puts().length, 0);
  }
});
test('failed PUT retries preserve the original binding only while current', async () => {
  const h = harness(); await settle(); h.putStatus = 500; h.succeed(h.capture('test.json')); await h.timersAt(500);
  assert.equal(h.puts().length, 1); h.putStatus = 200; await h.timersAt(15000); assert.equal(h.puts().length, 2);
  assert.equal(h.puts()[0].options.body, h.puts()[1].options.body);
  h.putStatus = 500; h.succeed(h.capture('test.json')); await h.timersAt(500);
  h.bindings = [binding('replacement@example.test')]; h.refresh(); await settle(); h.putStatus = 200; await h.timersAt(15000); assert.equal(h.puts().length, 3);
});
test('failed PUT completion after replacement cannot requeue the old batch', async () => {
  const h = harness(); await settle(); h.hold('/quota/cache', 'PUT'); h.succeed(h.capture('test.json')); await h.timersAt(500); const put = h.puts()[0];
  h.bindings = [binding('replacement@example.test')]; h.refresh(); await settle(); put.release({status: 500}); await settle(); await h.timersAt(15000); assert.equal(h.puts().length, 1);
});
test('a late 409 cannot erase the replacement display or discard its queued upload', async () => {
  const h = harness(); await settle(); h.hold('/quota/cache', 'PUT');
  h.succeed(h.capture('test.json'), success(90)); await h.timersAt(500); const failed = h.puts()[0];
  h.bindings = [binding('replacement@example.test')]; h.refresh(); await settle();
  const replacement = h.capture('test.json'), live = success(10); h.succeed(replacement, live);
  const clears = h.clearCalls.length; failed.release({status: 409}); await settle();
  assert.equal(h.quota.getState().codexQuota['test.json'], live);
  assert.equal(h.clearCalls.length, clears, 'a stale failure must not reset the replacement native generation');
  assert.equal(h.commit(replacement, () => {}), true);
  await h.timersAt(500); await h.timersAt(15000);
  assert.equal(h.puts().length, 2, 'the replacement must upload, the stale batch must never retry');
  const entries = h.puts().map(call => JSON.parse(call.options.body).entries[0]);
  assert.deepEqual(entries.map(entry => [entry.account, entry.state.windows[0].usedPercent]), [['person@example.test', 90], ['replacement@example.test', 10]]);
});
test('a late 409 never drops newer valid pending work for the same account', async () => {
  const h = harness(); await settle(); h.hold('/quota/cache', 'PUT');
  h.succeed(h.capture('test.json'), success(90)); await h.timersAt(500); const failed = h.puts()[0];
  const newer = h.capture('test.json'), live = success(10); h.succeed(newer, live); await h.timersAt(500);
  failed.release({status: 409}); await settle();
  assert.equal(h.quota.getState().codexQuota['test.json'], live);
  assert.equal(h.clearCalls.length, 0);
  assert.equal(h.commit(newer, () => {}), true);
  await h.timersAt(15000); assert.equal(h.puts().length, 2);
  const entries = h.puts().map(call => JSON.parse(call.options.body).entries[0]);
  assert.deepEqual(entries.map(entry => [entry.account, entry.state.windows[0].usedPercent]), [['person@example.test', 90], ['person@example.test', 10]]);
});
test('409 is terminal for the whole batch and never re-stamps or retries it', async () => {
  const h = harness(); await settle(); h.putStatus = 409; h.succeed(h.capture('test.json')); h.bindings = [binding('replacement@example.test')]; await h.timersAt(500);
  h.putStatus = 200; await h.timersAt(15000); assert.equal(h.puts().length, 1); assert.equal(h.quota.getState().codexQuota['test.json'], undefined);
  h.succeed(h.capture('test.json'), success(10)); await h.timersAt(500); assert.equal(h.puts().length, 2); assert.equal(JSON.parse(h.puts()[1].options.body).entries[0].account, 'replacement@example.test');
});
test('409 does not permanently disable new operations for unchanged valid batch siblings', async () => {
  const h = harness(); await settle(); const old = h.capture('test.json'); h.putStatus = 409; h.succeed(old); await h.timersAt(500);
  assert.equal(h.succeed(old), false); h.putStatus = 200; h.succeed(h.capture('test.json'), success(12)); await h.timersAt(500);
  assert.equal(h.puts().length, 2); assert.equal(JSON.parse(h.puts()[1].options.body).entries[0].state.windows[0].usedPercent, 12);
});
test('a per-file reset while a PUT fails prevents requeue without clearing unrelated pending quota', async () => {
  const other = binding('other@example.test', 'email', {key: 'other.json', auth_index: 'index2'}), h = harness({bindings: [binding(), other]}); await settle();
  h.hold('/quota/cache', 'PUT'); h.succeed(h.capture('test.json')); await h.timersAt(500); const failed = h.puts()[0];
  h.succeed(h.capture('other.json'), success(12), 'codex', 'other.json'); h.clear(['test.json']); failed.release({status: 500}); await settle();
  await h.timersAt(15000); assert.equal(h.puts().length, 2); assert.deepEqual(JSON.parse(h.puts()[1].options.body).entries.map(entry => entry.key), ['other.json']);
});
test('global and file resets discard retry/window state and preserve native generation scopes', async () => {
  const other = binding('other@example.test', 'email', {key: 'other.json'}), h = harness({bindings: [binding(), other], cached: [cachedEntry(), cachedEntry(other)]}); await settle();
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
test('identity refresh responses cannot install in reverse completion order', async () => {
  const h = harness(); await settle(); h.hold('/identities'); h.refresh(); const stale = h.calls.at(-1);
  h.bindings = [binding('replacement@example.test')]; h.refresh(); await settle(); stale.release(); await settle();
  h.succeed(h.capture('test.json')); await h.timersAt(500); assert.equal(JSON.parse(h.puts()[0].options.body).entries[0].account, 'replacement@example.test');
});
test('cache and normalized payloads recheck bindings after delayed responses', async () => {
  const h = harness(); await settle(); h.cached = [cachedEntry()]; h.snapshots = [{provider: 'codex', account: 'person@example.test', account_kind: 'email', windows: [{id: 'primary', window_seconds: 18000, used_percent: 99, observed_at: new Date().toISOString()}]}];
  h.hold('/quota/cache'); h.refresh(); await settle(); const delayed = h.calls.find(call => call.release);
  h.bindings = [binding('replacement@example.test')]; delayed.release(); await settle(); assert.equal(h.quota.getState().codexQuota['test.json'], undefined);
});
test('a newer normalized observation keeps proving freshness across unchanged polls without re-uploading history', async () => {
  const now = Date.parse('2026-01-02T03:04:05Z'), iso = offset => new Date(now + offset).toISOString();
  const snapshot = (used, offset) => ({provider: 'codex', account: 'person@example.test', account_kind: 'email', windows: [{id: 'primary', window_seconds: 18000, used_percent: used, observed_at: iso(offset)}]});
  // The fixture must sit on the harness clock: the stored display state's own
  // observation time is the floor below which a normalized snapshot is ignored.
  const h = harness({now, cached: [cachedEntry(binding(), now - 60000)], snapshots: [snapshot(20, -30000)]}); await settle();
  const live = success(77); h.succeed(h.capture('test.json'), live);
  h.refresh(); await settle();
  assert.equal(h.quota.getState().codexQuota['test.json'], live, 'older cached and normalized data must not replace fresh live state');
  assert.equal(h.clearCalls.length, 0, 'unchanged polls must not reset native operations');
  h.now += 2000; h.snapshots = [snapshot(88, 1000)]; h.refresh(); await settle();
  assert.equal(h.quota.getState().codexQuota['test.json'].windows[0].usedPercent, 88, 'a newer normalized observation still overlays');
  h.snapshots = [snapshot(89, 1500)]; h.refresh(); await settle();
  assert.equal(h.quota.getState().codexQuota['test.json'].windows[0].usedPercent, 89);
  // Only the one live success was ever uploaded; no hydrated or overlaid history follows it.
  await h.timersAt(500); await h.timersAt(15000);
  assert.deepEqual(h.puts().map(call => JSON.parse(call.options.body).entries[0].state.windows[0].usedPercent), [77]);
  assert.equal(h.clearCalls.length, 0);
});
test('an unchanged binding poll keeps a settled display without clearing native state', async () => {
  for (const state of [{status: 'error', error: 'original provider failure', errorCode: 429, windows: []}, {status: 'idle', windows: []}, {...success(77), originalDisplayField: 'keep locally'}]) {
    const h = harness(); await settle();
    h.quota.setState({codexQuota: {'test.json': state}});
    h.refresh(); await settle();
    assert.equal(h.quota.getState().codexQuota['test.json'], state, state.status);
    assert.equal(h.clearCalls.length, 0, 'a settled poll must not reset native operations');
    h.succeed(h.capture('test.json'), success(90)); await h.timersAt(500);
    assert.equal(h.puts().length, 1, state.status);
  }
});
test('a repeated poll hydrates the newest cache observation for the same account facts', async () => {
  const h = harness({cached: [cachedEntry()]}); await settle();
  assert.equal(h.quota.getState().codexQuota['test.json'].windows[0].usedPercent, 12);
  // The stored observation is display history, never a new observation to publish.
  h.cached = [{...cachedEntry(), observed_at: new Date().toISOString(), state: {status: 'success', windows: [{id: 'five-hour', usedPercent: 37, label: 'Five hours', resetLabel: '-', periodHours: 5}]}}];
  h.refresh(); await settle();
  await h.timersAt(500); await h.timersAt(15000);
  assert.equal(h.puts().length, 0);
  assert.equal(h.clearCalls.length, 0);
});
test('a file reset fences an operation taken before it even when the binding is unchanged', async () => {
  const h = harness(); await settle();
  const before = h.capture('test.json');
  h.clear(['test.json']);
  assert.equal(h.commit(before, () => assert.fail('pre-reset callback ran')), false);
  assert.equal(h.succeed(before, success(90)), false);
  await h.timersAt(500); assert.equal(h.puts().length, 0);
  // A fresh operation after the reset is current again and persists.
  h.succeed(h.capture('test.json'), success(90)); await h.timersAt(500);
  assert.equal(JSON.parse(h.puts()[0].options.body).entries[0].state.windows[0].usedPercent, 90);
});
test('logout aborts synchronization and late callbacks cannot enter a new auth session', async () => {
  const h = harness({cached: [cachedEntry()]}); await settle(); const old = h.capture('test.json');
  h.hold('/quota/cache'); h.refresh(); await settle(); const delayed = h.calls.find(call => call.release);
  h.auth.setState({isAuthenticated: false, connectionStatus: 'disconnected', managementKey: ''}); assert.equal(delayed.options.signal.aborted, true);
  delayed.release(); await settle(); assert.equal(h.window.CPAQuotaPersistence.status.state, 'waiting-for-login'); assert.equal(h.succeed(old), false);
  h.auth.setState({isAuthenticated: true, connectionStatus: 'connected', managementKey: 'new-key'}); await settle(); assert.equal(h.succeed(old), false); await h.timersAt(500); assert.equal(h.puts().length, 0);
});

// Minimal DOM used by the navigation asset: enough structure to prove it finds the
// verified sidebar classes, appends one idempotent entry and leaves unknown markup alone.
function fakeDocument({section = true} = {}) {
  const created = [];
  const node = tag => {
    const element = {tag, attributes: {}, children: [], listeners: [], className: '', textContent: ''};
    element.setAttribute = (name, value) => {element.attributes[name] = value;};
    element.appendChild = child => {element.children.push(child); return child;};
    element.addEventListener = (type, fn) => element.listeners.push({type, fn});
    element.dispatch = event => {for (const listener of element.listeners) listener.fn(event);};
    element.closest = () => (section ? {classList: {}} : null);
    return element;
  };
  const sidebar = {className: 'sidebar'};
  const navSection = {...node('div'), className: 'nav-section', sidebar};
  navSection.closest = () => sidebar;
  const document = {
    created, navSection,
    createElement: tag => {const element = node(tag); created.push(element); return element;},
    createElementNS: (_namespace, tag) => {const element = node(tag); created.push(element); return element;},
    querySelector: selector => {
      if (selector.startsWith('[')) return created.some(element => Object.hasOwn(element.attributes, nav.entryAttribute)) ? {} : null;
      if (selector === '.sidebar .nav-section') return section ? navSection : null;
      return null;
    },
  };
  return document;
}

const nav = require('./management-nav.js');
test('navigation asset adds one statistics link to the verified sidebar markup', () => {
  const document = fakeDocument();
  assert.equal(nav.mount(document, null), true);
  assert.equal(document.navSection.children.length, 1);
  const group = document.navSection.children[0];
  assert.equal(group.className, 'nav-group');
  assert.ok(Object.hasOwn(group.attributes, nav.entryAttribute));
  const link = group.children[0];
  assert.equal(link.className, 'nav-item');
  assert.equal(link.attributes.href, nav.href);
  assert.equal(link.children[0].className, 'nav-icon');
  assert.equal(link.children[1].children[0].textContent, nav.label);
  assert.equal(nav.mount(document, null), true);
  assert.equal(document.navSection.children.length, 1);
});
test('navigation asset leaves unavailable or unknown sidebar markup unchanged', () => {
  const absent = fakeDocument({section: false});
  assert.equal(nav.mount(absent, null), false);
  assert.equal(absent.created.length, 0);
  assert.equal(nav.mount({querySelector: () => {throw new Error('unexpected');}}, null), false);
});
test('plain left clicks hand over the tab key and navigate; modified clicks are never rewritten', () => {
  const document = fakeDocument();
  const aims = [], opened = [], prevented = [];
  const bridge = {armStatistics: () => aims.push('armed')};
  assert.equal(nav.mount(document, bridge, target => opened.push(target)), true);
  const link = document.navSection.children[0].children[0];
  link.dispatch({defaultPrevented: false, button: 0, preventDefault: () => prevented.push('prevented')});
  assert.deepEqual(aims, ['armed']);
  assert.deepEqual(opened, [nav.href]);
  assert.deepEqual(prevented, ['prevented']);
  for (const event of [{defaultPrevented: false, button: 1}, {defaultPrevented: false, button: 0, ctrlKey: true}, {defaultPrevented: false, button: 0, metaKey: true}, {defaultPrevented: true, button: 0}, {defaultPrevented: false, button: 0, shiftKey: true}]) {
    event.preventDefault = () => prevented.push('modified');
    for (const listener of link.listeners) listener.fn(event);
  }
  assert.deepEqual(aims, ['armed']);
  assert.deepEqual(opened, [nav.href]);
  assert.deepEqual(prevented, ['prevented']);
  assert.equal(nav.plainLeftClick(null), false);
  assert.equal(link.attributes.href, nav.href);
});
test('navigation asset never exposes or reads the management key itself', () => {
  const source = read('management-nav.js');
  for (const secret of ['localStorage', 'managementKey', 'cpa-stats-management-key', 'Authorization']) {
    assert.equal(source.includes(secret), false, secret);
  }
});
test('the bridge never reads the management key from storage and never writes it to localStorage', () => {
  const source = read('management-bridge.js');
  assert.equal(source.includes('localStorage'), false);
  assert.equal(source.includes('getItem'), false);
  assert.equal(source.includes('sessionStorage.getItem'), false);
  assert.match(source, /window\.sessionStorage\.setItem\('cpa-stats-management-key', key\)/);
  assert.equal(source.includes('auth-files'), false);
});
test('statistics handoff exposes only the tab-scoped key the dashboard already reads', async () => {
  const h = harness(); await settle();
  h.window.CPAQuotaPersistence.armStatistics();
  assert.equal(h.session.get('cpa-stats-management-key'), 'secret-key');
  h.auth.setState({isAuthenticated: false, connectionStatus: 'disconnected', managementKey: ''});
  h.session.clear();
  h.window.CPAQuotaPersistence.armStatistics();
  assert.equal(h.session.has('cpa-stats-management-key'), false);
});
test('the dashboard reads the tab-scoped handoff key only from sessionStorage', () => {
  const source = read('stats.js');
  assert.match(source, /storage\.get\(sessionStorage, sessionKey\)/);
  assert.match(source, /storage\.set\(sessionStorage, sessionKey,/);
  assert.equal(source.includes('localStorage, sessionKey'), false);
});
