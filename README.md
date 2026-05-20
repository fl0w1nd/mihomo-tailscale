<h1 align="center">
  <br>Mihomo has officially supported Tailscale outbound<br>
  <br>this repository is only for archiving purposes<br>
</h1>

https://github.com/MetaCubeX/mihomo/pull/2786


<h1 align="center">
  <img src="Meta.png" alt="Meta Kernel" width="200">
  <br>mihomo + Tailscale<br>
</h1>

<h3 align="center">mihomo fork with built-in Tailscale outbound routing and inbound service forwarding via tsnet.</h3>

<p align="center">
  <a href="https://github.com/MetaCubeX/mihomo">
    <img src="https://img.shields.io/badge/upstream-MetaCubeX%2Fmihomo-blue?style=flat-square">
  </a>
  <img src="https://img.shields.io/badge/go-%E2%89%A51.26.1-00ADD8?style=flat-square">
  <img src="https://img.shields.io/badge/tailscale.com-v1.96.5-005eff?style=flat-square">
</p>

## Overview

A personal fork of [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) (based on the `Alpha` branch) that adds two Tailscale capabilities behind the `with_tailscale` build tag:

- **Outbound routing** via `type: tailscale` in `proxies`, using [tsnet](https://pkg.go.dev/tailscale.com/tsnet) as a first-class outbound adapter.
- **Inbound service forwarding** via `type: tailscale` in `listeners`, exposing selected local services on the node's Tailscale IPs.

Outbound and inbound automatically **share a single tsnet identity** when configured with the same `hostname` / `state-dir` / `control-url` — so a typical "outbound + inbound on the same node" deployment registers exactly one device on the Tailscale control plane.

All upstream features remain intact. The Tailscale integration is isolated behind the `with_tailscale` build tag — the default binary is identical to upstream.

> 中文文档：[docs/tailscale-zh.md](docs/tailscale-zh.md) 提供完整的中文使用教程，包含部署示例、共享身份配置和常见问题。

## What's Different from Upstream

| Area | Upstream mihomo | This Fork |
|---|---|---|
| Tailscale outbound | Not available | `type: tailscale` proxy adapter via tsnet |
| Tailscale service forwards | Not available | `type: tailscale` listener with multi-port forwards |
| Go version | `go 1.20` | `go 1.26.1` (required by tailscale.com v1.96.5) |
| Binary size | Baseline | +~32 MB when built with `with_tailscale` tag |
| Default build | Unaffected | Identical — stub returns an error if tailscale type is used |

### New Files

| File | Purpose |
|---|---|
| `adapter/outbound/tailscale.go` | Outbound adapter (`//go:build with_tailscale`) |
| `adapter/outbound/tailscale_stub.go` | Stub (`//go:build !with_tailscale`) — returns error |
| `adapter/outbound/tailscale_udp_bind.go` | UDP bind address selection for outbound |
| `adapter/outbound/tailscale_test.go` | Unit tests for hostname sanitization, peer matching, option validation |
| `adapter/outbound/tailscale_udp_bind_test.go` | Unit tests for UDP bind selection |
| `component/tailscale/server.go` | Shared tsnet lifecycle with identity-keyed registry and refcounted instance sharing |
| `component/tailscale/server_stub.go` | Stub for the shared lifecycle layer |
| `component/tailscale/server_test.go` | Tests for `Acquire` / release / grace-period close / first-wins conflict policy |
| `listener/inbound/tailscale.go` | Tailscale listener with service forwards |
| `listener/inbound/tailscale_stub.go` | Stub for tailscale listener support |
| `listener/inbound/tailscale_test.go` | Tests for forward startup and rollback paths |
| `component/tailscalestamp/stamp.go` | Patches Tailscale version stamp at runtime to avoid `ERR-BuildInfo` |
| `component/tailscalestamp/stamp_test.go` | Verifies version stamp is correctly populated |
| `constant/features/with_tailscale.go` | Feature flag `WithTailscale = true` |
| `constant/features/with_tailscale_stub.go` | Feature flag `WithTailscale = false` |
| `docs/tailscale-zh.md` | Chinese-language tutorial for the Tailscale integration |

### Modified Files

| File | Change |
|---|---|
| `go.mod` | `go 1.20` → `1.26.1`; added `tailscale.com v1.96.5` |
| `constant/adapters.go` | Added `Tailscale` enum and `String()` case |
| `adapter/parser.go` | Added `case "tailscale"` in `ParseProxy` |
| `listener/parse.go` | Added `case "tailscale"` in `ParseListener` |
| `listener/tunnel/tcp.go` | Added listener reuse constructor for forwarded TCP services |
| `listener/tunnel/udp.go` | Added packet-conn reuse constructor for forwarded UDP services |
| `constant/features/tags.go` | `with_tailscale` appears in `mihomo -v` output |
| `docs/config.yaml` | Added tailscale outbound and inbound examples |

## Configuration

### Outbound

Minimal example:

```yaml
proxies:
  - name: ts-exit
    type: tailscale
    auth-key: tskey-auth-xxxxxxxxxxxxxxxxxxxxx
    hostname: mihomo-exit
```

Full example with all options:

```yaml
proxies:
  - name: ts-exit
    type: tailscale
    auth-key: tskey-auth-xxxxxxxxxxxxxxxxxxxxx  # Required on first run; removable after state is persisted
    hostname: mihomo-exit                        # Hostname shown in Tailscale admin console
    control-url: https://controlplane.tailscale.com  # Optional, default is the official control plane
    ephemeral: false                             # Optional, true = node auto-removed after going offline
    exit-node: exit-gateway.example.ts.net       # Optional, route all traffic through this exit node
    accept-routes: true                          # Optional, default true; accept subnet routes advertised by tailnet peers
    state-dir: /var/lib/mihomo/tailscale/ts-exit # Optional, default is <HomeDir>/tailscale/<name>/

rules:
  - IP-CIDR,100.64.0.0/10,ts-exit,no-resolve   # Tailscale CGNAT range
  - DOMAIN-SUFFIX,ts.net,ts-exit                # Tailscale MagicDNS domains
  - GEOIP,US,ts-exit                            # Example: route US traffic through the exit node
  - MATCH,DIRECT
```

### Outbound Option Reference

| Option | Required | Default | Description |
|---|---|---|---|
| `name` | Yes | — | Unique proxy name within the mihomo instance. |
| `type` | Yes | — | Must be `tailscale`. |
| `auth-key` | First run | — | Tailscale auth key (`tskey-auth-...`). Can be removed once state is persisted. |
| `hostname` | No | `<name>` | Hostname visible in Tailscale admin console. Sanitized to DNS label format. |
| `control-url` | No | Official Tailscale | Custom control plane URL (e.g., for Headscale). |
| `ephemeral` | No | `false` | If `true`, the node is removed from the tailnet after going offline. |
| `exit-node` | No | — | Exit node for routing. Accepts FQDN, HostName, Tailscale IP, or StableNodeID. Omitting clears any persisted selection. |
| `accept-routes` | No | `true` | Accept subnet routes advertised by tailnet peers (subnet routers). Set `false` to ignore advertised routes. Node-level: takes effect for whichever side (outbound or inbound) brings the shared tsnet up first. |
| `state-dir` | No | `<HomeDir>/tailscale/<hostname\|name>/` | Directory for persisting node state. Two adapters sharing this directory (and `hostname` / `control-url`) share the same tsnet identity. |

### Inapplicable Options

The following `BasicOption` fields are ignored because tsnet manages its own sockets:

`dialer-proxy` · `interface-name` · `routing-mark` · `ip-version` · `tfo` · `mptcp`

A warning is logged at startup if any of these are set.

### Operational Notes

- **First run** requires `auth-key`. After state is persisted to `state-dir`, the key can be removed.
- **Long-lived nodes** should have key expiry disabled in the Tailscale admin console.
- **Exit node fallback**: if the specified exit node is unavailable, the adapter falls back to direct routing and logs a warning. It does not automatically select another exit node.
- **Lazy initialization (outbound)**: the outbound tsnet server starts on the first `Dial`/`ListenPacket` call (or via the post-startup warmup pass), not at mihomo startup.
- **Async initialization (inbound)**: `Listen()` returns immediately after acquiring the shared instance; the tsnet handshake runs in a per-listener goroutine with a bounded per-attempt timeout (60s) and exponential backoff (1s → 60s) on failure. **mihomo's main `ApplyConfig` path never blocks on the Tailscale handshake** — if the control plane is silent or the auth-key is wrong, only the affected listener's forwards stay pending and retry in the background until they succeed or `Close()` is called. Forwards become available the moment the handshake completes (`forwards listening at: ...` log line).
- **Lifecycle-bound bring-up**: `Close()` cancels the per-listener context, waits for the bring-up goroutine to exit, then releases the shared instance. Both `Close()` and the registry's release closure are idempotent.
- **Shared identity**: an outbound proxy and an inbound listener that resolve to the same identity tuple (`state-dir`, `control-url`, sanitized `hostname`-or-`name`) share a single tsnet server. They appear as **one device** on the Tailscale control plane. Differently named entries remain fully independent.

### Sharing One Tailnet Identity Between Outbound and Inbound

The most common deployment uses one tailnet identity to (a) route mihomo traffic out through Tailscale, and (b) expose local services to the tailnet. To make outbound and inbound share that identity, give them the same `hostname` (or the same `state-dir`) and the same `control-url`:

```yaml
proxies:
  - name: ts-home
    type: tailscale
    auth-key: tskey-auth-xxxxxxxxxxxxxxxxxxxxx
    hostname: ts-home              # identity anchor
    accept-routes: true

listeners:
  - name: ts-home-services
    type: tailscale
    hostname: ts-home              # same identity → shared tsnet
    forwards:
      - listen: 22
        target: 127.0.0.1:22
      - listen: 80
        target: 127.0.0.1:8080
```

What happens internally:

- Both entries call `tailscale.Acquire(opts)` against a process-wide registry keyed by the resolved identity tuple.
- The first call creates the shared `*Instance`; the second call reuses that `*Instance` and bumps a reference counter. The tsnet server is created later by the first `Start()` call.
- `accept-routes` is a node-level preference applied once at `Up()` time; the **first caller wins** if the two entries disagree, and a warning is logged.
- `auth-key` is normally first-wins as well, with one carve-out: if the first `Acquire` had an empty key (e.g. the outbound can use persisted state) and a later `Acquire` supplies one before the shared `Instance` begins its first `Start()` attempt, the new key is **promoted** onto the shared identity so the upcoming authentication uses it. The promotion is logged at INFO. After `Start()` has taken its option snapshot, later auth-keys are ignored with a WARN.
- `exit-node` and per-port forwards remain owned by their respective adapters: only outbound applies the exit-node preference, only inbound runs the forward listeners.
- On config reload, the registry keeps the instance alive for a short grace window (~5 seconds) after the last reference is dropped, so a follow-up `Acquire` with the same identity reuses the live tsnet and avoids redundant device re-registration on the control plane. If the grace timer fires while `Start()` is active, it waits and rechecks after another grace window.
- If the two entries differ on `state-dir` / `hostname` / `control-url`, they create distinct identities → two devices on the control plane (the pre-registry behavior).

### Inbound Service Forwarding

`listeners.type: tailscale` exposes local services on the node's Tailscale IPs. Each forward maps a tailnet-side port to a local target.

```yaml
listeners:
  - name: ts-services
    type: tailscale
    auth-key: tskey-auth-xxxxxxxxxxxxxxxxxxxxx
    hostname: mihomo-services
    # control-url: https://controlplane.tailscale.com
    # ephemeral: false
    # state-dir: /var/lib/mihomo/tailscale/mihomo-services
    # rule: custom-sub-rule
    # proxy: proxy-name
    forwards:
      - listen: 22
        target: 127.0.0.1:22
      - listen: 80
        target: 127.0.0.1:8080
      - listen: 53
        target: 127.0.0.1:53
        network: udp
```

### Inbound Option Reference

Listener-level options:

| Option | Required | Default | Description |
|---|---|---|---|
| `name` | Yes | — | Listener name. |
| `type` | Yes | — | Must be `tailscale`. |
| `auth-key` | First run | — | Tailscale auth key for the listener node. |
| `hostname` | No | `<name>` | Hostname shown in the Tailscale admin console. Identity anchor — match it to an outbound entry to share a single tsnet. |
| `control-url` | No | Official Tailscale | Custom control plane URL. |
| `ephemeral` | No | `false` | Creates an ephemeral listener node. |
| `accept-routes` | No | `true` | Node-level subnet-route acceptance. Defaults match outbound so a shared identity sees no first-wins conflict. Explicit `false` (on either side) will be honored under first-wins semantics. |
| `state-dir` | No | `<HomeDir>/tailscale/<hostname\|name>/` | State directory. Pair it with an outbound entry on the same directory to share the tsnet identity. |
| `rule` | No | — | Sub-rule name applied to forwarded traffic. |
| `proxy` | No | — | Global outbound override for all forwards on this listener. |
| `forwards` | Yes | — | List of tailnet-side ports to expose. |

Per-forward options:

| Option | Required | Default | Description |
|---|---|---|---|
| `listen` | Yes | — | Tailnet-side port to expose (e.g., `22`). |
| `target` | Yes | — | Local or routed destination in `host:port` form. |
| `network` | No | `auto` | `auto` = TCP + UDP. `tcp` or `udp` for a single protocol. |
| `proxy` | No | inherit listener `proxy` | Per-forward outbound override. |

### Forwarding Notes

- Forwards listen on the node's Tailscale IPs, not on the host's regular network interfaces.
- A single Tailscale listener node can expose many forwarded ports through one shared tsnet instance.
- Forwarding is explicit per port. Userspace tsnet does not provide TUN-style transparent interception.
- Bring-up is asynchronous: `Listen()` registers the listener and returns; a per-listener goroutine drives `Instance.Start()` and creates the TCP/UDP forwards. Until the goroutine succeeds, `Address()` returns the empty string and tailnet traffic to the configured ports is refused. Failures are logged as `Tailscale[<name>] bring-up attempt N failed: ...` and retried with exponential backoff; recovery is automatic once the underlying issue (auth-key, DNS, firewall) is resolved.

## Building

### Prerequisites

- **Go ≥ 1.26.1** (required by `tailscale.com v1.96.5`)

### Default Build (without Tailscale)

```bash
go build ./...
```

The binary is identical to upstream. Using `type: tailscale` in the config returns an error.

### Build with Tailscale

```bash
go build -tags with_tailscale ./...
```

### Production Build

```bash
CGO_ENABLED=0 go build \
  -tags "with_gvisor,with_tailscale" \
  -trimpath \
  -ldflags '-w -s -buildid=' \
  -o bin/mihomo .
```

### Cross-Compilation

```bash
# macOS ARM64
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build \
  -tags "with_gvisor,with_tailscale" \
  -trimpath -ldflags '-w -s -buildid=' \
  -o bin/mihomo-darwin-arm64 .

# macOS AMD64
GOOS=darwin GOARCH=amd64 GOAMD64=v3 CGO_ENABLED=0 go build \
  -tags "with_gvisor,with_tailscale" \
  -trimpath -ldflags '-w -s -buildid=' \
  -o bin/mihomo-darwin-amd64 .

# Linux AMD64
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build \
  -tags "with_gvisor,with_tailscale" \
  -trimpath -ldflags '-w -s -buildid=' \
  -o bin/mihomo-linux-amd64 .
```

### Verify Build

```bash
go build ./...                             # stub path
go build -tags with_tailscale ./...        # tailscale path
go vet -tags with_tailscale ./...          # static analysis
go test -tags with_tailscale ./adapter/outbound/ ./component/tailscalestamp/ -v
```

## Architecture

```
component/tailscale/server.go
├── ServerOptions             Identity + node-level prefs (incl. accept-routes)
├── Instance                  Refcounted wrapper around one tsnet.Server
├── Acquire(opts) → (Instance, releaseFn)
│                             Identity-keyed registry lookup; first call creates,
│                             subsequent calls increment a refcount and reuse
├── release / maybeClose      Refcount decrement; last release schedules close
│                             with a 5s grace window and waits for active Start()
├── applyNodePrefs            Syncs node-level prefs (accept-routes) after Up()
├── Start()                   Context-aware one-shot tsnet.Up() with option snapshot
│                             and resolver bypass
└── SanitizeHostname          Normalize to a valid DNS label

adapter/outbound/tailscale.go
├── TailscaleOption           YAML → Go struct mapping
├── Tailscale                 Embeds *Base, holds an *Instance + release fn
├── Warmup()                  Post-startup eager init with TryLock and lifecycle cancel
├── init()                    Context-aware one-time init + exit-node sync
├── syncExitNodePreference    Outbound-only preference; not part of identity
├── DialContext()             server.Dial("tcp") → backfill remote IP → NewConn()
├── ListenPacketContext()     pickTailscaleUDPBind() → server.ListenPacket("udp")
└── Close()                   cancel lifecycle + release shared Instance

listener/inbound/tailscale.go
├── TailscaleOption           Listener-level config + forwards list (incl. accept-routes)
├── TailscaleForward          Per-port forward entry
├── Listen()                  Acquire shared Instance → spawn async bring-up goroutine, return nil
├── runBringUp()              Lifecycle-bounded goroutine: Start + setupForwards with exp-backoff retry
├── setupForwards()           Create TCP/UDP forwards on tailnet ports (with rollback on partial failure)
├── openForwardUDP()          Bind concrete TS IPv4/IPv6 UDP sockets
└── Close()                   Cancel lifecycle, wait for goroutine, close listeners + release()
```

### Design Decisions

| Decision | Rationale |
|---|---|
| Build tag isolation | tsnet adds ~32 MB; not compiled by default |
| Lazy init (outbound) | First Dial triggers Up(), avoids blocking startup; warmup pre-flights after `OnRunning` |
| Async init (inbound) | `Listen()` returns immediately; the tsnet handshake runs in a per-listener goroutine with bounded per-attempt timeout and exponential backoff retry, so mihomo's main `ApplyConfig` never blocks on a slow / unreachable control plane. Trade-off: forwards are "eventually ready" instead of ready the instant `Listen()` returns |
| Identity-keyed registry + refcount | Outbound and inbound sharing one identity register exactly one device on the control plane; refcount lets each adapter own its own lifecycle without coordinating directly |
| 5s grace close on refcount = 0 | Config reload runs Close-then-Listen; the grace window prevents the tsnet from briefly dropping and re-registering |
| First-wins on conflicting node prefs (auth-key promotes on idle empty cache) | `ephemeral` / `accept-routes` / mismatched non-empty `auth-key` keep the original value with a WARN. The carve-out: an empty cached `auth-key` can be promoted from a follow-up Acquire supplying one before the first `Start()` attempt takes its option snapshot, with an INFO log. |
| Resolver bypass per process (refcounted) | tsnet uses stdlib resolver internally; the global bypass must outlive every active instance |
| Exit node as declarative state (outbound only) | Setting overwrites previous; omitting clears persisted state. Not part of identity since multiple outbounds *could* target different exit nodes with the same identity in principle |
| `accept-routes` lives on `ServerOptions` | Subnet acceptance is a node-level property; lifting it out of outbound lets inbound-only deployments use it too |
| Silent fallback when exit node unavailable | No ExitNodeID = direct routing; warning log is the only signal |
| State dir keyed by hostname or name | Human-readable layout with stable defaults; also the identity anchor |
| Inbound forwards default to `network: auto` | One entry exposes the same port on both TCP and UDP |
| Explicit per-port service exposure | Userspace tsnet only supports explicit listeners, not TUN-style interception |
| Runtime version stamp patching | Keeps control plane version display accurate; avoids `ERR-BuildInfo` |

## Keeping Up with Upstream

This fork tracks `upstream/Alpha`. The `tailscale-dev` branch contains all tailscale-related changes rebased on top.

### Rebase Workflow

```bash
git fetch upstream Alpha
git log --oneline HEAD..upstream/Alpha | head -20
git rebase upstream/Alpha

# After resolving conflicts:
go get tailscale.com@v1.96.5
go mod tidy
go build ./... && go build -tags with_tailscale ./...
```

### Conflict Resolution

| File | Rule |
|---|---|
| `go.mod` | Keep `go 1.26.1` |
| `constant/adapters.go` | `Tailscale` stays at the end of the enum |
| `adapter/parser.go` | `case "tailscale"` stays right before `default` |

### Upgrading Tailscale

```bash
go list -m -versions tailscale.com | tr ' ' '\n' | tail -10
curl -sL https://raw.githubusercontent.com/tailscale/tailscale/<new-tag>/go.mod | head -3
go get tailscale.com@<new-tag>
go mod tidy
go build ./... && go build -tags with_tailscale ./...
```

## Known Limitations

- `dialer-proxy` / `interface-name` / `routing-mark` / `ip-version` / `tfo` / `mptcp` do not take effect (tsnet manages its own sockets)
- First run requires `auth-key`; state is persisted afterward
- If the specified exit node is unavailable, traffic falls back silently to direct routing
- All `tailscale` proxy names must be unique within the same mihomo instance
- Inbound service forwarding is explicit per port; userspace tsnet does not provide transparent forwarding
- **Sharing is implicit by identity**: any two entries that resolve to the same `(state-dir, control-url, sanitized hostname-or-name)` share a tsnet instance. If you want them isolated, give them distinct hostnames or state directories.
- **First-wins on shared instances**: when two entries share an identity but disagree on node-level options (`ephemeral`, `accept-routes`, mismatched non-empty `auth-key`), the first `Acquire` wins; the conflict is logged at WARN level. The exception is an empty cached `auth-key` being promoted from a follow-up Acquire's bootstrap key before the first `Start()` attempt takes its option snapshot, with an INFO log.
- UPX does not support macOS binaries; use gzip for distribution

## Credits

- [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) — upstream project
- [Tailscale](https://tailscale.com/) — tsnet library
- [Dreamacro/clash](https://github.com/Dreamacro/clash) — original Clash project

## License

This software is released under the [GPL-3.0](LICENSE) license.
