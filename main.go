package main

import (
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
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// 💡 將 nginx.conf 範本直接編譯進二進位檔
//go:embed nginx.conf
var nginxTemplate string

type Service struct {
	Name        string `json:"name"`
	Port        int    `json:"port"`
	Script      string `json:"script"`
	Cwd         string `json:"cwd"`
	Interpreter string `json:"interpreter"`
	Args        string `json:"args"`
}

type Config struct {
	Nginx struct {
		Port       int    `json:"port"`
		ConfigName string `json:"config_name"`
	} `json:"nginx"`
	Services []Service `json:"services"`
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

// 讀取日誌並加上服務前綴輸出
func logReader(serviceName string, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		timeStr := time.Now().Format("2006/01/02 15:04:05")
		fmt.Printf("[%s] %s | %s\n", serviceName, timeStr, scanner.Text())
	}
}

// 保存目前運行的子進程
var activeCommands = make(map[string]*exec.Cmd)
var shouldRestart = true

// 啟動與監控單一服務
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

		// 解析啟動參數
		args := []string{s.Script}
		if s.Args != "" {
			args = append(args, strings.Fields(s.Args)...)
		}

		cmd := exec.Command(s.Interpreter, args...)
		cmd.Dir = absCwd

		// 擷取輸出
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

		// 記錄 active 進程
		activeCommands[s.Name] = cmd

		// 非同步讀取日誌
		go logReader(s.Name, stdout)
		go logReader(s.Name, stderr)

		// 等待進程退出
		err = cmd.Wait()
		delete(activeCommands, s.Name)

		if !shouldRestart {
			fmt.Printf("[%s] 🛑 服務已停止。\n", s.Name)
			return
		}

		fmt.Printf("[%s] ⚠️ 進程已退出 (錯誤: %v)，2 秒後將自動重啟...\n", s.Name, err)
		time.Sleep(2 * time.Second)
	}
}

// 停止所有正在運行的子進程
func stopAllActiveServices() {
	shouldRestart = false
	fmt.Println("\n正在終止所有子服務進程...")
	for name, cmd := range activeCommands {
		if cmd.Process != nil {
			fmt.Printf("正在終止服務: %s (PID: %d)...\n", name, cmd.Process.Pid)
			// 發送 SIGTERM
			cmd.Process.Signal(syscall.SIGTERM)
		}
	}
	// 給子進程 1 秒時間優雅退出，否則強制 SIGKILL
	time.Sleep(1 * time.Second)
	for name, cmd := range activeCommands {
		if cmd.Process != nil {
			cmd.Process.Kill()
			fmt.Printf("已強制結束服務: %s\n", name)
		}
	}
}

// 執行監控與守護 (Foreground 運行)
func startDaemon() {
	// 1. 檢查是否已有實例在運行
	if oldPidBytes, err := ioutil.ReadFile(pidFile); err == nil {
		oldPid, _ := strconv.Atoi(strings.TrimSpace(string(oldPidBytes)))
		if oldPid > 0 {
			// 檢查該進程是否真的存在 (在 Unix 上向 pid 發送 0 號信號)
			process, err := os.FindProcess(oldPid)
			if err == nil {
				if err := process.Signal(syscall.Signal(0)); err == nil {
					fmt.Printf("錯誤: one4all 已在運行中 (PID: %d)，請勿重複啟動。\n", oldPid)
					os.Exit(1)
				}
			}
		}
	}

	// 2. 寫入目前 PID
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

	// 3. 啟動所有子服務
	for _, s := range config.Services {
		go monitorService(s)
	}

	// 4. 監聽系統訊號
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	for {
		sig := <-sigChan
		switch sig {
		case syscall.SIGHUP:
			// 💡 SIGHUP 用於重載設定檔與重啟所有子服務
			fmt.Println("\n收到 SIGHUP 信號，正在重新讀取設定檔並重啟子服務...")
			shouldRestart = false
			stopAllActiveServices()
			
			// 重新載入設定
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

// 停止遠端的 one4all 守護進程
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

	// 發送 SIGTERM 訊號
	err = process.Signal(syscall.SIGTERM)
	if err != nil {
		fmt.Printf("無法向進程發送關閉訊號: %v，直接清除 PID 檔案。\n", err)
		os.Remove(pidFile)
		return
	}

	// 循環等待進程退出
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

	fmt.Printf("正在更新 Nginx 對外 Port 至 %d (寫入至 %s)...\n", config.Nginx.Port, targetNginxConf)

	re := regexp.MustCompile(`listen\s+\d+;`)
	updatedConf := re.ReplaceAllString(nginxTemplate, fmt.Sprintf("listen %d;", config.Nginx.Port))

	err = ioutil.WriteFile(targetNginxConf, []byte(updatedConf), 0644)
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

// 取得當前守護進程狀態
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
		
		// 載入設定檔印出管理的子服務
		config, err := loadConfig()
		if err == nil {
			fmt.Println("\n所管理的服務清單:")
			for _, s := range config.Services {
				fmt.Printf(" - %s (Port: %d, Cwd: %s)\n", s.Name, s.Port, s.Cwd)
			}
		}
	} else {
		fmt.Println("Status: one4all 目前未在運行 (殘留無效的 PID 檔案已清除)。")
		os.Remove(pidFile)
	}
}

// 通知主進程重載子服務
func notifyReload() {
	// 先更新與重載 Nginx
	reloadNginx()

	// 再通知 one4all 主進程重載子服務
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
		// 背景啟動的實現: 呼叫自身並在背景執行
		exePath, _ := os.Executable()
		cmd := exec.Command(exePath, "run")
		// 將 stdout/stderr 重定向到日誌檔案
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
