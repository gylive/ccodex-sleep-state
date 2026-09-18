# Linux：解压后启动，不用安装 Go

Linux 发布包包含静态 Go 可执行文件，不需要安装 Go 或额外运行时。x86_64 机器选择 `linux-amd64`；ARM64 机器（例如多数 ARM 云主机）选择 `linux-arm64`。下载后核对 `SHA256SUMS`，完整解压到自己的目录，例如 `~/Applications/ccodex-sleep-state`。

## 启动

先确认 Codex 原来的配置能用，并退出 Codex。进入解压目录执行：

```sh
chmod +x ccodex-sleep-state start.sh
./start.sh
```

也可以直接运行：

```sh
./ccodex-sleep-state setup
```

程序会创建本地配置、备份并接管 Codex 配置，然后打开管理面板。请保留终端窗口，停止时按 **Ctrl+C**，让程序恢复 Codex 配置。默认面板地址是 [http://127.0.0.1:17841/admin/](http://127.0.0.1:17841/admin/)。

Linux 没有桌面环境时，使用终端打印的地址在同一台机器的浏览器打开；如果浏览器在另一台机器上，不要直接暴露监听地址，优先使用 SSH 本地端口转发：

```sh
ssh -N -L 17841:127.0.0.1:17841 user@server
```

然后在本地打开 `http://127.0.0.1:17841/admin/`。管理口令只使用本次启动输出的短期凭证，不要发送到群里。

## 常用命令

```sh
./ccodex-sleep-state doctor                 # 只读脱敏诊断
./ccodex-sleep-state paths                  # 查看实际配置目录
./ccodex-sleep-state setup --no-config      # 只启动面板，不接管 Codex
./ccodex-sleep-state restore                # 服务退出后恢复配置
```

默认服务数据目录是 `${XDG_CONFIG_HOME:-$HOME/.config}/ccodex-sleep-state`；设置 `CCODEX_STATE_HOME` 可改位置，设置 `CODEX_HOME` 可指定 Codex 配置目录。首次启动本身不发送模型请求；接管后的 state 采集和正常请求可能消耗上游额度。

## 权限和依赖

程序只需要当前用户对服务数据目录和 `CODEX_HOME` 的读写权限，不需要 root。包使用 `CGO_ENABLED=0` 构建，适用于常见 glibc/musl Linux；仍需要网络访问你配置的上游或代理。不要用 `sudo` 启动，否则配置会落到 root 的 home 目录。
