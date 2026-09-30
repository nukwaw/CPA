/* Explicit adapter for the verified upstream quota/auth stores. No UI/DOM replacement: the
   separate management-nav.js asset adds the statistics link. */
(function (root) {
  'use strict';
  const mapNames = {antigravity: 'antigravityQuota', claude: 'claudeQuota', codex: 'codexQuota', devin: 'devinQuota', kimi: 'kimiQuota', meta: 'metaQuota', xai: 'xaiQuota'};
  const string = 'string', number = 'number', bool = 'boolean';
  const labels = {name: string, duration: string, index: number};
  const windowShape = {id: string, label: string, labelKey: string, labelParams: labels, usedPercent: number, remainingPercent: number, resetLabel: string, resetAtMs: number, periodHours: number};
  const bucketShape = {id: string, label: string, window: string, remainingFraction: number, resetTime: string, description: string, resetAtMs: number, periodHours: number};
  const schemas = {
    codex: {status: string, windows: [windowShape], planType: string, subscriptionActiveUntil: 'scalar', rateLimitResetCreditsAvailableCount: number, rateLimitResetCreditsApplicableAvailableCount: number, rateLimitResetCredits: [{id: string, status: string, grantedAt: string, expiresAt: string}]},
    claude: {status: string, windows: [windowShape], planType: string, extraUsage: {is_enabled: bool, monthly_limit: number, used_credits: number, utilization: number}},
    antigravity: {status: string, groups: [{id: string, label: string, description: string, buckets: [bucketShape]}], subscription: {plan: string, tierName: string, tierId: string}, serverTimeOffsetMs: number},
    devin: {status: string, windows: [windowShape], observedAtMs: number, plan: string, planStartMs: number, planEndMs: number},
    kimi: {status: string, rows: [{id: string, label: string, labelKey: string, labelParams: labels, used: number, limit: number, resetHint: string, resetAtMs: number, periodHours: number}]},
    meta: {status: string, data: {planName: string, isSubscriptionActive: bool, windows: [{id: string, usedPercent: number, resetAt: number, durationMinutes: number}]}},
    xai: {status: string, billing: {mode: string, source: string, planType: string, healthStatus: string, periodType: string, usagePercent: number, periodStart: string, periodEnd: string, productUsage: [{product: string, usagePercent: number}], monthlyLimitCents: number, usedCents: number, includedUsedCents: number, onDemandCapCents: number, onDemandUsedCents: number, onDemandUsedPercent: number, billingPeriodStart: string, billingPeriodEnd: string, usedPercent: number, resetAtMs: number, periodHours: number}}
  };
  function clean(value, shape) {
    if (value === null) return null;
    if (Array.isArray(shape)) return Array.isArray(value) ? value.slice(0, 256).map(item => clean(item, shape[0])).filter(item => item !== undefined) : undefined;
    if (typeof shape === 'object') {if (!value || typeof value !== 'object' || Array.isArray(value)) return undefined; const result = {}; for (const key of Object.keys(shape)) if (Object.hasOwn(value, key)) {const next = clean(value[key], shape[key]); if (next !== undefined) result[key] = next;} return result;}
    if ((shape === string || shape === 'scalar') && typeof value === 'string') return value.length <= 1024 && !/[\u0000-\u001f\u007f-\u009f]/.test(value) ? value : undefined;
    if ((shape === number || shape === 'scalar') && typeof value === 'number') return Number.isFinite(value) && Math.abs(value) <= 1e18 ? value : undefined;
    if (shape === bool && typeof value === 'boolean') return value;
    return undefined;
  }
  function sanitize(provider, value) {if (!Object.hasOwn(schemas, provider) || value?.status !== 'success') return null; const state = clean(value, schemas[provider]); if (!state) return null; const list = provider === 'antigravity' ? state.groups : provider === 'kimi' ? state.rows : provider === 'meta' ? state.data?.windows : provider === 'xai' ? state.billing : state.windows; return list && (provider === 'xai' || Array.isArray(list)) ? state : null;}
  const stamp = value => {const n = Date.parse(value); return Number.isFinite(n) ? n : 0;};
  const percent = value => typeof value === 'number' && Number.isFinite(value) ? Math.min(100, Math.max(0, value)) : null;
  function normalizedWindowID(provider, window, existing = []) {
    if (provider === 'claude') return window.id === 'iguana_necktie' ? 'seven-day-fable' : String(window.id || '').replaceAll('_', '-');
    if (provider !== 'codex') return window.id;
    const id = String(window.id || ''), seconds = Number(window.window_seconds), period = seconds === 18000 ? 'five-hour' : seconds >= 2419200 && seconds <= 2678400 ? 'monthly' : seconds === 604800 ? 'weekly' : null;
    if (!period) return null;
    if (id === 'primary' || id === 'secondary') return period;
    if (id.startsWith('code_review:')) return `code-review-${period}`;
    // Match a unique observed additional window by name and duration, never by
    // the UI-only (unstable) array index suffix.
    if (id.startsWith('additional:')) {
      const slug = value => String(value || '').trim().toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
      const name = id.split(':')[1], matches = existing.filter(item => slug(item.labelParams?.name) === name && Number(item.periodHours) * 3600 === seconds);
      return matches.length === 1 ? matches[0].id : null;
    }
    return null;
  }
  function overlay(provider, current, snapshot, observed, windowTimes = new Map()) {
    if (!snapshot || !Array.isArray(snapshot.windows)) return {state: current, changed: false};
    if (current && !['success', 'idle'].includes(current.status)) return {state: current, changed: false};
    const supported = ['codex', 'claude', 'devin', 'meta', 'kimi', 'antigravity'];
    if (!supported.includes(provider)) return {state: current, changed: false};
    let next = current ? sanitize(provider, current) : null;
    // Only Codex/Claude have a verified conversion without prior display metadata.
    if (!next && !['codex', 'claude'].includes(provider)) return {state: current, changed: false};
    if (!next) next = {status: 'success', windows: []};
    let changed = false;
    const items = provider === 'meta' ? next.data.windows : provider === 'kimi' ? next.rows : provider === 'antigravity' ? next.groups.flatMap(group => group.buckets) : next.windows;
    for (const window of snapshot.windows) {
      const id = normalizedWindowID(provider, window, items), at = stamp(window.observed_at || snapshot.observed_at);
      if (!id || !at || at <= (windowTimes.get(id) || observed || 0) || at > Date.now() + 300000) continue;
      let item = items.find(entry => entry.id === id);
      if (!item && ['codex', 'claude'].includes(provider)) {
        const knownCodex = ['five-hour', 'weekly', 'monthly', 'code-review-five-hour', 'code-review-weekly', 'code-review-monthly'];
        const knownClaude = ['five-hour', 'seven-day', 'seven-day-oauth-apps', 'seven-day-opus', 'seven-day-sonnet', 'seven-day-cowork', 'seven-day-fable'];
        if (!(provider === 'codex' ? knownCodex : knownClaude).includes(id)) continue;
        const codexKeys = {'five-hour': 'primary_window', weekly: 'secondary_window', monthly: 'team_secondary_window', 'code-review-five-hour': 'code_review_primary_window', 'code-review-weekly': 'code_review_secondary_window', 'code-review-monthly': 'code_review_team_secondary_window'};
        item = {id, label: window.label || id.replaceAll('-', ' '), labelKey: `${provider}_quota.${provider === 'codex' ? codexKeys[id] : id.replaceAll('-', '_')}`, usedPercent: null, resetLabel: '-'};
        items.push(item);
      }
      if (!item) continue;
      const used = percent(window.used_percent), remaining = percent(window.remaining_percent);
      if (provider === 'antigravity') {if (remaining != null || used != null) item.remainingFraction = (remaining ?? 100 - used) / 100;}
      else if (provider === 'devin') {if (remaining != null || used != null) item.remainingPercent = remaining ?? 100 - used;}
      else if (provider === 'kimi') {if (Number.isFinite(window.used)) item.used = window.used; if (Number.isFinite(window.limit)) item.limit = window.limit;}
      else if (used != null || remaining != null) item.usedPercent = used ?? 100 - remaining;
      const reset = stamp(window.reset_at);
      if (reset) {if (provider === 'meta') item.resetAt = reset / 1000; else item.resetAtMs = reset; if (provider === 'antigravity') item.resetTime = window.reset_at; else if (provider === 'codex' || provider === 'claude') item.resetLabel = new Date(reset).toLocaleString();}
      else {
        // A newer normalized window is authoritative about reset metadata. The
        // backend already retains a still-valid future reset when appropriate;
        // keeping display-cache resets here would mix fresh usage with old time.
        const resetFields = provider === 'meta' ? ['resetAt'] : provider === 'antigravity' ? ['resetAtMs', 'resetTime'] : provider === 'kimi' ? ['resetAtMs', 'resetHint'] : ['resetAtMs', 'resetLabel'];
        for (const field of resetFields) delete item[field];
      }
      if (Number.isFinite(window.window_seconds) && window.window_seconds > 0) {if (provider === 'meta') item.durationMinutes = window.window_seconds / 60; else item.periodHours = window.window_seconds / 3600;}
      windowTimes.set(id, at); changed = true;
    }
    if (changed && snapshot.plan && ['codex', 'claude'].includes(provider) && !next.planType) next.planType = String(snapshot.plan).slice(0, 1024);
    return {state: changed ? next : current, changed};
  }
  // Bounded synchronous SHA-256 over exact UTF-8 (also works on insecure HTTP).
  // Selector values and their hashes never enter state, status, or cache PUTs.
  function selectorDigest(value) {
    if (typeof value !== 'string' || value.length > 4096) return null;
    const bytes = new TextEncoder().encode(value), length = bytes.length;
    const words = new Uint32Array(((length + 9 + 63) >>> 6) * 16);
    for (let i = 0; i < length; i++) words[i >>> 2] |= bytes[i] << (24 - (i & 3) * 8);
    words[length >>> 2] |= 0x80 << (24 - (length & 3) * 8); words[words.length - 1] = length * 8;
    const k = [0x428a2f98,0x71374491,0xb5c0fbcf,0xe9b5dba5,0x3956c25b,0x59f111f1,0x923f82a4,0xab1c5ed5,0xd807aa98,0x12835b01,0x243185be,0x550c7dc3,0x72be5d74,0x80deb1fe,0x9bdc06a7,0xc19bf174,0xe49b69c1,0xefbe4786,0x0fc19dc6,0x240ca1cc,0x2de92c6f,0x4a7484aa,0x5cb0a9dc,0x76f988da,0x983e5152,0xa831c66d,0xb00327c8,0xbf597fc7,0xc6e00bf3,0xd5a79147,0x06ca6351,0x14292967,0x27b70a85,0x2e1b2138,0x4d2c6dfc,0x53380d13,0x650a7354,0x766a0abb,0x81c2c92e,0x92722c85,0xa2bfe8a1,0xa81a664b,0xc24b8b70,0xc76c51a3,0xd192e819,0xd6990624,0xf40e3585,0x106aa070,0x19a4c116,0x1e376c08,0x2748774c,0x34b0bcb5,0x391c0cb3,0x4ed8aa4a,0x5b9cca4f,0x682e6ff3,0x748f82ee,0x78a5636f,0x84c87814,0x8cc70208,0x90befffa,0xa4506ceb,0xbef9a3f7,0xc67178f2];
    const hash = [0x6a09e667,0xbb67ae85,0x3c6ef372,0xa54ff53a,0x510e527f,0x9b05688c,0x1f83d9ab,0x5be0cd19], w = new Uint32Array(64);
    const rotr = (x, n) => (x >>> n) | (x << (32 - n));
    for (let offset = 0; offset < words.length; offset += 16) {
      w.set(words.subarray(offset, offset + 16));
      for (let i = 16; i < 64; i++) {const x = w[i - 15], y = w[i - 2]; w[i] = w[i - 16] + (rotr(x, 7) ^ rotr(x, 18) ^ (x >>> 3)) + w[i - 7] + (rotr(y, 17) ^ rotr(y, 19) ^ (y >>> 10));}
      let [a,b,c,d,e,f,g,h] = hash;
      for (let i = 0; i < 64; i++) {const t = (h + (rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25)) + ((e & f) ^ (~e & g)) + k[i] + w[i]) | 0, u = ((rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22)) + ((a & b) ^ (a & c) ^ (b & c))) | 0; h = g; g = f; f = e; e = (d + t) | 0; d = c; c = b; b = a; a = (t + u) | 0;}
      [a,b,c,d,e,f,g,h].forEach((value, i) => {hash[i] = (hash[i] + value) >>> 0;});
    }
    return hash.map(value => value.toString(16).padStart(8, '0')).join('');
  }
  function manualProof(value) {
    if (!value || value.v !== 1 || Object.keys(value).some(key => !['v', 'selector_hashes'].includes(key))) return null;
    const hashes = value.selector_hashes;
    if (!hashes || typeof hashes !== 'object' || Array.isArray(hashes) || !Object.keys(hashes).length || Object.keys(hashes).some(key => !['account_id', 'project_id', 'user_id'].includes(key) || typeof hashes[key] !== 'string' || !/^[a-f0-9]{64}$/.test(hashes[key]))) return null;
    return Object.freeze({...hashes});
  }
  const api = {sanitize, overlay, normalizedWindowID, status: {state: 'waiting'}};
  if (typeof module !== 'undefined' && module.exports) {module.exports = {...api, selectorDigest}; return;}
  if (root.CPAQuotaPersistence) return;
  root.CPAQuotaPersistence = api;
  let attached = false, captureBinding = () => null, activeCommit = null, pendingCommit = null, updaterFrame = null;
  const captures = new WeakMap(), updaterProvenance = new WeakMap(), proofs = new WeakMap();
  let checkSource = () => {}, beginSource = () => () => {};
  api.wrapQuotaSource = (original, project) => function (...args) {
    let source;
    try {source = Reflect.apply(project, this, args);} catch {try {source = {name: args[0]?.name};} catch {source = null;}}
    if (source?.mode === 'async-result') {
      // Establish the optional-upload gate BEFORE the original resolver starts.
      // Observe its one actual result; never call it again or replace its Promise.
      const finish = beginSource(source);
      let result;
      try {result = Reflect.apply(original, this, args);} catch (error) {finish(null); throw error;}
      try {Promise.prototype.then.call(result, value => finish(value), () => finish(null));} catch {finish(null);}
      return result;
    }
    if (source?.mode === 'result') {
      let result;
      try {result = Reflect.apply(original, this, args);} catch (error) {checkSource(source, null); throw error;}
      checkSource(source, result);
      return result;
    }
    checkSource(source);
    return Reflect.apply(original, this, args);
  };
  // Return the ORIGINAL generation object. No fields are added to upstream state.
  api.wrapCapture = original => function (...args) {
    const generation = Reflect.apply(original, this, args);
    // A capture taken while this tab cannot trust the binding table still records an
    // untrusted operation, so no later subscriber can adopt it by accident.
    if (generation && typeof generation === 'object') {const scope = captureBinding(generation); captures.set(generation, scope || {untrusted: 0});}
    return generation;
  };
  api.wrapCommit = original => function (...args) {
    const callback = args[1], generation = args[0];
    // Preserve omitted/default third arguments: only replace the callback.
    args[1] = function (...callbackArgs) {
      const previous = activeCommit, previousPending = pendingCommit;
      const scope = {operation: captures.get(generation), name: args[2] === undefined ? generation.name : args[2]};
      activeCommit = scope;
      if (updaterFrame) updaterFrame.scopes.push(scope);
      // The bundle resolves its own updaters through a lexical helper instead of this
      // wrapper, so a synchronous state write made by the callback must still find the
      // same scope. The subscription that write triggers consumes it, and it is dropped
      // when the callback ends -- including when it throws -- so no later unguarded
      // write can inherit a finished operation's provenance.
      pendingCommit = scope;
      try {return Reflect.apply(callback, this, callbackArgs);} finally {activeCommit = previous; if (pendingCommit === scope) pendingCommit = previousPending;}
    };
    // The ORIGINAL native guard alone decides whether the callback runs.
    return Reflect.apply(original, this, args);
  };
  api.wrapUpdater = original => function (...args) {
    // Batch success callbacks mutate a map inside resolveUpdater; subscribers run
    // AFTER those commit callbacks return. Carry only those synchronous scopes to
    // that exact returned map, never to a later arbitrary subscription/microtask.
    const previous = updaterFrame, frame = {scopes: activeCommit ? [activeCommit] : []};
    updaterFrame = frame;
    try {
      const result = Reflect.apply(original, this, args);
      // An updater resolved inside a commit callback is the same synchronous scope,
      // even when the app calls its own resolver instead of this wrapper.
      if (activeCommit && !frame.scopes.includes(activeCommit)) frame.scopes.push(activeCommit);
      if (result && typeof result === 'object' && result !== args[1] && frame.scopes.length) updaterProvenance.set(result, frame.scopes.slice());
      return result;
    } finally {updaterFrame = previous;}
  };
  function report(state, detail = {}) {api.status = {state, ...detail}; root.dispatchEvent(new CustomEvent('cpa-quota-persistence-status', {detail: api.status})); if (state === 'error' || state === 'incompatible' || state === 'unavailable') console.warn('CPA quota persistence:', detail.message || state);}
  api.attach = function ({quotaStore, authStore} = {}) {
    if (attached) return true;
    if (!quotaStore || !authStore || typeof quotaStore.getState !== 'function' || typeof quotaStore.setState !== 'function' || typeof quotaStore.subscribe !== 'function' || typeof quotaStore.getState().clearQuotaCache !== 'function' || typeof authStore.getState !== 'function' || typeof authStore.subscribe !== 'function' || !Object.values(mapNames).every(key => Object.hasOwn(quotaStore.getState(), key))) {report('incompatible', {message: 'Unrecognized upstream stores; original quota UI was not modified.'}); return false;}
    attached = true;
    const clearQuotaCache = quotaStore.getState().clearQuotaCache;
    let session = 0, authenticated = false, key = '', base = '', applying = false, identityClearing = false;
    // Hand the current same-origin management key to the embedded statistics
    // dashboard in this tab only, exactly as the upstream "remember for this tab"
    // option does. The value is never returned or placed in navigation markup.
    api.armStatistics = () => {
      try {if (authenticated && key) window.sessionStorage.setItem('cpa-stats-management-key', key);} catch {}
    };
    let bindings = new Map(), poll = 0, debounce = 0, flushing = null, refreshTicket = 0, globalInvalidated = 0;
    let unknownBatch = false;
    const pending = new Map(), observation = new Map(), invalidated = new Map(), windowTimes = new Map(), denied = new Set(), unknownNames = new Set();
    const requests = new Set(), filename = name => name.split('\0')[0];
    const sourceFences = new Map(), sourcePending = new Map();
    let globalSourceFence = {};
    const sourceName = value => typeof value === 'string' && value.length > 0 && value.length <= 4096 && !value.includes('\0') ? value : null;
    const sourceToken = name => {if (!sourceFences.has(name)) sourceFences.set(name, {}); return sourceFences.get(name);};
    function rejectSource(name) {if (sourceName(name)) sourceFences.set(name, {}); else globalSourceFence = {};}
    function sourceCurrent(operation, name) {return !!operation && !operation.untrusted && operation.globalSourceFence === globalSourceFence && operation.sourceFences.get(name) === sourceToken(name) && !sourcePending.has(name) && bindingCurrent(operation, name);}
    function validateSource(source, result) {
      const name = sourceName(source?.name);
      if (!authenticated || !name || !Object.hasOwn(mapNames, source?.provider)) return false;
      const index = source.index, key = source.provider === 'devin' ? `${name}\0${index}` : name;
      const who = bindingFor(source.provider, key);
      if (!who || !currentBinding(who) || typeof index !== 'string' || index !== who.auth_index) return false;
      let selectors = source.selectors;
      if (source.mode === 'async-result') selectors = {project_id: result};
      if (source.mode === 'result') selectors = {user_id: result?.['x-userid']};
      if (!selectors) return true; // Verified token-only paths still require exact index.
      const proof = proofs.get(who), keys = Object.keys(selectors);
      return !!proof && keys.length > 0 && keys.every(key => typeof selectors[key] === 'string' && selectors[key].length > 0 && Object.hasOwn(proof, key) && selectorDigest(selectors[key]) === proof[key]);
    }
    checkSource = (source, result) => {
      try {if (!validateSource(source, result)) rejectSource(source?.name);} catch {rejectSource(null);}
    };
    beginSource = source => {
      const name = sourceName(source?.name), epoch = session, marker = {};
      if (!name) rejectSource(null);
      else {
        // Concurrent unresolved selectors cannot be assigned to a latest capture.
        if (sourcePending.has(name)) rejectSource(name);
        const set = sourcePending.get(name) || new Set(); set.add(marker); sourcePending.set(name, set);
      }
      let finished = false;
      return result => {
        if (finished || epoch !== session) return; finished = true;
        checkSource(source, result);
        if (name) {const set = sourcePending.get(name); set?.delete(marker); if (!set?.size) sourcePending.delete(name);}
      };
    };
    const slot = (provider, name) => JSON.stringify([provider, name]);
    // Records carry plain account facts. The backend publishes no opaque credential
    // identity any more, so the exact published binding is the operation fence: a
    // removed binding revokes its own operations and nothing else. `auth_index` is
    // transient correlation data only — it is compared to the native request and
    // never stored, uploaded, or used as a map key.
    const identityKey = who => JSON.stringify([who.provider, who.key, who.account, who.account_kind, who.auth_index]);
    const same = (a, b) => !!a && !!b && identityKey(a) === identityKey(b);
    // The dashboard groups by (provider, account), so the bridge keys its own
    // bookkeeping the same way. Several live credentials can resolve to one pair;
    // they share the one persisted cache row the backend keeps for it.
    const groupOf = (map, provider, account, create = false) => {
      const id = slot(provider, account);
      let group = map.get(id);
      if (group) return group;
      if (!create) return undefined;
      group = [];
      map.set(id, group);
      return group;
    };
    const sameGroup = (a, b) => !!a && !!b && a.length === b.length && a.every((who, index) => same(who, b[index]));
    // A binding stays current while the exact published form it was captured from is
    // still in its group and no 409 has revoked that exact form. Membership in the
    // group is the fence: a rotation inside one pair fences only its own old
    // operations, and every other credential is left alone.
    function currentBinding(who) {return (bindings.get(slot(who.provider, who.account)) || []).some(candidate => same(who, candidate)) && !denied.has(identityKey(who));}
    function bindingFor(provider, key) {
      for (const group of bindings.values()) for (const who of group) if (who.provider === provider && who.key === key) return who;
      return undefined;
    }
    function nativeCurrent(operation, name) {
      if (!operation || operation.session !== session) return false;
      const current = quotaStore.getState(), generation = operation.generation;
      return current.cacheGeneration === generation.cacheGeneration && (current.fileGenerations?.[name] ?? 0) === (generation.fileGenerations?.[name] ?? 0);
    }
    // The captured binding table must still describe the same credentials: a poll
    // that learned a different state for that pair has fenced its older operations.
    function bindingCurrent(operation, name) {
      if (operation.bindings === bindings) return true;
      const who = [...operation.bindings.values()].flat().find(candidate => filename(candidate.key) === name);
      return !!who && sameGroup(operation.bindings.get(slot(who.provider, who.account)), bindings.get(slot(who.provider, who.account)));
    }
    captureBinding = generation => {
      if (!authenticated) return null;
      const selected = [...bindings.values()].flat().filter(who => !denied.has(identityKey(who)) && (generation.name === undefined || filename(who.key) === generation.name));
      // A named capture for a filename no binding covers is remembered: a binding
      // that appears later must fence it instead of adopting it. A bulk capture is
      // remembered with it, because it covers every filename at once.
      if (generation.name === undefined) unknownBatch = true;
      else if (!selected.length) unknownNames.add(generation.name);
      const fences = new Map();
      for (const who of selected) {const name = filename(who.key); if (!fences.has(name)) fences.set(name, sourcePending.has(name) ? {} : sourceToken(name));}
      return {session, generation, provider: generation.provider, bindings, sourceFences: fences, globalSourceFence};
    };
    function clearNative(names) {if (!names.length) return; identityClearing = true; try {clearQuotaCache(names);} finally {identityClearing = false;}}
    function reset() {
      if (authenticated) clearNative([...new Set([...bindings.values()].flat().map(who => filename(who.key)))]);
      session++; refreshTicket++; authenticated = false; key = ''; base = ''; bindings = new Map();
      pending.clear(); observation.clear(); invalidated.clear(); windowTimes.clear(); denied.clear(); unknownNames.clear(); unknownBatch = false; globalInvalidated = 0;
      sourceFences.clear(); sourcePending.clear(); globalSourceFence = {};
      clearTimeout(poll); clearTimeout(debounce); for (const controller of requests) controller.abort(); requests.clear(); flushing = null;
    }
    async function request(path, options = {}) {
      const controller = new AbortController(); requests.add(controller); const epoch = session;
      try {
        const response = await fetch(`${base}/v0/management/${path}`, {method: options.method || 'GET', headers: {'Accept': 'application/json', ...(key ? {'Authorization': `Bearer ${key}`} : {}), ...(options.body ? {'Content-Type': 'application/json'} : {})}, credentials: 'same-origin', redirect: 'error', referrerPolicy: 'no-referrer', cache: 'no-store', signal: controller.signal, ...(options.body ? {body: JSON.stringify(options.body)} : {})});
        if (epoch !== session) throw new DOMException('Session changed', 'AbortError');
        if (response.status === 401 || response.status === 403) {reset(); report('authentication-required'); throw new Error('Management authentication required.');}
        if (!response.ok) {const error = new Error(`Quota persistence is unavailable (HTTP ${response.status}). Check persistence storage and server logs. The original management page and manual quota refresh remain available.`); error.status = response.status; throw error;}
        const result = response.status === 204 ? {} : await response.json();
        if (epoch !== session) throw new DOMException('Session changed', 'AbortError');
        return result;
      } finally {requests.delete(controller);}
    }
    function installBindings(payload) {
      // Parse only secret-free server bindings. Malformed entries are dropped, and
      // no token ever appears in them. The account fact is persisted deliberately;
      // `auth_index` stays transient correlation data and is never used as a key.
      const next = new Map();
      for (const item of Array.isArray(payload?.bindings) ? payload.bindings : []) {
        if (!item || !Object.hasOwn(mapNames, item.provider) || typeof item.key !== 'string' || !item.key || item.key.length > 4096) continue;
        if (!['account', 'account_kind', 'auth_index'].every(field => typeof item[field] === 'string' && item[field].length <= 4096)) continue;
        const who = Object.freeze({provider: item.provider, key: item.key, account: item.account, account_kind: item.account_kind, auth_index: item.auth_index});
        if (!filename(who.key) || (who.provider === 'devin' ? who.key !== `${filename(who.key)}\0${who.auth_index}` : who.key.includes('\0'))) continue;
        const proof = manualProof(item.manual_source_proof);
        if (proof) proofs.set(who, proof);
        const group = groupOf(next, who.provider, who.account, true);
        // Compare against the binding table still in force, not the table being built.
        // A selector proof that changed rotates this filename's source fence so an
        // operation captured under the previous proof can never be re-stamped.
        const previous = (groupOf(bindings, who.provider, who.account, false) || []).find(candidate => same(candidate, who));
        if (previous && JSON.stringify(proofs.get(previous)) !== JSON.stringify(proof || undefined)) rejectSource(filename(who.key));
        // Several live credentials may resolve to one account. They all remain
        // correlatable, and they share the single persisted cache row for the pair.
        if (!group.some(candidate => same(candidate, who))) group.push(who);
      }
      const changed = new Set();
      const touched = new Set([...bindings.keys(), ...next.keys()]);
      for (const id of touched) {
        const before = bindings.get(id), after = next.get(id);
        const removed = [...(before || [])].filter(who => !(after || []).some(candidate => same(candidate, who)));
        if (!removed.length) continue;
        for (const who of removed) {
          const name = filename(who.key); changed.add(name);
          // A -> B -> A in this tab must not revive A's pre-replacement history.
          invalidated.set(name, Date.now());
        }
      }
      for (const group of next.values()) for (const who of group) {
        const name = filename(who.key);
        const before = bindings.get(slot(who.provider, who.account)) || [];
        if (before.some(candidate => same(candidate, who))) continue;
        // A named capture already proved that this tab ran an operation the binding
        // table did not cover, so the new binding must fence and clear it. A bulk
        // capture keeps no such proof, and a binding appearing after one owns no old
        // operation: nothing is cleared for it.
        if (unknownBatch || unknownNames.has(name)) changed.add(name);
      }
      // Native clears are filename-wide, including unchanged sibling bindings. A
      // binding that was dropped or newly observed in this tab is treated as
      // replaced: its old operations are fenced and its display state reloads from
      // the server cache. An unchanged binding is never cleared here.
      bindings = next;
      for (const name of changed) discardFile(name);
      // A successful fresh lookup permits NEW operations even when one valid
      // sibling was dropped in a 409 batch. Old operations remain natively fenced.
      denied.clear();
      if (changed.size) clearNative([...changed]);
      for (const who of [...bindings.values()].flat()) unknownNames.delete(filename(who.key));
    }
    function discardFile(name) {
      for (const [id, value] of pending) if (filename(value.entry.key) === name) pending.delete(id);
      for (const map of [observation, windowTimes]) for (const id of map.keys()) if (filename(JSON.parse(id)[1]) === name) map.delete(id);
    }
    function sourceBinding(provider, name, scopes) {
      const matches = scopes.filter(scope => scope && (scope.name === undefined || scope.name === filename(name)));
      if (!matches.length) return null;
      const operation = matches[0].operation;
      // Distinct operations touching one output are ambiguous, never guessed.
      if (matches.some(scope => scope.operation !== operation) || !nativeCurrent(operation, filename(name)) || !sourceCurrent(operation, filename(name))) return null;
      const who = bindingFor(provider, name);
      return who && currentBinding(who) ? {who, operation} : null;
    }
    function queue(provider, name, value, scopes) {
      const source = sourceBinding(provider, name, scopes), safe = sanitize(provider, value);
      if (!source || !safe) return; // Unknown-at-start success is NEVER promoted.
      const {who, operation} = source, id = identityKey(who), observed = new Date().toISOString();
      observation.set(id, stamp(observed)); windowTimes.delete(id);
      // The queued entry keeps the captured binding so a retry can still prove it is
      // current; `uploadEntry` alone decides what leaves this page.
      pending.set(id, {entry: {...who, observed_at: observed, state: safe}, operation});
      clearTimeout(debounce); debounce = setTimeout(() => void flush(), 500);
    }
    // Plain account facts only: the transient credential index that correlates the
    // native request never leaves the page, and no opaque credential identity is
    // recorded at all.
    const uploadEntry = entry => ({provider: entry.provider, key: entry.key, account: entry.account, account_kind: entry.account_kind, observed_at: entry.observed_at, state: entry.state});
    const uploadCurrent = value => currentBinding(value.entry) && nativeCurrent(value.operation, filename(value.entry.key)) && sourceCurrent(value.operation, filename(value.entry.key));
    async function flush() {
      if (!authenticated || !pending.size || flushing) return;
      const token = {}; flushing = token; const epoch = session, batch = []; let bytes = 32;
      for (const [id, value] of pending) {
        if (!uploadCurrent(value)) {pending.delete(id); continue;}
        const size = new TextEncoder().encode(JSON.stringify(uploadEntry(value.entry))).length + 1;
        if (size > 900000) {pending.delete(id); report('error', {message: 'A quota observation exceeds the persistence size limit; refresh remains available but this observation was not saved.'}); continue;}
        if (batch.length >= 100 || bytes + size > 900000) break;
        batch.push([id, value]); bytes += size;
      }
      for (const [id] of batch) pending.delete(id);
      try {
        if (batch.length) {await request('stats/quota/cache', {method: 'PUT', body: {entries: batch.map(([, value]) => uploadEntry(value.entry))}}); if (epoch === session) report('ready');}
      } catch (error) {
        if (epoch === session && error.name !== 'AbortError') {
          if (error.status === 409) {
            // A delayed conflict owns only its still-current binding AND native
            // operation, never a filename with newer valid work waiting to upload.
            const newer = new Set([...pending.values()].filter(uploadCurrent).map(value => filename(value.entry.key)));
            const names = new Set(); for (const [id, value] of batch) if (uploadCurrent(value) && !newer.has(filename(value.entry.key))) {denied.add(id); names.add(filename(value.entry.key));}
            for (const name of names) discardFile(name); clearNative([...names]);
            report('synchronizing'); void refresh(); // Terminal drop: no restamp/retry.
          } else {
            for (const [id, value] of batch) if (!pending.has(id) && uploadCurrent(value)) pending.set(id, value);
            report(error.status === 503 ? 'unavailable' : 'error', {message: `${error.status === 503 ? error.message + ' ' : ''}Could not persist quota observations. Current observations remain in memory and will retry while this session is open.`});
          }
        }
      } finally {if (flushing === token) flushing = null; if (epoch === session && pending.size) debounce = setTimeout(() => void flush(), 15000);}
    }
    quotaStore.subscribe((next, previous) => {
      if (!authenticated) return;
      if (next.cacheGeneration !== previous.cacheGeneration) {
        pending.clear(); observation.clear(); windowTimes.clear(); invalidated.clear();
        if (!identityClearing) globalInvalidated = Date.now();
      }
      for (const name of new Set([...Object.keys(next.fileGenerations || {}), ...Object.keys(previous.fileGenerations || {})])) {
        if (next.fileGenerations?.[name] !== previous.fileGenerations?.[name]) {discardFile(name); if (!identityClearing) invalidated.set(name, Date.now());}
      }
      if (applying) return;
      for (const [provider, map] of Object.entries(mapNames)) {
        if (next[map] === previous[map]) continue;
        const scopes = updaterProvenance.get(next[map]) || (activeCommit ? [activeCommit] : pendingCommit ? [pendingCommit] : []);
        updaterProvenance.delete(next[map]);
        pendingCommit = null;
        for (const [name, value] of Object.entries(next[map] || {})) if (value !== previous[map]?.[name] && value?.status === 'success') queue(provider, name, value, scopes);
      }
    });
    async function refresh() {
      if (!authenticated) return; const epoch = session, ticket = ++refreshTicket;
      const currentRefresh = () => epoch === session && ticket === refreshTicket && authenticated;
      try {
        const identities = await request('stats/quota/identities'); if (!currentRefresh()) return;
        installBindings(identities);
        const [cached, normalized] = await Promise.all([request('stats/quota/cache'), request('stats/quota')]); if (!currentRefresh()) return;
        // Re-read AFTER both payloads. A delayed A response must not hydrate B.
        const verified = await request('stats/quota/identities'); if (!currentRefresh()) return;
        installBindings(verified);
        const patch = {}, current = quotaStore.getState(); let hydrated = 0, updated = 0, newest = 0;
        const cutoff = name => Math.max(globalInvalidated, invalidated.get(filename(name)) || 0);
        for (const entry of Array.isArray(cached.entries) ? cached.entries : []) {
          if (!entry) continue;
          const group = bindings.get(slot(entry.provider, entry.account));
          // Durable cache identity is the account fact alone; stored history
          // hydrates, and it is never uploaded back as a new observation.
          const who = group?.find(candidate => candidate.account === String(entry.account || '') && candidate.account_kind === String(entry.account_kind || '') && currentBinding(candidate));
          if (!who) continue;
          const map = mapNames[who.provider], existing = current[map]?.[who.key], at = stamp(entry.observed_at), id = identityKey(who);
          if (existing && existing.status !== 'idle' || !at || at > Date.now() + 300000 || at <= cutoff(who.key)) continue;
          const safe = sanitize(who.provider, entry.state); if (!safe) continue;
          patch[map] ||= {...current[map]}; patch[map][who.key] = safe; observation.set(id, at); newest = Math.max(newest, at); hydrated++;
        }
        for (const snapshot of Array.isArray(normalized.snapshots) ? normalized.snapshots : []) {
          if (!snapshot || !Object.hasOwn(mapNames, snapshot.provider)) continue;
          for (const who of [...bindings.values()].flat()) {
            if (who.provider !== snapshot.provider || who.account !== String(snapshot.account || '') || !currentBinding(who)) continue;
            const map = mapNames[who.provider], id = identityKey(who), existing = patch[map]?.[who.key] || current[map]?.[who.key];
            if (existing?.status === 'success' && !observation.has(id)) continue;
            if (!windowTimes.has(id)) windowTimes.set(id, new Map());
            const result = overlay(who.provider, existing, snapshot, Math.max(observation.get(id) || 0, cutoff(who.key)), windowTimes.get(id));
            if (result.changed) {patch[map] ||= {...current[map]}; patch[map][who.key] = result.state; if (!observation.has(id)) observation.set(id, 0); updated++;}
          }
        }
        if (Object.keys(patch).length && currentRefresh()) {applying = true; try {quotaStore.setState(patch);} finally {applying = false;}}
        report('ready', {hydrated, updated, ...(newest ? {cache_observed_at: new Date(newest).toISOString()} : {})});
      } catch (error) {if (currentRefresh() && error.name !== 'AbortError') report(error.status === 503 ? 'unavailable' : 'error', {message: error.status === 503 ? error.message : 'Quota history could not synchronize. The original quota page and manual refresh are unchanged.'});}
      finally {if (currentRefresh()) {clearTimeout(poll); poll = setTimeout(() => {if (!document.hidden && navigator.onLine !== false) void refresh(); else {poll = setTimeout(() => void refresh(), 15000);}}, 15000);}}
    }
    function authChanged(auth) {
      const active = auth.isAuthenticated === true && auth.connectionStatus === 'connected';
      let url; try {url = new URL(auth.apiBase || location.origin, location.href);} catch {reset(); report('incompatible', {message: 'Invalid management API origin.'}); return;}
      if (!active) {if (authenticated) reset(); report('waiting-for-login'); return;}
      // An externally hosted management UI may connect to a different server.
      // Never send that server's management key to this page's local endpoint.
      if (url.origin !== location.origin || url.username || url.password || url.search || url.hash) {reset(); report('incompatible', {message: 'Quota persistence only attaches to a same-origin management connection.'}); return;}
      const nextBase = url.origin + url.pathname.replace(/\/$/, '');
      if (authenticated && key === auth.managementKey && base === nextBase) return;
      reset(); authenticated = true; key = typeof auth.managementKey === 'string' ? auth.managementKey : ''; base = nextBase; report('synchronizing'); void refresh();
    }
    authStore.subscribe(authChanged); authChanged(authStore.getState());
    root.addEventListener('pagehide', reset);
    root.addEventListener('pageshow', event => {if (event.persisted) authChanged(authStore.getState());});
    root.addEventListener('online', () => {if (authenticated) void refresh();});
    return true;
  };
})(typeof window === 'undefined' ? globalThis : window);
