package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/csv"
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
	speedtestBrowserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

	defaultSpeedtestCatalogCSVURL  = "https://raw.githubusercontent.com/spiritLHLS/speedtest.cn-CN-ID/main/CN.csv"
	defaultSpeedtestCatalogJSONURL = "https://raw.githubusercontent.com/oneclickvirt/speedtest/main/model/snapshot/speedtest-servers.json"
	speedtestCatalogTTL            = 6 * time.Hour
	speedtestCatalogRetryTTL       = 5 * time.Minute
	maxSpeedtestCandidatesPerNode  = 6
	defaultSpeedtestDownloadBytes  = 104857600
	minValidUploadBytes            = int64(256 << 10)
	minFastTestUploadBytes         = int64(64 << 10)
	maxDirectReportSessions        = 128

	edgeSpeedtestPathPrefix       = "/_yundu/st/"
	edgeSpeedtestUIPathPrefix     = "/_yundu/st-ui/"
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

var errUploadWindowReached = errors.New("upload duration window reached")

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
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
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
	Code   string `json:"code"`
	Label  string `json:"label"`
	Region string `json:"-"`
}

var speedtestProvinceCatalog = []speedtestProvinceDef{
	{Code: "bj", Label: "北京", Region: "north"},
	{Code: "sh", Label: "上海", Region: "east"},
	{Code: "gd", Label: "广东", Region: "south"},
	{Code: "js", Label: "江苏", Region: "east"},
	{Code: "zj", Label: "浙江", Region: "east"},
	{Code: "sc", Label: "四川", Region: "southwest"},
	{Code: "hb", Label: "湖北", Region: "central"},
	{Code: "fj", Label: "福建", Region: "east"},
	{Code: "sd", Label: "山东", Region: "north"},
	{Code: "sn", Label: "陕西", Region: "northwest"},
	{Code: "ln", Label: "辽宁", Region: "north"},
	{Code: "cq", Label: "重庆", Region: "southwest"},
}

var speedtestRegionFallbackOrder = map[string][]string{
	"north":     {"north", "east", "central", "south", "southwest", "northwest"},
	"east":      {"east", "central", "north", "south", "southwest", "northwest"},
	"south":     {"south", "east", "central", "southwest", "north", "northwest"},
	"central":   {"central", "east", "north", "south", "southwest", "northwest"},
	"southwest": {"southwest", "central", "south", "east", "north", "northwest"},
	"northwest": {"northwest", "north", "southwest", "central", "east", "south"},
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

type speedtestLocationAlias struct {
	keywords []string
	code     string
	label    string
	region   string
}

var speedtestLocationAliases = []speedtestLocationAlias{
	{keywords: []string{"北京", "beijing"}, code: "bj", label: "北京", region: "north"},
	{keywords: []string{"上海", "shanghai"}, code: "sh", label: "上海", region: "east"},
	{keywords: []string{"广东", "广州", "深圳", "guangzh", "guangzhou", "shenzhen"}, code: "gd", label: "广东", region: "south"},
	{keywords: []string{"江苏", "南京", "苏州", "镇江", "无锡", "连云港", "nanjing", "suzhou", "zhenj", "zhenjiang", "wuxi"}, code: "js", label: "江苏", region: "east"},
	{keywords: []string{"浙江", "杭州", "宁波", "温州", "hangzhou", "ningbo", "wenzhou"}, code: "zj", label: "浙江", region: "east"},
	{keywords: []string{"四川", "成都", "绵阳", "南充", "chengdu", "mianyang", "nanchong"}, code: "sc", label: "四川", region: "southwest"},
	{keywords: []string{"湖北", "武汉", "wuhan", "hubei"}, code: "hb", label: "湖北", region: "central"},
	{keywords: []string{"福建", "福州", "厦门", "fujian", "fuzhou", "xiamen"}, code: "fj", label: "福建", region: "east"},
	{keywords: []string{"山东", "济南", "青岛", "shandong", "jinan", "qingdao"}, code: "sd", label: "山东", region: "north"},
	{keywords: []string{"陕西", "西安", "shaanxi", "xian"}, code: "sn", label: "陕西", region: "northwest"},
	{keywords: []string{"辽宁", "沈阳", "大连", "liaoning", "shenyang", "dalian"}, code: "ln", label: "辽宁", region: "north"},
	{keywords: []string{"重庆", "chongqing"}, code: "cq", label: "重庆", region: "southwest"},
	{keywords: []string{"天津", "tianjin"}, code: "tj", label: "天津", region: "north"},
	{keywords: []string{"山西", "太原", "taiyuan"}, code: "sx", label: "山西", region: "north"},
	{keywords: []string{"河北", "石家庄", "shijiazhuang"}, code: "he", label: "河北", region: "north"},
	{keywords: []string{"河南", "郑州", "zhengzhou"}, code: "ha", label: "河南", region: "central"},
	{keywords: []string{"湖南", "长沙", "changsha"}, code: "hn", label: "湖南", region: "central"},
	{keywords: []string{"安徽", "合肥", "hefei"}, code: "ah", label: "安徽", region: "east"},
	{keywords: []string{"江西", "南昌", "nanchang"}, code: "jx", label: "江西", region: "east"},
	{keywords: []string{"广西", "南宁", "nanning"}, code: "gx", label: "广西", region: "south"},
	{keywords: []string{"海南", "海口", "haikou"}, code: "hi", label: "海南", region: "south"},
	{keywords: []string{"甘肃", "兰州", "lanzhou"}, code: "gs", label: "甘肃", region: "northwest"},
	{keywords: []string{"贵州", "贵阳", "guiyang"}, code: "gz", label: "贵州", region: "southwest"},
	{keywords: []string{"云南", "昆明", "kunming"}, code: "yn", label: "云南", region: "southwest"},
	{keywords: []string{"吉林", "长春", "changchun"}, code: "jl", label: "吉林", region: "north"},
	{keywords: []string{"黑龙江", "哈尔滨", "harbin"}, code: "hl", label: "黑龙江", region: "north"},
}

func normalizeSpeedtestCatalogLocation(provRaw, cityRaw string) (provCode, provLabel, region string) {
	combined := strings.ToLower(strings.TrimSpace(provRaw) + " " + strings.TrimSpace(cityRaw))
	if combined == "" {
		return "", "", ""
	}
	for _, entry := range speedtestLocationAliases {
		for _, kw := range entry.keywords {
			if strings.Contains(combined, kw) {
				return entry.code, entry.label, entry.region
			}
		}
	}
	return "", "", ""
}

func mapSpeedtestOperator(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "ct", "电信", "中国电信", "chinatelecom":
		return "ct"
	case "cu", "联通", "中国联通", "chinaunicom":
		return "cu"
	case "cm", "移动", "中国移动", "chinamobile":
		return "cm"
	}
	switch {
	case strings.Contains(v, "电信") || strings.Contains(v, "telecom"):
		return "ct"
	case strings.Contains(v, "联通") || strings.Contains(v, "unicom"):
		return "cu"
	case strings.Contains(v, "移动") || strings.Contains(v, "mobile"):
		return "cm"
	default:
		return ""
	}
}

type speedtestCatalogNode struct {
	Host        string
	Label       string
	ProvCode    string
	ProvLabel   string
	Region      string
	ISPGroup    string
	DownloadURL string
	UploadURL   string
	Method      string
	DirectHTTP  bool
}

var embeddedChinaSpeedtestCatalog = []speedtestCatalogNode{
	// 电信 CT
	{Host: "speedtest1.online.sh.cn:8080", Label: "上海电信", ProvCode: "sh", ProvLabel: "上海", Region: "east", ISPGroup: "ct", DirectHTTP: true},
	{Host: "5gnanjing.speedtest.jsinfo.net:8080", Label: "江苏南京电信", ProvCode: "js", ProvLabel: "江苏", Region: "east", ISPGroup: "ct", DirectHTTP: true},
	{Host: "5gzhenjiang.speedtest.jsinfo.net:8080", Label: "江苏镇江电信", ProvCode: "js", ProvLabel: "江苏", Region: "east", ISPGroup: "ct", DirectHTTP: true},
	{Host: "4gsuzhou1.speedtest.jsinfo.net:8080", Label: "江苏苏州电信", ProvCode: "js", ProvLabel: "江苏", Region: "east", ISPGroup: "ct", DirectHTTP: true},
	{Host: "cesu-hz.zjtelecom.com.cn:8080", Label: "浙江杭州电信", ProvCode: "zj", ProvLabel: "浙江", Region: "east", ISPGroup: "ct", DirectHTTP: true},
	{Host: "cesu-nb.zjtelecom.com.cn:8080", Label: "浙江宁波电信", ProvCode: "zj", ProvLabel: "浙江", Region: "east", ISPGroup: "ct", DirectHTTP: true},
	{Host: "vipspeedtest8.wuhan.net.cn:8080", Label: "湖北武汉电信", ProvCode: "hb", ProvLabel: "湖北", Region: "central", ISPGroup: "ct", DirectHTTP: true},
	{Host: "hntelecom5g.cn:8080", Label: "湖南长沙电信", ProvCode: "hn", ProvLabel: "湖南", Region: "central", ISPGroup: "ct", DirectHTTP: true},
	{Host: "speedtest1.sc.189.cn:8080", Label: "四川成都电信", ProvCode: "sc", ProvLabel: "四川", Region: "southwest", ISPGroup: "ct", DirectHTTP: true},
	// 联通 CU
	{Host: "beijing.unicomtest.com:8080", Label: "北京联通", ProvCode: "bj", ProvLabel: "北京", Region: "north", ISPGroup: "cu", DirectHTTP: true},
	{Host: "speedtest3.online.tj.cn:8080", Label: "天津联通", ProvCode: "tj", ProvLabel: "天津", Region: "north", ISPGroup: "cu", DirectHTTP: true},
	{Host: "mobile.shunicomtest.com:8080", Label: "上海联通", ProvCode: "sh", ProvLabel: "上海", Region: "east", ISPGroup: "cu", DirectHTTP: true},
	{Host: "cuscspeed.169ol.com:8080", Label: "四川成都联通", ProvCode: "sc", ProvLabel: "四川", Region: "southwest", ISPGroup: "cu", DirectHTTP: true},
	{Host: "mycuspeed.169ol.com:8080", Label: "四川绵阳联通", ProvCode: "sc", ProvLabel: "四川", Region: "southwest", ISPGroup: "cu", DirectHTTP: true},
	// 移动 CM
	{Host: "speedtest.bmcc.com.cn:8080", Label: "北京移动", ProvCode: "bj", ProvLabel: "北京", Region: "north", ISPGroup: "cm", DirectHTTP: true},
	{Host: "speedtest.jsqiuying.com:8080", Label: "江苏苏州移动", ProvCode: "js", ProvLabel: "江苏", Region: "east", ISPGroup: "cm", DirectHTTP: true},
	{Host: "csfw.fj.chinamobile.com:8080", Label: "福建福州移动", ProvCode: "fj", ProvLabel: "福建", Region: "east", ISPGroup: "cm", DirectHTTP: true},
	{Host: "speedtest1.sc.chinamobile.com:8080", Label: "四川成都移动", ProvCode: "sc", ProvLabel: "四川", Region: "southwest", ISPGroup: "cm", DirectHTTP: true},
}

func normalizeSpeedtestDownloadURL(raw string) string {
	cleaned, err := validateCustomSpeedtestTargetURL(raw, false)
	if err != nil || cleaned == "" {
		return ""
	}
	u, err := url.Parse(cleaned)
	if err != nil {
		return ""
	}
	trimmedPath := strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(trimmedPath, "/download") && u.RawQuery == "" {
		u.RawQuery = "size=" + strconv.Itoa(defaultSpeedtestDownloadBytes)
	}
	return u.String()
}

func makeHTTPSiblingForOoklaPort(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme != "https" {
		return ""
	}
	port := u.Port()
	if port != "8080" && port != "8088" {
		return ""
	}
	if strings.HasSuffix(strings.ToLower(u.Hostname()), ".ooklaserver.net") {
		return ""
	}
	clone := *u
	clone.Scheme = "http"
	return clone.String()
}

func buildEmbeddedChinaCatalog() []speedtestCatalogNode {
	out := make([]speedtestCatalogNode, 0, len(embeddedChinaSpeedtestCatalog))
	for _, n := range embeddedChinaSpeedtestCatalog {
		item := n
		item.DownloadURL = normalizeSpeedtestDownloadURL("http://" + item.Host + "/download")
		item.UploadURL = "http://" + item.Host + "/upload"
		item.Method = http.MethodPost
		out = append(out, item)
	}
	return out
}

// ---------------------------------------------------------------------------
// DTOs shared by Panel API and Edge Agent Heartbeat

type speedtestCandidate struct {
	Label       string `json:"label"`
	ResolvedIP  string `json:"resolved_ip,omitempty"`
	UploadURL   string `json:"upload_url,omitempty"`
	DownloadURL string `json:"download_url,omitempty"`
	Method      string `json:"method,omitempty"`
}

type agentSpeedtestTarget struct {
	TargetKey   string               `json:"target_key"`
	Label       string               `json:"label"`
	Province    string               `json:"province"`
	ISPGroup    string               `json:"isp_group"`
	ISPLabel    string               `json:"isp_label"`
	TargetKind  string               `json:"target_kind"` // builtin | apple | custom
	ResolvedIP  string               `json:"resolved_ip,omitempty"`
	UploadURL   string               `json:"upload_url,omitempty"`
	DownloadURL string               `json:"download_url,omitempty"`
	Method      string               `json:"method,omitempty"`
	DurationMs  int                  `json:"duration_ms,omitempty"`
	IsAppleCDN  bool                 `json:"is_apple_cdn,omitempty"`
	Candidates  []speedtestCandidate `json:"candidates,omitempty"`
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
	ResolvedNode  string  `json:"resolved_node,omitempty"`
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
	Version     int    `json:"v"`
	Kind        string `json:"k"`
	JTI         string `json:"jti"`
	NodeID      int64  `json:"n"`
	MaxBytes    int64  `json:"mb"`
	ExpiresAt   int64  `json:"exp"`
	PanelOrigin string `json:"po,omitempty"`
	ReportID    string `json:"rid,omitempty"`
	ReportToken string `json:"rtok,omitempty"`
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

func sanitizeSpeedtestOrigin(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > 256 || strings.ContainsAny(trimmed, "\r\n\x00'\"<>") {
		return ""
	}
	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return ""
	}
	if u.Path != "" && u.Path != "/" {
		return ""
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + u.Host
}

func sealEdgeSpeedtestTicket(token string, nodeID int64, maxBytes int64) (string, error) {
	return sealEdgeSpeedtestTicketWithReport(token, nodeID, maxBytes, "", "", "")
}

func sealEdgeSpeedtestTicketWithReport(token string, nodeID int64, maxBytes int64, panelOrigin, reportID, reportToken string) (string, error) {
	if token == "" || nodeID <= 0 {
		return "", errors.New("edge node credentials unavailable")
	}
	if maxBytes <= 0 || maxBytes > maxDirectStreamBytes {
		maxBytes = maxDirectStreamBytes
	}
	ticket := edgeSpeedtestTicket{
		Version:     1,
		Kind:        "speedtest_direct",
		JTI:         newSpeedtestRandomHex(8),
		NodeID:      nodeID,
		MaxBytes:    maxBytes,
		ExpiresAt:   time.Now().Add(edgeSpeedtestTicketTTL).Unix(),
		PanelOrigin: sanitizeSpeedtestOrigin(panelOrigin),
		ReportID:    strings.TrimSpace(reportID),
		ReportToken: strings.TrimSpace(reportToken),
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
	nonceBytes := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonceBytes, nonceBytes, plain, nil)
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
	if !addr.IsValid() || addr.Zone() != "" {
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

func extractRemoteIPString(conn net.Conn) string {
	if conn == nil || conn.RemoteAddr() == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		host = conn.RemoteAddr().String()
	}
	if addr, err := netip.ParseAddr(strings.TrimSpace(host)); err == nil {
		return addr.Unmap().String()
	}
	return strings.TrimSpace(host)
}

type speedtestTrackedConn struct {
	net.Conn
	dialRTTMs int
	remoteIP  string

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
		remoteIP:  extractRemoteIPString(conn),
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
	windowDone bool
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
	if s.totalBytes >= s.maxBytes {
		s.lastAt = now
		return 0, io.EOF
	}
	if now.After(s.deadline) {
		s.windowDone = true
		s.lastAt = now
		return 0, errUploadWindowReached
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

func (s *timedUploadStream) sentBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalBytes
}

func (s *timedUploadStream) hitWindow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.windowDone
}

func (s *timedUploadStream) elapsed() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return 0
	}
	end := s.lastAt
	if end.IsZero() {
		end = time.Now()
	}
	if end.Before(s.startAt) {
		return 0
	}
	return end.Sub(s.startAt)
}

func (s *timedUploadStream) wallElapsed(endAt time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || endAt.Before(s.startAt) {
		return 0
	}
	return endAt.Sub(s.startAt)
}

func (s *timedUploadStream) computeBps(httpDoneAt time.Time) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.totalBytes <= 0 {
		return 0
	}
	endAt := httpDoneAt
	if s.windowDone && !s.lastAt.IsZero() && s.lastAt.After(s.startAt) {
		endAt = s.lastAt
	} else if !s.lastAt.IsZero() && s.lastAt.After(s.startAt) && endAt.Sub(s.lastAt) > 2*time.Second {
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
		Timeout:   4 * time.Second,
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
			TLSHandshakeTimeout:   4 * time.Second,
			ResponseHeaderTimeout: 6 * time.Second,
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
						return nil, errors.New("拒绝连接非公网目标 IP")
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
						if !isPublicSpeedtestIP(candidate) {
							return nil, errors.New("目标域名解析包含非公网或保留 IP，已阻止连接")
						}
						if !targetAddr.IsValid() {
							targetAddr = candidate.Unmap()
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

func conciseSpeedtestError(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) > 120 {
		return string(runes[:117]) + "..."
	}
	return s
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

	if len(target.Candidates) > 0 {
		rowBudget := max(min(dur*3+12*time.Second, 30*time.Second), 12*time.Second)
		rowCtx, rowCancel := context.WithTimeout(ctx, rowBudget)
		defer rowCancel()

		var bestPartial *speedtestTargetResult
		var lastErrRes speedtestTargetResult
		tried := 0
		for _, cand := range target.Candidates {
			if ctx.Err() != nil || rowCtx.Err() != nil {
				break
			}
			tried++
			candTarget := target
			candTarget.Candidates = nil
			candTarget.ResolvedIP = cand.ResolvedIP
			if cand.UploadURL != "" {
				candTarget.UploadURL = cand.UploadURL
			}
			if cand.DownloadURL != "" {
				candTarget.DownloadURL = cand.DownloadURL
			}
			if cand.Method != "" {
				candTarget.Method = cand.Method
			}
			res := executeSingleSpeedtestCandidate(rowCtx, candTarget, dur, transportOverride)
			if cand.Label != "" {
				res.ResolvedNode = cand.Label
			}
			if res.Status == "done" {
				return res
			}
			if bestPartial == nil && (res.ReturnBps > 0 || res.OutboundBps > 0) {
				cp := res
				bestPartial = &cp
			}
			lastErrRes = res
		}
		if bestPartial != nil {
			return *bestPartial
		}
		if lastErrRes.TargetKey != "" {
			if tried > 1 && lastErrRes.Error != "" {
				lastErrRes.Error = fmt.Sprintf("已尝试 %d 个节点均失败（%s）", tried, conciseSpeedtestError(lastErrRes.Error))
			}
			return lastErrRes
		}
	}

	res := executeSingleSpeedtestCandidate(ctx, target, dur, transportOverride)
	if target.TargetKind == "apple" && res.ResolvedNode == "" {
		res.ResolvedNode = appleSpeedtestHost
	}
	return res
}

func executeSingleSpeedtestCandidate(ctx context.Context, target agentSpeedtestTarget, dur time.Duration, transportOverride *http.Transport) speedtestTargetResult {
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
	if strings.TrimSpace(target.UploadURL) == "" && strings.TrimSpace(target.DownloadURL) == "" {
		res.Status = "error"
		res.Error = "无可用测速地址"
		return res
	}

	var uploadErr error
	if strings.TrimSpace(target.UploadURL) != "" {
		bps, retrans, rttMs, remoteIP, err := measureSingleUpload(ctx, target, dur, transportOverride)
		res.ReturnBps = bps
		res.ReturnMbps = roundMbps(bps)
		res.ReturnRetrans = retrans
		res.ReturnRTTMs = rttMs
		if res.ResolvedIP == "" && remoteIP != "" {
			res.ResolvedIP = remoteIP
		}
		uploadErr = err
	}

	var downloadErr error
	if strings.TrimSpace(target.DownloadURL) != "" {
		bps, rttMs, remoteIP, err := measureSingleDownload(ctx, target, dur, transportOverride)
		res.OutboundBps = bps
		res.OutboundMbps = roundMbps(bps)
		res.OutboundRTTMs = rttMs
		if res.ResolvedIP == "" && remoteIP != "" {
			res.ResolvedIP = remoteIP
		}
		downloadErr = err
	}

	if uploadErr != nil || downloadErr != nil {
		res.Status = "error"
		if uploadErr != nil && downloadErr != nil {
			res.Error = conciseSpeedtestError(uploadErr.Error())
		} else if uploadErr != nil && strings.TrimSpace(target.DownloadURL) == "" {
			res.Error = conciseSpeedtestError(uploadErr.Error())
		} else if downloadErr != nil && strings.TrimSpace(target.UploadURL) == "" {
			res.Error = conciseSpeedtestError(downloadErr.Error())
		} else if uploadErr != nil {
			res.Error = "回程失败: " + conciseSpeedtestError(uploadErr.Error())
		} else {
			res.Error = "去程失败: " + conciseSpeedtestError(downloadErr.Error())
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

func measureSingleUpload(ctx context.Context, target agentSpeedtestTarget, dur time.Duration, transportOverride *http.Transport) (bps int64, retrans int, rttMs int, remoteIP string, err error) {
	retrans = -1
	rttMs = -1
	reqCtx, cancel := context.WithTimeout(ctx, dur+10*time.Second)
	defer cancel()

	method := strings.ToUpper(strings.TrimSpace(target.Method))
	if method != http.MethodPut {
		method = http.MethodPost
	}
	plannedBytes := int64(64 << 20)
	if dur < time.Second {
		plannedBytes = int64(4 << 20)
	}
	bodyStream := newTimedUploadReaderExact(dur, plannedBytes)
	req, err := http.NewRequestWithContext(reqCtx, method, target.UploadURL, bodyStream)
	if err != nil {
		return 0, -1, -1, "", err
	}
	req.ContentLength = plannedBytes
	req.Header.Set("Content-Type", "application/octet-stream")
	if target.IsAppleCDN {
		req.Header.Set("User-Agent", appleSpeedtestUserAgent)
		req.Header.Set("Upload-Draft-Interop-Version", "6")
		req.Header.Set("Upload-Complete", "?1")
	} else {
		req.Header.Set("User-Agent", speedtestBrowserUserAgent)
	}

	targetHost := targetHostnameForURL(target.UploadURL)
	client, getConn := newSpeedtestSingleConnHTTPClient(target.ResolvedIP, targetHost, dur+10*time.Second, transportOverride)
	resp, doErr := client.Do(req)
	endAt := time.Now()

	if resp != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		if tc := getConn(); tc != nil {
			tc.capture()
		}
		_ = resp.Body.Close()
	}
	if tc := getConn(); tc != nil {
		tc.capture()
		remoteIP = tc.remoteIP
		rRetrans, rRTT, hasInfo := tc.stats()
		if hasInfo {
			retrans = rRetrans
			rttMs = rRTT
		} else {
			rttMs = tc.dialRTTMs
		}
	}

	sent := bodyStream.sentBytes()
	elapsed := bodyStream.elapsed()
	minBytes := minValidUploadBytes
	if dur < time.Second {
		minBytes = minFastTestUploadBytes
	}
	validWindow := doErr != nil && ctx.Err() == nil &&
		(errors.Is(doErr, errUploadWindowReached) || bodyStream.hitWindow()) &&
		sent >= minBytes &&
		elapsed >= (dur*3)/4

	if !validWindow {
		if doErr != nil {
			return 0, retrans, rttMs, remoteIP, doErr
		}
		if resp == nil {
			return 0, retrans, rttMs, remoteIP, errors.New("未收到服务器响应")
		}
		minElapsed413 := min(dur/2, 250*time.Millisecond)
		elapsed413 := elapsed
		if sent >= bodyStream.maxBytes {
			if wall := bodyStream.wallElapsed(endAt); wall > elapsed413 {
				elapsed413 = wall
			}
		}
		if resp.StatusCode == http.StatusRequestEntityTooLarge && sent >= minBytes && elapsed413 >= minElapsed413 {
			// Accept partial prefix uploaded over sustained duration before HTTP 413 payload limit (e.g. Apple CDN).
		} else if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return 0, retrans, rttMs, remoteIP, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
	}

	bps = bodyStream.computeBps(endAt)
	if bps <= 0 {
		return 0, retrans, rttMs, remoteIP, errors.New("上传零字节")
	}
	return bps, retrans, rttMs, remoteIP, nil
}

func measureSingleDownload(ctx context.Context, target agentSpeedtestTarget, dur time.Duration, transportOverride *http.Transport) (bps int64, rttMs int, remoteIP string, err error) {
	rttMs = -1
	reqCtx, cancel := context.WithTimeout(ctx, dur+10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target.DownloadURL, nil)
	if err != nil {
		return 0, -1, "", err
	}
	if target.IsAppleCDN {
		req.Header.Set("User-Agent", appleSpeedtestUserAgent)
	} else {
		req.Header.Set("User-Agent", speedtestBrowserUserAgent)
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Cache-Control", "no-store")

	targetHost := targetHostnameForURL(target.DownloadURL)
	client, getConn := newSpeedtestSingleConnHTTPClient(target.ResolvedIP, targetHost, dur+10*time.Second, transportOverride)
	resp, doErr := client.Do(req)
	if doErr != nil {
		if tc := getConn(); tc != nil {
			remoteIP = tc.remoteIP
		}
		return 0, -1, remoteIP, doErr
	}
	if resp.StatusCode >= 400 {
		if tc := getConn(); tc != nil {
			tc.capture()
			remoteIP = tc.remoteIP
			_, infoRTT, hasInfo := tc.stats()
			if hasInfo {
				rttMs = infoRTT
			} else {
				rttMs = tc.dialRTTMs
			}
		}
		_ = resp.Body.Close()
		return 0, rttMs, remoteIP, fmt.Errorf("HTTP %d", resp.StatusCode)
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
		remoteIP = tc.remoteIP
		_, infoRTT, hasInfo := tc.stats()
		if hasInfo {
			rttMs = infoRTT
		} else {
			rttMs = tc.dialRTTMs
		}
	}
	_ = resp.Body.Close()

	if transferErr != nil {
		return 0, rttMs, remoteIP, transferErr
	}
	if firstAt.IsZero() || totalRead == 0 {
		return 0, rttMs, remoteIP, errors.New("空响应")
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
	return bps, rttMs, remoteIP, nil
}

// ---------------------------------------------------------------------------
// Public China 3-ISP Speedtest Catalog (remote refresh + embedded fallback)

func csvFieldValue(row []string, idx int) string {
	if idx < 0 || idx >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[idx])
}

func formatCatalogNodeLabel(provLabel, city, ispGroup string) string {
	ispLbl := ispGroupLabel(ispGroup)
	p := strings.TrimSpace(provLabel)
	c := strings.TrimSpace(city)
	if p == "" {
		if c != "" {
			return c + ispLbl
		}
		return ispLbl
	}
	if c == "" || strings.Contains(strings.ToLower(p), strings.ToLower(c)) || strings.Contains(strings.ToLower(c), strings.ToLower(p)) {
		return p + ispLbl
	}
	// Skip appending ASCII city names when province label is already clear.
	hasNonASCII := false
	for _, r := range c {
		if r > 127 {
			hasNonASCII = true
			break
		}
	}
	if !hasNonASCII {
		return p + ispLbl
	}
	return p + c + " · " + ispLbl
}

func appendNormalizedCatalogVariants(dst []speedtestCatalogNode, base speedtestCatalogNode, dlRaw, ulRaw string) []speedtestCatalogNode {
	dlNorm := normalizeSpeedtestDownloadURL(dlRaw)
	ulClean, ulErr := validateCustomSpeedtestTargetURL(ulRaw, false)
	if dlNorm == "" || ulErr != nil || ulClean == "" {
		return dst
	}
	if httpDL, httpUL := makeHTTPSiblingForOoklaPort(dlNorm), makeHTTPSiblingForOoklaPort(ulClean); httpDL != "" && httpUL != "" {
		httpNode := base
		httpNode.DownloadURL = normalizeSpeedtestDownloadURL(httpDL)
		httpNode.UploadURL = httpUL
		httpNode.Method = http.MethodPost
		httpNode.DirectHTTP = true
		dst = append(dst, httpNode)
	}
	node := base
	node.DownloadURL = dlNorm
	node.UploadURL = ulClean
	node.Method = http.MethodPost
	lowerDL := strings.ToLower(dlNorm)
	node.DirectHTTP = strings.HasPrefix(lowerDL, "http://") && !strings.Contains(lowerDL, ".ooklaserver.net")
	return append(dst, node)
}

func parseSpeedtestCatalogCSV(data []byte) []speedtestCatalogNode {
	if len(data) == 0 {
		return nil
	}
	r := csv.NewReader(strings.NewReader(string(data)))
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil || len(header) == 0 {
		return nil
	}
	cols := make(map[string]int, len(header))
	for i, name := range header {
		key := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(name, "\ufeff")))
		cols[key] = i
	}
	idxOf := func(name string) int {
		if idx, ok := cols[name]; ok {
			return idx
		}
		return -1
	}
	activeIdx := idxOf("active")
	verIdx := idxOf("ver")
	opIdx := idxOf("operator")
	provIdx := idxOf("province")
	cityIdx := idxOf("city")
	hostIdx := idxOf("host")
	dlIdx := idxOf("downloadurl")
	ulIdx := idxOf("uploadurl")
	if verIdx < 0 || opIdx < 0 || dlIdx < 0 || ulIdx < 0 {
		return nil
	}

	var out []speedtestCatalogNode
	for {
		rec, readErr := r.Read()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			continue
		}
		if activeIdx >= 0 && csvFieldValue(rec, activeIdx) != "1" {
			continue
		}
		if csvFieldValue(rec, verIdx) != "1" {
			continue
		}
		isp := mapSpeedtestOperator(csvFieldValue(rec, opIdx))
		if isp == "" {
			continue
		}
		provRaw := csvFieldValue(rec, provIdx)
		cityRaw := csvFieldValue(rec, cityIdx)
		provCode, provLabel, region := normalizeSpeedtestCatalogLocation(provRaw, cityRaw)
		if provCode == "" {
			continue
		}
		base := speedtestCatalogNode{
			Host:      csvFieldValue(rec, hostIdx),
			Label:     formatCatalogNodeLabel(provLabel, cityRaw, isp),
			ProvCode:  provCode,
			ProvLabel: provLabel,
			Region:    region,
			ISPGroup:  isp,
		}
		out = appendNormalizedCatalogVariants(out, base, csvFieldValue(rec, dlIdx), csvFieldValue(rec, ulIdx))
	}
	return out
}

func parseSpeedtestCatalogJSON(data []byte) []speedtestCatalogNode {
	if len(data) == 0 {
		return nil
	}
	var items []struct {
		Name     string `json:"name"`
		Host     string `json:"host"`
		URL      string `json:"url"`
		Provider string `json:"provider"`
		Country  string `json:"country"`
		City     string `json:"city"`
	}
	if err := json.Unmarshal(data, &items); err != nil {
		return nil
	}
	var out []speedtestCatalogNode
	for _, item := range items {
		if !strings.EqualFold(strings.TrimSpace(item.Country), "China") {
			continue
		}
		isp := mapSpeedtestOperator(item.Provider)
		if isp == "" {
			continue
		}
		provCode, provLabel, region := normalizeSpeedtestCatalogLocation(item.Name, item.City)
		if provCode == "" {
			continue
		}
		ulRaw := strings.TrimSpace(item.URL)
		if ulRaw == "" && strings.TrimSpace(item.Host) != "" {
			ulRaw = "http://" + strings.TrimSpace(item.Host) + "/upload"
		}
		u, err := url.Parse(ulRaw)
		if err != nil || u.Host == "" {
			continue
		}
		port := u.Port()
		if port != "8080" && port != "8088" {
			continue
		}
		dlURL := url.URL{
			Scheme:   u.Scheme,
			Host:     u.Host,
			Path:     "/download",
			RawQuery: "size=" + strconv.Itoa(defaultSpeedtestDownloadBytes),
		}
		base := speedtestCatalogNode{
			Host:      strings.TrimSpace(item.Host),
			Label:     formatCatalogNodeLabel(provLabel, item.City, isp),
			ProvCode:  provCode,
			ProvLabel: provLabel,
			Region:    region,
			ISPGroup:  isp,
		}
		out = appendNormalizedCatalogVariants(out, base, dlURL.String(), ulRaw)
	}
	return out
}

func catalogPriorityRank(n speedtestCatalogNode) int {
	if n.DirectHTTP {
		return 0
	}
	if strings.HasPrefix(strings.ToLower(n.DownloadURL), "http://") {
		return 1
	}
	return 2
}

func mergeSpeedtestCatalogNodes(remote, fallback []speedtestCatalogNode) []speedtestCatalogNode {
	combined := make([]speedtestCatalogNode, 0, len(remote)+len(fallback))
	combined = append(combined, remote...)
	combined = append(combined, fallback...)

	seen := make(map[string]bool, len(combined))
	deduped := make([]speedtestCatalogNode, 0, len(combined))
	for _, n := range combined {
		key := strings.ToLower(n.DownloadURL) + "|" + strings.ToLower(n.UploadURL)
		if n.DownloadURL == "" || n.UploadURL == "" || seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, n)
	}
	sort.SliceStable(deduped, func(i, j int) bool {
		return catalogPriorityRank(deduped[i]) < catalogPriorityRank(deduped[j])
	})
	return deduped
}

func selectSpeedtestCandidates(catalog []speedtestCatalogNode, prov speedtestProvinceDef, ispGroup string) []speedtestCandidate {
	out := make([]speedtestCandidate, 0, maxSpeedtestCandidatesPerNode)
	seen := make(map[string]bool, maxSpeedtestCandidatesPerNode)
	appendNode := func(n speedtestCatalogNode) bool {
		key := n.DownloadURL + "|" + n.UploadURL
		if seen[key] {
			return false
		}
		seen[key] = true
		out = append(out, speedtestCandidate{
			Label:       n.Label,
			UploadURL:   n.UploadURL,
			DownloadURL: n.DownloadURL,
			Method:      n.Method,
		})
		return len(out) >= maxSpeedtestCandidatesPerNode
	}

	// Tier 1: Exact province + ISP matches.
	for _, n := range catalog {
		if n.ISPGroup == ispGroup && n.ProvCode == prov.Code {
			if appendNode(n) {
				return out
			}
		}
	}
	// Tier 2: Regional fallback in geographic proximity order.
	regions := speedtestRegionFallbackOrder[prov.Region]
	if len(regions) == 0 {
		regions = []string{"east", "north", "central", "south", "southwest", "northwest"}
	}
	for _, reg := range regions {
		for _, n := range catalog {
			if n.ISPGroup == ispGroup && n.Region == reg {
				if appendNode(n) {
					return out
				}
			}
		}
	}
	// Tier 3: Remaining nationwide matches for this ISP.
	for _, n := range catalog {
		if n.ISPGroup == ispGroup {
			if appendNode(n) {
				return out
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Master Speedtest Manager

type directSpeedtestReportState struct {
	ID            string    `json:"id"`
	Token         string    `json:"-"`
	NodeID        int64     `json:"node_id"`
	Status        string    `json:"status"` // idle | running | done | error
	TTFBMs        int       `json:"ttfb_ms"`
	Mbps          float64   `json:"mbps"`
	ReceivedBytes int64     `json:"received_bytes"`
	Error         string    `json:"error,omitempty"`
	Terminal      bool      `json:"terminal,omitempty"`
	ExpiresAt     time.Time `json:"-"`
}

type speedtestManager struct {
	db              *DB
	pm              *ProxyManager
	catalogClient   *http.Client
	catalogCSVURL   string
	catalogJSONURL  string
	targetTransport *http.Transport
	phaseDuration   time.Duration
	customTargetMu  sync.Mutex

	mu                sync.Mutex
	catalogNodes      []speedtestCatalogNode
	catalogUpdated    time.Time
	catalogExpiresAt  time.Time
	catalogRefreshing bool
	catalogWaitCh     chan struct{}
	directReports     map[string]*directSpeedtestReportState
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
		catalogClient: &http.Client{
			Transport: tr,
			Timeout:   5 * time.Second,
		},
		catalogCSVURL:     defaultSpeedtestCatalogCSVURL,
		catalogJSONURL:    defaultSpeedtestCatalogJSONURL,
		phaseDuration:     defaultSpeedtestPhaseDuration,
		directReports:     make(map[string]*directSpeedtestReportState),
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

func fetchCatalogBytes(ctx context.Context, client *http.Client, rawURL string) []byte {
	if client == nil {
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", speedtestBrowserUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil
	}
	return body
}

func (m *speedtestManager) catalogSnapshot() (int, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.catalogNodes) > 0 {
		updated := ""
		if !m.catalogUpdated.IsZero() {
			updated = m.catalogUpdated.UTC().Format(time.RFC3339)
		}
		return len(m.catalogNodes), updated
	}
	return len(embeddedChinaSpeedtestCatalog), ""
}

func (m *speedtestManager) loadCatalog(ctx context.Context) []speedtestCatalogNode {
	for {
		now := time.Now()
		m.mu.Lock()
		if len(m.catalogNodes) > 0 && now.Before(m.catalogExpiresAt) {
			cached := append([]speedtestCatalogNode(nil), m.catalogNodes...)
			m.mu.Unlock()
			return cached
		}
		if m.catalogRefreshing && m.catalogWaitCh != nil {
			waitCh := m.catalogWaitCh
			m.mu.Unlock()
			select {
			case <-waitCh:
				continue
			case <-ctx.Done():
				m.mu.Lock()
				cached := append([]speedtestCatalogNode(nil), m.catalogNodes...)
				m.mu.Unlock()
				if len(cached) > 0 {
					return cached
				}
				return buildEmbeddedChinaCatalog()
			}
		}
		m.catalogRefreshing = true
		waitCh := make(chan struct{})
		m.catalogWaitCh = waitCh
		client := m.catalogClient
		csvURL := m.catalogCSVURL
		jsonURL := m.catalogJSONURL
		m.mu.Unlock()

		var csvData, jsonData []byte
		var wg sync.WaitGroup
		if strings.TrimSpace(csvURL) != "" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				csvData = fetchCatalogBytes(ctx, client, csvURL)
			}()
		}
		if strings.TrimSpace(jsonURL) != "" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				jsonData = fetchCatalogBytes(ctx, client, jsonURL)
			}()
		}
		wg.Wait()

		var remote []speedtestCatalogNode
		if len(csvData) > 0 {
			remote = append(remote, parseSpeedtestCatalogCSV(csvData)...)
		}
		if len(jsonData) > 0 {
			remote = append(remote, parseSpeedtestCatalogJSON(jsonData)...)
		}

		embedded := buildEmbeddedChinaCatalog()
		now = time.Now()

		m.mu.Lock()
		var result []speedtestCatalogNode
		if len(remote) > 0 {
			result = mergeSpeedtestCatalogNodes(remote, embedded)
			m.catalogNodes = result
			m.catalogUpdated = now
			m.catalogExpiresAt = now.Add(speedtestCatalogTTL)
		} else if len(m.catalogNodes) > 0 && !m.catalogUpdated.IsZero() {
			result = m.catalogNodes
			m.catalogExpiresAt = now.Add(speedtestCatalogRetryTTL)
		} else {
			result = embedded
			m.catalogNodes = result
			m.catalogExpiresAt = now.Add(speedtestCatalogRetryTTL)
		}
		m.catalogRefreshing = false
		m.catalogWaitCh = nil
		close(waitCh)
		out := append([]speedtestCatalogNode(nil), result...)
		m.mu.Unlock()
		return out
	}
}

func (m *speedtestManager) createDirectReportSession(nodeID int64) (string, string) {
	now := time.Now()
	id := "dr_" + newSpeedtestRandomHex(12)
	token := newSpeedtestRandomHex(16)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.directReports == nil {
		m.directReports = make(map[string]*directSpeedtestReportState)
	}
	for k, v := range m.directReports {
		if now.After(v.ExpiresAt) {
			delete(m.directReports, k)
		}
	}
	for len(m.directReports) >= maxDirectReportSessions {
		var evictKey string
		for k, v := range m.directReports {
			evictKey = k
			if v.Terminal {
				break
			}
		}
		if evictKey == "" {
			break
		}
		delete(m.directReports, evictKey)
	}
	m.directReports[id] = &directSpeedtestReportState{
		ID:        id,
		Token:     token,
		NodeID:    nodeID,
		Status:    "idle",
		TTFBMs:    -1,
		ExpiresAt: now.Add(edgeSpeedtestTicketTTL + time.Minute),
	}
	return id, token
}

func (m *speedtestManager) updateDirectReport(reportID, reportToken, phase string, ttfbMs int, mbps float64, receivedBytes int64, errMsg string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	rep := m.directReports[strings.TrimSpace(reportID)]
	if rep == nil || time.Now().After(rep.ExpiresAt) {
		return http.StatusNotFound
	}
	if !hmac.Equal([]byte(reportToken), []byte(rep.Token)) {
		return http.StatusUnauthorized
	}
	if rep.Terminal {
		return http.StatusConflict
	}
	if math.IsNaN(mbps) || math.IsInf(mbps, 0) || mbps < 0 || mbps > 100000 {
		return http.StatusBadRequest
	}
	if ttfbMs < -1 || ttfbMs > 60000 {
		return http.StatusBadRequest
	}
	if receivedBytes < 0 || receivedBytes > maxDirectStreamBytes {
		return http.StatusBadRequest
	}

	switch phase {
	case "progress":
		rep.Status = "running"
		rep.TTFBMs = ttfbMs
		rep.Mbps = mbps
		rep.ReceivedBytes = receivedBytes
	case "done":
		rep.TTFBMs = ttfbMs
		rep.Mbps = mbps
		rep.ReceivedBytes = receivedBytes
		if receivedBytes <= 0 || ttfbMs <= 0 {
			rep.Status = "error"
			rep.Error = "未收到有效测速字节"
		} else {
			rep.Status = "done"
			rep.Error = ""
		}
		rep.Terminal = true
	case "error":
		rep.Status = "error"
		rep.TTFBMs = ttfbMs
		rep.Mbps = mbps
		rep.ReceivedBytes = receivedBytes
		rep.Error = conciseSpeedtestError(errMsg)
		rep.Terminal = true
	default:
		return http.StatusBadRequest
	}
	return http.StatusOK
}

func (m *speedtestManager) getDirectReport(reportID string) (directSpeedtestReportState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rep := m.directReports[strings.TrimSpace(reportID)]
	if rep == nil || time.Now().After(rep.ExpiresAt) {
		return directSpeedtestReportState{}, false
	}
	return *rep, true
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

	// Reserve admission slot BEFORE remote catalog fetch or target expansion.
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
		catalogCtx, catalogCancel := context.WithTimeout(context.Background(), 6*time.Second)
		catalog := m.loadCatalog(catalogCtx)
		catalogCancel()

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
				cands := selectSpeedtestCandidates(catalog, def, g)
				if len(cands) == 0 {
					continue
				}
				ispLbl := ispGroupLabel(g)
				first := cands[0]
				targets = append(targets, agentSpeedtestTarget{
					TargetKey:   def.Code + "-" + g,
					Label:       def.Label + ispLbl,
					Province:    def.Label,
					ISPGroup:    g,
					ISPLabel:    ispLbl,
					TargetKind:  "builtin",
					ResolvedIP:  first.ResolvedIP,
					UploadURL:   first.UploadURL,
					DownloadURL: first.DownloadURL,
					Method:      first.Method,
					DurationMs:  durationMs,
					Candidates:  cands,
				})
			}
			if len(targets) >= maxSpeedtestTargetsPerJob {
				break
			}
		}
	}

	if mode == "apple" {
		targets = append(targets, agentSpeedtestTarget{
			TargetKey:   "apple-default",
			Label:       "Apple CDN 就近节点",
			Province:    "默认路由",
			ISPGroup:    "other",
			ISPLabel:    "就近",
			TargetKind:  "apple",
			UploadURL:   appleSpeedtestUploadURL,
			DownloadURL: appleSpeedtestDownloadURL,
			Method:      http.MethodPut,
			DurationMs:  durationMs,
			IsAppleCDN:  true,
		})
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
		resolvedNode := ""
		if len(tgt.Candidates) > 0 {
			resolvedNode = tgt.Candidates[0].Label
		}
		initialResults[i] = speedtestTargetResult{
			TargetKey:     tgt.TargetKey,
			Label:         tgt.Label,
			Province:      tgt.Province,
			ISPGroup:      tgt.ISPGroup,
			ISPLabel:      tgt.ISPLabel,
			TargetKind:    tgt.TargetKind,
			ResolvedNode:  resolvedNode,
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

	catalogCount, catalogUpdatedAt := a.speedtest.catalogSnapshot()
	a.jsonOK(w, map[string]any{
		"provinces":          provs,
		"isps":               isps,
		"default_provinces":  defaultSpeedtestProvinces,
		"default_isps":       defaultSpeedtestISPs,
		"custom_targets":     custom,
		"nodes":              nodes,
		"catalog_count":      catalogCount,
		"catalog_updated_at": catalogUpdatedAt,
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
	if mode != "custom" && mode != "all" && mode != "apple" {
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
		PopupURL          string `json:"popup_url,omitempty"`
		ReportID          string `json:"report_id,omitempty"`
		SupportsPopup     bool   `json:"supports_popup"`
		SizeBytes         int64  `json:"size_bytes"`
		UnavailableReason string `json:"unavailable_reason,omitempty"`
	}

	var nodeFilter int64
	hasNodeFilter := false
	if raw := strings.TrimSpace(r.URL.Query().Get("node_id")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed >= 0 {
			nodeFilter = parsed
			hasNodeFilter = true
		}
	}

	panelScheme := "http"
	if a.panelTLSEnabled || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") || r.TLS != nil {
		panelScheme = "https"
	}
	panelOrigin := sanitizeSpeedtestOrigin(r.Header.Get("Origin"))
	if panelOrigin == "" && strings.TrimSpace(r.Host) != "" {
		panelOrigin = sanitizeSpeedtestOrigin(panelScheme + "://" + strings.TrimSpace(r.Host))
	}

	list := make([]directTarget, 0)
	if !hasNodeFilter || nodeFilter == 0 {
		list = append(list, directTarget{
			NodeID:        0,
			Name:          "主机",
			IsMaster:      true,
			Online:        true,
			Scheme:        panelScheme,
			StreamURL:     fmt.Sprintf("/api/speedtest/direct-stream?bytes=%d", defaultDirectStreamBytes),
			SupportsPopup: true,
			SizeBytes:     defaultDirectStreamBytes,
		})
	}

	if a.pm != nil && a.pm.edges != nil {
		now := time.Now()
		a.pm.edges.mu.RLock()
		ids := make([]int64, 0, len(a.pm.edges.nodes))
		for id := range a.pm.edges.nodes {
			if hasNodeFilter && id != nodeFilter {
				continue
			}
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
				NodeID:        node.id,
				Name:          node.name,
				Online:        online,
				PublicURL:     pubURL,
				Scheme:        scheme,
				SupportsPopup: node.supportsDirectPopup,
				SizeBytes:     defaultDirectStreamBytes,
			}
			if !online {
				item.UnavailableReason = "节点离线或未配置访问地址"
			} else {
				var reportID, reportToken string
				if node.supportsDirectPopup && a.speedtest != nil && panelOrigin != "" {
					reportID, reportToken = a.speedtest.createDirectReportSession(node.id)
				}
				if sealed, err := sealEdgeSpeedtestTicketWithReport(node.tokenPlain, node.id, maxDirectStreamBytes, panelOrigin, reportID, reportToken); err != nil {
					item.Online = false
					item.UnavailableReason = "签发测速令牌失败"
				} else {
					base := strings.TrimRight(pubURL, "/")
					item.StreamURL = fmt.Sprintf("%s%s%s?bytes=%d", base, edgeSpeedtestPathPrefix, sealed, defaultDirectStreamBytes)
					if node.supportsDirectPopup {
						item.PopupURL = fmt.Sprintf("%s%s%s?bytes=%d", base, edgeSpeedtestUIPathPrefix, sealed, defaultDirectStreamBytes)
						item.ReportID = reportID
					}
				}
			}
			list = append(list, item)
		}
		a.pm.edges.mu.RUnlock()
	}

	a.jsonOK(w, map[string]any{"targets": list})
}

func (a *App) handleSpeedtestDirectReport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.speedtest == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "speedtest unavailable")
		return
	}
	var req struct {
		ID            string  `json:"id"`
		Token         string  `json:"token"`
		Phase         string  `json:"phase"`
		TTFBMs        int     `json:"ttfb_ms"`
		Mbps          float64 `json:"mbps"`
		ReceivedBytes int64   `json:"received_bytes"`
		Error         string  `json:"error"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		a.jsonErr(w, http.StatusBadRequest, "invalid report payload")
		return
	}
	status := a.speedtest.updateDirectReport(req.ID, req.Token, req.Phase, req.TTFBMs, req.Mbps, req.ReceivedBytes, req.Error)
	if status != http.StatusOK {
		a.jsonErr(w, status, "report rejected")
		return
	}
	a.jsonOK(w, map[string]any{"ok": true})
}

func (a *App) handleSpeedtestDirectReportPoll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.speedtest == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "speedtest unavailable")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		a.jsonErr(w, http.StatusBadRequest, "missing report id")
		return
	}
	rep, ok := a.speedtest.getDirectReport(id)
	if !ok {
		a.jsonErr(w, http.StatusNotFound, "report session not found")
		return
	}
	a.jsonOK(w, map[string]any{"report": rep})
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
