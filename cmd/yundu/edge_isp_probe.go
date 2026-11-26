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
	"strconv"
	"strings"
	"sync"
	"time"
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
)

// probeableISPGroups are the three mainland Chinese ISP groups that run active
// TCP handshake probing. When all three are tripped simultaneously, the node is
// also treated as tripped in the "other" group.
var probeableISPGroups = []string{ispGroupCT, ispGroupCU, ispGroupCM}

// ispProbeAllowLoopbackInTests lets offline unit tests bind 127.0.0.1 TCP
// listeners as deterministic probe targets. Production code keeps this false.
var ispProbeAllowLoopbackInTests bool

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
}

func defaultISPProbeSettings() ISPProbeSettings {
	return ISPProbeSettings{
		Enabled:             true,
		IntervalSec:         defaultISPProbeIntervalSec,
		RecoveryIntervalSec: defaultISPProbeRecoveryIntervalSec,
		FailThreshold:       defaultISPProbeFailThreshold,
		RecoverThreshold:    defaultISPProbeRecoverThreshold,
		Targets:             defaultISPProbeTargets(),
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
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
INSERT OR IGNORE INTO isp_probe_settings (id) VALUES (1);
`

func ensureISPProbeSchema(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, ispProbeSchemaSQL)
	return err
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
	)
	err := d.db.QueryRow(`SELECT enabled, interval_sec, recovery_interval_sec, fail_threshold, recover_threshold, targets_json FROM isp_probe_settings WHERE id=1`).
		Scan(&enabledInt, &intervalSec, &recoveryIntervalSec, &failThreshold, &recoverThreshold, &targetsJSON)
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
	_, err = d.db.Exec(
		`INSERT INTO isp_probe_settings(id, enabled, interval_sec, recovery_interval_sec, fail_threshold, recover_threshold, targets_json, updated_at)
		 VALUES(1, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(id) DO UPDATE SET
		   enabled=excluded.enabled,
		   interval_sec=excluded.interval_sec,
		   recovery_interval_sec=excluded.recovery_interval_sec,
		   fail_threshold=excluded.fail_threshold,
		   recover_threshold=excluded.recover_threshold,
		   targets_json=excluded.targets_json,
		   updated_at=CURRENT_TIMESTAMP`,
		sqliteBool(s.Enabled), s.IntervalSec, s.RecoveryIntervalSec, s.FailThreshold, s.RecoverThreshold, string(targetsBytes),
	)
	return err
}

// ---------------------------------------------------------------------------
// Wire DTOs between Master and Edge Agent

type agentISPProbeConfig struct {
	Enabled      bool                `json:"enabled"`
	IntervalSec  int                 `json:"interval_sec"`
	ForceProbeID string              `json:"force_probe_id,omitempty"`
	Targets      map[string][]string `json:"targets,omitempty"`
}

type agentISPProbeResult struct {
	LatencyMs      int   `json:"latency_ms"` // best ms among reachable targets, -1 if all unreachable
	ReachableCount int   `json:"reachable_count"`
	TotalCount     int   `json:"total_count"`
	CheckedAtUnix  int64 `json:"checked_at_unix,omitempty"`
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
	ISPGroup       string `json:"isp_group"`
	ISPLabel       string `json:"isp_label"`
	LatencyMs      int    `json:"latency_ms"`
	ReachableCount int    `json:"reachable_count"`
	TotalCount     int    `json:"total_count"`
	ConsecFails    int    `json:"consec_fails"`
	Tripped        bool   `json:"tripped"`
	TrippedAt      string `json:"tripped_at,omitempty"`
	LastCheckedAt  string `json:"last_checked_at,omitempty"`
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
// Shared concurrent TCP handshake prober (used by both Edge Agent & Master)

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

func runISPProbeTargets(ctx context.Context, targets map[string][]string) map[string]agentISPProbeResult {
	out := make(map[string]agentISPProbeResult, len(probeableISPGroups))
	if len(targets) == 0 {
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
			latencies := make([]int, len(list))
			for i, addr := range list {
				innerWG.Add(1)
				go func(idx int, target string) {
					defer innerWG.Done()
					latencies[idx] = probeTCPAddressMs(ctx, dialer, target)
				}(i, addr)
			}
			innerWG.Wait()
			reachable := 0
			bestMs := -1
			for _, ms := range latencies {
				if ms > 0 {
					reachable++
					if bestMs < 0 || ms < bestMs {
						bestMs = ms
					}
				}
			}
			mu.Lock()
			out[group] = agentISPProbeResult{
				LatencyMs:      bestMs,
				ReachableCount: reachable,
				TotalCount:     len(list),
				CheckedAtUnix:  time.Now().Unix(),
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
	a.ispProbeMu.Lock()
	prevEnabled := a.ispProbeEnabled
	prevInterval := a.ispProbeIntervalSec
	a.ispProbeEnabled = cfg.Enabled
	if cfg.IntervalSec >= minISPProbeIntervalSec {
		a.ispProbeIntervalSec = cfg.IntervalSec
	} else {
		a.ispProbeIntervalSec = defaultISPProbeIntervalSec
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
		triggerNow = true
	}
	if cfg.Enabled && (!prevEnabled || targetsChanged) {
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
	if a.ispProbeSeq == 0 || len(a.ispProbeResults) == 0 {
		return 0, nil, a.ispProbeClaim
	}
	cp := make(map[string]agentISPProbeResult, len(a.ispProbeResults))
	for k, v := range a.ispProbeResults {
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
	targets := cloneISPProbeTargets(a.ispProbeTargets)
	a.ispProbeMu.RUnlock()
	if !enabled {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	results := runISPProbeTargets(probeCtx, targets)
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
