// 用车账单生成器（Go 版）
// 由原 PHP 版（index.php / admin.php）重写，功能与数据结构完全一致。
// 数据文件格式与原版 config/firms.json 完全相同，可直接迁移。
package main

import (
	"archive/zip"
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite（无 CGO，alpine 可编译）
)

// 页面模板。从 templates 目录运行时加载（不编译进二进制），
// 好处：改页面样式不用重新编译，改完重启容器即可。
type pageTemplates struct {
	index string // 首页（静态 HTML，动态注入公司列表）
	login *template.Template
	admin *template.Template
}

func loadTemplates(dir string) (*pageTemplates, error) {
	indexRaw, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		return nil, fmt.Errorf("加载 index.html 失败: %w", err)
	}
	loginRaw, err := os.ReadFile(filepath.Join(dir, "login.html"))
	if err != nil {
		return nil, fmt.Errorf("加载 login.html 失败: %w", err)
	}
	adminRaw, err := os.ReadFile(filepath.Join(dir, "admin.html"))
	if err != nil {
		return nil, fmt.Errorf("加载 admin.html 失败: %w", err)
	}
	login, err := template.New("login").Parse(string(loginRaw))
	if err != nil {
		return nil, err
	}
	admin, err := template.New("admin").Parse(string(adminRaw))
	if err != nil {
		return nil, err
	}
	return &pageTemplates{index: string(indexRaw), login: login, admin: admin}, nil
}

// ---------- 数据结构（与 firms.json 完全一致） ----------

type Transfer struct {
	NormalAirport float64 `json:"normal_airport"`
	NormalStation float64 `json:"normal_station"`
	VanAirport    float64 `json:"van_airport"`
	VanStation    float64 `json:"van_station"`
	SignFee       float64 `json:"sign_fee"`
	NightFee      float64 `json:"night_fee"`
}

type Package struct {
	Normal4H     float64 `json:"normal_4h"`
	Normal8H     float64 `json:"normal_8h"`
	NormalOverH  float64 `json:"normal_over_h"`
	NormalOverKm float64 `json:"normal_over_km"`
	Van4H        float64 `json:"van_4h"`
	Van8H        float64 `json:"van_8h"`
	VanOverH     float64 `json:"van_over_h"`
	VanOverKm    float64 `json:"van_over_km"`
}

type Point struct {
	NormalRate float64 `json:"normal_rate"`
	NormalMin  float64 `json:"normal_min"`
	VanRate    float64 `json:"van_rate"`
	VanMin     float64 `json:"van_min"`
}

type Firm struct {
	Name     string   `json:"name"`
	Transfer Transfer `json:"transfer"`
	Package  Package  `json:"package"`
	Point    Point    `json:"point"`
}

// ---------- 应用状态 ----------

const (
	cookieName    = "admin_session"
	sessionTTL    = 12 * time.Hour // 登录态有效期
	defaultListen = "8080"
)

type app struct {
	adminPassword string
	dataFile      string
	ordersDB      string
	secret        []byte // 会话签名密钥
	tmpl          *pageTemplates
	mu            sync.Mutex
	db            *sql.DB // 订单数据库（SQLite）
}

// ---------- 费率数据（v2.0.0 起存 SQLite，firms.json 仅作为首次启动迁移源） ----------

// ensureDataFile 确保数据目录与迁移源文件存在（缺失时创建空数组）
func ensureDataFile(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, []byte("[]\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// migrateFirmsIntoDB 首次启动时把 firms.json 导入 SQLite（费率以数据库为准，之后 json 不再读写）
func migrateFirmsIntoDB(db *sql.DB, jsonPath string) error {
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM firms").Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil // 数据库已有费率，不覆盖
	}
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 没有 json 源，保持空费率
		}
		return err
	}
	var firms []Firm
	if err := json.Unmarshal(data, &firms); err != nil {
		return fmt.Errorf("firms.json 解析失败: %w", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, f := range firms {
		b, err := json.Marshal(f)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO firms (name, data) VALUES (?, ?)", f.Name, string(b)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *app) loadFirms() ([]Firm, error) {
	rows, err := a.db.Query("SELECT name, data FROM firms ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	firms := []Firm{}
	for rows.Next() {
		var name, data string
		if err := rows.Scan(&name, &data); err != nil {
			continue
		}
		var f Firm
		f.Name = name
		if err := json.Unmarshal([]byte(data), &f); err != nil {
			continue
		}
		firms = append(firms, f)
	}
	return firms, nil
}

// saveFirms 事务写入 SQLite（DELETE + 批量 INSERT）
func (a *app) saveFirms(firms []Firm) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM firms"); err != nil {
		return err
	}
	for _, f := range firms {
		b, err := json.Marshal(f)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO firms (name, data) VALUES (?, ?)", f.Name, string(b)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------- 订单记账（SQLite 数据库） ----------

// Order 一条记账订单（公司取自计费模板，即前台选中的合作公司）
type Order struct {
	ID        int64  `json:"id"`
	Company   string `json:"company"`
	Date      string `json:"date"`
	Amount    string `json:"amount"`    // 总金额（优先从结果文本解析）
	Received  string `json:"received"`  // 实收金额（实际收到的钱）
	Contact   string `json:"contact"`
	Content   string `json:"content"`
	Paid      int    `json:"paid"`
	MemoID    string `json:"memo_id"` // 已推送到 Memos 的笔记标识（空=未推送）
	CreatedAt string `json:"created_at"`
}

// ensureColumn 检查表中是否存在某列，不存在则 ALTER TABLE 补上（兼容旧版本数据库）
func ensureColumn(db *sql.DB, table, col, ddl string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt interface{}
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			continue
		}
		if name == col {
			found = true
			break
		}
	}
	if found {
		return nil
	}
	_, err = db.Exec("ALTER TABLE " + table + " ADD COLUMN " + ddl)
	return err
}

// openOrdersDB 打开/创建订单数据库并建表
func openOrdersDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite 单写者，避免并发锁冲突
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS orders (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		company TEXT NOT NULL DEFAULT '',
		date TEXT NOT NULL DEFAULT '',
		amount TEXT NOT NULL DEFAULT '',
		received TEXT NOT NULL DEFAULT '',
		contact TEXT NOT NULL DEFAULT '',
		content TEXT NOT NULL DEFAULT '',
		paid INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		db.Close()
		return nil, err
	}
	// 旧版本升级：补齐 amount / received 列（VPS 上已有订单数据的库自动加列，不丢数据）
	if err := ensureColumn(db, "orders", "amount", "amount TEXT NOT NULL DEFAULT ''"); err != nil {
		db.Close()
		return nil, err
	}
	if err := ensureColumn(db, "orders", "received", "received TEXT NOT NULL DEFAULT ''"); err != nil {
		db.Close()
		return nil, err
	}
	// Memos 同步：已推送的笔记 ID（0=未推送）
	if err := ensureColumn(db, "orders", "memo_id", "memo_id TEXT NOT NULL DEFAULT ''"); err != nil {
		db.Close()
		return nil, err
	}
	// 历史记录表（前台输入历史，存数据库而非浏览器 localStorage，换设备/清缓存不丢）
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL DEFAULT '',
		value TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		db.Close()
		return nil, err
	}
	// 费率表（v2.0.0 起费率也存数据库，备份一个 .db 文件即整套系统）
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS firms (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL DEFAULT '',
		data TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		db.Close()
		return nil, err
	}
	// 系统设置表（key-value）：Memos 地址 / 账号 / Token 等
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// ---------- 前台历史记录（存 SQLite，每类最多 10 条） ----------

// histKinds 历史记录分类（与 index.html 输入项一致）
var histKinds = map[string]bool{"contact": true, "customer": true, "flight": true, "route": true, "extra_name": true}

// histLimit 每类历史保留条数（其他费用明目只用 5 条，其余 10 条）
func histLimit(kind string) int {
	if kind == "extra_name" {
		return 4 // 保留 4 条 + 新插入 1 条 = 5 条
	}
	return 9 // 保留 9 条 + 新插入 1 条 = 10 条
}

// handleHistorySave 保存一条历史（前台免登录）：去重后保留最新 N 条
func (a *app) handleHistorySave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"ok": false, "error": "POST only"})
		return
	}
	var d struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	if err := readJSON(r, &d); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "请求格式错误"})
		return
	}
	d.Kind = strings.TrimSpace(d.Kind)
	d.Value = strings.TrimSpace(d.Value)
	if !histKinds[d.Kind] || d.Value == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "无效的历史记录"})
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	defer tx.Rollback()
	// 同值去重（保留最新一条）
	if _, err := tx.Exec("DELETE FROM history WHERE kind = ? AND value = ?", d.Kind, d.Value); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	// 只保留最新 N-1 条，再加新的这条 = N 条
	if _, err := tx.Exec(`DELETE FROM history WHERE kind = ? AND id NOT IN
		(SELECT id FROM history WHERE kind = ? ORDER BY id DESC LIMIT ?)`, d.Kind, d.Kind, histLimit(d.Kind)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	if _, err := tx.Exec("INSERT INTO history (kind, value, created_at) VALUES (?, ?, ?)",
		d.Kind, d.Value, time.Now().Format("2006-01-02 15:04:05")); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// handleHistoryList 返回全部分类历史（前台免登录），每类最多 10 条、新的在前
func (a *app) handleHistoryList(w http.ResponseWriter, r *http.Request) {
	hist := map[string][]string{}
	for kind := range histKinds {
		rows, err := a.db.Query("SELECT value FROM history WHERE kind = ? ORDER BY id DESC LIMIT 10", kind)
		if err != nil {
			continue
		}
		vals := []string{}
		for rows.Next() {
			var v string
			if rows.Scan(&v) == nil {
				vals = append(vals, v)
			}
		}
		rows.Close()
		hist[kind] = vals
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "history": hist})
}

// ---------- 数据备份 / 恢复（后台，需登录） ----------

// sqliteMagic SQLite 文件头魔数，用于校验上传的备份文件
var sqliteMagic = []byte("SQLite format 3\x00")

// handleBackupDownload 用 VACUUM INTO 生成一致性快照并下载（文件名带日期时间）
func (a *app) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	dir := filepath.Dir(a.ordersDB)
	snap := filepath.Join(dir, "orders-backup-snapshot.db")
	_ = os.Remove(snap)
	// VACUUM INTO 在连接打开时也能生成一致性快照（路径是程序内部拼接，无注入风险）
	sql := "VACUUM INTO '" + strings.ReplaceAll(snap, "'", "''") + "'"
	if _, err := a.db.Exec(sql); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": "生成备份失败: " + err.Error()})
		return
	}
	defer os.Remove(snap)
	data, err := os.ReadFile(snap)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": "读取备份失败: " + err.Error()})
		return
	}
	stamp := time.Now().Format("2006-01-02_150405")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="orders_`+stamp+`.db"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// handleBackupRestore 上传 .db 备份文件覆盖恢复（自动迁移补列；失败自动回滚）
func (a *app) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"ok": false, "error": "POST only"})
		return
	}
	if err := r.ParseMultipartForm(128 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "文件过大或格式错误"})
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "请选择要上传的 .db 备份文件"})
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 128<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "读取上传文件失败"})
		return
	}
	// 校验是 SQLite 数据库文件
	if len(data) < 16 || !bytes.Equal(data[:16], sqliteMagic) {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "不是有效的 SQLite 数据库文件（.db）"})
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	dir := filepath.Dir(a.ordersDB)
	restoreTmp := filepath.Join(dir, "orders-restore-tmp.db")
	oldFile := a.ordersDB
	bakFile := oldFile + ".bak"

	if err := os.WriteFile(restoreTmp, data, 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": "写入临时文件失败: " + err.Error()})
		return
	}
	defer os.Remove(restoreTmp)

	// 关闭旧连接，旧库改名备份（保险，失败可回滚）
	if a.db != nil {
		_ = a.db.Close()
		a.db = nil
	}
	hasOld := false
	if _, err := os.Stat(oldFile); err == nil {
		hasOld = true
		_ = os.Remove(bakFile)
		if err := os.Rename(oldFile, bakFile); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": "备份旧数据失败: " + err.Error()})
			return
		}
	}
	// 装入新库
	if err := os.Rename(restoreTmp, oldFile); err != nil {
		if hasOld {
			_ = os.Rename(bakFile, oldFile)
		}
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": "写入备份失败: " + err.Error()})
		return
	}
	// 重新打开（自动建表 / 补列迁移）
	db, err := openOrdersDB(oldFile)
	if err != nil {
		_ = os.Remove(oldFile)
		if hasOld {
			_ = os.Rename(bakFile, oldFile)
		}
		if db2, err2 := openOrdersDB(oldFile); err2 == nil {
			a.db = db2
		}
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": "备份文件无法打开: " + err.Error()})
		return
	}
	a.db = db
	if hasOld {
		_ = os.Remove(bakFile) // 恢复成功，清理 .bak
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "note": "恢复成功"})
}

// ---------- 会话（HMAC 签名 Cookie，HttpOnly + SameSite=Lax） ----------

func (a *app) issueSession(w http.ResponseWriter) {
	exp := time.Now().Add(sessionTTL).Unix()
	rb := make([]byte, 16)
	_, _ = rand.Read(rb)
	payload := fmt.Sprintf("%d.%s", exp, hex.EncodeToString(rb))
	sig := a.sign(payload)
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    payload + "." + sig,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func (a *app) sign(payload string) string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func (a *app) isAuthed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 {
		return false
	}
	payload, sig := parts[0]+"."+parts[1], parts[2]
	if !hmac.Equal([]byte(sig), []byte(a.sign(payload))) {
		return false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return true
}

func clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: -1,
	})
}

// 常量时间比较，避免时序攻击
func secureEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}

// ---------- 页面渲染 ----------

// 首页：账单生成器（HTML 基本静态，动态注入公司列表与 JSON）
func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	firms, err := a.loadFirms()
	if err != nil {
		http.Error(w, "数据文件读取失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	firmsJSON, _ := json.Marshal(firms)
	var opts strings.Builder
	for i, f := range firms {
		fmt.Fprintf(&opts, `<option value="%d">%s</option>`, i, html.EscapeString(f.Name))
	}
	page := strings.Replace(a.tmpl.index, "__FIRMS_JSON__", string(firmsJSON), 1)
	page = strings.Replace(page, "<!--__FIRM_OPTIONS__-->", opts.String(), 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, page)
}

// 后台：登录页 / 管理页 / POST 处理
func (a *app) handleAdmin(w http.ResponseWriter, r *http.Request) {
	// ---- POST ----
	if r.Method == http.MethodPost {
		if !a.isAuthed(r) {
			// 未登录 → 当作登录尝试
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if secureEqual(r.FormValue("pwd"), a.adminPassword) {
				a.issueSession(w)
				http.Redirect(w, r, "/admin", http.StatusSeeOther)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = a.tmpl.login.Execute(w, map[string]string{"Error": "密码错误"})
			return
		}
		// 已登录 → 处理增删改
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		action := r.FormValue("action")
		firms, err := a.loadFirms()
		if err != nil {
			http.Error(w, "数据文件读取失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
		switch action {
		case "add":
			firms = append(firms, parseFirm(r))
		case "edit":
			idx, _ := strconv.Atoi(r.FormValue("idx"))
			if idx >= 0 && idx < len(firms) {
				firms[idx] = parseFirm(r)
			}
		case "del":
			idx, _ := strconv.Atoi(r.FormValue("idx"))
			if idx >= 0 && idx < len(firms) {
				firms = append(firms[:idx], firms[idx+1:]...)
			}
		}
		if err := a.saveFirms(firms); err != nil {
			http.Error(w, "保存失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}

	// ---- GET ----
	if !a.isAuthed(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = a.tmpl.login.Execute(w, map[string]string{"Error": ""})
		return
	}
	firms, err := a.loadFirms()
	if err != nil {
		http.Error(w, "数据文件读取失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = a.tmpl.admin.Execute(w, map[string]interface{}{"Firms": firms})
}

// 从 POST 表单解析一家公司的完整费率（字段名与原版 admin.php 完全一致）
func parseFirm(r *http.Request) Firm {
	val := func(key string) float64 {
		v, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue(key)), 64)
		return v
	}
	return Firm{
		Name: strings.TrimSpace(r.FormValue("name")),
		Transfer: Transfer{
			NormalAirport: val("tr_normal_airport"),
			NormalStation: val("tr_normal_station"),
			VanAirport:    val("tr_van_airport"),
			VanStation:    val("tr_van_station"),
			SignFee:       val("tr_sign"),
			NightFee:      val("tr_night"),
		},
		Package: Package{
			Normal4H:     val("pk_n4"),
			Normal8H:     val("pk_n8"),
			NormalOverH:  val("pk_noh"),
			NormalOverKm: val("pk_nokm"),
			Van4H:        val("pk_v4"),
			Van8H:        val("pk_v8"),
			VanOverH:     val("pk_voh"),
			VanOverKm:    val("pk_vokm"),
		},
		Point: Point{
			NormalRate: val("pt_nr"),
			NormalMin:  val("pt_nmin"),
			VanRate:    val("pt_vr"),
			VanMin:     val("pt_vmin"),
		},
	}
}

func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	clearSession(w)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// ---------- 订单 API（JSON） ----------

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// requireAuth 后台接口鉴权：未登录返回 401
func (a *app) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.isAuthed(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]interface{}{"ok": false, "error": "未登录"})
			return
		}
		next(w, r)
	}
}

// ---------- 系统设置（key-value）+ Memos 推送 ----------

func (a *app) getSetting(key string) string {
	var v string
	a.db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	return v
}
func (a *app) setSetting(key, value string) error {
	_, err := a.db.Exec("INSERT OR REPLACE INTO settings(key, value) VALUES(?, ?)", key, value)
	return err
}

type memosConfig struct {
	URL   string
	Token string
}

func (a *app) memosConfig() memosConfig {
	return memosConfig{
		URL:   strings.TrimRight(a.getSetting("memos_url"), "/"),
		Token: strings.TrimSpace(a.getSetting("memos_token")),
	}
}

func sanitizeTag(s string) string {
	s = strings.TrimSpace(s)
	r := strings.NewReplacer(" ", "", "#", "", "/", "-", "（", "", "）", "", "(", "", ")", "")
	return r.Replace(s)
}

// buildMemoContent 构建推送到 Memos 的文本（标签 + 正文；已收款头尾包 ~~删除线~~）
func buildMemoContent(o *Order) string {
	tag := "#用车"
	if o.Company != "" {
		tag += " #" + sanitizeTag(o.Company)
	}
	if o.Date != "" {
		tag += " #" + sanitizeTag(o.Date)
	}
	if o.Paid == 1 {
		return tag + "\n~~\n" + o.Content + "\n~~"
	}
	return tag + "\n" + o.Content
}

// memosCreate 创建一条 memo，返回 memo name（新版 Memos 是 memos/xxx，旧版是数字 id）
func memosCreate(cfg memosConfig, content string) (string, error) {
	if cfg.URL == "" || cfg.Token == "" {
		return "", nil
	}
	body, _ := json.Marshal(map[string]interface{}{
		"content":    content,
		"visibility": "PRIVATE",
	})
	req, err := http.NewRequest("POST", cfg.URL+"/api/v1/memos", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println("[memos] 创建请求失败:", err)
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	fmt.Printf("[memos] 创建响应 status=%d body=%s\n", resp.StatusCode, string(respBody))
	var raw map[string]interface{}
	json.Unmarshal(respBody, &raw)
	// 新版 Memos: {"name":"memos/5qbZ6DhJocxBbk4SE85LQB"}
	// 旧版 Memos: {"id":123}
	var memoRef string
	if v, ok := raw["name"].(string); ok && v != "" {
		memoRef = v
	} else if v, ok := raw["id"].(float64); ok && v > 0 {
		memoRef = fmt.Sprintf("%d", int64(v))
	}
	if memoRef == "" {
		return "", fmt.Errorf("memos 创建失败 (status=%d)", resp.StatusCode)
	}
	return memoRef, nil
}

// memosUpdate 更新已有 memo 的内容
func memosUpdate(cfg memosConfig, memoRef string, content string) error {
	if cfg.URL == "" || cfg.Token == "" || memoRef == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]interface{}{"content": content})
	// memoRef 可能是 "memos/xxx" 或纯数字 id
	url := fmt.Sprintf("%s/api/v1/memos/%s", cfg.URL, memoRef)
	req, err := http.NewRequest("PATCH", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println("[memos] 更新请求失败:", err)
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	fmt.Printf("[memos] 更新响应 status=%d body=%s\n", resp.StatusCode, string(rb))
	return nil
}

// handleMemosConfig 读取/保存 Memos 连接配置
func (a *app) handleMemosConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":    true,
			"url":   a.getSetting("memos_url"),
			"token": a.getSetting("memos_token"),
		})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"ok": false, "error": "GET/POST only"})
		return
	}
	var d struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := readJSON(r, &d); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "请求格式错误"})
		return
	}
	a.setSetting("memos_url", strings.TrimSpace(d.URL))
	a.setSetting("memos_token", strings.TrimSpace(d.Token))
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// 保存订单（前台调用，无需登录）
func (a *app) handleOrderSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"ok": false, "error": "POST only"})
		return
	}
	var d struct {
		Company  string `json:"company"`
		Date     string `json:"date"`
		Amount   string `json:"amount"`
		Received string `json:"received"`
		Contact  string `json:"contact"`
		Content  string `json:"content"`
	}
	if err := readJSON(r, &d); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "请求格式错误"})
		return
	}
	d.Company = strings.TrimSpace(d.Company)
	d.Content = strings.TrimSpace(d.Content)
	if d.Content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "账单内容不能为空"})
		return
	}
	res, err := a.db.Exec(`INSERT INTO orders (company, date, amount, received, contact, content, paid, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?)`,
		d.Company, strings.TrimSpace(d.Date), strings.TrimSpace(d.Amount), strings.TrimSpace(d.Received), strings.TrimSpace(d.Contact), d.Content, time.Now().Format("2006-01-02 15:04:05"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": "保存失败: " + err.Error()})
		return
	}
	id, _ := res.LastInsertId()

	// 自动推送到 Memos（配置了才推），成功后把 memo 标识存库
	cfg := a.memosConfig()
	if cfg.URL != "" && cfg.Token != "" {
		o := &Order{Company: d.Company, Date: strings.TrimSpace(d.Date), Content: d.Content, Paid: 0}
		if memoRef, err := memosCreate(cfg, buildMemoContent(o)); err == nil && memoRef != "" {
			a.db.Exec("UPDATE orders SET memo_id = ? WHERE id = ?", memoRef, id)
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "id": id})
}

// 订单列表（后台，登录后调用），支持 company/paid/date_from/date_to 筛选
func (a *app) handleOrderList(w http.ResponseWriter, r *http.Request) {
	conds := []string{}
	args := []interface{}{}
	if v := r.URL.Query().Get("company"); v != "" {
		conds = append(conds, "company = ?")
		args = append(args, v)
	}
	if v := r.URL.Query().Get("paid"); v != "" {
		conds = append(conds, "paid = ?")
		args = append(args, v)
	}
	if v := r.URL.Query().Get("date_from"); v != "" {
		conds = append(conds, "date >= ?")
		args = append(args, v)
	}
	if v := r.URL.Query().Get("date_to"); v != "" {
		conds = append(conds, "date <= ?")
		args = append(args, v)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	rows, err := a.db.Query("SELECT id, company, date, amount, received, contact, content, paid, memo_id, created_at FROM orders"+where+" ORDER BY id DESC", args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	defer rows.Close()

	orders := []Order{}
	total, unpaid := 0, 0
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.Company, &o.Date, &o.Amount, &o.Received, &o.Contact, &o.Content, &o.Paid, &o.MemoID, &o.CreatedAt); err != nil {
			continue
		}
		orders = append(orders, o)
		total++
		if o.Paid == 0 {
			unpaid++
		}
	}

	// 公司列表（供筛选下拉）
	compRows, err := a.db.Query(`SELECT DISTINCT company FROM orders WHERE company != '' ORDER BY company DESC`)
	companies := []string{}
	if err == nil {
		defer compRows.Close()
		for compRows.Next() {
			var c string
			if compRows.Scan(&c) == nil && c != "" {
				companies = append(companies, c)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":        true,
		"total":     total,
		"unpaid":    unpaid,
		"paid":      total - unpaid,
		"companies": companies,
		"orders":    orders,
	})
}

// 切换收钱状态（后台，二次确认由前端完成）
func (a *app) handleOrderToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"ok": false, "error": "POST only"})
		return
	}
	var d struct {
		ID int64 `json:"id"`
	}
	if err := readJSON(r, &d); err != nil || d.ID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "缺少订单ID"})
		return
	}
	var paid int
	if err := a.db.QueryRow("SELECT paid FROM orders WHERE id = ?", d.ID).Scan(&paid); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{"ok": false, "error": "订单不存在"})
		return
	}
	newPaid := 0
	if paid == 0 {
		newPaid = 1
	}
	if _, err := a.db.Exec("UPDATE orders SET paid = ? WHERE id = ?", newPaid, d.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	// 同步 Memos：已收款加删除线，未收款去掉删除线
	var memoRef, company, date, content string
	a.db.QueryRow("SELECT memo_id, company, date, content FROM orders WHERE id = ?", d.ID).
		Scan(&memoRef, &company, &date, &content)
	if memoRef != "" {
		cfg := a.memosConfig()
		if cfg.URL != "" && cfg.Token != "" {
			o := &Order{Company: company, Date: date, Content: content, Paid: newPaid}
			memosUpdate(cfg, memoRef, buildMemoContent(o))
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "id": d.ID, "paid": newPaid})
}

// 删除订单（后台）
func (a *app) handleOrderDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"ok": false, "error": "POST only"})
		return
	}
	var d struct {
		ID int64 `json:"id"`
	}
	if err := readJSON(r, &d); err != nil || d.ID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "缺少订单ID"})
		return
	}
	if _, err := a.db.Exec("DELETE FROM orders WHERE id = ?", d.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "id": d.ID})
}

// ---------- 导出：xlsx（WPS/Excel） / csv ----------

func excelCol(n int) string {
	col := ""
	for n > 0 {
		n--
		col = string(rune('A'+n%26)) + col
		n /= 26
	}
	return col
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	s = r.Replace(s)
	s = strings.ReplaceAll(s, "\r\n", "&#10;")
	s = strings.ReplaceAll(s, "\r", "&#10;")
	s = strings.ReplaceAll(s, "\n", "&#10;")
	return s
}

// buildXlsx 生成标准 .xlsx（内联字符串 + 自动换行），与 WPS/Excel 兼容
func buildXlsx(orders []Order) ([]byte, error) {
	head := []string{"公司", "日期", "总金额(元)", "实收(元)", "对接人", "是否已收钱", "账单内容", "创建时间"}
	allRows := [][]string{head}
	for _, o := range orders {
		paid := "未收钱"
		if o.Paid == 1 {
			paid = "已收钱"
		}
		allRows = append(allRows, []string{o.Company, o.Date, o.Amount, o.Received, o.Contact, paid, o.Content, o.CreatedAt})
	}

	var sheet strings.Builder
	sheet.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	sheet.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	sheet.WriteString(`<cols><col min="1" max="1" width="24" customWidth="1"/><col min="2" max="2" width="14" customWidth="1"/><col min="3" max="3" width="12" customWidth="1"/><col min="4" max="4" width="12" customWidth="1"/><col min="5" max="5" width="14" customWidth="1"/><col min="6" max="6" width="12" customWidth="1"/><col min="7" max="7" width="70" customWidth="1"/><col min="8" max="8" width="20" customWidth="1"/></cols>`)
	sheet.WriteString(`<sheetData>`)
	for i, row := range allRows {
		r := i + 1
		sheet.WriteString(`<row r="` + strconv.Itoa(r) + `">`)
		for j, v := range row {
			col := excelCol(j + 1)
			sheet.WriteString(`<c r="` + col + strconv.Itoa(r) + `" t="inlineStr" s="1"><is><t>` + xmlEscape(v) + `</t></is></c>`)
		}
		sheet.WriteString(`</row>`)
	}
	sheet.WriteString(`</sheetData></worksheet>`)

	files := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
			`<Default Extension="xml" ContentType="application/xml"/>` +
			`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
			`<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>` +
			`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>` +
			`</Types>`,
		"_rels/.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>` +
			`</Relationships>`,
		"xl/workbook.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
			`<sheets><sheet name="记账本" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>` +
			`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>` +
			`</Relationships>`,
		"xl/styles.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
			`<fonts count="1"><font><sz val="11"/><name val="Calibri"/></font></fonts>` +
			`<fills count="1"><fill><patternFill patternType="none"/></fill></fills>` +
			`<borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders>` +
			`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>` +
			`<cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles>` +
			`<cellXfs count="2">` +
			`<xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/>` +
			`<xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0" applyAlignment="1"><alignment vertical="top" wrapText="1"/></xf>` +
			`</cellXfs></styleSheet>`,
		"xl/worksheets/sheet1.xml": sheet.String(),
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := zw.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := f.Write([]byte(content)); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (a *app) handleOrderExport(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "xlsx"
	}

	conds := []string{}
	args := []interface{}{}
	if v := r.URL.Query().Get("company"); v != "" {
		conds = append(conds, "company = ?")
		args = append(args, v)
	}
	if v := r.URL.Query().Get("paid"); v != "" {
		conds = append(conds, "paid = ?")
		args = append(args, v)
	}
	if v := r.URL.Query().Get("date_from"); v != "" {
		conds = append(conds, "date >= ?")
		args = append(args, v)
	}
	if v := r.URL.Query().Get("date_to"); v != "" {
		conds = append(conds, "date <= ?")
		args = append(args, v)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	rows, err := a.db.Query("SELECT id, company, date, amount, received, contact, content, paid, created_at FROM orders"+where+" ORDER BY id DESC", args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	defer rows.Close()
	orders := []Order{}
	for rows.Next() {
		var o Order
		if rows.Scan(&o.ID, &o.Company, &o.Date, &o.Amount, &o.Received, &o.Contact, &o.Content, &o.Paid, &o.CreatedAt) == nil {
			orders = append(orders, o)
		}
	}

	stamp := time.Now().Format("20060102_150405")

	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="orders_`+stamp+`.csv"`)
		// UTF-8 BOM，保证 WPS/Excel 中文不乱码
		w.Write([]byte{0xEF, 0xBB, 0xBF})
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"公司", "日期", "总金额(元)", "实收(元)", "对接人", "是否已收钱", "账单内容", "创建时间"})
		for _, o := range orders {
			paid := "未收钱"
			if o.Paid == 1 {
				paid = "已收钱"
			}
			_ = cw.Write([]string{o.Company, o.Date, o.Amount, o.Received, o.Contact, paid, o.Content, o.CreatedAt})
		}
		cw.Flush()
		return
	}

	// 默认导出 .xlsx
	data, err := buildXlsx(orders)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": "导出失败: " + err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="orders_`+stamp+`.xlsx"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok")
}

// ---------- 日志中间件 ----------

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (rec *statusRecorder) WriteHeader(code int) {
	rec.status = code
	rec.ResponseWriter.WriteHeader(code)
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, rec.status, time.Since(start))
	})
}

// ---------- 入口 ----------

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultListen
	}
	adminPassword := os.Getenv("ADMIN_PASSWORD")
	if adminPassword == "" {
		adminPassword = "123456" // 与原版默认一致，强烈建议部署时修改
	}
	dataFile := os.Getenv("FIRMS_DATA")
	if dataFile == "" {
		dataFile = filepath.Join("data", "firms.json")
	}
	ordersDB := os.Getenv("ORDERS_DB")
	if ordersDB == "" {
		ordersDB = filepath.Join("data", "orders.db")
	}
	templatesDir := os.Getenv("TEMPLATES_DIR")
	if templatesDir == "" {
		templatesDir = "templates"
	}

	if err := ensureDataFile(dataFile); err != nil {
		log.Fatalf("初始化数据文件失败: %v", err)
	}
	tmpl, err := loadTemplates(templatesDir)
	if err != nil {
		log.Fatalf("加载页面模板失败: %v", err)
	}

	orderDB, err := openOrdersDB(ordersDB)
	if err != nil {
		log.Fatalf("初始化订单数据库失败: %v", err)
	}
	defer orderDB.Close()

	// 首次启动：把旧的 firms.json 费率迁移进 SQLite（之后以数据库为准）
	if err := migrateFirmsIntoDB(orderDB, dataFile); err != nil {
		log.Fatalf("迁移费率数据失败: %v", err)
	}

	secret := []byte(os.Getenv("SESSION_SECRET"))
	if len(secret) == 0 {
		// 未配置时每次启动随机生成，重启后旧登录态失效（安全）
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			log.Fatalf("生成会话密钥失败: %v", err)
		}
		log.Println("提示: 未设置 SESSION_SECRET，重启后需重新登录后台")
	}

	a := &app{
		adminPassword: adminPassword,
		dataFile:      dataFile,
		ordersDB:      ordersDB,
		secret:        secret,
		tmpl:          tmpl,
		db:            orderDB,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/admin", a.handleAdmin)
	mux.HandleFunc("/logout", a.handleLogout)
	mux.HandleFunc("/healthz", handleHealth)
	// 订单记账 API
	mux.HandleFunc("/api/order/save", a.handleOrderSave) // 前台保存，无需登录
	mux.HandleFunc("/api/order/list", a.requireAuth(a.handleOrderList))
	mux.HandleFunc("/api/order/toggle", a.requireAuth(a.handleOrderToggle))
	mux.HandleFunc("/api/order/delete", a.requireAuth(a.handleOrderDelete))
	mux.HandleFunc("/api/order/export", a.requireAuth(a.handleOrderExport))
	mux.HandleFunc("/api/memos/config", a.requireAuth(a.handleMemosConfig))
	// 前台历史记录（存数据库）
	mux.HandleFunc("/api/history/save", a.handleHistorySave) // 前台保存，无需登录
	mux.HandleFunc("/api/history/list", a.handleHistoryList) // 前台读取，无需登录
	// 数据备份 / 恢复（后台）
	mux.HandleFunc("/api/backup/download", a.requireAuth(a.handleBackupDownload))
	mux.HandleFunc("/api/backup/restore", a.requireAuth(a.handleBackupRestore))

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           withLogging(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("用车账单生成器已启动: http://0.0.0.0:%s  (数据文件: %s)", port, dataFile)
	log.Fatal(srv.ListenAndServe())
}
