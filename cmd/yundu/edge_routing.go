package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

var (
	errEdgeFullNodeUnavailable    = errors.New("bound edge node is offline or unavailable")
	errEdgeAgentEgressUnsupported = errors.New("edge agent does not support full-chain egress; please upgrade agent to v1.4.8+")
)

type edgeEgressTargetContextKey struct{}

type edgeEgressTarget struct {
	NodeID    int64
	NodeName  string
	PublicURL string
	Token     string
}

func withEdgeEgressTarget(ctx context.Context, target edgeEgressTarget) context.Context {
	return context.WithValue(ctx, edgeEgressTargetContextKey{}, target)
}

func edgeEgressTargetFrom(ctx context.Context) (edgeEgressTarget, bool) {
	if ctx == nil {
		return edgeEgressTarget{}, false
	}
	v, ok := ctx.Value(edgeEgressTargetContextKey{}).(edgeEgressTarget)
	if !ok || v.NodeID <= 0 || v.PublicURL == "" || v.Token == "" {
		return edgeEgressTarget{}, false
	}
	return v, true
}

type edgeRequestContextKey struct{}

type edgeRequestContext struct {
	clientIP         string
	shareUser        *shareUserState
	shareTokenHash   string
	edgeFullResolved bool
	edgeFullDecision edgeRouteDecision
}

func withEdgeRequestContext(ctx context.Context, clientIP string, shareUser *shareUserState, shareTokenHash string, edgeFullResolved bool, edgeFullDecision edgeRouteDecision) context.Context {
	return context.WithValue(ctx, edgeRequestContextKey{}, edgeRequestContext{
		clientIP:         clientIP,
		shareUser:        shareUser,
		shareTokenHash:   shareTokenHash,
		edgeFullResolved: edgeFullResolved,
		edgeFullDecision: edgeFullDecision,
	})
}

func edgeRequestContextFrom(ctx context.Context) edgeRequestContext {
	if ctx == nil {
		return edgeRequestContext{}
	}
	v, _ := ctx.Value(edgeRequestContextKey{}).(edgeRequestContext)
	return v
}

// resolveEdgeFullDecision determines the single consistent egress node for a
// site running in edge_full mode. lockedNodeID is >0 for a specific edge node,
// or -1 when explicitly bound to master. Returns errEdgeFullNodeUnavailable
// (fail-closed) if a pinned or session-locked edge node is offline/unroutable.
func resolveEdgeFullDecision(pm *ProxyManager, site Site, clientIP string, sessionEgressNodeID int64, advanceRR bool) (edgeRouteDecision, int64, error) {
	if site.EdgeRoutingMode != edgeRoutingModeEdgeFull {
		return edgeRouteDecision{}, 0, nil
	}
	if site.EdgePinnedNodeID > 0 {
		if pm == nil || pm.edges == nil {
			return edgeRouteDecision{}, 0, errEdgeFullNodeUnavailable
		}
		decision, ok := pm.edges.nodeDecisionByID(site.EdgePinnedNodeID)
		if !ok {
			return edgeRouteDecision{}, 0, errEdgeFullNodeUnavailable
		}
		return decision, decision.NodeID, nil
	}
	if sessionEgressNodeID > 0 {
		if pm == nil || pm.edges == nil {
			return edgeRouteDecision{}, 0, errEdgeFullNodeUnavailable
		}
		decision, ok := pm.edges.nodeDecisionByID(sessionEgressNodeID)
		if !ok {
			return edgeRouteDecision{}, 0, errEdgeFullNodeUnavailable
		}
		return decision, decision.NodeID, nil
	}
	if sessionEgressNodeID < 0 {
		masterName := defaultMasterNodeName
		if pm != nil && pm.edges != nil {
			masterName = pm.edges.masterDisplayName()
		}
		return edgeRouteDecision{NodeID: 0, NodeName: masterName}, -1, nil
	}
	if pm == nil || pm.edges == nil {
		return edgeRouteDecision{NodeID: 0, NodeName: defaultMasterNodeName}, -1, nil
	}
	decision, lockedID := pm.edges.selectEgressDecision(clientIP, advanceRR)
	return decision, lockedID, nil
}

type edgeEgressTransport struct {
	base http.RoundTripper
}

func (t *edgeEgressTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	target, ok := edgeEgressTargetFrom(req.Context())
	if !ok {
		return t.base.RoundTrip(req)
	}
	egressEndpoint := strings.TrimRight(target.PublicURL, "/") + edgeEgressPath
	egressURL, err := url.Parse(egressEndpoint)
	if err != nil || (egressURL.Scheme != "http" && egressURL.Scheme != "https") || egressURL.Host == "" {
		return nil, fmt.Errorf("invalid edge egress endpoint: %w", errEdgeFullNodeUnavailable)
	}
	hostHeader := req.Host
	if hostHeader == "" && req.URL != nil {
		hostHeader = req.URL.Host
	}
	ticket := edgeEgressTicket{
		NodeID:    target.NodeID,
		TargetURL: req.URL.String(),
		Host:      hostHeader,
		ExpiresAt: time.Now().Add(edgeEgressTicketTTL).Unix(),
	}
	sealed, err := sealEdgeEgressTicket(target.Token, ticket)
	if err != nil {
		return nil, err
	}
	outReq := req.Clone(req.Context())
	outReq.URL = egressURL
	outReq.Host = egressURL.Host
	outReq.RequestURI = ""
	outReq.Header.Set(edgeEgressTicketHeader, sealed)

	resp, err := t.base.RoundTrip(outReq)
	if err != nil {
		return nil, err
	}
	if resp.Header.Get(edgeEgressMarkerHeader) != "1" && resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, errEdgeAgentEgressUnsupported
	}
	resp.Header.Del(edgeEgressMarkerHeader)
	resp.Request = req
	return resp, nil
}

// isEdgeOffloadableMediaRequest returns true for video/audio playback streams
// that should be 307-redirected to the client's optimal edge node while
// control-plane APIs, metadata, posters, subtitles and WebSockets stay on master.
func isEdgeOffloadableMediaRequest(r *http.Request) bool {
	if r == nil || r.URL == nil || hasUpgradeIntent(r) {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	p := strings.ToLower(r.URL.Path)
	if strings.Contains(p, "/images/") || strings.Contains(p, "/subtitles/") || strings.Contains(p, "/playbackinfo") {
		return false
	}
	for _, ext := range []string{".js", ".css", ".html", ".htm", ".png", ".jpg", ".jpeg", ".webp", ".gif", ".svg", ".ico", ".woff", ".woff2", ".ttf", ".json", ".vtt", ".srt", ".ass"} {
		if strings.HasSuffix(p, ext) {
			return false
		}
	}
	return isMainVideoStreamRequest(r) || isPlaybackRequest(r.URL.Path) || isPlaybackRedirectEndpoint(r.URL.Path)
}

func compactEdgeHeaders(src http.Header, extraHeaderKeys []string) map[string]string {
	keep := []string{
		"Authorization",
		"X-Emby-Authorization",
		"X-Emby-Token",
		"X-MediaBrowser-Token",
		"Cookie",
		"User-Agent",
		"Referer",
		"Origin",
		"X-Forwarded-For",
		"X-Real-IP",
		"X-Forwarded-Host",
		"X-Forwarded-Proto",
	}
	out := make(map[string]string, len(keep)+len(extraHeaderKeys))
	for _, name := range keep {
		if v := strings.TrimSpace(src.Get(name)); v != "" {
			out[name] = v
		}
	}
	for _, name := range extraHeaderKeys {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if v := strings.TrimSpace(src.Get(name)); v != "" {
			out[http.CanonicalHeaderKey(name)] = v
		}
	}
	return out
}

func sanitizeEdgeFilename(rawPath string) string {
	base := path.Base(rawPath)
	if base == "" || base == "." || base == "/" {
		return "stream"
	}
	var b strings.Builder
	b.Grow(len(base))
	for i := 0; i < len(base); i++ {
		c := base[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '_' {
			b.WriteByte(c)
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return "stream"
	}
	if len(out) > 64 {
		out = out[len(out)-64:]
	}
	return out
}

func writeEdgePlaybackRedirect(w http.ResponseWriter, r *http.Request, pm *ProxyManager, site Site, decision edgeRouteDecision, targetURL *url.URL, headers map[string]string, shareUser *shareUserState, shareTokenHash string) bool {
	if decision.NodeID <= 0 || decision.PublicURL == "" || decision.Token == "" || targetURL == nil {
		return false
	}
	speedMbps := site.SpeedLimit
	var shareUserID int64
	if shareUser != nil {
		shareUserID = shareUser.id
		if pm != nil && pm.shares != nil {
			if userSpeed := pm.shares.effectiveSpeedLimitMbps(shareUser); userSpeed > 0 {
				if speedMbps == 0 || userSpeed < speedMbps {
					speedMbps = userSpeed
				}
			}
		}
	}
	ticket := edgePlaybackTicket{
		NodeID:      decision.NodeID,
		SiteID:      site.ID,
		ShareUserID: shareUserID,
		TokenHash:   shareTokenHash,
		TargetURL:   targetURL.String(),
		Headers:     headers,
		SpeedMbps:   speedMbps,
		FollowRedir: true,
		ExpiresAt:   time.Now().Add(edgeTicketTTL).Unix(),
	}
	sealed, err := sealEdgePlaybackTicket(decision.Token, ticket)
	if err != nil {
		return false
	}
	filename := sanitizeEdgeFilename(targetURL.Path)
	redirectURL := strings.TrimRight(decision.PublicURL, "/") + edgeTicketPathPrefix + sealed + "/" + filename
	if pm != nil && pm.shares != nil && shareTokenHash != "" {
		pm.shares.markEdgePlayback(shareTokenHash, decision.NodeID, decision.NodeName)
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Yundu-Edge-Node", decision.NodeName)
	http.Redirect(w, r, redirectURL, http.StatusTemporaryRedirect) // #nosec G710 -- redirectURL is built from admin-configured edge node public URL and an authenticated AES-GCM ticket.
	return true
}

func tryRedirectToEdgeNode(
	w http.ResponseWriter,
	r *http.Request,
	pm *ProxyManager,
	site Site,
	target, playbackTarget *url.URL,
	isRedirectMode bool,
	policy UAHeaderPolicy,
	configuredHeaders upstreamHeaderPolicy,
	trustedProxies []*net.IPNet,
	clientIP string,
	shareUser *shareUserState,
	shareTokenHash string,
) bool {
	if pm == nil || pm.edges == nil || site.MainVideoStreamMode == mainVideoStreamModeDirect || site.EdgeRoutingMode == edgeRoutingModeMaster {
		return false
	}
	if !isEdgeOffloadableMediaRequest(r) {
		return false
	}
	var decision edgeRouteDecision
	if site.EdgeRoutingMode == edgeRoutingModeEdgeFull {
		edgeCtx := edgeRequestContextFrom(r.Context())
		if !edgeCtx.edgeFullResolved {
			return false
		}
		decision = edgeCtx.edgeFullDecision
	} else {
		decision = pm.edges.selectNodeForSessionDispatch(clientIP, shareTokenHash)
	}
	if decision.NodeID <= 0 {
		if decision.NodeID == 0 && pm != nil && pm.shares != nil && shareTokenHash != "" {
			pm.shares.markMasterPlayback(shareTokenHash, decision.NodeName)
		}
		return false
	}
	upstream := target
	if !isRedirectMode {
		upstream = upstreamTargetForRequest(r, target, playbackTarget)
	}
	if upstream == nil {
		return false
	}
	upstreamURL := *r.URL
	applyUpstreamURL(&upstreamURL, upstream)

	headers := r.Header.Clone()
	prepareUpstreamHeadersWithClientIPMode(headers, r, policy, site.ClientIPMode, trustedProxies)
	if !sameRedirectAuthority(target, upstream) {
		headers = crossAuthorityRedirectHeadersWithClientIPMode(headers, site.ClientIPMode)
	}
	applySiteForwardedHost(headers, r, site)
	configuredHeaders.apply(headers, upstream)

	extraKeys := make([]string, 0, len(configuredHeaders.values))
	for k := range configuredHeaders.values {
		extraKeys = append(extraKeys, k)
	}
	compact := compactEdgeHeaders(headers, extraKeys)
	return writeEdgePlaybackRedirect(w, r, pm, site, decision, &upstreamURL, compact, shareUser, shareTokenHash)
}

func tryRedirectDynamicResourceToEdgeNode(
	w http.ResponseWriter,
	r *http.Request,
	pm *ProxyManager,
	site Site,
	target *url.URL,
	outboundHeaders http.Header,
	requiredHeaders []dynamicCapabilityHeaderClaim,
) bool {
	if pm == nil || pm.edges == nil || site.MainVideoStreamMode == mainVideoStreamModeDirect || site.EdgeRoutingMode == edgeRoutingModeMaster {
		return false
	}
	if r == nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	edgeCtx := edgeRequestContextFrom(r.Context())
	var decision edgeRouteDecision
	if site.EdgeRoutingMode == edgeRoutingModeEdgeFull {
		if !edgeCtx.edgeFullResolved {
			return false
		}
		decision = edgeCtx.edgeFullDecision
	} else {
		decision = pm.edges.selectNodeForSessionDispatch(edgeCtx.clientIP, edgeCtx.shareTokenHash)
	}
	if decision.NodeID <= 0 {
		if decision.NodeID == 0 && pm != nil && pm.shares != nil && edgeCtx.shareTokenHash != "" {
			pm.shares.markMasterPlayback(edgeCtx.shareTokenHash, decision.NodeName)
		}
		return false
	}
	extraKeys := make([]string, 0, len(requiredHeaders))
	for _, h := range requiredHeaders {
		extraKeys = append(extraKeys, h.Name)
	}
	compact := compactEdgeHeaders(outboundHeaders, extraKeys)
	return writeEdgePlaybackRedirect(w, r, pm, site, decision, target, compact, edgeCtx.shareUser, edgeCtx.shareTokenHash)
}
