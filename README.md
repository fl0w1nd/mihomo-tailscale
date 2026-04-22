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

All upstream features remain intact. The Tailscale integration is isolated behind the `with_tailscale` build tag — the default binary is identical to upstream.

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
| `component/tailscale/server.go` | Shared tsnet lifecycle management |
| `component/tailscale/server_stub.go` | Stub for the shared lifecycle layer |
| `listener/inbound/tailscale.go` | Tailscale listener with service forwards |
| `listener/inbound/tailscale_stub.go` | Stub for tailscale listener support |
| `listener/inbound/tailscale_test.go` | Tests for forward startup and rollback paths |
| `component/tailscalestamp/stamp.go` | Patches Tailscale version stamp at runtime to avoid `ERR-BuildInfo` |
| `component/tailscalestamp/stamp_test.go` | Verifies version stamp is correctly populated |
| `constant/features/with_tailscale.go` | Feature flag `WithTailscale = true` |
| `constant/features/with_tailscale_stub.go` | Feature flag `WithTailscale = false` |

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
| `state-dir` | No | `<HomeDir>/tailscale/<hostname\|name>/` | Directory for persisting node state. |

### Inapplicable Options

The following `BasicOption` fields are ignored because tsnet manages its own sockets:

`dialer-proxy` · `interface-name` · `routing-mark` · `ip-version` · `tfo` · `mptcp`

A warning is logged at startup if any of these are set.

### Operational Notes

- **First run** requires `auth-key`. After state is persisted to `state-dir`, the key can be removed.
- **Long-lived nodes** should have key expiry disabled in the Tailscale admin console.
- **Exit node fallback**: if the specified exit node is unavailable, the adapter falls back to direct routing and logs a warning. It does not automatically select another exit node.
- **Lazy initialization**: the tsnet server starts on the first `Dial`/`ListenPacket` call, not at mihomo startup.
- **Multiple proxies**: each `type: tailscale` proxy runs an independent tsnet instance with its own state.

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
| `hostname` | No | `<name>` | Hostname shown in the Tailscale admin console. |
| `control-url` | No | Official Tailscale | Custom control plane URL. |
| `ephemeral` | No | `false` | Creates an ephemeral listener node. |
| `state-dir` | No | `<HomeDir>/tailscale/<hostname\|name>/` | State directory for this listener node. |
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
├── Instance              Shared tsnet lifecycle manager
├── Start()               Create state dir → resolver bypass → tsnet.Up()
└── Close()               server.Close() + release resolver bypass

adapter/outbound/tailscale.go
├── TailscaleOption       YAML → Go struct mapping
├── Tailscale             Embeds *Base, holds *tailscale.Instance
├── init()                Double-checked one-time init + exit-node sync
├── setupExitNode()       LocalClient().EditPrefs() → set ExitNodeID
├── clearExitNode()       Clear persisted ExitNodeID/IP/AutoExitNode
├── resolveExitNode()     Match peer by ID / DNSName / HostName / IP
├── DialContext()         server.Dial("tcp") → backfill remote IP → NewConn()
├── ListenPacketContext() pickTailscaleUDPBind() → server.ListenPacket("udp")
└── Close()               instance.Close()

listener/inbound/tailscale.go
├── TailscaleOption       Listener-level config + forwards list
├── TailscaleForward      Per-port forward entry
├── Listen()              Start shared tsnet instance → create forwards
├── listenOnServer()      Create TCP/UDP forwards on tailnet ports
├── listenForwardUDP()    Bind concrete TS IPv4/IPv6 UDP sockets
└── Close()               Close all forward listeners + instance
```

### Design Decisions

| Decision | Rationale |
|---|---|
| Build tag isolation | tsnet adds ~32 MB; not compiled by default |
| Lazy init (outbound) | First Dial triggers Up(), avoids blocking startup |
| Eager init (inbound) | Ports must be ready before traffic arrives |
| Resolver bypass per instance | tsnet uses stdlib resolver internally; bypass must span the server lifetime |
| Exit node as declarative state | Setting overwrites previous; omitting clears persisted state |
| Silent fallback when exit node unavailable | No ExitNodeID = direct routing; warning log is the only signal |
| State dir keyed by hostname or name | Human-readable layout with stable defaults |
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
- UPX does not support macOS binaries; use gzip for distribution

## Credits

- [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) — upstream project
- [Tailscale](https://tailscale.com/) — tsnet library
- [Dreamacro/clash](https://github.com/Dreamacro/clash) — original Clash project

## License

This software is released under the [GPL-3.0](LICENSE) license.
