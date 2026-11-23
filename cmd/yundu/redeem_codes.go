package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

const (
	redeemCodeKindRenew    = "renew"
	redeemCodeKindRegister = "register"

	redeemCodeStatusUnused  = "unused"
	redeemCodeStatusUsed    = "used"
	redeemCodeStatusRevoked = "revoked"

	redeemCodeAlphabet   = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	redeemPrefixAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	redeemMaxBatchCount  = 100
)

const redeemCodeSchemaSQL = `
CREATE TABLE IF NOT EXISTS share_redeem_codes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	code TEXT NOT NULL UNIQUE,
	kind TEXT NOT NULL DEFAULT 'renew',
	days INTEGER NOT NULL DEFAULT 30,
	traffic_quota INTEGER NOT NULL DEFAULT 0,
	traffic_reset_day INTEGER NOT NULL DEFAULT 0,
	max_devices INTEGER NOT NULL DEFAULT 0,
	speed_limit INTEGER NOT NULL DEFAULT 0,
	sites_json TEXT NOT NULL DEFAULT '[]',
	expires_at TEXT NOT NULL DEFAULT '',
	note TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'unused',
	used_by_user_id INTEGER NOT NULL DEFAULT 0,
	used_by_username TEXT NOT NULL DEFAULT '',
	used_at TEXT NOT NULL DEFAULT '',
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_share_redeem_codes_status ON share_redeem_codes(status, id DESC);
`

func ensureRedeemCodeSchema(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, redeemCodeSchemaSQL)
	return err
}

type ShareRedeemCode struct {
	ID             int64    `json:"id"`
	Code           string   `json:"code"`
	Kind           string   `json:"kind"` // renew | register
	Days           int      `json:"days"`
	TrafficQuota   int64    `json:"traffic_quota"`
	ResetDay       int      `json:"traffic_reset_day"`
	MaxDevices     int      `json:"max_devices"`
	SpeedLimit     int      `json:"speed_limit"`
	SiteIDs        []int64  `json:"site_ids"`
	SiteNames      []string `json:"site_names,omitempty"`
	ExpiresAt      string   `json:"expires_at"`
	Note           string   `json:"note"`
	Status         string   `json:"status"` // unused | used | revoked | expired
	UsedByUserID   int64    `json:"used_by_user_id,omitempty"`
	UsedByUsername string   `json:"used_by_username,omitempty"`
	UsedAt         string   `json:"used_at,omitempty"`
	CreatedAt      string   `json:"created_at"`
}

type ShareRedeemCodeCreateInput struct {
	Kind         string  `json:"kind"`
	Count        int     `json:"count"`
	Days         int     `json:"days"`
	TrafficQuota int64   `json:"traffic_quota"`
	ResetDay     int     `json:"traffic_reset_day"`
	MaxDevices   int     `json:"max_devices"`
	SpeedLimit   int     `json:"speed_limit"`
	SiteIDs      []int64 `json:"site_ids"`
	ExpiresAt    string  `json:"expires_at"`
	Note         string  `json:"note"`
}

func generateRandomFromAlphabet(alphabet string, length int) (string, error) {
	maxIdx := big.NewInt(int64(len(alphabet)))
	buf := make([]byte, length)
	for i := 0; i < length; i++ {
		n, err := rand.Int(rand.Reader, maxIdx)
		if err != nil {
			return "", err
		}
		buf[i] = alphabet[n.Int64()]
	}
	return string(buf), nil
}

func generateRedeemCode() (string, error) {
	raw, err := generateRandomFromAlphabet(redeemCodeAlphabet, 12)
	if err != nil {
		return "", err
	}
	return "YD-" + raw[0:4] + "-" + raw[4:8] + "-" + raw[8:12], nil
}

func normalizeRedeemCodeText(raw string) string {
	return strings.ToUpper(strings.TrimSpace(raw))
}

func (a *App) generateUniqueShareRoutePrefix(tx *sql.Tx) (string, error) {
	for attempt := 0; attempt < 24; attempt++ {
		token, err := generateRandomFromAlphabet(redeemPrefixAlphabet, 6)
		if err != nil {
			return "", err
		}
		candidate := "/" + token
		if err := a.validateShareRoutePrefixTx(tx, candidate); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("无法生成唯一专属后缀，请稍后重试")
}

func (a *App) validateShareRoutePrefixTx(tx *sql.Tx, prefix string) error {
	normalized, err := normalizePathPrefix(prefix)
	if err != nil {
		return err
	}
	if normalized == "/app" || normalized == "/legacy" {
		return errors.New("reserved prefix")
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM share_users WHERE route_prefix=?`, normalized).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return errors.New("duplicate share route prefix")
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sites WHERE path_prefix=?`, normalized).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return errors.New("duplicate site path prefix")
	}
	return nil
}

func (d *DB) listRedeemCodes() ([]ShareRedeemCode, error) {
	siteNames := make(map[int64]string)
	if sRows, err := d.db.Query(`SELECT id, name FROM sites`); err == nil {
		for sRows.Next() {
			var id int64
			var name string
			if sRows.Scan(&id, &name) == nil {
				siteNames[id] = name
			}
		}
		_ = sRows.Close()
	}

	rows, err := d.db.Query(`SELECT id, code, kind, days, traffic_quota, traffic_reset_day, max_devices, speed_limit, sites_json, expires_at, note, status, used_by_user_id, used_by_username, used_at, COALESCE(created_at, '') FROM share_redeem_codes ORDER BY id DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	now := time.Now()
	out := make([]ShareRedeemCode, 0)
	for rows.Next() {
		var item ShareRedeemCode
		var sitesJSON string
		if err := rows.Scan(
			&item.ID, &item.Code, &item.Kind, &item.Days,
			&item.TrafficQuota, &item.ResetDay, &item.MaxDevices, &item.SpeedLimit,
			&sitesJSON, &item.ExpiresAt, &item.Note, &item.Status,
			&item.UsedByUserID, &item.UsedByUsername, &item.UsedAt, &item.CreatedAt,
		); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(sitesJSON), &item.SiteIDs)
		if item.SiteIDs == nil {
			item.SiteIDs = []int64{}
		}
		names := make([]string, 0, len(item.SiteIDs))
		for _, sid := range item.SiteIDs {
			if name := siteNames[sid]; name != "" {
				names = append(names, name)
			}
		}
		item.SiteNames = names
		if item.Status == redeemCodeStatusUnused && strings.TrimSpace(item.ExpiresAt) != "" {
			if exp := d.shareExpiryTime(item.ExpiresAt); !exp.IsZero() && !now.Before(exp) {
				item.Status = "expired"
			}
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func validateRedeemSiteIDs(db *DB, rawIDs []int64) ([]int64, error) {
	seen := make(map[int64]struct{}, len(rawIDs))
	valid := make([]int64, 0, len(rawIDs))
	for _, sid := range rawIDs {
		if sid <= 0 {
			continue
		}
		if _, dup := seen[sid]; dup {
			continue
		}
		var exists int
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM sites WHERE id=?`, sid).Scan(&exists); err != nil {
			return nil, err
		}
		if exists == 0 {
			return nil, fmt.Errorf("站点 #%d 不存在", sid)
		}
		seen[sid] = struct{}{}
		valid = append(valid, sid)
	}
	return valid, nil
}

// GET/POST /api/redeem-codes
func (a *App) handleRedeemCodes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		items, err := a.db.listRedeemCodes()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "读取兑换码失败")
			return
		}
		a.jsonOK(w, items)
	case http.MethodPost:
		var req ShareRedeemCodeCreateInput
		if err := decodeJSONBody(w, r, &req); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "请求参数格式无效")
			return
		}
		kind := strings.ToLower(strings.TrimSpace(req.Kind))
		if kind == "" {
			kind = redeemCodeKindRenew
		}
		if kind != redeemCodeKindRenew && kind != redeemCodeKindRegister {
			a.jsonErr(w, http.StatusBadRequest, "兑换码类型仅支持 renew（续期）或 register（开号）")
			return
		}
		count := req.Count
		if count == 0 {
			count = 1
		}
		if count < 1 || count > redeemMaxBatchCount {
			a.jsonErr(w, http.StatusBadRequest, fmt.Sprintf("单次生成数量应为 1–%d", redeemMaxBatchCount))
			return
		}
		if req.Days < 0 || req.Days > 3650 {
			a.jsonErr(w, http.StatusBadRequest, "有效天数应为 0–3650（0 表示永久/不改到期日）")
			return
		}
		if kind == redeemCodeKindRenew && req.Days == 0 && req.TrafficQuota <= 0 && len(req.SiteIDs) == 0 {
			a.jsonErr(w, http.StatusBadRequest, "续期码至少需要设置续期天数、流量额度或站点之一")
			return
		}
		if req.TrafficQuota < 0 {
			a.jsonErr(w, http.StatusBadRequest, "流量额度不能为负数")
			return
		}
		if req.ResetDay < 0 || req.ResetDay > 28 {
			a.jsonErr(w, http.StatusBadRequest, "每月重置日应为 0–28")
			return
		}
		if req.TrafficQuota == 0 {
			req.ResetDay = 0
		}
		if req.MaxDevices < 0 || req.MaxDevices > 100 {
			a.jsonErr(w, http.StatusBadRequest, "最大设备数应为 0–100")
			return
		}
		if req.SpeedLimit < 0 || req.SpeedLimit > maxSpeedLimitMbps {
			a.jsonErr(w, http.StatusBadRequest, "限速值无效")
			return
		}
		expiresAt := strings.TrimSpace(req.ExpiresAt)
		if expiresAt != "" {
			if _, err := a.db.parseSiteAccountExpiry(expiresAt); err != nil {
				a.jsonErr(w, http.StatusBadRequest, "兑换码截止日期格式应为 YYYY-MM-DD")
				return
			}
		}
		note := strings.TrimSpace(req.Note)
		if len([]rune(note)) > 120 {
			a.jsonErr(w, http.StatusBadRequest, "备注不能超过 120 个字符")
			return
		}
		siteIDs, err := validateRedeemSiteIDs(a.db, req.SiteIDs)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if kind == redeemCodeKindRegister && len(siteIDs) == 0 {
			a.jsonErr(w, http.StatusBadRequest, "开号兑换码至少需要选择一个站点")
			return
		}
		sitesJSON, _ := json.Marshal(siteIDs)

		tx, err := a.db.db.Begin()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "生成兑换码失败")
			return
		}
		created := make([]ShareRedeemCode, 0, count)
		for i := 0; i < count; i++ {
			var code string
			var insertedID int64
			for attempt := 0; attempt < 8; attempt++ {
				c, genErr := generateRedeemCode()
				if genErr != nil {
					_ = tx.Rollback()
					a.jsonErr(w, http.StatusInternalServerError, "生成随机卡密失败")
					return
				}
				res, insErr := tx.Exec(
					`INSERT INTO share_redeem_codes(code, kind, days, traffic_quota, traffic_reset_day, max_devices, speed_limit, sites_json, expires_at, note, status)
					 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
					c, kind, req.Days, req.TrafficQuota, req.ResetDay, req.MaxDevices, req.SpeedLimit, string(sitesJSON), expiresAt, note, redeemCodeStatusUnused,
				)
				if insErr == nil {
					code = c
					insertedID, _ = res.LastInsertId()
					break
				}
				if !isSQLiteUniqueConstraintError(insErr) {
					_ = tx.Rollback()
					a.jsonErr(w, http.StatusInternalServerError, "保存兑换码失败")
					return
				}
			}
			if code == "" {
				_ = tx.Rollback()
				a.jsonErr(w, http.StatusInternalServerError, "生成唯一卡密失败，请重试")
				return
			}
			created = append(created, ShareRedeemCode{
				ID:           insertedID,
				Code:         code,
				Kind:         kind,
				Days:         req.Days,
				TrafficQuota: req.TrafficQuota,
				ResetDay:     req.ResetDay,
				MaxDevices:   req.MaxDevices,
				SpeedLimit:   req.SpeedLimit,
				SiteIDs:      siteIDs,
				ExpiresAt:    expiresAt,
				Note:         note,
				Status:       redeemCodeStatusUnused,
			})
		}
		if err := tx.Commit(); err != nil {
			_ = tx.Rollback()
			a.jsonErr(w, http.StatusInternalServerError, "提交兑换码失败")
			return
		}
		a.jsonResponse(w, http.StatusCreated, created)
	default:
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// POST /api/redeem-codes/{id}/revoke, DELETE /api/redeem-codes/{id}
func (a *App) handleRedeemCodeByID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	rest := strings.TrimPrefix(r.URL.Path, "/api/redeem-codes/")
	idPart, action, _ := strings.Cut(rest, "/")
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil || id <= 0 {
		a.jsonErr(w, http.StatusBadRequest, "无效的兑换码 ID")
		return
	}
	switch action {
	case "":
		if r.Method != http.MethodDelete {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		res, err := a.db.db.Exec(`DELETE FROM share_redeem_codes WHERE id=?`, id)
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "删除失败")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			a.jsonErr(w, http.StatusNotFound, "兑换码不存在")
			return
		}
		a.jsonOK(w, map[string]bool{"deleted": true})
	case "revoke":
		if r.Method != http.MethodPost {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		res, err := a.db.db.Exec(`UPDATE share_redeem_codes SET status=? WHERE id=? AND status=?`, redeemCodeStatusRevoked, id, redeemCodeStatusUnused)
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "作废失败")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			a.jsonErr(w, http.StatusBadRequest, "兑换码不存在或已使用/已作废")
			return
		}
		a.jsonOK(w, map[string]bool{"revoked": true})
	default:
		a.jsonErr(w, http.StatusNotFound, "not found")
	}
}

// POST /api/me/redeem (authenticated share user redeems a renewal code)
func (a *App) handleShareSelfRedeem(w http.ResponseWriter, r *http.Request, user *storedShareUser) {
	if r.Method != http.MethodPost {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := decodeJSONBody(w, r, &req); err != nil {
		a.jsonErr(w, http.StatusBadRequest, "请求参数无效")
		return
	}
	code := normalizeRedeemCodeText(req.Code)
	if code == "" || len(code) > 64 {
		a.jsonErr(w, http.StatusBadRequest, "请输入有效的兑换码")
		return
	}

	tx, err := a.db.db.Begin()
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "兑换暂时不可用")
		return
	}
	var c ShareRedeemCode
	var sitesJSON string
	err = tx.QueryRow(
		`SELECT id, code, kind, days, traffic_quota, traffic_reset_day, max_devices, speed_limit, sites_json, expires_at, status FROM share_redeem_codes WHERE code=?`,
		code,
	).Scan(&c.ID, &c.Code, &c.Kind, &c.Days, &c.TrafficQuota, &c.ResetDay, &c.MaxDevices, &c.SpeedLimit, &sitesJSON, &c.ExpiresAt, &c.Status)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		a.jsonErr(w, http.StatusBadRequest, "兑换码无效或不存在")
		return
	}
	if err != nil {
		_ = tx.Rollback()
		a.jsonErr(w, http.StatusInternalServerError, "兑换暂时不可用")
		return
	}
	if c.Status != redeemCodeStatusUnused {
		_ = tx.Rollback()
		a.jsonErr(w, http.StatusBadRequest, "该兑换码已被使用或已作废")
		return
	}
	if c.Kind != redeemCodeKindRenew {
		_ = tx.Rollback()
		a.jsonErr(w, http.StatusBadRequest, "这是新账号开号兑换码，请在登录页使用「卡密开号」")
		return
	}
	now := time.Now()
	if strings.TrimSpace(c.ExpiresAt) != "" {
		if exp := a.db.shareExpiryTime(c.ExpiresAt); !exp.IsZero() && !now.Before(exp) {
			_ = tx.Rollback()
			a.jsonErr(w, http.StatusBadRequest, "该兑换码已过使用期限")
			return
		}
	}

	claimRes, err := tx.Exec(
		`UPDATE share_redeem_codes SET status=?, used_by_user_id=?, used_by_username=?, used_at=CURRENT_TIMESTAMP WHERE id=? AND status=?`,
		redeemCodeStatusUsed, user.ID, user.Username, c.ID, redeemCodeStatusUnused,
	)
	if err != nil {
		_ = tx.Rollback()
		a.jsonErr(w, http.StatusInternalServerError, "兑换失败")
		return
	}
	if n, _ := claimRes.RowsAffected(); n != 1 {
		_ = tx.Rollback()
		a.jsonErr(w, http.StatusBadRequest, "该兑换码已被使用")
		return
	}

	// Compute updated expiry, quota, limits, and sites
	var curExpires string
	var curQuota, curUsed int64
	var curResetDay, curMaxDev, curSpeed int
	var curResetPeriod string
	if err := tx.QueryRow(
		`SELECT expires_at, traffic_quota, traffic_used, traffic_reset_day, traffic_reset_period, max_devices, speed_limit FROM share_users WHERE id=?`,
		user.ID,
	).Scan(&curExpires, &curQuota, &curUsed, &curResetDay, &curResetPeriod, &curMaxDev, &curSpeed); err != nil {
		_ = tx.Rollback()
		a.jsonErr(w, http.StatusInternalServerError, "读取账号状态失败")
		return
	}

	loc := a.db.shareLocation()
	var summaryParts []string
	nextExpires := curExpires
	if c.Days > 0 {
		if curExpires == "" {
			// Already permanent; keep permanent.
			summaryParts = append(summaryParts, "账号为永久有效")
		} else {
			base := now.In(loc)
			if exp := a.db.shareExpiryTime(curExpires); exp.After(base) {
				base = exp.Add(-time.Second).In(loc)
			}
			nextExpires = base.AddDate(0, 0, c.Days).Format("2006-01-02")
			summaryParts = append(summaryParts, fmt.Sprintf("续期 %d 天（至 %s）", c.Days, nextExpires))
		}
	}
	nextQuota := curQuota
	nextUsed := curUsed
	nextResetDay := curResetDay
	nextResetPeriod := curResetPeriod
	if c.TrafficQuota > 0 {
		nextQuota = c.TrafficQuota
		if c.ResetDay > 0 {
			nextResetDay = c.ResetDay
			nextResetPeriod, _ = shareResetPeriod(now.In(loc), nextResetDay)
		}
		if curQuota > 0 && curUsed >= curQuota {
			nextUsed = 0
		}
		summaryParts = append(summaryParts, fmt.Sprintf("流量额度更新为 %s", formatBytesForAlert(nextQuota)))
	}
	nextMaxDev := curMaxDev
	if c.MaxDevices > 0 {
		nextMaxDev = c.MaxDevices
	}
	nextSpeed := curSpeed
	if c.SpeedLimit > 0 {
		nextSpeed = c.SpeedLimit
	}

	if _, err := tx.Exec(
		`UPDATE share_users SET expires_at=?, traffic_quota=?, traffic_used=?, traffic_reset_day=?, traffic_reset_period=?, max_devices=?, speed_limit=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		nextExpires, nextQuota, nextUsed, nextResetDay, nextResetPeriod, nextMaxDev, nextSpeed, user.ID,
	); err != nil {
		_ = tx.Rollback()
		a.jsonErr(w, http.StatusInternalServerError, "更新账号失败")
		return
	}

	var codeSites []int64
	_ = json.Unmarshal([]byte(sitesJSON), &codeSites)
	if len(codeSites) > 0 {
		var maxOrder int
		_ = tx.QueryRow(`SELECT COALESCE(MAX(sort_order), 0) FROM share_user_sites WHERE share_user_id=?`, user.ID).Scan(&maxOrder)
		addedSites := 0
		for _, sid := range codeSites {
			var siteExists int
			if tx.QueryRow(`SELECT COUNT(*) FROM sites WHERE id=?`, sid).Scan(&siteExists) != nil || siteExists == 0 {
				continue
			}
			maxOrder++
			res, insErr := tx.Exec(`INSERT OR IGNORE INTO share_user_sites(share_user_id, site_id, site_account_id, sort_order) VALUES(?, ?, 0, ?)`, user.ID, sid, maxOrder)
			if insErr == nil {
				if n, _ := res.RowsAffected(); n > 0 {
					addedSites++
				}
			}
		}
		if addedSites > 0 {
			summaryParts = append(summaryParts, fmt.Sprintf("新增 %d 个站点权限", addedSites))
		}
	}

	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		a.jsonErr(w, http.StatusInternalServerError, "兑换提交失败")
		return
	}
	a.reloadShareUsers()
	updated, err := a.db.getShareUser(user.ID)
	if err != nil || updated == nil {
		updated = user
	}
	summary := "兑换成功"
	if len(summaryParts) > 0 {
		summary = "兑换成功：" + strings.Join(summaryParts, "，")
	}
	a.jsonOK(w, struct {
		shareSelfView
		RedeemSummary string `json:"redeem_summary"`
	}{
		shareSelfView: a.buildShareSelfView(updated, r),
		RedeemSummary: summary,
	})
}

func validateRedeemUsername(raw string) (string, error) {
	username := strings.TrimSpace(raw)
	if username == "" || len([]rune(username)) > 64 {
		return "", errors.New("用户名需要 1–64 个字符")
	}
	for _, r := range username {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", errors.New("用户名不能包含空格或控制字符")
		}
	}
	return username, nil
}

// POST /api/redeem/register (public self-registration with a 'register' redeem code)
func (a *App) handleRedeemRegister(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	client := "redeem|" + requestClientKey(r, a.trustedProxies)
	now := time.Now()
	if allowed, retryAfter := a.limiter().allow(client, now); !allowed {
		a.authRateLimitErr(w, "尝试次数过多，请稍后再试", retryAfter)
		return
	}
	var req struct {
		Code     string `json:"code"`
		Username string `json:"username"`
		Password string `json:"password"` // #nosec G117 -- request-only credential DTO.
	}
	if err := decodeJSONBody(w, r, &req); err != nil {
		a.jsonErr(w, http.StatusBadRequest, "请求参数格式无效")
		return
	}
	code := normalizeRedeemCodeText(req.Code)
	if code == "" || len(code) > 64 {
		a.jsonErr(w, http.StatusBadRequest, "请输入有效的开号兑换码")
		return
	}
	username, err := validateRedeemUsername(req.Username)
	if err != nil {
		a.jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len([]rune(req.Password)) < 4 || len(req.Password) > 72 {
		a.jsonErr(w, http.StatusBadRequest, "密码需要 4–72 个字符")
		return
	}
	// Do not allow registering a username that collides with an administrator username.
	var adminCount int
	if err := a.db.db.QueryRow(`SELECT COUNT(*) FROM users WHERE username=? COLLATE NOCASE`, username).Scan(&adminCount); err == nil && adminCount > 0 {
		a.jsonErr(w, http.StatusBadRequest, "该用户名已被占用，请更换")
		return
	}

	tx, err := a.db.db.Begin()
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "注册暂时不可用")
		return
	}
	var c ShareRedeemCode
	var sitesJSON string
	err = tx.QueryRow(
		`SELECT id, code, kind, days, traffic_quota, traffic_reset_day, max_devices, speed_limit, sites_json, expires_at, note, status FROM share_redeem_codes WHERE code=?`,
		code,
	).Scan(&c.ID, &c.Code, &c.Kind, &c.Days, &c.TrafficQuota, &c.ResetDay, &c.MaxDevices, &c.SpeedLimit, &sitesJSON, &c.ExpiresAt, &c.Note, &c.Status)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		a.limiter().recordFailure(client, now)
		a.jsonErr(w, http.StatusBadRequest, "兑换码无效或不存在")
		return
	}
	if err != nil {
		_ = tx.Rollback()
		a.jsonErr(w, http.StatusInternalServerError, "注册暂时不可用")
		return
	}
	if c.Status != redeemCodeStatusUnused {
		_ = tx.Rollback()
		a.limiter().recordFailure(client, now)
		a.jsonErr(w, http.StatusBadRequest, "该兑换码已被使用或已作废")
		return
	}
	if c.Kind != redeemCodeKindRegister {
		_ = tx.Rollback()
		a.jsonErr(w, http.StatusBadRequest, "这是续期码，请先登录后在个人页兑换续期")
		return
	}
	if strings.TrimSpace(c.ExpiresAt) != "" {
		if exp := a.db.shareExpiryTime(c.ExpiresAt); !exp.IsZero() && !now.Before(exp) {
			_ = tx.Rollback()
			a.jsonErr(w, http.StatusBadRequest, "该兑换码已过使用期限")
			return
		}
	}

	var codeSites []int64
	_ = json.Unmarshal([]byte(sitesJSON), &codeSites)
	validSites := make([]int64, 0, len(codeSites))
	for _, sid := range codeSites {
		var exists int
		if tx.QueryRow(`SELECT COUNT(*) FROM sites WHERE id=?`, sid).Scan(&exists) == nil && exists > 0 {
			validSites = append(validSites, sid)
		}
	}
	if len(validSites) == 0 {
		_ = tx.Rollback()
		a.jsonErr(w, http.StatusBadRequest, "该兑换码绑定的站点已不存在，请联系管理员")
		return
	}

	routePrefix, err := a.generateUniqueShareRoutePrefix(tx)
	if err != nil {
		_ = tx.Rollback()
		a.jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	portalToken, err := newSharePortalToken()
	if err != nil {
		_ = tx.Rollback()
		a.jsonErr(w, http.StatusInternalServerError, "创建账号失败")
		return
	}
	pwHash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		_ = tx.Rollback()
		a.jsonErr(w, http.StatusInternalServerError, "创建账号失败")
		return
	}
	pwCipher, err := encryptSiteAccountSecret(req.Password)
	if err != nil {
		_ = tx.Rollback()
		a.jsonErr(w, http.StatusInternalServerError, "创建账号失败")
		return
	}

	loc := a.db.shareLocation()
	expiresAt := ""
	if c.Days > 0 {
		expiresAt = now.In(loc).AddDate(0, 0, c.Days).Format("2006-01-02")
	}
	resetPeriod, _ := shareResetPeriod(now.In(loc), c.ResetDay)
	userNote := strings.TrimSpace(c.Note)
	if userNote == "" {
		userNote = "卡密开号 " + c.Code
	}

	res, err := tx.Exec(
		`INSERT INTO share_users(username, password_hash, password_ciphertext, route_prefix, note, enabled, expires_at, traffic_quota, max_devices, speed_limit, traffic_reset_day, traffic_reset_period, portal_token)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		username, string(pwHash), pwCipher, routePrefix, userNote, 1, expiresAt, c.TrafficQuota, c.MaxDevices, c.SpeedLimit, c.ResetDay, resetPeriod, portalToken,
	)
	if err != nil {
		_ = tx.Rollback()
		if isSQLiteUniqueConstraintError(err) {
			a.jsonErr(w, http.StatusBadRequest, "用户名已存在，请更换")
			return
		}
		a.jsonErr(w, http.StatusInternalServerError, "创建账号失败")
		return
	}
	userID, err := res.LastInsertId()
	if err != nil {
		_ = tx.Rollback()
		a.jsonErr(w, http.StatusInternalServerError, "创建账号失败")
		return
	}
	for i, sid := range validSites {
		if _, err := tx.Exec(`INSERT INTO share_user_sites(share_user_id, site_id, site_account_id, sort_order) VALUES(?, ?, 0, ?)`, userID, sid, i+1); err != nil {
			_ = tx.Rollback()
			a.jsonErr(w, http.StatusInternalServerError, "分配站点失败")
			return
		}
	}
	claimRes, err := tx.Exec(
		`UPDATE share_redeem_codes SET status=?, used_by_user_id=?, used_by_username=?, used_at=CURRENT_TIMESTAMP WHERE id=? AND status=?`,
		redeemCodeStatusUsed, userID, username, c.ID, redeemCodeStatusUnused,
	)
	if err != nil {
		_ = tx.Rollback()
		a.jsonErr(w, http.StatusInternalServerError, "核销兑换码失败")
		return
	}
	if n, _ := claimRes.RowsAffected(); n != 1 {
		_ = tx.Rollback()
		a.jsonErr(w, http.StatusBadRequest, "该兑换码已被使用")
		return
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		a.jsonErr(w, http.StatusInternalServerError, "创建账号失败")
		return
	}
	a.limiter().reset(client)
	a.reloadShareUsers()
	a.setShareSessionCookie(w, r, signShareSession(userID, string(pwHash), time.Now()), int(shareSessionDuration.Seconds()))

	origin := a.shareTextOrigin(r)
	routeURL := routePrefix
	if origin != "" {
		routeURL = strings.TrimRight(origin, "/") + routePrefix
	}
	a.jsonResponse(w, http.StatusCreated, map[string]any{
		"username":      username,
		"route_prefix":  routePrefix,
		"route_url":     routeURL,
		"share_account": true,
	})
}
