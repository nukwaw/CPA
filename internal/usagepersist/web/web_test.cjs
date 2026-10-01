'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const C = require('./stats-core.js');
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
test('two auth files sharing one account keep one card EACH, as the control panel does', () => {
  const authFiles = [
    {provider: 'claude', name: 'claude-a.json', email: 'shared@example.test', auth_index: 'index-a'},
    {provider: 'claude', name: 'claude-b.json', email: 'shared@example.test', auth_index: 'index-b'},
    // Kimi publishes no email through auth-files; a saved entry for the same file
    // would fill its device account fact. The entry's account field is never read:
    // it can carry an API key.
    {provider: 'kimi', name: 'kimi.json', account: 'sk-secret-api-key', auth_index: 'device-42'},
    {provider: 'gemini', name: 'ignored.json', runtime_only: true, auth_index: 'runtime'},
  ];
  const cards = C.summarizeCredentials([], [], authFiles);
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
  const kimi = cards.find(card => card.provider === 'kimi');
  assert.equal(kimi.account, '', 'the account field never becomes an account fact');
  assert.equal(kimi.account_kind, '');
  assert.ok(!cards.some(card => card.key === 'ignored.json'), 'runtime-only entries get no card');
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
test('the dashboard groups by account facts and never reads a removed credential identity', () => {
  const source = read('stats.js'), core = read('stats-core.js'), html = read('stats.html');
  // Cards are identified by the credential file, matching the control panel's own
  // quota page. The account is recorded as a fact on each card, not used as the key.
  assert.match(core, /const credentialKey = \(provider, key\) =>/);
  assert.match(source, /C\.summarizeCredentials\(state\.quota\.entries, state\.quota\.snapshots, C\.arrays\(authFiles, 'files'\)\)/);
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
  assert.equal(nav.mount(document), true);
  assert.equal(document.navSection.children.length, 1);
  const group = document.navSection.children[0];
  assert.equal(group.className, 'nav-group');
  assert.ok(Object.hasOwn(group.attributes, nav.entryAttribute));
  const link = group.children[0];
  assert.equal(link.className, 'nav-item');
  assert.equal(link.attributes.href, nav.href);
  assert.equal(link.children[0].className, 'nav-icon');
  assert.equal(link.children[1].children[0].textContent, nav.label);
  assert.equal(nav.mount(document), true);
  assert.equal(document.navSection.children.length, 1);
});
test('navigation asset leaves unavailable or unknown sidebar markup unchanged', () => {
  const absent = fakeDocument({section: false});
  assert.equal(nav.mount(absent), false);
  assert.equal(absent.created.length, 0);
  assert.equal(nav.mount({querySelector: () => {throw new Error('unexpected');}}), false);
});
test('the entry is a plain anchor: every click keeps its native behavior', () => {
  const document = fakeDocument();
  assert.equal(nav.mount(document), true);
  const link = document.navSection.children[0].children[0];
  // No handler intercepts clicks: there is no key to hand over, so modified
  // and plain clicks alike simply open the dashboard, which authenticates itself.
  assert.equal(link.listeners.length, 0);
  assert.equal(link.attributes.href, nav.href);
});
test('navigation asset never exposes or reads the management key itself', () => {
  const source = read('management-nav.js');
  for (const secret of ['localStorage', 'managementKey', 'cpa-stats-management-key', 'Authorization']) {
    assert.equal(source.includes(secret), false, secret);
  }
});
test('the dashboard remembers its own management key only in tab-scoped sessionStorage', () => {
  const source = read('stats.js');
  assert.match(source, /storage\.get\(sessionStorage, sessionKey\)/);
  assert.match(source, /storage\.set\(sessionStorage, sessionKey,/);
  assert.equal(source.includes('localStorage, sessionKey'), false);
});
