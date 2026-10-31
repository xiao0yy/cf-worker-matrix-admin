"use strict";
window.Management = (() => {
  let catalog = { points: [], tasks: [] },
    catalogAt = 0,
    loading = false,
    loadError = "",
    releases = [],
    releasesAt = 0,
    releaseLoading = false,
    tab = "backups",
    page = 0,
    takeoverContext = null;
  const terminal = new Set(["SUCCEEDED", "FAILED", "CANCELLED", "INTERRUPTED"]);
  const states = {
    QUEUED: "等待执行",
    RUNNING: "正在执行",
    SUCCEEDED: "已完成",
    FAILED: "失败",
    CANCELLED: "已取消",
    INTERRUPTED: "执行中断",
  };
  const stages = {
    queued: "等待实例开始",
    "checking-authority": "核对操作权限",
    "check-dependencies": "检查基础依赖",
    "verify-recovery-catalog": "校验恢复配置",
    "check-backup-storage": "检查备份可读性与容量",
    "pull-pinned-images": "准备固定版本镜像",
    "checkpoint-and-upload": "制作并上传数据库备份",
    "read-recovery-point": "读取恢复点",
    "download-and-verify-databases": "下载并校验数据库",
    "isolated-database-startup": "隔离启动验证",
    "download-runtime": "下载并校验版本",
    "install-runtime": "安装运行程序",
    "claim-new-epoch": "取得新一代归属",
    "takeover-restoring": "恢复数据库",
    "takeover-validating": "验证恢复结果",
    "takeover-starting": "等待服务上线",
    "prune-and-check": "回收存储并校验仓库",
    "retire-reviewed-snapshots": "删除计划内的旧恢复点",
    "remove-retired-local-data": "清理旧实例数据",
  };
  const node = (tag, cls, value) => element(tag, cls, value);
  function blocker(i, action) {
    const common = commandBlocker(i);
    if (common) return common;
    if (
      i.agent?.management_protocol !== 1 ||
      !i.agent.actions?.includes(action)
    )
      return "此实例需先升级到支持远程维护的版本";
    const c = snapshot.control,
      owner = c.owner === i.id,
      stopped = i.report.blocked && !i.report.serving;
    if (
      ![
        "task-cancel",
        "task-retry",
        "handover-pause",
        "handover-continue",
      ].includes(action) &&
      i.agent.active_tasks?.length
    )
      return "已有维护任务，请等待完成或先取消。";
    if (
      ["backup-now", "confirm-delivery"].includes(action) &&
      (!owner || !i.report.serving || c.phase !== "ONLINE")
    )
      return "此操作需要正在服务的当前实例。";
    if (action === "upgrade-runtime" && (!stopped || c.phase !== "ONLINE"))
      return "先停用此实例，并等待当前切换结束，再安装版本。";
    if (action === "refresh-config" && owner && !stopped)
      return "当前服务实例需先停用，才能更新 nginx。";
    if (
      action === "storage-apply" &&
      (!owner || !stopped || c.phase !== "ONLINE")
    )
      return "先停用当前归属实例，再执行备份回收计划。";
    if (
      action === "maintenance-recover" &&
      (!owner || !stopped || c.phase !== "MAINTENANCE")
    )
      return "仅用于退出当前实例未完成的存储维护。";
    if (
      ["cleanup-plan", "cleanup-apply"].includes(action) &&
      (owner || !stopped || c.phase !== "ONLINE")
    )
      return "仅可在新实例上线后，清理已停用的旧实例。";
    if (
      ["preflight", "restore-drill"].includes(action) &&
      c.phase === "MAINTENANCE"
    )
      return "存储维护结束后再检查或恢复。";
    return "";
  }
  async function queue(id, action, parameters = {}, confirm = true) {
    const i = findInstance(id),
      why = blocker(i, action);
    if (why) {
      notice(why, true);
      return;
    }
    const expected = expectedState(),
      bound = i.report.bound_epoch;
    if (
      confirm &&
      !(await confirmRequest(
        actions[action]?.[0] || action,
        `实例：${instanceName(i)}。${description(action)}`,
        "确认提交",
      ))
    )
      return;
    if (
      !sameState(expected) ||
      blocker(findInstance(id), action) ||
      findInstance(id).report.bound_epoch !== bound
    ) {
      notice("状态已经变化，请刷新后重新确认。", true);
      return;
    }
    const command = makeCommand(findInstance(id), action, expected);
    delete command.operation_id;
    delete command.source_epoch;
    command.parameters = parameters;
    await send(command);
    catalogAt = 0;
    await load(true);
    if (action !== "force-takeover") selectTab("tasks");
  }
  function description(action) {
    return (
      {
        "backup-now": "保存当前数据库，媒体继续使用原 S3。",
        "restore-drill": "下载到独立目录，校验数据库；不会启用业务。",
        "upgrade-runtime":
          "安装所选固定版本。实例需保持停用或备用，不会自动接管服务。",
        "force-takeover": "使用指定旧备份恢复，随后启用目标实例。",
        "storage-apply": "按已审阅计划删除备份并回收空间；完成后实例仍停用。",
        "cleanup-apply": "永久删除计划中的本地数据，保留实例身份和配置。",
        "handover-pause":
          "暂停后续切换步骤并终止长任务；已完成的停写或归属变更会保留。",
        "handover-continue":
          "继续原有任务。STARTING 阶段被停用时，仍需显式恢复目标实例。",
        "task-cancel": "停止任务后续步骤，保留已经完成的操作和数据。",
        "task-retry": "沿用原任务编号重试；权限或状态已改变时会拒绝。",
        "confirm-delivery": "只记录你观察到的双向新消息结果，有效期 15 分钟。",
      }[action] || "操作由实例执行，关闭页面不会中止。"
    );
  }
  function selectTab(next) {
    tab = next;
    for (const b of document.querySelectorAll("[data-maint-tab]")) {
      const on = b.dataset.maintTab === tab;
      b.setAttribute("aria-selected", String(on));
      b.tabIndex = on ? 0 : -1;
      $("panel-" + b.dataset.maintTab).hidden = !on;
    }
    render();
    if (tab === "updates") loadReleases();
  }
  async function load(force = false, append = false) {
    if (
      !snapshot ||
      loading ||
      (!force &&
        (($("page-tools").hidden && !$("takeover-dialog").open) ||
          Date.now() - catalogAt < 30000))
    )
      return;
    if (!append) page = 0;
    loading = true;
    $("catalog-refresh").disabled = true;
    try {
      const result = await api("/api/catalog?offset=" + page * 100);
      catalog = append
        ? {
            ...result,
            points: [...catalog.points, ...result.points],
            tasks: [...catalog.tasks, ...result.tasks],
          }
        : result;
      catalogAt = Date.now();
      loadError = "";
    } catch (e) {
      loadError = e.message;
    } finally {
      loading = false;
      $("catalog-refresh").disabled = false;
      render();
    }
  }
  async function loadReleases(force = false) {
    if (
      releaseLoading ||
      !snapshot ||
      (!force && Date.now() - releasesAt < 3600000)
    )
      return;
    releaseLoading = true;
    text("release-message", "正在检查公开发布的完整版本…");
    try {
      const result = await api("/api/releases");
      releases = result.releases;
      releasesAt = Date.now();
      text(
        "release-message",
        releases.length
          ? "已校验版本清单；选择安装前可查看发布说明。"
          : "尚无支持远程升级的完整版本。旧版本需由 Agent 首次升级。",
      );
      renderVersions();
    } catch (e) {
      text("release-message", e.message + "。已安装版本不受影响。");
    } finally {
      releaseLoading = false;
    }
  }
  function taskButton(label, id, action, p) {
    const b = button(label, () => queue(id, action, p), "text-button");
    b.disabled = !!blocker(findInstance(id), action);
    b.title = blocker(findInstance(id), action);
    return b;
  }
  function render() {
    const i = selected();
    text(
      "management-capability",
      i?.agent?.management_protocol === 1
        ? "任务在实例上独立执行，结果保存在控制存储。"
        : "此实例尚未支持远程维护，先由 Agent 升级运行程序；新版本不会自动安装。",
    );
    text("management-error", loadError);
    $("tasks-more").hidden = !catalog.tasks_truncated;
    for (const b of document.querySelectorAll("[data-maint-action]")) {
      const why = blocker(i, b.dataset.maintAction);
      b.disabled = !!why;
      b.title = why;
    }
    for (const [id, action] of [
      ["backup-now", "backup-now"],
      ["runtime-upgrade", "upgrade-runtime"],
      ["storage-plan", "storage-plan"],
      ["delivery-confirm", "confirm-delivery"],
    ]) {
      $(id).disabled = !!blocker(i, action);
      $(id).title = blocker(i, action);
    }
    $("force-open").disabled = !snapshot || !currentStatus();
    if (tab === "backups") renderPoints(i);
    if (tab === "tasks") renderTasks(i);
    if (tab === "storage") renderPlans(i);
    if (tab === "updates") renderVersions();
  }
  function renderPoints(i) {
    const rows = catalog.points.map((p) => {
      const row = node("div", "maintenance-row"),
        copy = node("div", "maintenance-copy");
      copy.append(
        node("strong", "", time(p.data_time)),
        node(
          "span",
          "small muted",
          `${p.consistency === "globally-quiesced" ? "停写一致性备份" : "在线检查点"} · ${p.database_startup_verified ? "已验证数据库启动" : "数据库启动未验证"}`,
        ),
        node("code", "snapshot-id", p.snapshot_id),
      );
      row.append(
        copy,
        taskButton("恢复演练", i?.id, "restore-drill", {
          snapshot_id: p.snapshot_id,
          point_sha256: p.point_sha256,
        }),
      );
      return row;
    });
    replaceContent(
      $("recovery-history"),
      rows.length
        ? rows
        : [
            node(
              "p",
              "empty-state",
              loading
                ? "正在读取恢复点…"
                : "尚无可用恢复点。先在在线实例完成一次备份。",
            ),
          ],
    );
    text(
      "catalog-coverage",
      `已显示 ${catalog.points.length} 个恢复点；归档淘汰的恢复点不可用于接管。`,
    );
    $("catalog-more").hidden =
      !catalog.points_truncated && !catalog.tasks_truncated;
  }
  function renderTasks(i) {
    const scheduled = i?.agent?.scheduled_backup;
    text(
      "scheduled-backup",
      scheduled
        ? `定时备份：${states[scheduled.state] || scheduled.state} · ${time(scheduled.updated_at)}${scheduled.error ? " · " + scheduled.error : ""}`
        : "定时备份尚未报告任务状态。",
    );
    const tasks = catalog.tasks.filter((t) => t.request?.target === i?.id),
      byId = new Map(tasks.map((t) => [t.request.id, t]));
    for (const active of i?.agent?.active_tasks || []) {
      const prior = byId.get(active.id);
      byId.set(active.id, {
        ...prior,
        ...active,
        request: prior?.request || {
          id: active.id,
          action: active.action,
          target: i.id,
        },
      });
    }
    const rows = [...byId.values()]
      .sort((a, b) => new Date(b.updated_at) - new Date(a.updated_at))
      .map((t) => {
        const row = node("div", "maintenance-row task-row"),
          copy = node("div", "maintenance-copy"),
          heading = node("div", "task-heading");
        heading.append(
          node(
            "strong",
            "",
            actions[t.request.action]?.[0] || t.request.action,
          ),
          badge(
            states[t.state] || t.state,
            t.state === "SUCCEEDED"
              ? "good"
              : ["FAILED", "INTERRUPTED"].includes(t.state)
                ? "bad"
                : "neutral",
          ),
        );
        copy.append(
          heading,
          node(
            "span",
            "small muted",
            `${stages[t.stage] || t.stage} · ${time(t.updated_at)}`,
          ),
        );
        if (t.error) copy.append(node("p", "task-error", t.error));
        if (t.result) {
          const detail = node("details", "task-detail"),
            summary = node("summary", "", "执行结果");
          detail.append(
            summary,
            node("pre", "", JSON.stringify(t.result, null, 2)),
          );
          copy.append(detail);
        }
        row.append(copy);
        if (!terminal.has(t.state)) {
          const cancel = taskButton(
            t.stage === "install-runtime" ? "安装中，等待收尾" : "取消任务",
            i.id,
            "task-cancel",
            { task_id: t.request.id },
          );
          if (t.stage === "install-runtime") cancel.disabled = true;
          row.append(cancel);
        } else if (t.state !== "SUCCEEDED")
          row.append(
            taskButton("重试", i.id, "task-retry", { task_id: t.request.id }),
          );
        return row;
      });
    replaceContent(
      $("task-history"),
      rows.length
        ? rows
        : [node("p", "empty-state", "这台实例还没有维护任务。")],
    );
  }
  function renderVersions() {
    const i = selected();
    text("runtime-version", i?.agent?.software?.version || "尚未上报");
    const old = $("runtime-release").value;
    $("runtime-release").replaceChildren(
      ...(releases.length
        ? releases.map((r) => {
            const o = node(
              "option",
              "",
              `v${r.version}${r.version === i?.agent?.software?.version ? " · 已安装" : ""}`,
            );
            o.value = r.version;
            return o;
          })
        : [node("option", "", "暂无可安装版本")]),
    );
    if (releases.some((r) => r.version === old))
      $("runtime-release").value = old;
    $("runtime-upgrade").disabled =
      !!blocker(i, "upgrade-runtime") || !releases.length;
    const release = releases.find(
      (r) => r.version === $("runtime-release").value,
    );
    const notes = $("release-notes");
    notes.hidden = !release;
    if (release)
      notes.href =
        "https://github.com/xiao0yy/cf-worker-matrix-admin/releases/tag/v" +
        encodeURIComponent(release.version);
    const h = i?.agent?.health;
    text(
      "business-health",
      `入口：${i?.report?.serving ? "已开放" : "已关闭"}。Telegram 双向投递：${h?.telegram_delivery === "operator_verified_both_directions" ? "人工验证通过（" + time(h.delivery_verified_at) + "）" : "尚未验证"}。容器运行或 HTTP 检查通过不代表群消息双向正常。`,
    );
  }
  function renderPlans(i) {
    const rows = catalog.tasks
      .filter(
        (t) =>
          t.request?.target === i?.id &&
          ["storage-plan", "cleanup-plan"].includes(t.request.action) &&
          t.state === "SUCCEEDED" &&
          t.result?.plan,
      )
      .map((t) => {
        const p = t.result.plan,
          row = node("div", "maintenance-row"),
          copy = node("div", "maintenance-copy");
        copy.append(
          node(
            "strong",
            "",
            t.request.action === "storage-plan"
              ? `删除 ${p.delete_snapshots.length} 个旧恢复点，保留 ${p.keep_snapshots.length} 个`
              : `删除 ${p.paths.length} 处本地数据，约 ${(p.bytes / 1048576).toFixed(1)} MiB`,
          ),
          node("span", "small muted", time(p.at)),
        );
        const details = node("details", "task-detail"),
          list = node(
            "pre",
            "",
            (p.delete_snapshots || p.paths).join("\n") || "没有需要删除的数据",
          );
        details.append(node("summary", "", "核对待删除内容"), list);
        copy.append(details);
        row.append(
          copy,
          taskButton(
            "确认执行计划",
            i.id,
            t.request.action === "storage-plan"
              ? "storage-apply"
              : "cleanup-apply",
            { plan_id: p.id, plan_sha256: t.result.plan_sha256 },
          ),
        );
        return row;
      });
    replaceContent(
      $("cleanup-plans"),
      rows.length ? rows : [node("p", "empty-state", "生成计划不会删除数据。")],
    );
  }
  async function openTakeover() {
    await load(true);
    if (loadError) {
      notice(loadError, true);
      return;
    }
    takeoverContext = expectedState();
    const candidates = snapshot.instances.filter(
      (i) =>
        i.id !== snapshot.control.owner &&
        i.agent?.management_protocol === 1 &&
        !blocker(i, "force-takeover"),
    );
    $("takeover-target").replaceChildren(
      ...candidates.map((i) => {
        const o = node("option", "", instanceLabel(i));
        o.value = i.id;
        return o;
      }),
    );
    $("takeover-point").replaceChildren(
      ...catalog.points.map((p) => {
        const o = node(
          "option",
          "",
          time(p.data_time) + " · " + p.snapshot_id.slice(0, 12),
        );
        o.value = p.snapshot_id;
        return o;
      }),
    );
    $("takeover-note").value = "";
    $("takeover-confirm").value = "";
    $("takeover-loss").checked = false;
    $("takeover-route").checked = false;
    text(
      "takeover-error",
      candidates.length
        ? catalog.points.length
          ? ""
          : "没有可用恢复点，无法接管。"
        : "没有支持强制接管且报告有效的备用实例。请先升级并启动新实例轮询。",
    );
    $("takeover-submit").disabled =
      !candidates.length || !catalog.points.length;
    renderIsolation();
    pointNote();
    $("takeover-dialog").showModal();
  }
  function renderIsolation() {
    const c = snapshot.control,
      target = $("takeover-target").value;
    const ids = [
      ...new Set(
        [c.owner, c.operation?.source, c.operation?.target].filter(
          (id) => id && id !== target,
        ),
      ),
    ];
    $("takeover-isolated").replaceChildren(
      ...ids.map((id) => {
        const label = node("label", "inline-check"),
          check = node("input");
        check.type = "checkbox";
        check.value = id;
        check.required = true;
        label.append(
          check,
          node("span", "", instanceLabel(findInstance(id) || { id })),
        );
        return label;
      }),
    );
  }
  function pointNote() {
    const p = catalog.points.find(
      (p) => p.snapshot_id === $("takeover-point").value,
    );
    text(
      "takeover-point-note",
      p
        ? `恢复至 ${time(p.data_time)}。之后的数据可能丢失；${p.consistency === "globally-quiesced" ? "此点已冻结所有数据库。" : "此点是在线备份，Matrix 与桥的取样时刻可能不同。"}`
        : "请选择恢复点。",
    );
  }
  $("takeover-form").onsubmit = async (event) => {
    event.preventDefault();
    const target = $("takeover-target").value,
      p = catalog.points.find(
        (p) => p.snapshot_id === $("takeover-point").value,
      ),
      checks = [...$("takeover-isolated").querySelectorAll("input")];
    if (
      !sameState(takeoverContext) ||
      !p ||
      $("takeover-confirm").value !== "强制接管" ||
      !$("takeover-loss").checked ||
      !$("takeover-route").checked ||
      checks.some((x) => !x.checked) ||
      !checks.length ||
      $("takeover-note").value.trim().length < 3
    ) {
      text(
        "takeover-error",
        "请完成隔离与数据损失确认；状态若已变化，请关闭后重新打开。 ",
      );
      return;
    }
    const parameters = {
      snapshot_id: p.snapshot_id,
      point_sha256: p.point_sha256,
      isolated_instances: checks.map((x) => x.value),
      isolation_method: $("takeover-method").value,
      note: $("takeover-note").value.trim(),
      accept_data_loss: true,
    };
    $("takeover-dialog").close();
    await queue(target, "force-takeover", parameters, false);
    navigate("switch");
  };
  for (const b of document.querySelectorAll("[data-maint-tab]")) {
    b.onclick = () => selectTab(b.dataset.maintTab);
    b.onkeydown = (e) => {
      if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(e.key)) return;
      e.preventDefault();
      const tabs = [...document.querySelectorAll("[data-maint-tab]")],
        at = tabs.indexOf(b);
      const next =
        e.key === "Home"
          ? 0
          : e.key === "End"
            ? tabs.length - 1
            : (at + (e.key === "ArrowRight" ? 1 : -1) + tabs.length) %
              tabs.length;
      selectTab(tabs[next].dataset.maintTab);
      tabs[next].focus();
    };
  }
  for (const b of document.querySelectorAll("[data-maint-action]"))
    b.onclick = () => queue(selected()?.id, b.dataset.maintAction, {});
  $("backup-now").onclick = () =>
    queue(selected()?.id, "backup-now", { full: $("backup-full").checked });
  $("storage-plan").onclick = () =>
    queue(selected()?.id, "storage-plan", {
      keep_last: Number($("keep-last").value),
      keep_days: Number($("keep-days").value),
    });
  $("runtime-release").onchange = renderVersions;
  $("runtime-upgrade").onclick = () => {
    const r = releases.find((r) => r.version === $("runtime-release").value);
    if (r)
      queue(selected()?.id, "upgrade-runtime", {
        version: r.version,
        manifest_sha256: r.manifest_sha256,
      });
  };
  $("delivery-confirm").onclick = () =>
    queue(selected()?.id, "confirm-delivery", {
      note: "both-directions-received",
    });
  $("release-refresh").onclick = () => loadReleases(true);
  $("catalog-refresh").onclick = () => {
    page = 0;
    load(true);
  };
  $("catalog-more").onclick = $("tasks-more").onclick = () => {
    page++;
    load(true, true);
  };
  $("force-open").onclick = openTakeover;
  $("takeover-target").onchange = renderIsolation;
  $("takeover-point").onchange = pointNote;
  render();
  if (location.protocol === "https:") load();
  return { render, load, queue, openTakeover, selectTab };
})();
