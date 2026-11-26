package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
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

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

const (
	defaultISPProbeIntervalSec         = 30
	minISPProbeIntervalSec             = 10
	maxISPProbeIntervalSec             = 600
	defaultISPProbeRecoveryIntervalSec = 180
	minISPProbeRecoveryIntervalSec     = 30
	maxISPProbeRecoveryIntervalSec     = 3600
	defaultISPProbeFailThreshold       = 3
	defaultISPProbeRecoverThreshold    = 2
	minISPProbeThreshold               = 1
	maxISPProbeThreshold               = 10
	ispProbeDialTimeout                = 3 * time.Second
	ispProbeMaxTargetsPerGroup         = 6

	edgeProbeModeTCP  = "tcp"
	edgeProbeModeICMP = "icmp"
	edgeProbeModeOff  = "off"
)

func normalizeEdgeProbeMode(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", edgeProbeModeTCP:
		return edgeProbeModeTCP, nil
	case edgeProbeModeICMP:
		return edgeProbeModeICMP, nil
	case edgeProbeModeOff:
		return edgeProbeModeOff, nil
	default:
		return "", fmt.Errorf("三网探测方式仅支持 tcp、icmp 或 off")
	}
}

// probeableISPGroups are the three mainland Chinese ISP groups that run active
// TCP/ICMP probing. When all three are tripped simultaneously, the node is
// also treated as tripped in the "other" group.
var probeableISPGroups = []string{ispGroupCT, ispGroupCU, ispGroupCM}

// ispProbeAllowLoopbackInTests lets offline unit tests bind 127.0.0.1 TCP
// listeners as deterministic probe targets. Production code keeps this false.
var ispProbeAllowLoopbackInTests bool

// ispICMPProbeHook allows unit tests to stub ICMP echo probing deterministically.
var ispICMPProbeHook func(ctx context.Context, host string) int

var (
	icmpEchoIDCounter atomic.Int32
	icmpPermWarnOnce  sync.Once
)

func defaultISPProbeTargets() map[string][]string {
	return map[string][]string{
		ispGroupCT: {
			"bj-ct-v4.ip.zstaticcdn.com:80",
			"sh-ct-v4.ip.zstaticcdn.com:80",
			"gd-ct-v4.ip.zstaticcdn.com:80",
		},
		ispGroupCU: {
			"bj-cu-v4.ip.zstaticcdn.com:80",
			"sh-cu-v4.ip.zstaticcdn.com:80",
			"gd-cu-v4.ip.zstaticcdn.com:80",
		},
		ispGroupCM: {
			"bj-cm-v4.ip.zstaticcdn.com:80",
			"sh-cm-v4.ip.zstaticcdn.com:80",
			"gd-cm-v4.ip.zstaticcdn.com:80",
		},
	}
}

func defaultISPICMPTargets() map[string][]string {
	return map[string][]string{
		ispGroupCT: {
			"36.110.147.20",  // 北京电信
			"202.96.209.133", // 上海电信
			"202.96.128.86",  // 广东电信
		},
		ispGroupCU: {
			"123.125.81.6", // 北京联通
			"210.22.97.1",  // 上海联通
			"221.5.88.88",  // 广东联通
		},
		ispGroupCM: {
			"221.179.155.161", // 北京移动
			"211.136.112.200", // 上海移动
			"211.136.192.6",   // 广东移动
		},
	}
}

func inferISPProbeTargetLabel(target string) string {
	host := strings.TrimSpace(target)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	switch {
	case strings.HasPrefix(host, "bj-"), host == "36.110.147.20", host == "123.125.81.6", host == "221.179.155.161":
		return "北京"
	case strings.HasPrefix(host, "sh-"), host == "202.96.209.133", host == "210.22.97.1", host == "211.136.112.200":
		return "上海"
	case strings.HasPrefix(host, "gd-"), host == "202.96.128.86", host == "221.5.88.88", host == "211.136.192.6":
		return "广东"
	default:
		if host != "" {
			return host
		}
		return target
	}
}

func cloneISPProbeTargets(src map[string][]string) map[string][]string {
	out := make(map[string][]string, len(probeableISPGroups))
	for _, g := range probeableISPGroups {
		out[g] = append([]string(nil), src[g]...)
	}
	return out
}

type ISPProbeSettings struct {
	Enabled             bool                `json:"enabled"`
	IntervalSec         int                 `json:"interval_sec"`
	RecoveryIntervalSec int                 `json:"recovery_interval_sec"`
	FailThreshold       int                 `json:"fail_threshold"`
	RecoverThreshold    int                 `json:"recover_threshold"`
	Targets             map[string][]string `json:"targets"`
	ICMPTargets         map[string][]string `json:"icmp_targets"`
}

func defaultISPProbeSettings() ISPProbeSettings {
	return ISPProbeSettings{
		Enabled:             true,
		IntervalSec:         defaultISPProbeIntervalSec,
		RecoveryIntervalSec: defaultISPProbeRecoveryIntervalSec,
		FailThreshold:       defaultISPProbeFailThreshold,
		RecoverThreshold:    defaultISPProbeRecoverThreshold,
		Targets:             defaultISPProbeTargets(),
		ICMPTargets:         defaultISPICMPTargets(),
	}
}

const ispProbeSchemaSQL = `
CREATE TABLE IF NOT EXISTS isp_probe_settings (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	enabled INTEGER NOT NULL DEFAULT 1,
	interval_sec INTEGER NOT NULL DEFAULT 30,
	recovery_interval_sec INTEGER NOT NULL DEFAULT 180,
	fail_threshold INTEGER NOT NULL DEFAULT 3,
	recover_threshold INTEGER NOT NULL DEFAULT 2,
	targets_json TEXT NOT NULL DEFAULT '',
	icmp_targets_json TEXT NOT NULL DEFAULT '',
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
INSERT OR IGNORE INTO isp_probe_settings (id) VALUES (1);
`

func ensureISPProbeSchema(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, ispProbeSchemaSQL); err != nil {
		return err
	}
	var exists int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('isp_probe_settings') WHERE name='icmp_targets_json'").Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE isp_probe_settings ADD COLUMN icmp_targets_json TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
}

func normalizeISPICMPTarget(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "icmp://")
	s = strings.TrimPrefix(s, "tcp://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "https://")
	if slash := strings.IndexByte(s, '/'); slash >= 0 {
		s = s[:slash]
	}
	if s == "" {
		return "", nil
	}
	host := s
	if h, _, err := net.SplitHostPort(s); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSpace(strings.Trim(host, "[]")))
	if host == "" || len(host) > 253 {
		return "", fmt.Errorf("无效的 ICMP 探测目标：%s", raw)
	}
	if addr, parseErr := netip.ParseAddr(host); parseErr == nil {
		addr = addr.Unmap()
		if !ispProbeAllowLoopbackInTests && (addr.IsLoopback() || addr.IsUnspecified() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast()) {
			return "", fmt.Errorf("ICMP 探测目标不能使用本机或保留地址：%s", raw)
		}
		return addr.String(), nil
	}
	if !ispProbeAllowLoopbackInTests && (host == "localhost" || strings.HasSuffix(host, ".localhost")) {
		return "", fmt.Errorf("ICMP 探测目标不能使用本机地址：%s", raw)
	}
	if _, err := normalizePublicHost(host); err != nil {
		return "", fmt.Errorf("ICMP 探测目标主机名无效：%s", raw)
	}
	return host, nil
}

func normalizeISPProbeTarget(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "tcp://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "https://")
	if slash := strings.IndexByte(s, '/'); slash >= 0 {
		s = s[:slash]
	}
	if s == "" {
		return "", nil
	}
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		// Default to port 80 when a bare hostname or IP is supplied.
		host = strings.Trim(s, "[]")
		portStr = "80"
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || len(host) > 253 {
		return "", fmt.Errorf("无效的探测目标：%s", raw)
	}
	port, err := strconv.Atoi(strings.TrimSpace(portStr))
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("探测目标端口无效：%s", raw)
	}
	if addr, parseErr := netip.ParseAddr(host); parseErr == nil {
		addr = addr.Unmap()
		if !ispProbeAllowLoopbackInTests && (addr.IsLoopback() || addr.IsUnspecified() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast()) {
			return "", fmt.Errorf("探测目标不能使用本机或保留地址：%s", raw)
		}
		return net.JoinHostPort(addr.String(), strconv.Itoa(port)), nil
	}
	if !ispProbeAllowLoopbackInTests && (host == "localhost" || strings.HasSuffix(host, ".localhost")) {
		return "", fmt.Errorf("探测目标不能使用本机地址：%s", raw)
	}
	if _, err := normalizePublicHost(host); err != nil {
		return "", fmt.Errorf("探测目标主机名无效：%s", raw)
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func normalizeISPProbeSettings(in ISPProbeSettings) (ISPProbeSettings, error) {
	out := in
	if out.IntervalSec == 0 {
		out.IntervalSec = defaultISPProbeIntervalSec
	}
	if out.IntervalSec < minISPProbeIntervalSec || out.IntervalSec > maxISPProbeIntervalSec {
		return out, fmt.Errorf("正常探测间隔应为 %d–%d 秒", minISPProbeIntervalSec, maxISPProbeIntervalSec)
	}
	if out.RecoveryIntervalSec == 0 {
		out.RecoveryIntervalSec = defaultISPProbeRecoveryIntervalSec
	}
	if out.RecoveryIntervalSec < minISPProbeRecoveryIntervalSec || out.RecoveryIntervalSec > maxISPProbeRecoveryIntervalSec {
		return out, fmt.Errorf("熔断复测间隔应为 %d–%d 秒", minISPProbeRecoveryIntervalSec, maxISPProbeRecoveryIntervalSec)
	}
	if out.FailThreshold == 0 {
		out.FailThreshold = defaultISPProbeFailThreshold
	}
	if out.FailThreshold < minISPProbeThreshold || out.FailThreshold > maxISPProbeThreshold {
		return out, fmt.Errorf("连续失败熔断次数应为 %d–%d 次", minISPProbeThreshold, maxISPProbeThreshold)
	}
	if out.RecoverThreshold == 0 {
		out.RecoverThreshold = defaultISPProbeRecoverThreshold
	}
	if out.RecoverThreshold < minISPProbeThreshold || out.RecoverThreshold > maxISPProbeThreshold {
		return out, fmt.Errorf("连续成功恢复次数应为 %d–%d 次", minISPProbeThreshold, maxISPProbeThreshold)
	}

	defaults := defaultISPProbeTargets()
	cleanTargets := make(map[string][]string, len(probeableISPGroups))
	for _, g := range probeableISPGroups {
		rawList := out.Targets[g]
		if out.Targets == nil {
			rawList = defaults[g]
		}
		seen := make(map[string]bool, len(rawList))
		list := make([]string, 0, len(rawList))
		for _, item := range rawList {
			norm, err := normalizeISPProbeTarget(item)
			if err != nil {
				return out, err
			}
			if norm == "" || seen[norm] {
				continue
			}
			seen[norm] = true
			list = append(list, norm)
			if len(list) > ispProbeMaxTargetsPerGroup {
				return out, fmt.Errorf("%s探测目标不能超过 %d 个", ispGroupLabel(g), ispProbeMaxTargetsPerGroup)
			}
		}
		if out.Enabled && len(list) == 0 {
			return out, fmt.Errorf("启用探测时，%s至少需要配置 1 个探测目标", ispGroupLabel(g))
		}
		cleanTargets[g] = list
	}
	out.Targets = cleanTargets

	icmpDefaults := defaultISPICMPTargets()
	cleanICMPTargets := make(map[string][]string, len(probeableISPGroups))
	for _, g := range probeableISPGroups {
		rawList := out.ICMPTargets[g]
		if out.ICMPTargets == nil {
			rawList = icmpDefaults[g]
		}
		seen := make(map[string]bool, len(rawList))
		list := make([]string, 0, len(rawList))
		for _, item := range rawList {
			norm, err := normalizeISPICMPTarget(item)
			if err != nil {
				return out, err
			}
			if norm == "" || seen[norm] {
				continue
			}
			seen[norm] = true
			list = append(list, norm)
			if len(list) > ispProbeMaxTargetsPerGroup {
				return out, fmt.Errorf("%s ICMP 探测目标不能超过 %d 个", ispGroupLabel(g), ispProbeMaxTargetsPerGroup)
			}
		}
		if out.Enabled && len(list) == 0 {
			return out, fmt.Errorf("启用探测时，%s至少需要配置 1 个 ICMP 探测目标", ispGroupLabel(g))
		}
		cleanICMPTargets[g] = list
	}
	out.ICMPTargets = cleanICMPTargets
	return out, nil
}

func (d *DB) loadISPProbeSettings() ISPProbeSettings {
	def := defaultISPProbeSettings()
	if d == nil || d.db == nil {
		return def
	}
	var (
		enabledInt          int
		intervalSec         int
		recoveryIntervalSec int
		failThreshold       int
		recoverThreshold    int
		targetsJSON         string
		icmpTargetsJSON     string
	)
	err := d.db.QueryRow(`SELECT enabled, interval_sec, recovery_interval_sec, fail_threshold, recover_threshold, targets_json, icmp_targets_json FROM isp_probe_settings WHERE id=1`).
		Scan(&enabledInt, &intervalSec, &recoveryIntervalSec, &failThreshold, &recoverThreshold, &targetsJSON, &icmpTargetsJSON)
	if err != nil {
		return def
	}
	s := ISPProbeSettings{
		Enabled:             enabledInt != 0,
		IntervalSec:         intervalSec,
		RecoveryIntervalSec: recoveryIntervalSec,
		FailThreshold:       failThreshold,
		RecoverThreshold:    recoverThreshold,
		Targets:             defaultISPProbeTargets(),
		ICMPTargets:         defaultISPICMPTargets(),
	}
	if strings.TrimSpace(targetsJSON) != "" {
		var parsed map[string][]string
		if err := json.Unmarshal([]byte(targetsJSON), &parsed); err == nil && len(parsed) > 0 {
			for _, g := range probeableISPGroups {
				if list, ok := parsed[g]; ok {
					s.Targets[g] = list
				}
			}
		}
	}
	if strings.TrimSpace(icmpTargetsJSON) != "" {
		var parsed map[string][]string
		if err := json.Unmarshal([]byte(icmpTargetsJSON), &parsed); err == nil && len(parsed) > 0 {
			for _, g := range probeableISPGroups {
				if list, ok := parsed[g]; ok {
					s.ICMPTargets[g] = list
				}
			}
		}
	}
	norm, err := normalizeISPProbeSettings(s)
	if err != nil {
		return def
	}
	return norm
}

func (d *DB) saveISPProbeSettings(s ISPProbeSettings) error {
	if d == nil || d.db == nil {
		return errors.New("database not initialized")
	}
	targetsBytes, err := json.Marshal(s.Targets)
	if err != nil {
		return err
	}
	icmpTargetsBytes, err := json.Marshal(s.ICMPTargets)
	if err != nil {
		return err
	}
	_, err = d.db.Exec(
		`INSERT INTO isp_probe_settings(id, enabled, interval_sec, recovery_interval_sec, fail_threshold, recover_threshold, targets_json, icmp_targets_json, updated_at)
		 VALUES(1, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(id) DO UPDATE SET
		   enabled=excluded.enabled,
		   interval_sec=excluded.interval_sec,
		   recovery_interval_sec=excluded.recovery_interval_sec,
		   fail_threshold=excluded.fail_threshold,
		   recover_threshold=excluded.recover_threshold,
		   targets_json=excluded.targets_json,
		   icmp_targets_json=excluded.icmp_targets_json,
		   updated_at=CURRENT_TIMESTAMP`,
		sqliteBool(s.Enabled), s.IntervalSec, s.RecoveryIntervalSec, s.FailThreshold, s.RecoverThreshold, string(targetsBytes), string(icmpTargetsBytes),
	)
	return err
}

// ---------------------------------------------------------------------------
// Wire DTOs between Master and Edge Agent

type agentISPProbeConfig struct {
	Enabled      bool                `json:"enabled"`
	Mode         string              `json:"mode,omitempty"`
	IntervalSec  int                 `json:"interval_sec"`
	ForceProbeID string              `json:"force_probe_id,omitempty"`
	Targets      map[string][]string `json:"targets,omitempty"`
}

type agentISPProbeTargetResult struct {
	Target    string `json:"target"`
	Label     string `json:"label,omitempty"`
	Reachable bool   `json:"reachable"`
	LatencyMs int    `json:"latency_ms"`
}

type agentISPProbeResult struct {
	LatencyMs      int                         `json:"latency_ms"` // best ms among reachable targets, -1 if all unreachable
	ReachableCount int                         `json:"reachable_count"`
	TotalCount     int                         `json:"total_count"`
	CheckedAtUnix  int64                       `json:"checked_at_unix,omitempty"`
	Targets        []agentISPProbeTargetResult `json:"targets,omitempty"`
}

// ---------------------------------------------------------------------------
// Runtime circuit-breaker state & API view DTOs

type ispHealthState struct {
	latencyMs       int
	reachableCount  int
	totalCount      int
	consecFails     int
	consecSuccesses int
	tripped         bool
	trippedAt       time.Time
	lastCheckedAt   time.Time
	alertNotified   bool
	targets         []agentISPProbeTargetResult
}

type ispProbeAlert struct {
	nodeID int64
	text   string
}

func (rt *edgeNodeRuntime) queueISPAlertsLocked(nodeID int64, messages []string) {
	for _, message := range messages {
		rt.pendingISPAlerts = append(rt.pendingISPAlerts, ispProbeAlert{nodeID: nodeID, text: message})
	}
}

type edgeISPHealthView struct {
	ISPGroup       string                      `json:"isp_group"`
	ISPLabel       string                      `json:"isp_label"`
	LatencyMs      int                         `json:"latency_ms"`
	ReachableCount int                         `json:"reachable_count"`
	TotalCount     int                         `json:"total_count"`
	ConsecFails    int                         `json:"consec_fails"`
	Tripped        bool                        `json:"tripped"`
	TrippedAt      string                      `json:"tripped_at,omitempty"`
	LastCheckedAt  string                      `json:"last_checked_at,omitempty"`
	Targets        []agentISPProbeTargetResult `json:"targets,omitempty"`
}

func buildISPHealthViews(health map[string]*ispHealthState) ([]edgeISPHealthView, bool) {
	if len(health) == 0 {
		return nil, false
	}
	views := make([]edgeISPHealthView, 0, len(probeableISPGroups))
	anyTripped := false
	for _, g := range probeableISPGroups {
		st := health[g]
		if st == nil || st.totalCount <= 0 {
			continue
		}
		v := edgeISPHealthView{
			ISPGroup:       g,
			ISPLabel:       ispGroupLabel(g),
			LatencyMs:      st.latencyMs,
			ReachableCount: st.reachableCount,
			TotalCount:     st.totalCount,
			ConsecFails:    st.consecFails,
			Tripped:        st.tripped,
		}
		if len(st.targets) > 0 {
			v.Targets = append([]agentISPProbeTargetResult(nil), st.targets...)
		}
		if st.tripped {
			anyTripped = true
			if !st.trippedAt.IsZero() {
				v.TrippedAt = st.trippedAt.UTC().Format(time.RFC3339)
			}
		}
		if !st.lastCheckedAt.IsZero() {
			v.LastCheckedAt = st.lastCheckedAt.UTC().Format(time.RFC3339)
		}
		views = append(views, v)
	}
	if len(views) == 0 {
		return nil, false
	}
	return views, anyTripped
}

func isISPTrippedInMap(health map[string]*ispHealthState, group string) bool {
	if len(health) == 0 {
		return false
	}
	switch group {
	case ispGroupCT, ispGroupCU, ispGroupCM:
		st := health[group]
		return st != nil && st.tripped
	case ispGroupOther:
		// In "other", only exclude a node if ALL 3 mainland ISPs are tripped (whole-IP block).
		for _, g := range probeableISPGroups {
			st := health[g]
			if st == nil || !st.tripped {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func anyISPTrippedInMap(health map[string]*ispHealthState) bool {
	for _, g := range probeableISPGroups {
		if st := health[g]; st != nil && st.tripped {
			return true
		}
	}
	return false
}

// authorizeISPProbeLocked changes the reporting process only after a round trip
// with the current master. A delayed request alone cannot claim ownership.
func (rt *edgeNodeRuntime) authorizeISPProbeLocked(n *edgeNodeLiveState, raw, claim string, now time.Time) (bool, string) {
	var instance, zero [16]byte
	if raw == "" {
		if n.ispProbeInstance != zero {
			// Give a downgraded agent a way to report again once the current
			// version has stopped heartbeating.
			if now.Sub(n.ispProbeSeenAt) <= edgeNodeOfflineAfter {
				return false, ""
			}
			n.switchISPProbeInstanceLocked(zero, now)
		}
		return true, ""
	}
	if len(raw) != hex.EncodedLen(len(instance)) {
		return false, ""
	}
	if _, err := hex.Decode(instance[:], []byte(raw)); err != nil || instance == zero {
		return false, ""
	}
	if instance == n.ispProbeInstance {
		n.ispProbeSeenAt = now
		return true, ""
	}

	var scope [2 * binary.MaxVarintLen64]byte
	binding := binary.AppendVarint(scope[:0], n.id)
	binding = binary.AppendUvarint(binding, n.ispProbeGeneration)
	mac := hmac.New(sha256.New, rt.ispProbeClaimKey[:])
	mac.Write(binding)
	mac.Write(instance[:])
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(claim), []byte(expected)) {
		return false, expected
	}
	n.switchISPProbeInstanceLocked(instance, now)
	return true, ""
}

func (n *edgeNodeLiveState) switchISPProbeInstanceLocked(instance [16]byte, now time.Time) {
	n.ispProbeInstance = instance
	n.ispProbeGeneration++
	n.ispProbeSeenAt = now
	n.ispProbeSeq = 0
	for _, health := range n.ispHealth {
		if health != nil {
			health.consecFails = 0
			health.consecSuccesses = 0
		}
	}
}

func resetISPCircuits(health map[string]*ispHealthState) {
	for _, st := range health {
		if st == nil {
			continue
		}
		st.tripped = false
		st.trippedAt = time.Time{}
		st.consecFails = 0
		st.consecSuccesses = 0
		st.alertNotified = false
	}
}

// applyISPProbeReportLocked updates per-ISP circuit breaker state for a node
// (or Master when isMaster=true) from a new probe sequence and returns any
// state-transition Telegram alert messages to dispatch.
func (rt *edgeNodeRuntime) applyISPProbeReportLocked(
	nodeName string,
	isMaster bool,
	monitorOnly bool,
	lastSeq *uint64,
	healthMap *map[string]*ispHealthState,
	seq uint64,
	results map[string]agentISPProbeResult,
	now time.Time,
) []string {
	if seq == 0 || len(results) == 0 || !rt.ispProbe.Enabled {
		return nil
	}
	if seq <= *lastSeq {
		return nil
	}
	*lastSeq = seq
	if *healthMap == nil {
		*healthMap = make(map[string]*ispHealthState, len(probeableISPGroups))
	}
	failThresh := rt.ispProbe.FailThreshold
	if failThresh < minISPProbeThreshold {
		failThresh = defaultISPProbeFailThreshold
	}
	recThresh := rt.ispProbe.RecoverThreshold
	if recThresh < minISPProbeThreshold {
		recThresh = defaultISPProbeRecoverThreshold
	}

	var newlyTripped []string
	var newlyRecovered []string

	for _, g := range probeableISPGroups {
		res, ok := results[g]
		if !ok || res.TotalCount <= 0 {
			continue
		}
		st := (*healthMap)[g]
		if st == nil {
			st = &ispHealthState{}
			(*healthMap)[g] = st
		}
		st.reachableCount = max(0, res.ReachableCount)
		st.totalCount = res.TotalCount
		if res.CheckedAtUnix > 0 {
			st.lastCheckedAt = time.Unix(res.CheckedAtUnix, 0)
		} else {
			st.lastCheckedAt = now
		}

		st.latencyMs = -1
		if res.ReachableCount > 0 {
			st.latencyMs = res.LatencyMs
		}
		if len(res.Targets) > 0 {
			targets := make([]agentISPProbeTargetResult, len(res.Targets))
			for i, t := range res.Targets {
				if strings.TrimSpace(t.Label) == "" {
					t.Label = inferISPProbeTargetLabel(t.Target)
				}
				targets[i] = t
			}
			st.targets = targets
		} else {
			st.targets = nil
		}
		if monitorOnly {
			continue
		}

		if res.ReachableCount > 0 {
			st.consecFails = 0
			if st.tripped {
				st.consecSuccesses++
				if st.consecSuccesses >= recThresh {
					st.tripped = false
					st.trippedAt = time.Time{}
					st.consecSuccesses = 0
					if st.alertNotified {
						st.alertNotified = false
						newlyRecovered = append(newlyRecovered, fmt.Sprintf("%s（%dms）", ispGroupLabel(g), st.latencyMs))
					}
				}
			} else {
				st.consecSuccesses = 0
			}
		} else {
			st.consecSuccesses = 0
			st.consecFails++
			if !st.tripped && st.consecFails >= failThresh {
				st.tripped = true
				st.trippedAt = now
				if !st.alertNotified {
					st.alertNotified = true
					newlyTripped = append(newlyTripped, ispGroupLabel(g))
				}
			}
		}
	}

	var alerts []string
	if len(newlyTripped) > 0 {
		if isMaster {
			alerts = append(alerts, fmt.Sprintf("⚠️ 云渡主机三网连通异常\n节点：%s\n异常线路：%s（北上广全部探测目标不可达）\n说明：主机仍保留最后兜底能力，请检查主机线路状态",
				nodeName, strings.Join(newlyTripped, "、")))
		} else {
			recMin := max(1, rt.ispProbe.RecoveryIntervalSec/60)
			alerts = append(alerts, fmt.Sprintf("🚨 云渡从机疑似被阻断（已自动熔断切流）\n节点：%s\n熔断线路：%s（连续 %d 次北上广均不可达）\n状态：已自动切至顺位下一节点，后台每 %d 分钟低频探活自动恢复",
				nodeName, strings.Join(newlyTripped, "、"), failThresh, recMin))
		}
	}
	if len(newlyRecovered) > 0 {
		if isMaster {
			alerts = append(alerts, fmt.Sprintf("✅ 云渡主机三网连通已恢复\n节点：%s\n恢复线路：%s",
				nodeName, strings.Join(newlyRecovered, "、")))
		} else {
			alerts = append(alerts, fmt.Sprintf("✅ 云渡从机连通已恢复（已自动重新分流）\n节点：%s\n恢复线路：%s\n状态：已自动解除熔断并重新加入分流池",
				nodeName, strings.Join(newlyRecovered, "、")))
		}
	}
	return alerts
}

// ---------------------------------------------------------------------------
// Shared concurrent TCP / ICMP prober (used by both Edge Agent & Master)

func probeTCPAddressMs(ctx context.Context, dialer *net.Dialer, addr string) int {
	start := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return -1
	}
	_ = conn.Close()
	ms := int(time.Since(start).Milliseconds())
	if ms < 1 {
		ms = 1
	}
	return ms
}

func resolveIPv4ForICMP(ctx context.Context, raw string) (net.IP, error) {
	host := strings.TrimSpace(raw)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			return ip4, nil
		}
		return nil, errors.New("ipv6 icmp unsupported")
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil || len(addrs) == 0 {
		return nil, errors.New("dns lookup failed")
	}
	ip4 := addrs[0].Unmap().As4()
	return net.IP(ip4[:]), nil
}

func probeICMPAddressMs(ctx context.Context, rawTarget string) int {
	if hook := ispICMPProbeHook; hook != nil {
		return hook(ctx, rawTarget)
	}
	targetIP, err := resolveIPv4ForICMP(ctx, rawTarget)
	if err != nil {
		return -1
	}
	// Prefer unprivileged datagram ICMP socket when permitted, then raw IPv4 ICMP.
	unprivileged := true
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		unprivileged = false
		conn, err = icmp.ListenPacket("ip4:icmp", "0.0.0.0")
		if err != nil {
			if errors.Is(err, os.ErrPermission) || strings.Contains(strings.ToLower(err.Error()), "operation not permitted") || strings.Contains(strings.ToLower(err.Error()), "permission denied") {
				icmpPermWarnOnce.Do(func() {
					log.Printf("edge agent: ICMP probe requires CAP_NET_RAW or net.ipv4.ping_group_range; re-run install command to grant CAP_NET_RAW (%v)", err)
				})
			}
			return -1
		}
	}
	defer conn.Close()

	id := (os.Getpid() ^ int(icmpEchoIDCounter.Add(1))) & 0xffff
	if id == 0 {
		id = 1
	}
	seq := 1
	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   id,
			Seq:  seq,
			Data: []byte("yundu-isp-probe"),
		},
	}
	wire, err := msg.Marshal(nil)
	if err != nil {
		return -1
	}

	deadline := time.Now().Add(ispProbeDialTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)

	var dst net.Addr
	if unprivileged {
		dst = &net.UDPAddr{IP: targetIP}
	} else {
		dst = &net.IPAddr{IP: targetIP}
	}

	start := time.Now()
	if _, err := conn.WriteTo(wire, dst); err != nil {
		return -1
	}

	buf := make([]byte, 1500)
	for {
		if ctx.Err() != nil {
			return -1
		}
		n, peer, err := conn.ReadFrom(buf)
		if err != nil {
			return -1
		}
		rm, err := icmp.ParseMessage(1, buf[:n])
		if err != nil || rm.Type != ipv4.ICMPTypeEchoReply {
			continue
		}
		echo, ok := rm.Body.(*icmp.Echo)
		if !ok || echo.Seq != seq {
			continue
		}
		if !unprivileged {
			if echo.ID != id {
				continue
			}
			if ipAddr, ok := peer.(*net.IPAddr); ok && !ipAddr.IP.Equal(targetIP) {
				continue
			}
		}
		ms := int(time.Since(start).Milliseconds())
		if ms < 1 {
			ms = 1
		}
		return ms
	}
}

func runISPProbeTargets(ctx context.Context, targets map[string][]string) map[string]agentISPProbeResult {
	return runISPProbeTargetsWithMode(ctx, edgeProbeModeTCP, targets)
}

func runISPProbeTargetsWithMode(ctx context.Context, mode string, targets map[string][]string) map[string]agentISPProbeResult {
	out := make(map[string]agentISPProbeResult, len(probeableISPGroups))
	if len(targets) == 0 || mode == edgeProbeModeOff {
		return out
	}
	dialer := &net.Dialer{Timeout: ispProbeDialTimeout}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, g := range probeableISPGroups {
		addrs := targets[g]
		if len(addrs) == 0 {
			continue
		}
		wg.Add(1)
		go func(group string, list []string) {
			defer wg.Done()
			var innerWG sync.WaitGroup
			details := make([]agentISPProbeTargetResult, len(list))
			for i, addr := range list {
				innerWG.Add(1)
				go func(idx int, target string) {
					defer innerWG.Done()
					var ms int
					if mode == edgeProbeModeICMP {
						ms = probeICMPAddressMs(ctx, target)
					} else {
						ms = probeTCPAddressMs(ctx, dialer, target)
					}
					details[idx] = agentISPProbeTargetResult{
						Target:    target,
						Label:     inferISPProbeTargetLabel(target),
						Reachable: ms > 0,
						LatencyMs: ms,
					}
				}(i, addr)
			}
			innerWG.Wait()
			reachable := 0
			bestMs := -1
			for _, d := range details {
				if d.Reachable && d.LatencyMs > 0 {
					reachable++
					if bestMs < 0 || d.LatencyMs < bestMs {
						bestMs = d.LatencyMs
					}
				}
			}
			mu.Lock()
			out[group] = agentISPProbeResult{
				LatencyMs:      bestMs,
				ReachableCount: reachable,
				TotalCount:     len(list),
				CheckedAtUnix:  time.Now().Unix(),
				Targets:        details,
			}
			mu.Unlock()
		}(g, append([]string(nil), addrs...))
	}
	wg.Wait()
	return out
}

// ---------------------------------------------------------------------------
// Edge Agent ISP prober loop

func (a *edgeAgent) applyISPProbeConfig(cfg *agentISPProbeConfig) {
	if a == nil || cfg == nil {
		return
	}
	triggerNow := false
	mode, err := normalizeEdgeProbeMode(cfg.Mode)
	if err != nil {
		mode = edgeProbeModeTCP
	}
	effectiveEnabled := cfg.Enabled && mode != edgeProbeModeOff

	a.ispProbeMu.Lock()
	prevEnabled := a.ispProbeEnabled
	prevMode := a.ispProbeMode
	prevInterval := a.ispProbeIntervalSec
	a.ispProbeEnabled = effectiveEnabled
	a.ispProbeMode = mode
	if cfg.IntervalSec >= minISPProbeIntervalSec {
		a.ispProbeIntervalSec = cfg.IntervalSec
	} else {
		a.ispProbeIntervalSec = defaultISPProbeIntervalSec
	}
	if !effectiveEnabled {
		a.ispProbeResults = nil
	}
	targetsChanged := false
	if cfg.Targets != nil {
		if len(a.ispProbeTargets) == 0 {
			targetsChanged = true
		} else {
			for _, g := range probeableISPGroups {
				oldList := a.ispProbeTargets[g]
				newList := cfg.Targets[g]
				if len(oldList) != len(newList) {
					targetsChanged = true
					break
				}
				for i := range newList {
					if oldList[i] != newList[i] {
						targetsChanged = true
						break
					}
				}
				if targetsChanged {
					break
				}
			}
		}
		a.ispProbeTargets = cloneISPProbeTargets(cfg.Targets)
	}
	if cfg.ForceProbeID != "" && cfg.ForceProbeID != a.ispProbeLastForceID {
		a.ispProbeLastForceID = cfg.ForceProbeID
		if effectiveEnabled {
			triggerNow = true
		}
	}
	if effectiveEnabled && (!prevEnabled || prevMode != mode || targetsChanged) {
		triggerNow = true
	}
	intervalChanged := prevInterval != a.ispProbeIntervalSec
	a.ispProbeMu.Unlock()

	if triggerNow || intervalChanged {
		select {
		case a.ispProbeTriggerCh <- triggerNow:
		default:
		}
	}
}

func (a *edgeAgent) snapshotISPProbe() (uint64, map[string]agentISPProbeResult, string) {
	if a == nil {
		return 0, nil, ""
	}
	a.ispProbeMu.RLock()
	defer a.ispProbeMu.RUnlock()
	if !a.ispProbeEnabled || a.ispProbeSeq == 0 || len(a.ispProbeResults) == 0 {
		return 0, nil, a.ispProbeClaim
	}
	cp := make(map[string]agentISPProbeResult, len(a.ispProbeResults))
	for k, v := range a.ispProbeResults {
		if len(v.Targets) > 0 {
			v.Targets = append([]agentISPProbeTargetResult(nil), v.Targets...)
		}
		cp[k] = v
	}
	return a.ispProbeSeq, cp, a.ispProbeClaim
}

func (a *edgeAgent) ispProbeLoop(ctx context.Context) {
	currentInterval := time.Duration(defaultISPProbeIntervalSec) * time.Second
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case runNow := <-a.ispProbeTriggerCh:
			a.ispProbeMu.RLock()
			iv := a.ispProbeIntervalSec
			a.ispProbeMu.RUnlock()
			if iv < minISPProbeIntervalSec {
				iv = defaultISPProbeIntervalSec
			}
			currentInterval = time.Duration(iv) * time.Second
			if runNow {
				a.runISPProbeOnce(ctx)
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(currentInterval)
		case <-timer.C:
			a.runISPProbeOnce(ctx)
			a.ispProbeMu.RLock()
			iv := a.ispProbeIntervalSec
			a.ispProbeMu.RUnlock()
			if iv < minISPProbeIntervalSec {
				iv = defaultISPProbeIntervalSec
			}
			currentInterval = time.Duration(iv) * time.Second
			timer.Reset(currentInterval)
		}
	}
}

func (a *edgeAgent) runISPProbeOnce(ctx context.Context) {
	a.ispProbeMu.RLock()
	enabled := a.ispProbeEnabled
	mode := a.ispProbeMode
	targets := cloneISPProbeTargets(a.ispProbeTargets)
	a.ispProbeMu.RUnlock()
	if !enabled || mode == edgeProbeModeOff {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	results := runISPProbeTargetsWithMode(probeCtx, mode, targets)
	cancel()
	if len(results) == 0 {
		return
	}
	a.ispProbeMu.Lock()
	a.ispProbeSeq++
	a.ispProbeResults = results
	a.ispProbeMu.Unlock()
	a.triggerFlush()
}

// ---------------------------------------------------------------------------
// Master-side 3-ISP probe runner (shows Master's own 3-ISP latency pills)

func (rt *edgeNodeRuntime) probeMasterISPOnce(ctx context.Context) {
	if rt == nil {
		return
	}
	rt.mu.RLock()
	cfg := rt.ispProbe
	targets := cloneISPProbeTargets(cfg.Targets)
	rt.mu.RUnlock()
	if !cfg.Enabled {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	results := runISPProbeTargets(probeCtx, targets)
	cancel()
	if len(results) == 0 {
		return
	}
	now := time.Now()
	rt.mu.Lock()
	nextSeq := rt.master.ispProbeSeq + 1
	alerts := rt.applyISPProbeReportLocked(rt.masterDisplayNameLocked(), true, false, &rt.master.ispProbeSeq, &rt.master.ispHealth, nextSeq, results, now)
	rt.queueISPAlertsLocked(0, alerts)
	rt.mu.Unlock()
}

func runMasterISPProbeMonitor(ctx context.Context, rt *edgeNodeRuntime) {
	if rt == nil {
		return
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			rt.probeMasterISPOnce(ctx)
			rt.mu.RLock()
			iv := rt.ispProbe.IntervalSec
			if anyISPTrippedInMap(rt.master.ispHealth) {
				iv = rt.ispProbe.RecoveryIntervalSec
			}
			rt.mu.RUnlock()
			if iv < minISPProbeIntervalSec {
				iv = defaultISPProbeIntervalSec
			}
			timer.Reset(time.Duration(iv) * time.Second)
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP Handlers: /api/isp-probe-settings

func (a *App) handleISPProbeSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.db == nil || a.pm == nil || a.pm.edges == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "节点服务未初始化")
		return
	}
	switch r.Method {
	case http.MethodGet:
		a.pm.edges.mu.RLock()
		cfg := a.pm.edges.ispProbe
		cfg.Targets = cloneISPProbeTargets(cfg.Targets)
		cfg.ICMPTargets = cloneISPProbeTargets(cfg.ICMPTargets)
		a.pm.edges.mu.RUnlock()
		a.jsonOK(w, cfg)
	case http.MethodPut:
		var req struct {
			Enabled             *bool               `json:"enabled"`
			IntervalSec         int                 `json:"interval_sec"`
			RecoveryIntervalSec int                 `json:"recovery_interval_sec"`
			FailThreshold       int                 `json:"fail_threshold"`
			RecoverThreshold    int                 `json:"recover_threshold"`
			Targets             map[string][]string `json:"targets"`
			ICMPTargets         map[string][]string `json:"icmp_targets"`
			ResetDefaults       bool                `json:"reset_defaults"`
		}
		if err := decodeJSONBody(w, r, &req); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "请求参数无效")
			return
		}
		cur := a.db.loadISPProbeSettings()
		next := cur
		if req.ResetDefaults {
			next = defaultISPProbeSettings()
			if req.Enabled != nil {
				next.Enabled = *req.Enabled
			}
		} else {
			if req.Enabled != nil {
				next.Enabled = *req.Enabled
			}
			if req.IntervalSec > 0 {
				next.IntervalSec = req.IntervalSec
			}
			if req.RecoveryIntervalSec > 0 {
				next.RecoveryIntervalSec = req.RecoveryIntervalSec
			}
			if req.FailThreshold > 0 {
				next.FailThreshold = req.FailThreshold
			}
			if req.RecoverThreshold > 0 {
				next.RecoverThreshold = req.RecoverThreshold
			}
			if req.Targets != nil {
				next.Targets = req.Targets
			}
			if req.ICMPTargets != nil {
				next.ICMPTargets = req.ICMPTargets
			}
		}
		normalized, err := normalizeISPProbeSettings(next)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := a.db.saveISPProbeSettings(normalized); err != nil {
			log.Printf("isp probe settings: save failed: %v", err)
			a.jsonErr(w, http.StatusInternalServerError, "保存阻断探测设置失败")
			return
		}
		a.pm.edges.mu.Lock()
		a.pm.edges.ispProbe = normalized
		if !normalized.Enabled {
			// Clear tripped states immediately when probing is turned off so no node stays blocked.
			for _, node := range a.pm.edges.nodes {
				resetISPCircuits(node.ispHealth)
			}
			resetISPCircuits(a.pm.edges.master.ispHealth)
		}
		a.pm.edges.mu.Unlock()
		a.jsonOK(w, normalized)
	default:
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
