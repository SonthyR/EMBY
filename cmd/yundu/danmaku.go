package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	danmakuSchemaSQL = `
CREATE TABLE IF NOT EXISTS danmaku_sources (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	upstream_url TEXT NOT NULL,
	egress_node_id INTEGER NOT NULL DEFAULT 0,
	enabled INTEGER NOT NULL DEFAULT 1,
	sort_order INTEGER NOT NULL DEFAULT 0,
	note TEXT NOT NULL DEFAULT '',
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS share_user_danmaku_sources (
	share_user_id INTEGER NOT NULL REFERENCES share_users(id) ON DELETE CASCADE,
	danmaku_source_id INTEGER NOT NULL REFERENCES danmaku_sources(id) ON DELETE CASCADE,
	PRIMARY KEY(share_user_id, danmaku_source_id)
);
CREATE INDEX IF NOT EXISTS idx_share_user_danmaku_source ON share_user_danmaku_sources(danmaku_source_id);
`
	danmakuProxyTimeout    = 20 * time.Second
	danmakuTestTimeout     = 10 * time.Second
	danmakuMaxReqBodyBytes = 2 << 20
)

var danmakuMaxRespBodyBytes int64 = 32 << 20

type DanmakuSource struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	UpstreamURL       string `json:"upstream_url"`
	EgressNodeID      int64  `json:"egress_node_id"`
	EgressName        string `json:"egress_name,omitempty"`
	EgressOnline      bool   `json:"egress_online"`
	Enabled           bool   `json:"enabled"`
	SortOrder         int    `json:"sort_order"`
	Note              string `json:"note"`
	AssignedUserCount int    `json:"assigned_user_count"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
}

type DanmakuSourceInput struct {
	Name         string `json:"name"`
	UpstreamURL  string `json:"upstream_url"`
	EgressNodeID int64  `json:"egress_node_id"`
	Enabled      *bool  `json:"enabled"`
	Note         string `json:"note"`
}

type DanmakuTestResult struct {
	OK         bool   `json:"ok"`
	StatusCode int    `json:"status_code"`
	LatencyMs  int64  `json:"latency_ms"`
	EgressName string `json:"egress_name"`
	Error      string `json:"error,omitempty"`
}

type shareSelfDanmakuSource struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
	Note string `json:"note,omitempty"`
}

func ensureDanmakuSchema(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, danmakuSchemaSQL)
	return err
}

func newShareDanmakuToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func normalizeDanmakuUpstreamURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("上游弹幕 API 地址不能为空")
	}
	if len(raw) > 1024 {
		return "", errors.New("上游弹幕 API 地址过长")
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || strings.TrimSpace(parsed.Host) == "" {
		return "", errors.New("上游弹幕 API 地址必须以 http:// 或 https:// 开头且包含有效主机名")
	}
	if parsed.User != nil {
		return "", errors.New("上游弹幕 API 地址不支持内嵌用户名密码")
	}
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func (d *DB) ListDanmakuSources() ([]DanmakuSource, error) {
	rows, err := d.db.Query(`SELECT id, name, upstream_url, egress_node_id, enabled, sort_order, note, COALESCE(created_at, ''), COALESCE(updated_at, '') FROM danmaku_sources ORDER BY sort_order ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DanmakuSource, 0)
	for rows.Next() {
		var item DanmakuSource
		var enabled int
		if err := rows.Scan(&item.ID, &item.Name, &item.UpstreamURL, &item.EgressNodeID, &enabled, &item.SortOrder, &item.Note, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.Enabled = enabled != 0
		out = append(out, item)
	}
	return out, rows.Err()
}

func (d *DB) ListEnabledDanmakuSources() ([]DanmakuSource, error) {
	rows, err := d.db.Query(`SELECT id, name, upstream_url, egress_node_id, enabled, sort_order, note, COALESCE(created_at, ''), COALESCE(updated_at, '') FROM danmaku_sources WHERE enabled=1 ORDER BY sort_order ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DanmakuSource, 0)
	for rows.Next() {
		var item DanmakuSource
		var enabled int
		if err := rows.Scan(&item.ID, &item.Name, &item.UpstreamURL, &item.EgressNodeID, &enabled, &item.SortOrder, &item.Note, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.Enabled = enabled != 0
		out = append(out, item)
	}
	return out, rows.Err()
}

func (d *DB) GetDanmakuSource(id int64) (*DanmakuSource, error) {
	var item DanmakuSource
	var enabled int
	err := d.db.QueryRow(`SELECT id, name, upstream_url, egress_node_id, enabled, sort_order, note, COALESCE(created_at, ''), COALESCE(updated_at, '') FROM danmaku_sources WHERE id=?`, id).
		Scan(&item.ID, &item.Name, &item.UpstreamURL, &item.EgressNodeID, &enabled, &item.SortOrder, &item.Note, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		return nil, err
	}
	item.Enabled = enabled != 0
	return &item, nil
}

func (d *DB) CreateDanmakuSource(input DanmakuSourceInput) (*DanmakuSource, error) {
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	var maxOrder int
	_ = d.db.QueryRow(`SELECT COALESCE(MAX(sort_order), 0) FROM danmaku_sources`).Scan(&maxOrder)
	res, err := d.db.Exec(
		`INSERT INTO danmaku_sources(name, upstream_url, egress_node_id, enabled, sort_order, note) VALUES(?,?,?,?,?,?)`,
		input.Name, input.UpstreamURL, input.EgressNodeID, sqliteBool(enabled), maxOrder+1, input.Note,
	)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return d.GetDanmakuSource(id)
}

func (d *DB) UpdateDanmakuSource(id int64, input DanmakuSourceInput) (*DanmakuSource, error) {
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	} else {
		var current int
		if err := d.db.QueryRow(`SELECT enabled FROM danmaku_sources WHERE id=?`, id).Scan(&current); err != nil {
			return nil, err
		}
		enabled = current != 0
	}
	res, err := d.db.Exec(
		`UPDATE danmaku_sources SET name=?, upstream_url=?, egress_node_id=?, enabled=?, note=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		input.Name, input.UpstreamURL, input.EgressNodeID, sqliteBool(enabled), input.Note, id,
	)
	if err != nil {
		return nil, err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return nil, sql.ErrNoRows
	}
	return d.GetDanmakuSource(id)
}

func (d *DB) DeleteDanmakuSource(id int64) error {
	res, err := d.db.Exec(`DELETE FROM danmaku_sources WHERE id=?`, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (d *DB) resetShareUserDanmakuToken(userID int64) (string, error) {
	token, err := newShareDanmakuToken()
	if err != nil {
		return "", err
	}
	res, err := d.db.Exec(`UPDATE share_users SET danmaku_token=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, token, userID)
	if err != nil {
		return "", err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return "", sql.ErrNoRows
	}
	return token, nil
}

func (d *DB) shareUserDanmakuSourceIDs(userID int64) ([]int64, error) {
	rows, err := d.db.Query(`SELECT a.danmaku_source_id FROM share_user_danmaku_sources a JOIN danmaku_sources s ON s.id=a.danmaku_source_id WHERE a.share_user_id=? ORDER BY s.sort_order ASC, s.id ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (d *DB) shareUserHasDanmakuSource(userID, sourceID int64) (bool, error) {
	var count int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM share_user_danmaku_sources WHERE share_user_id=? AND danmaku_source_id=?`, userID, sourceID).Scan(&count)
	return count > 0, err
}

func (d *DB) ListAssignedEnabledDanmakuSources(userID int64) ([]DanmakuSource, error) {
	rows, err := d.db.Query(`SELECT s.id, s.name, s.upstream_url, s.egress_node_id, s.enabled, s.sort_order, s.note, COALESCE(s.created_at, ''), COALESCE(s.updated_at, '') FROM danmaku_sources s JOIN share_user_danmaku_sources a ON a.danmaku_source_id=s.id WHERE a.share_user_id=? AND s.enabled=1 ORDER BY s.sort_order ASC, s.id ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DanmakuSource, 0)
	for rows.Next() {
		var item DanmakuSource
		var enabled int
		if err := rows.Scan(&item.ID, &item.Name, &item.UpstreamURL, &item.EgressNodeID, &enabled, &item.SortOrder, &item.Note, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.Enabled = enabled != 0
		out = append(out, item)
	}
	return out, rows.Err()
}

func (a *App) enrichDanmakuSource(item DanmakuSource) DanmakuSource {
	_ = a.db.db.QueryRow(`SELECT COUNT(*) FROM share_user_danmaku_sources WHERE danmaku_source_id=?`, item.ID).Scan(&item.AssignedUserCount)
	if item.EgressNodeID <= 0 {
		masterName := "主机直出"
		if a.pm != nil && a.pm.edges != nil {
			if name := strings.TrimSpace(a.pm.edges.masterDisplayName()); name != "" && name != defaultMasterNodeName {
				masterName = name + "（主机直出）"
			}
		}
		item.EgressName = masterName
		item.EgressOnline = true
		return item
	}
	if a.pm != nil && a.pm.edges != nil {
		if decision, ok := a.pm.edges.nodeDecisionByID(item.EgressNodeID); ok {
			item.EgressName = decision.NodeName
			item.EgressOnline = true
			return item
		}
		if name := a.pm.edges.nodeNameByID(item.EgressNodeID); name != "" {
			item.EgressName = name
			item.EgressOnline = false
			return item
		}
	}
	var dbName string
	if err := a.db.db.QueryRow(`SELECT name FROM edge_nodes WHERE id=?`, item.EgressNodeID).Scan(&dbName); err == nil && dbName != "" {
		item.EgressName = dbName
	} else {
		item.EgressName = "节点 #" + strconv.FormatInt(item.EgressNodeID, 10)
	}
	item.EgressOnline = false
	return item
}

func (a *App) normalizeDanmakuInput(input DanmakuSourceInput) (DanmakuSourceInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.Note = strings.TrimSpace(input.Note)
	if input.Name == "" || len([]rune(input.Name)) > 64 {
		return input, errors.New("弹幕源名称需要 1–64 个字符")
	}
	if len([]rune(input.Note)) > 200 {
		return input, errors.New("备注不能超过 200 个字符")
	}
	normalizedURL, err := normalizeDanmakuUpstreamURL(input.UpstreamURL)
	if err != nil {
		return input, err
	}
	input.UpstreamURL = normalizedURL
	if input.EgressNodeID < 0 {
		return input, errors.New("出口节点选择无效")
	}
	if input.EgressNodeID > 0 {
		var count int
		if err := a.db.db.QueryRow(`SELECT COUNT(*) FROM edge_nodes WHERE id=?`, input.EgressNodeID).Scan(&count); err != nil || count == 0 {
			return input, errors.New("所选出口节点不存在")
		}
	}
	return input, nil
}

func (a *App) buildShareUserDanmakuSources(user *storedShareUser, origin string) []shareSelfDanmakuSource {
	if user == nil || strings.TrimSpace(user.danmakuToken) == "" || strings.TrimSpace(origin) == "" {
		return nil
	}
	sources, err := a.db.ListAssignedEnabledDanmakuSources(user.ID)
	if err != nil || len(sources) == 0 {
		return nil
	}
	base := strings.TrimRight(strings.TrimSpace(origin), "/")
	out := make([]shareSelfDanmakuSource, 0, len(sources))
	for _, src := range sources {
		out = append(out, shareSelfDanmakuSource{
			ID:   src.ID,
			Name: src.Name,
			URL:  base + "/dm/" + strconv.FormatInt(src.ID, 10) + "/" + user.danmakuToken,
			Note: src.Note,
		})
	}
	return out
}

func newDanmakuHTTPTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   8 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
}

var danmakuSharedTransport = newDanmakuHTTPTransport()

func (a *App) newDanmakuClient(timeout time.Duration, useEdge bool) *http.Client {
	var rt http.RoundTripper = danmakuSharedTransport
	if useEdge {
		rt = &edgeEgressTransport{base: danmakuSharedTransport}
	}
	return &http.Client{
		Transport: rt,
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (a *App) resolveDanmakuEgress(egressNodeID int64) (edgeEgressTarget, string, bool, error) {
	if egressNodeID <= 0 {
		masterName := "主机直出"
		if a.pm != nil && a.pm.edges != nil {
			if name := strings.TrimSpace(a.pm.edges.masterDisplayName()); name != "" && name != defaultMasterNodeName {
				masterName = name + "（主机直出）"
			}
		}
		return edgeEgressTarget{}, masterName, false, nil
	}
	if a.pm == nil || a.pm.edges == nil {
		return edgeEgressTarget{}, "", true, errEdgeFullNodeUnavailable
	}
	decision, ok := a.pm.edges.nodeDecisionByID(egressNodeID)
	if !ok {
		return edgeEgressTarget{}, a.pm.edges.nodeNameByID(egressNodeID), true, errEdgeFullNodeUnavailable
	}
	return edgeEgressTarget{
		NodeID:    decision.NodeID,
		NodeName:  decision.NodeName,
		PublicURL: decision.PublicURL,
		Token:     decision.Token,
	}, decision.NodeName, true, nil
}

func (a *App) testDanmakuTarget(ctx context.Context, upstreamURL string, egressNodeID int64) DanmakuTestResult {
	target, egressName, useEdge, err := a.resolveDanmakuEgress(egressNodeID)
	if err != nil {
		if egressName == "" && egressNodeID > 0 {
			egressName = "节点 #" + strconv.FormatInt(egressNodeID, 10)
		}
		return DanmakuTestResult{
			OK:         false,
			EgressName: egressName,
			Error:      "绑定的出口节点离线或不可用（已拒绝主机直连以防 IP 泄露）",
		}
	}
	reqCtx, cancel := context.WithTimeout(ctx, danmakuTestTimeout)
	defer cancel()
	if useEdge {
		reqCtx = withEdgeEgressTarget(reqCtx, target)
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		return DanmakuTestResult{OK: false, EgressName: egressName, Error: err.Error()}
	}
	req.Header.Set("User-Agent", "Yundu-Danmaku/"+appVersion)
	req.Header.Set("Accept", "application/json, text/plain, */*")

	start := time.Now()
	resp, err := a.newDanmakuClient(danmakuTestTimeout, useEdge).Do(req) // #nosec G704 -- upstream URL is admin-configured and validated to http/https; edge-pinned sources route exclusively through authenticated edge egress.
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return DanmakuTestResult{
			OK:         false,
			LatencyMs:  latency,
			EgressName: egressName,
			Error:      err.Error(),
		}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()

	// Danmaku API roots commonly return 200, 30x, 401, or 404 when reachable.
	ok := resp.StatusCode > 0 && resp.StatusCode < 500
	res := DanmakuTestResult{
		OK:         ok,
		StatusCode: resp.StatusCode,
		LatencyMs:  latency,
		EgressName: egressName,
	}
	if !ok {
		res.Error = "上游返回服务端错误 HTTP " + strconv.Itoa(resp.StatusCode)
	}
	return res
}

// GET/POST /api/danmaku-sources
func (a *App) handleDanmakuSources(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		list, err := a.db.ListDanmakuSources()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "弹幕源读取失败")
			return
		}
		for i := range list {
			list[i] = a.enrichDanmakuSource(list[i])
		}
		a.jsonOK(w, list)
	case http.MethodPost:
		var input DanmakuSourceInput
		if err := decodeJSONBody(w, r, &input); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "invalid request")
			return
		}
		input, err := a.normalizeDanmakuInput(input)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		created, err := a.db.CreateDanmakuSource(input)
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "创建弹幕源失败")
			return
		}
		a.jsonResponse(w, http.StatusCreated, a.enrichDanmakuSource(*created))
	default:
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// /api/danmaku-sources/test and /api/danmaku-sources/{id}[/test]
func (a *App) handleDanmakuSourceByID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/danmaku-sources/"), "/")
	if rest == "test" {
		if r.Method != http.MethodPost {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			UpstreamURL  string `json:"upstream_url"`
			EgressNodeID int64  `json:"egress_node_id"`
		}
		if err := decodeJSONBody(w, r, &req); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "invalid request")
			return
		}
		normalizedURL, err := normalizeDanmakuUpstreamURL(req.UpstreamURL)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		a.jsonOK(w, a.testDanmakuTarget(r.Context(), normalizedURL, req.EgressNodeID))
		return
	}
	parts := strings.Split(rest, "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		a.jsonErr(w, http.StatusBadRequest, "invalid danmaku source id")
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	if len(parts) > 2 {
		a.jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	switch action {
	case "":
		switch r.Method {
		case http.MethodPut:
			var input DanmakuSourceInput
			if err := decodeJSONBody(w, r, &input); err != nil {
				a.jsonErr(w, http.StatusBadRequest, "invalid request")
				return
			}
			input, err := a.normalizeDanmakuInput(input)
			if err != nil {
				a.jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
			updated, err := a.db.UpdateDanmakuSource(id, input)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					a.jsonErr(w, http.StatusNotFound, "弹幕源不存在")
				} else {
					a.jsonErr(w, http.StatusInternalServerError, "保存弹幕源失败")
				}
				return
			}
			a.jsonOK(w, a.enrichDanmakuSource(*updated))
		case http.MethodDelete:
			if err := a.db.DeleteDanmakuSource(id); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					a.jsonErr(w, http.StatusNotFound, "弹幕源不存在")
				} else {
					a.jsonErr(w, http.StatusInternalServerError, "删除弹幕源失败")
				}
				return
			}
			a.jsonOK(w, map[string]bool{"deleted": true})
		default:
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "test":
		if r.Method != http.MethodPost {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		src, err := a.db.GetDanmakuSource(id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				a.jsonErr(w, http.StatusNotFound, "弹幕源不存在")
			} else {
				a.jsonErr(w, http.StatusInternalServerError, "弹幕源读取失败")
			}
			return
		}
		a.jsonOK(w, a.testDanmakuTarget(r.Context(), src.UpstreamURL, src.EgressNodeID))
	default:
		a.jsonErr(w, http.StatusNotFound, "not found")
	}
}

func setDanmakuCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, HEAD, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Accept, X-Requested-With")
}

func isDanmakuHopByHopHeader(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "proxy-connection":
		return true
	default:
		return false
	}
}

func buildDanmakuUpstreamURL(baseRaw, restPath, rawQuery string) (string, error) {
	base, err := url.Parse(baseRaw)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return "", errors.New("invalid upstream url")
	}
	u := *base
	if restPath != "" {
		u.Path = singleJoiningSlash(base.Path, restPath)
		u.RawPath = ""
	}
	switch {
	case base.RawQuery != "" && rawQuery != "":
		u.RawQuery = base.RawQuery + "&" + rawQuery
	case rawQuery != "":
		u.RawQuery = rawQuery
	}
	return u.String(), nil
}

func setDanmakuSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
}

func isAllowedDanmakuMediaType(rawContentType string) (string, bool) {
	rawContentType = strings.TrimSpace(rawContentType)
	if rawContentType == "" {
		return "", false
	}
	mediaType, _, err := mime.ParseMediaType(rawContentType)
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(strings.Split(rawContentType, ";")[0]))
	} else {
		mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	}
	switch mediaType {
	case "application/json", "application/xml", "text/xml", "text/plain":
		return rawContentType, true
	}
	if strings.HasPrefix(mediaType, "application/") && (strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml")) {
		return rawContentType, true
	}
	return "", false
}

// Public danmaku proxy: /dm/{source_id}/{danmaku_token}[/{rest...}]
func (a *App) handleDanmakuProxy(w http.ResponseWriter, r *http.Request) {
	setDanmakuSecurityHeaders(w)
	setDanmakuCORSHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	trimmed := strings.TrimPrefix(r.URL.Path, "/dm/")
	if trimmed == "" {
		a.jsonErr(w, http.StatusNotFound, "弹幕路径无效")
		return
	}
	seg1, afterSource, hasToken := strings.Cut(trimmed, "/")
	if !hasToken || seg1 == "" {
		a.jsonErr(w, http.StatusForbidden, "弹幕凭证无效")
		return
	}
	sourceID, err := strconv.ParseInt(seg1, 10, 64)
	if err != nil || sourceID <= 0 {
		a.jsonErr(w, http.StatusNotFound, "弹幕源不存在")
		return
	}
	token, tail, hasTail := strings.Cut(afterSource, "/")
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 128 {
		a.jsonErr(w, http.StatusForbidden, "弹幕凭证无效")
		return
	}
	restPath := ""
	if hasTail {
		restPath = "/" + tail
	}

	user, err := a.db.getShareUserWhere(`danmaku_token=?`, token)
	if err != nil {
		a.jsonErr(w, http.StatusForbidden, "弹幕凭证无效或已重置")
		return
	}
	view := a.shareUserView(user, false, "")
	switch view.Status {
	case "active":
		// Allowed
	case "disabled":
		a.jsonErr(w, http.StatusForbidden, "账号已停用，弹幕服务已暂停")
		return
	case "expired":
		a.jsonErr(w, http.StatusForbidden, "账号已过期，弹幕服务已暂停")
		return
	case "quota":
		a.jsonErr(w, http.StatusForbidden, "本期流量已用完，弹幕服务已暂停")
		return
	default:
		a.jsonErr(w, http.StatusForbidden, "账号不可用，弹幕服务已暂停")
		return
	}

	src, err := a.db.GetDanmakuSource(sourceID)
	if err != nil || !src.Enabled {
		a.jsonErr(w, http.StatusNotFound, "弹幕源不存在或已停用")
		return
	}
	assigned, err := a.db.shareUserHasDanmakuSource(user.ID, sourceID)
	if err != nil || !assigned {
		a.jsonErr(w, http.StatusForbidden, "该账号未分配此弹幕服务")
		return
	}

	upstreamURL, err := buildDanmakuUpstreamURL(src.UpstreamURL, restPath, r.URL.RawQuery)
	if err != nil {
		a.jsonErr(w, http.StatusBadGateway, "弹幕源地址配置无效")
		return
	}

	target, _, useEdge, err := a.resolveDanmakuEgress(src.EgressNodeID)
	if err != nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "弹幕出口节点离线或不可用")
		return
	}

	reqCtx, cancel := context.WithTimeout(r.Context(), danmakuProxyTimeout)
	defer cancel()
	if useEdge {
		reqCtx = withEdgeEgressTarget(reqCtx, target)
	}

	var body io.Reader
	if r.Body != nil && r.Method == http.MethodPost {
		body = io.LimitReader(r.Body, danmakuMaxReqBodyBytes)
	}
	outReq, err := http.NewRequestWithContext(reqCtx, r.Method, upstreamURL, body) // #nosec G704 -- scheme and host come strictly from admin-configured danmaku_sources.upstream_url; only path suffix and query are appended.
	if err != nil {
		a.jsonErr(w, http.StatusBadGateway, "构造弹幕请求失败")
		return
	}
	for _, h := range []string{"Accept", "Accept-Encoding", "Accept-Language", "Content-Type", "If-None-Match", "If-Modified-Since"} {
		if v := r.Header.Get(h); v != "" {
			outReq.Header.Set(h, v)
		}
	}
	if ua := strings.TrimSpace(r.Header.Get("User-Agent")); ua != "" {
		outReq.Header.Set("User-Agent", ua)
	} else {
		outReq.Header.Set("User-Agent", "Yundu-Danmaku/"+appVersion)
	}

	resp, err := a.newDanmakuClient(danmakuProxyTimeout, useEdge).Do(outReq) // #nosec G704 -- upstream URL is validated admin config and edge-pinned traffic flows exclusively over authenticated edge egress.
	if err != nil {
		a.jsonErr(w, http.StatusBadGateway, "弹幕上游连接失败")
		return
	}
	defer resp.Body.Close()

	maxBytes := danmakuMaxRespBodyBytes
	if maxBytes <= 0 {
		maxBytes = 32 << 20
	}
	noBodyStatus := resp.StatusCode == http.StatusNotModified || resp.StatusCode == http.StatusNoContent || (resp.StatusCode >= 100 && resp.StatusCode < 200)
	if resp.ContentLength > maxBytes {
		a.jsonErr(w, http.StatusBadGateway, "弹幕上游响应超出大小限制")
		return
	}

	rawCT := strings.TrimSpace(resp.Header.Get("Content-Type"))
	allowedCT, ctOK := isAllowedDanmakuMediaType(rawCT)
	if rawCT != "" && !ctOK {
		a.jsonErr(w, http.StatusBadGateway, "弹幕上游返回了不支持的内容类型")
		return
	}

	var bodyBytes []byte
	if r.Method != http.MethodHead && !noBodyStatus {
		limited, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
		if readErr != nil {
			a.jsonErr(w, http.StatusBadGateway, "读取弹幕上游响应失败")
			return
		}
		if int64(len(limited)) > maxBytes {
			a.jsonErr(w, http.StatusBadGateway, "弹幕上游响应超出大小限制")
			return
		}
		bodyBytes = limited
		if rawCT == "" && len(bodyBytes) > 0 {
			if strings.TrimSpace(resp.Header.Get("Content-Encoding")) != "" {
				a.jsonErr(w, http.StatusBadGateway, "弹幕上游缺少响应类型")
				return
			}
			sniffed := strings.ToLower(strings.TrimSpace(http.DetectContentType(bodyBytes)))
			if strings.HasPrefix(sniffed, "text/plain") {
				allowedCT = "text/plain; charset=utf-8"
			} else {
				a.jsonErr(w, http.StatusBadGateway, "弹幕上游返回了不支持的内容类型")
				return
			}
		}
	}

	dstHeaders := w.Header()
	// Copy only explicit allowlisted headers matching the exact payload sent to the client.
	if allowedCT != "" {
		dstHeaders.Set("Content-Type", allowedCT)
	}
	if ce := strings.TrimSpace(resp.Header.Get("Content-Encoding")); ce != "" && !resp.Uncompressed {
		dstHeaders.Set("Content-Encoding", ce)
	}
	for _, name := range []string{"Cache-Control", "ETag", "Last-Modified", "Vary"} {
		if vals := resp.Header.Values(name); len(vals) > 0 {
			for _, v := range vals {
				dstHeaders.Add(name, v)
			}
		}
	}
	if r.Method == http.MethodHead {
		if resp.ContentLength >= 0 {
			dstHeaders.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
		}
	} else if !noBodyStatus {
		dstHeaders.Set("Content-Length", strconv.Itoa(len(bodyBytes)))
	}
	setDanmakuSecurityHeaders(w)
	setDanmakuCORSHeaders(w)
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead && !noBodyStatus && len(bodyBytes) > 0 {
		_, _ = w.Write(bodyBytes) // #nosec G705 -- validated non-active danmaku payload served with CSP sandbox and nosniff.
	}
}
