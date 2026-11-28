package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	edgeIPModeCompatible = "compatible"
	edgeIPModeStrict     = "strict"
	edgeIPModeSetting    = "_edge_client_ip_mode"
	edgeIPCompatHeader   = "X-Yundu-Client-IP-Compat"
	edgeIPCheckPath      = "/api/agent/client-ip-check"
	edgeIPCheckTimeout   = 2 * time.Second
)

type edgeClientIPVerdict string

const (
	edgeIPExactMatch       edgeClientIPVerdict = "exact_match"
	edgeIPSameCarrier      edgeClientIPVerdict = "same_mainland_carrier"
	edgeIPStrictMismatch   edgeClientIPVerdict = "strict_mismatch"
	edgeIPNonMainland      edgeClientIPVerdict = "non_mainland"
	edgeIPDifferentCarrier edgeClientIPVerdict = "different_carrier"
	edgeIPUnknown          edgeClientIPVerdict = "unknown_region_or_carrier"
	edgeIPCheckUnavailable edgeClientIPVerdict = "check_unavailable"
)

func (rt *edgeNodeRuntime) edgeClientIPCheckMode(nodeID int64) string {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.edgeClientIPCheckModeLocked(rt.nodes[nodeID], time.Now())
}

func (rt *edgeNodeRuntime) edgeClientIPCheckModeLocked(node *edgeNodeLiveState, now time.Time) string {
	if !rt.blockNonMainland || !rt.edgeBindClientIP {
		return ""
	}
	if rt.edgeBindClientIPMode == edgeIPModeCompatible && node != nil && node.enabled && node.isOnline(now) && node.supportsClientIPCompat {
		return edgeIPModeCompatible
	}
	return edgeIPModeStrict
}

func compareEdgeClientIPRegions(expected, actual ip2regionResult) edgeClientIPVerdict {
	if expected.NonMainlandCN || actual.NonMainlandCN {
		return edgeIPNonMainland
	}
	for _, result := range []ip2regionResult{expected, actual} {
		if !result.MainlandCN || result.ISPGroup == "" || result.ISPGroup == ispGroupOther {
			return edgeIPUnknown
		}
	}
	if expected.ISPGroup != actual.ISPGroup {
		return edgeIPDifferentCarrier
	}
	return edgeIPSameCarrier
}

type agentClientIPCheckRequest struct {
	Ticket string `json:"ticket"`
	PeerIP string `json:"peer_ip"`
}

type agentClientIPCheckResponse struct {
	Verdict edgeClientIPVerdict `json:"verdict"`
}

func (a *App) handleAgentClientIPCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.pm == nil || a.pm.edges == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "edge runtime unavailable")
		return
	}
	nodeID, err := strconv.ParseInt(r.Header.Get("X-Yundu-Node-ID"), 10, 64)
	token := r.Header.Get("X-Yundu-Node-Token")
	if err != nil || nodeID <= 0 || token == "" {
		a.jsonErr(w, http.StatusUnauthorized, "invalid agent credentials")
		return
	}

	rt := a.pm.edges
	rt.mu.RLock()
	node := rt.nodes[nodeID]
	authenticated := node != nil && node.enabled && hmac.Equal([]byte(node.tokenHash), []byte(edgeNodeTokenHash(token)))
	mode := rt.edgeClientIPCheckModeLocked(node, time.Now())
	rt.mu.RUnlock()
	if !authenticated {
		a.jsonErr(w, http.StatusUnauthorized, "invalid agent credentials")
		return
	}

	var request agentClientIPCheckRequest
	if err := decodeJSONBodyLimit(w, r, &request, edgeMaxTicketBytes*2+256); err != nil {
		a.jsonErr(w, http.StatusBadRequest, "invalid IP check payload")
		return
	}
	ticket, err := openEdgePlaybackTicket(token, request.Ticket)
	if err != nil || ticket.NodeID != nodeID || ticket.IPCheckMode != edgeIPModeCompatible || ticket.ClientIP == "" {
		a.jsonErr(w, http.StatusUnauthorized, "invalid playback ticket")
		return
	}
	verdict := edgeIPStrictMismatch
	if mode == edgeIPModeCompatible {
		verdict = compareEdgeClientIPRegions(rt.ip2region.Lookup(ticket.ClientIP), rt.ip2region.Lookup(request.PeerIP))
	}
	a.jsonOK(w, agentClientIPCheckResponse{Verdict: verdict})
}

func (a *edgeAgent) checkPlaybackClientIP(r *http.Request, sealed string, ticket edgePlaybackTicket) edgeClientIPVerdict {
	expectedIP := normalizeRawIPString(ticket.ClientIP)
	if ticket.ClientIP == "" {
		return edgeIPExactMatch
	}
	peerIP := a.clientPeerIP(r)
	if expectedIP != "" && expectedIP == peerIP {
		return edgeIPExactMatch
	}
	if ticket.IPCheckMode != edgeIPModeCompatible {
		return edgeIPStrictMismatch
	}
	if expectedIP == "" || peerIP == "" {
		return edgeIPUnknown
	}
	return a.askMasterClientIPCheck(r.Context(), sealed, peerIP)
}

func (a *edgeAgent) askMasterClientIPCheck(ctx context.Context, sealed, peerIP string) edgeClientIPVerdict {
	ctx, cancel := context.WithTimeout(ctx, edgeIPCheckTimeout)
	defer cancel()
	payload, err := json.Marshal(agentClientIPCheckRequest{Ticket: sealed, PeerIP: peerIP})
	if err != nil {
		return edgeIPCheckUnavailable
	}
	endpoint := strings.TrimRight(a.cfg.MasterURL, "/") + edgeIPCheckPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return edgeIPCheckUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Yundu-Node-ID", strconv.FormatInt(a.cfg.NodeID, 10))
	req.Header.Set("X-Yundu-Node-Token", a.cfg.Token)
	req.Header.Set("User-Agent", "Yundu-Agent/"+appVersion)
	client := *a.masterClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return edgeIPCheckUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return edgeIPCheckUnavailable
	}
	var result agentClientIPCheckResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024)).Decode(&result); err == nil {
		switch result.Verdict {
		case edgeIPSameCarrier, edgeIPStrictMismatch, edgeIPNonMainland, edgeIPDifferentCarrier, edgeIPUnknown:
			return result.Verdict
		}
	}
	return edgeIPCheckUnavailable
}

func edgeClientIPRejectionMessage(verdict edgeClientIPVerdict) string {
	switch verdict {
	case edgeIPNonMainland:
		return "主控与从机的客户端 IP 不同，且存在非中国大陆来源"
	case edgeIPDifferentCarrier:
		return "主控与从机的客户端 IP 不同，且运营商不一致"
	case edgeIPUnknown:
		return "主控与从机的客户端 IP 不同，且无法确认两边属于同一大陆运营商"
	case edgeIPCheckUnavailable:
		return "主控与从机的客户端 IP 不同，暂时无法确认运营商"
	default:
		return "主控签发播放链接时的客户端 IP 与从机看到的 IP 不一致"
	}
}
