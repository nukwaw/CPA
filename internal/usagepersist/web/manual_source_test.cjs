'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict');
const {createHash} = require('node:crypto');
const {selectorDigest} = require('./management-bridge.js');

test('bounded synchronous selector SHA-256 matches Node exact UTF-8 vectors without WebCrypto', () => {
  const vectors = ['', 'abc', 'B', 'account-A', ' account-A ', 'é', 'e\u0301', '账户🛰️', '\ud800', 'line\nvalue', '\0'];
  for (const length of [1, 7, 55, 56, 63, 64, 65, 127, 128, 129, 4095, 4096]) vectors.push('a'.repeat(length));
  vectors.push('🛰️'.repeat(1000));
  for (const value of vectors) assert.equal(selectorDigest(value), createHash('sha256').update(value, 'utf8').digest('hex'), JSON.stringify(value.slice(0, 64)));
  assert.notEqual(selectorDigest('é'), selectorDigest('e\u0301'), 'must not normalize Unicode');
  assert.notEqual(selectorDigest(' A'), selectorDigest('A'), 'must not trim the actual resolved selector');
});
test('selector digest rejects unbounded or non-string inputs instead of coercing aliases', () => {
  for (const value of ['a'.repeat(4097), null, undefined, 1, {}, [], new String('B')]) assert.equal(selectorDigest(value), null);
});
