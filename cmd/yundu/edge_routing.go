package main

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

type edgeRequestContextKey struct{}

type edgeRequestContext struct {
	clientIP       string
	shareUser      *shareUserState
	shareTokenHash string
}

func withEdgeRequestContext(ctx context.Context, clientIP string, shareUser *shareUserState, shareTokenHash string) context.Context {
	return context.WithValue(ctx, edgeRequestContextKey{}, edgeRequestContext{
		clientIP:       clientIP,
		shareUser:      shareUser,
		shareTokenHash: shareTokenHash,
	})
}

func edgeRequestContextFrom(ctx context.Context) edgeRequestContext {
	if ctx == nil {
		return edgeRequestContext{}
	}
	v, _ := ctx.Value(edgeRequestContextKey{}).(edgeRequestContext)
	return v
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
		pm.shares.markEdgePlayback(shareTokenHash)
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
	if pm == nil || pm.edges == nil || site.MainVideoStreamMode == mainVideoStreamModeDirect {
		return false
	}
	if !isEdgeOffloadableMediaRequest(r) {
		return false
	}
	decision := pm.edges.selectNodeForIP(clientIP)
	if decision.NodeID <= 0 {
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
	if pm == nil || pm.edges == nil || site.MainVideoStreamMode == mainVideoStreamModeDirect {
		return false
	}
	if r == nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	edgeCtx := edgeRequestContextFrom(r.Context())
	decision := pm.edges.selectNodeForIP(edgeCtx.clientIP)
	if decision.NodeID <= 0 {
		return false
	}
	extraKeys := make([]string, 0, len(requiredHeaders))
	for _, h := range requiredHeaders {
		extraKeys = append(extraKeys, h.Name)
	}
	compact := compactEdgeHeaders(outboundHeaders, extraKeys)
	return writeEdgePlaybackRedirect(w, r, pm, site, decision, target, compact, edgeCtx.shareUser, edgeCtx.shareTokenHash)
}
