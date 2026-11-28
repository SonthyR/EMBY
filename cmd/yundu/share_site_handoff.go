package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// sharePersonalVirtualServerID derives a stable 32-hex Emby-compatible ServerId
// for a share user's personal route prefix. Keeping ServerId constant across
// primary-site switches allows clients like Infuse and official Emby apps to
// refresh the library without re-authenticating or throwing server-mismatch errors.
func sharePersonalVirtualServerID(userID int64, routePrefix string) string {
	mac := hmac.New(sha256.New, jwtSecret)
	mac.Write([]byte("yundu-share-server-id:v1:"))
	mac.Write([]byte(strconv.FormatInt(userID, 10)))
	mac.Write([]byte(":"))
	mac.Write([]byte(strings.ToLower(strings.TrimSpace(routePrefix))))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

func (rt *shareUserRuntime) personalVirtualServerID(userID int64) string {
	if rt == nil || userID <= 0 {
		return ""
	}
	rt.mu.RLock()
	state := rt.users[userID]
	var prefix string
	if state != nil {
		prefix = state.routePrefix
	}
	rt.mu.RUnlock()
	if prefix == "" {
		return ""
	}
	return sharePersonalVirtualServerID(userID, prefix)
}

func rewriteEmbyAuthHeaderAttribute(value, attrName, oldVal, newVal string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || attrName == "" || oldVal == "" || newVal == "" || strings.EqualFold(oldVal, newVal) {
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
	var out strings.Builder
	out.Grow(len(value) + len(newVal))
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
		if strings.EqualFold(key, attrName) && strings.EqualFold(rawVal, oldVal) {
			out.WriteString(trimmed[pos:valStart])
			if quoted {
				out.WriteString(strings.ReplaceAll(newVal, `"`, `\"`))
			} else {
				out.WriteString(newVal)
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

// rewriteEmbyRequestUserID transparently maps a client's cached UserId (from
// the site it originally signed in to) to the session's current upstream UserId
// across URL path segments, query parameters, and Emby Authorization headers.
func rewriteEmbyRequestUserID(r *http.Request, clientUID, upstreamUID string) {
	if r == nil || clientUID == "" || upstreamUID == "" || strings.EqualFold(clientUID, upstreamUID) {
		return
	}
	if r.URL != nil && r.URL.Path != "" {
		segments := strings.Split(r.URL.Path, "/")
		pathChanged := false
		for i, seg := range segments {
			if strings.EqualFold(seg, clientUID) {
				segments[i] = upstreamUID
				pathChanged = true
			}
		}
		if pathChanged {
			r.URL.Path = strings.Join(segments, "/")
			r.URL.RawPath = ""
		}
	}
	if r.URL != nil && r.URL.RawQuery != "" {
		q := r.URL.Query()
		queryChanged := false
		for key, values := range q {
			switch strings.ToLower(key) {
			case "userid", "user_id":
				for idx, v := range values {
					if strings.EqualFold(strings.TrimSpace(v), clientUID) {
						values[idx] = upstreamUID
						queryChanged = true
					}
				}
			}
		}
		if queryChanged {
			r.URL.RawQuery = q.Encode()
		}
	}
	for _, hdr := range []string{"X-Emby-Authorization", "Authorization"} {
		if vals := r.Header.Values(hdr); len(vals) > 0 {
			for idx, v := range vals {
				vals[idx] = rewriteEmbyAuthHeaderAttribute(v, "UserId", clientUID, upstreamUID)
			}
		}
	}
}

func (rt *shareUserRuntime) applyUpstreamUserID(r *http.Request, tokenHash string) {
	if rt == nil || r == nil || tokenHash == "" {
		return
	}
	rt.mu.RLock()
	entry, ok := rt.sessions[tokenHash]
	rt.mu.RUnlock()
	if !ok || entry.clientUID == "" || entry.upstreamUID == "" || strings.EqualFold(entry.clientUID, entry.upstreamUID) {
		return
	}
	rewriteEmbyRequestUserID(r, entry.clientUID, entry.upstreamUID)
}

func (rt *shareUserRuntime) sessionUIDPair(tokenHash string) (clientUID, upstreamUID string) {
	if rt == nil || tokenHash == "" {
		return "", ""
	}
	rt.mu.RLock()
	entry, ok := rt.sessions[tokenHash]
	rt.mu.RUnlock()
	if !ok {
		return "", ""
	}
	return entry.clientUID, entry.upstreamUID
}

func isEmbySystemInfoPath(pathValue string) bool {
	p := strings.ToLower(strings.TrimRight(pathValue, "/"))
	p = strings.TrimPrefix(p, "/emby")
	p = strings.TrimPrefix(p, "/jellyfin")
	return p == "/system/info" || p == "/system/info/public"
}

func isEmbyUserIdentityPath(pathValue string) bool {
	p := strings.ToLower(strings.TrimRight(pathValue, "/"))
	p = strings.TrimPrefix(p, "/emby")
	p = strings.TrimPrefix(p, "/jellyfin")
	if p == "/users/me" || p == "/sessions" {
		return true
	}
	if strings.HasPrefix(p, "/users/") {
		rest := strings.TrimPrefix(p, "/users/")
		return rest != "" && !strings.Contains(rest, "/")
	}
	return false
}

func rewriteShareLoginResponseServerID(body []byte, virtualServerID string) []byte {
	if len(body) == 0 || virtualServerID == "" {
		return body
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	changed := false
	if _, ok := payload["ServerId"]; ok {
		payload["ServerId"] = virtualServerID
		changed = true
	}
	if userObj, ok := payload["User"].(map[string]any); ok {
		if _, has := userObj["ServerId"]; has {
			userObj["ServerId"] = virtualServerID
			changed = true
		}
	}
	if sessObj, ok := payload["SessionInfo"].(map[string]any); ok {
		if _, has := sessObj["ServerId"]; has {
			sessObj["ServerId"] = virtualServerID
			changed = true
		}
	}
	if !changed {
		return body
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

// rewritePersonalShareMetadataResponse normalizes ServerId (on /System/Info*)
// and maps upstream UserId back to the client's original UserId (on user/session
// identity responses after a seamless cross-site handoff).
func rewritePersonalShareMetadataResponse(resp *http.Response, upstreamBasePath, virtualServerID, clientUID, upstreamUID string) error {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil || resp.StatusCode != http.StatusOK || resp.Body == nil {
		return nil
	}
	relPath := stripUpstreamBasePath(resp.Request.URL.Path, upstreamBasePath)
	wantServerID := virtualServerID != "" && isEmbySystemInfoPath(relPath)
	wantUIDMap := clientUID != "" && upstreamUID != "" && !strings.EqualFold(clientUID, upstreamUID) && isEmbyUserIdentityPath(relPath)
	if !wantServerID && !wantUIDMap {
		return nil
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(ct, "json") {
		return nil
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, shareLoginResponseLimit))
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	decoded := raw
	wasGzip := strings.EqualFold(strings.TrimSpace(resp.Header.Get("Content-Encoding")), "gzip")
	if wasGzip {
		gr, gerr := gzip.NewReader(bytes.NewReader(raw))
		if gerr != nil {
			resp.Body = io.NopCloser(bytes.NewReader(raw))
			return nil
		}
		unpacked, rerr := io.ReadAll(io.LimitReader(gr, shareLoginResponseLimit))
		_ = gr.Close()
		if rerr != nil {
			resp.Body = io.NopCloser(bytes.NewReader(raw))
			return nil
		}
		decoded = unpacked
	}

	var parsed any
	if err := json.Unmarshal(decoded, &parsed); err != nil {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		return nil
	}

	changed := false
	if obj, ok := parsed.(map[string]any); ok {
		if wantServerID {
			if _, has := obj["Id"]; has {
				obj["Id"] = virtualServerID
				changed = true
			}
			if _, has := obj["ServerId"]; has {
				obj["ServerId"] = virtualServerID
				changed = true
			}
		}
		if wantUIDMap {
			if idStr, ok := obj["Id"].(string); ok && strings.EqualFold(idStr, upstreamUID) {
				obj["Id"] = clientUID
				changed = true
			}
			if uidStr, ok := obj["UserId"].(string); ok && strings.EqualFold(uidStr, upstreamUID) {
				obj["UserId"] = clientUID
				changed = true
			}
			if virtualServerID != "" {
				if _, has := obj["ServerId"]; has {
					obj["ServerId"] = virtualServerID
					changed = true
				}
			}
		}
	} else if arr, ok := parsed.([]any); ok && wantUIDMap {
		for _, elem := range arr {
			m, ok := elem.(map[string]any)
			if !ok {
				continue
			}
			if uidStr, ok := m["UserId"].(string); ok && strings.EqualFold(uidStr, upstreamUID) {
				m["UserId"] = clientUID
				changed = true
			}
			if virtualServerID != "" {
				if _, has := m["ServerId"]; has {
					m["ServerId"] = virtualServerID
					changed = true
				}
			}
		}
	}

	if !changed {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		return nil
	}
	encoded, err := json.Marshal(parsed)
	if err != nil {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		return nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(encoded))
	resp.ContentLength = int64(len(encoded))
	resp.Header.Del("Content-Encoding")
	resp.Header.Set("Content-Length", strconv.Itoa(len(encoded)))
	return nil
}

// handoffPersonalRouteSite transparently migrates an active personal-route
// share session from its old site to targetSiteID by silently signing in to
// targetSiteID's upstream Emby account and persisting the new upstream token
// and UserId while keeping the client's original token and UserId unchanged.
func (rt *shareUserRuntime) handoffPersonalRouteSite(
	ctx context.Context,
	tokenHash string,
	targetSiteID int64,
	clientIP string,
) (int, string) {
	if rt == nil || rt.pm == nil || tokenHash == "" || targetSiteID <= 0 {
		return http.StatusUnauthorized, "服务器已切换，请重新登录"
	}

	lock := rt.sessionHandoffLock(tokenHash)
	lock.Lock()
	defer lock.Unlock()

	rt.mu.RLock()
	entry, ok := rt.sessions[tokenHash]
	_, isRevoked := rt.revoked[tokenHash]
	if !ok || isRevoked {
		rt.mu.RUnlock()
		return http.StatusUnauthorized, "登录已失效，请重新登录"
	}
	if entry.siteID == targetSiteID {
		rt.mu.RUnlock()
		return 0, ""
	}
	state := rt.users[entry.userID]
	blockReason := state.blockReasonLocked(time.Now())
	var preferredAccount int64
	var targetAssigned bool
	var oldAssigned bool
	if state != nil {
		preferredAccount, targetAssigned = state.sites[targetSiteID]
		_, oldAssigned = state.sites[entry.siteID]
	}
	oldSiteID := entry.siteID
	rt.mu.RUnlock()

	if blockReason != "" {
		_ = rt.revokeSession(tokenHash, &entry)
		return http.StatusUnauthorized, shareBlockMessage(blockReason)
	}
	if !targetAssigned {
		if oldAssigned {
			return http.StatusServiceUnavailable, "目标服务器授权已变更，已保留当前服务器会话"
		}
		_ = rt.revokeSession(tokenHash, &entry)
		return http.StatusUnauthorized, "分享账号无权访问目标服务器，请重新登录"
	}

	rt.pm.mu.RLock()
	targetInst := rt.pm.proxies[targetSiteID]
	rt.pm.mu.RUnlock()
	if targetInst == nil || targetInst.shareLogin == nil {
		return http.StatusServiceUnavailable, "目标服务器暂不可用，已保留当前会话"
	}

	candidates, err := rt.upstreamCredentialCandidates(targetSiteID, preferredAccount)
	if err != nil || len(candidates) == 0 {
		log.Printf("[%s] seamless site switch for user #%d: no upstream credentials: %v", targetInst.Site.Name, entry.userID, err)
		return http.StatusServiceUnavailable, "目标服务器暂未配置可用源站账号，已保留当前会话，请稍后重试"
	}

	var targetEgressNodeID int64
	var targetEgressDecision edgeRouteDecision
	if targetInst.Site.EdgeRoutingMode == edgeRoutingModeEdgeFull {
		decision, lockedNodeID, edgeErr := resolveEdgeFullDecision(rt.pm, targetInst.Site, clientIP, 0, true)
		if edgeErr != nil {
			return http.StatusServiceUnavailable, "目标服务器绑定的从机离线或不可用，请稍后重试"
		}
		targetEgressNodeID = lockedNodeID
		targetEgressDecision = decision
	}

	var newToken, newUID string
	var chosenAccountID int64
	var lastStatus int
	var lastErr error
	for _, cand := range candidates {
		loginCtx, cancel := context.WithTimeout(ctx, shareHandoffLoginTimeout)
		if targetEgressDecision.NodeID > 0 {
			loginCtx = withEdgeEgressTarget(loginCtx, edgeEgressTarget{
				NodeID:    targetEgressDecision.NodeID,
				NodeName:  targetEgressDecision.NodeName,
				PublicURL: targetEgressDecision.PublicURL,
				Token:     targetEgressDecision.Token,
			})
		}
		tok, uid, status, loginErr := targetInst.shareLogin(
			loginCtx, cand.username, cand.password, entry.deviceID, entry.deviceName, entry.clientName,
		)
		cancel()
		if loginErr == nil && status == http.StatusOK && tok != "" {
			newToken = tok
			newUID = uid
			chosenAccountID = cand.accountID
			break
		}
		lastStatus = status
		lastErr = loginErr
		if status != http.StatusUnauthorized && status != http.StatusForbidden && status != http.StatusTooManyRequests {
			break
		}
	}
	if newToken == "" {
		log.Printf("[%s] seamless site switch login failed (status=%d): %v", targetInst.Site.Name, lastStatus, lastErr)
		return http.StatusServiceUnavailable, "切换到新服务器失败，已保留当前会话，请稍后重试"
	}

	committed := false
	defer func() {
		if !committed {
			logoutUncommittedHandoffToken(ctx, targetInst, newToken, targetEgressDecision)
		}
	}()

	upCipher, encErr := encryptSiteAccountSecret(newToken)
	if encErr != nil {
		log.Printf("[%s] seamless site switch encrypt token: %v", targetInst.Site.Name, encErr)
		return http.StatusServiceUnavailable, "切换到新服务器失败，已保留当前会话，请稍后重试"
	}

	next := entry
	next.siteID = targetSiteID
	next.siteAccountID = chosenAccountID
	next.egressNodeID = targetEgressNodeID
	next.upstreamToken = newToken
	next.upstreamUID = newUID
	if next.clientUID == "" {
		next.clientUID = entry.upstreamUID
	}
	if clientIP != "" {
		next.clientIP = clientIP
	}
	if rt.pm.edges != nil && next.clientIP != "" {
		next.ispGroup = rt.pm.edges.lookupISPGroup(next.clientIP)
	}

	status, message := rt.commitPersonalRouteSiteHandoff(ctx, tokenHash, entry, next, targetInst, preferredAccount, upCipher)
	if status != 0 {
		if status == http.StatusUnauthorized {
			_ = rt.revokeSession(tokenHash, &entry)
		}
		return status, message
	}
	committed = true
	if entry.upstreamToken != "" {
		rt.logoutUpstream(oldSiteID, entry.upstreamToken, entry.egressNodeID)
	}

	log.Printf("[%s] seamless personal-route site switch: user #%d site #%d -> #%d (client_uid=%s -> upstream_uid=%s)",
		targetInst.Site.Name, entry.userID, oldSiteID, targetSiteID, next.clientUID, next.upstreamUID)
	return 0, ""
}

// Use the same instance and edge egress that minted the token. The site's
// runtime may have been stopped or replaced during the login request.
func logoutUncommittedHandoffToken(ctx context.Context, inst *ProxyInstance, token string, egress edgeRouteDecision) {
	if inst.shareLogout == nil {
		return
	}
	go func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shareLogoutTimeout)
		defer cancel()
		if egress.NodeID > 0 {
			cleanupCtx = withEdgeEgressTarget(cleanupCtx, edgeEgressTarget{
				NodeID:    egress.NodeID,
				NodeName:  egress.NodeName,
				PublicURL: egress.PublicURL,
				Token:     egress.Token,
			})
		}
		if err := inst.shareLogout(cleanupCtx, token); err != nil {
			log.Printf("[%s] uncommitted handoff token logout: %v", inst.Site.Name, err)
		}
	}()
}

func (s shareSessionEntry) sameHandoffSource(prior shareSessionEntry) bool {
	return s.loginGeneration == prior.loginGeneration && s.userID == prior.userID && s.siteID == prior.siteID &&
		s.upstreamToken == prior.upstreamToken && s.egressNodeID == prior.egressNodeID
}

func (rt *shareUserRuntime) commitPersonalRouteSiteHandoff(ctx context.Context, tokenHash string, prior, next shareSessionEntry, targetInst *ProxyInstance, preferredAccount int64, upCipher string) (int, string) {
	rt.sessionStoreMu.Lock()
	defer rt.sessionStoreMu.Unlock()

	rt.mu.RLock()
	cur, exists := rt.sessions[tokenHash]
	_, revoked := rt.revoked[tokenHash]
	state := rt.users[prior.userID]
	block := state.blockReasonLocked(time.Now())
	var pendingUsage int64
	if state != nil {
		pendingUsage = state.pending.Load()
	}
	rt.mu.RUnlock()

	if !exists || revoked {
		return http.StatusUnauthorized, "登录已失效，请重新登录"
	}
	if !cur.sameHandoffSource(prior) {
		return http.StatusServiceUnavailable, "会话状态已变更，请重试"
	}
	if block != "" {
		return http.StatusUnauthorized, shareBlockMessage(block)
		}
	rt.pm.mu.RLock()
	sameTarget := rt.pm.proxies[next.siteID] == targetInst
	rt.pm.mu.RUnlock()
	if !sameTarget {
		return http.StatusServiceUnavailable, "目标服务器状态已变更，已保留当前会话"
	}
	status, message, err := rt.persistPersonalRouteSiteHandoff(ctx, tokenHash, prior, next, preferredAccount, upCipher, pendingUsage)
	if err != nil {
		log.Printf("share session #%d seamless site switch persist to site #%d: %v", prior.userID, next.siteID, err)
		return http.StatusServiceUnavailable, "保存新服务器会话失败，已保留当前会话，请稍后重试"
	}
	if status != 0 {
		return status, message
	}

	now := time.Now()
	rt.mu.Lock()
	next.lastHandoffAt = now
	next.lastSiteSwitchAt = now
	rt.sessions[tokenHash] = next
		if act := rt.activity[tokenHash]; act != nil {
			act.draining = false
			act.drainingSince = time.Time{}
			act.playing = false
			act.lastPlaybackAt = time.Time{}
			act.hasNode = false
			act.lastNodeID = 0
			act.lastNodeName = ""
		}
		delete(rt.dirtyClientIP, tokenHash)
		rt.lastSeen[tokenHash] = now
	rt.mu.Unlock()
	return 0, ""
	}

// Re-read authorization in the same transaction as the session update. An
// administrator's changes may be committed before their runtime reload runs.
func (rt *shareUserRuntime) persistPersonalRouteSiteHandoff(ctx context.Context, tokenHash string, prior, next shareSessionEntry, preferredAccount int64, upCipher string, pendingUsage int64) (int, string, error) {
	tx, err := rt.db.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback()

	var enabled, oldAssigned, targetEnabled bool
	var expires string
	var quota, used, currentPreferredAccount int64
	err = tx.QueryRowContext(ctx, `
		SELECT u.enabled, u.expires_at, u.traffic_quota, u.traffic_used,
		       EXISTS (SELECT 1 FROM share_user_sites WHERE share_user_id=u.id AND site_id=?),
		       COALESCE((SELECT site_account_id FROM share_user_sites WHERE share_user_id=u.id AND site_id=?), -1),
		       EXISTS (SELECT 1 FROM sites WHERE id=? AND enabled=1)
		FROM share_users u JOIN share_user_sessions s ON s.share_user_id=u.id
		WHERE u.id=? AND s.token_hash=? AND s.site_id=?
		  AND NOT EXISTS (SELECT 1 FROM share_revoked_tokens WHERE token_hash=s.token_hash)`,
		prior.siteID, next.siteID, next.siteID, prior.userID, tokenHash, prior.siteID,
	).Scan(&enabled, &expires, &quota, &used, &oldAssigned, &currentPreferredAccount, &targetEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return http.StatusUnauthorized, "登录已失效，请重新登录", nil
	}
	if err != nil {
		return 0, "", err
	}
	if !enabled {
		return http.StatusUnauthorized, shareBlockMessage("disabled"), nil
	}
	if expiry := rt.db.shareExpiryTime(expires); !expiry.IsZero() && !time.Now().Before(expiry) {
		return http.StatusUnauthorized, shareBlockMessage("expired"), nil
	}
	if quota > 0 && used+pendingUsage >= quota {
		return http.StatusUnauthorized, shareBlockMessage("quota"), nil
	}
	if currentPreferredAccount < 0 && !oldAssigned {
		return http.StatusUnauthorized, "分享账号无权访问目标服务器，请重新登录", nil
	}
	if currentPreferredAccount != preferredAccount || !targetEnabled {
		return http.StatusServiceUnavailable, "目标服务器授权或状态已变更，已保留当前会话", nil
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE share_user_sessions SET site_id=?, site_account_id=?, egress_node_id=?, upstream_token_ciphertext=?, upstream_user_id=?, client_user_id=CASE WHEN client_user_id='' THEN ? ELSE client_user_id END, client_ip=?, last_seen_at=CURRENT_TIMESTAMP WHERE token_hash=? AND share_user_id=? AND site_id=?`,
		next.siteID, next.siteAccountID, next.egressNodeID, upCipher, next.upstreamUID, next.clientUID, next.clientIP, tokenHash, prior.userID, prior.siteID,
	)
	if err != nil {
		return 0, "", err
	}
	if affected, err := res.RowsAffected(); err != nil {
		return 0, "", err
	} else if affected != 1 {
		return http.StatusUnauthorized, "登录已失效，请重新登录", nil
	}
	return 0, "", tx.Commit()
}

// activeStreamCounts returns the number of currently playing devices for userID,
// deduplicated by DeviceId (or tokenHash when DeviceId is absent), optionally
// excluding a specific session (by its device key) so a device's own parallel
// Range/HLS connections never block itself.
func (rt *shareUserRuntime) activeStreamCountForUser(userID int64, excludeTokenHash string, now time.Time) int {
	if rt == nil || userID <= 0 {
		return 0
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.activeStreamCountForUserLocked(userID, excludeTokenHash, now)
}

func shareSessionDeviceKey(tokenHash, deviceID string) string {
	if strings.TrimSpace(deviceID) != "" {
		return "d:" + strings.TrimSpace(deviceID)
	}
	return "t:" + tokenHash
}

func (rt *shareUserRuntime) activeStreamCountForUserLocked(userID int64, excludeTokenHash string, now time.Time) int {
	var excludeKey string
	if excludeTokenHash != "" {
		if exEntry, ok := rt.sessions[excludeTokenHash]; ok {
			excludeKey = shareSessionDeviceKey(excludeTokenHash, exEntry.deviceID)
		} else {
			excludeKey = "t:" + excludeTokenHash
		}
	}
	seen := make(map[string]struct{})
	for hash, entry := range rt.sessions {
		if entry.userID != userID {
			continue
		}
		act := rt.activity[hash]
		if act == nil || !act.isWatching(now) {
			continue
		}
		dk := shareSessionDeviceKey(hash, entry.deviceID)
		if excludeKey != "" && dk == excludeKey {
			continue
		}
		seen[dk] = struct{}{}
	}
	return len(seen)
}

func (rt *shareUserRuntime) checkConcurrentStreamLimit(userID int64, tokenHash string, now time.Time) (blocked bool, limit int) {
	if rt == nil || userID <= 0 {
		return false, 0
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	state := rt.users[userID]
	if state == nil || state.maxConcurrentStreams <= 0 {
		return false, 0
	}
	otherPlaying := rt.activeStreamCountForUserLocked(userID, tokenHash, now)
	if otherPlaying >= state.maxConcurrentStreams {
		return true, state.maxConcurrentStreams
	}
	return false, state.maxConcurrentStreams
}

func shareConcurrentStreamLimitMessage(limit int) string {
	return fmt.Sprintf("同时播放路数已达上限（最多 %d 路），请先停止其他设备的播放后再试", limit)
}

const shareSiteSwitchGraceWindow = 10 * time.Minute

func (rt *shareUserRuntime) isRecentSiteSwitchSession(tokenHash string, now time.Time) bool {
	if rt == nil || tokenHash == "" {
		return false
	}
	rt.mu.RLock()
	entry, ok := rt.sessions[tokenHash]
	rt.mu.RUnlock()
	if !ok || entry.lastSiteSwitchAt.IsZero() {
		return false
	}
	return now.Sub(entry.lastSiteSwitchAt) <= shareSiteSwitchGraceWindow
}

func normalizeEmbyRelativePath(pathValue string) string {
	p := strings.ToLower(strings.TrimRight(pathValue, "/"))
	p = strings.TrimPrefix(p, "/emby")
	p = strings.TrimPrefix(p, "/jellyfin")
	return p
}

func hasStaleEmbyIDQueryParam(rawQuery string) bool {
	if rawQuery == "" {
		return false
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return false
	}
	for key, values := range q {
		switch strings.ToLower(key) {
		case "parentid", "ancestorids", "ids", "seriesid", "seasonid",
			"albumids", "artistids", "personids", "studioids", "genreids", "excludeitemids":
			for _, v := range values {
				if strings.TrimSpace(v) != "" {
					return true
				}
			}
		}
	}
	return false
}

type staleEmbyFallbackKind uint8

const (
	staleEmbyFallbackNone staleEmbyFallbackKind = iota
	staleEmbyFallbackItemsObject
	staleEmbyFallbackJSONArray
	staleEmbyFallbackNotFound
)

func classifyPostSiteSwitchStaleIDFallback(method, relPath, rawQuery string) staleEmbyFallbackKind {
	p := normalizeEmbyRelativePath(relPath)
	if p == "" {
		return staleEmbyFallbackNone
	}
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	isRead := method == http.MethodGet || method == http.MethodHead

	// /users/{uid}/...
	if len(segs) >= 3 && segs[0] == "users" && segs[1] != "" {
		rest := segs[2:]
		if isRead && len(rest) == 1 && rest[0] == "items" && hasStaleEmbyIDQueryParam(rawQuery) {
			return staleEmbyFallbackItemsObject
		}
		if isRead && len(rest) == 2 && rest[0] == "items" {
			switch rest[1] {
			case "latest":
				if hasStaleEmbyIDQueryParam(rawQuery) {
					return staleEmbyFallbackJSONArray
				}
			case "resume":
				if hasStaleEmbyIDQueryParam(rawQuery) {
					return staleEmbyFallbackItemsObject
				}
			case "counts", "filters", "filters2", "suggestions":
				return staleEmbyFallbackNone
			default:
				// /users/{uid}/items/{itemId}
				return staleEmbyFallbackNotFound
			}
		}
		if isRead && len(rest) == 3 && rest[0] == "items" && rest[1] != "" {
			switch rest[2] {
			case "localtrailers", "specialfeatures":
				return staleEmbyFallbackJSONArray
			case "intros", "additionalparts", "similar":
				return staleEmbyFallbackItemsObject
			}
		}
	}

	// /items, /items/{itemId}, /items/{itemId}/...
	if segs[0] == "items" {
		if isRead && len(segs) == 1 && hasStaleEmbyIDQueryParam(rawQuery) {
			return staleEmbyFallbackItemsObject
		}
		if len(segs) == 2 && isRead {
			switch segs[1] {
			case "counts", "filters", "filters2", "latest", "resume", "suggestions":
				return staleEmbyFallbackNone
			default:
				return staleEmbyFallbackNotFound
			}
		}
		if len(segs) >= 3 && segs[1] != "" {
			switch segs[2] {
			case "playbackinfo":
				if method == http.MethodPost || isRead {
					return staleEmbyFallbackNotFound
				}
			case "images":
				if isRead {
					return staleEmbyFallbackNotFound
				}
			case "similar", "intros", "additionalparts":
				if isRead {
					return staleEmbyFallbackItemsObject
				}
			case "localtrailers", "specialfeatures":
				if isRead {
					return staleEmbyFallbackJSONArray
				}
			}
		}
	}

	// /shows/nextup, /shows/{id}/seasons, /shows/{id}/episodes, /shows/{id}/similar, /movies/{id}/similar
	if isRead && segs[0] == "shows" {
		if len(segs) == 2 && segs[1] == "nextup" && hasStaleEmbyIDQueryParam(rawQuery) {
			return staleEmbyFallbackItemsObject
		}
		if len(segs) == 3 && segs[1] != "" && (segs[2] == "seasons" || segs[2] == "episodes" || segs[2] == "similar") {
			return staleEmbyFallbackItemsObject
		}
	}
	if isRead && segs[0] == "movies" && len(segs) == 3 && segs[1] != "" && segs[2] == "similar" {
		return staleEmbyFallbackItemsObject
	}
	if isRead && segs[0] == "videos" && len(segs) >= 2 && segs[1] != "" {
		return staleEmbyFallbackNotFound
	}
	return staleEmbyFallbackNone
}

// sanitizePostSiteSwitchStaleIDError prevents stale cached item/library IDs
// from an old Emby server (e.g., ULID ParentIds sent to a numeric-ID Emby
// server right after a seamless site switch) from returning HTTP 500/400 and
// tripping client-side circuit breakers such as CapyPlayer's CircuitOpenException.
func sanitizePostSiteSwitchStaleIDError(resp *http.Response, upstreamBasePath string) bool {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return false
	}
	if resp.StatusCode != http.StatusInternalServerError && resp.StatusCode != http.StatusBadRequest {
		return false
	}
	relPath := stripUpstreamBasePath(resp.Request.URL.Path, upstreamBasePath)
	kind := classifyPostSiteSwitchStaleIDFallback(resp.Request.Method, relPath, resp.Request.URL.RawQuery)
	if kind == staleEmbyFallbackNone {
		return false
	}

	if resp.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}

	var (
		newStatus int
		payload   []byte
	)
	switch kind {
	case staleEmbyFallbackItemsObject:
		newStatus = http.StatusOK
		payload = []byte(`{"Items":[],"TotalRecordCount":0,"StartIndex":0}`)
	case staleEmbyFallbackJSONArray:
		newStatus = http.StatusOK
		payload = []byte(`[]`)
	case staleEmbyFallbackNotFound:
		newStatus = http.StatusNotFound
		payload = []byte(`{"error":"item not found after site switch"}`)
	default:
		return false
	}

	resp.StatusCode = newStatus
	resp.Status = fmt.Sprintf("%d %s", newStatus, http.StatusText(newStatus))
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Transfer-Encoding")
	resp.Header.Del("ETag")
	resp.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp.Header.Set("Cache-Control", "no-store")
	if resp.Request.Method == http.MethodHead {
		resp.Body = http.NoBody
		resp.ContentLength = 0
		resp.Header.Set("Content-Length", "0")
	} else {
		resp.Body = io.NopCloser(bytes.NewReader(payload))
		resp.ContentLength = int64(len(payload))
		resp.Header.Set("Content-Length", strconv.Itoa(len(payload)))
	}
	return true
}
