package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	siteIconMaxBytes          = 512 << 10 // 512 KiB
	siteIconFetchTimeout      = 12 * time.Second
	siteIconAutoSourcePublic  = "public_brand"
	siteIconAutoSourceConfirm = "admin_confirmed"
)

func isValidPublicSiteIconAutoSource(source string) bool {
	return source == siteIconAutoSourcePublic || source == siteIconAutoSourceConfirm
}

type cachedSiteIcon struct {
	Data        []byte
	ContentType string
	Source      string // "custom", "auto", or "default"
	Hash        string
}

func detectSiteIconContentType(data []byte) (string, bool) {
	if len(data) == 0 || len(data) > siteIconMaxBytes {
		return "", false
	}
	if len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp", true
	}
	ct := http.DetectContentType(data)
	switch ct {
	case "image/png", "image/jpeg", "image/gif":
		return ct, true
	default:
		return "", false
	}
}

// normalizeSiteIconBytes validates supported image formats (PNG/JPEG/GIF/WebP)
// and transparently converts Windows .ico favicons (both embedded-PNG and
// 32-bit/24-bit BMP DIB frames) into standard PNG.
func normalizeSiteIconBytes(raw []byte) ([]byte, string, bool) {
	if len(raw) == 0 || len(raw) > siteIconMaxBytes {
		return nil, "", false
	}
	if ct, ok := detectSiteIconContentType(raw); ok {
		return raw, ct, true
	}
	if pngData, ok := decodeICOToPNG(raw); ok && len(pngData) <= siteIconMaxBytes {
		return pngData, "image/png", true
	}
	return nil, "", false
}

// decodeICOToPNG extracts the highest-resolution supported frame from a Windows
// ICO file (embedded PNG, 32-bit BGRA DIB, or 24-bit BGR DIB) and encodes it as PNG.
func decodeICOToPNG(raw []byte) ([]byte, bool) {
	if len(raw) < 6 || raw[0] != 0 || raw[1] != 0 || raw[2] != 1 || raw[3] != 0 {
		return nil, false
	}
	count := int(binary.LittleEndian.Uint16(raw[4:6]))
	if count <= 0 || count > 64 || len(raw) < 6+count*16 {
		return nil, false
	}

	var bestPNG []byte
	bestScore := -1
	pngMagic := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

	for i := 0; i < count; i++ {
		off := 6 + i*16
		w := int(raw[off])
		if w == 0 {
			w = 256
		}
		h := int(raw[off+1])
		if h == 0 {
			h = 256
		}
		bpp := int(binary.LittleEndian.Uint16(raw[off+6 : off+8]))
		size := int(binary.LittleEndian.Uint32(raw[off+8 : off+12]))
		imgOff := int(binary.LittleEndian.Uint32(raw[off+12 : off+16]))
		if size <= 0 || imgOff < 6+count*16 || imgOff+size > len(raw) {
			continue
		}
		entry := raw[imgOff : imgOff+size]

		// Embedded PNG frame.
		if len(entry) >= 8 && bytes.Equal(entry[:8], pngMagic) {
			if cfg, err := png.DecodeConfig(bytes.NewReader(entry)); err == nil && cfg.Width > 0 && cfg.Height > 0 {
				score := cfg.Width*cfg.Height*100 + 64
				if score > bestScore {
					bestScore = score
					bestPNG = append([]byte(nil), entry...)
				}
			}
			continue
		}

		// BMP DIB frame (40-byte BITMAPINFOHEADER, uncompressed BI_RGB = 0).
		if len(entry) < 40 {
			continue
		}
		hdrSizeU := binary.LittleEndian.Uint32(entry[0:4])
		rawW := binary.LittleEndian.Uint32(entry[4:8])
		rawH := binary.LittleEndian.Uint32(entry[8:12])
		dibBPP := int(binary.LittleEndian.Uint16(entry[14:16]))
		compression := binary.LittleEndian.Uint32(entry[16:20])
		if hdrSizeU < 40 || int(hdrSizeU) > len(entry) || compression != 0 {
			continue
		}
		hdrSize := int(hdrSizeU)
		if rawW > 0 && rawW <= 512 {
			w = int(rawW)
		} else if rawW > 512 {
			continue
		}
		if rawH > 0 && rawH <= 1024 {
			dibH := int(rawH)
			// ICO DIB headers double height to account for XOR + AND masks.
			if dibH == h*2 || dibH%2 == 0 {
				h = dibH / 2
			} else {
				h = dibH
			}
		} else if rawH > 1024 {
			continue
		}
		if w <= 0 || h <= 0 || w > 512 || h > 512 {
			continue
		}
		if dibBPP != 32 && dibBPP != 24 {
			continue
		}
		if bpp == 0 {
			bpp = dibBPP
		}
		bytesPerPixel := dibBPP / 8
		xorRowStride := ((w*bytesPerPixel + 3) / 4) * 4
		xorSize := xorRowStride * h
		andRowStride := ((w + 31) / 32) * 4
		if hdrSize+xorSize > len(entry) {
			continue
		}
		xorData := entry[hdrSize : hdrSize+xorSize]
		var andData []byte
		if hdrSize+xorSize+andRowStride*h <= len(entry) {
			andData = entry[hdrSize+xorSize : hdrSize+xorSize+andRowStride*h]
		}

		// Detect whether 32-bit DIB carries a non-zero alpha channel.
		hasAlpha := false
		if dibBPP == 32 {
			for y := 0; y < h && !hasAlpha; y++ {
				row := xorData[y*xorRowStride:]
				for x := 0; x < w; x++ {
					if row[x*4+3] != 0 {
						hasAlpha = true
						break
					}
				}
			}
		}

		canvas := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			srcRow := xorData[(h-1-y)*xorRowStride:]
			var maskRow []byte
			if len(andData) > 0 {
				maskRow = andData[(h-1-y)*andRowStride:]
			}
			for x := 0; x < w; x++ {
				b := srcRow[x*bytesPerPixel]
				g := srcRow[x*bytesPerPixel+1]
				r := srcRow[x*bytesPerPixel+2]
				a := uint8(255)
				if dibBPP == 32 && hasAlpha {
					a = srcRow[x*4+3]
				} else if len(maskRow) > 0 {
					if (maskRow[x/8]>>(7-(x%8)))&1 != 0 {
						a = 0
					}
				}
				canvas.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: a})
			}
		}

		var buf bytes.Buffer
		if err := png.Encode(&buf, canvas); err != nil {
			continue
		}
		score := w*h*100 + bpp
		if score > bestScore {
			bestScore = score
			bestPNG = buf.Bytes()
		}
	}

	if len(bestPNG) == 0 {
		return nil, false
	}
	return bestPNG, true
}

func siteIconDataHash(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

func (d *DB) invalidateSiteIconCache(siteID int64) {
	if d != nil {
		d.siteIconCache.Delete(siteID)
	}
}

func (d *DB) getEffectiveSiteIcon(siteID int64) cachedSiteIcon {
	if d == nil || siteID <= 0 {
		return cachedSiteIcon{Source: "default"}
	}
	if v, ok := d.siteIconCache.Load(siteID); ok {
		if cached, ok := v.(cachedSiteIcon); ok {
			return cached
		}
	}
	var customData, autoData []byte
	var customCT, autoCT, autoSource string
	err := d.db.QueryRow(
		`SELECT custom_data, custom_content_type, auto_data, auto_content_type, auto_source FROM site_icons WHERE site_id=?`,
		siteID,
	).Scan(&customData, &customCT, &autoData, &autoCT, &autoSource)
	var out cachedSiteIcon
	switch {
	case err == nil && len(customData) > 0 && customCT != "":
		out = cachedSiteIcon{
			Data:        customData,
			ContentType: customCT,
			Source:      "custom",
			Hash:        siteIconDataHash(customData),
		}
	case err == nil && len(autoData) > 0 && autoCT != "" && isValidPublicSiteIconAutoSource(autoSource):
		out = cachedSiteIcon{
			Data:        autoData,
			ContentType: autoCT,
			Source:      "auto",
			Hash:        siteIconDataHash(autoData),
		}
	default:
		out = cachedSiteIcon{Source: "default"}
	}
	d.siteIconCache.Store(siteID, out)
	return out
}

func (d *DB) hasAutoSiteIcon(siteID int64) bool {
	if d == nil || siteID <= 0 {
		return false
	}
	var count int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM site_icons WHERE site_id=? AND auto_data IS NOT NULL AND length(auto_data) > 0 AND auto_source IN (?, ?)`, siteID, siteIconAutoSourcePublic, siteIconAutoSourceConfirm).Scan(&count); err != nil {
		return false
	}
	return count > 0
}

func (d *DB) saveSiteCustomIcon(siteID int64, data []byte, contentType string) error {
	if d == nil || siteID <= 0 {
		return errors.New("invalid site id")
	}
	ct, ok := detectSiteIconContentType(data)
	if !ok {
		return errors.New("仅支持 PNG / JPG / GIF / WebP 图片且大小不超过 512 KB")
	}
	if contentType == "" {
		contentType = ct
	} else {
		contentType = ct
	}
	_, err := d.db.Exec(
		`INSERT INTO site_icons(site_id, custom_data, custom_content_type, updated_at) VALUES(?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(site_id) DO UPDATE SET custom_data=excluded.custom_data, custom_content_type=excluded.custom_content_type, updated_at=CURRENT_TIMESTAMP`,
		siteID, data, contentType,
	)
	if err == nil {
		d.invalidateSiteIconCache(siteID)
	}
	return err
}

func (d *DB) clearSiteCustomIcon(siteID int64) error {
	if d == nil || siteID <= 0 {
		return errors.New("invalid site id")
	}
	_, err := d.db.Exec(
		`UPDATE site_icons SET custom_data=NULL, custom_content_type='', updated_at=CURRENT_TIMESTAMP WHERE site_id=?`,
		siteID,
	)
	if err == nil {
		d.invalidateSiteIconCache(siteID)
	}
	return err
}

func (d *DB) saveSiteAutoIcon(siteID int64, data []byte, contentType string) error {
	return d.saveSiteAutoIconWithSource(siteID, data, contentType, siteIconAutoSourcePublic)
}

func (d *DB) saveSiteAutoIconWithSource(siteID int64, data []byte, contentType, autoSource string) error {
	if d == nil || siteID <= 0 {
		return errors.New("invalid site id")
	}
	if !isValidPublicSiteIconAutoSource(autoSource) {
		return errors.New("invalid site icon auto source")
	}
	ct, ok := detectSiteIconContentType(data)
	if !ok {
		return errors.New("unsupported upstream icon image")
	}
	contentType = ct
	_, err := d.db.Exec(
		`INSERT INTO site_icons(site_id, auto_data, auto_content_type, auto_source, updated_at) VALUES(?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(site_id) DO UPDATE SET auto_data=excluded.auto_data, auto_content_type=excluded.auto_content_type, auto_source=excluded.auto_source, updated_at=CURRENT_TIMESTAMP`,
		siteID, data, contentType, autoSource,
	)
	if err == nil {
		d.invalidateSiteIconCache(siteID)
	}
	return err
}

func publicSiteBrandIconPaths() []string {
	return []string{
		"/img/u/default",
		"/web/touchicon.png",
		"/web/touchicon144.png",
		"/web/assets/img/icon-transparent.png",
		"/favicon.ico",
		"/web/favicon.ico",
	}
}

// newSiteIconFetchFunc builds an upstream icon fetcher sharing the site's exact
// egressTransport (respecting edge_full), UAHeaderPolicy, and upstream headers.
// An anonymous lookup tries the known brand icon paths. A manual account
// lookup tries its avatar first, then those paths with the temporary token.
func newSiteIconFetchFunc(
	transport http.RoundTripper,
	target *url.URL,
	policy UAHeaderPolicy,
	headers upstreamHeaderPolicy,
) func(ctx context.Context, upstreamUID, upstreamToken string) ([]byte, string, error) {
	return func(ctx context.Context, upstreamUID, upstreamToken string) ([]byte, string, error) {
		candidates := publicSiteBrandIconPaths()
		if uid := strings.TrimSpace(upstreamUID); uid != "" {
			candidates = append([]string{"/Users/" + url.PathEscape(uid) + "/Images/Primary"}, candidates...)
		}

		client := &http.Client{
			Transport: transport,
			Timeout:   siteIconFetchTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}

		clientHdrs := shareHandoffClientHeadersFromContext(ctx)
		ua := sanitizeShareHandoffUserAgent(clientHdrs.UserAgent)

		var lastErr error
		for _, relPath := range candidates {
			cur := *target
			cur.Path = singleJoiningSlash(target.Path, relPath)
			cur.RawPath = ""
			cur.RawQuery = ""

			for hop := 0; hop < 4; hop++ {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, cur.String(), nil)
				if err != nil {
					lastErr = err
					break
				}
				req.Header.Set("Accept", "image/png,image/jpeg,image/webp,image/gif,*/*;q=0.5")
				req.Header.Set("Accept-Encoding", "identity")
				req.Header.Set("User-Agent", ua)
				if upstreamToken != "" {
					req.Header.Set("X-Emby-Token", upstreamToken)
				}
				applyUAHeaderPolicy(req.Header, policy)
				headers.apply(req.Header, &cur)

				resp, err := client.Do(req)
				if err != nil {
					lastErr = err
					break
				}
				if resp.StatusCode >= 300 && resp.StatusCode < 400 {
					loc := strings.TrimSpace(resp.Header.Get("Location"))
					_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
					_ = resp.Body.Close()
					if loc == "" {
						break
					}
					nextURL, parseErr := cur.Parse(loc)
					if parseErr != nil || !strings.EqualFold(nextURL.Host, target.Host) {
						lastErr = errors.New("rejected cross-host icon redirect")
						break
					}
					if (nextURL.Scheme != "http" && nextURL.Scheme != "https") || (cur.Scheme == "https" && nextURL.Scheme != "https") {
						lastErr = errors.New("rejected unsafe icon redirect")
						break
					}
					cur = *nextURL
					continue
				}
				raw, readErr := io.ReadAll(io.LimitReader(resp.Body, siteIconMaxBytes+1))
				_ = resp.Body.Close()
				if readErr != nil {
					lastErr = readErr
					break
				}
				if resp.StatusCode != http.StatusOK {
					lastErr = fmt.Errorf("upstream icon %s returned %d", cur.Path, resp.StatusCode)
					break
				}
				if len(raw) > siteIconMaxBytes {
					lastErr = errors.New("upstream icon exceeds 512 KB")
					break
				}
				if normData, ct, ok := normalizeSiteIconBytes(raw); ok {
					return normData, ct, nil
				}
				lastErr = errors.New("upstream icon is not a supported image format")
				break
			}
		}
		if lastErr == nil {
			lastErr = errors.New("no upstream site icon found")
		}
		return nil, "", lastErr
	}
}

func isPassiveSiteAvatarPath(pathValue string) bool {
	p := normalizeEmbyRelativePath(pathValue)
	for _, allowed := range publicSiteBrandIconPaths() {
		if p == allowed {
			return true
		}
	}
	return false
}

// captureSiteAvatarFromResponse passively caches a site's brand avatar image
// whenever a client naturally fetches /Users/{uid}/Images/Primary or
// /img/u/default through the proxy (zero extra upstream traffic, 100% edge_full safe).
func captureSiteAvatarFromResponse(resp *http.Response, upstreamBasePath string, siteID int64, db *DB) {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil || db == nil || siteID <= 0 {
		return
	}
	if resp.Request.Method != http.MethodGet || resp.StatusCode != http.StatusOK || resp.Body == nil {
		return
	}
	if resp.Header.Get("Content-Encoding") != "" {
		return
	}
	relPath := stripUpstreamBasePath(resp.Request.URL.Path, upstreamBasePath)
	if !isPassiveSiteAvatarPath(relPath) {
		return
	}
	if db.hasAutoSiteIcon(siteID) {
		return
	}
	ctHeader := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if ctHeader != "" && !strings.HasPrefix(ctHeader, "image/") {
		return
	}
	if resp.ContentLength > siteIconMaxBytes {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, siteIconMaxBytes+1))
	if err != nil {
		// Restore whatever was read plus remaining stream so client response is never broken.
		resp.Body = struct {
			io.Reader
			io.Closer
		}{
			Reader: io.MultiReader(bytes.NewReader(raw), resp.Body),
			Closer: resp.Body,
		}
		return
	}
	if len(raw) <= siteIconMaxBytes {
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		if ct, ok := detectSiteIconContentType(raw); ok {
			if err := db.saveSiteAutoIcon(siteID, raw, ct); err != nil {
				log.Printf("[site-icon] passive save site #%d: %v", siteID, err)
			}
		}
		return
	}
	// Oversized image: restore full stream transparently without caching.
	resp.Body = struct {
		io.Reader
		io.Closer
	}{
		Reader: io.MultiReader(bytes.NewReader(raw), resp.Body),
		Closer: resp.Body,
	}
}

type siteIconFetchOutcome struct {
	Data            []byte
	ContentType     string
	Kind            string // "public_brand" or "account_avatar"
	RequiresConfirm bool
}

// maybeAsyncFetchSiteAutoIcon triggers a one-shot background fetch of explicit
// public brand icons when the site does not yet have a cached icon. It NEVER
// borrows any user's personal session or avatar, and in edge_full mode strictly
// routes through the bound edge node.
func (rt *shareUserRuntime) maybeAsyncFetchSiteAutoIcon(siteID int64, _ string, _ string, egressNodeID int64) {
	if rt == nil || rt.db == nil || rt.pm == nil || siteID <= 0 {
		return
	}
	if rt.db.hasAutoSiteIcon(siteID) {
		return
	}
	if _, loaded := rt.db.siteIconFetchInflight.LoadOrStore(siteID, struct{}{}); loaded {
		return
	}
	go func() {
		defer rt.db.siteIconFetchInflight.Delete(siteID)
		ctx, cancel := context.WithTimeout(context.Background(), siteIconFetchTimeout)
		defer cancel()
		_, _, _ = rt.fetchAndCacheSiteIcon(ctx, siteID, "", "", egressNodeID, false)
	}()
}

// fetchAndCacheSiteIcon fetches the site's public brand icon (saving it when
// found). When allowAccountPreview is true (explicit admin action in the panel)
// and no public brand asset exists, it probes the admin's configured site
// account avatar and returns it for explicit preview-before-publish confirmation
// without saving it automatically or reusing any share user's live session.
func (rt *shareUserRuntime) fetchSiteIconWithOutcome(
	ctx context.Context,
	siteID int64,
	egressNodeID int64,
	allowAccountPreview bool,
) (siteIconFetchOutcome, error) {
	if rt == nil || rt.db == nil || rt.pm == nil || siteID <= 0 {
		return siteIconFetchOutcome{}, errors.New("site runtime unavailable")
	}
	rt.pm.mu.RLock()
	inst := rt.pm.proxies[siteID]
	rt.pm.mu.RUnlock()
	if inst == nil || inst.fetchSiteIcon == nil {
		return siteIconFetchOutcome{}, errors.New("站点未运行，无法拉取图标")
	}

	// Respect edge_full strictly (fail-closed).
	reqCtx := ctx
	if inst.Site.EdgeRoutingMode == edgeRoutingModeEdgeFull {
		decision, _, edgeErr := resolveEdgeFullDecision(rt.pm, inst.Site, "", egressNodeID, egressNodeID == 0)
		if edgeErr != nil {
			return siteIconFetchOutcome{}, errors.New("当前站点绑定的从机离线或不可用，为保护源站 IP 已拦截拉取")
		}
		if decision.NodeID > 0 {
			reqCtx = withEdgeEgressTarget(reqCtx, edgeEgressTarget{
				NodeID:    decision.NodeID,
				NodeName:  decision.NodeName,
				PublicURL: decision.PublicURL,
				Token:     decision.Token,
			})
		}
	}

	// 1. Always try explicit public site-wide brand assets first (no credentials).
	publicCtx, cancelPublic := context.WithTimeout(reqCtx, siteIconFetchTimeout)
	data, ct, publicErr := inst.fetchSiteIcon(publicCtx, "", "")
	cancelPublic()
	if publicErr == nil && len(data) > 0 {
		if err := rt.db.saveSiteAutoIconWithSource(siteID, data, ct, siteIconAutoSourcePublic); err != nil {
			return siteIconFetchOutcome{}, err
		}
		return siteIconFetchOutcome{
			Data:            data,
			ContentType:     ct,
			Kind:            siteIconAutoSourcePublic,
			RequiresConfirm: false,
		}, nil
	}

	if !allowAccountPreview {
		if publicErr != nil {
			return siteIconFetchOutcome{}, publicErr
		}
		return siteIconFetchOutcome{}, errors.New("未找到可用的公开图标")
	}
	if inst.shareLogin == nil {
		return siteIconFetchOutcome{}, errors.New("未找到可用的公开图标，当前站点无法使用源站账号预览")
	}

	// Use temporary credentials from the configured accounts for admin preview.
	cands, err := rt.upstreamCredentialCandidates(siteID, 0)
	if errors.Is(err, errNoSavedSiteAccountCredentials) {
		return siteIconFetchOutcome{}, errors.New("未找到可用的公开图标，且没有可用的源站账号")
	}
	if err != nil {
		return siteIconFetchOutcome{}, errors.New("未找到可用的公开图标，读取已配置的源站账号失败")
	}
	var loginStatus int
	for _, cand := range cands {
		token, uid, status, loginErr := inst.shareLogin(reqCtx, cand.username, cand.password, "yundu-icon-probe", "Yundu", "Yundu")
		loginStatus = status
		if token != "" && inst.shareLogout != nil {
			defer func() {
				logoutCtx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), shareLogoutTimeout)
				defer cancel()
				_ = inst.shareLogout(logoutCtx, token)
			}()
		}
		if loginErr == nil && status == http.StatusOK && token != "" && uid != "" {
			data, contentType, fetchErr := inst.fetchSiteIcon(reqCtx, uid, token)
			if reqCtx.Err() != nil {
				return siteIconFetchOutcome{}, errors.New("已登录源站账号，但抓取图标超时")
			}
			if fetchErr != nil || len(data) == 0 {
				return siteIconFetchOutcome{}, errors.New("已登录源站账号，但未找到可用的头像或图标")
			}
			return siteIconFetchOutcome{
				Data:            data,
				ContentType:     contentType,
				Kind:            "account_avatar",
				RequiresConfirm: true,
			}, nil
		}
		if status != http.StatusUnauthorized && status != http.StatusForbidden && status != http.StatusTooManyRequests {
			break
		}
	}
	if reqCtx.Err() != nil {
		return siteIconFetchOutcome{}, errors.New("源站账号登录失败，抓取请求已超时")
	}
	if loginStatus >= http.StatusBadRequest {
		return siteIconFetchOutcome{}, fmt.Errorf("源站账号登录失败（HTTP %d），请检查账号或源站状态", loginStatus)
	}
	return siteIconFetchOutcome{}, errors.New("源站账号登录失败，请检查账号和源站连通性")
}

func (rt *shareUserRuntime) fetchAndCacheSiteIcon(
	ctx context.Context,
	siteID int64,
	_ string,
	_ string,
	egressNodeID int64,
	allowLoginFallback bool,
) ([]byte, string, error) {
	outcome, err := rt.fetchSiteIconWithOutcome(ctx, siteID, egressNodeID, allowLoginFallback)
	if err != nil {
		return nil, "", err
	}
	return outcome.Data, outcome.ContentType, nil
}

// handleSiteIcon serves GET/PUT/DELETE /api/sites/{id}/icon and POST /api/sites/{id}/icon/fetch.
func (a *App) handleSiteIcon(w http.ResponseWriter, r *http.Request, siteID int64, sub string) {
	if _, err := a.db.GetSite(siteID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			a.jsonErr(w, http.StatusNotFound, "site not found")
		} else {
			a.jsonErr(w, http.StatusInternalServerError, "site unavailable")
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case sub == "" && r.Method == http.MethodGet:
		icon := a.db.getEffectiveSiteIcon(siteID)
		w.Header().Set("X-Site-Icon-Source", icon.Source)
		if len(icon.Data) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", icon.ContentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(icon.Data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(icon.Data) // #nosec G705 -- validated image magic bytes served with exact image Content-Type and nosniff.

	case sub == "" && r.Method == http.MethodPut:
		raw, err := io.ReadAll(io.LimitReader(r.Body, siteIconMaxBytes+1))
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, "读取图片失败")
			return
		}
		if len(raw) == 0 || len(raw) > siteIconMaxBytes {
			a.jsonErr(w, http.StatusBadRequest, "图片大小需在 512 KB 以内")
			return
		}
		normData, ct, ok := normalizeSiteIconBytes(raw)
		if !ok {
			a.jsonErr(w, http.StatusBadRequest, "仅支持 PNG / JPG / GIF / WebP / ICO 格式图片")
			return
		}
		if err := a.db.saveSiteCustomIcon(siteID, normData, ct); err != nil {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		a.jsonOK(w, map[string]any{
			"ok":           true,
			"source":       "custom",
			"content_type": ct,
			"hash":         siteIconDataHash(normData),
		})

	case sub == "" && r.Method == http.MethodDelete:
		if err := a.db.clearSiteCustomIcon(siteID); err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "恢复默认图标失败")
			return
		}
		icon := a.db.getEffectiveSiteIcon(siteID)
		a.jsonOK(w, map[string]any{
			"ok":     true,
			"source": icon.Source,
			"hash":   icon.Hash,
		})

	case sub == "fetch" && r.Method == http.MethodPost:
		if a.pm == nil || a.pm.shares == nil {
			a.jsonErr(w, http.StatusServiceUnavailable, "站点运行时未就绪")
			return
		}
		// Keep time for account preview after the anonymous lookup.
		ctx, cancel := context.WithTimeout(r.Context(), 2*siteIconFetchTimeout)
		defer cancel()
		outcome, err := a.pm.shares.fetchSiteIconWithOutcome(ctx, siteID, 0, true)
		if err != nil {
			a.jsonErr(w, http.StatusBadGateway, err.Error())
			return
		}
		icon := a.db.getEffectiveSiteIcon(siteID)
		payload := map[string]any{
			"ok":               true,
			"source":           icon.Source,
			"fetched_type":     outcome.ContentType,
			"fetched_kind":     outcome.Kind,
			"requires_confirm": outcome.RequiresConfirm,
			"hash":             siteIconDataHash(outcome.Data),
		}
		if outcome.RequiresConfirm {
			payload["preview_data_url"] = "data:" + outcome.ContentType + ";base64," + base64.StdEncoding.EncodeToString(outcome.Data)
		}
		a.jsonOK(w, payload)

	default:
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// compositeInPlayerSwitchCardWithIcon renders a crisp 16:9 or 2:3 PNG card with
// the server's real brand icon cleanly scaled and centered inside a rounded
// luminous badge (preserving aspect ratio with zero stretching or clipping).
func compositeInPlayerSwitchCardWithIcon(
	kind string,
	active bool,
	siteID int64,
	landscape bool,
	icon cachedSiteIcon,
) ([]byte, string) {
	if len(icon.Data) == 0 {
		return renderInPlayerSwitchPNG(kind, active, siteID, landscape), "image/png"
	}
	cacheKey := fmt.Sprintf("icon:%s:%t:%d:%t:%s", kind, active, siteID%6, landscape, icon.Hash)
	if cached, ok := inPlayerSwitchPNGCache.Load(cacheKey); ok {
		if data, ok := cached.([]byte); ok {
			return data, "image/png"
		}
	}

	srcImg, _, err := image.Decode(bytes.NewReader(icon.Data))
	if err != nil {
		// WebP or undecodable format: serve raw validated image directly.
		return icon.Data, icon.ContentType
	}

	w, h := 400, 600
	if landscape {
		w, h = 640, 360
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))

	// Sleek dark card background matching active/inactive palette.
	var c0, c1, accent color.NRGBA
	switch {
	case kind == "lib":
		c0 = color.NRGBA{R: 18, G: 24, B: 46, A: 255}
		c1 = color.NRGBA{R: 22, G: 58, B: 82, A: 255}
		accent = color.NRGBA{R: 56, G: 189, B: 248, A: 255}
	case active:
		c0 = color.NRGBA{R: 6, G: 68, B: 52, A: 255}
		c1 = color.NRGBA{R: 15, G: 118, B: 96, A: 255}
		accent = color.NRGBA{R: 52, G: 211, B: 153, A: 255}
	default:
		palettes := [][3]color.NRGBA{
			{{R: 24, G: 31, B: 48, A: 255}, {R: 42, G: 54, B: 74, A: 255}, {R: 129, G: 140, B: 248, A: 255}},
			{{R: 22, G: 30, B: 52, A: 255}, {R: 39, G: 58, B: 94, A: 255}, {R: 96, G: 165, B: 250, A: 255}},
			{{R: 36, G: 22, B: 54, A: 255}, {R: 62, G: 39, B: 92, A: 255}, {R: 192, G: 132, B: 252, A: 255}},
			{{R: 40, G: 29, B: 22, A: 255}, {R: 72, G: 49, B: 35, A: 255}, {R: 251, G: 191, B: 36, A: 255}},
			{{R: 46, G: 23, B: 36, A: 255}, {R: 78, G: 37, B: 58, A: 255}, {R: 251, G: 113, B: 133, A: 255}},
			{{R: 16, G: 38, B: 48, A: 255}, {R: 28, G: 68, B: 86, A: 255}, {R: 34, G: 211, B: 238, A: 255}},
		}
		p := palettes[int(math.Abs(float64(siteID%6)))%len(palettes)]
		c0, c1, accent = p[0], p[1], p[2]
	}

	cx, cy := float64(w)/2, float64(h)/2
	minDim := math.Min(float64(w), float64(h))
	badgeSize := minDim * 0.62
	halfBadge := badgeSize / 2
	cornerR := badgeSize * 0.22

	for y := 0; y < h; y++ {
		fy := float64(y) / float64(h)
		for x := 0; x < w; x++ {
			fx := float64(x) / float64(w)
			t := 0.55*fx + 0.45*fy
			r := uint8(float64(c0.R)*(1-t) + float64(c1.R)*t)
			g := uint8(float64(c0.G)*(1-t) + float64(c1.G)*t)
			b := uint8(float64(c0.B)*(1-t) + float64(c1.B)*t)

			dist := math.Hypot(float64(x)-cx, float64(y)-cy)
			if dist < badgeSize*0.95 {
				glow := (1 - dist/(badgeSize*0.95)) * 0.16
				r = blendByte(r, accent.R, glow)
				g = blendByte(g, accent.G, glow)
				b = blendByte(b, accent.B, glow)
			}
			if y < 6 || y >= h-6 {
				r = blendByte(r, accent.R, 0.85)
				g = blendByte(g, accent.G, 0.85)
				b = blendByte(b, accent.B, 0.85)
			}
			img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: 255})
		}
	}

	// Composite scaled brand icon centered inside rounded-square medallion.
	sb := srcImg.Bounds()
	sw, sh := float64(sb.Dx()), float64(sb.Dy())
	if sw > 0 && sh > 0 {
		// Fit inside badge with small padding so transparent/non-square logos breathe nicely.
		inner := badgeSize * 0.88
		scale := math.Min(inner/sw, inner/sh)
		dw := sw * scale
		dh := sh * scale
		ox := cx - dw/2
		oy := cy - dh/2

		minX := int(math.Floor(cx - halfBadge - 3))
		maxX := int(math.Ceil(cx + halfBadge + 3))
		minY := int(math.Floor(cy - halfBadge - 3))
		maxY := int(math.Ceil(cy + halfBadge + 3))
		if minX < 0 {
			minX = 0
		}
		if maxX >= w {
			maxX = w - 1
		}
		if minY < 0 {
			minY = 0
		}
		if maxY >= h {
			maxY = h - 1
		}

		for y := minY; y <= maxY; y++ {
			for x := minX; x <= maxX; x++ {
				sdf := roundedRectSDF(float64(x)-cx, float64(y)-cy, halfBadge, halfBadge, cornerR)
				if sdf > 2.5 {
					continue
				}
				bg := img.NRGBAAt(x, y)
				if sdf >= -2.5 && sdf <= 2.0 {
					// Crisp subtle border ring around medallion.
					ringAlpha := 0.55
					if active && kind != "lib" {
						ringAlpha = 0.90
					}
					img.SetNRGBA(x, y, color.NRGBA{
						R: blendByte(bg.R, accent.R, ringAlpha),
						G: blendByte(bg.G, accent.G, ringAlpha),
						B: blendByte(bg.B, accent.B, ringAlpha),
						A: 255,
					})
					continue
				}
				if sdf < 0 {
					// Subtle frosted backdrop behind transparent PNG icons.
					bg = color.NRGBA{
						R: blendByte(bg.R, 255, 0.08),
						G: blendByte(bg.G, 255, 0.08),
						B: blendByte(bg.B, 255, 0.08),
						A: 255,
					}
					img.SetNRGBA(x, y, bg)
					ux := (float64(x) - ox) / scale
					uy := (float64(y) - oy) / scale
					if ux >= 0 && ux < sw && uy >= 0 && uy < sh {
						fg := sampleBilinearNRGBA(srcImg, float64(sb.Min.X)+ux, float64(sb.Min.Y)+uy)
						if fg.A > 0 {
							a := float64(fg.A) / 255.0
							img.SetNRGBA(x, y, color.NRGBA{
								R: blendByte(bg.R, fg.R, a),
								G: blendByte(bg.G, fg.G, a),
								B: blendByte(bg.B, fg.B, a),
								A: 255,
							})
						}
					}
				}
			}
		}
	}

	// Small active checkmark badge at bottom-right of medallion for the active server card.
	if active && kind != "lib" {
		bx := cx + halfBadge*0.72
		by := cy + halfBadge*0.72
		br := badgeSize * 0.13
		for y := int(by - br - 2); y <= int(by + br + 2); y++ {
			if y < 0 || y >= h {
				continue
			}
			for x := int(bx - br - 2); x <= int(bx + br + 2); x++ {
				if x < 0 || x >= w {
					continue
				}
				d := math.Hypot(float64(x)-bx, float64(y)-by)
				if d <= br {
					img.SetNRGBA(x, y, color.NRGBA{R: 16, G: 185, B: 129, A: 255})
				} else if d <= br+2.0 {
					img.SetNRGBA(x, y, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
				}
			}
		}
		white := color.NRGBA{R: 255, G: 255, B: 255, A: 255}
		st := br * 0.24
		drawThickSegment(img, bx-br*0.42, by+br*0.02, bx-br*0.10, by+br*0.34, st, white)
		drawThickSegment(img, bx-br*0.10, by+br*0.34, bx+br*0.44, by-br*0.30, st, white)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return icon.Data, icon.ContentType
	}
	out := buf.Bytes()
	inPlayerSwitchPNGCache.Store(cacheKey, out)
	return out, "image/png"
}

func roundedRectSDF(px, py, hw, hh, r float64) float64 {
	qx := math.Abs(px) - (hw - r)
	qy := math.Abs(py) - (hh - r)
	ox := math.Max(qx, 0)
	oy := math.Max(qy, 0)
	return math.Hypot(ox, oy) + math.Min(math.Max(qx, qy), 0) - r
}

func sampleBilinearNRGBA(src image.Image, x, y float64) color.NRGBA {
	b := src.Bounds()
	x0 := int(math.Floor(x))
	y0 := int(math.Floor(y))
	x1 := x0 + 1
	y1 := y0 + 1
	fx := x - float64(x0)
	fy := y - float64(y0)
	clamp := func(v, lo, hi int) int {
		if v < lo {
			return lo
		}
		if v > hi {
			return hi
		}
		return v
	}
	x0 = clamp(x0, b.Min.X, b.Max.X-1)
	x1 = clamp(x1, b.Min.X, b.Max.X-1)
	y0 = clamp(y0, b.Min.Y, b.Max.Y-1)
	y1 = clamp(y1, b.Min.Y, b.Max.Y-1)

	toNRGBA := func(c color.Color) color.NRGBA {
		return color.NRGBAModel.Convert(c).(color.NRGBA)
	}
	c00 := toNRGBA(src.At(x0, y0))
	c10 := toNRGBA(src.At(x1, y0))
	c01 := toNRGBA(src.At(x0, y1))
	c11 := toNRGBA(src.At(x1, y1))

	interp := func(v00, v10, v01, v11 uint8) uint8 {
		top := float64(v00)*(1-fx) + float64(v10)*fx
		bot := float64(v01)*(1-fx) + float64(v11)*fx
		res := top*(1-fy) + bot*fy
		if res < 0 {
			return 0
		}
		if res > 255 {
			return 255
		}
		return uint8(res)
	}
	return color.NRGBA{
		R: interp(c00.R, c10.R, c01.R, c11.R),
		G: interp(c00.G, c10.G, c01.G, c11.G),
		B: interp(c00.B, c10.B, c01.B, c11.B),
		A: interp(c00.A, c10.A, c01.A, c11.A),
	}
}
