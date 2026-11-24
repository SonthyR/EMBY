package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	appleSpeedtestUploadURL   = "https://mensura.cdn-apple.com/api/v1/gm/slurp"
	appleSpeedtestDownloadURL = "https://mensura.cdn-apple.com/api/v1/gm/large"
	appleSpeedtestHost        = "mensura.cdn-apple.com"
	appleSpeedtestUserAgent   = "networkQuality/194.80.3 CFNetwork/3860.400.51 Darwin/25.3.0"

	edgeSpeedtestPathPrefix       = "/_yundu/st/"
	edgeSpeedtestTicketTTL        = 5 * time.Minute
	defaultSpeedtestPhaseDuration = 4 * time.Second
	defaultDirectStreamBytes      = int64(32 << 20)
	maxDirectStreamBytes          = int64(128 << 20)
	minDirectStreamBytes          = int64(1 << 20)
	speedtestTransferMaxBytes     = int64(512 << 20)
	maxCustomSpeedtestTargets     = 50
	maxSpeedtestTargetsPerJob     = 40
	maxAgentSpeedtestTaskBytes    = 512 << 10
	maxSpeedtestHistoryJobs       = 50
	edgeSpeedtestIdleTimeout      = 45 * time.Second
	edgeSpeedtestHardMaxAge       = 10 * time.Minute
	edgeSpeedtestCancelTTL        = 60 * time.Second
	maxSpeedtestURLBytes          = 512
	maxSpeedtestProvinceRunes     = 40
)

var nonPublicSpeedtestPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fec0::/10"),
}

const speedtestSchemaSQL = `
CREATE TABLE IF NOT EXISTS speedtest_custom_targets (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	isp_group TEXT NOT NULL DEFAULT 'other',
	province TEXT NOT NULL DEFAULT '',
	upload_url TEXT NOT NULL DEFAULT '',
	download_url TEXT NOT NULL DEFAULT '',
	method TEXT NOT NULL DEFAULT 'POST',
	enabled INTEGER NOT NULL DEFAULT 1,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS speedtest_jobs (
	id TEXT PRIMARY KEY,
	node_id INTEGER NOT NULL DEFAULT 0,
	node_name TEXT NOT NULL DEFAULT '',
	mode TEXT NOT NULL DEFAULT 'builtin',
	provinces_json TEXT NOT NULL DEFAULT '[]',
	isps_json TEXT NOT NULL DEFAULT '[]',
	status TEXT NOT NULL DEFAULT 'pending',
	total_targets INTEGER NOT NULL DEFAULT 0,
	completed_targets INTEGER NOT NULL DEFAULT 0,
	results_json TEXT NOT NULL DEFAULT '[]',
	error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_speedtest_jobs_created ON speedtest_jobs(created_at DESC);
CREATE TABLE IF NOT EXISTS edge_node_speed_metrics (
	node_id INTEGER NOT NULL,
	isp_group TEXT NOT NULL,
	avg_return_bps INTEGER NOT NULL DEFAULT 0,
	avg_outbound_bps INTEGER NOT NULL DEFAULT 0,
	avg_return_rtt_ms INTEGER NOT NULL DEFAULT 0,
	avg_outbound_rtt_ms INTEGER NOT NULL DEFAULT 0,
	total_retrans INTEGER NOT NULL DEFAULT -1,
	sample_count INTEGER NOT NULL DEFAULT 0,
	tested_at TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(node_id, isp_group)
);
`

func ensureSpeedtestSchema(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, speedtestSchemaSQL)
	return err
}

// speedtestPatternChunk is an immutable 64 KiB pseudo-random byte pattern reused
// for zero-disk-I/O upload and direct-download speedtesting.
var speedtestPatternChunk = func() []byte {
	buf := make([]byte, 64<<10)
	var x uint64 = 0x9e3779b97f4a7c15
	for i := 0; i < len(buf); i += 8 {
		x ^= x >> 12
		x ^= x << 25
		x ^= x >> 27
		v := x * 0x2545f4914f6cdd1d
		buf[i] = byte(v)
		buf[i+1] = byte(v >> 8)
		buf[i+2] = byte(v >> 16)
		buf[i+3] = byte(v >> 24)
		buf[i+4] = byte(v >> 32)
		buf[i+5] = byte(v >> 40)
		buf[i+6] = byte(v >> 48)
		buf[i+7] = byte(v >> 56)
	}
	return buf
}()

type speedtestProvinceDef struct {
	Code     string              `json:"code"`
	Label    string              `json:"label"`
	ECSCIDRs map[string][]string `json:"-"`
}

var speedtestProvinceCatalog = []speedtestProvinceDef{
	{Code: "bj", Label: "北京", ECSCIDRs: map[string][]string{
		"ct": {"124.238.121.0/24", "219.141.136.0/24"},
		"cu": {"202.106.0.0/24", "123.125.99.0/24"},
		"cm": {"221.130.33.0/24", "111.13.100.0/24"},
	}},
	{Code: "sh", Label: "上海", ECSCIDRs: map[string][]string{
		"ct": {"202.96.209.0/24", "101.88.0.0/24"},
		"cu": {"210.22.70.0/24", "112.64.0.0/24"},
		"cm": {"211.136.112.0/24", "117.131.0.0/24"},
	}},
	{Code: "gd", Label: "广东", ECSCIDRs: map[string][]string{
		"ct": {"202.96.128.0/24", "125.89.170.0/24"},
		"cu": {"210.21.196.0/24", "221.4.66.0/24"},
		"cm": {"211.136.192.0/24", "120.241.224.0/24"},
	}},
	{Code: "js", Label: "江苏", ECSCIDRs: map[string][]string{
		"ct": {"218.2.135.0/24"},
		"cu": {"221.6.4.0/24"},
		"cm": {"221.131.143.0/24"},
	}},
	{Code: "zj", Label: "浙江", ECSCIDRs: map[string][]string{
		"ct": {"202.101.172.0/24"},
		"cu": {"221.12.1.0/24"},
		"cm": {"211.140.13.0/24"},
	}},
	{Code: "sc", Label: "四川", ECSCIDRs: map[string][]string{
		"ct": {"61.139.2.0/24"},
		"cu": {"119.6.6.0/24"},
		"cm": {"211.137.96.0/24"},
	}},
	{Code: "hb", Label: "湖北", ECSCIDRs: map[string][]string{
		"ct": {"202.103.24.0/24"},
		"cu": {"218.104.111.0/24"},
		"cm": {"211.137.58.0/24"},
	}},
	{Code: "fj", Label: "福建", ECSCIDRs: map[string][]string{
		"ct": {"218.85.157.0/24"},
		"cu": {"58.22.96.0/24"},
		"cm": {"211.143.144.0/24"},
	}},
	{Code: "sd", Label: "山东", ECSCIDRs: map[string][]string{
		"ct": {"219.146.1.0/24"},
		"cu": {"202.102.128.0/24"},
		"cm": {"211.137.191.0/24"},
	}},
	{Code: "sn", Label: "陕西", ECSCIDRs: map[string][]string{
		"ct": {"218.30.19.0/24"},
		"cu": {"221.11.1.0/24"},
		"cm": {"211.137.130.0/24"},
	}},
	{Code: "ln", Label: "辽宁", ECSCIDRs: map[string][]string{
		"ct": {"219.148.204.0/24"},
		"cu": {"202.96.64.0/24"},
		"cm": {"211.137.47.0/24"},
	}},
	{Code: "cq", Label: "重庆", ECSCIDRs: map[string][]string{
		"ct": {"61.128.128.0/24"},
		"cu": {"221.5.203.0/24"},
		"cm": {"211.139.5.0/24"},
	}},
}

var defaultSpeedtestProvinces = []string{"bj", "sh", "gd"}
var defaultSpeedtestISPs = []string{"ct", "cu", "cm"}

func lookupSpeedtestProvince(codeOrLabel string) (speedtestProvinceDef, bool) {
	needle := strings.ToLower(strings.TrimSpace(codeOrLabel))
	for _, p := range speedtestProvinceCatalog {
		if strings.ToLower(p.Code) == needle || strings.ToLower(p.Label) == needle {
			return p, true
		}
	}
	return speedtestProvinceDef{}, false
}

// ---------------------------------------------------------------------------
// DTOs shared by Panel API and Edge Agent Heartbeat

type agentSpeedtestTarget struct {
	TargetKey   string `json:"target_key"`
	Label       string `json:"label"`
	Province    string `json:"province"`
	ISPGroup    string `json:"isp_group"`
	ISPLabel    string `json:"isp_label"`
	TargetKind  string `json:"target_kind"` // builtin | custom
	ResolvedIP  string `json:"resolved_ip,omitempty"`
	UploadURL   string `json:"upload_url,omitempty"`
	DownloadURL string `json:"download_url,omitempty"`
	Method      string `json:"method,omitempty"`
	DurationMs  int    `json:"duration_ms,omitempty"`
}

type agentSpeedtestTask struct {
	TaskID     string                 `json:"task_id"`
	DurationMs int                    `json:"duration_ms,omitempty"`
	Targets    []agentSpeedtestTarget `json:"targets"`
}

type speedtestTargetResult struct {
	TargetKey     string  `json:"target_key"`
	Label         string  `json:"label"`
	Province      string  `json:"province"`
	ISPGroup      string  `json:"isp_group"`
	ISPLabel      string  `json:"isp_label"`
	TargetKind    string  `json:"target_kind"`
	ResolvedIP    string  `json:"resolved_ip,omitempty"`
	Status        string  `json:"status"` // pending | running | done | error
	Error         string  `json:"error,omitempty"`
	ReturnMbps    float64 `json:"return_mbps"`
	ReturnBps     int64   `json:"return_bps"`
	OutboundMbps  float64 `json:"outbound_mbps"`
	OutboundBps   int64   `json:"outbound_bps"`
	ReturnRetrans int     `json:"return_retrans"`  // -1 when unavailable
	ReturnRTTMs   int     `json:"return_rtt_ms"`   // -1 when unavailable
	OutboundRTTMs int     `json:"outbound_rtt_ms"` // -1 when unavailable
	TestedAt      string  `json:"tested_at,omitempty"`
}

type agentSpeedtestProgress struct {
	TaskID           string                  `json:"task_id"`
	Status           string                  `json:"status"` // running | done | failed | canceled
	CompletedTargets int                     `json:"completed_targets"`
	TotalTargets     int                     `json:"total_targets"`
	Error            string                  `json:"error,omitempty"`
	Results          []speedtestTargetResult `json:"results"`
}

type edgeNodeSpeedMetric struct {
	ISPGroup         string  `json:"isp_group"`
	ISPLabel         string  `json:"isp_label"`
	AvgReturnBps     int64   `json:"avg_return_bps"`
	AvgReturnMbps    float64 `json:"avg_return_mbps"`
	AvgOutboundBps   int64   `json:"avg_outbound_bps"`
	AvgOutboundMbps  float64 `json:"avg_outbound_mbps"`
	AvgReturnRTTMs   int     `json:"avg_return_rtt_ms"`
	AvgOutboundRTTMs int     `json:"avg_outbound_rtt_ms"`
	TotalRetrans     int     `json:"total_retrans"`
	SampleCount      int     `json:"sample_count"`
	TestedAt         string  `json:"tested_at"`
}

type speedtestCustomTarget struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	ISPGroup    string `json:"isp_group"`
	ISPLabel    string `json:"isp_label"`
	Province    string `json:"province"`
	UploadURL   string `json:"upload_url"`
	DownloadURL string `json:"download_url"`
	Method      string `json:"method"`
	Enabled     bool   `json:"enabled"`
	CreatedAt   string `json:"created_at"`
}

type speedtestJobView struct {
	ID               string                  `json:"id"`
	NodeID           int64                   `json:"node_id"`
	NodeName         string                  `json:"node_name"`
	Mode             string                  `json:"mode"`
	Provinces        []string                `json:"provinces"`
	ISPs             []string                `json:"isps"`
	Status           string                  `json:"status"`
	TotalTargets     int                     `json:"total_targets"`
	CompletedTargets int                     `json:"completed_targets"`
	Results          []speedtestTargetResult `json:"results"`
	Error            string                  `json:"error,omitempty"`
	CreatedAt        string                  `json:"created_at"`
	UpdatedAt        string                  `json:"updated_at"`
}

// ---------------------------------------------------------------------------
// Edge Agent direct browser speedtest sealed ticket

type edgeSpeedtestTicket struct {
	Version   int    `json:"v"`
	Kind      string `json:"k"`
	JTI       string `json:"jti"`
	NodeID    int64  `json:"n"`
	MaxBytes  int64  `json:"mb"`
	ExpiresAt int64  `json:"exp"`
}

func edgeSpeedtestKeyFromToken(token string) []byte {
	sum := sha256.Sum256(append([]byte("yundu-edge-speedtest:v1:"), []byte(token)...))
	return sum[:]
}

func newSpeedtestRandomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sealEdgeSpeedtestTicket(token string, nodeID int64, maxBytes int64) (string, error) {
	if token == "" || nodeID <= 0 {
		return "", errors.New("edge node credentials unavailable")
	}
	if maxBytes <= 0 || maxBytes > maxDirectStreamBytes {
		maxBytes = maxDirectStreamBytes
	}
	ticket := edgeSpeedtestTicket{
		Version:   1,
		Kind:      "speedtest_direct",
		JTI:       newSpeedtestRandomHex(8),
		NodeID:    nodeID,
		MaxBytes:  maxBytes,
		ExpiresAt: time.Now().Add(edgeSpeedtestTicketTTL).Unix(),
	}
	plain, err := json.Marshal(ticket)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(edgeSpeedtestKeyFromToken(token))
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

func openEdgeSpeedtestTicket(token, encoded string) (edgeSpeedtestTicket, error) {
	var ticket edgeSpeedtestTicket
	if token == "" || encoded == "" || len(encoded) > 4096 {
		return ticket, errors.New("invalid speedtest ticket")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return ticket, errors.New("malformed speedtest ticket")
	}
	block, err := aes.NewCipher(edgeSpeedtestKeyFromToken(token))
	if err != nil {
		return ticket, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return ticket, err
	}
	if len(raw) < gcm.NonceSize()+gcm.Overhead() {
		return ticket, errors.New("truncated speedtest ticket")
	}
	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return ticket, errors.New("invalid speedtest ticket signature")
	}
	if err := json.Unmarshal(plain, &ticket); err != nil {
		return ticket, errors.New("corrupt speedtest ticket")
	}
	if ticket.Version != 1 || ticket.Kind != "speedtest_direct" || ticket.NodeID <= 0 || ticket.JTI == "" {
		return ticket, errors.New("unsupported speedtest ticket")
	}
	if ticket.ExpiresAt <= 0 || time.Now().Unix() > ticket.ExpiresAt {
		return ticket, errors.New("speedtest ticket expired")
	}
	return ticket, nil
}

// ---------------------------------------------------------------------------
// IP Validation & Pure-Go HTTP/1.1 single-connection speedtest runner

func isPublicSpeedtestIP(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsLoopback() || addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	for _, prefix := range nonPublicSpeedtestPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

func isPublicUnicastIPv4(raw string) bool {
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	return addr.Is4() && isPublicSpeedtestIP(addr)
}

type speedtestTrackedConn struct {
	net.Conn
	dialRTTMs int

	mu       sync.Mutex
	captured bool
	closed   bool
	retrans  int
	rttMs    int
	hasInfo  bool
}

func newSpeedtestTrackedConn(conn net.Conn, dialRTTMs int) *speedtestTrackedConn {
	return &speedtestTrackedConn{
		Conn:      conn,
		dialRTTMs: dialRTTMs,
		retrans:   -1,
		rttMs:     -1,
	}
}

func (c *speedtestTrackedConn) captureLocked() {
	if c.captured || c.closed {
		return
	}
	retrans, rttMs, ok := readTCPConnStats(c.Conn)
	c.captured = true
	if ok {
		c.retrans = retrans
		c.rttMs = rttMs
		c.hasInfo = true
	}
}

func (c *speedtestTrackedConn) capture() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.captureLocked()
	c.mu.Unlock()
}

func (c *speedtestTrackedConn) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	c.captureLocked()
	alreadyClosed := c.closed
	c.closed = true
	c.mu.Unlock()
	if alreadyClosed {
		return nil
	}
	return c.Conn.Close()
}

func (c *speedtestTrackedConn) stats() (retrans int, rttMs int, hasTCPInfo bool) {
	if c == nil {
		return -1, -1, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.retrans, c.rttMs, c.hasInfo
}

func newTimedUploadReaderExact(duration time.Duration, maxBytes int64) *timedUploadStream {
	warmup := duration / 4
	if warmup > time.Second {
		warmup = time.Second
	}
	if duration < 1500*time.Millisecond {
		warmup = 0
	}
	return &timedUploadStream{
		duration:  duration,
		warmupDur: warmup,
		maxBytes:  maxBytes,
	}
}

type timedUploadStream struct {
	mu         sync.Mutex
	duration   time.Duration
	warmupDur  time.Duration
	maxBytes   int64
	started    bool
	startAt    time.Time
	lastAt     time.Time
	deadline   time.Time
	warmed     bool
	warmupAt   time.Time
	warmupByte int64
	totalBytes int64
}

func (s *timedUploadStream) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if !s.started {
		s.started = true
		s.startAt = now
		s.deadline = now.Add(s.duration)
	}
	if now.After(s.deadline) || s.totalBytes >= s.maxBytes {
		s.lastAt = now
		return 0, io.EOF
	}
	if s.warmupDur > 0 && !s.warmed && now.Sub(s.startAt) >= s.warmupDur {
		s.warmed = true
		s.warmupAt = now
		s.warmupByte = s.totalBytes
	}
	remain := s.maxBytes - s.totalBytes
	n := len(p)
	if int64(n) > remain {
		n = int(remain)
	}
	copied := 0
	for copied < n {
		chunk := copy(p[copied:n], speedtestPatternChunk)
		copied += chunk
	}
	s.totalBytes += int64(copied)
	s.lastAt = now
	return copied, nil
}

func (s *timedUploadStream) computeBps(httpDoneAt time.Time) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.totalBytes <= 0 {
		return 0
	}
	endAt := httpDoneAt
	if !s.lastAt.IsZero() && s.lastAt.After(s.startAt) && endAt.Sub(s.lastAt) > 2*time.Second {
		endAt = s.lastAt
	}
	if s.warmed && endAt.Sub(s.warmupAt) >= 350*time.Millisecond && s.totalBytes > s.warmupByte {
		elapsed := endAt.Sub(s.warmupAt).Seconds()
		return int64(float64(s.totalBytes-s.warmupByte) / elapsed)
	}
	elapsed := endAt.Sub(s.startAt).Seconds()
	if elapsed < 0.01 {
		elapsed = 0.01
	}
	return int64(float64(s.totalBytes) / elapsed)
}

func newSpeedtestSingleConnHTTPClient(resolvedIP, targetHost string, totalTimeout time.Duration, transportOverride *http.Transport) (*http.Client, func() *speedtestTrackedConn) {
	var connMu sync.Mutex
	var lastConn *speedtestTrackedConn

	dialer := &net.Dialer{
		Timeout:   8 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	resolvedAddr, _ := netip.ParseAddr(strings.TrimSpace(resolvedIP))

	var transport *http.Transport
	if transportOverride != nil {
		transport = transportOverride.Clone()
	} else {
		transport = &http.Transport{
			ForceAttemptHTTP2:     false,
			DisableKeepAlives:     true,
			TLSHandshakeTimeout:   8 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
			DialContext: func(dctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					host = addr
					port = "443"
				}
				var targetAddr netip.Addr
				if resolvedAddr.IsValid() && (targetHost == "" || strings.EqualFold(host, targetHost)) {
					if !isPublicSpeedtestIP(resolvedAddr) {
						return nil, errors.New("拒绝连接非公网 ECS 目标 IP")
					}
					targetAddr = resolvedAddr.Unmap()
				} else if lit, parseErr := netip.ParseAddr(host); parseErr == nil {
					if !isPublicSpeedtestIP(lit) {
						return nil, errors.New("拒绝连接内网或保留 IP 目标")
					}
					targetAddr = lit.Unmap()
				} else {
					ips, lookupErr := net.DefaultResolver.LookupNetIP(dctx, "ip", host)
					if lookupErr != nil || len(ips) == 0 {
						return nil, errors.New("目标域名解析失败")
					}
					for _, candidate := range ips {
						if isPublicSpeedtestIP(candidate) {
							targetAddr = candidate.Unmap()
							break
						}
					}
					if !targetAddr.IsValid() {
						return nil, errors.New("目标域名解析到非公网 IP，已阻止连接")
					}
				}
				dialNet := "tcp4"
				if targetAddr.Is6() {
					dialNet = "tcp6"
				}
				start := time.Now()
				conn, err := dialer.DialContext(dctx, dialNet, net.JoinHostPort(targetAddr.String(), port))
				if err != nil {
					return nil, err
				}
				rttMs := int(math.Round(float64(time.Since(start).Microseconds()) / 1000.0))
				if rttMs <= 0 {
					rttMs = 1
				}
				tracked := newSpeedtestTrackedConn(conn, rttMs)
				connMu.Lock()
				lastConn = tracked
				connMu.Unlock()
				return tracked, nil
			},
		}
	}
	transport.ForceAttemptHTTP2 = false
	transport.DisableKeepAlives = true
	if transport.DialContext != nil && transportOverride != nil {
		origDial := transport.DialContext
		transport.DialContext = func(dctx context.Context, network, addr string) (net.Conn, error) {
			start := time.Now()
			conn, err := origDial(dctx, network, addr)
			if err != nil {
				return nil, err
			}
			rttMs := int(math.Round(float64(time.Since(start).Microseconds()) / 1000.0))
			if rttMs <= 0 {
				rttMs = 1
			}
			tracked := newSpeedtestTrackedConn(conn, rttMs)
			connMu.Lock()
			lastConn = tracked
			connMu.Unlock()
			return tracked, nil
		}
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   totalTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("测速请求禁止跟随重定向")
		},
	}
	getter := func() *speedtestTrackedConn {
		connMu.Lock()
		defer connMu.Unlock()
		return lastConn
	}
	return client, getter
}

func executeSpeedtestTarget(ctx context.Context, target agentSpeedtestTarget, defaultDur time.Duration, transportOverride *http.Transport) speedtestTargetResult {
	dur := defaultDur
	if target.DurationMs > 0 {
		dur = time.Duration(target.DurationMs) * time.Millisecond
	}
	if dur < 150*time.Millisecond {
		dur = 150 * time.Millisecond
	}
	if dur > 15*time.Second {
		dur = 15 * time.Second
	}

	res := speedtestTargetResult{
		TargetKey:     target.TargetKey,
		Label:         target.Label,
		Province:      target.Province,
		ISPGroup:      target.ISPGroup,
		ISPLabel:      target.ISPLabel,
		TargetKind:    target.TargetKind,
		ResolvedIP:    target.ResolvedIP,
		Status:        "done",
		ReturnRetrans: -1,
		ReturnRTTMs:   -1,
		OutboundRTTMs: -1,
		TestedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	if res.TargetKind == "builtin" && strings.TrimSpace(res.ResolvedIP) == "" {
		res.Status = "error"
		res.Error = "ECS DNS 解析失败"
		return res
	}

	var uploadErr error
	if strings.TrimSpace(target.UploadURL) != "" {
		bps, retrans, rttMs, err := measureSingleUpload(ctx, target, dur, transportOverride)
		res.ReturnBps = bps
		res.ReturnMbps = roundMbps(bps)
		res.ReturnRetrans = retrans
		res.ReturnRTTMs = rttMs
		uploadErr = err
	}

	var downloadErr error
	if strings.TrimSpace(target.DownloadURL) != "" {
		bps, rttMs, err := measureSingleDownload(ctx, target, dur, transportOverride)
		res.OutboundBps = bps
		res.OutboundMbps = roundMbps(bps)
		res.OutboundRTTMs = rttMs
		downloadErr = err
	}

	if uploadErr != nil || downloadErr != nil {
		if uploadErr != nil && downloadErr != nil {
			res.Status = "error"
			res.Error = uploadErr.Error()
		} else if uploadErr != nil && strings.TrimSpace(target.DownloadURL) == "" {
			res.Status = "error"
			res.Error = uploadErr.Error()
		} else if downloadErr != nil && strings.TrimSpace(target.UploadURL) == "" {
			res.Status = "error"
			res.Error = downloadErr.Error()
		} else if uploadErr != nil {
			res.Status = "error"
			res.Error = "回程失败: " + uploadErr.Error()
		} else {
			res.Status = "error"
			res.Error = "去程失败: " + downloadErr.Error()
		}
		return res
	}
	res.TestedAt = time.Now().UTC().Format(time.RFC3339)
	return res
}

func roundMbps(bps int64) float64 {
	if bps <= 0 {
		return 0
	}
	mbps := float64(bps) * 8.0 / 1_000_000.0
	return math.Round(mbps*10.0) / 10.0
}

func targetHostnameForURL(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func measureSingleUpload(ctx context.Context, target agentSpeedtestTarget, dur time.Duration, transportOverride *http.Transport) (bps int64, retrans int, rttMs int, err error) {
	retrans = -1
	rttMs = -1
	reqCtx, cancel := context.WithTimeout(ctx, dur+10*time.Second)
	defer cancel()

	method := strings.ToUpper(strings.TrimSpace(target.Method))
	if method != http.MethodPut {
		method = http.MethodPost
	}
	bodyStream := newTimedUploadReaderExact(dur, speedtestTransferMaxBytes)
	req, err := http.NewRequestWithContext(reqCtx, method, target.UploadURL, bodyStream)
	if err != nil {
		return 0, -1, -1, err
	}
	req.ContentLength = -1
	req.Header.Set("User-Agent", appleSpeedtestUserAgent)
	req.Header.Set("Content-Type", "application/octet-stream")

	targetHost := targetHostnameForURL(target.UploadURL)
	client, getConn := newSpeedtestSingleConnHTTPClient(target.ResolvedIP, targetHost, dur+10*time.Second, transportOverride)
	resp, doErr := client.Do(req)
	endAt := time.Now()

	if resp != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		if tc := getConn(); tc != nil {
			tc.capture()
		}
		_ = resp.Body.Close()
	}
	if tc := getConn(); tc != nil {
		tc.capture()
		rRetrans, rRTT, hasInfo := tc.stats()
		if hasInfo {
			retrans = rRetrans
			rttMs = rRTT
		} else {
			rttMs = tc.dialRTTMs
		}
	}

	if doErr != nil {
		return 0, retrans, rttMs, doErr
	}
	if resp == nil || resp.StatusCode >= 400 {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		return 0, retrans, rttMs, fmt.Errorf("HTTP %d", code)
	}
	bps = bodyStream.computeBps(endAt)
	if bps <= 0 {
		return 0, retrans, rttMs, errors.New("上传零字节")
	}
	return bps, retrans, rttMs, nil
}

func measureSingleDownload(ctx context.Context, target agentSpeedtestTarget, dur time.Duration, transportOverride *http.Transport) (bps int64, rttMs int, err error) {
	rttMs = -1
	reqCtx, cancel := context.WithTimeout(ctx, dur+10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target.DownloadURL, nil)
	if err != nil {
		return 0, -1, err
	}
	req.Header.Set("User-Agent", appleSpeedtestUserAgent)
	req.Header.Set("Cache-Control", "no-store")

	targetHost := targetHostnameForURL(target.DownloadURL)
	client, getConn := newSpeedtestSingleConnHTTPClient(target.ResolvedIP, targetHost, dur+10*time.Second, transportOverride)
	resp, doErr := client.Do(req)
	if doErr != nil {
		return 0, -1, doErr
	}
	if resp.StatusCode >= 400 {
		if tc := getConn(); tc != nil {
			tc.capture()
			_, infoRTT, hasInfo := tc.stats()
			if hasInfo {
				rttMs = infoRTT
			} else {
				rttMs = tc.dialRTTMs
			}
		}
		_ = resp.Body.Close()
		return 0, rttMs, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	buf := make([]byte, 64<<10)
	warmupDur := dur / 4
	if warmupDur > time.Second {
		warmupDur = time.Second
	}
	if dur < 1500*time.Millisecond {
		warmupDur = 0
	}

	var totalRead int64
	var firstAt, warmupAt, deadline time.Time
	var warmupBytes int64
	var warmed bool
	var transferErr error

	for totalRead < speedtestTransferMaxBytes {
		n, readErr := resp.Body.Read(buf)
		now := time.Now()
		if n > 0 {
			if firstAt.IsZero() {
				firstAt = now
				deadline = now.Add(dur)
			}
			totalRead += int64(n)
			if warmupDur > 0 && !warmed && now.Sub(firstAt) >= warmupDur {
				warmed = true
				warmupAt = now
				warmupBytes = totalRead
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			transferErr = readErr
			break
		}
		if !deadline.IsZero() && now.After(deadline) {
			break
		}
	}
	endAt := time.Now()
	if tc := getConn(); tc != nil {
		tc.capture()
		_, infoRTT, hasInfo := tc.stats()
		if hasInfo {
			rttMs = infoRTT
		} else {
			rttMs = tc.dialRTTMs
		}
	}
	_ = resp.Body.Close()

	if transferErr != nil {
		return 0, rttMs, transferErr
	}
	if firstAt.IsZero() || totalRead == 0 {
		return 0, rttMs, errors.New("空响应")
	}
	if warmed && endAt.Sub(warmupAt) >= 350*time.Millisecond && totalRead > warmupBytes {
		bps = int64(float64(totalRead-warmupBytes) / endAt.Sub(warmupAt).Seconds())
	} else {
		elapsed := endAt.Sub(firstAt).Seconds()
		if elapsed < 0.01 {
			elapsed = 0.01
		}
		bps = int64(float64(totalRead) / elapsed)
	}
	return bps, rttMs, nil
}

// ---------------------------------------------------------------------------
// DoH EDNS Client Subnet (ECS) resolver for Apple CDN edge IPs

type speedtestECSCacheEntry struct {
	ip        string
	expiresAt time.Time
}

type dohJSONResponse struct {
	Answer []struct {
		Type int    `json:"type"`
		Data string `json:"data"`
		TTL  int    `json:"TTL"`
	} `json:"Answer"`
}

// ---------------------------------------------------------------------------
// Master Speedtest Manager

type speedtestManager struct {
	db              *DB
	pm              *ProxyManager
	dohClient       *http.Client
	dohEndpoints    []string
	targetTransport *http.Transport
	phaseDuration   time.Duration
	customTargetMu  sync.Mutex

	mu                sync.Mutex
	ecsCache          map[string]speedtestECSCacheEntry
	activeNode        map[int64]string // node_id -> job_id or "preparing:..."
	masterCanc        map[string]context.CancelFunc
	masterDone        map[string]chan struct{}
	agentTasks        map[int64]*agentSpeedtestTask // pending/running task per edge node
	agentTaskStarted  map[int64]time.Time
	agentTaskLastProg map[int64]time.Time
	agentCancel       map[int64]string // node_id -> canceled task_id awaiting ack/expiry
	agentCancelExpiry map[int64]time.Time
	lastCleaned       time.Time
}

func newSpeedtestManager(db *DB, pm *ProxyManager) *speedtestManager {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = secureTLSConfig("")
	mgr := &speedtestManager{
		db: db,
		pm: pm,
		dohClient: &http.Client{
			Transport: tr,
			Timeout:   6 * time.Second,
		},
		dohEndpoints: []string{
			"https://dns.alidns.com/resolve",
			"https://dns.google/resolve",
		},
		phaseDuration:     defaultSpeedtestPhaseDuration,
		ecsCache:          make(map[string]speedtestECSCacheEntry),
		activeNode:        make(map[int64]string),
		masterCanc:        make(map[string]context.CancelFunc),
		masterDone:        make(map[string]chan struct{}),
		agentTasks:        make(map[int64]*agentSpeedtestTask),
		agentTaskStarted:  make(map[int64]time.Time),
		agentTaskLastProg: make(map[int64]time.Time),
		agentCancel:       make(map[int64]string),
		agentCancelExpiry: make(map[int64]time.Time),
	}
	if db != nil && db.db != nil {
		nowStr := time.Now().UTC().Format(time.RFC3339)
		_, _ = db.db.Exec(
			`UPDATE speedtest_jobs SET status='failed', error='主机重启，测速已中断', updated_at=? WHERE status IN ('pending','running')`,
			nowStr,
		)
	}
	return mgr
}

func newSpeedtestJobID() string {
	return "st_" + newSpeedtestRandomHex(8)
}

func (m *speedtestManager) resolveECSIp(ctx context.Context, cacheKey string, cidrs []string) string {
	now := time.Now()
	m.mu.Lock()
	if cached, ok := m.ecsCache[cacheKey]; ok && now.Before(cached.expiresAt) {
		m.mu.Unlock()
		return cached.ip
	}
	client := m.dohClient
	endpoints := append([]string(nil), m.dohEndpoints...)
	m.mu.Unlock()

	for _, cidr := range cidrs {
		for _, base := range endpoints {
			ip, ttl := queryDoHECS(ctx, client, base, appleSpeedtestHost, cidr)
			if ip != "" {
				if ttl < 30 {
					ttl = 30
				}
				if ttl > 300 {
					ttl = 300
				}
				m.mu.Lock()
				m.ecsCache[cacheKey] = speedtestECSCacheEntry{
					ip:        ip,
					expiresAt: time.Now().Add(time.Duration(ttl) * time.Second),
				}
				m.mu.Unlock()
				return ip
			}
		}
	}
	return ""
}

func queryDoHECS(ctx context.Context, client *http.Client, endpointBase, domain, subnet string) (string, int) {
	if client == nil || endpointBase == "" || subnet == "" {
		return "", 0
	}
	u, err := url.Parse(endpointBase)
	if err != nil {
		return "", 0
	}
	q := u.Query()
	q.Set("name", domain)
	q.Set("type", "A")
	q.Set("edns_client_subnet", subnet)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", 0
	}
	req.Header.Set("Accept", "application/dns-json, application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0
	}
	var parsed dohJSONResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&parsed); err != nil {
		return "", 0
	}
	for _, ans := range parsed.Answer {
		if ans.Type == 1 && isPublicUnicastIPv4(ans.Data) {
			return strings.TrimSpace(ans.Data), ans.TTL
		}
	}
	return "", 0
}

func (m *speedtestManager) listCustomTargets() ([]speedtestCustomTarget, error) {
	rows, err := m.db.db.Query(`SELECT id, name, isp_group, province, upload_url, download_url, method, enabled, created_at FROM speedtest_custom_targets ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]speedtestCustomTarget, 0)
	for rows.Next() {
		var t speedtestCustomTarget
		var enabled int
		if err := rows.Scan(&t.ID, &t.Name, &t.ISPGroup, &t.Province, &t.UploadURL, &t.DownloadURL, &t.Method, &enabled, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.Enabled = enabled != 0
		t.ISPLabel = ispGroupLabel(t.ISPGroup)
		out = append(out, t)
	}
	return out, rows.Err()
}

func validateCustomSpeedtestTargetURL(raw string, allowPrivate bool) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	if len(trimmed) > maxSpeedtestURLBytes {
		return "", fmt.Errorf("目标地址长度不能超过 %d 字符", maxSpeedtestURLBytes)
	}
	if strings.ContainsAny(trimmed, "\r\n\x00") {
		return "", errors.New("目标地址包含非法字符")
	}
	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", errors.New("目标地址必须是有效的 http/https URL")
	}
	if u.User != nil {
		return "", errors.New("目标地址不支持内嵌认证信息")
	}
	if !allowPrivate {
		if ip, parseErr := netip.ParseAddr(u.Hostname()); parseErr == nil && !isPublicSpeedtestIP(ip) {
			return "", errors.New("测速地址不能为内网、回环或保留 IP")
		}
	}
	return u.String(), nil
}

func (m *speedtestManager) expireStaleJobsLocked(now time.Time) []string {
	var expiredIDs []string
	for nodeID, started := range m.agentTaskStarted {
		lastProg := m.agentTaskLastProg[nodeID]
		if lastProg.IsZero() {
			lastProg = started
		}
		if now.Sub(lastProg) > edgeSpeedtestIdleTimeout || now.Sub(started) > edgeSpeedtestHardMaxAge {
			if jobID := m.activeNode[nodeID]; jobID != "" && !strings.HasPrefix(jobID, "preparing:") {
				expiredIDs = append(expiredIDs, jobID)
				m.agentCancel[nodeID] = jobID
				m.agentCancelExpiry[nodeID] = now.Add(edgeSpeedtestCancelTTL)
			}
			delete(m.activeNode, nodeID)
			delete(m.agentTasks, nodeID)
			delete(m.agentTaskStarted, nodeID)
			delete(m.agentTaskLastProg, nodeID)
		}
	}
	for nodeID, exp := range m.agentCancelExpiry {
		if now.After(exp) {
			delete(m.agentCancel, nodeID)
			delete(m.agentCancelExpiry, nodeID)
		}
	}
	return expiredIDs
}

func (m *speedtestManager) markJobsExpired(jobIDs []string) {
	if len(jobIDs) == 0 || m.db == nil || m.db.db == nil {
		return
	}
	nowStr := time.Now().UTC().Format(time.RFC3339)
	for _, id := range jobIDs {
		_, _ = m.db.db.Exec(
			`UPDATE speedtest_jobs SET status='failed', error='测速超时或从机未响应', updated_at=? WHERE id=? AND status IN ('pending','running')`,
			nowStr, id,
		)
	}
}

func (m *speedtestManager) createJob(nodeID int64, nodeName, mode string, provinces, isps []string, customIDs []int64) (*speedtestJobView, error) {
	jobID := newSpeedtestJobID()
	now := time.Now()

	// Reserve admission slot BEFORE DoH DNS resolution or target expansion.
	m.mu.Lock()
	expiredIDs := m.expireStaleJobsLocked(now)
	if existingID := m.activeNode[nodeID]; existingID != "" {
		m.mu.Unlock()
		m.markJobsExpired(expiredIDs)
		return nil, errSpeedtestAlreadyRunning
	}
	m.activeNode[nodeID] = "preparing:" + jobID
	dur := m.phaseDuration
	m.mu.Unlock()

	m.markJobsExpired(expiredIDs)

	committed := false
	defer func() {
		if !committed {
			m.mu.Lock()
			if m.activeNode[nodeID] == "preparing:"+jobID {
				delete(m.activeNode, nodeID)
			}
			m.mu.Unlock()
		}
	}()

	if dur <= 0 {
		dur = defaultSpeedtestPhaseDuration
	}
	durationMs := int(dur / time.Millisecond)

	var cleanProvs []string
	var cleanISPs []string
	var targets []agentSpeedtestTarget

	if mode == "builtin" || mode == "all" {
		if len(provinces) == 0 {
			provinces = append([]string(nil), defaultSpeedtestProvinces...)
		}
		if len(isps) == 0 {
			isps = append([]string(nil), defaultSpeedtestISPs...)
		}
		seenProv := make(map[string]bool, len(provinces))
		seenISP := make(map[string]bool, len(isps))
		for _, isp := range isps {
			g := strings.ToLower(strings.TrimSpace(isp))
			if (g == "ct" || g == "cu" || g == "cm") && !seenISP[g] {
				seenISP[g] = true
				cleanISPs = append(cleanISPs, g)
			}
		}
		resolveCtx, resolveCancel := context.WithTimeout(context.Background(), 20*time.Second)
		for _, pCode := range provinces {
			def, ok := lookupSpeedtestProvince(pCode)
			if !ok || seenProv[def.Code] {
				continue
			}
			seenProv[def.Code] = true
			cleanProvs = append(cleanProvs, def.Code)
			for _, g := range cleanISPs {
				if len(targets) >= maxSpeedtestTargetsPerJob {
					break
				}
				ip := m.resolveECSIp(resolveCtx, def.Code+"-"+g, def.ECSCIDRs[g])
				ispLbl := ispGroupLabel(g)
				targets = append(targets, agentSpeedtestTarget{
					TargetKey:   def.Code + "-" + g,
					Label:       def.Label + ispLbl,
					Province:    def.Label,
					ISPGroup:    g,
					ISPLabel:    ispLbl,
					TargetKind:  "builtin",
					ResolvedIP:  ip,
					UploadURL:   appleSpeedtestUploadURL,
					DownloadURL: appleSpeedtestDownloadURL,
					Method:      http.MethodPost,
					DurationMs:  durationMs,
				})
			}
			if len(targets) >= maxSpeedtestTargetsPerJob {
				break
			}
		}
		resolveCancel()
	}

	if (mode == "custom" || mode == "all") && len(targets) < maxSpeedtestTargetsPerJob {
		customTargets, err := m.listCustomTargets()
		if err != nil {
			return nil, err
		}
		selectedSet := make(map[int64]bool, len(customIDs))
		for _, id := range customIDs {
			if id > 0 {
				selectedSet[id] = true
			}
		}
		for _, ct := range customTargets {
			if !ct.Enabled {
				continue
			}
			if len(selectedSet) > 0 && !selectedSet[ct.ID] {
				continue
			}
			if len(targets) >= maxSpeedtestTargetsPerJob {
				break
			}
			prov := strings.TrimSpace(ct.Province)
			if prov == "" {
				prov = ct.Name
			}
			targets = append(targets, agentSpeedtestTarget{
				TargetKey:   fmt.Sprintf("custom-%d", ct.ID),
				Label:       ct.Name,
				Province:    prov,
				ISPGroup:    ct.ISPGroup,
				ISPLabel:    ispGroupLabel(ct.ISPGroup),
				TargetKind:  "custom",
				UploadURL:   ct.UploadURL,
				DownloadURL: ct.DownloadURL,
				Method:      ct.Method,
				DurationMs:  durationMs,
			})
		}
	}

	if len(targets) == 0 {
		return nil, errors.New("没有可执行的测速目标，请检查省份、运营商或自定义目标选择")
	}
	if len(cleanProvs) == 0 {
		cleanProvs = []string{}
	}
	if len(cleanISPs) == 0 {
		cleanISPs = []string{}
	}

	var agentTask *agentSpeedtestTask
	if nodeID > 0 {
		agentTask = &agentSpeedtestTask{
			TaskID:     jobID,
			DurationMs: durationMs,
			Targets:    targets,
		}
		if encoded, err := json.Marshal(agentTask); err == nil && len(encoded) > maxAgentSpeedtestTaskBytes {
			return nil, errors.New("测速目标列表体积过大，请缩减自定义目标")
		}
	}

	nowStr := time.Now().UTC().Format(time.RFC3339)
	initialStatus := "running"
	if nodeID > 0 {
		initialStatus = "pending"
	}
	initialResults := make([]speedtestTargetResult, len(targets))
	for i, tgt := range targets {
		initialResults[i] = speedtestTargetResult{
			TargetKey:     tgt.TargetKey,
			Label:         tgt.Label,
			Province:      tgt.Province,
			ISPGroup:      tgt.ISPGroup,
			ISPLabel:      tgt.ISPLabel,
			TargetKind:    tgt.TargetKind,
			ResolvedIP:    tgt.ResolvedIP,
			Status:        "pending",
			ReturnRetrans: -1,
			ReturnRTTMs:   -1,
			OutboundRTTMs: -1,
		}
	}

	provsJSON, _ := json.Marshal(cleanProvs)
	ispsJSON, _ := json.Marshal(cleanISPs)
	resJSON, _ := json.Marshal(initialResults)

	if _, err := m.db.db.Exec(
		`INSERT INTO speedtest_jobs(id, node_id, node_name, mode, provinces_json, isps_json, status, total_targets, completed_targets, results_json, error, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		jobID, nodeID, nodeName, mode, string(provsJSON), string(ispsJSON), initialStatus, len(targets), 0, string(resJSON), "", nowStr, nowStr,
	); err != nil {
		return nil, err
	}

	// Clone results for the background worker so HTTP response serialization has zero shared mutable slice backing array.
	workerResults := append([]speedtestTargetResult(nil), initialResults...)
	respResults := append([]speedtestTargetResult(nil), initialResults...)

	m.mu.Lock()
	m.activeNode[nodeID] = jobID
	if nodeID == 0 {
		ctx, cancel := context.WithCancel(context.Background())
		doneCh := make(chan struct{})
		m.masterCanc[jobID] = cancel
		m.masterDone[jobID] = doneCh
		trOverride := m.targetTransport
		m.mu.Unlock()
		committed = true
		go m.runMasterJob(ctx, doneCh, jobID, nodeID, targets, workerResults, dur, trOverride)
	} else {
		nowTs := time.Now()
		m.agentTasks[nodeID] = agentTask
		m.agentTaskStarted[nodeID] = nowTs
		m.agentTaskLastProg[nodeID] = nowTs
		m.mu.Unlock()
		committed = true
	}

	return &speedtestJobView{
		ID:               jobID,
		NodeID:           nodeID,
		NodeName:         nodeName,
		Mode:             mode,
		Provinces:        cleanProvs,
		ISPs:             cleanISPs,
		Status:           initialStatus,
		TotalTargets:     len(targets),
		CompletedTargets: 0,
		Results:          respResults,
		CreatedAt:        nowStr,
		UpdatedAt:        nowStr,
	}, nil
}

var errSpeedtestAlreadyRunning = errors.New("speedtest already running on this node")

func (m *speedtestManager) runMasterJob(ctx context.Context, doneCh chan struct{}, jobID string, nodeID int64, targets []agentSpeedtestTarget, results []speedtestTargetResult, dur time.Duration, transportOverride *http.Transport) {
	defer func() {
		m.mu.Lock()
		if m.activeNode[nodeID] == jobID {
			delete(m.activeNode, nodeID)
		}
		delete(m.masterCanc, jobID)
		delete(m.masterDone, jobID)
		m.mu.Unlock()
		close(doneCh)
	}()

	completed := 0
	for i, tgt := range targets {
		if ctx.Err() != nil {
			break
		}
		results[i].Status = "running"
		m.saveJobProgress(jobID, "running", completed, results, "")

		res := executeSpeedtestTarget(ctx, tgt, dur, transportOverride)
		if ctx.Err() != nil {
			results[i].Status = "error"
			results[i].Error = "已取消"
			break
		}
		results[i] = res
		completed++
		m.saveJobProgress(jobID, "running", completed, results, "")
	}

	finalStatus := "done"
	finalErr := ""
	if ctx.Err() != nil {
		finalStatus = "canceled"
		finalErr = "任务已手动取消"
	} else {
		succeeded := 0
		for _, r := range results {
			if r.Status == "done" {
				succeeded++
			}
		}
		if succeeded == 0 && len(results) > 0 {
			finalStatus = "failed"
			finalErr = "全部测速目标执行失败"
		}
	}
	if m.saveJobProgress(jobID, finalStatus, completed, results, finalErr) {
		m.persistNodeSpeedSummary(nodeID, results)
	}
}

func (m *speedtestManager) saveJobProgress(jobID, status string, completed int, results []speedtestTargetResult, jobErr string) bool {
	resJSON, _ := json.Marshal(results)
	nowStr := time.Now().UTC().Format(time.RFC3339)
	res, err := m.db.db.Exec(
		`UPDATE speedtest_jobs SET status=?, completed_targets=?, results_json=?, error=?, updated_at=? WHERE id=? AND status IN ('pending','running')`,
		status, completed, string(resJSON), jobErr, nowStr, jobID,
	)
	if err != nil {
		log.Printf("speedtest: save job %q failed: %v", jobID, err)
		return false
	}
	n, _ := res.RowsAffected()
	m.pruneOldJobs()
	return n > 0
}

func (m *speedtestManager) pruneOldJobs() {
	m.mu.Lock()
	if time.Since(m.lastCleaned) < time.Minute {
		m.mu.Unlock()
		return
	}
	m.lastCleaned = time.Now()
	m.mu.Unlock()

	_, _ = m.db.db.Exec(
		`DELETE FROM speedtest_jobs WHERE status IN ('done','failed','canceled') AND id NOT IN (SELECT id FROM speedtest_jobs ORDER BY created_at DESC LIMIT ?)`,
		maxSpeedtestHistoryJobs,
	)
}

func (m *speedtestManager) persistNodeSpeedSummary(nodeID int64, results []speedtestTargetResult) {
	type groupAgg struct {
		retSum     int64
		retCnt     int
		outSum     int64
		outCnt     int
		retRTTSum  int64
		retRTTCnt  int
		outRTTSum  int64
		outRTTCnt  int
		retransSum int
		hasRetrans bool
		samples    int
	}
	byISP := make(map[string]*groupAgg)
	for _, r := range results {
		if r.Status != "done" || r.Error != "" || !validISPGroup(r.ISPGroup) {
			continue
		}
		agg := byISP[r.ISPGroup]
		if agg == nil {
			agg = &groupAgg{}
			byISP[r.ISPGroup] = agg
		}
		agg.samples++
		if r.ReturnBps > 0 {
			agg.retSum += r.ReturnBps
			agg.retCnt++
		}
		if r.OutboundBps > 0 {
			agg.outSum += r.OutboundBps
			agg.outCnt++
		}
		if r.ReturnRTTMs >= 0 {
			agg.retRTTSum += int64(r.ReturnRTTMs)
			agg.retRTTCnt++
		}
		if r.OutboundRTTMs >= 0 {
			agg.outRTTSum += int64(r.OutboundRTTMs)
			agg.outRTTCnt++
		}
		if r.ReturnRetrans >= 0 {
			agg.retransSum += r.ReturnRetrans
			agg.hasRetrans = true
		}
	}
	if len(byISP) == 0 {
		return
	}

	testedAt := time.Now().UTC().Format(time.RFC3339)
	for group, agg := range byISP {
		var avgRetBps, avgOutBps int64
		if agg.retCnt > 0 {
			avgRetBps = agg.retSum / int64(agg.retCnt)
		}
		if agg.outCnt > 0 {
			avgOutBps = agg.outSum / int64(agg.outCnt)
		}
		avgRetRTT := -1
		if agg.retRTTCnt > 0 {
			avgRetRTT = int(agg.retRTTSum / int64(agg.retRTTCnt))
		}
		avgOutRTT := -1
		if agg.outRTTCnt > 0 {
			avgOutRTT = int(agg.outRTTSum / int64(agg.outRTTCnt))
		}
		retrans := -1
		if agg.hasRetrans {
			retrans = agg.retransSum
		}
		if _, err := m.db.db.Exec(
			`INSERT INTO edge_node_speed_metrics(node_id, isp_group, avg_return_bps, avg_outbound_bps, avg_return_rtt_ms, avg_outbound_rtt_ms, total_retrans, sample_count, tested_at)
			 VALUES(?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(node_id, isp_group) DO UPDATE SET
			   avg_return_bps=excluded.avg_return_bps,
			   avg_outbound_bps=excluded.avg_outbound_bps,
			   avg_return_rtt_ms=excluded.avg_return_rtt_ms,
			   avg_outbound_rtt_ms=excluded.avg_outbound_rtt_ms,
			   total_retrans=excluded.total_retrans,
			   sample_count=excluded.sample_count,
			   tested_at=excluded.tested_at`,
			nodeID, group, avgRetBps, avgOutBps, avgRetRTT, avgOutRTT, retrans, agg.samples, testedAt,
		); err != nil {
			log.Printf("speedtest: persist metrics for node %d/%s failed: %v", nodeID, group, err)
		}
	}

	if m.pm != nil && m.pm.edges != nil {
		m.pm.edges.reloadSpeedMetrics()
	}
}

// nextAgentDirective is called from handleAgentHeartbeat to fetch pending task or cancel ID.
func (m *speedtestManager) nextAgentDirective(nodeID int64) (task *agentSpeedtestTask, cancelID string) {
	if m == nil || nodeID <= 0 {
		return nil, ""
	}
	now := time.Now()
	m.mu.Lock()
	expiredIDs := m.expireStaleJobsLocked(now)
	if cID := m.agentCancel[nodeID]; cID != "" {
		cancelID = cID
	}
	if t := m.agentTasks[nodeID]; t != nil {
		taskCopy := &agentSpeedtestTask{
			TaskID:     t.TaskID,
			DurationMs: t.DurationMs,
			Targets:    append([]agentSpeedtestTarget(nil), t.Targets...),
		}
		task = taskCopy
	}
	m.mu.Unlock()
	m.markJobsExpired(expiredIDs)
	return task, cancelID
}

// applyAgentProgress merges speedtest progress uploaded in agent heartbeat and returns
// an acknowledgment ID once a terminal update is safely durable in SQLite.
func (m *speedtestManager) applyAgentProgress(nodeID int64, p *agentSpeedtestProgress) string {
	if m == nil || nodeID <= 0 || p == nil || strings.TrimSpace(p.TaskID) == "" {
		return ""
	}
	terminal := p.Status == "done" || p.Status == "failed" || p.Status == "canceled"

	m.mu.Lock()
	activeID := m.activeNode[nodeID]
	canceledID := m.agentCancel[nodeID]
	if p.Status == "canceled" && canceledID == p.TaskID {
		delete(m.agentCancel, nodeID)
		delete(m.agentCancelExpiry, nodeID)
	}
	isExpected := activeID == p.TaskID
	if isExpected && !terminal {
		m.agentTaskLastProg[nodeID] = time.Now()
	}
	m.mu.Unlock()

	if !isExpected {
		// If the Master already canceled this task via cancelJob, acknowledge once the Agent reports canceled.
		if p.Status == "canceled" && canceledID == p.TaskID {
			return p.TaskID
		}
		return ""
	}

	status := p.Status
	if status == "" {
		status = "running"
	}
	savedOK := m.saveJobProgress(p.TaskID, status, p.CompletedTargets, p.Results, p.Error)
	if !terminal {
		return ""
	}
	if savedOK {
		m.mu.Lock()
		if m.activeNode[nodeID] == p.TaskID {
			delete(m.activeNode, nodeID)
			delete(m.agentTasks, nodeID)
			delete(m.agentTaskStarted, nodeID)
			delete(m.agentTaskLastProg, nodeID)
		}
		m.mu.Unlock()
		m.persistNodeSpeedSummary(nodeID, p.Results)
		return p.TaskID
	}

	// Row may already have transitioned via cancelJob; clear active slot if owned by this task.
	row, found, err := m.getJob(p.TaskID)
	if err == nil && found && (row.Status == "done" || row.Status == "failed" || row.Status == "canceled") {
		m.mu.Lock()
		if m.activeNode[nodeID] == p.TaskID {
			delete(m.activeNode, nodeID)
			delete(m.agentTasks, nodeID)
			delete(m.agentTaskStarted, nodeID)
			delete(m.agentTaskLastProg, nodeID)
		}
		m.mu.Unlock()
		return p.TaskID
	}
	return ""
}

func (m *speedtestManager) cancelJob(jobID string) (bool, error) {
	job, found, err := m.getJob(jobID)
	if err != nil || !found {
		return false, err
	}
	if job.Status != "running" && job.Status != "pending" {
		return true, nil
	}

	m.mu.Lock()
	cancel := m.masterCanc[jobID]
	doneCh := m.masterDone[jobID]
	if job.NodeID > 0 {
		if t := m.agentTasks[job.NodeID]; t != nil && t.TaskID == jobID {
			delete(m.agentTasks, job.NodeID)
		}
		if m.activeNode[job.NodeID] == jobID {
			delete(m.activeNode, job.NodeID)
			delete(m.agentTaskStarted, job.NodeID)
			delete(m.agentTaskLastProg, job.NodeID)
		}
		m.agentCancel[job.NodeID] = jobID
		m.agentCancelExpiry[job.NodeID] = time.Now().Add(edgeSpeedtestCancelTTL)
	}
	m.mu.Unlock()

	if cancel != nil {
		cancel()
		if doneCh != nil {
			select {
			case <-doneCh:
			case <-time.After(2 * time.Second):
			}
		}
	}

	if m.saveJobProgress(jobID, "canceled", job.CompletedTargets, job.Results, "任务已手动取消") {
		m.persistNodeSpeedSummary(job.NodeID, job.Results)
	}
	return true, nil
}

func (m *speedtestManager) sweepExpiredJobs() {
	m.mu.Lock()
	expiredIDs := m.expireStaleJobsLocked(time.Now())
	m.mu.Unlock()
	m.markJobsExpired(expiredIDs)
}

func (m *speedtestManager) getJob(id string) (speedtestJobView, bool, error) {
	m.sweepExpiredJobs()
	var job speedtestJobView
	var provsJSON, ispsJSON, resJSON string
	err := m.db.db.QueryRow(
		`SELECT id, node_id, node_name, mode, provinces_json, isps_json, status, total_targets, completed_targets, results_json, error, created_at, updated_at FROM speedtest_jobs WHERE id=?`,
		id,
	).Scan(&job.ID, &job.NodeID, &job.NodeName, &job.Mode, &provsJSON, &ispsJSON, &job.Status, &job.TotalTargets, &job.CompletedTargets, &resJSON, &job.Error, &job.CreatedAt, &job.UpdatedAt)
	if err == sql.ErrNoRows {
		return job, false, nil
	}
	if err != nil {
		return job, false, err
	}
	_ = json.Unmarshal([]byte(provsJSON), &job.Provinces)
	_ = json.Unmarshal([]byte(ispsJSON), &job.ISPs)
	_ = json.Unmarshal([]byte(resJSON), &job.Results)
	if job.Provinces == nil {
		job.Provinces = []string{}
	}
	if job.ISPs == nil {
		job.ISPs = []string{}
	}
	if job.Results == nil {
		job.Results = []speedtestTargetResult{}
	}
	return job, true, nil
}

func (m *speedtestManager) listJobs(nodeID *int64, limit int) ([]speedtestJobView, error) {
	m.sweepExpiredJobs()
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	var rows *sql.Rows
	var err error
	if nodeID != nil {
		rows, err = m.db.db.Query(
			`SELECT id, node_id, node_name, mode, provinces_json, isps_json, status, total_targets, completed_targets, results_json, error, created_at, updated_at FROM speedtest_jobs WHERE node_id=? ORDER BY created_at DESC LIMIT ?`,
			*nodeID, limit,
		)
	} else {
		rows, err = m.db.db.Query(
			`SELECT id, node_id, node_name, mode, provinces_json, isps_json, status, total_targets, completed_targets, results_json, error, created_at, updated_at FROM speedtest_jobs ORDER BY created_at DESC LIMIT ?`,
			limit,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]speedtestJobView, 0)
	for rows.Next() {
		var job speedtestJobView
		var provsJSON, ispsJSON, resJSON string
		if err := rows.Scan(&job.ID, &job.NodeID, &job.NodeName, &job.Mode, &provsJSON, &ispsJSON, &job.Status, &job.TotalTargets, &job.CompletedTargets, &resJSON, &job.Error, &job.CreatedAt, &job.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(provsJSON), &job.Provinces)
		_ = json.Unmarshal([]byte(ispsJSON), &job.ISPs)
		_ = json.Unmarshal([]byte(resJSON), &job.Results)
		if job.Results == nil {
			job.Results = []speedtestTargetResult{}
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Admin HTTP handlers

func (a *App) handleSpeedtestTargets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.speedtest == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "speedtest unavailable")
		return
	}
	custom, err := a.speedtest.listCustomTargets()
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "加载自定义测速目标失败")
		return
	}

	type provDTO struct {
		Code  string `json:"code"`
		Label string `json:"label"`
	}
	provs := make([]provDTO, 0, len(speedtestProvinceCatalog))
	for _, p := range speedtestProvinceCatalog {
		provs = append(provs, provDTO{Code: p.Code, Label: p.Label})
	}
	type ispDTO struct {
		Group string `json:"group"`
		Label string `json:"label"`
	}
	isps := []ispDTO{
		{Group: "ct", Label: "电信"},
		{Group: "cu", Label: "联通"},
		{Group: "cm", Label: "移动"},
	}

	type nodeSummary struct {
		ID           int64                 `json:"id"`
		Name         string                `json:"name"`
		IsMaster     bool                  `json:"is_master"`
		Online       bool                  `json:"online"`
		Status       string                `json:"status"`
		PublicURL    string                `json:"public_url,omitempty"`
		DetectedIP   string                `json:"detected_ip,omitempty"`
		SpeedMetrics []edgeNodeSpeedMetric `json:"speed_metrics,omitempty"`
	}

	nodes := make([]nodeSummary, 0)
	var masterMetrics []edgeNodeSpeedMetric
	if a.pm != nil && a.pm.edges != nil {
		masterMetrics = a.pm.edges.nodeSpeedMetricsCopy(0)
	}
	nodes = append(nodes, nodeSummary{
		ID:           0,
		Name:         "主机",
		IsMaster:     true,
		Online:       true,
		Status:       "online",
		SpeedMetrics: masterMetrics,
	})

	if a.pm != nil && a.pm.edges != nil {
		now := time.Now()
		a.pm.edges.mu.RLock()
		ids := make([]int64, 0, len(a.pm.edges.nodes))
		for id := range a.pm.edges.nodes {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			st := a.pm.edges.nodes[id]
			if st == nil {
				continue
			}
			nodes = append(nodes, nodeSummary{
				ID:           st.id,
				Name:         st.name,
				Online:       st.isOnline(now),
				Status:       st.statusAt(now),
				PublicURL:    st.publicURL(),
				DetectedIP:   st.detectedIP,
				SpeedMetrics: a.pm.edges.speedMetricsMapCopyLocked(st.id),
			})
		}
		a.pm.edges.mu.RUnlock()
	}

	a.jsonOK(w, map[string]any{
		"provinces":         provs,
		"isps":              isps,
		"default_provinces": defaultSpeedtestProvinces,
		"default_isps":      defaultSpeedtestISPs,
		"custom_targets":    custom,
		"nodes":             nodes,
	})
}

func (a *App) handleSpeedtestCustomTargets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.speedtest == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "speedtest unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		list, err := a.speedtest.listCustomTargets()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "加载自定义目标失败")
			return
		}
		a.jsonOK(w, map[string]any{"targets": list})
	case http.MethodPost:
		var req struct {
			Name        string `json:"name"`
			ISPGroup    string `json:"isp_group"`
			Province    string `json:"province"`
			UploadURL   string `json:"upload_url"`
			DownloadURL string `json:"download_url"`
			Method      string `json:"method"`
			Enabled     *bool  `json:"enabled"`
		}
		if err := decodeJSONBody(w, r, &req); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "请求参数无效")
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" || len([]rune(name)) > 80 {
			a.jsonErr(w, http.StatusBadRequest, "目标名称不能为空且不超过80字符")
			return
		}
		prov := strings.TrimSpace(req.Province)
		if len([]rune(prov)) > maxSpeedtestProvinceRunes {
			a.jsonErr(w, http.StatusBadRequest, "地区标签过长")
			return
		}
		ispGroup := strings.ToLower(strings.TrimSpace(req.ISPGroup))
		if !validISPGroup(ispGroup) {
			ispGroup = "other"
		}
		uploadURL, err := validateCustomSpeedtestTargetURL(req.UploadURL, false)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, "回程测速地址无效: "+err.Error())
			return
		}
		downloadURL, err := validateCustomSpeedtestTargetURL(req.DownloadURL, false)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, "去程测速地址无效: "+err.Error())
			return
		}
		if uploadURL == "" && downloadURL == "" {
			a.jsonErr(w, http.StatusBadRequest, "回程地址与去程地址至少填写一项")
			return
		}
		method := strings.ToUpper(strings.TrimSpace(req.Method))
		if method != http.MethodPut {
			method = http.MethodPost
		}
		enabled := true
		if req.Enabled != nil {
			enabled = *req.Enabled
		}

		a.speedtest.customTargetMu.Lock()
		defer a.speedtest.customTargetMu.Unlock()
		var cnt int
		if err := a.db.db.QueryRow(`SELECT COUNT(*) FROM speedtest_custom_targets`).Scan(&cnt); err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "检查自定义目标数量失败")
			return
		}
		if cnt >= maxCustomSpeedtestTargets {
			a.jsonErr(w, http.StatusBadRequest, "自定义测速目标数量已达上限")
			return
		}
		res, err := a.db.db.Exec(
			`INSERT INTO speedtest_custom_targets(name, isp_group, province, upload_url, download_url, method, enabled) VALUES(?,?,?,?,?,?,?)`,
			name, ispGroup, prov, uploadURL, downloadURL, method, sqliteBool(enabled),
		)
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "创建自定义测速目标失败")
			return
		}
		id, _ := res.LastInsertId()
		a.jsonResponse(w, http.StatusCreated, map[string]any{
			"id": id,
			"target": speedtestCustomTarget{
				ID:          id,
				Name:        name,
				ISPGroup:    ispGroup,
				ISPLabel:    ispGroupLabel(ispGroup),
				Province:    prov,
				UploadURL:   uploadURL,
				DownloadURL: downloadURL,
				Method:      method,
				Enabled:     enabled,
			},
		})
	default:
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *App) handleSpeedtestCustomTargetByID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.speedtest == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "speedtest unavailable")
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/api/speedtest/custom-targets/")
	id, err := strconv.ParseInt(strings.Trim(idStr, "/"), 10, 64)
	if err != nil || id <= 0 {
		a.jsonErr(w, http.StatusBadRequest, "invalid target id")
		return
	}
	switch r.Method {
	case http.MethodDelete:
		a.speedtest.customTargetMu.Lock()
		res, err := a.db.db.Exec(`DELETE FROM speedtest_custom_targets WHERE id=?`, id)
		a.speedtest.customTargetMu.Unlock()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "删除失败")
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			a.jsonErr(w, http.StatusNotFound, "target not found")
			return
		}
		a.jsonOK(w, map[string]any{"deleted": true})
	case http.MethodPut:
		var req struct {
			Name        string `json:"name"`
			ISPGroup    string `json:"isp_group"`
			Province    string `json:"province"`
			UploadURL   string `json:"upload_url"`
			DownloadURL string `json:"download_url"`
			Method      string `json:"method"`
			Enabled     *bool  `json:"enabled"`
		}
		if err := decodeJSONBody(w, r, &req); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "请求参数无效")
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" || len([]rune(name)) > 80 {
			a.jsonErr(w, http.StatusBadRequest, "目标名称不能为空且不超过80字符")
			return
		}
		prov := strings.TrimSpace(req.Province)
		if len([]rune(prov)) > maxSpeedtestProvinceRunes {
			a.jsonErr(w, http.StatusBadRequest, "地区标签过长")
			return
		}
		ispGroup := strings.ToLower(strings.TrimSpace(req.ISPGroup))
		if !validISPGroup(ispGroup) {
			ispGroup = "other"
		}
		uploadURL, err := validateCustomSpeedtestTargetURL(req.UploadURL, false)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, "回程测速地址无效: "+err.Error())
			return
		}
		downloadURL, err := validateCustomSpeedtestTargetURL(req.DownloadURL, false)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, "去程测速地址无效: "+err.Error())
			return
		}
		if uploadURL == "" && downloadURL == "" {
			a.jsonErr(w, http.StatusBadRequest, "回程地址与去程地址至少填写一项")
			return
		}
		method := strings.ToUpper(strings.TrimSpace(req.Method))
		if method != http.MethodPut {
			method = http.MethodPost
		}
		enabled := true
		if req.Enabled != nil {
			enabled = *req.Enabled
		}
		a.speedtest.customTargetMu.Lock()
		res, err := a.db.db.Exec(
			`UPDATE speedtest_custom_targets SET name=?, isp_group=?, province=?, upload_url=?, download_url=?, method=?, enabled=? WHERE id=?`,
			name, ispGroup, prov, uploadURL, downloadURL, method, sqliteBool(enabled), id,
		)
		a.speedtest.customTargetMu.Unlock()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "更新失败")
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			a.jsonErr(w, http.StatusNotFound, "target not found")
			return
		}
		a.jsonOK(w, map[string]any{"updated": true})
	default:
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *App) handleSpeedtestStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.speedtest == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "speedtest unavailable")
		return
	}
	var req struct {
		NodeID          int64    `json:"node_id"`
		Mode            string   `json:"mode"`
		Provinces       []string `json:"provinces"`
		ISPs            []string `json:"isps"`
		CustomTargetIDs []int64  `json:"custom_target_ids"`
	}
	if err := decodeJSONBody(w, r, &req); err != nil {
		a.jsonErr(w, http.StatusBadRequest, "请求参数无效")
		return
	}
	if req.NodeID < 0 {
		a.jsonErr(w, http.StatusBadRequest, "节点 ID 无效")
		return
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode != "custom" && mode != "all" {
		mode = "builtin"
	}

	nodeName := "主机"
	if req.NodeID > 0 {
		if a.pm == nil || a.pm.edges == nil {
			a.jsonErr(w, http.StatusServiceUnavailable, "从机运行时未初始化")
			return
		}
		now := time.Now()
		a.pm.edges.mu.RLock()
		node := a.pm.edges.nodes[req.NodeID]
		var online, outdatedAgent bool
		if node != nil {
			nodeName = node.name
			online = node.isOnline(now)
			outdatedAgent = node.agentVersion != "" && !node.supportsSpeedtest
		}
		a.pm.edges.mu.RUnlock()
		if node == nil {
			a.jsonErr(w, http.StatusNotFound, "从机不存在")
			return
		}
		if !online {
			a.jsonErr(w, http.StatusBadRequest, "从机当前离线，无法下发测速任务")
			return
		}
		if outdatedAgent {
			a.jsonErr(w, http.StatusBadRequest, "该从机版本过旧，不支持远程测速，请先在从机页面点击升级")
			return
		}
	}

	job, err := a.speedtest.createJob(req.NodeID, nodeName, mode, req.Provinces, req.ISPs, req.CustomTargetIDs)
	if err != nil {
		if errors.Is(err, errSpeedtestAlreadyRunning) {
			a.jsonErr(w, http.StatusConflict, "该节点正在测速中，请等待完成或取消后重试")
			return
		}
		a.jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.jsonResponse(w, http.StatusCreated, map[string]any{"job": job})
}

func (a *App) handleSpeedtestJobs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.speedtest == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "speedtest unavailable")
		return
	}
	var nodeFilter *int64
	if raw := strings.TrimSpace(r.URL.Query().Get("node_id")); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v >= 0 {
			nodeFilter = &v
		}
	}
	jobs, err := a.speedtest.listJobs(nodeFilter, 20)
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "加载测速记录失败")
		return
	}
	a.jsonOK(w, map[string]any{"jobs": jobs})
}

func (a *App) handleSpeedtestJobByID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.speedtest == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "speedtest unavailable")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/speedtest/jobs/")
	rest = strings.Trim(rest, "/")
	if rest == "" {
		a.jsonErr(w, http.StatusBadRequest, "missing job id")
		return
	}
	if strings.HasSuffix(rest, "/cancel") {
		if r.Method != http.MethodPost {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		jobID := strings.TrimSuffix(rest, "/cancel")
		jobID = strings.Trim(jobID, "/")
		found, err := a.speedtest.cancelJob(jobID)
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "取消测速失败")
			return
		}
		if !found {
			a.jsonErr(w, http.StatusNotFound, "job not found")
			return
		}
		a.jsonOK(w, map[string]any{"canceled": true})
		return
	}
	if r.Method != http.MethodGet {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	job, found, err := a.speedtest.getJob(rest)
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "查询测速任务失败")
		return
	}
	if !found {
		a.jsonErr(w, http.StatusNotFound, "job not found")
		return
	}
	a.jsonOK(w, map[string]any{"job": job})
}

func (a *App) handleSpeedtestDirectTargets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	type directTarget struct {
		NodeID            int64  `json:"node_id"`
		Name              string `json:"name"`
		IsMaster          bool   `json:"is_master"`
		Online            bool   `json:"online"`
		PublicURL         string `json:"public_url,omitempty"`
		Scheme            string `json:"scheme"`
		StreamURL         string `json:"stream_url"`
		SizeBytes         int64  `json:"size_bytes"`
		UnavailableReason string `json:"unavailable_reason,omitempty"`
	}

	panelScheme := "http"
	if a.panelTLSEnabled || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") || r.TLS != nil {
		panelScheme = "https"
	}
	list := []directTarget{
		{
			NodeID:    0,
			Name:      "主机",
			IsMaster:  true,
			Online:    true,
			Scheme:    panelScheme,
			StreamURL: fmt.Sprintf("/api/speedtest/direct-stream?bytes=%d", defaultDirectStreamBytes),
			SizeBytes: defaultDirectStreamBytes,
		},
	}

	if a.pm != nil && a.pm.edges != nil {
		now := time.Now()
		a.pm.edges.mu.RLock()
		ids := make([]int64, 0, len(a.pm.edges.nodes))
		for id := range a.pm.edges.nodes {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			node := a.pm.edges.nodes[id]
			if node == nil {
				continue
			}
			pubURL := node.publicURL()
			online := node.isOnline(now) && pubURL != "" && node.tokenPlain != ""
			scheme := "http"
			if strings.HasPrefix(pubURL, "https://") {
				scheme = "https"
			}
			item := directTarget{
				NodeID:    node.id,
				Name:      node.name,
				Online:    online,
				PublicURL: pubURL,
				Scheme:    scheme,
				SizeBytes: defaultDirectStreamBytes,
			}
			if !online {
				item.UnavailableReason = "节点离线或未配置访问地址"
			} else if sealed, err := sealEdgeSpeedtestTicket(node.tokenPlain, node.id, maxDirectStreamBytes); err != nil {
				item.Online = false
				item.UnavailableReason = "签发测速令牌失败"
			} else {
				item.StreamURL = fmt.Sprintf("%s%s%s?bytes=%d", strings.TrimRight(pubURL, "/"), edgeSpeedtestPathPrefix, sealed, defaultDirectStreamBytes)
			}
			list = append(list, item)
		}
		a.pm.edges.mu.RUnlock()
	}

	a.jsonOK(w, map[string]any{"targets": list})
}

func serveSpeedtestByteStream(w http.ResponseWriter, r *http.Request, maxAllowed int64) {
	if maxAllowed <= 0 || maxAllowed > maxDirectStreamBytes {
		maxAllowed = maxDirectStreamBytes
	}
	total := defaultDirectStreamBytes
	if raw := strings.TrimSpace(r.URL.Query().Get("bytes")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
			total = parsed
		}
	}
	if total < minDirectStreamBytes {
		total = minDirectStreamBytes
	}
	if total > maxAllowed {
		total = maxAllowed
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}

	deadline := time.Now().Add(30 * time.Second)
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	rc := http.NewResponseController(w)

	var written int64
	for written < total {
		select {
		case <-ctx.Done():
			return
		default:
		}
		writeTimeout := time.Now().Add(15 * time.Second)
		if writeTimeout.After(deadline) {
			writeTimeout = deadline
		}
		_ = rc.SetWriteDeadline(writeTimeout)
		rem := total - written
		n := int64(len(speedtestPatternChunk))
		if rem < n {
			n = rem
		}
		wBytes, err := w.Write(speedtestPatternChunk[:n])
		written += int64(wBytes)
		if err != nil {
			return
		}
	}
}

func (a *App) handleSpeedtestDirectStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	serveSpeedtestByteStream(w, r, maxDirectStreamBytes)
}
