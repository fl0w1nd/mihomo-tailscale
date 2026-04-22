<h1 align="center">
  <img src="Meta.png" alt="Meta Kernel" width="200">
  <br>mihomo + Tailscale<br>
</h1>

<h3 align="center">mihomo fork with built-in Tailscale outbound proxy support via tsnet.</h3>

<p align="center">
  <a href="https://github.com/MetaCubeX/mihomo">
    <img src="https://img.shields.io/badge/upstream-MetaCubeX%2Fmihomo-blue?style=flat-square">
  </a>
  <img src="https://img.shields.io/badge/go-%E2%89%A51.26.1-00ADD8?style=flat-square">
  <img src="https://img.shields.io/badge/tailscale.com-v1.96.5-005eff?style=flat-square">
</p>

## Overview

This is a personal fork of [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) (based on the `Alpha` branch) that adds **`type: tailscale`** as a first-class outbound proxy adapter. It embeds [tsnet](https://pkg.go.dev/tailscale.com/tsnet) so that any Tailscale node can be used as a regular outbound in mihomo's rule system — without running a separate `tailscaled` daemon.

All upstream features remain intact. The Tailscale integration is isolated behind the `with_tailscale` build tag, so the default binary is completely unchanged.

## What's Different from Upstream

| Area | Upstream mihomo | This Fork |
|---|---|---|
| Tailscale outbound | Not available | `type: tailscale` proxy adapter via tsnet |
| Go version | `go 1.20` | `go 1.26.1` (required by tailscale.com v1.96.5) |
| Binary size | Baseline | +~32 MB when built with `with_tailscale` tag |
| Default build | Unaffected | Identical — stub returns an error if tailscale type is used |

### New Files

| File | Purpose |
|---|---|
| `adapter/outbound/tailscale.go` | Core adapter (`//go:build with_tailscale`) |
| `adapter/outbound/tailscale_stub.go` | Stub (`//go:build !with_tailscale`) — returns error |
| `adapter/outbound/tailscale_test.go` | Unit tests for hostname sanitization, peer matching, option validation |
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
| `constant/features/tags.go` | `with_tailscale` appears in `mihomo -v` output |
| `docs/config.yaml` | Added tailscale configuration example |

## Configuration

### Minimal Example

```yaml
proxies:
  - name: ts-exit
    type: tailscale
    auth-key: tskey-auth-xxxxxxxxxxxxxxxxxxxxx
    hostname: mihomo-exit
```

### Full Example

```yaml
proxies:
  - name: ts-exit
    type: tailscale
    auth-key: tskey-auth-xxxxxxxxxxxxxxxxxxxxx  # Required on first run; can be removed after state is persisted
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

### Option Reference

| Option | Required | Default | Description |
|---|---|---|---|
| `name` | Yes | — | Unique proxy name. Must be unique across all tailscale proxies in the same mihomo instance. |
| `type` | Yes | — | Must be `tailscale`. |
| `auth-key` | First run | — | Tailscale auth key (`tskey-auth-...`). Required for initial authentication; once state is persisted, it can be removed. |
| `hostname` | No | `<name>` | Hostname visible in Tailscale admin console. Sanitized to DNS label format automatically. |
| `control-url` | No | Official Tailscale | Custom control plane URL (e.g. for Headscale). |
| `ephemeral` | No | `false` | If `true`, the node is automatically removed from the tailnet 30–60 minutes after going offline. |
| `exit-node` | No | — | Specifies an exit node to route all traffic through. Accepts FQDN, HostName, Tailscale IP, or StableNodeID. Omitting this clears any persisted exit node selection. |
| `state-dir` | No | `<HomeDir>/tailscale/<name>/` | Directory for persisting Tailscale node state (keys, preferences, etc.). |

### Options That Do NOT Apply

The following `BasicOption` fields are silently ignored because tsnet manages its own sockets:

`dialer-proxy`, `interface-name`, `routing-mark`, `ip-version`, `tfo`, `mptcp`

A warning is logged at startup if any of these are set.

### Operational Notes

- **First run** requires `auth-key`. After the node state is persisted to `state-dir`, the key can be removed from the config.
- **Long-lived nodes** should have "Disable key expiry" enabled in the Tailscale admin console.
- **Exit node fallback**: If the specified exit node is unavailable at startup, the adapter falls back to direct routing through the Tailscale node and logs a warning. It does **not** automatically pick another exit node.
- **Lazy initialization**: The tsnet server is started on the first `Dial`/`ListenPacket` call, not at mihomo startup. This avoids blocking the start of other proxies.
- **Multiple tailscale proxies**: You can define multiple `type: tailscale` proxies in the same mihomo instance. Each must have a unique `name` (which determines its state directory).

## Building

### Prerequisites

- **Go ≥ 1.26.1** (required by `tailscale.com v1.96.5`)

### Default Build (without Tailscale)

```bash
go build ./...
```

The binary is identical to upstream. Using `type: tailscale` in the config will return an error.

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

### Cross-Compilation Examples

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
adapter/outbound/tailscale.go
├── TailscaleOption        YAML → Go struct mapping
├── Tailscale struct       Embeds *Base, holds *tsnet.Server
├── init()                 Double-checked locking lazy init (mirrors wireguard.go)
│   ├── Create stateDir (<HomeDir>/tailscale/<name>/)
│   ├── Acquire stdlib resolver bypass (held until Close)
│   ├── tsnet.Server.Up()
│   └── syncExitNodePreference()
├── setupExitNode()        LocalClient().EditPrefs() → set ExitNodeID
├── clearExitNode()        Clear persisted ExitNodeID/IP/AutoExitNode
├── resolveExitNode()      Match peer by ID / DNSName / HostName / IP
├── DialContext()          server.Dial("tcp") → backfill remote IP → NewConn()
├── ListenPacketContext()  server.Dial("udp") → fakePacketConn wrapper
├── Close()                server.Close() + release resolver bypass
└── IsL3Protocol()         true (same as WireGuard)
```

### Design Decisions

| Decision | Rationale |
|---|---|
| Build tag isolation | tsnet adds ~32 MB; not compiled by default |
| Lazy initialization | First Dial triggers Up(), avoids blocking startup |
| Resolver bypass held per instance lifetime | tsnet continues to use stdlib resolver during operation |
| Exit node as declarative state | Setting overwrites previous; omitting clears persisted state |
| Silent fallback when exit node unavailable | No ExitNodeID = direct routing; warning log is the only signal |
| State dir isolated by name | Multiple tailscale proxies don't conflict |
| UDP via Dial("udp") + fakePacketConn | tsnet's ListenPacket is for inbound; Dial("udp") suits outbound |
| Runtime version stamp patching | Keeps control plane version display correct; avoids `ERR-BuildInfo` |

## Keeping Up with Upstream

This fork tracks `upstream/Alpha`. The `tailscale-dev` branch contains all tailscale-related changes rebased on top.

### Rebase Workflow

```bash
git fetch upstream Alpha
git log --oneline HEAD..upstream/Alpha | head -20  # check what's new
git rebase upstream/Alpha

# After resolving conflicts:
go get tailscale.com@v1.96.5
go mod tidy
go build ./... && go build -tags with_tailscale ./...
```

### Conflict Resolution Cheat Sheet

| File | Rule |
|---|---|
| `go.mod` | Keep `go 1.26.1` (don't regress to upstream's 1.20) |
| `constant/adapters.go` | `Tailscale` stays at the end of the enum |
| `adapter/parser.go` | `case "tailscale"` stays right before `default` |

### Upgrading Tailscale Version

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
- UPX does not support macOS binaries; use gzip for distribution

## Credits

- [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) — upstream project
- [Tailscale](https://tailscale.com/) — tsnet library
- [Dreamacro/clash](https://github.com/Dreamacro/clash) — original Clash project

## License

This software is released under the [GPL-3.0](LICENSE) license.
