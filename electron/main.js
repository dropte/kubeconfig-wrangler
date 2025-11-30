const { app, BrowserWindow, ipcMain, dialog, shell, nativeImage, powerMonitor } = require('electron');
const path = require('path');
const { spawn } = require('child_process');
const fs = require('fs');
const net = require('net');
const crypto = require('crypto');

// Set app name for development
app.setName('Kubeconfig Wrangler');

let mainWindow;
let backendProcess;
let serverPort = 18080;
let serverToken = '';

// Generate a cryptographically secure random token
function generateSecurityToken() {
  return crypto.randomBytes(32).toString('hex');
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

// Start the Go backend server
async function startBackend() {
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

  // Find an available port
  serverPort = await findAvailablePort(18080);

  // Generate a new security token for this session
  serverToken = generateSecurityToken();

  console.log('Starting backend server on port', serverPort);
  console.log('Backend path:', backendPath);
  console.log('Security token enabled');

  return new Promise((resolve) => {
    backendProcess = spawn(backendPath, ['serve', '--port', serverPort.toString(), '--addr', '127.0.0.1', '--token', serverToken], {
      stdio: ['ignore', 'pipe', 'pipe']
    });

    backendProcess.stdout.on('data', (data) => {
      console.log('Backend:', data.toString());
      // Check if server is ready
      if (data.toString().includes('Starting web server')) {
        setTimeout(() => resolve(true), 500); // Give it a moment to fully start
      }
    });

    backendProcess.stderr.on('data', (data) => {
      console.error('Backend error:', data.toString());
    });

    backendProcess.on('error', (error) => {
      console.error('Failed to start backend:', error);
      resolve(false);
    });

    backendProcess.on('close', (code) => {
      console.log('Backend process exited with code', code);
      backendProcess = null;
    });

    // Timeout in case the server doesn't output the expected message
    setTimeout(() => resolve(true), 3000);
  });
}

// Stop the backend server
function stopBackend() {
  if (backendProcess) {
    console.log('Stopping backend server...');
    backendProcess.kill();
    backendProcess = null;
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
      preload: path.join(__dirname, 'preload.js')
    },
    icon: path.join(__dirname, 'build', 'icon.png'),
    title: 'Kubeconfig Wrangler',
    show: false // Don't show until ready
  });

  // Load the web interface from the backend server
  mainWindow.loadURL(`http://127.0.0.1:${serverPort}`);

  // Show window when ready
  mainWindow.once('ready-to-show', () => {
    mainWindow.show();
  });

  // Handle window closed
  mainWindow.on('closed', () => {
    mainWindow = null;
  });

  // Open external links in the default browser
  mainWindow.webContents.setWindowOpenHandler(({ url }) => {
    shell.openExternal(url);
    return { action: 'deny' };
  });

  // Handle page load failures (e.g., after sleep/resume)
  mainWindow.webContents.on('did-fail-load', (event, errorCode, errorDescription) => {
    console.log('Page failed to load:', errorCode, errorDescription);
    // Retry loading after a short delay
    setTimeout(() => {
      if (mainWindow && !mainWindow.isDestroyed()) {
        console.log('Retrying page load...');
        mainWindow.loadURL(`http://127.0.0.1:${serverPort}`);
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

// IPC handlers for save dialog
ipcMain.handle('show-save-dialog', async (event, options) => {
  const result = await dialog.showSaveDialog(mainWindow, {
    title: 'Save Kubeconfig',
    defaultPath: options.defaultPath || 'kubeconfig.yaml',
    filters: [
      { name: 'YAML Files', extensions: ['yaml', 'yml'] },
      { name: 'All Files', extensions: ['*'] }
    ]
  });
  return result;
});

ipcMain.handle('save-file', async (event, { filePath, content }) => {
  try {
    fs.writeFileSync(filePath, content);
    return { success: true };
  } catch (error) {
    return { success: false, error: error.message };
  }
});

// IPC handlers for open dialog (file loading)
ipcMain.handle('show-open-dialog', async (event, options) => {
  const result = await dialog.showOpenDialog(mainWindow, {
    title: options.title || 'Open File',
    defaultPath: options.defaultPath,
    filters: options.filters || [
      { name: 'YAML Files', extensions: ['yaml', 'yml'] },
      { name: 'All Files', extensions: ['*'] }
    ],
    properties: ['openFile']
  });
  return result;
});

ipcMain.handle('read-file', async (event, { filePath }) => {
  try {
    const content = fs.readFileSync(filePath, 'utf8');
    return { success: true, content };
  } catch (error) {
    return { success: false, error: error.message };
  }
});

// App lifecycle events
app.whenReady().then(async () => {
  // Set dock icon on macOS (for development)
  if (process.platform === 'darwin' && app.dock) {
    const iconPath = path.join(__dirname, 'build', 'icon.png');
    if (fs.existsSync(iconPath)) {
      const icon = nativeImage.createFromPath(iconPath);
      app.dock.setIcon(icon);
    }
  }

  const backendStarted = await startBackend();

  if (!backendStarted) {
    dialog.showErrorBox(
      'Failed to Start',
      'Could not start the backend server. The application will now close.'
    );
    app.quit();
    return;
  }

  createWindow();

  app.on('activate', () => {
    if (BrowserWindow.getAllWindows().length === 0) {
      createWindow();
    }
  });
});

app.on('window-all-closed', () => {
  stopBackend();
  if (process.platform !== 'darwin') {
    app.quit();
  }
});

app.on('before-quit', () => {
  stopBackend();
});

app.on('will-quit', () => {
  stopBackend();
});

// Handle system power events
powerMonitor.on('resume', () => {
  console.log('System resumed from sleep');
  // Give the network a moment to reconnect, then reload
  setTimeout(() => {
    if (mainWindow && !mainWindow.isDestroyed()) {
      console.log('Reloading window after resume...');
      mainWindow.reload();
    }
  }, 1500);
});

powerMonitor.on('unlock-screen', () => {
  console.log('Screen unlocked');
  // Check if page needs reload by trying to reload if blank
  if (mainWindow && !mainWindow.isDestroyed()) {
    mainWindow.webContents.executeJavaScript('document.body.innerHTML.length')
      .then(length => {
        if (length === 0) {
          console.log('Page appears blank, reloading...');
          mainWindow.reload();
        }
      })
      .catch(() => {
        console.log('Could not check page content, reloading...');
        mainWindow.reload();
      });
  }
});
