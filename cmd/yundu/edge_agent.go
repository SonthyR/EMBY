package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	edgeAgentHeartbeatInterval = 5 * time.Second
	edgeAgentMaxRedirects      = 10
	edgeAgentCopyBufSize       = 64 << 10
)

type edgeAgentConfig struct {
	MasterURL      string
	NodeID         int64
	Token          string
	BindAddr       string
	Port           int
	ReleaseBaseURL string
	ReleaseClient  *http.Client
	ExecutablePath string
}

func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func runEdgeAgentCommand(args []string) error {
	cfg := edgeAgentConfig{
		MasterURL: firstNonEmptyEnv("YUNDU_MASTER_URL", "AGENT_MASTER"),
		Token:     firstNonEmptyEnv("YUNDU_NODE_TOKEN", "AGENT_TOKEN"),
		BindAddr:  firstNonEmptyEnv("YUNDU_AGENT_BIND", "AGENT_BIND"),
		Port:      edgeNodeDefaultPort,
	}
	if v := firstNonEmptyEnv("YUNDU_NODE_ID", "AGENT_NODE_ID"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.NodeID = id
		}
	}
	if v := firstNonEmptyEnv("YUNDU_AGENT_PORT", "AGENT_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Port = p
		}
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--master":
			if i+1 >= len(args) {
				return errors.New("--master requires a URL")
			}
			cfg.MasterURL = strings.TrimSpace(args[i+1])
			i++
		case "--id":
			if i+1 >= len(args) {
				return errors.New("--id requires a node ID")
			}
			id, err := strconv.ParseInt(strings.TrimSpace(args[i+1]), 10, 64)
			if err != nil || id <= 0 {
				return errors.New("invalid --id")
			}
			cfg.NodeID = id
			i++
		case "--token":
			if i+1 >= len(args) {
				return errors.New("--token requires a node token")
			}
			cfg.Token = strings.TrimSpace(args[i+1])
			i++
		case "--port", "-p":
			if i+1 >= len(args) {
				return errors.New("--port requires a port number")
			}
			p, err := strconv.Atoi(strings.TrimSpace(args[i+1]))
			if err != nil || p < 1 || p > 65535 {
				return errors.New("invalid --port")
			}
			cfg.Port = p
			i++
		case "--bind":
			if i+1 >= len(args) {
				return errors.New("--bind requires an IP address")
			}
			cfg.BindAddr = strings.TrimSpace(args[i+1])
			i++
		default:
			return fmt.Errorf("unknown agent flag: %s", args[i])
		}
	}
	if cfg.MasterURL == "" || cfg.NodeID <= 0 || cfg.Token == "" {
		return errors.New("usage: meridian agent --master <url> --id <node-id> --token <token> [--port 19090] [--bind 0.0.0.0]")
	}
	parsedMaster, err := normalizeTargetURL(cfg.MasterURL)
	if err != nil {
		return fmt.Errorf("invalid --master URL: %w", err)
	}
	cfg.MasterURL = strings.TrimRight(parsedMaster.String(), "/")
	if cfg.Port < 1 || cfg.Port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}

	agent := newEdgeAgent(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Perform initial heartbeat (best-effort) so TLS cert is loaded right away if available.
	_ = agent.sendHeartbeat(ctx)

	listenAddr := fmt.Sprintf(":%d", cfg.Port)
	if cfg.BindAddr != "" {
		listenAddr = net.JoinHostPort(cfg.BindAddr, strconv.Itoa(cfg.Port))
	}
	rawListener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("agent listen %s: %w", listenAddr, err)
	}
	rawListener = limitListener(rawListener, 2048)
	listener := &autoTLSListener{
		Listener: rawListener,
		getCert:  agent.getCertificate,
	}

	srv := &http.Server{
		Handler:           agent.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	go agent.heartbeatLoop(ctx)
	go agent.probeLoop(ctx)

	log.Println("============================================================")
	log.Printf("  Yundu Edge Agent %s (node #%d)", appVersion, cfg.NodeID)
	log.Printf("  Master: %s", cfg.MasterURL)
	log.Printf("  Listening on: %s (auto HTTP/HTTPS)", rawListener.Addr())
	log.Println("============================================================")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case <-sigCh:
		log.Println("Yundu Edge Agent shutting down...")
	case <-agent.restartCh:
		log.Println("Yundu Edge Agent restarting after self-upgrade...")
	case serveErr := <-errCh:
		return serveErr
	}

	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	_ = srv.Shutdown(shutdownCtx)
	shutdownCancel()

	// Final flush of metered traffic back to Master.
	finalCtx, finalCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = agent.sendHeartbeat(finalCtx)
	finalCancel()
	return nil
}

// ---------------------------------------------------------------------------
// Auto-TLS dual-protocol listener (peeks first byte for TLS ClientHello 0x16)

type peekedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

type autoTLSListener struct {
	net.Listener
	getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error)
}

func (l *autoTLSListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(conn, 64)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	first, err := br.Peek(1)
	_ = conn.SetReadDeadline(time.Time{})
	wrapped := &peekedConn{Conn: conn, reader: br}
	if err == nil && len(first) == 1 && first[0] == 0x16 {
		if cert, certErr := l.getCert(nil); certErr == nil && cert != nil {
			tlsCfg := &tls.Config{
				MinVersion:     tls.VersionTLS12,
				GetCertificate: l.getCert,
				NextProtos:     []string{"h2", "http/1.1"},
			}
			return tls.Server(wrapped, tlsCfg), nil
		}
	}
	return wrapped, nil
}

// ---------------------------------------------------------------------------
// Edge Agent runtime

type agentTrafficKey struct {
	siteID      int64
	shareUserID int64
	tokenHash   string
}

type agentTrafficAccum struct {
	bytesIn  int64
	bytesOut int64
	requests int64
	playing  bool
}

type edgeAgent struct {
	cfg            edgeAgentConfig
	startedAt      time.Time
	masterClient   *http.Client
	upstreamClient *http.Client
	egressClient   *http.Client
	nodeEnabled    atomic.Bool
	activeStreams  atomic.Int64
	windowBytes    atomic.Int64

	certMu          sync.RWMutex
	cert            *tls.Certificate
	certFingerprint string

	policyMu     sync.RWMutex
	blockedUsers map[int64]bool
	revoked      map[string]bool
	limiters     map[int64]*shareRateLimiter
	speedLimits  map[int64]int

	trafficMu sync.Mutex
	traffic   map[agentTrafficKey]*agentTrafficAccum
	flushCh   chan struct{}

	probeMu      sync.RWMutex
	probeTargets []agentProbeTarget
	latencies    map[int64]int
	probeNowCh   chan struct{}

	upgradeMu            sync.RWMutex
	upgradeReqID         string
	upgradeStatus        string
	lastHandledUpgradeID string
	upgrading            atomic.Bool

	speedtestMu         sync.Mutex
	speedtestActiveID   string
	speedtestLastDoneID string
	speedtestCancel     context.CancelFunc
	speedtestProgress   *agentSpeedtestProgress
	speedtestTransport  *http.Transport
	speedtestNotifyCh   chan struct{}
	lastWindowAt        atomic.Int64

	directActive  atomic.Int32
	directMu      sync.Mutex
	directBudgets map[string]directStreamBudget

	restartCh   chan struct{}
	restartOnce sync.Once
}

type directStreamBudget struct {
	used int64
	exp  int64
}

func (a *edgeAgent) requestRestart() {
	if a == nil || a.restartCh == nil {
		return
	}
	a.restartOnce.Do(func() { close(a.restartCh) })
}

func newEdgeAgent(cfg edgeAgentConfig) *edgeAgent {
	masterTransport := http.DefaultTransport.(*http.Transport).Clone()
	masterTransport.TLSClientConfig = secureTLSConfig("")
	masterTransport.IdleConnTimeout = 90 * time.Second

	upstreamTransport := http.DefaultTransport.(*http.Transport).Clone()
	upstreamTransport.TLSClientConfig = secureTLSConfig("")
	upstreamTransport.ResponseHeaderTimeout = 30 * time.Second
	upstreamTransport.MaxIdleConnsPerHost = 64
	upstreamTransport.IdleConnTimeout = 90 * time.Second

	egressTransport := upstreamTransport.Clone()
	egressTransport.DisableCompression = true

	agent := &edgeAgent{
		cfg:       cfg,
		startedAt: time.Now(),
		masterClient: &http.Client{
			Transport: masterTransport,
			Timeout:   10 * time.Second,
		},
		upstreamClient: &http.Client{
			Transport: upstreamTransport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		egressClient: &http.Client{
			Transport: egressTransport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		blockedUsers:      make(map[int64]bool),
		revoked:           make(map[string]bool),
		limiters:          make(map[int64]*shareRateLimiter),
		speedLimits:       make(map[int64]int),
		traffic:           make(map[agentTrafficKey]*agentTrafficAccum),
		flushCh:           make(chan struct{}, 1),
		latencies:         make(map[int64]int),
		probeNowCh:        make(chan struct{}, 1),
		speedtestNotifyCh: make(chan struct{}, 1),
		directBudgets:     make(map[string]directStreamBudget),
		restartCh:         make(chan struct{}),
	}
	agent.lastWindowAt.Store(time.Now().UnixNano())
	agent.nodeEnabled.Store(true)
	return agent
}

func (a *edgeAgent) getCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	a.certMu.RLock()
	cert := a.cert
	a.certMu.RUnlock()
	if cert == nil {
		return nil, errors.New("no TLS certificate loaded")
	}
	return cert, nil
}

func (a *edgeAgent) hasTLS() bool {
	a.certMu.RLock()
	ok := a.cert != nil
	a.certMu.RUnlock()
	return ok
}

func (a *edgeAgent) recordTraffic(siteID, shareUserID int64, tokenHash string, bytesIn, bytesOut, requests int64, playing bool) {
	if bytesOut > 0 {
		a.windowBytes.Add(bytesOut)
	}
	key := agentTrafficKey{siteID: siteID, shareUserID: shareUserID, tokenHash: tokenHash}
	a.trafficMu.Lock()
	acc := a.traffic[key]
	if acc == nil {
		acc = &agentTrafficAccum{}
		a.traffic[key] = acc
	}
	acc.bytesIn += bytesIn
	acc.bytesOut += bytesOut
	acc.requests += requests
	if playing {
		acc.playing = true
	}
	a.trafficMu.Unlock()
}

func (a *edgeAgent) drainTrafficReports() []agentTrafficReport {
	a.trafficMu.Lock()
	if len(a.traffic) == 0 {
		a.trafficMu.Unlock()
		return nil
	}
	reports := make([]agentTrafficReport, 0, len(a.traffic))
	for key, acc := range a.traffic {
		reports = append(reports, agentTrafficReport{
			SiteID:      key.siteID,
			ShareUserID: key.shareUserID,
			TokenHash:   key.tokenHash,
			BytesIn:     acc.bytesIn,
			BytesOut:    acc.bytesOut,
			Requests:    acc.requests,
			Playing:     acc.playing,
		})
	}
	a.traffic = make(map[agentTrafficKey]*agentTrafficAccum)
	a.trafficMu.Unlock()
	return reports
}

func (a *edgeAgent) restoreTrafficReports(reports []agentTrafficReport) {
	for _, r := range reports {
		a.recordTraffic(r.SiteID, r.ShareUserID, r.TokenHash, r.BytesIn, r.BytesOut, r.Requests, r.Playing)
	}
}

func (a *edgeAgent) triggerFlush() {
	select {
	case a.flushCh <- struct{}{}:
	default:
	}
}

func (a *edgeAgent) heartbeatLoop(ctx context.Context) {
	backoff := time.Second
	ticker := time.NewTicker(edgeAgentHeartbeatInterval)
	defer ticker.Stop()
	lastFlush := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.sendHeartbeat(ctx); err != nil {
				log.Printf("edge agent heartbeat failed: %v", err)
				if backoff < 30*time.Second {
					backoff *= 2
				}
			} else {
				backoff = time.Second
				lastFlush = time.Now()
			}
		case <-a.flushCh:
			if time.Since(lastFlush) < time.Second {
				continue
			}
			if err := a.sendHeartbeat(ctx); err == nil {
				lastFlush = time.Now()
			}
		case <-a.speedtestNotifyCh:
			if err := a.sendHeartbeat(ctx); err == nil {
				lastFlush = time.Now()
			}
		}
	}
}

func (a *edgeAgent) triggerSpeedtestNotify() {
	if a == nil || a.speedtestNotifyCh == nil {
		return
	}
	select {
	case a.speedtestNotifyCh <- struct{}{}:
	default:
	}
}

func (a *edgeAgent) snapshotSpeedtestProgress() *agentSpeedtestProgress {
	a.speedtestMu.Lock()
	defer a.speedtestMu.Unlock()
	if a.speedtestProgress == nil {
		return nil
	}
	cp := *a.speedtestProgress
	cp.Results = append([]speedtestTargetResult(nil), a.speedtestProgress.Results...)
	return &cp
}

func (a *edgeAgent) ackSpeedtestProgress(sent *agentSpeedtestProgress, ackID string) {
	if sent == nil || ackID == "" || ackID != sent.TaskID {
		return
	}
	if sent.Status != "done" && sent.Status != "failed" && sent.Status != "canceled" {
		return
	}
	a.speedtestMu.Lock()
	if a.speedtestProgress != nil && a.speedtestProgress.TaskID == sent.TaskID && a.speedtestProgress.Status == sent.Status {
		a.speedtestProgress = nil
	}
	a.speedtestMu.Unlock()
}

func (a *edgeAgent) handleSpeedtestDirective(parentCtx context.Context, task *agentSpeedtestTask, cancelID string) {
	var cancelFn context.CancelFunc
	var startTask *agentSpeedtestTask
	var taskCtx context.Context

	a.speedtestMu.Lock()
	if cancelID != "" && a.speedtestActiveID == cancelID && a.speedtestCancel != nil {
		cancelFn = a.speedtestCancel
	}
	if task != nil && task.TaskID != "" &&
		task.TaskID != cancelID &&
		task.TaskID != a.speedtestActiveID &&
		task.TaskID != a.speedtestLastDoneID &&
		a.speedtestActiveID == "" {
		taskCtx, a.speedtestCancel = context.WithCancel(parentCtx)
		a.speedtestActiveID = task.TaskID
		cp := agentSpeedtestTask{
			TaskID:     task.TaskID,
			DurationMs: task.DurationMs,
			Targets:    append([]agentSpeedtestTarget(nil), task.Targets...),
		}
		startTask = &cp
	}
	a.speedtestMu.Unlock()

	if cancelFn != nil {
		cancelFn()
	}
	if startTask != nil {
		go a.runSpeedtestTask(taskCtx, *startTask)
	}
}

func (a *edgeAgent) runSpeedtestTask(ctx context.Context, task agentSpeedtestTask) {
	dur := defaultSpeedtestPhaseDuration
	if task.DurationMs > 0 {
		dur = time.Duration(task.DurationMs) * time.Millisecond
	}
	results := make([]speedtestTargetResult, len(task.Targets))
	for i, tgt := range task.Targets {
		results[i] = speedtestTargetResult{
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

	update := func(status string, completed int, errMsg string) {
		a.speedtestMu.Lock()
		a.speedtestProgress = &agentSpeedtestProgress{
			TaskID:           task.TaskID,
			Status:           status,
			CompletedTargets: completed,
			TotalTargets:     len(results),
			Error:            errMsg,
			Results:          append([]speedtestTargetResult(nil), results...),
		}
		a.speedtestMu.Unlock()
		a.triggerSpeedtestNotify()
	}

	a.speedtestMu.Lock()
	trOverride := a.speedtestTransport
	a.speedtestMu.Unlock()

	update("running", 0, "")

	completed := 0
	for i, tgt := range task.Targets {
		if ctx.Err() != nil {
			break
		}
		results[i].Status = "running"
		update("running", completed, "")

		res := executeSpeedtestTarget(ctx, tgt, dur, trOverride)
		if ctx.Err() != nil {
			results[i].Status = "error"
			results[i].Error = "已取消"
			break
		}
		results[i] = res
		completed++
		update("running", completed, "")
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

	a.speedtestMu.Lock()
	a.speedtestProgress = &agentSpeedtestProgress{
		TaskID:           task.TaskID,
		Status:           finalStatus,
		CompletedTargets: completed,
		TotalTargets:     len(results),
		Error:            finalErr,
		Results:          append([]speedtestTargetResult(nil), results...),
	}
	a.speedtestActiveID = ""
	a.speedtestLastDoneID = task.TaskID
	a.speedtestCancel = nil
	a.speedtestMu.Unlock()

	a.triggerSpeedtestNotify()
}

func (a *edgeAgent) sendHeartbeat(ctx context.Context) error {
	reports := a.drainTrafficReports()
	bytesInWindow := a.windowBytes.Swap(0)
	nowNano := time.Now().UnixNano()
	prevNano := a.lastWindowAt.Swap(nowNano)
	elapsedSec := float64(nowNano-prevNano) / float64(time.Second)
	if elapsedSec < 1.0 {
		elapsedSec = 1.0
	}
	bps := int64(float64(bytesInWindow*8) / elapsedSec)

	a.certMu.RLock()
	fp := a.certFingerprint
	tlsActive := a.cert != nil
	a.certMu.RUnlock()

	stProgress := a.snapshotSpeedtestProgress()
	upgradeReqID, upgradeStatus := a.getUpgradeState()
	payload := agentHeartbeatRequest{
		Version:           appVersion,
		ListenPort:        a.cfg.Port,
		TLSActive:         tlsActive,
		ActiveStreams:     int(a.activeStreams.Load()),
		CurrentBps:        bps,
		MemoryBytes:       currentProcessMemoryBytes(),
		UptimeSec:         int64(time.Since(a.startedAt).Seconds()),
		SupportsSpeedtest: true,
		UpstreamLatencies: a.snapshotLatencies(),
		SpeedtestProgress: stProgress,
		UpgradeRequestID:  upgradeReqID,
		UpgradeStatus:     upgradeStatus,
		CertFingerprint:   fp,
		TrafficReports:    reports,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		a.restoreTrafficReports(reports)
		return err
	}
	endpoint := a.cfg.MasterURL + "/api/agent/heartbeat"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		a.restoreTrafficReports(reports)
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Yundu-Node-ID", strconv.FormatInt(a.cfg.NodeID, 10))
	req.Header.Set("X-Yundu-Node-Token", a.cfg.Token)
	req.Header.Set("X-Yundu-Direct-Popup", "1")
	req.Header.Set("User-Agent", "Yundu-Agent/"+appVersion)

	resp, err := a.masterClient.Do(req)
	if err != nil {
		a.restoreTrafficReports(reports)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		a.restoreTrafficReports(reports)
		return fmt.Errorf("master returned HTTP %d", resp.StatusCode)
	}
	var reply agentHeartbeatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&reply); err != nil {
		return err
	}
	a.nodeEnabled.Store(reply.NodeEnabled)

	a.policyMu.Lock()
	blocked := make(map[int64]bool, len(reply.BlockedUsers))
	for _, id := range reply.BlockedUsers {
		blocked[id] = true
	}
	a.blockedUsers = blocked
	revoked := make(map[string]bool, len(reply.RevokedHashes))
	for _, hash := range reply.RevokedHashes {
		revoked[hash] = true
	}
	a.revoked = revoked
	a.speedLimits = reply.UserSpeedLimits
	for userID, mbps := range reply.UserSpeedLimits {
		if mbps > 0 {
			lim := a.limiters[userID]
			if lim == nil {
				lim = &shareRateLimiter{}
				a.limiters[userID] = lim
			}
			lim.setRate(mbps)
		}
	}
	for userID := range a.limiters {
		if reply.UserSpeedLimits[userID] <= 0 {
			delete(a.limiters, userID)
		}
	}
	a.policyMu.Unlock()

	if reply.TLSCertPEM != "" && reply.TLSKeyPEM != "" {
		if pair, err := tls.X509KeyPair([]byte(reply.TLSCertPEM), []byte(reply.TLSKeyPEM)); err == nil {
			a.certMu.Lock()
			a.cert = &pair
			a.certFingerprint = reply.CertFingerprint
			a.certMu.Unlock()
			log.Printf("edge agent: synced TLS certificate (fingerprint %s...)", reply.CertFingerprint[:min(12, len(reply.CertFingerprint))])
		} else {
			log.Printf("edge agent: invalid synced TLS certificate: %v", err)
		}
	}
	a.ackSpeedtestProgress(stProgress, reply.AckSpeedtestID)
	a.setProbeTargets(reply.ProbeTargets)
	a.handleSpeedtestDirective(ctx, reply.SpeedtestTask, reply.CancelSpeedtestID)
	if reply.UpgradeToVersion != "" && reply.UpgradeRequestID != "" {
		a.maybeTriggerUpgrade(ctx, reply.UpgradeToVersion, reply.UpgradeRequestID)
	}
	return nil
}

func (a *edgeAgent) reserveDirectBudget(ticket edgeSpeedtestTicket, requestedBytes int64) bool {
	if requestedBytes < minDirectStreamBytes {
		requestedBytes = minDirectStreamBytes
	}
	if ticket.MaxBytes <= 0 || ticket.MaxBytes > maxDirectStreamBytes {
		ticket.MaxBytes = maxDirectStreamBytes
	}
	if requestedBytes > ticket.MaxBytes {
		requestedBytes = ticket.MaxBytes
	}
	budgetLimit := ticket.MaxBytes * 2
	nowUnix := time.Now().Unix()

	a.directMu.Lock()
	defer a.directMu.Unlock()
	if a.directBudgets == nil {
		a.directBudgets = make(map[string]directStreamBudget)
	}
	for k, v := range a.directBudgets {
		if nowUnix > v.exp {
			delete(a.directBudgets, k)
		}
	}
	entry := a.directBudgets[ticket.JTI]
	if entry.used+requestedBytes > budgetLimit {
		return false
	}
	entry.used += requestedBytes
	entry.exp = ticket.ExpiresAt
	a.directBudgets[ticket.JTI] = entry
	return true
}

func (a *edgeAgent) handleSpeedtestDirectRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Range, Accept, Cache-Control")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Length")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !a.nodeEnabled.Load() {
		writeShareJSONError(w, http.StatusUnauthorized, "节点已停用")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, edgeSpeedtestPathPrefix)
	sealed, _, _ := strings.Cut(rest, "/")
	ticket, err := openEdgeSpeedtestTicket(a.cfg.Token, sealed)
	if err != nil || ticket.NodeID != a.cfg.NodeID {
		writeShareJSONError(w, http.StatusUnauthorized, "测速链接已失效")
		return
	}
	if r.Method == http.MethodGet {
		if cur := a.directActive.Add(1); cur > 2 {
			a.directActive.Add(-1)
			writeShareJSONError(w, http.StatusTooManyRequests, "当前直测并发已满，请稍后再试")
			return
		}
		defer a.directActive.Add(-1)
		requested := defaultDirectStreamBytes
		if raw := strings.TrimSpace(r.URL.Query().Get("bytes")); raw != "" {
			if parsed, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil {
				requested = parsed
			}
		}
		if !a.reserveDirectBudget(ticket, requested) {
			writeShareJSONError(w, http.StatusTooManyRequests, "本机直测令牌流量额度已耗尽")
			return
		}
	}
	serveSpeedtestByteStream(w, r, ticket.MaxBytes)
}

const edgeSpeedtestDirectErrorHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>直连测速</title><style>body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0f1115;color:#e5e7eb;font:14px/1.6 system-ui,-apple-system,sans-serif}.card{max-width:360px;padding:20px 24px;border:1px solid #262b36;border-radius:12px;background:#171a21;text-align:center}p{margin:8px 0 0;color:#9ca3af}</style></head><body><div class="card"><strong>测速链接已失效</strong><p>请返回云渡面板重新发起本机直测。</p></div></body></html>`

const edgeSpeedtestDirectUITemplate = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>云渡 · HTTP 从机直连测速</title>
<style>
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:grid;place-items:center;padding:16px;background:#0f1115;color:#e5e7eb;font:14px/1.5 system-ui,-apple-system,sans-serif}
.card{width:100%;max-width:400px;padding:20px 22px;border:1px solid #262b36;border-radius:12px;background:#171a21}
.head{display:flex;align-items:center;justify-content:space-between;gap:12px;margin-bottom:14px}
.title{font-size:15px;font-weight:600}
.pill{padding:2px 9px;border-radius:999px;font-size:12px;background:#262b36;color:#cbd5e1}
.pill.ok{background:rgba(16,185,129,.16);color:#34d399}
.pill.err{background:rgba(239,68,68,.16);color:#f87171}
.grid{display:grid;grid-template-columns:repeat(3,1fr);gap:10px;padding:12px 0;border-top:1px solid #262b36;border-bottom:1px solid #262b36}
.k{font-size:12px;color:#9ca3af}.v{margin-top:2px;font-size:16px;font-weight:600;font-variant-numeric:tabular-nums}
.note{margin:12px 0 0;font-size:12px;color:#9ca3af;min-height:18px}
.btn{margin-top:12px;height:30px;padding:0 12px;border:1px solid #333a48;border-radius:7px;background:#1f2430;color:#e5e7eb;font-size:12.5px;cursor:pointer}
</style></head><body>
<div class="card">
  <div class="head"><span class="title">本机 ↔ HTTP 从机直连测速</span><span id="st" class="pill">测速中</span></div>
  <div class="grid">
    <div><div class="k">首字节 TTFB</div><div id="ttfb" class="v">--</div></div>
    <div><div class="k">单线程下行</div><div id="mbps" class="v">--</div></div>
    <div><div class="k">已拉取流量</div><div id="bytes" class="v">--</div></div>
  </div>
  <p id="note" class="note">正在建立直连单线程 HTTP 数据流...</p>
  <button id="closeBtn" class="btn" type="button" hidden>关闭窗口</button>
</div>
<script>
(function(){
  const cfg = __CONFIG__;
  const stEl = document.getElementById('st');
  const ttfbEl = document.getElementById('ttfb');
  const mbpsEl = document.getElementById('mbps');
  const bytesEl = document.getElementById('bytes');
  const noteEl = document.getElementById('note');
  const closeBtn = document.getElementById('closeBtn');
  closeBtn.addEventListener('click', function(){ try { window.close(); } catch(e) {} });

  function report(phase, data) {
    if (!cfg.reportUrl || !cfg.reportId || !cfg.reportToken) return Promise.resolve();
    return fetch(cfg.reportUrl, {
      method: 'POST',
      mode: 'cors',
      keepalive: phase !== 'progress',
      headers: { 'Content-Type': 'text/plain' },
      body: JSON.stringify({
        id: cfg.reportId,
        token: cfg.reportToken,
        phase: phase,
        ttfb_ms: data.ttfbMs,
        mbps: data.mbps,
        received_bytes: data.receivedBytes,
        error: data.error || ''
      })
    }).catch(function(){});
  }

  function fmtBytes(n) {
    if (!n || n <= 0) return '0 B';
    if (n >= 1048576) return (n / 1048576).toFixed(1) + ' MiB';
    if (n >= 1024) return (n / 1024).toFixed(1) + ' KiB';
    return n + ' B';
  }

  async function finishSuccess(ttfbMs, mbps, received) {
    stEl.textContent = '完成';
    stEl.className = 'pill ok';
    noteEl.textContent = '测速完成，正在回传结果...';
    await report('done', { ttfbMs: ttfbMs, mbps: mbps, receivedBytes: received });
    if (cfg.reportUrl) {
      noteEl.textContent = '结果已回传面板，窗口即将关闭';
      setTimeout(function(){ try { window.close(); } catch(e) {} }, 400);
    }
    closeBtn.hidden = false;
  }

  async function finishError(ttfbMs, mbps, received, msg) {
    stEl.textContent = '失败';
    stEl.className = 'pill err';
    noteEl.textContent = msg;
    closeBtn.hidden = false;
    await report('error', { ttfbMs: ttfbMs, mbps: mbps, receivedBytes: received, error: msg });
  }

  async function run() {
    const maxMs = cfg.durationMs || 5000;
    const ctrl = new AbortController();
    const timer = setTimeout(function(){ ctrl.abort('duration_reached'); }, maxMs + 2500);
    let ttfbMs = -1, mbps = 0, received = 0;
    let warmAt = 0, warmBytes = 0, lastPost = 0;
    try {
      const startAt = performance.now();
      const resp = await fetch(cfg.streamUrl, { method: 'GET', cache: 'no-store', signal: ctrl.signal });
      if (!resp.ok) throw new Error('HTTP ' + resp.status);
      ttfbMs = Math.max(1, Math.round(performance.now() - startAt));
      ttfbEl.textContent = ttfbMs + 'ms';
      if (!resp.body) throw new Error('浏览器不支持流式读取');
      const reader = resp.body.getReader();
      const streamStart = performance.now();
      noteEl.textContent = '测速中...';
      while (true) {
        if (performance.now() - streamStart >= maxMs) { ctrl.abort('duration_reached'); break; }
        const chunk = await reader.read();
        if (chunk.done) break;
        if (chunk.value) {
          received += chunk.value.byteLength;
          bytesEl.textContent = fmtBytes(received);
          const now = performance.now();
          if (warmAt === 0 && now - streamStart >= 700) { warmAt = now; warmBytes = received; }
          const measureMs = warmAt > 0 && now - warmAt >= 300 ? now - warmAt : Math.max(10, now - streamStart);
          const measureBytes = warmAt > 0 && now - warmAt >= 300 ? received - warmBytes : received;
          mbps = Math.round(((measureBytes * 8) / (measureMs * 1000)) * 10) / 10;
          mbpsEl.textContent = mbps.toFixed(1) + ' Mbps';
          if (now - lastPost >= 250) {
            lastPost = now;
            report('progress', { ttfbMs: ttfbMs, mbps: mbps, receivedBytes: received });
          }
        }
      }
      if (received <= 0) {
        await finishError(ttfbMs, 0, 0, '连接超时或未收到字节');
        return;
      }
      const endNow = performance.now();
      const totalElapsed = Math.max(10, endNow - (warmAt > 0 && endNow - warmAt >= 400 ? warmAt : streamStart));
      const countedBytes = warmAt > 0 && endNow - warmAt >= 400 ? received - warmBytes : received;
      mbps = Math.round(((countedBytes * 8) / (totalElapsed * 1000)) * 10) / 10;
      mbpsEl.textContent = mbps.toFixed(1) + ' Mbps';
      await finishSuccess(ttfbMs, mbps, received);
    } catch (err) {
      if ((ctrl.signal.reason === 'duration_reached' || String(err).indexOf('AbortError') >= 0) && received > 0 && ttfbMs > 0) {
        await finishSuccess(ttfbMs, mbps, received);
      } else {
        const msg = ctrl.signal.reason === 'duration_reached' ? '连接或读取首字节超时' : ((err && err.message) ? err.message : String(err || '直连测速失败'));
        await finishError(ttfbMs, mbps, received, msg);
      }
    } finally {
      clearTimeout(timer);
    }
  }
  run();
})();
</script></body></html>`

func (a *edgeAgent) handleSpeedtestDirectUIRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")

	setCSP := func(reportOrigin string) {
		csp := "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'"
		if reportOrigin != "" {
			csp += " " + reportOrigin
		}
		w.Header().Set("Content-Security-Policy", csp)
	}
	setCSP("")

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !a.nodeEnabled.Load() {
		w.WriteHeader(http.StatusUnauthorized)
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, edgeSpeedtestDirectErrorHTML)
		}
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, edgeSpeedtestUIPathPrefix)
	sealed, _, _ := strings.Cut(rest, "/")
	ticket, err := openEdgeSpeedtestTicket(a.cfg.Token, sealed)
	if err != nil || ticket.NodeID != a.cfg.NodeID {
		w.WriteHeader(http.StatusUnauthorized)
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, edgeSpeedtestDirectErrorHTML)
		}
		return
	}

	maxAllowed := ticket.MaxBytes
	if maxAllowed <= 0 || maxAllowed > maxDirectStreamBytes {
		maxAllowed = maxDirectStreamBytes
	}
	totalBytes := defaultDirectStreamBytes
	if raw := strings.TrimSpace(r.URL.Query().Get("bytes")); raw != "" {
		if parsed, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil {
			totalBytes = parsed
		}
	}
	if totalBytes < minDirectStreamBytes {
		totalBytes = minDirectStreamBytes
	}
	if totalBytes > maxAllowed {
		totalBytes = maxAllowed
	}

	reportOrigin := sanitizeSpeedtestOrigin(ticket.PanelOrigin)
	reportURL := ""
	if reportOrigin != "" && ticket.ReportID != "" && ticket.ReportToken != "" {
		reportURL = reportOrigin + "/api/speedtest/direct-report"
		setCSP(reportOrigin)
	}

	cfg := struct {
		StreamURL   string `json:"streamUrl"`
		ReportURL   string `json:"reportUrl"`
		ReportID    string `json:"reportId"`
		ReportToken string `json:"reportToken"`
		DurationMs  int    `json:"durationMs"`
	}{
		StreamURL:   edgeSpeedtestPathPrefix + url.PathEscape(sealed) + "?bytes=" + strconv.FormatInt(totalBytes, 10),
		ReportURL:   reportURL,
		ReportID:    ticket.ReportID,
		ReportToken: ticket.ReportToken,
		DurationMs:  5000,
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	pageHTML := strings.Replace(edgeSpeedtestDirectUITemplate, "__CONFIG__", string(cfgJSON), 1)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, pageHTML)
	}
}

func (a *edgeAgent) isRequestBlocked(ticket edgePlaybackTicket) (bool, string) {
	if !a.nodeEnabled.Load() {
		return true, "节点已停用"
	}
	a.policyMu.RLock()
	defer a.policyMu.RUnlock()
	if ticket.ShareUserID > 0 && a.blockedUsers[ticket.ShareUserID] {
		return true, "分享账号不可用"
	}
	if ticket.TokenHash != "" && a.revoked[ticket.TokenHash] {
		return true, "登录已失效，请重新登录"
	}
	return false, ""
}

func (a *edgeAgent) limiterForTicket(ticket edgePlaybackTicket) *shareRateLimiter {
	a.policyMu.Lock()
	defer a.policyMu.Unlock()
	if ticket.ShareUserID > 0 {
		if lim := a.limiters[ticket.ShareUserID]; lim != nil {
			return lim
		}
		if mbps := a.speedLimits[ticket.ShareUserID]; mbps > 0 {
			lim := &shareRateLimiter{}
			lim.setRate(mbps)
			a.limiters[ticket.ShareUserID] = lim
			return lim
		}
	}
	if ticket.SpeedMbps > 0 {
		lim := &shareRateLimiter{}
		lim.setRate(ticket.SpeedMbps)
		return lim
	}
	return nil
}

func (a *edgeAgent) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_yundu/health" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"ok":true,"version":%q,"tls_active":%v}`, appVersion, a.hasTLS())
			return
		}
		if r.URL.Path == edgeEgressPath {
			a.handleEgressRequest(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, edgeSpeedtestUIPathPrefix) {
			a.handleSpeedtestDirectUIRequest(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, edgeSpeedtestPathPrefix) {
			a.handleSpeedtestDirectRequest(w, r)
			return
		}
		if !strings.HasPrefix(r.URL.Path, edgeTicketPathPrefix) {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Range, If-Range, Accept")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, edgeTicketPathPrefix)
		sealed, subPath, _ := strings.Cut(rest, "/")
		ticket, err := openEdgePlaybackTicket(a.cfg.Token, sealed)
		if err != nil || ticket.NodeID != a.cfg.NodeID {
			writeShareJSONError(w, http.StatusUnauthorized, "播放链接已失效")
			return
		}
		if blocked, reason := a.isRequestBlocked(ticket); blocked {
			writeShareJSONError(w, http.StatusUnauthorized, reason)
			return
		}

		targetURL, err := resolveAgentTicketURL(ticket.TargetURL, subPath, r.URL.RawQuery)
		if err != nil {
			writeShareJSONError(w, http.StatusBadRequest, "无效的媒体地址")
			return
		}

		a.activeStreams.Add(1)
		defer func() {
			a.activeStreams.Add(-1)
			a.triggerFlush()
		}()

		resp, err := a.fetchUpstream(r.Context(), r, targetURL, ticket)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			log.Printf("edge agent upstream error: %v", err)
			writeShareJSONError(w, http.StatusBadGateway, "上游媒体暂不可用")
			return
		}
		defer resp.Body.Close()

		copyAgentResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if r.Method == http.MethodHead {
			a.recordTraffic(ticket.SiteID, ticket.ShareUserID, ticket.TokenHash, 0, 0, 1, true)
			return
		}

		limiter := a.limiterForTicket(ticket)
		buf := make([]byte, edgeAgentCopyBufSize)
		var unreported int64
		first := true
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				if limiter != nil {
					if waitErr := limiter.wait(r.Context(), n); waitErr != nil {
						break
					}
				}
				written, writeErr := w.Write(buf[:n])
				unreported += int64(written)
				if unreported >= 1<<20 {
					reqs := int64(0)
					if first {
						reqs = 1
						first = false
					}
					a.recordTraffic(ticket.SiteID, ticket.ShareUserID, ticket.TokenHash, 0, unreported, reqs, true)
					unreported = 0
					// Mid-stream check so kicked/blocked users are cut off promptly.
					if blocked, _ := a.isRequestBlocked(ticket); blocked {
						break
					}
				}
				if writeErr != nil {
					break
				}
			}
			if readErr != nil {
				break
			}
		}
		reqs := int64(0)
		if first {
			reqs = 1
		}
		if unreported > 0 || reqs > 0 {
			a.recordTraffic(ticket.SiteID, ticket.ShareUserID, ticket.TokenHash, 0, unreported, reqs, false)
		}
	})
}

func resolveAgentTicketURL(rawBase, subPath, clientQuery string) (*url.URL, error) {
	base, err := url.Parse(rawBase)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, errors.New("invalid base target URL")
	}
	subPath = strings.TrimSpace(subPath)
	baseName := path.Base(base.Path)
	// If the client requested a relative HLS/DASH segment or sibling manifest,
	// resolve it relative to the ticket's base URL directory.
	if subPath != "" && subPath != baseName {
		rel, err := url.Parse(subPath)
		if err == nil && !rel.IsAbs() && rel.Host == "" {
			base = base.ResolveReference(rel)
		}
	}
	if clientQuery != "" {
		if base.RawQuery == "" {
			base.RawQuery = clientQuery
		} else if base.RawQuery != clientQuery {
			base.RawQuery = base.RawQuery + "&" + clientQuery
		}
	}
	return base, nil
}

func (a *edgeAgent) fetchUpstream(ctx context.Context, inbound *http.Request, initialURL *url.URL, ticket edgePlaybackTicket) (*http.Response, error) {
	currentURL := initialURL
	originAuthority := redirectHostKey(initialURL)
	for hop := 0; hop <= edgeAgentMaxRedirects; hop++ {
		// #nosec G704 -- currentURL comes from an AES-256-GCM ticket authenticated by the master's node token and scheme-validated to http/https.
		req, err := http.NewRequestWithContext(ctx, inbound.Method, currentURL.String(), nil)
		if err != nil {
			return nil, err
		}
		sameOrigin := redirectHostKey(currentURL) == originAuthority
		for k, v := range ticket.Headers {
			if !sameOrigin && isSensitiveEdgeRedirectHeader(k) {
				continue
			}
			req.Header.Set(k, v)
		}
		for _, passthrough := range []string{"Range", "If-Range", "Accept", "Accept-Encoding", "If-Modified-Since", "If-None-Match"} {
			if vals := inbound.Header.Values(passthrough); len(vals) > 0 {
				req.Header[http.CanonicalHeaderKey(passthrough)] = append([]string(nil), vals...)
			}
		}
		if req.Header.Get("User-Agent") == "" {
			if ua := inbound.Header.Get("User-Agent"); ua != "" {
				req.Header.Set("User-Agent", ua)
			} else {
				req.Header.Set("User-Agent", "Yundu-Agent/"+appVersion)
			}
		}
		// #nosec G704 -- upstream media URL is authorized by the master's AES-GCM playback ticket.
		resp, err := a.upstreamClient.Do(req)
		if err != nil {
			return nil, err
		}
		if !ticket.FollowRedir || !isEdgeRedirectStatus(resp.StatusCode) {
			return resp, nil
		}
		loc := strings.TrimSpace(resp.Header.Get("Location"))
		if loc == "" {
			return resp, nil
		}
		next, parseErr := currentURL.Parse(loc)
		if parseErr != nil || (next.Scheme != "http" && next.Scheme != "https") || next.Host == "" {
			return resp, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		currentURL = next
	}
	return nil, errors.New("too many upstream redirects")
}

func isEdgeRedirectStatus(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

func isSensitiveEdgeRedirectHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "authorization", "cookie", "x-emby-authorization", "x-emby-token", "x-mediabrowser-token":
		return true
	default:
		return false
	}
}

func copyAgentResponseHeaders(dst, src http.Header) {
	allowed := []string{
		"Accept-Ranges", "Content-Disposition", "Content-Encoding", "Content-Language",
		"Content-Length", "Content-Range", "Content-Type", "Date", "ETag", "Last-Modified",
	}
	for _, name := range allowed {
		if vals := src.Values(name); len(vals) > 0 {
			dst[http.CanonicalHeaderKey(name)] = append([]string(nil), vals...)
		}
	}
	dst.Set("Cache-Control", "private, no-store")
	dst.Set("Access-Control-Allow-Origin", "*")
	dst.Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges")
}

func isHopByHopHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func connectionHeaderTokens(h http.Header) map[string]bool {
	var tokens map[string]bool
	for _, val := range h.Values("Connection") {
		for _, part := range strings.Split(val, ",") {
			name := strings.ToLower(strings.TrimSpace(part))
			if name == "" {
				continue
			}
			if tokens == nil {
				tokens = make(map[string]bool)
			}
			tokens[name] = true
		}
	}
	return tokens
}

// handleEgressRequest forwards an authenticated master control-plane HTTP or
// WebSocket request to the upstream Emby server out through this edge node's IP.
func (a *edgeAgent) handleEgressRequest(w http.ResponseWriter, r *http.Request) {
	if !a.nodeEnabled.Load() {
		w.Header().Set(edgeEgressMarkerHeader, "1")
		writeShareJSONError(w, http.StatusUnauthorized, "节点已停用")
		return
	}
	rawTicket := strings.TrimSpace(r.Header.Get(edgeEgressTicketHeader))
	ticket, err := openEdgeEgressTicket(a.cfg.Token, rawTicket)
	if err != nil || ticket.NodeID != a.cfg.NodeID {
		w.Header().Set(edgeEgressMarkerHeader, "1")
		writeShareJSONError(w, http.StatusUnauthorized, "出口票据无效或已过期")
		return
	}
	targetURL, err := url.Parse(ticket.TargetURL)
	if err != nil || (targetURL.Scheme != "http" && targetURL.Scheme != "https") || targetURL.Host == "" {
		w.Header().Set(edgeEgressMarkerHeader, "1")
		writeShareJSONError(w, http.StatusBadRequest, "无效的回源地址")
		return
	}
	if hasUpgradeIntent(r) {
		if !isWebSocketUpgrade(r) {
			w.Header().Set(edgeEgressMarkerHeader, "1")
			writeShareJSONError(w, http.StatusBadRequest, "invalid websocket upgrade")
			return
		}
		a.handleEgressWebSocket(w, r, targetURL, ticket.Host)
		return
	}

	client := a.egressClient
	if client == nil {
		client = a.upstreamClient
	}
	// #nosec G704 -- targetURL comes from an AES-256-GCM ticket authenticated by the master's node token and scheme-validated to http/https.
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL.String(), r.Body)
	if err != nil {
		w.Header().Set(edgeEgressMarkerHeader, "1")
		writeShareJSONError(w, http.StatusBadRequest, "构建回源请求失败")
		return
	}
	outReq.ContentLength = r.ContentLength
	dropTokens := connectionHeaderTokens(r.Header)
	for k, vals := range r.Header {
		lk := strings.ToLower(k)
		if lk == strings.ToLower(edgeEgressTicketHeader) || isHopByHopHeader(lk) || dropTokens[lk] {
			continue
		}
		outReq.Header[k] = append([]string(nil), vals...)
	}
	if strings.TrimSpace(ticket.Host) != "" {
		outReq.Host = strings.TrimSpace(ticket.Host)
	} else {
		outReq.Host = targetURL.Host
	}
	// #nosec G704 -- upstream target URL is authorized by the master's AES-GCM egress ticket.
	resp, err := client.Do(outReq)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		log.Printf("edge agent egress error (%s): %v", targetURL.Host, err)
		w.Header().Set(edgeEgressMarkerHeader, "1")
		writeShareJSONError(w, http.StatusBadGateway, "上游暂不可用")
		return
	}
	defer resp.Body.Close()

	respDropTokens := connectionHeaderTokens(resp.Header)
	dst := w.Header()
	for k, vals := range resp.Header {
		lk := strings.ToLower(k)
		if isHopByHopHeader(lk) || respDropTokens[lk] {
			continue
		}
		dst[k] = append([]string(nil), vals...)
	}
	dst.Set(edgeEgressMarkerHeader, "1")
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	buf := make([]byte, edgeAgentCopyBufSize)
	_, _ = io.CopyBuffer(w, resp.Body, buf)
}

func (a *edgeAgent) handleEgressWebSocket(w http.ResponseWriter, r *http.Request, target *url.URL, overrideHost string) {
	if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
		w.Header().Set(edgeEgressMarkerHeader, "1")
		http.Error(w, "websocket upgrade must not carry a body", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		w.Header().Set(edgeEgressMarkerHeader, "1")
		http.Error(w, "WebSocket not supported", http.StatusInternalServerError)
		return
	}
	port := target.Port()
	if port == "" {
		if target.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	hostPort := net.JoinHostPort(target.Hostname(), port)
	var upstreamConn net.Conn
	var err error
	if target.Scheme == "https" {
		upstreamConn, err = tls.DialWithDialer(dialer, "tcp", hostPort, secureTLSConfig(target.Hostname()))
	} else {
		upstreamConn, err = dialer.Dial("tcp", hostPort)
	}
	if err != nil {
		log.Printf("edge agent egress ws dial error: %v", err)
		w.Header().Set(edgeEgressMarkerHeader, "1")
		writeShareJSONError(w, http.StatusBadGateway, "上游 WebSocket 暂不可用")
		return
	}
	defer upstreamConn.Close()

	if err := upstreamConn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return
	}
	reqLine := fmt.Sprintf("%s %s HTTP/1.1\r\n", r.Method, target.RequestURI())
	if _, err := io.WriteString(upstreamConn, reqLine); err != nil { // #nosec G705 -- method and RequestURI are validated by net/http and url.Parse.
		return
	}
	outHeader := r.Header.Clone()
	outHeader.Del(edgeEgressTicketHeader)
	upstreamHost := target.Host
	if strings.TrimSpace(overrideHost) != "" {
		upstreamHost = strings.TrimSpace(overrideHost)
	}
	outHeader.Set("Host", upstreamHost)
	if err := outHeader.Write(upstreamConn); err != nil {
		return
	}
	if _, err := io.WriteString(upstreamConn, "\r\n"); err != nil {
		return
	}
	if err := upstreamConn.SetWriteDeadline(time.Time{}); err != nil {
		return
	}
	if err := upstreamConn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return
	}
	upstreamReader := bufio.NewReader(upstreamConn)
	resp, err := http.ReadResponse(upstreamReader, r)
	if err != nil {
		log.Printf("edge agent egress ws handshake error: %v", err)
		w.Header().Set(edgeEgressMarkerHeader, "1")
		writeShareJSONError(w, http.StatusBadGateway, "上游 WebSocket 握手失败")
		return
	}
	defer resp.Body.Close()
	if err := upstreamConn.SetReadDeadline(time.Time{}); err != nil {
		return
	}
	if resp.StatusCode != http.StatusSwitchingProtocols ||
		!strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") ||
		!headerHasToken(resp.Header, "Connection", "upgrade") {
		w.Header().Set(edgeEgressMarkerHeader, "1")
		writeShareJSONError(w, http.StatusBadGateway, "upstream refused websocket upgrade")
		return
	}

	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()
	if _, err := io.WriteString(clientConn, "HTTP/1.1 101 Switching Protocols\r\n"); err != nil {
		return
	}
	if err := resp.Header.Write(clientConn); err != nil {
		return
	}
	if _, err := io.WriteString(clientConn, "\r\n"); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstreamConn, clientBuf)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(clientConn, upstreamReader)
		done <- struct{}{}
	}()
	<-done
	_ = clientConn.Close()
	_ = upstreamConn.Close()
	<-done
}
