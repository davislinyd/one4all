# One for All - Nginx Reverse Proxy & PM2 Gateway

此專案為多個 Python/SSE/WebSocket MCP (Model Context Protocol) 服務的本地開發與部署閘道器 (Gateway)。
它統一對外提供單一的 Port 通訊，並利用 Nginx 進行路由分流，將不同 Path 的流量轉發至內部運行於不同 Port 的 Python 服務。

---

## 1. 支援的傳輸協定

* **HTTP / SSE (Server-Sent Events)** ── 對外統一由 Nginx 代理，底層配置了停用緩衝 (`proxy_buffering off`) 與大超時設定，保證 AI 模型上下文流式傳輸不受影響。
* **WebSocket** ── 透過 Nginx `map` 配置，支持在相同路由下動態將協議升級為 WebSocket，提供即時雙向通信。
* **傳統 HTTP REST API** (例如 `POST /convert`) ── 統一代理至相應的後端 Python 服務。

---

## 2. 專案架構與設定

本專案主要由以下四個核心檔案組成：

1. **[nginx.conf](file:///Users/lindav/git/one4all/nginx.conf)**：
   * Nginx 反向代理設定範本。已最佳化 SSE 與 WebSocket 的連線特性。
2. **[one4all.json](file:///Users/lindav/git/one4all/one4all.json)**：
   * JSON 格式的設定檔。
   * **`nginx.port`**：您可以在此欄位**自由決定 Nginx 對外的 Port**（例如 `9002`、`9005` 等）。
   * **`services`**：定義內部 Python 服務的名稱、各自運行的 Port、工作目錄與啟動參數。
3. **[one4all](file:///Users/lindav/git/one4all/one4all)**：
   * 用於本地開發控制的 CLI 腳本，包裝了 PM2。
   * 每次執行 `reload` 時，**腳本會自動讀取 `one4all.json` 裡的 `nginx.port`，動態替換並寫入 Nginx 設定檔**，不需手動修改 Nginx 配置。
4. **[.gitignore](file:///Users/lindav/git/one4all/.gitignore)**：
   * Git 忽略設定檔，排除 local 設定、PM2 暫存檔與無關的日誌。

---

## 3. Nginx 部署與設定

### 建議的設定放置路徑
* **macOS (Homebrew)**:
  - Apple Silicon: `/opt/homebrew/etc/nginx/servers/one4all.conf`
  - Intel CPU: `/usr/local/etc/nginx/servers/one4all.conf`
* **Linux (Ubuntu/Debian)**:
  - `/etc/nginx/sites-available/one4all` (並軟連結至 `sites-enabled/`)

### 常用 Nginx 指令
* **測試設定檔語法**：
  - macOS: `nginx -t`
  - Linux: `sudo nginx -t`
* **重載設定（不中斷連線）**：
  - macOS: `nginx -s reload`
  - Linux: `sudo systemctl reload nginx`

---

## 4. 進程管理器 (PM2) 與 CLI 操作

專案採用 **PM2** 來管理多個 Python 服務進程，以實現崩潰自動重啟、狀態監控與日誌收集。

### 安裝 PM2
```bash
npm install -g pm2
```

### CLI 常用指令
* **一鍵啟動所有服務**：
  ```bash
  ./one4all start
  ```
* **查看服務運行狀態**：
  ```bash
  ./one4all status
  ```
* **一鍵重載服務與 Nginx** (會自動套用 `one4all.json` 中的新 Nginx Port)：
  ```bash
  ./one4all reload
  ```
* **一鍵停止所有服務**：
  ```bash
  ./one4all stop
  ```
