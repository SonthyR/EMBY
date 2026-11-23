package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	edgeNodeTLSModeAuto      = "auto"
	edgeNodeTLSModeExternal  = "external"
	edgeNodeTLSModeHTTP      = "http"
	edgeNodeOfflineAfter     = 30 * time.Second
	edgeTicketTTL            = 6 * time.Hour
	edgeTicketPathPrefix     = "/_yundu/p/"
	edgeMaxTicketBytes       = 16 << 10
	edgeHeartbeatBodyLimit   = 1 << 20
	edgeNodeDefaultPort      = 19090
	edgeInstallScriptRawBase = "https://raw.githubusercontent.com/SonthyR/EMBY/master/install.sh"
)

const edgeNodeSchemaSQL = `
CREATE TABLE IF NOT EXISTS edge_nodes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	public_port INTEGER NOT NULL DEFAULT 19090,
	listen_port INTEGER NOT NULL DEFAULT 0,
	public_host TEXT NOT NULL DEFAULT '',
	public_ip TEXT NOT NULL DEFAULT '',
	detected_ip TEXT NOT NULL DEFAULT '',
	tls_mode TEXT NOT NULL DEFAULT 'auto',
	token_hash TEXT NOT NULL,
	token_ciphertext TEXT NOT NULL DEFAULT '',
	enabled INTEGER NOT NULL DEFAULT 1,
	traffic_quota INTEGER NOT NULL DEFAULT 0,
	traffic_used INTEGER NOT NULL DEFAULT 0,
	traffic_reset_day INTEGER NOT NULL DEFAULT 0,
	traffic_reset_period TEXT NOT NULL DEFAULT '',
	note TEXT NOT NULL DEFAULT '',
	agent_version TEXT NOT NULL DEFAULT '',
	last_seen_at TEXT NOT NULL DEFAULT '',
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS isp_route_rules (
	isp_group TEXT NOT NULL,
	node_id INTEGER NOT NULL,
	sort_order INTEGER NOT NULL DEFAULT 0,
	enabled INTEGER NOT NULL DEFAULT 1,
	PRIMARY KEY(isp_group, node_id)
);
`

func ensureEdgeNodeSchema(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, edgeNodeSchemaSQL); err != nil {
		return err
	}
	for _, group := range allISPGroups {
		if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO isp_route_rules(isp_group, node_id, sort_order, enabled) VALUES(?, 0, 1, 1)`, group); err != nil {
			return err
		}
	}
	return nil
}

// reencryptEdgeNodeSecrets moves edge node secrets to a new credential key during backup restore.
func reencryptEdgeNodeSecrets(tx *sql.Tx, oldKey, newKey []byte) error {
	hasTable, err := backupSQLiteColumnExists(tx, "edge_nodes", "token_ciphertext")
	if err != nil || !hasTable {
		return err
	}
	rows, err := tx.Query(`SELECT id, token_ciphertext FROM edge_nodes`)
	if err != nil {
		return err
	}
	type updateRow struct {
		id     int64
		cipher string
	}
	var updates []updateRow
	for rows.Next() {
		var item updateRow
		if err := rows.Scan(&item.id, &item.cipher); err != nil {
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
		if _, err := tx.Exec(`UPDATE edge_nodes SET token_ciphertext=? WHERE id=?`, item.cipher, item.id); err != nil {
			return err
		}
	}
	return nil
}

func generateEdgeNodeToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func edgeNodeTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func edgeTicketKeyFromToken(token string) []byte {
	sum := sha256.Sum256(append([]byte("yundu-edge-ticket:v1:"), []byte(token)...))
	return sum[:]
}

type edgePlaybackTicket struct {
	Version      int               `json:"v"`
	NodeID       int64             `json:"n"`
	SiteID       int64             `json:"s"`
	ShareUserID  int64             `json:"u,omitempty"`
	TokenHash    string            `json:"th,omitempty"`
	SpeedMbps    int               `json:"sp,omitempty"`
	TargetURL    string            `json:"t"`
	Headers      map[string]string `json:"h,omitempty"`
	ClientIPMode string            `json:"cm,omitempty"`
	FollowRedir  bool              `json:"fr,omitempty"`
	ExpiresAt    int64             `json:"exp"`
}

func sealEdgePlaybackTicket(token string, ticket edgePlaybackTicket) (string, error) {
	if token == "" {
		return "", errors.New("edge node token unavailable")
	}
	ticket.Version = 1
	plain, err := json.Marshal(ticket)
	if err != nil {
		return "", err
	}
	if len(plain) > edgeMaxTicketBytes {
		return "", errors.New("playback ticket too large")
	}
	block, err := aes.NewCipher(edgeTicketKeyFromToken(token))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, plain, nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func openEdgePlaybackTicket(token, encoded string) (edgePlaybackTicket, error) {
	var ticket edgePlaybackTicket
	if token == "" || encoded == "" || len(encoded) > edgeMaxTicketBytes*2 {
		return ticket, errors.New("invalid playback ticket")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return ticket, errors.New("malformed playback ticket")
	}
	block, err := aes.NewCipher(edgeTicketKeyFromToken(token))
	if err != nil {
		return ticket, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return ticket, err
	}
	if len(raw) < gcm.NonceSize()+gcm.Overhead() {
		return ticket, errors.New("truncated playback ticket")
	}
	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return ticket, errors.New("invalid playback ticket signature")
	}
	if err := json.Unmarshal(plain, &ticket); err != nil {
		return ticket, errors.New("corrupt playback ticket")
	}
	if ticket.Version != 1 || ticket.TargetURL == "" {
		return ticket, errors.New("unsupported playback ticket")
	}
	if ticket.ExpiresAt > 0 && time.Now().Unix() > ticket.ExpiresAt {
		return ticket, errors.New("playback ticket expired")
	}
	return ticket, nil
}

// ---------------------------------------------------------------------------
// Edge node DTOs and runtime state

type EdgeNode struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	PublicPort     int    `json:"public_port"`
	ListenPort     int    `json:"listen_port"`
	PublicHost     string `json:"public_host"`
	PublicIP       string `json:"public_ip"`
	DetectedIP     string `json:"detected_ip"`
	TLSMode        string `json:"tls_mode"`
	Enabled        bool   `json:"enabled"`
	TrafficQuota   int64  `json:"traffic_quota"`
	TrafficUsed    int64  `json:"traffic_used"`
	ResetDay       int    `json:"traffic_reset_day"`
	NextResetAt    string `json:"next_reset_at,omitempty"`
	Note           string `json:"note"`
	AgentVersion   string `json:"agent_version"`
	LastSeenAt     string `json:"last_seen_at"`
	Online         bool   `json:"online"`
	TLSActive      bool   `json:"tls_active"`
	ActiveStreams  int    `json:"active_streams"`
	CurrentBps     int64  `json:"current_bps"`
	PublicURL      string `json:"public_url"`
	Status         string `json:"status"` // online, offline, disabled, quota, waiting
	InstallCommand string `json:"install_command,omitempty"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

type EdgeNodeInput struct {
	Name         string `json:"name"`
	PublicPort   int    `json:"public_port"`
	ListenPort   int    `json:"listen_port"`
	PublicHost   string `json:"public_host"`
	PublicIP     string `json:"public_ip"`
	TLSMode      string `json:"tls_mode"`
	Enabled      *bool  `json:"enabled"`
	TrafficQuota int64  `json:"traffic_quota"`
	ResetDay     int    `json:"traffic_reset_day"`
	Note         string `json:"note"`
}

type edgeNodeLiveState struct {
	id            int64
	name          string
	publicPort    int
	listenPort    int
	publicHost    string
	publicIP      string
	detectedIP    string
	tlsMode       string
	tokenHash     string
	tokenPlain    string
	enabled       bool
	quota         int64
	persistedUsed int64
	resetDay      int
	resetPeriod   string
	note          string
	agentVersion  string
	lastSeen      time.Time
	tlsActive     bool
	activeStreams int
	currentBps    int64

	pending atomic.Int64
}

func (n *edgeNodeLiveState) effectiveListenPort() int {
	if n.listenPort > 0 {
		return n.listenPort
	}
	return n.publicPort
}

func (n *edgeNodeLiveState) isOnline(now time.Time) bool {
	return !n.lastSeen.IsZero() && now.Sub(n.lastSeen) <= edgeNodeOfflineAfter
}

func (n *edgeNodeLiveState) totalUsed() int64 {
	return n.persistedUsed + n.pending.Load()
}

func (n *edgeNodeLiveState) publicURL() string {
	if n == nil || n.publicPort <= 0 {
		return ""
	}
	if n.publicHost != "" {
		scheme := "https"
		if n.tlsMode == edgeNodeTLSModeHTTP {
			scheme = "http"
		} else if n.tlsMode == edgeNodeTLSModeAuto && !n.tlsActive {
			// Wait until TLS is active if auto cert was requested, unless fallback needed.
			scheme = "https"
		}
		if (scheme == "https" && n.publicPort == 443) || (scheme == "http" && n.publicPort == 80) {
			return scheme + "://" + n.publicHost
		}
		return fmt.Sprintf("%s://%s:%d", scheme, n.publicHost, n.publicPort)
	}
	ip := strings.TrimSpace(n.publicIP)
	if ip == "" {
		ip = strings.TrimSpace(n.detectedIP)
	}
	if ip == "" {
		return ""
	}
	host := ip
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	if n.publicPort == 80 {
		return "http://" + host
	}
	return fmt.Sprintf("http://%s:%d", host, n.publicPort)
}

func (n *edgeNodeLiveState) statusAt(now time.Time) string {
	if !n.enabled {
		return "disabled"
	}
	if n.quota > 0 && n.totalUsed() >= n.quota {
		return "quota"
	}
	if n.isOnline(now) {
		if n.publicURL() == "" {
			return "waiting"
		}
		return "online"
	}
	if n.lastSeen.IsZero() {
		return "waiting"
	}
	return "offline"
}

func (n *edgeNodeLiveState) routableAt(now time.Time) bool {
	if n == nil || !n.enabled || !n.isOnline(now) || n.tokenPlain == "" {
		return false
	}
	if n.quota > 0 && n.totalUsed() >= n.quota {
		return false
	}
	if n.publicHost != "" && n.tlsMode == edgeNodeTLSModeAuto && !n.tlsActive {
		return false
	}
	return n.publicURL() != ""
}

type ispRouteRule struct {
	NodeID    int64 `json:"node_id"`
	SortOrder int   `json:"sort_order"`
	Enabled   bool  `json:"enabled"`
}

type edgeNodeRuntime struct {
	db        *DB
	pm        *ProxyManager
	ip2region *ip2regionManager

	mu      sync.RWMutex
	nodes   map[int64]*edgeNodeLiveState
	routes  map[string][]ispRouteRule // isp_group -> ordered rules
	flushMu sync.Mutex
}

func newEdgeNodeRuntime(db *DB, pm *ProxyManager, ip2 *ip2regionManager) *edgeNodeRuntime {
	rt := &edgeNodeRuntime{
		db:        db,
		pm:        pm,
		ip2region: ip2,
		nodes:     make(map[int64]*edgeNodeLiveState),
		routes:    make(map[string][]ispRouteRule),
	}
	if db != nil {
		if err := rt.reload(); err != nil {
			log.Printf("edge nodes: initial load failed: %v", err)
		}
	}
	return rt
}

func (rt *edgeNodeRuntime) reload() error {
	if rt == nil || rt.db == nil {
		return nil
	}
	rows, err := rt.db.db.Query(`SELECT id, name, public_port, listen_port, public_host, public_ip, detected_ip, tls_mode, token_hash, token_ciphertext, enabled, traffic_quota, traffic_used, traffic_reset_day, traffic_reset_period, note, agent_version, last_seen_at FROM edge_nodes ORDER BY id`)
	if err != nil {
		return err
	}
	type loadedNode struct {
		id, quota, used                  int64
		publicPort, listenPort, resetDay int
		name, publicHost, publicIP       string
		detectedIP, tlsMode              string
		tokenHash, tokenCipher           string
		resetPeriod, note, version, seen string
		enabled                          bool
	}
	var list []loadedNode
	for rows.Next() {
		var item loadedNode
		var enabled int
		if err := rows.Scan(&item.id, &item.name, &item.publicPort, &item.listenPort, &item.publicHost, &item.publicIP, &item.detectedIP, &item.tlsMode, &item.tokenHash, &item.tokenCipher, &enabled, &item.quota, &item.used, &item.resetDay, &item.resetPeriod, &item.note, &item.version, &item.seen); err != nil {
			rows.Close()
			return err
		}
		item.enabled = enabled != 0
		list = append(list, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	routeRows, err := rt.db.db.Query(`SELECT isp_group, node_id, sort_order, enabled FROM isp_route_rules ORDER BY isp_group, sort_order, node_id`)
	if err != nil {
		return err
	}
	routes := make(map[string][]ispRouteRule, len(allISPGroups))
	for routeRows.Next() {
		var group string
		var rule ispRouteRule
		var enabled int
		if err := routeRows.Scan(&group, &rule.NodeID, &rule.SortOrder, &enabled); err != nil {
			routeRows.Close()
			return err
		}
		rule.Enabled = enabled != 0
		routes[group] = append(routes[group], rule)
	}
	if err := routeRows.Err(); err != nil {
		routeRows.Close()
		return err
	}
	routeRows.Close()

	rt.mu.Lock()
	defer rt.mu.Unlock()
	seen := make(map[int64]bool, len(list))
	for _, item := range list {
		seen[item.id] = true
		state := rt.nodes[item.id]
		if state == nil {
			state = &edgeNodeLiveState{id: item.id}
			rt.nodes[item.id] = state
		}
		state.name = item.name
		state.publicPort = item.publicPort
		state.listenPort = item.listenPort
		state.publicHost = item.publicHost
		state.publicIP = item.publicIP
		if item.detectedIP != "" {
			state.detectedIP = item.detectedIP
		}
		state.tlsMode = item.tlsMode
		state.tokenHash = item.tokenHash
		if item.tokenCipher != "" {
			if plain, err := decryptSiteAccountSecret(item.tokenCipher); err == nil {
				state.tokenPlain = plain
			}
		}
		state.enabled = item.enabled
		state.quota = item.quota
		state.persistedUsed = item.used
		state.resetDay = item.resetDay
		state.resetPeriod = item.resetPeriod
		state.note = item.note
		if state.agentVersion == "" {
			state.agentVersion = item.version
		}
		if state.lastSeen.IsZero() && item.seen != "" {
			if parsed, err := time.Parse(time.RFC3339, item.seen); err == nil {
				state.lastSeen = parsed
			}
		}
	}
	for id := range rt.nodes {
		if !seen[id] {
			delete(rt.nodes, id)
		}
	}
	// Guarantee every ISP group contains master (0) and every existing node.
	for _, group := range allISPGroups {
		existing := routes[group]
		present := make(map[int64]bool, len(existing))
		filtered := make([]ispRouteRule, 0, len(existing)+1)
		for _, r := range existing {
			if r.NodeID == 0 || seen[r.NodeID] {
				present[r.NodeID] = true
				filtered = append(filtered, r)
			}
		}
		if !present[0] {
			filtered = append([]ispRouteRule{{NodeID: 0, SortOrder: 1, Enabled: true}}, filtered...)
		}
		for _, item := range list {
			if !present[item.id] {
				filtered = append(filtered, ispRouteRule{NodeID: item.id, SortOrder: len(filtered) + 1, Enabled: true})
			}
		}
		routes[group] = filtered
	}
	rt.routes = routes
	return nil
}

type edgeRouteDecision struct {
	ISPGroup  string
	ISPLabel  string
	Region    string
	ISP       string
	NodeID    int64 // 0 = master
	NodeName  string
	PublicURL string
	Token     string
}

// selectNodeForIP picks the highest-priority online node for the client IP's ISP.
// Pure in-memory; never touches SQLite on the hot path.
func (rt *edgeNodeRuntime) selectNodeForIP(clientIP string) edgeRouteDecision {
	decision := edgeRouteDecision{
		ISPGroup: ispGroupOther,
		ISPLabel: ispGroupLabel(ispGroupOther),
		NodeID:   0,
		NodeName: "主机",
	}
	if rt == nil {
		return decision
	}
	if rt.ip2region != nil {
		res := rt.ip2region.Lookup(clientIP)
		decision.ISPGroup = res.ISPGroup
		decision.ISPLabel = res.ISPLabel
		decision.Region = res.Region
		decision.ISP = res.ISP
	}
	now := time.Now()
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	rules := rt.routes[decision.ISPGroup]
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		if rule.NodeID == 0 {
			decision.NodeID = 0
			decision.NodeName = "主机"
			return decision
		}
		node := rt.nodes[rule.NodeID]
		if node != nil && node.routableAt(now) {
			decision.NodeID = node.id
			decision.NodeName = node.name
			decision.PublicURL = node.publicURL()
			decision.Token = node.tokenPlain
			return decision
		}
	}
	// Always fall back to Master if every configured edge node is unavailable.
	return decision
}

func (rt *edgeNodeRuntime) flush() {
	if rt == nil || rt.db == nil {
		return
	}
	rt.flushMu.Lock()
	defer rt.flushMu.Unlock()

	rt.mu.RLock()
	type snapshot struct {
		state      *edgeNodeLiveState
		detectedIP string
		version    string
		lastSeen   time.Time
	}
	items := make([]snapshot, 0, len(rt.nodes))
	for _, node := range rt.nodes {
		items = append(items, snapshot{
			state:      node,
			detectedIP: node.detectedIP,
			version:    node.agentVersion,
			lastSeen:   node.lastSeen,
		})
	}
	rt.mu.RUnlock()

	for _, item := range items {
		delta := item.state.pending.Swap(0)
		if delta > 0 {
			if _, err := rt.db.db.Exec(`UPDATE edge_nodes SET traffic_used=traffic_used+? WHERE id=?`, delta, item.state.id); err != nil {
				item.state.pending.Add(delta)
				log.Printf("edge node %d: flush traffic: %v", item.state.id, err)
			} else {
				rt.mu.Lock()
				item.state.persistedUsed += delta
				rt.mu.Unlock()
			}
		}
		if !item.lastSeen.IsZero() {
			_, _ = rt.db.db.Exec(`UPDATE edge_nodes SET detected_ip=?, agent_version=?, last_seen_at=? WHERE id=?`,
				item.detectedIP, item.version, item.lastSeen.UTC().Format(time.RFC3339), item.state.id)
		}
	}
	rt.applyMonthlyResets(time.Now().In(rt.db.shareLocation()))
}

func (rt *edgeNodeRuntime) applyMonthlyResets(now time.Time) {
	rows, err := rt.db.db.Query(`SELECT id, traffic_reset_day, traffic_reset_period FROM edge_nodes WHERE traffic_reset_day > 0`)
	if err != nil {
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
		if _, err := rt.db.db.Exec(`UPDATE edge_nodes SET traffic_used=0, traffic_reset_period=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, item.period, item.id); err != nil {
			continue
		}
		rt.mu.Lock()
		if state := rt.nodes[item.id]; state != nil {
			state.pending.Store(0)
			state.persistedUsed = 0
			state.resetPeriod = item.period
		}
		rt.mu.Unlock()
		log.Printf("edge node %d: monthly traffic reset (%s)", item.id, item.period)
	}
}

// ---------------------------------------------------------------------------
// Admin API: /api/edge-nodes, /api/isp-routes, /api/ip2region

func (a *App) edgeNodeInstallCommand(nodeID int64, listenPort int, token, origin string) string {
	if origin == "" {
		origin = "https://你的面板域名"
	}
	if listenPort <= 0 {
		listenPort = edgeNodeDefaultPort
	}
	return fmt.Sprintf("bash <(curl -fsSL %s) agent --master %s --id %d --token %s --port %d",
		edgeInstallScriptRawBase, origin, nodeID, token, listenPort)
}

func (a *App) buildEdgeNodeView(id int64, origin string, includeCommand bool) (EdgeNode, bool) {
	if a.pm == nil || a.pm.edges == nil {
		return EdgeNode{}, false
	}
	now := time.Now()
	a.pm.edges.mu.RLock()
	state := a.pm.edges.nodes[id]
	if state == nil {
		a.pm.edges.mu.RUnlock()
		return EdgeNode{}, false
	}
	view := EdgeNode{
		ID:            state.id,
		Name:          state.name,
		PublicPort:    state.publicPort,
		ListenPort:    state.listenPort,
		PublicHost:    state.publicHost,
		PublicIP:      state.publicIP,
		DetectedIP:    state.detectedIP,
		TLSMode:       state.tlsMode,
		Enabled:       state.enabled,
		TrafficQuota:  state.quota,
		TrafficUsed:   state.totalUsed(),
		ResetDay:      state.resetDay,
		Note:          state.note,
		AgentVersion:  state.agentVersion,
		Online:        state.isOnline(now),
		TLSActive:     state.tlsActive,
		ActiveStreams: state.activeStreams,
		CurrentBps:    state.currentBps,
		PublicURL:     state.publicURL(),
		Status:        state.statusAt(now),
	}
	if !state.lastSeen.IsZero() {
		view.LastSeenAt = state.lastSeen.UTC().Format(time.RFC3339)
	}
	token := state.tokenPlain
	listenPort := state.effectiveListenPort()
	a.pm.edges.mu.RUnlock()

	if view.ResetDay > 0 {
		if _, next := shareResetPeriod(now.In(a.db.shareLocation()), view.ResetDay); !next.IsZero() {
			view.NextResetAt = next.Format("2006-01-02")
		}
	}
	if includeCommand && token != "" {
		view.InstallCommand = a.edgeNodeInstallCommand(view.ID, listenPort, token, origin)
	}
	return view, true
}

func normalizeEdgeNodeInput(input EdgeNodeInput) (EdgeNodeInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.PublicHost = strings.TrimSpace(input.PublicHost)
	input.PublicIP = strings.TrimSpace(input.PublicIP)
	input.TLSMode = strings.ToLower(strings.TrimSpace(input.TLSMode))
	input.Note = strings.TrimSpace(input.Note)

	if input.Name == "" || len(input.Name) > 80 {
		return input, errors.New("从机名称需要 1–80 个字符")
	}
	if input.PublicPort < 1 || input.PublicPort > 65535 {
		return input, errors.New("端口应为 1–65535")
	}
	if input.ListenPort < 0 || input.ListenPort > 65535 {
		return input, errors.New("本机监听端口应为 0–65535（0 表示与对外端口一致）")
	}
	if input.PublicHost != "" {
		host, err := normalizePublicHost(input.PublicHost)
		if err != nil {
			return input, errors.New("访问域名格式无效，请填写纯域名（不含 http:// 和端口）")
		}
		input.PublicHost = host
	}
	if input.PublicIP != "" {
		addr, err := netip.ParseAddr(input.PublicIP)
		if err != nil {
			return input, errors.New("公网 IP 格式无效")
		}
		input.PublicIP = addr.Unmap().String()
	}
	if input.TLSMode == "" {
		if input.PublicHost != "" {
			input.TLSMode = edgeNodeTLSModeAuto
		} else {
			input.TLSMode = edgeNodeTLSModeHTTP
		}
	}
	switch input.TLSMode {
	case edgeNodeTLSModeAuto, edgeNodeTLSModeExternal, edgeNodeTLSModeHTTP:
	default:
		return input, errors.New("不支持的 TLS 接入模式")
	}
	if input.TrafficQuota < 0 {
		return input, errors.New("月流量上限不能为负数")
	}
	if input.ResetDay < 0 || input.ResetDay > 28 {
		return input, errors.New("流量重置日应为 1–28，0 表示不自动重置")
	}
	if len(input.Note) > 500 {
		return input, errors.New("备注过长")
	}
	return input, nil
}

// GET/POST /api/edge-nodes
func (a *App) handleEdgeNodes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	origin := a.shareTextOrigin(r)
	switch r.Method {
	case http.MethodGet:
		if a.pm == nil || a.pm.edges == nil {
			a.jsonOK(w, []EdgeNode{})
			return
		}
		rows, err := a.db.db.Query(`SELECT id FROM edge_nodes ORDER BY id`)
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "从机列表读取失败")
			return
		}
		defer rows.Close()
		result := make([]EdgeNode, 0)
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				continue
			}
			if view, ok := a.buildEdgeNodeView(id, origin, true); ok {
				result = append(result, view)
			}
		}
		a.jsonOK(w, result)
	case http.MethodPost:
		var input EdgeNodeInput
		if err := decodeJSONBody(w, r, &input); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "invalid request")
			return
		}
		input, err := normalizeEdgeNodeInput(input)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		token, err := generateEdgeNodeToken()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "生成节点密钥失败")
			return
		}
		tokenCipher, err := encryptSiteAccountSecret(token)
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "加密节点密钥失败")
			return
		}
		enabled := true
		if input.Enabled != nil {
			enabled = *input.Enabled
		}
		resetPeriod, _ := shareResetPeriod(time.Now().In(a.db.shareLocation()), input.ResetDay)
		tx, err := a.db.db.Begin()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "创建从机失败")
			return
		}
		defer tx.Rollback()
		res, err := tx.Exec(`INSERT INTO edge_nodes(name, public_port, listen_port, public_host, public_ip, tls_mode, token_hash, token_ciphertext, enabled, traffic_quota, traffic_reset_day, traffic_reset_period, note) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			input.Name, input.PublicPort, input.ListenPort, input.PublicHost, input.PublicIP, input.TLSMode, edgeNodeTokenHash(token), tokenCipher, sqliteBool(enabled), input.TrafficQuota, input.ResetDay, resetPeriod, input.Note)
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "创建从机失败")
			return
		}
		id, err := res.LastInsertId()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "创建从机失败")
			return
		}
		for _, group := range allISPGroups {
			var maxOrder int
			_ = tx.QueryRow(`SELECT COALESCE(MAX(sort_order), 0) FROM isp_route_rules WHERE isp_group=?`, group).Scan(&maxOrder)
			if _, err := tx.Exec(`INSERT OR IGNORE INTO isp_route_rules(isp_group, node_id, sort_order, enabled) VALUES(?,?,?,1)`, group, id, maxOrder+1); err != nil {
				a.jsonErr(w, http.StatusInternalServerError, "初始化线路规则失败")
				return
			}
		}
		if err := tx.Commit(); err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "创建从机失败")
			return
		}
		if a.pm != nil && a.pm.edges != nil {
			_ = a.pm.edges.reload()
		}
		view, _ := a.buildEdgeNodeView(id, origin, true)
		a.jsonResponse(w, http.StatusCreated, view)
	default:
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// /api/edge-nodes/{id}[/action]
func (a *App) handleEdgeNodeByID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/edge-nodes/"), "/"), "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		a.jsonErr(w, http.StatusBadRequest, "invalid edge node id")
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	origin := a.shareTextOrigin(r)
	if _, exists := a.buildEdgeNodeView(id, origin, false); !exists {
		a.jsonErr(w, http.StatusNotFound, "从机不存在")
		return
	}

	switch action {
	case "":
		switch r.Method {
		case http.MethodGet:
			view, _ := a.buildEdgeNodeView(id, origin, true)
			a.jsonOK(w, view)
		case http.MethodPut:
			var input EdgeNodeInput
			if err := decodeJSONBody(w, r, &input); err != nil {
				a.jsonErr(w, http.StatusBadRequest, "invalid request")
				return
			}
			input, err := normalizeEdgeNodeInput(input)
			if err != nil {
				a.jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
			enabled := true
			if input.Enabled != nil {
				enabled = *input.Enabled
			} else {
				var cur int
				if err := a.db.db.QueryRow(`SELECT enabled FROM edge_nodes WHERE id=?`, id).Scan(&cur); err == nil {
					enabled = cur != 0
				}
			}
			resetPeriod, _ := shareResetPeriod(time.Now().In(a.db.shareLocation()), input.ResetDay)
			if _, err := a.db.db.Exec(`UPDATE edge_nodes SET name=?, public_port=?, listen_port=?, public_host=?, public_ip=?, tls_mode=?, enabled=?, traffic_quota=?, traffic_reset_period=CASE WHEN traffic_reset_day<>? THEN ? ELSE traffic_reset_period END, traffic_reset_day=?, note=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
				input.Name, input.PublicPort, input.ListenPort, input.PublicHost, input.PublicIP, input.TLSMode, sqliteBool(enabled), input.TrafficQuota, input.ResetDay, resetPeriod, input.ResetDay, input.Note, id); err != nil {
				a.jsonErr(w, http.StatusInternalServerError, "保存失败")
				return
			}
			if a.pm != nil && a.pm.edges != nil {
				_ = a.pm.edges.reload()
			}
			view, _ := a.buildEdgeNodeView(id, origin, true)
			a.jsonOK(w, view)
		case http.MethodDelete:
			tx, err := a.db.db.Begin()
			if err != nil {
				a.jsonErr(w, http.StatusInternalServerError, "删除失败")
				return
			}
			defer tx.Rollback()
			if _, err := tx.Exec(`DELETE FROM isp_route_rules WHERE node_id=?`, id); err != nil {
				a.jsonErr(w, http.StatusInternalServerError, "删除失败")
				return
			}
			if _, err := tx.Exec(`DELETE FROM edge_nodes WHERE id=?`, id); err != nil {
				a.jsonErr(w, http.StatusInternalServerError, "删除失败")
				return
			}
			if err := tx.Commit(); err != nil {
				a.jsonErr(w, http.StatusInternalServerError, "删除失败")
				return
			}
			if a.pm != nil && a.pm.edges != nil {
				_ = a.pm.edges.reload()
			}
			a.jsonOK(w, map[string]bool{"deleted": true})
		default:
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "reset-token":
		if r.Method != http.MethodPost {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		token, err := generateEdgeNodeToken()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "重置密钥失败")
			return
		}
		cipherText, err := encryptSiteAccountSecret(token)
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "重置密钥失败")
			return
		}
		if _, err := a.db.db.Exec(`UPDATE edge_nodes SET token_hash=?, token_ciphertext=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, edgeNodeTokenHash(token), cipherText, id); err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "重置密钥失败")
			return
		}
		if a.pm != nil && a.pm.edges != nil {
			_ = a.pm.edges.reload()
		}
		view, _ := a.buildEdgeNodeView(id, origin, true)
		a.jsonOK(w, view)
	case "reset-traffic":
		if r.Method != http.MethodPost {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if a.pm != nil && a.pm.edges != nil {
			a.pm.edges.flushMu.Lock()
			_, err = a.db.db.Exec(`UPDATE edge_nodes SET traffic_used=0, updated_at=CURRENT_TIMESTAMP WHERE id=?`, id)
			if err == nil {
				a.pm.edges.mu.Lock()
				if state := a.pm.edges.nodes[id]; state != nil {
					state.pending.Store(0)
					state.persistedUsed = 0
				}
				a.pm.edges.mu.Unlock()
			}
			a.pm.edges.flushMu.Unlock()
			if err != nil {
				a.jsonErr(w, http.StatusInternalServerError, "流量清零失败")
				return
			}
		}
		view, _ := a.buildEdgeNodeView(id, origin, true)
		a.jsonOK(w, view)
	default:
		a.jsonErr(w, http.StatusNotFound, "not found")
	}
}

type ispRouteNodeView struct {
	NodeID    int64  `json:"node_id"`
	Name      string `json:"name"`
	IsMaster  bool   `json:"is_master"`
	Enabled   bool   `json:"enabled"`
	Online    bool   `json:"online"`
	Status    string `json:"status"`
	Active    bool   `json:"active"`
	PublicURL string `json:"public_url,omitempty"`
}

type ispRouteGroupView struct {
	Group string             `json:"group"`
	Label string             `json:"label"`
	Nodes []ispRouteNodeView `json:"nodes"`
}

func (a *App) buildISPRouteGroups() []ispRouteGroupView {
	now := time.Now()
	result := make([]ispRouteGroupView, 0, len(allISPGroups))
	if a.pm == nil || a.pm.edges == nil {
		return result
	}
	a.pm.edges.mu.RLock()
	defer a.pm.edges.mu.RUnlock()
	for _, group := range allISPGroups {
		rules := a.pm.edges.routes[group]
		nodes := make([]ispRouteNodeView, 0, len(rules))
		activeAssigned := false
		for _, rule := range rules {
			if rule.NodeID == 0 {
				isActive := false
				if rule.Enabled && !activeAssigned {
					isActive = true
					activeAssigned = true
				}
				nodes = append(nodes, ispRouteNodeView{
					NodeID:   0,
					Name:     "主机",
					IsMaster: true,
					Enabled:  rule.Enabled,
					Online:   true,
					Status:   "online",
					Active:   isActive,
				})
				continue
			}
			node := a.pm.edges.nodes[rule.NodeID]
			if node == nil {
				continue
			}
			routable := node.routableAt(now)
			isActive := false
			if rule.Enabled && routable && !activeAssigned {
				isActive = true
				activeAssigned = true
			}
			nodes = append(nodes, ispRouteNodeView{
				NodeID:    node.id,
				Name:      node.name,
				Enabled:   rule.Enabled,
				Online:    node.isOnline(now),
				Status:    node.statusAt(now),
				Active:    isActive,
				PublicURL: node.publicURL(),
			})
		}
		if !activeAssigned {
			// Fallback indicator on Master if every enabled edge node is offline.
			for i := range nodes {
				if nodes[i].NodeID == 0 {
					nodes[i].Active = true
					break
				}
			}
		}
		result = append(result, ispRouteGroupView{
			Group: group,
			Label: ispGroupLabel(group),
			Nodes: nodes,
		})
	}
	return result
}

// GET/PUT /api/isp-routes
func (a *App) handleISPRoutes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		a.jsonOK(w, map[string]any{"groups": a.buildISPRouteGroups()})
	case http.MethodPut:
		var req struct {
			Group string `json:"group"`
			Nodes []struct {
				NodeID  int64 `json:"node_id"`
				Enabled bool  `json:"enabled"`
			} `json:"nodes"`
		}
		if err := decodeJSONBody(w, r, &req); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "invalid request")
			return
		}
		req.Group = strings.ToLower(strings.TrimSpace(req.Group))
		if !validISPGroup(req.Group) || len(req.Nodes) == 0 {
			a.jsonErr(w, http.StatusBadRequest, "线路分组参数无效")
			return
		}
		tx, err := a.db.db.Begin()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "保存线路排序失败")
			return
		}
		defer tx.Rollback()
		seen := make(map[int64]bool, len(req.Nodes))
		for idx, item := range req.Nodes {
			if item.NodeID < 0 || seen[item.NodeID] {
				a.jsonErr(w, http.StatusBadRequest, "节点列表无效")
				return
			}
			if item.NodeID > 0 {
				var count int
				if err := tx.QueryRow(`SELECT COUNT(*) FROM edge_nodes WHERE id=?`, item.NodeID).Scan(&count); err != nil || count == 0 {
					a.jsonErr(w, http.StatusBadRequest, "包含不存在的从机")
					return
				}
			}
			seen[item.NodeID] = true
			if _, err := tx.Exec(`INSERT INTO isp_route_rules(isp_group, node_id, sort_order, enabled) VALUES(?,?,?,?) ON CONFLICT(isp_group, node_id) DO UPDATE SET sort_order=excluded.sort_order, enabled=excluded.enabled`,
				req.Group, item.NodeID, idx+1, sqliteBool(item.Enabled)); err != nil {
				a.jsonErr(w, http.StatusInternalServerError, "保存线路排序失败")
				return
			}
		}
		if err := tx.Commit(); err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "保存线路排序失败")
			return
		}
		if a.pm != nil && a.pm.edges != nil {
			_ = a.pm.edges.reload()
		}
		a.jsonOK(w, map[string]any{"groups": a.buildISPRouteGroups()})
	default:
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// GET /api/ip2region, DELETE /api/ip2region?version=v4|v6, POST /api/ip2region/update, POST /api/ip2region/lookup
func (a *App) handleIP2Region(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.ip2region == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "IP 地区库未初始化")
		return
	}
	action := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/ip2region"), "/")
	switch action {
	case "":
		switch r.Method {
		case http.MethodGet:
			a.jsonOK(w, a.ip2region.Status())
		case http.MethodDelete:
			version := r.URL.Query().Get("version")
			if err := a.ip2region.Delete(version); err != nil {
				a.jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
			a.jsonOK(w, a.ip2region.Status())
		default:
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "update":
		if r.Method != http.MethodPost {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			Version string `json:"version"`
		}
		if err := decodeJSONBody(w, r, &req); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "invalid request")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		item, err := a.ip2region.Update(ctx, req.Version)
		if err != nil {
			a.jsonErr(w, http.StatusBadGateway, err.Error())
			return
		}
		a.jsonOK(w, map[string]any{"item": item, "status": a.ip2region.Status()})
	case "lookup":
		if r.Method != http.MethodPost {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			IP string `json:"ip"`
		}
		if err := decodeJSONBody(w, r, &req); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "invalid request")
			return
		}
		ip := strings.TrimSpace(req.IP)
		if _, err := netip.ParseAddr(ip); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "请输入有效的 IPv4 或 IPv6 地址")
			return
		}
		var decision edgeRouteDecision
		if a.pm != nil && a.pm.edges != nil {
			decision = a.pm.edges.selectNodeForIP(ip)
		} else {
			res := a.ip2region.Lookup(ip)
			decision = edgeRouteDecision{
				ISPGroup: res.ISPGroup,
				ISPLabel: res.ISPLabel,
				Region:   res.Region,
				ISP:      res.ISP,
				NodeID:   0,
				NodeName: "主机",
			}
		}
		a.jsonOK(w, map[string]any{
			"ip":         ip,
			"region":     decision.Region,
			"isp":        decision.ISP,
			"isp_group":  decision.ISPGroup,
			"isp_label":  decision.ISPLabel,
			"node_id":    decision.NodeID,
			"node_name":  decision.NodeName,
			"public_url": decision.PublicURL,
		})
	default:
		a.jsonErr(w, http.StatusNotFound, "not found")
	}
}

// ---------------------------------------------------------------------------
// Agent Heartbeat Protocol (POST /api/agent/heartbeat)

type agentTrafficReport struct {
	SiteID      int64  `json:"site_id"`
	ShareUserID int64  `json:"share_user_id"`
	TokenHash   string `json:"token_hash"`
	BytesIn     int64  `json:"bytes_in"`
	BytesOut    int64  `json:"bytes_out"`
	Requests    int64  `json:"requests"`
	Playing     bool   `json:"playing"`
}

type agentHeartbeatRequest struct {
	Version         string               `json:"version"`
	ListenPort      int                  `json:"listen_port"`
	TLSActive       bool                 `json:"tls_active"`
	ActiveStreams   int                  `json:"active_streams"`
	CurrentBps      int64                `json:"current_bps"`
	CertFingerprint string               `json:"cert_fingerprint"`
	TrafficReports  []agentTrafficReport `json:"traffic_reports"`
}

type agentHeartbeatResponse struct {
	OK              bool          `json:"ok"`
	NodeEnabled     bool          `json:"node_enabled"`
	BlockedUsers    []int64       `json:"blocked_users"`
	RevokedHashes   []string      `json:"revoked_hashes"`
	UserSpeedLimits map[int64]int `json:"user_speed_limits"`
	TLSCertPEM      string        `json:"tls_cert_pem,omitempty"`
	TLSKeyPEM       string        `json:"tls_key_pem,omitempty"`
	CertFingerprint string        `json:"cert_fingerprint,omitempty"`
}

func (a *App) masterTLSPEMForHost(hostname string) (certPEM, keyPEM, fingerprint string, ok bool) {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" || a == nil {
		return "", "", "", false
	}
	certFile, keyFile := panelTLSPaths(a.dbPath)
	if a.db != nil {
		if settings, err := a.db.PanelSettings(); err == nil && settings.CertificatePath != "" && settings.PrivateKeyPath != "" {
			certFile = settings.CertificatePath
			keyFile = settings.PrivateKeyPath
		}
	}
	if certFile == "" || keyFile == "" {
		return "", "", "", false
	}
	// #nosec G304 -- certFile and keyFile are administrator-managed TLS paths.
	certBytes, err := os.ReadFile(certFile)
	if err != nil {
		return "", "", "", false
	}
	// #nosec G304 -- certFile and keyFile are administrator-managed TLS paths.
	keyBytes, err := os.ReadFile(keyFile)
	if err != nil {
		return "", "", "", false
	}
	pair, err := tls.X509KeyPair(certBytes, keyBytes)
	if err != nil || len(pair.Certificate) == 0 {
		return "", "", "", false
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return "", "", "", false
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return "", "", "", false
	}
	if err := leaf.VerifyHostname(hostname); err != nil {
		return "", "", "", false
	}
	sum := sha256.Sum256(pair.Certificate[0])
	return string(certBytes), string(keyBytes), hex.EncodeToString(sum[:]), true
}

func (a *App) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.pm == nil || a.pm.edges == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "edge runtime unavailable")
		return
	}
	nodeID, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get("X-Yundu-Node-ID")), 10, 64)
	token := strings.TrimSpace(r.Header.Get("X-Yundu-Node-Token"))
	if err != nil || nodeID <= 0 || token == "" {
		a.jsonErr(w, http.StatusUnauthorized, "missing agent credentials")
		return
	}
	gotHash := edgeNodeTokenHash(token)

	a.pm.edges.mu.RLock()
	node := a.pm.edges.nodes[nodeID]
	var expectedHash, publicHost, tlsMode string
	var nodeEnabled bool
	if node != nil {
		expectedHash = node.tokenHash
		publicHost = node.publicHost
		tlsMode = node.tlsMode
		nodeEnabled = node.enabled
	}
	a.pm.edges.mu.RUnlock()

	if node == nil || !hmac.Equal([]byte(gotHash), []byte(expectedHash)) {
		a.jsonErr(w, http.StatusUnauthorized, "invalid agent credentials")
		return
	}

	var req agentHeartbeatRequest
	if err := decodeJSONBodyLimit(w, r, &req, edgeHeartbeatBodyLimit); err != nil {
		a.jsonErr(w, http.StatusBadRequest, "invalid heartbeat payload")
		return
	}

	now := time.Now()
	detectedIP := strings.TrimSpace(requestClientIP(r, a.trustedProxies))
	if parsed, parseErr := netip.ParseAddr(detectedIP); parseErr == nil {
		parsed = parsed.Unmap()
		if !parsed.IsLoopback() {
			detectedIP = parsed.String()
		} else if node.detectedIP != "" {
			detectedIP = node.detectedIP
		}
	} else {
		detectedIP = ""
	}

	billingMode := trafficBillingModeBidirectional
	if a.db != nil {
		billingMode = a.db.currentSystemSettings().TrafficBillingMode
	}
	var nodeBytesDelta int64
	for _, report := range req.TrafficReports {
		if report.BytesIn < 0 || report.BytesOut < 0 {
			continue
		}
		billed := trafficBillableBytes(billingMode, report.BytesIn, report.BytesOut)
		nodeBytesDelta += billed
		// Credit site traffic meters if the site is loaded.
		if report.SiteID > 0 {
			a.pm.recordExternalSiteTraffic(report.SiteID, report.BytesIn, report.BytesOut, report.Requests, now)
		}
		// Credit share user traffic and refresh draining playback timestamp.
		if a.pm.shares != nil && report.ShareUserID > 0 {
			a.pm.shares.recordExternalTraffic(report.ShareUserID, report.TokenHash, billed, report.Playing, now)
		}
	}
	if nodeBytesDelta > 0 {
		node.pending.Add(nodeBytesDelta)
	}

	a.pm.edges.mu.Lock()
	node.lastSeen = now
	if detectedIP != "" {
		node.detectedIP = detectedIP
	}
	if req.Version != "" && len(req.Version) <= 64 {
		node.agentVersion = req.Version
	}
	node.tlsActive = req.TLSActive
	node.activeStreams = max(0, req.ActiveStreams)
	node.currentBps = max(int64(0), req.CurrentBps)
	a.pm.edges.mu.Unlock()

	resp := agentHeartbeatResponse{
		OK:              true,
		NodeEnabled:     nodeEnabled,
		BlockedUsers:    []int64{},
		RevokedHashes:   []string{},
		UserSpeedLimits: map[int64]int{},
	}
	if a.pm.shares != nil {
		resp.BlockedUsers, resp.RevokedHashes, resp.UserSpeedLimits = a.pm.shares.agentSyncSnapshot(now)
	}
	if publicHost != "" && tlsMode == edgeNodeTLSModeAuto {
		if certPEM, keyPEM, fp, ok := a.masterTLSPEMForHost(publicHost); ok {
			resp.CertFingerprint = fp
			if fp != req.CertFingerprint {
				resp.TLSCertPEM = certPEM
				resp.TLSKeyPEM = keyPEM
			}
		}
	}
	a.jsonOK(w, resp)
}

// recordExternalSiteTraffic adds agent-reported traffic into a site's live meters.
func (pm *ProxyManager) recordExternalSiteTraffic(siteID, bytesIn, bytesOut, requests int64, now time.Time) {
	if pm == nil || siteID <= 0 || (bytesIn == 0 && bytesOut == 0 && requests == 0) {
		return
	}
	pm.mu.RLock()
	inst := pm.proxies[siteID]
	pm.mu.RUnlock()
	if inst == nil {
		return
	}
	if bytesIn > 0 {
		inst.bytesIn.Add(bytesIn)
		inst.cumulativeBytesIn.Add(bytesIn)
	}
	if bytesOut > 0 {
		inst.bytesOut.Add(bytesOut)
		inst.cumulativeBytesOut.Add(bytesOut)
	}
	if requests > 0 {
		inst.reqCount.Add(requests)
		inst.pendingRequests.Add(requests)
	}
	minuteStart := now.UTC().Truncate(time.Minute).Unix()
	inst.trafficMu.Lock()
	if inst.pendingMinuteTraffic == nil {
		inst.pendingMinuteTraffic = make(map[int64]trafficMinuteDelta)
	}
	delta := inst.pendingMinuteTraffic[minuteStart]
	delta.BytesIn += bytesIn
	delta.BytesOut += bytesOut
	delta.Requests += requests
	inst.pendingMinuteTraffic[minuteStart] = delta
	inst.trafficMu.Unlock()
}

// recordExternalTraffic credits bytes reported by an edge node to a share user
// and keeps active-playback tracking fresh for graceful site switching.
func (rt *shareUserRuntime) recordExternalTraffic(userID int64, tokenHash string, billed int64, playing bool, now time.Time) {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	if state := rt.users[userID]; state != nil && billed > 0 {
		state.pending.Add(billed)
	}
	if tokenHash != "" && playing {
		act := rt.activity[tokenHash]
		if act == nil {
			act = &shareSessionActivity{}
			rt.activity[tokenHash] = act
		}
		act.lastPlaybackAt = now
	}
	rt.mu.Unlock()
}

// agentSyncSnapshot returns blocked share users, revoked token hashes, and per-user speed limits.
func (rt *shareUserRuntime) agentSyncSnapshot(now time.Time) ([]int64, []string, map[int64]int) {
	blocked := make([]int64, 0)
	revoked := make([]string, 0)
	speeds := make(map[int64]int)
	if rt == nil {
		return blocked, revoked, speeds
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	for id, state := range rt.users {
		if state.blockReasonLocked(now) != "" {
			blocked = append(blocked, id)
		}
		if state.speedLimit > 0 {
			speeds[id] = state.speedLimit
		}
	}
	for hash := range rt.revoked {
		revoked = append(revoked, hash)
	}
	return blocked, revoked, speeds
}

func requestClientIP(r *http.Request, trustedProxies []*net.IPNet) string {
	return requestClientKey(r, trustedProxies)
}
