# siteproxy-go

**siteproxy 的原生 Go 实现** —— 基于 [netptop/siteproxy](https://github.com/netptop/siteproxy) v2.7.11 的行为逆向移植，
无 Node.js 依赖，编译后是**单个静态二进制文件**，开箱即用。

```
                                                  +----> google / youtube
                              +----------------+  |
                              |                |  |
user browser +--------------->+ siteproxy-go   +-------> wikipedia
                              |                |  |
                              +----------------+  |
                                                  +----> duckduckgo / telegram
```

## 与原版的功能对齐

| 功能 | 原版 (Node.js) | siteproxy-go |
|------|---------------|--------------|
| 路径式反代 `{token}{proto}/{host}/{path}` | ✅ | ✅ 同样的 URL 格式 |
| token_prefix 密码保护 | ✅ | ✅ |
| HTML/CSS/JS 服务端 URL 重写（href/src/srcset/action/url()/@import） | ✅ | ✅ 含 v2.7.x 各项修复（srcset 逗号、style 内 url()、自引用防双包等） |
| 客户端运行时（location / history / window.open / fetch / XHR / WebSocket / EventSource / sendBeacon 覆盖） | ✅ 混淆 | ✅ 明文重写，逻辑一致 |
| Service Worker（越界请求修复、404 重试、PROXY_CUR_LOCATION 同步） | ✅ 混淆 | ✅ 明文重写 |
| Cookie 按站点隔离（v2.6.0，防 8K Cookie 502） | ✅ | ✅ Path 改写 + Domain 剥离 + document.cookie 拦截 + 控制 cookie 过滤 |
| CSP / X-Frame-Options / COOP / COEP 剥离 | ✅ | ✅ |
| GBK / GB18030 / Big5 / Shift-JIS / EUC-KR 自动转码为 UTF-8 | ✅ | ✅（含 meta 嗅探与 charset 注入） |
| gzip / deflate / brotli 透明解码 | ➖ node 模式原样转发压缩体（不重写） | ✅ 解码后重写（与 CF worker 模式行为一致） |
| 无引号 HTML 属性重写（如百度 `href=//...`） | ➖ | ✅ 额外支持 |
| SSE / 流式响应（Duck.ai 对话依赖） | ✅ | ✅ 逐块 flush |
| WebSocket 双向代理（Telegram Web 等） | ✅ | ✅ hijack + 双向管道 |
| 被墙站点黑名单 | ✅ | ✅ 配置文件 / 环境变量 |
| 首页（solarized 明暗双主题） | ✅ | ✅ 原版移植 |
| Duck.ai / 常用网站入口 | ✅ | ✅ 首页内置链接 |
| 单文件部署 | ❌ 需 Node 22 | ✅ ~10MB 静态二进制 |
| RSA+AES 密码加密提交 | ✅（面向公共站 netptop.com） | ➖ 未实现，见 FAQ |

## 快速开始

### 1. 从本仓库构建

```bash
git clone https://github.com/ssdxftx/siteproxy-go.git
cd siteproxy-go

# 需要 Go 1.26+
make test
make build
./siteproxy-go -version
```

`make build` 生成当前平台的 `./siteproxy-go`；`make cross` 生成 `./dist/` 下的
linux/amd64、linux/arm64、darwin/amd64、darwin/arm64、windows/amd64、freebsd/amd64
静态二进制。运行时不依赖 Go 工具链或 Node.js，只需要二进制和 `config.json`。

### 2. 修改配置

`config.json` 与原版 siteproxy 完全兼容：

```json
{
  "proxy_url": "https://your-proxy.domain.name",
  "token_prefix": "/user-SetYourPasswordHere/",
  "local_listen_port": 5006,
  "blocked_sites": [],
  "description": "注意:token_prefix相当于网站密码，请谨慎设置,首尾的斜杠必须保留。 proxy_url和token_prefix合起来就是访问网址。"
}
```

| 字段 | 说明 |
|------|------|
| `proxy_url` | 对外访问的完整地址（nginx 反代之后的公网 URL），用于生成代理链接、判断 HTTPS |
| `token_prefix` | 访问密码，**首尾斜杠必须保留**；显式设为 `"/"` 表示免密码，空字符串回退到原版默认密码 `/user22334455/` |
| `local_listen_port` | 监听端口（默认 5006，与原版/nginx 配置兼容） |
| `blocked_sites` | 禁止代理的站点列表（自动附带 exchangebank.com / netsuite.com 等） |

环境变量（优先级高于 config.json，便于容器部署）：

```bash
# 环境变量优先级高于 config.json，适合 systemd / Docker
export SITEPROXY_PROXY_URL=https://proxy.example.com
export SITEPROXY_TOKEN_PREFIX=/user-your-password/
export SITEPROXY_LOCAL_LISTEN_PORT=5006
export SITEPROXY_BLOCKED_SITES=a.com,b.com

# 仅调试自签名上游证书时设为 1，生产环境保持 0
export SITEPROXY_INSECURE_SKIP_VERIFY=0
```

### 3. 本地运行

```bash
./siteproxy-go -config ./config.json
```

默认配置监听 `5006`，可访问：

```
http://127.0.0.1:5006/user22334455/https/example.com/
```

按 `Ctrl+C` 停止。本地 HTTP 仅用于测试，公网部署请使用下面的 nginx + TLS。

## 服务器部署（systemd + nginx）

生产环境推荐拓扑：

```
浏览器 --HTTPS--> nginx :443 --HTTP--> 127.0.0.1:5006 siteproxy-go
```

Go 进程以 HTTP 监听 `5006`，TLS 由 nginx 终止，nginx 通过回环地址访问它；公网只开放 80/443。

### 1. 安装文件

先在仓库目录把 `config.json` 的 `proxy_url`、`token_prefix` 改成生产值。首次部署执行：

```bash
sudo groupadd --system siteproxy
sudo useradd --system --gid siteproxy --no-create-home --shell /usr/sbin/nologin siteproxy
sudo install -d -m 0755 /opt/siteproxy-go /etc/siteproxy
sudo install -m 0755 -o root -g root siteproxy-go /opt/siteproxy-go/siteproxy-go
sudo install -m 0640 -o root -g siteproxy config.json /etc/siteproxy/config.json
```

`/etc/siteproxy/config.json` 含访问密码，权限保持 `0640`；不要把真实密码提交到仓库。

### 2. 创建 systemd 服务

```bash
sudo tee /etc/systemd/system/siteproxy-go.service >/dev/null <<'EOF'
[Unit]
Description=siteproxy-go reverse proxy
Documentation=https://github.com/ssdxftx/siteproxy-go
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=siteproxy
Group=siteproxy
WorkingDirectory=/opt/siteproxy-go
ExecStart=/opt/siteproxy-go/siteproxy-go -config /etc/siteproxy/config.json
Restart=on-failure
RestartSec=3
LimitNOFILE=65536
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true

[Install]
WantedBy=multi-user.target
EOF
```

### 3. 配置 nginx

先定义 WebSocket 升级映射（如果已有同名 `map` 则跳过）：

```bash
sudo tee /etc/nginx/conf.d/siteproxy-upgrade.conf >/dev/null <<'EOF'
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}
EOF
```

公共反代参数放到 snippet：

```bash
sudo tee /etc/nginx/snippets/siteproxy-proxy.conf >/dev/null <<'EOF'
proxy_pass http://127.0.0.1:5006;
proxy_http_version 1.1;
proxy_set_header Host $host;
proxy_set_header X-Real-IP $remote_addr;
proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
proxy_set_header X-Forwarded-Proto $scheme;
proxy_set_header Upgrade $http_upgrade;
proxy_set_header Connection $connection_upgrade;
proxy_buffering off;
proxy_request_buffering off;
proxy_read_timeout 3600s;
proxy_send_timeout 3600s;
client_max_body_size 0;
EOF
```

站点配置（把 `proxy.example.com` 换成你的域名）：

```bash
sudo tee /etc/nginx/sites-available/siteproxy-go >/dev/null <<'EOF'
server {
    listen 80;
    listen [::]:80;
    server_name proxy.example.com;

    location / {
        include snippets/siteproxy-proxy.conf;
    }
}
EOF

sudo ln -s /etc/nginx/sites-available/siteproxy-go /etc/nginx/sites-enabled/siteproxy-go
sudo nginx -t
sudo systemctl reload nginx
```

`proxy_buffering off` 是 SSE 流式响应所需；`proxy_request_buffering off` 避免大上传落盘；
`client_max_body_size 0` 允许代理大文件上传，可按需收紧。

### 4. 申请 TLS 证书

```bash
sudo apt install -y certbot python3-certbot-nginx
sudo certbot --nginx -d proxy.example.com
sudo nginx -t
sudo systemctl reload nginx
```

其他发行版使用对应的 certbot 安装方式。证书续期由 certbot 的 timer 自动完成。

### 5. 启动与验证

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now siteproxy-go
sudo systemctl status siteproxy-go --no-pager
```

```bash
curl -sS -o /dev/null -w '%{http_code}\n' https://proxy.example.com/
curl -sS -o /dev/null -w '%{http_code}\n' https://proxy.example.com/user-your-password/https/example.com/
```

两个请求都应返回 `200`。异常时查看日志：

```bash
sudo journalctl -u siteproxy-go -n 100 --no-pager
```

### 6. 防火墙

只对公网开放 80/443，禁止直接访问 5006：

```bash
sudo ufw allow 'Nginx Full'
sudo ufw deny 5006/tcp
```

如果使用云厂商安全组，同样只放行 80/443。Go 进程监听 `:5006`（所有网卡），必须用防火墙
阻止公网直连；nginx 通过 `127.0.0.1:5006` 访问，不要用 `http://IP:5006` 对外提供服务。

### 7. 更新

```bash
cd siteproxy-go
git pull --ff-only
make test
make build
sudo install -m 0755 -o root -g root siteproxy-go /opt/siteproxy-go/siteproxy-go
sudo systemctl restart siteproxy-go
```

升级只替换二进制，不会覆盖 `/etc/siteproxy/config.json`。首页、客户端脚本和
Service Worker 都通过 `go:embed` 编进二进制，重启后自动生效。

## Docker 部署

```bash
git clone https://github.com/ssdxftx/siteproxy-go.git
cd siteproxy-go

# 修改 config.json 中的 proxy_url / token_prefix
docker compose build
docker compose up -d
docker compose logs --tail=100
```

`docker-compose.yml` 默认把端口映射到所有网卡。生产环境建议改成只绑定回环地址，
再由宿主机 nginx 反代：

```yaml
ports:
  - "127.0.0.1:5006:5006"
```

修改后重新执行 `docker compose up -d`。配置通过 volume 挂载到容器内
`/etc/siteproxy/config.json`，也可以取消 `docker-compose.yml` 中 `SITEPROXY_*`
环境变量的注释来覆盖配置。

更新：

```bash
git pull --ff-only
docker compose build
docker compose up -d
```

## 子路径部署

`proxy_url` 支持带路径，例如把服务挂在 `https://proxy.example.com/siteproxy`：

```json
{
  "proxy_url": "https://proxy.example.com/siteproxy",
  "token_prefix": "/user-your-password/"
}
```

nginx 使用相同前缀转发，`proxy_pass` 后面不要加 URI 或结尾斜杠：

```nginx
location = /siteproxy {
    return 301 /siteproxy/;
}

location /siteproxy/ {
    include snippets/siteproxy-proxy.conf;
}
```

访问地址为：

```
https://proxy.example.com/siteproxy/user-your-password/https/example.com/
```

`proxy_url` 的路径必须与 nginx 的 location 前缀一致；cookie Path、客户端运行时和
Service Worker 会自动带上该前缀。

## 工作原理

一次请求经过的完整流水线：

1. **路由**：`/{token_prefix}https/{host}/{path}` → 鉴权 → 解析目标；其余路径回落到首页。
2. **请求侧**：剥离 hop-by-hop 与 CDN 注入头（`cf-*`、`x-forwarded-*` 等）；`Referer`/`Origin`
   从代理形式还原为真实目标；`Cookie` 因 Path 隔离天然只含当前站点；透传 POST/PUT 请求体。
3. **WebSocket**：`Upgrade: websocket` 请求走 hijack 双向管道，完整支持 Telegram Web 等长连接。
4. **上游抓取**：不跟随重定向（302 交给浏览器在代理内继续）；`Accept-Encoding: gzip, deflate, br`。
5. **响应侧**：剥离 CSP / COOP / COEP / X-Frame-Options；`Location` / `Refresh` / `Set-Cookie` 重写
   （Cookie 按站点隔离到 `Path={token}{proto}/{host}`）；SSE 与二进制逐块透传。
6. **文本重写**：HTML（属性 / srcset / style / meta refresh / `<base>`）注入运行时脚本；CSS（url() / @import）；
   JS（引号包裹的绝对 URL 字面量，正则字面量不受影响）。
7. **字符集**：GBK/Big5/Shift-JIS 等自动转码为 UTF-8 并注入 `<meta charset>`。
8. **浏览器端**：注入脚本以源码形式提供 `proxyRewrite()`（与原版规则一致），覆盖
   location / pushState / fetch / XHR / WebSocket / EventSource / sendBeacon / document.URL，
   并注册 Service Worker 修复越界子资源请求与 404 重试。

## 项目结构

```
siteproxy-go/
├── main.go                 # 入口：配置加载、HTTP 服务、优雅退出
├── config.go               # config.json + 环境变量覆盖
├── server.go               # 路由：首页 / 注入脚本 / SW / 代理分发 + token 鉴权
├── proxy.go                # 核心反代流水线（请求构建、响应分流、流式）
├── headers.go              # 请求头过滤、响应头重写、Set-Cookie 隔离
├── urlmap.go               # 代理 URL ↔ 真实 URL 转换（ParseTarget / RewriteURL）
├── rewrite.go              # HTML / CSS / JS 重写引擎 + 脚本注入
├── charset.go              # 多字符集自动转码
├── compress.go             # gzip / deflate / brotli 解码
├── websocket.go            # WebSocket 双向代理
├── assets.go               # go:embed 资源渲染
├── assets/
│   ├── index.html          # 原版首页移植
│   ├── injected.tmpl.js    # 客户端运行时（明文）
│   └── service-worker.tmpl.js
├── proxy_test.go           # 单元 + 集成测试（mock 上游全覆盖）
├── Makefile / Dockerfile / docker-compose.yml
└── config.json
```

## 测试

```bash
make test
```

覆盖：URL 转换、HTML/CSS/JS 重写、CSP 剥离、Cookie 隔离、重定向、gzip/brotli 解码、
GBK 转码、二进制透传、SSE 流式、请求头变换、黑名单、token 鉴权、免密码模式、WebSocket 回显。

## FAQ

**Q: 为什么没有 RSA+AES 密码加密提交？**
原版该功能用于公共站点 www.netptop.com 的共享密码防嗅探（浏览器 → Cloudflare 边缘段）。
自部署场景下浏览器到你的服务器之间已由你自己的 TLS（nginx + certbot）保护，该功能无增量价值。
客户端脚本中表单提交 hook 已保留，如需可自行扩展。

**Q: 与 Cloudflare Worker 版有什么区别？**
Go 版面向 VPS 部署（与原版 Node VPS 部署方式一致）。原版为绕过 Cloudflare 响应头大小限制而做的
`x-siteproxy-body-headers` 头走私机制不再需要（Go/nginx 无此限制）。

**Q: 与原版有哪些有意的行为差异（差分测试结论）？**
对原版 v2.7.11 做过三轮逐请求差分审计（mock 上游 + 真实站点 + 去混淆源码逐函数比对；第二、三轮
将 HTML/JS/CSS 重写输出、srcset 解析、请求头过滤、Location/Cookie 行为逐字节比对到与 node 模式一致），
以下为经过验证的差异：
- **压缩响应**：node/Go 行为等价——node 依赖 undici 自动解压后重写，Go 自行解码 gzip/deflate/br 后重写；
  遇到双方都无法解码的编码（如 zstd）时，原版直接 500 崩溃，Go 版原样透传（保留 Content-Encoding，不重写）。
- **空 Content-Type**：两版一致——均不重写、按二进制透传（node 默认按 text/plain 处理，Go 同样不注入）。
- **空响应体**：两版一致——不注入、原样返回空体。
- **cookie 生命周期**：原版给所有 Set-Cookie 统一追加 `Max-Age=1800`（30 分钟上限）并回填
  `Domain=<代理主机>`；Go 版保留上游原始过期策略（站点会话不会被意外截断），Domain 一律剥离成 host-only。
- **Secure / SameSite 归一**：原版原样保留 `Secure; SameSite=None`，在纯 HTTP 自部署下浏览器会拒收；
  Go 版按部署协议归一（HTTP 下去掉 Secure、SameSite 改 Lax），HTTPS 部署下与原版等效。
- **控制 cookie**：两版一致——仅在被重写的文本响应（html/js/css，status<500）上追加
  `proxy_real_protocol/host; Path=/; HttpOnly`，并在上游转发前过滤。
- **X-Forwarded-For**：原版不向上游发送，Go 版同样不发送。
- **Connection 命名头**：与原版一致地转发（RFC 语义上属于 hop-by-hop，但保持严格对齐）。
- **srcset 解析**：与原版 v2.7.10 算法逐字节一致（URL token 扫描到空白为止、仅尾部逗号作分隔符），
  压缩 HTML（`1x,b.png 2x` 无空格形式）与含逗号 URL 均正确处理。
- **WebSocket**：原版 node VPS 模式不支持 WebSocket；Go 版原生支持，且升级请求同样过滤控制 cookie
  并接管 hijack 缓冲字节（提前到达的帧不丢失）。
- **首页 / SW**：原版 node 模式不内置首页与 Service Worker（仅 CF 模式提供）；Go 版两者都提供，行为取 CF 模式超集。
- **`/{token}/` 根路径**：原版 302 跳转到硬编码的 `www.netptop.com`；Go 版直接显示首页（自部署更合理）。
- **YouTube 播放页重定向**：原版把 `youtube.com/watch` 重定向到 netptop.com 的嵌入播放器（公共站专用）；
  Go 版不做该跳转，保持站内代理播放。
- **404 处理**：原版 SW 把 404 延迟 3 秒返回（不重发）；Go 版已对齐为延迟后原样返回。
- **meta refresh / @import 重写**：原版保留相对形式（浏览器按当前代理页解析，结果一致）；
  Go 版改写为代理绝对 URL，避免 `<base>` 干扰，语义等价。
- **`x-siteproxy-body-headers`**：原版 node 会把该内部头原样转发给上游；Go 版一律剥离（防内部头泄漏）。
- **响应头剥离超集**：Go 版额外剥离 COOP/COEP/CORP/Permissions-Policy/Alt-Svc 等安全头
  （原版仅剥离 CSP 与 x-frame-options 及 siteproxy*/cf-*/x-forwarded* 家族——Go 已对齐该家族的前缀过滤）。
- **加固项（Go 版增强）**：handler panic 恢复中间件（单请求 502，不拖垮进程）；out-of-scope 修复路径
  对控制 cookie 中的主机做严格校验（防 cookie 投毒 / userinfo 走私）；客户端运行时增加
  Element.setAttribute 陷阱与 data-src 重写（与原版客户端对齐）。
其余行为（URL 格式、HTML/JS/CSS 重写输出、注入脚本与内联引导脚本、cookie 路径隔离、
431 超大 Cookie 自愈、out-of-scope 路径修复、CSP 剥离、转码等）已与原版 node 模式逐字节/逐头对齐。

**Q: 如何修改首页网站列表？**
编辑 `assets/index.html` 后重新 `make build`（资源以 go:embed 打包进二进制）。

## 免责声明

仅供学习研究，请勿用于任何违法用途；使用者自行承担相应责任。
原项目：https://github.com/netptop/siteproxy （MIT License）
