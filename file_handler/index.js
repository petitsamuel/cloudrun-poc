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

// Basic CORS support (configurable via CORS_ORIGINS env: comma-separated list or '*')
const CORS_ORIGINS = '*';
function isOriginAllowed(origin) {
  if (!origin) return true; // allow non-CORS requests
  if (CORS_ORIGINS === '*') return true;
  const allowed = CORS_ORIGINS.split(',').map((s) => s.trim()).filter(Boolean);
  return allowed.includes(origin);
}
app.use((req, res, next) => {
  const origin = req.headers.origin;
  if (isOriginAllowed(origin)) {
    res.header('Access-Control-Allow-Origin', CORS_ORIGINS === '*' ? '*' : origin || '*');
    res.header('Vary', 'Origin');
  }
  res.header('Access-Control-Allow-Methods', 'GET,POST,OPTIONS');
  res.header('Access-Control-Allow-Headers', 'Content-Type, Authorization, X-Requested-With');
  // We do not use cookies; disallow credentials with wildcard origin
  res.header('Access-Control-Allow-Credentials', 'false');
  if (req.method === 'OPTIONS') {
    return res.sendStatus(204);
  }
  next();
});

app.post('/sync', async (req, res) => {
  try {
    // Preferred service shape: { files: { path: base64 }, deleted_file_paths: [...], prewarm_config: {...} }
    // Back-compat: body is a map of path -> base64 content
    const body = req.body || {};
    const hasServiceShape = body && (body.files || body.deleted_file_paths || body.prewarm_config);
    const filesMap = hasServiceShape ? (body.files || {}) : (body || {});
    const deleted = hasServiceShape ? (Array.isArray(body.deleted_file_paths) ? body.deleted_file_paths : []) : [];
    const prewarmConfig = hasServiceShape ? (body.prewarm_config || null) : null;

    const writePromises = Object.entries(filesMap).map(([p, b64]) => writeFileBase64(p, b64));
    const deletePromises = deleted.map((p) => deletePath(p));
    await Promise.all([...writePromises, ...deletePromises]);

    // Fallback to legacy querystring-based prewarm if no prewarm_config provided
    const qp = req.query || {};
    const str = (v) => (v === undefined || v === null ? '' : String(v));
    const toBool = (v, defaultVal = false) => {
      const s = str(v).toLowerCase();
      if (s === '') return defaultVal;
      return s === '1' || s === 'true' || s === 'yes' || s === 'on';
    };
    let prewarmRequested = Boolean(prewarmConfig);
    let prewarmWait = false;
    let prewarmPort = APP_PORT_DEFAULT;
    let prewarmPathsInput = PREWARM_DEFAULT_PATHS;
    if (prewarmConfig) {
      prewarmWait = Boolean(prewarmConfig.wait_for_completion);
      prewarmPort = Number(prewarmConfig.port || APP_PORT_DEFAULT) || APP_PORT_DEFAULT;
      prewarmPathsInput = Array.isArray(prewarmConfig.paths) && prewarmConfig.paths.length > 0 ? prewarmConfig.paths : PREWARM_DEFAULT_PATHS;
    } else if (toBool(qp.prewarm, false)) {
      prewarmRequested = true;
      prewarmWait = toBool(qp.wait, false);
      prewarmPort = Number(qp.port) || APP_PORT_DEFAULT;
      const parsePaths = (input) => {
        if (Array.isArray(input)) return input;
        const s = str(input).trim();
        if (!s) return PREWARM_DEFAULT_PATHS;
        try { const parsed = JSON.parse(s); if (Array.isArray(parsed)) return parsed; } catch { }
        if (s.includes(',')) return s.split(',').map((p) => p.trim()).filter(Boolean);
        return [s];
      };
      prewarmPathsInput = parsePaths(qp.prewarmPaths ?? qp.paths);
    }

    if (prewarmRequested) {
      const job = prewarmPaths(prewarmPathsInput, prewarmPort, true).catch(() => { });
      if (prewarmWait) {
        await job;
      }
    }

    return res.status(200).json({ success: true, message: 'Files synced successfully' });
  } catch (error) {
    console.error('Error syncing files:', error);
    return res.status(500).json({ success: false, message: 'Error syncing files', error: String(error) });
  }
});

// Management: npm install and dev server control (framework-agnostic)
const APP_DIR = process.env.APP_DIR || '/app/applet';
const PID_FILE = path.join(APP_DIR, '.dev.pid');
const DEV_CONTEXT_FILE = path.join(APP_DIR, '.dev.context.json');
const APP_PORT_DEFAULT = Number(process.env.APP_PORT || '3000');
const PREWARM_DEFAULT_PATHS = ['/', '/api/hello'];
let devOpInProgress = false;

// Context and file helpers
const Framework = {
  FRAMEWORK_UNSPECIFIED: 0,
  FRAMEWORK_NEXTJS: 1,
  FRAMEWORK_VITE: 2,
};

function frameworkFromInput(input) {
  if (typeof input === 'number') {
    const asNumber = input | 0;
    for (const [k, v] of Object.entries(Framework)) {
      if (v === asNumber) return k;
    }
    return 'FRAMEWORK_UNSPECIFIED';
  }
  const s = String(input || '').trim().toUpperCase();
  if (s in Framework) return s;
  return 'FRAMEWORK_UNSPECIFIED';
}

function getFrameworkSourceDir(framework) {
  const f = frameworkFromInput(framework);
  // Map proto frameworks to repo sample directories
  if (f === 'FRAMEWORK_NEXTJS') return 'next';
  if (f === 'FRAMEWORK_VITE') return 'react';
  // Auto-detect in order of preference
  const candidates = ['next', 'react', 'angular'];
  for (const c of candidates) {
    const p = path.resolve('/app', c);
    try { if (fsSync.existsSync(p) && fsSync.statSync(p).isDirectory()) return c; } catch { }
  }
  return null;
}

async function syncFrameworkFiles(framework, cwd = APP_DIR) {
  // For now, always generate a basic app for the selected framework
  console.log(`Generating basic app for ${frameworkFromInput(framework)} in ${cwd}`);
  await resetDirectoryContents(cwd);
  await createBasicFrameworkApp(frameworkFromInput(framework), cwd);
  console.log('Framework files prepared');
}

function loadContext() {
  try {
    const raw = fsSync.readFileSync(DEV_CONTEXT_FILE, 'utf8');
    const parsed = JSON.parse(raw);
    const framework = frameworkFromInput(parsed.framework);
    const environment_variables = parsed.environment_variables && typeof parsed.environment_variables === 'object' ? parsed.environment_variables : {};
    return { framework, environment_variables };
  } catch {
    return { framework: 'FRAMEWORK_UNSPECIFIED', environment_variables: {} };
  }
}

function saveContext(ctx) {
  try {
    fsSync.mkdirSync(path.dirname(DEV_CONTEXT_FILE), { recursive: true });
    fsSync.writeFileSync(DEV_CONTEXT_FILE, JSON.stringify(ctx, null, 2));
  } catch { }
}

let currentContext = loadContext();

function resolveWithinAppDir(requestPath) {
  const input = String(requestPath || '');
  const resolved = path.isAbsolute(input) ? input : path.resolve(APP_DIR, input);
  const normalizedAppDir = path.resolve(APP_DIR) + path.sep;
  const normalized = path.resolve(resolved);
  if (!normalized.startsWith(normalizedAppDir)) {
    throw new Error(`Path outside of APP_DIR is not permitted: ${input}`);
  }
  return normalized;
}

async function writeFileBase64(targetPath, base64Content) {
  const filePath = resolveWithinAppDir(targetPath);
  const dirname = path.dirname(filePath);
  await fs.mkdir(dirname, { recursive: true });
  const decoded = Buffer.from(base64Content || '', 'base64');
  await fs.writeFile(filePath, decoded);
}

async function deletePath(targetPath) {
  const filePath = resolveWithinAppDir(targetPath);
  try {
    await fs.rm(filePath, { recursive: true, force: true });
  } catch { }
}

function resolveWithinNodeModules(requestPath) {
  const nodeModulesRoot = path.resolve(APP_DIR, 'node_modules') + path.sep;
  const target = path.resolve(path.join(APP_DIR, 'node_modules', String(requestPath || '')));
  if (!target.startsWith(nodeModulesRoot)) {
    throw new Error('Access outside node_modules is not permitted');
  }
  return target;
}

function guessMime(filePath) {
  const ext = path.extname(filePath).toLowerCase();
  switch (ext) {
    case '.js':
    case '.mjs':
    case '.cjs':
    case '.ts':
      return 'text/javascript; charset=utf-8';
    case '.json':
    case '.map':
      return 'application/json; charset=utf-8';
    case '.css':
      return 'text/css; charset=utf-8';
    case '.html':
    case '.htm':
      return 'text/html; charset=utf-8';
    case '.wasm':
      return 'application/wasm';
    case '.svg':
      return 'image/svg+xml';
    case '.png':
      return 'image/png';
    case '.jpg':
    case '.jpeg':
      return 'image/jpeg';
    case '.gif':
      return 'image/gif';
    default:
      return 'application/octet-stream';
  }
}

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

function resolveDevCommand(cwd, port, framework) {
  const pkg = readPackageJson(cwd) || {};
  const deps = { ...(pkg.dependencies || {}), ...(pkg.devDependencies || {}) };
  // Dedicated handling for popular dev servers so we can force host/port
  // Next.js
  const desired = frameworkFromInput(framework);
  if (desired === 'FRAMEWORK_NEXTJS' || deps.next) {
    return ['node', ['node_modules/next/dist/bin/next', 'dev', '-H', '0.0.0.0', '-p', String(port)]];
  }
  const scripts = pkg.scripts || {};
  const devScript = String(scripts.dev || '');
  const hasAngularWorkspace = fsSync.existsSync(path.join(cwd, 'angular.json'));
  const hasViteConfig = [
    'vite.config.ts',
    'vite.config.js',
    'vite.config.mjs',
    'vite.config.cjs',
  ].some((f) => fsSync.existsSync(path.join(cwd, f)));
  const usesVite = hasViteConfig || Boolean(deps.vite) || /(^|\s)vite(\s|$)/.test(devScript);
  const usesAngularCli = hasAngularWorkspace && (Boolean(deps['@angular/cli'] || deps['@angular/build']) || /ng\s+serve/.test(devScript));

  // Angular CLI
  if (usesAngularCli) {
    return ['node', ['node_modules/@angular/cli/bin/ng.js', 'serve', '--host', '0.0.0.0', '--port', String(port)]];
  }

  // Vite
  if (desired === 'FRAMEWORK_VITE' || usesVite) {
    return ['node', ['node_modules/vite/bin/vite.js', '--host', '0.0.0.0', '--port', String(port)]];
  }

  // Fallback: rely on package scripts; PORT/HOST are still exported via env
  const npmArgs = scripts.dev ? ['run', 'dev'] : ['start'];
  return ['npm', npmArgs];
}

async function waitForServerReady(baseUrl, timeoutMs = 20000) {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    try {
      const res = await fetchWithTimeout(baseUrl, 2000);
      if (res.ok || res.status === 404) return true;
    } catch { }
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
    try { await fetchWithTimeout(`${baseUrl}${p}`, 5000); } catch { }
  }));
}

app.post('/npm/install', async (req, res) => {
  // Backward-compatible shim to the new /dependencies/install
  const { cwd, extraArgs, prewarm_config, port, prewarm, prewarmPaths } = req.body || {};
  const body = {
    cwd: cwd || APP_DIR,
    extra_args: Array.isArray(extraArgs) ? extraArgs : [],
    prewarm_config: prewarm_config || (prewarm ? { paths: Array.isArray(prewarmPaths) ? prewarmPaths : PREWARM_DEFAULT_PATHS, port: port || APP_PORT_DEFAULT, wait_for_completion: false } : undefined),
  };
  req.body = body;
  return dependenciesInstallHandler(req, res);
});

app.post('/dependencies/install', dependenciesInstallHandler);

async function dependenciesInstallHandler(req, res) {
  const { cwd = APP_DIR, extra_args = [], prewarm_config = null } = req.body || {};
  try {
    const args = ['install', '--no-fund', '--no-audit'];
    const result = await runCommand('npm', [...args, ...extra_args], { cwd });
    if (result.code === 0) {
      if (prewarm_config) {
        const paths = Array.isArray(prewarm_config.paths) ? prewarm_config.paths : PREWARM_DEFAULT_PATHS;
        const port = Number(prewarm_config.port || APP_PORT_DEFAULT) || APP_PORT_DEFAULT;
        const wait = Boolean(prewarm_config.wait_for_completion);
        const job = prewarmPaths(paths, port, true).catch(() => { });
        if (wait) await job;
      }
      return res.status(200).json({ success: true, exit_code: result.code });
    } else {
      return res.status(500).json({ success: false, exit_code: result.code, error_message: result.stderr || 'npm install failed' });
    }
  } catch (error) {
    return res.status(500).json({ success: false, exit_code: -1, error_message: String(error) });
  }
}

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
  try { process.kill(-pid, 'SIGTERM'); } catch { try { process.kill(pid, 'SIGTERM'); } catch { } }
  const exited = await waitForExit(pid, 10000);
  if (!exited) {
    try { process.kill(-pid, 'SIGKILL'); } catch { try { process.kill(pid, 'SIGKILL'); } catch { } }
  }
  try { fsSync.unlinkSync(PID_FILE); } catch { }
  return res.status(200).json({ stopped: true, message: 'Dev server stopped successfully' });
});


app.post('/dev/start', async (req, res) => {
  const { port = APP_PORT_DEFAULT, prewarm_config = null } = req.body || {};
  const existingPid = readPidFile();
  if (existingPid && isProcessAlive(existingPid)) {
    return res.status(409).json({ operation_initiated: false, message: 'Already running', pid: existingPid });
  }
  if (devOpInProgress) {
    return res.status(409).json({ operation_initiated: false, message: 'Another dev operation is in progress' });
  }
  if (await isPortInUse(port)) {
    return res.status(409).json({ operation_initiated: false, message: `Port ${port} is already in use` });
  }
  try {
    devOpInProgress = true;
    const [cmd, args] = resolveDevCommand(APP_DIR, port, currentContext.framework);
    const child = spawn(cmd, args, {
      cwd: APP_DIR,
      env: { ...process.env, ...currentContext.environment_variables, HOST: '0.0.0.0', PORT: String(port) },
      detached: true,
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    // Put child in its own process group to allow group signals
    try { process.kill(-child.pid, 0); } catch { }
    try { fsSync.writeFileSync(PID_FILE, String(child.pid)); } catch { }
    child.stdout.on('data', (d) => process.stdout.write(d));
    child.stderr.on('data', (d) => process.stderr.write(d));
    child.on('exit', () => { try { fsSync.unlinkSync(PID_FILE); } catch { } });
    if (prewarm_config) {
      const paths = Array.isArray(prewarm_config.paths) ? prewarm_config.paths : PREWARM_DEFAULT_PATHS;
      const wait = Boolean(prewarm_config.wait_for_completion);
      const job = prewarmPaths(paths, Number(prewarm_config.port || port) || port, true).catch(() => { });
      if (wait) await job;
    }
    return res.status(202).json({ operation_initiated: true, pid: child.pid });
  } catch (error) {
    return res.status(500).json({ operation_initiated: false, message: String(error) });
  } finally {
    devOpInProgress = false;
  }
});

app.post('/dev/restart', async (req, res) => {
  const { port = APP_PORT_DEFAULT, prewarm_config = null } = req.body || {};
  if (devOpInProgress) {
    return res.status(409).json({ operation_initiated: false, message: 'Another dev operation is in progress' });
  }
  try {
    devOpInProgress = true;
    const pid = readPidFile();
    if (pid && isProcessAlive(pid)) {
      try { process.kill(-pid, 'SIGTERM'); } catch { try { process.kill(pid, 'SIGTERM'); } catch { } }
      await waitForExit(pid, 10000);
      if (isProcessAlive(pid)) {
        try { process.kill(-pid, 'SIGKILL'); } catch { try { process.kill(pid, 'SIGKILL'); } catch { } }
      }
      try { fsSync.unlinkSync(PID_FILE); } catch { }
      await waitForPortToBeFree(port, 5000);
    }
    const [cmd, args] = resolveDevCommand(APP_DIR, port, currentContext.framework);
    const child = spawn(cmd, args, {
      cwd: APP_DIR,
      env: { ...process.env, ...currentContext.environment_variables, HOST: '0.0.0.0', PORT: String(port) },
      detached: true,
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    try { process.kill(-child.pid, 0); } catch { }
    try { fsSync.writeFileSync(PID_FILE, String(child.pid)); } catch { }
    child.stdout.on('data', (d) => process.stdout.write(d));
    child.stderr.on('data', (d) => process.stderr.write(d));
    child.on('exit', () => { try { fsSync.unlinkSync(PID_FILE); } catch { } });
    if (prewarm_config) {
      const paths = Array.isArray(prewarm_config.paths) ? prewarm_config.paths : PREWARM_DEFAULT_PATHS;
      const wait = Boolean(prewarm_config.wait_for_completion);
      const job = prewarmPaths(paths, Number(prewarm_config.port || port) || port, true).catch(() => { });
      if (wait) await job;
    }
    return res.status(202).json({ operation_initiated: true, pid: child.pid });
  } catch (error) {
    return res.status(500).json({ operation_initiated: false, message: String(error) });
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

// Read-only access to files within node_modules
// - List directory: GET /modules?path=react
// - Read file:      GET /modules/file?path=react/package.json
app.get('/modules', async (req, res) => {
  try {
    const inputPath = String((req.query && req.query.path) || '').trim();
    const target = resolveWithinNodeModules(inputPath);
    const stat = await fs.stat(target).catch(() => null);
    if (!stat) return res.status(404).json({ ok: false, error: 'Not found' });
    if (!stat.isDirectory()) return res.status(400).json({ ok: false, error: 'Not a directory' });
    const entries = await fs.readdir(target, { withFileTypes: true });
    const list = entries.map((e) => ({ name: e.name, type: e.isDirectory() ? 'dir' : 'file' }));
    return res.status(200).json({ ok: true, path: inputPath || '', entries: list });
  } catch (error) {
    return res.status(500).json({ ok: false, error: String(error) });
  }
});

app.get('/modules/file', async (req, res) => {
  try {
    const inputPath = String((req.query && req.query.path) || '').trim();
    const target = resolveWithinNodeModules(inputPath);
    const stat = await fs.stat(target).catch(() => null);
    if (!stat) return res.status(404).json({ ok: false, error: 'Not found' });
    if (stat.isDirectory()) return res.status(400).json({ ok: false, error: 'Path is a directory' });
    const data = await fs.readFile(target);
    res.setHeader('Content-Type', guessMime(target));
    return res.status(200).send(data);
  } catch (error) {
    return res.status(500).json({ ok: false, error: String(error) });
  }
});

// Context reset endpoint (change context)
async function cleanWorkspace(framework, cwd = APP_DIR) {
  const desired = frameworkFromInput(framework);
  const targets = new Set();
  if (desired === 'FRAMEWORK_NEXTJS' || desired === 'FRAMEWORK_UNSPECIFIED') {
    targets.add('.next');
    targets.add('out');
  }
  if (desired === 'FRAMEWORK_VITE' || desired === 'FRAMEWORK_UNSPECIFIED') {
    targets.add('dist');
    targets.add(path.join('node_modules', '.vite'));
  }
  // Common caches
  targets.add('build');

  await Promise.all(Array.from(targets).map(async (p) => {
    const target = path.resolve(cwd, p);
    try { await fs.rm(target, { recursive: true, force: true }); } catch { }
  }));
}

async function resetDirectoryContents(dirPath) {
  try {
    await fs.rm(dirPath, { recursive: true, force: true });
  } catch { }
  await fs.mkdir(dirPath, { recursive: true });
}

async function createBasicFrameworkApp(framework, cwd = APP_DIR) {
  try {
    console.log(`Creating basic ${framework} app in ${cwd}`);

    if (framework === 'FRAMEWORK_NEXTJS') {
      // Create basic Next.js app
      const packageJson = {
        "name": "nextjs-applet",
        "version": "0.1.0",
        "private": true,
        "scripts": {
          "dev": "next dev",
          "build": "next build",
          "start": "next start"
        },
        "dependencies": {
          "next": "^14.0.0",
          "react": "^18.0.0",
          "react-dom": "^18.0.0"
        }
      };

      const nextConfig = `/** @type {import('next').NextConfig} */
const nextConfig = {
  experimental: {
    appDir: true,
  },
}

module.exports = nextConfig`;

      const pageJs = `export default function Home() {
  return (
    <div>
      <h1>Welcome to Next.js Applet</h1>
      <p>This is a basic Next.js app created automatically.</p>
    </div>
  )
}`;

      const layoutJs = `export default function RootLayout({ children }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  )
}`;

      // Write files
      await fs.writeFile(path.join(cwd, 'package.json'), JSON.stringify(packageJson, null, 2));
      await fs.writeFile(path.join(cwd, 'next.config.js'), nextConfig);
      await fs.mkdir(path.join(cwd, 'app'), { recursive: true });
      await fs.writeFile(path.join(cwd, 'app', 'page.js'), pageJs);
      await fs.writeFile(path.join(cwd, 'app', 'layout.js'), layoutJs);

    } else if (framework === 'FRAMEWORK_VITE') {
      // Create basic Vite app
      const packageJson = {
        "name": "vite-applet",
        "version": "0.1.0",
        "private": true,
        "scripts": {
          "dev": "vite",
          "build": "vite build",
          "preview": "vite preview"
        },
        "dependencies": {
          "react": "^18.0.0",
          "react-dom": "^18.0.0"
        },
        "devDependencies": {
          "@vitejs/plugin-react": "^4.0.0",
          "vite": "^4.0.0"
        }
      };

      const viteConfig = `import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    host: '0.0.0.0',
    port: 3000
  }
})`;

      const indexHtml = `<!DOCTYPE html>
<html lang="en">
  <head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1.0" />
    <title>Vite Applet</title>
  </head>
  <body>
    <div id="root"></div>
    <script type="module" src="/src/main.jsx"></script>
  </body>
</html>`;

      const mainJsx = `import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App.jsx'

ReactDOM.createRoot(document.getElementById('root')).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
)`;

      const appJsx = `function App() {
  return (
    <div>
      <h1>Welcome to Vite Applet</h1>
      <p>This is a basic Vite app created automatically.</p>
    </div>
  )
}

export default App`;

      // Write files
      await fs.writeFile(path.join(cwd, 'package.json'), JSON.stringify(packageJson, null, 2));
      await fs.writeFile(path.join(cwd, 'vite.config.js'), viteConfig);
      await fs.writeFile(path.join(cwd, 'index.html'), indexHtml);
      await fs.mkdir(path.join(cwd, 'src'), { recursive: true });
      await fs.writeFile(path.join(cwd, 'src', 'main.jsx'), mainJsx);
      await fs.writeFile(path.join(cwd, 'src', 'App.jsx'), appJsx);
    }

    console.log(`Basic ${framework} app created successfully`);
  } catch (error) {
    console.error(`Error creating basic framework app: ${error}`);
    throw error;
  }
}

async function installDependenciesForFramework(framework, cwd = APP_DIR) {
  const desired = frameworkFromInput(framework);

  try {
    console.log(`Installing dependencies for ${desired} framework`);

    // Check if package.json exists
    const packageJsonPath = path.join(cwd, 'package.json');
    try {
      await fs.access(packageJsonPath);
    } catch {
      console.log('No package.json found, skipping dependency installation');
      return;
    }

    // Install dependencies
    const result = await runCommand('npm', ['install', '--no-fund', '--no-audit'], { cwd });
    if (result.code !== 0) {
      throw new Error(`npm install failed with code ${result.code}: ${result.stderr}`);
    }

    console.log(`Dependencies installed successfully for ${desired}`);
  } catch (error) {
    console.error(`Error installing dependencies: ${error}`);
    throw error;
  }
}

app.post('/dev/context/reset', async (req, res) => {
  try {
    const { framework = 'FRAMEWORK_UNSPECIFIED', environment_variables = {} } = req.body || {};

    // Stop server if running with robust termination (TERM then KILL)
    const pid = readPidFile();
    if (pid && isProcessAlive(pid)) {
      try { process.kill(-pid, 'SIGTERM'); } catch { try { process.kill(pid, 'SIGTERM'); } catch { } }
      const exited = await waitForExit(pid, 10000);
      if (!exited) {
        try { process.kill(-pid, 'SIGKILL'); } catch { try { process.kill(pid, 'SIGKILL'); } catch { } }
        await waitForExit(pid, 5000);
      }
      try { fsSync.unlinkSync(PID_FILE); } catch { }
    }

    // Update context
    currentContext = { framework: frameworkFromInput(framework), environment_variables: environment_variables && typeof environment_variables === 'object' ? environment_variables : {} };
    saveContext(currentContext);

    // Ensure port is free before starting a new server
    await waitForPortToBeFree(APP_PORT_DEFAULT, 15000);

    // Prepare framework files from hardcoded templates
    await syncFrameworkFiles(currentContext.framework, APP_DIR);

    // Install dependencies for the new framework
    await installDependenciesForFramework(currentContext.framework, APP_DIR);

    // Start dev server with new context
    const [cmd, args] = resolveDevCommand(APP_DIR, APP_PORT_DEFAULT, currentContext.framework);
    const child = spawn(cmd, args, {
      cwd: APP_DIR,
      env: { ...process.env, ...currentContext.environment_variables, HOST: '0.0.0.0', PORT: String(APP_PORT_DEFAULT) },
      detached: true,
      stdio: ['ignore', 'pipe', 'pipe'],
    });

    // Put child in its own process group to allow group signals
    try { process.kill(-child.pid, 0); } catch { }
    try { fsSync.writeFileSync(PID_FILE, String(child.pid)); } catch { }
    child.stdout.on('data', (d) => process.stdout.write(d));
    child.stderr.on('data', (d) => process.stderr.write(d));
    child.on('exit', () => { try { fsSync.unlinkSync(PID_FILE); } catch { } });

    return res.status(200).json({
      success: true,
      pid: child.pid,
      message: `Context changed to ${currentContext.framework}, files generated, dependencies installed, and dev server started.`,
      framework: currentContext.framework,
      port: APP_PORT_DEFAULT
    });
  } catch (error) {
    return res.status(500).json({ success: false, message: String(error) });
  }
});

// Lightweight health endpoint used by the startup script
app.get('/health', (_req, res) => {
  res.status(200).send('ok');
});

// Initial setup endpoint for container startup
app.post('/dev/setup', async (req, res) => {
  try {
    const { framework = 'FRAMEWORK_UNSPECIFIED', environment_variables = {} } = req.body || {};

    // Set initial context
    currentContext = { framework: frameworkFromInput(framework), environment_variables: environment_variables && typeof environment_variables === 'object' ? environment_variables : {} };
    saveContext(currentContext);

    // Clean any existing artifacts
    await cleanWorkspace(currentContext.framework, APP_DIR);

    // Sync framework files
    await syncFrameworkFiles(currentContext.framework, APP_DIR);

    // Install dependencies
    await installDependenciesForFramework(currentContext.framework, APP_DIR);

    // Start dev server
    const [cmd, args] = resolveDevCommand(APP_DIR, APP_PORT_DEFAULT, currentContext.framework);
    const child = spawn(cmd, args, {
      cwd: APP_DIR,
      env: { ...process.env, ...currentContext.environment_variables, HOST: '0.0.0.0', PORT: String(APP_PORT_DEFAULT) },
      detached: true,
      stdio: ['ignore', 'pipe', 'pipe'],
    });

    // Put child in its own process group to allow group signals
    try { process.kill(-child.pid, 0); } catch { }
    try { fsSync.writeFileSync(PID_FILE, String(child.pid)); } catch { }
    child.stdout.on('data', (d) => process.stdout.write(d));
    child.stderr.on('data', (d) => process.stderr.write(d));
    child.on('exit', () => { try { fsSync.unlinkSync(PID_FILE); } catch { } });

    return res.status(200).json({
      success: true,
      pid: child.pid,
      message: `Initial setup complete for ${currentContext.framework}, dev server started.`,
      framework: currentContext.framework,
      port: APP_PORT_DEFAULT
    });
  } catch (error) {
    return res.status(500).json({ success: false, message: String(error) });
  }
});

// Detailed health endpoint aligned with service HealthCheckResponse
app.get('/health', (_req, res) => {
  try {
    const envCount = currentContext && currentContext.environment_variables ? Object.keys(currentContext.environment_variables).length : 0;
    res.status(200).json({ status: 'healthy', timestamp: new Date().toISOString(), framework: currentContext.framework, environment_variables_count: envCount });
  } catch (error) {
    res.status(500).json({ status: 'unhealthy', error: String(error) });
  }
});

app.listen(port, () => {
  console.log(`Server listening at http://localhost:${port}`);
});
