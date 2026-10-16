<p align="center">
  <img src="Logo.png" alt="Interstellar Server Logo" width="160">
</p>

<h1 align="center">Interstellar Server</h1>

<p align="center">
  <a href="README.md">English</a> | <b>简体中文</b>
</p>

BetterCrewLink 信令服务器的 Go 重写版（项目代号 Interstellar），单二进制、零运行时依赖，
内置 Pion TURN 中继（Logo.png 已通过 go:embed 打进二进制，主页与 favicon 直接使用）。
协议上同时兼容 **Engine.IO v3（socket.io v2 旧客户端）** 和
**Engine.IO v4（socket.io v4 新客户端）**。

> 本服务器与 BetterCrewLink 客户端协议完全兼容（`clientPeerConfig` / `join` / `signal` /
> `VAD` / `lobbybrowser` 等全部信令事件 1:1 复刻原版 TS 服务器语义）。

---

# 教程：从零搭建国内自建 BCL 语音服务器

目标：一台国内/香港 VPS，跑起本服务器 + TURN 中继，让国内玩家以最低延迟通话，
并支持客户端按 region 自动/手动选 TURN 节点。

## 第 1 步：准备 VPS

- 配置要求极低：1 核 / 512MB 内存即可（信令本身几乎不吃资源；TURN 吃带宽，
  一个 12 人房全员说话约 3~5 Mbps 出流量）。
- 位置建议：国内（延迟最低）或香港/日本（免备案）。**不要把 TURN 放欧美**——
  实测德国官方 TURN 中继往返 436ms 且丢包，国内用户体验等于不可用。
- 需要放行的端口（云控制台安全组 + 防火墙都要放行）：

| 端口 | 协议 | 用途 |
|---|---|---|
| 9736（或你指定的 PORT） | TCP | 信令（Socket.IO） |
| 3478 | UDP | TURN/STUN 主端口 |
| 49152-65535（建议收窄，如 49152-49255） | UDP | TURN 中继端口段 |

## 第 2 步：构建二进制

有 Go 环境（1.21+）的机器上：

```bash
git clone <本项目> && cd Interstellar-server
go build -o interstellar-server.exe .     # Windows
go build -o interstellar-server .         # Linux/macOS
```

无 Go 环境：在任何机器上构建一次（或从 GitHub Actions / Releases 下载），
把二进制拷到 VPS 即可（无其他运行时依赖）。

## 第 3 步：编写配置

`config/peerConfig.yml`（参考 `config/peerConfig.example.yml`，国内推荐写法）：

```yaml
forceRelayOnly: false          # 保持 false：P2P 优先，打洞失败才走中继

integratedRelay:               # 内置 TURN（Pion），与信令同机，省一套 coturn
  enabled: true
  listeningIps: ['0.0.0.0']
  externalIps: ['你的VPS公网IP']   # SNAT 后必填！留空虽能自动探测，多网卡/NAT 时可能不准
  minPort: 49152
  maxPort: 49255                # 与安全组放行的 UDP 段一致
  listeningPort: 3478
  debugLevel: 'INFO'
  defaultUsername: '换成随机长字符串'
  defaultPassword: '换成另一个随机长字符串'

iceServers:                    # 下发给客户端的 STUN/TURN 列表
  - urls: 'stun:stun.miwifi.com:3478'    # 小米 STUN，国内稳
    region: 'cn'
  - urls: 'stun:stun.qq.com:3478'
    region: 'cn'
  - urls: 'turn:你的域名或IP:3478'       # ← 内置 TURN 也可以同时写进列表做手动选路
    username: '与 defaultUsername 相同'
    credential: '与 defaultPassword 相同'
    region: 'cn'
  # - urls: 'turn:hk.example.com:3478'   # 可选：异地容灾节点，排在后面优先级自动更低
  #   username: 'x'
  #   credential: 'y'
  #   region: 'hk'
```

要点（都是踩过坑的结论）：

- **STUN 顺序无意义**（libwebrtc 用 `std::set` 存储，全并行查询、按反射地址去重），
  放 2~3 个只为冗余；**千万别放死掉的 STUN**——gathering 要等所有 STUN 成功/失败才结束，
  死地址会拖慢每个连接最多约 16.5 秒。
- **TURN 列表顺序 = relay 候选优先级**（第一个最高），把延迟最低的节点排第一。
- `maxPort` 收窄后务必同步改安全组，否则 TURN 分配出 relay 地址但数据不通（静默失效）。

## 第 4 步：启动

```bash
# Linux
HOSTNAME=bcl.yourdomain.com ./interstellar-server -config config/peerConfig.yml

# Windows
set HOSTNAME=bcl.yourdomain.com
interstellar-server.exe -config config\peerConfig.yml
```

`HOSTNAME` 在启用 integratedRelay 时**必填**（客户端要用它连 TURN）。

Linux 建议配 systemd 常驻（`/etc/systemd/system/interstellar.service`）：

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

## 第 5 步：验证（服务器端）

```bash
# 1) 信令进程活着
curl http://127.0.0.1:9736/health
# -> {"uptime":..., "connectionCount":0, "lobbiesCount":0, ..., "version":"..."}

# 2) 协议全流程 + region 透传（把脚本里的 URL 改成你的服务器）
python _test/test_client.py     # EIO=4：13 项
python _test/test_eio3.py       # EIO=3 旧客户端兼容：12 项
python _test/test_turn.py       # TURN 鉴权 + relay 端口段校验：6 项（脚本头部改 HOST/账密）
```

三项全 PASS 即服务器端就绪。从外网机器跑测试脚本可以顺带验证安全组放行是否正确
（外网 Allocate 超时 = UDP 3478 或端口段没通）。

## 第 6 步：客户端接入

**基础接入（不改客户端）**：下载 [Interstellar 客户端](https://github.com/Fungle-Galaxy/Interstellar)，
设置 → 服务器地址填 `bcl.yourdomain.com:9736`，即可正常语音。
此时打洞失败的用户会自动落到你列表里的 TURN（按列表顺序选）。

**进阶改造（region 选路 + NAT 修复优化，需重打包客户端）**：见下文
[客户端改造教程](#客户端改造教程region-选路--nat-修复优化)。

## 第 7 步（可选）：外接 coturn 替代内置 TURN

大规模（>50 人常驻）或需要 TLS 443 逃生通道（`turns:`，穿透只放行 443 的企业网）时，
建议外接 coturn：

```bash
apt install coturn
# /etc/turnserver.conf
listening-port=3478
tls-listening-port=5349
fingerprint
lt-cred-mech
user=bcluser:与YML相同的密码
realm=bcl.yourdomain.com
no-multicast-peers
min-port=49152
max-port=49255
# NAT 后追加: external-ip=你的公网IP
systemctl enable --now coturn
```

然后 YML 里 `integratedRelay.enabled: false`，把 coturn 写进 `iceServers`（可加
`?transport=tcp` 和 `turns:域名:443` 条目），记得给每条标 `region`。

---

# 客户端改造教程（region 选路 + NAT 修复优化）

前提知识：WebRTC 拿到 `iceServers` 数组后，**TURN 顺序 = relay 候选优先级**
（libwebrtc `basic_port_allocator.cc` 的 `relative_priority--`），且**没有任何延迟选路能力**。
所以"选延迟最优的 TURN"必须在客户端做，服务器只负责把菜单（region 标签）发下去。

## 1. 服务端零改动

客户端的 `validateClientPeerConfig.ts`（AJV）没有 `additionalProperties: false`，
YML 里的 region 字段会原样进入 `this.iceConfig`，**无需改校验**。

## 2. 设置项（settingsStore.ts）

```ts
preferredTurnRegion: 'auto' as 'auto' | string,   // 'auto' 或 YML 里的某个 region
```

## 3. 选择 + 探测排序（新建 lib/turnSelect.ts）

```ts
// 哑 RTCPeerConnection 探针：测"到该 TURN 的 relay 候选可用时间"
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
    // 手动模式：只保留用户选中 region 的 TURN（确定性最强）
    const kept = turns.filter((s) => (s as any).region === pref);
    return { ...base, iceServers: [...others, ...(kept.length ? kept : turns)] };
  }

  // 自动模式：并行探测 → 按 RTT 升序 → 保留前 2 个（探测失败的剔除）
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

## 4. 接入点（ConnectionController.ts ~428 行）

```ts
// 收到 clientPeerConfig 时缓存一次探测结果
socket.on('clientPeerConfig', async (cfg) => {
  this.iceConfig = { iceTransportPolicy: cfg.forceRelayOnly ? 'relay' : 'all',
                     iceServers: cfg.iceServers };
  this.rankedIceConfig = await pickIceConfig(this.iceConfig);   // 缓存
});

// 建连时
const config = natFixEnabled
  ? { ...this.rankedIceConfig, iceTransportPolicy: 'relay' }  // NAT 修复 = 强制中继但复用选路
  : this.rankedIceConfig;
```

**重要**：NAT 修复不要再硬编码独立 TURN（原版缺陷）——复用同一份已排序配置，
只加 `iceTransportPolicy: 'relay'`，这样 natFix 用户同样走最近的节点。
原版 BCL 的 natFix 写死官方德国 TURN（实测中继 436ms），自建服场景下必须改掉。

## 5. UI

设置里加"中继节点"下拉框，选项 = `'auto'` + 从 `clientPeerConfig.iceServers`
收集到的所有 `region` 值（动态生成，以后加节点不用改客户端）。

## 6. 失败升级（可选闭环）

`retryPeer` 连续失败 2 次后自动切到 `{ ...ranked, iceTransportPolicy: 'relay' }`，
即自动打开"强制中继"，修复原版"失败后永远用同一配置死磕"的问题。

---

# 常见问题排查

| 症状 | 原因 | 处理 |
|---|---|---|
| 客户端连不上，/health 正常 | PORT 未对公网放行 / 客户端地址格式错 | 安全组放 TCP；地址填 `域名:端口`（客户端要求 pathname 为 `/`） |
| TURN Allocate 从外网超时 | UDP 3478 或中继端口段没放行 | 安全组放行 UDP 3478 + minPort-maxPort |
| Allocate 返回 400/401 | 账密不符；或自建 coturn 时凭证格式问题 | YML 的 username/credential 必须与 TURN 侧一致 |
| relay 地址是内网 IP（打洞全败） | SNAT 后没配 externalIps | 写 `externalIps: ['公网IP']` 并重启 |
| 有人进房半天才能说话 | YML 里有死 STUN 地址拖慢 gathering | 删掉探测不通的 STUN（用 `bcl_probe.py` 逐个测） |
| 开 NAT 修复后延迟暴涨 | 客户端硬编码了国外 TURN | 按上文改造客户端，natFix 复用本服配置 |
| 双方都是对称 NAT 仍连不上 | 普通 TURN 候选对无法打洞 | 确认至少一个 TURN 可达（relay pair 是最终兜底） |
| 集成 TURN 启动报错"HOSTNAME" | 未设置 HOSTNAME 环境变量 | 启用 integratedRelay 时必填 |
| 改了 YML 没生效 | 进程未重启 / 文件编码被 PowerShell ANSI 搞坏 | 重启；用 UTF-8 保存 YML |

---

# 配置文件参考

见 `config/peerConfig.example.yml` 的详细注释。环境变量：`HOSTNAME`（集成 TURN 的
对外域名/IP，启用时必填）、`PORT`、`HTTPS=1` + `SSLPATH`（fullchain.pem/privkey.pem
目录）、`NAME`（/health 显示名）、`PEER_CONFIG`（配置路径，等价于 -config）。

HTTP 路由：`/socket.io/`（信令）、`/`（状态页——**默认英语**，浏览器
`Accept-Language` 为中文时自动显示中文，可用 `?lang=en` / `?lang=zh` 手动切换
并写入 Cookie 记忆）、`/health`、`/lobbies`、`/public/`。

### 与原版 TS 服务器的行为差异

| 项 | 原版 (socket.io 2.4.1 + node-turn) | Interstellar |
|---|---|---|
| 协议 | 仅 EIO=3 | EIO=3 + EIO=4 双栈 |
| 配置合并 | 写了 yml 就整体替换（缺 integratedRelay 会崩） | 缺省字段与默认值合并 |
| 集成 TURN | node-turn | Pion TURN；externalIps 留空自动探测公网 IP |
| join_lobby 回调 | 可能触发两次（fallthrough bug） | 修掉，只回一次 |
| region 字段 | 依赖 YAML 解析器透传 | 结构化字段，稳定透传 |
| iceServers[].urls | string/string[] | 同前，校验 scheme 必须是 stun/stuns/turn/turns |

信令事件语义与原版一致：`clientPeerConfig`/`join`/`setClients`/`setHost`/`setClient`/
`signal`/`VAD`/`lobby`/`join_lobby`(ack)/`lobbybrowser`/`remove_lobby`/`leave`/`id`/`disconnect`。

---

# 测试

`_test/` 下三个脚本（服务器需以 `config/peerConfig.test.yml` 启动，HOSTNAME=127.0.0.1）：

```bash
python _test/test_client.py   # EIO=4 全流程：clientPeerConfig/region/join/signal/lobbybrowser/ack/health — 13 项
python _test/test_eio3.py     # EIO=3（旧客户端）：握手/分帧/40 协商/ws 升级/事件 — 12 项
python _test/test_turn.py     # 集成 TURN：401 挑战 → 鉴权 Allocate → relay IP/端口段校验 — 6 项
```

实测结论（本机 Windows，2026-08）：三项全部 PASS。
测试中发现并修复了一个真实 bug：Pion 的 `AuthHandler` 返回值是 **HMAC 原始密钥**，
长期凭证必须返回 `MD5(user:realm:pass)` 而非明文密码（否则所有真客户端鉴权失败）。

## 进阶安全：TURN 动态凭证（后续可选升级）

静态账密会随客户端下发、可被抓包重放白嫖中继带宽。加固方向：
coturn/本服务器改用 `use-auth-secret` + `static-auth-secret` 模式，信令下发
`clientPeerConfig` 时生成带 TTL 的 temporary credential（如 1 小时有效），
客户端二进制内零凭据，泄露也自然过期。
