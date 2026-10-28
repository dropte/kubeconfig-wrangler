'use strict';

// Exercises the macOS Dock-reopen path in main.js with Electron, the updater
// and child_process stubbed out. No real backend binary is ever spawned.

const { test } = require('node:test');
const assert = require('node:assert');
const EventEmitter = require('node:events');
const Module = require('node:module');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const MAIN = path.join(__dirname, '..', 'main.js');

function loadMain({ binaryExists }) {
  const resources = fs.mkdtempSync(path.join(os.tmpdir(), 'kcw-electron-test-'));
  if (binaryExists) {
    const bin = path.join(resources, 'bin');
    fs.mkdirSync(bin);
    const name = process.platform === 'win32' ? 'kubeconfig-wrangler.exe' : 'kubeconfig-wrangler';
    fs.writeFileSync(path.join(bin, name), '');
  }
  process.resourcesPath = resources;

  const calls = { errorBoxes: [], windows: [], spawns: [] };
  const appHandlers = {};
  const noop = () => {};

  class FakeWindow extends EventEmitter {
    constructor(opts) {
      super();
      calls.windows.push(opts);
      this.webContents = new EventEmitter();
      this.webContents.setWindowOpenHandler = noop;
    }
    loadURL() {}
    isDestroyed() { return false; }
    show() {}
    reload() {}
  }
  FakeWindow.getAllWindows = () => [];

  const electron = {
    app: {
      isPackaged: true,
      setName: noop,
      getVersion: () => '0.0.0-test',
      whenReady: () => new Promise(noop), // never run the real startup
      on: (evt, fn) => { appHandlers[evt] = fn; },
      quit: noop
    },
    BrowserWindow: FakeWindow,
    ipcMain: { handle: noop },
    dialog: { showErrorBox: (title, msg) => calls.errorBoxes.push({ title, msg }) },
    shell: { openExternal: noop },
    nativeImage: { createFromPath: noop },
    powerMonitor: { on: noop },
    Menu: { buildFromTemplate: noop, setApplicationMenu: noop }
  };
  const updater = { autoUpdater: { on: noop } };
  const realCP = require('node:child_process');
  const childProcess = {
    ...realCP,
    spawn: (cmd, args) => {
      calls.spawns.push({ cmd, args });
      const child = new EventEmitter();
      child.stdout = new EventEmitter();
      child.stderr = new EventEmitter();
      child.exitCode = null;
      child.signalCode = null;
      child.kill = () => { child.signalCode = 'SIGKILL'; };
      setImmediate(() => child.stdout.emit('data', Buffer.from('Starting web server\n')));
      return child;
    }
  };

  const origLoad = Module._load;
  Module._load = function (request, ...rest) {
    if (request === 'electron') return electron;
    if (request === 'electron-updater') return updater;
    if (request === 'child_process' || request === 'node:child_process') return childProcess;
    return origLoad.call(this, request, ...rest);
  };
  delete require.cache[MAIN];
  let main;
  try {
    main = require(MAIN);
  } finally {
    Module._load = origLoad;
  }

  const cleanup = () => {
    if (appHandlers['before-quit']) appHandlers['before-quit']();
    fs.rmSync(resources, { recursive: true, force: true });
  };
  return { main, calls, appHandlers, cleanup };
}

test('activate with a backend that cannot start shows an error and creates no window', async (t) => {
  const { main, calls, appHandlers, cleanup } = loadMain({ binaryExists: false });
  t.after(cleanup);

  assert.strictEqual(appHandlers.activate, undefined, 'activate is registered only after startup');
  await main.onActivate();

  assert.strictEqual(calls.windows.length, 0, 'no window against a dead backend');
  assert.strictEqual(calls.spawns.length, 0);
  assert.ok(
    calls.errorBoxes.some((b) => b.title === 'Backend Error'),
    `expected a Backend Error dialog, got ${JSON.stringify(calls.errorBoxes)}`
  );
  assert.ok(main.isHealthCheckRunning(), 'health check keeps retrying after a failed restart');
});

test('ensureBackend starts the health check even when the restart fails', async (t) => {
  const { main, cleanup } = loadMain({ binaryExists: false });
  t.after(cleanup);

  assert.strictEqual(main.isHealthCheckRunning(), false);
  assert.strictEqual(await main.ensureBackend(), false);
  assert.ok(main.isHealthCheckRunning());
});

test('activate with a backend that starts creates the window', async (t) => {
  const { main, calls, cleanup } = loadMain({ binaryExists: true });
  t.after(cleanup);

  await main.onActivate();

  assert.strictEqual(calls.spawns.length, 1);
  assert.deepStrictEqual(calls.errorBoxes, []);
  assert.strictEqual(calls.windows.length, 1);
  assert.ok(main.isHealthCheckRunning());
});
