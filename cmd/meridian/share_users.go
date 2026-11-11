package main

// Share users let the administrator hand an Emby site to friends without
// giving out the upstream account. A friend signs in to the proxied site with
// a Meridian-issued username and password; Meridian verifies it, swaps the
// login body for the site's saved upstream account and remembers which share
// user owns the upstream access token that comes back. Every later request
// carrying that token is checked against the share user's state (enabled,
// expiry, traffic quota) and its bytes are counted against that user. When a
// share user is blocked, the tokens are rejected locally and logged out
// upstream so they cannot be replayed against the source directly.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	shareLoginBodyLimit       = 64 << 10
	shareLoginResponseLimit   = 1 << 20
	shareLoginFailureLimit    = 8
	shareLoginFailureWindow   = 15 * time.Minute
	shareLogoutTimeout        = 10 * time.Second
	shareSessionTouchInterval = time.Minute
)

const shareUserSchemaSQL = `
CREATE TABLE IF NOT EXISTS share_users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	username TEXT NOT NULL UNIQUE COLLATE NOCASE,
	password_hash TEXT NOT NULL,
	password_ciphertext TEXT NOT NULL DEFAULT '',
	note TEXT NOT NULL DEFAULT '',
	enabled INTEGER NOT NULL DEFAULT 1,
	expires_at TEXT NOT NULL DEFAULT '',
	traffic_quota INTEGER NOT NULL DEFAULT 0,
	traffic_used INTEGER NOT NULL DEFAULT 0,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS share_user_sites (
	share_user_id INTEGER NOT NULL REFERENCES share_users(id) ON DELETE CASCADE,
	site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
	site_account_id INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(share_user_id, site_id)
);
CREATE INDEX IF NOT EXISTS idx_share_user_sites_site ON share_user_sites(site_id);
CREATE TABLE IF NOT EXISTS share_user_sessions (
	token_hash TEXT PRIMARY KEY,
	share_user_id INTEGER NOT NULL REFERENCES share_users(id) ON DELETE CASCADE,
	site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
	token_ciphertext TEXT NOT NULL,
	device_name TEXT NOT NULL DEFAULT '',
	client TEXT NOT NULL DEFAULT '',
	client_ip TEXT NOT NULL DEFAULT '',
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	last_seen_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_share_user_sessions_user ON share_user_sessions(share_user_id);
CREATE TABLE IF NOT EXISTS share_revoked_tokens (
	token_hash TEXT PRIMARY KEY,
	site_id INTEGER NOT NULL DEFAULT 0,
	revoked_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`

// Revoked share tokens stay rejected locally for this long, even if the
// upstream logout failed, so a kicked device cannot keep using its token.
const shareRevokedTokenRetention = 90 * 24 * time.Hour

func ensureShareUserSchema(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, shareUserSchemaSQL); err != nil {
		return err
	}
	for _, column := range []struct{ table, name, sql string }{
		{"share_users", "max_devices", "ALTER TABLE share_users ADD COLUMN max_devices INTEGER NOT NULL DEFAULT 0"},
		{"share_users", "traffic_reset_day", "ALTER TABLE share_users ADD COLUMN traffic_reset_day INTEGER NOT NULL DEFAULT 0"},
		{"share_users", "traffic_reset_period", "ALTER TABLE share_users ADD COLUMN traffic_reset_period TEXT NOT NULL DEFAULT ''"},
		{"share_users", "portal_token", "ALTER TABLE share_users ADD COLUMN portal_token TEXT NOT NULL DEFAULT ''"},
		{"share_users", "speed_limit", "ALTER TABLE share_users ADD COLUMN speed_limit INTEGER NOT NULL DEFAULT 0"},
		{"share_user_sessions", "device_id", "ALTER TABLE share_user_sessions ADD COLUMN device_id TEXT NOT NULL DEFAULT ''"},
	} {
		var exists int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('"+column.table+"') WHERE name=?", column.name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := conn.ExecContext(ctx, column.sql); err != nil {
				return err
			}
		}
	}
	if _, err := conn.ExecContext(ctx, "UPDATE share_users SET portal_token=lower(hex(randomblob(16))) WHERE portal_token=''"); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS idx_share_users_portal ON share_users(portal_token) WHERE portal_token <> ''")
	return err
}

func newSharePortalToken() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

// shareResetPeriod names the traffic cycle that contains now for a monthly
// reset on day (1–28): the cycle starts on that day of the month.
func shareResetPeriod(now time.Time, day int) (string, time.Time) {
	if day < 1 || day > 28 {
		return "", time.Time{}
	}
	start := time.Date(now.Year(), now.Month(), day, 0, 0, 0, 0, now.Location())
	if now.Before(start) {
		start = start.AddDate(0, -1, 0)
	}
	return start.Format("2006-01-02"), start.AddDate(0, 1, 0)
}

func (d *DB) shareLocation() *time.Location {
	settings := d.currentSystemSettings()
	return timezoneLocationByName(settings.ScheduleTimezoneName, settings.ScheduleTimezone)
}

// ---------------------------------------------------------------------------
// Runtime state

type shareSessionEntry struct {
	userID int64
	siteID int64
}

type shareUserState struct {
	id int64
	// The fields below are guarded by shareUserRuntime.mu.
	username      string
	passwordHash  string
	enabled       bool
	expiresAt     time.Time
	quota         int64
	persistedUsed int64
	maxDevices    int
	speedLimit    int               // Mbps, 0 = unlimited
	limiter       *shareRateLimiter // nil when unlimited
	resetDay      int
	resetPeriod   string
	sites         map[int64]int64 // site ID -> preferred site_account ID (0 = first usable)

	pending atomic.Int64 // bytes metered but not yet written to share_users.traffic_used
}

type shareLoginFailure struct {
	count int
	first time.Time
}

type shareUserRuntime struct {
	db *DB
	pm *ProxyManager

	mu       sync.RWMutex
	users    map[int64]*shareUserState
	byName   map[string]int64
	sessions map[string]shareSessionEntry
	revoked  map[string]int64 // token hash -> site ID
	lastSeen map[string]time.Time
	failures map[string]*shareLoginFailure
	revoking map[int64]bool
	flushMu  sync.Mutex

	capMu        sync.Mutex
	capabilities map[string]shareCapabilityOwner
	loadError    error
}

func newShareUserRuntime(db *DB, pm *ProxyManager) *shareUserRuntime {
	rt := &shareUserRuntime{
		db:       db,
		pm:       pm,
		users:    make(map[int64]*shareUserState),
		byName:   make(map[string]int64),
		sessions: make(map[string]shareSessionEntry),
		revoked:  make(map[string]int64),
		lastSeen: make(map[string]time.Time),
		failures: make(map[string]*shareLoginFailure),
		revoking: make(map[int64]bool),
	}
	if db != nil {
		if err := rt.reload(); err != nil {
			rt.loadError = err
			log.Printf("share users: initial load failed: %v", err)
		}
	}
	return rt
}

func shareTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (d *DB) shareExpiryTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	parsed, err := d.parseSiteAccountExpiry(value)
	if err != nil {
		// An unreadable expiry fails closed: treat it as already expired.
		return time.Unix(1, 0)
	}
	if len(value) == len("2006-01-02") {
		// A date-only expiry stays valid through the whole day.
		parsed = parsed.AddDate(0, 0, 1)
	}
	return parsed
}

// reload refreshes users, site assignments and sessions from the database.
// Existing *shareUserState values are updated in place so in-flight traffic
// meters keep pointing at the live record.
func (rt *shareUserRuntime) reload() error {
	if rt == nil || rt.db == nil {
		return nil
	}
	type row struct {
		id                      int64
		username, hash, expires string
		enabled                 bool
		quota, used             int64
		maxDevices, resetDay    int
		speedLimit              int
		resetPeriod             string
	}
	rows, err := rt.db.db.Query(`SELECT id, username, password_hash, enabled, expires_at, traffic_quota, traffic_used, max_devices, traffic_reset_day, traffic_reset_period, speed_limit FROM share_users`)
	if err != nil {
		return err
	}
	var loaded []row
	for rows.Next() {
		var item row
		var enabled int
		if err := rows.Scan(&item.id, &item.username, &item.hash, &enabled, &item.expires, &item.quota, &item.used, &item.maxDevices, &item.resetDay, &item.resetPeriod, &item.speedLimit); err != nil {
			rows.Close()
			return err
		}
		item.enabled = enabled != 0
		loaded = append(loaded, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	siteRows, err := rt.db.db.Query(`SELECT share_user_id, site_id, site_account_id FROM share_user_sites`)
	if err != nil {
		return err
	}
	assignments := make(map[int64]map[int64]int64)
	for siteRows.Next() {
		var userID, siteID, accountID int64
		if err := siteRows.Scan(&userID, &siteID, &accountID); err != nil {
			siteRows.Close()
			return err
		}
		if assignments[userID] == nil {
			assignments[userID] = make(map[int64]int64)
		}
		assignments[userID][siteID] = accountID
	}
	if err := siteRows.Err(); err != nil {
		siteRows.Close()
		return err
	}
	siteRows.Close()

	sessionRows, err := rt.db.db.Query(`SELECT token_hash, share_user_id, site_id FROM share_user_sessions`)
	if err != nil {
		return err
	}
	sessions := make(map[string]shareSessionEntry)
	for sessionRows.Next() {
		var hash string
		var entry shareSessionEntry
		if err := sessionRows.Scan(&hash, &entry.userID, &entry.siteID); err != nil {
			sessionRows.Close()
			return err
		}
		sessions[hash] = entry
	}
	if err := sessionRows.Err(); err != nil {
		sessionRows.Close()
		return err
	}
	sessionRows.Close()

	revokedRows, err := rt.db.db.Query(`SELECT token_hash, site_id FROM share_revoked_tokens`)
	if err != nil {
		return err
	}
	revoked := make(map[string]int64)
	for revokedRows.Next() {
		var hash string
		var siteID int64
		if err := revokedRows.Scan(&hash, &siteID); err != nil {
			revokedRows.Close()
			return err
		}
		revoked[hash] = siteID
	}
	if err := revokedRows.Err(); err != nil {
		revokedRows.Close()
		return err
	}
	revokedRows.Close()

	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.revoked = revoked
	seen := make(map[int64]bool, len(loaded))
	byName := make(map[string]int64, len(loaded))
	for _, item := range loaded {
		seen[item.id] = true
		state := rt.users[item.id]
		if state == nil {
			state = &shareUserState{id: item.id}
			rt.users[item.id] = state
		}
		state.username = item.username
		state.passwordHash = item.hash
		state.enabled = item.enabled
		state.expiresAt = rt.db.shareExpiryTime(item.expires)
		state.quota = item.quota
		state.persistedUsed = item.used
		state.maxDevices = item.maxDevices
		state.speedLimit = item.speedLimit
		if item.speedLimit > 0 {
			if state.limiter == nil {
				state.limiter = &shareRateLimiter{}
			}
			state.limiter.setRate(item.speedLimit)
		} else {
			state.limiter = nil
		}
		state.resetDay = item.resetDay
		state.resetPeriod = item.resetPeriod
		state.sites = assignments[item.id]
		if state.sites == nil {
			state.sites = map[int64]int64{}
		}
		byName[strings.ToLower(item.username)] = item.id
	}
	for id := range rt.users {
		if !seen[id] {
			delete(rt.users, id)
		}
	}
	rt.byName = byName
	rt.sessions = sessions
	for hash := range rt.lastSeen {
		if _, ok := sessions[hash]; !ok {
			delete(rt.lastSeen, hash)
		}
	}
	rt.loadError = nil
	return nil
}

// blockReasonLocked reports why a share user may not use the proxy right now.
// The caller holds rt.mu (read or write).
func (s *shareUserState) blockReasonLocked(now time.Time) string {
	if s == nil {
		return "missing"
	}
	if !s.enabled {
		return "disabled"
	}
	if !s.expiresAt.IsZero() && !now.Before(s.expiresAt) {
		return "expired"
	}
	if s.quota > 0 && s.persistedUsed+s.pending.Load() >= s.quota {
		return "quota"
	}
	return ""
}

func shareBlockMessage(reason string) string {
	switch reason {
	case "disabled":
		return "分享账号已停用"
	case "expired":
		return "分享账号已过期"
	case "quota":
		return "分享账号流量已用完"
	case "site":
		return "分享账号无权访问此服务器"
	case "account":
		return "服务器暂未配置可用的源站账号"
	default:
		return "分享账号不可用"
	}
}

func writeShareJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]string{"error": message})
	_, _ = w.Write(body)
}

// ---------------------------------------------------------------------------
// Emby request parsing

func isShareLoginRequest(r *http.Request) bool {
	if r == nil || r.Method != http.MethodPost || r.URL == nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(strings.TrimRight(r.URL.Path, "/")), "/users/authenticatebyname")
}

func embyAuthorizationAttribute(value, name string) string {
	value = strings.TrimSpace(value)
	offset := 0
	for offset < len(value) && isEmbyAuthToken(value[offset]) {
		offset++
	}
	scheme := value[:offset]
	if !strings.EqualFold(scheme, "MediaBrowser") && !strings.EqualFold(scheme, "Emby") {
		return ""
	}
	attributes, ok := parseEmbyAuthorizationAttributes(value, offset)
	if !ok {
		return ""
	}
	for _, attribute := range attributes {
		if strings.EqualFold(attribute.name, name) {
			return value[attribute.valueStart:attribute.valueEnd]
		}
	}
	return ""
}

func embyRequestAuthorizationAttribute(r *http.Request, name string) string {
	for _, header := range []string{"X-Emby-Authorization", "Authorization"} {
		for _, value := range r.Header.Values(header) {
			if found := embyAuthorizationAttribute(value, name); found != "" {
				return found
			}
		}
	}
	return ""
}

// embyRequestTokens returns every access token candidate the request carries.
func embyRequestTokens(r *http.Request) []string {
	if r == nil {
		return nil
	}
	tokens := make([]string, 0, 2)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 512 {
			return
		}
		for _, existing := range tokens {
			if existing == value {
				return
			}
		}
		tokens = append(tokens, value)
	}
	add(r.Header.Get("X-Emby-Token"))
	add(r.Header.Get("X-MediaBrowser-Token"))
	add(embyRequestAuthorizationAttribute(r, "Token"))
	if r.URL != nil && r.URL.RawQuery != "" {
		for key, values := range r.URL.Query() {
			switch strings.ToLower(key) {
			case "api_key", "apikey", "x-emby-token", "x-mediabrowser-token":
				for _, value := range values {
					add(value)
				}
			}
		}
	}
	return tokens
}

type shareLoginBody struct {
	json     map[string]any
	form     url.Values
	username string
	password string
}

func parseShareLoginBody(contentType string, body []byte) (*shareLoginBody, bool) {
	trimmed := bytes.TrimSpace(body)
	if strings.Contains(strings.ToLower(contentType), "json") || (len(trimmed) > 0 && trimmed[0] == '{') {
		values := map[string]any{}
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.UseNumber()
		if err := decoder.Decode(&values); err != nil {
			return nil, false
		}
		parsed := &shareLoginBody{json: values}
		for key, value := range values {
			text, _ := value.(string)
			switch strings.ToLower(key) {
			case "username":
				parsed.username = text
			case "pw":
				parsed.password = text
			}
		}
		return parsed, true
	}
	form, err := url.ParseQuery(string(trimmed))
	if err != nil {
		return nil, false
	}
	parsed := &shareLoginBody{form: form}
	for key, values := range form {
		if len(values) == 0 {
			continue
		}
		switch strings.ToLower(key) {
		case "username":
			parsed.username = values[0]
		case "pw":
			parsed.password = values[0]
		}
	}
	return parsed, true
}

// rewrite replaces the credentials and drops legacy hashed password fields so
// the upstream only sees the saved account.
func (b *shareLoginBody) rewrite(username, password string) ([]byte, string) {
	if b.json != nil {
		next := make(map[string]any, len(b.json))
		for key, value := range b.json {
			switch strings.ToLower(key) {
			case "username", "pw", "password", "passwordmd5":
				continue
			}
			next[key] = value
		}
		next["Username"] = username
		next["Pw"] = password
		encoded, _ := json.Marshal(next)
		return encoded, "application/json"
	}
	next := url.Values{}
	for key, values := range b.form {
		switch strings.ToLower(key) {
		case "username", "pw", "password", "passwordmd5":
			continue
		}
		next[key] = values
	}
	next.Set("Username", username)
	next.Set("Pw", password)
	return []byte(next.Encode()), "application/x-www-form-urlencoded"
}

type replayBody struct {
	io.Reader
	closer io.Closer
}

func (b *replayBody) Close() error {
	if b.closer == nil {
		return nil
	}
	return b.closer.Close()
}

// ---------------------------------------------------------------------------
// Proxy interception

type shareLoginPending struct {
	rt       *shareUserRuntime
	user     *shareUserState
	siteID   int64
	deviceID string
	device   string
	client   string
	clientIP string
}

// intercept runs before a proxied request is forwarded. It returns the request
// to forward, a pending login when the request is a share-user sign-in, the
// share user to meter traffic against, and whether a response was already
// written (the request must then not be forwarded).
func (rt *shareUserRuntime) intercept(w http.ResponseWriter, r *http.Request, site Site, clientIP string) (*http.Request, *shareLoginPending, *shareUserState, bool) {
	if rt == nil {
		return r, nil, nil, false
	}
	if isShareLoginRequest(r) {
		return rt.interceptLogin(w, r, site, clientIP)
	}
	tokens := embyRequestTokens(r)
	now := time.Now()
	for _, token := range tokens {
		hash := shareTokenHash(token)
		rt.mu.RLock()
		entry, ok := rt.sessions[hash]
		revokedSite, revoked := rt.revoked[hash]
		if !ok && revoked && (revokedSite == 0 || revokedSite == site.ID) {
			rt.mu.RUnlock()
			writeShareJSONError(w, http.StatusUnauthorized, "登录已失效，请重新登录")
			return r, nil, nil, true
		}
		if !ok || entry.siteID != site.ID {
			rt.mu.RUnlock()
			continue
		}
		state := rt.users[entry.userID]
		reason := state.blockReasonLocked(now)
		if reason == "" {
			if _, allowed := state.sites[site.ID]; !allowed {
				reason = "site"
			}
		}
		last := rt.lastSeen[hash]
		rt.mu.RUnlock()
		if reason != "" {
			if state != nil {
				rt.revokeAsync(entry.userID, 0, reason)
			}
			writeShareJSONError(w, http.StatusUnauthorized, shareBlockMessage(reason))
			return r, nil, nil, true
		}
		if now.Sub(last) >= shareSessionTouchInterval {
			rt.mu.Lock()
			rt.lastSeen[hash] = now
			rt.mu.Unlock()
		}
		return r, nil, state, false
	}
	// Dynamic playback routes carry no Emby token; attribute them through the
	// capability handed to the share user earlier.
	if userID, ok := rt.capabilityOwner(r.URL.Path, site.ID); ok {
		rt.mu.RLock()
		state := rt.users[userID]
		reason := state.blockReasonLocked(now)
		if reason == "" {
			if _, allowed := state.sites[site.ID]; !allowed {
				reason = "site"
			}
		}
		rt.mu.RUnlock()
		if reason != "" {
			writeShareJSONError(w, http.StatusUnauthorized, shareBlockMessage(reason))
			return r, nil, nil, true
		}
		return r, nil, state, false
	}
	return r, nil, nil, false
}

func (rt *shareUserRuntime) loginBlocked(key string, now time.Time) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	failure := rt.failures[key]
	if failure == nil {
		return false
	}
	if now.Sub(failure.first) > shareLoginFailureWindow {
		delete(rt.failures, key)
		return false
	}
	return failure.count >= shareLoginFailureLimit
}

func (rt *shareUserRuntime) recordLoginFailure(key string, now time.Time) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.failures) > 10000 {
		for k, v := range rt.failures {
			if now.Sub(v.first) > shareLoginFailureWindow {
				delete(rt.failures, k)
			}
		}
	}
	failure := rt.failures[key]
	if failure == nil || now.Sub(failure.first) > shareLoginFailureWindow {
		failure = &shareLoginFailure{first: now}
		rt.failures[key] = failure
	}
	failure.count++
}

func (rt *shareUserRuntime) clearLoginFailures(key string) {
	rt.mu.Lock()
	delete(rt.failures, key)
	rt.mu.Unlock()
}

func (rt *shareUserRuntime) interceptLogin(w http.ResponseWriter, r *http.Request, site Site, clientIP string) (*http.Request, *shareLoginPending, *shareUserState, bool) {
	if r.Body == nil {
		return r, nil, nil, false
	}
	original := r.Body
	buffered, err := io.ReadAll(io.LimitReader(original, shareLoginBodyLimit+1))
	restore := func() *http.Request {
		r.Body = &replayBody{Reader: io.MultiReader(bytes.NewReader(buffered), original), closer: original}
		return r
	}
	if err != nil || len(buffered) > shareLoginBodyLimit {
		return restore(), nil, nil, false
	}
	parsed, ok := parseShareLoginBody(r.Header.Get("Content-Type"), buffered)
	if !ok || strings.TrimSpace(parsed.username) == "" {
		return restore(), nil, nil, false
	}
	lookup := strings.ToLower(strings.TrimSpace(parsed.username))
	rt.mu.RLock()
	userID, known := rt.byName[lookup]
	state := rt.users[userID]
	var hash string
	var assigned bool
	var preferredAccount int64
	if known && state != nil {
		hash = state.passwordHash
		preferredAccount, assigned = state.sites[site.ID]
	}
	rt.mu.RUnlock()
	if !known || state == nil || !assigned {
		// Not a share user for this site: keep the normal pass-through login.
		return restore(), nil, nil, false
	}
	_ = original.Close()

	now := time.Now()
	failureKey := clientIP + "|" + lookup
	if rt.loginBlocked(failureKey, now) {
		w.Header().Set("Retry-After", strconv.Itoa(int(shareLoginFailureWindow/time.Second)))
		writeShareJSONError(w, http.StatusTooManyRequests, "登录失败次数过多，请稍后再试")
		return r, nil, nil, true
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(parsed.password)) != nil {
		rt.recordLoginFailure(failureKey, now)
		writeShareJSONError(w, http.StatusUnauthorized, "用户名或密码错误")
		return r, nil, nil, true
	}
	rt.clearLoginFailures(failureKey)
	rt.mu.RLock()
	reason := state.blockReasonLocked(now)
	rt.mu.RUnlock()
	if reason != "" {
		writeShareJSONError(w, http.StatusUnauthorized, shareBlockMessage(reason))
		return r, nil, nil, true
	}
	upstreamUser, upstreamPassword, err := rt.upstreamCredentials(site.ID, preferredAccount)
	if err != nil {
		log.Printf("[%s] share login for %s: %v", site.Name, lookup, err)
		writeShareJSONError(w, http.StatusServiceUnavailable, shareBlockMessage("account"))
		return r, nil, nil, true
	}
	body, contentType := parsed.rewrite(upstreamUser, upstreamPassword)
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	r.Header.Set("Content-Type", contentType)
	// The login response is parsed for the access token, so keep it plain.
	r.Header.Set("Accept-Encoding", "identity")
	device := embyRequestAuthorizationAttribute(r, "Device")
	if device == "" {
		device = r.Header.Get("X-Emby-Device-Name")
	}
	client := embyRequestAuthorizationAttribute(r, "Client")
	if client == "" {
		client = r.Header.Get("X-Emby-Client")
	}
	if decoded, err := url.QueryUnescape(device); err == nil {
		device = decoded
	}
	deviceID := embyRequestAuthorizationAttribute(r, "DeviceId")
	if deviceID == "" {
		deviceID = r.Header.Get("X-Emby-Device-Id")
	}
	return r, &shareLoginPending{rt: rt, user: state, siteID: site.ID, deviceID: truncateShareText(deviceID, 128), device: truncateShareText(device, 128), client: truncateShareText(client, 128), clientIP: clientIP}, state, false
}

func truncateShareText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

// upstreamCredentials picks the saved account used to sign in upstream.
func (rt *shareUserRuntime) upstreamCredentials(siteID, preferredAccount int64) (string, string, error) {
	accounts, err := rt.db.listSiteAccounts(siteID)
	if err != nil {
		return "", "", err
	}
	pick := func(item storedSiteAccount) (string, string, bool) {
		if item.Username == "" || item.PasswordCiphertext == "" {
			return "", "", false
		}
		password, err := decryptSiteAccountSecret(item.PasswordCiphertext)
		if err != nil || password == "" {
			return "", "", false
		}
		return item.Username, password, true
	}
	if preferredAccount > 0 {
		for _, item := range accounts {
			if item.ID == preferredAccount {
				if username, password, ok := pick(item); ok {
					return username, password, nil
				}
			}
		}
	}
	for _, item := range accounts {
		if username, password, ok := pick(item); ok {
			return username, password, nil
		}
	}
	return "", "", errors.New("site has no saved account with a password")
}

// shareLoginCapture buffers the upstream login response so the access token
// can be recorded before the response reaches the client.
type shareLoginCapture struct {
	target      http.ResponseWriter
	status      int
	buffer      bytes.Buffer
	passthrough bool
	wroteHeader bool
}

func newShareLoginCapture(target http.ResponseWriter) *shareLoginCapture {
	return &shareLoginCapture{target: target}
}

func (c *shareLoginCapture) Header() http.Header { return c.target.Header() }

func (c *shareLoginCapture) WriteHeader(status int) {
	if c.wroteHeader {
		return
	}
	c.wroteHeader = true
	c.status = status
}

func (c *shareLoginCapture) Write(b []byte) (int, error) {
	if !c.wroteHeader {
		c.WriteHeader(http.StatusOK)
	}
	if c.passthrough {
		return c.target.Write(b)
	}
	if c.buffer.Len()+len(b) > shareLoginResponseLimit {
		c.passthrough = true
		c.target.WriteHeader(c.status)
		if _, err := c.target.Write(c.buffer.Bytes()); err != nil {
			return 0, err
		}
		c.buffer.Reset()
		return c.target.Write(b)
	}
	return c.buffer.Write(b)
}

// Flush is a no-op while buffering; the whole response is written in finish.
func (c *shareLoginCapture) Flush() {}

func (c *shareLoginCapture) finish(pending *shareLoginPending) {
	if c.passthrough {
		return
	}
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	if status == http.StatusOK && pending != nil {
		var payload struct {
			AccessToken string `json:"AccessToken"`
		}
		if err := json.Unmarshal(c.buffer.Bytes(), &payload); err == nil && strings.TrimSpace(payload.AccessToken) != "" {
			if err := pending.rt.registerSession(pending, payload.AccessToken); err != nil {
				log.Printf("share login: record session: %v", err)
			}
		}
	}
	c.target.WriteHeader(status)
	_, _ = c.target.Write(c.buffer.Bytes())
}

func (rt *shareUserRuntime) registerSession(pending *shareLoginPending, token string) error {
	ciphertext, err := encryptSiteAccountSecret(token)
	if err != nil {
		return err
	}
	hash := shareTokenHash(token)
	if _, err := rt.db.db.Exec(`INSERT INTO share_user_sessions(token_hash, share_user_id, site_id, token_ciphertext, device_id, device_name, client, client_ip) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(token_hash) DO UPDATE SET share_user_id=excluded.share_user_id, site_id=excluded.site_id, token_ciphertext=excluded.token_ciphertext, device_id=excluded.device_id, device_name=excluded.device_name, client=excluded.client, client_ip=excluded.client_ip, last_seen_at=CURRENT_TIMESTAMP`,
		hash, pending.user.id, pending.siteID, ciphertext, pending.deviceID, pending.device, pending.client, pending.clientIP); err != nil {
		return err
	}
	// Upstreams may hand the same token back to a device that signs in again.
	if _, err := rt.db.db.Exec(`DELETE FROM share_revoked_tokens WHERE token_hash=?`, hash); err != nil {
		return err
	}
	rt.mu.Lock()
	delete(rt.revoked, hash)
	rt.sessions[hash] = shareSessionEntry{userID: pending.user.id, siteID: pending.siteID}
	rt.lastSeen[hash] = time.Now()
	maxDevices := pending.user.maxDevices
	rt.mu.Unlock()
	if err := rt.enforceDeviceLimit(pending.user.id, pending.siteID, hash, pending.deviceID, maxDevices); err != nil {
		log.Printf("share user %d: device limit: %v", pending.user.id, err)
	}
	return nil
}

// enforceDeviceLimit drops the older token of a device that signed in again
// and, when maxDevices > 0, signs out the least recently active devices so the
// user keeps at most maxDevices. A device is identified by its Emby DeviceId,
// or by its token when the client sent none; all of a device's sessions (one
// per site) count once.
func (rt *shareUserRuntime) enforceDeviceLimit(userID, siteID int64, keepHash, deviceID string, maxDevices int) error {
	sessions, err := rt.listSessions(userID)
	if err != nil {
		return err
	}
	rt.mu.RLock()
	for i := range sessions {
		if at, ok := rt.lastSeen[sessions[i].TokenHash]; ok {
			sessions[i].lastSeen = at
		}
	}
	rt.mu.RUnlock()
	key := func(session shareStoredSession) string {
		if session.DeviceID != "" {
			return "d:" + session.DeviceID
		}
		return "t:" + session.TokenHash
	}
	keepKey := "t:" + keepHash
	if deviceID != "" {
		keepKey = "d:" + deviceID
	}
	var stale []shareStoredSession
	groups := make(map[string][]shareStoredSession)
	latest := make(map[string]time.Time)
	for _, session := range sessions {
		if session.TokenHash == keepHash {
			continue
		}
		k := key(session)
		if k == keepKey && session.SiteID == siteID {
			stale = append(stale, session)
			continue
		}
		groups[k] = append(groups[k], session)
		if session.lastSeen.After(latest[k]) {
			latest[k] = session.lastSeen
		}
	}
	for _, session := range stale {
		if err := rt.revokeOne(session); err != nil {
			return err
		}
	}
	if maxDevices <= 0 {
		return nil
	}
	others := make([]string, 0, len(groups))
	for k := range groups {
		if k != keepKey {
			others = append(others, k)
		}
	}
	allowed := maxDevices
	if keepHash != "" {
		allowed-- // the device that just signed in always stays
	}
	if len(others) <= allowed {
		return nil
	}
	sort.Slice(others, func(i, j int) bool { return latest[others[i]].After(latest[others[j]]) })
	for _, k := range others[allowed:] {
		for _, session := range groups[k] {
			if err := rt.revokeOne(session); err != nil {
				return err
			}
		}
	}
	return nil
}

// deviceCount counts a user's signed-in devices the same way as the limit.
func (rt *shareUserRuntime) deviceCount(userID int64) int {
	sessions, err := rt.listSessions(userID)
	if err != nil {
		return 0
	}
	seen := make(map[string]bool, len(sessions))
	for _, session := range sessions {
		k := "t:" + session.TokenHash
		if session.DeviceID != "" {
			k = "d:" + session.DeviceID
		}
		seen[k] = true
	}
	return len(seen)
}

// ---------------------------------------------------------------------------
// Revocation

type shareStoredSession struct {
	TokenHash  string `json:"-"`
	ciphertext string
	lastSeen   time.Time
	SiteID     int64  `json:"site_id"`
	SiteName   string `json:"site_name"`
	DeviceID   string `json:"-"`
	DeviceName string `json:"device_name"`
	Client     string `json:"client"`
	ClientIP   string `json:"client_ip"`
	CreatedAt  string `json:"created_at"`
	LastSeenAt string `json:"last_seen_at"`
}

func (rt *shareUserRuntime) listSessions(userID int64) ([]shareStoredSession, error) {
	rows, err := rt.db.db.Query(`SELECT s.token_hash, s.token_ciphertext, s.site_id, COALESCE(t.name, ''), s.device_id, s.device_name, s.client, s.client_ip, COALESCE(s.created_at, ''), COALESCE(s.last_seen_at, '') FROM share_user_sessions s LEFT JOIN sites t ON t.id=s.site_id WHERE s.share_user_id=? ORDER BY s.last_seen_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]shareStoredSession, 0)
	for rows.Next() {
		var item shareStoredSession
		if err := rows.Scan(&item.TokenHash, &item.ciphertext, &item.SiteID, &item.SiteName, &item.DeviceID, &item.DeviceName, &item.Client, &item.ClientIP, &item.CreatedAt, &item.LastSeenAt); err != nil {
			return nil, err
		}
		if parsed, err := time.Parse("2006-01-02 15:04:05", item.LastSeenAt); err == nil {
			item.lastSeen = parsed
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// revokeOne denylists one session's token, removes it and logs it out upstream.
func (rt *shareUserRuntime) revokeOne(session shareStoredSession) error {
	if _, err := rt.db.db.Exec(`INSERT INTO share_revoked_tokens(token_hash, site_id) VALUES(?,?) ON CONFLICT(token_hash) DO UPDATE SET site_id=excluded.site_id, revoked_at=CURRENT_TIMESTAMP`, session.TokenHash, session.SiteID); err != nil {
		return err
	}
	if _, err := rt.db.db.Exec(`DELETE FROM share_user_sessions WHERE token_hash=?`, session.TokenHash); err != nil {
		return err
	}
	rt.mu.Lock()
	delete(rt.sessions, session.TokenHash)
	delete(rt.lastSeen, session.TokenHash)
	rt.revoked[session.TokenHash] = session.SiteID
	rt.mu.Unlock()
	if token, err := decryptSiteAccountSecret(session.ciphertext); err == nil && token != "" {
		rt.logoutUpstream(session.SiteID, token)
	}
	return nil
}

// revokeSessions deletes the user's sessions (all sites when siteID is 0, or
// every site except keepSites when keepSites is non-nil) and logs the tokens
// out upstream. It returns the number of sessions removed.
func (rt *shareUserRuntime) revokeSessions(userID int64, siteID int64, keepSites map[int64]int64) (int, error) {
	if rt == nil || rt.db == nil {
		return 0, nil
	}
	sessions, err := rt.listSessions(userID)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, session := range sessions {
		if siteID > 0 && session.SiteID != siteID {
			continue
		}
		if keepSites != nil {
			if _, keep := keepSites[session.SiteID]; keep {
				continue
			}
		}
		if err := rt.revokeOne(session); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func (rt *shareUserRuntime) revokeAsync(userID, siteID int64, reason string) {
	rt.mu.Lock()
	if rt.revoking[userID] {
		rt.mu.Unlock()
		return
	}
	rt.revoking[userID] = true
	rt.mu.Unlock()
	go func() {
		defer func() {
			rt.mu.Lock()
			delete(rt.revoking, userID)
			rt.mu.Unlock()
		}()
		if count, err := rt.revokeSessions(userID, siteID, nil); err != nil {
			log.Printf("share user %d: revoke sessions (%s): %v", userID, reason, err)
		} else if count > 0 {
			log.Printf("share user %d: revoked %d session(s) (%s)", userID, count, reason)
		}
	}()
}

func (rt *shareUserRuntime) logoutUpstream(siteID int64, token string) {
	if rt.pm == nil {
		return
	}
	rt.pm.mu.RLock()
	inst := rt.pm.proxies[siteID]
	rt.pm.mu.RUnlock()
	if inst == nil || inst.shareLogout == nil {
		return
	}
	logout := inst.shareLogout
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), shareLogoutTimeout)
		defer cancel()
		if err := logout(ctx, token); err != nil {
			log.Printf("[%s] share session upstream logout failed: %v", inst.Site.Name, err)
		}
	}()
}

// newShareLogoutFunc builds the upstream /Sessions/Logout call for a site with
// the same transport, identity policy and configured headers as proxied traffic.
func newShareLogoutFunc(transport http.RoundTripper, target *url.URL, policy UAHeaderPolicy, headers upstreamHeaderPolicy) func(context.Context, string) error {
	return func(ctx context.Context, token string) error {
		endpoint := *target
		endpoint.Path = singleJoiningSlash(target.Path, "/Sessions/Logout")
		endpoint.RawPath = ""
		endpoint.RawQuery = ""
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), nil)
		if err != nil {
			return err
		}
		request.Header.Set("X-Emby-Token", token)
		request.Header.Set("X-Emby-Authorization", fmt.Sprintf(`MediaBrowser Client="Meridian", Device="Meridian", DeviceId="meridian-share", Version="%s", Token="%s"`, appVersion, token))
		applyUAHeaderPolicy(request.Header, policy)
		headers.apply(request.Header, &endpoint)
		response, err := (&http.Client{Transport: transport, Timeout: shareLogoutTimeout}).Do(request)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
		if response.StatusCode >= 400 && response.StatusCode != http.StatusUnauthorized {
			return fmt.Errorf("upstream returned %d", response.StatusCode)
		}
		return nil
	}
}

// ---------------------------------------------------------------------------
// Persistence and periodic enforcement

func (rt *shareUserRuntime) flush() {
	if rt == nil || rt.db == nil {
		return
	}
	rt.flushMu.Lock()
	defer rt.flushMu.Unlock()
	rt.mu.RLock()
	states := make([]*shareUserState, 0, len(rt.users))
	for _, state := range rt.users {
		states = append(states, state)
	}
	touched := make(map[string]time.Time, len(rt.lastSeen))
	for hash, at := range rt.lastSeen {
		touched[hash] = at
	}
	rt.mu.RUnlock()

	for _, state := range states {
		delta := state.pending.Swap(0)
		if delta == 0 {
			continue
		}
		if _, err := rt.db.db.Exec(`UPDATE share_users SET traffic_used=traffic_used+? WHERE id=?`, delta, state.id); err != nil {
			state.pending.Add(delta)
			log.Printf("share user %d: flush traffic: %v", state.id, err)
			continue
		}
		rt.mu.Lock()
		state.persistedUsed += delta
		rt.mu.Unlock()
	}
	for hash, at := range touched {
		_, _ = rt.db.db.Exec(`UPDATE share_user_sessions SET last_seen_at=? WHERE token_hash=?`, at.UTC().Format("2006-01-02 15:04:05"), hash)
	}
	rt.applyMonthlyResets(time.Now().In(rt.db.shareLocation()))
	cutoff := time.Now().Add(-shareRevokedTokenRetention).UTC().Format("2006-01-02 15:04:05")
	_, _ = rt.db.db.Exec(`DELETE FROM share_revoked_tokens WHERE revoked_at < ?`, cutoff)
	if err := rt.reload(); err != nil {
		log.Printf("share users: reload: %v", err)
		return
	}
	rt.enforce(time.Now())
}

// applyMonthlyResets clears the used traffic of users whose monthly cycle has
// rolled over since the last reset. The caller holds flushMu.
func (rt *shareUserRuntime) applyMonthlyResets(now time.Time) {
	rows, err := rt.db.db.Query(`SELECT id, traffic_reset_day, traffic_reset_period FROM share_users WHERE traffic_reset_day > 0`)
	if err != nil {
		log.Printf("share users: monthly reset query: %v", err)
		return
	}
	type due struct {
		id     int64
		period string
	}
	var resets []due
	for rows.Next() {
		var id int64
		var day int
		var period string
		if err := rows.Scan(&id, &day, &period); err != nil {
			continue
		}
		if current, _ := shareResetPeriod(now, day); current != "" && current != period {
			resets = append(resets, due{id: id, period: current})
		}
	}
	rows.Close()
	for _, item := range resets {
		if _, err := rt.db.db.Exec(`UPDATE share_users SET traffic_used=0, traffic_reset_period=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, item.period, item.id); err != nil {
			log.Printf("share user %d: monthly reset: %v", item.id, err)
			continue
		}
		rt.mu.Lock()
		if state := rt.users[item.id]; state != nil {
			state.pending.Store(0)
			state.persistedUsed = 0
			state.resetPeriod = item.period
		}
		rt.mu.Unlock()
		log.Printf("share user %d: monthly traffic reset (%s)", item.id, item.period)
	}
}

// enforce revokes the sessions of every user who is no longer allowed in.
func (rt *shareUserRuntime) enforce(now time.Time) {
	rt.mu.RLock()
	blocked := make(map[int64]string)
	for _, entry := range rt.sessions {
		if _, done := blocked[entry.userID]; done {
			continue
		}
		state := rt.users[entry.userID]
		if state == nil {
			continue
		}
		if reason := state.blockReasonLocked(now); reason != "" {
			blocked[entry.userID] = reason
		}
	}
	rt.mu.RUnlock()
	for userID, reason := range blocked {
		rt.revokeAsync(userID, 0, reason)
	}
}

func (rt *shareUserRuntime) usage(userID int64) (used int64, sessions int) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if state := rt.users[userID]; state != nil {
		used = state.persistedUsed + state.pending.Load()
	}
	for _, entry := range rt.sessions {
		if entry.userID == userID {
			sessions++
		}
	}
	return used, sessions
}

func (rt *shareUserRuntime) resetTraffic(userID int64) error {
	rt.flushMu.Lock()
	defer rt.flushMu.Unlock()
	if _, err := rt.db.db.Exec(`UPDATE share_users SET traffic_used=0, updated_at=CURRENT_TIMESTAMP WHERE id=?`, userID); err != nil {
		return err
	}
	rt.mu.Lock()
	if state := rt.users[userID]; state != nil {
		state.pending.Store(0)
		state.persistedUsed = 0
	}
	rt.mu.Unlock()
	return nil
}

// reencryptShareSecrets moves share-user secrets to a new credential key during
// a restore. Older backups without the tables are skipped. A secret that cannot
// be decrypted is dropped rather than failing the restore: the stored bcrypt
// hash still verifies the password, and an orphaned session only needs a new
// sign-in.
func reencryptShareSecrets(tx *sql.Tx, oldKey, newKey []byte) error {
	hasUsers, err := backupSQLiteColumnExists(tx, "share_users", "password_ciphertext")
	if err != nil || !hasUsers {
		return err
	}
	type secretUpdate struct {
		key    string
		cipher string
	}
	migrate := func(query, update string) error {
		rows, err := tx.Query(query)
		if err != nil {
			return err
		}
		var updates []secretUpdate
		for rows.Next() {
			var item secretUpdate
			if err := rows.Scan(&item.key, &item.cipher); err != nil {
				rows.Close()
				return err
			}
			if item.cipher == "" {
				continue
			}
			plain, err := decryptSiteAccountSecretWithSecrets(item.cipher, [][]byte{oldKey, newKey})
			if err != nil {
				item.cipher = ""
			} else if item.cipher, err = encryptSiteAccountSecretWithSecret(plain, newKey); err != nil {
				rows.Close()
				return err
			}
			updates = append(updates, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, item := range updates {
			if _, err := tx.Exec(update, item.cipher, item.key); err != nil {
				return err
			}
		}
		return nil
	}
	if err := migrate("SELECT CAST(id AS TEXT), password_ciphertext FROM share_users", "UPDATE share_users SET password_ciphertext=? WHERE CAST(id AS TEXT)=?"); err != nil {
		return err
	}
	if err := migrate("SELECT token_hash, token_ciphertext FROM share_user_sessions", "UPDATE share_user_sessions SET token_ciphertext=? WHERE token_hash=?"); err != nil {
		return err
	}
	_, err = tx.Exec("DELETE FROM share_user_sessions WHERE token_ciphertext=''")
	return err
}

func generateSharePassword(length int) (string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	var builder strings.Builder
	for i := 0; i < length; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		builder.WriteByte(alphabet[n.Int64()])
	}
	return builder.String(), nil
}
