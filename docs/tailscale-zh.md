# mihomo-alpha · Tailscale 集成中文教程

本文面向想在 mihomo 里同时把 Tailscale 用作**出站代理**与**入站服务转发**的中文用户，覆盖从首次接入到生产部署的完整流程，并重点说明本 fork 独有的「身份共享」设计。

> 英文文档请查阅根目录 [README.md](../README.md)。

---

## 1. 为什么需要这个集成

upstream 的 mihomo 并不内置 Tailscale 支持，要把 mihomo 节点接入 tailnet，常见做法是：

1. 系统侧装 `tailscaled`，再让 mihomo 走 `direct` 或 socks5 出站；或者
2. mihomo 跑在容器/系统里，TUN 接收所有流量。

两种方式都需要额外组件，且无法直接「按规则路由」到 Tailscale。

本 fork 把官方 [`tsnet`](https://pkg.go.dev/tailscale.com/tsnet) 库直接编进 mihomo（编译时打 `with_tailscale` tag），提供：

- `type: tailscale` 的出站代理：通过 tsnet 拨号到 tailnet 内任意节点，支持 exit-node 选择；
- `type: tailscale` 的入站监听：在 tailnet 上暴露本机服务端口（SSH、HTTP、DNS 等）。

整个集成隐藏在 build tag 之后，**默认编译路径与上游完全一致**。

---

## 2. 编译

### 2.1 仅需 mihomo（与上游相同）

```bash
go build ./...
```

### 2.2 启用 Tailscale 支持

```bash
go build -tags with_tailscale ./...
```

### 2.3 生产构建（推荐）

```bash
CGO_ENABLED=0 go build \
  -tags "with_gvisor,with_tailscale" \
  -trimpath \
  -ldflags '-w -s -buildid=' \
  -o bin/mihomo .
```

需要 Go ≥ 1.26.1。运行 `./mihomo -v` 时会在 tags 列表里看到 `with_tailscale`。

---

## 3. 概念：什么是「身份」？

mihomo 在内部把每个 tsnet 节点抽象为一个**身份**，由这三项决定：

| 维度 | 来源 | 说明 |
|---|---|---|
| `state-dir` | 配置项 `state-dir`，默认 `<HomeDir>/tailscale/<hostname-or-name>/` | 持久化目录；tsnet 启动后的 node key、prefs 都存在这里 |
| `control-url` | 配置项 `control-url`，默认官方控制面板 | 你的 tailnet 控制平面 |
| 主机名 | 配置项 `hostname`（若空则用 `name`，再经 DNS label 规范化） | 控制面板上显示的设备名 |

**只要三者完全相同，mihomo 就认为是同一份身份**。出站与入站两端如果落在同一份身份上，会共用一个 `tsnet.Server`，因此在 Tailscale 管理端只会注册**一台设备**——这是本 fork 与朴素「出入站各一台」做法最大的区别。

> 注意：`auth-key` 不参与身份判定。它是首启动凭据，写完一次就可以删除；之后从 `state-dir` 里恢复持久化身份。

---

## 4. 最小可用配置

只把 mihomo 接入 tailnet（首次需要 auth key）：

```yaml
proxies:
  - name: ts-home
    type: tailscale
    auth-key: tskey-auth-xxxxxxxxxxxxxxxx
    hostname: ts-home

rules:
  - IP-CIDR,100.64.0.0/10,ts-home,no-resolve
  - DOMAIN-SUFFIX,ts.net,ts-home
  - MATCH,DIRECT
```

启动后查看日志，应该能看到类似：

```
[TS](ts-home) online (ipv4=100.x.y.z, ipv6=fd7a:...)
```

此时 Tailscale 控制面板上多了一台名为 `ts-home` 的设备；mihomo 内部的 `ts-home` 出站可以拨号到 tailnet 中任意节点。

---

## 5. 推荐配置：出入站共享同一身份

这是绝大多数场景下的正确写法——同一台 mihomo 既走 tailnet 出流量，又把本机服务暴露在 tailnet 上：

```yaml
proxies:
  - name: ts-home
    type: tailscale
    auth-key: tskey-auth-xxxxxxxxxxxxxxxx
    hostname: ts-home
    accept-routes: true              # 接受 subnet-router 通告的子网路由
    # exit-node: exit-gateway.example.ts.net  # 可选

listeners:
  - name: ts-home-services
    type: tailscale
    hostname: ts-home                # 关键：与上面 proxies.ts-home 完全一致
    forwards:
      - listen: 22                   # tailnet 上的 100.x.y.z:22
        target: 127.0.0.1:22         # 转到本机 SSH
      - listen: 80
        target: 127.0.0.1:8080
      - listen: 53
        target: 127.0.0.1:53
        network: udp                 # 默认 auto = tcp+udp，DNS 这里显式只走 UDP

rules:
  - IP-CIDR,100.64.0.0/10,ts-home,no-resolve
  - DOMAIN-SUFFIX,ts.net,ts-home
  - MATCH,DIRECT
```

启动后行为：

1. ApplyConfig 期间出站先 `Acquire`（refcount=1，但不立即 Start）；接着入站 `Acquire` 命中同一身份 → refcount=2，**直接复用**同一个 `Instance`。
2. 入站 `Listen()` **立即返回 nil**，同时启动后台 goroutine 调用 `Instance.Start()` 触发 `tsnet.Up()` 注册设备；mihomo 主流程（API / TUN / 其他 listener）**不受任何阻塞**，即使 tailscale 网络不可达或 auth-key 错误也只影响这台 listener 自己的可用时机。
3. 后台 goroutine 每次握手有 60s 上限（`startAttemptTimeout`），失败按 1s → 60s 指数退避自动重试，直到成功或 listener 被关闭；这意味着控制面恢复后端口会自动可用，无需用户干预。日志能看到 `bring-up attempt N failed: ... (retry in ...)`；成功一次后输出 `forwards listening at: tcp://...,udp://...`。
4. mihomo 启动完成、`OnRunning` 之后的 warmup 阶段，出站发现 `initOk` 已置位（如果入站抢先 Start 完成）直接返回；否则出站独立完成自己的 lazy init，但两路依然共享同一台 `Instance`、控制面板始终只显示一台设备。
5. `accept-routes: true` 在 tsnet 完成 `Up()` 之后立即生效；入站默认也是 `true`（与出站对齐），所以共享身份时不会有 first-wins WARN 噪音。如果你显式把某一侧写成 `false`、另一侧写成 `true`，第一个 `Acquire` 的设置会胜出。
6. **auth-key 的 bootstrap 行为**：如果出站没写 auth-key（依赖既有持久化状态），入站写了 auth-key，由于出站先 Acquire 时缓存的 auth-key 为空、共享 `Instance` 仍处于 idle 状态，入站这次 Acquire 会把 auth-key 自动「过继」到共享身份上，紧接着的 `Start` 用这把 key 完成首次认证。日志会看到 `adopting auth-key from later Acquire on shared identity`。一旦 `Start()` 已经取走 option 快照，后续 auth-key 会进入 WARN 路径。

控制面板上始终只会显示一台名为 `ts-home` 的设备。

> 注意：入站 listener 的握手是异步的，端口从 mihomo 配置生效到实际可用之间会有几秒到几十秒（首次注册设备）的窗口。这段时间访问 tailnet 上的对应端口会被拒绝，等到日志里出现 `forwards listening at: ...` 后才正式可用。如果握手长期失败（比如 auth-key 错误），日志会持续打印 `bring-up attempt N failed ... (retry in ...)`，但不会影响 mihomo 主流程。

---

## 6. 配置项详解

### 6.1 出站（`proxies.type: tailscale`）

| 字段 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `name` | 是 | — | mihomo 内部 proxy 名，必须唯一 |
| `auth-key` | 首次 | — | Tailscale 注册密钥；持久化后可删除 |
| `hostname` | 否 | `<name>` | 控制面板上的设备名，**身份锚点** |
| `control-url` | 否 | 官方 | 自建 Headscale 等控制平面填这里 |
| `ephemeral` | 否 | `false` | 设为 `true` → 节点下线后被自动清理 |
| `exit-node` | 否 | — | 出口节点，支持 FQDN / HostName / Tailscale IP / StableNodeID；留空清空已持久化的选择 |
| `accept-routes` | 否 | `true` | 是否接受 tailnet 对端通告的子网路由；节点级属性 |
| `state-dir` | 否 | `<HomeDir>/tailscale/<hostname-or-name>/` | 持久化目录，**身份锚点** |

**对 Tailscale 不生效的字段（设置会在启动日志里看到 warn）**：
`dialer-proxy` · `interface-name` · `routing-mark` · `ip-version` · `tfo` · `mptcp`

### 6.2 入站（`listeners.type: tailscale`）

监听器级字段：

| 字段 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `name` | 是 | — | listener 名 |
| `auth-key` | 首次 | — | Tailscale 注册密钥 |
| `hostname` | 否 | `<name>` | **身份锚点**；和出站填同样的值即可共享 tsnet |
| `control-url` | 否 | 官方 | — |
| `ephemeral` | 否 | `false` | — |
| `accept-routes` | 否 | `true` | 与出站默认对齐；显式写值则按 first-wins 在共享实例上生效 |
| `state-dir` | 否 | `<HomeDir>/tailscale/<hostname-or-name>/` | **身份锚点** |
| `rule` | 否 | — | 子规则名，作用于转发流量 |
| `proxy` | 否 | — | 全局出站覆盖：所有 forward 默认走哪个 proxy |
| `forwards` | 是 | — | 端口转发列表，至少一条 |

每条 forward 字段：

| 字段 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `listen` | 是 | — | tailnet 侧暴露的端口 |
| `target` | 是 | — | `host:port`，转发目的地 |
| `network` | 否 | `auto` | `auto` = TCP + UDP，可显式填 `tcp` 或 `udp` |
| `proxy` | 否 | 继承 listener `proxy` | 该条 forward 单独走的 proxy |

---

## 7. 几种典型用法

### 7.1 只做出站（用 Tailscale 做翻墙/跨地域路由）

```yaml
proxies:
  - name: ts-exit
    type: tailscale
    auth-key: tskey-auth-...
    hostname: ts-exit
    exit-node: exit-gateway.example.ts.net

rules:
  - GEOIP,CN,DIRECT
  - MATCH,ts-exit
```

### 7.2 只做入站（把本地服务暴露到 tailnet）

```yaml
listeners:
  - name: ts-services
    type: tailscale
    auth-key: tskey-auth-...
    hostname: ts-services
    forwards:
      - listen: 22
        target: 127.0.0.1:22
      - listen: 5000
        target: 127.0.0.1:5000
```

此时控制面板上只有一台 `ts-services`。

### 7.3 多身份并存（确实需要两台设备）

如果有理由让出入站作为**不同设备**（例如要在 ACL 里给它们写不同 tag）：

```yaml
proxies:
  - name: ts-out
    type: tailscale
    auth-key: tskey-...
    hostname: mihomo-out

listeners:
  - name: ts-in
    type: tailscale
    auth-key: tskey-...
    hostname: mihomo-in           # 故意不一致 → 两台设备
    forwards:
      - listen: 22
        target: 127.0.0.1:22
```

只要 `hostname` 或 `state-dir` 任一不同，就是不同身份，控制面板上分别注册两台设备。

### 7.4 Headscale 等自建控制平面

```yaml
proxies:
  - name: ts-headscale
    type: tailscale
    auth-key: yourkey
    hostname: mihomo-1
    control-url: https://headscale.example.com
```

---

## 8. 与其他 mihomo 特性的协作

### 8.1 TUN 模式

入站 listener 在 mihomo 启动早期就跑 `tsnet.Up()`；为了不与 TUN 接管系统路由的过程发生竞争，调用顺序已经在 [hub/executor/executor.go](../hub/executor/executor.go) 里调整为「TUN 先，listeners 后」。一般无需特别配置。

### 8.2 DNS

tsnet 内部使用 Go 标准库 resolver；mihomo 在启动 tsnet 时会把 `net.DefaultResolver.Dial` 临时替换成一个跳过 mihomo DNS 重写的拨号器（refcount 实现，多身份并存安全），不会干扰你已有的 DNS 配置。

### 8.3 配置热重载

修改 listeners.forwards 等字段触发 reload 时：

- `PatchInboundListeners` 会先 Close 旧 listener、再 Listen 新 listener。
- 旧 listener 的 release 让 refcount 减一；如果 outbound 仍然引用同一身份，refcount 仍 ≥ 1，tsnet 不动。
- 即使 refcount 减到 0，registry 还会保留 5 秒（`closeGracePeriod`）；新 listener 的 Acquire 落在窗口内会复用同一个 `tsnet.Server`，**不会触发设备重新注册**。如果 grace timer 到期时 `Start()` 仍在进行，timer 会再等一个 grace 窗口后重查。

---

## 9. 常见问题

### Q1：控制面板看到两台设备，我并没有想要这样

检查两份配置的 `hostname`、`state-dir`、`control-url` 是否完全一致。任何一项不同都会产生独立身份。

### Q2：日志里看到 `reusing existing tsnet identity, ignoring conflicting options: ...`

说明两个 Tailscale 入口共享了同一身份，但 `ephemeral` / `accept-routes` 或双方都填的不同 `auth-key` 写得不一样。mihomo 按 first-wins 策略，沿用第一个 Acquire 的设置。把两份配置对齐即可。

`auth-key` 例外：如果第一个 Acquire 没填 key（比如出站靠持久化 state 复用身份）、第二个填了，并且共享 `Instance` 仍处于 idle 状态（`initOk == false` 且 `startInProgress == false`），那么新 key 会被采纳，日志显示 `adopting auth-key from later Acquire on shared identity`。`Start()` 已经取走 option 快照或节点已经上线时，再传新 key 会以 warn 提示「new auth-key ignored ...」。

### Q3：首次启动卡在 `tsnet.Up()` 没动静

入站 listener 的握手在后台异步进行，不会阻塞 mihomo 主流程；如果背景 goroutine 一直失败，日志会持续打印 `Tailscale[<name>] bring-up attempt N failed: ... (retry in ...)`，通常原因是 `auth-key` 失效、控制面板不可达，或者防火墙拦截了 tsnet 的 DERP / WireGuard 出站。`[TS](<name>) Logf: ...` 一行会带出 tsnet 自身的诊断信息，按其指引修即可——修好之后无需 reload，下一轮退避到期就会自动重新尝试。

出站这一侧是 lazy 的（首次拨号或 warmup 触发）；如果出站 warmup 卡住，主流程同样不阻塞，只会在 5 分钟超时后日志里出现 `startup warmup failed: ...`，下次 Dial 会再试一次。

### Q3.1：mihomo 主进程会被 tailscale 卡住吗？

不会。`Listen()` 同步返回 nil，整个握手在 per-listener 的 goroutine 里跑，并被 `lifecycleCtx` + 60s 单次握手超时双重约束。即使配置写错了 `auth-key`，mihomo 的 API、TUN、其他 listener、其他出站都能正常起来，只有这台 tailscale listener 的 forward 端口暂时不可用，并在每次退避到期自动重试。`Close()`（reload / 退出）触发的 lifecycle 取消会让 goroutine 立刻退出，不会留下僵尸。

### Q4：UDP 转发为什么需要绑定具体 IP？

userspace tsnet 的 `ListenPacket("udp", ...)` 不支持 `:port` 通配，必须给出具体的 Tailscale IP。mihomo 自动用 `tsnet.Server.TailscaleIPs()` 拿到的 IPv4 / IPv6 分别绑定，对用户透明。

### Q5：怎么换 Tailscale 版本？

```bash
go list -m -versions tailscale.com | tr ' ' '\n' | tail -10
go get tailscale.com@<new-tag>
go mod tidy
go build ./... && go build -tags with_tailscale ./...
```

注意 tailscale.com 对 Go 版本有要求，必要时同步 `go.mod` 里的 `go` 行（本 fork 当前是 `go 1.26.1`）。

### Q6：dialer-proxy / TFO / MPTCP 为什么不生效？

tsnet 自管 socket，mihomo 的 BasicOption 拨号选项不会传到 tsnet 内部。这一点会在启动时打 warn 提醒。

---

## 10. 相关源码索引

| 模块 | 路径 |
|---|---|
| 注册表 / 引用计数 / grace timer | [component/tailscale/server.go](../component/tailscale/server.go) |
| 出站适配器 | [adapter/outbound/tailscale.go](../adapter/outbound/tailscale.go) |
| 入站监听 / 端口转发 | [listener/inbound/tailscale.go](../listener/inbound/tailscale.go) |
| Tailscale 版本戳补丁 | [component/tailscalestamp/stamp.go](../component/tailscalestamp/stamp.go) |
| 项目维护者指南 | [AGENTS.md](../AGENTS.md) |

---

## 11. 反馈

本 fork 是个人项目，源码与 upstream `MetaCubeX/mihomo` 的 `Alpha` 分支同步。Tailscale 相关问题可以直接看 [AGENTS.md](../AGENTS.md) 里维护者视角的设计说明与实现 invariants。
