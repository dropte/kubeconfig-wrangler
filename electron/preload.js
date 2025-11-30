const { contextBridge, ipcRenderer } = require('electron');

// Expose protected methods to the renderer process
contextBridge.exposeInMainWorld('electronAPI', {
  showSaveDialog: (options) => ipcRenderer.invoke('show-save-dialog', options),
  saveFile: (filePath, content) => ipcRenderer.invoke('save-file', { filePath, content }),
  showOpenDialog: (options) => ipcRenderer.invoke('show-open-dialog', options),
  readFile: (filePath) => ipcRenderer.invoke('read-file', { filePath }),
  platform: process.platform
});
