package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// 32-char lowercase hex IDs (Emby GUID-compatible) reserved exclusively for
	// the in-player virtual "switch server" library and its site cards.
	shareSwitchLibraryID    = "79756e64757377697463686c69623030" // "yunduswitchlib00"
	shareSwitchSiteIDPrefix = "79756e647573"                     // "yundus" (12 hex) + 8-hex epoch + 12-hex siteID = 32 hex
)

var inPlayerSwitchPNGCache sync.Map

type inPlayerSwitchSite struct {
	ID               int64
	Name             string
	EdgeRoutingMode  string
	EdgePinnedNodeID int64
	EdgePinnedOnline bool
	IconHash         string
}

func nextInPlayerSwitchEpoch(prev uint32) uint32 {
	unixNow := time.Now().Unix()
	var nowEpoch uint32
	if unixNow > 0 && unixNow <= math.MaxUint32 {
		nowEpoch = uint32(unixNow)
	}
	if nowEpoch <= prev {
		return prev + 1
	}
	return nowEpoch
}

func shareSwitchSiteItemIDForEpoch(siteID int64, epoch uint32) string {
	if siteID <= 0 {
		return ""
	}
	return fmt.Sprintf("%s%08x%012x", shareSwitchSiteIDPrefix, epoch, uint64(siteID)&0xffffffffffff)
}

func shareSwitchSiteItemID(siteID int64) string {
	return shareSwitchSiteItemIDForEpoch(siteID, 0)
}

func isShareSwitchLibraryID(raw string) bool {
	return strings.EqualFold(strings.TrimSpace(raw), shareSwitchLibraryID)
}

func parseShareSwitchSiteItemID(raw string) (int64, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if len(trimmed) != 32 || isShareSwitchLibraryID(trimmed) || !strings.HasPrefix(trimmed, shareSwitchSiteIDPrefix) {
		return 0, false
	}
	// Validate the middle 8-hex epoch slice is valid hexadecimal (also accepts
	// legacy v1.7.0 "77730000" epoch bytes seamlessly).
	if _, err := strconv.ParseUint(trimmed[12:20], 16, 32); err != nil {
		return 0, false
	}
	parsed, err := strconv.ParseUint(trimmed[20:], 16, 48)
	if err != nil || parsed == 0 {
		return 0, false
	}
	return int64(parsed), true
}

func (rt *shareUserRuntime) userSwitchEpoch(userID int64) uint32 {
	if rt == nil || userID <= 0 {
		return 0
	}
	rt.mu.RLock()
	var epoch uint32
	if st := rt.users[userID]; st != nil {
		epoch = st.switchEpoch
	}
	rt.mu.RUnlock()
	return epoch
}

func isShareSwitchAnyVirtualID(raw string) bool {
	if isShareSwitchLibraryID(raw) {
		return true
	}
	_, ok := parseShareSwitchSiteItemID(raw)
	return ok
}

func isEmbyUserViewsPath(pathValue string) bool {
	p := normalizeEmbyRelativePath(pathValue)
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	return len(segs) == 3 && segs[0] == "users" && segs[1] != "" && segs[2] == "views"
}

func shortInPlayerSiteName(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "服务器"
	}
	runes := []rune(trimmed)
	if len(runes) <= 10 {
		return trimmed
	}
	if fields := strings.Fields(trimmed); len(fields) > 1 {
		first := []rune(fields[0])
		if len(first) >= 2 && len(first) <= 10 {
			return fields[0]
		}
	}
	return string(runes[:10])
}

func inPlayerSwitchImageTag(kind string, siteID int64, name string, active bool, iconHash string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("yundu-sw-img:%s:%d:%s:%t:%s", kind, siteID, name, active, iconHash)))
	return hex.EncodeToString(sum[:8])
}

// isInPlayerSiteSwitchRequest quickly checks whether an incoming Emby request
// targets the virtual in-player switch library, any of its site cards, their
// PNG covers, or their Favorite toggle endpoints.
func isInPlayerSiteSwitchRequest(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	p := normalizeEmbyRelativePath(r.URL.Path)
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(segs) == 0 {
		return false
	}
	// Query-scoped list requests (/Items or /Users/{uid}/Items[/Latest] with ParentId/AncestorIds/Ids).
	if (len(segs) == 1 && segs[0] == "items") ||
		(len(segs) == 3 && segs[0] == "users" && segs[2] == "items") ||
		(len(segs) == 4 && segs[0] == "users" && segs[2] == "items" && segs[3] == "latest") {
		if q, err := url.ParseQuery(r.URL.RawQuery); err == nil {
			for key, vals := range q {
				switch strings.ToLower(key) {
				case "parentid", "ancestorids", "ids":
					for _, raw := range vals {
						for _, part := range strings.Split(raw, ",") {
							if isShareSwitchAnyVirtualID(part) {
								return true
							}
						}
					}
				}
			}
		}
	}
	// Path-scoped requests carrying a virtual ID segment.
	for _, seg := range segs {
		if isShareSwitchAnyVirtualID(seg) {
			return true
		}
	}
	return false
}

func (rt *shareUserRuntime) inPlayerSiteSwitchEnabled() bool {
	if rt == nil || rt.db == nil {
		return false
	}
	return rt.db.currentSystemSettings().InPlayerSiteSwitchEnabled
}

func (rt *shareUserRuntime) inPlayerSwitchableSites(userID int64) []inPlayerSwitchSite {
	if rt == nil || userID <= 0 {
		return nil
	}
	rt.mu.RLock()
	state := rt.users[userID]
	var order []int64
	if state != nil && state.routePrefix != "" {
		order = append([]int64(nil), state.siteOrder...)
	}
	rt.mu.RUnlock()
	if len(order) == 0 {
		return nil
	}

	sites := make([]inPlayerSwitchSite, 0, len(order))
	if rt.pm != nil {
		rt.pm.mu.RLock()
		for _, sid := range order {
			inst := rt.pm.proxies[sid]
			if inst == nil || !inst.Site.Enabled {
				continue
			}
			item := inPlayerSwitchSite{
				ID:               inst.Site.ID,
				Name:             inst.Site.Name,
				EdgeRoutingMode:  inst.Site.EdgeRoutingMode,
				EdgePinnedNodeID: inst.Site.EdgePinnedNodeID,
				EdgePinnedOnline: true,
			}
			sites = append(sites, item)
		}
		rt.pm.mu.RUnlock()
		if rt.pm.edges != nil {
			for i := range sites {
				if sites[i].EdgeRoutingMode == edgeRoutingModeEdgeFull && sites[i].EdgePinnedNodeID > 0 {
					_, online := rt.pm.edges.nodeDecisionByID(sites[i].EdgePinnedNodeID)
					sites[i].EdgePinnedOnline = online
				}
			}
		}
	}
	if rt.db != nil {
		for i := range sites {
			sites[i].IconHash = rt.db.getEffectiveSiteIcon(sites[i].ID).Hash
		}
	}
	return sites
}

func (rt *shareUserRuntime) promoteShareUserSite(userID, targetSiteID int64) error {
	if rt == nil || rt.db == nil || userID <= 0 || targetSiteID <= 0 {
		return fmt.Errorf("invalid share switch target")
	}
	user, err := rt.db.getShareUser(userID)
	if err != nil {
		return err
	}
	if len(user.Sites) == 0 {
		return fmt.Errorf("no assigned sites")
	}
	ordered := make([]int64, 0, len(user.Sites))
	found := false
	for _, s := range user.Sites {
		if s.SiteID == targetSiteID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("site not assigned to share user")
	}
	ordered = append(ordered, targetSiteID)
	for _, s := range user.Sites {
		if s.SiteID != targetSiteID {
			ordered = append(ordered, s.SiteID)
		}
	}
	tx, err := rt.db.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for idx, sid := range ordered {
		if _, err := tx.Exec(
			`UPDATE share_user_sites SET sort_order=? WHERE share_user_id=? AND site_id=?`,
			idx+1, userID, sid,
		); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := rt.reload(); err != nil {
		log.Printf("[share-switch] reload after in-player switch failed: %v", err)
	}
	rt.reconcileSiteSwitch(userID, false)
	return nil
}

func buildInPlayerSwitchLibraryDTO(activeSite inPlayerSwitchSite, siteCount int, virtualServerID string) map[string]any {
	imgTag := inPlayerSwitchImageTag("lib", activeSite.ID, activeSite.Name, true, activeSite.IconHash)
	return map[string]any{
		"Name":              "切服 · " + shortInPlayerSiteName(activeSite.Name),
		"ServerId":          virtualServerID,
		"Id":                shareSwitchLibraryID,
		"Etag":              imgTag,
		"Type":              "CollectionFolder",
		"CollectionType":    "movies",
		"IsFolder":          true,
		"ChildCount":        siteCount,
		"LocationType":      "Virtual",
		"ImageTags":         map[string]string{"Primary": imgTag},
		"BackdropImageTags": []string{},
		"UserData": map[string]any{
			"PlaybackPositionTicks": 0,
			"PlayCount":             0,
			"IsFavorite":            false,
			"Played":                false,
		},
	}
}

func buildInPlayerSwitchSiteDTO(site inPlayerSwitchSite, isActive bool, virtualServerID string, epoch uint32) map[string]any {
	itemID := shareSwitchSiteItemIDForEpoch(site.ID, epoch)
	imgTag := inPlayerSwitchImageTag("site", site.ID, site.Name, isActive, site.IconHash)
	displayName := site.Name
	tagline := "点亮爱心收藏 ❤️ 即刻无感切换到此服务器"
	overview := fmt.Sprintf("点击右上角或操作栏的「爱心收藏 ❤️」即可无感切换到「%s」。切换后返回首页下拉刷新即可浏览新服片库。", site.Name)
	if isActive {
		displayName = "✅ " + site.Name
		tagline = "当前正在使用此服务器"
		overview = fmt.Sprintf("当前正在使用「%s」。如需切换其他服务器，请返回「切服」列表并点亮目标服务器的爱心收藏 ❤️。", site.Name)
	} else if site.EdgeRoutingMode == edgeRoutingModeEdgeFull && site.EdgePinnedNodeID > 0 && !site.EdgePinnedOnline {
		overview = fmt.Sprintf("注意：「%s」当前绑定的专属从机处于离线状态，切过去后可能暂不可用。仍可点击爱心收藏 ❤️ 切换。", site.Name)
	}
	return map[string]any{
		"Name":                  displayName,
		"OriginalTitle":         site.Name,
		"SortName":              site.Name,
		"ServerId":              virtualServerID,
		"Id":                    itemID,
		"ParentId":              shareSwitchLibraryID,
		"Etag":                  imgTag,
		"Type":                  "Movie",
		"MediaType":             "Video",
		"IsFolder":              false,
		"LocationType":          "Virtual",
		"ProductionYear":        2026,
		"RunTimeTicks":          int64(60 * 10000000),
		"Overview":              overview,
		"Taglines":              []string{tagline},
		"Genres":                []string{"云渡快捷切服"},
		"ImageTags":             map[string]string{"Primary": imgTag},
		"BackdropImageTags":     []string{imgTag},
		"PrimaryImageAspectRatio": 0.6667,
		"MediaSources":          []any{},
		"UserData": map[string]any{
			"PlaybackPositionTicks": 0,
			"PlayCount":             0,
			"IsFavorite":            isActive,
			"Played":                false,
			"Key":                   itemID,
		},
	}
}

// injectInPlayerSwitchViewResponse prepends the virtual "切服 · <activeSite>"
// library card as the first item in /Users/{uid}/Views for multi-site personal
// routes when the admin toggle is enabled.
func (rt *shareUserRuntime) injectInPlayerSwitchViewResponse(
	resp *http.Response,
	upstreamBasePath string,
	routeUserID int64,
	currentSiteID int64,
	virtualServerID string,
) error {
	if rt == nil || resp == nil || resp.Request == nil || resp.Request.URL == nil || resp.StatusCode != http.StatusOK || resp.Body == nil {
		return nil
	}
	if !rt.inPlayerSiteSwitchEnabled() {
		return nil
	}
	relPath := stripUpstreamBasePath(resp.Request.URL.Path, upstreamBasePath)
	if !isEmbyUserViewsPath(relPath) {
		return nil
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(ct, "json") {
		return nil
	}
	sites := rt.inPlayerSwitchableSites(routeUserID)
	if len(sites) < 2 {
		return nil
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, shareLoginResponseLimit))
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	decoded := raw
	if strings.EqualFold(strings.TrimSpace(resp.Header.Get("Content-Encoding")), "gzip") {
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

	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		return nil
	}
	items, ok := payload["Items"].([]any)
	if !ok {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		return nil
	}
	for _, elem := range items {
		if obj, isMap := elem.(map[string]any); isMap {
			if idStr, _ := obj["Id"].(string); isShareSwitchLibraryID(idStr) {
				resp.Body = io.NopCloser(bytes.NewReader(raw))
				return nil
			}
		}
	}

	activeSite := sites[0]
	for _, candidate := range sites {
		if candidate.ID == currentSiteID {
			activeSite = candidate
			break
		}
	}
	virtualView := buildInPlayerSwitchLibraryDTO(activeSite, len(sites), virtualServerID)
	newItems := make([]any, 0, len(items)+1)
	newItems = append(newItems, virtualView)
	newItems = append(newItems, items...)
	payload["Items"] = newItems
	if total, ok := payload["TotalRecordCount"].(float64); ok {
		payload["TotalRecordCount"] = int(total) + 1
	} else {
		payload["TotalRecordCount"] = len(newItems)
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		return nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(encoded))
	resp.ContentLength = int64(len(encoded))
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Transfer-Encoding")
	resp.Header.Del("ETag")
	resp.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp.Header.Set("Cache-Control", "no-store")
	resp.Header.Set("Content-Length", strconv.Itoa(len(encoded)))
	return nil
}

func writeInPlayerSwitchJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		writeShareJSONError(w, http.StatusInternalServerError, "encode response failed")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// handleInPlayerSiteSwitchRequest locally serves the virtual "切服" library,
// its server cards, PNG covers, and Favorite-to-switch actions before any
// upstream proxy or edge_full egress hop executes.
func (rt *shareUserRuntime) handleInPlayerSiteSwitchRequest(
	w http.ResponseWriter,
	r *http.Request,
	routeUserID int64,
	shareUser *shareUserState,
	virtualServerID string,
) bool {
	if rt == nil || r == nil || r.URL == nil || routeUserID <= 0 {
		return false
	}
	if !rt.inPlayerSiteSwitchEnabled() || !isInPlayerSiteSwitchRequest(r) {
		return false
	}
	sites := rt.inPlayerSwitchableSites(routeUserID)
	if len(sites) < 2 {
		return false
	}

	p := normalizeEmbyRelativePath(r.URL.Path)
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	isRead := r.Method == http.MethodGet || r.Method == http.MethodHead

	// 1. Virtual PNG cover images (/Items/{virtualID}/Images/{type}[/0]).
	// Outermost "切服 · <active>" card displays the CURRENT active site's own icon;
	// each inner server card displays its own site's icon.
	// Emby clients often omit X-Emby-Token on image fetches, so we authorize by
	// personal-route user + assigned site membership.
	if isRead && len(segs) >= 4 && segs[0] == "items" && segs[2] == "images" && isShareSwitchAnyVirtualID(segs[1]) {
		imgType := strings.ToLower(segs[3])
		landscape := imgType == "backdrop" || imgType == "thumb"
		var imgBytes []byte
		imgContentType := "image/png"
		if isShareSwitchLibraryID(segs[1]) {
			activeIcon := rt.db.getEffectiveSiteIcon(sites[0].ID)
			imgBytes, imgContentType = compositeInPlayerSwitchCardWithIcon("lib", true, sites[0].ID, true, activeIcon)
		} else if sid, ok := parseShareSwitchSiteItemID(segs[1]); ok {
			found := false
			for _, s := range sites {
				if s.ID == sid {
					found = true
					break
				}
			}
			if !found {
				writeShareJSONError(w, http.StatusNotFound, "server card not found")
				return true
			}
			siteIcon := rt.db.getEffectiveSiteIcon(sid)
			imgBytes, imgContentType = compositeInPlayerSwitchCardWithIcon("site", sid == sites[0].ID, sid, landscape, siteIcon)
		}
		if len(imgBytes) > 0 {
			w.Header().Set("Content-Type", imgContentType)
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Cache-Control", "public, max-age=300")
			w.Header().Set("Content-Length", strconv.Itoa(len(imgBytes)))
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = w.Write(imgBytes) // #nosec G705 -- rendered PNG or validated image bytes served with nosniff.
			}
			return true
		}
	}

	// All data & mutation endpoints require an authenticated share session belonging
	// to this personal route.
	if shareUser == nil || shareUser.id != routeUserID {
		writeShareJSONError(w, http.StatusUnauthorized, "请先登录后再使用快捷切服")
		return true
	}
	activeSiteID := sites[0].ID
	switchEpoch := rt.userSwitchEpoch(routeUserID)

	// 2. Favorite click -> switch primary site!
	// Both POST (favorite) and DELETE (in case a client's stale local cache still
	// thinks a non-active card is favorited) switch to targetSiteID and return
	// IsFavorite=true so single-select heart state never gets stuck.
	if (r.Method == http.MethodPost || r.Method == http.MethodDelete) &&
		len(segs) == 4 && segs[0] == "users" && segs[2] == "favoriteitems" {
		if targetSiteID, ok := parseShareSwitchSiteItemID(segs[3]); ok {
			var targetSite *inPlayerSwitchSite
			for i := range sites {
				if sites[i].ID == targetSiteID {
					targetSite = &sites[i]
					break
				}
			}
			if targetSite == nil {
				writeShareJSONError(w, http.StatusNotFound, "无权切换到该服务器")
				return true
			}
			if targetSiteID != activeSiteID {
				if err := rt.promoteShareUserSite(routeUserID, targetSiteID); err != nil {
					log.Printf("[share-switch] promote user #%d -> site #%d failed: %v", routeUserID, targetSiteID, err)
					writeShareJSONError(w, http.StatusInternalServerError, "切换服务器失败，请稍后重试")
					return true
				}
				log.Printf("[share-switch] in-player favorite switch: user #%d -> site #%d (%s) method=%s ua=%s",
					routeUserID, targetSite.ID, targetSite.Name, r.Method, r.Header.Get("User-Agent"))
			}
			writeInPlayerSwitchJSON(w, http.StatusOK, map[string]any{
				"PlaybackPositionTicks": 0,
				"PlayCount":             0,
				"IsFavorite":            true,
				"Played":                false,
				"Key":                   strings.ToLower(strings.TrimSpace(segs[3])),
			})
			return true
		}
		if isShareSwitchLibraryID(segs[3]) {
			writeInPlayerSwitchJSON(w, http.StatusOK, map[string]any{
				"PlaybackPositionTicks": 0,
				"PlayCount":             0,
				"IsFavorite":            false,
				"Played":                false,
				"Key":                   shareSwitchLibraryID,
			})
			return true
		}
	}

	// 3. List endpoints (/Users/{uid}/Items, /Items, /Users/{uid}/Items/Latest).
	if isRead && ((len(segs) == 1 && segs[0] == "items") ||
		(len(segs) == 3 && segs[0] == "users" && segs[2] == "items") ||
		(len(segs) == 4 && segs[0] == "users" && segs[2] == "items" && segs[3] == "latest")) {
		isLatest := len(segs) == 4 && segs[3] == "latest"
		q := r.URL.Query()
		parentID := strings.TrimSpace(q.Get("ParentId"))
		if parentID == "" {
			parentID = strings.TrimSpace(q.Get("parentId"))
		}
		if parentID == "" {
			parentID = strings.TrimSpace(q.Get("AncestorIds"))
		}
		idsParam := strings.TrimSpace(q.Get("Ids"))
		if idsParam == "" {
			idsParam = strings.TrimSpace(q.Get("ids"))
		}

		var allItems []any
		if isShareSwitchLibraryID(parentID) {
			allItems = make([]any, 0, len(sites))
			for _, s := range sites {
				allItems = append(allItems, buildInPlayerSwitchSiteDTO(s, s.ID == activeSiteID, virtualServerID, switchEpoch))
				if s.IconHash == "" {
					rt.maybeAsyncFetchSiteAutoIcon(s.ID, "", "", 0)
				}
			}
		} else if idsParam != "" {
			for _, part := range strings.Split(idsParam, ",") {
				if isShareSwitchLibraryID(part) {
					allItems = append(allItems, buildInPlayerSwitchLibraryDTO(sites[0], len(sites), virtualServerID))
				} else if sid, ok := parseShareSwitchSiteItemID(part); ok {
					for _, s := range sites {
						if s.ID == sid {
							allItems = append(allItems, buildInPlayerSwitchSiteDTO(s, s.ID == activeSiteID, virtualServerID, switchEpoch))
							break
						}
					}
				}
			}
		}
		if allItems != nil {
			total := len(allItems)
			startIndex := 0
			if rawStart := strings.TrimSpace(q.Get("StartIndex")); rawStart != "" {
				if n, err := strconv.Atoi(rawStart); err == nil && n > 0 {
					startIndex = n
				}
			}
			paged := allItems
			if startIndex >= len(paged) {
				paged = []any{}
			} else if startIndex > 0 {
				paged = paged[startIndex:]
			}
			if rawLimit := strings.TrimSpace(q.Get("Limit")); rawLimit != "" {
				if n, err := strconv.Atoi(rawLimit); err == nil && n >= 0 && n < len(paged) {
					paged = paged[:n]
				}
			}
			if isLatest {
				writeInPlayerSwitchJSON(w, http.StatusOK, paged)
			} else {
				writeInPlayerSwitchJSON(w, http.StatusOK, map[string]any{
					"Items":            paged,
					"TotalRecordCount": total,
					"StartIndex":       startIndex,
				})
			}
			return true
		}
	}

	// 4. Single-item detail (/Users/{uid}/Items/{virtualID} or /Items/{virtualID}).
	if isRead && ((len(segs) == 4 && segs[0] == "users" && segs[2] == "items" && isShareSwitchAnyVirtualID(segs[3])) ||
		(len(segs) == 2 && segs[0] == "items" && isShareSwitchAnyVirtualID(segs[1]))) {
		idSeg := segs[len(segs)-1]
		if isShareSwitchLibraryID(idSeg) {
			writeInPlayerSwitchJSON(w, http.StatusOK, buildInPlayerSwitchLibraryDTO(sites[0], len(sites), virtualServerID))
			return true
		}
		if sid, ok := parseShareSwitchSiteItemID(idSeg); ok {
			for _, s := range sites {
				if s.ID == sid {
					writeInPlayerSwitchJSON(w, http.StatusOK, buildInPlayerSwitchSiteDTO(s, s.ID == activeSiteID, virtualServerID, switchEpoch))
					return true
				}
			}
			writeShareJSONError(w, http.StatusNotFound, "服务器不存在或无权限访问")
			return true
		}
	}

	// 5. Auxiliary metadata requests when player opens the virtual card detail page.
	if len(segs) >= 3 && (segs[0] == "items" || segs[0] == "movies" || segs[0] == "shows") && isShareSwitchAnyVirtualID(segs[1]) {
		sub := segs[2]
		switch sub {
		case "playbackinfo":
			// Players like CapyPlayer automatically pre-fetch PlaybackInfo on detail
			// page open to render codec badges; return a clean 200 empty MediaSources
			// payload to avoid noisy 404 logs.
			writeInPlayerSwitchJSON(w, http.StatusOK, map[string]any{
				"MediaSources":  []any{},
				"PlaySessionId": "yundu-switch",
			})
			return true
		case "similar", "intros", "additionalparts":
			writeInPlayerSwitchJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0})
			return true
		case "localtrailers", "specialfeatures":
			writeInPlayerSwitchJSON(w, http.StatusOK, []any{})
			return true
		case "thememedia", "themevideos", "themesongs":
			writeInPlayerSwitchJSON(w, http.StatusOK, map[string]any{
				"ThemeVideosResult":     map[string]any{"Items": []any{}, "TotalRecordCount": 0},
				"ThemeSongsResult":      map[string]any{"Items": []any{}, "TotalRecordCount": 0},
				"SoundtrackSongsResult": map[string]any{"Items": []any{}, "TotalRecordCount": 0},
			})
			return true
		}
	}
	if len(segs) == 5 && segs[0] == "users" && segs[2] == "items" && isShareSwitchAnyVirtualID(segs[3]) {
		switch segs[4] {
		case "localtrailers", "specialfeatures":
			writeInPlayerSwitchJSON(w, http.StatusOK, []any{})
			return true
		case "intros", "additionalparts", "similar":
			writeInPlayerSwitchJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0})
			return true
		}
	}
	if len(segs) >= 2 && segs[0] == "videos" && isShareSwitchAnyVirtualID(segs[1]) {
		writeShareJSONError(w, http.StatusNotFound, "请点亮爱心收藏 ❤️ 即可切换服务器，无需播放")
		return true
	}

	return false
}

func renderInPlayerSwitchPNG(kind string, active bool, siteID int64, landscape bool) []byte {
	paletteIdx := int(siteID % 6)
	if paletteIdx < 0 {
		paletteIdx = -paletteIdx
	}
	cacheKey := fmt.Sprintf("%s:%t:%d:%t", kind, active, paletteIdx, landscape)
	if cached, ok := inPlayerSwitchPNGCache.Load(cacheKey); ok {
		if data, ok := cached.([]byte); ok {
			return data
		}
	}

	w, h := 400, 600
	if landscape {
		w, h = 640, 360
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))

	// Choose gradient palette:
	// - lib: brand indigo -> teal
	// - active site: vibrant emerald -> teal
	// - inactive site: 6 tasteful dark palettes so each server card feels distinct
	var c0, c1, accent color.NRGBA
	switch {
	case kind == "lib":
		c0 = color.NRGBA{R: 20, G: 29, B: 58, A: 255}
		c1 = color.NRGBA{R: 15, G: 118, B: 142, A: 255}
		accent = color.NRGBA{R: 56, G: 189, B: 248, A: 255}
	case active:
		c0 = color.NRGBA{R: 6, G: 78, B: 59, A: 255}
		c1 = color.NRGBA{R: 16, G: 155, B: 118, A: 255}
		accent = color.NRGBA{R: 110, G: 231, B: 183, A: 255}
	default:
		palettes := [][3]color.NRGBA{
			{{R: 30, G: 41, B: 59, A: 255}, {R: 51, G: 65, B: 85, A: 255}, {R: 129, G: 140, B: 248, A: 255}},
			{{R: 28, G: 37, B: 65, A: 255}, {R: 49, G: 72, B: 117, A: 255}, {R: 96, G: 165, B: 250, A: 255}},
			{{R: 46, G: 28, B: 68, A: 255}, {R: 76, G: 49, B: 112, A: 255}, {R: 192, G: 132, B: 252, A: 255}},
			{{R: 48, G: 36, B: 28, A: 255}, {R: 87, G: 60, B: 42, A: 255}, {R: 251, G: 191, B: 36, A: 255}},
			{{R: 56, G: 29, B: 44, A: 255}, {R: 94, G: 44, B: 70, A: 255}, {R: 251, G: 113, B: 133, A: 255}},
			{{R: 20, G: 46, B: 58, A: 255}, {R: 34, G: 82, B: 102, A: 255}, {R: 34, G: 211, B: 238, A: 255}},
		}
		p := palettes[paletteIdx%len(palettes)]
		c0, c1, accent = p[0], p[1], p[2]
	}

	cx, cy := float64(w)/2, float64(h)/2
	minDim := float64(w)
	if float64(h) < minDim {
		minDim = float64(h)
	}
	ringR := minDim * 0.24
	ringThickness := minDim * 0.028

	for y := 0; y < h; y++ {
		fy := float64(y) / float64(h)
		for x := 0; x < w; x++ {
			fx := float64(x) / float64(w)
			t := 0.55*fx + 0.45*fy
			r := uint8(float64(c0.R)*(1-t) + float64(c1.R)*t)
			g := uint8(float64(c0.G)*(1-t) + float64(c1.G)*t)
			b := uint8(float64(c0.B)*(1-t) + float64(c1.B)*t)

			// Subtle radial highlight around center emblem.
			dx := float64(x) - cx
			dy := float64(y) - cy
			dist := math.Hypot(dx, dy)
			if dist < ringR*1.8 {
				glow := (1 - dist/(ringR*1.8)) * 0.14
				r = blendByte(r, accent.R, glow)
				g = blendByte(g, accent.G, glow)
				b = blendByte(b, accent.B, glow)
			}
			// Circular badge ring.
			if math.Abs(dist-ringR) <= ringThickness/2 {
				r = blendByte(r, 255, 0.85)
				g = blendByte(g, 255, 0.85)
				b = blendByte(b, 255, 0.85)
			} else if dist < ringR-ringThickness/2 {
				r = blendByte(r, 255, 0.08)
				g = blendByte(g, 255, 0.08)
				b = blendByte(b, 255, 0.08)
			}
			// Top & bottom subtle accent strip.
			if y < 6 || y >= h-6 {
				r = blendByte(r, accent.R, 0.85)
				g = blendByte(g, accent.G, 0.85)
				b = blendByte(b, accent.B, 0.85)
			}
			img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: 255})
		}
	}

	white := color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	stroke := ringR * 0.16
	switch {
	case kind == "lib":
		// Crisp double horizontal swap arrows (⇄).
		yTop := cy - ringR*0.24
		yBot := cy + ringR*0.24
		xL := cx - ringR*0.42
		xR := cx + ringR*0.42
		drawThickSegment(img, xL, yTop, xR, yTop, stroke, white)
		drawThickSegment(img, xR-ringR*0.20, yTop-ringR*0.17, xR, yTop, stroke, white)
		drawThickSegment(img, xR-ringR*0.20, yTop+ringR*0.17, xR, yTop, stroke, white)
		drawThickSegment(img, xL, yBot, xR, yBot, stroke, white)
		drawThickSegment(img, xL+ringR*0.20, yBot-ringR*0.17, xL, yBot, stroke, white)
		drawThickSegment(img, xL+ringR*0.20, yBot+ringR*0.17, xL, yBot, stroke, white)
	case active:
		// Crisp white checkmark (✓).
		drawThickSegment(img, cx-ringR*0.36, cy+ringR*0.02, cx-ringR*0.08, cy+ringR*0.30, stroke*1.15, white)
		drawThickSegment(img, cx-ringR*0.08, cy+ringR*0.30, cx+ringR*0.38, cy-ringR*0.28, stroke*1.15, white)
	default:
		// Clean 3-bar server emblem.
		for _, offset := range []float64{-0.28, 0.0, 0.28} {
			yy := cy + ringR*offset
			drawThickSegment(img, cx-ringR*0.38, yy, cx+ringR*0.38, yy, stroke, white)
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	data := buf.Bytes()
	inPlayerSwitchPNGCache.Store(cacheKey, data)
	return data
}

func blendByte(base, over uint8, alpha float64) uint8 {
	v := float64(base)*(1-alpha) + float64(over)*alpha
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

func drawThickSegment(img *image.NRGBA, x1, y1, x2, y2, thickness float64, c color.NRGBA) {
	b := img.Bounds()
	r := thickness / 2
	minX := int(math.Floor(math.Min(x1, x2) - r - 1))
	maxX := int(math.Ceil(math.Max(x1, x2) + r + 1))
	minY := int(math.Floor(math.Min(y1, y2) - r - 1))
	maxY := int(math.Ceil(math.Max(y1, y2) + r + 1))
	if minX < b.Min.X {
		minX = b.Min.X
	}
	if maxX >= b.Max.X {
		maxX = b.Max.X - 1
	}
	if minY < b.Min.Y {
		minY = b.Min.Y
	}
	if maxY >= b.Max.Y {
		maxY = b.Max.Y - 1
	}
	dx := x2 - x1
	dy := y2 - y1
	lenSq := dx*dx + dy*dy
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			var t float64
			if lenSq > 0 {
				t = ((float64(x)-x1)*dx + (float64(y)-y1)*dy) / lenSq
				if t < 0 {
					t = 0
				} else if t > 1 {
					t = 1
				}
			}
			px := x1 + t*dx
			py := y1 + t*dy
			if math.Hypot(float64(x)-px, float64(y)-py) <= r {
				img.SetNRGBA(x, y, c)
			}
		}
	}
}
