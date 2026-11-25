package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	routeScheduleModeAlways = "always"
	routeScheduleModeAvoid  = "avoid"
	routeScheduleModeOnly   = "only"
)

// Fixed UTC+8 location (Beijing time) so schedule evaluation never depends on
// host OS tzdata (e.g. minimal Alpine/Docker scratch images).
var routeScheduleBeijingLoc = time.FixedZone("CST", 8*3600)

// routeScheduleNow is overridden in unit tests to verify daytime / peak / cross-midnight switching deterministically.
var routeScheduleNow = time.Now

func parseClockMinutes(raw string) (int, string, bool) {
	s := strings.TrimSpace(raw)
	if len(s) != 5 || s[2] != ':' {
		return 0, "", false
	}
	h, errH := strconv.Atoi(s[:2])
	m, errM := strconv.Atoi(s[3:])
	if errH != nil || errM != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, "", false
	}
	return h*60 + m, fmt.Sprintf("%02d:%02d", h, m), true
}

func normalizeRouteSchedule(mode, start, end string) (string, string, string, error) {
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" || m == routeScheduleModeAlways {
		return routeScheduleModeAlways, "", "", nil
	}
	if m != routeScheduleModeAvoid && m != routeScheduleModeOnly {
		return "", "", "", fmt.Errorf("时段模式无效（仅支持全天、避开时段、仅指定时段）")
	}
	startMin, startNorm, okStart := parseClockMinutes(start)
	endMin, endNorm, okEnd := parseClockMinutes(end)
	if !okStart || !okEnd {
		return "", "", "", fmt.Errorf("时段格式无效，请使用 HH:MM（例如 19:30 至 00:30）")
	}
	if startMin == endMin {
		return "", "", "", fmt.Errorf("开始时间与结束时间不能相同")
	}
	return m, startNorm, endNorm, nil
}

func (r ispRouteRule) effectiveScheduleMode() string {
	switch r.ScheduleMode {
	case routeScheduleModeAvoid, routeScheduleModeOnly:
		return r.ScheduleMode
	default:
		return routeScheduleModeAlways
	}
}

func (r ispRouteRule) isScheduledActiveAt(now time.Time) bool {
	mode := r.effectiveScheduleMode()
	if mode == routeScheduleModeAlways {
		return true
	}
	startMin, _, okStart := parseClockMinutes(r.ScheduleStart)
	endMin, _, okEnd := parseClockMinutes(r.ScheduleEnd)
	if !okStart || !okEnd || startMin == endMin {
		return true
	}
	bj := now.In(routeScheduleBeijingLoc)
	nowMin := bj.Hour()*60 + bj.Minute()
	var inWindow bool
	if startMin < endMin {
		inWindow = nowMin >= startMin && nowMin < endMin
	} else {
		// Cross-midnight window, e.g., 19:30 - 00:30.
		inWindow = nowMin >= startMin || nowMin < endMin
	}
	if mode == routeScheduleModeAvoid {
		return !inWindow
	}
	return inWindow
}

func (r ispRouteRule) isScheduledDormantAt(now time.Time) bool {
	return r.Enabled && !r.isScheduledActiveAt(now)
}

func (r ispRouteRule) scheduleResumeAt(now time.Time) string {
	if !r.isScheduledDormantAt(now) {
		return ""
	}
	switch r.effectiveScheduleMode() {
	case routeScheduleModeAvoid:
		return r.ScheduleEnd
	case routeScheduleModeOnly:
		return r.ScheduleStart
	default:
		return ""
	}
}
