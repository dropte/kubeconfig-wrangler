'use strict';

const { test } = require('node:test');
const assert = require('node:assert');
const pkg = require('../package.json');

// electron-builder only attaches to the release workflow's draft when its tag
// equals v${version}. On a miss it creates its own release, so that release
// must be a draft too, never a live one that bypasses the verify gate.
test('electron-builder publishes to a draft release', () => {
  const publish = [].concat(pkg.build.publish);
  assert.ok(publish.length > 0);
  for (const p of publish) {
    assert.strictEqual(p.provider, 'github');
    assert.strictEqual(p.releaseType, 'draft');
  }
});

test('tests are not packaged into the app', () => {
  assert.ok(pkg.build.files.some((f) => f.startsWith('!test')));
});
