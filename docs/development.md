# 构建与维护

应用本身是 Go，发布包运行时不用装编译器。开发需要 Go 和 Git；代理核心带来较多依赖，第一次下载和编译会比之后慢。

```sh
git clone https://github.com/gylive/ccodex-sleep-state.git
cd ccodex-sleep-state
go test ./...
go build -trimpath -o ccodex-sleep-state ./cmd/ccodex-sleep-state
```

Windows 构建输出名加 `.exe` 即可。最低 Go 版本以 `go.mod` 为准，CI 使用 Go 1.27 系列。

## 检查改动

```sh
gofmt -w cmd internal
go vet ./...
go test -race -count=1 ./...
go test ./internal/turnstate -fuzz=FuzzParse -fuzztime=10s
```

测试里的认证、代理密码、state 都是合成数据。测试只访问本地模拟服务器，不需要真实订阅或模型账号。配置文件和日志放在测试临时目录。`-race` 需要当前系统支持的竞态检测工具链；Windows 通常还需要受支持的 C 编译工具链。

直接运行 `setup` / `serve` 和运行测试不是一回事：前者默认接管当前用户的 Codex。想手工测试启动，显式提供隔离的 `--data-dir` 并用 `--no-config`；要测试配置修改，把服务 JSON 的 `codex_home` 指向另一个测试目录。

## 参数表

所有参数都在本程序的 JSON 配置里，未知字段会报错。

| 字段 | 默认值 | 作用 |
|---|---|---|
| `model` | `gpt-6-astra` | 默认模型；允许 Astra、`gpt-5.6-sol`、`gpt-5.6-terra`，不替上游开通权限 |
| `account_mode` | `auto` | 自动套餐提示 / `personal` 10 块 / `team` 12 块；提示不是认证证明 |
| `state_fallback` | 内核空值为 `strict`；首次 `setup` 无明确值时设为 `passthrough` | `setup` 保留已明确的 `strict`；首页再次接入确认后选择兜底，不解除拒绝或重放 |
| `upstream_mode` | 自动 | 跟随当前 Codex 上游；`manual` 要自行确认认证与目标 |
| `upstream_kind` | `official` | `relay` 为 API 通道，不采集官方 state |
| `codex_profile` | 空 | 显式选择 profile；客户端必须使用同一 profile |
| `injection_disabled` | `false` | 关闭采集与注入，但仍通过本服务转发 |
| `pinned_route` | 空 | 固定匿名出口 ID；空为自动选择 |
| `listen` | `127.0.0.1:17841` | 唯一回环监听地址 |
| `upstream` | `https://chatgpt.com/backend-api/codex` | 上游；修改它会改变认证与正文的接收方 |
| `codex_home` | 自动检测 | 优先显式路径，其次 `CODEX_HOME`，再用户 `.codex` |
| `direct` | `true` | 将直连放在出口池最前；代理专用部署改为 `false` |
| `proxy_urls` / `proxy_envs` | 空 | 直接代理 URI / 存放 URI 的环境变量名 |
| `subscriptions` | 空 | `{url}` 或 `{url_env}` 列表；每项可设置 `user_agent`、`include_protocols`、`exclude_keywords` |
| `subscription_proxy_env` | 空 | 仅订阅下载使用的代理变量 |
| `probe_timeout_seconds` | `20` | 单次探测超时，1–60 秒 |
| `max_probes_per_round` | `6` | 每轮最多尝试出口数，1–20；单轮不重复同一出口 |
| `probe_cooldown_seconds` | `180` | 两轮采集最小间隔，30–3600 秒 |
| `state_ttl_seconds` | `3600` | 本地估计 TTL，120–3600 秒 |
| `refresh_before_seconds` | `1200` | 估计过期前进入续补窗口，至少 30 秒且小于 TTL |
| `baseline_blocks` | `10` | 旧版自定义基线；自动模式保留非默认值，明确选择个人 / Team 可覆盖该旧规则；不是质量保证 |

不要为了“更积极”把这些限制一路调小。先看请求记录与实际效果，保留对照实验；没有对照数据，不给出质量提升百分比。

## 发版本

CI 在 Windows、macOS 和 Linux 上跑测试与 `go vet`。发布工作流额外做竞态测试，再交叉构建 Windows x64/ARM64、macOS Apple Silicon/Intel、Linux x64/ARM64 六个包；附上 SHA256 与包含 vendored 依赖的源码包。

维护者提交带 `v` 前缀的版本标签会触发发布。不要从带私有配置的工作树手工 `zip` 整个目录。发布前应先看完 CI，核对 LICENSE、变更说明、问题验收清单和敏感信息扫描结果。Windows 包将 `scripts/start.cmd` 复制到 exe 同目录；macOS 和 Linux 包同样附上可执行的启动脚本。测试完实际发布包，不能只测试工作树二进制。

仓库只保留维护所需的源文件、测试和文档；构建缓存、运行目录、日志、私有配置、个人环境快照都不属于版本历史。
