const express = require('express');
const bodyParser = require('body-parser');
const fs = require('fs').promises;
const fsSync = require('fs');
const path = require('path');
const { spawn } = require('child_process');
const net = require('net');

const app = express();
const port = process.env.PORT || 8000;

app.use(bodyParser.json({ limit: '50mb' }));

app.post('/sync', async (req, res) => {
    const files = req.body;
    const promises = [];

    for (const filePath in files) {
        if (Object.hasOwnProperty.call(files, filePath)) {
            const promise = (async () => {
                const encodedContent = files[filePath];
                const decodedContent = Buffer.from(encodedContent, 'base64');

                const dirname = path.dirname(filePath);
                await fs.mkdir(dirname, { recursive: true });
                await fs.writeFile(filePath, decodedContent);
            })();
            promises.push(promise);
        }
    }

    try {
        await Promise.all(promises);
        res.status(200).send('Files synced successfully.');
    } catch (error) {
        console.error('Error syncing files:', error);
        res.status(500).send('Error syncing files.');
    }
});

// Management: npm install and dev server control (framework-agnostic)
const APP_DIR = process.env.APP_DIR || '/app/applet';
const PID_FILE = path.join(APP_DIR, '.dev.pid');
const APP_PORT_DEFAULT = Number(process.env.APP_PORT || '3000');
const PREWARM_DEFAULT_PATHS = ['/', '/api/hello'];
let devOpInProgress = false;

function readPidFile() {
  try {
    if (!fsSync.existsSync(PID_FILE)) return null;
    const content = fsSync.readFileSync(PID_FILE, 'utf8').trim();
    const pid = Number(content);
    return Number.isFinite(pid) ? pid : null;
  } catch {
    return null;
  }
}

function isProcessAlive(pid) {
  if (!pid) return false;
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

async function waitForExit(pid, timeoutMs = 10000) {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    if (!isProcessAlive(pid)) return true;
    await new Promise((r) => setTimeout(r, 150));
  }
  return !isProcessAlive(pid);
}

function runCommand(command, args, options = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {
      stdio: ['ignore', 'pipe', 'pipe'],
      ...options,
    });
    let stdout = '';
    let stderr = '';
    child.stdout.on('data', (d) => {
      const s = d.toString();
      stdout += s;
      process.stdout.write(s);
    });
    child.stderr.on('data', (d) => {
      const s = d.toString();
      stderr += s;
      process.stderr.write(s);
    });
    child.on('error', (err) => reject(err));
    child.on('close', (code) => resolve({ code, stdout, stderr }));
  });
}

// Prewarm helpers
async function fetchWithTimeout(url, timeoutMs = 5000) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), timeoutMs);
  try {
    const res = await fetch(url, { signal: controller.signal });
    return res;
  } finally {
    clearTimeout(timeout);
  }
}

// Port helpers
function isPortInUse(port) {
  // Check on IPv4 host to detect typical dev servers bound to 0.0.0.0
  return new Promise((resolve) => {
    const tester = net.createServer()
      .once('error', () => resolve(true))
      .once('listening', () => {
        tester.once('close', () => resolve(false)).close();
      })
      .listen(port, '0.0.0.0');
  });
}

async function waitForPortToBeFree(port, timeoutMs = 10000) {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    const used = await isPortInUse(port);
    if (!used) return true;
    await new Promise((r) => setTimeout(r, 100));
  }
  return !(await isPortInUse(port));
}

function readPackageJson(cwd) {
  try { return JSON.parse(fsSync.readFileSync(path.join(cwd, 'package.json'), 'utf8')); } catch { return null; }
}

function resolveDevCommand(cwd, port) {
  const pkg = readPackageJson(cwd) || {};
  const deps = { ...(pkg.dependencies || {}), ...(pkg.devDependencies || {}) };
  if (deps.next) {
    return ['node', ['node_modules/next/dist/bin/next', 'dev', '-H', '0.0.0.0', '-p', String(port)]];
  }
  const scripts = pkg.scripts || {};
  const npmArgs = scripts.dev ? ['run', 'dev'] : ['start'];
  return ['npm', npmArgs];
}

async function waitForServerReady(baseUrl, timeoutMs = 20000) {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    try {
      const res = await fetchWithTimeout(baseUrl, 2000);
      if (res.ok || res.status === 404) return true;
    } catch {}
    await new Promise((r) => setTimeout(r, 250));
  }
  return false;
}

async function prewarmPaths(paths, port = APP_PORT_DEFAULT, waitUntilReady = true) {
  const baseUrl = `http://localhost:${port}`;
  if (waitUntilReady) {
    await waitForServerReady(baseUrl);
  }
  const unique = Array.from(new Set(paths));
  await Promise.all(unique.map(async (p) => {
    try { await fetchWithTimeout(`${baseUrl}${p}`, 5000); } catch {}
  }));
}

app.post('/npm/install', async (req, res) => {
  const { cwd = APP_DIR, ci = false, extraArgs = [], prewarm = false, prewarmPaths: paths = PREWARM_DEFAULT_PATHS, port = APP_PORT_DEFAULT } = req.body || {};
  try {
    const args = ci ? ['ci'] : ['install', '--no-fund', '--no-audit'];
    const result = await runCommand('npm', [...args, ...extraArgs], { cwd });
    if (result.code === 0) {
      if (prewarm) {
        prewarmPaths(Array.isArray(paths) ? paths : PREWARM_DEFAULT_PATHS, port).catch(() => {});
      }
      res.status(200).json({ ok: true, code: result.code, prewarming: Boolean(prewarm) });
    } else {
      res.status(500).json({ ok: false, code: result.code });
    }
  } catch (error) {
    res.status(500).json({ ok: false, error: String(error) });
  }
});

app.get('/dev/status', (_req, res) => {
  const pid = readPidFile();
  const running = isProcessAlive(pid);
  res.status(200).json({ running, pid: running ? pid : null });
});

app.post('/dev/stop', async (_req, res) => {
  const pid = readPidFile();
  if (!pid || !isProcessAlive(pid)) {
    return res.status(200).json({ stopped: true, message: 'Dev server not running' });
  }
  // Kill the whole process group to avoid orphan child processes holding the port
  try { process.kill(-pid, 'SIGTERM'); } catch { try { process.kill(pid, 'SIGTERM'); } catch {} }
  const exited = await waitForExit(pid, 10000);
  if (!exited) {
    try { process.kill(-pid, 'SIGKILL'); } catch { try { process.kill(pid, 'SIGKILL'); } catch {} }
  }
  try { fsSync.unlinkSync(PID_FILE); } catch {}
  return res.status(200).json({ stopped: true });
});


app.post('/dev/start', async (req, res) => {
  const { port = APP_PORT_DEFAULT, prewarm = false, prewarmPaths: paths = PREWARM_DEFAULT_PATHS } = req.body || {};
  const existingPid = readPidFile();
  if (existingPid && isProcessAlive(existingPid)) {
    return res.status(409).json({ started: false, message: 'Already running', pid: existingPid });
  }
  if (devOpInProgress) {
    return res.status(409).json({ started: false, message: 'Another dev operation is in progress' });
  }
  if (await isPortInUse(port)) {
    return res.status(409).json({ started: false, message: `Port ${port} is already in use` });
  }
  try {
    devOpInProgress = true;
    const [cmd, args] = resolveDevCommand(APP_DIR, port);
    const child = spawn(cmd, args, {
      cwd: APP_DIR,
      env: { ...process.env, HOST: '0.0.0.0', PORT: String(port) },
      detached: true,
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    // Put child in its own process group to allow group signals
    try { process.kill(-child.pid, 0); } catch {}
    try { fsSync.writeFileSync(PID_FILE, String(child.pid)); } catch {}
    child.stdout.on('data', (d) => process.stdout.write(d));
    child.stderr.on('data', (d) => process.stderr.write(d));
    child.on('exit', () => { try { fsSync.unlinkSync(PID_FILE); } catch {} });
    if (prewarm) {
      prewarmPaths(Array.isArray(paths) ? paths : PREWARM_DEFAULT_PATHS, port).catch(() => {});
    }
    return res.status(202).json({ started: true, pid: child.pid, prewarming: Boolean(prewarm) });
  } catch (error) {
    return res.status(500).json({ started: false, error: String(error) });
  } finally {
    devOpInProgress = false;
  }
});

app.post('/dev/restart', async (req, res) => {
  const { port = APP_PORT_DEFAULT, prewarm = false, prewarmPaths: paths = PREWARM_DEFAULT_PATHS } = req.body || {};
  if (devOpInProgress) {
    return res.status(409).json({ restarted: false, message: 'Another dev operation is in progress' });
  }
  try {
    devOpInProgress = true;
    const pid = readPidFile();
    if (pid && isProcessAlive(pid)) {
      try { process.kill(-pid, 'SIGTERM'); } catch { try { process.kill(pid, 'SIGTERM'); } catch {} }
      await waitForExit(pid, 10000);
      if (isProcessAlive(pid)) {
        try { process.kill(-pid, 'SIGKILL'); } catch { try { process.kill(pid, 'SIGKILL'); } catch {} }
      }
      try { fsSync.unlinkSync(PID_FILE); } catch {}
      await waitForPortToBeFree(port, 5000);
    }
    const [cmd, args] = resolveDevCommand(APP_DIR, port);
    const child = spawn(cmd, args, {
      cwd: APP_DIR,
      env: { ...process.env, HOST: '0.0.0.0', PORT: String(port) },
      detached: true,
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    try { process.kill(-child.pid, 0); } catch {}
    try { fsSync.writeFileSync(PID_FILE, String(child.pid)); } catch {}
    child.stdout.on('data', (d) => process.stdout.write(d));
    child.stderr.on('data', (d) => process.stderr.write(d));
    child.on('exit', () => { try { fsSync.unlinkSync(PID_FILE); } catch {} });
    if (prewarm) {
      prewarmPaths(Array.isArray(paths) ? paths : PREWARM_DEFAULT_PATHS, port).catch(() => {});
    }
    return res.status(202).json({ restarted: true, pid: child.pid, prewarming: Boolean(prewarm) });
  } catch (error) {
    return res.status(500).json({ restarted: false, error: String(error) });
  } finally {
    devOpInProgress = false;
  }
});

// Standalone prewarm endpoint
app.post('/prewarm', async (req, res) => {
  try {
    const { paths = PREWARM_DEFAULT_PATHS, port = APP_PORT_DEFAULT, wait = true } = req.body || {};
    await prewarmPaths(Array.isArray(paths) ? paths : PREWARM_DEFAULT_PATHS, port, Boolean(wait));
    res.status(200).json({ ok: true, warmed: Array.isArray(paths) ? paths : PREWARM_DEFAULT_PATHS });
  } catch (error) {
    res.status(500).json({ ok: false, error: String(error) });
  }
});

// Lightweight health endpoint used by the startup script
app.get('/healthz', (_req, res) => {
    res.status(200).send('ok');
});

app.listen(port, () => {
    console.log(`Server listening at http://localhost:${port}`);
});
