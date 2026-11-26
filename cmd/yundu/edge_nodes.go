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
	"net/url"
	"os"
	"slices"
	"sort"
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
	edgeNodeOfflineAfter     = 25 * time.Second
	edgeTicketTTL            = 6 * time.Hour
	edgeTicketPathPrefix     = "/_yundu/p/"
	edgeEgressPath           = "/_yundu/egress"
	edgeEgressTicketHeader   = "X-Yundu-Egress-Ticket"
	edgeEgressMarkerHeader   = "X-Yundu-Egress"
	edgeEgressTicketTTL      = 2 * time.Minute
	edgeMaxTicketBytes       = 16 << 10
	edgeHeartbeatBodyLimit   = 1 << 20
	edgeNodeDefaultPort      = 19090
	edgeInstallScriptRawBase = "https://raw.githubusercontent.com/SonthyR/EMBY/master/install.sh"
	ispSettingNonMainlandKey = "_non_mainland"
	ispStrategyPriority      = "priority"
	ispStrategyBalance       = "balance"
	speedtestMetricMaxAge    = 30 * 24 * time.Hour
)

func normalizeISPStrategy(raw string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case ispStrategyPriority, ispStrategyBalance:
		return s, true
	default:
		return ispStrategyPriority, false
	}
}

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
	auto_circuit_breaker INTEGER NOT NULL DEFAULT 1,
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
CREATE TABLE IF NOT EXISTS isp_group_settings (
	isp_group TEXT PRIMARY KEY,
	block_connect INTEGER NOT NULL DEFAULT 0,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
`

func ensureEdgeNodeSchema(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, edgeNodeSchemaSQL); err != nil {
		return err
	}
	for _, col := range []struct{ table, name, alter string }{
		{"isp_group_settings", "strategy", `ALTER TABLE isp_group_settings ADD COLUMN strategy TEXT NOT NULL DEFAULT 'priority'`},
		{"edge_nodes", "total_bytes_in", `ALTER TABLE edge_nodes ADD COLUMN total_bytes_in INTEGER NOT NULL DEFAULT 0`},
		{"edge_nodes", "total_bytes_out", `ALTER TABLE edge_nodes ADD COLUMN total_bytes_out INTEGER NOT NULL DEFAULT 0`},
		{"edge_nodes", "auto_https", `ALTER TABLE edge_nodes ADD COLUMN auto_https INTEGER NOT NULL DEFAULT 0`},
		{"edge_nodes", "cf_record_id", `ALTER TABLE edge_nodes ADD COLUMN cf_record_id TEXT NOT NULL DEFAULT ''`},
		{"edge_nodes", "cf_record_type", `ALTER TABLE edge_nodes ADD COLUMN cf_record_type TEXT NOT NULL DEFAULT ''`},
		{"edge_nodes", "cf_record_ip", `ALTER TABLE edge_nodes ADD COLUMN cf_record_ip TEXT NOT NULL DEFAULT ''`},
		{"edge_nodes", "cf_dns_status", `ALTER TABLE edge_nodes ADD COLUMN cf_dns_status TEXT NOT NULL DEFAULT ''`},
		{"edge_nodes", "sort_order", `ALTER TABLE edge_nodes ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0`},
		{"edge_nodes", "max_balance_streams", `ALTER TABLE edge_nodes ADD COLUMN max_balance_streams INTEGER NOT NULL DEFAULT 2`},
		{"edge_nodes", "auto_circuit_breaker", `ALTER TABLE edge_nodes ADD COLUMN auto_circuit_breaker INTEGER NOT NULL DEFAULT 1`},
		{"isp_route_rules", "schedule_mode", `ALTER TABLE isp_route_rules ADD COLUMN schedule_mode TEXT NOT NULL DEFAULT 'always'`},
		{"isp_route_rules", "schedule_start", `ALTER TABLE isp_route_rules ADD COLUMN schedule_start TEXT NOT NULL DEFAULT ''`},
		{"isp_route_rules", "schedule_end", `ALTER TABLE isp_route_rules ADD COLUMN schedule_end TEXT NOT NULL DEFAULT ''`},
	} {
		var exists int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('"+col.table+"') WHERE name=?", col.name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := conn.ExecContext(ctx, col.alter); err != nil {
				return err
			}
			if col.table == "edge_nodes" && col.name == "total_bytes_out" {
				if _, err := conn.ExecContext(ctx, `UPDATE edge_nodes SET total_bytes_out=traffic_used WHERE traffic_used > 0 AND total_bytes_in = 0 AND total_bytes_out = 0`); err != nil {
					return err
				}
			}
			if col.table == "edge_nodes" && col.name == "sort_order" {
				if _, err := conn.ExecContext(ctx, `UPDATE edge_nodes SET sort_order = id`); err != nil {
					return err
				}
			}
		}
	}
	for _, group := range allISPGroups {
		if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO isp_route_rules(isp_group, node_id, sort_order, enabled) VALUES(?, 0, 1, 1)`, group); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO isp_group_settings(isp_group, block_connect, strategy) VALUES(?, 0, 'priority')`, group); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO isp_group_settings(isp_group, block_connect, strategy) VALUES(?, 0, 'priority')`, ispSettingNonMainlandKey); err != nil {
		return err
	}
	// Migrate legacy removed 'auto' strategy to 'priority'.
	if _, err := conn.ExecContext(ctx, `UPDATE isp_group_settings SET strategy='priority' WHERE strategy='auto'`); err != nil {
		return err
	}
	if err := ensureMasterNodeSchema(ctx, conn); err != nil {
		return err
	}
	if err := ensureISPProbeSchema(ctx, conn); err != nil {
		return err
	}
	return ensureEdgeAutoHTTPSSchema(ctx, conn)
}

// reencryptEdgeNodeSecrets moves edge node secrets to a new credential key during backup restore.
func reencryptEdgeNodeSecrets(tx *sql.Tx, oldKey, newKey []byte) error {
	if hasTable, err := backupSQLiteColumnExists(tx, "edge_tls_settings", "cf_token_ciphertext"); err == nil && hasTable {
		var cipher string
		if err := tx.QueryRow(`SELECT cf_token_ciphertext FROM edge_tls_settings WHERE id=1`).Scan(&cipher); err == nil && cipher != "" {
			plain, decErr := decryptPanelACMETokenWithSecret(cipher, oldKey)
			if decErr != nil {
				plain, decErr = decryptPanelACMETokenWithSecret(cipher, newKey)
			}
			if decErr != nil {
				_, _ = tx.Exec(`UPDATE edge_tls_settings SET cf_token_ciphertext='' WHERE id=1`)
			} else if reenc, encErr := encryptPanelACMETokenWithSecret(plain, newKey); encErr == nil {
				_, _ = tx.Exec(`UPDATE edge_tls_settings SET cf_token_ciphertext=? WHERE id=1`, reenc)
			}
		}
	}
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

func edgeEgressKeyFromToken(token string) []byte {
	sum := sha256.Sum256(append([]byte("yundu-edge-egress:v1:"), []byte(token)...))
	return sum[:]
}

type edgeEgressTicket struct {
	Version   int    `json:"v"`
	NodeID    int64  `json:"n"`
	TargetURL string `json:"t"`
	Host      string `json:"h,omitempty"`
	ExpiresAt int64  `json:"exp"`
}

func sealEdgeEgressTicket(token string, ticket edgeEgressTicket) (string, error) {
	if token == "" {
		return "", errors.New("edge node token unavailable")
	}
	ticket.Version = 1
	plain, err := json.Marshal(ticket)
	if err != nil {
		return "", err
	}
	if len(plain) > edgeMaxTicketBytes {
		return "", errors.New("egress ticket too large")
	}
	block, err := aes.NewCipher(edgeEgressKeyFromToken(token))
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

func openEdgeEgressTicket(token, encoded string) (edgeEgressTicket, error) {
	var ticket edgeEgressTicket
	if token == "" || encoded == "" || len(encoded) > edgeMaxTicketBytes*2 {
		return ticket, errors.New("invalid egress ticket")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return ticket, errors.New("malformed egress ticket")
	}
	block, err := aes.NewCipher(edgeEgressKeyFromToken(token))
	if err != nil {
		return ticket, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return ticket, err
	}
	if len(raw) < gcm.NonceSize()+gcm.Overhead() {
		return ticket, errors.New("truncated egress ticket")
	}
	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return ticket, errors.New("invalid egress ticket signature")
	}
	if err := json.Unmarshal(plain, &ticket); err != nil {
		return ticket, errors.New("corrupt egress ticket")
	}
	if ticket.Version != 1 || ticket.TargetURL == "" {
		return ticket, errors.New("unsupported egress ticket")
	}
	if ticket.ExpiresAt > 0 && time.Now().Unix() > ticket.ExpiresAt {
		return ticket, errors.New("egress ticket expired")
	}
	return ticket, nil
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
	ID                int64                 `json:"id"`
	IsMaster          bool                  `json:"is_master,omitempty"`
	Name              string                `json:"name"`
	PublicPort        int                   `json:"public_port"`
	ListenPort        int                   `json:"listen_port"`
	PublicHost        string                `json:"public_host"`
	PublicIP          string                `json:"public_ip"`
	DetectedIP        string                `json:"detected_ip"`
	TLSMode           string                `json:"tls_mode"`
	AutoHTTPS         bool                  `json:"auto_https"`
	CFDNSStatus       string                `json:"cf_dns_status,omitempty"`
	CFRecordIP        string                `json:"cf_record_ip,omitempty"`
	Enabled           bool                  `json:"enabled"`
	AutoCircuitBreaker *bool                 `json:"auto_circuit_breaker,omitempty"`
	TrafficQuota      int64                 `json:"traffic_quota"`
	TrafficUsed       int64                 `json:"traffic_used"`
	ResetDay          int                   `json:"traffic_reset_day"`
	NextResetAt       string                `json:"next_reset_at,omitempty"`
	MaxBalanceStreams int                   `json:"max_balance_streams"`
	Note              string                `json:"note"`
	AgentVersion      string                `json:"agent_version"`
	MasterVersion     string                `json:"master_version,omitempty"`
	UpgradeAvailable  bool                  `json:"upgrade_available"`
	UpgradeTarget     string                `json:"upgrade_target,omitempty"`
	UpgradeStatus     string                `json:"upgrade_status,omitempty"`
	LastSeenAt        string                `json:"last_seen_at"`
	Online            bool                  `json:"online"`
	TLSActive         bool                  `json:"tls_active"`
	ActiveStreams     int                   `json:"active_streams"`
	CurrentBps        int64                 `json:"current_bps"`
	MemoryBytes       uint64                `json:"memory_bytes"`
	UptimeSec         int64                 `json:"uptime_sec"`
	UpstreamLatencies []edgeSiteLatency     `json:"upstream_latencies,omitempty"`
	SpeedMetrics      []edgeNodeSpeedMetric `json:"speed_metrics,omitempty"`
	ISPHealth         []edgeISPHealthView   `json:"isp_health,omitempty"`
	AnyCircuitTripped bool                  `json:"any_circuit_tripped,omitempty"`
	PublicURL         string                `json:"public_url"`
	Status            string                `json:"status"` // online, offline, disabled, quota, waiting
	InstallCommand    string                `json:"install_command,omitempty"`
	UninstallCommand  string                `json:"uninstall_command,omitempty"`
	CreatedAt         string                `json:"created_at"`
	UpdatedAt         string                `json:"updated_at"`
}

type EdgeNodeInput struct {
	Name         string `json:"name"`
	PublicPort   int    `json:"public_port"`
	ListenPort   int    `json:"listen_port"`
	PublicHost   string `json:"public_host"`
	PublicIP     string `json:"public_ip"`
	TLSMode      string `json:"tls_mode"`
	AutoHTTPS    bool   `json:"auto_https"`
	Enabled      *bool  `json:"enabled"`
	AutoCircuitBreaker *bool  `json:"auto_circuit_breaker"`
	TrafficQuota      int64  `json:"traffic_quota"`
	ResetDay          int    `json:"traffic_reset_day"`
	MaxBalanceStreams int    `json:"max_balance_streams"`
	Note              string `json:"note"`
}

type edgeNodeLiveState struct {
	id                  int64
	name                string
	publicPort          int
	listenPort          int
	publicHost          string
	publicIP            string
	detectedIP          string
	tlsMode             string
	autoHTTPS           bool
	cfRecordID          string
	cfRecordType        string
	cfRecordIP          string
	cfDNSStatus         string
	tokenHash           string
	tokenPlain          string
	enabled             bool
	monitorISPOnly      bool
	quota               int64
	persistedUsed       int64
	resetDay            int
	resetPeriod         string
	maxBalanceStreams   int
	note                string
	agentVersion        string
	lastSeen            time.Time
	tlsActive           bool
	activeStreams       int
	currentBps          int64
	memoryBytes         uint64
	uptimeSec           int64
	upstreamLatencies   map[int64]int
	upgradeTarget       string
	upgradeRequestID    string
	upgradeStatus       string
	alertSeenOnline     bool
	alertKnownOffline   bool
	alertQuota80Period  string
	alertQuota100Period string
	supportsSpeedtest   bool
	supportsDirectPopup bool
	supportsEgress      bool
	ispHealth           map[string]*ispHealthState
	ispProbeSeq         uint64
	ispForceProbeID     string
	ispProbeInstance    [16]byte
	ispProbeGeneration  uint64
	ispProbeSeenAt      time.Time

	pending           atomic.Int64
	persistedTotalIn  int64
	persistedTotalOut int64
	pendingTotalIn    atomic.Int64
	pendingTotalOut   atomic.Int64
}

func (n *edgeNodeLiveState) totalDirections() (int64, int64) {
	if n == nil {
		return 0, 0
	}
	return n.persistedTotalIn + n.pendingTotalIn.Load(), n.persistedTotalOut + n.pendingTotalOut.Load()
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

func (n *edgeNodeLiveState) isISPTripped(group string) bool {
	if n == nil || n.monitorISPOnly {
		return false
	}
	return isISPTrippedInMap(n.ispHealth, group)
}

func (n *edgeNodeLiveState) routableForGroupAt(group string, now time.Time) bool {
	if !n.routableAt(now) {
		return false
	}
	return !n.isISPTripped(group)
}

func agentVersionSupportsEgress(version string) bool {
	v := strings.TrimSpace(strings.TrimPrefix(strings.ToLower(version), "v"))
	if v == "" {
		return false
	}
	if cut, _, ok := strings.Cut(v, "-"); ok {
		v = cut
	}
	parts := strings.Split(v, ".")
	if len(parts) < 3 {
		return false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	patch, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return false
	}
	if major != 1 {
		return major > 1
	}
	if minor != 4 {
		return minor > 4
	}
	return patch >= 8
}

func (n *edgeNodeLiveState) supportsEgressControl() bool {
	if n == nil {
		return false
	}
	return n.supportsEgress || agentVersionSupportsEgress(n.agentVersion)
}

type ispRouteRule struct {
	NodeID        int64  `json:"node_id"`
	SortOrder     int    `json:"sort_order"`
	Enabled       bool   `json:"enabled"`
	ScheduleMode  string `json:"schedule_mode,omitempty"`
	ScheduleStart string `json:"schedule_start,omitempty"`
	ScheduleEnd   string `json:"schedule_end,omitempty"`
}

type edgeNodeRuntime struct {
	db        *DB
	pm        *ProxyManager
	ip2region *ip2regionManager

	mu               sync.RWMutex
	master           masterNodeState
	nodes            map[int64]*edgeNodeLiveState
	routes           map[string][]ispRouteRule // isp_group -> ordered rules
	blocked          map[string]bool           // isp_group -> block_connect enabled
	strategies       map[string]string         // isp_group -> priority | auto | balance
	speedMetrics     map[int64]map[string]edgeNodeSpeedMetric
	rrCounters       map[string]*atomic.Uint64
	blockNonMainland bool
	ispProbe         ISPProbeSettings
	ispProbeClaimKey [32]byte
	pendingISPAlerts []ispProbeAlert
	flushMu          sync.Mutex
	reloadMu         sync.Mutex
	speedReloadMu    sync.Mutex
}

func newEdgeNodeRuntime(db *DB, pm *ProxyManager, ip2 *ip2regionManager) *edgeNodeRuntime {
	counters := make(map[string]*atomic.Uint64, len(allISPGroups))
	for _, g := range allISPGroups {
		counters[g] = &atomic.Uint64{}
	}
	rt := &edgeNodeRuntime{
		db:           db,
		pm:           pm,
		ip2region:    ip2,
		nodes:        make(map[int64]*edgeNodeLiveState),
		routes:       make(map[string][]ispRouteRule),
		blocked:      make(map[string]bool),
		strategies:   make(map[string]string),
		speedMetrics: make(map[int64]map[string]edgeNodeSpeedMetric),
		rrCounters:   counters,
		ispProbe:     defaultISPProbeSettings(),
	}
	_, _ = rand.Read(rt.ispProbeClaimKey[:])
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
	// Serialize database snapshots with their runtime publication.
	rt.reloadMu.Lock()
	defer rt.reloadMu.Unlock()
	rows, err := rt.db.db.Query(`SELECT id, name, public_port, listen_port, public_host, public_ip, detected_ip, tls_mode, auto_https, cf_record_id, cf_record_type, cf_record_ip, cf_dns_status, token_hash, token_ciphertext, enabled, auto_circuit_breaker, traffic_quota, traffic_used, traffic_reset_day, traffic_reset_period, max_balance_streams, note, agent_version, last_seen_at, total_bytes_in, total_bytes_out FROM edge_nodes ORDER BY sort_order ASC, id ASC`)
	if err != nil {
		return err
	}
	type loadedNode struct {
		id, quota, used                              int64
		totalIn, totalOut                            int64
		publicPort, listenPort, resetDay, maxBalance int
		name, publicHost, publicIP                   string
		detectedIP, tlsMode                          string
		autoHTTPS                                    bool
		cfRecordID, cfRecordType                     string
		cfRecordIP, cfDNSStatus                      string
		tokenHash, tokenCipher                       string
		resetPeriod, note, version, seen             string
		enabled, autoCircuitBreaker                  bool
	}
	var list []loadedNode
	for rows.Next() {
		var item loadedNode
		var enabled, autoHTTPSInt, autoCircuitBreaker int
		if err := rows.Scan(&item.id, &item.name, &item.publicPort, &item.listenPort, &item.publicHost, &item.publicIP, &item.detectedIP, &item.tlsMode, &autoHTTPSInt, &item.cfRecordID, &item.cfRecordType, &item.cfRecordIP, &item.cfDNSStatus, &item.tokenHash, &item.tokenCipher, &enabled, &autoCircuitBreaker, &item.quota, &item.used, &item.resetDay, &item.resetPeriod, &item.maxBalance, &item.note, &item.version, &item.seen, &item.totalIn, &item.totalOut); err != nil {
			rows.Close()
			return err
		}
		item.enabled = enabled != 0
		item.autoCircuitBreaker = autoCircuitBreaker != 0
		item.autoHTTPS = autoHTTPSInt != 0
		list = append(list, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	routeRows, err := rt.db.db.Query(`SELECT isp_group, node_id, sort_order, enabled, schedule_mode, schedule_start, schedule_end FROM isp_route_rules ORDER BY isp_group, sort_order, node_id`)
	if err != nil {
		return err
	}
	routes := make(map[string][]ispRouteRule, len(allISPGroups))
	for routeRows.Next() {
		var group string
		var rule ispRouteRule
		var enabled int
		if err := routeRows.Scan(&group, &rule.NodeID, &rule.SortOrder, &enabled, &rule.ScheduleMode, &rule.ScheduleStart, &rule.ScheduleEnd); err != nil {
			routeRows.Close()
			return err
		}
		rule.Enabled = enabled != 0
		if normMode, normStart, normEnd, normErr := normalizeRouteSchedule(rule.ScheduleMode, rule.ScheduleStart, rule.ScheduleEnd); normErr == nil {
			rule.ScheduleMode = normMode
			rule.ScheduleStart = normStart
			rule.ScheduleEnd = normEnd
		} else {
			rule.ScheduleMode = routeScheduleModeAlways
			rule.ScheduleStart = ""
			rule.ScheduleEnd = ""
		}
		routes[group] = append(routes[group], rule)
	}
	if err := routeRows.Err(); err != nil {
		routeRows.Close()
		return err
	}
	routeRows.Close()

	blocked := make(map[string]bool, len(allISPGroups))
	strategies := make(map[string]string, len(allISPGroups))
	for _, g := range allISPGroups {
		strategies[g] = ispStrategyPriority
	}
	blockNonMainland := false
	if settingRows, err := rt.db.db.Query(`SELECT isp_group, block_connect, strategy FROM isp_group_settings`); err == nil {
		for settingRows.Next() {
			var group, strategy string
			var block int
			if err := settingRows.Scan(&group, &block, &strategy); err == nil {
				if group == ispSettingNonMainlandKey {
					if block != 0 {
						blockNonMainland = true
					}
				} else if validISPGroup(group) {
					if block != 0 {
						blocked[group] = true
					}
					if norm, ok := normalizeISPStrategy(strategy); ok {
						strategies[group] = norm
					}
				}
			}
		}
		settingRows.Close()
	}

	speedMap := rt.loadSpeedMetricsFromDB()
	ispProbeCfg := rt.db.loadISPProbeSettings()

	var (
		mName        = defaultMasterNodeName
		mQuota       int64
		mUsed        int64
		mResetDay    int
		mResetPeriod string
		mMaxBalance  = defaultNodeMaxBalanceStreams
		mNote        string
	)
	_ = rt.db.db.QueryRow(`SELECT name, traffic_quota, traffic_used, traffic_reset_day, traffic_reset_period, max_balance_streams, note FROM master_node_settings WHERE id=1`).
		Scan(&mName, &mQuota, &mUsed, &mResetDay, &mResetPeriod, &mMaxBalance, &mNote)

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if strings.TrimSpace(mName) == "" {
		mName = defaultMasterNodeName
	}
	rt.master.name = mName
	rt.master.quota = mQuota
	rt.master.persistedUsed = mUsed
	rt.master.resetDay = mResetDay
	rt.master.resetPeriod = mResetPeriod
	rt.master.maxBalanceStreams = normalizeMaxBalanceStreams(mMaxBalance)
	rt.master.note = mNote
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
		state.autoHTTPS = item.autoHTTPS
		state.cfRecordID = item.cfRecordID
		state.cfRecordType = item.cfRecordType
		state.cfRecordIP = item.cfRecordIP
		state.cfDNSStatus = item.cfDNSStatus
		state.tokenHash = item.tokenHash
		if item.tokenCipher != "" {
			if plain, err := decryptSiteAccountSecret(item.tokenCipher); err == nil {
				state.tokenPlain = plain
			}
		}
		state.enabled = item.enabled
		monitorOnly := !item.autoCircuitBreaker
		if state.monitorISPOnly != monitorOnly {
			resetISPCircuits(state.ispHealth)
			state.monitorISPOnly = monitorOnly
			if monitorOnly {
				rt.pendingISPAlerts = slices.DeleteFunc(rt.pendingISPAlerts, func(alert ispProbeAlert) bool {
					return alert.nodeID == item.id
				})
			}
		}
		state.quota = item.quota
		state.persistedUsed = item.used
		state.persistedTotalIn = item.totalIn
		state.persistedTotalOut = item.totalOut
		state.resetDay = item.resetDay
		state.resetPeriod = item.resetPeriod
		state.maxBalanceStreams = normalizeMaxBalanceStreams(item.maxBalance)
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
	rt.blocked = blocked
	rt.strategies = strategies
	rt.speedMetrics = speedMap
	rt.blockNonMainland = blockNonMainland
	rt.ispProbe = ispProbeCfg
	return nil
}

func (rt *edgeNodeRuntime) loadSpeedMetricsFromDB() map[int64]map[string]edgeNodeSpeedMetric {
	result := make(map[int64]map[string]edgeNodeSpeedMetric)
	if rt == nil || rt.db == nil || rt.db.db == nil {
		return result
	}
	rows, err := rt.db.db.Query(`SELECT node_id, isp_group, avg_return_bps, avg_outbound_bps, avg_return_rtt_ms, avg_outbound_rtt_ms, total_retrans, sample_count, tested_at FROM edge_node_speed_metrics`)
	if err != nil {
		return result
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID int64
		var m edgeNodeSpeedMetric
		if err := rows.Scan(&nodeID, &m.ISPGroup, &m.AvgReturnBps, &m.AvgOutboundBps, &m.AvgReturnRTTMs, &m.AvgOutboundRTTMs, &m.TotalRetrans, &m.SampleCount, &m.TestedAt); err != nil {
			continue
		}
		m.ISPLabel = ispGroupLabel(m.ISPGroup)
		m.AvgReturnMbps = roundMbps(m.AvgReturnBps)
		m.AvgOutboundMbps = roundMbps(m.AvgOutboundBps)
		byGroup := result[nodeID]
		if byGroup == nil {
			byGroup = make(map[string]edgeNodeSpeedMetric)
			result[nodeID] = byGroup
		}
		byGroup[m.ISPGroup] = m
	}
	return result
}

func (rt *edgeNodeRuntime) reloadSpeedMetrics() {
	if rt == nil {
		return
	}
	rt.speedReloadMu.Lock()
	defer rt.speedReloadMu.Unlock()
	speedMap := rt.loadSpeedMetricsFromDB()
	rt.mu.Lock()
	rt.speedMetrics = speedMap
	rt.mu.Unlock()
}

func (rt *edgeNodeRuntime) publicOrigins() []string {
	if rt == nil {
		return nil
	}
	rt.mu.RLock()
	seen := make(map[string]bool, len(rt.nodes))
	origins := make([]string, 0, len(rt.nodes))
	for _, n := range rt.nodes {
		if n == nil {
			continue
		}
		raw := n.publicURL()
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			continue
		}
		origin := strings.ToLower(u.Scheme) + "://" + u.Host
		if strings.ContainsAny(origin, " \t\r\n;,") || seen[origin] {
			continue
		}
		seen[origin] = true
		origins = append(origins, origin)
	}
	rt.mu.RUnlock()
	sort.Strings(origins)
	return origins
}

func (rt *edgeNodeRuntime) speedMetricsMapCopyLocked(nodeID int64) []edgeNodeSpeedMetric {
	byGroup := rt.speedMetrics[nodeID]
	if len(byGroup) == 0 {
		return nil
	}
	out := make([]edgeNodeSpeedMetric, 0, len(byGroup))
	for _, g := range allISPGroups {
		if m, ok := byGroup[g]; ok {
			out = append(out, m)
		}
	}
	return out
}

func (rt *edgeNodeRuntime) nodeSpeedMetricsCopy(nodeID int64) []edgeNodeSpeedMetric {
	if rt == nil {
		return nil
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.speedMetricsMapCopyLocked(nodeID)
}

func (rt *edgeNodeRuntime) strategyForGroupLocked(group string) string {
	if rt.strategies != nil {
		if s, ok := rt.strategies[group]; ok && s != "" {
			return s
		}
	}
	return ispStrategyPriority
}

type edgeRouteDecision struct {
	ISPGroup      string
	ISPLabel      string
	Region        string
	ISP           string
	NonMainlandCN bool
	NodeID        int64 // 0 = master, -1 = blocked
	NodeName      string
	PublicURL     string
	Token         string
	Blocked       bool
	BlockReason   string // "non_mainland" or "group"
}

// isClientBlocked checks whether the client IP should be rejected on Emby site
// proxy routes. Fast-paths to false when no block policy is enabled, and never
// blocks private/loopback/unparseable addresses.
func (rt *edgeNodeRuntime) isClientBlocked(clientIP string) (bool, string) {
	if rt == nil {
		return false, ""
	}
	rt.mu.RLock()
	active := rt.blockNonMainland || len(rt.blocked) > 0
	rt.mu.RUnlock()
	if !active {
		return false, ""
	}
	decision := rt.selectNodeForIP(clientIP)
	return decision.Blocked, decision.BlockReason
}

// balanceCandidate represents a routable node (Master = 0 or Edge > 0) in exact
// user-configured drag-sort order within an ISP group.
type balanceCandidate struct {
	nodeID            int64
	name              string
	publicURL         string
	token             string
	maxBalanceStreams int
	fallbackStreams   int
}

// pickBalanceCandidateLocked selects a node in Waterfall + Ordered Overflow order:
//  1. Sticky: if the watching device session already has an assigned node that is
//     still in candidates, keep it on that node.
//  2. Phase 1 (Waterfall fill): scan candidates #1 -> #N in drag-sort order and pick
//     the first node whose active watching session count < maxBalanceStreams.
//  3. Phase 2 (All full -> Ordered overflow): pick the node with the smallest
//     excess (activeSessions - maxBalanceStreams), breaking ties strictly by #1 -> #N
//     order (e.g. for 3 nodes capped at 2: #7->#1, #8->#2, #9->#3, #10->#1).
func pickBalanceCandidateLocked(
	candidates []balanceCandidate,
	watchingCounts map[int64]int,
	hasShareCounts bool,
	stickyNodeID int64,
	hasSticky bool,
) *balanceCandidate {
	if len(candidates) == 0 {
		return nil
	}
	if hasSticky {
		for i := range candidates {
			if candidates[i].nodeID == stickyNodeID {
				return &candidates[i]
			}
		}
	}
	loadFor := func(c balanceCandidate) int {
		if hasShareCounts && len(watchingCounts) > 0 {
			return watchingCounts[c.nodeID]
		}
		if hasShareCounts && watchingCounts[c.nodeID] > 0 {
			return watchingCounts[c.nodeID]
		}
		return c.fallbackStreams
	}
	// Phase 1: fill each node in drag-sort order up to its cap.
	for i := range candidates {
		capStreams := normalizeMaxBalanceStreams(candidates[i].maxBalanceStreams)
		if loadFor(candidates[i]) < capStreams {
			return &candidates[i]
		}
	}
	// Phase 2: all nodes are at/above cap; distribute evenly in #1 -> #N order.
	bestIdx := 0
	bestExcess := loadFor(candidates[0]) - normalizeMaxBalanceStreams(candidates[0].maxBalanceStreams)
	for i := 1; i < len(candidates); i++ {
		excess := loadFor(candidates[i]) - normalizeMaxBalanceStreams(candidates[i].maxBalanceStreams)
		if excess < bestExcess {
			bestExcess = excess
			bestIdx = i
		}
	}
	return &candidates[bestIdx]
}

// selectNodeForIP picks the highest-priority / next-balanced online node for the
// client IP's ISP without mutating any session or round-robin state (safe for
// policy checks and diagnostics).
func (rt *edgeNodeRuntime) selectNodeForIP(clientIP string) edgeRouteDecision {
	return rt.selectNodeForIPInternal(clientIP, "", false)
}

// selectNodeForDispatch picks a node for live media playback when no share token
// is present (e.g. direct admin playback or tests).
func (rt *edgeNodeRuntime) selectNodeForDispatch(clientIP string) edgeRouteDecision {
	return rt.selectNodeForIPInternal(clientIP, "", true)
}

// selectNodeForSessionDispatch picks a node for a share-user playback session,
// respecting active watching session stickiness and global per-node session caps.
func (rt *edgeNodeRuntime) selectNodeForSessionDispatch(clientIP, shareTokenHash string) edgeRouteDecision {
	return rt.selectNodeForIPInternal(clientIP, shareTokenHash, true)
}

// nodeDecisionByID returns a routable edge decision for a specific node ID.
func (rt *edgeNodeRuntime) nodeDecisionByID(nodeID int64) (edgeRouteDecision, bool) {
	if rt == nil || nodeID <= 0 {
		return edgeRouteDecision{}, false
	}
	now := time.Now()
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	node := rt.nodes[nodeID]
	if node == nil || !node.routableAt(now) {
		return edgeRouteDecision{}, false
	}
	return edgeRouteDecision{
		NodeID:    node.id,
		NodeName:  node.name,
		PublicURL: node.publicURL(),
		Token:     node.tokenPlain,
	}, true
}

func (rt *edgeNodeRuntime) nodeNameByID(nodeID int64) string {
	if rt == nil || nodeID <= 0 {
		return ""
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if node := rt.nodes[nodeID]; node != nil {
		return node.name
	}
	return ""
}

func (rt *edgeNodeRuntime) lookupISPGroup(clientIP string) string {
	if rt == nil || rt.ip2region == nil || strings.TrimSpace(clientIP) == "" {
		return ispGroupOther
	}
	res := rt.ip2region.Lookup(clientIP)
	if validISPGroup(res.ISPGroup) {
		return res.ISPGroup
	}
	return ispGroupOther
}

func (rt *edgeNodeRuntime) selectEgressTargetLocked(group string, watchingCounts map[int64]int, hasShareCounts bool, now time.Time) (edgeRouteDecision, int64) {
	decision := edgeRouteDecision{
		ISPGroup: group,
		ISPLabel: ispGroupLabel(group),
		NodeID:   0,
		NodeName: rt.masterDisplayNameLocked(),
	}
	rules := rt.routes[group]
	strategy := rt.strategyForGroupLocked(group)
	if strategy == ispStrategyBalance {
		candidates := make([]balanceCandidate, 0, len(rules))
		for _, rule := range rules {
			if !rule.Enabled || !rule.isScheduledActiveAt(now) {
				continue
			}
			if rule.NodeID == 0 {
				if rt.masterQuotaExceededLocked() {
					continue
				}
				candidates = append(candidates, balanceCandidate{
					nodeID:            0,
					name:              decision.NodeName,
					maxBalanceStreams: rt.master.maxBalanceStreams,
					fallbackStreams:   watchingCounts[0],
				})
				continue
			}
			node := rt.nodes[rule.NodeID]
			if node != nil && node.routableForGroupAt(group, now) && node.supportsEgressControl() {
				candidates = append(candidates, balanceCandidate{
					nodeID:            node.id,
					name:              node.name,
					publicURL:         node.publicURL(),
					token:             node.tokenPlain,
					maxBalanceStreams: node.maxBalanceStreams,
					fallbackStreams:   node.activeStreams,
				})
			}
		}
		if chosen := pickBalanceCandidateLocked(candidates, watchingCounts, hasShareCounts, 0, false); chosen != nil {
			if chosen.nodeID == 0 {
				return decision, -1
			}
			decision.NodeID = chosen.nodeID
			decision.NodeName = chosen.name
			decision.PublicURL = chosen.publicURL
			decision.Token = chosen.token
			return decision, chosen.nodeID
		}
		return decision, -1
	}

	for _, rule := range rules {
		if !rule.Enabled || !rule.isScheduledActiveAt(now) {
			continue
		}
		if rule.NodeID == 0 {
			// Soft-deprioritize Master when it has exceeded its traffic quota;
			// if no online egress-capable edge node is available, fall back to Master below.
			if rt.masterQuotaExceededLocked() {
				continue
			}
			return decision, -1
		}
		node := rt.nodes[rule.NodeID]
		if node != nil && node.routableForGroupAt(group, now) && node.supportsEgressControl() {
			decision.NodeID = node.id
			decision.NodeName = node.name
			decision.PublicURL = node.publicURL()
			decision.Token = node.tokenPlain
			return decision, node.id
		}
	}
	return decision, -1
}

// selectEgressDecision picks the best online egress-capable node (or master = -1)
// for a client IP in edge_full mode.
func (rt *edgeNodeRuntime) selectEgressDecision(clientIP string, _ bool) (edgeRouteDecision, int64) {
	group := rt.lookupISPGroup(clientIP)
	if rt == nil {
		return edgeRouteDecision{ISPGroup: group, ISPLabel: ispGroupLabel(group), NodeID: 0, NodeName: defaultMasterNodeName}, -1
	}
	now := routeScheduleNow()
	var watchingCounts map[int64]int
	hasShareCounts := false
	if rt.pm != nil && rt.pm.shares != nil {
		watchingCounts = rt.pm.shares.activeWatchingCountsByNode(now, "")
		hasShareCounts = true
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.selectEgressTargetLocked(group, watchingCounts, hasShareCounts, now)
}

type edgeHandoffEval struct {
	clientISPGroup  string
	strategy        string
	currentRoutable bool
	currentAllowed  bool
	targetDecision  edgeRouteDecision
	targetLockedID  int64
	shouldHandoff   bool
	reason          string
}

// evaluateEdgeFullHandoff determines whether an idle edge_full session should
// seamlessly hand off from currentNodeID (-1=master, >0=edge) to a better node
// for clientIP while respecting balance/auto stability and egress capability.
func (rt *edgeNodeRuntime) evaluateEdgeFullHandoff(clientIP string, currentNodeID int64, currentISPGroup string) edgeHandoffEval {
	group := rt.lookupISPGroup(clientIP)
	eval := edgeHandoffEval{
		clientISPGroup: group,
		targetDecision: edgeRouteDecision{ISPGroup: group, ISPLabel: ispGroupLabel(group), NodeID: 0, NodeName: defaultMasterNodeName},
		targetLockedID: -1,
	}
	if rt == nil {
		eval.currentRoutable = currentNodeID <= 0
		eval.currentAllowed = true
		return eval
	}
	now := routeScheduleNow()
	var watchingCounts map[int64]int
	hasShareCounts := false
	if rt.pm != nil && rt.pm.shares != nil {
		watchingCounts = rt.pm.shares.activeWatchingCountsByNode(now, "")
		hasShareCounts = true
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	eval.targetDecision.NodeName = rt.masterDisplayNameLocked()

	eval.strategy = rt.strategyForGroupLocked(group)
	if currentNodeID < 0 {
		eval.currentRoutable = !rt.masterQuotaExceededLocked()
	} else if currentNodeID > 0 {
		node := rt.nodes[currentNodeID]
		eval.currentRoutable = node != nil && node.routableForGroupAt(group, now) && node.supportsEgressControl()
	}

	ruleNodeID := currentNodeID
	if ruleNodeID < 0 {
		ruleNodeID = 0
	}
	for _, rule := range rt.routes[group] {
		if rule.NodeID == ruleNodeID && rule.Enabled && rule.isScheduledActiveAt(now) {
			eval.currentAllowed = true
			break
		}
	}

	ispChanged := currentISPGroup != "" && group != currentISPGroup
	eval.targetDecision, eval.targetLockedID = rt.selectEgressTargetLocked(group, watchingCounts, hasShareCounts, now)

	if currentNodeID == 0 || eval.targetLockedID == currentNodeID {
		return eval
	}
	if !eval.currentRoutable {
		eval.shouldHandoff = true
		eval.reason = "current_node_unavailable"
		return eval
	}
	if !eval.currentAllowed {
		eval.shouldHandoff = true
		eval.reason = "node_not_allowed_in_isp"
		return eval
	}
	if ispChanged {
		eval.shouldHandoff = true
		eval.reason = "isp_changed"
		return eval
	}
	if eval.strategy == ispStrategyPriority {
		eval.shouldHandoff = true
		eval.reason = "higher_priority_node"
		return eval
	}
	// In balance/auto strategy within the same ISP group, stay pinned to the
	// current routable & enabled node to avoid thrashing on every request.
	return eval
}

func (rt *edgeNodeRuntime) selectNodeForIPInternal(clientIP, shareTokenHash string, _ bool) edgeRouteDecision {
	decision := edgeRouteDecision{
		ISPGroup: ispGroupOther,
		ISPLabel: ispGroupLabel(ispGroupOther),
		NodeID:   0,
		NodeName: defaultMasterNodeName,
	}
	if rt == nil {
		return decision
	}
	addr, parseErr := netip.ParseAddr(strings.TrimSpace(clientIP))
	isPublic := parseErr == nil && dynamicIPIsPublic(addr.Unmap())
	var lookupFound bool
	if rt.ip2region != nil {
		res := rt.ip2region.Lookup(clientIP)
		lookupFound = res.Found
		decision.ISPGroup = res.ISPGroup
		decision.ISPLabel = res.ISPLabel
		decision.Region = res.Region
		decision.ISP = res.ISP
		decision.NonMainlandCN = res.NonMainlandCN
	}
	now := routeScheduleNow()
	// Fetch share watching counts & session stickiness BEFORE acquiring rt.mu
	// to guarantee zero lock inversion between edges.mu and shares.mu.
	var watchingCounts map[int64]int
	var hasShareCounts bool
	var stickyNodeID int64
	var hasSticky bool
	if rt.pm != nil && rt.pm.shares != nil {
		watchingCounts = rt.pm.shares.activeWatchingCountsByNode(now, shareTokenHash)
		hasShareCounts = true
		if shareTokenHash != "" {
			stickyNodeID, hasSticky = rt.pm.shares.activeWatchingSessionNode(shareTokenHash, now)
		}
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	masterName := rt.masterDisplayNameLocked()
	decision.NodeName = masterName
	if isPublic {
		if rt.blockNonMainland && lookupFound && decision.NonMainlandCN {
			decision.Blocked = true
			decision.BlockReason = "non_mainland"
			decision.NodeID = -1
			decision.NodeName = "已禁止连接（非中国大陆）"
			return decision
		}
		if rt.blocked[decision.ISPGroup] {
			decision.Blocked = true
			decision.BlockReason = "group"
			decision.NodeID = -1
			decision.NodeName = "已禁止连接（分组已禁）"
			return decision
		}
	}
	rules := rt.routes[decision.ISPGroup]
	strategy := rt.strategyForGroupLocked(decision.ISPGroup)
	if strategy == ispStrategyBalance {
		candidates := make([]balanceCandidate, 0, len(rules))
		for _, rule := range rules {
			if !rule.Enabled || !rule.isScheduledActiveAt(now) {
				continue
			}
			if rule.NodeID == 0 {
				if rt.masterQuotaExceededLocked() {
					continue
				}
				candidates = append(candidates, balanceCandidate{
					nodeID:            0,
					name:              masterName,
					maxBalanceStreams: rt.master.maxBalanceStreams,
					fallbackStreams:   watchingCounts[0],
				})
				continue
			}
			node := rt.nodes[rule.NodeID]
			if node != nil && node.routableForGroupAt(decision.ISPGroup, now) {
				candidates = append(candidates, balanceCandidate{
					nodeID:            node.id,
					name:              node.name,
					publicURL:         node.publicURL(),
					token:             node.tokenPlain,
					maxBalanceStreams: node.maxBalanceStreams,
					fallbackStreams:   node.activeStreams,
				})
			}
		}
		if chosen := pickBalanceCandidateLocked(candidates, watchingCounts, hasShareCounts, stickyNodeID, hasSticky); chosen != nil {
			decision.NodeID = chosen.nodeID
			decision.NodeName = chosen.name
			decision.PublicURL = chosen.publicURL
			decision.Token = chosen.token
			return decision
		}
		// Always fall back to Master if every enabled node in the group is unavailable.
		decision.NodeID = 0
		decision.NodeName = masterName
		return decision
	}

	for _, rule := range rules {
		if !rule.Enabled || !rule.isScheduledActiveAt(now) {
			continue
		}
		if rule.NodeID == 0 {
			// Soft-deprioritize Master when it exceeds its traffic quota; if no
			// online edge node exists in this group, Master still serves as fallback.
			if rt.masterQuotaExceededLocked() {
				continue
			}
			decision.NodeID = 0
			decision.NodeName = masterName
			return decision
		}
		node := rt.nodes[rule.NodeID]
		if node != nil && node.routableForGroupAt(decision.ISPGroup, now) {
			decision.NodeID = node.id
			decision.NodeName = node.name
			decision.PublicURL = node.publicURL()
			decision.Token = node.tokenPlain
			return decision
		}
	}
	// Always fall back to Master if every configured edge node is unavailable.
	decision.NodeID = 0
	decision.NodeName = masterName
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
		inDelta := item.state.pendingTotalIn.Swap(0)
		outDelta := item.state.pendingTotalOut.Swap(0)
		if delta > 0 || inDelta > 0 || outDelta > 0 {
			if _, err := rt.db.db.Exec(
				`UPDATE edge_nodes SET traffic_used=traffic_used+?, total_bytes_in=total_bytes_in+?, total_bytes_out=total_bytes_out+? WHERE id=?`,
				delta, inDelta, outDelta, item.state.id,
			); err != nil {
				if delta > 0 {
					item.state.pending.Add(delta)
				}
				if inDelta > 0 {
					item.state.pendingTotalIn.Add(inDelta)
				}
				if outDelta > 0 {
					item.state.pendingTotalOut.Add(outDelta)
				}
				log.Printf("edge node %d: flush traffic: %v", item.state.id, err)
			} else {
				rt.mu.Lock()
				item.state.persistedUsed += delta
				item.state.persistedTotalIn += inDelta
				item.state.persistedTotalOut += outDelta
				rt.mu.Unlock()
			}
		}
		if !item.lastSeen.IsZero() {
			_, _ = rt.db.db.Exec(`UPDATE edge_nodes SET detected_ip=?, agent_version=?, last_seen_at=? WHERE id=?`,
				item.detectedIP, item.version, item.lastSeen.UTC().Format(time.RFC3339), item.state.id)
		}
	}
	if masterDelta := rt.master.pending.Swap(0); masterDelta > 0 {
		if _, err := rt.db.db.Exec(`UPDATE master_node_settings SET traffic_used=traffic_used+? WHERE id=1`, masterDelta); err != nil {
			rt.master.pending.Add(masterDelta)
			log.Printf("master node: flush traffic: %v", err)
		} else {
			rt.mu.Lock()
			rt.master.persistedUsed += masterDelta
			rt.mu.Unlock()
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
	var mDay int
	var mPeriod string
	if err := rt.db.db.QueryRow(`SELECT traffic_reset_day, traffic_reset_period FROM master_node_settings WHERE id=1`).Scan(&mDay, &mPeriod); err == nil && mDay > 0 {
		if current, _ := shareResetPeriod(now, mDay); current != "" && current != mPeriod {
			if _, err := rt.db.db.Exec(`UPDATE master_node_settings SET traffic_used=0, traffic_reset_period=?, updated_at=CURRENT_TIMESTAMP WHERE id=1`, current); err == nil {
				rt.mu.Lock()
				rt.master.pending.Store(0)
				rt.master.persistedUsed = 0
				rt.master.resetPeriod = current
				rt.mu.Unlock()
				log.Printf("master node: monthly traffic reset (%s)", current)
			}
		}
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

func edgeNodeUninstallCommand() string {
	return fmt.Sprintf("bash <(curl -fsSL %s) agent --uninstall", edgeInstallScriptRawBase)
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
	online := state.isOnline(now)
	autoCircuitBreaker := !state.monitorISPOnly
	view := EdgeNode{
		ID:               state.id,
		Name:             state.name,
		PublicPort:       state.publicPort,
		ListenPort:       state.listenPort,
		PublicHost:       state.publicHost,
		PublicIP:         state.publicIP,
		DetectedIP:       state.detectedIP,
		TLSMode:          state.tlsMode,
		AutoHTTPS:        state.autoHTTPS,
		CFDNSStatus:      state.cfDNSStatus,
		CFRecordIP:       state.cfRecordIP,
		Enabled:          state.enabled,
		AutoCircuitBreaker: &autoCircuitBreaker,
		TrafficQuota:      state.quota,
		TrafficUsed:       state.totalUsed(),
		ResetDay:          state.resetDay,
		MaxBalanceStreams: normalizeMaxBalanceStreams(state.maxBalanceStreams),
		Note:              state.note,
		AgentVersion:     state.agentVersion,
		MasterVersion:    appVersion,
		UpgradeAvailable: online && state.agentVersion != "" && state.agentVersion != appVersion,
		UpgradeTarget:    state.upgradeTarget,
		UpgradeStatus:    state.upgradeStatus,
		Online:           online,
		TLSActive:        state.tlsActive,
		ActiveStreams:    state.activeStreams,
		CurrentBps:       state.currentBps,
		MemoryBytes:      state.memoryBytes,
		UptimeSec:        state.uptimeSec,
		PublicURL:        state.publicURL(),
		Status:           state.statusAt(now),
	}
	var rawLatencies map[int64]int
	if len(state.upstreamLatencies) > 0 {
		rawLatencies = make(map[int64]int, len(state.upstreamLatencies))
		for sid, ms := range state.upstreamLatencies {
			rawLatencies[sid] = ms
		}
	}
	if !state.lastSeen.IsZero() {
		view.LastSeenAt = state.lastSeen.UTC().Format(time.RFC3339)
	}
	token := state.tokenPlain
	listenPort := state.effectiveListenPort()
	view.SpeedMetrics = a.pm.edges.speedMetricsMapCopyLocked(state.id)
	view.ISPHealth, view.AnyCircuitTripped = buildISPHealthViews(state.ispHealth)
	a.pm.edges.mu.RUnlock()

	if len(rawLatencies) > 0 && a.pm != nil {
		latViews := make([]edgeSiteLatency, 0, len(rawLatencies))
		a.pm.mu.RLock()
		for sid, ms := range rawLatencies {
			name := fmt.Sprintf("站点 #%d", sid)
			if inst := a.pm.proxies[sid]; inst != nil && inst.Site.Name != "" {
				name = inst.Site.Name
			}
			latViews = append(latViews, edgeSiteLatency{SiteName: name, LatencyMs: ms})
		}
		a.pm.mu.RUnlock()
		sort.Slice(latViews, func(i, j int) bool {
			return latViews[i].SiteName < latViews[j].SiteName
		})
		view.UpstreamLatencies = latViews
	}

	if view.ResetDay > 0 {
		if _, next := shareResetPeriod(now.In(a.db.shareLocation()), view.ResetDay); !next.IsZero() {
			view.NextResetAt = next.Format("2006-01-02")
		}
	}
	if includeCommand {
		if token != "" {
			view.InstallCommand = a.edgeNodeInstallCommand(view.ID, listenPort, token, origin)
		}
		view.UninstallCommand = edgeNodeUninstallCommand()
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
	if input.MaxBalanceStreams < 0 || input.MaxBalanceStreams > maxNodeMaxBalanceStreams {
		return input, errors.New("负载均衡上限应为 1–99 路")
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
		rows, err := a.db.db.Query(`SELECT id FROM edge_nodes ORDER BY sort_order ASC, id ASC`)
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
		var autoCfg edgeAutoHTTPSSettings
		if input.AutoHTTPS {
			if a.edgeAutoHTTPS == nil {
				a.jsonErr(w, http.StatusBadRequest, "从机自动 HTTPS 服务未初始化")
				return
			}
			autoCfg, err = a.edgeAutoHTTPS.loadSettings()
			if err != nil || autoCfg.BaseDomain == "" || autoCfg.CFTokenCiphertext == "" {
				a.jsonErr(w, http.StatusBadRequest, "请先在「设置 → 从机自动 HTTPS」配置泛域名与 Cloudflare API Token")
				return
			}
			if input.PublicHost != "" && !isSingleLevelSubdomainOf(input.PublicHost, autoCfg.BaseDomain) {
				a.jsonErr(w, http.StatusBadRequest, fmt.Sprintf("开启自动 HTTPS 时，自定义域名必须是 *.%s 下的一级子域名（留空将自动分配）", autoCfg.BaseDomain))
				return
			}
			input.TLSMode = edgeNodeTLSModeAuto
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
		initialDNSStatus := ""
		if input.AutoHTTPS {
			initialDNSStatus = "pending_ip"
		}
		tx, err := a.db.db.Begin()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "创建从机失败")
			return
		}
		defer tx.Rollback()
		var maxNodeOrder int
		_ = tx.QueryRow(`SELECT COALESCE(MAX(sort_order), 0) FROM edge_nodes`).Scan(&maxNodeOrder)
		maxBalance := normalizeMaxBalanceStreams(input.MaxBalanceStreams)
		res, err := tx.Exec(`INSERT INTO edge_nodes(name, public_port, listen_port, public_host, public_ip, tls_mode, auto_https, cf_dns_status, token_hash, token_ciphertext, enabled, auto_circuit_breaker, traffic_quota, traffic_reset_day, traffic_reset_period, max_balance_streams, note, sort_order) VALUES(?,?,?,?,?,?,?,?,?,?,?,COALESCE(?,1),?,?,?,?,?,?)`,
			input.Name, input.PublicPort, input.ListenPort, input.PublicHost, input.PublicIP, input.TLSMode, sqliteBool(input.AutoHTTPS), initialDNSStatus, edgeNodeTokenHash(token), tokenCipher, sqliteBool(enabled), input.AutoCircuitBreaker, input.TrafficQuota, input.ResetDay, resetPeriod, maxBalance, input.Note, maxNodeOrder+1)
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "创建从机失败")
			return
		}
		id, err := res.LastInsertId()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "创建从机失败")
			return
		}
		if input.AutoHTTPS && input.PublicHost == "" {
			assignedHost := autoEdgeHostname(autoCfg.SubdomainPrefix, id, autoCfg.BaseDomain)
			if _, err := tx.Exec(`UPDATE edge_nodes SET public_host=? WHERE id=?`, assignedHost, id); err != nil {
				a.jsonErr(w, http.StatusInternalServerError, "分配自动 HTTPS 子域名失败")
				return
			}
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
		if input.AutoHTTPS && input.PublicIP != "" && a.edgeAutoHTTPS != nil {
			syncCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			_ = a.edgeAutoHTTPS.syncNodeDNS(syncCtx, id)
			cancel()
		}
		view, _ := a.buildEdgeNodeView(id, origin, true)
		a.jsonResponse(w, http.StatusCreated, view)
	default:
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

var errInvalidEdgeNodeOrder = errors.New("invalid edge node order")

func (d *DB) reorderEdgeNodes(nodeIDs []int64) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT id FROM edge_nodes`)
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
	if len(nodeIDs) != len(stored) {
		return fmt.Errorf("%w: node_ids must contain every edge node exactly once", errInvalidEdgeNodeOrder)
	}
	seen := make(map[int64]struct{}, len(nodeIDs))
	for position, id := range nodeIDs {
		if id <= 0 {
			return fmt.Errorf("%w: invalid edge node id", errInvalidEdgeNodeOrder)
		}
		if _, ok := stored[id]; !ok {
			return fmt.Errorf("%w: edge node %d does not exist", errInvalidEdgeNodeOrder, id)
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("%w: edge node %d is duplicated", errInvalidEdgeNodeOrder, id)
		}
		seen[id] = struct{}{}
		if _, err := tx.Exec(`UPDATE edge_nodes SET sort_order=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, position+1, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PUT /api/edge-nodes/reorder
func (a *App) handleEdgeNodeReorder(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPut {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		NodeIDs []int64 `json:"node_ids"`
	}
	if err := decodeJSONBody(w, r, &req); err != nil {
		a.jsonErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := a.db.reorderEdgeNodes(req.NodeIDs); err != nil {
		if errors.Is(err, errInvalidEdgeNodeOrder) {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		log.Printf("reorder edge nodes: %v", err)
		a.jsonErr(w, http.StatusInternalServerError, "保存从机顺序失败")
		return
	}
	a.jsonOK(w, map[string]any{
		"node_ids": req.NodeIDs,
	})
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
			var prevAutoInt int
			var prevHost, prevRecID, prevRecType, prevRecIP, prevDNSStatus, prevDetectedIP string
			_ = a.db.db.QueryRow(`SELECT auto_https, public_host, cf_record_id, cf_record_type, cf_record_ip, cf_dns_status, detected_ip FROM edge_nodes WHERE id=?`, id).
				Scan(&prevAutoInt, &prevHost, &prevRecID, &prevRecType, &prevRecIP, &prevDNSStatus, &prevDetectedIP)
			prevAuto := prevAutoInt != 0

			nextRecID, nextRecType, nextRecIP, nextDNSStatus := prevRecID, prevRecType, prevRecIP, prevDNSStatus
			var staleRecordToDelete string

			if input.AutoHTTPS {
				if a.edgeAutoHTTPS == nil {
					a.jsonErr(w, http.StatusBadRequest, "从机自动 HTTPS 服务未初始化")
					return
				}
				autoCfg, cfgErr := a.edgeAutoHTTPS.loadSettings()
				if cfgErr != nil || autoCfg.BaseDomain == "" || autoCfg.CFTokenCiphertext == "" {
					a.jsonErr(w, http.StatusBadRequest, "请先在「设置 → 从机自动 HTTPS」配置泛域名与 Cloudflare API Token")
					return
				}
				if input.PublicHost == "" {
					input.PublicHost = autoEdgeHostname(autoCfg.SubdomainPrefix, id, autoCfg.BaseDomain)
				} else if !isSingleLevelSubdomainOf(input.PublicHost, autoCfg.BaseDomain) {
					a.jsonErr(w, http.StatusBadRequest, fmt.Sprintf("开启自动 HTTPS 时，自定义域名必须是 *.%s 下的一级子域名（留空将自动分配）", autoCfg.BaseDomain))
					return
				}
				input.TLSMode = edgeNodeTLSModeAuto
				if prevRecID != "" && !strings.EqualFold(prevHost, input.PublicHost) {
					staleRecordToDelete = prevRecID
					nextRecID, nextRecType, nextRecIP = "", "", ""
				}
				if nextDNSStatus == "" || !prevAuto || staleRecordToDelete != "" {
					nextDNSStatus = "pending_ip"
				}
			} else if prevAuto {
				staleRecordToDelete = prevRecID
				nextRecID, nextRecType, nextRecIP, nextDNSStatus = "", "", "", ""
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
			if _, err := a.db.db.Exec(`UPDATE edge_nodes SET name=?, public_port=?, listen_port=?, public_host=?, public_ip=?, tls_mode=?, auto_https=?, cf_record_id=?, cf_record_type=?, cf_record_ip=?, cf_dns_status=?, enabled=?, auto_circuit_breaker=COALESCE(?,auto_circuit_breaker), traffic_quota=?, traffic_reset_period=CASE WHEN traffic_reset_day<>? THEN ? ELSE traffic_reset_period END, traffic_reset_day=?, max_balance_streams=CASE WHEN ? > 0 THEN ? ELSE max_balance_streams END, note=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
				input.Name, input.PublicPort, input.ListenPort, input.PublicHost, input.PublicIP, input.TLSMode, sqliteBool(input.AutoHTTPS), nextRecID, nextRecType, nextRecIP, nextDNSStatus, sqliteBool(enabled), input.AutoCircuitBreaker, input.TrafficQuota, input.ResetDay, resetPeriod, input.ResetDay, input.MaxBalanceStreams, input.MaxBalanceStreams, input.Note, id); err != nil {
				a.jsonErr(w, http.StatusInternalServerError, "保存失败")
				return
			}
			var reloadErr error
			if a.pm != nil && a.pm.edges != nil {
				reloadErr = a.pm.edges.reload()
			}
			if staleRecordToDelete != "" && a.edgeAutoHTTPS != nil {
				a.edgeAutoHTTPS.deleteRecordAsync(staleRecordToDelete)
			}
			if input.AutoHTTPS && a.edgeAutoHTTPS != nil {
				effectiveIP := input.PublicIP
				if effectiveIP == "" {
					if a.pm != nil && a.pm.edges != nil {
						a.pm.edges.mu.RLock()
						if st := a.pm.edges.nodes[id]; st != nil && st.detectedIP != "" {
							effectiveIP = st.detectedIP
						}
						a.pm.edges.mu.RUnlock()
					}
					if effectiveIP == "" {
						effectiveIP = prevDetectedIP
					}
				}
				if effectiveIP != "" {
					syncCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
					_ = a.edgeAutoHTTPS.syncNodeDNS(syncCtx, id)
					cancel()
				}
			}
			if reloadErr != nil && input.AutoCircuitBreaker != nil {
				log.Printf("edge node %d settings saved but runtime reload failed: %v", id, reloadErr)
				a.jsonErr(w, http.StatusInternalServerError, "配置已保存，但节点运行状态刷新失败，请重试保存")
				return
			}
			view, _ := a.buildEdgeNodeView(id, origin, true)
			a.jsonOK(w, view)
		case http.MethodDelete:
			var cfRecordID string
			_ = a.db.db.QueryRow(`SELECT cf_record_id FROM edge_nodes WHERE id=?`, id).Scan(&cfRecordID)
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
			if cfRecordID != "" && a.edgeAutoHTTPS != nil {
				a.edgeAutoHTTPS.deleteRecordAsync(cfRecordID)
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
	case "upgrade":
		if r.Method != http.MethodPost {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			Version string `json:"version"`
		}
		if r.Body != nil {
			_ = decodeJSONBody(w, r, &req)
		}
		target := strings.TrimSpace(req.Version)
		if target == "" {
			target = appVersion
		}
		if !validEdgeReleaseVersion(target) {
			a.jsonErr(w, http.StatusBadRequest, "目标版本号格式无效")
			return
		}
		if a.pm != nil && a.pm.edges != nil {
			a.pm.edges.mu.Lock()
			if state := a.pm.edges.nodes[id]; state != nil {
				state.upgradeTarget = target
				state.upgradeRequestID = newEdgeUpgradeRequestID()
				state.upgradeStatus = "pending"
			}
			a.pm.edges.mu.Unlock()
		}
		view, _ := a.buildEdgeNodeView(id, origin, true)
		a.jsonOK(w, view)
	case "probe-isp":
		if r.Method != http.MethodPost {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if a.pm != nil && a.pm.edges != nil {
			a.pm.edges.mu.Lock()
			if state := a.pm.edges.nodes[id]; state != nil {
				state.ispForceProbeID = newEdgeUpgradeRequestID()
			}
			a.pm.edges.mu.Unlock()
		}
		view, _ := a.buildEdgeNodeView(id, origin, true)
		a.jsonOK(w, view)
	case "reset-circuit":
		if r.Method != http.MethodPost {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if a.pm != nil && a.pm.edges != nil {
			a.pm.edges.mu.Lock()
			if state := a.pm.edges.nodes[id]; state != nil {
				resetISPCircuits(state.ispHealth)
			}
			a.pm.edges.mu.Unlock()
		}
		view, _ := a.buildEdgeNodeView(id, origin, true)
		a.jsonOK(w, view)
	default:
		a.jsonErr(w, http.StatusNotFound, "not found")
	}
}

type ispRouteNodeView struct {
	NodeID            int64                `json:"node_id"`
	Name              string               `json:"name"`
	IsMaster          bool                 `json:"is_master"`
	Enabled           bool                 `json:"enabled"`
	Online            bool                 `json:"online"`
	Status            string               `json:"status"`
	Active            bool                 `json:"active"`
	CircuitTripped    bool                 `json:"circuit_tripped,omitempty"`
	ScheduleMode      string               `json:"schedule_mode"`
	ScheduleStart     string               `json:"schedule_start,omitempty"`
	ScheduleEnd       string               `json:"schedule_end,omitempty"`
	ScheduleDormant   bool                 `json:"schedule_dormant,omitempty"`
	ScheduleResumeAt  string               `json:"schedule_resume_at,omitempty"`
	MaxBalanceStreams int                  `json:"max_balance_streams"`
	ActiveSessions    int                  `json:"active_sessions"`
	PublicURL         string               `json:"public_url,omitempty"`
	SpeedMetric       *edgeNodeSpeedMetric `json:"speed_metric,omitempty"`
}

type ispRouteGroupView struct {
	Group        string             `json:"group"`
	Label        string             `json:"label"`
	Strategy     string             `json:"strategy"`
	BlockConnect bool               `json:"block_connect"`
	Nodes        []ispRouteNodeView `json:"nodes"`
}

func (a *App) buildISPRoutesPayload() map[string]any {
	blockNonMainland := false
	if a.pm != nil && a.pm.edges != nil {
		a.pm.edges.mu.RLock()
		blockNonMainland = a.pm.edges.blockNonMainland
		a.pm.edges.mu.RUnlock()
	}
	return map[string]any{
		"block_non_mainland": blockNonMainland,
		"groups":             a.buildISPRouteGroups(),
	}
}

func (a *App) buildISPRouteGroups() []ispRouteGroupView {
	now := routeScheduleNow()
	result := make([]ispRouteGroupView, 0, len(allISPGroups))
	if a.pm == nil || a.pm.edges == nil {
		return result
	}
	var watchingCounts map[int64]int
	if a.pm.shares != nil {
		watchingCounts = a.pm.shares.activeWatchingCountsByNode(now, "")
	}
	a.pm.edges.mu.RLock()
	defer a.pm.edges.mu.RUnlock()
	for _, group := range allISPGroups {
		rules := a.pm.edges.routes[group]
		blockConnect := a.pm.edges.blocked[group]
		strategy := a.pm.edges.strategyForGroupLocked(group)
		nodes := make([]ispRouteNodeView, 0, len(rules))

		metricPtrFor := func(nodeID int64) *edgeNodeSpeedMetric {
			if byGroup := a.pm.edges.speedMetrics[nodeID]; byGroup != nil {
				if m, ok := byGroup[group]; ok {
					mc := m
					return &mc
				}
			}
			return nil
		}

		// Determine active node(s) according to the active dispatch strategy.
		activePool := make(map[int64]bool)
		if !blockConnect {
			switch strategy {
			case ispStrategyBalance:
				for _, rule := range rules {
					if !rule.Enabled || !rule.isScheduledActiveAt(now) {
						continue
					}
					if rule.NodeID == 0 {
						if !a.pm.edges.masterQuotaExceededLocked() {
							activePool[0] = true
						}
						continue
					}
					if node := a.pm.edges.nodes[rule.NodeID]; node != nil && node.routableForGroupAt(group, now) {
						activePool[node.id] = true
					}
				}
				if len(activePool) == 0 {
					activePool[0] = true
				}
			default:
				assigned := false
				for _, rule := range rules {
					if !rule.Enabled || !rule.isScheduledActiveAt(now) {
						continue
					}
					if rule.NodeID == 0 {
						if a.pm.edges.masterQuotaExceededLocked() {
							continue
						}
						activePool[0] = true
						assigned = true
						break
					}
					if node := a.pm.edges.nodes[rule.NodeID]; node != nil && node.routableForGroupAt(group, now) {
						activePool[node.id] = true
						assigned = true
						break
					}
				}
				if !assigned {
					activePool[0] = true
				}
			}
		}

		masterName := a.pm.edges.masterDisplayNameLocked()
		masterStatus := a.pm.edges.master.status()
		masterCap := normalizeMaxBalanceStreams(a.pm.edges.master.maxBalanceStreams)
		for _, rule := range rules {
			schedMode := rule.effectiveScheduleMode()
			schedDormant := rule.isScheduledDormantAt(now)
			schedResume := rule.scheduleResumeAt(now)
			if rule.NodeID == 0 {
				nodes = append(nodes, ispRouteNodeView{
					NodeID:            0,
					Name:              masterName,
					IsMaster:          true,
					Enabled:           rule.Enabled,
					Online:            true,
					Status:            masterStatus,
					Active:            activePool[0],
					ScheduleMode:      schedMode,
					ScheduleStart:     rule.ScheduleStart,
					ScheduleEnd:       rule.ScheduleEnd,
					ScheduleDormant:   schedDormant,
					ScheduleResumeAt:  schedResume,
					MaxBalanceStreams: masterCap,
					ActiveSessions:    watchingCounts[0],
					SpeedMetric:       metricPtrFor(0),
				})
				continue
			}
			node := a.pm.edges.nodes[rule.NodeID]
			if node == nil {
				continue
			}
			nodes = append(nodes, ispRouteNodeView{
				NodeID:            node.id,
				Name:              node.name,
				Enabled:           rule.Enabled,
				Online:            node.isOnline(now),
				Status:            node.statusAt(now),
				Active:            activePool[node.id],
				CircuitTripped:    node.isISPTripped(group),
				ScheduleMode:      schedMode,
				ScheduleStart:     rule.ScheduleStart,
				ScheduleEnd:       rule.ScheduleEnd,
				ScheduleDormant:   schedDormant,
				ScheduleResumeAt:  schedResume,
				MaxBalanceStreams: normalizeMaxBalanceStreams(node.maxBalanceStreams),
				ActiveSessions:    watchingCounts[node.id],
				PublicURL:         node.publicURL(),
				SpeedMetric:       metricPtrFor(node.id),
			})
		}
		result = append(result, ispRouteGroupView{
			Group:        group,
			Label:        ispGroupLabel(group),
			Strategy:     strategy,
			BlockConnect: blockConnect,
			Nodes:        nodes,
		})
	}
	return result
}

// GET/PUT /api/isp-routes
func (a *App) handleISPRoutes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		a.jsonOK(w, a.buildISPRoutesPayload())
	case http.MethodPut:
		var req struct {
			BlockNonMainland *bool   `json:"block_non_mainland"`
			Group            string  `json:"group"`
			Strategy         *string `json:"strategy"`
			BlockConnect     *bool   `json:"block_connect"`
			Nodes            []struct {
				NodeID            int64   `json:"node_id"`
				Enabled           bool    `json:"enabled"`
				MaxBalanceStreams *int    `json:"max_balance_streams,omitempty"`
				ScheduleMode      *string `json:"schedule_mode,omitempty"`
				ScheduleStart     *string `json:"schedule_start,omitempty"`
				ScheduleEnd       *string `json:"schedule_end,omitempty"`
			} `json:"nodes"`
			SchedulePatch *struct {
				NodeID        int64    `json:"node_id"`
				Groups        []string `json:"groups"`
				ScheduleMode  string   `json:"schedule_mode"`
				ScheduleStart string   `json:"schedule_start"`
				ScheduleEnd   string   `json:"schedule_end"`
			} `json:"schedule_patch,omitempty"`
		}
		if err := decodeJSONBody(w, r, &req); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "invalid request")
			return
		}
		req.Group = strings.ToLower(strings.TrimSpace(req.Group))
		hasGroupOp := req.Group != "" || len(req.Nodes) > 0 || req.BlockConnect != nil || req.Strategy != nil
		if !hasGroupOp && req.BlockNonMainland == nil && req.SchedulePatch == nil {
			a.jsonErr(w, http.StatusBadRequest, "线路配置参数无效")
			return
		}
		if hasGroupOp && (!validISPGroup(req.Group) || (len(req.Nodes) == 0 && req.BlockConnect == nil && req.Strategy == nil)) {
			a.jsonErr(w, http.StatusBadRequest, "线路分组参数无效")
			return
		}
		var normalizedStrategy string
		if req.Strategy != nil {
			var ok bool
			normalizedStrategy, ok = normalizeISPStrategy(*req.Strategy)
			if !ok {
				a.jsonErr(w, http.StatusBadRequest, "调度策略无效")
				return
			}
		}
		var patchMode, patchStart, patchEnd string
		var patchGroups []string
		if req.SchedulePatch != nil {
			if req.SchedulePatch.NodeID < 0 {
				a.jsonErr(w, http.StatusBadRequest, "节点参数无效")
				return
			}
			var err error
			patchMode, patchStart, patchEnd, err = normalizeRouteSchedule(
				req.SchedulePatch.ScheduleMode,
				req.SchedulePatch.ScheduleStart,
				req.SchedulePatch.ScheduleEnd,
			)
			if err != nil {
				a.jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
			seenGrp := make(map[string]bool, len(req.SchedulePatch.Groups))
			for _, g := range req.SchedulePatch.Groups {
				g = strings.ToLower(strings.TrimSpace(g))
				if !validISPGroup(g) {
					a.jsonErr(w, http.StatusBadRequest, "同步分组参数无效")
					return
				}
				if !seenGrp[g] {
					seenGrp[g] = true
					patchGroups = append(patchGroups, g)
				}
			}
			if len(patchGroups) == 0 {
				a.jsonErr(w, http.StatusBadRequest, "请至少选择一个线路分组")
				return
			}
		}
		tx, err := a.db.db.Begin()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "保存线路配置失败")
			return
		}
		defer tx.Rollback()
		if req.BlockNonMainland != nil {
			if _, err := tx.Exec(`INSERT INTO isp_group_settings(isp_group, block_connect, updated_at) VALUES(?, ?, CURRENT_TIMESTAMP) ON CONFLICT(isp_group) DO UPDATE SET block_connect=excluded.block_connect, updated_at=CURRENT_TIMESTAMP`,
				ispSettingNonMainlandKey, sqliteBool(*req.BlockNonMainland)); err != nil {
				a.jsonErr(w, http.StatusInternalServerError, "保存线路配置失败")
				return
			}
		}
		if req.BlockConnect != nil {
			if _, err := tx.Exec(`INSERT INTO isp_group_settings(isp_group, block_connect, updated_at) VALUES(?, ?, CURRENT_TIMESTAMP) ON CONFLICT(isp_group) DO UPDATE SET block_connect=excluded.block_connect, updated_at=CURRENT_TIMESTAMP`,
				req.Group, sqliteBool(*req.BlockConnect)); err != nil {
				a.jsonErr(w, http.StatusInternalServerError, "保存线路配置失败")
				return
			}
		}
		if req.Strategy != nil {
			if _, err := tx.Exec(`INSERT INTO isp_group_settings(isp_group, strategy, updated_at) VALUES(?, ?, CURRENT_TIMESTAMP) ON CONFLICT(isp_group) DO UPDATE SET strategy=excluded.strategy, updated_at=CURRENT_TIMESTAMP`,
				req.Group, normalizedStrategy); err != nil {
				a.jsonErr(w, http.StatusInternalServerError, "保存调度策略失败")
				return
			}
		}
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
			if item.MaxBalanceStreams != nil {
				capVal := *item.MaxBalanceStreams
				if capVal < minNodeMaxBalanceStreams || capVal > maxNodeMaxBalanceStreams {
					a.jsonErr(w, http.StatusBadRequest, "负载均衡上限应为 1–99 路")
					return
				}
				if item.NodeID == 0 {
					if _, err := tx.Exec(`UPDATE master_node_settings SET max_balance_streams=?, updated_at=CURRENT_TIMESTAMP WHERE id=1`, capVal); err != nil {
						a.jsonErr(w, http.StatusInternalServerError, "保存主机负载上限失败")
						return
					}
				} else {
					if _, err := tx.Exec(`UPDATE edge_nodes SET max_balance_streams=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, capVal, item.NodeID); err != nil {
						a.jsonErr(w, http.StatusInternalServerError, "保存节点负载上限失败")
						return
					}
				}
			}
			if item.ScheduleMode != nil {
				startStr := ""
				endStr := ""
				if item.ScheduleStart != nil {
					startStr = *item.ScheduleStart
				}
				if item.ScheduleEnd != nil {
					endStr = *item.ScheduleEnd
				}
				sMode, sStart, sEnd, sErr := normalizeRouteSchedule(*item.ScheduleMode, startStr, endStr)
				if sErr != nil {
					a.jsonErr(w, http.StatusBadRequest, sErr.Error())
					return
				}
				if _, err := tx.Exec(`INSERT INTO isp_route_rules(isp_group, node_id, sort_order, enabled, schedule_mode, schedule_start, schedule_end) VALUES(?,?,?,?,?,?,?) ON CONFLICT(isp_group, node_id) DO UPDATE SET sort_order=excluded.sort_order, enabled=excluded.enabled, schedule_mode=excluded.schedule_mode, schedule_start=excluded.schedule_start, schedule_end=excluded.schedule_end`,
					req.Group, item.NodeID, idx+1, sqliteBool(item.Enabled), sMode, sStart, sEnd); err != nil {
					a.jsonErr(w, http.StatusInternalServerError, "保存线路排序失败")
					return
				}
			} else {
				if _, err := tx.Exec(`INSERT INTO isp_route_rules(isp_group, node_id, sort_order, enabled) VALUES(?,?,?,?) ON CONFLICT(isp_group, node_id) DO UPDATE SET sort_order=excluded.sort_order, enabled=excluded.enabled`,
					req.Group, item.NodeID, idx+1, sqliteBool(item.Enabled)); err != nil {
					a.jsonErr(w, http.StatusInternalServerError, "保存线路排序失败")
					return
				}
			}
		}
		if req.SchedulePatch != nil {
			if req.SchedulePatch.NodeID > 0 {
				var count int
				if err := tx.QueryRow(`SELECT COUNT(*) FROM edge_nodes WHERE id=?`, req.SchedulePatch.NodeID).Scan(&count); err != nil || count == 0 {
					a.jsonErr(w, http.StatusBadRequest, "包含不存在的从机")
					return
				}
			}
			for _, g := range patchGroups {
				var nextOrder int
				_ = tx.QueryRow(`SELECT COALESCE(MAX(sort_order), 0) + 1 FROM isp_route_rules WHERE isp_group=?`, g).Scan(&nextOrder)
				if nextOrder <= 0 {
					nextOrder = 1
				}
				if _, err := tx.Exec(`INSERT INTO isp_route_rules(isp_group, node_id, sort_order, enabled, schedule_mode, schedule_start, schedule_end) VALUES(?,?,?,1,?,?,?) ON CONFLICT(isp_group, node_id) DO UPDATE SET schedule_mode=excluded.schedule_mode, schedule_start=excluded.schedule_start, schedule_end=excluded.schedule_end`,
					g, req.SchedulePatch.NodeID, nextOrder, patchMode, patchStart, patchEnd); err != nil {
					a.jsonErr(w, http.StatusInternalServerError, "保存时段策略失败")
					return
				}
			}
		}
		if err := tx.Commit(); err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "保存线路配置失败")
			return
		}
		if a.pm != nil && a.pm.edges != nil {
			_ = a.pm.edges.reload()
		}
		a.jsonOK(w, a.buildISPRoutesPayload())
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
			"ip":              ip,
			"region":          decision.Region,
			"isp":             decision.ISP,
			"isp_group":       decision.ISPGroup,
			"isp_label":       decision.ISPLabel,
			"non_mainland_cn": decision.NonMainlandCN,
			"node_id":         decision.NodeID,
			"node_name":       decision.NodeName,
			"public_url":      decision.PublicURL,
			"blocked":         decision.Blocked,
			"block_reason":    decision.BlockReason,
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
	Version             string                  `json:"version"`
	ListenPort          int                     `json:"listen_port"`
	TLSActive           bool                    `json:"tls_active"`
	ActiveStreams       int                     `json:"active_streams"`
	CurrentBps          int64                   `json:"current_bps"`
	MemoryBytes         uint64                  `json:"memory_bytes"`
	UptimeSec           int64                   `json:"uptime_sec"`
	SupportsSpeedtest   bool                    `json:"supports_speedtest,omitempty"`
	SupportsDirectPopup bool                    `json:"supports_direct_popup,omitempty"`
	UpstreamLatencies   map[int64]int                  `json:"upstream_latencies,omitempty"`
	ISPProbeSeq         uint64                         `json:"isp_probe_seq,omitempty"`
	ISPProbeResults     map[string]agentISPProbeResult `json:"isp_probe_results,omitempty"`
	SpeedtestProgress   *agentSpeedtestProgress        `json:"speedtest_progress,omitempty"`
	UpgradeRequestID    string                         `json:"upgrade_request_id,omitempty"`
	UpgradeStatus       string                         `json:"upgrade_status,omitempty"`
	CertFingerprint     string                         `json:"cert_fingerprint"`
	TrafficReports      []agentTrafficReport           `json:"traffic_reports"`
}

type agentHeartbeatResponse struct {
	OK                bool                 `json:"ok"`
	NodeEnabled       bool                 `json:"node_enabled"`
	BlockedUsers      []int64              `json:"blocked_users"`
	RevokedHashes     []string             `json:"revoked_hashes"`
	UserSpeedLimits   map[int64]int        `json:"user_speed_limits"`
	ProbeTargets      []agentProbeTarget   `json:"probe_targets,omitempty"`
	ISPProbeConfig    *agentISPProbeConfig `json:"isp_probe_config,omitempty"`
	SpeedtestTask     *agentSpeedtestTask  `json:"speedtest_task,omitempty"`
	CancelSpeedtestID string              `json:"cancel_speedtest_id,omitempty"`
	AckSpeedtestID    string              `json:"ack_speedtest_id,omitempty"`
	UpgradeToVersion  string              `json:"upgrade_to_version,omitempty"`
	UpgradeRequestID  string              `json:"upgrade_request_id,omitempty"`
	TLSCertPEM        string              `json:"tls_cert_pem,omitempty"`
	TLSKeyPEM         string              `json:"tls_key_pem,omitempty"`
	CertFingerprint   string              `json:"cert_fingerprint,omitempty"`
}

func (a *App) masterTLSPEMForHost(hostname string) (certPEM, keyPEM, fingerprint string, ok bool) {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" || a == nil {
		return "", "", "", false
	}
	// Prefer dedicated Edge Auto-HTTPS wildcard certificate (works even behind OpenResty/1Panel).
	if a.edgeAutoHTTPS != nil {
		if c, k, fp, found := a.edgeAutoHTTPS.certPEMForHost(hostname); found {
			return c, k, fp, true
		}
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
	var expectedHash, publicHost, tlsMode, nodeName, prevDetectedIP string
	var nodeEnabled bool
	if node != nil {
		expectedHash = node.tokenHash
		publicHost = node.publicHost
		tlsMode = node.tlsMode
		nodeName = node.name
		prevDetectedIP = node.detectedIP
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
		} else if prevDetectedIP != "" {
			detectedIP = prevDetectedIP
		}
	} else {
		detectedIP = ""
	}

	billingMode := trafficBillingModeBidirectional
	if a.db != nil {
		billingMode = a.db.currentSystemSettings().TrafficBillingMode
	}
	var nodeBytesDelta, nodeInDelta, nodeOutDelta int64
	for _, report := range req.TrafficReports {
		if report.BytesIn < 0 || report.BytesOut < 0 {
			continue
		}
		billed := trafficBillableBytes(billingMode, report.BytesIn, report.BytesOut)
		nodeBytesDelta += billed
		nodeInDelta += report.BytesIn
		nodeOutDelta += report.BytesOut
		// Credit site traffic meters if the site is loaded.
		if report.SiteID > 0 {
			a.pm.recordExternalSiteTraffic(report.SiteID, report.BytesIn, report.BytesOut, report.Requests, now)
		}
		// Credit share user traffic and refresh draining playback timestamp.
		if a.pm.shares != nil && report.ShareUserID > 0 {
			a.pm.shares.recordExternalTraffic(report.ShareUserID, report.TokenHash, billed, report.Playing, node.id, nodeName, now)
		}
	}
	if nodeBytesDelta > 0 {
		node.pending.Add(nodeBytesDelta)
	}
	if nodeInDelta > 0 {
		node.pendingTotalIn.Add(nodeInDelta)
	}
	if nodeOutDelta > 0 {
		node.pendingTotalOut.Add(nodeOutDelta)
	}
	var ackSpeedtestID string
	if req.SpeedtestProgress != nil && a.speedtest != nil {
		ackSpeedtestID = a.speedtest.applyAgentProgress(nodeID, req.SpeedtestProgress)
	}

	a.pm.edges.mu.Lock()
	node.lastSeen = now
	if req.SupportsSpeedtest {
		node.supportsSpeedtest = true
	}
	node.supportsDirectPopup = req.SupportsDirectPopup || strings.TrimSpace(r.Header.Get("X-Yundu-Direct-Popup")) == "1"
	if strings.TrimSpace(r.Header.Get("X-Yundu-Egress-Capable")) == "1" || agentVersionSupportsEgress(req.Version) {
		node.supportsEgress = true
	}
	if detectedIP != "" {
		node.detectedIP = detectedIP
	}
	if req.Version != "" && len(req.Version) <= 64 {
		node.agentVersion = req.Version
	}
	node.tlsActive = req.TLSActive
	node.activeStreams = max(0, req.ActiveStreams)
	node.currentBps = max(int64(0), req.CurrentBps)
	node.memoryBytes = req.MemoryBytes
	if req.UptimeSec >= 0 {
		node.uptimeSec = req.UptimeSec
	}
	if len(req.UpstreamLatencies) > 0 && len(req.UpstreamLatencies) <= 128 {
		lat := make(map[int64]int, len(req.UpstreamLatencies))
		for sid, ms := range req.UpstreamLatencies {
			if sid > 0 {
				lat[sid] = ms
			}
		}
		node.upstreamLatencies = lat
	}
	probeAccepted, probeClaim := a.pm.edges.authorizeISPProbeLocked(
		node, r.Header.Get(edgeAgentInstanceHeader), r.Header.Get(edgeAgentProbeClaimHeader), time.Now(),
	)
	if probeAccepted && req.ISPProbeSeq > 0 && len(req.ISPProbeResults) > 0 {
		alerts := a.pm.edges.applyISPProbeReportLocked(node.name, false, node.monitorISPOnly, &node.ispProbeSeq, &node.ispHealth, req.ISPProbeSeq, req.ISPProbeResults, now)
		a.pm.edges.queueISPAlertsLocked(nodeID, alerts)
	}
	ispProbeInterval := a.pm.edges.ispProbe.IntervalSec
	if !node.monitorISPOnly && anyISPTrippedInMap(node.ispHealth) {
		ispProbeInterval = a.pm.edges.ispProbe.RecoveryIntervalSec
	}
	ispProbeCfg := &agentISPProbeConfig{
		Enabled:      a.pm.edges.ispProbe.Enabled,
		IntervalSec:  ispProbeInterval,
		Targets:     cloneISPProbeTargets(a.pm.edges.ispProbe.Targets),
	}
	if probeAccepted {
		ispProbeCfg.ForceProbeID = node.ispForceProbeID
		node.ispForceProbeID = ""
	}
	if node.upgradeTarget != "" {
		if req.Version == node.upgradeTarget {
			node.upgradeTarget = ""
			node.upgradeRequestID = ""
			node.upgradeStatus = ""
		} else if req.UpgradeRequestID != "" && req.UpgradeRequestID == node.upgradeRequestID && req.UpgradeStatus != "" {
			status := strings.TrimSpace(req.UpgradeStatus)
			if len(status) > 200 {
				status = status[:200]
			}
			node.upgradeStatus = status
			if strings.HasPrefix(status, "failed:") {
				node.upgradeTarget = ""
				node.upgradeRequestID = ""
			}
		}
	}
	upgradeVersion := node.upgradeTarget
	upgradeReqID := node.upgradeRequestID
	needAutoDNSSync := false
	if node.autoHTTPS && node.publicHost != "" {
		effectiveIP := strings.TrimSpace(node.publicIP)
		if effectiveIP == "" {
			effectiveIP = strings.TrimSpace(node.detectedIP)
		}
		if effectiveIP != "" && (effectiveIP != node.cfRecordIP || node.cfRecordID == "" || strings.HasPrefix(node.cfDNSStatus, "error:")) {
			needAutoDNSSync = true
		}
	}
	a.pm.edges.mu.Unlock()

	if needAutoDNSSync && a.edgeAutoHTTPS != nil {
		a.edgeAutoHTTPS.maybeSyncNodeDNSAsync(nodeID, false)
	}

	var probeTargets []agentProbeTarget
	a.pm.mu.RLock()
	for sid, inst := range a.pm.proxies {
		if inst != nil && inst.Site.Enabled && strings.TrimSpace(inst.Site.TargetURL) != "" {
			probeTargets = append(probeTargets, agentProbeTarget{SiteID: sid, URL: inst.Site.TargetURL})
		}
	}
	a.pm.mu.RUnlock()
	sort.Slice(probeTargets, func(i, j int) bool {
		return probeTargets[i].SiteID < probeTargets[j].SiteID
	})

	resp := agentHeartbeatResponse{
		OK:               true,
		NodeEnabled:      nodeEnabled,
		BlockedUsers:     []int64{},
		RevokedHashes:    []string{},
		UserSpeedLimits:  map[int64]int{},
		ProbeTargets:     probeTargets,
		ISPProbeConfig:   ispProbeCfg,
		UpgradeToVersion: upgradeVersion,
		UpgradeRequestID: upgradeReqID,
	}
	if a.pm.shares != nil {
		resp.BlockedUsers, resp.RevokedHashes, resp.UserSpeedLimits = a.pm.shares.agentSyncSnapshot(now)
	}
	if a.speedtest != nil {
		resp.SpeedtestTask, resp.CancelSpeedtestID = a.speedtest.nextAgentDirective(nodeID)
		resp.AckSpeedtestID = ackSpeedtestID
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
	if probeClaim != "" {
		w.Header().Set(edgeAgentProbeClaimHeader, probeClaim)
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
func (rt *shareUserRuntime) recordExternalTraffic(userID int64, tokenHash string, billed int64, playing bool, nodeID int64, nodeName string, now time.Time) {
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
		if nodeID > 0 {
			act.lastNodeID = nodeID
			act.hasNode = true
		}
		if strings.TrimSpace(nodeName) != "" {
			act.lastNodeName = strings.TrimSpace(nodeName)
		}
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
