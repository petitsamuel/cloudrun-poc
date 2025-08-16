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

// --- HTTP Handlers ---

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

	// Send an initial connected message.
	fmt.Fprintf(w, "data: Connected to log stream.\n\n")
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			// Client has disconnected.
			return
		case msg := <-clientChan:
			// Format message as a Server-Sent Event.
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		}
	}
}

type SyncRequest struct {
	Files            map[string]string `json:"files"`
	DeletedFilePaths []string          `json:"deleted_file_paths"`
}

func syncHandler(w http.ResponseWriter, r *http.Request) {
	var req SyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(req.Files)+len(req.DeletedFilePaths))

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

	if len(allErrors) > 0 {
		httpError(w, strings.Join(allErrors, "; "), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{"success": true, "message": "Files synced successfully"})
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

	args := append([]string{"install", "--no-fund", "--no-audit"}, req.ExtraArgs...)
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

type StartRequest struct {
	Port int `json:"port"`
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

// --- Core Logic ---

func handleDevOperation(w http.ResponseWriter, r *http.Request, operation string) {
	var req StartRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "Invalid request body", http.StatusBadRequest)
			return
		}
	}

	if req.Port == 0 {
		req.Port = defaultAppPort
	}

	devOpMutex.Lock()
	defer devOpMutex.Unlock()

	pid, err := readPID()
	isAlive := err == nil && isProcessAlive(pid)

	switch operation {
	case "stop":
		if !isAlive {
			jsonResponse(w, http.StatusOK, map[string]interface{}{"stopped": true, "message": "Dev server not running"})
			return
		}
		if err := stopDevServer(); err != nil {
			httpError(w, fmt.Sprintf("Failed to stop dev server: %v", err), http.StatusInternalServerError)
			return
		}
		jsonResponse(w, http.StatusOK, map[string]interface{}{"stopped": true, "message": "Dev server stopped successfully"})

	case "start":
		if isAlive {
			httpError(w, "Already running", http.StatusConflict)
			return
		}
		newPid, err := startDevServer(req.Port)
		if err != nil {
			httpError(w, fmt.Sprintf("Failed to start dev server: %v", err), http.StatusInternalServerError)
			return
		}
		jsonResponse(w, http.StatusAccepted, map[string]interface{}{"operation_initiated": true, "pid": newPid})

	case "restart":
		logBroadcaster.Submit("--- Server restarting... ---")
		if isAlive {
			if err := stopDevServer(); err != nil {
				log.Printf("Failed to stop dev server during restart, proceeding anyway: %v", err)
			}
		}
		newPid, err := startDevServer(req.Port)
		if err != nil {
			httpError(w, fmt.Sprintf("Failed to start dev server: %v", err), http.StatusInternalServerError)
			return
		}
		jsonResponse(w, http.StatusAccepted, map[string]interface{}{"operation_initiated": true, "pid": newPid})
	}
}

func startDevServer(port int) (int, error) {
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
	return proc.Process.Pid, nil
}

func stopDevServer() error {
	pid, err := readPID()
	if err != nil {
		return nil // Not running or no pid file.
	}
	if !isProcessAlive(pid) {
		os.Remove(pidFile)
		return nil
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
			return nil
		default:
			time.Sleep(150 * time.Millisecond)
		}
	}

	log.Printf("Process %d stopped.", pid)
	logBroadcaster.Submit(fmt.Sprintf("--- Server (PID %d) stopped ---", pid))
	os.Remove(pidFile)
	return nil
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

	// Prefer framework-specific commands for better control
	if _, ok := pkg.Dependencies["next"]; ok {
		return "node", []string{"node_modules/next/dist/bin/next", "dev", "-p", strconv.Itoa(port)}, nil
	}
	if _, ok := pkg.Dependencies["vite"]; ok {
		return "node", []string{"node_modules/vite/bin/vite.js", "--port", strconv.Itoa(port)}, nil
	}

	// Fallback to standard npm scripts
	if _, ok := pkg.Scripts["dev"]; ok {
		return "npm", []string{"run", "dev"}, nil
	}
	if _, ok := pkg.Scripts["start"]; ok {
		return "npm", []string{"start"}, nil
	}

	return "", nil, fmt.Errorf("no suitable dev command found in package.json (checked for 'next'/'vite' deps and 'dev'/'start' scripts)")
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// TODO: make this more secure
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

// TODO:
// make reading logs return if there are compilation errors or not
// start cannot use a different port
// add pre-warm logic
// stop should return if it force stoped or not
// updating package.json in sync should auto-install (add req param)
// add authentication support with cloud run service account
