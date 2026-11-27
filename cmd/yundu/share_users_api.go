package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

type ShareUserSite struct {
	SiteID        int64  `json:"site_id"`
	SiteAccountID int64  `json:"site_account_id"`
	SiteName      string `json:"site_name,omitempty"`
	AccountName   string `json:"account_name,omitempty"`
	SiteEnabled   bool   `json:"site_enabled,omitempty"`
	SortOrder     int    `json:"sort_order"`
	Active        bool   `json:"active,omitempty"`
}

type ShareUser struct {
	ID               int64                `json:"id"`
	Username         string               `json:"username"`
	Password         string               `json:"password,omitempty"` // #nosec G117 -- authorized share-user memo for the administrator.
	RoutePrefix      string               `json:"route_prefix"`
	RouteURL         string               `json:"route_url,omitempty"`
	Note             string               `json:"note"`
	Enabled          bool                 `json:"enabled"`
	ExpiresAt        string               `json:"expires_at"`
	TrafficQuota     int64                `json:"traffic_quota"`
	TrafficUsed      int64                `json:"traffic_used"`
	Status           string               `json:"status"`
	SessionCount     int                  `json:"session_count"`
	DrainingSessions int                  `json:"draining_sessions,omitempty"`
	MaxDevices       int                  `json:"max_devices"`
	SpeedLimit       int                  `json:"speed_limit"`
	ResetDay         int                  `json:"traffic_reset_day"`
	NextResetAt      string               `json:"next_reset_at,omitempty"`
	ActiveSiteID     int64                `json:"active_site_id,omitempty"`
	ActiveSiteName   string               `json:"active_site_name,omitempty"`
	Sites            []ShareUserSite          `json:"sites"`
	Sessions         []shareStoredSession     `json:"sessions,omitempty"`
	DanmakuSourceIDs []int64                  `json:"danmaku_source_ids"`
	DanmakuSources   []shareSelfDanmakuSource `json:"danmaku_sources,omitempty"`
	CreatedAt        string                   `json:"created_at"`
	UpdatedAt        string                   `json:"updated_at"`
}

type ShareUserSiteInput struct {
	SiteID        int64 `json:"site_id"`
	SiteAccountID int64 `json:"site_account_id"`
}

type ShareUserInput struct {
	Username     string               `json:"username"`
	Password     string               `json:"password"` // #nosec G117 -- request-only credential DTO.
	RoutePrefix  string               `json:"route_prefix"`
	Note         string               `json:"note"`
	Enabled      *bool                `json:"enabled"`
	ExpiresAt    string               `json:"expires_at"`
	TrafficQuota int64                `json:"traffic_quota"`
	MaxDevices   int                  `json:"max_devices"`
	SpeedLimit   int                  `json:"speed_limit"`
	ResetDay         int                  `json:"traffic_reset_day"`
	Sites            []ShareUserSiteInput `json:"sites"`
	DanmakuSourceIDs *[]int64             `json:"danmaku_source_ids"`
}

type storedShareUser struct {
	ShareUser
	passwordCiphertext string
	portalToken        string
	danmakuToken       string
}

func (d *DB) getShareUser(id int64) (*storedShareUser, error) {
	return d.getShareUserWhere(`id=?`, id)
}

func (d *DB) getShareUserWhere(where string, arg any) (*storedShareUser, error) {
	var item storedShareUser
	var enabled int
	err := d.db.QueryRow(`SELECT id, username, password_ciphertext, route_prefix, note, enabled, expires_at, traffic_quota, traffic_used, max_devices, traffic_reset_day, speed_limit, portal_token, danmaku_token, COALESCE(created_at, ''), COALESCE(updated_at, '') FROM share_users WHERE `+where, arg).
		Scan(&item.ID, &item.Username, &item.passwordCiphertext, &item.RoutePrefix, &item.Note, &enabled, &item.ExpiresAt, &item.TrafficQuota, &item.TrafficUsed, &item.MaxDevices, &item.ResetDay, &item.SpeedLimit, &item.portalToken, &item.danmakuToken, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		return nil, err
	}
	item.Enabled = enabled != 0
	sites, err := d.shareUserSites(item.ID)
	if err != nil {
		return nil, err
	}
	item.Sites = sites
	dmIDs, err := d.shareUserDanmakuSourceIDs(item.ID)
	if err != nil {
		return nil, err
	}
	item.DanmakuSourceIDs = dmIDs
	return &item, nil
}

func (d *DB) shareUserSites(id int64) ([]ShareUserSite, error) {
	rows, err := d.db.Query(`SELECT a.site_id, a.site_account_id, a.sort_order, s.name, s.enabled, COALESCE(c.account_name, '') FROM share_user_sites a JOIN sites s ON s.id=a.site_id LEFT JOIN site_accounts c ON c.id=a.site_account_id AND c.site_id=a.site_id WHERE a.share_user_id=? ORDER BY a.sort_order, s.sort_order, s.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ShareUserSite, 0)
	for rows.Next() {
		var item ShareUserSite
		var enabled int
		if err := rows.Scan(&item.SiteID, &item.SiteAccountID, &item.SortOrder, &item.SiteName, &enabled, &item.AccountName); err != nil {
			return nil, err
		}
		item.SiteEnabled = enabled != 0
		result = append(result, item)
	}
	return result, rows.Err()
}

func (d *DB) listShareUserIDs() ([]int64, error) {
	rows, err := d.db.Query(`SELECT id FROM share_users ORDER BY sort_order ASC, id ASC`)
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

func validShareUsername(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (a *App) normalizeShareUserInput(input ShareUserInput, excludeID int64, creating bool) (ShareUserInput, error) {
	input.Username = strings.TrimSpace(input.Username)
	input.Note = strings.TrimSpace(input.Note)
	input.ExpiresAt = strings.TrimSpace(input.ExpiresAt)
	if !validShareUsername(input.Username) {
		return input, errors.New("用户名需要 1–64 个字符，且不能包含空格")
	}
	var adminNamed int
	if err := a.db.db.QueryRow(`SELECT COUNT(*) FROM users WHERE username=? COLLATE NOCASE`, input.Username).Scan(&adminNamed); err == nil && adminNamed > 0 {
		return input, errors.New("不能和管理员用户名相同")
	}
	if strings.TrimSpace(input.RoutePrefix) != "" {
		normalized, err := normalizePathPrefix(input.RoutePrefix)
		if err != nil {
			if strings.Contains(err.Error(), "reserved") {
				return input, errors.New("专属后缀不能使用系统保留路径")
			}
			return input, errors.New("专属后缀只能包含英文字母、数字、连字符和下划线（最多 64 位）")
		}
		if normalized == "/app" || normalized == "/legacy" {
			return input, errors.New("专属后缀不能使用系统保留路径")
		}
		input.RoutePrefix = normalized
		var siteConflict int
		if err := a.db.db.QueryRow(`SELECT COUNT(*) FROM sites WHERE path_prefix=?`, input.RoutePrefix).Scan(&siteConflict); err == nil && siteConflict > 0 {
			return input, errors.New("专属后缀与已有站点路径冲突")
		}
		if a.pm != nil {
			if _, exists := a.pm.PathPrefixSiteID(input.RoutePrefix); exists {
				return input, errors.New("专属后缀与已有站点路径冲突")
			}
		}
		var userConflict int
		if err := a.db.db.QueryRow(`SELECT COUNT(*) FROM share_users WHERE route_prefix=? AND id<>?`, input.RoutePrefix, excludeID).Scan(&userConflict); err == nil && userConflict > 0 {
			return input, errors.New("专属后缀已被其他分享账号使用")
		}
	} else {
		input.RoutePrefix = ""
	}
	if input.Password != "" && (len([]rune(input.Password)) < 4 || len(input.Password) > 72) {
		return input, errors.New("密码需要 4–72 个字符")
	}
	if creating && input.Password == "" {
		generated, err := generateSharePassword(10)
		if err != nil {
			return input, err
		}
		input.Password = generated
	}
	if len(input.Note) > 1000 {
		return input, errors.New("备注过长")
	}
	if input.ExpiresAt != "" {
		if len(input.ExpiresAt) > 64 {
			return input, errors.New("到期时间过长")
		}
		if _, err := a.db.parseSiteAccountExpiry(input.ExpiresAt); err != nil {
			return input, errors.New("到期时间格式应为 YYYY-MM-DD")
		}
	}
	if input.TrafficQuota < 0 {
		return input, errors.New("流量额度不能为负数")
	}
	if input.MaxDevices < 0 || input.MaxDevices > 100 {
		return input, errors.New("同时在线设备数应为 0–100，0 表示不限")
	}
	if input.SpeedLimit < 0 || input.SpeedLimit > 100000 {
		return input, errors.New("限速应为 0–100000 Mbps，0 表示不限")
	}
	if input.ResetDay < 0 || input.ResetDay > 28 {
		return input, errors.New("流量重置日应为 1–28，0 表示不自动重置")
	}
	if len(input.Sites) == 0 {
		return input, errors.New("请至少选择一个可用站点")
	}
	if len(input.Sites) > 500 {
		return input, errors.New("站点数量过多")
	}
	seen := make(map[int64]bool, len(input.Sites))
	for _, site := range input.Sites {
		if site.SiteID <= 0 || seen[site.SiteID] {
			return input, errors.New("站点选择无效")
		}
		seen[site.SiteID] = true
		if _, err := a.db.GetSite(site.SiteID); err != nil {
			return input, errors.New("所选站点不存在")
		}
		if site.SiteAccountID > 0 {
			if _, err := a.db.getSiteAccount(site.SiteID, site.SiteAccountID); err != nil {
				return input, errors.New("所选源站账号不属于该站点")
			}
		}
	}
	if input.DanmakuSourceIDs != nil {
		if len(*input.DanmakuSourceIDs) > 100 {
			return input, errors.New("弹幕源选择数量过多")
		}
		dedup := make([]int64, 0, len(*input.DanmakuSourceIDs))
		seenDM := make(map[int64]bool, len(*input.DanmakuSourceIDs))
		for _, dmID := range *input.DanmakuSourceIDs {
			if dmID <= 0 {
				return input, errors.New("弹幕源选择无效")
			}
			if seenDM[dmID] {
				continue
			}
			seenDM[dmID] = true
			if _, err := a.db.GetDanmakuSource(dmID); err != nil {
				return input, errors.New("所选弹幕源不存在")
			}
			dedup = append(dedup, dmID)
		}
		input.DanmakuSourceIDs = &dedup
	}
	return input, nil
}

func (a *App) saveShareUser(id int64, input ShareUserInput) (int64, error) {
	tx, err := a.db.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	// A new or changed reset day starts counting from the current cycle, so
	// saving the form never wipes usage on the spot.
	resetPeriod, _ := shareResetPeriod(time.Now().In(a.db.shareLocation()), input.ResetDay)
	if id == 0 {
		portalToken, err := newSharePortalToken()
		if err != nil {
			return 0, err
		}
		danmakuToken, err := newShareDanmakuToken()
		if err != nil {
			return 0, err
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
		if err != nil {
			return 0, err
		}
		cipher, err := encryptSiteAccountSecret(input.Password)
		if err != nil {
			return 0, err
		}
		var maxOrder int
		_ = tx.QueryRow(`SELECT COALESCE(MAX(sort_order), 0) FROM share_users`).Scan(&maxOrder)
		result, err := tx.Exec(`INSERT INTO share_users(username, password_hash, password_ciphertext, route_prefix, note, enabled, expires_at, traffic_quota, max_devices, speed_limit, traffic_reset_day, traffic_reset_period, portal_token, danmaku_token, sort_order) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			input.Username, string(hash), cipher, input.RoutePrefix, input.Note, sqliteBool(enabled), input.ExpiresAt, input.TrafficQuota, input.MaxDevices, input.SpeedLimit, input.ResetDay, resetPeriod, portalToken, danmakuToken, maxOrder+1)
		if err != nil {
			if isSQLiteUniqueConstraintError(err) {
				if strings.Contains(err.Error(), "route_prefix") {
					return 0, errors.New("专属后缀已被其他分享账号使用")
				}
				return 0, errors.New("用户名已存在")
			}
			return 0, err
		}
		id, err = result.LastInsertId()
		if err != nil {
			return 0, err
		}
	} else {
		if input.Enabled == nil {
			var current int
			if err := tx.QueryRow(`SELECT enabled FROM share_users WHERE id=?`, id).Scan(&current); err != nil {
				return 0, err
			}
			enabled = current != 0
		}
		result, err := tx.Exec(`UPDATE share_users SET username=?, route_prefix=?, note=?, enabled=?, expires_at=?, traffic_quota=?, max_devices=?, speed_limit=?, traffic_reset_period=CASE WHEN traffic_reset_day<>? THEN ? ELSE traffic_reset_period END, traffic_reset_day=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
			input.Username, input.RoutePrefix, input.Note, sqliteBool(enabled), input.ExpiresAt, input.TrafficQuota, input.MaxDevices, input.SpeedLimit, input.ResetDay, resetPeriod, input.ResetDay, id)
		if err != nil {
			if isSQLiteUniqueConstraintError(err) {
				if strings.Contains(err.Error(), "route_prefix") {
					return 0, errors.New("专属后缀已被其他分享账号使用")
				}
				return 0, errors.New("用户名已存在")
			}
			return 0, err
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return 0, sql.ErrNoRows
		}
		if input.Password != "" {
			hash, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
			if err != nil {
				return 0, err
			}
			cipher, err := encryptSiteAccountSecret(input.Password)
			if err != nil {
				return 0, err
			}
			if _, err := tx.Exec(`UPDATE share_users SET password_hash=?, password_ciphertext=? WHERE id=?`, string(hash), cipher, id); err != nil {
				return 0, err
			}
		}
		if _, err := tx.Exec(`DELETE FROM share_user_sites WHERE share_user_id=?`, id); err != nil {
			return 0, err
		}
	}
	for index, site := range input.Sites {
		if _, err := tx.Exec(`INSERT INTO share_user_sites(share_user_id, site_id, site_account_id, sort_order) VALUES(?,?,?,?)`, id, site.SiteID, site.SiteAccountID, index+1); err != nil {
			return 0, err
		}
	}
	if input.DanmakuSourceIDs != nil {
		if _, err := tx.Exec(`DELETE FROM share_user_danmaku_sources WHERE share_user_id=?`, id); err != nil {
			return 0, err
		}
		for _, dmID := range *input.DanmakuSourceIDs {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO share_user_danmaku_sources(share_user_id, danmaku_source_id) VALUES(?,?)`, id, dmID); err != nil {
				return 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (a *App) shareUserView(item *storedShareUser, includeSecrets bool, origin string) ShareUser {
	view := item.ShareUser
	if a.pm != nil && a.pm.shares != nil {
		used, _ := a.pm.shares.usage(item.ID)
		if used > view.TrafficUsed {
			view.TrafficUsed = used
		}
		view.SessionCount = a.pm.shares.deviceCount(item.ID)
		view.DrainingSessions = a.pm.shares.drainingCount(item.ID)
	}
	if view.ResetDay > 0 {
		if _, next := shareResetPeriod(time.Now().In(a.db.shareLocation()), view.ResetDay); !next.IsZero() {
			view.NextResetAt = next.Format("2006-01-02")
		}
	}
	view.Status = "active"
	switch {
	case !view.Enabled:
		view.Status = "disabled"
	case view.ExpiresAt != "" && !time.Now().Before(a.db.shareExpiryTime(view.ExpiresAt)):
		view.Status = "expired"
	case view.TrafficQuota > 0 && view.TrafficUsed >= view.TrafficQuota:
		view.Status = "quota"
	}
	if includeSecrets {
		if password, err := decryptSiteAccountSecret(item.passwordCiphertext); err == nil {
			view.Password = password
		}
		if origin != "" {
			if dm := a.buildShareUserDanmakuSources(item, origin); len(dm) > 0 {
				view.DanmakuSources = dm
			}
		}
	}
	if view.Sites == nil {
		view.Sites = []ShareUserSite{}
	} else {
		copied := make([]ShareUserSite, len(view.Sites))
		copy(copied, view.Sites)
		view.Sites = copied
	}
	if view.DanmakuSourceIDs == nil {
		view.DanmakuSourceIDs = []int64{}
	} else {
		copiedDM := make([]int64, len(view.DanmakuSourceIDs))
		copy(copiedDM, view.DanmakuSourceIDs)
		view.DanmakuSourceIDs = copiedDM
	}
	if view.RoutePrefix != "" && origin != "" {
		view.RouteURL = strings.TrimRight(origin, "/") + view.RoutePrefix
	}
	activeSiteID := int64(0)
	if a.pm != nil && a.pm.shares != nil {
		activeSiteID = a.pm.shares.activeSiteID(item.ID)
	}
	if activeSiteID == 0 {
		for _, site := range view.Sites {
			if site.SiteEnabled {
				activeSiteID = site.SiteID
				break
			}
		}
	}
	for i := range view.Sites {
		if view.Sites[i].SiteID == activeSiteID && activeSiteID != 0 {
			view.Sites[i].Active = true
			view.ActiveSiteID = view.Sites[i].SiteID
			view.ActiveSiteName = view.Sites[i].SiteName
		}
	}
	return view
}

func formatShareBytes(value int64) string {
	if value <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	size := float64(value)
	unit := 0
	for size >= 1024 && unit < len(units)-1 {
		size /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d B", value)
	}
	return fmt.Sprintf("%.2f %s", size, units[unit])
}

func (a *App) shareUserText(item *storedShareUser, origin string) (string, error) {
	view := a.shareUserView(item, true, origin)
	var builder strings.Builder
	builder.WriteString("【Emby 分享账号】\n")
	builder.WriteString("用户名：" + view.Username + "\n")
	if view.Password != "" {
		builder.WriteString("密码：" + view.Password + "\n")
	} else {
		builder.WriteString("密码：（无法读取，请在面板中重设）\n")
	}
	if view.ExpiresAt != "" {
		builder.WriteString("到期时间：" + formatSiteAccountExpiryDate(view.ExpiresAt) + "\n")
	} else {
		builder.WriteString("到期时间：永久\n")
	}
	if view.TrafficQuota > 0 {
		quota := formatShareBytes(view.TrafficQuota)
		if view.ResetDay > 0 {
			quota += fmt.Sprintf("（每月 %d 日重置）", view.ResetDay)
		}
		builder.WriteString("流量额度：" + quota + "\n")
	}
	if view.MaxDevices > 0 {
		builder.WriteString(fmt.Sprintf("同时在线设备：最多 %d 台\n", view.MaxDevices))
	}
	if view.SpeedLimit > 0 {
		builder.WriteString(fmt.Sprintf("限速：%d Mbps\n", view.SpeedLimit))
	}
	if view.RouteURL != "" {
		builder.WriteString("服务器地址：" + view.RouteURL + "\n")
		if view.ActiveSiteName != "" {
			builder.WriteString("当前默认服务器：" + view.ActiveSiteName + "\n")
		}
		if len(view.Sites) > 1 {
			names := make([]string, 0, len(view.Sites))
			for _, assigned := range view.Sites {
				if assigned.SiteName != "" {
					names = append(names, assigned.SiteName)
				}
			}
			if len(names) > 0 {
				builder.WriteString("可用服务器：" + strings.Join(names, "、") + "\n")
			}
			if origin != "" {
				builder.WriteString("查看用量、到期和切换服务器：用上面的账号登录 " + origin + "\n")
			}
		} else if origin != "" {
			builder.WriteString("查看用量和到期：用上面的账号登录 " + origin + "\n")
		}
	} else {
		builder.WriteString("服务器地址：\n")
		for _, assigned := range view.Sites {
			site, err := a.db.GetSite(assigned.SiteID)
			if err != nil {
				continue
			}
			lines := a.siteProxyLinesFrom(*site, origin)
			if len(lines) == 0 {
				builder.WriteString("- " + site.Name + "：（入口未配置）\n")
				continue
			}
			for _, line := range lines {
				builder.WriteString("- " + site.Name + "：" + line + "\n")
			}
		}
		if origin != "" {
			builder.WriteString("查看用量和到期：用上面的账号登录 " + origin + "\n")
		}
	}
	if view.Status == "active" && len(view.DanmakuSources) > 0 {
		builder.WriteString("专属弹幕 API（随账号有效期联动）：\n")
		for _, dm := range view.DanmakuSources {
			builder.WriteString("- " + dm.Name + "：" + dm.URL + "\n")
		}
	}
	builder.WriteString("在 Emby 客户端中添加服务器地址后，用上面的用户名和密码登录。")
	return builder.String(), nil
}

// shareTextOrigin is the public origin written into the share text. An
// explicit public URL wins; otherwise it is the address the admin is using
// right now, which is also what the panel's site list shows. A loopback
// address (SSH tunnel) is useless to a friend, so it falls back to the
// configured panel domain.
func (a *App) shareTextOrigin(r *http.Request) string {
	if settings, err := a.db.telegramBotSettings(); err == nil && strings.TrimSpace(settings.PublicURL) != "" {
		return strings.TrimRight(strings.TrimSpace(settings.PublicURL), "/")
	}
	if r != nil && r.Host != "" {
		host := r.Host
		if hostname, _, err := net.SplitHostPort(host); err == nil {
			host = hostname
		}
		ip := net.ParseIP(strings.Trim(host, "[]"))
		if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			scheme := "http"
			if requestIsHTTPS(r, a.trustedProxies) {
				scheme = "https"
			}
			return scheme + "://" + r.Host
		}
	}
	if origin := a.panelAccessURL(); origin != "" {
		return origin
	}
	if a.panelListenPort > 0 {
		scheme := "http"
		if a.panelTLSEnabled {
			scheme = "https"
		}
		return scheme + "://127.0.0.1:" + strconv.Itoa(a.panelListenPort)
	}
	return ""
}

// GET/POST /api/share-users
func (a *App) handleShareUsers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		ids, err := a.db.listShareUserIDs()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "分享账号读取失败")
			return
		}
		origin := a.shareTextOrigin(r)
		result := make([]ShareUser, 0, len(ids))
		for _, id := range ids {
			item, err := a.db.getShareUser(id)
			if err != nil {
				continue
			}
			result = append(result, a.shareUserView(item, true, origin))
		}
		a.jsonOK(w, result)
	case http.MethodPost:
		var input ShareUserInput
		if err := decodeJSONBody(w, r, &input); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "invalid request")
			return
		}
		input, err := a.normalizeShareUserInput(input, 0, true)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		id, err := a.saveShareUser(0, input)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		a.reloadShareUsers()
		item, err := a.db.getShareUser(id)
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "分享账号读取失败")
			return
		}
		a.jsonResponse(w, http.StatusCreated, a.shareUserView(item, true, a.shareTextOrigin(r)))
	default:
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

var errInvalidShareUserOrder = errors.New("invalid share user order")

func (d *DB) reorderShareUsers(userIDs []int64) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT id FROM share_users`)
	if err != nil {
		return err
	}
	stored := make(map[int64]struct{})
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		stored[id] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(userIDs) != len(stored) {
		return fmt.Errorf("%w: user_ids must contain every share user exactly once", errInvalidShareUserOrder)
	}
	seen := make(map[int64]struct{}, len(userIDs))
	for position, id := range userIDs {
		if id <= 0 {
			return fmt.Errorf("%w: invalid share user id", errInvalidShareUserOrder)
		}
		if _, ok := stored[id]; !ok {
			return fmt.Errorf("%w: share user %d does not exist", errInvalidShareUserOrder, id)
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("%w: share user %d is duplicated", errInvalidShareUserOrder, id)
		}
		seen[id] = struct{}{}
		if _, err := tx.Exec(`UPDATE share_users SET sort_order=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, position+1, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PUT /api/share-users/reorder
func (a *App) handleShareUserReorder(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPut {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		UserIDs []int64 `json:"user_ids"`
	}
	if err := decodeJSONBody(w, r, &req); err != nil {
		a.jsonErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := a.db.reorderShareUsers(req.UserIDs); err != nil {
		if errors.Is(err, errInvalidShareUserOrder) {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		log.Printf("reorder share users: %v", err)
		a.jsonErr(w, http.StatusInternalServerError, "保存分享账号顺序失败")
		return
	}
	a.jsonOK(w, map[string]any{
		"user_ids": req.UserIDs,
	})
}

func (a *App) reloadShareUsers() {
	if a.pm == nil || a.pm.shares == nil {
		return
	}
	if err := a.pm.shares.reload(); err != nil {
		log.Printf("share users: reload after change: %v", err)
	}
}

// /api/share-users/{id}[/action]
func (a *App) handleShareUserByID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/share-users/"), "/"), "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		a.jsonErr(w, http.StatusBadRequest, "invalid share user id")
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	item, err := a.db.getShareUser(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			a.jsonErr(w, http.StatusNotFound, "分享账号不存在")
		} else {
			a.jsonErr(w, http.StatusInternalServerError, "分享账号读取失败")
		}
		return
	}
	shares := a.pm.shares
	switch action {
	case "":
		switch r.Method {
		case http.MethodGet:
			view := a.shareUserView(item, true, a.shareTextOrigin(r))
			if shares != nil {
				if sessions, err := shares.listSessions(id); err == nil {
					view.Sessions = sessions
				}
			}
			a.jsonOK(w, view)
		case http.MethodPut:
			var input ShareUserInput
			if err := decodeJSONBody(w, r, &input); err != nil {
				a.jsonErr(w, http.StatusBadRequest, "invalid request")
				return
			}
			input, err := a.normalizeShareUserInput(input, id, false)
			if err != nil {
				a.jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
			if _, err := a.saveShareUser(id, input); err != nil {
				a.jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
			a.reloadShareUsers()
			updated, err := a.db.getShareUser(id)
			if err != nil {
				a.jsonErr(w, http.StatusInternalServerError, "分享账号读取失败")
				return
			}
			if shares != nil {
				if input.Password != "" || !updated.Enabled {
					_, _ = shares.revokeSessions(id, 0, nil)
				} else {
					shares.reconcileSiteSwitch(id, false)
				}
				shares.enforce(time.Now())
				if input.MaxDevices > 0 {
					_ = shares.enforceDeviceLimit(id, 0, "", "", input.MaxDevices)
				}
			}
			a.jsonOK(w, a.shareUserView(updated, true, a.shareTextOrigin(r)))
		case http.MethodDelete:
			if shares != nil {
				_, _ = shares.revokeSessions(id, 0, nil)
			}
			if _, err := a.db.db.Exec(`DELETE FROM share_users WHERE id=?`, id); err != nil {
				a.jsonErr(w, http.StatusInternalServerError, "分享账号删除失败")
				return
			}
			a.reloadShareUsers()
			a.jsonOK(w, map[string]bool{"deleted": true})
		default:
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "text":
		if r.Method != http.MethodGet {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		text, err := a.shareUserText(item, a.shareTextOrigin(r))
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "分享信息生成失败")
			return
		}
		a.jsonOK(w, map[string]string{"text": text})
	case "kick":
		if r.Method != http.MethodPost {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		count := 0
		if shares != nil {
			count, err = shares.revokeSessions(id, 0, nil)
			if err != nil {
				a.jsonErr(w, http.StatusInternalServerError, "踢下线失败")
				return
			}
		}
		a.jsonOK(w, map[string]int{"revoked": count})
	case "sessions":
		// POST /api/share-users/{id}/sessions/{session_id}/kick
		if len(parts) != 4 || parts[3] != "kick" {
			a.jsonErr(w, http.StatusNotFound, "not found")
			return
		}
		if r.Method != http.MethodPost {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		revoked := false
		if shares != nil {
			revoked, err = shares.revokeUserSession(id, parts[2])
			if err != nil {
				a.jsonErr(w, http.StatusInternalServerError, "断开设备失败")
				return
			}
		}
		if !revoked {
			a.jsonErr(w, http.StatusNotFound, "设备会话不存在或已下线")
			return
		}
		a.jsonOK(w, map[string]bool{"revoked": true})
	case "reset-traffic":
		if r.Method != http.MethodPost {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if shares != nil {
			if err := shares.resetTraffic(id); err != nil {
				a.jsonErr(w, http.StatusInternalServerError, "流量重置失败")
				return
			}
		} else if _, err := a.db.db.Exec(`UPDATE share_users SET traffic_used=0 WHERE id=?`, id); err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "流量重置失败")
			return
		}
		updated, _ := a.db.getShareUser(id)
		if updated == nil {
			updated = item
		}
		a.jsonOK(w, a.shareUserView(updated, true, a.shareTextOrigin(r)))
	case "reset-danmaku-token":
		if r.Method != http.MethodPost {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if _, err := a.db.resetShareUserDanmakuToken(id); err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "弹幕凭证重置失败")
			return
		}
		updated, err := a.db.getShareUser(id)
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "分享账号读取失败")
			return
		}
		view := a.shareUserView(updated, true, a.shareTextOrigin(r))
		if shares != nil {
			if sessions, err := shares.listSessions(id); err == nil {
				view.Sessions = sessions
			}
		}
		a.jsonOK(w, view)
	case "renew":
		if r.Method != http.MethodPost {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			Days int `json:"days"`
		}
		if err := decodeJSONBody(w, r, &req); err != nil || req.Days < 1 || req.Days > 3650 {
			a.jsonErr(w, http.StatusBadRequest, "续期天数应为 1–3650")
			return
		}
		settings := a.db.currentSystemSettings()
		location := timezoneLocationByName(settings.ScheduleTimezoneName, settings.ScheduleTimezone)
		base := time.Now().In(location)
		if item.ExpiresAt != "" {
			if current := a.db.shareExpiryTime(item.ExpiresAt); current.After(base) {
				// shareExpiryTime moves date-only values to the next midnight.
				base = current.Add(-time.Second).In(location)
			}
		}
		next := base.AddDate(0, 0, req.Days).Format("2006-01-02")
		if _, err := a.db.db.Exec(`UPDATE share_users SET expires_at=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, next, id); err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "续期失败")
			return
		}
		a.reloadShareUsers()
		updated, _ := a.db.getShareUser(id)
		if updated == nil {
			updated = item
		}
		a.jsonOK(w, a.shareUserView(updated, true, a.shareTextOrigin(r)))
	default:
		a.jsonErr(w, http.StatusNotFound, "not found")
	}
}

// GET /api/live-sessions
func (a *App) handleLiveSessions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.pm == nil || a.pm.shares == nil {
		a.jsonOK(w, []liveShareSessionView{})
		return
	}
	sessions, err := a.pm.shares.listAllLiveSessions()
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "获取在线会话失败")
		return
	}
	if sessions == nil {
		sessions = []liveShareSessionView{}
	}
	a.jsonOK(w, sessions)
}
