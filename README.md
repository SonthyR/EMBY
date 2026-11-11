# 云渡 Yundu

**把 Emby 源站安全地分享给朋友：朋友用你发的账号登录，看片全程经你的服务器中转，源站账号密码从不外泄。**

云渡是一个单文件的 Emby 多站点反代面板，基于 [Meridian](https://github.com/snnabb/Meridian) v1.14.2 二次开发。

## 主要功能

- **分享账号**：在面板里给朋友发账号，朋友在 Emby 客户端填反代地址并登录；服务器用你保存的源站账号代为登录。
  - 到期时间、续期、停用、删除
  - 流量额度，可每月固定日期自动清零
  - 同时在线设备数上限，超出时踢掉最久没用的设备
  - 按账号限速（所有设备合计）
  - 一键复制分享信息；朋友用分享账号登录面板，查看自己的用量、到期和设备
  - 被停用、过期、超额、踢下线的登录立即失效，并在源站注销
- **多站点反代**：路径、独立端口、子域名入口；主备线路自动切换；自动发现、手动回源、直连三种播放模式
- **账号备忘与到期提醒**、**流量统计与限速**、**请求日志**、**TLS 证书**（含 Cloudflare 泛域名）、**加密备份**、**Telegram Bot 管理**
- 单个 Go 程序，内嵌前端与 SQLite，无外部依赖

## 一键安装（Linux）

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/SonthyR/EMBY/master/install.sh)
```

需要先在 [Releases](https://github.com/SonthyR/EMBY/releases) 发布对应版本的程序。安装后的服务名、程序名和目录沿用 `meridian`（与上游一致，便于迁移）。Docker 镜像：`ghcr.io/sonthyr/yundu`。

## 从源码构建与运行

需要 Go 1.26.11 或更高版本。

```bash
go build -trimpath -buildvcs=false -o yundu ./cmd/yundu
```

运行前需要设置独立密钥（只在首次部署时生成一次）：`JWT_SECRET`、`UPSTREAM_HEADER_KEY`、`DYNAMIC_ROUTE_KEY`、`MERIDIAN_SECRET_KEY`、`SETUP_TOKEN`，然后：

```bash
./yundu --port 9090 --db ./data/yundu.db
```

打开 `http://127.0.0.1:9090`，用 `SETUP_TOKEN` 创建管理员。详细说明见 [docs/wiki](docs/wiki)（沿用上游文档，后续逐步改写）。

## 版本

最新发布：`v1.0.1` · [更新记录](CHANGELOG.md)

## 许可与致谢

MIT License。云渡基于 snnabb 的 Meridian（MIT）开发，保留原作者版权声明，见 [LICENSE](LICENSE)。上游原始说明见 [docs/MERIDIAN-README.md](docs/MERIDIAN-README.md)。
