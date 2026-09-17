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

// ---------- 数据文件读写 ----------

// ensureDataFile 确保数据目录与文件存在；缺失时创建空数组（与原版行为一致）
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

func (a *app) loadFirms() ([]Firm, error) {
	data, err := os.ReadFile(a.dataFile)
	if err != nil {
		if os.IsNotExist(err) {
			return []Firm{}, nil
		}
		return nil, err
	}
	var firms []Firm
	if err := json.Unmarshal(data, &firms); err != nil {
		return nil, fmt.Errorf("firms.json 解析失败: %w", err)
	}
	if firms == nil {
		firms = []Firm{}
	}
	return firms, nil
}

// saveFirms 原子写入（临时文件+重命名），避免写一半损坏数据
func (a *app) saveFirms(firms []Firm) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	data, err := json.MarshalIndent(firms, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := a.dataFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.dataFile)
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
	return db, nil
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

	rows, err := a.db.Query("SELECT id, company, date, amount, received, contact, content, paid, created_at FROM orders"+where+" ORDER BY id DESC", args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	defer rows.Close()

	orders := []Order{}
	total, unpaid := 0, 0
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.Company, &o.Date, &o.Amount, &o.Received, &o.Contact, &o.Content, &o.Paid, &o.CreatedAt); err != nil {
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

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           withLogging(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("用车账单生成器已启动: http://0.0.0.0:%s  (数据文件: %s)", port, dataFile)
	log.Fatal(srv.ListenAndServe())
}
