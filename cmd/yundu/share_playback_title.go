package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	playbackTitleLookupTimeout = 3 * time.Second
	playbackTitleRetryInterval = 30 * time.Second
	playbackTitleLookupSlots   = 8
	playbackTitleBodyLimit     = 128 << 10
)

type playbackTitleIdentity struct {
	itemID string
	playID string
}

func (id playbackTitleIdentity) matches(other playbackTitleIdentity) bool {
	return id.itemID != "" && id.itemID == other.itemID &&
		(id.playID == "" || other.playID == "" || id.playID == other.playID)
}

type playbackTitleReport struct {
	identity playbackTitleIdentity
	start    bool
	stop     bool
	at       time.Time
}

type playbackTitleReportKey struct{}

type sharePlaybackTitle struct {
	identity   playbackTitleIdentity
	title      string
	retryAfter time.Time
}

// Guarded by shareUserRuntime.mu. The previous identity only suppresses late reports.
type sharePlaybackTitleState struct {
	current   *sharePlaybackTitle
	previous  playbackTitleIdentity
	updatedAt time.Time
}

func (s *sharePlaybackTitleState) apply(report playbackTitleReport, at time.Time) bool {
	if at.Before(s.updatedAt) {
		return false
	}
	if at.Sub(s.updatedAt) > sharePlayingIdleTimeout {
		*s = sharePlaybackTitleState{}
	}

	if report.stop {
		if s.ignoresStop(report, at) {
			return false
		}
		if current := s.current; current != nil {
			s.previous = current.identity
		} else if report.identity.itemID != "" {
			s.previous = report.identity
		}
		s.current = nil
		s.updatedAt = at
		return true
	}

	if s.current != nil && s.current.identity.matches(report.identity) {
		if s.current.identity.playID == "" {
			s.current.identity.playID = report.identity.playID
		}
		s.updatedAt = at
		return true
	}
	if !report.start && s.previous.matches(report.identity) {
		return false
	}
	if s.current != nil {
		s.previous = s.current.identity
	}
	s.current = &sharePlaybackTitle{identity: report.identity}
	s.updatedAt = at
	return true
}

func (s *sharePlaybackTitleState) ignoresStop(report playbackTitleReport, at time.Time) bool {
	if at.Before(s.updatedAt) {
		return true
	}
	current := s.current
	if current == nil || at.Sub(s.updatedAt) > sharePlayingIdleTimeout {
		return false
	}
	if report.identity.itemID != "" && !current.identity.matches(report.identity) {
		return true
	}
	return report.identity.playID != "" && current.identity.playID != "" && report.identity.playID != current.identity.playID
}

func readPlaybackTitleReport(r *http.Request) (playbackTitleReport, bool) {
	report := playbackTitleReport{
		start: isSharePlaybackStartRequest(r),
		stop:  isSharePlaybackStoppedRequest(r),
	}
	if !report.start && !report.stop && !isSharePlaybackProgressRequest(r) {
		return report, false
	}

	payload, complete := readProgressRequestBody(r)
	var body struct {
		ItemID        string `json:"ItemId"`
		PlaySessionID string `json:"PlaySessionId"`
	}
	if complete && json.Unmarshal(payload, &body) == nil {
		if validPlaybackTitlePathID(body.ItemID) {
			report.identity.itemID = body.ItemID
		}
		if len(body.PlaySessionID) <= progressMaxKeyComponentBytes {
			report.identity.playID = body.PlaySessionID
		}
	}
	return report, report.stop || report.identity.itemID != ""
}

func validPlaybackTitlePathID(id string) bool {
	if id == "" || id == "." || id == ".." || len(id) > progressMaxKeyComponentBytes {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || strings.ContainsRune("-_.+=:", c)) {
			return false
		}
	}
	return true
}

func samePlaybackTitleSession(current, expected shareSessionEntry) bool {
	return current.siteID == expected.siteID && current.upstreamUID == expected.upstreamUID &&
		current.upstreamToken == expected.upstreamToken && current.loginGeneration == expected.loginGeneration &&
		current.lastSiteSwitchAt.Equal(expected.lastSiteSwitchAt) && current.lastHandoffAt.Equal(expected.lastHandoffAt)
}

// Prepare the observation after routing and token substitution, then commit it
// only once the corresponding upstream playback report succeeds.
func (rt *shareUserRuntime) preparePlaybackTitleReport(inst *ProxyInstance, tokenHash string, r *http.Request) func() {
	if tokenHash == "" || inst.fetchPlaybackTitle == nil || rt.playbackTitleSlots == nil {
		return nil
	}
	report, ok := r.Context().Value(playbackTitleReportKey{}).(playbackTitleReport)
	if !ok {
		return nil
	}
	rt.mu.RLock()
	entry, ok := rt.sessions[tokenHash]
	rt.mu.RUnlock()
	if !ok || entry.siteID != inst.Site.ID {
		return nil
	}

	background := inst.ctx
	if egress, ok := edgeEgressTargetFrom(r.Context()); ok {
		background = withEdgeEgressTarget(background, egress)
	}
	userAgent := r.UserAgent()
	return func() {
		rt.trackPlaybackTitle(background, inst, tokenHash, entry, report, userAgent, report.at)
	}
}

func (rt *shareUserRuntime) trackPlaybackTitle(ctx context.Context, inst *ProxyInstance, tokenHash string, entry shareSessionEntry, report playbackTitleReport, userAgent string, at time.Time) {
	rt.mu.Lock()
	act := rt.activity[tokenHash]
	if act == nil || !samePlaybackTitleSession(rt.sessions[tokenHash], entry) || !act.mediaTitle.apply(report, at) {
		rt.mu.Unlock()
		return
	}
	current := act.mediaTitle.current
	now := time.Now()
	if current == nil || current.title != "" || now.Before(current.retryAfter) {
		rt.mu.Unlock()
		return
	}
	select {
	case rt.playbackTitleSlots <- struct{}{}:
		current.retryAfter = now.Add(playbackTitleRetryInterval)
	default:
		rt.mu.Unlock()
		return
	}
	rt.mu.Unlock()

	go func() {
		defer func() { <-rt.playbackTitleSlots }()
		lookupCtx, cancel := context.WithTimeout(ctx, playbackTitleLookupTimeout)
		defer cancel()
		title := inst.fetchPlaybackTitle(lookupCtx, entry.upstreamUID, entry.upstreamToken, report.identity.itemID, userAgent)
		if title == "" || lookupCtx.Err() != nil {
			return
		}
		rt.mu.Lock()
		defer rt.mu.Unlock()
		if act := rt.activity[tokenHash]; act != nil && act.mediaTitle.current == current && samePlaybackTitleSession(rt.sessions[tokenHash], entry) {
			current.title = title
		}
	}()
}

func newPlaybackTitleFetchFunc(transport http.RoundTripper, target *url.URL, policy UAHeaderPolicy, headers upstreamHeaderPolicy) func(context.Context, string, string, string, string) string {
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return func(ctx context.Context, upstreamUID, token, itemID, userAgent string) string {
		if token == "" || !validPlaybackTitlePathID(upstreamUID) || !validPlaybackTitlePathID(itemID) {
			return ""
		}
		endpoint := *target
		endpoint.Path = singleJoiningSlash(target.Path, "/Users/"+upstreamUID+"/Items/"+itemID)
		endpoint.RawPath = ""
		endpoint.RawQuery = ""
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return ""
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("User-Agent", sanitizeShareHandoffUserAgent(userAgent))
		req.Header.Set("X-Emby-Token", token)
		applyUAHeaderPolicy(req.Header, policy)
		headers.apply(req.Header, &endpoint)
		// #nosec G704 -- the authority comes from the site configuration; validated identifiers only extend its path and redirects are disabled.
		resp, err := client.Do(req)
		if err != nil {
			return ""
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return ""
		}

		data, err := io.ReadAll(io.LimitReader(resp.Body, playbackTitleBodyLimit+1))
		if err != nil || len(data) > playbackTitleBodyLimit {
			return ""
		}
		var item struct {
			Type              string
			Name              string
			SeriesName        string
			ParentIndexNumber *int
			IndexNumber       *int
		}
		if json.Unmarshal(data, &item) != nil {
			return ""
		}
		title := strings.TrimSpace(item.Name)
		if strings.EqualFold(item.Type, "Episode") {
			if series := strings.TrimSpace(item.SeriesName); series != "" {
				title = series
			}
			if title != "" && item.ParentIndexNumber != nil && *item.ParentIndexNumber >= 0 && item.IndexNumber != nil && *item.IndexNumber > 0 {
				title += fmt.Sprintf(" S%02dE%02d", *item.ParentIndexNumber, *item.IndexNumber)
			}
		}
		if len(title) > 512 {
			return ""
		}
		return title
	}
}
