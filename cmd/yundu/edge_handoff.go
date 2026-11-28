package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	shareHandoffCooldown     = 30 * time.Second
	shareHandoffLoginTimeout = 10 * time.Second
)

type sharePreWatchingContextKey struct{}

func withSharePreWatching(ctx context.Context, watching bool) context.Context {
	return context.WithValue(ctx, sharePreWatchingContextKey{}, watching)
}

func sharePreWatchingFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(sharePreWatchingContextKey{}).(bool)
	return v
}

func isShareClientLogoutRequest(r *http.Request) bool {
	if r == nil || r.Method != http.MethodPost || r.URL == nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(strings.TrimRight(r.URL.Path, "/")), "/sessions/logout")
}

// rewriteEmbyAuthHeaderToken replaces Token="..." (or Token=...) inside a
// MediaBrowser/Emby Authorization header when its value matches oldToken.
func rewriteEmbyAuthHeaderToken(value, oldToken, newToken string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || oldToken == "" || newToken == "" || oldToken == newToken {
		return value
	}
	offset := 0
	for offset < len(trimmed) && isEmbyAuthToken(trimmed[offset]) {
		offset++
	}
	scheme := trimmed[:offset]
	if !strings.EqualFold(scheme, "MediaBrowser") && !strings.EqualFold(scheme, "Emby") {
		return value
	}
	// Walk key=value pairs while preserving exact spacing and other attributes.
	var out strings.Builder
	out.Grow(len(value) + len(newToken))
	pos := 0
	i := offset
	changed := false
	for i < len(trimmed) {
		for i < len(trimmed) && (trimmed[i] == ' ' || trimmed[i] == '\t' || trimmed[i] == ',') {
			i++
		}
		if i >= len(trimmed) {
			break
		}
		keyStart := i
		for i < len(trimmed) && isEmbyAuthToken(trimmed[i]) {
			i++
		}
		key := trimmed[keyStart:i]
		for i < len(trimmed) && (trimmed[i] == ' ' || trimmed[i] == '\t') {
			i++
		}
		if i >= len(trimmed) || trimmed[i] != '=' {
			for i < len(trimmed) && trimmed[i] != ',' {
				i++
			}
			continue
		}
		i++ // skip '='
		for i < len(trimmed) && (trimmed[i] == ' ' || trimmed[i] == '\t') {
			i++
		}
		if i >= len(trimmed) {
			break
		}
		quoted := false
		valStart := i
		var valEnd int
		var rawVal string
		if trimmed[i] == '"' {
			quoted = true
			i++
			valStart = i
			for i < len(trimmed) {
				if trimmed[i] == '\\' && i+1 < len(trimmed) {
					i += 2
					continue
				}
				if trimmed[i] == '"' {
					break
				}
				i++
			}
			valEnd = i
			rawVal = trimmed[valStart:valEnd]
			if i < len(trimmed) && trimmed[i] == '"' {
				i++
			}
		} else {
			for i < len(trimmed) && trimmed[i] != ',' {
				i++
			}
			valEnd = i
			rawVal = strings.TrimSpace(trimmed[valStart:valEnd])
		}
		if strings.EqualFold(key, "Token") && rawVal == oldToken {
			out.WriteString(trimmed[pos:valStart])
			if quoted {
				out.WriteString(strings.ReplaceAll(newToken, `"`, `\"`))
			} else {
				out.WriteString(newToken)
			}
			pos = valEnd
			changed = true
		}
	}
	if !changed {
		return value
	}
	out.WriteString(trimmed[pos:])
	return out.String()
}

// rewriteEmbyRequestToken transparently substitutes the client's original Emby
// access token with the session's active upstream token across headers and URL query.
func rewriteEmbyRequestToken(r *http.Request, oldToken, newToken string) {
	if r == nil || oldToken == "" || newToken == "" || oldToken == newToken {
		return
	}
	for _, hdr := range []string{"X-Emby-Token", "X-MediaBrowser-Token"} {
		if strings.TrimSpace(r.Header.Get(hdr)) == oldToken {
			r.Header.Set(hdr, newToken)
		}
	}
	for _, hdr := range []string{"X-Emby-Authorization", "Authorization"} {
		if vals := r.Header.Values(hdr); len(vals) > 0 {
			for idx, v := range vals {
				vals[idx] = rewriteEmbyAuthHeaderToken(v, oldToken, newToken)
			}
		}
	}
	if r.URL != nil && r.URL.RawQuery != "" {
		q := r.URL.Query()
		queryChanged := false
		for key, values := range q {
			switch strings.ToLower(key) {
			case "api_key", "apikey", "x-emby-token", "x-mediabrowser-token":
				for idx, v := range values {
					if strings.TrimSpace(v) == oldToken {
						values[idx] = newToken
						queryChanged = true
					}
				}
			}
		}
		if queryChanged {
			r.URL.RawQuery = q.Encode()
		}
	}
}

const defaultShareHandoffUserAgent = "Emby/4.8.10"

type shareHandoffClientHeadersKey struct{}

type shareHandoffClientHeaders struct {
	UserAgent     string
	ClientVersion string
}

func extractShareHandoffClientHeaders(r *http.Request) shareHandoffClientHeaders {
	if r == nil {
		return shareHandoffClientHeaders{}
	}
	ua := strings.TrimSpace(r.Header.Get("User-Agent"))
	ver := strings.TrimSpace(r.Header.Get("X-Emby-Client-Version"))
	if ver == "" {
		ver = strings.TrimSpace(embyRequestAuthorizationAttribute(r, "Version"))
	}
	return shareHandoffClientHeaders{
		UserAgent:     ua,
		ClientVersion: ver,
	}
}

func withShareHandoffClientHeaders(ctx context.Context, r *http.Request) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	hdrs := extractShareHandoffClientHeaders(r)
	if hdrs.UserAgent == "" && hdrs.ClientVersion == "" {
		return ctx
	}
	return context.WithValue(ctx, shareHandoffClientHeadersKey{}, hdrs)
}

func shareHandoffClientHeadersFromContext(ctx context.Context) shareHandoffClientHeaders {
	if ctx == nil {
		return shareHandoffClientHeaders{}
	}
	hdrs, _ := ctx.Value(shareHandoffClientHeadersKey{}).(shareHandoffClientHeaders)
	return hdrs
}

func sanitizeShareHandoffUserAgent(ua string) string {
	ua = strings.TrimSpace(strings.NewReplacer("\r", "", "\n", "").Replace(ua))
	if ua == "" || strings.HasPrefix(strings.ToLower(ua), "go-http-client/") {
		return defaultShareHandoffUserAgent
	}
	return ua
}

func quoteEmbyAuthValue(v, fallback string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		v = fallback
	}
	v = strings.NewReplacer(`"`, "", "\r", "", "\n", "").Replace(v)
	return v
}

// newShareLoginFunc builds the upstream /Users/AuthenticateByName call used by
// edge_full seamless handoff, sharing the same transport, identity policy, and
// configured upstream headers as proxied traffic.
func newShareLoginFunc(
	transport http.RoundTripper,
	target *url.URL,
	policy UAHeaderPolicy,
	headers upstreamHeaderPolicy,
) func(context.Context, string, string, string, string, string) (string, string, int, error) {
	return func(ctx context.Context, username, password, deviceID, deviceName, clientName string) (string, string, int, error) {
		endpoint := *target
		endpoint.Path = singleJoiningSlash(target.Path, "/Users/AuthenticateByName")
		endpoint.RawPath = ""
		endpoint.RawQuery = ""
		body, err := json.Marshal(map[string]string{
			"Username": username,
			"Pw":       password,
		})
		if err != nil {
			return "", "", 0, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
		if err != nil {
			return "", "", 0, err
		}
		clientHdrs := shareHandoffClientHeadersFromContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json")
		request.Header.Set("Accept-Encoding", "identity")
		request.Header.Set("User-Agent", sanitizeShareHandoffUserAgent(clientHdrs.UserAgent))
		authClient := quoteEmbyAuthValue(clientName, "Yundu")
		authDevice := quoteEmbyAuthValue(deviceName, "Yundu")
		authDeviceID := quoteEmbyAuthValue(deviceID, "yundu-share-handoff")
		authVersion := quoteEmbyAuthValue(clientHdrs.ClientVersion, "4.8.10")
		request.Header.Set("X-Emby-Authorization", fmt.Sprintf(
			`MediaBrowser Client="%s", Device="%s", DeviceId="%s", Version="%s"`,
			authClient, authDevice, authDeviceID, authVersion,
		))
		applyUAHeaderPolicy(request.Header, policy)
		headers.apply(request.Header, &endpoint)

		client := &http.Client{
			Transport: transport,
			Timeout:   shareHandoffLoginTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		resp, err := client.Do(request)
		if err != nil {
			return "", "", 0, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, shareLoginResponseLimit))
		if err != nil {
			return "", "", resp.StatusCode, err
		}
		if resp.StatusCode != http.StatusOK {
			return "", "", resp.StatusCode, fmt.Errorf("upstream login returned %d", resp.StatusCode)
		}
		var payload struct {
			AccessToken string `json:"AccessToken"`
			User        struct {
				ID string `json:"Id"`
			} `json:"User"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return "", "", resp.StatusCode, err
		}
		token := strings.TrimSpace(payload.AccessToken)
		if token == "" {
			return "", "", resp.StatusCode, errors.New("upstream login returned empty AccessToken")
		}
		return token, strings.TrimSpace(payload.User.ID), resp.StatusCode, nil
	}
}

func (rt *shareUserRuntime) sessionHandoffLock(tokenHash string) *sync.Mutex {
	rt.handoffMu.Lock()
	defer rt.handoffMu.Unlock()
	if rt.handoffLocks == nil {
		rt.handoffLocks = make(map[string]*sync.Mutex)
	}
	lock := rt.handoffLocks[tokenHash]
	if lock == nil {
		lock = &sync.Mutex{}
		rt.handoffLocks[tokenHash] = lock
	}
	return lock
}

func (rt *shareUserRuntime) handoffCredentials(siteID, accountID int64) (shareUpstreamCandidate, bool) {
	accounts, err := rt.db.listSiteAccounts(siteID)
	if err != nil {
		return shareUpstreamCandidate{}, false
	}
	for _, item := range accounts {
		if accountID > 0 && item.ID != accountID {
			continue
		}
		if item.Username == "" || item.PasswordCiphertext == "" {
			continue
		}
		password, decErr := decryptSiteAccountSecret(item.PasswordCiphertext)
		if decErr != nil || password == "" {
			continue
		}
		return shareUpstreamCandidate{
			accountID: item.ID,
			username:  item.Username,
			password:  password,
		}, true
	}
	return shareUpstreamCandidate{}, false
}

func (rt *shareUserRuntime) applyUpstreamToken(r *http.Request, tokenHash string) {
	if rt == nil || r == nil || tokenHash == "" {
		return
	}
	rt.mu.RLock()
	entry, ok := rt.sessions[tokenHash]
	rt.mu.RUnlock()
	if !ok || entry.clientToken == "" || entry.upstreamToken == "" || entry.clientToken == entry.upstreamToken {
		return
	}
	rewriteEmbyRequestToken(r, entry.clientToken, entry.upstreamToken)
}

// dropLocalSession removes the source session after a failed edge handoff
// whose old upstream token was already logged out.
func (rt *shareUserRuntime) dropLocalSession(tokenHash string, expected shareSessionEntry) {
	if rt == nil || tokenHash == "" {
		return
	}
	rt.sessionStoreMu.Lock()
	defer rt.sessionStoreMu.Unlock()
	rt.mu.RLock()
	cur, exists := rt.sessions[tokenHash]
	rt.mu.RUnlock()
	if !exists || !cur.sameHandoffSource(expected) {
		return
	}
	if rt.db != nil {
		_, _ = rt.db.db.Exec(`DELETE FROM share_user_sessions WHERE token_hash=?`, tokenHash)
	}
	rt.mu.Lock()
	delete(rt.sessions, tokenHash)
	delete(rt.lastSeen, tokenHash)
	delete(rt.activity, tokenHash)
	delete(rt.dirtyClientIP, tokenHash)
	rt.mu.Unlock()
	rt.handoffMu.Lock()
	delete(rt.handoffLocks, tokenHash)
	rt.handoffMu.Unlock()
}

// maybeHandoffEdgeFull performs a seamless, single-IP-safe session handoff for
// an edge_full auto-ISP share session whenever the client switches networks/ISPs,
// a higher-priority node becomes available, or the previously bound edge node
// goes offline. Active playback is never interrupted while the current node is
// healthy; concurrent requests on the same session wait on a per-session lock so
// all of them transition atomically to the new upstream token and node.
func (rt *shareUserRuntime) maybeHandoffEdgeFull(
	ctx context.Context,
	inst *ProxyInstance,
	r *http.Request,
	tokenHash string,
	clientIP string,
	wasWatching bool,
) (int, string) {
	if rt == nil || inst == nil || rt.pm == nil || rt.pm.edges == nil || tokenHash == "" {
		return 0, ""
	}
	site := inst.Site
	if site.EdgeRoutingMode != edgeRoutingModeEdgeFull || site.EdgePinnedNodeID > 0 {
		return 0, ""
	}
	if inst.shareLogin == nil || isShareClientLogoutRequest(r) {
		return 0, ""
	}

	rt.mu.RLock()
	entry, ok := rt.sessions[tokenHash]
	rt.mu.RUnlock()
	if !ok || entry.egressNodeID == 0 {
		return 0, ""
	}

	eval := rt.pm.edges.evaluateEdgeFullHandoff(clientIP, entry.egressNodeID, entry.ispGroup)
	if !eval.shouldHandoff {
		return 0, ""
	}
	if wasWatching && eval.currentRoutable {
		return 0, ""
	}
	if eval.currentRoutable && !entry.lastHandoffAt.IsZero() && time.Since(entry.lastHandoffAt) < shareHandoffCooldown {
		return 0, ""
	}

	// Serialize concurrent requests on this session so only one handoff executes.
	lock := rt.sessionHandoffLock(tokenHash)
	lock.Lock()
	defer lock.Unlock()

	rt.mu.RLock()
	entry, ok = rt.sessions[tokenHash]
	rt.mu.RUnlock()
	if !ok || entry.egressNodeID == 0 {
		return 0, ""
	}
	eval = rt.pm.edges.evaluateEdgeFullHandoff(clientIP, entry.egressNodeID, entry.ispGroup)
	if !eval.shouldHandoff {
		return 0, ""
	}
	if wasWatching && eval.currentRoutable {
		return 0, ""
	}
	if eval.currentRoutable && !entry.lastHandoffAt.IsZero() && time.Since(entry.lastHandoffAt) < shareHandoffCooldown {
		return 0, ""
	}

	cred, hasCred := rt.handoffCredentials(site.ID, entry.siteAccountID)
	if !hasCred {
		if eval.currentRoutable {
			return 0, ""
		}
		return http.StatusServiceUnavailable, "当前绑定的从机离线且未找到可用源站账号，请重新登录"
	}

	oldNodeID := entry.egressNodeID
	oldNodeName := rt.pm.edges.masterDisplayName()
	if oldNodeID > 0 {
		if name := rt.pm.edges.nodeNameByID(oldNodeID); name != "" {
			oldNodeName = name
		}
	}

	// Step 1: Cleanly log out the old upstream token from the old node first so
	// the upstream Emby server never sees two IPs active simultaneously.
	if eval.currentRoutable && inst.shareLogout != nil && entry.upstreamToken != "" {
		logoutCtx, logoutCancel := context.WithTimeout(withShareHandoffClientHeaders(context.Background(), r), shareLogoutTimeout)
		if oldNodeID > 0 {
			if oldDecision, ok := rt.pm.edges.nodeDecisionByID(oldNodeID); ok {
				logoutCtx = withEdgeEgressTarget(logoutCtx, edgeEgressTarget{
					NodeID:    oldDecision.NodeID,
					NodeName:  oldDecision.NodeName,
					PublicURL: oldDecision.PublicURL,
					Token:     oldDecision.Token,
				})
			}
		}
		if err := inst.shareLogout(logoutCtx, entry.upstreamToken); err != nil {
			log.Printf("[%s] edge_full handoff old-node logout (%s): %v", site.Name, oldNodeName, err)
		}
		logoutCancel()
	}

	// Step 2: Re-authenticate with the exact same upstream account via the new node.
	loginCtx, loginCancel := context.WithTimeout(withShareHandoffClientHeaders(ctx, r), shareHandoffLoginTimeout)
	if eval.targetLockedID > 0 {
		loginCtx = withEdgeEgressTarget(loginCtx, edgeEgressTarget{
			NodeID:    eval.targetDecision.NodeID,
			NodeName:  eval.targetDecision.NodeName,
			PublicURL: eval.targetDecision.PublicURL,
			Token:     eval.targetDecision.Token,
		})
	}
	newToken, newUID, statusCode, loginErr := inst.shareLogin(
		loginCtx, cred.username, cred.password, entry.deviceID, entry.deviceName, entry.clientName,
	)
	loginCancel()
	if loginErr != nil || statusCode != http.StatusOK || newToken == "" {
		log.Printf("[%s] edge_full handoff login via %s failed (status=%d): %v", site.Name, eval.targetDecision.NodeName, statusCode, loginErr)
		rt.dropLocalSession(tokenHash, entry)
		return http.StatusUnauthorized, "线路切换后重新登录失败，请重新登录"
	}

	// Guard against upstream User.Id mismatch so client cached user paths stay valid.
	if entry.upstreamUID != "" && newUID != "" && entry.upstreamUID != newUID {
		log.Printf("[%s] edge_full handoff aborted: upstream User.Id changed (%s -> %s)", site.Name, entry.upstreamUID, newUID)
		logoutUncommittedHandoffToken(ctx, inst, newToken, eval.targetDecision)
		rt.dropLocalSession(tokenHash, entry)
		return http.StatusUnauthorized, "源站账号信息已变更，请重新登录"
	}

	upCipher, encErr := encryptSiteAccountSecret(newToken)
	if encErr != nil {
		log.Printf("[%s] edge_full handoff encrypt token: %v", site.Name, encErr)
		logoutUncommittedHandoffToken(ctx, inst, newToken, eval.targetDecision)
		rt.dropLocalSession(tokenHash, entry)
		return http.StatusUnauthorized, "线路切换失败，请重新登录"
	}

	newNodeName := eval.targetDecision.NodeName
	if newNodeName == "" {
		newNodeName = rt.pm.edges.masterDisplayName()
	}
	status, message := rt.commitEdgeFullHandoff(tokenHash, entry, newToken, newUID, upCipher, clientIP, eval.clientISPGroup, eval.targetLockedID, newNodeName)
	if status != 0 {
		logoutUncommittedHandoffToken(ctx, inst, newToken, eval.targetDecision)
		return status, message
		}

	log.Printf("[%s] edge_full seamless handoff (%s): %s -> %s (client_ip=%s, isp=%s)",
		site.Name, eval.reason, oldNodeName, newNodeName, clientIP, eval.clientISPGroup)
	return 0, ""
}

func (rt *shareUserRuntime) commitEdgeFullHandoff(tokenHash string, prior shareSessionEntry, token, uid, cipher, clientIP, ispGroup string, nodeID int64, nodeName string) (int, string) {
	rt.sessionStoreMu.Lock()
	defer rt.sessionStoreMu.Unlock()
	rt.mu.RLock()
	cur, exists := rt.sessions[tokenHash]
	_, revoked := rt.revoked[tokenHash]
	rt.mu.RUnlock()
	if !exists || revoked {
		return http.StatusUnauthorized, "登录已失效，请重新登录"
	}
	if !cur.sameHandoffSource(prior) {
		return http.StatusServiceUnavailable, "会话状态已变更，请重试"
	}
	res, err := rt.db.db.Exec(
		`UPDATE share_user_sessions SET upstream_token_ciphertext=?, upstream_user_id=CASE WHEN upstream_user_id='' THEN ? ELSE upstream_user_id END, egress_node_id=?, client_ip=?, last_seen_at=CURRENT_TIMESTAMP
		 WHERE token_hash=? AND share_user_id=? AND site_id=?
		   AND NOT EXISTS (SELECT 1 FROM share_revoked_tokens WHERE token_hash=?)`,
		cipher, uid, nodeID, clientIP, tokenHash, prior.userID, prior.siteID, tokenHash,
	)
	if err != nil {
		log.Printf("share session #%d edge_full handoff persist: %v", prior.userID, err)
		return http.StatusServiceUnavailable, "保存线路切换失败，请稍后重试"
	}
	if affected, err := res.RowsAffected(); err != nil || affected != 1 {
		return http.StatusUnauthorized, "登录已失效，请重新登录"
	}

	rt.mu.Lock()
	cur = rt.sessions[tokenHash]
	cur.upstreamToken = token
	if cur.upstreamUID == "" {
		cur.upstreamUID = uid
	}
	cur.egressNodeID = nodeID
	if clientIP != "" {
		cur.clientIP = clientIP
	}
	cur.ispGroup = ispGroup
	cur.lastHandoffAt = time.Now()
	rt.sessions[tokenHash] = cur
	delete(rt.dirtyClientIP, tokenHash)
	if act := rt.activity[tokenHash]; act != nil {
		act.lastNodeName = nodeName
		act.lastNodeID = max(0, nodeID)
		act.hasNode = true
	}
	rt.mu.Unlock()
	return 0, ""
}
