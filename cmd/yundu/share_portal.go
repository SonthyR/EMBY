package main

import (
	"html/template"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The self-service page lets a friend check their own share account through
// an unguessable link: status, expiry, traffic and server addresses. It never
// shows the password or upstream details, so a forwarded link leaks nothing
// that would let someone sign in.

type sharePortalSite struct {
	Name      string
	Addresses []string
}

type sharePortalView struct {
	Username     string
	Status       string
	StatusText   string
	ExpiresAt    string
	DaysLeft     string
	Used         string
	Quota        string
	Percent      int
	HasQuota     bool
	NextReset    string
	Devices      int
	MaxDevices   int
	SpeedLimit   int
	Sites        []sharePortalSite
	GeneratedAt  string
	StatusIsGood bool
}

var sharePortalTemplate = template.Must(template.New("portal").Parse(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>我的 Emby 账号</title>
<style>
:root { --bg:#0b0d11; --panel:#141820; --border:#262b35; --text:#e6e9ef; --muted:#8e97a8; --accent:#78aaff; --good:#66d4a6; --warn:#e4b572; --track:#232833; }
@media (prefers-color-scheme: light) { :root { --bg:#f4f6f9; --panel:#fff; --border:#e2e6ec; --text:#1c2230; --muted:#6a7385; --accent:#3a76e0; --good:#1f9d6b; --warn:#b7791f; --track:#e8ecf2; } }
* { box-sizing: border-box; }
body { margin:0; background:var(--bg); color:var(--text); font:15px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif; }
main { max-width:560px; margin:0 auto; padding:32px 16px 48px; }
h1 { font-size:20px; font-weight:600; margin:0 0 4px; }
.sub { color:var(--muted); font-size:13px; margin:0 0 20px; }
.card { background:var(--panel); border:1px solid var(--border); border-radius:16px; padding:18px 20px; margin-bottom:14px; }
.row { display:flex; justify-content:space-between; gap:12px; padding:8px 0; border-bottom:1px solid var(--border); }
.row:last-child { border-bottom:0; }
.label { color:var(--muted); }
.value { text-align:right; overflow-wrap:anywhere; }
.badge { display:inline-block; padding:2px 10px; border-radius:999px; font-size:13px; background:var(--track); color:var(--warn); }
.badge.good { color:var(--good); }
.bar { height:6px; background:var(--track); border-radius:3px; overflow:hidden; margin-top:10px; }
.bar span { display:block; height:100%; background:var(--accent); }
h2 { font-size:15px; font-weight:600; margin:0 0 8px; }
.site { padding:8px 0; border-bottom:1px solid var(--border); }
.site:last-child { border-bottom:0; }
.addr { font-family:ui-monospace, SFMono-Regular, Consolas, monospace; font-size:13px; color:var(--accent); overflow-wrap:anywhere; user-select:all; }
.foot { color:var(--muted); font-size:12px; text-align:center; margin-top:18px; }
</style>
</head>
<body>
<main>
<h1>{{.Username}}</h1>
<p class="sub">Emby 分享账号 · 状态 <span class="badge{{if .StatusIsGood}} good{{end}}">{{.StatusText}}</span></p>
<section class="card">
<div class="row"><span class="label">到期时间</span><span class="value">{{.ExpiresAt}}{{if .DaysLeft}}（{{.DaysLeft}}）{{end}}</span></div>
<div class="row"><span class="label">已用流量</span><span class="value">{{.Used}}{{if .HasQuota}} / {{.Quota}}{{end}}</span></div>
{{if .NextReset}}<div class="row"><span class="label">下次流量重置</span><span class="value">{{.NextReset}}</span></div>{{end}}
{{if .SpeedLimit}}<div class="row"><span class="label">限速</span><span class="value">{{.SpeedLimit}} Mbps</span></div>{{end}}
<div class="row"><span class="label">在线设备</span><span class="value">{{.Devices}}{{if .MaxDevices}} / 最多 {{.MaxDevices}} 台{{end}}</span></div>
{{if .HasQuota}}<div class="bar"><span style="width:{{.Percent}}%"></span></div>{{end}}
</section>
<section class="card">
<h2>服务器地址</h2>
{{range .Sites}}<div class="site"><div>{{.Name}}</div>{{range .Addresses}}<div class="addr">{{.}}</div>{{end}}</div>{{else}}<p class="label">暂无可用服务器</p>{{end}}
</section>
<p class="foot">在 Emby 客户端添加上面的服务器地址，用分享给你的用户名和密码登录。<br>更新于 {{.GeneratedAt}}</p>
</main>
</body>
</html>
`))

// GET /share/{token}
func (a *App) handleSharePortal(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	header.Set("Cache-Control", "no-store")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Robots-Tag", "noindex, nofollow")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := strings.Trim(strings.TrimPrefix(r.URL.Path, "/share/"), "/")
	item, err := a.db.getShareUserByPortalToken(token)
	if err != nil {
		http.Error(w, "链接无效或已重置", http.StatusNotFound)
		return
	}
	view := a.shareUserView(item, false)
	location := a.db.shareLocation()
	now := time.Now().In(location)
	page := sharePortalView{
		Username:    view.Username,
		Status:      view.Status,
		Used:        formatShareBytes(view.TrafficUsed),
		HasQuota:    view.TrafficQuota > 0,
		Devices:     view.SessionCount,
		MaxDevices:  view.MaxDevices,
		SpeedLimit:  view.SpeedLimit,
		NextReset:   view.NextResetAt,
		GeneratedAt: now.Format("2006-01-02 15:04"),
	}
	switch view.Status {
	case "active":
		page.StatusText, page.StatusIsGood = "正常", true
	case "disabled":
		page.StatusText = "已停用"
	case "expired":
		page.StatusText = "已过期"
	case "quota":
		page.StatusText = "流量已用完"
	default:
		page.StatusText = view.Status
	}
	if view.ExpiresAt == "" {
		page.ExpiresAt = "永久"
	} else {
		page.ExpiresAt = formatSiteAccountExpiryDate(view.ExpiresAt)
		if expiry := a.db.shareExpiryTime(view.ExpiresAt); !expiry.IsZero() {
			days := int(math.Ceil(expiry.Sub(now).Hours() / 24))
			if days > 0 {
				page.DaysLeft = "剩余 " + strconv.Itoa(days) + " 天"
			} else {
				page.DaysLeft = "已到期"
			}
		}
	}
	if page.HasQuota {
		page.Quota = formatShareBytes(view.TrafficQuota)
		page.Percent = int(math.Min(100, math.Round(float64(view.TrafficUsed)/float64(view.TrafficQuota)*100)))
	}
	for _, assigned := range view.Sites {
		site, err := a.db.GetSite(assigned.SiteID)
		if err != nil || !site.Enabled {
			continue
		}
		page.Sites = append(page.Sites, sharePortalSite{Name: site.Name, Addresses: a.siteProxyLines(*site)})
	}
	header.Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}
	_ = sharePortalTemplate.Execute(w, page)
}
