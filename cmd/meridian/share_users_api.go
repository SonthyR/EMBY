package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
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
}

type ShareUser struct {
	ID           int64                `json:"id"`
	Username     string               `json:"username"`
	Password     string               `json:"password,omitempty"` // #nosec G117 -- authorized share-user memo for the administrator.
	Note         string               `json:"note"`
	Enabled      bool                 `json:"enabled"`
	ExpiresAt    string               `json:"expires_at"`
	TrafficQuota int64                `json:"traffic_quota"`
	TrafficUsed  int64                `json:"traffic_used"`
	Status       string               `json:"status"`
	SessionCount int                  `json:"session_count"`
	MaxDevices   int                  `json:"max_devices"`
	SpeedLimit   int                  `json:"speed_limit"`
	ResetDay     int                  `json:"traffic_reset_day"`
	NextResetAt  string               `json:"next_reset_at,omitempty"`
	PortalPath   string               `json:"portal_path"`
	PortalURL    string               `json:"portal_url,omitempty"`
	Sites        []ShareUserSite      `json:"sites"`
	Sessions     []shareStoredSession `json:"sessions,omitempty"`
	CreatedAt    string               `json:"created_at"`
	UpdatedAt    string               `json:"updated_at"`
}

type ShareUserSiteInput struct {
	SiteID        int64 `json:"site_id"`
	SiteAccountID int64 `json:"site_account_id"`
}

type ShareUserInput struct {
	Username     string               `json:"username"`
	Password     string               `json:"password"` // #nosec G117 -- request-only credential DTO.
	Note         string               `json:"note"`
	Enabled      *bool                `json:"enabled"`
	ExpiresAt    string               `json:"expires_at"`
	TrafficQuota int64                `json:"traffic_quota"`
	MaxDevices   int                  `json:"max_devices"`
	SpeedLimit   int                  `json:"speed_limit"`
	ResetDay     int                  `json:"traffic_reset_day"`
	Sites        []ShareUserSiteInput `json:"sites"`
}

type storedShareUser struct {
	ShareUser
	passwordCiphertext string
	portalToken        string
}

func (d *DB) getShareUser(id int64) (*storedShareUser, error) {
	return d.getShareUserWhere(`id=?`, id)
}

func (d *DB) getShareUserByPortalToken(token string) (*storedShareUser, error) {
	if len(token) != 32 {
		return nil, sql.ErrNoRows
	}
	return d.getShareUserWhere(`portal_token=? AND portal_token<>''`, token)
}

func (d *DB) getShareUserWhere(where string, arg any) (*storedShareUser, error) {
	var item storedShareUser
	var enabled int
	err := d.db.QueryRow(`SELECT id, username, password_ciphertext, note, enabled, expires_at, traffic_quota, traffic_used, max_devices, traffic_reset_day, speed_limit, portal_token, COALESCE(created_at, ''), COALESCE(updated_at, '') FROM share_users WHERE `+where, arg).
		Scan(&item.ID, &item.Username, &item.passwordCiphertext, &item.Note, &enabled, &item.ExpiresAt, &item.TrafficQuota, &item.TrafficUsed, &item.MaxDevices, &item.ResetDay, &item.SpeedLimit, &item.portalToken, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		return nil, err
	}
	item.Enabled = enabled != 0
	if item.portalToken != "" {
		item.PortalPath = "/share/" + item.portalToken
	}
	sites, err := d.shareUserSites(item.ID)
	if err != nil {
		return nil, err
	}
	item.Sites = sites
	return &item, nil
}

func (d *DB) shareUserSites(id int64) ([]ShareUserSite, error) {
	rows, err := d.db.Query(`SELECT a.site_id, a.site_account_id, s.name, s.enabled, COALESCE(c.account_name, '') FROM share_user_sites a JOIN sites s ON s.id=a.site_id LEFT JOIN site_accounts c ON c.id=a.site_account_id AND c.site_id=a.site_id WHERE a.share_user_id=? ORDER BY s.sort_order, s.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]ShareUserSite, 0)
	for rows.Next() {
		var item ShareUserSite
		var enabled int
		if err := rows.Scan(&item.SiteID, &item.SiteAccountID, &item.SiteName, &enabled, &item.AccountName); err != nil {
			return nil, err
		}
		item.SiteEnabled = enabled != 0
		result = append(result, item)
	}
	return result, rows.Err()
}

func (d *DB) listShareUserIDs() ([]int64, error) {
	rows, err := d.db.Query(`SELECT id FROM share_users ORDER BY id`)
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

func (a *App) normalizeShareUserInput(input ShareUserInput, creating bool) (ShareUserInput, error) {
	input.Username = strings.TrimSpace(input.Username)
	input.Note = strings.TrimSpace(input.Note)
	input.ExpiresAt = strings.TrimSpace(input.ExpiresAt)
	if !validShareUsername(input.Username) {
		return input, errors.New("用户名需要 1–64 个字符，且不能包含空格")
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
		hash, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
		if err != nil {
			return 0, err
		}
		cipher, err := encryptSiteAccountSecret(input.Password)
		if err != nil {
			return 0, err
		}
		result, err := tx.Exec(`INSERT INTO share_users(username, password_hash, password_ciphertext, note, enabled, expires_at, traffic_quota, max_devices, speed_limit, traffic_reset_day, traffic_reset_period, portal_token) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			input.Username, string(hash), cipher, input.Note, sqliteBool(enabled), input.ExpiresAt, input.TrafficQuota, input.MaxDevices, input.SpeedLimit, input.ResetDay, resetPeriod, portalToken)
		if err != nil {
			if isSQLiteUniqueConstraintError(err) {
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
		result, err := tx.Exec(`UPDATE share_users SET username=?, note=?, enabled=?, expires_at=?, traffic_quota=?, max_devices=?, speed_limit=?, traffic_reset_period=CASE WHEN traffic_reset_day<>? THEN ? ELSE traffic_reset_period END, traffic_reset_day=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
			input.Username, input.Note, sqliteBool(enabled), input.ExpiresAt, input.TrafficQuota, input.MaxDevices, input.SpeedLimit, input.ResetDay, resetPeriod, input.ResetDay, id)
		if err != nil {
			if isSQLiteUniqueConstraintError(err) {
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
	for _, site := range input.Sites {
		if _, err := tx.Exec(`INSERT INTO share_user_sites(share_user_id, site_id, site_account_id) VALUES(?,?,?)`, id, site.SiteID, site.SiteAccountID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (a *App) shareUserView(item *storedShareUser, includeSecrets bool) ShareUser {
	view := item.ShareUser
	if a.pm != nil && a.pm.shares != nil {
		used, _ := a.pm.shares.usage(item.ID)
		if used > view.TrafficUsed {
			view.TrafficUsed = used
		}
		view.SessionCount = a.pm.shares.deviceCount(item.ID)
	}
	if view.ResetDay > 0 {
		if _, next := shareResetPeriod(time.Now().In(a.db.shareLocation()), view.ResetDay); !next.IsZero() {
			view.NextResetAt = next.Format("2006-01-02")
		}
	}
	if view.PortalPath != "" {
		if origin := a.sharePortalOrigin(); origin != "" {
			view.PortalURL = origin + view.PortalPath
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
	}
	if view.Sites == nil {
		view.Sites = []ShareUserSite{}
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

func (a *App) shareUserText(item *storedShareUser) (string, error) {
	view := a.shareUserView(item, true)
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
	builder.WriteString("服务器地址：\n")
	for _, assigned := range view.Sites {
		site, err := a.db.GetSite(assigned.SiteID)
		if err != nil {
			continue
		}
		lines := a.siteProxyLines(*site)
		if len(lines) == 0 {
			builder.WriteString("- " + site.Name + "：（入口未配置）\n")
			continue
		}
		for _, line := range lines {
			builder.WriteString("- " + site.Name + "：" + line + "\n")
		}
	}
	if view.PortalURL != "" {
		builder.WriteString("自助查询（到期、流量、地址）：" + view.PortalURL + "\n")
	}
	builder.WriteString("在 Emby 客户端中添加服务器地址后，用上面的用户名和密码登录。")
	return builder.String(), nil
}

// sharePortalOrigin is the public origin friends reach the panel on, chosen
// the same way as the site addresses in the share text.
func (a *App) sharePortalOrigin() string {
	if settings, err := a.db.telegramBotSettings(); err == nil && strings.TrimSpace(settings.PublicURL) != "" {
		return strings.TrimRight(strings.TrimSpace(settings.PublicURL), "/")
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
		result := make([]ShareUser, 0, len(ids))
		for _, id := range ids {
			item, err := a.db.getShareUser(id)
			if err != nil {
				continue
			}
			result = append(result, a.shareUserView(item, true))
		}
		a.jsonOK(w, result)
	case http.MethodPost:
		var input ShareUserInput
		if err := decodeJSONBody(w, r, &input); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "invalid request")
			return
		}
		input, err := a.normalizeShareUserInput(input, true)
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
		a.jsonResponse(w, http.StatusCreated, a.shareUserView(item, true))
	default:
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
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
			view := a.shareUserView(item, true)
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
			input, err := a.normalizeShareUserInput(input, false)
			if err != nil {
				a.jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
			if _, err := a.saveShareUser(id, input); err != nil {
				a.jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
			a.reloadShareUsers()
			if shares != nil {
				keep := make(map[int64]int64, len(input.Sites))
				for _, site := range input.Sites {
					keep[site.SiteID] = site.SiteAccountID
				}
				changedPassword := input.Password != ""
				if changedPassword {
					keep = map[int64]int64{}
				}
				_, _ = shares.revokeSessions(id, 0, keep)
				shares.enforce(time.Now())
				if input.MaxDevices > 0 {
					_ = shares.enforceDeviceLimit(id, 0, "", "", input.MaxDevices)
				}
			}
			updated, err := a.db.getShareUser(id)
			if err != nil {
				a.jsonErr(w, http.StatusInternalServerError, "分享账号读取失败")
				return
			}
			a.jsonOK(w, a.shareUserView(updated, true))
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
		text, err := a.shareUserText(item)
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "分享信息生成失败")
			return
		}
		a.jsonOK(w, map[string]string{"text": text})
	case "reset-portal":
		if r.Method != http.MethodPost {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		token, err := newSharePortalToken()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "生成链接失败")
			return
		}
		if _, err := a.db.db.Exec(`UPDATE share_users SET portal_token=? WHERE id=?`, token, id); err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "生成链接失败")
			return
		}
		updated, _ := a.db.getShareUser(id)
		if updated == nil {
			updated = item
		}
		a.jsonOK(w, a.shareUserView(updated, true))
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
		a.jsonOK(w, a.shareUserView(updated, true))
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
		a.jsonOK(w, a.shareUserView(updated, true))
	default:
		a.jsonErr(w, http.StatusNotFound, "not found")
	}
}
