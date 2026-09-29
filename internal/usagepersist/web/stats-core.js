/* Pure presentation helpers; shared by the dashboard and dependency-free tests. */
(function (root) {
  'use strict';
  const number = value => Number.isFinite(Number(value)) ? Math.max(0, Number(value)) : 0;
  const integer = value => new Intl.NumberFormat(undefined, {maximumFractionDigits: 0}).format(number(value));
  const compact = value => new Intl.NumberFormat(undefined, {notation: 'compact', maximumFractionDigits: 1}).format(number(value));
  const money = value => new Intl.NumberFormat(undefined, {style: 'currency', currency: 'USD', minimumFractionDigits: 2, maximumFractionDigits: number(value) > 0 && number(value) < .01 ? 6 : 2}).format(number(value));
  const duration = value => value == null ? '—' : number(value) < 1000 ? `${integer(value)} ms` : `${(number(value) / 1000).toFixed(2)} s`;
  const percent = (part, total) => number(total) ? `${Math.min(100, number(part) / number(total) * 100).toFixed(1)}%` : '—';
  const date = value => {
    const parsed = new Date(value);
    return value && Number.isFinite(parsed.getTime()) ? parsed.toLocaleString() : '—';
  };
  function query(filters, now = Date.now()) {
    const params = new URLSearchParams();
    const ranges = {'1h': 3600000, '24h': 86400000, '7d': 604800000, '30d': 2592000000};
    if (filters.range === 'custom') {
      const from = new Date(filters.from), to = new Date(filters.to);
      if (!filters.from || !filters.to || !Number.isFinite(from.getTime()) || !Number.isFinite(to.getTime()) || from >= to) throw new Error('Choose a valid custom range with the end after the start.');
      params.set('from', from.toISOString()); params.set('to', to.toISOString());
      params.set('bucket', to - from > 3 * 86400000 ? 'day' : 'hour');
    } else if (ranges[filters.range]) {
      params.set('from', new Date(now - ranges[filters.range]).toISOString()); params.set('to', new Date(now).toISOString());
      params.set('bucket', ranges[filters.range] > 3 * 86400000 ? 'day' : 'hour');
    } else {
      params.set('from', '1970-01-01T00:00:00.000Z'); params.set('to', new Date(now).toISOString());
      params.set('bucket', 'day');
    }
    for (const key of ['provider', 'model', 'auth_index', 'key_id', 'status']) if (filters[key]) params.set(key, filters[key]);
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
  const api = {number, integer, compact, money, duration, percent, date, query, seriesPoints, csvCell, price, arrays};
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.CPAStats = api;
})(typeof window === 'undefined' ? globalThis : window);
