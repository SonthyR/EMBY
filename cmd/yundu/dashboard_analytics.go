package main

import (
	"math"
	"net/http"
	"sort"
	"time"
)

type analyticsSummary struct {
	TotalTraffic    int64   `json:"total_traffic"`
	MonthlyTraffic  int64   `json:"monthly_traffic"`
	MasterTraffic   int64   `json:"master_traffic"`
	EdgeTraffic     int64   `json:"edge_traffic"`
	OffloadPercent  float64 `json:"offload_percent"`
	TotalRequests   int64   `json:"total_requests"`
	ActiveStreams   int     `json:"active_streams"`
	OnlineSessions  int     `json:"online_sessions"`
	ShareUserCount  int     `json:"share_user_count"`
	ActiveEdgeNodes int     `json:"active_edge_nodes"`
	TotalEdgeNodes  int     `json:"total_edge_nodes"`
}

type analyticsNodeTrafficItem struct {
	NodeID        int64   `json:"node_id"`
	Name          string  `json:"name"`
	IsMaster      bool    `json:"is_master"`
	Online        bool    `json:"online"`
	Status        string  `json:"status"`
	ActiveStreams int     `json:"active_streams"`
	CurrentBps    int64   `json:"current_bps"`
	TrafficUsed   int64   `json:"traffic_used"`
	TrafficQuota  int64   `json:"traffic_quota"`
	SharePercent  float64 `json:"share_percent"`
}

type analyticsShareUserItem struct {
	ID           int64   `json:"id"`
	Username     string  `json:"username"`
	Note         string  `json:"note"`
	Status       string  `json:"status"`
	TrafficUsed  int64   `json:"traffic_used"`
	TrafficQuota int64   `json:"traffic_quota"`
	SessionCount int     `json:"session_count"`
	PlayingCount int     `json:"playing_count"`
	SharePercent float64 `json:"share_percent"`
}

type analyticsSiteTrafficItem struct {
	ID                 int64   `json:"id"`
	Name               string  `json:"name"`
	Running            bool    `json:"running"`
	TrafficUsed        int64   `json:"traffic_used"`
	MonthlyTraffic     int64   `json:"monthly_traffic"`
	CumulativeBytesIn  int64   `json:"cumulative_bytes_in"`
	CumulativeBytesOut int64   `json:"cumulative_bytes_out"`
	Requests           int64   `json:"requests"`
	TrafficQuota       int64   `json:"traffic_quota"`
	SharePercent       float64 `json:"share_percent"`
}

type analyticsDailyPoint struct {
	Date         string `json:"date"`
	Label        string `json:"label"`
	TrafficBytes int64  `json:"traffic_bytes"`
	BytesIn      int64  `json:"bytes_in"`
	BytesOut     int64  `json:"bytes_out"`
	Requests     int64  `json:"requests"`
}

type dashboardAnalyticsResponse struct {
	GeneratedAt   string                     `json:"generated_at"`
	BillingMode   string                     `json:"billing_mode"`
	Summary       analyticsSummary           `json:"summary"`
	NodeBreakdown []analyticsNodeTrafficItem `json:"node_breakdown"`
	TopShareUsers []analyticsShareUserItem   `json:"top_share_users"`
	SiteBreakdown []analyticsSiteTrafficItem `json:"site_breakdown"`
	DailyActivity []analyticsDailyPoint      `json:"daily_activity"`
}

func roundPercent(part, total int64) float64 {
	if total <= 0 || part <= 0 {
		return 0
	}
	pct := (float64(part) / float64(total)) * 100.0
	if pct > 100 {
		pct = 100
	}
	return math.Round(pct*10.0) / 10.0
}

// GET /api/dashboard-analytics
func (a *App) handleDashboardAnalytics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.pm == nil || a.db == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "analytics unavailable")
		return
	}

	now := time.Now()
	settings := a.db.currentSystemSettings()
	billingMode := settings.TrafficBillingMode

	// Read persisted daily buckets in SQL before overlaying live pending deltas
	// to minimize flush-window skew and avoid materializing raw minute rows.
	localNow := now.In(time.Local)
	todayStart := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, time.Local)
	startDay := todayStart.AddDate(0, 0, -13)
	tomorrowStart := todayStart.AddDate(0, 0, 1)

	dailyBuckets, _ := a.db.SumDailyTrafficBetween(startDay, tomorrowStart)
	pendingIn, pendingOut, pendingReqs := a.pm.SumPendingTraffic()

	snap, err := a.pm.TrafficSnapshot()
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "failed to load traffic snapshot")
		return
	}

	var onlineSessions, masterStreams, totalPlayingStreams int
	userPlayingCounts := map[int64]int{}
	if a.pm.shares != nil {
		onlineSessions, masterStreams, totalPlayingStreams, userPlayingCounts = a.pm.shares.analyticsSessionSummary()
	}

	// Build edge node traffic from monotonic directional byte ledgers under the same
	// billingMode as site traffic so quota resets or billing mode toggles stay consistent.
	var edgeTotalTraffic int64
	var activeEdgeNodes, totalEdgeNodes int
	edgeItems := make([]analyticsNodeTrafficItem, 0)
	if a.pm.edges != nil {
		a.pm.edges.mu.RLock()
		totalEdgeNodes = len(a.pm.edges.nodes)
		for _, n := range a.pm.edges.nodes {
			if n == nil {
				continue
			}
			inBytes, outBytes := n.totalDirections()
			cumulativeUsed := trafficBillableBytes(billingMode, inBytes, outBytes)
			online := n.isOnline(now)
			if online && n.enabled {
				activeEdgeNodes++
			}
			edgeTotalTraffic += cumulativeUsed
			edgeItems = append(edgeItems, analyticsNodeTrafficItem{
				NodeID:        n.id,
				Name:          n.name,
				IsMaster:      false,
				Online:        online,
				Status:        n.statusAt(now),
				ActiveStreams: max(0, n.activeStreams),
				CurrentBps:    max(int64(0), n.currentBps),
				TrafficUsed:   cumulativeUsed,
				TrafficQuota:  n.quota,
			})
		}
		a.pm.edges.mu.RUnlock()
	}

	masterTraffic := snap.TotalTraffic - edgeTotalTraffic
	if masterTraffic < 0 {
		masterTraffic = 0
	}
	nodeDenominator := masterTraffic + edgeTotalTraffic

	for i := range edgeItems {
		edgeItems[i].SharePercent = roundPercent(edgeItems[i].TrafficUsed, nodeDenominator)
	}
	sort.Slice(edgeItems, func(i, j int) bool {
		if edgeItems[i].TrafficUsed != edgeItems[j].TrafficUsed {
			return edgeItems[i].TrafficUsed > edgeItems[j].TrafficUsed
		}
		return edgeItems[i].NodeID < edgeItems[j].NodeID
	})

	nodeBreakdown := make([]analyticsNodeTrafficItem, 0, len(edgeItems)+1)
	nodeBreakdown = append(nodeBreakdown, analyticsNodeTrafficItem{
		NodeID:        0,
		Name:          "主机",
		IsMaster:      true,
		Online:        true,
		Status:        "online",
		ActiveStreams: masterStreams,
		TrafficUsed:   masterTraffic,
		SharePercent:  roundPercent(masterTraffic, nodeDenominator),
	})
	nodeBreakdown = append(nodeBreakdown, edgeItems...)

	// Bulk-load session counts per share user and all share users without N+1 site queries.
	sessionCounts := make(map[int64]int)
	if sRows, sErr := a.db.db.Query(`SELECT share_user_id, COUNT(*) FROM share_user_sessions GROUP BY share_user_id`); sErr == nil {
		for sRows.Next() {
			var uid int64
			var cnt int
			if sRows.Scan(&uid, &cnt) == nil {
				sessionCounts[uid] = cnt
			}
		}
		_ = sRows.Close()
	}

	var shareUserCount int
	var shareUsersTotalTraffic int64
	allShareUsers := make([]analyticsShareUserItem, 0)
	if uRows, uErr := a.db.db.Query(`SELECT id, username, note, enabled, expires_at, traffic_quota, traffic_used FROM share_users`); uErr == nil {
		for uRows.Next() {
			var id, quota, used int64
			var username, note, expiresAt string
			var enabledInt int
			if uRows.Scan(&id, &username, &note, &enabledInt, &expiresAt, &quota, &used) != nil {
				continue
			}
			shareUserCount++
			sessCnt := sessionCounts[id]
			if a.pm.shares != nil {
				liveUsed, liveSess := a.pm.shares.usage(id)
				if liveUsed > used {
					used = liveUsed
				}
				if liveSess > sessCnt {
					sessCnt = liveSess
				}
			}
			status := "active"
			switch {
			case enabledInt == 0:
				status = "disabled"
			case expiresAt != "" && !now.Before(a.db.shareExpiryTime(expiresAt)):
				status = "expired"
			case quota > 0 && used >= quota:
				status = "quota"
			}
			shareUsersTotalTraffic += used
			allShareUsers = append(allShareUsers, analyticsShareUserItem{
				ID:           id,
				Username:     username,
				Note:         note,
				Status:       status,
				TrafficUsed:  used,
				TrafficQuota: quota,
				SessionCount: sessCnt,
				PlayingCount: userPlayingCounts[id],
			})
		}
		_ = uRows.Close()
	}
	for i := range allShareUsers {
		allShareUsers[i].SharePercent = roundPercent(allShareUsers[i].TrafficUsed, shareUsersTotalTraffic)
	}
	sort.Slice(allShareUsers, func(i, j int) bool {
		if allShareUsers[i].TrafficUsed != allShareUsers[j].TrafficUsed {
			return allShareUsers[i].TrafficUsed > allShareUsers[j].TrafficUsed
		}
		return allShareUsers[i].ID < allShareUsers[j].ID
	})
	if len(allShareUsers) > 10 {
		allShareUsers = allShareUsers[:10]
	}

	// Site traffic breakdown sorted by cumulative billed usage.
	siteBreakdown := make([]analyticsSiteTrafficItem, 0, len(snap.LiveSites))
	for _, st := range snap.LiveSites {
		siteBreakdown = append(siteBreakdown, analyticsSiteTrafficItem{
			ID:                 st.ID,
			Name:               st.Name,
			Running:            st.Running,
			TrafficUsed:        st.TrafficUsed,
			MonthlyTraffic:     st.MonthlyTraffic,
			CumulativeBytesIn:  st.CumulativeBytesIn,
			CumulativeBytesOut: st.CumulativeBytesOut,
			Requests:           st.Requests,
			TrafficQuota:       st.TrafficQuota,
			SharePercent:       roundPercent(st.TrafficUsed, snap.TotalTraffic),
		})
	}
	sort.Slice(siteBreakdown, func(i, j int) bool {
		if siteBreakdown[i].TrafficUsed != siteBreakdown[j].TrafficUsed {
			return siteBreakdown[i].TrafficUsed > siteBreakdown[j].TrafficUsed
		}
		return siteBreakdown[i].ID < siteBreakdown[j].ID
	})

	todayKey := todayStart.Format("2006-01-02")
	dailyActivity := make([]analyticsDailyPoint, 14)
	for i := 0; i < 14; i++ {
		day := startDay.AddDate(0, 0, i)
		key := day.Format("2006-01-02")
		b := dailyBuckets[key]
		if key == todayKey {
			b.BytesIn += pendingIn
			b.BytesOut += pendingOut
			b.Requests += pendingReqs
		}
		dailyActivity[i] = analyticsDailyPoint{
			Date:         key,
			Label:        day.Format("01-02"),
			BytesIn:      b.BytesIn,
			BytesOut:     b.BytesOut,
			Requests:     b.Requests,
			TrafficBytes: trafficBillableBytes(billingMode, b.BytesIn, b.BytesOut),
		}
	}

	resp := dashboardAnalyticsResponse{
		GeneratedAt: now.UTC().Format(time.RFC3339),
		BillingMode: snap.BillingMode,
		Summary: analyticsSummary{
			TotalTraffic:    snap.TotalTraffic,
			MonthlyTraffic:  snap.MonthlyTraffic,
			MasterTraffic:   masterTraffic,
			EdgeTraffic:     edgeTotalTraffic,
			OffloadPercent:  roundPercent(edgeTotalTraffic, nodeDenominator),
			TotalRequests:   snap.TotalRequests,
			ActiveStreams:   totalPlayingStreams,
			OnlineSessions:  onlineSessions,
			ShareUserCount:  shareUserCount,
			ActiveEdgeNodes: activeEdgeNodes,
			TotalEdgeNodes:  totalEdgeNodes,
		},
		NodeBreakdown: nodeBreakdown,
		TopShareUsers: allShareUsers,
		SiteBreakdown: siteBreakdown,
		DailyActivity: dailyActivity,
	}
	a.jsonOK(w, resp)
}
