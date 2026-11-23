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
	MasterURL string
	NodeID    int64
	Token     string
	BindAddr  string
	Port      int
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
	masterClient   *http.Client
	upstreamClient *http.Client
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

	agent := &edgeAgent{
		cfg: cfg,
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
		blockedUsers: make(map[int64]bool),
		revoked:      make(map[string]bool),
		limiters:     make(map[int64]*shareRateLimiter),
		speedLimits:  make(map[int64]int),
		traffic:      make(map[agentTrafficKey]*agentTrafficAccum),
		flushCh:      make(chan struct{}, 1),
	}
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
		}
	}
}

func (a *edgeAgent) sendHeartbeat(ctx context.Context) error {
	reports := a.drainTrafficReports()
	bytesInWindow := a.windowBytes.Swap(0)
	bps := bytesInWindow * 8 / int64(edgeAgentHeartbeatInterval/time.Second)

	a.certMu.RLock()
	fp := a.certFingerprint
	tlsActive := a.cert != nil
	a.certMu.RUnlock()

	payload := agentHeartbeatRequest{
		Version:         appVersion,
		ListenPort:      a.cfg.Port,
		TLSActive:       tlsActive,
		ActiveStreams:   int(a.activeStreams.Load()),
		CurrentBps:      bps,
		CertFingerprint: fp,
		TrafficReports:  reports,
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
	return nil
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
