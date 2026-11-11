package main

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Per-share-user speed limit and dynamic-route attribution.
//
// Dynamic discovery hands clients Meridian capability URLs
// (/_meridian/d/<token>/...) that carry no Emby access token. To keep a share
// user's video traffic attributed to them (and therefore counted, limited and
// blocked), every response served to a share user is scanned for capability
// tokens, and later requests on those tokens are treated as that user's.

const (
	shareCapabilityTTL        = 12 * time.Hour
	shareCapabilityMaxEntries = 200000
	shareCapabilityScanLimit  = 8 << 20
	shareLimiterBurst         = 500 * time.Millisecond
)

var shareCapabilityPattern = regexp.MustCompile(`/_meridian/d/([A-Za-z0-9_\-.~]{16,})`)

type shareCapabilityOwner struct {
	userID  int64
	siteID  int64
	expires time.Time
}

// rememberCapabilities records capability tokens found in text as owned by user.
func (rt *shareUserRuntime) rememberCapabilities(text string, userID, siteID int64) {
	matches := shareCapabilityPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return
	}
	now := time.Now()
	rt.capMu.Lock()
	defer rt.capMu.Unlock()
	if rt.capabilities == nil {
		rt.capabilities = make(map[string]shareCapabilityOwner)
	}
	if len(rt.capabilities)+len(matches) > shareCapabilityMaxEntries {
		for token, owner := range rt.capabilities {
			if now.After(owner.expires) {
				delete(rt.capabilities, token)
			}
		}
		if len(rt.capabilities)+len(matches) > shareCapabilityMaxEntries {
			rt.capabilities = make(map[string]shareCapabilityOwner)
		}
	}
	for _, match := range matches {
		rt.capabilities[match[1]] = shareCapabilityOwner{userID: userID, siteID: siteID, expires: now.Add(shareCapabilityTTL)}
	}
}

// capabilityOwner returns the share user that received the capability in path.
func (rt *shareUserRuntime) capabilityOwner(requestPath string, siteID int64) (int64, bool) {
	normalized := normalizeDynamicCapabilityPath(requestPath)
	if normalized == "" {
		return 0, false
	}
	token, _, _ := strings.Cut(strings.TrimPrefix(normalized, dynamicRoutePrefix), "/")
	if token == "" {
		return 0, false
	}
	rt.capMu.Lock()
	defer rt.capMu.Unlock()
	owner, ok := rt.capabilities[token]
	if !ok || owner.siteID != siteID {
		return 0, false
	}
	if time.Now().After(owner.expires) {
		delete(rt.capabilities, token)
		return 0, false
	}
	return owner.userID, true
}

// shareRateLimiter is a token bucket shared by every request of one user.
type shareRateLimiter struct {
	mu          sync.Mutex
	bytesPerSec float64
	next        time.Time
}

func (l *shareRateLimiter) setRate(mbps int) {
	l.mu.Lock()
	l.bytesPerSec = float64(mbps) * 125000
	l.mu.Unlock()
}

func (l *shareRateLimiter) chunkSize() int {
	l.mu.Lock()
	rate := l.bytesPerSec
	l.mu.Unlock()
	size := int(rate / 10) // about 100 ms of data per write
	if size < 16<<10 {
		size = 16 << 10
	}
	return size
}

// wait blocks until n more bytes fit within the user's rate.
func (l *shareRateLimiter) wait(ctx context.Context, n int) error {
	l.mu.Lock()
	if l.bytesPerSec <= 0 {
		l.mu.Unlock()
		return nil
	}
	now := time.Now()
	if earliest := now.Add(-shareLimiterBurst); l.next.Before(earliest) {
		l.next = earliest
	}
	l.next = l.next.Add(time.Duration(float64(n) / l.bytesPerSec * float64(time.Second)))
	delay := l.next.Sub(now)
	l.mu.Unlock()
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// shareResponseWriter applies a share user's speed limit and records the
// capability URLs handed to them.
type shareResponseWriter struct {
	http.ResponseWriter
	rt          *shareUserRuntime
	userID      int64
	siteID      int64
	limiter     *shareRateLimiter
	ctx         context.Context
	scan        bool
	scanned     int
	tail        string
	wroteHeader bool
}

func (rt *shareUserRuntime) wrapWriter(w http.ResponseWriter, r *http.Request, user *shareUserState, siteID int64) http.ResponseWriter {
	if rt == nil || user == nil {
		return w
	}
	rt.mu.RLock()
	limiter := user.limiter
	rt.mu.RUnlock()
	return &shareResponseWriter{ResponseWriter: w, rt: rt, userID: user.id, siteID: siteID, limiter: limiter, ctx: r.Context()}
}

func shareScannableContentType(value string) bool {
	value = strings.ToLower(value)
	for _, marker := range []string{"json", "mpegurl", "dash+xml", "xml", "text/"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func (w *shareResponseWriter) WriteHeader(status int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		if location := w.Header().Get("Location"); location != "" {
			w.rt.rememberCapabilities(location, w.userID, w.siteID)
		}
		w.scan = shareScannableContentType(w.Header().Get("Content-Type"))
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *shareResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.scan && w.scanned < shareCapabilityScanLimit {
		chunk := w.tail + string(b)
		w.rt.rememberCapabilities(chunk, w.userID, w.siteID)
		w.scanned += len(b)
		if len(chunk) > 256 {
			w.tail = chunk[len(chunk)-256:]
		} else {
			w.tail = chunk
		}
	}
	if w.limiter == nil {
		return w.ResponseWriter.Write(b)
	}
	written := 0
	size := w.limiter.chunkSize()
	for written < len(b) {
		end := written + size
		if end > len(b) {
			end = len(b)
		}
		if err := w.limiter.wait(w.ctx, end-written); err != nil {
			return written, err
		}
		n, err := w.ResponseWriter.Write(b[written:end])
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func (w *shareResponseWriter) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *shareResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
