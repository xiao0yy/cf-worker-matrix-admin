"use strict";

const $ = (id) => document.getElementById(id);
const text = (id, value) => {
  $(id).textContent =
    typeof value === "string" ? value : JSON.stringify(value, null, 2);
};
const actions = {
  probe: ["检查指令通路", "只检查管理指令能否到达实例，不启停业务。"],
  "handover-request": [
    "切换实例",
    "旧实例停写并完成最终备份后，新实例恢复、校验并接管服务。",
  ],
  "handover-resume": [
    "继续目标实例切换",
    "继续所选目标的原有切换任务，保留原操作编号，不覆盖可能已经写入的新数据。",
  ],
  "handover-cancel": [
    "取消目标实例请求",
    "仅在源实例尚未认领请求时直接取消。如果源实例已开始交接，需要先中止最终备份并恢复源实例，再取消此请求。",
  ],
  "fence-local": [
    "停用实例",
    "关闭所选实例的入口并停止服务。归属不会自动转移到其他机器。",
  ],
  "resume-local": [
    "恢复当前归属实例",
    "只恢复仍拥有当前服务归属、数据代数一致的实例。不能借此接管其他实例。",
  ],
  "source-resume": [
    "中止交接并恢复源实例",
    "仅限归属释放之前；若备份任务还在执行会拒绝操作。恢复后还需取消目标请求，并将 Cloudflare 入口切回源实例。",
  ],
};
const phaseNames = {
  ONLINE: "在线",
  REQUESTED: "等待源实例响应",
  QUIESCING: "正在停写",
  QUIESCED: "已停止写入",
  SNAPSHOTTING: "正在生成最终备份",
  RELEASED: "已释放归属",
  RESTORING: "正在恢复数据",
  VALIDATING: "正在校验数据",
  STARTING: "正在启动服务",
};
const receiptNames = {
  CLAIMED: "实例已接收",
  APPLIED: "指令已处理",
  FAILED: "执行失败",
  UNKNOWN: "结果待确认",
  REJECTED: "指令被拒绝",
};
const terminalReceipts = new Set(["APPLIED", "FAILED", "UNKNOWN", "REJECTED"]);
let snapshot = null,
  statusFetchedAt = 0,
  statusError = "",
  refreshing = false,
  loginExpired = false;
let inFlight = false,
  dnsBusy = false,
  infoBusy = false,
  dnsRecord = null;
let lastCommand = null,
  lastResponse = null,
  commandError = "",
  receiptDismissed = false;
let switchTarget = "",
  reviewContext = null,
  actionContext = null;
let infoTarget = null,
  infoETag = null,
  infoDirty = false;
const storageKey = () => "matrix-admin-v1:" + location.origin;

// All values coming from R2 are text nodes, never HTML.
function element(tag, className, value) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (value !== undefined) node.textContent = value;
  return node;
}
function icon(name) {
  const node = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  node.setAttribute("class", "icon");
  node.setAttribute("viewBox", "0 0 24 24");
  node.setAttribute("fill", "none");
  node.setAttribute("aria-hidden", "true");
  const use = document.createElementNS("http://www.w3.org/2000/svg", "use");
  use.setAttribute("href", "#icon-" + name);
  node.append(use);
  return node;
}
function badge(value, tone) {
  return element("span", "badge " + tone, value);
}
function button(label, handler, className = "text-button") {
  const node = element("button", className, label);
  node.type = "button";
  node.onclick = handler;
  return node;
}
function setBadge(id, value, tone) {
  $(id).className = "badge " + tone;
  text(id, value);
}
function notice(message = "", error = false) {
  text("notice", message);
  $("notice").hidden = !message;
  $("notice").className = "notice" + (error ? " error" : "");
  $("login-link").hidden = !loginExpired;
}
function replaceContent(node, children) {
  const focused = node.contains(document.activeElement)
    ? document.activeElement?.dataset.focusKey
    : null;
  node.replaceChildren(...children);
  if (focused)
    [...node.querySelectorAll("[data-focus-key]")]
      .find((x) => x.dataset.focusKey === focused)
      ?.focus({ preventScroll: true });
}
function time(value) {
  const date = new Date(value);
  return value && Number.isFinite(date.getTime())
    ? date.toLocaleString("zh-CN", { hour12: false })
    : "尚未上报";
}
function ago(value) {
  const age = Date.now() - Date.parse(value);
  if (!Number.isFinite(age)) return "时间未知";
  if (age < -30000) return "时间异常";
  if (age < 60000) return "刚刚";
  if (age < 3600000) return `${Math.floor(age / 60000)} 分钟前`;
  if (age < 86400000) return `${Math.floor(age / 3600000)} 小时前`;
  return `${Math.floor(age / 86400000)} 天前`;
}
function instanceName(i) {
  return (
    i?.info?.name ||
    i?.agent?.host?.hostname ||
    i?.report?.hostname ||
    i?.id ||
    "未知实例"
  );
}
function instanceLabel(i) {
  return `${instanceName(i)}${i?.info?.public_ip ? " · " + i.info.public_ip : ""} [${i?.id || ""}]`;
}
function findInstance(id) {
  return snapshot?.instances.find((i) => i.id === id);
}
function selected() {
  return findInstance($("target").value);
}
function currentStatus() {
  return (
    !!snapshot &&
    !statusError &&
    !loginExpired &&
    Date.now() - statusFetchedAt < 45000
  );
}
function fresh(i, part = "report") {
  const age = Date.now() - Date.parse(i?.[part]?.reported_at);
  return (
    currentStatus() &&
    i?.[part + "_fresh"] === true &&
    Number.isFinite(age) &&
    age >= -30000 &&
    age <= 90000
  );
}
function expectedState() {
  const c = snapshot.control;
  return {
    revision: c.revision,
    epoch: c.epoch,
    owner: c.owner,
    phase: c.phase,
  };
}
function sameState(expected) {
  return (
    currentStatus() &&
    JSON.stringify(expected) === JSON.stringify(expectedState())
  );
}
function instanceError(i) {
  return (
    i?.report?.error ||
    i?.agent?.error ||
    Object.values(i?.agent?.worker_errors || {})
      .map((x) => (typeof x === "string" ? x : x?.error || JSON.stringify(x)))
      .join("；")
  );
}
function role(i) {
  if (!fresh(i)) return ["状态待确认", "warn"];
  if (i.report.serving) {
    if (
      i.id !== snapshot.control.owner ||
      i.report.bound_epoch !== snapshot.control.epoch
    )
      return ["归属异常", "bad"];
    return ["正在服务", "good"];
  }
  if (instanceError(i)) return ["需要处理", "bad"];
  if (i.id === snapshot.control.owner)
    return [
      snapshot.control.phase === "ONLINE" ? "服务已停止" : "正在交接",
      "warn",
    ];
  return fresh(i, "agent") ? ["待命", "neutral"] : ["轮询待确认", "warn"];
}
function health() {
  if (!snapshot)
    return [
      "尚未连接",
      "等待集群状态",
      "成功读取状态后，这里会显示当前服务实例。",
      "neutral",
    ];
  if (!currentStatus())
    return [
      "状态待确认",
      "暂时无法确认服务状态",
      "下方保留最近一次观测。刷新失败不代表实例已经停止。",
      "warn",
    ];
  const c = snapshot.control,
    owner = findInstance(c.owner);
  if (!snapshot.instances.length)
    return [
      "尚无实例",
      "等待实例接入",
      "在服务器准备实例并启动轮询后，这里会显示服务状态。",
      "neutral",
    ];
  const unexpected = snapshot.instances.some(
    (i) =>
      fresh(i) &&
      i.report.serving &&
      (i.id !== c.owner || i.report.bound_epoch !== c.epoch),
  );
  if (unexpected)
    return [
      "需要处理",
      "实例报告与服务归属不一致",
      "请查看实例详情和诊断信息，暂勿发起新的切换。",
      "bad",
    ];
  if (c.phase !== "ONLINE")
    return [
      "切换进行中",
      phaseNames[c.phase] || "正在交接",
      "系统正在完成备份与接管，可在切换页面查看进度。",
      "info",
    ];
  if (!owner || !fresh(owner))
    return [
      "状态待确认",
      "当前实例报告已过期",
      "无法确认当前服务是否仍在运行，请检查实例报告。",
      "warn",
    ];
  if (owner.report.serving !== true)
    return [
      "服务未就绪",
      "当前实例尚未提供服务",
      "查看实例详情，确认停止原因后再执行恢复操作。",
      "warn",
    ];
  if (instanceError(owner) || !fresh(owner, "agent"))
    return [
      "需要检查",
      "服务运行中，有报告异常",
      "服务实例仍报告在线，请检查指令轮询或后台任务。",
      "warn",
    ];
  return [
    "服务运行中",
    "当前服务正常",
    "当前归属与实例报告一致，服务入口已打开。",
    "good",
  ];
}
function backupSource() {
  const owner = findInstance(snapshot?.control.owner);
  if (owner?.agent?.backup_observation) return owner;
  return [...(snapshot?.instances || [])]
    .filter((i) => i.agent?.backup_observation)
    .sort(
      (a, b) =>
        (Date.parse(b.agent.backup_observation.data_time) || 0) -
        (Date.parse(a.agent.backup_observation.data_time) || 0),
    )[0];
}
function backupVerification(b) {
  if (
    b?.verification === "full-restore" &&
    b.database_startup_verified === true
  )
    return "恢复及启动校验已通过";
  if (b?.verification === "database-tree") return "数据库文件校验已通过";
  if (b?.verification === "full-restore")
    return "恢复校验已通过，启动校验未确认";
  return "校验结果待确认";
}
function renderOverview() {
  const [label, title, description, tone] = health();
  setBadge("health-badge", label, tone);
  text("health-title", title);
  text("health-description", description);
  const owner = findInstance(snapshot?.control.owner);
  text(
    "owner-name",
    owner
      ? instanceName(owner)
      : snapshot?.control.owner ||
          (snapshot?.control.phase === "RELEASED"
            ? "归属已释放，等待接管"
            : snapshot
              ? "暂无服务归属"
              : "—"),
  );
  text("owner-ip", owner?.info?.public_ip || (owner ? "未登记公网 IP" : ""));
  text("owner-seen", owner ? ago(owner.report?.reported_at) : "尚未上报");
  text("owner-version", owner?.agent?.software?.version || "尚未上报");
  $("owner-details").disabled = !owner;
  const source = backupSource(),
    backup = source?.agent?.backup_observation;
  text("backup-age", backup ? ago(backup.data_time) : "暂无备份记录");
  text(
    "backup-time",
    backup ? "数据截至 " + time(backup.data_time) : "等待实例上报已完成的备份",
  );
  text("backup-verification", backupVerification(backup));
  document.querySelector(".backup-summary").dataset.verified = String(
    ["database-tree", "full-restore"].includes(backup?.verification),
  );
  text(
    "backup-description",
    backup
      ? `${source.id === snapshot.control.owner ? "" : "来自 " + instanceName(source) + " · "}${fresh(source, "agent") ? "" : "报告已过期 · "}${backup.telegram_included ? "含 Matrix 与 Telegram 数据库" : "Telegram 数据库是否包含：未确认"}`
      : "完成首次备份后显示数据时间和校验结果。",
  );
  $("backup-details").disabled = !backup;
  const warnings =
    snapshot?.instances.filter(
      (i) => !fresh(i) || !fresh(i, "agent") || instanceError(i),
    ) || [];
  const changing = snapshot && snapshot.control.phase !== "ONLINE";
  $("attention").hidden = !changing && !warnings.length;
  if (!$("attention").hidden) {
    $("attention").replaceChildren(
      element(
        "span",
        "",
        changing
          ? "交接正在进行，当前阶段：" +
              (phaseNames[snapshot.control.phase] || snapshot.control.phase)
          : `${warnings.length} 台实例需要检查。过期报告不能用来判断实例已停机。`,
      ),
      button(changing ? "查看切换" : "查看详情", () =>
        changing ? navigate("switch") : openInstance(warnings[0].id),
      ),
    );
  }
  text("instance-count", String(snapshot?.instances.length ?? 0));
  const rows = (snapshot?.instances || []).map((i) => {
    const row = element("div", "instance-row"),
      machine = element("div", "machine");
    const symbol = element(
      "span",
      "machine-symbol" + (i.id === snapshot.control.owner ? " active" : ""),
    );
    symbol.append(icon("server"));
    machine.append(symbol);
    const copy = element("div"),
      name = button(instanceName(i), () => openInstance(i.id), "machine-name");
    name.dataset.focusKey = i.id;
    copy.append(
      name,
      element(
        "span",
        "machine-meta mono",
        i.info?.public_ip || "未登记公网 IP",
      ),
    );
    machine.append(copy);
    const [state, color] = role(i),
      status = element("div", "instance-status");
    status.append(badge(state, color));
    const seen = element("div", "machine-seen", ago(i.report?.reported_at));
    seen.title = time(i.report?.reported_at);
    const version = element(
      "div",
      "machine-version",
      i.agent?.software?.version
        ? "v" + String(i.agent.software.version).replace(/^v/, "")
        : "未上报",
    );
    const detail = button("详情", () => openInstance(i.id));
    detail.dataset.focusKey = i.id + "-detail";
    row.append(machine, status, version, seen, detail);
    return row;
  });
  replaceContent(
    $("instances"),
    rows.length
      ? rows
      : [
          element(
            "div",
            "empty-state",
            snapshot
              ? "尚无实例上报。先在服务器准备实例并启动轮询。"
              : "未能读取实例列表",
          ),
        ],
  );
  renderActivity();
}
function renderActivity() {
  const rows = (snapshot?.instances || [])
    .filter((i) => i.pending)
    .sort(
      (a, b) =>
        Date.parse(b.pending.created_at) - Date.parse(a.pending.created_at),
    )
    .map((i) => {
      const p = i.pending,
        r = i.result,
        row = element("div", "activity-row");
      const state =
        r?.state ||
        (Date.parse(p.expires_at) <= Date.now() ? "EXPIRED" : "PENDING");
      const label =
        receiptNames[state] ||
        (state === "EXPIRED" ? "已过期，待核对" : "等待实例接收");
      row.append(
        badge(
          label,
          state === "APPLIED"
            ? "neutral"
            : ["FAILED", "UNKNOWN", "REJECTED", "EXPIRED"].includes(state)
              ? "warn"
              : "info",
        ),
      );
      const copy = element(
        "div",
        "activity-copy",
        (actions[p.action]?.[0] || p.action) + " · " + instanceName(i),
      );
      copy.append(
        element(
          "small",
          "",
          r?.error ||
            (p.action?.startsWith("handover") && state === "APPLIED"
              ? "指令已处理；交接是否完成以服务状态为准"
              : time(r?.at || p.created_at)),
        ),
      );
      const view = button("查看", () => {
        lastCommand = p;
        lastResponse = null;
        commandError = "";
        receiptDismissed = false;
        renderReceipt();
        $("receipt").scrollIntoView({ behavior: "smooth", block: "nearest" });
      });
      view.dataset.focusKey = p.id;
      row.append(copy, element("time", "", ago(r?.at || p.created_at)), view);
      return row;
    });
  replaceContent(
    $("activity"),
    rows.length
      ? rows
      : [
          element(
            "p",
            "empty-inline",
            "暂无管理操作。服务运行不需要保持此页面打开。",
          ),
        ],
  );
}

async function api(path, method = "GET", payload) {
  if (loginExpired) throw new Error("登录已失效，请重新登录。");
  const res = await fetch(path, {
    method,
    headers: payload ? { "Content-Type": "application/json" } : {},
    body: payload ? JSON.stringify(payload) : undefined,
    cache: "no-store",
    credentials: "same-origin",
    redirect: "manual",
    signal: AbortSignal.timeout(25000),
  });
  if (
    res.type === "opaqueredirect" ||
    res.status === 401 ||
    res.status === 403
  ) {
    loginExpired = true;
    notice(
      "登录已失效或未获授权，请重新登录。已停止刷新，不会自动重发指令。",
      true,
    );
    if (snapshot) render();
    throw new Error("登录已失效或未获授权，请重新登录。");
  }
  let data;
  try {
    data = await res.json();
  } catch {
    throw new Error("服务未返回有效数据，请检查登录状态或 Worker 部署。");
  }
  if (!res.ok) throw new Error(data.error || "请求失败：HTTP " + res.status);
  return data;
}
async function refresh() {
  if (refreshing || loginExpired) return;
  refreshing = true;
  $("refresh").disabled = true;
  try {
    const next = await api("/api/status");
    if (!next.control || !Array.isArray(next.instances))
      throw new Error("状态响应不完整，请导出诊断并检查服务。");
    snapshot = next;
    statusFetchedAt = Date.now();
    statusError = "";
    notice();
    text(
      "updated",
      "更新于 " +
        new Date(statusFetchedAt).toLocaleTimeString("zh-CN", {
          hour12: false,
        }),
    );
  } catch (e) {
    statusError = e.message;
    notice(
      e.message + (snapshot ? " 正在显示上次观测，操作已暂停。" : ""),
      true,
    );
    text("updated", snapshot ? "刷新失败 · 保留上次观测" : "状态读取失败");
  } finally {
    refreshing = false;
    $("refresh").disabled = false;
    render();
  }
}
function render() {
  renderOverview();
  const before = $("target").value;
  $("target").replaceChildren(
    ...(snapshot?.instances || []).map((i) => {
      const option = element(
        "option",
        "",
        instanceName(i) + (i.info?.public_ip ? " · " + i.info.public_ip : ""),
      );
      option.value = i.id;
      return option;
    }),
  );
  $("target").value = findInstance(before)
    ? before
    : snapshot?.control.owner || snapshot?.instances[0]?.id || "";
  if (!selected() && snapshot?.instances.length)
    $("target").value = snapshot.instances[0].id;
  renderTools();
  renderSwitch();
  renderReceipt();
  text("raw", snapshot || {});
  if ($("instance-dialog").open && findInstance(infoTarget))
    renderInstanceDetail();
}
function navigate(page) {
  if (!["overview", "switch", "tools"].includes(page)) page = "overview";
  for (const node of document.querySelectorAll("[data-page]")) {
    if (node.dataset.page === page) node.setAttribute("aria-current", "page");
    else node.removeAttribute("aria-current");
  }
  for (const id of ["overview", "switch", "tools"])
    $("page-" + id).hidden = id !== page;
  text(
    "page-label",
    { overview: "总览", switch: "切换实例", tools: "运维工具" }[page],
  );
  if (location.hash !== "#" + page) history.replaceState(null, "", "#" + page);
  $(page + "-title").focus({ preventScroll: true });
  if (page === "switch") renderSwitch();
  syncHandoverVisibility();
}

function commandBlocker(i) {
  if (!currentStatus()) return "请先刷新，确认状态仍然有效。";
  if (!i) return "请选择实例。";
  if (!fresh(i) || !fresh(i, "agent"))
    return "实例或指令轮询报告已过期，暂不能提交。";
  if (!Number.isSafeInteger(i.report?.bound_epoch))
    return "实例数据代数尚未上报。";
  if (i.pending && !terminalReceipts.has(i.result?.state))
    return "此实例有未结束的指令，请先核对回执。";
  if (inFlight) return "正在提交指令，请稍候。";
  return "";
}
function switchBlocker(i) {
  const blocked = commandBlocker(i);
  if (blocked) return blocked;
  if (snapshot.capabilities?.planned_handover !== true)
    return "此集群尚未启用正常交接。";
  if (snapshot.control.phase !== "ONLINE") return "已有交接正在进行。";
  if (i.id === snapshot.control.owner) return "这已经是当前服务实例。";
  if (
    snapshot.instances.some(
      (node) =>
        fresh(node) &&
        node.report.serving &&
        (node.id !== snapshot.control.owner ||
          node.report.bound_epoch !== snapshot.control.epoch),
    )
  )
    return "实例服务报告与归属不一致，请先处理异常。";
  const owner = findInstance(snapshot.control.owner);
  if (
    !fresh(owner) ||
    !fresh(owner, "agent") ||
    owner.report.serving !== true ||
    owner.report.bound_epoch !== snapshot.control.epoch
  )
    return "请先确认源实例正常在线，才能发起正常交接。";
  if (i.report.serving !== false)
    return "目标实例仍报告正在服务，请先检查归属。";
  return "";
}
function renderSwitch() {
  const c = snapshot?.control;
  const model = window.Handover.derive(snapshot, currentStatus());
  const active =
    !!model && (c.phase !== "ONLINE" || dismissedHandover !== model.id);
  const demo = !$("handover-demo").hidden;
  $("switch-active").hidden = !active || demo;
  $("switch-wizard").hidden = active || demo;
  if (active) {
    liveHandover.render(model);
    $("handover-new").hidden = model.mode !== "complete";
    text(
      "progress-note",
      model.mode === "complete"
        ? "已确认目标上线；旧实例的数据仍保留。"
        : "页面只展示观测状态，关闭页面不会中止交接。",
    );
  }
  if (c && c.phase !== "ONLINE" && !model) {
    $("switch-wizard").hidden = true;
    $("switch-active").hidden = demo;
    text(
      "progress-note",
      "当前不是可识别的实例交接记录，请在运维工具查看完整状态。",
    );
    $("handover-live-view").hidden = true;
    $("handover-new").hidden = true;
  } else $("handover-live-view").hidden = false;
  syncHandoverVisibility();
  text(
    "switch-source",
    "当前服务实例：" +
      instanceName(findInstance(c?.owner) || { id: c?.owner || "尚未确认" }),
  );
  const targets = (snapshot?.instances || []).filter((i) => i.id !== c?.owner);
  if (!targets.some((i) => i.id === switchTarget)) switchTarget = "";
  const rows = targets.map((i) => {
    const blocked = switchBlocker(i),
      label = element("label", "target-option");
    const radio = element("input");
    radio.type = "radio";
    radio.name = "switch-target";
    radio.value = i.id;
    radio.checked = switchTarget === i.id;
    radio.disabled = !!blocked;
    radio.dataset.focusKey = i.id;
    radio.onchange = () => {
      switchTarget = i.id;
      resetReview();
      renderSwitch();
    };
    const copy = element("span", "target-copy");
    copy.append(
      element("strong", "", instanceName(i)),
      element("small", "", i.info?.public_ip || "未登记公网 IP"),
      element("small", "", blocked || "实例与指令轮询报告有效"),
    );
    label.append(
      radio,
      copy,
      badge(blocked ? "暂不可切换" : "可作为目标", blocked ? "warn" : "good"),
    );
    return label;
  });
  replaceContent(
    $("switch-targets"),
    rows.length
      ? rows
      : [
          element(
            "div",
            "empty-state",
            "还没有备用实例。在新 VPS 准备实例并启动轮询后，它会出现在这里。",
          ),
        ],
  );
  const blocked = switchBlocker(findInstance(switchTarget));
  text(
    "switch-blocker",
    switchTarget
      ? blocked
      : targets.length
        ? "请选择一台目标实例。"
        : "准备备用实例不会自动恢复数据或接管服务。",
  );
  $("switch-next").disabled = !!blocked;
  if (reviewContext && !reviewValid()) {
    text("review-error", "集群状态已变化，请返回选择，重新核对后确认。");
  }
  updateSwitchSubmit();
}
let dismissedHandover = "",
  demoTimer = null,
  demoPlaying = false;
const liveHandover = window.Handover.create($("handover-live-view"));
const demoHandover = window.Handover.create($("handover-demo-view"));
const demoSequence = [
  "REQUESTED",
  "QUIESCING",
  "QUIESCED",
  "SNAPSHOTTING",
  "READY",
  "RELEASED",
  "RESTORING",
  "VALIDATING",
  "STARTING",
  "ONLINE",
];
function stopDemo() {
  if (demoTimer !== null) clearTimeout(demoTimer);
  demoTimer = null;
  demoPlaying = false;
  text("handover-demo-play", "播放全过程");
}
function syncHandoverVisibility() {
  const visible = !$("page-switch").hidden && !document.hidden;
  liveHandover.setVisible(visible && !$("switch-active").hidden);
  demoHandover.setVisible(visible && !$("handover-demo").hidden);
  if (!visible) stopDemo();
}
function renderDemo() {
  demoHandover.render(
    window.Handover.demonstration($("handover-demo-phase").value),
  );
  syncHandoverVisibility();
}
function playDemo() {
  if (demoPlaying) {
    stopDemo();
    return;
  }
  demoPlaying = true;
  text("handover-demo-play", "暂停演示");
  let index = demoSequence.indexOf($("handover-demo-phase").value);
  if (index < 0 || index === demoSequence.length - 1) index = 0;
  const step = () => {
    if (!demoPlaying) return;
    $("handover-demo-phase").value = demoSequence[index];
    renderDemo();
    if (index === demoSequence.length - 1) {
      stopDemo();
      return;
    }
    index++;
    demoTimer = setTimeout(step, 2400);
  };
  step();
}

function resetReview() {
  reviewContext = null;
  $("route").checked = false;
  $("switch-confirm").value = "";
  text("review-error", "");
  $("switch-review").hidden = true;
  $("switch-choose").hidden = false;
  $("wizard-step-one").setAttribute("aria-current", "step");
  $("wizard-step-two").removeAttribute("aria-current");
}
function startReview() {
  const target = findInstance(switchTarget),
    blocked = switchBlocker(target);
  if (blocked) {
    text("switch-blocker", blocked);
    return;
  }
  reviewContext = {
    target: target.id,
    expected: expectedState(),
    bound_epoch: target.report.bound_epoch,
    public_ip: target.info?.public_ip || "",
    operation: "move-" + crypto.randomUUID(),
  };
  $("route").checked = false;
  $("switch-confirm").value = "";
  text("review-source", instanceName(findInstance(snapshot.control.owner)));
  text("review-target", instanceName(target));
  text(
    "review-ip",
    target.info?.public_ip || "未登记公网 IP，请先确认目标机器的地址",
  );
  text("review-error", "");
  $("review-dns").hidden = !snapshot.capabilities?.dns;
  $("switch-choose").hidden = true;
  $("switch-review").hidden = false;
  $("wizard-step-one").removeAttribute("aria-current");
  $("wizard-step-two").setAttribute("aria-current", "step");
  updateSwitchSubmit();
  $("route").focus();
}
function reviewValid() {
  const target = findInstance(reviewContext?.target);
  return (
    reviewContext &&
    sameState(reviewContext.expected) &&
    reviewContext.bound_epoch === target?.report?.bound_epoch &&
    reviewContext.public_ip === (target?.info?.public_ip || "") &&
    !switchBlocker(target)
  );
}
function updateSwitchSubmit() {
  $("switch-submit").disabled =
    !reviewValid() ||
    !$("route").checked ||
    $("switch-confirm").value !== "确认";
}
function makeCommand(i, action, expected = expectedState(), operation = "") {
  let source = 0;
  if (action === "handover-request") source = expected.epoch;
  else if (
    ["handover-resume", "handover-cancel", "source-resume"].includes(action)
  ) {
    const current =
      action === "source-resume" ? i.report?.final_backup : i.report?.handover;
    if (!current) throw new Error("此实例没有对应的操作。");
    operation = current.id;
    source = current.source_epoch;
  }
  return {
    schema: 1,
    id: "cmd-" + crypto.randomUUID(),
    cluster_id: snapshot.control.cluster_id,
    target: i.id,
    action,
    created_at: new Date().toISOString(),
    expires_at: new Date(Date.now() + 300000).toISOString(),
    expected,
    bound_epoch: i.report.bound_epoch,
    operation_id: operation,
    source_epoch: source,
  };
}
async function submitSwitch() {
  if (
    !reviewValid() ||
    !$("route").checked ||
    $("switch-confirm").value !== "确认"
  ) {
    text("review-error", "请核对最新状态、入口地址，并输入“确认”。");
    return;
  }
  const command = makeCommand(
    findInstance(reviewContext.target),
    "handover-request",
    reviewContext.expected,
    reviewContext.operation,
  );
  // Leave the wizard after one attempt. An uncertain response can only be retried
  // with the same immutable command, never by silently generating a new ID.
  resetReview();
  await send(command);
}
function renderTools() {
  const i = selected();
  $("tool-target-status").replaceChildren(
    i ? badge(...role(i)) : element("span", "small muted", "等待实例上报"),
  );
  for (const node of document.querySelectorAll("[data-action]")) {
    let why = commandBlocker(i);
    const action = node.dataset.action;
    if (!why && action.startsWith("handover-") && !i.report?.handover)
      why = "此实例没有目标切换任务。";
    if (!why && action === "source-resume" && !i.report?.final_backup)
      why = "此实例没有源备份任务。";
    if (
      !why &&
      action === "resume-local" &&
      (i.id !== snapshot.control.owner ||
        i.report.bound_epoch !== snapshot.control.epoch)
    )
      why = "所选实例不拥有当前服务归属。";
    node.disabled = !!why;
    node.title = why || "";
    node.querySelector("small").textContent = why || node.dataset.description;
  }
  $("edit-selected").disabled = !i;
  $("dns-open").disabled = !currentStatus() || !snapshot?.capabilities?.dns;
  text(
    "dns-capability",
    snapshot?.capabilities?.dns
      ? "读取或修改已配置的回源 A 记录"
      : "未启用 API 管理，请在 Cloudflare 手动修改",
  );
  const abandonable =
    currentStatus() &&
    i?.pending &&
    !i.result &&
    Date.parse(i.pending.expires_at) <= Date.now();
  $("abandon").disabled = !abandonable || inFlight;
  text(
    "abandon-description",
    abandonable
      ? "有一条过期且未收到认领回执的指令"
      : i?.result?.state === "CLAIMED"
        ? "指令已被认领，不能按过期清除"
        : "没有可关闭的过期未认领指令",
  );
}
function openAction(action) {
  const i = selected(),
    blocked = commandBlocker(i);
  if (blocked) {
    notice(blocked, true);
    return;
  }
  actionContext = {
    action,
    target: i.id,
    expected: expectedState(),
    bound_epoch: i.report.bound_epoch,
  };
  text("action-dialog-title", actions[action][0]);
  text("action-target", instanceLabel(i));
  text("action-description", actions[action][1]);
  $("action-confirm").value = "";
  $("action-submit").disabled = true;
  text("action-error", "");
  $("action-dialog").showModal();
}
async function submitAction() {
  const ctx = actionContext,
    i = findInstance(ctx?.target);
  if (
    !ctx ||
    $("action-confirm").value !== "确认" ||
    !sameState(ctx.expected) ||
    commandBlocker(i) ||
    i.report.bound_epoch !== ctx.bound_epoch
  ) {
    text(
      "action-error",
      "状态已变化或尚未确认，请关闭此窗口，刷新后重新操作。",
    );
    return;
  }
  try {
    const command = makeCommand(i, ctx.action, ctx.expected);
    $("action-dialog").close();
    await send(command);
  } catch (e) {
    text("action-error", e.message);
  }
}

async function send(command) {
  if (inFlight) return;
  inFlight = true;
  lastCommand = command;
  lastResponse = null;
  commandError = "";
  receiptDismissed = false;
  try {
    localStorage.setItem(storageKey(), JSON.stringify(command));
  } catch {
    /* Private browsing may disable storage. Keep the in-memory original. */
  }
  renderReceipt();
  renderTools();
  updateSwitchSubmit();
  try {
    lastResponse = await api("/api/command", "POST", command);
  } catch (e) {
    commandError = e.message;
  } finally {
    inFlight = false;
    await refresh();
    renderReceipt();
    renderTools();
    updateSwitchSubmit();
  }
}
function renderReceipt() {
  $("receipt").hidden = !lastCommand || receiptDismissed;
  if (!lastCommand) return;
  const i = findInstance(lastCommand.target);
  const result =
    i?.pending?.id === lastCommand.id ? i.result : lastResponse?.result;
  const state = result?.state;
  let title = inFlight
    ? "正在提交指令"
    : receiptNames[state] ||
      (commandError
        ? "提交结果待确认"
        : lastResponse?.submitted || i?.pending?.id === lastCommand.id
          ? "已提交，等待实例接收"
          : "上次指令，等待核对");
  let message =
    result?.error ||
    commandError ||
    "指令接收与服务完成是两个阶段，请继续查看实例状态。";
  let tone =
    ["FAILED", "UNKNOWN", "REJECTED"].includes(state) || commandError
      ? "warn"
      : "info";
  if (state === "APPLIED" && lastCommand.action === "probe") {
    title = "指令通路检查通过";
    message = "实例已收到并处理这条检查指令，未启停服务。";
    tone = "good";
  }
  if (state === "APPLIED" && lastCommand.action.startsWith("handover")) {
    message =
      "管理指令已处理，尚不代表交接已完成。请查看切换进度与当前服务实例。";
    const c = snapshot?.control;
    if (
      ["handover-request", "handover-resume"].includes(lastCommand.action) &&
      currentStatus() &&
      c.phase === "ONLINE" &&
      c.last_completed?.id === lastCommand.operation_id &&
      c.owner === lastCommand.target &&
      c.epoch === lastCommand.source_epoch + 1 &&
      fresh(i) &&
      i.report.serving &&
      i.report.bound_epoch === c.epoch
    ) {
      title = "切换完成";
      message = "新实例已获得服务归属，并报告正在提供服务。";
      tone = "good";
    }
  }
  if (commandError && !state)
    message += " 先刷新核对；需要重试时，只重发这一条原指令。";
  setBadge("receipt-badge", title, tone);
  text(
    "receipt-title",
    (actions[lastCommand.action]?.[0] || lastCommand.action) +
      " · " +
      instanceName(i || { id: lastCommand.target }),
  );
  text("receipt-message", message);
  text("result", {
    command: lastCommand,
    response: lastResponse,
    result,
    error: commandError || undefined,
  });
  $("retry").hidden = !!state && terminalReceipts.has(state);
  $("retry").disabled =
    inFlight ||
    loginExpired ||
    Date.parse(lastCommand.expires_at) <= Date.now();
  if (!$("retry").hidden && Date.parse(lastCommand.expires_at) <= Date.now())
    $("retry").title = "原指令已过期，请核对实例回执后再处理。";
}
async function retryCommand() {
  if (!lastCommand || inFlight || loginExpired) return;
  if (Date.parse(lastCommand.expires_at) <= Date.now()) {
    notice("这条原指令已过期。请先核对回执，不会自动创建新指令。", true);
    return;
  }
  const original = lastCommand;
  if (
    await confirmRequest(
      "重发这条原指令？",
      `目标：${instanceName(findInstance(original.target) || { id: original.target })}\n将沿用同一编号和完整内容，不会创建新的操作。\n指令编号：${original.id}`,
      "重发原指令",
    )
  ) {
    if (
      inFlight ||
      loginExpired ||
      Date.parse(original.expires_at) <= Date.now()
    ) {
      notice("原指令已过期或当前不可提交，请刷新核对回执。", true);
      return;
    }
    await send(original);
  }
}
async function abandonCommand() {
  const i = selected(),
    pending = i?.pending;
  if (
    !currentStatus() ||
    !pending ||
    i.result ||
    Date.parse(pending.expires_at) > Date.now() ||
    inFlight
  )
    return;
  if (
    !(await confirmRequest(
      "关闭过期指令？",
      `${instanceName(i)} 的这条指令尚未收到认领回执。关闭它不会更改服务归属。\n指令编号：${pending.id}`,
      "关闭指令",
    ))
  )
    return;
  const latest = findInstance(i.id);
  if (
    !currentStatus() ||
    latest?.pending?.id !== pending.id ||
    latest.result ||
    inFlight
  ) {
    notice("指令状态已变化，请刷新后核对。", true);
    return;
  }
  inFlight = true;
  let failure = "";
  try {
    await api("/api/abandon-expired", "POST", { target: i.id, id: pending.id });
    lastCommand = pending;
    commandError = "";
    receiptDismissed = false;
  } catch (e) {
    failure = e.message;
  } finally {
    inFlight = false;
    await refresh();
    if (failure) notice(failure, true);
  }
}

function detailList(entries) {
  const list = element("dl", "detail-list");
  for (const [label, value] of entries) {
    const row = element("div");
    row.append(
      element("dt", "", label),
      element("dd", "", String(value ?? "尚未上报")),
    );
    list.append(row);
  }
  return list;
}
function rawDetails(label, value) {
  const details = element("details", "technical");
  details.append(
    element("summary", "", label),
    element("pre", "", JSON.stringify(value, null, 2)),
  );
  return details;
}
function openInstance(id, edit = false) {
  infoTarget = id;
  infoDirty = false;
  renderInstanceDetail();
  $("identity-editor").open = edit;
  text("instance-info-result", "");
  $("instance-dialog").showModal();
}
function fillInstanceInfo(force = false) {
  const i = findInstance(infoTarget) || selected();
  if (!force && infoDirty && infoTarget === i?.id) return;
  infoTarget = i?.id;
  infoETag = i?.info_etag ?? null;
  infoDirty = false;
  $("instance-name").value = i?.info?.name || "";
  $("instance-ip").value = i?.info?.public_ip || "";
  text("instance-info-target", i?.id || "请选择实例");
}
function renderInstanceDetail() {
  const i = findInstance(infoTarget);
  if (!i) return;
  text("instance-dialog-title", instanceName(i));
  const [status, tone] = role(i);
  const parts = [
    badge(status, tone),
    detailList([
      ["公网 IP", i.info?.public_ip || "未登记"],
      ["主机名", i.agent?.host?.hostname || i.report?.hostname],
      ["服务报告", time(i.report?.reported_at)],
      ["指令轮询", time(i.agent?.reported_at)],
      ["运行版本", i.agent?.software?.version],
      ["实例编号", i.id],
    ]),
  ];
  if (instanceError(i))
    parts.push(element("p", "detail-note", instanceError(i)));
  parts.push(
    rawDetails("技术详情与原始报告", {
      report: i.report,
      agent: i.agent,
      pending: i.pending,
      result: i.result,
    }),
  );
  $("instance-detail").replaceChildren(...parts);
  fillInstanceInfo();
}
async function saveInstanceInfo() {
  const i = findInstance(infoTarget);
  if (!i || infoBusy || !currentStatus()) {
    text("instance-info-result", "请先刷新有效状态。");
    return;
  }
  const id = i.id;
  infoBusy = true;
  $("instance-info-save").disabled = true;
  text("instance-info-result", "正在保存…");
  try {
    await api("/api/instance-info", "POST", {
      id,
      name: $("instance-name").value,
      public_ip: $("instance-ip").value,
      expected_etag: infoETag,
    });
    if (infoTarget === id) {
      infoDirty = false;
      text("instance-info-result", "已保存");
    }
    await refresh();
  } catch (e) {
    text("instance-info-result", e.message);
  } finally {
    infoBusy = false;
    $("instance-info-save").disabled = false;
  }
}
function openBackup() {
  const source = backupSource(),
    b = source?.agent?.backup_observation;
  if (!b) return;
  const note = b.database_startup_verified
    ? "此记录包含数据库启动校验结果。"
    : "此记录未包含数据库启动校验，文件校验通过不等同于完成恢复演练。";
  $("backup-detail").replaceChildren(
    detailList([
      ["来源实例", instanceName(source)],
      ["数据时间", time(b.data_time)],
      ["校验时间", time(b.verified_at)],
      ["校验范围", backupVerification(b)],
      ["Telegram", b.telegram_included ? "已包含" : "未确认包含"],
      [
        "媒体文件",
        b.media_included ? "包含在备份中" : "未打包，继续使用原 S3 存储",
      ],
      [
        "备份方式",
        b.consistency === "globally-quiesced"
          ? "所有服务停写后备份"
          : b.consistency === "independent-online-snapshots"
            ? "各数据库独立在线快照"
            : b.consistency,
      ],
      ["快照编号", b.snapshot_id],
    ]),
    element("p", "detail-note", note),
    element(
      "p",
      "small muted",
      "这里只显示实例上报的最近完成记录，不代表整个备份仓库的历史。正常切换会额外生成停写后的最终备份。",
    ),
    rawDetails("完整备份记录", b),
  );
  $("backup-dialog").showModal();
}
async function diagnostic() {
  try {
    const report = await api("/api/diagnostic");
    const blob = new Blob([JSON.stringify(report, null, 2)], {
        type: "application/json",
      }),
      url = URL.createObjectURL(blob),
      a = element("a");
    a.href = url;
    a.download =
      "matrix-diagnostic-" +
      new Date().toISOString().replaceAll(":", "-") +
      ".json";
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  } catch (e) {
    notice(e.message, true);
  }
}
async function readDNS() {
  if (dnsBusy) return;
  dnsBusy = true;
  $("dns-read").disabled = true;
  $("dns-write").disabled = true;
  text("dns-message", "");
  try {
    dnsRecord = (await api("/api/dns")).record;
    text("dns-state", dnsRecord.name + "\n当前回源 IP：" + dnsRecord.content);
    $("ip").value =
      findInstance(reviewContext?.target)?.info?.public_ip || dnsRecord.content;
  } catch (e) {
    dnsRecord = null;
    text("dns-message", e.message);
  } finally {
    dnsBusy = false;
    $("dns-read").disabled = false;
    $("dns-write").disabled = !dnsRecord;
  }
}
async function openDNS() {
  if (!currentStatus() || !snapshot.capabilities?.dns) return;
  $("dns-dialog").showModal();
  await readDNS();
}
async function writeDNS() {
  if (dnsBusy || !dnsRecord) return;
  const ip = $("ip").value.trim();
  if (
    !(await confirmRequest(
      "修改回源地址？",
      `${dnsRecord.name}\n当前地址：${dnsRecord.content}\n目标地址：${ip}\n这一步只修改 DNS；完成后仍需提交实例交接。`,
      "修改回源 IP",
    ))
  )
    return;
  dnsBusy = true;
  $("dns-write").disabled = true;
  $("dns-read").disabled = true;
  try {
    const result = await api("/api/dns", "POST", {
      id: "dns-" + crypto.randomUUID(),
      ip,
      expected_ip: dnsRecord.content,
    });
    dnsRecord = result.record;
    text("dns-state", dnsRecord.name + "\n当前回源 IP：" + dnsRecord.content);
    text("dns-message", "DNS 已更新。核对入口后，再在切换页面提交交接。");
  } catch (e) {
    dnsRecord = null;
    text("dns-message", e.message + " 请重新读取记录后核对。");
  } finally {
    dnsBusy = false;
    $("dns-read").disabled = false;
    $("dns-write").disabled = !dnsRecord;
  }
}

let confirmationResolve = null;
function confirmRequest(title, description, accept) {
  if (confirmationResolve) return Promise.resolve(false);
  text("confirm-title", title);
  text("confirm-description", description);
  text("confirm-accept", accept);
  $("confirm-dialog").showModal();
  $("confirm-cancel").focus();
  return new Promise((resolve) => {
    confirmationResolve = resolve;
  });
}
function resolveConfirmation(accepted) {
  const resolve = confirmationResolve;
  confirmationResolve = null;
  $("confirm-dialog").close();
  resolve?.(accepted);
}
$("confirm-accept").onclick = () => resolveConfirmation(true);
$("confirm-cancel").onclick = $("confirm-close").onclick = () =>
  resolveConfirmation(false);
$("confirm-dialog").addEventListener("cancel", (e) => {
  e.preventDefault();
  resolveConfirmation(false);
});

async function runProbe() {
  const i = selected(),
    blocked = commandBlocker(i);
  if (blocked) {
    notice(blocked, true);
    return;
  }
  await send(makeCommand(i, "probe"));
}

for (const node of document.querySelectorAll("[data-page]"))
  node.onclick = (e) => {
    e.preventDefault();
    navigate(node.dataset.page);
  };
for (const node of document.querySelectorAll("[data-close]"))
  node.onclick = () => $(node.dataset.close).close();
for (const node of document.querySelectorAll("[data-action]")) {
  node.dataset.description = node.querySelector("small").textContent;
  node.onclick = () =>
    node.dataset.action === "probe"
      ? runProbe()
      : openAction(node.dataset.action);
}
window.addEventListener("hashchange", () => navigate(location.hash.slice(1)));
$("skip-content").onclick = (e) => {
  e.preventDefault();
  $("main-content").focus();
};
$("refresh").onclick = () => {
  loginExpired = false;
  return refresh();
};
$("start-switch").onclick = () => navigate("switch");
$("owner-details").onclick = () =>
  snapshot?.control.owner && openInstance(snapshot.control.owner);
$("progress-tools").onclick = () => {
  const model = window.Handover.derive(snapshot, currentStatus());
  if (model?.worker) $("target").value = model.worker;
  renderTools();
  navigate("tools");
};
$("handover-new").onclick = () => {
  dismissedHandover = snapshot?.control.last_completed?.id || "";
  resetReview();
  renderSwitch();
};
$("handover-demo-open").onclick = () => {
  stopDemo();
  $("handover-demo").hidden = false;
  $("handover-demo-phase").value = "REQUESTED";
  renderDemo();
  renderSwitch();
  $("handover-demo-play").focus();
};
$("handover-demo-close").onclick = () => {
  stopDemo();
  $("handover-demo").hidden = true;
  renderSwitch();
  $("handover-demo-open").focus();
};
$("handover-demo-phase").onchange = () => {
  stopDemo();
  renderDemo();
};
$("handover-demo-play").onclick = playDemo;
document.addEventListener("visibilitychange", syncHandoverVisibility);
$("switch-next").onclick = startReview;
$("switch-back").onclick = () => {
  resetReview();
  renderSwitch();
};
$("route").onchange = $("switch-confirm").oninput = updateSwitchSubmit;
$("switch-submit").onclick = submitSwitch;
$("target").onchange = renderTools;
$("edit-selected").onclick = () =>
  selected() && openInstance(selected().id, true);
$("instance-name").oninput = $("instance-ip").oninput = () => {
  infoDirty = true;
};
$("instance-info-save").onclick = saveInstanceInfo;
$("backup-details").onclick = openBackup;
$("action-confirm").oninput = () => {
  $("action-submit").disabled =
    $("action-confirm").value !== "确认" || inFlight;
};
$("action-submit").onclick = submitAction;
$("retry").onclick = retryCommand;
$("abandon").onclick = abandonCommand;
$("receipt-hide").onclick = () => {
  receiptDismissed = true;
  $("receipt").hidden = true;
};
for (const id of ["diagnostic", "overview-diagnostic", "receipt-diagnostic"])
  $(id).onclick = diagnostic;
$("dns-open").onclick = $("review-dns").onclick = openDNS;
$("dns-read").onclick = readDNS;
$("dns-write").onclick = writeDNS;
try {
  const saved = JSON.parse(localStorage.getItem(storageKey()) || "null");
  if (
    saved?.schema === 1 &&
    actions[saved.action] &&
    typeof saved.id === "string"
  )
    lastCommand = saved;
} catch {
  /* An unavailable storage or malformed previous entry is not a command. */
}
navigate(location.hash.slice(1));
if (location.protocol === "https:") {
  refresh();
  setInterval(refresh, 15000);
} else {
  notice("请打开已部署的 Worker HTTPS 地址，通过 Cloudflare Access 登录。");
}
