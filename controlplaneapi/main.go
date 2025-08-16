// main.go
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// --- Configuration ---
var (
	listenAddr     = ":8000"
	appDir         = "/app/applet"
	pidFile        = "/app/applet/.dev.pid"
	defaultAppPort = 3000
)

// --- State Management ---
var (
	// devOpMutex prevents concurrent start/stop/restart operations.
	devOpMutex sync.Mutex
	// logBroadcaster handles streaming dev server logs to connected clients.
	logBroadcaster = newBroadcaster()
)

// --- Main Application ---
func main() {
	flag.StringVar(&listenAddr, "listen-addr", ":8000", "The address to listen on")
	flag.StringVar(&appDir, "app-dir", "/app/applet", "The directory of the application")
	flag.IntVar(&defaultAppPort, "default-app-port", 3000, "The default port for the application")
	flag.Parse()

	pidFile = filepath.Join(appDir, ".dev.pid")

	// Start the log broadcaster in a separate goroutine.
	go logBroadcaster.run()

	// Register all HTTP handlers.
	mux := http.NewServeMux()
	mux.HandleFunc("/sync", syncHandler)
	mux.HandleFunc("/dev/install", dependenciesInstallHandler)
	mux.HandleFunc("/dev/status", statusHandler)
	mux.HandleFunc("/dev/start", startHandler)
	mux.HandleFunc("/dev/stop", stopHandler)
	mux.HandleFunc("/dev/restart", restartHandler)
	mux.HandleFunc("/dev/logs", logsHandler)
	mux.HandleFunc("/health", healthHandler)

	server := &http.Server{
		Addr:    listenAddr,
		Handler: corsMiddleware(mux),
	}

	// Run server in a goroutine so it doesn't block.
	go func() {
		log.Printf("Control Plane API listening on %s", listenAddr)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// Wait for an interrupt signal for graceful shutdown.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Ensure the dev server is stopped cleanly on shutdown.
	if pid, err := readPID(); err == nil && isProcessAlive(pid) {
		log.Println("Stopping dev server during shutdown...")
		stopDevServer()
	}

	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Server exiting.")
}

// --- Log Broadcasting (for /dev/logs) ---

// Broadcaster manages active clients for log streaming.
type Broadcaster struct {
	clients    map[chan string]bool
	register   chan chan string
	unregister chan chan string
	messages   chan BroadcastMessage
	mu         sync.Mutex
}

// BroadcastMessage represents a log line with its output stream.
type BroadcastMessage struct {
	Text     string
	IsStderr bool
}

func newBroadcaster() *Broadcaster {
	return &Broadcaster{
		clients:    make(map[chan string]bool),
		register:   make(chan chan string),
		unregister: make(chan chan string),
		messages:   make(chan BroadcastMessage, 100), // Buffered channel
	}
}

// run is the central loop that manages clients and broadcasts messages.
func (b *Broadcaster) run() {
	for {
		select {
		case client := <-b.register:
			b.mu.Lock()
			b.clients[client] = true
			b.mu.Unlock()
			log.Println("Log stream client registered.")
		case client := <-b.unregister:
			b.mu.Lock()
			if _, ok := b.clients[client]; ok {
				delete(b.clients, client)
				close(client)
			}
			b.mu.Unlock()
			log.Println("Log stream client unregistered.")
		case msg := <-b.messages:
			b.mu.Lock()
			for client := range b.clients {
				// Non-blocking send to prevent one slow client from blocking all others.
				select {
				case client <- msg.Text:
				default:
					log.Println("Log stream client channel is full. Dropping message.")
				}
			}
			b.mu.Unlock()
			// Also write to the appropriate OS stream.
			if msg.IsStderr {
				fmt.Fprintln(os.Stderr, msg.Text)
			} else {
				fmt.Fprintln(os.Stdout, msg.Text)
			}
		}
	}
}

// Submit sends a message to all connected clients.
func (b *Broadcaster) Submit(msg string) {
	b.messages <- BroadcastMessage{Text: msg, IsStderr: false}
}

// SubmitStderr sends a stderr-classified message to all connected clients.
func (b *Broadcaster) SubmitStderr(msg string) {
	b.messages <- BroadcastMessage{Text: msg, IsStderr: true}
}

func logsHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	clientChan := make(chan string, 10)
	logBroadcaster.register <- clientChan
	defer func() {
		logBroadcaster.unregister <- clientChan
	}()

	type logEntry struct {
		Log           string `json:"log"`
		Error         bool   `json:"error"`
		SystemMessage string `json:"system_message"`
	}

	errorRegex := regexp.MustCompile(`(?i)error|exception|failed|unhandled`)

	initialEntry := logEntry{SystemMessage: "CONNECTED"}
	initialData, err := json.Marshal(initialEntry)
	if err == nil {
		fmt.Fprintf(w, "data: %s\n\n", initialData)
		flusher.Flush()
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			closedEntry := logEntry{SystemMessage: "DISCONNECTED"}
			jsonData, err := json.Marshal(closedEntry)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", jsonData)
			flusher.Flush()
			return
		case msg := <-clientChan:
			isError := errorRegex.MatchString(msg)
			entry := logEntry{
				Log:   msg,
				Error: isError,
			}
			jsonData, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", jsonData)
			flusher.Flush()
		}
	}
}

type SyncRequest struct {
	Files            map[string]string `json:"files"`
	DeletedFilePaths []string          `json:"deleted_file_paths"`
}

// runCommandAndStreamOutput executes a command and streams its output to the log broadcaster.
func runCommandAndStreamOutput(command string, args []string) error {
	cmd := exec.Command(command, args...)
	cmd.Dir = appDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe for %s: %w", command, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe for %s: %w", command, err)
	}

	log.Printf("Running: %s %s in %s", command, strings.Join(args, " "), appDir)
	logBroadcaster.Submit(fmt.Sprintf("--- Running: %s %s ---", command, strings.Join(args, " ")))

	if err := cmd.Start(); err != nil {
		logBroadcaster.Submit(fmt.Sprintf("--- Failed to start command: %s ---", command))
		return fmt.Errorf("failed to start command %s: %w", command, err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		streamPipeToBroadcaster(stdout, "STDOUT")
	}()
	go func() {
		defer wg.Done()
		streamPipeToBroadcaster(stderr, "STDERR")
	}()

	wg.Wait() // Wait for pipes to be fully drained to capture all output.

	err = cmd.Wait()
	if err != nil {
		logBroadcaster.Submit(fmt.Sprintf("--- Command failed: %s %s (%v) ---", command, strings.Join(args, " "), err))
		return err
	}

	logBroadcaster.Submit(fmt.Sprintf("--- Command finished successfully: %s %s ---", command, strings.Join(args, " ")))
	return nil
}

func syncHandler(w http.ResponseWriter, r *http.Request) {
	var req SyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	// Check if package.json is being modified before applying changes.
	packageJsonModified := false
	for p := range req.Files {
		if filepath.Clean(p) == "package.json" {
			packageJsonModified = true
			break
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(req.Files)+len(req.DeletedFilePaths))

	// Apply file changes concurrently.
	for p, b64 := range req.Files {
		wg.Add(1)
		go func(p, b64 string) {
			defer wg.Done()
			if err := writeFileBase64(p, b64); err != nil {
				errs <- fmt.Errorf("failed to write %s: %w", p, err)
			}
		}(p, b64)
	}

	for _, p := range req.DeletedFilePaths {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			if err := deletePath(p); err != nil {
				errs <- fmt.Errorf("failed to delete %s: %w", p, err)
			}
		}(p)
	}

	wg.Wait()
	close(errs)

	var allErrors []string
	for err := range errs {
		allErrors = append(allErrors, err.Error())
	}

	// If file operations failed, stop here.
	if len(allErrors) > 0 {
		httpError(w, strings.Join(allErrors, "; "), http.StatusInternalServerError)
		return
	}

	// If package.json was changed, run npm install and prune.
	var depMessages []string
	if packageJsonModified {
		log.Println("package.json modified, running dependency reconciliation.")
		logBroadcaster.Submit("--- package.json updated. Reconciling dependencies... ---")

		// Install dependencies.
		installArgs := []string{"install", "--no-fund", "--prefer-offline", "--no-optional", "--no-audit"}
		if err := runCommandAndStreamOutput("npm", installArgs); err != nil {
			msg := fmt.Sprintf("npm install failed: %v", err)
			log.Println(msg)
			allErrors = append(allErrors, msg)
		} else {
			depMessages = append(depMessages, "npm install completed successfully.")
			// Prune unused dependencies after install.
			pruneArgs := []string{"prune"}
			if err := runCommandAndStreamOutput("npm", pruneArgs); err != nil {
				msg := fmt.Sprintf("npm prune failed: %v", err)
				log.Println(msg)
				allErrors = append(allErrors, msg)
			} else {
				depMessages = append(depMessages, "npm prune completed successfully.")
			}
		}
		logBroadcaster.Submit("--- Dependency reconciliation finished. ---")
	}

	if len(allErrors) > 0 {
		httpError(w, strings.Join(allErrors, "; "), http.StatusInternalServerError)
		return
	}

	finalMessage := "Files synced successfully"
	if len(depMessages) > 0 {
		finalMessage = fmt.Sprintf("%s. %s", finalMessage, strings.Join(depMessages, " "))
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"success": true, "message": finalMessage})
}

type InstallRequest struct {
	ExtraArgs []string `json:"extra_args"`
}

func dependenciesInstallHandler(w http.ResponseWriter, r *http.Request) {
	var req InstallRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "Invalid request body", http.StatusBadRequest)
			return
		}
	}

	args := append([]string{"install", "--no-fund", "--prefer-offline", "--no-optional", "--no-audit"}, req.ExtraArgs...)
	cmd := exec.Command("npm", args...)
	cmd.Dir = appDir

	log.Printf("Running: npm %s in %s", strings.Join(args, " "), appDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		exitCode := -1
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
		log.Printf("npm install failed: %s", string(output))
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{
			"success":       false,
			"exit_code":     exitCode,
			"error_message": string(output),
		})
		return
	}

	log.Println("npm install completed successfully")
	jsonResponse(w, http.StatusOK, map[string]interface{}{"success": true, "exit_code": 0})
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	pid, err := readPID()
	if err != nil || !isProcessAlive(pid) {
		jsonResponse(w, http.StatusOK, map[string]interface{}{"running": false, "pid": nil})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"running": true, "pid": pid})
}

func startHandler(w http.ResponseWriter, r *http.Request) {
	handleDevOperation(w, r, "start")
}

func stopHandler(w http.ResponseWriter, r *http.Request) {
	handleDevOperation(w, r, "stop")
}

func restartHandler(w http.ResponseWriter, r *http.Request) {
	handleDevOperation(w, r, "restart")
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":    "healthy",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

type DevOpRequest struct {
	Prewarm *PrewarmConfig `json:"prewarm,omitempty"`
}

type PrewarmConfig struct {
	Paths             []string `json:"paths"`
	WaitForCompletion bool     `json:"wait_for_completion"`
}

type DevOpResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	// Included only for start/restart operations
	PID         int  `json:"pid,omitempty"`
	ForceKilled bool `json:"force_killed,omitempty"`
}

func sendJSONResponse(w http.ResponseWriter, statusCode int, payload DevOpResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("Error encoding JSON response: %v", err)
	}
}

// performPrewarming sends GET requests to a list of paths to warm up the dev server.
func performPrewarming(config PrewarmConfig, port int) {
	log.Printf("Starting pre-warming for %d paths...", len(config.Paths))

	// Wait for the dev server to accept connections before prewarming.
	// Treat either 2xx or 404 responses as "ready" (mirrors Node helper).
	if !waitForServerReady(port, 20*time.Second) {
		log.Printf("Dev server on port %d did not become ready within timeout; proceeding anyway", port)
	}

	client := &http.Client{
		Timeout: 10 * time.Second, // Timeout for each pre-warm request.
	}
	var wg sync.WaitGroup

	for _, path := range config.Paths {
		if !strings.HasPrefix(path, "/") {
			log.Printf("Skipping invalid pre-warm path: %s", path)
			continue
		}

		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			url := fmt.Sprintf("http://localhost:%d%s", port, p)
			log.Printf("Pre-warming path: %s", url)
			resp, err := client.Get(url)
			if err != nil {
				log.Printf("Pre-warm request to %s failed: %v", url, err)
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			log.Printf("Pre-warmed %s - Status: %s", url, resp.Status)
		}(path)
	}

	wg.Wait()
	log.Println("Pre-warming completed.")
}

// waitForServerReady polls the base URL until it responds (2xx or 404) or times out.
func waitForServerReady(port int, timeout time.Duration) bool {
	baseURL := fmt.Sprintf("http://localhost:%d", port)
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(baseURL)
		if err == nil {
			status := resp.StatusCode
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if (status >= 200 && status < 300) || status == http.StatusNotFound {
				return true
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

func handleDevOperation(w http.ResponseWriter, r *http.Request, operation string) {
	devOpMutex.Lock()
	defer devOpMutex.Unlock()

	pid, err := readPID()
	isAlive := err == nil && isProcessAlive(pid)

	// Optional request body for pre-warming config.
	var req DevOpRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			sendJSONResponse(w, http.StatusBadRequest, DevOpResponse{
				Success: false,
				Message: fmt.Sprintf("Invalid JSON body: %v", err),
			})
			return
		}
	}

	switch operation {
	case "stop":
		if !isAlive {
			sendJSONResponse(w, http.StatusOK, DevOpResponse{
				Success: true,
				Message: "Dev server not running",
			})
			return
		}
		forceKilled, err := stopDevServer()
		if err != nil {
			httpError(w, fmt.Sprintf("Failed to stop dev server: %v", err), http.StatusInternalServerError)
			return
		}
		sendJSONResponse(w, http.StatusOK, DevOpResponse{
			Success:     true,
			Message:     "Dev server stopped successfully",
			ForceKilled: forceKilled,
		})

	case "start":
		if isAlive {
			httpError(w, "Already running", http.StatusConflict)
			return
		}
		newPid, err := startDevServer(defaultAppPort, req.Prewarm)
		if err != nil {
			httpError(w, fmt.Sprintf("Failed to start dev server: %v", err), http.StatusInternalServerError)
			return
		}
		sendJSONResponse(w, http.StatusAccepted, DevOpResponse{
			Success: true,
			Message: "Dev server started successfully",
			PID:     newPid,
		})

	case "restart":
		logBroadcaster.Submit("--- Server restarting... ---")
		forceKilled := false
		var err error
		if isAlive {
			forceKilled, err = stopDevServer()
			if err != nil {
				log.Printf("Failed to stop dev server during restart, proceeding anyway: %v", err)
			}
		}
		newPid, err := startDevServer(defaultAppPort, req.Prewarm)
		if err != nil {
			httpError(w, fmt.Sprintf("Failed to start dev server: %v", err), http.StatusInternalServerError)
			return
		}
		sendJSONResponse(w, http.StatusAccepted, DevOpResponse{
			Success:     true,
			Message:     "Dev server restarted successfully",
			PID:         newPid,
			ForceKilled: forceKilled,
		})
	}
}

func startDevServer(port int, prewarm *PrewarmConfig) (int, error) {
	cmd, args, err := resolveDevCommand(appDir, port)
	if err != nil {
		return 0, fmt.Errorf("could not resolve dev command: %w", err)
	}

	log.Printf("Starting dev server: %s %s", cmd, strings.Join(args, " "))
	proc := exec.Command(cmd, args...)
	proc.Dir = appDir
	proc.Env = append(os.Environ(), fmt.Sprintf("PORT=%d", port), "HOST=0.0.0.0")

	// Crucial for robust process killing: create a new process group.
	proc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Capture stdout and stderr for log streaming.
	stdout, _ := proc.StdoutPipe()
	stderr, _ := proc.StderrPipe()
	go streamPipeToBroadcaster(stdout, "STDOUT")
	go streamPipeToBroadcaster(stderr, "STDERR")

	if err := proc.Start(); err != nil {
		return 0, fmt.Errorf("failed to start process: %w", err)
	}

	if err := writePID(proc.Process.Pid); err != nil {
		proc.Process.Kill() // Kill orphan process if we can't track it.
		return 0, fmt.Errorf("failed to write pid file: %w", err)
	}

	log.Printf("Dev server started with PID: %d", proc.Process.Pid)
	logBroadcaster.Submit(fmt.Sprintf("--- Server started with PID %d on port %d ---", proc.Process.Pid, port))

	if prewarm != nil && len(prewarm.Paths) > 0 {
		logBroadcaster.Submit(fmt.Sprintf("--- Pre-warming %d paths ---", len(prewarm.Paths)))
		if prewarm.WaitForCompletion {
			performPrewarming(*prewarm, port)
			logBroadcaster.Submit("--- Pre-warming completed ---")
		} else {
			go performPrewarming(*prewarm, port)
			logBroadcaster.Submit("--- Pre-warming running in the background ---")
		}
	}

	return proc.Process.Pid, nil
}

// stopDevServer returns true if the server was force-killed, false if it exited gracefully.
func stopDevServer() (bool, error) {
	pid, err := readPID()
	if err != nil {
		return false, nil // Not running or no pid file.
	}
	if !isProcessAlive(pid) {
		os.Remove(pidFile)
		return false, nil
	}

	log.Printf("Stopping process group with PGID: %d", pid)
	// Kill the entire process group by sending a signal to -PID.
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		log.Printf("Failed to kill process group %d with SIGTERM, trying single process: %v", pid, err)
		syscall.Kill(pid, syscall.SIGTERM) // Fallback for safety.
	}

	// Wait for the process to exit, with a timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for isProcessAlive(pid) {
		select {
		case <-ctx.Done():
			log.Printf("Process %d did not exit gracefully, sending SIGKILL.", pid)
			syscall.Kill(-pid, syscall.SIGKILL) // Force kill the group.
			time.Sleep(1 * time.Second)         // Give SIGKILL time to work.
			logBroadcaster.Submit(fmt.Sprintf("--- Server (PID %d) force-killed ---", pid))
			os.Remove(pidFile)
			return true, nil
		default:
			time.Sleep(150 * time.Millisecond)
		}
	}

	log.Printf("Process %d stopped.", pid)
	logBroadcaster.Submit(fmt.Sprintf("--- Server (PID %d) stopped ---", pid))
	os.Remove(pidFile)
	return false, nil
}

// --- Utility Functions ---

func streamPipeToBroadcaster(pipe io.Reader, prefix string) {
	scanner := bufio.NewScanner(pipe)
	for scanner.Scan() {
		if prefix == "STDERR" {
			logBroadcaster.SubmitStderr(scanner.Text())
		} else {
			logBroadcaster.Submit(scanner.Text())
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("Error reading from %s pipe: %v", prefix, err)
	}
}

type PackageJSON struct {
	Scripts      map[string]string `json:"scripts"`
	Dependencies map[string]string `json:"dependencies"`
}

func readPackageJSON(dir string) (*PackageJSON, error) {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil, err
	}
	var pkg PackageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, err
	}
	return &pkg, nil
}

func resolveDevCommand(cwd string, port int) (string, []string, error) {
	pkg, err := readPackageJSON(cwd)
	if err != nil {
		return "", nil, fmt.Errorf("cannot read package.json: %w", err)
	}

	// Prefer package.json scripts.
	if _, ok := pkg.Scripts["dev"]; ok {
		return "npm", []string{"run", "dev"}, nil
	}
	if _, ok := pkg.Scripts["start"]; ok {
		return "npm", []string{"start"}, nil
	}

	// Fallback to framework-specific commands.
	if _, ok := pkg.Dependencies["next"]; ok {
		return "node", []string{"node_modules/next/dist/bin/next", "dev", "-p", strconv.Itoa(port)}, nil
	}
	if _, ok := pkg.Dependencies["vite"]; ok {
		return "node", []string{"node_modules/vite/bin/vite.js", "--port", strconv.Itoa(port)}, nil
	}
	if _, ok := pkg.Dependencies["@angular/cli"]; ok {
		return "npx", []string{"ng", "serve", "--port", strconv.Itoa(port)}, nil
	}

	return "", nil, fmt.Errorf("no suitable dev command found in package.json (checked for 'next'/'vite' deps and 'dev'/'start' scripts)")
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// TODO: samuelpetit - only allow AI Studio origins when in prod.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func httpError(w http.ResponseWriter, message string, code int) {
	log.Printf("HTTP Error %d: %s", code, message)
	jsonResponse(w, code, map[string]string{"error": message})
}

func jsonResponse(w http.ResponseWriter, code int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if payload != nil {
		json.NewEncoder(w).Encode(payload)
	}
}

// --- File System & Process Helpers ---

func resolveWithinAppDir(p string) (string, error) {
	cleanPath := filepath.Join(appDir, p)
	absAppDir, _ := filepath.Abs(appDir)
	absCleanPath, _ := filepath.Abs(cleanPath)
	if !strings.HasPrefix(absCleanPath, absAppDir) {
		return "", fmt.Errorf("path traversal attempt detected: %s", p)
	}
	return absCleanPath, nil
}

func writeFileBase64(p, b64 string) error {
	dest, err := resolveWithinAppDir(p)
	if err != nil {
		return err
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("invalid base64 content for %s: %w", p, err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0644)
}

func deletePath(p string) error {
	dest, err := resolveWithinAppDir(p)
	if err != nil {
		return err
	}
	return os.RemoveAll(dest)
}

func readPID() (int, error) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func writePID(pid int) error {
	return os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0644)
}

func isProcessAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, sending signal 0 to a process checks if it exists without killing it.
	return proc.Signal(syscall.Signal(0)) == nil
}
