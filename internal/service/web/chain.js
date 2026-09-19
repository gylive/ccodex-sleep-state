"use strict";
let chainDirty = false;
let chainRevision = 0;

function renderProbeChain(s) {
  const chain = s.probe_chain || {};
  if (!chainDirty) {
    $("probe-chain-enabled").checked = !!chain.enabled;
    $("probe-chain-protocol").value = chain.protocol || "http";
  }
  $("probe-chain-status").textContent = chain.enabled
    ? `已启用：${chain.count || 0} 个落地出口仅用于采集，正式请求只走本地代理。`
    : chain.configured
      ? `已保存 ${chain.count || 0} 个落地出口，当前未启用。地址留空可保留；填写新值按追加选项处理。`
      : "尚未配置：请填写本地代理和落地出口。";
}
function probeChainBody() {
  return {
    enabled: $("probe-chain-enabled").checked,
    local_proxy: $("probe-chain-local").value.trim(),
    exit_proxies: $("probe-chain-exit").value.trim(),
    append: $("probe-chain-append").checked,
    protocol: $("probe-chain-protocol").value,
  };
}
$("probe-chain-form").addEventListener("input", () => { chainDirty = true; chainRevision++; });
$("logout").addEventListener("click", () => {
  $("probe-chain-local").value = "";
  $("probe-chain-exit").value = "";
  chainDirty = false;
  chainRevision++;
});
$("probe-chain-test").addEventListener("click", () => action(async () => {
  const result = await api("probe-chain/test", probeChainBody());
  $("probe-chain-result").textContent = result.message;
}));
$("probe-chain-form").addEventListener("submit", event => {
  event.preventDefault();
  action(async () => {
    const revision = chainRevision;
    const result = await api("probe-chain", probeChainBody());
    if (revision === chainRevision) {
      chainDirty = false;
      $("probe-chain-local").value = "";
      $("probe-chain-exit").value = "";
    }
    $("probe-chain-result").textContent = result.message;
    await refresh();
  });
});
