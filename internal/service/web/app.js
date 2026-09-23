"use strict";
const $ = (id) => document.getElementById(id);
const launchTicket = new URLSearchParams(location.hash.slice(1)).get("launch");
if (location.hash) history.replaceState(null, "", location.pathname);
let token = sessionStorage.getItem("sleep-state-control") || "";
let state = null;
let busy = false;
let noticeTimer;
let recovery = null;
let preferencesDirty = false;
let timingDirty = false;
const timingFields = {
  probe_timeout_seconds: "probe-timeout",
  probe_cooldown_seconds: "probe-cooldown",
  state_ttl_seconds: "state-ttl",
  refresh_before_seconds: "refresh-before",
  max_probes_per_round: "max-probes",
};
function readTiming() {
  return Object.fromEntries(Object.entries(timingFields).map(([key, id]) => [key, Number($(id).value)]));
}
function timingSummary() {
  const t = readTiming();
  $("refresh-before").setCustomValidity(t.refresh_before_seconds >= t.state_ttl_seconds ? "提前刷新时间必须小于本地有效期" : "");
  $("timing-summary").textContent = `${timingDirty ? "尚未保存 · " : "已保存 · "}两轮至少相隔 ${t.probe_cooldown_seconds} 秒，每轮最多 ${t.max_probes_per_round} 次；state 签发约 ${t.state_ttl_seconds - t.refresh_before_seconds} 秒后进入刷新窗口。`;
}
function fillTiming(t) {
  if (!t) return;
  for (const [key, id] of Object.entries(timingFields)) $(id).value = t[key];
  timingSummary();
}
function notice(text) {
  $("notice").textContent = text;
  $("notice").hidden = false;
  clearTimeout(noticeTimer);
  noticeTimer = setTimeout(() => {
    $("notice").hidden = true;
  }, 12000);
}
async function api(path, body) {
  const response = await fetch("/admin/api/" + path, {
    method: body === undefined ? "GET" : "POST",
    headers: {
      Authorization: "Bearer " + token,
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    cache: "no-store",
    redirect: "error",
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
  const value = await response.json();
  if (!response.ok)
    throw new Error(value.error || "操作未完成，请检查服务终端。");
  return value;
}
async function action(fn) {
  if (busy) return;
  busy = true;
  document.querySelectorAll("button").forEach((button) => {
    button.disabled = true;
  });
  try {
    await fn();
  } catch (error) {
    notice(error.message);
  } finally {
    busy = false;
    document.querySelectorAll("button").forEach((button) => {
      button.disabled = false;
    });
    if (state && state.upstream_kind === "relay") $("toggle").disabled = true;
    document
      .querySelectorAll("[data-locked]")
      .forEach(
        (button) => (button.disabled = button.dataset.locked === "true"),
      );
    if (recovery) {
      $("keep-current").disabled = !recovery.can_keep_current;
      $("restore-backup").disabled = !recovery.can_restore_backup;
    }
    if (state && !state.configuration_writable)
      for (const id of [
        "recover",
        "inspect-recovery",
        "keep-current",
        "restore-backup",
        "quick-setup",
      ])
        $(id).disabled = true;
  }
}
const phases = {
  ready: "已有符合规则的 state",
  collecting: "正在采集，请稍等",
  waiting_for_state: "尚未采到合格 state",
  auth_blocked: "上游拒绝登录或权限",
  rate_limited: "上游要求等待",
  passthrough: "普通转发，不注入",
};
function textNode(tag, text, className) {
  const node = document.createElement(tag);
  node.textContent = text;
  if (className) node.className = className;
  return node;
}
async function refresh() {
  state = await api("status");
 if(typeof renderUpgrade==="function") renderUpgrade(state);
  $("rescue-card").hidden = !state.rescue_mode;
  $("headline").textContent =
    state.config_error || state.route_error
      ? "有一项配置需要处理。"
      : !state.configured_codex
        ? "服务已启动，等待 Codex 接入。"
        : state.injection_enabled
          ? "已接入 Codex，等待或正在处理请求。"
          : "服务在运行，当前不注入。";
  $("mode").textContent =
    state.upstream_kind === "relay" ? "中转 / API 转发" : "官方 ChatGPT";
  $("route-count").textContent = state.routes;
  $("managed").textContent = state.configured_codex ? "已接管" : "未接管";
  $("toggle").textContent = state.injection_enabled ? "关闭注入" : "开启注入";
  $("toggle").disabled = state.upstream_kind === "relay";
  $("injection-help").textContent =
    state.injection_reason ||
    (state.injection_enabled
      ? "注入已开启，是否已有可用 state 请看会话状态。"
      : "注入已关闭，不采集或替换 state。");
  for (const id of ["recover", "inspect-recovery"])
    $(id).disabled = !state.configuration_writable;
  if (!preferencesDirty && document.activeElement?.id !== "model-select")
    $("model-select").value = state.model;
  if (!preferencesDirty && document.activeElement?.id !== "account-select")
    $("account-select").value = state.account_mode || "auto";
  if (!preferencesDirty && document.activeElement?.id !== "fallback-select")
    $("fallback-select").value = state.state_fallback || "strict";
  if (!timingDirty) fillTiming(state.timing);
  const traffic = state.traffic || { total: 0, failed: 0, recent: [] };
  $("step-config").textContent = state.configured_codex
    ? "已接入 · 可检查或修复"
    : "未接入 · 点这里处理";
  $("step-route").textContent = state.routes
    ? `${state.routes} 个出口配置 · 可更换`
    : "没有可用出口 · 点这里配置";
  $("step-request").textContent = traffic.total
    ? `已收到 ${traffic.total} 次请求`
    : "重启 Codex，再试一次";
  $("traffic-summary").textContent =
    `本次启动收到 ${traffic.total} 次请求，${traffic.failed} 次返回错误。切换出口不会清空这份记录。`;
  $("recent-requests").replaceChildren();
  for (const item of (traffic.recent || []).slice(-5).reverse()) {
    $("recent-requests").append(
      textNode(
        "div",
        `${new Date(item.at).toLocaleTimeString()} · ${item.kind} · HTTP ${item.status} · ${item.duration_ms} ms`,
        "hint",
      ),
    );
  }
  $("warnings").replaceChildren();
  for (const message of [state.config_error, state.route_error, state.pool_error])
    if (message) $("warnings").append(textNode("div", message, "warning"));
  $("sessions").replaceChildren();
  if (!state.sessions?.length)
    $("sessions").append(
      textNode(
        "p",
        traffic.total
          ? "已经有请求到达，但还没有保留中的模型会话。请看上面的状态码；模型列表请求、被拦截请求或过期会话不代表已成功生成。"
          : state.configured_codex
            ? "还没收到请求。请重启 Codex，并新建会话发一条短消息；若仍为空，检查是否启动了另一份 Codex 配置。"
            : "还没有请求到达本服务。先到「连接设置」完成配置接管，再重启 Codex。仅能打开面板不代表接入完成。",
        "hint",
      ),
    );
  for (const [i, session] of (state.sessions || []).entries()) {
    let description =
      "会话 " + (i + 1) + " · " + (phases[session.phase] || session.phase);
    if (session.retry_after_seconds)
      description += " · 还需等待约 " + session.retry_after_seconds + " 秒";
    description += session.model ? " · " + session.model : "";
    const acceptedLengths = (session.accepted_lengths || [session.expected_length])
      .filter(Boolean)
      .join(" / ");
    description += acceptedLengths ? " · 接纳 " + acceptedLengths : "";
    const row = textNode("div", "", "session");
    const detail = textNode("div", description);
    for (const note of [session.account_note, session.diagnostic_message])
      if (note)
        detail.append(
          textNode(
            "p",
            typeof note === "string" ? note : JSON.stringify(note),
            "hint",
          ),
        );
    row.append(detail);
    if (session.observed_length)
      detail.append(
        textNode(
          "p",
          `最近收到 ${session.observed_length} 字符；当前接纳 ${acceptedLengths}。`,
          "hint",
        ),
      );
    if (session.id && state.injection_enabled) {
      const retry = textNode(
        "button",
        session.cooldown_seconds > 0
          ? `等待 ${session.cooldown_seconds} 秒后再采集`
          : "重新采集",
        "secondary",
      );
      retry.dataset.retry = session.id;
      retry.dataset.locked = String(
        session.cooldown_seconds > 0 ||
          ["ready", "collecting", "auth_blocked", "rate_limited"].includes(
            session.phase,
          ),
      );
      retry.disabled = retry.dataset.locked === "true";
      retry.addEventListener("click", () =>
        action(async () => {
          if (
            !confirm(
              `重新采集会使用这个会话的账号和模型，本轮最多 ${state.max_probes_per_round || 6} 次短请求，会消耗额度。冷却和登录/限流暂停不会被跳过。继续？`,
            )
          )
            return;
          try {
            const result = await api("state/retry", { id: session.id });
            notice(result.message);
          } finally {
            await refresh();
          }
        }),
      );
      row.append(retry);
    }
    $("sessions").append(row);
  }
}
async function enter() {
  await refresh();
  sessionStorage.setItem("sleep-state-control", token);
  $("token").value = "";
  $("login").hidden = true;
  $("workspace").hidden = false;
  $("logout").hidden = false;
}
$("login-form").addEventListener("submit", (event) => {
  event.preventDefault();
  action(async () => {
    token = $("token").value.trim();
    await enter();
    clearTimeout(noticeTimer);$("notice").hidden=true;
  });
});
$("logout").addEventListener("click", () => {
  sessionStorage.removeItem("sleep-state-control");
  token = "";
  state = null;
  $("login").hidden = false;
  $("workspace").hidden = true;
  $("logout").hidden = true;
});
document.querySelectorAll("[data-page]").forEach((button) =>
  button.addEventListener("click", () => {
    document
      .querySelectorAll("[data-page]")
      .forEach((item) => item.classList.toggle("active", item === button));
    document.querySelectorAll("[data-view]").forEach((view) => {
      view.hidden = view.dataset.view !== button.dataset.page;
    });
  }),
);
$("toggle").addEventListener("click", () =>
  action(async () => {
    const result = await api("injection", {
      enabled: !state.injection_enabled,
    });
    notice(result.message);
    await refresh();
  }),
);
$("recover").addEventListener("click", () =>
  action(async () => {
    const result = await api("recover", {});
    notice(result.message);
    await refresh();
  }),
);
function sourceMode() {
  const mode = $("source-mode").value;
  const subscription = mode === "subscription" || mode === "file";
  $("subscription-options").hidden = !subscription;
  $("source-value-wrap").hidden = mode === "direct";
  $("source-label").textContent =
    {
      proxy: "代理地址",
      subscription: "订阅链接",
      file: "本机订阅文件的完整路径",
    }[mode] || "";
  $("source-value").placeholder =
    {
      proxy: "socks5://127.0.0.1:7897",
      subscription: "https://… 或 http://127.0.0.1:端口/…",
      file: "C:\\Users\\你的用户名\\Downloads\\subscription.yaml",
    }[mode] || "";
  $("source-hint").textContent =
    mode === "file"
      ? "在这台电脑上读取你指定的普通文件，支持 YAML、URI 列表和 Base64 订阅。不会修改原文件。"
      : mode === "subscription"
        ? "链接可能含订阅密码，请勿截图分享。HTTP 只接受 127.0.0.1 等本机地址；远程链接必须使用 HTTPS。"
        : "端口以你的代理软件为准。支持 http://、https://、socks5://。这里填地址，不是 PowerShell 命令。";
}
$("source-mode").addEventListener("change", sourceMode);
function sourceBody() {
  const split = (id) =>
    $(id)
      .value.split(/[,，]/)
      .map((value) => value.trim())
      .filter(Boolean);
  return {
    mode: $("source-mode").value,
    value: $("source-value").value.trim(),
    user_agent: $("user-agent").value.trim(),
    exclude_keywords: split("exclude"),
    include_protocols: split("protocols"),
  };
}
function resultAt(id, value) {
  $(id).hidden = false;
  $(id).textContent =
    value.message +
    (value.routes ? "\n共 " + value.routes.length + " 个出口配置。" : "") +
    (value.status
      ? "\nHTTP " + value.status + " · " + value.duration_ms + " ms"
      : "");
}
$("test-source").addEventListener("click", () =>
  action(async () => {
    const value = await api("sources/test", sourceBody());
    resultAt("source-result", value);
  }),
);
$("source-form").addEventListener("submit", (event) => {
  event.preventDefault();
  action(async () => {
    if (
      !confirm(
        "用这份设置替换现有出口来源？旧配置会备份，已经采集的 state 会清空。",
      )
    )
      return;
    const value = await api("sources/apply", sourceBody());
    resultAt("source-result", value);
    $("source-value").value = "";
    await refresh();
  });
});
$("load-routes").addEventListener("click", () =>
  action(async () => {
    const value = await api("routes", {});
    $("routes-list").replaceChildren();
    for (const route of value.routes) {
      const row = textNode("div", "", "route");
      const label = textNode("div", "");
      label.append(
        textNode(
          "strong",
          (route.label || route.id) +
            (value.pinned_route === route.id ? " · 当前固定出口" : ""),
        ),
      );
      label.append(textNode("div", route.id, "hint"));
      row.append(label);
      const actions = textNode("div", "", "actions");
      const test = textNode("button", "测试连接", "secondary");
      test.addEventListener("click", () =>
        action(async () =>
          resultAt("route-result", await api("routes/test", { id: route.id })),
        ),
      );
      const pin = textNode("button", "固定此出口", "secondary");
      pin.addEventListener("click", () =>
        action(async () => {
          resultAt("route-result", await api("routes/pin", { id: route.id }));
          await refresh();
        }),
      );
      actions.append(test, pin);
      row.append(actions);
      $("routes-list").append(row);
    }
  }),
);
$("auto-route").addEventListener("click", () =>
  action(async () => {
    resultAt("route-result", await api("routes/pin", { id: "" }));
    await refresh();
  }),
);
if (launchTicket)
  action(async () => {
    const value = await api("launch", { ticket: launchTicket });
    token = value.control_token;
    await enter();
  });
else if (token) action(enter);
setInterval(() => {
  if (token && !busy && !document.hidden)
    refresh().catch((error) => notice(error.message));
}, 10000);

$("preferences-form").addEventListener("submit", (event) => {
  event.preventDefault();
  action(async () => {
    const result = await api("preferences", {
      model: $("model-select").value,
      account_mode: $("account-select").value,
      state_fallback: $("fallback-select").value,
    });
    preferencesDirty = false;
    notice(result.message);
    await refresh();
  });
});
$("inspect-recovery").addEventListener("click", () =>
  action(async () => {
    recovery = await api("recovery/preview", {});
    $("recovery-panel").hidden = false;
    $("recovery-summary").textContent = recovery.message;
    $("keep-current").disabled = !recovery.can_keep_current;
    $("restore-backup").disabled = !recovery.can_restore_backup;
  }),
);
async function repair(mode) {
  if (!recovery) return;
  const question =
    mode === "restore_backup"
      ? "恢复接管前的备份？这会撤销之后对 Codex 配置的修改。当前文件会独立备份。"
      : "保留 CCS 当前选择，移除能确认属于旧版本的配置，再重新接管？当前文件会先备份。";
  if (!confirm(question)) return;
  const result = await api("recovery/apply", {
    mode,
    expected_config_sha256: recovery.config_sha256,
    expected_transaction_sha256: recovery.transaction_sha256,
  });
  recovery = null;
  $("recovery-panel").hidden = true;
  notice(result.message);
  await refresh();
}
$("keep-current").addEventListener("click", () =>
  action(() => repair("keep_current")),
);
$("restore-backup").addEventListener("click", () =>
  action(() => repair("restore_backup")),
);
$("download-diagnostics").addEventListener("click", () =>
  action(async () => {
    const value = await api("diagnostics", {});
    const url = URL.createObjectURL(
      new Blob([JSON.stringify(value, null, 2)], { type: "application/json" }),
    );
    const a = document.createElement("a");
    a.href = url;
    a.download = "sleep-state-diagnostics.json";
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }),
);

$("reset-service-config").addEventListener("click", () =>
  action(async () => {
    const preview = await api("service-config/preview", {});
    if (!confirm(preview.message)) return;
    const result = await api("service-config/reset", {
      expected_config_sha256: preview.config_sha256,
    });
    notice(result.message);
  }),
);

$("rebuild-kind").addEventListener("change", () => {
  $("rebuild-relay").hidden = $("rebuild-kind").value !== "relay";
});
$("rebuild-form").addEventListener("submit", (event) => {
  event.preventDefault();
  action(async () => {
    const preview = await api("codex-config/preview", {});
    if (
      !confirm(
        "将完整备份旧 Codex 配置，再按所选账号重建最小配置。原有插件、MCP 和其他设置只保留在备份中，不会自动迁移。确认继续？",
      )
    )
      return;
    const value = await api("codex-config/rebuild", {
      kind: $("rebuild-kind").value,
      upstream: $("rebuild-url").value.trim(),
      env_key: $("rebuild-env").value.trim(),
      expected_config_sha256: preview.config_sha256,
      expected_exists: preview.exists,
    });
    $("rebuild-url").value = "";
    notice(value.message);
    await refresh();
  });
});

document.querySelectorAll("[data-jump]").forEach((button) =>
  button.addEventListener("click", () => {
    document.querySelector(`[data-page="${button.dataset.jump}"]`).click();
  }),
);

$("quick-setup").addEventListener("click", () =>
  action(async () => {
    if (
      !confirm(
        "自动备份并接入当前 Codex，检查常见本地代理；已有订阅和账号不重置。采不到合格 state 时先普通转发。开启注入后的模型请求可能触发有限采集并消耗额度。继续？",
      )
    )
      return;
    try {
      const value = await api("quick-setup", {});
      notice(value.message);
    } finally {
      await refresh();
    }
  }),
);

for (const id of ["model-select", "account-select", "fallback-select"])
  $(id).addEventListener("change", () => {
    preferencesDirty = true;
  });

for (const id of Object.values(timingFields)) {
  $(id).addEventListener("input", () => { timingDirty = true; timingSummary(); });
}
$("timing-defaults").addEventListener("click", () => {
  if (!state) return;
  timingDirty = true; fillTiming(state.timing_defaults);
});
$("timing-low-frequency").addEventListener("click", () => {
  if (!state) return;
  timingDirty = true;
  fillTiming({ ...state.timing_defaults, probe_cooldown_seconds: 600, max_probes_per_round: 2 });
});
$("timing-cancel").addEventListener("click", () => {
  if (!state) return;
  timingDirty = false; fillTiming(state.timing);
});
$("timing-form").addEventListener("submit", (event) => {
  event.preventDefault();
  timingSummary();
  if (!$("timing-form").reportValidity()) return;
  const next = readTiming();
  if (!confirm("保存采集设置？旧配置会备份，现有 state 缓存会清空。无需重启 Codex；后续请求可能重新采集并消耗额度。")) return;
  action(async () => {
    const result = await api("timing", next);
    // Do not discard edits typed while the save request was in flight.
    timingDirty = JSON.stringify(readTiming()) !== JSON.stringify(next);
    $("timing-result").textContent = result.message;
    await refresh();
  });
});
