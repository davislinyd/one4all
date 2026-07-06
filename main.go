package main

import (
	"bytes"
	bufio "bufio"
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

// 💡 透過 go:embed 將 nginx.tmpl 範本編譯進二進位檔
//go:embed nginx.tmpl
var nginxTemplate string

type ExtraPath struct {
	Path        string `json:"path"`
	BackendPath string `json:"backend_path"`
}

type Proxy struct {
	Type         string      `json:"type"`          // "direct" 或 "path"
	ExternalPort int         `json:"external_port"` // Nginx 對外暴露的 Port
	Path         string      `json:"path"`          // /video2gif/ 等分流路由
	ExtraPaths   []ExtraPath `json:"extra_paths"`   // 額外的 Path 對應
}

type Service struct {
	Name        string `json:"name"`
	Port        int    `json:"port"`
	Script      string `json:"script"`
	Cwd         string `json:"cwd"`
	Interpreter string `json:"interpreter"`
	Args        string `json:"args"`
	Proxy       *Proxy `json:"proxy"` // 支援為 nil 的代理選項
}

type Config struct {
	Services []Service `json:"services"`
}

// 模板渲染所使用的資料結構
type LocationConfig struct {
	Path        string
	BackendPort int
	BackendPath string
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
		return nil, fmt.Errorf("無法讀取 %s: %w", configPath, err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("解析 one4all.json 失敗: %w", err)
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
	return "", fmt.Errorf("找不到執行檔: %s", name)
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
	absCwd := s.Cwd
	if strings.HasPrefix(s.Cwd, ".") {
		absCwd = filepath.Join(scriptDir, s.Cwd)
	}

	for {
		if !shouldRestart {
			return
		}

		fmt.Printf("[%s] 🚀 正在啟動服務 (在 %s)...\n", s.Name, absCwd)
		if _, err := os.Stat(absCwd); os.IsNotExist(err) {
			fmt.Printf("[%s] ❌ 錯誤: 目錄 %s 不存在，5 秒後重試...\n", s.Name, absCwd)
			time.Sleep(5 * time.Second)
			continue
		}

		args := []string{s.Script}
		if s.Args != "" {
			// 動態將 {{.Port}} 替換為實際 Port 值，避免設定檔中資訊重複
			replacedArgs := strings.ReplaceAll(s.Args, "{{.Port}}", strconv.Itoa(s.Port))
			args = append(args, strings.Fields(replacedArgs)...)
		}

		cmd := exec.Command(s.Interpreter, args...)
		cmd.Dir = absCwd

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			fmt.Printf("[%s] ❌ 無法建立 stdout 管道: %v\n", s.Name, err)
			time.Sleep(2 * time.Second)
			continue
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			fmt.Printf("[%s] ❌ 無法建立 stderr 管道: %v\n", s.Name, err)
			time.Sleep(2 * time.Second)
			continue
		}

		if err := cmd.Start(); err != nil {
			fmt.Printf("[%s] ❌ 啟動失敗: %v，5 秒後重試...\n", s.Name, err)
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
			fmt.Printf("[%s] 🛑 服務已停止。\n", s.Name)
			return
		}

		fmt.Printf("[%s] ⚠️ 進程已退出 (錯誤: %v)，2 秒後將自動重啟...\n", s.Name, err)
		time.Sleep(2 * time.Second)
	}
}

func stopAllActiveServices() {
	shouldRestart = false
	fmt.Println("\n正在終止所有子服務進程...")
	
	activeCmdMu.Lock()
	for name, cmd := range activeCommands {
		if cmd.Process != nil {
			fmt.Printf("正在終止服務: %s (PID: %d)...\n", name, cmd.Process.Pid)
			cmd.Process.Signal(syscall.SIGTERM)
		}
	}
	activeCmdMu.Unlock()
	
	time.Sleep(1 * time.Second)
	
	activeCmdMu.Lock()
	for name, cmd := range activeCommands {
		if cmd.Process != nil {
			cmd.Process.Kill()
			fmt.Printf("已強制結束服務: %s\n", name)
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
					fmt.Printf("錯誤: one4all 已在運行中 (PID: %d)，請勿重複啟動。\n", oldPid)
					os.Exit(1)
				}
			}
		}
	}

	currentPid := os.Getpid()
	err := ioutil.WriteFile(pidFile, []byte(strconv.Itoa(currentPid)), 0644)
	if err != nil {
		fmt.Printf("錯誤: 無法寫入 PID 檔案 %s: %v\n", pidFile, err)
		os.Exit(1)
	}
	defer os.Remove(pidFile)

	fmt.Printf("=== One for All 進程守護器已啟動 (PID: %d) ===\n", currentPid)

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
			fmt.Println("\n收到 SIGHUP 信號，正在重新讀取設定檔並重啟子服務...")
			shouldRestart = false
			stopAllActiveServices()
			
			config, err = loadConfig()
			if err != nil {
				fmt.Printf("重載設定失敗: %v\n", err)
				os.Exit(1)
			}
			
			shouldRestart = true
			for _, s := range config.Services {
				go monitorService(s)
			}
			fmt.Println("所有子服務已重新啟動完成。")
		case syscall.SIGINT, syscall.SIGTERM:
			fmt.Printf("\n收到終止信號 (%v)，正在關閉...\n", sig)
			stopAllActiveServices()
			return
		}
	}
}

func stopDaemon() {
	oldPidBytes, err := ioutil.ReadFile(pidFile)
	if err != nil {
		fmt.Println("one4all 目前未在運行中。")
		return
	}

	pid, _ := strconv.Atoi(strings.TrimSpace(string(oldPidBytes)))
	if pid <= 0 {
		fmt.Println("無效的 PID 檔案。")
		os.Remove(pidFile)
		return
	}

	fmt.Printf("正在通知 one4all 主進程 (PID: %d) 優雅關閉...\n", pid)
	process, err := os.FindProcess(pid)
	if err != nil {
		fmt.Printf("找不到進程 PID %d。\n", pid)
		os.Remove(pidFile)
		return
	}

	err = process.Signal(syscall.SIGTERM)
	if err != nil {
		fmt.Printf("無法向進程發送關閉訊號: %v，直接清除 PID 檔案。\n", err)
		os.Remove(pidFile)
		return
	}

	for i := 0; i < 5; i++ {
		time.Sleep(500 * time.Millisecond)
		if err := process.Signal(syscall.Signal(0)); err != nil {
			fmt.Println("one4all 服務已成功停止。")
			return
		}
	}
	fmt.Println("主進程仍在運行，請手動確認。")
}

func reloadNginx() {
	fmt.Println("\n=== 正在測試與重載 Nginx ===")
	config, err := loadConfig()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	nginxCmd, err := findExecutable("nginx")
	if err != nil {
		fmt.Printf("警告: %v，跳過 Nginx 重載。請手動重載 Nginx。\n", err)
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

	// 💡 依據 Nginx 對外 Port 將服務進行分組，生成 ServerConfig
	serverGroups := make(map[int][]LocationConfig)
	for _, s := range config.Services {
		if s.Proxy == nil {
			continue // 沒有配置代理路由，跳過
		}
		
		extPort := s.Proxy.ExternalPort
		locs := serverGroups[extPort]
		
		if s.Proxy.Type == "direct" {
			locs = append(locs, LocationConfig{
				Path:        "/",
				BackendPort: s.Port,
				BackendPath: "/",
			})
		} else if s.Proxy.Type == "path" {
			// 加入主 path
			locs = append(locs, LocationConfig{
				Path:        s.Proxy.Path,
				BackendPort: s.Port,
				BackendPath: "/",
			})
			// 加入額外的 extra_paths 對應
			for _, ep := range s.Proxy.ExtraPaths {
				locs = append(locs, LocationConfig{
					Path:        ep.Path,
					BackendPort: s.Port,
					BackendPath: ep.BackendPath,
				})
			}
		}
		serverGroups[extPort] = locs
	}

	// 排序 ServerConfig 以保證生成的文件格式穩定
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

	fmt.Printf("正在使用 Go Template 動態生成 Nginx 設定檔 (寫入至 %s)...\n", targetNginxConf)
	
	// 動態解析與執行模板
	tmpl, err := template.New("nginx").Parse(nginxTemplate)
	if err != nil {
		fmt.Printf("錯誤: 語法錯誤的 Nginx 模板: %v\n", err)
		return
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, servers); err != nil {
		fmt.Printf("錯誤: 模板渲染失敗: %v\n", err)
		return
	}

	// 寫入目的地
	err = ioutil.WriteFile(targetNginxConf, buf.Bytes(), 0644)
	if err != nil {
		fmt.Printf("錯誤: 無法寫入 Nginx 設定檔！\n詳細錯誤: %v\n", err)
		if runtime.GOOS != "darwin" {
			fmt.Println("提示: 請嘗試以管理員權限執行 (例如: sudo ./one4all reload)。")
		}
		return
	}

	fmt.Println("測試 Nginx 設定檔語法...")
	var testCmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		testCmd = exec.Command(nginxCmd, "-t")
	} else {
		testCmd = exec.Command("sudo", nginxCmd, "-t")
	}
	testCmd.Stdout = os.Stdout
	testCmd.Stderr = os.Stderr
	if err := testCmd.Run(); err != nil {
		fmt.Println("錯誤: Nginx 設定語法測試失敗！")
		return
	}
	fmt.Println("Nginx 設定語法正確。")

	fmt.Println("正在重載 Nginx 服務...")
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
		fmt.Println("Nginx 重載成功！")
	} else {
		fmt.Printf("錯誤: Nginx 重載失敗: %v\n", err)
	}
}

func showStatus() {
	oldPidBytes, err := ioutil.ReadFile(pidFile)
	if err != nil {
		fmt.Println("Status: one4all 目前未在運行。")
		return
	}

	pid, _ := strconv.Atoi(strings.TrimSpace(string(oldPidBytes)))
	process, err := os.FindProcess(pid)
	if err == nil && process.Signal(syscall.Signal(0)) == nil {
		fmt.Printf("Status: one4all 正在背景運行中 (PID: %d)\n", pid)
		
		config, err := loadConfig()
		if err == nil {
			fmt.Println("\n所管理的服務清單:")
			for _, s := range config.Services {
				proxyInfo := "無對外代理"
				if s.Proxy != nil {
					if s.Proxy.Type == "direct" {
						proxyInfo = fmt.Sprintf("直連代理 Port: %d ➜ %d", s.Proxy.ExternalPort, s.Port)
					} else {
						proxyInfo = fmt.Sprintf("分流代理 Port: %d ➜ %s (➜ %d)", s.Proxy.ExternalPort, s.Proxy.Path, s.Port)
					}
				}
				fmt.Printf(" - %s (Port: %d, Cwd: %s, %s)\n", s.Name, s.Port, s.Cwd, proxyInfo)
			}
		}
	} else {
		fmt.Println("Status: one4all 目前未在運行 (殘留無效的 PID 檔案已清除)。")
		os.Remove(pidFile)
	}
}

func notifyReload() {
	reloadNginx()

	oldPidBytes, err := ioutil.ReadFile(pidFile)
	if err != nil {
		fmt.Println("提示: one4all 主進程未在運行中，已完成 Nginx 設定重載。")
		return
	}

	pid, _ := strconv.Atoi(strings.TrimSpace(string(oldPidBytes)))
	process, err := os.FindProcess(pid)
	if err == nil && process.Signal(syscall.Signal(0)) == nil {
		fmt.Printf("正在發送 SIGHUP 信號給 one4all 主進程 (PID: %d) 重載子服務...\n", pid)
		process.Signal(syscall.SIGHUP)
	}
}

func showUsage() {
	fmt.Printf("使用方式: %s {run|start|stop|reload|status}\n", os.Args[0])
	fmt.Println("  run     : 在前台啟動守護進程並管理所有子服務 (印出日誌)")
	fmt.Println("  start   : 在背景啟動守護進程並管理所有子服務")
	fmt.Println("  stop    : 停止守護進程與所有子服務")
	fmt.Println("  reload  : 重新產生並載入 Nginx 設定，並通知守護進程重啟所有子服務")
	fmt.Println("  status  : 查看守護進程與所管理的子服務狀態")
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
			fmt.Printf("無法建立日誌檔: %v\n", err)
			os.Exit(1)
		}
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		
		err = cmd.Start()
		if err != nil {
			fmt.Printf("背景啟動失敗: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("one4all 服務已在背景啟動 (PID: %d)，日誌寫入至 one4all_daemon.log。\n", cmd.Process.Pid)
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
