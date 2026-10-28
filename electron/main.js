const { app, BrowserWindow, ipcMain, dialog, shell, nativeImage, powerMonitor, Menu } = require('electron');
const path = require('path');
const http = require('http');
const { spawn } = require('child_process');
const fs = require('fs');
const net = require('net');
const crypto = require('crypto');
const { autoUpdater } = require('electron-updater');

// Set app name for development
app.setName('Kubeconfig Wrangler');

let mainWindow = null;
let backendProcess = null;
let serverPort = 18080;
let serverToken = '';
let isQuitting = false;
let healthCheckInterval = null;
let healthCheckInFlight = false;
let restartPromise = null;
let restartAttempts = 0;
let gaveUpRestarting = false;
let loadRetries = 0;
let interactiveUpdateCheck = false;

// Every spawned backend child, so stopBackend() can kill stragglers too.
const backendChildren = new Set();

const MAX_RESTART_ATTEMPTS = 3;
const MAX_LOAD_RETRIES = 5;
const HEALTH_CHECK_INTERVAL = 5000; // 5 seconds
const READY_POLL_INTERVAL = 250;
const READY_TIMEOUT = 10000;

// Generate a cryptographically secure random token
function generateSecurityToken() {
  return crypto.randomBytes(32).toString('hex');
}

function backendOrigin() {
  return `http://127.0.0.1:${serverPort}`;
}

// URL the window loads; "/" is only served to requests carrying the token.
function appURL() {
  return `${backendOrigin()}/?token=${serverToken}`;
}

function isBackendURL(url) {
  try {
    return new URL(url).origin === backendOrigin();
  } catch {
    return false;
  }
}

// Find an available port
async function findAvailablePort(startPort) {
  return new Promise((resolve) => {
    const server = net.createServer();
    server.listen(startPort, '127.0.0.1', () => {
      const port = server.address().port;
      server.close(() => resolve(port));
    });
    server.on('error', () => {
      resolve(findAvailablePort(startPort + 1));
    });
  });
}

// Map Node.js platform names to electron-builder directory names
function getPlatformDir() {
  switch (process.platform) {
    case 'darwin':
      return 'mac';
    case 'win32':
      return 'win';
    default:
      return 'linux';
  }
}

// Get the path to the backend binary
function getBackendPath() {
  const isDev = !app.isPackaged;
  const isWindows = process.platform === 'win32';
  const binaryName = isWindows ? 'kubeconfig-wrangler.exe' : 'kubeconfig-wrangler';

  if (isDev) {
    // In development, look for the binary in the parent bin directory
    const platformDir = getPlatformDir();
    return path.join(__dirname, '..', 'bin', platformDir, binaryName);
  } else {
    // In production, look in the resources directory
    return path.join(process.resourcesPath, 'bin', binaryName);
  }
}

// Kill a backend child we no longer want; its 'close' handler will not
// treat the resulting exit as a crash.
function killChild(child, signal) {
  child.expectedExit = true;
  try {
    child.kill(signal);
  } catch (e) {
    // Ignore errors killing the process
  }
}

// Start the Go backend server. Resolves true once the server answers.
async function startBackend() {
  if (backendProcess && await checkBackendHealth()) {
    return true;
  }

  const backendPath = getBackendPath();

  // Check if the backend binary exists
  if (!fs.existsSync(backendPath)) {
    console.error('Backend binary not found at:', backendPath);
    const platformDir = getPlatformDir();
    dialog.showErrorBox(
      'Backend Not Found',
      `The backend server binary was not found at:\n${backendPath}\n\nPlease build the Go backend first using:\ngo build -o bin/${platformDir}/kubeconfig-wrangler`
    );
    return false;
  }

  if (backendProcess) {
    killChild(backendProcess, 'SIGKILL');
    backendProcess = null;
  }

  // Find an available port
  serverPort = await findAvailablePort(18080);

  // Generate a new security token for this session
  serverToken = generateSecurityToken();

  console.log('Starting backend server on port', serverPort);
  console.log('Backend path:', backendPath);

  // The token goes through the environment, not argv, so it is not visible in ps.
  const child = spawn(backendPath, ['serve', '--port', serverPort.toString(), '--addr', '127.0.0.1'], {
    stdio: ['ignore', 'pipe', 'pipe'],
    env: { ...process.env, KUBECONFIG_WRANGLER_TOKEN: serverToken }
  });
  backendProcess = child;
  backendChildren.add(child);

  child.on('close', (code, signal) => {
    backendChildren.delete(child);
    if (child !== backendProcess || child.expectedExit) {
      return;
    }
    console.log('Backend process exited unexpectedly with', code ?? signal);
    backendProcess = null;
    if (!isQuitting) {
      restartBackend();
    }
  });

  return new Promise((resolve) => {
    let settled = false;
    let pollTimer = null;
    let deadline = null;

    const finish = (ok) => {
      if (settled) return;
      settled = true;
      clearInterval(pollTimer);
      clearTimeout(deadline);
      child.removeListener('exit', onExit);
      child.removeListener('error', onError);
      if (!ok) {
        // A failed start is reported to the caller; the exit is not a crash to restart from.
        child.expectedExit = true;
        if (child.exitCode === null && child.signalCode === null) {
          killChild(child, 'SIGKILL');
        }
        if (backendProcess === child) backendProcess = null;
      }
      resolve(ok);
    };

    const onLog = (stream) => (data) => {
      const text = data.toString();
      if (stream === 'stdout') {
        console.log('Backend:', text);
      } else {
        console.error('Backend:', text);
      }
      // The Go server logs 'Listening on <addr>' once the port is bound
      // (older builds logged 'Starting web server').
      if (text.includes('Listening on') || text.includes('Starting web server')) {
        finish(true);
      }
    };
    const onExit = (code, signal) => {
      console.error('Backend exited before becoming ready:', code ?? signal);
      finish(false);
    };
    const onError = (error) => {
      console.error('Failed to start backend:', error);
      finish(false);
    };

    child.stdout.on('data', onLog('stdout'));
    child.stderr.on('data', onLog('stderr'));
    child.once('exit', onExit);
    child.once('error', onError);

    let probing = false;
    pollTimer = setInterval(async () => {
      if (probing || settled) return;
      probing = true;
      const ok = await checkBackendHealth();
      probing = false;
      if (ok) finish(true);
    }, READY_POLL_INTERVAL);

    deadline = setTimeout(() => {
      console.error('Backend did not become ready within', READY_TIMEOUT, 'ms');
      finish(false);
    }, READY_TIMEOUT);
  });
}

// Stop the backend server (and any other children we spawned)
function stopBackend() {
  stopHealthCheck();
  if (backendChildren.size > 0) {
    console.log('Stopping backend server...');
  }
  for (const child of backendChildren) {
    killChild(child);
  }
  backendProcess = null;
}

function probe(pathname) {
  return new Promise((resolve) => {
    const req = http.request({
      hostname: '127.0.0.1',
      port: serverPort,
      path: pathname,
      method: 'GET',
      headers: { 'X-Auth-Token': serverToken },
      timeout: 3000
    }, (res) => {
      res.resume();
      resolve(res.statusCode);
    });

    req.on('error', () => resolve(0));
    req.on('timeout', () => {
      req.destroy();
      resolve(0);
    });
    req.end();
  });
}

// Health check: authenticated GET /api/status. A server that does not know
// our token (401/403) is not our backend and counts as unhealthy.
async function checkBackendHealth() {
  const status = await probe('/api/status');
  if (status === 404) {
    // Older backend without /api/status: the token-gated index page works too.
    return (await probe('/')) === 200;
  }
  return status === 200;
}

// Start health check monitoring
function startHealthCheck() {
  stopHealthCheck();

  healthCheckInterval = setInterval(async () => {
    if (isQuitting || healthCheckInFlight || restartPromise) return;
    healthCheckInFlight = true;
    try {
      if (!backendProcess) {
        console.log('Backend process not running, attempting restart...');
        await restartBackend();
        return;
      }

      if (await checkBackendHealth()) {
        restartAttempts = 0;
        return;
      }

      console.log('Backend health check failed, checking once more...');
      if (!(await checkBackendHealth())) {
        console.log('Backend still not responding, restarting...');
        await restartBackend();
      }
    } finally {
      healthCheckInFlight = false;
    }
  }, HEALTH_CHECK_INTERVAL);
}

// Stop health check monitoring
function stopHealthCheck() {
  if (healthCheckInterval) {
    clearInterval(healthCheckInterval);
    healthCheckInterval = null;
  }
}

// Restart backend with retry logic. Concurrent callers share one restart.
function restartBackend() {
  if (restartPromise) return restartPromise;
  restartPromise = doRestartBackend().finally(() => {
    restartPromise = null;
  });
  return restartPromise;
}

async function doRestartBackend() {
  if (isQuitting || gaveUpRestarting) return false;

  if (restartAttempts >= MAX_RESTART_ATTEMPTS) {
    console.error('Max restart attempts reached, giving up');
    gaveUpRestarting = true;
    stopHealthCheck();
    dialog.showErrorBox(
      'Backend Error',
      'The backend server has crashed multiple times and cannot be restarted. Please restart the application.'
    );
    return false;
  }

  restartAttempts++;
  console.log(`Attempting to restart backend (attempt ${restartAttempts}/${MAX_RESTART_ATTEMPTS})...`);

  // Kill the existing process if it's still around
  if (backendProcess) {
    killChild(backendProcess, 'SIGKILL');
    backendProcess = null;
  }

  // Wait a moment before restarting
  await new Promise(resolve => setTimeout(resolve, 1000));
  if (isQuitting) return false;

  const success = await startBackend();

  if (success && mainWindow && !mainWindow.isDestroyed()) {
    console.log('Backend restarted successfully, reloading window...');
    loadRetries = 0;
    mainWindow.loadURL(appURL());
  }

  return success;
}

// Bring the backend back after it was stopped or gave up (e.g. macOS Dock reopen).
async function ensureBackend() {
  if (backendProcess || isQuitting) return true;
  restartAttempts = 0;
  gaveUpRestarting = false;
  const ok = await restartBackend();
  // Keep monitoring even when this restart failed: each tick retries while
  // backendProcess is null, until MAX_RESTART_ATTEMPTS gives up with a dialog.
  if (!gaveUpRestarting && !isQuitting) startHealthCheck();
  return ok;
}

// macOS Dock reopen: bring the backend back if needed, then reopen the window.
// A window pointed at a dead backend would only show a blank page, so tell the
// user instead (the health check keeps retrying in the background).
async function onActivate() {
  if (BrowserWindow.getAllWindows().length > 0 || isQuitting) return;
  if (!backendProcess) {
    const ok = await ensureBackend();
    if (!ok) {
      if (!isQuitting) {
        dialog.showErrorBox(
          'Backend Error',
          'The backend server could not be started. Please try again or restart the application.'
        );
      }
      return;
    }
  }
  if (!mainWindow && !isQuitting) {
    createWindow();
  }
}

// Create the main window
function createWindow() {
  mainWindow = new BrowserWindow({
    width: 1000,
    height: 800,
    minWidth: 800,
    minHeight: 600,
    webPreferences: {
      nodeIntegration: false,
      contextIsolation: true,
      sandbox: true,
      preload: path.join(__dirname, 'preload.js')
    },
    icon: path.join(__dirname, 'build', 'icon.png'),
    title: 'Kubeconfig Wrangler',
    show: false // Don't show until ready
  });

  loadRetries = 0;
  // Load the web interface from the backend server
  mainWindow.loadURL(appURL());

  // Show window when ready
  mainWindow.once('ready-to-show', () => {
    mainWindow.show();
  });

  // Handle window closed
  mainWindow.on('closed', () => {
    mainWindow = null;
  });

  // Open external web links in the default browser; never hand other
  // schemes (file:, smb:, custom protocol handlers) to the OS.
  mainWindow.webContents.setWindowOpenHandler(({ url }) => {
    try {
      const { protocol } = new URL(url);
      if (protocol === 'https:' || protocol === 'http:') {
        shell.openExternal(url);
      }
    } catch {
      // Invalid URL: deny
    }
    return { action: 'deny' };
  });

  // Keep the window on the backend origin.
  const blockForeignNavigation = (event, url) => {
    if (!isBackendURL(url)) {
      console.warn('Blocked navigation to', url);
      event.preventDefault();
    }
  };
  mainWindow.webContents.on('will-navigate', blockForeignNavigation);
  mainWindow.webContents.on('will-redirect', blockForeignNavigation);

  mainWindow.webContents.on('did-finish-load', () => {
    loadRetries = 0;
  });

  // Handle page load failures (e.g., after sleep/resume), with a bounded retry.
  mainWindow.webContents.on('did-fail-load', (event, errorCode, errorDescription, validatedURL, isMainFrame) => {
    console.log('Page failed to load:', errorCode, errorDescription);
    // -3 (ERR_ABORTED) means another navigation replaced this one.
    if (!isMainFrame || errorCode === -3 || isQuitting) return;
    if (loadRetries >= MAX_LOAD_RETRIES) {
      console.error('Giving up reloading the page after', MAX_LOAD_RETRIES, 'attempts');
      return;
    }
    loadRetries++;
    setTimeout(() => {
      if (!isQuitting && mainWindow && !mainWindow.isDestroyed()) {
        console.log(`Retrying page load (${loadRetries}/${MAX_LOAD_RETRIES})...`);
        mainWindow.loadURL(appURL());
      }
    }, 1000);
  });

  // Handle render process crashes
  mainWindow.webContents.on('render-process-gone', (event, details) => {
    console.log('Render process gone:', details.reason);
    if (mainWindow && !mainWindow.isDestroyed()) {
      mainWindow.reload();
    }
  });

  // Handle unresponsive page
  mainWindow.webContents.on('unresponsive', () => {
    console.log('Page became unresponsive, reloading...');
    if (mainWindow && !mainWindow.isDestroyed()) {
      mainWindow.reload();
    }
  });
}

// Register an IPC handler that only answers frames served by our backend.
function handleTrusted(channel, handler) {
  ipcMain.handle(channel, (event, ...args) => {
    const frameURL = event.senderFrame ? event.senderFrame.url : '';
    if (!isBackendURL(frameURL)) {
      console.warn(`Rejected IPC "${channel}" from`, frameURL || 'unknown frame');
      throw new Error('Unauthorized IPC sender');
    }
    return handler(event, ...args);
  });
}

// Open a kubeconfig via a native dialog and return its content. The renderer
// never supplies a path, so it cannot read arbitrary files.
handleTrusted('open-kubeconfig-file', async () => {
  const options = {
    title: 'Select Kubeconfig File',
    filters: [
      { name: 'YAML Files', extensions: ['yaml', 'yml'] },
      { name: 'All Files', extensions: ['*'] }
    ],
    properties: ['openFile']
  };
  const result = mainWindow && !mainWindow.isDestroyed()
    ? await dialog.showOpenDialog(mainWindow, options)
    : await dialog.showOpenDialog(options);

  if (result.canceled || result.filePaths.length === 0) {
    return { canceled: true, content: '', error: '' };
  }
  try {
    const content = await fs.promises.readFile(result.filePaths[0], 'utf8');
    return { canceled: false, content, error: '' };
  } catch (error) {
    return { canceled: false, content: '', error: error.message };
  }
});

// Manual update check
handleTrusted('check-for-updates', async () => {
  checkForUpdates(false);
});

// Current version
handleTrusted('get-app-version', () => {
  return app.getVersion();
});

// App lifecycle events
app.whenReady().then(async () => {
  createAppMenu();

  // Set dock icon on macOS (for development)
  if (process.platform === 'darwin' && app.dock) {
    const iconPath = path.join(__dirname, 'build', 'icon.png');
    if (fs.existsSync(iconPath)) {
      const icon = nativeImage.createFromPath(iconPath);
      app.dock.setIcon(icon);
    }
  }

  registerPowerHandlers();

  const backendStarted = await startBackend();

  if (!backendStarted) {
    dialog.showErrorBox(
      'Failed to Start',
      'Could not start the backend server. The application will now close.'
    );
    app.quit();
    return;
  }

  // Start health monitoring
  startHealthCheck();

  createWindow();

  app.on('activate', onActivate);

  // Check for updates silently on startup (after a short delay)
  setTimeout(() => {
    checkForUpdates(true);
  }, 5000);
});

// On macOS the app (and its backend) stays alive without windows;
// before-quit/will-quit stop the backend on a real quit.
app.on('window-all-closed', () => {
  if (process.platform !== 'darwin') {
    app.quit();
  }
});

app.on('before-quit', () => {
  isQuitting = true;
  stopBackend();
});

app.on('will-quit', () => {
  isQuitting = true;
  stopBackend();
});

// Handle system power events (powerMonitor is only usable once the app is ready)
function registerPowerHandlers() {
  powerMonitor.on('resume', async () => {
    console.log('System resumed from sleep');
    // Give the network a moment to reconnect
    await new Promise(resolve => setTimeout(resolve, 1500));
    if (isQuitting) return;

    // Check backend health and restart if needed
    const isHealthy = await checkBackendHealth();
    if (!isHealthy) {
      console.log('Backend not responding after resume, restarting...');
      await restartBackend();
    } else if (mainWindow && !mainWindow.isDestroyed()) {
      console.log('Backend healthy, reloading window after resume...');
      loadRetries = 0;
      mainWindow.loadURL(appURL());
    }
  });

  powerMonitor.on('unlock-screen', () => {
    console.log('Screen unlocked');
    // Check if page needs reload by trying to reload if blank
    if (mainWindow && !mainWindow.isDestroyed()) {
      mainWindow.webContents.executeJavaScript('document.body.innerHTML.length')
        .then(length => {
          if (length === 0) {
            console.log('Page appears blank, reloading...');
            mainWindow.loadURL(appURL());
          }
        })
        .catch(() => {
          console.log('Could not check page content, reloading...');
          if (mainWindow && !mainWindow.isDestroyed()) {
            mainWindow.loadURL(appURL());
          }
        });
    }
  });
}

// ============================================
// Auto-updater configuration and event handlers
// ============================================

// Configure auto-updater
autoUpdater.autoDownload = false; // Don't download automatically, let user decide
autoUpdater.autoInstallOnAppQuit = true;

// electron-updater can only update AppImage installs on Linux; deb/rpm
// installs are updated by the package manager.
function updatesSupported() {
  return process.platform !== 'linux' || Boolean(process.env.APPIMAGE);
}

// Auto-updater event handlers
autoUpdater.on('checking-for-update', () => {
  console.log('Checking for updates...');
});

autoUpdater.on('update-available', (info) => {
  console.log('Update available:', info.version);

  dialog.showMessageBox(mainWindow, {
    type: 'info',
    title: 'Update Available',
    message: `A new version (${info.version}) is available.`,
    detail: 'Would you like to download and install it now?',
    buttons: ['Download', 'Later'],
    defaultId: 0,
    cancelId: 1
  }).then(({ response }) => {
    if (response === 0) {
      // The user asked for the download, so report its failures.
      interactiveUpdateCheck = true;
      autoUpdater.downloadUpdate().catch(() => {
        // Reported through the 'error' event
      });
    } else {
      interactiveUpdateCheck = false;
    }
  });
});

autoUpdater.on('update-not-available', (info) => {
  console.log('No update available, current version:', info.version);
  if (interactiveUpdateCheck) {
    interactiveUpdateCheck = false;
    dialog.showMessageBox(mainWindow, {
      type: 'info',
      title: 'No Updates',
      message: `You are running the latest version (${app.getVersion()}).`
    });
  }
});

autoUpdater.on('error', (err) => {
  console.error('Auto-updater error:', err);
  // Silent startup checks (offline, proxy, rate limit) only log.
  if (interactiveUpdateCheck) {
    interactiveUpdateCheck = false;
    dialog.showErrorBox('Update Error', `Failed to update: ${err.message}`);
  }
});

autoUpdater.on('download-progress', (progressObj) => {
  const percent = Math.round(progressObj.percent);
  console.log(`Download progress: ${percent}%`);

  // Update window title to show progress
  if (mainWindow && !mainWindow.isDestroyed()) {
    mainWindow.setTitle(`Kubeconfig Wrangler - Downloading update ${percent}%`);
  }
});

autoUpdater.on('update-downloaded', (info) => {
  console.log('Update downloaded:', info.version);
  interactiveUpdateCheck = false;

  // Reset window title
  if (mainWindow && !mainWindow.isDestroyed()) {
    mainWindow.setTitle('Kubeconfig Wrangler');
  }

  dialog.showMessageBox(mainWindow, {
    type: 'info',
    title: 'Update Ready',
    message: 'Update downloaded successfully!',
    detail: 'The application will restart to apply the update.',
    buttons: ['Restart Now', 'Later'],
    defaultId: 0,
    cancelId: 1
  }).then(({ response }) => {
    if (response === 0) {
      isQuitting = true;
      stopBackend();
      autoUpdater.quitAndInstall();
    }
  });
});

// Check for updates. silent=true is the automatic startup check, which never
// shows dialogs; silent=false is a user-initiated check.
function checkForUpdates(silent = false) {
  if (!app.isPackaged || !updatesSupported()) {
    const reason = app.isPackaged
      ? 'Automatic updates are only available for the AppImage build. Please update using your package manager.'
      : 'Update checking is disabled in development mode.';
    console.log('Skipping update check:', reason);
    if (!silent) {
      dialog.showMessageBox(mainWindow, {
        type: 'info',
        title: 'Updates Unavailable',
        message: reason
      });
    }
    return;
  }

  interactiveUpdateCheck = !silent;
  autoUpdater.checkForUpdates().catch(err => {
    // Failures are reported through the 'error' event.
    console.error('Failed to check for updates:', err.message);
  });
}

// Create application menu with update option
function createAppMenu() {
  const isMac = process.platform === 'darwin';

  const template = [
    ...(isMac ? [{
      label: app.name,
      submenu: [
        { role: 'about' },
        { type: 'separator' },
        {
          label: 'Check for Updates...',
          click: () => checkForUpdates(false)
        },
        { type: 'separator' },
        { role: 'services' },
        { type: 'separator' },
        { role: 'hide' },
        { role: 'hideOthers' },
        { role: 'unhide' },
        { type: 'separator' },
        { role: 'quit' }
      ]
    }] : []),
    {
      label: 'File',
      submenu: [
        isMac ? { role: 'close' } : { role: 'quit' }
      ]
    },
    {
      label: 'Edit',
      submenu: [
        { role: 'undo' },
        { role: 'redo' },
        { type: 'separator' },
        { role: 'cut' },
        { role: 'copy' },
        { role: 'paste' },
        { role: 'selectAll' }
      ]
    },
    {
      label: 'View',
      submenu: [
        { role: 'reload' },
        { role: 'forceReload' },
        { role: 'toggleDevTools' },
        { type: 'separator' },
        { role: 'resetZoom' },
        { role: 'zoomIn' },
        { role: 'zoomOut' },
        { type: 'separator' },
        { role: 'togglefullscreen' }
      ]
    },
    {
      label: 'Window',
      submenu: [
        { role: 'minimize' },
        { role: 'zoom' },
        ...(isMac ? [
          { type: 'separator' },
          { role: 'front' }
        ] : [
          { role: 'close' }
        ])
      ]
    },
    {
      label: 'Help',
      submenu: [
        ...(!isMac ? [{
          label: 'Check for Updates...',
          click: () => checkForUpdates(false)
        }, { type: 'separator' }] : []),
        {
          label: 'About Kubeconfig Wrangler',
          click: async () => {
            dialog.showMessageBox(mainWindow, {
              type: 'info',
              title: 'About Kubeconfig Wrangler',
              message: 'Kubeconfig Wrangler',
              detail: `Version: ${app.getVersion()}\n\nManage kubeconfigs from multiple sources including Rancher and AWS EKS.`
            });
          }
        }
      ]
    }
  ];

  const menu = Menu.buildFromTemplate(template);
  Menu.setApplicationMenu(menu);
}

// Exposed for unit tests (test/*.test.js); Electron ignores main-module exports.
module.exports = {
  ensureBackend,
  onActivate,
  isHealthCheckRunning: () => healthCheckInterval !== null
};
