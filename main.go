package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// 💡 透過 go:embed 將 nginx.conf 範本直接編譯進二進位檔
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

// 取得與目前二進位檔同目錄的路徑
var scriptDir string

func init() {
	exePath, err := os.Executable()
	if err != nil {
		scriptDir = "."
	} else {
		scriptDir = filepath.Dir(exePath)
	}
}

// 尋找執行檔，並動態追加常用路徑
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

func manageServices(action string) {
	pm2Path, err := findExecutable("pm2")
	if err != nil {
		fmt.Printf("錯誤: %v。請先安裝 Node.js 與 PM2。\n", err)
		os.Exit(1)
	}

	config, err := loadConfig()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	fmt.Printf("\n=== 執行後端服務操作: %s ===\n", action)

	for _, s := range config.Services {
		absCwd := s.Cwd
		if strings.HasPrefix(s.Cwd, ".") {
			absCwd = filepath.Join(scriptDir, s.Cwd)
		}

		if action == "start" {
			fmt.Printf("正在啟動服務: %s (在 %s)...\n", s.Name, absCwd)
			if _, err := os.Stat(absCwd); os.IsNotExist(err) {
				fmt.Printf(" 警告: 目錄 %s 不存在，跳過。\n", absCwd)
				continue
			}

			// 組裝 PM2 參數
			args := []string{"start", s.Script, "--name", s.Name, "--interpreter", s.Interpreter}
			if s.Args != "" {
				args = append(args, "--")
				args = append(args, strings.Fields(s.Args)...)
			}

			cmd := exec.Command(pm2Path, args...)
			cmd.Dir = absCwd
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.Run()

		} else if action == "stop" {
			fmt.Printf("正在停止服務: %s...\n", s.Name)
			cmd := exec.Command(pm2Path, "stop", s.Name)
			cmd.Run()

		} else if action == "restart" {
			fmt.Printf("正在重啟服務: %s...\n", s.Name)
			// 檢查是否在 PM2 中運行
			checkCmd := exec.Command(pm2Path, "describe", s.Name)
			if err := checkCmd.Run(); err == nil {
				// 已存在，重啟
				cmd := exec.Command(pm2Path, "restart", s.Name)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				cmd.Run()
			} else {
				// 不存在，直接啟動
				if _, err := os.Stat(absCwd); os.IsNotExist(err) {
					fmt.Printf(" 警告: 目錄 %s 不存在，跳過。\n", absCwd)
					continue
				}
				args := []string{"start", s.Script, "--name", s.Name, "--interpreter", s.Interpreter}
				if s.Args != "" {
					args = append(args, "--")
					args = append(args, strings.Fields(s.Args)...)
				}
				cmd := exec.Command(pm2Path, args...)
				cmd.Dir = absCwd
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				cmd.Run()
			}
		}
	}
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

	// 決定目標設定檔寫入路徑
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

	fmt.Printf("正在將 Nginx 對外 Port 設定為 %d (寫入至 %s)...\n", config.Nginx.Port, targetNginxConf)

	// 正則替換 listen 埠
	re := regexp.MustCompile(`listen\s+\d+;`)
	updatedConf := re.ReplaceAllString(nginxTemplate, fmt.Sprintf("listen %d;", config.Nginx.Port))

	// 寫入檔案
	err = ioutil.WriteFile(targetNginxConf, []byte(updatedConf), 0644)
	if err != nil {
		fmt.Printf("錯誤: 無法寫入 Nginx 設定檔！\n詳細錯誤: %v\n", err)
		if runtime.GOOS != "darwin" {
			fmt.Println("提示: 請嘗試以管理員權限執行 (例如: sudo ./one4all reload)。")
		}
		return
	}

	// 測試 Nginx 設定檔
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

	// 重載 Nginx 服務
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

func showUsage() {
	fmt.Printf("使用方式: %s {start|stop|restart|reload|status}\n", os.Args[0])
	fmt.Println("  start   : 啟動 one4all.json 中定義的所有後端服務")
	fmt.Println("  stop    : 停止所有後端服務")
	fmt.Println("  restart : 重啟所有後端服務")
	fmt.Println("  reload  : 重啟所有後端服務並重載 Nginx")
	fmt.Println("  status  : 查看所有後端服務運行狀態 (PM2 list)")
}

func main() {
	if len(os.Args) < 2 {
		showUsage()
		os.Exit(1)
	}

	action := os.Args[1]
	switch action {
	case "start":
		manageServices("start")
	case "stop":
		manageServices("stop")
	case "restart":
		manageServices("restart")
	case "reload":
		manageServices("restart")
		reloadNginx()
	case "status":
		pm2Path, err := findExecutable("pm2")
		if err != nil {
			fmt.Printf("錯誤: %v\n", err)
			os.Exit(1)
		}
		cmd := exec.Command(pm2Path, "list")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Run()
	default:
		showUsage()
		os.Exit(1)
	}
}
