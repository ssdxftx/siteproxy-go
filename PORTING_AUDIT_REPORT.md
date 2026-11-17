# siteproxy-go 移植一致性核验报告

- 报告日期：2026-11-17
- 核验对象：当前工作区 `siteproxy-go`
- 对照基线：`netptop/siteproxy` v2.7.11（commit `c39ee16`）
- 核验方式：源码反混淆比对 + 双实例差分测试 + 现有测试/静态检查
- 核验结论：核心链路大体完成，但**不属于忠实移植**；存在 4 项未记录的功能缺失，其中 2 项会直接影响真实站点

---

## 1. 摘要

本次核验拉取了上游 v2.7.11 源码，使用 webcrack 对 Node 模式入口 `bundle.mjs`、注入脚本、Service Worker 进行反混淆，并同时启动原版 Node 服务与当前 Go 二进制，对同一 mock 上游执行逐请求差分。核验覆盖路由、URL 重写、请求头/响应头、Cookie、重定向、字符集、压缩、Service Worker、客户端运行时等主要面。

总体判断：

- 首页资源与上游 `index_www.netptop.com.html` 完全一致。
- 路径式反代格式、常见 HTML/CSS/JS 重写、srcset 解析、GBK/Big5 等字符集转码、Cookie 路径隔离、常规 Location 重写等核心能力基本对齐。
- 存在以下未记录的上游行为缺失：
  1. `telegram.org` 的 URL 重写豁免缺失。
  2. `google.com` 的内联 `integrity` 清理缺失。
  3. 路径中间出现 token 时的越界修复缺失。
  4. 查询串中代理 URL 的还原缺失。
- 另有上游硬编码黑名单未移植，以及客户端运行时、Service Worker、相对 URL 解析、响应头处理等多处差异。
- README 中若干“与原版一致”“逐字节一致”的描述与实际上游代码不符。

现有 `go test ./...` 全部通过，`go vet ./...` 无输出，但测试用例没有覆盖上述关键缺失，且部分测试把有意差异固化为期望行为。

---

## 2. 核验范围与方法

### 2.1 基线版本

| 项目 | 内容 |
| --- | --- |
| 上游仓库 | `https://github.com/netptop/siteproxy` |
| 基线版本 | v2.7.11 |
| 基线 commit | `c39ee16` |
| Node 模式入口 | `bundle.mjs` |
| 客户端注入脚本 | `bundle.mjs` 内嵌字符串，反混淆后约 6.8 万字节 |
| Service Worker | Node 服务动态拼接返回，反混淆后约 320 行 |

### 2.2 反混淆

使用 webcrack 对 `bundle.mjs` 进行字符串数组还原、控制流还原、反混淆和格式化，得到可读的服务端逻辑、客户端运行时和 Service Worker。关键上游函数对应关系如下。

| 上游函数/变量 | 作用 | Go 对应实现 |
| --- | --- | --- |
| `_0x41151c` | 路由、token 解析、越界路径修复、query 还原 | `当前工作区/server.go:94`、`当前工作区/urlmap.go:39` |
| `_0x27494d` | 主代理中间件 | `当前工作区/proxy.go:60`、`当前工作区/proxy.go:175` |
| `_0x43f3ca` | 响应头与响应体处理入口 | `当前工作区/proxy.go:335` |
| `_0x4b0bf2` | 文本响应重写、字符集、注入 | `当前工作区/proxy.go:562` |
| `_0x531801` | HTML/CSS/JS URL 与标识符重写 | `当前工作区/rewrite.go:121` |
| `_0x51f5a2` | JS 字符串/注释/正则感知的标识符替换 | `当前工作区/jsident.go:95` |
| `_0x4898df` | srcset 候选解析 | `当前工作区/rewrite.go:322` |
| `_0x4f39c1` | Set-Cookie 隔离 | `当前工作区/headers.go:300` |
| `_0x5edbe3` | 请求头过滤 | `当前工作区/headers.go:58` |
| `_0x2ab74b` | 响应头过滤与 Set-Cookie 拆分 | `当前工作区/headers.go:112` |
| `_0x75fef1` | Service Worker 响应 | `当前工作区/server.go:362`、`当前工作区/assets/service-worker.tmpl.js` |
| `_0x4f393b` | 客户端注入脚本 | `当前工作区/assets/injected.tmpl.js` |

### 2.3 差分测试

构造 mock 上游，同时运行：

- 原版：Node 22.22.0，`node bundle.mjs`，监听 5606。
- 当前实现：Go 构建产物，监听 5607。
- 两者使用相同的 `proxy_url = http://proxy.test` 和 `token_prefix = /user22334455/`，排除链接前缀差异。
- mock 上游覆盖 HTML、CSS、JS、重定向、Cookie、GBK、gzip、brotli、deflate、206、304、空响应、无 Content-Type、超长 Cookie 等场景。
- 对响应体去除注入脚本后逐字节比较，对关键响应头逐项比较。

### 2.4 现有质量门禁

```bash
cd /workspace
go test ./...
go vet ./...
go build -o /tmp/diff/siteproxy-go .
```

结果：测试全部通过，静态检查无输出，构建成功。

---

## 3. 问题汇总

| 编号 | 严重级别 | 类型 | 问题 | 实测结果 |
| --- | --- | --- | --- | --- |
| P-01 | 高 | 未移植 | `telegram.org` URL 重写豁免缺失 | 原版不重写，Go 全部重写 |
| P-02 | 高 | 未移植 | `google.com` 内联 integrity 清理缺失 | 原版删除，Go 保留 |
| P-03 | 中高 | 未移植 | 路径中间 token 的越界修复缺失 | 原版 200，Go 404 |
| P-04 | 中 | 未移植 | query 中的代理 URL 未还原 | 上游收到 URL 不同 |
| P-05 | 中 | 有意差异 | 上游硬编码黑名单未移植 | 原版 404，Go 200 |
| P-06 | 中 | 重写差异 | 客户端运行时缺少 Location 代理兜底及若干兼容全局 | 页面脚本兼容性风险 |
| P-07 | 中 | 差异 | Service Worker 注册与鉴权策略不同 | 旧注册复用/升级行为不同 |
| P-08 | 低中 | 差异 | 相对 URL、query-only、fragment 解析策略不同 | 输出不同，Go 更符合 URL 语义 |
| P-09 | 低中 | 差异 | 压缩、HSTS、Cookie 属性、Refresh/Link、206、OPTIONS 等 | 多数已在 README 记录 |
| P-10 | 中 | 文档 | README 多处与上游实际代码不符 | 见第 5 节 |

---

## 4. 详细发现

### P-01 `telegram.org` URL 重写豁免缺失（高）

**上游行为**

上游在 `_0x531801` 中先判断目标 host 是否包含 `telegram.org`：

- host 列表：`_0x542024 = ["telegram.org"]`，反混淆源码第 2110 行。
- 判断函数：`_0x461d75`，第 2111 行。
- 命中后跳过整个 URL 重写分支：第 2402 行。

也就是说，原版对 Telegram Web 页面**不做任何 href/src/srcset/style url() 等 URL 重写**，只做标识符重写和 integrity 清理。

**当前实现**

`当前工作区/rewrite.go:121` 的 `rewriteHTMLWithCharset` 与 `当前工作区/rewrite.go:429` 的 `rewriteCSS` 都没有目标 host 判断，任何站点都会执行完整 URL 重写。

**实测**

使用可解析到 127.0.0.1 的 host 别名：

```text
telegram.org.127.0.0.1.nip.io:5700
```

请求同一 HTML 后：

| 原版 | Go |
| --- | --- |
| `href="https://example.com/a?b=1#c"` | `href="http://proxy.test/user22334455/https/example.com/a?b=1#c"` |
| `href="//cdn.example.com/lib.js"` | `href="http://proxy.test/user22334455/https/cdn.example.com/lib.js"` |

**影响**

README 宣称 WebSocket 支持 Telegram Web，但页面 URL 被全面重写后，Telegram Web 的静态资源和运行时行为会偏离上游设计。该差异未被记录，判断为移植遗漏。

**建议**

在 HTML/CSS 重写入口增加目标 host 判断，命中 `telegram.org` 时跳过 URL 重写，仅保留标识符重写与 integrity 清理，并补充对应用例。

---

### P-02 `google.com` 内联 integrity 清理缺失（高）

**上游行为**

上游存在 Google 专用替换表：

- 定义：`_0x1afa37`，反混淆源码第 2314 行。
- 应用：第 2381 行，仅在 host 包含 `google.com` 且未跳过 URL 重写时执行。

正则：

```js
/;\w+?\.integrity='sha.+?';/
```

替换为 `;`。

**当前实现**

当前 Go 只在 `当前工作区/rewrite.go:178` 清理 `<script>`、`<link>` 标签上的 `integrity`/`crossorigin` 属性，没有处理内联 JS 中对 `integrity` 属性的赋值。

**实测**

mock 返回：

```html
<script>var s=document.createElement('script');s.integrity='sha256-abc';</script>
```

使用 `google.com.127.0.0.1.nip.io:5700` 访问：

| 原版 | Go |
| --- | --- |
| 输出中 `s.integrity='sha256-abc';` 被删除 | 原样保留 |

**影响**

Google 页面可能通过内联脚本给动态创建的脚本设置 SRI。经代理重写后的脚本内容已变化，SRI 校验会失败，导致脚本被浏览器拒绝执行。该差异未被记录，判断为移植遗漏。

**建议**

在 HTML 重写阶段对 `google.com` 执行同样的 `integrity` 赋值清理；或在客户端补齐对 `integrity` 属性赋值/`setAttribute` 的拦截。需同时增加回归测试。

---

### P-03 路径中间 token 的越界修复缺失（中高）

**上游行为**

`_0x41151c` 在 pathname 中调用 `indexOf(token_prefix)`：

- 反混淆源码第 3597-3611 行。
- 只要 token 出现在任意位置，就截取 token 之后的 `proto/host/path`，重建为合法的代理 URL 后继续处理。

**当前实现**

`当前工作区/server.go:214` 的 `repairOutOfScope` 只在以下两种情况修复：

1. Referer 以 `{proxy_url}{token_prefix}` 开头。
2. Cookie 中存在合法的 `proxy_real_protocol` / `proxy_real_host`。

没有扫描 pathname 中的 token。

**实测**

```text
/foo/user22334455/http/127.0.0.1:5700/basic
```

| 原版 | Go |
| --- | --- |
| 200，请求被修复并代理到 `/basic` | 404 |

**影响**

当页面自行拼接根路径、Service Worker 未接管、或代理链接被二次编码时，路径中可能出现 token 但不在开头。此时 Go 会直接 404，而原版会尝试修复。该差异未被 README 记录。

**建议**

在 `repairOutOfScope` 中增加 pathname 内 token 扫描，并按与上游一致的规则重建目标；注意复用现有的 `isPlausibleHost` 校验，避免引入 cookie/path 注入风险。

---

### P-04 query 中的代理 URL 未还原（中）

**上游行为**

`_0x2a0626`（反混淆源码第 3656 行）会在 query 中匹配：

```text
{proxy_url}{token_prefix}(http|https)/host
```

并替换为 `http(s)://host`。该逻辑在 `_0x41151c` 第 3646 行调用。

**当前实现**

`当前工作区/proxy.go:175` 的 `buildUpstreamRequest` 直接使用 `target.RawQuery`，没有对 query 内容做代理 URL 还原。

**实测**

请求：

```text
/user22334455/http/127.0.0.1:5700/echo-url?u=http://proxy.test/user22334455/https/example.com/x
```

| 原版上游收到 | Go 上游收到 |
| --- | --- |
| `/echo-url?u=https://example.com/x` | `/echo-url?u=http://proxy.test/user22334455/https/example.com/x` |

**影响**

带 `redirect=`、`url=`、`return_url=`、`next=` 等参数的登录、支付、跳转链路会把代理 URL 透传给目标站，目标站随后可能跳回错误地址。

**建议**

在构造上游 URL 前对 query 做与上游一致的还原，并增加包含嵌套代理 URL 的测试。

---

### P-05 上游硬编码黑名单未移植（中）

**上游行为**

上游硬编码过滤列表：

```js
["telegram.org/service_worker.js", "elcomercio.pe", "exchangebank.com"]
```

定义于反混淆源码第 2120 行，实际检查位于第 3272 行的主中间件入口。命中后直接进入后续处理器，最终返回 404。

**当前实现**

`当前工作区/config.go:155`、`当前工作区/config.go:188` 只匹配用户显式配置的 `blocked_sites`，没有内置任何域名。

**实测**

mock 对以下路径返回 200：

```text
/elcomercio.pe/x
/telegram.org/service_worker.js
/exchangebank.com
```

| 路径 | 原版 | Go |
| --- | --- | --- |
| `/elcomercio.pe/x` | 404 | 200 |
| `/telegram.org/service_worker.js` | 404 | 200 |
| `/exchangebank.com` | 404 | 200 |

**说明**

README 将该行为描述为“仅显式配置生效，不内置任何域名”，属于有意差异，但没有说明上游实际上存在硬编码列表。需要明确产品决策：要么移植三条内置规则，要么在 README 中完整披露。

---

### P-06 客户端运行时差异（中）

`当前工作区/assets/injected.tmpl.js` 是重写实现，不是逐函数移植。以下上游行为缺失或不同：

| 上游行为 | 上游位置（反混淆注入脚本） | 当前实现 |
| --- | --- | --- |
| `window.___location` 是 `Proxy`，未定义属性回落到原生 `window.location` | 第 705 行 | 普通对象，缺少原生属性兜底，例如 `location.ancestorOrigins` 为 `undefined` |
| 暴露 `window.delay` | 第 1375 行 | 未暴露 |
| 暴露 `window.traverseAndModifyNode` | 第 1133 行附近 | 未暴露 |
| 暴露 `window.siteproxyAttributeChanged` / `debouncedAttributeChanged` | 脚本尾部 | 未暴露 |
| 暴露 `window.removeProxyPrefix` | 脚本尾部 | 未暴露 |
| 暴露 `window.cookieInterceptionRan` | 第 1259 行 | 未暴露 |
| 顶部提示条 key 为 `topBarLastShown`，关闭时才写入，并调整 `body.marginTop` | 第 1377-1411 行 | key 为 `siteproxy_topbar_last_shown`，展示即写入，不调整 margin |
| 加载时若目标根路径缺少结尾 `/`，自动补齐 | 第 1484-1490 行 | 未实现 |
| Bing `#b_content` 可见性修复 | 第 1492-1504 行 | 未实现 |
| `requestStorageAccessFor` 拒绝使用 `NotAllowedError` | 第 800 行 | 使用 `SecurityError` |
| 拒绝字符串 `"undefined"`、`"null"` 作为 location 值 | `_0x1019e3` | 只拒绝 null 和普通对象 |

这些差异多数属于兼容性边角，不影响常规浏览；但 `___location` 缺少原生属性兜底、缺少若干 window 兼容全局，可能影响依赖这些行为的页面脚本。

**建议**

优先补齐 `___location` 的 Proxy 兜底与常用兼容全局；顶部提示条、根路径补斜杠属于低风险对齐项。

---

### P-07 Service Worker 注册与鉴权差异（中）

**上游**

- 注册 URL：`/siteproxy_service_worker.js?proxy_real_protocol=...&proxy_real_host=...&v=3`。
- 服务端 `_0x75fef1` 只要求 `proxy_real_host` 存在，不校验 token。
- Service Worker 脚本使用明文 token_prefix 参与路径判断。

**当前实现**

- 注册 URL 额外携带 `token_prefix`，版本为 `v=4`。
- `当前工作区/server.go:362` 强制校验 `token_prefix` 与配置一致，匿名请求返回 404。
- 注册作用域显式传入 `(proxyBasePath || '') + '/'`。

**影响**

- 安全性优于上游，可避免未认证方读取包含 token 的 Worker。
- 但升级场景下，浏览器中已有的 v3 注册没有 `token_prefix` 查询参数，Go 服务端会对其更新请求返回 404。当前客户端会通过 `registrationMatches` 重新注册新 Worker，一般可自愈，但升级瞬间的行为与上游不同。
- 若页面或其他工具直接请求旧版 SW URL，会得到 404。

该差异未在 README 的差异列表中明确说明。

---

### P-08 相对 URL 解析策略不同（低中）

上游服务端对相对 URL 的处理是**一律按站点根目录锚定**：

- `_0xb27ab3`：相对路径直接拼成 `{proxy_prefix}{proto}/{host}/{url}`。
- 因此 `?page=2` 被重写为 `{site}/?page=2`，`#frag` 被重写为 `{site}/#frag`。

当前 Go 在 `当前工作区/urlmap.go:167` 的 `RewriteWithBase` 中按目标站当前路径解析：

- `?page=2` -> 当前路径 + `?page=2`。
- `#frag` -> 保持不变。
- `../up` -> 在目标 URL 空间解析。

**实测**

| 输入 | 原版 | Go |
| --- | --- | --- |
| `?page=2` | `.../127.0.0.1:5700/?page=2` | `.../127.0.0.1:5700/basic?page=2` |
| `#frag` | `.../127.0.0.1:5700/#frag` | `#frag` 原样 |
| `rel/path` | `.../{host}/rel/path` | 按当前目录解析 |

**评价**

Go 的行为更符合浏览器 URL 解析语义，但与原版输出不一致。`当前工作区/regression_test.go:38` 明确把 Go 行为固化为期望值，说明这是有意的改进，但 README 未列出该差异。

---

### P-09 服务端与响应处理差异（低中）

以下差异大多已在 README 的差异列表中提及，汇总如下。

| 项目 | 上游 v2.7.11 | 当前 Go | 评价 |
| --- | --- | --- | --- |
| `Accept-Encoding` | 仅 `gzip` | `gzip, deflate, br` | Go 支持更广，非忠实 |
| 响应压缩 | 解码后按原编码重新压缩 | 解码后以 identity 返回 | 语义等价，字节不同 |
| HSTS | 原样转发 | 剥离 | 安全增强 |
| COOP/COEP/CORP/Permissions-Policy/Alt-Svc | 转发 | 剥离 | 安全增强 |
| `Refresh` / `Link` | 原样转发 | 重写为代理 URL | 安全增强 |
| Set-Cookie `Domain` | 改写为代理 host | 直接删除，host-only | 隔离更严格 |
| Set-Cookie `Secure` / `SameSite` | 保留 | HTTP 下删除 Secure，`SameSite=None` 改 Lax | 部署适配 |
| 协议相对 `Location: //host/x` | 生成错误的双斜杠路径 | 生成正确的 `https/host/x` | Go 修复了上游 bug |
| 206 Partial Content | 当完整文档重写 | 原样流式透传 | Go 修复了上游 bug |
| 损坏 gzip | 500 | 回放已消费字节后透传 | Go 更健壮 |
| zstd 等未知编码 | 解码失败后产生乱码/500 | 原样透传 | Go 更健壮 |
| 空 Content-Type | `Response` 补 `text/plain` | 不补 | Go 行为不同 |
| 304 | 清空正文 | 不处理正文 | 上游代码清空 204/304 |
| OPTIONS | 500 | 200 并返回内容 | 行为不同，Go 未必更正确 |
| GET 请求体 | 不转发 | 转发 | 行为不同 |
| 重写体大小上限 | 无 | 16 MiB | Go 新增保护 |
| `crossorigin` 属性 | 仅删 `integrity` | 同时删除 `crossorigin` | Go 额外 |
| `@charset` | 不重写 | 改写为 UTF-8 | Go 修复乱码 |
| meta CSP | 不处理 | 剥离 | Go 安全增强 |

这些差异不全是缺陷，但说明当前项目是“行为兼容的改进版”，不是严格意义上的等价移植。

---

## 5. README 与事实不符之处

| README 表述 | 实际情况 |
| --- | --- |
| “原版 node 模式不内置首页与 Service Worker（仅 CF 模式提供）” | 首页确实不内置；但 Node 模式内置并注册 Service Worker，`_0x75fef1` 会返回 `/siteproxy_service_worker.js`，实测原版返回 200 |
| “空 Content-Type：两版都不补响应头” | 原版会因 `Response` 使用字符串体而补出 `text/plain; charset=UTF-8`，Go 不补 |
| “`x-siteproxy-body-headers`：原版 node 会把该内部头原样转发给上游” | 原版 `_0x5edbe3` 会剥离所有 `siteproxy*` 前缀头，不会转发 |
| “非重定向 3xx：300/304/305/306/309 保留正文” | 原版 `_0x43f3ca` 对 204 和 304 都会清空正文 |
| “Accept-Encoding: gzip, deflate, br”作为双方共同行为 | 原版 Node 只发送 `gzip` |
| “被墙站点黑名单：node 模式忽略 blocked_sites，Go 仅显式配置生效” | 上游忽略 `blocked_sites` 字段，但硬编码了 `telegram.org/service_worker.js`、`elcomercio.pe`、`exchangebank.com` 三条规则 |
| “五轮逐请求差分审计……逐字节比对到与 node 模式一致” | Telegram 豁免、Google integrity、路径内 token 修复、query 还原、硬编码黑名单均未被覆盖，且存在多字节输出差异 |

---

## 6. 已验证一致的部分

以下部分经过差分验证，行为基本一致：

- 首页 `assets/index.html` 与上游 `index_www.netptop.com.html` 逐字节一致。
- 路径式反代 URL 格式 `{token_prefix}{http|https}/{host}/{path}`。
- 常规绝对 URL、root-relative URL、协议相对 URL 的重写结果。
- srcset/imagesrcset 逗号解析，包括 `a.png 1x,b.png 2x` 与 URL 内含逗号的情况。
- 内联脚本与外链 JS 中的 `location`、`navigator.serviceWorker`、`document.requestStorageAccessFor` 标识符重写。
- GBK/GB18030 等字符集转码与 `<meta charset>` 更新。
- 常规 301/302 重写 `Location`，相对 Location 处理。
- Set-Cookie 路径隔离与超长 Cookie 431 自愈。
- 请求头 `siteproxy*`、`cf-*`、`x-forwarded*` 过滤。
- Cookie 中 `proxy_real_*` 控制 Cookie 的上游过滤。
- 普通 gzip 响应的解码与文本重写。
- 空响应体不注入、不追加控制 Cookie。
- 控制 Cookie 只在被重写的文本响应上追加。

---

## 7. 复现记录

### 7.1 构建与测试

```bash
cd /workspace
go test ./...
go vet ./...
go build -o /tmp/diff/siteproxy-go .
```

结果：测试通过，静态检查无输出，构建成功。

### 7.2 P-01 复现

```bash
H='telegram.org.127.0.0.1.nip.io:5700'
curl -sS "http://127.0.0.1:5606/user22334455/http/$H/basic"
curl -sS "http://127.0.0.1:5607/user22334455/http/$H/basic"
```

观察 `href` 值：原版保持真实 URL，Go 改写为代理 URL。

### 7.3 P-02 复现

mock 返回：

```html
<script>var s=document.createElement('script');s.integrity='sha256-abc';</script>
```

请求 `google.com.127.0.0.1.nip.io:5700/googletest`：

- 原版：`s.integrity` 赋值被删除。
- Go：保留。

### 7.4 P-03 复现

```bash
curl -sS -o /dev/null -w '%{http_code}\n' \
  'http://127.0.0.1:5606/foo/user22334455/http/127.0.0.1:5700/basic'
curl -sS -o /dev/null -w '%{http_code}\n' \
  'http://127.0.0.1:5607/foo/user22334455/http/127.0.0.1:5700/basic'
```

结果：原版 200，Go 404。

### 7.5 P-04 复现

```bash
curl -sS 'http://127.0.0.1:5606/user22334455/http/127.0.0.1:5700/echo-url?u=http://proxy.test/user22334455/https/example.com/x'
curl -sS 'http://127.0.0.1:5607/user22334455/http/127.0.0.1:5700/echo-url?u=http://proxy.test/user22334455/https/example.com/x'
```

结果：原版上游收到 `u=https://example.com/x`，Go 保留完整代理 URL。

### 7.6 P-05 复现

mock 对以下路径返回 200：

```text
/elcomercio.pe/x
/telegram.org/service_worker.js
/exchangebank.com
```

原版均返回 404，Go 均返回 200。

---

## 8. 结论与建议

### 8.1 结论

当前项目完成了 siteproxy 的核心反向代理与内容重写能力，常规网页浏览场景可以工作；但它属于**带安全增强和行为修正的 Go 重写版**，不是与 v2.7.11 严格等价的移植。

如果验收标准是“忠实移植”，当前状态不达标，主要阻塞项是 P-01 至 P-04。若验收标准是“兼容并改进”，则需要把 P-05 至 P-10 完整写入文档，并修正 README 中与上游实际代码不符的描述。

### 8.2 建议优先级

1. **P-01 Telegram 豁免**：补齐 host 判断，避免 Telegram Web 行为回归。
2. **P-02 Google integrity 清理**：补齐内联 integrity 赋值清理，避免 Google 页面脚本 SRI 失败。
3. **P-03 路径内 token 修复**：补齐越界路径扫描，复用现有 host 校验。
4. **P-04 query 内代理 URL 还原**：补齐查询串还原，覆盖登录跳转链路。
5. **P-05 黑名单决策**：移植上游三条硬编码规则，或明确记录为产品差异。
6. **P-06 客户端兼容**：优先补齐 `___location` Proxy 兜底与常用兼容全局。
7. **P-10 文档修正**：按第 5 节逐条修正 README，避免后续维护者误判行为基线。
8. 为 P-01 至 P-04 增加差分回归测试，防止再次遗漏。

---

## 9. 附录：上游与当前实现关键差异速查

| 行为 | 上游 v2.7.11 | 当前 Go | 是否已记录 |
| --- | --- | --- | --- |
| Telegram URL 重写 | 跳过 | 执行 | 否 |
| Google 内联 integrity | 删除 | 保留 | 否 |
| 路径内 token 修复 | 支持 | 不支持 | 否 |
| query 内代理 URL 还原 | 支持 | 不支持 | 否 |
| 硬编码黑名单 | 3 条 | 无 | 部分 |
| `___location` 原生属性兜底 | Proxy 兜底 | 无 | 否 |
| SW 注册 URL | v3，无 token | v4，带 token | 否 |
| SW 匿名访问 | 可返回脚本 | 404 | 否 |
| 相对 URL、query-only、fragment | 站点根锚定 | 按当前路径解析 | 否 |
| 空 Content-Type | 补 text/plain | 不补 | 是（描述不准确） |
| HSTS | 转发 | 剥离 | 是 |
| Refresh/Link | 不重写 | 重写 | 是 |
| 206 | 重写 | 透传 | 是 |
| OPTIONS | 500 | 200 | 否 |
| GET 请求体 | 不转发 | 转发 | 否 |
| 16 MiB 重写上限 | 无 | 有 | 否 |
| 压缩 | gzip 重压 | 解码后 identity | 部分 |

---

## 10. 第二轮深度对比（2026-11-17 晚）

第一轮修复后再次以双实例差分（Node :5606 / Go :5607，mock 上游 :5701）复查了路由、方法、请求头、Cookie、字符集、压缩、HTML/CSS/JS 边角、客户端运行时与 Service Worker。结论：P-01 至 P-04 已修复，P-06 修复一部分；另发现 8 组新差异。

### 10.1 第一轮问题状态

| 编号 | 状态 | 说明 |
| --- | --- | --- |
| P-01 Telegram 豁免 | 已修复 | `rewriteHTMLForHost` 跳过 URL 重写，保留标识符/integrity 处理 |
| P-02 Google integrity | 已修复 | 内联赋值在脚本抽取前删除 |
| P-03 路径内 token | 已修复 | `targetFromPathToken` 支持 `/foo/{token}/http/host/x` 与二次 token |
| P-04 query 还原 | 已修复 | 且已改为只还原第一个匹配，与上游 `String.replace` 一致 |
| P-05 硬编码黑名单 | 保留差异 | 仍只按 `blocked_sites` 生效，README 已披露 |
| P-06 客户端运行时 | 部分修复 | 增加 Proxy 兜底、`window.delay`、`removeProxyPrefix`、`traverseAndModifyNode`、`cookieInterceptionRan`、`NotAllowedError`；顶栏/根路径/Bing 等仍未完全对齐 |
| P-07 SW 鉴权 | 保留差异 | v4 + token 校验，安全增强 |
| P-08 相对 URL | 保留差异 | 按当前路径解析，优于上游站点根锚定 |
| P-09 响应处理 | 部分差异 | 见 10.2 |
| P-10 README | 已修正 | 空 Content-Type、304、SW、内部头、Telegram/Google 等已按实测改写 |

### 10.2 新增发现

| 编号 | 级别 | 差异 | 实测 / 结论 |
| --- | --- | --- | --- |
| P-11 | 中 | query 还原替换了所有代理 URL | 上游无 `g` 标志只还原第一个；Go 原用 `ReplaceAllString`。已修复并加测试 |
| P-12 | 中 | Cookie 生命周期被延长 | 上游删除 `Max-Age=60`（变会话 Cookie）；Go 原把它改成 `Max-Age=1800`。已改为“上限”语义：短于 1800 的 `Max-Age`/`Expires` 原样保留，超长才截断 |
| P-13 | 低 | 无 `window.Proxy` 兜底 | 不支持 Proxy 的引擎会因 `new Proxy` 抛错跳过全部覆盖。已按上游加入降级 |
| P-14 | 低 | DOMException 类型不同 | 上游 `requestStorageAccessFor` / `serviceWorker` 均为 `NotAllowedError`；Go 原为 `SecurityError`。已对齐 |
| P-15 | 中 | 附件响应处理不同 | 上游把 `Content-Disposition: attachment` 的 HTML 当普通页面重写并注入；Go 原样透传、不注入、不追加控制 Cookie。Go 行为更安全，已写入 README |
| P-16 | 中 | 超大响应头策略不同 | 上游 >4096 字节的非 Set-Cookie 头在无 SW 时直接丢弃，有 SW 用 `SITEPROXYHDR` 帧塞入响应体；Go 直接转发大响应头，未实现拆包。Go 无需 CF 头限制，已披露 |
| P-17 | 低 | 字符集输出不同 | 额外注入 `<meta Content-Type charset=UTF-8>` 是 Go 的既有设计，保留；声明 UTF-8 的非法字节已改为替换为 U+FFFD（连续非法字节合并为一个，数量可能与上游 `TextDecoder` 不同） |
| P-18 | 低 | 可见文本 URL 处理不同 | 上游正则改写 `<p>`/`<textarea>`/`<pre>` 文本中的 `"https://..."`；Go 只改写标签属性，保留可见文本。Go 更精确 |
| P-19 | 低 | 路径内 token 的 host-only 场景 | 已修复：`/foo/{token}/http/host` 与上游一样返回 404，只在 host 后还有路径时修复 |
| P-20 | 低 | SW 兼容面 | 已补齐 `PROXY_URL_HOST_MAP` 消息通道（30 秒清理）；超大响应头拆包按设计不实现（见 10.5） |

### 10.3 复现要点

查询串：

```bash
# 修复前 Go 会把 a、b、c 全部还原；修复后与 Node 一致，只还原 a
curl -sS 'http://127.0.0.1:5607/user22334455/http/127.0.0.1:5701/echo?a=http://proxy.test/user22334455/https/a.test/x&b=http://proxy.test/user22334455/https/b.test/y'
```

Cookie：

```bash
# Node: short/long 的 Max-Age 被删除；expsoon/expfar 被改为 Max-Age=1800
# Go:   short 保留 Max-Age=60，long 截断为 1800，expsoon 保留 Expires，expfar 截断为 1800
curl -sS -D - -o /dev/null 'http://127.0.0.1:5607/user22334455/http/127.0.0.1:5701/cookie3'
```

附件：

```bash
# Node 重写 <a href>；Go 原样返回
curl -sS 'http://127.0.0.1:5607/user22334455/http/127.0.0.1:5701/download'
```

### 10.4 质量门禁

```bash
cd /workspace
gofmt -w proxy.go headers.go fix_test.go audit_fixes_test.go
node --check assets/injected.tmpl.js
go test ./...
go vet ./...
go build -o /tmp/diff/siteproxy-go .
```

结果：测试通过，vet 无输出，构建成功；重启 Go 实例后 query 与 Cookie 差分符合预期。

### 10.5 已按设计关闭的差异

以下差异经确认保留 Go 的安全/简化行为，不再作为待办：

- P-15 附件重写：原版会改写 `Content-Disposition: attachment` 的 HTML，Go 原样透传，避免改变下载文件。
- P-16 超大响应头拆包：4 KB 限制与 `SITEPROXYHDR` 帧是 Cloudflare 约束；Go 的 HTTP 栈直接转发大响应头。
- P-17 额外 charset meta：Go 统一注入 UTF-8 声明是既有设计；非法 UTF-8 已改为替换。
- P-18 可见文本 URL：原版会改写 `<p>`/`<textarea>`/`<pre>` 中的可见 URL，Go 只改标签属性。
- Cookie `Max-Age`：原版会丢失站点短生命周期设置，Go 采用上限语义。
- SW 鉴权：原版无 token 校验并复用旧注册，Go 的 v4 + token 校验更安全。

其余待办仅剩第一轮的 P-05（内置黑名单产品决策）和 P-06 顶栏/根路径/Bing 等低风险兼容项。
