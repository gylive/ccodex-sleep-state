# ccodex-sleep-state

**一个尝试改善 Codex 降智、限流和连接问题的本地工具。不是百分百有效，但把这套思路做成了大家能自己用的程序。**

做这个项目，是因为 Codex 有时用着用着就不对劲：回答质量变了，请求被限了，换个配置又接不上。我们从代理出口和 turn-state 入手做了一些尝试，觉得值得继续验证，就整理成 Go 程序开源了。

它不会增加账号额度，也不能保证换个出口就恢复质量。真正要做的是：**让连接、配置、采集和排错都看得见，别把所有问题都留给使用者手改文件。**

一个服务，一个本地网页。官方 ChatGPT、官方 API key 和 Responses 中转按各自的认证方式走；支持 Astra、5.6 Sol、5.6 Terra，能否调用仍取决于你的账号和上游。已有代理软件就填本地 HTTP / SOCKS5 地址，也可以导入自己的订阅或本地文件。

[Windows 上手](docs/windows.md) · [macOS 上手](docs/macos.md) · [Linux 上手](docs/linux.md) · [面板教程](docs/web-panel.md) · [代理与订阅](docs/proxies.md) · [问题与验收清单](docs/issues-and-verification.md) · [测试记录](docs/testing.md) · [群聊交流](#一起试一起反馈)

> **当前仍是公开测试版。** 此 README 描述当前源码；下载时以对应 Release 的说明为准，旧发布包不会自动多出新功能。本轮真实 Sol 回复、V2 远程压缩及压缩后回复、Terra 回复已跑通；追加的两次 Astra 短回复也成功，但本轮仍没有采到合格 292；旧式 V1 压缩直连上游返回 404，Team 仍只做合成测试。不能把部分成功写成全部验收通过。具体边界见[问题与验收清单](docs/issues-and-verification.md)。

## 下载后，怎么开始

先确认 Codex **不经过本工具时原本就能用**。官方账号先登录；中转先配好自己的 API key。用 CCS / CC Switch 的，先选好这次要用的配置，再退出 Codex，暂时别继续切换。

1. 到 [Releases](https://github.com/gylive/ccodex-sleep-state/releases) 下载并完整解压。普通 Windows 选 `windows-amd64`，Windows ARM 选 `windows-arm64`；Apple 芯片 Mac 选 `darwin-arm64`，Intel Mac 选 `darwin-amd64`；Linux x86_64 选 `linux-amd64`，Linux ARM64 选 `linux-arm64`。
2. **Windows 双击 `start.cmd`，macOS 双击 `start.command`，Linux 执行 `./start.sh`。** 浏览器会自动打开并进入本地面板，不需要先复制口令。
3. 程序会自动备份并接入，打开面板。看到已接管后，**重启 Codex，新建会话**，发一条短消息；不用再点一次接入。旧会话不会被热切换。

已有代理、订阅、模型和注入开关会保留；没有配过来源时尝试识别常见本地代理。首次 `setup` 没有明确设置兜底策略，就采用“采不到 state 时先正常转发”；之前明确选过严格模式的会继续保留。

没有找到本地代理，或你只拿到了订阅链接，再到「订阅与代理」填写并保存。首页「一键接入 Codex」用于需要时重新检查和修复，不是每次启动的必点步骤。

如果浏览器没有打开，按终端显示的地址手动进入，再粘贴本次管理口令。默认地址是 [http://127.0.0.1:17841/admin/](http://127.0.0.1:17841/admin/)。如果包里没有启动脚本，可以在程序目录执行以下命令；旧版不支持 `setup` 时先更新。

Windows：

```powershell
.\ccodex-sleep-state.exe setup
```

macOS：

```sh
./ccodex-sleep-state setup
```

Linux：

```sh
chmod +x ccodex-sleep-state start.sh
./start.sh
```

`setup` 会创建缺失的服务配置、启动服务并打开面板；已有订阅和代理设置不会被重置。自动接入只检查本机 `127.0.0.1` 的常见 SOCKS5 端口，不扫描网络、不读取代理软件账号库。**启动本身不发送模型请求**；接入 Codex 后的采集可能消耗额度。服务 JSON 写坏了会进入修复面板，不会因为启动失败就静默删掉旧文件。

服务运行时保留终端窗口。退出用 **Ctrl+C**，等配置恢复完成，再重启 Codex。关闭浏览器、关闭注入和停止服务，是三个不同操作。

## 面板里能做什么

| 你想做的事 | 去哪里 |
| --- | --- |
| 看 Codex 有没有接上、请求有没有经过 | 「开始使用」里的配置、请求计数和会话 |
| 开关 state 注入 | 「turn-state 注入」；开关开启不代表已经拿到合格 state |
| 切 Astra / Sol / Terra | 「连接设置」里的模型与账号规则；修改默认模型后重启 Codex 或新建会话 |
| Team 账号不再死等 292 | 「连接设置」选自动识别，识别不了就手动选 Team |
| 没有合格 state 时先正常回复 | 首次 setup 未设置策略时采用普通转发兜底；「连接设置」可明确改成严格 |
| 填本地代理、远程订阅、本地订阅文件 | 「订阅与代理」；不用手改 JSON，也不用猜环境变量在哪个终端生效 |
| 测试连接、固定出口、恢复自动选择 | 「路由」；连接成功不代表拿到了合格 state |
| 从旧版本或 CCS 冲突中恢复 | 「检查与修复配置」；先预览，再确认保留当前配置或恢复备份 |
| 不知道怎么反馈问题 | 「遇到问题」下载脱敏诊断，或运行 `doctor` |

[面板教程](docs/web-panel.md)有逐步操作，不需要先看懂代码。我们也对照研究了 GPT-Load 的路由诊断、失败分类和部署体验，但**没有把它的账号池、所有协议、计费和多渠道平台完整移植进来**。[功能映射与取舍](docs/gpt-load-design.md)

## 292、332 到底是什么

程序处理的是响应里的 `X-Codex-Turn-State`。它从候选出口发短请求，检查封装、时间和密文块数，保存可用值；后续请求带上这份值，并走对应出口。

| 账号规则 | 本项目接纳的经验形状 | 不会当成合格值 |
| --- | --- | --- |
| 个人 | 10 块，通常 292 字符 | 11 块 / 312 字符 |
| Team / Business | 12 块，通常 332 字符 | 13 块 / 356 字符 |

**这不是 OpenAI 公布的质量指标。** 符合长度不证明模型“满血”，不符合也不能只凭这一点断定代理坏了或模型降智了。

自动识别使用当前请求里的套餐提示，提示不明确时会说明采用了什么规则；不会仅凭长度把账号认成 Team。你也可以在面板明确选个人或 Team。state 按账号、凭据和模型隔离，不在 Astra、Sol、Terra 之间混用。

没有合格 state 时有两种选择：

- **严格模式。** 不使用不合格 state；采集失败返回明确原因。明确选过 `strict`，之后 `setup` 不会自动覆盖。底层转发在策略为空时仍按严格处理。
- **普通转发兜底。** 首次 `setup` 没有明确策略时采用，也可在面板自行开启；重新点首页接入按钮会提示并选择此模式。 没有可用 state 时不注入，仍走选定出口，正式请求最多转发一次。状态会说明本次没有注入，而不是假装采集成功。

兜底不是解除限流。401、403、429 的停止规则照常生效；已经发到上游的正式生成不会换出口再重放一次。

## CCS、中转和官方登录怎样兼容

程序只处理当前 Codex 选择的 provider，不接管 CCS 的账号数据库。它读取上游与认证方式，临时把 Codex 的入口接到本地服务，停止后恢复。

| 你原来怎么用 | 本工具怎么处理 |
| --- | --- |
| 官方 ChatGPT 登录 | 可采集、注入 state，也可关闭注入 |
| OpenAI 官方 API key | 按 API 通道转发，不当成 ChatGPT 订阅登录 |
| Responses 中转、CCS API key | 保留原上游和 API key 方式，不向中转发送官方登录凭据，不注入官方 state |
| Codex profile / 独立 Codex Home | 需要选择同一份配置；本工具不会自动清空项目或任务级覆盖 |

运行中只改注释或无关设置，不应再把整份配置判为冲突。**CCS 真正切换了 provider、上游或认证时，仍会停止转发**，防止凭据送错地方。这时去面板检查并保留当前配置，再重新接管；不要删除事务文件，也不用照着群消息盲改 TOML。

最稳妥的切换顺序还是：**停止本服务 → CCS 切换 → 启动本服务 → 重启 Codex。** 兼容不是让两个程序同时抢写同一份配置。

远程上下文压缩单独验收：本轮真实 Sol 的新版 V2 压缩及压缩后继续回复已成功；旧式 V1 直连上游返回 404，发送前桥接已在本地模拟客户端通过，但追加真实验收的第一条 Sol 回复发生出口传输失败，不能写成所有压缩方式都已跑通。[压缩问题进展](docs/issues-and-verification.md#p01-远程上下文压缩优先项)

## 有问题，先看这几项

| 现象 | 下一步 |
| --- | --- |
| 面板能打开，但显示「未接管」 | 服务启动不等于 Codex 接入。处理配置提示；确认不是 `--no-config` 只读模式 |
| Codex 能回复，面板没有请求 | 重启 Codex，检查是不是另一份配置、profile、任务覆盖或另一实例 |
| `codex_config_changed` / 409 | 面板点「检查与修复配置」，优先保留 CCS 当前配置，再重新接管 |
| `state_unavailable` / 本地 503 | 看账号规则、采集诊断和冷却；需要先用时可明确选普通转发兜底或关闭注入 |
| `state_shape_changed` | 正式请求可能已经消耗额度，服务不会自动重放；检查规则和上游变化 |
| 415 / 不支持请求编码 | 新版支持官方客户端的 gzip/zstd 请求；确认不是仍在运行旧版，再查看脱敏诊断 |
| 上游 503 / 502 / 超时 | 查看出口连接及上游情况，不能改个 state 长度掩盖网络问题 |
| 401 / 403 / 429 | 分别处理登录、访问权限或等待限流恢复，不通过换 IP 继续撞 |
| 服务配置 JSON 坏了 | `setup` 打开只读修复面板，确认备份后重建，重新启动 |
| Codex 提示 `503 service_not_ready` | 直接打开错误里的 `admin_url`，按面板唯一的绿色按钮「一键接入 / 检查与修复」操作；不要在 CCS 或 Codex 里手改配置 |
| Codex TOML 坏了 | 先保留备份；使用明确的重建流程或让 CCS 重新生成，不从坏文件里猜 API key |

给群友或 AI Agent 发排查信息，先运行：

```powershell
.\ccodex-sleep-state.exe doctor
```

macOS 对应 `./ccodex-sleep-state doctor`。不要发 `auth.json`、管理口令、完整订阅或整个数据目录。更多问题、修复与尚未验收项都记录在[问题清单](docs/issues-and-verification.md)，不会只留在聊天记录里。

## 用之前知道这些边界

- 支持的模型是 **`gpt-6-astra`、`gpt-5.6-sol`、`gpt-5.6-terra`**，上游必须实际提供对应模型。工具不会替你开通权限，也不会悄悄换模型。
- 目前走 **Responses HTTP / SSE**，不支持 WebSocket 和任意协议转换。
- 采集会使用实际额度。默认每轮最多 6 个候选出口，单次探测最多 20 秒，两轮至少间隔 180 秒。正常生成请求不自动重放。
- 代理支持 HTTP、HTTPS、SOCKS5，以及 Clash/Mihomo YAML、逐行 URI、Base64 URI 订阅。内嵌出站适配支持 AnyTLS、SS、SSR、VMess、VLESS、Trojan、Hysteria、Hysteria2、TUIC；不导入订阅的 TUN、DNS 或系统规则。[具体限制](docs/proxies.md)
- 认证值和完整 state 不写常规日志。程序没有遥测，不把日志上传给作者；但本地代理配置和恢复备份仍可能含敏感信息，要自己保管。
- 不切换系统代理，不刷新 Codex 登录，不使用重置卡。中转站能接上不代表 state 思路对中转也有效。

## 文件一般放在哪

| 内容 | Windows | macOS | Linux |
| --- | --- | --- | --- |
| 程序，建议完整解压位置 | `%LOCALAPPDATA%\Programs\ccodex-sleep-state` | `~/Applications/ccodex-sleep-state` | `~/Applications/ccodex-sleep-state` |
| 本工具配置与日志 | `%LOCALAPPDATA%\ccodex-sleep-state` | `~/Library/Application Support/ccodex-sleep-state` | `${XDG_CONFIG_HOME:-$HOME/.config}/ccodex-sleep-state` |
| 默认 Codex 配置 | `%USERPROFILE%\.codex\config.toml` | `~/.codex/config.toml` | `~/.codex/config.toml` |
| 接管前备份 | 与 Codex 的 `config.toml` 同目录 | 与 Codex 的 `config.toml` 同目录 | 与 Codex 的 `config.toml` 同目录 |
| 修复时新增的归档 | 本工具数据目录下的 `backups` | 本工具数据目录下的 `backups` | 本工具数据目录下的 `backups` |

设置过 `CODEX_HOME` 会跟随它；本工具还有 `CCODEX_STATE_HOME` 和 `--data-dir`。不确定实际用的是哪份，运行 `paths`，不要凭目录名猜。

## 一起试，一起反馈

用起来有没有改善、哪个版本接不上、什么情况下又出问题，都欢迎来聊。报错和复现步骤也可以提 [Issue](https://github.com/gylive/ccodex-sleep-state/issues)，方便后面查找。

| 微信群 · 爱的交流 | QQ 群 · 此间大梦无边 |
|:---:|:---:|
| <img src="docs/assets/wechat-group-2026-09-18.jpg" alt="微信群“爱的交流”二维码，图中标注 9 月 25 日前有效" width="280"> | <img src="docs/assets/qq-group.jpg" alt="QQ 群“此间大梦无边”二维码，群号 754842541" width="280"> |
| 图中标注 **9 月 25 日前有效**；过期后请先用 QQ 群入口。 | 扫码，或搜索群号 **754842541**。 |

反馈时带上系统、Codex 版本、工具版本和错误提示就够了。**不要发账号凭据、完整订阅链接或未经检查的配置文件。**

## 想改代码

项目用 Go 编写，静态管理页嵌入二进制，不需要另外启动前端服务。

- `cmd/ccodex-sleep-state`：命令、一键入口、诊断。
- `internal/codexconfig`：provider 解析、接管、语义检查与可恢复修复。
- `internal/gateway`：模型与账号隔离、采集、压缩和流式转发。
- `internal/turnstate`：封装规则、当前值与备用值。
- `internal/proxyroute`：订阅和出站连接。
- `internal/service`：单实例、面板、配置热切换和请求诊断。

[构建方法](docs/development.md) · [设计取舍](docs/architecture.md) · [GPT-Load 功能映射](docs/gpt-load-design.md) · [隐私说明](SECURITY.md)

## 许可证

GPL-3.0，见 [LICENSE](LICENSE)。代理部分复用了 Mihomo，见 [依赖说明](THIRD_PARTY_NOTICES.md)。本项目与 OpenAI、Mihomo、GPT-Load 没有隶属关系。
