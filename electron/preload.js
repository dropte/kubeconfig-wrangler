const { contextBridge, ipcRenderer } = require('electron');

// Expose a minimal API to the renderer. File access goes through a native
// dialog in the main process; the page never passes file paths.
contextBridge.exposeInMainWorld('electronAPI', {
  openKubeconfigFile: () => ipcRenderer.invoke('open-kubeconfig-file'),
  checkForUpdates: () => ipcRenderer.invoke('check-for-updates'),
  getVersion: () => ipcRenderer.invoke('get-app-version'),
  platform: process.platform
});
