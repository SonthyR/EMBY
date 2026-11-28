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
		{"share_users", "max_concurrent_streams", "ALTER TABLE share_users ADD COLUMN max_concurrent_streams INTEGER NOT NULL DEFAULT 0"},
		{"share_users", "traffic_reset_day", "ALTER TABLE share_users ADD COLUMN traffic_reset_day INTEGER NOT NULL DEFAULT 0"},
		{"share_users", "traffic_reset_period", "ALTER TABLE share_users ADD COLUMN traffic_reset_period TEXT NOT NULL DEFAULT ''"},
		{"share_users", "portal_token", "ALTER TABLE share_users ADD COLUMN portal_token TEXT NOT NULL DEFAULT ''"},
		{"share_users", "danmaku_token", "ALTER TABLE share_users ADD COLUMN danmaku_token TEXT NOT NULL DEFAULT ''"},
		{"share_users", "speed_limit", "ALTER TABLE share_users ADD COLUMN speed_limit INTEGER NOT NULL DEFAULT 0"},
		{"share_users", "route_prefix", "ALTER TABLE share_users ADD COLUMN route_prefix TEXT NOT NULL DEFAULT ''"},
		{"share_users", "sort_order", "ALTER TABLE share_users ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0"},
		{"share_user_sites", "sort_order", "ALTER TABLE share_user_sites ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0"},
		{"share_user_sessions", "device_id", "ALTER TABLE share_user_sessions ADD COLUMN device_id TEXT NOT NULL DEFAULT ''"},
		{"share_user_sessions", "site_account_id", "ALTER TABLE share_user_sessions ADD COLUMN site_account_id INTEGER NOT NULL DEFAULT 0"},
		{"share_user_sessions", "egress_node_id", "ALTER TABLE share_user_sessions ADD COLUMN egress_node_id INTEGER NOT NULL DEFAULT 0"},
		{"share_user_sessions", "upstream_token_ciphertext", "ALTER TABLE share_user_sessions ADD COLUMN upstream_token_ciphertext TEXT NOT NULL DEFAULT ''"},
		{"share_user_sessions", "upstream_user_id", "ALTER TABLE share_user_sessions ADD COLUMN upstream_user_id TEXT NOT NULL DEFAULT ''"},
		{"share_user_sessions", "client_user_id", "ALTER TABLE share_user_sessions ADD COLUMN client_user_id TEXT NOT NULL DEFAULT ''"},
	} {
		var exists int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('"+column.table+"') WHERE name=?", column.name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := conn.ExecContext(ctx, column.sql); err != nil {
				return err
			}
			if column.table == "share_users" && column.name == "sort_order" {
				if _, err := conn.ExecContext(ctx, `UPDATE share_users SET sort_order = id`); err != nil {
					return err
				}
			}
			if column.table == "share_user_sites" && column.name == "sort_order" {
				if _, err := conn.ExecContext(ctx, `UPDATE share_user_sites SET sort_order = COALESCE((SELECT s.sort_order FROM sites s WHERE s.id = share_user_sites.site_id), site_id)`); err != nil {
					return err
				}
			}
		}
	}
	if _, err := conn.ExecContext(ctx, "UPDATE share_users SET portal_token=lower(hex(randomblob(16))) WHERE portal_token=''"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS idx_share_users_portal ON share_users(portal_token) WHERE portal_token <> ''"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE share_users SET danmaku_token=lower(hex(randomblob(16))) WHERE danmaku_token=''"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS idx_share_users_danmaku ON share_users(danmaku_token) WHERE danmaku_token <> ''"); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS idx_share_users_route_prefix ON share_users(route_prefix) WHERE route_prefix <> ''")
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

var (
	shareStreamIdleTimeout  = 25 * time.Second
	sharePlayingIdleTimeout = 90 * time.Second
	shareDrainSweepInterval = 5 * time.Second
)

type shareSessionActivity struct {
	activeStreams  int
	playing        bool
	lastPlaybackAt time.Time
	lastNodeID     int64
	hasNode        bool
	lastNodeName   string
	draining       bool
	drainingSince  time.Time
	mediaTitle     sharePlaybackTitleState
}

func (a *shareSessionActivity) isWatching(now time.Time) bool {
	if a == nil {
		return false
	}
	if a.activeStreams > 0 {
		return true
	}
	if a.playing && !a.lastPlaybackAt.IsZero() && now.Sub(a.lastPlaybackAt) < sharePlayingIdleTimeout {
		return true
	}
	if !a.lastPlaybackAt.IsZero() && now.Sub(a.lastPlaybackAt) < shareStreamIdleTimeout {
		return true
	}
	return false
}

type shareSessionEntry struct {
	loginGeneration  uint64
	userID        int64
	siteID        int64
	siteAccountID int64
	egressNodeID  int64
	clientToken   string
	upstreamToken string
	clientUID     string
	upstreamUID   string
	deviceID      string
	deviceName    string
	clientName    string
	clientIP         string
	ispGroup         string
	lastHandoffAt    time.Time
	lastSiteSwitchAt time.Time
}

type shareUserState struct {
	id int64
	// The fields below are guarded by shareUserRuntime.mu.
	username             string
	passwordHash         string
	routePrefix          string
	enabled              bool
	expiresAt            time.Time
	quota                int64
	persistedUsed        int64
	maxDevices           int
	maxConcurrentStreams int
	speedLimit           int               // Mbps, 0 = unlimited
	limiter              *shareRateLimiter // nil when unlimited
	resetDay             int
	resetPeriod          string
	siteOrder            []int64         // ordered site IDs (first enabled = active site)
	sites                map[int64]int64 // site ID -> preferred site_account ID (0 = first usable)
	switchEpoch          uint32          // rotates on primary site change so clients clear stale local favorite cache

	pending atomic.Int64 // bytes metered but not yet written to share_users.traffic_used
}

type shareLoginFailure struct {
	count int
	first time.Time
}

type shareUserRuntime struct {
	db *DB
	pm *ProxyManager

	// Serialize persisted session changes with reload's snapshot and publication.
	// A handoff takes its per-session lock first, then sessionStoreMu, then mu.
	sessionStoreMu  sync.Mutex
	loginGeneration uint64 // guarded by mu

	mu         sync.RWMutex
	users      map[int64]*shareUserState
	byName     map[string]int64
	byRoute    map[string]int64 // normalized route prefix (e.g. "/ws") -> user ID
	sessions   map[string]shareSessionEntry
	revoked    map[string]int64 // token hash -> site ID
	lastSeen      map[string]time.Time
	dirtyClientIP map[string]string
	activity      map[string]*shareSessionActivity
	playbackTitleSlots chan struct{}
	drainTimer    *time.Timer
	failures      map[string]*shareLoginFailure
	revoking      map[int64]bool
	flushMu       sync.Mutex

	handoffMu    sync.Mutex
	handoffLocks map[string]*sync.Mutex

	capMu        sync.Mutex
	capabilities map[string]shareCapabilityOwner
	loadError    error
	accountRR    atomic.Uint64
}

func newShareUserRuntime(db *DB, pm *ProxyManager) *shareUserRuntime {
	rt := &shareUserRuntime{
		db:       db,
		pm:       pm,
		users:    make(map[int64]*shareUserState),
		byName:   make(map[string]int64),
		byRoute:  make(map[string]int64),
		sessions: make(map[string]shareSessionEntry),
		revoked:       make(map[string]int64),
		lastSeen:      make(map[string]time.Time),
		dirtyClientIP: make(map[string]string),
		activity:      make(map[string]*shareSessionActivity),
		playbackTitleSlots: make(chan struct{}, playbackTitleLookupSlots),
		failures:      make(map[string]*shareLoginFailure),
		revoking:      make(map[int64]bool),
		handoffLocks:  make(map[string]*sync.Mutex),
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
	rt.sessionStoreMu.Lock()
	defer rt.sessionStoreMu.Unlock()
	type row struct {
		id                                       int64
		username, hash, routePrefix, expires     string
		enabled                                  bool
		quota, used                              int64
		maxDevices, maxConcurrentStreams, resetDay int
		speedLimit                               int
		resetPeriod                              string
	}
	rows, err := rt.db.db.Query(`SELECT id, username, password_hash, route_prefix, enabled, expires_at, traffic_quota, traffic_used, max_devices, max_concurrent_streams, traffic_reset_day, traffic_reset_period, speed_limit FROM share_users`)
	if err != nil {
		return err
	}
	var loaded []row
	for rows.Next() {
		var item row
		var enabled int
		if err := rows.Scan(&item.id, &item.username, &item.hash, &item.routePrefix, &enabled, &item.expires, &item.quota, &item.used, &item.maxDevices, &item.maxConcurrentStreams, &item.resetDay, &item.resetPeriod, &item.speedLimit); err != nil {
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

	siteRows, err := rt.db.db.Query(`SELECT a.share_user_id, a.site_id, a.site_account_id FROM share_user_sites a LEFT JOIN sites s ON s.id=a.site_id ORDER BY a.share_user_id, a.sort_order, COALESCE(s.sort_order, a.site_id), a.site_id`)
	if err != nil {
		return err
	}
	assignments := make(map[int64]map[int64]int64)
	siteOrders := make(map[int64][]int64)
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
		siteOrders[userID] = append(siteOrders[userID], siteID)
	}
	if err := siteRows.Err(); err != nil {
		siteRows.Close()
		return err
	}
	siteRows.Close()

	sessionRows, err := rt.db.db.Query(`SELECT token_hash, share_user_id, site_id, site_account_id, egress_node_id, token_ciphertext, upstream_token_ciphertext, client_user_id, upstream_user_id, device_id, device_name, client, client_ip FROM share_user_sessions`)
	if err != nil {
		return err
	}
	sessions := make(map[string]shareSessionEntry)
	for sessionRows.Next() {
		var hash, tokenCipher, upTokenCipher string
		var entry shareSessionEntry
		if err := sessionRows.Scan(&hash, &entry.userID, &entry.siteID, &entry.siteAccountID, &entry.egressNodeID, &tokenCipher, &upTokenCipher, &entry.clientUID, &entry.upstreamUID, &entry.deviceID, &entry.deviceName, &entry.clientName, &entry.clientIP); err != nil {
			sessionRows.Close()
			return err
		}
		if entry.clientUID == "" && entry.upstreamUID != "" {
			entry.clientUID = entry.upstreamUID
		}
		if tokenCipher != "" {
			if plain, decErr := decryptSiteAccountSecret(tokenCipher); decErr == nil {
				entry.clientToken = plain
				entry.upstreamToken = plain
			}
		}
		if upTokenCipher != "" {
			if plain, decErr := decryptSiteAccountSecret(upTokenCipher); decErr == nil && plain != "" {
				entry.upstreamToken = plain
			}
		}
		if rt.pm != nil && rt.pm.edges != nil && entry.clientIP != "" {
			entry.ispGroup = rt.pm.edges.lookupISPGroup(entry.clientIP)
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
	byRoute := make(map[string]int64, len(loaded))
	for _, item := range loaded {
		seen[item.id] = true
		state := rt.users[item.id]
		var prevFirstSite int64
		if state == nil {
			state = &shareUserState{id: item.id, switchEpoch: nextInPlayerSwitchEpoch(0)}
			rt.users[item.id] = state
		} else if len(state.siteOrder) > 0 {
			prevFirstSite = state.siteOrder[0]
		}
		state.username = item.username
		state.passwordHash = item.hash
		state.routePrefix = item.routePrefix
		state.enabled = item.enabled
		state.expiresAt = rt.db.shareExpiryTime(item.expires)
		state.quota = item.quota
		state.persistedUsed = item.used
		state.maxDevices = item.maxDevices
		state.maxConcurrentStreams = item.maxConcurrentStreams
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
		state.siteOrder = siteOrders[item.id]
		if len(state.siteOrder) > 0 && prevFirstSite > 0 && state.siteOrder[0] != prevFirstSite {
			state.switchEpoch = nextInPlayerSwitchEpoch(state.switchEpoch)
		} else if state.switchEpoch == 0 {
			state.switchEpoch = nextInPlayerSwitchEpoch(0)
		}
		state.sites = assignments[item.id]
		if state.sites == nil {
			state.sites = map[int64]int64{}
		}
		byName[strings.ToLower(item.username)] = item.id
		if item.routePrefix != "" {
			byRoute[item.routePrefix] = item.id
		}
	}
	for id := range rt.users {
		if !seen[id] {
			delete(rt.users, id)
		}
	}
	rt.byName = byName
	rt.byRoute = byRoute
	prevSessions := rt.sessions
	for hash, entry := range sessions {
		if prev, exists := prevSessions[hash]; exists {
			entry.loginGeneration = prev.loginGeneration
			if !prev.lastHandoffAt.IsZero() {
				entry.lastHandoffAt = prev.lastHandoffAt
			}
			if !prev.lastSiteSwitchAt.IsZero() {
				entry.lastSiteSwitchAt = prev.lastSiteSwitchAt
			}
			if prev.ispGroup != "" && entry.ispGroup == "" {
				entry.ispGroup = prev.ispGroup
			}
			if pendingIP, dirty := rt.dirtyClientIP[hash]; dirty && pendingIP != "" {
				entry.clientIP = pendingIP
			}
			sessions[hash] = entry
		}
	}
	rt.sessions = sessions
	for hash := range rt.lastSeen {
		if _, ok := sessions[hash]; !ok {
			delete(rt.lastSeen, hash)
		}
	}
	for hash := range rt.activity {
		if _, ok := sessions[hash]; !ok {
			delete(rt.activity, hash)
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
		var fallbackPassword string
		for key, value := range values {
			text, _ := value.(string)
			switch strings.ToLower(key) {
			case "username":
				parsed.username = text
			case "pw":
				parsed.password = text
			case "password":
				fallbackPassword = text
			}
		}
		if parsed.password == "" && fallbackPassword != "" {
			parsed.password = fallbackPassword
		}
		return parsed, true
	}
	form, err := url.ParseQuery(string(trimmed))
	if err != nil {
		return nil, false
	}
	parsed := &shareLoginBody{form: form}
	var fallbackPassword string
	for key, values := range form {
		if len(values) == 0 {
			continue
		}
		switch strings.ToLower(key) {
		case "username":
			parsed.username = values[0]
		case "pw":
			parsed.password = values[0]
		case "password":
			fallbackPassword = values[0]
		}
	}
	if parsed.password == "" && fallbackPassword != "" {
		parsed.password = fallbackPassword
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
// Personal share routes and playback-aware switching

func shareNormalizedPath(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	return strings.ToLower(strings.TrimRight(r.URL.Path, "/"))
}

func isSharePlaybackStartRequest(r *http.Request) bool {
	return r != nil && r.Method == http.MethodPost && strings.HasSuffix(shareNormalizedPath(r), "/sessions/playing")
}

func isSharePlaybackProgressRequest(r *http.Request) bool {
	return r != nil && r.Method == http.MethodPost && strings.HasSuffix(shareNormalizedPath(r), "/sessions/playing/progress")
}

func isSharePlaybackStoppedRequest(r *http.Request) bool {
	return r != nil && r.Method == http.MethodPost && strings.HasSuffix(shareNormalizedPath(r), "/sessions/playing/stopped")
}

func isShareActiveMediaRequest(r *http.Request) bool {
	if r == nil || r.URL == nil || hasUpgradeIntent(r) {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	path := strings.ToLower(r.URL.Path)
	if strings.Contains(path, "/images/") || strings.Contains(path, "/subtitles/") {
		return false
	}
	return isReservedDynamicRoute(r.URL.Path) || isPlaybackRequest(r.URL.Path) || isPlaybackRedirectEndpoint(r.URL.Path) || isMainVideoStreamRequest(r)
}

func (rt *shareUserRuntime) matchRoutePrefix(requestPath string) (int64, string, bool) {
	if rt == nil {
		return 0, "", false
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	for prefix, userID := range rt.byRoute {
		if ingressPathMatches(requestPath, prefix) || embeddedIngressPathMatches(requestPath, prefix) {
			return userID, prefix, true
		}
	}
	return 0, "", false
}

func (rt *shareUserRuntime) routePrefixUserID(prefix string) (int64, bool) {
	if rt == nil || prefix == "" {
		return 0, false
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	userID, ok := rt.byRoute[prefix]
	return userID, ok
}

// activeSiteID returns the first operational site in the share user's ordered
// site list, falling back to the first assigned site if ProxyManager is absent.
func (rt *shareUserRuntime) activeSiteID(userID int64) int64 {
	if rt == nil {
		return 0
	}
	rt.mu.RLock()
	state := rt.users[userID]
	var order []int64
	if state != nil && len(state.siteOrder) > 0 {
		order = append([]int64(nil), state.siteOrder...)
	}
	rt.mu.RUnlock()
	for _, siteID := range order {
		if rt.pm == nil {
			return siteID
		}
		if handler, ok := rt.pm.SiteHandler(siteID); ok && handler != nil {
			return siteID
		}
	}
	return 0
}

// resolveShareRouteTarget selects which site should serve a request arriving
// on a share user's fixed personal prefix (e.g. /ws). New logins and
// unauthenticated pre-login probes always hit the user's #1 active site; an
// existing session on a previous site continues hitting that old site while it
// is actively finishing a video playback (draining), and otherwise seamlessly
// hands off to the new #1 active site without forcing re-login.
func (rt *shareUserRuntime) resolveShareRouteTarget(userID int64, r *http.Request, clientIP string) (int64, int, string) {
	if rt == nil {
		return 0, http.StatusServiceUnavailable, "服务暂不可用"
	}
	activeSite := rt.activeSiteID(userID)
	now := time.Now()
	if isShareLoginRequest(r) {
		if activeSite == 0 {
			return 0, http.StatusServiceUnavailable, "暂无可用服务器"
		}
		return activeSite, 0, ""
	}
	if isReservedDynamicRoute(r.URL.Path) {
		owner, ok := rt.capabilityOwnerInfo(r.URL.Path)
		if !ok || owner.userID != userID {
			return 0, http.StatusUnauthorized, "播放链接已失效，请重新登录"
		}
		rt.mu.RLock()
		state := rt.users[userID]
		reason := state.blockReasonLocked(now)
		var sessionOK bool
		var act *shareSessionActivity
		if owner.tokenHash != "" {
			entry, exists := rt.sessions[owner.tokenHash]
			sessionOK = exists && entry.userID == userID
			act = rt.activity[owner.tokenHash]
		} else {
			sessionOK = true
		}
		rt.mu.RUnlock()
		if reason != "" {
			return 0, http.StatusUnauthorized, shareBlockMessage(reason)
		}
		if !sessionOK {
			return 0, http.StatusUnauthorized, "登录已失效，请重新登录"
		}
		if owner.siteID == activeSite || (act != nil && act.draining && act.isWatching(now)) {
			return owner.siteID, 0, ""
		}
		// Return 404 (not 401) for a stale media capability from an older site so
		// clients keep their saved session token and simply refresh the home library.
		return 0, http.StatusNotFound, "服务器已切换，请返回首页刷新后重新播放"
	}
	for _, token := range embyRequestTokens(r) {
		hash := shareTokenHash(token)
		rt.mu.RLock()
		entry, ok := rt.sessions[hash]
		_, revoked := rt.revoked[hash]
		if !ok && revoked {
			rt.mu.RUnlock()
			return 0, http.StatusUnauthorized, "登录已失效，请重新登录"
		}
		if !ok {
			rt.mu.RUnlock()
			continue
		}
		if entry.userID != userID {
			rt.mu.RUnlock()
			return 0, http.StatusUnauthorized, "此入口仅限对应分享账号使用"
		}
		state := rt.users[userID]
		reason := state.blockReasonLocked(now)
		act := rt.activity[hash]
		drainingAllowed := act != nil && act.draining && (act.isWatching(now) || isSharePlaybackStoppedRequest(r))
		rt.mu.RUnlock()
		if reason != "" {
			if state != nil {
				rt.revokeAsync(userID, 0, reason)
			}
			return 0, http.StatusUnauthorized, shareBlockMessage(reason)
		}
		// Logout always targets the existing share login rather than signing in
		// to the preferred site first. Any live handler can finish a personal
		// route logout if its session's former proxy has already stopped.
		if isShareClientLogoutRequest(r) {
			if handler, ok := rt.pm.SiteHandler(entry.siteID); ok && handler != nil {
				return entry.siteID, 0, ""
			}
			if activeSite > 0 {
				return activeSite, 0, ""
			}
		}
		if entry.siteID == activeSite {
			return entry.siteID, 0, ""
		}
		if drainingAllowed || isInPlayerSiteSwitchRequest(r) {
			return entry.siteID, 0, ""
		}
		if activeSite == 0 {
			_ = rt.revokeSession(hash, &entry)
			return 0, http.StatusServiceUnavailable, "暂无可用服务器"
		}
		if status, msg := rt.handoffPersonalRouteSite(withShareHandoffClientHeaders(r.Context(), r), hash, activeSite, clientIP); status != 0 {
			return 0, status, msg
		}
		return activeSite, 0, ""
	}
	if activeSite == 0 {
		return 0, http.StatusServiceUnavailable, "暂无可用服务器"
	}
	return activeSite, 0, ""
}

// acquireRequestActivity atomically checks the concurrent stream limit (when
// required) and registers request playback/stream activity under a single
// rt.mu.Lock() acquisition so simultaneous playback starts from different
// devices cannot both bypass a 1-stream cap.
func (rt *shareUserRuntime) acquireRequestActivity(userID int64, tokenHash string, r *http.Request, now time.Time, checkLimit bool) (func(), bool, int) {
	noop := func() {}
	if rt == nil || tokenHash == "" {
		return noop, false, 0
	}
	isStart := isSharePlaybackStartRequest(r)
	isProgress := isSharePlaybackProgressRequest(r)
	isStopped := isSharePlaybackStoppedRequest(r)
	isMedia := isShareActiveMediaRequest(r)
	shouldCheckLimit := checkLimit && userID > 0 && (isMedia || isStart)
	if !shouldCheckLimit && !isStart && !isProgress && !isStopped && !isMedia {
		return noop, false, 0
	}
	masterName := rt.masterNodeName()
	if now.IsZero() {
		now = time.Now()
	}
	rt.mu.Lock()
	if shouldCheckLimit {
		if state := rt.users[userID]; state != nil && state.maxConcurrentStreams > 0 {
			otherPlaying := rt.activeStreamCountForUserLocked(userID, tokenHash, now)
			if otherPlaying >= state.maxConcurrentStreams {
				limit := state.maxConcurrentStreams
				rt.mu.Unlock()
				return noop, true, limit
			}
		}
	}
	if !isStart && !isProgress && !isStopped && !isMedia {
		rt.mu.Unlock()
		return noop, false, 0
	}
	act := rt.activity[tokenHash]
	if act == nil {
		act = &shareSessionActivity{}
		rt.activity[tokenHash] = act
	}
	if isStart || isProgress {
		act.playing = true
		act.lastPlaybackAt = now
	}
	if isMedia {
		act.activeStreams++
		act.lastPlaybackAt = now
		if act.lastNodeName == "" {
			act.lastNodeName = masterName
		}
	}
	rt.mu.Unlock()
	if !isMedia && !isStopped {
		return noop, false, 0
	}
	return func() {
		revokeNow := false
		rt.mu.Lock()
		act := rt.activity[tokenHash]
		if act != nil {
			if isMedia {
				if act.activeStreams > 0 {
					act.activeStreams--
				}
				act.lastPlaybackAt = time.Now()
				if act.draining {
					rt.scheduleDrainSweepLocked()
				}
			}
			if isStopped {
				if report, ok := r.Context().Value(playbackTitleReportKey{}).(playbackTitleReport); ok && act.mediaTitle.ignoresStop(report, report.at) {
					rt.mu.Unlock()
					return
				}
				act.playing = false
				if act.activeStreams == 0 {
					act.lastPlaybackAt = time.Time{}
					act.hasNode = false
					act.lastNodeID = 0
				}
				if act.draining {
					var canSeamless bool
					if entry, exists := rt.sessions[tokenHash]; exists {
						if st := rt.users[entry.userID]; st != nil && st.routePrefix != "" && len(st.siteOrder) > 0 {
							canSeamless = true
						}
					}
					if canSeamless {
						act.draining = false
						act.drainingSince = time.Time{}
					} else {
						revokeNow = true
					}
				}
			}
		}
		rt.mu.Unlock()
		if revokeNow {
			_ = rt.revokeByHash(tokenHash)
		}
	}, false, 0
}

func (rt *shareUserRuntime) beginRequestActivity(tokenHash string, r *http.Request) func() {
	finish, _, _ := rt.acquireRequestActivity(0, tokenHash, r, time.Now(), false)
	return finish
}

func (rt *shareUserRuntime) scheduleDrainSweepLocked() {
	if rt == nil || rt.drainTimer != nil {
		return
	}
	interval := shareDrainSweepInterval
	if interval <= 0 {
		interval = time.Second
	}
	rt.drainTimer = time.AfterFunc(interval, func() {
		rt.mu.Lock()
		rt.drainTimer = nil
		rt.mu.Unlock()
		rt.sweepDrainingSessions(time.Now())
	})
}

func (rt *shareUserRuntime) sweepDrainingSessions(now time.Time) {
	if rt == nil {
		return
	}
	var toRevoke []string
	hasRemaining := false
	rt.mu.Lock()
	for hash, act := range rt.activity {
		if !act.draining {
			continue
		}
		entry, exists := rt.sessions[hash]
		if !exists {
			delete(rt.activity, hash)
			continue
		}
		if !act.isWatching(now) {
			st := rt.users[entry.userID]
			if st != nil && st.routePrefix != "" && len(st.siteOrder) > 0 {
				act.draining = false
				act.drainingSince = time.Time{}
			} else {
				toRevoke = append(toRevoke, hash)
			}
		} else {
			hasRemaining = true
		}
	}
	if hasRemaining {
		rt.scheduleDrainSweepLocked()
	}
	rt.mu.Unlock()
	for _, hash := range toRevoke {
		_ = rt.revokeByHash(hash)
	}
}

// reconcileSiteSwitch handles switching a share user's active/assigned sites.
// For personal-route accounts with an available active site, idle sessions stay
// alive for seamless cross-site handoff on the next client request (counted in
// seamless) while actively playing sessions drain until playback ends. For
// non-personal-route accounts or forceImmediate, mismatched sessions are
// revoked immediately if idle or drained while watching.
func (rt *shareUserRuntime) reconcileSiteSwitch(userID int64, forceImmediate bool) (revoked int, draining int, seamless int) {
	if rt == nil || rt.db == nil {
		return 0, 0, 0
	}
	sessions, err := rt.listSessions(userID)
	if err != nil {
		return 0, 0, 0
	}
	activeSite := rt.activeSiteID(userID)
	now := time.Now()
	rt.mu.Lock()
	state := rt.users[userID]
	if state == nil {
		rt.mu.Unlock()
		return 0, 0, 0
	}
	hasPersonalRoute := state.routePrefix != ""
	var immediate []shareStoredSession
	for _, session := range sessions {
		_, assigned := state.sites[session.SiteID]
		matchesActive := !hasPersonalRoute || activeSite == 0 || session.SiteID == activeSite
		if assigned && matchesActive {
			if act := rt.activity[session.TokenHash]; act != nil {
				act.draining = false
				act.drainingSince = time.Time{}
			}
			continue
		}
		act := rt.activity[session.TokenHash]
		if !forceImmediate && act != nil && act.isWatching(now) {
			if !act.draining {
				act.draining = true
				act.drainingSince = now
			}
			draining++
			rt.scheduleDrainSweepLocked()
			continue
		}
		if !forceImmediate && hasPersonalRoute && activeSite > 0 {
			if act != nil {
				act.draining = false
				act.drainingSince = time.Time{}
			}
			seamless++
			continue
		}
		immediate = append(immediate, session)
	}
	rt.mu.Unlock()
	for _, session := range immediate {
		if err := rt.revokeOne(session); err == nil {
			revoked++
		}
	}
	return revoked, draining, seamless
}

func (rt *shareUserRuntime) drainingCount(userID int64) int {
	if rt == nil {
		return 0
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	count := 0
	for hash, entry := range rt.sessions {
		if entry.userID != userID {
			continue
		}
		if act := rt.activity[hash]; act != nil && act.draining {
			count++
		}
	}
	return count
}

func (rt *shareUserRuntime) effectiveSpeedLimitMbps(user *shareUserState) int {
	if rt == nil || user == nil {
		return 0
	}
	rt.mu.RLock()
	limit := user.speedLimit
	rt.mu.RUnlock()
	return limit
}

func (rt *shareUserRuntime) markEdgePlayback(tokenHash string, nodeID int64, nodeName string) {
	if rt == nil || tokenHash == "" {
		return
	}
	now := time.Now()
	rt.mu.Lock()
	act := rt.activity[tokenHash]
	if act == nil {
		act = &shareSessionActivity{}
		rt.activity[tokenHash] = act
	}
	act.playing = true
	act.lastPlaybackAt = now
	act.lastNodeID = nodeID
	act.hasNode = true
	if strings.TrimSpace(nodeName) != "" {
		act.lastNodeName = strings.TrimSpace(nodeName)
	}
	rt.mu.Unlock()
}

func (rt *shareUserRuntime) markMasterPlayback(tokenHash, masterName string) {
	if rt == nil || tokenHash == "" {
		return
	}
	now := time.Now()
	rt.mu.Lock()
	act := rt.activity[tokenHash]
	if act == nil {
		act = &shareSessionActivity{}
		rt.activity[tokenHash] = act
	}
	act.playing = true
	act.lastPlaybackAt = now
	act.lastNodeID = 0
	act.hasNode = true
	if strings.TrimSpace(masterName) != "" {
		act.lastNodeName = strings.TrimSpace(masterName)
	}
	rt.mu.Unlock()
}

func (rt *shareUserRuntime) activeWatchingSessionNode(tokenHash string, now time.Time) (int64, bool) {
	if rt == nil || tokenHash == "" {
		return 0, false
	}
	rt.mu.RLock()
	act := rt.activity[tokenHash]
	var nodeID int64
	var ok bool
	if act != nil && act.isWatching(now) && act.hasNode {
		nodeID = act.lastNodeID
		ok = true
	}
	rt.mu.RUnlock()
	return nodeID, ok
}

// activeWatchingCountsByNode counts active watching device sessions globally
// per node ID (0 = Master, >0 = Edge). One watching device counts as 1 stream
// regardless of parallel Range connections opened by clients like Infuse.
func (rt *shareUserRuntime) activeWatchingCountsByNode(now time.Time, excludeTokenHash string) map[int64]int {
	counts := make(map[int64]int)
	if rt == nil {
		return counts
	}
	rt.mu.RLock()
	for hash, act := range rt.activity {
		if hash == excludeTokenHash || act == nil || !act.isWatching(now) {
			continue
		}
		if act.hasNode {
			counts[act.lastNodeID]++
		} else {
			counts[0]++
		}
	}
	rt.mu.RUnlock()
	return counts
}

func (rt *shareUserRuntime) sessionEgressNodeID(tokenHash string) (int64, bool) {
	if rt == nil || tokenHash == "" {
		return 0, false
	}
	rt.mu.RLock()
	entry, ok := rt.sessions[tokenHash]
	rt.mu.RUnlock()
	if !ok {
		return 0, false
	}
	return entry.egressNodeID, true
}

func (rt *shareUserRuntime) revokeByHash(tokenHash string) error {
	if rt == nil || rt.db == nil || tokenHash == "" {
		return nil
	}
	return rt.revokeSession(tokenHash, nil)
}

// ---------------------------------------------------------------------------
// Proxy interception

type shareUpstreamCandidate struct {
	accountID int64
	username  string
	password  string
}

type shareLoginPending struct {
	rt              *shareUserRuntime
	user            *shareUserState
	siteID          int64
	siteName        string
	deviceID        string
	device          string
	client          string
	clientIP        string
	egressNodeID    int64
	virtualServerID string
	parsed          *shareLoginBody
	candidates      []shareUpstreamCandidate
	candidateIdx    int
}

func (p *shareLoginPending) currentAccountID() int64 {
	if p == nil || p.candidateIdx < 0 || p.candidateIdx >= len(p.candidates) {
		return 0
	}
	return p.candidates[p.candidateIdx].accountID
}

func (p *shareLoginPending) applyCandidate(r *http.Request) {
	if p == nil || p.candidateIdx < 0 || p.candidateIdx >= len(p.candidates) {
		return
	}
	cand := p.candidates[p.candidateIdx]
	body, contentType := p.parsed.rewrite(cand.username, cand.password)
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.TransferEncoding = nil
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	r.Header.Set("Content-Type", contentType)
	// The login response is parsed for the access token, so keep it plain.
	r.Header.Set("Accept-Encoding", "identity")
}

func (p *shareLoginPending) advanceCandidate(r *http.Request) bool {
	if p == nil || p.candidateIdx+1 >= len(p.candidates) {
		return false
	}
	p.candidateIdx++
	p.applyCandidate(r)
	return true
}

// intercept runs before a proxied request is forwarded. It returns the request
// to forward, a pending login when the request is a share-user sign-in, the
// share user to meter traffic against, the active session token hash, a finish
// hook for playback tracking, and whether a response was already written.
func (rt *shareUserRuntime) intercept(w http.ResponseWriter, r *http.Request, site Site, clientIP string) (*http.Request, *shareLoginPending, *shareUserState, string, func(), bool) {
	noop := func() {}
	if rt == nil {
		return r, nil, nil, "", noop, false
	}
	if isShareLoginRequest(r) {
		req, pending, user, handled := rt.interceptLogin(w, r, site, clientIP)
		return req, pending, user, "", noop, handled
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
			return r, nil, nil, "", noop, true
		}
		if !ok || entry.siteID != site.ID {
			rt.mu.RUnlock()
			continue
		}
		state := rt.users[entry.userID]
		reason := state.blockReasonLocked(now)
		act := rt.activity[hash]
		draining := act != nil && act.draining
		if reason == "" {
			if _, allowed := state.sites[site.ID]; !allowed {
				if !(draining && (act.isWatching(now) || isSharePlaybackStoppedRequest(r))) {
					reason = "site"
				}
			}
		}
		last := rt.lastSeen[hash]
		wasWatching := act != nil && act.isWatching(now)
		prevClientIP := entry.clientIP
		rt.mu.RUnlock()
		if reason != "" {
			if state != nil {
				if reason == "site" {
					_ = rt.revokeSession(hash, &entry)
				} else {
					rt.revokeAsync(entry.userID, 0, reason)
				}
			}
			writeShareJSONError(w, http.StatusUnauthorized, shareBlockMessage(reason))
			return r, nil, nil, "", noop, true
		}
		needTouch := now.Sub(last) >= shareSessionTouchInterval
		needIPUpdate := clientIP != "" && clientIP != prevClientIP
		if needTouch || needIPUpdate {
			rt.mu.Lock()
			if needTouch {
				rt.lastSeen[hash] = now
			}
			if needIPUpdate {
				if cur, exists := rt.sessions[hash]; exists {
					cur.clientIP = clientIP
					rt.sessions[hash] = cur
				}
				rt.dirtyClientIP[hash] = clientIP
			}
			rt.mu.Unlock()
		}
		if report, ok := readPlaybackTitleReport(r); ok {
			report.at = now
			r = r.WithContext(context.WithValue(r.Context(), playbackTitleReportKey{}, report))
		}
		finish, blocked, limit := rt.acquireRequestActivity(entry.userID, hash, r, now, true)
		if blocked {
			writeShareJSONError(w, http.StatusTooManyRequests, shareConcurrentStreamLimitMessage(limit))
			return r, nil, nil, "", noop, true
		}
		r = r.WithContext(withSharePreWatching(r.Context(), wasWatching))
		return r, nil, state, hash, finish, false
	}
	// Dynamic playback routes carry no Emby token; attribute them through the
	// capability handed to the share user earlier.
	if userID, tokenHash, ok := rt.capabilityOwner(r.URL.Path, site.ID); ok {
		rt.mu.RLock()
		state := rt.users[userID]
		reason := state.blockReasonLocked(now)
		var act *shareSessionActivity
		sessionOK := true
		if tokenHash != "" {
			entry, exists := rt.sessions[tokenHash]
			sessionOK = exists && entry.userID == userID && entry.siteID == site.ID
			act = rt.activity[tokenHash]
		}
		if reason == "" && !sessionOK {
			reason = "revoked"
		}
		if reason == "" {
			if _, allowed := state.sites[site.ID]; !allowed {
				if !(act != nil && act.draining && act.isWatching(now)) {
					reason = "site"
				}
			}
		}
		rt.mu.RUnlock()
		if reason != "" {
			msg := shareBlockMessage(reason)
			if reason == "revoked" {
				msg = "登录已失效，请重新登录"
			}
			writeShareJSONError(w, http.StatusUnauthorized, msg)
			return r, nil, nil, "", noop, true
		}
		finish, blocked, limit := rt.acquireRequestActivity(userID, tokenHash, r, now, isShareActiveMediaRequest(r))
		if blocked {
			writeShareJSONError(w, http.StatusTooManyRequests, shareConcurrentStreamLimitMessage(limit))
			return r, nil, nil, "", noop, true
		}
		return r, nil, state, tokenHash, finish, false
	}
	return r, nil, nil, "", noop, false
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
	routeUserID, isPersonalRoute := shareRouteUserFromContext(r.Context())
	if !known || state == nil || !assigned || (isPersonalRoute && userID != routeUserID) {
		if isPersonalRoute {
			_ = original.Close()
			writeShareJSONError(w, http.StatusUnauthorized, "用户名或密码错误")
			return r, nil, nil, true
		}
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
	candidates, err := rt.upstreamCredentialCandidates(site.ID, preferredAccount)
	if err != nil {
		log.Printf("[%s] share login for %s: %v", site.Name, lookup, err)
		writeShareJSONError(w, http.StatusServiceUnavailable, shareBlockMessage("account"))
		return r, nil, nil, true
	}
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
	var virtualServerID string
	if isPersonalRoute {
		virtualServerID = rt.personalVirtualServerID(state.id)
	}
	pending := &shareLoginPending{
		rt:              rt,
		user:            state,
		siteID:          site.ID,
		siteName:        site.Name,
		deviceID:        truncateShareText(deviceID, 128),
		device:          truncateShareText(device, 128),
		client:          truncateShareText(client, 128),
		clientIP:        clientIP,
		virtualServerID: virtualServerID,
		parsed:          parsed,
		candidates:      candidates,
	}
	pending.applyCandidate(r)
	return r, pending, state, false
}

func truncateShareText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

// upstreamCredentialCandidates returns an ordered list of valid upstream
// accounts for a site: if preferredAccount > 0 it is placed first with other
// accounts as failover backups; when preferredAccount == 0 (auto-balance) all
// non-expired accounts are ordered by fewest active sessions with round-robin
// tie-breaking, followed by any expired accounts as last resort.
var errNoSavedSiteAccountCredentials = errors.New("site has no saved account with a password")

func (rt *shareUserRuntime) upstreamCredentialCandidates(siteID, preferredAccount int64) ([]shareUpstreamCandidate, error) {
	accounts, err := rt.db.listSiteAccounts(siteID)
	if err != nil {
		return nil, err
	}
	activeCounts := make(map[int64]int)
	if rows, err := rt.db.db.Query(`SELECT site_account_id, COUNT(*) FROM share_user_sessions WHERE site_id=? AND site_account_id>0 GROUP BY site_account_id`, siteID); err == nil {
		for rows.Next() {
			var accID int64
			var cnt int
			if scanErr := rows.Scan(&accID, &cnt); scanErr == nil {
				activeCounts[accID] = cnt
			}
		}
		_ = rows.Close()
	}
	now := time.Now()
	type scoredCandidate struct {
		cand    shareUpstreamCandidate
		expired bool
		active  int
	}
	var preferred *scoredCandidate
	others := make([]scoredCandidate, 0, len(accounts))
	for _, item := range accounts {
		if item.Username == "" || item.PasswordCiphertext == "" {
			continue
		}
		password, decErr := decryptSiteAccountSecret(item.PasswordCiphertext)
		if decErr != nil || password == "" {
			continue
		}
		expired := false
		if exp := rt.db.shareExpiryTime(item.ExpiresAt); !exp.IsZero() && !now.Before(exp) {
			expired = true
		}
		sc := scoredCandidate{
			cand: shareUpstreamCandidate{
				accountID: item.ID,
				username:  item.Username,
				password:  password,
			},
			expired: expired,
			active:  activeCounts[item.ID],
		}
		if preferredAccount > 0 && item.ID == preferredAccount {
			preferred = &sc
		} else {
			others = append(others, sc)
		}
	}
	if len(others) > 1 {
		rr := rt.accountRR.Add(1)
		sort.SliceStable(others, func(i, j int) bool {
			if others[i].expired != others[j].expired {
				return !others[i].expired
			}
			if others[i].active != others[j].active {
				return others[i].active < others[j].active
			}
			return others[i].cand.accountID < others[j].cand.accountID
		})
		// Rotate equal-priority leading accounts so concurrent or zero-session logins spread evenly.
		sameEnd := 1
		for sameEnd < len(others) && others[sameEnd].expired == others[0].expired && others[sameEnd].active == others[0].active {
			sameEnd++
		}
		if sameEnd > 1 {
			shift := int(rr % uint64(sameEnd)) // #nosec G115 -- sameEnd is positive and bounded by len(others).
			if shift > 0 {
				rotated := append([]scoredCandidate{}, others[shift:sameEnd]...)
				rotated = append(rotated, others[:shift]...)
				copy(others[:sameEnd], rotated)
			}
		}
	}
	result := make([]shareUpstreamCandidate, 0, len(others)+1)
	if preferred != nil {
		result = append(result, preferred.cand)
	}
	for _, sc := range others {
		result = append(result, sc.cand)
	}
	if len(result) == 0 {
		return nil, errNoSavedSiteAccountCredentials
	}
	return result, nil
}

// shareLoginCapture buffers the upstream login response so the access token
// can be recorded before the response reaches the client, and headers stay
// isolated until flushed so 401/403/429 account failover never leaks stale headers.
type shareLoginCapture struct {
	target      http.ResponseWriter
	headers     http.Header
	status      int
	buffer      bytes.Buffer
	passthrough bool
	wroteHeader bool
}

func newShareLoginCapture(target http.ResponseWriter) *shareLoginCapture {
	return &shareLoginCapture{target: target, headers: make(http.Header)}
}

func (c *shareLoginCapture) Header() http.Header {
	if c.passthrough {
		return c.target.Header()
	}
	return c.headers
}

func (c *shareLoginCapture) copyHeadersToTarget() {
	dst := c.target.Header()
	for k, vals := range c.headers {
		dst[k] = append([]string(nil), vals...)
	}
}

func (c *shareLoginCapture) shouldFailover() bool {
	if c.passthrough {
		return false
	}
	return c.status == http.StatusUnauthorized || c.status == http.StatusForbidden || c.status == http.StatusTooManyRequests
}

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
		c.copyHeadersToTarget()
		c.target.WriteHeader(c.status)
		if _, err := c.target.Write(c.buffer.Bytes()); err != nil { // #nosec G705 -- relays the upstream Emby login response unchanged, with its own headers and content type.
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
	bodyBytes := c.buffer.Bytes()
	if status == http.StatusOK && pending != nil {
		var payload struct {
			AccessToken string `json:"AccessToken"`
			User        struct {
				ID string `json:"Id"`
			} `json:"User"`
		}
		if err := json.Unmarshal(bodyBytes, &payload); err == nil && strings.TrimSpace(payload.AccessToken) != "" {
			if err := pending.rt.registerSession(pending, payload.AccessToken, strings.TrimSpace(payload.User.ID)); err != nil {
				log.Printf("share login: record session: %v", err)
			}
		}
		if pending.virtualServerID != "" {
			if rewritten := rewriteShareLoginResponseServerID(bodyBytes, pending.virtualServerID); len(rewritten) > 0 {
				bodyBytes = rewritten
				c.headers.Del("Content-Encoding")
				c.headers.Set("Content-Length", strconv.Itoa(len(bodyBytes)))
			}
		}
	}
	c.copyHeadersToTarget()
	c.target.WriteHeader(status)
	_, _ = c.target.Write(bodyBytes) // #nosec G705 -- relays the upstream Emby login response unchanged except normalized virtual ServerId.
}

func (rt *shareUserRuntime) registerSession(pending *shareLoginPending, token string, upstreamUIDs ...string) error {
	upstreamUID := ""
	if len(upstreamUIDs) > 0 {
		upstreamUID = strings.TrimSpace(upstreamUIDs[0])
	}
	ciphertext, err := encryptSiteAccountSecret(token)
	if err != nil {
		return err
	}
	hash := shareTokenHash(token)
	accountID := pending.currentAccountID()
	ispGroup := ""
	if rt.pm != nil && rt.pm.edges != nil && pending.clientIP != "" {
		ispGroup = rt.pm.edges.lookupISPGroup(pending.clientIP)
	}
	rt.sessionStoreMu.Lock()
	if err := rt.persistRegisteredSession(pending, hash, ciphertext, upstreamUID); err != nil {
		rt.sessionStoreMu.Unlock()
		return err
	}
	rt.mu.Lock()
	delete(rt.revoked, hash)
	delete(rt.dirtyClientIP, hash)
	rt.loginGeneration++
	rt.sessions[hash] = shareSessionEntry{
		loginGeneration: rt.loginGeneration,
		userID:        pending.user.id,
		siteID:        pending.siteID,
		siteAccountID: accountID,
		egressNodeID:  pending.egressNodeID,
		clientToken:   token,
		upstreamToken: token,
		clientUID:     upstreamUID,
		upstreamUID:   upstreamUID,
		deviceID:      pending.deviceID,
		deviceName:    pending.device,
		clientName:    pending.client,
		clientIP:      pending.clientIP,
		ispGroup:      ispGroup,
	}
	rt.lastSeen[hash] = time.Now()
	if act := rt.activity[hash]; act != nil {
		act.draining = false
		act.drainingSince = time.Time{}
	}
	maxDevices := pending.user.maxDevices
	rt.mu.Unlock()
	rt.sessionStoreMu.Unlock()
	if err := rt.enforceDeviceLimit(pending.user.id, pending.siteID, hash, pending.deviceID, maxDevices); err != nil {
		log.Printf("share user %d: device limit: %v", pending.user.id, err)
	}
	return nil
}

// persistRegisteredSession is called with sessionStoreMu held.
func (rt *shareUserRuntime) persistRegisteredSession(pending *shareLoginPending, hash, ciphertext, upstreamUID string) error {
	tx, err := rt.db.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO share_user_sessions(token_hash, share_user_id, site_id, site_account_id, egress_node_id, token_ciphertext, upstream_token_ciphertext, client_user_id, upstream_user_id, device_id, device_name, client, client_ip) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(token_hash) DO UPDATE SET share_user_id=excluded.share_user_id, site_id=excluded.site_id, site_account_id=excluded.site_account_id, egress_node_id=excluded.egress_node_id, token_ciphertext=excluded.token_ciphertext, upstream_token_ciphertext=excluded.upstream_token_ciphertext, client_user_id=excluded.client_user_id, upstream_user_id=excluded.upstream_user_id, device_id=excluded.device_id, device_name=excluded.device_name, client=excluded.client, client_ip=excluded.client_ip, last_seen_at=CURRENT_TIMESTAMP`,
		hash, pending.user.id, pending.siteID, pending.currentAccountID(), pending.egressNodeID, ciphertext, "", upstreamUID, upstreamUID, pending.deviceID, pending.device, pending.client, pending.clientIP); err != nil {
		return err
	}
	// Upstreams may hand the same token back to a device that signs in again.
	if _, err := tx.Exec(`DELETE FROM share_revoked_tokens WHERE token_hash=?`, hash); err != nil {
		return err
	}
	return tx.Commit()
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
		if k == keepKey && (session.SiteID == siteID || session.Draining) {
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
	TokenHash          string `json:"-"`
	SessionID          string `json:"session_id,omitempty"`
	ciphertext         string
	upstreamCiphertext string
	egressNodeID       int64
	lastSeen           time.Time
	SiteID       int64  `json:"site_id"`
	SiteName     string `json:"site_name"`
	DeviceID     string `json:"-"`
	DeviceName   string `json:"device_name"`
	Client       string `json:"client"`
	ClientIP     string `json:"client_ip"`
	ClientRegion string `json:"client_region,omitempty"`
	NodeName     string `json:"node_name,omitempty"`
	CreatedAt    string `json:"created_at"`
	LastSeenAt   string `json:"last_seen_at"`
	Draining     bool   `json:"draining,omitempty"`
	Playing      bool   `json:"playing,omitempty"`
}

type liveShareSessionView struct {
	ShareUserID  int64  `json:"share_user_id"`
	Username     string `json:"username"`
	SessionID    string `json:"session_id"`
	SiteID       int64  `json:"site_id"`
	SiteName     string `json:"site_name"`
	DeviceName   string `json:"device_name"`
	Client       string `json:"client"`
	ClientIP     string `json:"client_ip"`
	ClientRegion string `json:"client_region,omitempty"`
	NodeName     string `json:"node_name,omitempty"`
	Playing      bool   `json:"playing"`
	NowPlayingTitle string `json:"now_playing_title,omitempty"`
	Draining     bool   `json:"draining,omitempty"`
	LastSeenAt   string `json:"last_seen_at"`
}

func (rt *shareUserRuntime) lookupSessionRegion(clientIP string) string {
	if rt == nil || rt.pm == nil || rt.pm.edges == nil || rt.pm.edges.ip2region == nil || strings.TrimSpace(clientIP) == "" {
		return ""
	}
	if disp, ok := rt.pm.edges.ip2region.LookupDisplay(clientIP); ok {
		return disp
	}
	return ""
}

func (rt *shareUserRuntime) listSessions(userID int64) ([]shareStoredSession, error) {
	rows, err := rt.db.db.Query(`SELECT s.token_hash, s.token_ciphertext, s.upstream_token_ciphertext, s.site_id, COALESCE(t.name, ''), s.device_id, s.device_name, s.client, s.client_ip, s.egress_node_id, COALESCE(s.created_at, ''), COALESCE(s.last_seen_at, '') FROM share_user_sessions s LEFT JOIN sites t ON t.id=s.site_id WHERE s.share_user_id=? ORDER BY s.last_seen_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]shareStoredSession, 0)
	for rows.Next() {
		var item shareStoredSession
		if err := rows.Scan(&item.TokenHash, &item.ciphertext, &item.upstreamCiphertext, &item.SiteID, &item.SiteName, &item.DeviceID, &item.DeviceName, &item.Client, &item.ClientIP, &item.egressNodeID, &item.CreatedAt, &item.LastSeenAt); err != nil {
			return nil, err
		}
		item.SessionID = item.TokenHash
		if parsed, err := time.Parse("2006-01-02 15:04:05", item.LastSeenAt); err == nil {
			item.lastSeen = parsed
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	masterName := rt.masterNodeName()
	now := time.Now()
	rt.mu.RLock()
	for i := range result {
		if entry, ok := rt.sessions[result[i].TokenHash]; ok {
			if entry.clientIP != "" {
				result[i].ClientIP = entry.clientIP
			}
			if entry.egressNodeID != 0 {
				result[i].egressNodeID = entry.egressNodeID
			}
		}
		if act := rt.activity[result[i].TokenHash]; act != nil {
			result[i].Draining = act.draining
			result[i].Playing = act.isWatching(now)
			if act.lastNodeName != "" {
				if act.lastNodeName == defaultMasterNodeName {
					result[i].NodeName = masterName
				} else {
					result[i].NodeName = act.lastNodeName
				}
			} else if result[i].Playing {
				result[i].NodeName = masterName
			}
		}
	}
	rt.mu.RUnlock()
	for i := range result {
		result[i].ClientRegion = rt.lookupSessionRegion(result[i].ClientIP)
		if result[i].NodeName == "" {
			if result[i].egressNodeID > 0 && rt.pm != nil && rt.pm.edges != nil {
				result[i].NodeName = rt.pm.edges.nodeNameByID(result[i].egressNodeID)
			} else if result[i].egressNodeID < 0 {
				result[i].NodeName = masterName
			}
		}
	}
	return result, nil
}

func (rt *shareUserRuntime) masterNodeName() string {
	if rt != nil && rt.pm != nil && rt.pm.edges != nil {
		return rt.pm.edges.masterDisplayName()
	}
	return defaultMasterNodeName
}

func (rt *shareUserRuntime) analyticsSessionSummary() (onlineCount, masterPlaying, totalPlaying int, playingByUser map[int64]int) {
	playingByUser = make(map[int64]int)
	if rt == nil || rt.db == nil {
		return 0, 0, 0, playingByUser
	}
	_ = rt.db.db.QueryRow(`SELECT COUNT(*) FROM share_user_sessions`).Scan(&onlineCount)
	masterName := rt.masterNodeName()
	now := time.Now()
	rt.mu.RLock()
	for tokenHash, act := range rt.activity {
		if act != nil && act.isWatching(now) {
			totalPlaying++
			if entry, ok := rt.sessions[tokenHash]; ok && entry.userID > 0 {
				playingByUser[entry.userID]++
			}
			if act.lastNodeName == "" || act.lastNodeName == defaultMasterNodeName || act.lastNodeName == masterName {
				masterPlaying++
			}
		}
	}
	rt.mu.RUnlock()
	if totalPlaying > onlineCount {
		onlineCount = totalPlaying
	}
	return onlineCount, masterPlaying, totalPlaying, playingByUser
}

func (rt *shareUserRuntime) listAllLiveSessions() ([]liveShareSessionView, error) {
	if rt == nil || rt.db == nil {
		return []liveShareSessionView{}, nil
	}
	rows, err := rt.db.db.Query(`SELECT s.share_user_id, COALESCE(u.username, ''), s.token_hash, s.site_id, COALESCE(t.name, ''), s.device_name, s.client, s.client_ip, s.egress_node_id, COALESCE(s.last_seen_at, '') FROM share_user_sessions s JOIN share_users u ON u.id=s.share_user_id LEFT JOIN sites t ON t.id=s.site_id ORDER BY s.last_seen_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]liveShareSessionView, 0)
	egressNodes := make([]int64, 0)
	for rows.Next() {
		var item liveShareSessionView
		var egressNodeID int64
		if err := rows.Scan(&item.ShareUserID, &item.Username, &item.SessionID, &item.SiteID, &item.SiteName, &item.DeviceName, &item.Client, &item.ClientIP, &egressNodeID, &item.LastSeenAt); err != nil {
			return nil, err
		}
		item.ClientRegion = rt.lookupSessionRegion(item.ClientIP)
		result = append(result, item)
		egressNodes = append(egressNodes, egressNodeID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	masterName := rt.masterNodeName()
	now := time.Now()
	rt.mu.RLock()
	for i := range result {
		if entry, ok := rt.sessions[result[i].SessionID]; ok {
			if entry.clientIP != "" {
				result[i].ClientIP = entry.clientIP
			}
			if entry.egressNodeID != 0 {
				egressNodes[i] = entry.egressNodeID
			}
		}
		if act := rt.activity[result[i].SessionID]; act != nil {
			result[i].Draining = act.draining
			result[i].Playing = act.isWatching(now)
			if current := act.mediaTitle.current; result[i].Playing && current != nil && now.Sub(act.mediaTitle.updatedAt) < sharePlayingIdleTimeout {
				result[i].NowPlayingTitle = current.title
			}
			if act.lastNodeName != "" {
				if act.lastNodeName == defaultMasterNodeName {
					result[i].NodeName = masterName
				} else {
					result[i].NodeName = act.lastNodeName
				}
			} else if result[i].Playing {
				result[i].NodeName = masterName
			}
		}
	}
	rt.mu.RUnlock()
	for i := range result {
		result[i].ClientRegion = rt.lookupSessionRegion(result[i].ClientIP)
		if result[i].NodeName == "" {
			if egressNodes[i] > 0 && rt.pm != nil && rt.pm.edges != nil {
				result[i].NodeName = rt.pm.edges.nodeNameByID(egressNodes[i])
			} else if egressNodes[i] < 0 {
				result[i].NodeName = masterName
			}
		}
	}
	// Sort playing sessions first, preserving recency order within each bucket.
	playing := make([]liveShareSessionView, 0, len(result))
	idle := make([]liveShareSessionView, 0, len(result))
	for _, item := range result {
		if item.Playing {
			playing = append(playing, item)
		} else {
			idle = append(idle, item)
		}
	}
	return append(playing, idle...), nil
}

func (rt *shareUserRuntime) revokeUserSession(userID int64, sessionID string) (bool, error) {
	if rt == nil || rt.db == nil {
		return false, nil
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false, nil
	}
	var session shareStoredSession
	err := rt.db.db.QueryRow(`SELECT token_hash, token_ciphertext, upstream_token_ciphertext, site_id, device_id, egress_node_id FROM share_user_sessions WHERE share_user_id=? AND token_hash=?`, userID, sessionID).
		Scan(&session.TokenHash, &session.ciphertext, &session.upstreamCiphertext, &session.SiteID, &session.DeviceID, &session.egressNodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := rt.revokeOne(session); err != nil {
		return false, err
	}
	return true, nil
}

// revokeOne denylists one session's token, removes it and logs it out upstream.
func (rt *shareUserRuntime) revokeOne(session shareStoredSession) error {
	return rt.revokeSession(session.TokenHash, nil)
}

// Re-read the session under the store lock so a kick logs out the token that
// actually owns the session, even if it moved to a different site meanwhile.
func (rt *shareUserRuntime) revokeSession(tokenHash string, expected *shareSessionEntry) error {
	rt.sessionStoreMu.Lock()
	if expected != nil {
		rt.mu.RLock()
		cur, exists := rt.sessions[tokenHash]
		rt.mu.RUnlock()
		if !exists || !cur.sameHandoffSource(*expected) {
			rt.sessionStoreMu.Unlock()
			return nil
		}
	}
	session, err := rt.revokeSessionLocked(tokenHash)
	rt.sessionStoreMu.Unlock()
	if err != nil {
		return err
	}
	rt.logoutStoredShareSession(context.Background(), session)
	return nil
}

func (rt *shareUserRuntime) revokeSessionLocked(tokenHash string) (shareStoredSession, error) {
	session, exists, err := rt.persistSessionRevocation(tokenHash)
	if err != nil {
		return session, err
	}
	rt.mu.Lock()
	delete(rt.sessions, tokenHash)
	delete(rt.lastSeen, tokenHash)
	delete(rt.dirtyClientIP, tokenHash)
	delete(rt.activity, tokenHash)
	if exists {
		rt.revoked[tokenHash] = session.SiteID
	}
	rt.mu.Unlock()
	rt.handoffMu.Lock()
	delete(rt.handoffLocks, tokenHash)
	rt.handoffMu.Unlock()
	return session, nil
}

func (rt *shareUserRuntime) logoutStoredShareSession(ctx context.Context, session shareStoredSession) {
	activeCipher := session.upstreamCiphertext
	if activeCipher == "" {
		activeCipher = session.ciphertext
	}
	if token, err := decryptSiteAccountSecret(activeCipher); err == nil && token != "" {
		rt.logoutUpstreamWithContext(ctx, session.SiteID, token, session.egressNodeID)
	}
}

// For a registered share session the gateway owns the logout. Persist the
// revocation before acknowledging it, then clean up its actual upstream token
// independently of the request. Native upstream logouts still pass through.
func (rt *shareUserRuntime) handleShareClientLogout(w http.ResponseWriter, r *http.Request, siteID int64) bool {
	if rt == nil || !isShareClientLogoutRequest(r) {
		return false
	}
	routeUserID, personal := shareRouteUserFromContext(r.Context())
	for _, token := range embyRequestTokens(r) {
		hash := shareTokenHash(token)
		rt.sessionStoreMu.Lock()
		rt.mu.RLock()
		entry, exists := rt.sessions[hash]
		rt.mu.RUnlock()
		if !exists || (personal && entry.userID != routeUserID) || (!personal && entry.siteID != siteID) {
			rt.sessionStoreMu.Unlock()
			continue
		}
		session, err := rt.revokeSessionLocked(hash)
		rt.sessionStoreMu.Unlock()
		if err != nil {
			log.Printf("share user #%d: persist client logout: %v", entry.userID, err)
			writeShareJSONError(w, http.StatusServiceUnavailable, "暂时无法退出，请重试")
		} else {
			rt.logoutStoredShareSession(withShareHandoffClientHeaders(r.Context(), r), session)
			w.WriteHeader(http.StatusNoContent)
		}
		return true
	}
	return false
}

// persistSessionRevocation is called with sessionStoreMu held.
func (rt *shareUserRuntime) persistSessionRevocation(tokenHash string) (session shareStoredSession, exists bool, err error) {
	tx, err := rt.db.db.Begin()
	if err != nil {
		return session, false, err
	}
	defer tx.Rollback()
	err = tx.QueryRow(`SELECT token_ciphertext, upstream_token_ciphertext, site_id, egress_node_id FROM share_user_sessions WHERE token_hash=?`, tokenHash).
		Scan(&session.ciphertext, &session.upstreamCiphertext, &session.SiteID, &session.egressNodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return session, false, nil
	}
	if err != nil {
		return session, false, err
	}
	if _, err := tx.Exec(`INSERT INTO share_revoked_tokens(token_hash, site_id) VALUES(?,?) ON CONFLICT(token_hash) DO UPDATE SET site_id=excluded.site_id, revoked_at=CURRENT_TIMESTAMP`, tokenHash, session.SiteID); err != nil {
		return session, false, err
	}
	if _, err := tx.Exec(`DELETE FROM share_user_sessions WHERE token_hash=?`, tokenHash); err != nil {
		return session, false, err
	}
	return session, true, tx.Commit()
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

func (rt *shareUserRuntime) logoutUpstream(siteID int64, token string, egressNodeID int64) {
	rt.logoutUpstreamWithContext(context.Background(), siteID, token, egressNodeID)
}

func (rt *shareUserRuntime) logoutUpstreamWithContext(ctx context.Context, siteID int64, token string, egressNodeID int64) {
	if rt.pm == nil {
		return
	}
	rt.pm.mu.RLock()
	inst := rt.pm.proxies[siteID]
	rt.pm.mu.RUnlock()
	if inst == nil || inst.shareLogout == nil {
		return
	}
	var egressTarget *edgeEgressTarget
	if inst.Site.EdgeRoutingMode == edgeRoutingModeEdgeFull {
		nodeID := inst.Site.EdgePinnedNodeID
		if nodeID <= 0 {
			nodeID = egressNodeID
		}
		if nodeID > 0 {
			if rt.pm.edges == nil {
				log.Printf("[%s] skip upstream logout: bound edge node #%d unavailable", inst.Site.Name, nodeID)
				return
			}
			decision, ok := rt.pm.edges.nodeDecisionByID(nodeID)
			if !ok {
				log.Printf("[%s] skip upstream logout: bound edge node #%d offline (preventing master IP leak)", inst.Site.Name, nodeID)
				return
			}
			egressTarget = &edgeEgressTarget{
				NodeID:    decision.NodeID,
				NodeName:  decision.NodeName,
				PublicURL: decision.PublicURL,
				Token:     decision.Token,
			}
		}
	}
	logout := inst.shareLogout
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shareLogoutTimeout)
		defer cancel()
		if egressTarget != nil {
			ctx = withEdgeEgressTarget(ctx, *egressTarget)
		}
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
		clientHdrs := shareHandoffClientHeadersFromContext(ctx)
		request.Header.Set("User-Agent", sanitizeShareHandoffUserAgent(clientHdrs.UserAgent))
		request.Header.Set("X-Emby-Token", token)
		authVersion := quoteEmbyAuthValue(clientHdrs.ClientVersion, "4.8.10")
		request.Header.Set("X-Emby-Authorization", fmt.Sprintf(`MediaBrowser Client="Yundu", Device="Yundu", DeviceId="yundu-share", Version="%s", Token="%s"`, authVersion, token))
		applyUAHeaderPolicy(request.Header, policy)
		headers.apply(request.Header, &endpoint)
		response, err := (&http.Client{Transport: transport, Timeout: shareLogoutTimeout}).Do(request)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
		if response.StatusCode >= 400 &&
			response.StatusCode != http.StatusUnauthorized &&
			response.StatusCode != http.StatusNotFound &&
			response.StatusCode != http.StatusMethodNotAllowed {
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
	rt.mu.Lock()
	states := make([]*shareUserState, 0, len(rt.users))
	for _, state := range rt.users {
		states = append(states, state)
	}
	touched := make(map[string]time.Time, len(rt.lastSeen))
	for hash, at := range rt.lastSeen {
		touched[hash] = at
	}
	dirtyIPs := make(map[string]string, len(rt.dirtyClientIP))
	for hash, ip := range rt.dirtyClientIP {
		dirtyIPs[hash] = ip
	}
	rt.dirtyClientIP = make(map[string]string)
	rt.mu.Unlock()

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
	for hash, ip := range dirtyIPs {
		_, _ = rt.db.db.Exec(`UPDATE share_user_sessions SET client_ip=? WHERE token_hash=?`, ip, hash)
	}
	rt.applyMonthlyResets(time.Now().In(rt.db.shareLocation()))
	cutoff := time.Now().Add(-shareRevokedTokenRetention).UTC().Format("2006-01-02 15:04:05")
	_, _ = rt.db.db.Exec(`DELETE FROM share_revoked_tokens WHERE revoked_at < ?`, cutoff)
	if err := rt.reload(); err != nil {
		log.Printf("share users: reload: %v", err)
		return
	}
	rt.sweepDrainingSessions(time.Now())
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
	if hasUpToken, err := backupSQLiteColumnExists(tx, "share_user_sessions", "upstream_token_ciphertext"); err != nil {
		return err
	} else if hasUpToken {
		if err := migrate("SELECT token_hash, upstream_token_ciphertext FROM share_user_sessions", "UPDATE share_user_sessions SET upstream_token_ciphertext=? WHERE token_hash=?"); err != nil {
			return err
		}
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
