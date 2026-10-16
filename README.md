<p align="center">
  <img src="Logo.png" alt="Interstellar Server Logo" width="160">
</p>

<h1 align="center">Interstellar Server</h1>

<p align="center">
  <b>English</b> | <a href="README.zh-CN.md">简体中文</a>
</p>

A Go rewrite of the BetterCrewLink signaling server (codename **Interstellar**):
a single static binary with zero runtime dependencies and a built-in Pion TURN
relay (the Logo.png is embedded into the binary via `go:embed` and served for
the homepage and favicon). Speaks both **Engine.IO v3 (old socket.io v2
clients)** and **Engine.IO v4 (new socket.io v4 clients)** natively.

> Wire-compatible with BetterCrewLink clients — every signaling event
> (`clientPeerConfig` / `join` / `signal` / `VAD` / `lobbybrowser` …) is a 1:1
> port of the original TS server semantics.

---

# Tutorial: Self-hosting a low-latency voice server from scratch

Goal: run this server + TURN relay on a VPS close to your players (for China:
mainland or Hong Kong/Japan) so that users get minimum latency, with optional
client-side TURN node selection by region.

## Step 1: Prepare a VPS

- Requirements are tiny: 1 vCPU / 512 MB RAM is plenty (signaling is cheap;
  TURN costs bandwidth — a fully-talking 12-player lobby uses ~3–5 Mbps up).
- Location: as close to your players as possible. **Do not put TURN in
  Europe/US** — the official German TURN measured 436 ms RTT with packet loss
  from China, effectively unusable.
- Open these ports (both in the cloud security group **and** the OS firewall):

| Port | Protocol | Purpose |
|---|---|---|
| 9736 (or your `PORT`) | TCP | Signaling (Socket.IO) |
| 3478 | UDP | TURN/STUN main port |
| 49152–65535 (narrow it, e.g. 49152–49255) | UDP | TURN relay port range |

## Step 2: Build the binary

On a machine with Go (1.21+):

```bash
git clone <this project> && cd Interstellar-server
go build -o interstellar-server.exe .     # Windows
go build -o interstellar-server .         # Linux/macOS
```

No Go toolchain? Build once anywhere (or grab a binary from GitHub Actions /
Releases) and copy it to the VPS — there are no other runtime dependencies.

## Step 3: Write the config

`config/peerConfig.yml` (see `config/peerConfig.example.yml`; recommended setup):

```yaml
forceRelayOnly: false          # keep false: P2P first, relay only as fallback

integratedRelay:               # built-in TURN (Pion), same host as signaling
  enabled: true
  listeningIps: ['0.0.0.0']
  externalIps: ['YOUR_PUBLIC_IP']   # required behind NAT! auto-detect works but may guess wrong on multi-NIC hosts
  minPort: 49152
  maxPort: 49255                # must match the UDP range opened in your security group
  listeningPort: 3478
  debugLevel: 'INFO'
  defaultUsername: 'random-long-string'
  defaultPassword: 'another-random-string'

iceServers:                    # STUN/TURN list handed to clients
  - urls: 'stun:stun.miwifi.com:3478'
    region: 'cn'
  - urls: 'stun:stun.qq.com:3478'
    region: 'cn'
  - urls: 'turn:your.domain:3478'      # you can also list the built-in TURN here for manual selection
    username: 'same as defaultUsername'
    credential: 'same as defaultPassword'
    region: 'cn'
  # - urls: 'turn:hk.example.com:3478'  # optional failover node; later entries get lower priority
  #   username: 'x'
  #   credential: 'y'
  #   region: 'hk'
```

Hard-won notes:

- **STUN order is meaningless** (libwebrtc stores them in a `std::set`, queries
  all in parallel, dedupes by reflexive address) — list 2–3 for redundancy.
  **Never list a dead STUN server**: gathering waits for every STUN server to
  succeed or fail, and a dead one can delay each connection by up to ~16.5 s.
- **TURN list order = relay candidate priority** (first entry wins) — put the
  lowest-latency node first.
- If you narrow `maxPort`, update the security group accordingly, otherwise TURN
  will hand out relay addresses that never pass data (silent failure).

## Step 4: Run

```bash
# Linux
HOSTNAME=bcl.yourdomain.com ./interstellar-server -config config/peerConfig.yml

# Windows
set HOSTNAME=bcl.yourdomain.com
interstellar-server.exe -config config\peerConfig.yml
```

`HOSTNAME` is **required** when `integratedRelay` is enabled (clients use it to
reach the TURN server).

For a Linux daemon, use systemd (`/etc/systemd/system/interstellar.service`):

```ini
[Unit]
Description=Interstellar Server
After=network.target

[Service]
WorkingDirectory=/opt/interstellar
Environment=HOSTNAME=bcl.yourdomain.com
Environment=PORT=9736
ExecStart=/opt/interstellar/interstellar-server -config config/peerConfig.yml
Restart=always

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload && systemctl enable --now interstellar
```

## Step 5: Verify (server side)

```bash
# 1) process is alive
curl http://127.0.0.1:9736/health
# -> {"uptime":..., "connectionCount":0, "lobbiesCount":0, ..., "version":"..."}

# 2) full protocol flow + region passthrough (point the scripts at your server)
python _test/test_client.py     # EIO=4: 13 checks
python _test/test_eio3.py       # EIO=3 old-client compat: 12 checks
python _test/test_turn.py       # TURN auth + relay port-range check: 6 checks (edit HOST/creds at the top)
```

All three PASS = server side is ready. Running the scripts from an external
machine also validates your security group rules (an Allocate timeout from the
outside means UDP 3478 or the relay range is blocked).

## Step 6: Connect clients

**Basic (no client changes)**: download the
[Interstellar client](https://github.com/Fungle-Galaxy/Interstellar), set the
server address to `bcl.yourdomain.com:9736` in Settings. Users who fail hole
punching automatically fall back to the TURN servers in your list (chosen by
list order).

**Advanced (region-based TURN selection + NAT-fix optimization, requires a
client rebuild)**: see [Client modification guide](#client-modification-guide-region-based-turn-selection--nat-fix-optimization) below.

## Step 7 (optional): External coturn instead of the built-in TURN

For large deployments (>50 concurrent users) or a TLS 443 escape hatch
(`turns:`, for networks that only allow port 443), use coturn:

```bash
apt install coturn
# /etc/turnserver.conf
listening-port=3478
tls-listening-port=5349
fingerprint
lt-cred-mech
user=bcluser:same-password-as-in-yml
realm=bcl.yourdomain.com
no-multicast-peers
min-port=49152
max-port=49255
# behind NAT also add: external-ip=YOUR_PUBLIC_IP
systemctl enable --now coturn
```

Then set `integratedRelay.enabled: false` in the YML, list coturn under
`iceServers` (optionally with `?transport=tcp` and `turns:domain:443` entries),
and tag every entry with a `region`.

---

# Client modification guide (region-based TURN selection + NAT-fix optimization)

Background: once WebRTC receives the `iceServers` array, **TURN order = relay
candidate priority** (libwebrtc `basic_port_allocator.cc`,
`relative_priority--`) and it has **no latency-aware routing at all**. Picking
the lowest-latency TURN must therefore happen on the client; the server only
hands out the menu (region tags).

## 1. Zero server-side changes

The client's `validateClientPeerConfig.ts` (AJV) does not set
`additionalProperties: false`, so `region` fields flow straight into
`this.iceConfig` — **no validation changes needed**.

## 2. Setting (settingsStore.ts)

```ts
preferredTurnRegion: 'auto' as 'auto' | string,   // 'auto' or a region from the YML
```

## 3. Selection + probing (new file lib/turnSelect.ts)

```ts
// Dumb RTCPeerConnection probe: time-to-first-relay-candidate for one TURN server
function probeTurn(server: RTCIceServer, timeout = 2500): Promise<number> {
  const pc = new RTCPeerConnection({ iceTransportPolicy: 'relay', iceServers: [server] });
  const t0 = performance.now();
  return new Promise((resolve) => {
    const timer = setTimeout(() => { pc.close(); resolve(Infinity); }, timeout);
    pc.createDataChannel('probe');
    pc.onicecandidate = (e) => {
      if (e.candidate?.type === 'relay') {
        clearTimeout(timer); const rtt = performance.now() - t0; pc.close(); resolve(rtt);
      }
    };
    pc.createOffer().then((o) => pc.setLocalDescription(o));
  });
}

export async function pickIceConfig(base: RTCConfiguration): Promise<RTCConfiguration> {
  const pref = SettingsStore.store.preferredTurnRegion;
  const servers = base.iceServers ?? [];
  const turns = servers.filter((s) => [].concat(s.urls as any).some((u: string) => u.startsWith('turn')));
  const others = servers.filter((s) => !turns.includes(s));

  if (pref !== 'auto') {
    // Manual mode: keep only the TURN servers of the chosen region (most deterministic)
    const kept = turns.filter((s) => (s as any).region === pref);
    return { ...base, iceServers: [...others, ...(kept.length ? kept : turns)] };
  }

  // Auto mode: probe in parallel -> sort by RTT ascending -> keep top 2 (drop unreachable)
  const rtts = await Promise.all(turns.map((t) => probeTurn(t)));
  const ranked = turns
    .map((t, i) => ({ t, rtt: rtts[i] }))
    .filter((x) => x.rtt < Infinity)
    .sort((a, b) => a.rtt - b.rtt)
    .slice(0, 2)
    .map((x) => x.t);
  return { ...base, iceServers: [...others, ...ranked] };
}
```

## 4. Integration point (ConnectionController.ts, around line 428)

```ts
// Probe once when clientPeerConfig arrives and cache the result
socket.on('clientPeerConfig', async (cfg) => {
  this.iceConfig = { iceTransportPolicy: cfg.forceRelayOnly ? 'relay' : 'all',
                     iceServers: cfg.iceServers };
  this.rankedIceConfig = await pickIceConfig(this.iceConfig);   // cache
});

// When creating peer connections
const config = natFixEnabled
  ? { ...this.rankedIceConfig, iceTransportPolicy: 'relay' }  // NAT fix = relay-only but reuse the ranking
  : this.rankedIceConfig;
```

**Important**: do not hardcode a separate TURN server for NAT fix (the original
BCL flaw) — reuse the same ranked config and only add
`iceTransportPolicy: 'relay'`, so natFix users also go through the nearest
node. Stock BCL's natFix pins the official German TURN (436 ms relay measured);
that must be changed for self-hosted setups.

## 5. UI

Add a "Relay node" dropdown in settings whose options are `'auto'` plus every
`region` value collected from `clientPeerConfig.iceServers` (generated
dynamically, so adding nodes later requires no client change).

## 6. Failure escalation (optional)

After 2 consecutive `retryPeer` failures, automatically switch to
`{ ...ranked, iceTransportPolicy: 'relay' }` — i.e. turn forced-relay on by
yourself, fixing the original "retry forever with the same broken config"
behavior.

---

# Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Clients can't connect, /health fine | PORT not open to the internet / wrong address format | Open TCP in the security group; address must be `domain:port` (client requires pathname `/`) |
| TURN Allocate times out externally | UDP 3478 or relay range blocked | Open UDP 3478 + minPort–maxPort |
| Allocate returns 400/401 | Credential mismatch | YML username/credential must match the TURN side |
| Relay address is a private IP (all punching fails) | No externalIps behind NAT | Set `externalIps: ['PUBLIC_IP']` and restart |
| Someone takes forever to speak after joining | Dead STUN server in the YML stalls gathering | Remove unreachable STUN entries |
| Latency spikes after enabling NAT fix | Client hardcodes a foreign TURN | Modify the client as above so natFix reuses this server's config |
| Both peers behind symmetric NAT still fail | Symmetric candidates can't punch | Make sure at least one TURN is reachable (relay pair is the final fallback) |
| Integrated TURN refuses to start, mentions HOSTNAME | HOSTNAME env var missing | Required when integratedRelay is enabled |
| YML changes not taking effect | Process not restarted / file corrupted by PowerShell ANSI encoding | Restart; save YML as UTF-8 |

---

# Configuration reference

See the commented `config/peerConfig.example.yml`. Environment variables:
`HOSTNAME` (public domain/IP of the integrated TURN, required when enabled),
`PORT`, `HTTPS=1` + `SSLPATH` (directory with fullchain.pem/privkey.pem),
`NAME` (shown in /health), `PEER_CONFIG` (config path, same as -config).

HTTP routes: `/socket.io/` (signaling), `/` (status page — English by default,
Simplified Chinese auto-detected from `Accept-Language`, switchable via
`?lang=en` / `?lang=zh` which is remembered in a cookie), `/health`,
`/lobbies`, `/public/`.

### Behavior differences vs. the original TS server

| Aspect | Original (socket.io 2.4.1 + node-turn) | Interstellar |
|---|---|---|
| Protocol | EIO=3 only | Dual-stack EIO=3 + EIO=4 |
| Config merge | Whole-file replacement (crashes if integratedRelay missing) | Missing fields fall back to defaults |
| Integrated TURN | node-turn | Pion TURN; auto-detects the public IP when externalIps is empty |
| join_lobby callback | Could fire twice (fallthrough bug) | Fixed, replies once |
| region field | Accidental YAML-parser passthrough | First-class structured field |
| iceServers[].urls | string/string[] | Same, plus scheme validation (stun/stuns/turn/turns) |

Signaling event semantics match the original: `clientPeerConfig`/`join`/
`setClients`/`setHost`/`setClient`/`signal`/`VAD`/`lobby`/`join_lobby`(ack)/
`lobbybrowser`/`remove_lobby`/`leave`/`id`/`disconnect`.

---

# Testing

Three scripts under `_test/` (start the server with
`config/peerConfig.test.yml`, HOSTNAME=127.0.0.1):

```bash
python _test/test_client.py   # EIO=4 full flow: clientPeerConfig/region/join/signal/lobbybrowser/ack/health — 13 checks
python _test/test_eio3.py     # EIO=3 (old clients): handshake/framing/40 negotiation/ws upgrade/events — 12 checks
python _test/test_turn.py     # Integrated TURN: 401 challenge -> authenticated Allocate -> relay IP/port-range check — 6 checks
```

Results (Windows, 2026-08): all three PASS. One real bug was found and fixed
this way: Pion's `AuthHandler` returns the **raw HMAC key**, so for long-term
credentials you must return `MD5(user:realm:pass)`, not the plaintext password
(otherwise every real client fails authentication).

## Advanced security: dynamic TURN credentials (optional hardening)

Static credentials are handed to every client and can be replayed to leech
relay bandwidth. Hardening path: switch coturn/the server to
`use-auth-secret` + `static-auth-secret` and have the signaling server mint
short-TTL temporary credentials (e.g. 1 hour) when sending `clientPeerConfig`,
so the client binary contains zero credentials and leaks expire by themselves.
