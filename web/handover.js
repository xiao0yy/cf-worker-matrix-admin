"use strict";

// Read-only presentation of the existing protocol. Nothing in this module can
// submit a command or promote an instance. Demo playback uses the same view.
window.Handover = (() => {
  const stages = [
    "请求交接",
    "停止写入",
    "最终备份",
    "释放归属",
    "恢复与校验",
    "启动服务",
    "完成切换",
  ];
  const phases = {
    REQUESTED: [
      0,
      "等待源实例确认",
      "目标已请求交接，源实例尚未完成停写。",
      "等待响应",
      "等待最终备份",
      "等待接管",
    ],
    QUIESCING: [
      1,
      "正在停止旧实例的写入",
      "关闭入口，停止 Matrix 与 Telegram 桥，确认它们干净退出。",
      "正在停写",
      "等待冻结数据",
      "等待接管",
    ],
    QUIESCED: [
      2,
      "数据已冻结，准备最终备份",
      "旧实例已停止写入，接下来保存并验证冻结后的数据库。",
      "已停止写入",
      "准备备份",
      "等待接管",
    ],
    SNAPSHOTTING: [
      2,
      "正在保存最后一份数据",
      "上传冻结快照并完整回读验证，完成后才能释放服务归属。",
      "保持停写",
      "备份与验证中",
      "等待恢复点",
    ],
    READY: [
      3,
      "最终备份就绪，等待交接",
      "已生成经过验证的恢复点，正在等待源实例释放归属。",
      "等待释放",
      "恢复点已就绪",
      "等待归属释放",
    ],
    RELEASED: [
      3,
      "旧实例已释放服务归属",
      "这一刻没有实例拥有服务归属，等待目标实例认领。",
      "已退出服务",
      "恢复点已就绪",
      "等待认领",
    ],
    RESTORING: [
      4,
      "目标已认领，准备恢复数据",
      "目标获得下一代归属，业务入口保持关闭。",
      "保持停止",
      "提供恢复点",
      "准备恢复",
    ],
    VALIDATING: [
      4,
      "正在恢复并校验数据",
      "下载快照、恢复数据库并进行隔离启动验证。此阶段尚未对外服务。",
      "保持停止",
      "提供恢复点",
      "恢复与校验中",
    ],
    STARTING: [
      5,
      "正在启动新实例",
      "数据库恢复与校验已完成，等待服务健康检查和上线确认。",
      "保持停止",
      "恢复点已验证",
      "启动与健康检查",
    ],
    ONLINE: [
      6,
      "交接完成，新实例正在服务",
      "新的归属与实例服务报告一致。旧实例保持停止，不会自动删除数据。",
      "已退出服务",
      "保留恢复点",
      "正在服务",
    ],
  };
  const name = (i) =>
    i?.info?.name || i?.agent?.host?.hostname || i?.id || "尚未确认";
  const fresh = (i, part, now) => {
    const age = now - Date.parse(i?.[part]?.reported_at);
    return (
      i?.[part + "_fresh"] === true &&
      Number.isFinite(age) &&
      age >= -30000 &&
      age <= 90000
    );
  };
  function derive(snapshot, statusValid, now = Date.now()) {
    const c = snapshot?.control;
    if (!c) return null;
    const completed = c.phase === "ONLINE";
    const op = completed ? c.last_completed : c.operation;
    if (
      !op ||
      !["planned", "forced"].includes(op.mode) ||
      !op.id ||
      !op.source ||
      !op.target
    )
      return null;
    const forced = op.mode === "forced";
    const source = snapshot.instances.find((i) => i.id === op.source);
    const target = snapshot.instances.find((i) => i.id === op.target);
    // READY is a real intermediate state: the control phase returns to
    // QUIESCED after snapshot verification, before handing over ownership.
    const phase =
      c.phase === "QUIESCED" && op.phase === "READY" ? "READY" : c.phase;
    const definition = phases[phase];
    const known = !!definition;
    const [step, title, description, sourceStatus, storeStatus, targetStatus] =
      definition || [
        -1,
        "状态暂无法识别",
        "保留当前观测，请导出诊断检查协议状态。",
        "待确认",
        "待确认",
        "待确认",
      ];
    const worker = step < 3 || phase === "READY" ? source : target;
    const stale =
      !statusValid ||
      !fresh(worker, "report", now) ||
      !fresh(worker, "agent", now);
    const currentErrors = Object.values(
      worker?.agent?.worker_errors || {},
    ).filter((e) => e?.operation_id === op.id && e.error);
    const failure =
      worker?.report?.error ||
      worker?.agent?.error ||
      currentErrors[0]?.error ||
      "";
    const onlineConfirmed =
      completed &&
      statusValid &&
      c.owner === op.target &&
      c.epoch === op.next_epoch &&
      fresh(target, "report", now) &&
      target.report.serving === true &&
      target.report.bound_epoch === c.epoch;
    const conflict =
      statusValid &&
      snapshot.instances.some(
        (i) =>
          fresh(i, "report", now) &&
          (!forced ||
            !op.external_isolation?.instances?.includes(i.id) ||
            Date.parse(i.report.reported_at) > Date.parse(op.started_at)) &&
          i.report?.serving &&
          (i.id !== c.owner || i.report.bound_epoch !== c.epoch),
      );
    const mode =
      !known
        ? "unknown"
        : conflict
          ? "error"
          : stale
            ? "stale"
            : failure
              ? "error"
              : worker?.agent?.handover_paused && !completed
                ? "paused"
                : completed && !onlineConfirmed
                  ? "awaiting"
                  : completed
                    ? "complete"
                    : "running";
    const owner =
      c.owner === op.source
        ? "source"
        : c.owner === op.target
          ? "target"
          : c.owner === ""
            ? "none"
            : "unknown";
    return {
      id: op.id,
      forced,
      phase,
      step,
      mode,
      owner,
      source: {
        name: name(source || { id: op.source }),
        ip: source?.info?.public_ip || source?.agent?.public_address?.ip || "",
        status: forced ? "人工确认已隔离" : sourceStatus,
      },
      target: {
        name: name(target || { id: op.target }),
        ip: target?.info?.public_ip || target?.agent?.public_address?.ip || "",
        status:
          completed && mode !== "complete" ? "上线状态待确认" : targetStatus,
      },
      storeStatus: forced ? "使用选定恢复点" : storeStatus,
      title:
        mode === "paused"
          ? "切换推进已暂停"
          : mode === "stale"
            ? "状态更新中断，等待新的报告"
            : mode === "error"
              ? "交接需要检查"
              : mode === "awaiting"
                ? "等待新实例的上线报告"
                : title,
      description:
        mode === "paused"
          ? "保留当前阶段和数据，可在运维工具继续推进。"
          : mode === "stale"
            ? "以下是最后一次观测，动效已暂停。报告过期不代表实例已经停止。"
            : mode === "error"
              ? conflict
                ? "存在与当前归属不一致的服务报告，请先核对实例。"
                : failure
              : mode === "awaiting"
                ? "归属记录已完成，但还没有匹配的有效服务报告，暂不确认成功。"
                : forced
                  ? "从人工选定的恢复点接管；旧机隔离依据操作人的确认，恢复点之后的数据可能丢失。"
                  : description,
      flow:
        mode === "running" && phase === "SNAPSHOTTING"
          ? "upload"
          : mode === "running" && ["RESTORING", "VALIDATING"].includes(phase)
            ? "restore"
            : "none",
      worker: worker?.id || op.target,
      started: op.started_at,
      cutoff: op.cutoff_time,
      snapshot: op.recovery_set?.snapshot_id || "",
      raw: { control_phase: c.phase, operation: op },
    };
  }

  function create(root) {
    root.append(
      document.getElementById("handover-template").content.cloneNode(true),
    );
    const scene = root.querySelector(".handover-scene");
    const fields = Object.fromEntries(
      [...root.querySelectorAll("[data-field]")].map((n) => [
        n.dataset.field,
        n,
      ]),
    );
    const set = (key, value) => {
      if (fields[key].textContent !== value) fields[key].textContent = value;
    };
    const list = root.querySelector(".handover-stages");
    for (const label of stages) {
      const li = document.createElement("li");
      const dot = document.createElement("span");
      dot.className = "stage-dot";
      dot.setAttribute("aria-hidden", "true");
      const text = document.createElement("span");
      text.textContent = label;
      li.append(dot, text);
      list.append(li);
    }
    let model = null,
      visible = true,
      intersecting = true,
      paused = false,
      lastKey = "";
    const pause = root.querySelector("[data-pause]");
    function syncMotion() {
      scene.dataset.motion =
        visible &&
        intersecting &&
        !document.hidden &&
        !paused &&
        model?.mode === "running"
          ? "running"
          : "paused";
      pause.setAttribute("aria-pressed", String(paused));
      pause.textContent = paused ? "播放动效" : "暂停动效";
      pause.disabled = model?.mode !== "running";
    }
    pause.onclick = () => {
      paused = !paused;
      syncMotion();
    };
    if (typeof IntersectionObserver !== "undefined")
      new IntersectionObserver(([entry]) => {
        intersecting = entry.isIntersecting;
        syncMotion();
      }).observe(root);
    document.addEventListener("visibilitychange", syncMotion);
    return {
      setVisible(value) {
        visible = value;
        syncMotion();
      },
      render(next) {
        model = next;
        if (!next) return;
        const key = `${next.id}:${next.phase}:${next.mode}`;
        scene.dataset.phase = next.phase;
        scene.dataset.takeover = next.forced ? "forced" : "planned";
        scene.dataset.mode = next.mode;
        scene.dataset.flow = next.flow;
        scene.dataset.owner = next.owner;
        scene.dataset.stage = String(next.step);
        set("title", next.title);
        set("description", next.description);
        set(
          "state",
          {
            running: next.forced ? "强制接管中" : "交接进行中",
            paused: "已暂停推进",
            complete: "已确认上线",
            stale: "状态待确认",
            error: "需要处理",
            awaiting: "等待确认",
            unknown: "未知状态",
          }[next.mode],
        );
        set("source-name", next.source.name);
        set("source-ip", next.source.ip || "未登记公网 IP");
        set("source-status", next.source.status);
        set("target-name", next.target.name);
        set("target-ip", next.target.ip || "未登记公网 IP");
        set("target-status", next.target.status);
        set("store-status", next.storeStatus);
        set("source-transfer-label", next.forced ? "已有备份" : "保存快照");
        set(
          "owner",
          {
            source: "源实例持有归属",
            none: "归属已释放 · 等待认领",
            target: "目标实例持有归属",
            unknown: "归属待确认",
          }[next.owner],
        );
        set(
          "stage-count",
          next.step < 0
            ? "阶段未知"
            : next.mode === "complete"
              ? next.forced
                ? "3 个阶段已完成"
                : "7 个阶段已完成"
              : next.forced
                ? `阶段 ${next.step - 3} / 3`
                : `阶段 ${next.step + 1} / 7`,
        );
        set(
          "snapshot",
          next.snapshot
            ? "恢复点 " + next.snapshot.slice(0, 12)
            : "等待生成最终恢复点",
        );
        fields.snapshot.title = next.snapshot;
        set("detail", JSON.stringify(next.raw, null, 2));
        for (const [index, item] of [...list.children].entries()) {
          item.hidden = !!next.forced && index < 4;
          item.lastElementChild.textContent =
            next.forced && index === 6 ? "完成接管" : stages[index];
          const done = next.mode === "complete" || index < next.step;
          item.dataset.state = done
            ? "done"
            : index === next.step
              ? "current"
              : "pending";
          if (index === next.step && next.mode !== "complete")
            item.setAttribute("aria-current", "step");
          else item.removeAttribute("aria-current");
          item.querySelector(".stage-dot").textContent = done
            ? ""
            : String(next.forced ? index - 3 : index + 1);
        }
        // DOM identity is stable across polls. Only a real state change gets an
        // arrival transition; a 15-second refresh never restarts the sequence.
        if (
          key !== lastKey &&
          lastKey &&
          fields.title.animate &&
          !window.matchMedia?.("(prefers-reduced-motion: reduce)").matches
        ) {
          fields.title.animate(
            [{ transform: "translateY(5px)" }, { transform: "translateY(0)" }],
            { duration: 240, easing: "cubic-bezier(0.16,1,0.3,1)" },
          );
        }
        lastKey = key;
        syncMotion();
      },
    };
  }
  // Illustrative values only. No callbacks into the management API.
  function demonstration(phase) {
    const [step, title, description, sourceStatus, storeStatus, targetStatus] =
      phases[phase] || phases.REQUESTED;
    const failed = phase === "ERROR",
      stale = phase === "STALE";
    const base = failed || stale ? demonstration("VALIDATING") : null;
    if (base)
      return {
        ...base,
        phase,
        mode: failed ? "error" : "stale",
        flow: "none",
        title: failed ? "恢复校验失败，等待处理" : "目标实例报告已过期",
        description: failed
          ? "演示：数据库校验未通过。停止展示传输动效，保留故障发生的阶段。"
          : "演示：暂时收不到新报告。保留最后进度，不能据此确认实例已停止。",
      };
    return {
      id: "demo-only",
      phase,
      step,
      mode: phase === "ONLINE" ? "complete" : "running",
      owner: phase === "RELEASED" ? "none" : step >= 4 ? "target" : "source",
      source: { name: "源实例", ip: "192.0.2.10", status: sourceStatus },
      target: { name: "目标实例", ip: "192.0.2.20", status: targetStatus },
      title,
      description,
      storeStatus,
      flow:
        phase === "SNAPSHOTTING"
          ? "upload"
          : ["RESTORING", "VALIDATING"].includes(phase)
            ? "restore"
            : "none",
      snapshot: step >= 3 ? "demo-recovery-point" : "",
      raw: { demonstration: true, phase },
    };
  }
  return { derive, create, demonstration };
})();
