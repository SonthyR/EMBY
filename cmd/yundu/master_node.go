package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

const (
	defaultMasterNodeName        = "主机"
	masterBandwidthSampleTick    = 5 * time.Second
	defaultNodeMaxBalanceStreams = 2
	minNodeMaxBalanceStreams     = 1
	maxNodeMaxBalanceStreams     = 99
)

func normalizeMaxBalanceStreams(v int) int {
	if v <= 0 {
		return defaultNodeMaxBalanceStreams
	}
	if v < minNodeMaxBalanceStreams {
		return minNodeMaxBalanceStreams
	}
	if v > maxNodeMaxBalanceStreams {
		return maxNodeMaxBalanceStreams
	}
	return v
}

const masterNodeSchemaSQL = `
CREATE TABLE IF NOT EXISTS master_node_settings (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	name TEXT NOT NULL DEFAULT '主机',
	traffic_quota INTEGER NOT NULL DEFAULT 0,
	traffic_used INTEGER NOT NULL DEFAULT 0,
	traffic_reset_day INTEGER NOT NULL DEFAULT 0,
	traffic_reset_period TEXT NOT NULL DEFAULT '',
	note TEXT NOT NULL DEFAULT '',
	max_balance_streams INTEGER NOT NULL DEFAULT 2,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
INSERT OR IGNORE INTO master_node_settings (id) VALUES (1);
`

func ensureMasterNodeSchema(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, masterNodeSchemaSQL); err != nil {
		return err
	}
	var exists int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('master_node_settings') WHERE name='max_balance_streams'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE master_node_settings ADD COLUMN max_balance_streams INTEGER NOT NULL DEFAULT 2`); err != nil {
			return err
		}
	}
	return nil
}

type masterNodeState struct {
	name                string
	quota               int64
	persistedUsed       int64
	resetDay            int
	resetPeriod         string
	note                string
	maxBalanceStreams   int
	latencies           map[int64]int
	ispHealth           map[string]*ispHealthState
	ispProbeSeq         uint64
	currentBps          int64
	alertQuota80Period  string
	alertQuota100Period string

	pending      atomic.Int64
	windowBytes  atomic.Int64
	lastWindowAt atomic.Int64
}

type MasterNodeInput struct {
	Name              string `json:"name"`
	TrafficQuota      int64  `json:"traffic_quota"`
	ResetDay          int    `json:"traffic_reset_day"`
	MaxBalanceStreams int    `json:"max_balance_streams"`
	Note              string `json:"note"`
}

func (m *masterNodeState) displayName() string {
	if m == nil {
		return defaultMasterNodeName
	}
	name := strings.TrimSpace(m.name)
	if name == "" {
		return defaultMasterNodeName
	}
	return name
}

func (m *masterNodeState) totalUsed() int64 {
	if m == nil {
		return 0
	}
	return m.persistedUsed + m.pending.Load()
}

func (m *masterNodeState) quotaExceeded() bool {
	return m != nil && m.quota > 0 && m.totalUsed() >= m.quota
}

func (m *masterNodeState) status() string {
	if m.quotaExceeded() {
		return "quota"
	}
	return "online"
}

func (rt *edgeNodeRuntime) masterDisplayNameLocked() string {
	if rt == nil {
		return defaultMasterNodeName
	}
	return rt.master.displayName()
}

func (rt *edgeNodeRuntime) masterDisplayName() string {
	if rt == nil {
		return defaultMasterNodeName
	}
	rt.mu.RLock()
	name := rt.master.displayName()
	rt.mu.RUnlock()
	return name
}

func (rt *edgeNodeRuntime) masterQuotaExceededLocked() bool {
	if rt == nil {
		return false
	}
	return rt.master.quotaExceeded()
}

func (rt *edgeNodeRuntime) recordMasterTraffic(billed, outBytes int64) {
	if rt == nil {
		return
	}
	if billed > 0 {
		rt.master.pending.Add(billed)
	}
	if outBytes > 0 {
		rt.master.windowBytes.Add(outBytes)
	}
}

func (rt *edgeNodeRuntime) sampleMasterBandwidth() {
	if rt == nil {
		return
	}
	bytesInWindow := rt.master.windowBytes.Swap(0)
	nowNano := time.Now().UnixNano()
	prevNano := rt.master.lastWindowAt.Swap(nowNano)
	if prevNano <= 0 {
		return
	}
	elapsedSec := float64(nowNano-prevNano) / float64(time.Second)
	if elapsedSec < 1.0 {
		elapsedSec = 1.0
	}
	bps := int64(float64(bytesInWindow*8) / elapsedSec)
	rt.mu.Lock()
	rt.master.currentBps = bps
	rt.mu.Unlock()
}

func (rt *edgeNodeRuntime) probeMasterUpstreamLatencies(ctx context.Context) {
	if rt == nil || rt.pm == nil {
		return
	}
	var targets []agentProbeTarget
	rt.pm.mu.RLock()
	for sid, inst := range rt.pm.proxies {
		if inst != nil && inst.Site.TargetURL != "" {
			targets = append(targets, agentProbeTarget{SiteID: sid, URL: inst.Site.TargetURL})
		}
	}
	rt.pm.mu.RUnlock()
	if len(targets) == 0 {
		rt.mu.Lock()
		rt.master.latencies = nil
		rt.mu.Unlock()
		return
	}
	results := make(map[int64]int, len(targets))
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	for _, target := range targets {
		if ctx.Err() != nil {
			return
		}
		results[target.SiteID] = probeURLTCPMs(ctx, dialer, target.URL)
	}
	rt.mu.Lock()
	rt.master.latencies = results
	rt.mu.Unlock()
}

func runMasterNodeMonitor(ctx context.Context, rt *edgeNodeRuntime) {
	if rt == nil {
		return
	}
	rt.master.lastWindowAt.Store(time.Now().UnixNano())
	go runMasterISPProbeMonitor(ctx, rt)
	// Run an initial latency probe shortly after startup so sites show latencies quickly.
	initTimer := time.NewTimer(1500 * time.Millisecond)
	bwTicker := time.NewTicker(masterBandwidthSampleTick)
	probeTicker := time.NewTicker(edgeProbeInterval)
	defer initTimer.Stop()
	defer bwTicker.Stop()
	defer probeTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-initTimer.C:
			rt.probeMasterUpstreamLatencies(ctx)
		case <-bwTicker.C:
			rt.sampleMasterBandwidth()
		case <-probeTicker.C:
			rt.probeMasterUpstreamLatencies(ctx)
		}
	}
}

func (a *App) buildMasterNodeView(origin string) EdgeNode {
	now := time.Now()
	var (
		name              = defaultMasterNodeName
		quota             int64
		used              int64
		resetDay          int
		note              string
		maxBalanceStreams = defaultNodeMaxBalanceStreams
		currentBps        int64
		status            = "online"
		rawLatencies      map[int64]int
		speedMetrics      []edgeNodeSpeedMetric
		ispHealth         []edgeISPHealthView
		anyCircuitTripped bool
	)
	if a != nil && a.pm != nil && a.pm.edges != nil {
		a.pm.edges.mu.RLock()
		name = a.pm.edges.master.displayName()
		quota = a.pm.edges.master.quota
		used = a.pm.edges.master.totalUsed()
		resetDay = a.pm.edges.master.resetDay
		note = a.pm.edges.master.note
		maxBalanceStreams = normalizeMaxBalanceStreams(a.pm.edges.master.maxBalanceStreams)
		currentBps = max(int64(0), a.pm.edges.master.currentBps)
		status = a.pm.edges.master.status()
		if len(a.pm.edges.master.latencies) > 0 {
			rawLatencies = make(map[int64]int, len(a.pm.edges.master.latencies))
			for sid, ms := range a.pm.edges.master.latencies {
				rawLatencies[sid] = ms
			}
		}
		speedMetrics = a.pm.edges.speedMetricsMapCopyLocked(0)
		ispHealth, anyCircuitTripped = buildISPHealthViews(a.pm.edges.master.ispHealth)
		a.pm.edges.mu.RUnlock()
	}

	var activeStreams int
	if a != nil && a.pm != nil && a.pm.shares != nil {
		_, activeStreams, _, _ = a.pm.shares.analyticsSessionSummary()
	}

	var latViews []edgeSiteLatency
	if len(rawLatencies) > 0 && a != nil && a.pm != nil {
		latViews = make([]edgeSiteLatency, 0, len(rawLatencies))
		a.pm.mu.RLock()
		for sid, ms := range rawLatencies {
			siteName := fmt.Sprintf("站点 #%d", sid)
			if inst := a.pm.proxies[sid]; inst != nil && inst.Site.Name != "" {
				siteName = inst.Site.Name
			}
			latViews = append(latViews, edgeSiteLatency{SiteName: siteName, LatencyMs: ms})
		}
		a.pm.mu.RUnlock()
		sort.Slice(latViews, func(i, j int) bool {
			return latViews[i].SiteName < latViews[j].SiteName
		})
	}

	nextResetAt := ""
	if resetDay > 0 && a != nil && a.db != nil {
		if _, next := shareResetPeriod(now.In(a.db.shareLocation()), resetDay); !next.IsZero() {
			nextResetAt = next.Format("2006-01-02")
		}
	}

	return EdgeNode{
		ID:                0,
		IsMaster:          true,
		Name:              name,
		Enabled:           true,
		Online:            true,
		Status:            status,
		AgentVersion:      appVersion,
		MasterVersion:     appVersion,
		PublicURL:         strings.TrimSpace(origin),
		TLSActive:         a != nil && a.panelTLSEnabled,
		ActiveStreams:     activeStreams,
		CurrentBps:        currentBps,
		MemoryBytes:       currentProcessMemoryBytes(),
		UptimeSec:         max(int64(0), int64(time.Since(startTime).Seconds())),
		TrafficQuota:      quota,
		TrafficUsed:       used,
		ResetDay:          resetDay,
		NextResetAt:       nextResetAt,
		MaxBalanceStreams: maxBalanceStreams,
		Note:              note,
		UpstreamLatencies: latViews,
		SpeedMetrics:      speedMetrics,
		ISPHealth:         ispHealth,
		AnyCircuitTripped: anyCircuitTripped,
	}
}

func normalizeMasterNodeInput(input MasterNodeInput) (MasterNodeInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.Note = strings.TrimSpace(input.Note)
	if input.Name == "" {
		input.Name = defaultMasterNodeName
	}
	if len([]rune(input.Name)) > 40 || len(input.Name) > 80 {
		return input, errors.New("主机名称长度不能超过 40 个字")
	}
	if input.TrafficQuota < 0 {
		return input, errors.New("流量额度不能为负数")
	}
	if input.ResetDay < 0 || input.ResetDay > 28 {
		return input, errors.New("每月重置日需在 0–28 之间")
	}
	if input.MaxBalanceStreams < 0 || input.MaxBalanceStreams > maxNodeMaxBalanceStreams {
		return input, errors.New("负载均衡上限应为 1–99 路")
	}
	if len(input.Note) > 200 {
		input.Note = input.Note[:200]
	}
	return input, nil
}

// GET/PUT /api/master-node
func (a *App) handleMasterNode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.pm == nil || a.pm.edges == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "节点服务未初始化")
		return
	}
	origin := a.shareTextOrigin(r)
	switch r.Method {
	case http.MethodGet:
		a.jsonOK(w, a.buildMasterNodeView(origin))
	case http.MethodPut:
		var raw MasterNodeInput
		if err := decodeJSONBody(w, r, &raw); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "请求参数无效")
			return
		}
		input, err := normalizeMasterNodeInput(raw)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		period := ""
		if input.ResetDay > 0 {
			period, _ = shareResetPeriod(time.Now().In(a.db.shareLocation()), input.ResetDay)
		}
		if _, err := a.db.db.Exec(
			`UPDATE master_node_settings SET name=?, traffic_quota=?, traffic_reset_day=?, traffic_reset_period=?, note=?, max_balance_streams=CASE WHEN ? > 0 THEN ? ELSE max_balance_streams END, updated_at=CURRENT_TIMESTAMP WHERE id=1`,
			input.Name, input.TrafficQuota, input.ResetDay, period, input.Note, input.MaxBalanceStreams, input.MaxBalanceStreams,
		); err != nil {
			log.Printf("master node: update settings: %v", err)
			a.jsonErr(w, http.StatusInternalServerError, "保存主机配置失败")
			return
		}
		_ = a.pm.edges.reload()
		a.jsonOK(w, a.buildMasterNodeView(origin))
	default:
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// POST /api/master-node/reset-traffic | POST /api/master-node/probe
func (a *App) handleMasterNodeAction(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.pm == nil || a.pm.edges == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "节点服务未初始化")
		return
	}
	action := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/master-node/"), "/")
	origin := a.shareTextOrigin(r)
	switch action {
	case "reset-traffic":
		a.pm.edges.flushMu.Lock()
		_, err := a.db.db.Exec(`UPDATE master_node_settings SET traffic_used=0, updated_at=CURRENT_TIMESTAMP WHERE id=1`)
		if err == nil {
			a.pm.edges.mu.Lock()
			a.pm.edges.master.pending.Store(0)
			a.pm.edges.master.persistedUsed = 0
			a.pm.edges.master.alertQuota80Period = ""
			a.pm.edges.master.alertQuota100Period = ""
			a.pm.edges.mu.Unlock()
		}
		a.pm.edges.flushMu.Unlock()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "清零主机流量失败")
			return
		}
		a.jsonOK(w, a.buildMasterNodeView(origin))
	case "probe":
		probeCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		a.pm.edges.probeMasterUpstreamLatencies(probeCtx)
		a.pm.edges.probeMasterISPOnce(probeCtx)
		cancel()
		a.jsonOK(w, a.buildMasterNodeView(origin))
	case "probe-isp":
		probeCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		a.pm.edges.probeMasterISPOnce(probeCtx)
		cancel()
		a.jsonOK(w, a.buildMasterNodeView(origin))
	default:
		a.jsonErr(w, http.StatusNotFound, "unknown master node action")
	}
}
