'use strict';
// Executed by asset Go tests with the actual generated helper/store sequence on
// stdin. No frontend dependencies, browser globals guessed, or upstream writes.
const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const input = JSON.parse(fs.readFileSync(0, 'utf8'));
const providers = ['antigravity', 'claude', 'codex', 'devin', 'kimi', 'meta', 'xai'];
const settle = () => new Promise(resolve => setImmediate(resolve));
const plain = value => JSON.parse(JSON.stringify(value));
function store(initializer) {
  let state; const subscribers = [];
  const setState = update => {const next = typeof update === 'function' ? update(state) : update; if (next === state) return; const old = state; state = {...state, ...next}; subscribers.forEach(fn => fn(state, old));};
  const getState = () => state;
  state = typeof initializer === 'function' ? initializer(setState, getState) : initializer;
  return {getState, setState, subscribe(fn) {subscribers.push(fn); return () => {};}};
}
function setup(wrapped = true) {
  const timers = new Map(), calls = [], events = new Map(); let nextTimer = 0;
  const h = {calls, bindings: providers.map(provider => ({provider, key: provider === 'devin' ? 'devin.json\0i' : `${provider}.json`, account: `acct-${provider}`, account_kind: 'email', auth_index: 'i'}))};
  const window = {dispatchEvent() {}, addEventListener(type, fn) {events.set(type, fn);}};
  const context = vm.createContext({window, URL, AbortController, DOMException, TextEncoder, console: {warn() {}}, location: {origin: 'http://localhost', href: 'http://localhost/management.html'}, document: {hidden: false}, navigator: {onLine: true}, CustomEvent: class {}, setTimeout(fn, ms) {const id = ++nextTimer; timers.set(id, {fn, ms}); return id;}, clearTimeout(id) {timers.delete(id);}, fetch: async (url, options) => {calls.push({url, options}); const body = url.endsWith('/identities') ? {bindings: h.bindings} : url.endsWith('/cache') ? {entries: []} : {snapshots: []}; return {ok: true, status: 200, json: async () => structuredClone(body)};}});
  context[input.names.create] = store;
  context[input.names.filename] = key => key.split('\0')[0];
  if (wrapped) vm.runInContext(input.bridge, context);
  vm.runInContext((wrapped ? input.wrapped : input.original) + `;globalThis.helpers={store:${input.names.quota},capture:${input.names.capture},commit:${input.names.commit},updater:${input.names.updater}};`, context);
  Object.assign(h, context.helpers);
  if (wrapped) {
    const auth = store({isAuthenticated: true, connectionStatus: 'connected', apiBase: 'http://localhost', managementKey: 'NOT_IN_STATE_OR_URL'});
    assert.equal(window.CPAQuotaPersistence.attach({quotaStore: h.store, authStore: auth}), true);
  }
  h.flush = async () => {for (const [id, timer] of [...timers]) if (timer.ms === 500) {timers.delete(id); timer.fn();} await settle();};
  h.refresh = async () => {events.get('online')(); await settle();};
  h.puts = () => calls.filter(call => call.options.method === 'PUT');
  return h;
}
async function main() {
  // Same native decisions/results, including default/explicit third argument,
  // unrelated-file survival, empty/global clears, and original updater identity.
  for (const wrapped of [false, true]) {
    const h = setup(wrapped); await settle();
    const before = h.store.getState(), file = h.capture('codex.json'), batch = h.capture();
    assert.deepEqual(Object.keys(file).sort(), ['cacheGeneration', 'fileGenerations', 'name']);
    assert.equal(file.fileGenerations, before.fileGenerations);
    let commits = 0;
    assert.equal(h.commit(file, () => {commits++;}), true);
    assert.equal(h.commit(batch, () => {commits++;}, 'codex.json'), true);
    assert.equal(h.commit(file, () => {commits++;}, undefined), true);
    h.store.getState().clearQuotaCache([]);
    assert.equal(h.store.getState(), before);
    h.store.getState().clearQuotaCache(['unrelated.json']);
    assert.equal(h.commit(file, () => {commits++;}), true);
    assert.equal(h.commit(batch, () => {commits++;}), false);
    assert.equal(h.commit(batch, () => {commits++;}, 'codex.json'), true);
    h.store.getState().clearQuotaCache(['codex.json']);
    assert.equal(h.commit(file, () => {commits++;}), false);
    assert.equal(commits, 5);
    const newer = h.capture('codex.json');
    h.store.getState().clearQuotaCache();
    assert.equal(h.commit(newer, () => {}), false);
    const value = {x: 1}; assert.equal(h.updater(value, {}), value);
    assert.equal(h.updater(previous => previous, value), value);
    assert.throws(() => h.updater(() => {throw new Error('unchanged');}, {}), /unchanged/);
    const current = h.capture('codex.json');
    assert.throws(() => h.commit(current, () => {throw new Error('native throw');}), /native throw/);
  }
  const h = setup(); await settle();
  const states = {antigravity: {status: 'success', groups: []}, claude: {status: 'success', windows: []}, codex: {status: 'success', windows: []}, devin: {status: 'success', windows: []}, kimi: {status: 'success', rows: []}, meta: {status: 'success', data: {windows: []}}, xai: {status: 'success', billing: {usagePercent: 10}}};
  const setter = provider => h.store.getState()[`set${provider[0].toUpperCase()}${provider.slice(1)}Quota`];
  // Exact call order in useQuotaBatchLoader: native resolveUpdater encloses
  // commit callbacks; Zustand subscribers run only AFTER they have returned.
  const batch = h.capture();
  for (const who of h.bindings) setter(who.provider)(previous => {
    const next = {...previous};
    assert.equal(h.commit(batch, () => {next[who.key] = states[who.provider];}, who.key.split('\0')[0]), true);
    return next;
  });
  await h.flush(); assert.equal(h.puts().length, 1);
  const entries = JSON.parse(h.puts()[0].options.body).entries;
  assert.equal(entries.length, 7);
  for (const entry of entries) {
    assert.deepEqual(Object.keys(entry).sort(), ['account', 'account_kind', 'key', 'observed_at', 'provider', 'state']);
    assert.equal(entry.account, `acct-${entry.provider}`);
    assert.equal(entry.account_kind, 'email');
    assert.deepEqual(plain(h.store.getState()[`${entry.provider}Quota`][entry.key]), states[entry.provider]);
  }
  assert.equal(JSON.stringify(entries).includes('auth_index'), false, 'the transient credential index never leaves the page');
  // Actual original native file clear prevents stale compiled commits after
  // replacement, including Devin's filename+NUL+index map entry.
  const old = h.capture('devin.json');
  h.bindings = h.bindings.map(who => who.provider === 'devin' ? {...who, account: 'replacement-account'} : who);
  await h.refresh();
  assert.equal(h.store.getState().devinQuota['devin.json\0i'], undefined);
  assert.equal(h.commit(old, () => {throw new Error('stale callback ran');}), false);
  const latest = h.capture('devin.json');
  assert.equal(h.commit(latest, () => setter('devin')(previous => ({...previous, ['devin.json\0i']: states.devin}))), true);
  await h.flush(); assert.equal(h.puts().length, 2);
  assert.equal(JSON.parse(h.puts()[1].options.body).entries[0].account, 'replacement-account');
  assert.ok(h.calls.every(call => !call.url.includes('NOT_IN_STATE_OR_URL')));
  // A callback throw restores active context; unguarded subsequent setters must
  // not inherit operation provenance and silently become persisted observations.
  assert.throws(() => h.commit(h.capture('codex.json'), () => {throw new Error('test');}), /test/);
  setter('codex')(previous => ({...previous, 'codex.json': {status: 'success', windows: []}}));
  await h.flush(); assert.equal(h.puts().length, 2);
}
main().catch(error => {console.error(error); process.exitCode = 1;});
