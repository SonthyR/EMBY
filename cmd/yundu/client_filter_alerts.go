package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	clientFilterModeOff       = "off"
	clientFilterModeBlacklist = "blacklist"
	clientFilterModeWhitelist = "whitelist"
	clientFilterMaxRules      = 200
	clientFilterMaxRuleLen    = 128

	shareAlertCheckInterval = 5 * time.Minute
)

var (
	clientFilterBlacklistPresets = []string{
		"curl", "wget", "python", "aria2", "idm", "yt-dlp",
		"rclone", "go-http-client", "httpie", "scrapy", "ffmpeg", "axel", "lftp",
	}
	clientFilterWhitelistPresets = []string{
		"infuse", "vidhub", "senplayer", "hills", "yamby", "fileball",
		"forward", "afusekt", "emby", "jellyfin", "kodi", "mpv",
		"vlc", "iina", "potplayer", "nplayer", "mxplayer", "mozilla",
	}
)

const clientFilterAndAlertSchemaSQL = `
CREATE TABLE IF NOT EXISTS client_filter_settings (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	mode TEXT NOT NULL DEFAULT 'off',
	rules_json TEXT NOT NULL DEFAULT '[]',
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
INSERT OR IGNORE INTO client_filter_settings (id, mode, rules_json) VALUES (1, 'off', '[]');

CREATE TABLE IF NOT EXISTS share_alert_settings (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	enabled INTEGER NOT NULL DEFAULT 1,
	expiry_days INTEGER NOT NULL DEFAULT 3,
	quota_warning_pct INTEGER NOT NULL DEFAULT 80,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
INSERT OR IGNORE INTO share_alert_settings (id, enabled, expiry_days, quota_warning_pct) VALUES (1, 1, 3, 80);

CREATE TABLE IF NOT EXISTS share_alert_sent (
	alert_key TEXT PRIMARY KEY,
	sent_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
`

func ensureClientFilterAndAlertSchema(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, clientFilterAndAlertSchemaSQL)
	return err
}

// ---------------------------------------------------------------------------
// Client / UA access filter

type ClientFilterConfig struct {
	Mode             string   `json:"mode"`
	Rules            []string `json:"rules"`
	BlacklistPresets []string `json:"blacklist_presets,omitempty"`
	WhitelistPresets []string `json:"whitelist_presets,omitempty"`
}

type clientFilterRuntime struct {
	db    *DB
	mu    sync.RWMutex
	mode  string
	rules []string
}

func newClientFilterRuntime(db *DB) *clientFilterRuntime {
	rt := &clientFilterRuntime{
		db:   db,
		mode: clientFilterModeOff,
	}
	if db != nil {
		_ = rt.reload()
	}
	return rt
}

func normalizeClientFilterMode(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", clientFilterModeOff:
		return clientFilterModeOff, nil
	case clientFilterModeBlacklist:
		return clientFilterModeBlacklist, nil
	case clientFilterModeWhitelist:
		return clientFilterModeWhitelist, nil
	default:
		return "", errors.New("模式仅支持 off、blacklist 或 whitelist")
	}
}

func normalizeClientFilterRules(raw []string) ([]string, error) {
	if len(raw) > clientFilterMaxRules {
		return nil, fmt.Errorf("规则数量不能超过 %d 条", clientFilterMaxRules)
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		val := strings.ToLower(strings.TrimSpace(item))
		if val == "" {
			continue
		}
		if len(val) > clientFilterMaxRuleLen {
			return nil, fmt.Errorf("单条规则不能超过 %d 个字符", clientFilterMaxRuleLen)
		}
		if _, dup := seen[val]; dup {
			continue
		}
		seen[val] = struct{}{}
		out = append(out, val)
	}
	return out, nil
}

func (rt *clientFilterRuntime) reload() error {
	if rt == nil || rt.db == nil {
		return nil
	}
	var mode, rulesJSON string
	err := rt.db.db.QueryRow(`SELECT mode, rules_json FROM client_filter_settings WHERE id=1`).Scan(&mode, &rulesJSON)
	if errors.Is(err, sql.ErrNoRows) {
		rt.mu.Lock()
		rt.mode = clientFilterModeOff
		rt.rules = nil
		rt.mu.Unlock()
		return nil
	}
	if err != nil {
		return err
	}
	normMode, err := normalizeClientFilterMode(mode)
	if err != nil {
		normMode = clientFilterModeOff
	}
	var rawRules []string
	_ = json.Unmarshal([]byte(rulesJSON), &rawRules)
	rules, _ := normalizeClientFilterRules(rawRules)

	rt.mu.Lock()
	rt.mode = normMode
	rt.rules = rules
	rt.mu.Unlock()
	return nil
}

func (rt *clientFilterRuntime) snapshot() ClientFilterConfig {
	cfg := ClientFilterConfig{
		Mode:             clientFilterModeOff,
		Rules:            []string{},
		BlacklistPresets: append([]string(nil), clientFilterBlacklistPresets...),
		WhitelistPresets: append([]string(nil), clientFilterWhitelistPresets...),
	}
	if rt == nil {
		return cfg
	}
	rt.mu.RLock()
	cfg.Mode = rt.mode
	if len(rt.rules) > 0 {
		cfg.Rules = append([]string(nil), rt.rules...)
	}
	rt.mu.RUnlock()
	return cfg
}

func (rt *clientFilterRuntime) save(mode string, rawRules []string) (ClientFilterConfig, error) {
	normMode, err := normalizeClientFilterMode(mode)
	if err != nil {
		return ClientFilterConfig{}, err
	}
	rules, err := normalizeClientFilterRules(rawRules)
	if err != nil {
		return ClientFilterConfig{}, err
	}
	encoded, err := json.Marshal(rules)
	if err != nil {
		return ClientFilterConfig{}, err
	}
	if rt.db != nil {
		if _, err := rt.db.db.Exec(
			`INSERT INTO client_filter_settings(id, mode, rules_json, updated_at) VALUES(1, ?, ?, CURRENT_TIMESTAMP)
			 ON CONFLICT(id) DO UPDATE SET mode=excluded.mode, rules_json=excluded.rules_json, updated_at=CURRENT_TIMESTAMP`,
			normMode, string(encoded),
		); err != nil {
			return ClientFilterConfig{}, err
		}
	}
	rt.mu.Lock()
	rt.mode = normMode
	rt.rules = rules
	rt.mu.Unlock()
	return rt.snapshot(), nil
}

// isBlocked evaluates whether an incoming Emby proxy request should be rejected
// based on User-Agent, X-Emby-Client, and Emby Authorization Client fields.
func (rt *clientFilterRuntime) isBlocked(r *http.Request) (bool, string) {
	if rt == nil || r == nil {
		return false, ""
	}
	rt.mu.RLock()
	mode := rt.mode
	rules := rt.rules
	rt.mu.RUnlock()

	if mode == clientFilterModeOff || len(rules) == 0 {
		return false, ""
	}
	var sb strings.Builder
	if ua := strings.TrimSpace(r.Header.Get("User-Agent")); ua != "" {
		sb.WriteString(strings.ToLower(ua))
		sb.WriteByte(' ')
	}
	if cl := strings.TrimSpace(r.Header.Get("X-Emby-Client")); cl != "" {
		sb.WriteString(strings.ToLower(cl))
		sb.WriteByte(' ')
	}
	if authClient := strings.TrimSpace(embyRequestAuthorizationAttribute(r, "Client")); authClient != "" {
		sb.WriteString(strings.ToLower(authClient))
	}
	haystack := strings.TrimSpace(sb.String())

	switch mode {
	case clientFilterModeBlacklist:
		for _, rule := range rules {
			if strings.Contains(haystack, rule) {
				return true, rule
			}
		}
		return false, ""
	case clientFilterModeWhitelist:
		if haystack == "" {
			return true, "empty_client"
		}
		for _, rule := range rules {
			if strings.Contains(haystack, rule) {
				return false, ""
			}
		}
		return true, "not_in_whitelist"
	default:
		return false, ""
	}
}

// GET/PUT /api/client-filter
func (a *App) handleClientFilter(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.pm == nil || a.pm.clientFilter == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "客户端限制模块未初始化")
		return
	}
	switch r.Method {
	case http.MethodGet:
		a.jsonOK(w, a.pm.clientFilter.snapshot())
	case http.MethodPut:
		var req struct {
			Mode  string   `json:"mode"`
			Rules []string `json:"rules"`
		}
		if err := decodeJSONBody(w, r, &req); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "请求参数格式无效")
			return
		}
		cfg, err := a.pm.clientFilter.save(req.Mode, req.Rules)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		a.jsonOK(w, cfg)
	default:
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---------------------------------------------------------------------------
// Share user expiry & quota Telegram alerts

type ShareAlertSettings struct {
	Enabled         bool `json:"enabled"`
	ExpiryDays      int  `json:"expiry_days"`
	QuotaWarningPct int  `json:"quota_warning_pct"`
}

func (d *DB) getShareAlertSettings() ShareAlertSettings {
	out := ShareAlertSettings{
		Enabled:         true,
		ExpiryDays:      3,
		QuotaWarningPct: 80,
	}
	if d == nil || d.db == nil {
		return out
	}
	var enabled, days, pct int
	if err := d.db.QueryRow(`SELECT enabled, expiry_days, quota_warning_pct FROM share_alert_settings WHERE id=1`).Scan(&enabled, &days, &pct); err != nil {
		return out
	}
	out.Enabled = enabled != 0
	if days >= 1 && days <= 30 {
		out.ExpiryDays = days
	}
	if pct >= 50 && pct <= 99 {
		out.QuotaWarningPct = pct
	}
	return out
}

func (d *DB) saveShareAlertSettings(in ShareAlertSettings) (ShareAlertSettings, error) {
	if in.ExpiryDays < 1 || in.ExpiryDays > 30 {
		return ShareAlertSettings{}, errors.New("临期提前提醒天数应为 1–30 天")
	}
	if in.QuotaWarningPct < 50 || in.QuotaWarningPct > 99 {
		return ShareAlertSettings{}, errors.New("流量预警百分比应为 50–99%")
	}
	if _, err := d.db.Exec(
		`INSERT INTO share_alert_settings(id, enabled, expiry_days, quota_warning_pct, updated_at) VALUES(1, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(id) DO UPDATE SET enabled=excluded.enabled, expiry_days=excluded.expiry_days, quota_warning_pct=excluded.quota_warning_pct, updated_at=CURRENT_TIMESTAMP`,
		sqliteBool(in.Enabled), in.ExpiryDays, in.QuotaWarningPct,
	); err != nil {
		return ShareAlertSettings{}, err
	}
	return d.getShareAlertSettings(), nil
}

// GET/PUT /api/share-alerts
func (a *App) handleShareAlerts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		a.jsonOK(w, a.db.getShareAlertSettings())
	case http.MethodPut:
		var req ShareAlertSettings
		if err := decodeJSONBody(w, r, &req); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "请求参数格式无效")
			return
		}
		saved, err := a.db.saveShareAlertSettings(req)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		a.jsonOK(w, saved)
	default:
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

type pendingShareAlert struct {
	key     string
	message string
}

func runShareAlertMonitor(ctx context.Context, db *DB, shares *shareUserRuntime) {
	if db == nil || shares == nil {
		return
	}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(shareAlertCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			checkAndSendShareAlerts(ctx, db, shares, time.Now())
		case <-ticker.C:
			checkAndSendShareAlerts(ctx, db, shares, time.Now())
		}
	}
}

func collectPendingShareAlerts(db *DB, shares *shareUserRuntime, now time.Time) []pendingShareAlert {
	if db == nil || shares == nil {
		return nil
	}
	cfg := db.getShareAlertSettings()
	if !cfg.Enabled {
		return nil
	}
	type alertUserRow struct {
		id          int64
		username    string
		enabled     bool
		expiresAt   string
		quota       int64
		used        int64
		resetDay    int
		resetPeriod string
	}
	rows, err := db.db.Query(`SELECT id, username, enabled, expires_at, traffic_quota, traffic_used, traffic_reset_day, traffic_reset_period FROM share_users ORDER BY id`)
	if err != nil {
		return nil
	}
	var users []alertUserRow
	for rows.Next() {
		var u alertUserRow
		var en int
		if rows.Scan(&u.id, &u.username, &en, &u.expiresAt, &u.quota, &u.used, &u.resetDay, &u.resetPeriod) == nil {
			u.enabled = en != 0
			users = append(users, u)
		}
	}
	_ = rows.Close()
	if len(users) == 0 {
		return nil
	}
	sent := make(map[string]bool)
	if sRows, err := db.db.Query(`SELECT alert_key FROM share_alert_sent`); err == nil {
		for sRows.Next() {
			var k string
			if sRows.Scan(&k) == nil {
				sent[k] = true
			}
		}
		_ = sRows.Close()
	}

	loc := db.shareLocation()
	nowInLoc := now.In(loc)
	var alerts []pendingShareAlert

	for _, u := range users {
		if !u.enabled {
			continue
		}
		// 1. Expiry alerts
		if strings.TrimSpace(u.expiresAt) != "" {
			exp := db.shareExpiryTime(u.expiresAt)
			if !exp.IsZero() && exp.After(time.Unix(10, 0)) {
				remaining := exp.Sub(now)
				if remaining <= 0 {
					// Only notify recently expired accounts (within 7 days) to avoid spamming historical expired entries.
					if now.Sub(exp) <= 7*24*time.Hour {
						key := fmt.Sprintf("expired:%d:%s", u.id, u.expiresAt)
						if !sent[key] {
							alerts = append(alerts, pendingShareAlert{
								key:     key,
								message: fmt.Sprintf("⏰ 账号已到期：%s（到期日 %s）", u.username, u.expiresAt),
							})
						}
					}
				} else {
					daysLeft := int(remaining.Hours()/24) + 1
					if remaining <= time.Duration(cfg.ExpiryDays)*24*time.Hour {
						key := fmt.Sprintf("expiring:%d:%s", u.id, u.expiresAt)
						if !sent[key] {
							alerts = append(alerts, pendingShareAlert{
								key:     key,
								message: fmt.Sprintf("📅 账号临期提醒：%s 将在 %d 天内到期（%s）", u.username, daysLeft, u.expiresAt),
							})
						}
					}
				}
			}
		}
		// 2. Traffic quota alerts
		if u.quota > 0 {
			used := u.used
			shares.mu.RLock()
			if live := shares.users[u.id]; live != nil {
				used = live.persistedUsed + live.pending.Load()
			}
			shares.mu.RUnlock()

			period := u.resetPeriod
			if period == "" && u.resetDay > 0 {
				period, _ = shareResetPeriod(nowInLoc, u.resetDay)
			}
			if period == "" {
				period = "total"
			}
			pct := float64(used) * 100 / float64(u.quota)
			if used >= u.quota {
				key := fmt.Sprintf("quota_full:%d:%s", u.id, period)
				if !sent[key] {
					alerts = append(alerts, pendingShareAlert{
						key:     key,
						message: fmt.Sprintf("🚨 流量已耗尽：%s（%s / %s）", u.username, formatBytesForAlert(used), formatBytesForAlert(u.quota)),
					})
				}
			} else if pct >= float64(cfg.QuotaWarningPct) {
				key := fmt.Sprintf("quota_warn:%d:%s", u.id, period)
				if !sent[key] {
					alerts = append(alerts, pendingShareAlert{
						key:     key,
						message: fmt.Sprintf("📊 流量预警（%.0f%%）：%s（%s / %s）", pct, u.username, formatBytesForAlert(used), formatBytesForAlert(u.quota)),
					})
				}
			}
		}
	}
	return alerts
}

func checkAndSendShareAlerts(ctx context.Context, db *DB, shares *shareUserRuntime, now time.Time) {
	alerts := collectPendingShareAlerts(db, shares, now)
	if len(alerts) == 0 {
		return
	}
	stored, err := db.telegramReportSettings()
	if err != nil || !stored.Configured {
		return
	}
	if !stored.Enabled && (stored.Bot == nil || !stored.Bot.Enabled) {
		return
	}
	botToken, err := decryptTelegramBotToken(stored.BotTokenCiphertext)
	if err != nil || strings.TrimSpace(botToken) == "" || strings.TrimSpace(stored.ChatID) == "" {
		return
	}
	var sb strings.Builder
	sb.WriteString("🔔 云渡分享账号状态提醒\n")
	for _, a := range alerts {
		sb.WriteString("\n• ")
		sb.WriteString(a.message)
	}
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = sendTelegramReport(sendCtx, botToken, stored.ChatID, sb.String())
	cancel()
	if err != nil {
		log.Printf("share user TG alert failed: %v", err)
		return
	}
	for _, a := range alerts {
		_, _ = db.db.Exec(`INSERT OR IGNORE INTO share_alert_sent(alert_key) VALUES(?)`, a.key)
	}
}
