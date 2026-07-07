package main

import (
	"bytes"
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"
)

// Embed nginx.tmpl template into binary via go:embed
//go:embed nginx.tmpl
var nginxTemplate string

type ExtraPath struct {
	Path        string `json:"path"`
	BackendPath string `json:"backend_path"`
}

type Proxy struct {
	Type         string      `json:"type"`          // "direct" or "path"
	ExternalPort int         `json:"external_port"` // Port exposed externally by Nginx
	Path         string      `json:"path"`          // Routing path prefix like /video2gif/
	ExtraPaths   []ExtraPath `json:"extra_paths"`   // Extra route mapping
	ProxyPass    string      `json:"proxy_pass"`    // Custom proxy target (e.g., https://example.com/)
}

type Service struct {
	Name        string `json:"name"`
	Port        int    `json:"port"`
	Script      string `json:"script"`
	Cwd         string `json:"cwd"`
	Interpreter string `json:"interpreter"`
	Args        string `json:"args"`
	Proxy       *Proxy `json:"proxy"` // Optional proxy settings
}

type Config struct {
	Services []Service `json:"services"`
}

// Data structures for template rendering
type LocationConfig struct {
	Path          string
	BackendURL    string
	HostHeader    string
	EnableSSLName bool
}

type ServerConfig struct {
	Port      int
	Locations []LocationConfig
}

var scriptDir string
var pidFile string

func init() {
	exePath, err := os.Executable()
	if err != nil {
		scriptDir = "."
	} else {
		scriptDir = filepath.Dir(exePath)
	}
	pidFile = filepath.Join(scriptDir, "one4all.pid")
}

func loadConfig() (*Config, error) {
	configPath := filepath.Join(scriptDir, "one4all.json")
	data, err := ioutil.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", configPath, err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse one4all.json: %w", err)
	}
	return &config, nil
}

func findExecutable(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err == nil {
		return path, nil
	}

	extraPaths := []string{
		"/opt/homebrew/bin/" + name,
		"/usr/local/bin/" + name,
		"/usr/sbin/" + name,
		"/sbin/" + name,
	}
	for _, p := range extraPaths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("executable not found: %s", name)
}

func logReader(serviceName string, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		timeStr := time.Now().Format("2006/01/02 15:04:05")
		fmt.Printf("[%s] %s | %s\n", serviceName, timeStr, scanner.Text())
	}
}

var activeCmdMu sync.Mutex
var activeCommands = make(map[string]*exec.Cmd)
var shouldRestart = true

func monitorService(s Service) {
	if s.Script == "" || s.Port == 0 {
		fmt.Printf("[%s] ℹ️ Pure proxy service, no local process to monitor.\n", s.Name)
		return
	}

	absCwd := s.Cwd
	if strings.HasPrefix(s.Cwd, ".") {
		absCwd = filepath.Join(scriptDir, s.Cwd)
	}

	for {
		if !shouldRestart {
			return
		}

		fmt.Printf("[%s] 🚀 Starting service (in %s)...\n", s.Name, absCwd)
		if _, err := os.Stat(absCwd); os.IsNotExist(err) {
			fmt.Printf("[%s] ❌ Error: directory %s does not exist, retrying in 5s...\n", s.Name, absCwd)
			time.Sleep(5 * time.Second)
			continue
		}

		args := []string{s.Script}
		if s.Args != "" {
			// Substitute {{.Port}} placeholder dynamically to avoid redundancy
			replacedArgs := strings.ReplaceAll(s.Args, "{{.Port}}", strconv.Itoa(s.Port))
			args = append(args, strings.Fields(replacedArgs)...)
		}

		cmd := exec.Command(s.Interpreter, args...)
		cmd.Dir = absCwd

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			fmt.Printf("[%s] ❌ Failed to create stdout pipe: %v\n", s.Name, err)
			time.Sleep(2 * time.Second)
			continue
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			fmt.Printf("[%s] ❌ Failed to create stderr pipe: %v\n", s.Name, err)
			time.Sleep(2 * time.Second)
			continue
		}

		if err := cmd.Start(); err != nil {
			fmt.Printf("[%s] ❌ Failed to start: %v, retrying in 5s...\n", s.Name, err)
			time.Sleep(5 * time.Second)
			continue
		}

		activeCmdMu.Lock()
		activeCommands[s.Name] = cmd
		activeCmdMu.Unlock()

		go logReader(s.Name, stdout)
		go logReader(s.Name, stderr)

		err = cmd.Wait()
		
		activeCmdMu.Lock()
		delete(activeCommands, s.Name)
		activeCmdMu.Unlock()

		if !shouldRestart {
			fmt.Printf("[%s] 🛑 Service stopped.\n", s.Name)
			return
		}

		fmt.Printf("[%s] ⚠️ Process exited (err: %v), auto-restarting in 2s...\n", s.Name, err)
		time.Sleep(2 * time.Second)
	}
}

func stopAllActiveServices() {
	shouldRestart = false
	fmt.Println("\nTerminating all child processes...")
	
	activeCmdMu.Lock()
	for name, cmd := range activeCommands {
		if cmd.Process != nil {
			fmt.Printf("Terminating service: %s (PID: %d)...\n", name, cmd.Process.Pid)
			cmd.Process.Signal(syscall.SIGTERM)
		}
	}
	activeCmdMu.Unlock()
	
	time.Sleep(1 * time.Second)
	
	activeCmdMu.Lock()
	for name, cmd := range activeCommands {
		if cmd.Process != nil {
			cmd.Process.Kill()
			fmt.Printf("Forcefully terminated service: %s\n", name)
		}
	}
	activeCmdMu.Unlock()
}

func startDaemon() {
	if oldPidBytes, err := ioutil.ReadFile(pidFile); err == nil {
		oldPid, _ := strconv.Atoi(strings.TrimSpace(string(oldPidBytes)))
		if oldPid > 0 {
			process, err := os.FindProcess(oldPid)
			if err == nil {
				if err := process.Signal(syscall.Signal(0)); err == nil {
					fmt.Printf("Error: one4all is already running (PID: %d), do not start duplicate processes.\n", oldPid)
					os.Exit(1)
				}
			}
		}
	}

	currentPid := os.Getpid()
	err := ioutil.WriteFile(pidFile, []byte(strconv.Itoa(currentPid)), 0644)
	if err != nil {
		fmt.Printf("Error: cannot write PID file %s: %v\n", pidFile, err)
		os.Exit(1)
	}
	defer os.Remove(pidFile)

	fmt.Printf("=== One for All Supervisor Started (PID: %d) ===\n", currentPid)

	config, err := loadConfig()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	for _, s := range config.Services {
		go monitorService(s)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	for {
		sig := <-sigChan
		switch sig {
		case syscall.SIGHUP:
			fmt.Println("\nReceived SIGHUP, reloading configuration and restarting child services...")
			shouldRestart = false
			stopAllActiveServices()
			
			config, err = loadConfig()
			if err != nil {
				fmt.Printf("Failed to reload configuration: %v\n", err)
				os.Exit(1)
			}
			
			shouldRestart = true
			for _, s := range config.Services {
				go monitorService(s)
			}
			fmt.Println("All child services restarted successfully.")
		case syscall.SIGINT, syscall.SIGTERM:
			fmt.Printf("\nReceived shutdown signal (%v), shutting down...\n", sig)
			stopAllActiveServices()
			return
		}
	}
}

func stopDaemon() {
	oldPidBytes, err := ioutil.ReadFile(pidFile)
	if err != nil {
		fmt.Println("one4all is not currently running.")
		return
	}

	pid, _ := strconv.Atoi(strings.TrimSpace(string(oldPidBytes)))
	if pid <= 0 {
		fmt.Println("Invalid PID file.")
		os.Remove(pidFile)
		return
	}

	fmt.Printf("Notifying one4all main process (PID: %d) to gracefully shut down...\n", pid)
	process, err := os.FindProcess(pid)
	if err != nil {
		fmt.Printf("Process PID %d not found.\n", pid)
		os.Remove(pidFile)
		return
	}

	err = process.Signal(syscall.SIGTERM)
	if err != nil {
		fmt.Printf("Failed to send shutdown signal to process: %v, clearing PID file directly.\n", err)
		os.Remove(pidFile)
		return
	}

	for i := 0; i < 5; i++ {
		time.Sleep(500 * time.Millisecond)
		if err := process.Signal(syscall.Signal(0)); err != nil {
			fmt.Println("one4all service stopped successfully.")
			return
		}
	}
	fmt.Println("Main process is still running, please verify manually.")
}

func extractHost(urlStr string) string {
	temp := urlStr
	if strings.HasPrefix(temp, "https://") {
		temp = strings.TrimPrefix(temp, "https://")
	} else if strings.HasPrefix(temp, "http://") {
		temp = strings.TrimPrefix(temp, "http://")
	}
	if idx := strings.Index(temp, "/"); idx != -1 {
		temp = temp[:idx]
	}
	if idx := strings.Index(temp, ":"); idx != -1 {
		temp = temp[:idx]
	}
	return temp
}

func reloadNginx() {
	fmt.Println("\n=== Testing and Reloading Nginx ===")
	config, err := loadConfig()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	nginxCmd, err := findExecutable("nginx")
	if err != nil {
		fmt.Printf("Warning: %v, skipping Nginx reload. Please reload Nginx manually.\n", err)
		return
	}

	var targetNginxConf string
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat("/opt/homebrew/etc/nginx/servers"); err == nil {
			targetNginxConf = "/opt/homebrew/etc/nginx/servers/one4all.conf"
		} else {
			targetNginxConf = "/usr/local/etc/nginx/servers/one4all.conf"
		}
	} else {
		targetNginxConf = "/etc/nginx/sites-available/one4all"
	}

	// Group services by external_port to generate ServerConfig
	serverGroups := make(map[int][]LocationConfig)
	for _, s := range config.Services {
		if s.Proxy == nil {
			continue // No proxy configuration, skip
		}
		
		extPort := s.Proxy.ExternalPort
		locs := serverGroups[extPort]
		
		// Parse proxy target URL, Host header, and SSL configurations
		var backendURL string
		var hostHeader string
		var enableSSLName bool
		
		if s.Proxy.ProxyPass != "" {
			backendURL = s.Proxy.ProxyPass
			hostHeader = extractHost(s.Proxy.ProxyPass)
			if strings.HasPrefix(s.Proxy.ProxyPass, "https://") {
				enableSSLName = true
			}
		} else {
			backendURL = fmt.Sprintf("http://127.0.0.1:%d/", s.Port)
			hostHeader = "$host"
		}
		
		if s.Proxy.Type == "direct" {
			locs = append(locs, LocationConfig{
				Path:          "/",
				BackendURL:    backendURL,
				HostHeader:    hostHeader,
				EnableSSLName: enableSSLName,
			})
		} else if s.Proxy.Type == "path" {
			// Add primary path
			locs = append(locs, LocationConfig{
				Path:          s.Proxy.Path,
				BackendURL:    backendURL,
				HostHeader:    hostHeader,
				EnableSSLName: enableSSLName,
			})
			// Add extra route mapping
			for _, ep := range s.Proxy.ExtraPaths {
				var epBackendURL string
				if s.Proxy.ProxyPass != "" {
					epBackendURL = strings.TrimSuffix(s.Proxy.ProxyPass, "/") + "/" + strings.TrimPrefix(ep.BackendPath, "/")
				} else {
					epBackendURL = fmt.Sprintf("http://127.0.0.1:%d%s", s.Port, ep.BackendPath)
				}
				locs = append(locs, LocationConfig{
					Path:          ep.Path,
					BackendURL:    epBackendURL,
					HostHeader:    hostHeader,
					EnableSSLName: enableSSLName,
				})
			}
		}
		serverGroups[extPort] = locs
	}

	// Sort ServerConfig to ensure deterministic generation
	var sortedPorts []int
	for port := range serverGroups {
		sortedPorts = append(sortedPorts, port)
	}
	sort.Ints(sortedPorts)

	var servers []ServerConfig
	for _, port := range sortedPorts {
		servers = append(servers, ServerConfig{
			Port:      port,
			Locations: serverGroups[port],
		})
	}

	fmt.Printf("Dynamically generating Nginx configuration using Go template (Writing to %s)...\n", targetNginxConf)
	
	// Dynamically parse and execute template
	tmpl, err := template.New("nginx").Parse(nginxTemplate)
	if err != nil {
		fmt.Printf("Error: syntax error in Nginx template: %v\n", err)
		return
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, servers); err != nil {
		fmt.Printf("Error: template execution failed: %v\n", err)
		return
	}

	// Write Nginx config
	err = ioutil.WriteFile(targetNginxConf, buf.Bytes(), 0644)
	if err != nil {
		fmt.Printf("Error: failed to write Nginx configuration file!\nDetails: %v\n", err)
		if runtime.GOOS != "darwin" {
			fmt.Println("Hint: Please try running with administrative privileges (e.g., sudo ./one4all reload).")
		}
		return
	}

	fmt.Println("Testing Nginx configuration syntax...")
	var testCmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		testCmd = exec.Command(nginxCmd, "-t")
	} else {
		testCmd = exec.Command("sudo", nginxCmd, "-t")
	}
	testCmd.Stdout = os.Stdout
	testCmd.Stderr = os.Stderr
	if err := testCmd.Run(); err != nil {
		fmt.Println("Error: Nginx configuration syntax test failed!")
		return
	}
	fmt.Println("Nginx configuration syntax is correct.")

	fmt.Println("Reloading Nginx service...")
	var reloadCmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		reloadCmd = exec.Command(nginxCmd, "-s", "reload")
	} else {
		if _, err := exec.LookPath("systemctl"); err == nil {
			reloadCmd = exec.Command("sudo", "systemctl", "reload", "nginx")
		} else {
			reloadCmd = exec.Command("sudo", "service", "nginx", "reload")
		}
	}
	reloadCmd.Stdout = os.Stdout
	reloadCmd.Stderr = os.Stderr
	if err := reloadCmd.Run(); err == nil {
		fmt.Println("Nginx reloaded successfully!")
	} else {
		fmt.Printf("Error: Nginx reload failed: %v\n", err)
	}
}

func showStatus() {
	oldPidBytes, err := ioutil.ReadFile(pidFile)
	if err != nil {
		fmt.Println("Status: one4all is not currently running.")
		return
	}

	pid, _ := strconv.Atoi(strings.TrimSpace(string(oldPidBytes)))
	process, err := os.FindProcess(pid)
	if err == nil && process.Signal(syscall.Signal(0)) == nil {
		fmt.Printf("Status: one4all is running in the background (PID: %d)\n", pid)
		
		config, err := loadConfig()
		if err == nil {
			fmt.Println("\nManaged services list:")
			for _, s := range config.Services {
				proxyInfo := "No external proxy"
				if s.Proxy != nil {
					if s.Proxy.Type == "direct" {
						proxyInfo = fmt.Sprintf("Direct proxy Port: %d ➜ %d", s.Proxy.ExternalPort, s.Port)
					} else {
						proxyInfo = fmt.Sprintf("Path proxy Port: %d ➜ %s (➜ %d)", s.Proxy.ExternalPort, s.Proxy.Path, s.Port)
					}
				}
				fmt.Printf(" - %s (Port: %d, Cwd: %s, %s)\n", s.Name, s.Port, s.Cwd, proxyInfo)
			}
		}
	} else {
		fmt.Println("Status: one4all is not currently running (Stale PID file cleared).")
		os.Remove(pidFile)
	}
}

func notifyReload() {
	reloadNginx()

	oldPidBytes, err := ioutil.ReadFile(pidFile)
	if err != nil {
		fmt.Println("Hint: one4all main process is not running. Nginx configuration reloaded.")
		return
	}

	pid, _ := strconv.Atoi(strings.TrimSpace(string(oldPidBytes)))
	process, err := os.FindProcess(pid)
	if err == nil && process.Signal(syscall.Signal(0)) == nil {
		fmt.Printf("Sending SIGHUP to one4all main process (PID: %d) to reload child services...\n", pid)
		process.Signal(syscall.SIGHUP)
	}
}

func showUsage() {
	fmt.Printf("Usage: %s {run|start|stop|reload|status}\n", os.Args[0])
	fmt.Println("  run     : Start supervisor in foreground and monitor all child services (prints logs)")
	fmt.Println("  start   : Start supervisor in background and monitor all child services")
	fmt.Println("  stop    : Stop supervisor and all child services")
	fmt.Println("  reload  : Regenerate and reload Nginx configuration, notify supervisor to restart all child services")
	fmt.Println("  status  : View supervisor status and managed services")
}

func main() {
	if len(os.Args) < 2 {
		showUsage()
		os.Exit(1)
	}

	action := os.Args[1]
	switch action {
	case "run":
		startDaemon()
	case "start":
		exePath, _ := os.Executable()
		cmd := exec.Command(exePath, "run")
		logFile, err := os.OpenFile(filepath.Join(scriptDir, "one4all_daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			fmt.Printf("Failed to create log file: %v\n", err)
			os.Exit(1)
		}
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		
		err = cmd.Start()
		if err != nil {
			fmt.Printf("Background start failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("one4all service started in background (PID: %d). Logs are written to one4all_daemon.log.\n", cmd.Process.Pid)
	case "stop":
		stopDaemon()
	case "reload":
		notifyReload()
	case "status":
		showStatus()
	default:
		showUsage()
		os.Exit(1)
	}
}
