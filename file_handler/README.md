# AppletContainerControlPlaneService

A service for managing developer application environments with file synchronization, dependency management, and development server control.

## API Endpoints

### File Management

#### POST /sync
Synchronizes files to the server's filesystem and optionally deletes specified files.

Supports the service JSON shape that mirrors the protobuf messages.

**Request Body:**
```json
{
  "files": {
    "path/to/file": "base64-encoded-content",
    "path/to/file2": "base64-encoded-content2"
  },
  "deleted_file_paths": ["path/to/delete1", "path/to/delete2"],
  "prewarm_config": {
    "paths": ["/", "/api/hello"],
    "port": 3000,
    "wait_for_completion": true
  }
}
```

**Response:**
```json
{
  "success": true,
  "message": "Files synced successfully"
}
```

### Dependency Management

#### POST /dependencies/install
Installs npm dependencies with optional prewarming.

This endpoint aligns with `InstallDependenciesRequest`/`InstallDependenciesResponse`.

**Request Body:**
```json
{
  "cwd": "/app/applet",
  "extra_args": ["--production"],
  "prewarm_config": {
    "paths": ["/"],
    "port": 3000,
    "wait_for_completion": false
  }
}
```

**Response:**
```json
{
  "success": true,
  "exit_code": 0
}
```

### Development Server Control

#### POST /dev/start
Starts the development server.

Aligns with `StartDevServerRequest` → `DevServerResponse`.

**Request Body:**
```json
{
  "port": 3000,
  "prewarm_config": {
    "paths": ["/", "/api/hello"],
    "port": 3000,
    "wait_for_completion": true
  }
}
```

**Response:**
```json
{
  "operation_initiated": true,
  "pid": 12345
}
```

#### POST /dev/stop
Stops the development server.

**Response:**
```json
{
  "stopped": true,
  "message": "Dev server stopped successfully"
}
```

#### POST /dev/restart
Restarts the development server.

Aligns with `RestartDevServer` → `DevServerResponse`.

**Request Body:**
```json
{
  "port": 3000,
  "prewarm_config": {
    "paths": ["/"],
    "port": 3000,
    "wait_for_completion": false
  }
}
```

**Response:**
```json
{
  "operation_initiated": true,
  "pid": 12346
}
```

#### GET /dev/status
Gets the current status of the development server.

**Response:**
```json
{
  "running": true,
  "pid": 12345
}
```

### Context Management

#### POST /dev/context/reset
Resets the development server context for a new applet/framework.

Aligns with `ResetDevServerContextRequest` → `ResetDevServerContextResponse`.

**Request Body:**
```json
{
  "framework": "FRAMEWORK_NEXTJS",
  "environment_variables": {
    "NODE_ENV": "development",
    "API_KEY": "your-api-key"
  }
}
```

**Response:**
```json
{
  "success": true,
  "message": "Workspace cleaned, ready for FRAMEWORK_NEXTJS applet.",
  "pid": null
}
```

**Supported Frameworks:**
- `FRAMEWORK_UNSPECIFIED` (0) - Auto-detect
- `FRAMEWORK_NEXTJS` (1) - Next.js
- `FRAMEWORK_VITE` (2) - Vite

### Health Check

#### GET /health
Lightweight health check endpoint.

Aligns with `HealthCheckResponse`.

**Response:**
```json
{
  "status": "healthy",
  "timestamp": "2024-01-01T00:00:00.000Z",
  "framework": "FRAMEWORK_NEXTJS",
  "environment_variables_count": 2
}
```

#### GET /healthz
Legacy health endpoint for backward compatibility.

## Environment Variables

- `PORT` - Service port (default: 8000)
- `APP_DIR` - Application directory (default: /app/applet)
- `APP_PORT` - Default development server port (default: 3000)
- `CORS_ORIGINS` - CORS allowed origins (default: *)

## Features

- **File Synchronization**: Sync files with support for deletion and prewarming
- **Framework Detection**: Auto-detect and support Next.js, Vite, Angular CLI
- **Process Management**: Proper PID tracking and process group management
- **Workspace Cleaning**: Clean build artifacts when switching contexts
- **Environment Variables**: Persistent environment variable management
- **Prewarming**: Pre-compile pages for faster first requests
- **Health Monitoring**: Comprehensive health check endpoints

## Usage Examples

### Starting a Next.js Development Server
```bash
curl -X POST http://localhost:8000/dev/context/reset \
  -H "Content-Type: application/json" \
  -d '{"framework": "FRAMEWORK_NEXTJS"}'

curl -X POST http://localhost:8000/dev/start \
  -H "Content-Type: application/json" \
  -d '{"port": 3000}'
```

### Syncing Files with Prewarming
```bash
curl -X POST http://localhost:8000/sync \
  -H "Content-Type: application/json" \
  -d '{
    "files": {
      "src/app/page.tsx": "base64-encoded-content"
    },
    "prewarm_config": {
      "paths": ["/", "/api/hello"],
      "port": 3000,
      "wait_for_completion": true
    }
  }'
```
