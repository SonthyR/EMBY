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
	embyTicksPerSecond         = 10_000_000
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
	identity        playbackTitleIdentity
	start           bool
	stop            bool
	at              time.Time
	positionSeconds *int64
	paused          *bool
}

type playbackTitleReportKey struct{}

type sharePlaybackTitle struct {
	identity        playbackTitleIdentity
	metadata        playbackItemMetadata
	positionSeconds *int64
	paused          *bool
	retryAfter      time.Time
}

type playbackItemMetadata struct {
	title           string
	durationSeconds int64
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
		if report.positionSeconds != nil {
			s.current.positionSeconds = report.positionSeconds
		}
		if report.paused != nil {
			s.current.paused = report.paused
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
	s.current = &sharePlaybackTitle{identity: report.identity, positionSeconds: report.positionSeconds, paused: report.paused}
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
		PositionTicks *int64 `json:"PositionTicks"`
		IsPaused      *bool  `json:"IsPaused"`
		EventName     string `json:"EventName"`
	}
	if !complete || json.Unmarshal(payload, &body) != nil {
		return report, report.stop
	}
	if validPlaybackTitlePathID(body.ItemID) {
		report.identity.itemID = body.ItemID
	}
	if len(body.PlaySessionID) <= progressMaxKeyComponentBytes {
		report.identity.playID = body.PlaySessionID
	}
	if body.PositionTicks != nil && *body.PositionTicks >= 0 {
		seconds := *body.PositionTicks / embyTicksPerSecond
		report.positionSeconds = &seconds
	}
	report.paused = body.IsPaused
	if report.paused == nil {
		switch strings.ToLower(body.EventName) {
		case "pause", "unpause":
			paused := strings.EqualFold(body.EventName, "pause")
			report.paused = &paused
		}
	}
	return report, true
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
	return current.userID == expected.userID && current.siteID == expected.siteID && current.upstreamUID == expected.upstreamUID &&
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
	if act == nil || !samePlaybackTitleSession(rt.sessions[tokenHash], entry) || at.Before(act.mediaTitle.updatedAt) {
		rt.mu.Unlock()
		return
	}
	if report.identity.itemID == "" && !report.stop {
		current := act.mediaTitle.current
		if current != nil && at.Sub(act.mediaTitle.updatedAt) <= sharePlayingIdleTimeout &&
			(report.identity.playID == "" || report.identity.playID == current.identity.playID) {
			report.identity.itemID = current.identity.itemID
		}
	}
	if (report.identity.itemID != "" || report.stop) && !act.mediaTitle.apply(report, at) {
		rt.mu.Unlock()
		return
	}
	current := act.mediaTitle.current
	paused := report.paused
	if paused == nil && current != nil && current.identity.matches(report.identity) {
		paused = current.paused
	}
	if !report.stop && (paused == nil || !*paused) {
		if user := rt.users[entry.userID]; user != nil {
			user.lastWatchedMS = max(user.lastWatchedMS, at.UnixMilli())
		}
	}
	now := time.Now()
	if current == nil || !current.identity.matches(report.identity) || current.metadata != (playbackItemMetadata{}) || now.Before(current.retryAfter) {
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
		metadata := inst.fetchPlaybackTitle(lookupCtx, entry.upstreamUID, entry.upstreamToken, report.identity.itemID, userAgent)
		if metadata == (playbackItemMetadata{}) || lookupCtx.Err() != nil {
			return
		}
		rt.mu.Lock()
		defer rt.mu.Unlock()
		if act := rt.activity[tokenHash]; act != nil && act.mediaTitle.current == current && samePlaybackTitleSession(rt.sessions[tokenHash], entry) {
			current.metadata = metadata
		}
	}()
}

func newPlaybackTitleFetchFunc(transport http.RoundTripper, target *url.URL, policy UAHeaderPolicy, headers upstreamHeaderPolicy) func(context.Context, string, string, string, string) playbackItemMetadata {
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return func(ctx context.Context, upstreamUID, token, itemID, userAgent string) playbackItemMetadata {
		if token == "" || !validPlaybackTitlePathID(upstreamUID) || !validPlaybackTitlePathID(itemID) {
			return playbackItemMetadata{}
		}
		endpoint := *target
		endpoint.Path = singleJoiningSlash(target.Path, "/Users/"+upstreamUID+"/Items/"+itemID)
		endpoint.RawPath = ""
		endpoint.RawQuery = ""
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return playbackItemMetadata{}
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
			return playbackItemMetadata{}
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return playbackItemMetadata{}
		}

		data, err := io.ReadAll(io.LimitReader(resp.Body, playbackTitleBodyLimit+1))
		if err != nil || len(data) > playbackTitleBodyLimit {
			return playbackItemMetadata{}
		}
		var item struct {
			Type              string
			Name              string
			SeriesName        string
			ParentIndexNumber *int
			IndexNumber       *int
			RunTimeTicks      int64
		}
		if json.Unmarshal(data, &item) != nil {
			return playbackItemMetadata{}
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
			title = ""
		}
		return playbackItemMetadata{title: title, durationSeconds: max(0, item.RunTimeTicks/embyTicksPerSecond)}
	}
}
