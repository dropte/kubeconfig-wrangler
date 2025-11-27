const { app, BrowserWindow, ipcMain, dialog, shell } = require('electron');
const path = require('path');
const { spawn } = require('child_process');
const fs = require('fs');
const net = require('net');

let mainWindow;
let backendProcess;
let serverPort = 18080;

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

// Get the path to the backend binary
function getBackendPath() {
  const isDev = !app.isPackaged;

  if (isDev) {
    // In development, look for the binary in the parent bin directory
    const platform = process.platform;
    let binaryName = 'rancher-kubeconfig-proxy';
    if (platform === 'win32') {
      binaryName += '.exe';
    }
    return path.join(__dirname, '..', 'bin', platform, binaryName);
  } else {
    // In production, look in the resources directory
    const platform = process.platform;
    let binaryName = 'rancher-kubeconfig-proxy';
    if (platform === 'win32') {
      binaryName += '.exe';
    }
    return path.join(process.resourcesPath, 'bin', binaryName);
  }
}

// Start the Go backend server
async function startBackend() {
  const backendPath = getBackendPath();

  // Check if the backend binary exists
  if (!fs.existsSync(backendPath)) {
    console.error('Backend binary not found at:', backendPath);
    dialog.showErrorBox(
      'Backend Not Found',
      `The backend server binary was not found at:\n${backendPath}\n\nPlease build the Go backend first using:\ngo build -o bin/${process.platform}/rancher-kubeconfig-proxy`
    );
    return false;
  }

  // Find an available port
  serverPort = await findAvailablePort(18080);

  console.log('Starting backend server on port', serverPort);
  console.log('Backend path:', backendPath);

  return new Promise((resolve) => {
    backendProcess = spawn(backendPath, ['serve', '--port', serverPort.toString(), '--addr', '127.0.0.1'], {
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
    title: 'Rancher Kubeconfig Proxy',
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

// App lifecycle events
app.whenReady().then(async () => {
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
