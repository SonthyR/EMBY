package main

// Self-service for share users: a friend signs in to the panel with the same
// share account they use in Emby and sees only their own account. The share
// session is a separate cookie signed with a key derived from JWT_SECRET; it
// is never accepted by the administrator middleware, so it cannot reach any
// management endpoint.

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	shareSessionCookie   = "yundu_share_session"
	shareSessionDuration = 30 * 24 * time.Hour
)

func shareSessionKey() []byte {
	sum := sha256.Sum256(append([]byte("yundu-share-session:v1:"), jwtSecret...))
	return sum[:]
}

// shareSessionFingerprint ties a session to the current password hash, so a
// password change signs every self-service session out.
func shareSessionFingerprint(passwordHash string) string {
	sum := sha256.Sum256([]byte(passwordHash))
	return hex.EncodeToString(sum[:8])
}

func signShareSession(userID int64, passwordHash string, issued time.Time) string {
	payload := strconv.FormatInt(userID, 10) + "." + strconv.FormatInt(issued.Unix(), 10) + "." + shareSessionFingerprint(passwordHash)
	mac := hmac.New(sha256.New, shareSessionKey())
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *App) shareSessionUser(r *http.Request) (*storedShareUser, error) {
	cookie, err := r.Cookie(shareSessionCookie)
	if err != nil || cookie.Value == "" || len(cookie.Value) > 512 {
		return nil, errors.New("no share session")
	}
	encoded, signature, ok := strings.Cut(cookie.Value, ".")
	if !ok {
		return nil, errors.New("malformed share session")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	gotSig, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, shareSessionKey())
	mac.Write(payload)
	if !hmac.Equal(gotSig, mac.Sum(nil)) {
		return nil, errors.New("bad share session signature")
	}
	parts := strings.Split(string(payload), ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed share session")
	}
	userID, err1 := strconv.ParseInt(parts[0], 10, 64)
	issued, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || time.Since(time.Unix(issued, 0)) > shareSessionDuration {
		return nil, errors.New("expired share session")
	}
	var hash string
	if err := a.db.db.QueryRow(`SELECT password_hash FROM share_users WHERE id=?`, userID).Scan(&hash); err != nil {
		return nil, err
	}
	if !hmac.Equal([]byte(parts[2]), []byte(shareSessionFingerprint(hash))) {
		return nil, errors.New("share session revoked")
	}
	return a.db.getShareUser(userID)
}

func (a *App) setShareSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	// #nosec G124 -- same HTTP compatibility rules as the administrator session cookie.
	http.SetCookie(w, &http.Cookie{
		Name:     shareSessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   requestIsHTTPS(r, a.trustedProxies),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// POST /api/me/login
func (a *App) handleShareSelfLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"` // #nosec G117 -- request-only credential DTO.
	}
	if err := decodeJSONBody(w, r, &req); err != nil {
		a.jsonErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	username := strings.TrimSpace(req.Username)
	var id int64
	var hash string
	err := a.db.db.QueryRow(`SELECT id, password_hash FROM share_users WHERE username=? COLLATE NOCASE`, username).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		// Not a share account: the login page then tries the administrator login.
		a.jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not a share account", "share_account": false})
		return
	}
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "登录暂时不可用")
		return
	}
	client := "share|" + requestClientKey(r, a.trustedProxies)
	now := time.Now()
	if allowed, retryAfter := a.limiter().allow(client, now); !allowed {
		a.authRateLimitErr(w, "登录失败次数过多，请稍后再试", retryAfter)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		if blocked, retryAfter := a.limiter().recordFailure(client, now); blocked {
			a.authRateLimitErr(w, "登录失败次数过多，请稍后再试", retryAfter)
			return
		}
		a.jsonErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	a.limiter().reset(client)
	a.setShareSessionCookie(w, r, signShareSession(id, hash, now), int(shareSessionDuration.Seconds()))
	a.jsonOK(w, map[string]any{"username": username, "share_account": true})
}

// POST /api/me/logout
func (a *App) handleShareSelfLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	a.setShareSessionCookie(w, r, "", -1)
	a.jsonOK(w, map[string]bool{"logged_out": true})
}

type shareSelfSite struct {
	Name      string   `json:"name"`
	Addresses []string `json:"addresses"`
}

type shareSelfView struct {
	ShareUser
	Servers []shareSelfSite `json:"servers"`
}

// GET /api/me, POST /api/me/kick, POST /api/me/password
func (a *App) handleShareSelf(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	user, err := a.shareSessionUser(r)
	if err != nil {
		a.jsonErr(w, http.StatusUnauthorized, "请先登录")
		return
	}
	if stateChangingMethod(r.Method) && !requestHasSameOrigin(r) {
		a.jsonErr(w, http.StatusForbidden, "same-origin request required")
		return
	}
	action := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/me"), "/")
	switch action {
	case "":
		if r.Method != http.MethodGet {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		view := shareSelfView{ShareUser: a.shareUserView(user, false)}
		if a.pm != nil && a.pm.shares != nil {
			if sessions, err := a.pm.shares.listSessions(user.ID); err == nil {
				view.Sessions = sessions
			}
		}
		view.Note = ""
		for _, assigned := range view.Sites {
			site, err := a.db.GetSite(assigned.SiteID)
			if err != nil || !site.Enabled {
				continue
			}
			view.Servers = append(view.Servers, shareSelfSite{Name: site.Name, Addresses: a.siteProxyLines(*site)})
		}
		for i := range view.Sites {
			view.Sites[i].SiteAccountID = 0
			view.Sites[i].AccountName = ""
		}
		a.jsonOK(w, view)
	case "kick":
		if r.Method != http.MethodPost {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		count := 0
		if a.pm != nil && a.pm.shares != nil {
			if count, err = a.pm.shares.revokeSessions(user.ID, 0, nil); err != nil {
				a.jsonErr(w, http.StatusInternalServerError, "操作失败")
				return
			}
		}
		a.jsonOK(w, map[string]int{"revoked": count})
	case "password":
		if r.Method != http.MethodPost {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var req struct {
			Current string `json:"current_password"` // #nosec G117 -- request-only credential DTO.
			Next    string `json:"new_password"`     // #nosec G117 -- request-only credential DTO.
		}
		if err := decodeJSONBody(w, r, &req); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "invalid request")
			return
		}
		var hash string
		if err := a.db.db.QueryRow(`SELECT password_hash FROM share_users WHERE id=?`, user.ID).Scan(&hash); err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Current)) != nil {
			a.jsonErr(w, http.StatusBadRequest, "当前密码不正确")
			return
		}
		if len([]rune(req.Next)) < 4 || len(req.Next) > 72 {
			a.jsonErr(w, http.StatusBadRequest, "新密码需要 4–72 个字符")
			return
		}
		newHash, err := bcrypt.GenerateFromPassword([]byte(req.Next), bcrypt.DefaultCost)
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "操作失败")
			return
		}
		cipher, err := encryptSiteAccountSecret(req.Next)
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "操作失败")
			return
		}
		if _, err := a.db.db.Exec(`UPDATE share_users SET password_hash=?, password_ciphertext=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, string(newHash), cipher, user.ID); err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "操作失败")
			return
		}
		a.reloadShareUsers()
		if a.pm != nil && a.pm.shares != nil {
			_, _ = a.pm.shares.revokeSessions(user.ID, 0, nil)
		}
		// Keep this browser signed in with the new password.
		a.setShareSessionCookie(w, r, signShareSession(user.ID, string(newHash), time.Now()), int(shareSessionDuration.Seconds()))
		a.jsonOK(w, map[string]bool{"changed": true})
	default:
		a.jsonErr(w, http.StatusNotFound, "not found")
	}
}
