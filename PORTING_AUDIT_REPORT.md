# siteproxy-go 移植一致性核验报告

- 报告日期：2026-11-17
- 核验对象：当前工作区 `siteproxy-go`
- 对照基线：`netptop/siteproxy` v2.7.11（commit `c39ee16`）
- 核验方式：源码反混淆比对 + 双实例差分测试 + 现有测试/静态检查
- 核验结论：核心链路大体完成，但**不属于忠实移植**；存在 4 项未记录的功能缺失，其中 2 项会直接影响真实站点
- 轮次说明：第一轮见 §1-§9，第二轮见 §10，第三轮见 §11-§12，第四轮见 §13（P-30…P-36）与 §14 修复记录，第五轮见 §15（P-37…P-40）与 §16 修复记录

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

也就是说，原版对 Telegram Web 页面**不做任何 href/src/srcset/style url() 等 URL 重写**；§14 复核进一步确认，HTML 分支连标识符重写和 integrity 清理也不做。

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
| P-01 Telegram 豁免 | 已修复；§14 再修正 | `rewriteHTMLForHost` 对 Telegram 走独立分支，跳过 URL/标识符/integrity/CSP 处理 |
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

---

## 11. 第三轮深度对比（2026-11-17 晚）

本轮只做审查，未修改实现。基线仍为 Node v2.7.11（:5606），Go 为当前构建（:5607），
临时对比实例使用相同 `proxy_url=http://proxy.test`。

### 11.1 新增发现

以下 9 项已在第三轮修复中处理，具体变更与验证见 §12；本节保留审查时的原始描述。

| 编号 | 严重度 | 类型 | 结论 |
| --- | --- | --- | --- |
| P-21 | 高 | 安全/配置 | `token_prefix` 为 `"/"`、`"//"` 或 `""` 时语义与上游完全不同：上游统一改为 `/default/` 并设置 `default_password`，Go 把 `"/"` 当作免密码、把 `""` 当作 `/user22334455/`。已有 Node 配置直接切到 Go 时，`"/"` 会静默变成公开代理 |
| P-22 | 中 | 响应头泄漏 | Go 未剥离 `Permissions-Policy-Report-Only`；上游 `_0x14b59b` 会剥离，实测 Node 不返回该头，Go 原样返回。Go 的 `stripResponseHeaders` 只包含 `Permissions-Policy`，漏了 `-Report-Only` |
| P-23 | 中低 | 客户端兼容 | Go 未暴露上游的 `getRefererFromLocation`、`getHostFromProxyPrefixedURL`、`getPathnameFromProxyPrefixedURL`、`getProtocolFromProxyPrefixedURL`、`setProtocolFromProxyPrefixedURL`、`siteproxyAttributeChanged`、`debouncedAttributeChanged`、`siteproxyRegReplacementFallback`、`removeProxyPrefixFallback`；fetch/XHR 也未发送上游的 `siteproxy-target-protocol/host`、`siteproxy-real-referer`、`siteproxy-window-location-pathname` |
| P-24 | 中低 | 客户端逻辑 | `fetch(new Request(url), init)` 在 URL 需要重写时，Go 用 `new Request(newUrl, input)` 丢掉了调用方传入的 `init`（method/body/signal 等）；上游使用 `new Request(rewritten, init || input)` |
| P-25 | 低 | 客户端 UI | 顶栏 key 为 `siteproxy_topbar_last_shown`（上游 `topBarLastShown`）；Go 在显示时立即写 key，上游只在点击关闭时写；Go 未给 body 加 `marginTop`，固定顶栏会覆盖页面内容；文案也缺少 `target="_blank"` 与 `for details !!!` |
| P-26 | 低 | 客户端逻辑 | `isNotURL` 未拒绝字符串 `"undefined"`、`"null"`、`"[object Object]"`；上游会拒绝，Go 会把这些字符串当作相对 URL 导航 |
| P-27 | 低 | 客户端逻辑 | Go 只改写 `history` 实例属性；上游改写 `History.prototype.pushState/replaceState`。页面缓存 `const p = history.pushState` 或通过原型调用时，Go 不会拦截 |
| P-28 | 低 | 响应清理 | 304 与“非文本体重定向”响应，Go 保留 `Content-Length`/`Content-Encoding`；上游对 204/304/301/302/303/307/308 统一删除这三个头 |
| P-29 | 低 | 文档 | README 仍称 `token_prefix=""` 回退 `/user22334455/`、`"/"` 免密码；称 Connection 命名头“与原版一致地转发”；称 JS 会重写“引号包裹的绝对 URL 字面量”。这三条与上游实测或当前实现不符 |

### 11.2 关键证据

**P-21 令牌语义**

上游 `_0x500ad5`（`/tmp/deob/deobfuscated.js:2047`）对 `"/"`、`"//"`、`""` 统一执行：

```js
token_prefix = "/default/";
default_password = true;
```

临时 Node 实例（`token_prefix="/"` 与 `""`，端口 5610/5611）访问 `/`、`/default/` 均返回
`302 Location: http://proxy.test/default/https/www.netptop.com`；访问 `/default/http/127.0.0.1:5701/echo` 返回 200。

Go（`token_prefix="/"`，端口 5612）访问 `/` 返回 200 首页，访问 `/http/127.0.0.1:5701/echo` 返回 200；
Go（`token_prefix=""`，端口 5613）实际 token 变成 `/user22334455/`，`/default/` 返回 404。
对应实现为 `config.go` 的 `normalizeTokenPrefix` 与 `noPassword`。

**P-22 响应头泄漏**

临时目标返回 `Permissions-Policy-Report-Only: geolocation=()`：

```text
Node : 无该响应头
Go   : Permissions-Policy-Report-Only: geolocation=()
```

上游剥离集合见 `/tmp/deob/deobfuscated.js` 的 `_0x14b59b`；Go 集合见 `headers.go` 的
`stripResponseHeaders`。

**P-23 客户端兼容面**

`curl -sS http://127.0.0.1:5607/siteproxy-response-injected.js` 中以下字符串出现次数均为 0：

```text
getRefererFromLocation
getHostFromProxyPrefixedURL
getPathnameFromProxyPrefixedURL
getProtocolFromProxyPrefixedURL
setProtocolFromProxyPrefixedURL
siteproxyAttributeChanged
debouncedAttributeChanged
siteproxyRegReplacementFallback
removeProxyPrefixFallback
```

上游对应定义见 `/tmp/deob/injected/deobfuscated.js`；fetch/XHR 的 target 头见同文件
`window.fetch` 与 `XMLHttpRequest.prototype.open` 两段。

**P-24 fetch 初始化参数**

Go 当前逻辑（`assets/injected.tmpl.js` 的 `window.fetch`）：

```js
if (newUrl !== input.url) input = new Request(newUrl, input);
```

需要改成与上游一致：

```js
input = new Request(newUrl, init || input);
```

否则 `fetch(request, { method, body, signal })` 在 URL 被重写时会丢掉 `init`。

**P-25 顶栏**

上游使用 `localStorage.getItem("topBarLastShown")`，只在关闭按钮中写入时间，并执行
`document.body.style.marginTop = bar.offsetHeight + "px"`；Go 使用 `siteproxy_topbar_last_shown`，
在 `show()` 末尾立即写入，且没有设置 body 边距。

**P-28 304/重定向头清理**

上游 `_0x43f3ca` 对 204 和 301/302/303/304/307/308 执行：

```js
body = undefined;
headers.delete("content-length");
headers.delete("content-encoding");
headers.delete("transfer-encoding");
```

Go 的 `proxy.go` 只在重定向分支删除 `Content-Length`，304 分支不删除任何表示头，
非文本体的重定向也保留 `Content-Encoding`。

### 11.3 结论与优先级

P-21 是本轮唯一的“高危安全/配置”问题，优先级高于其余项：它会让沿用的 Node 配置在切换后
从密码保护变成公开代理。建议先做以下决策再改代码：

1. 是否保留 Go 的免密码模式；若保留，改为独立的显式配置（如 `disable_auth`），不再复用 `token_prefix="/"`。
2. 是否实现上游的 `default_password` 首页跳转；至少应把 `"/"`、`"//"`、`""` 映射到 `/default/`，避免配置语义漂移。
3. P-22 只需把 `Permissions-Policy-Report-Only` 加入剥离列表并加一条回归测试。
4. P-23/P-24/P-25/P-26/P-27 属于客户端兼容/浏览器行为，建议在 P-21/P-22 之后统一补齐。
5. P-28 按“严格移植”或“保留更宽容的 Go 行为”做产品决策，并同步 README；P-29 直接修正文档。

本节记录的是第三轮审查当时的状态。对应修复、测试和文档更新见 §12。


---

## 12. 第三轮修复记录（2026-11-17 晚）

第三轮发现已全部落地，未改变第一轮/第二轮已关闭的设计决策。质量门禁：

```bash
cd /workspace
gofmt -w audit5_test.go config.go headers.go main.go proxy.go server.go
node --check assets/injected.tmpl.js
node --check assets/service-worker.tmpl.js
go test ./...
go vet ./...
go build -o /tmp/diff/siteproxy-go .
```

结果：测试通过，vet 无输出，构建成功。

### 12.1 P-21 默认密码与免密模式

- `config.go`：`""`、`"/"`、`"//"` 统一归一化为 `/default/`，并设置 `DefaultPassword`；新增显式 `disable_auth` 配置与 `SITEPROXY_DISABLE_AUTH` 环境变量。`noPassword()` 只认显式 `disable_auth`，不再把 `token_prefix="/"` 当免密。
- `server.go`：`DefaultPassword` 模式下 `/`、`/{token}/` 与非法路径 302 到 `{proxy_url}/default/https/www.netptop.com`，合法的 `/default/http/{host}/...` 仍正常代理。
- 实测（Go 5612，`token_prefix="/"`）：`/` 与 `/default/` 均 302 到 `http://proxy.test/default/https/www.netptop.com`；`/default/http/127.0.0.1:5701/echo` 返回 200。与 Node 5610 对齐。
- 旧配置从 Node 切到 Go 不再把 `"/"` 静默变成公开代理；需要免密必须显式 `disable_auth: true`。

### 12.2 P-22 响应头

- `headers.go` 的 `stripResponseHeaders` 增加 `Permissions-Policy-Report-Only`。
- 新增 `TestPermissionsPolicyReportOnlyStripped`。

### 12.3 P-23/P-24/P-25/P-26/P-27 客户端运行时

`assets/injected.tmpl.js`：

- 新增 `removeProxyPrefixFallback`、`getRefererFromLocation`、`getHostFromProxyPrefixedURL`、`getPathnameFromProxyPrefixedURL`、`getProtocolFromProxyPrefixedURL`、`setProtocolFromProxyPrefixedURL`、`siteproxyRegReplacementFallback`、`siteproxyAttributeChanged`、`debouncedAttributeChanged`。
- fetch/XHR 增加上游的 `siteproxy-target-protocol`、`siteproxy-target-host`、`siteproxy-real-referer`、`siteproxy-window-location-pathname` 请求头。
- `fetch(Request, init)` 改为 `new Request(newUrl, init || input)`，不再丢 `init`。
- `isNotURL` 拒绝字符串 `"undefined"`、`"null"`、`"[object Object]"`。
- 改为 patch `History.prototype.pushState/replaceState`，保留 `History.prototype.___pushState/___replaceState` 原生方法；实例 `history.___pushState` 继续服务 inline bootstrap。
- 顶栏 key 改为 `topBarLastShown`，只在关闭时写 localStorage，插入 body 后设置 `marginTop`，文案补 `target="_blank"` 与 `for details !!!`。

新增 `TestInjectedCompatibilitySurface` 检查公共运行时包含这些兼容符号且不 bake token。

### 12.4 P-28 204/304 与重定向表示头

- `proxy.go` 新增 `stripNoBodyHeaders`：204/304 与响应体重写后的重定向删除 `Content-Length`、`Content-Encoding`、`Transfer-Encoding`。
- net/http 对无体重定向可能自行生成 `Content-Length: 0`，测试允许该框架生成值，但不允许上游原始长度/编码存活。
- 新增 `TestNoBodyAndRedirectHeadersAreStripped`。

### 12.5 P-29 文档

- README 修正 `token_prefix` 默认语义、`default_password` 跳转、`disable_auth`、304/重定向表示头、Connection 命名头、JS 字面量重写、`/{token}/` 根路径和兼容符号列表。
- 新增 `audit5_test.go`，覆盖 P-21、P-22、P-28 和客户端兼容面。

### 12.6 剩余边界

- `Connection` 命名的普通请求头仍按 Go 当前实现转发；原版 Node fetch 的运行时拒绝边界没有完全模拟，已在 README 明确披露。
- P-15/P-16/P-17（额外 charset meta）/P-18 等第一轮已确认的设计差异保持不变。

---

## 13. 第四轮深度对比（2026-11-17 下午）

本轮继续使用 Node v2.7.11（:5606）与当前 Go 构建（:5607，commit `dd33531`）做逐请求差分，
并额外启动三个临时上游验证边角：Telegram HTML（:5702）、17 MiB HTML（:5703）、Google 外部 JS（:5704）。
上游仍以 `/tmp/deob/deobfuscated.js`、`/tmp/deob/injected/deobfuscated.js` 和
`/tmp/deob/sw/deobfuscated.js` 为证据。本轮只做审查，未修改实现。

### 13.1 新增发现

以下问题已在 §14 修复，本节保留审查时状态。

| 编号 | 级别 | 类型 | 结论 |
| --- | --- | --- | --- |
| P-30 | 中低 | 特殊站点重写 | Telegram HTML 的上游分支会跳过整个 `_0x531801`；Go 仍执行内联脚本标识符重写、`integrity`/`crossorigin` 删除，并把 CSS `@charset` 改为 UTF-8 |
| P-31 | 中 | 目标主机兼容 | `isPlausibleHost` 拒绝非 ASCII / 百分号编码的 IDN 主机；浏览器把一个 Unicode 域名链接编码成 `%E4...` 后，Go 直接 404，Node 仍会尝试抓取（502 而非 404） |
| P-32 | 中低 | 超大 HTML | 超过 16 MiB 的 HTML 只注入外部 `siteproxy-response-injected.js`，没有注入 inline bootstrap；外部脚本读取不到 `proxy_url_prefix` / token / 目标 host，客户端运行时实际失效 |
| P-33 | 低 | 静态资源路由 | Node 对所有以 `siteproxy-response-injected.js` 结尾的路径返回脚本，Go 只匹配根路径和 base path；Node 使用 `no-cache, no-store, must-revalidate`，Go 只发 `no-cache` |
| P-34 | 低 | 文档/损坏压缩 | README 声称“损坏 gzip 原样透传”，但只覆盖 `gzip.NewReader` 构造失败；gzip 头合法、正文损坏时 Go 返回 502，Node 返回 500 |
| P-35 | 低 | Google 外部 JS | `renderJS` 对 Google 外部 JS 也执行 `stripGoogleIntegrityAssignments`；上游对 JS 走 `skipUrlRewrite=true`，不会做这项替换 |
| P-36 | 低 | Service Worker | 客户端已发送 `siteproxy-target-protocol/host` 和 `siteproxy-real-referer`，但当前 SW 完全不读取这些头；上游 SW 在 URL 不在 token scope 时用它们恢复目标站点 |

### 13.2 关键证据与复现

**P-30 Telegram HTML**

上游 `_0x4b0bf2` 在 `_0x430b91(host)` 为真（Telegram）时走专用分支，只做字符集处理、
注入和 JS 文件标识符重写，不调用 `_0x531801`；因此 HTML 内的内联脚本、`integrity`、
`crossorigin` 都不改。当前 `rewriteHTMLForHost` 只在 URL 重写步骤前判断
`skipURLRewriteForHost`，之后的 `restoreScriptBodies`、`scriptOrLinkTagRe` 仍会执行。

临时上游返回：

```html
<meta http-equiv="Content-Security-Policy" content="default-src 'none'">
<script>window.location.href='/x'; navigator.serviceWorker.register('/sw');</script>
<script integrity="sha256-x" crossorigin="anonymous" src="https://example.com/s.js"></script>
```

请求 `telegram.org.127.0.0.1.nip.io:5702/x`：

| 内容 | Node :5606 | Go :5607 |
| --- | --- | --- |
| 内联脚本 | `window.location.href` / `navigator.serviceWorker` 原样 | 改为 `window.___location.href` / `navigator.___serviceWorker` |
| 外部 script | `integrity`、`crossorigin` 保留 | 两者均删除 |
| meta CSP | 保留 | 删除 |

**P-31 IDN 主机**

`ParseTarget` 使用 `isPlausibleHost`（`当前工作区/server.go:331`），后者只接受
ASCII 字母数字、`.`、`-`、`_` 和数字端口；`RewriteURL` 却会把 Unicode 主机原样写进代理路径。
直接请求：

```text
/user22334455/https/例え.テスト/
```

| Node :5606 | Go :5607 |
| --- | --- |
| 502（已进入抓取阶段） | 404（路由阶段拒绝） |

这说明 Go 对 IDN 链接的失败发生在路由层，而不是上游 DNS。需要 IDNA ToASCII 转换，
或在 `RewriteURL` 阶段把主机编码为 punycode。

**P-32 超大 HTML**

`当前工作区/proxy.go:466` 的 truncated 分支：

```go
out = injectSnippetBytes(out, injectedScriptTagAt(p.cfg.BasePath()))
```

只插入外部脚本。对 17 MiB HTML 差分：

| 指标 | Node :5606 | Go :5607 |
| --- | --- | --- |
| inline `proxy_url_prefix` 出现次数 | 2 | 0 |
| 外部 runtime 标签次数 | 1 | 1 |

外部 `injected.tmpl.js` 依赖 inline bootstrap 定义
`config_proxy_url` / `config_token_prefix` / `proxy_real_protocol` / `proxy_real_host`
（`当前工作区/assets/injected.tmpl.js:40`）；没有 inline 时这些值全部回退为空，
`proxyRewrite` 无法生成正确的 token 路径。`TestOversizedGBKHTMLKeepsCharsetAndInjectsRuntime`
只检查外部标签存在，因此没有覆盖这个问题。

**P-33 静态资源路由与缓存**

| 请求 | Node :5606 | Go :5607 |
| --- | --- | --- |
| `/siteproxy-response-injected.js` | 200，`Cache-Control: no-cache, no-store, must-revalidate` | 200，`Cache-Control: no-cache` |
| `/foo/siteproxy-response-injected.js` | 200 | 404 |
| `/{token}/foo/siteproxy-response-injected.js` | 200 | 404 |

上游在中间件中直接判断 `req.path.endsWith("siteproxy-response-injected.js")`；
当前 Go 路由只匹配精确路径及 base path（`当前工作区/server.go:110`、`:137`），
`serveStaticJS` 的缓存头也只有 `no-cache`（`当前工作区/server.go:483`）。
这会让旧缓存或非标准路径下的 runtime 请求落到目标站点或 404。

**P-34 头合法但正文损坏的 gzip**

mock `/badgzip` 返回合法的 gzip 头（`1f 8b 08 ...`）加垃圾正文：

| Node :5606 | Go :5607 |
| --- | --- |
| 500 Internal Server Error | 502 Proxy fetch error |

`TestCorruptGzipPassesThroughWithoutHang` 使用 `"notgzip"`，只覆盖
`gzip.NewReader` 构造失败；构造成功后 `io.ReadAll` 报错时，当前 `renderHTML` 返回错误并由
`writeResponse` 转成 502，不会走 README 所说的“回放已消费字节后原样透传”。
README 需要缩小该承诺，或实现读取阶段失败时的整段回放。

**P-35 Google 外部 JS**

临时上游返回 `application/javascript`：

```js
var s=document.createElement('script');s.integrity='sha256-abc';console.log('x');
```

请求 `google.com.127.0.0.1.nip.io:5704/js`：

| Node :5606 | Go :5607 |
| --- | --- |
| 保留 `s.integrity='sha256-abc'` | 删除该赋值 |

上游 `_0x531801` 在 JS 分支以 `skipUrlRewrite=true` 调用，因此 Google 替换表不执行；
当前 `当前工作区/proxy.go:662` 的 `renderJS` 无条件调用
`stripGoogleIntegrityAssignments`。这会改动并非 SRI 失效原因的外部脚本。

**P-36 Service Worker 未消费 target 头**

上游 SW 在目标 URL 不在 token scope 时执行（`/tmp/deob/sw/deobfuscated.js:254-258`）：

```js
_x = headers.get("siteproxy-target-protocol");
host = headers.get("siteproxy-target-host");
referer = headers.get("siteproxy-real-referer");
```

当前 `当前工作区/assets/service-worker.tmpl.js` 没有这三个字符串；它只用
`event.clientId` 的 URL 和 referer，再回落到 `PROXY_CUR_LOCATION`。客户端在
`当前工作区/assets/injected.tmpl.js:603` 已经发送这些头，但 SW 侧没有对应分支，
因此 client URL 和 referer 都不可用时，目标站点上下文会保持旧值或为空。

### 13.3 仍未关闭的历史项

以下不是本轮新发现，但第四轮复核仍然存在：

- **P-06 剩余**：客户端仍未实现目标根路径缺尾斜杠自动补齐和 Bing `#b_content`
  可见性修复；上游位置为 `/tmp/deob/injected/deobfuscated.js:1484-1509`。
- **普通 `/{token}/` 根路径**：Node 302 到 `/{token}/https/www.netptop.com`，Go 返回本地首页；
  README 已将其描述为有意差异，但严格移植标准下仍是行为差。
- **OPTIONS / GET 请求体**：报告 §9 已记录，但 README 差异列表仍未完整列出。
- **硬编码黑名单**、**SW v3/v4 鉴权**、**P-15/P-16/P-17/P-18** 保持既有的有意差异。

### 13.4 建议优先级

1. **P-31 IDN 主机**：这是真实可达性缺陷，浏览器生成的 IDN 链接在 Go 上全部 404。
2. **P-30 Telegram HTML**：按上游对 Telegram 完全跳过 `_0x531801`，仅保留字符集和注入；
   否则 Telegram Web 的内联脚本和 SRI/安全头被无关地改写。
3. **P-32 超大 HTML**：要么注入压缩后的 inline bootstrap 配置，要么明确不注入外部 runtime；
   当前“只注入外部脚本”的状态会让运行时读到空配置。
4. **P-36 SW target 头**：补齐上游分支，否则新加的客户端头是死代码。
5. P-33 / P-34 / P-35：低风险路由、缓存和文档/特殊主机问题，可与相应测试一起修正。

### 13.5 质量门禁

```bash
cd /workspace
go test ./...
go vet ./...
go build -o /tmp/diff/siteproxy-go .
```

结果：测试通过，vet 无输出，构建成功；本轮未修改代码，因此没有新增测试或提交。

---

## 14. 第四轮修复记录（2026-11-17 下午）

P-30 至 P-36 已修复，未改变此前已关闭的设计差异。质量门禁：

```bash
cd /workspace
gofmt -w server.go urlmap.go proxy.go rewrite.go assets.go \
  audit6_test.go audit_fixes_test.go medium_priority_test.go
node --check assets/injected.tmpl.js
node --check assets/service-worker.tmpl.js
go test ./...
go vet ./...
go build -o /tmp/diff/siteproxy-go .
```

结果：测试通过，vet 无输出，构建成功。

### 14.1 P-30 Telegram 特殊分支

- `rewriteHTMLForHost` 在 `skipURLRewriteForHost(targetHost)` 时直接走专用分支：只做字符集 meta 规范化
  和注入，不再提取/恢复内联脚本、不再删除 `integrity`/`crossorigin`、不再删除 meta CSP。
- `renderCSS` 对 Telegram 先返回原始 CSS，不再改写 `@charset` 或 `url()`。
- 实测 Node :5606 与 Go :5607 对同一 Telegram HTML：`window.location.href`、`navigator.serviceWorker`、
  `integrity`、`crossorigin` 均保持原样。

### 14.2 P-31 IDN 目标主机

- 新增 `normalizeTargetHost`：对原始 Unicode 和百分号编码的 UTF-8 主机先做路径解码，拒绝解码后出现的
  `/ \ ? # @`、空白和 NUL，再通过 `golang.org/x/net/idna` 转为 punycode；ASCII 主机保留原有严格校验。
- `ParseTarget`、`repairOutOfScope`、`unrewriteReferer`、`serveServiceWorker` 统一使用规范化后的主机。
- 新增 `golang.org/x/net v0.34.0` 依赖。
- 实测 `/user22334455/https/例え.テスト/`：Node 502、Go 502，Go 不再提前返回 404；
  新增 `TestIDNTargetHostIsNormalized` 覆盖原始/百分号编码、`UnrewriteURLWithBase` 和 `%2F` 注入拒绝。

### 14.3 P-32 超大 HTML 的 inline bootstrap

- `writeResponse` 的 truncated 分支现在注入 `renderInlineBootstrap(...) + injectedScriptTagAt(...)`，
  而不是只有外部脚本；`jsEscape` 新增非 ASCII 转义，保证超大 GBK 文档里的 bootstrap 仍是 ASCII。
- `TestOversizedGBKHTMLKeepsCharsetAndInjectsRuntime` 改为检查 inline bootstrap 和
  `proxy_url_prefix` 配置，并移除整段注入后逐字节比较原 GBK 正文。

### 14.4 P-33 运行时脚本路由与缓存

- `serve` 在 base-path 检查前匹配任意以 `/siteproxy-response-injected.js` 结尾的路径，
  与上游 `req.path.endsWith(...)` 一致。
- `serveStaticJS` 统一返回 `Cache-Control: no-cache, no-store, must-revalidate`。
- 新增 `TestInjectedRuntimeSuffixAndCacheControl`。

### 14.5 P-34 头合法但正文损坏的 gzip

- `writeResponse` 保留构造解码器时的 `recordingReader`；当读取阶段失败时，用原始头
  （含 `Content-Encoding`/`Content-Length`）重放 `rec.buf + resp.Body`，不再返回 502。
- 新增 `TestCorruptGzipWithValidHeaderIsReplayed`；原有 `"notgzip"` 构造失败用例继续通过。
- README 的“损坏 gzip 原样透传”现在对两类损坏都成立。

### 14.6 P-35 Google 外部 JS

- `renderJS` 不再调用 `stripGoogleIntegrityAssignments`；Google SRI 清理只保留在 HTML 分支。
- 新增 `TestGoogleExternalJSIntegrityIsPreserved`；实测 Node 与 Go 对 `google.com` 外部 JS
  都保留 `s.integrity='sha256-abc'`。

### 14.7 P-36 Service Worker target 头

- `assets/service-worker.tmpl.js` 在 client URL/referer 都没有提供上下文时，读取
  `siteproxy-target-host` / `siteproxy-target-protocol`，并用 `siteproxy-real-referer` 设置
  `siteproxy-newreferer`；只对 proxy-origin 且不在 token scope 的请求生效。
- 新增 `TestServiceWorkerConsumesTargetHeaders` 检查脚本包含对应分支和字段。

### 14.8 本轮修改文件

- `server.go`、`urlmap.go`、`proxy.go`、`rewrite.go`、`assets.go`
- `assets/service-worker.tmpl.js`
- `audit6_test.go`、`audit_fixes_test.go`、`medium_priority_test.go`
- `README.md`
- `go.mod`、`go.sum`

---

## 15. 第五轮深度对比审查（2026-11-17 下午）

本轮继续使用 Node v2.7.11（:5606）与当前 Go 构建（:5607），并临时启动：

- 17 MiB HTML 上游 :5703；
- 带 `blocked_sites` 的 Go 实例 :5617（`/tmp/diff/go-blockidn.json`）。

上游证据仍为 `/tmp/deob/deobfuscated.js`、`/tmp/deob/sw/deobfuscated.js`。
本轮只审查，不修改实现。

### 15.1 新增发现

| 编号 | 级别 | 类型 | 结论 |
| --- | --- | --- | --- |
| P-37 | 中 | 超大响应 | 超过 16 MiB 的 HTML/CSS/JS 在 truncated 分支直接返回，没有追加 `proxy_real_protocol` / `proxy_real_host` 控制 Cookie；Node 会追加，导致超大页面的越界请求修复和 SW 上下文缺失 |
| P-38 | 中低 | Service Worker 端点 | 上游 `_0x75fef1` 在缺少 `proxy_real_host` 时返回 404；Go 在 token 正确但 host 缺失时返回 200，并从任意 Referer 路径回填目标 host |
| P-39 | 中低 | 黑名单与 IDN | P-31 把目标 host 规范化为 punycode，但 `blocked_sites` 条目没有做同样的 IDNA 规范化；配置 Unicode 域名时黑名单被绕过 |
| P-40 | 低 | Service Worker 上下文 | P-36 只在 client/referer 都未提供上下文时读取 `siteproxy-target-*`；上游在同一源越界请求上即使已有 client/referer，也会用 `siteproxy-real-referer` 覆盖 `siteproxy-newreferer`，当前 Go 会丢失实际页面 Referer |

### 15.2 证据与复现

**P-37 超大响应缺少控制 Cookie**

17 MiB HTML 上游 :5703 返回 `text/html`。请求：

```bash
curl -sS -D - -o /dev/null \
  'http://127.0.0.1:5606/user22334455/http/127.0.0.1:5703/'
curl -sS -D - -o /dev/null \
  'http://127.0.0.1:5607/user22334455/http/127.0.0.1:5703/'
```

| 响应 | Node :5606 | Go :5607 |
| --- | --- | --- |
| 状态 | 200 | 200 |
| `Set-Cookie` | `proxy_real_protocol=http`、`proxy_real_host=127.0.0.1:5703` | 无 |
| `Content-Length` | 17828516 | 无（chunked） |

`当前工作区/proxy.go:488-529` 的 truncated 分支在 `addControlCookies` 之前
`return`。同一分支也覆盖超大 CSS/JS。P-32 修复只补了 inline bootstrap，
没有补控制 Cookie。

**P-38 SW 端点缺少 host 校验**

上游 `_0x75fef1` 在 `proxy_real_host` 缺失时直接 `return _0x5e17e8()`
（交给后续处理器，最终 404）；Go `serveServiceWorker`
（`当前工作区/server.go:486-516`）只校验 token，host 为空仍渲染 SW：

```bash
curl -sS -D - -o /dev/null \
  'http://127.0.0.1:5607/siteproxy_service_worker.js?token_prefix=/user22334455/&proxy_real_protocol=https'
# Go: 200, SW 内 proxy_real_host: ""

curl -sS -H 'Referer: http://evil.test/user22334455/http/evil.test/x' \
  'http://127.0.0.1:5607/siteproxy_service_worker.js?token_prefix=/user22334455/'
# Go: 200，proxy_real_host 被回填为 evil.test
# Node 相同请求：404
```

实测还显示 Node 对 host 缺失请求返回 404，而 Go 返回 200。回填路径
`targetFromProxiedURL(ref, ...)` 没有像 `repairOutOfScope` 那样校验
Referer 是否以 `{proxy_url}{token_prefix}` 开头。

**P-39 IDN 黑名单绕过**

启动配置（`/tmp/diff/go-blockidn.json`）：

```json
{
  "proxy_url": "http://proxy.test",
  "token_prefix": "/user22334455/",
  "local_listen_port": 5617,
  "blocked_sites": ["127.0.0.1", "例え.テスト"]
}
```

请求结果：

| 请求 | 状态 |
| --- | --- |
| `/user22334455/http/127.0.0.1:5701/` | 403（命中） |
| `/user22334455/http/例え.テスト/` | 502（未命中，进入 DNS） |
| `/user22334455/http/xn--r8jz45g.xn--zckzah/` | 502（未命中） |

`normalizeTargetHost`（`当前工作区/server.go:346-416`）把目标 host 转成
punycode；`Config.IsBlocked`（`当前工作区/config.go:181-192`）只做
`strings.ToLower`，没有对 `BlockedSites` 条目做 IDNA 规范化。配置写
Unicode 域名时，`host == b` 和 `strings.HasSuffix(host, "."+b)` 都不会命中。

**P-40 SW target 头只在 contextKnown=false 时生效**

上游 SW 的 target-header 分支位于同源越界请求的 `else if`
（`/tmp/deob/sw/deobfuscated.js:254-258`），即使 client URL 或 referer
已经给出 siteHost，也会用 `siteproxy-real-referer` 覆盖
`siteproxy-newreferer`。当前 `当前工作区/assets/service-worker.tmpl.js:161-180`
用 `!contextKnown` 限制该分支，因此 client/referer 可用时不会读取
`siteproxy-real-referer`；后续 `siteReferer` 为空，只会发送
target origin 作为 `siteproxy-newreferer`。对检查完整 Referer 的上游站点，
Go 会发送 `https://target` 而不是实际页面 URL。

另外，上游该分支使用 `pathname.includes(config_token_prefix)`，当前 Go
使用 `pathInScope`（要求 base path 后以 token 开头），在路径中间出现 token
的极少数情况下判定不同。

### 15.3 其他低风险观察

- `serveStaticJS`（`当前工作区/server.go:553-558`）返回
  `application/javascript; charset=UTF-8` 和 `Service-Worker-Allowed`；
  实测 Node runtime 为 `text/plain; charset=UTF-8`，Node SW 为
  `application/javascript` 且没有 `Service-Worker-Allowed`。浏览器可执行，
  但响应头不是严格等价。
- Go 的 404 使用 `http.NotFound`，实测 body 为 `404 page not found` 且带
  `X-Content-Type-Options: nosniff`；Node 为 `404 Not Found`。仅影响错误页细节。
- `validPort`（`当前工作区/server.go:442-447`）使用 `strconv.Atoi`，会接受
  `+80` 这类端口写法；构造上游 URL 时通常只会得到 400/502，属于输入校验边角。

### 15.4 建议优先级

1. **P-37**：在 truncated 分支 `WriteHeader` 前调用 `addControlCookies`；
   这是超大页面会话上下文缺失的真实功能问题。
2. **P-39**：在配置加载时对 `blocked_sites` 的非路径条目执行
   `normalizeTargetHost`/IDNA，或同时用 punycode 匹配。
3. **P-38**：缺少 `proxy_real_host` 时按上游返回 404；若保留 Referer
   回填，必须先校验 Referer 属于 `{proxy_url}{token_prefix}`。
4. **P-40**：按上游在 contextKnown=true 且同源越界的 else-if 中继续消费
   `siteproxy-real-referer`，并确认 `pathInScope` 与 `includes` 的边界。
5. 其余为低风险响应头/错误页细节，可与下一轮一起处理。

### 15.5 质量门禁

本轮未修改代码，现有测试仍通过：

```bash
cd /workspace
go test ./...
go vet ./...
go build -o /tmp/diff/siteproxy-go .
```

结果：测试通过，vet 无输出，构建成功。第五节发现已在 §16 修复。

---

## 16. 第五轮修复记录（2026-11-17 下午）

P-37 至 P-40 已修复，P-15/P-16/P-17/P-18/P-05 等已披露的有意差异保持不变。

### 16.1 P-37 超大文本响应补控制 Cookie

- `writeResponse` 的 truncated 分支在 `WriteHeader` 前调用
  `addControlCookies`（`当前工作区/proxy.go:495`），HTML/CSS/JS 超限原样转发时
  也会设置 `proxy_real_protocol` / `proxy_real_host`。
- `addControlCookies` 的注释同步改为“被重写或超限转发的文本响应”。
- `TestOversizedGBKHTMLKeepsCharsetAndInjectsRuntime` 增加两枚控制 Cookie 断言。

### 16.2 P-39 blocked_sites 的 IDNA 规范化

- 新增 `normalizeBlockedEntry`（`当前工作区/config.go:182`）：非路径条目先转小写、
  去除尾部点，再复用 `normalizeTargetHost` 转 punycode，并用 `hostWithoutPort`
  去掉端口；路径条目保持原样。
- `Config.IsBlocked`（`当前工作区/config.go:197`）也会规范化传入 host，因此直接调用
  `IsBlocked("例え.テスト")` 与传入 punycode 的结果一致。
- 新增 `TestBlockedSitesUnicodeIsNormalized`，覆盖 Unicode 配置、子域名、
  大小写/尾部点、端口和路径条目。

### 16.3 P-38 Service Worker 缺少 host 时返回 404

- `serveServiceWorker`（`当前工作区/server.go:489`）先校验 token，再对
  `proxy_real_host` 执行 `normalizeTargetHost`；缺失或非法 host 直接
  `http.NotFound`，不再从 Referer 回填，和上游 `_0x75fef1` 一致。
- 新增 `TestServiceWorkerRequiresValidTargetHost`，覆盖缺失 host、非法 host、
  仅 Referer、合法 host 四种请求。

### 16.4 P-40 Service Worker target 头不再依赖 contextKnown

- 删除 `contextKnown`；target-header 分支改为同源越界修复的 `else-if`
  （`当前工作区/assets/service-worker.tmpl.js:185`），client URL 或 Referer 已给出
  上下文时仍会消费 `siteproxy-target-*` 并用 `siteproxy-real-referer` 覆盖
  `siteproxy-newreferer`。
- 分支内的 token 判断改为上游的 `!url.pathname.includes(TOKEN_PREFIX)`。
- `TestServiceWorkerConsumesTargetHeaders` 更新为检查新分支，并断言不再出现
  `contextKnown`。

### 16.5 本轮修改文件

- `config.go`、`server.go`、`proxy.go`
- `assets/service-worker.tmpl.js`
- `audit6_test.go`、`audit7_test.go`、`medium_priority_test.go`
- `README.md`

### 16.6 质量门禁

```bash
cd /workspace
gofmt -l config.go server.go proxy.go audit6_test.go audit7_test.go medium_priority_test.go
node --check assets/service-worker.tmpl.js
go test ./...
go vet ./...
go build -o /tmp/diff/siteproxy-go .
```

结果：`gofmt -l` 无输出，`node --check` 通过，测试通过，vet 无输出，构建成功。

实测新构建：

- 17 MiB HTML 经 `:5607` 返回 200，并带上 `proxy_real_protocol=http` 与
  `proxy_real_host=127.0.0.1:5703` 两枚控制 Cookie。
- 临时实例 `:5617` 配置 `blocked_sites:["127.0.0.1","例え.テスト"]` 后，
  ASCII 与 Unicode 目标均返回 403。
- SW 缺少 host 返回 404，合法 host 返回 200；返回脚本中 `contextKnown` 为 0 次，
  `pathname.includes(TOKEN_PREFIX)` 存在。

§15.3 中 `serveStaticJS` 响应头、404 错误页文案和 `validPort("+80")` 仍属于低风险
差异，未在本轮改动。

---

## 17. 第六轮深度对比审查（2026-11-17 傍晚）

本轮基线为 Node v2.7.11（:5606）与当前 Go 构建（:5607，commit `18d93c9`）。
新增两个临时上游验证边角：

- `:5705`：同时下发 `Max-Age` 与 `Expires` 的 `Set-Cookie`（`/both`）；
- `:5706`：Telegram 目标下返回 GBK 编码 CSS（`/tgcss2`）。

上游证据仍为 `/tmp/deob/deobfuscated.js`、`/tmp/deob/injected/deobfuscated.js`
和 `/tmp/deob/sw/deobfuscated.js`。本轮只审查，不修改实现。

### 17.1 新增发现

| 编号 | 级别 | 类型 | 结论 |
| --- | --- | --- | --- |
| P-41 | 中 | Cookie 生命周期 | `Set-Cookie` 同时带 `Max-Age` 与 `Expires` 时，Go 会输出两个 `Max-Age`（一个来自原属性、一个来自 `Expires` 截断），上游只输出一个；浏览器可能按最后一个值解释，导致上限/删除语义不稳定 |
| P-42 | 中低 | Telegram CSS 字符集 | Telegram 的 GBK CSS 被 Go 转成 UTF-8，但响应中的 `@charset "gbk"` 未改写；浏览器会按旧 `@charset` 重新解码 UTF-8 字节，中文内容变成乱码。上游保留原始 GBK 字节和 GBK 响应头 |
| P-43 | 低 | 客户端 URL 还原 | 目标根路径 `/{token}/{proto}/{host}/` 经客户端 `unRewriteURL` 后丢失尾斜杠，`___location.href` / `document.URL` 为 `http://host`，上游为 `http://host/` |
| P-44 | 低 | Service Worker | Go 对 POST/PUT/PATCH 的 404 也延迟 3 秒；上游只在非 POST 分支的后台请求返回 404 时延迟，POST 分支直接返回 |
| P-45 | 低 | 客户端兼容面 | `window.traverseAndModifyNode(node)` / `window.debouncedAttributeChanged(node)` 只遍历 `node` 的后代，不处理 `node` 自身的属性；上游 `_0x3ab317` 会先处理根节点 |

### 17.2 证据与复现

**P-41 同时带 Max-Age 与 Expires 的 Set-Cookie**

上游 mock 返回：

```text
Set-Cookie: both1=v; Path=/; Max-Age=60; Expires=Wed, 21 Oct 2030 07:28:00 GMT
Set-Cookie: both2=v; Path=/; Expires=Wed, 21 Oct 2030 07:28:00 GMT; Max-Age=60
```

实测：

```text
Node :5606:
  both1=v; Path=/...; Domain=proxy.test; Max-Age=1800
  both2=v; Path=/...; Domain=proxy.test; Max-Age=1800

Go :5607:
  both1=v; Path=/...; Max-Age=60; Max-Age=1800
  both2=v; Path=/...; Max-Age=1800; Max-Age=60
```

原因在 `当前工作区/headers.go:341-380`：`Max-Age` 属性保留/截断后设置
`hasLifetime=true`，但 `Expires` 分支仍会独立追加一个 `Max-Age=1800`。
`hasLifetime` 只阻止最后的兜底追加，不会阻止这两个分支各自追加。
重复属性不符合 RFC 6265；当 `Max-Age` 与 `Expires` 顺序不同时，浏览器选择
前一个或后一个会得到不同生命周期。极端情况下 `Max-Age` 非删除值 +
过去时间 `Expires` 还可能把本应删除的 Cookie 延长到 30 分钟。

**P-42 Telegram CSS 的旧 @charset 与 UTF-8 响应头冲突**

mock 返回 `Content-Type: text/css; charset=gbk`，正文为：

```css
@charset "gbk";
a{content:"中"}
```

其中“中”是 GBK 字节 `d6 d0`。实测：

| 响应 | Node :5606 | Go :5607 |
| --- | --- | --- |
| Content-Type | `text/css; charset=gbk` | `text/css; charset=UTF-8` |
| `@charset` | `"gbk"` | `"gbk"`（未改写） |
| “中”字节 | `d6 d0`（GBK） | `e4 b8 ad`（UTF-8） |

`当前工作区/proxy.go:676-680` 对 Telegram CSS 先调用 `decodeTextBody`，然后直接
`return txt`。注释写的是“返回原始 CSS”，实际返回的是已转码的 UTF-8 文本；
而非 Telegram 分支在 `当前工作区/proxy.go:685-687` 会调用 `rewriteCSSCharset`。
浏览器按 `@charset "gbk"` 解码 UTF-8 字节，中文因此显示为乱码。上游
`_0x4b0bf2` 的 Telegram 分支不进入 CSS 处理，保留原始字节与 GBK 响应头。

修复方向：Telegram CSS 应返回 `raw`（解码压缩后的原始字符集字节），或至少在
返回 `txt` 前调用 `rewriteCSSCharset` 并把响应头同步为 UTF-8；不能只改其中一边。

**P-43 客户端还原目标根路径丢失尾斜杠**

`当前工作区/assets/injected.tmpl.js:262-265`：

```js
var m = path.match(/^(https?)\/([^/]+)(?:\/(.*))?$/);
if (!m) return url;
var real = m[1] + '://' + m[2];
if (m[3]) real += '/' + m[3];
return real;
```

当路径为 `https/host/` 时，`m[3]` 是空字符串，`if (m[3])` 为假，结果变成
`https://host`。上游 `_0x33e758.unrewrite` 在去掉 token 后调用 `new URL()`，
`new URL("https://host").href` 会补成 `https://host/`。用 Node 直接执行当前
客户端脚本验证：

```text
root:  http://target.test
path:  http://target.test/a/b
query: http://target.test/?x=1
```

`___location.href`、`document.URL`、`document.documentURI` 和 `toString()`
都经过这个函数；依赖尾斜杠字符串比较的页面脚本会观察到差异。服务器端
`UnrewriteURLWithBase` 会把根路径补成 `/`，客户端与此不一致。

**P-44 Service Worker 的 404 延迟覆盖 POST**

上游 `/tmp/deob/sw/deobfuscated.js:300-318` 只在非 POST/PUT/PATCH 分支的
`fetch` 返回 404 时延迟 3 秒；POST 分支直接 `return fetch(...).then(...)`。
`当前工作区/assets/service-worker.tmpl.js:240-252` 在统一拿到 `resp` 后
对所有方法执行 404 延迟，因此 POST/PUT/PATCH 的 404 也会多等 3 秒。功能上
仍返回原响应，属于延迟差异。

**P-45 兼容遍历不处理根节点**

上游 `_0x3ab317` 在遍历一个节点时会先处理该节点自身的 URL 属性，再递归子节点。
当前客户端 `traverse(root)` 使用 `root.querySelectorAll('*')`，只覆盖后代；
`window.traverseAndModifyNode(node)` 和 `window.debouncedAttributeChanged(node)`
直接调用 `traverse`，因此传入的根元素自身的 `href/src/style` 等不会改写。
常规的 `rewriteElement` + `traverse` 插入路径会先单独处理根节点，所以影响主要
出现在页面脚本主动调用这两个兼容 API 时。

### 17.3 其他观察

- 上游的 HTML/JS 标识符重写会连注释里的 `location` 也改成 `___location`；
  Go 的 `rewriteJSIdentifiers` 保留注释和可见文本，只在真实 JS 语境中改写。
  这是 Go 更精确的行为，但属于未写入 README 的差异。
- 上游 CSS 的 `@charset` 大小写与 `@import url( '...' )` 空格会原样保留；
  Go 会规范化为 `@charset "UTF-8"` 并压缩 `url(...)` 内空白。属于字节级差异，
  不影响语义；普通 CSS 的 `@charset` 改写是有意的 UTF-8 修复。
- `validPort("+80")`、`serveStaticJS` 响应头和 404 错误页文案仍是 §15.3
  记录的低风险项，本轮未新增改动。

### 17.4 建议优先级

1. **P-42**：Telegram CSS 乱码是真实可见缺陷，优先修 `renderCSS` 的 Telegram
   分支，保证 `@charset`、响应头和字节编码三者一致。
2. **P-41**：避免重复 `Max-Age`；在任一有效生命周期属性处理后不再追加第二个上限，
   并补 `Max-Age` + `Expires` 组合测试。
3. **P-43**：客户端根路径补尾斜杠，补一个在 Node 中执行运行时脚本的回归测试，
   断言 `unRewriteURL(".../https/host/") === "https://host/"`。
4. **P-44**：把 404 延迟移回非 POST 分支，或明确记录这一延迟差异。
5. **P-45**：让兼容遍历先处理根节点，或改为与上游一致的递归遍历。

### 17.5 质量门禁

本轮未修改实现代码；现有测试和构建基线继续通过：

```bash
cd /workspace
go test ./...
go vet ./...
go build -o /tmp/diff/siteproxy-go .
```

结果：测试通过，vet 无输出，构建成功。以上 P-41 至 P-45 留待下一轮修复。

---

## 18. 第六轮修复记录（2026-11-18）

P-41 至 P-45 已修复。本轮未改变此前已披露的 P-05/P-07/P-15/P-16/P-17/P-18
等有意差异。

### 18.1 P-41 Set-Cookie 生命周期属性只输出一次

- `rewriteSetCookie` 在写入属性前先扫描全部属性：只要存在任意 `Max-Age`，
  就由 `Max-Age` 决定生命周期并丢弃 `Expires`，避免 `Expires` 截断逻辑再追加
  第二个 `Max-Age`。
- 任意过去的 `Expires` 视为删除标记，优先于正的 `Max-Age`：删除 `Max-Age`、
  保留过去的 `Expires`，避免把待删除 Cookie 复活。
- `TestSetCookieMultipleLifetimeAttrsHasSingleMaxAge` 覆盖四种组合：
  `Max-Age` 在前/在后、长 `Max-Age`+远 `Expires`、过去 `Expires`+长 `Max-Age`。

实测新构建（`/tmp/diff/siteproxy-go`，临时 `:5619`）：

```text
both1=v; Path=...; Max-Age=60
both2=v; Path=...; Max-Age=60
```

### 18.2 P-42 Telegram CSS 保留原始字符集字节

- `renderCSS` 把 Telegram 判断移到 `decodeTextBody` 之前；Telegram CSS 直接返回
  解压后的原始字节，不再转 UTF-8，也不再进入 `@charset` / `url()` 重写。
- 响应头由 `writeResponse` 按原始字节判断：GBK 字节不是合法 UTF-8，因此保留
  上游的 `text/css; charset=gbk`，`@charset "gbk"` 与正文编码一致。
- `TestTelegramLegacyCSSKeepsOriginalBytesAndHeader` 同时验证
  `renderCSS` 字节不变，以及 `writeResponse` 的响应头和正文不变。

实测 GBK 响应：

```text
Content-Type: text/css; charset=gbk
正文中的“中”：d6 d0（GBK）
```

### 18.3 P-43 客户端目标根路径补尾斜杠

- `unRewriteURL` 改为分别捕获 host、rest 和 hash；当 rest 为空或以 `?` 开头时先补
  `/`，再拼接 rest/hash。例如：
  - `https/host` -> `https://host/`
  - `https/host/` -> `https://host/`
  - `https/host?x=1` -> `https://host/?x=1`
  - `https/host#f` -> `https://host/#f`
- 用 Node 直接执行当前 `assets/injected.tmpl.js` 验证：

```text
root:  http://target.test/
path:  http://target.test/a/b
query: http://target.test/?x=1
```

- `TestClientUnrewriteRootAndTraverseRoot` 加入源码级回归守卫。

### 18.4 P-44 Service Worker 只对非写请求延迟 404

- 计算 `isWriteMethod`（POST/PUT/PATCH），404 延迟条件改为
  `resp.status === 404 && !isWriteMethod`。
- POST/PUT/PATCH 的 404 立即返回，与上游非写分支的 3 秒延迟保持一致。
- `TestServiceWorker404DelayExcludesWriteMethods` 检查两个关键分支。

### 18.5 P-45 兼容遍历处理根节点

- `traverse(root)` 先对 `root` 自身调用 `rewriteElement`，再遍历
  `root.querySelectorAll('*')`；`window.traverseAndModifyNode(node)` 和
  `window.debouncedAttributeChanged(node)` 因此也会处理传入元素自身的
  `href/src/style/srcset` 等属性。
- `TestClientUnrewriteRootAndTraverseRoot` 加入根节点处理守卫。

### 18.6 本轮修改文件

- `headers.go`、`proxy.go`
- `assets/injected.tmpl.js`、`assets/service-worker.tmpl.js`
- `audit8_test.go`（新增）
- `README.md`
- `PORTING_AUDIT_REPORT.md`

### 18.7 质量门禁

```bash
cd /workspace
gofmt -w headers.go proxy.go audit8_test.go
node --check assets/injected.tmpl.js
node --check assets/service-worker.tmpl.js
go test ./...
go vet ./...
go build -o /tmp/diff/siteproxy-go .
```

结果：`gofmt` 已执行，两个 `node --check` 通过，`go test ./...` 通过，
`go vet ./...` 无输出，构建成功。临时实例 `:5619` 的 Cookie 与 Telegram GBK
CSS 实测结果见 §18.1 和 §18.2。
