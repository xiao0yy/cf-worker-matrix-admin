// Bounded declarative commands shared with internal/management/contract.go.
export class ManagementError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}
const fail = (message, status = 400) => {
  throw new ManagementError(status, message);
};
const id = (v) => typeof v === "string" && /^[A-Za-z0-9_-]{1,100}$/.test(v);
const hash = (v) => typeof v === "string" && /^[a-f0-9]{64}$/.test(v);
const version = (v) =>
  typeof v === "string" &&
  /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[a-zA-Z0-9.-]+)?$/.test(v);
export const taskActions = new Set([
  "preflight",
  "backup-now",
  "restore-drill",
  "upgrade-runtime",
  "refresh-config",
  "storage-plan",
  "storage-apply",
  "cleanup-plan",
  "cleanup-apply",
  "health-check",
  "confirm-delivery",
  "maintenance-recover",
  "force-takeover",
]);
export const managementActions = new Set([
  ...taskActions,
  "task-cancel",
  "task-retry",
  "handover-pause",
  "handover-continue",
]);
export function validateParameters(c) {
  if (!id(c.id)) fail("任务编号无效");
  const p = c.parameters ?? {};
  if (!p || Array.isArray(p) || typeof p !== "object")
    fail("任务参数必须是对象");
  let allowed = [];
  switch (c.action) {
    case "preflight":
    case "restore-drill":
    case "refresh-config":
      allowed = ["snapshot_id", "point_sha256"];
      break;
    case "backup-now":
      allowed = ["full"];
      if (p.full !== undefined && typeof p.full !== "boolean")
        fail("full 参数无效");
      break;
    case "upgrade-runtime":
      allowed = ["version", "manifest_sha256"];
      if (!version(p.version) || !hash(p.manifest_sha256))
        fail("请选择有完整校验信息的固定版本");
      break;
    case "storage-plan":
      allowed = ["keep_last", "keep_days"];
      if (
        !Number.isInteger(p.keep_last) ||
        p.keep_last < 2 ||
        p.keep_last > 10000 ||
        !Number.isInteger(p.keep_days) ||
        p.keep_days < 1 ||
        p.keep_days > 3650
      )
        fail("至少保留 2 个恢复点和 1 天");
      break;
    case "storage-apply":
    case "cleanup-apply":
      allowed = ["plan_id", "plan_sha256"];
      if (!id(p.plan_id) || !hash(p.plan_sha256))
        fail("需要已审阅计划的编号和校验和");
      break;
    case "task-cancel":
    case "task-retry":
      allowed = ["task_id"];
      if (!id(p.task_id)) fail("请选择具体任务");
      break;
    case "confirm-delivery":
      allowed = ["note"];
      if (p.note !== "both-directions-received")
        fail("需要确认已验证两个方向的新消息");
      break;
    case "force-takeover":
      allowed = [
        "snapshot_id",
        "point_sha256",
        "isolated_instances",
        "isolation_method",
        "note",
        "accept_data_loss",
      ];
      if (
        !hash(p.snapshot_id) ||
        !hash(p.point_sha256) ||
        p.accept_data_loss !== true ||
        !Array.isArray(p.isolated_instances) ||
        p.isolated_instances.length < 1 ||
        p.isolated_instances.length > 16 ||
        p.isolated_instances.some((v) => !id(v) || v === c.target) ||
        new Set(p.isolated_instances).size !== p.isolated_instances.length ||
        !["powered-off", "fully-isolated"].includes(p.isolation_method) ||
        typeof p.note !== "string" ||
        p.note.trim().length < 3 ||
        p.note.length > 500
      )
        fail("强制接管需要明确的恢复点、旧节点隔离记录和数据损失确认");
      break;
  }
  if (Object.keys(p).some((k) => !allowed.includes(k)))
    fail("此操作不接受这些参数");
  if (
    (p.snapshot_id !== undefined || p.point_sha256 !== undefined) &&
    (!hash(p.snapshot_id) || !hash(p.point_sha256))
  )
    fail("恢复点编号与校验和必须成对提供");
  if (c.operation_id || c.source_epoch) fail("任务不能包含旧式切换参数");
  return p;
}
export function validateManagementTarget(c, state, instance) {
  if (!managementActions.has(c.action)) return;
  if (
    instance.agent?.management_protocol !== 1 ||
    !instance.agent?.actions?.includes(c.action)
  )
    fail("此实例尚未支持该操作，请先升级实例运行程序", 409);
  if (c.action === "force-takeover") {
    if (state.owner === c.target)
      fail("不能用旧备份覆盖当前归属实例，请选择另一台备用实例", 409);
    for (const possible of [
      state.owner,
      state.operation?.source,
      state.operation?.target,
    ])
      if (
        possible &&
        possible !== c.target &&
        !c.parameters.isolated_instances.includes(possible)
      )
        fail("必须确认全部可能写入的旧实例已关闭或完全隔离", 409);
  }
}
export async function catalog(env, helpers, offset = 0) {
  if (!Number.isInteger(offset) || offset < 0 || offset > 100000)
    fail("分页参数无效");
  const { prefix, jsonObject, allObjects } = helpers,
    base = prefix(env);
  const keys = await allObjects(env.CONTROL, base + "recovery/v2/points/");
  const retiredKeys = await allObjects(
    env.CONTROL,
    base + "recovery/v2/retired/",
  );
  const retired = new Set(
    retiredKeys.map((x) => x.key.split("/").pop().slice(0, -5)),
  );
  // Most recent upload first, bounded reads. The API tells callers when older
  // entries were omitted instead of presenting an incomplete list as complete.
  const newest = keys
    .filter((x) => !retired.has(x.key.split("/").pop().slice(0, -5)))
    .sort((a, b) => new Date(b.uploaded) - new Date(a.uploaded));
  const points = [];
  for (const entry of newest.slice(offset, offset + 100)) {
    const obj = await env.CONTROL.get(entry.key);
    if (!obj) continue;
    if (obj.size > 1024 * 1024) fail("恢复点对象过大", 502);
    const text = await obj.text();
    let p;
    try {
      p = JSON.parse(text);
    } catch {
      fail("恢复目录损坏", 502);
    }
    if (
      p.protocol !== 2 ||
      p.point_schema !== 2 ||
      p.instance_control?.cluster_id !== env.CLUSTER_ID ||
      !hash(p.snapshot_id) ||
      entry.key !== base + "recovery/v2/points/" + p.snapshot_id + ".json"
    )
      fail("恢复点身份不匹配", 502);
    points.push({
      snapshot_id: p.snapshot_id,
      point_sha256: await digest(text),
      data_time: p.data_time,
      consistency: p.consistency,
      verification: p.verification,
      database_startup_verified: p.database_startup_verified,
      source_instance_id: p.instance_control.source_instance_id,
      source_epoch: p.instance_control.source_epoch,
      software: p.software,
      media_included: false,
    });
  }
  points.sort((a, b) => new Date(b.data_time) - new Date(a.data_time));
  const taskKeys = await allObjects(env.CONTROL, base + "tasks/");
  taskKeys.sort((a, b) => new Date(b.uploaded) - new Date(a.uploaded));
  const tasks = [];
  for (const entry of taskKeys.slice(offset, offset + 100)) {
    const obj = await jsonObject(env.CONTROL, entry.key);
    if (
      obj?.value?.schema === 1 &&
      obj.value.request?.cluster_id === env.CLUSTER_ID
    )
      tasks.push(obj.value);
  }
  tasks.sort((a, b) => new Date(b.updated_at) - new Date(a.updated_at));
  return {
    points,
    tasks,
    points_truncated: newest.length > offset + 100,
    tasks_truncated: taskKeys.length > offset + 100,
    total_points: newest.length,
    total_tasks: taskKeys.length,
  };
}
async function digest(text) {
  return [
    ...new Uint8Array(
      await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text)),
    ),
  ]
    .map((v) => v.toString(16).padStart(2, "0"))
    .join("");
}
async function fetchText(url, max = 128 * 1024) {
  const r = await fetch(url, {
    headers: {
      Accept: "application/vnd.github+json",
      "User-Agent": "matrix-admin",
    },
    signal: AbortSignal.timeout(10000),
  });
  if (!r.ok) fail("版本目录暂不可用（GitHub " + r.status + "）", 502);
  const reader = r.body.getReader();
  let bytes = 0;
  const parts = [];
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    bytes += value.length;
    if (bytes > max) {
      await reader.cancel();
      fail("版本目录响应过大", 502);
    }
    parts.push(value);
  }
  const buf = new Uint8Array(bytes);
  let at = 0;
  for (const v of parts) {
    buf.set(v, at);
    at += v.length;
  }
  return new TextDecoder().decode(buf);
}
export async function releases(env, helpers) {
  const key = helpers.prefix(env) + "observations/releases.json",
    cached = await helpers.jsonObject(env.CONTROL, key);
  if (cached && Date.now() - Date.parse(cached.value.checked_at) < 3600000)
    return cached.value;
  const source =
    "https://api.github.com/repos/xiao0yy/cf-worker-matrix-admin/releases?per_page=5";
  const entries = JSON.parse(await fetchText(source, 512 * 1024));
  if (!Array.isArray(entries)) fail("版本目录格式无效", 502);
  const available = [];
  for (const entry of entries) {
    const v = entry.tag_name?.replace(/^v/, "");
    if (entry.draft || entry.prerelease || !version(v)) continue;
    const names = new Set(entry.assets?.map((a) => a.name));
    if (
      !["manifest.json", "SHA256SUMS", "matrix-admin-linux-amd64.tar.gz"].every(
        (n) => names.has(n),
      )
    )
      continue;
    const base =
      "https://github.com/xiao0yy/cf-worker-matrix-admin/releases/download/v" +
      v +
      "/";
    const [text, sums] = await Promise.all([
      fetchText(base + "manifest.json"),
      fetchText(base + "SHA256SUMS"),
    ]);
    const expected = sums
        .split("\n")
        .find((l) => /^[a-f0-9]{64}  manifest\.json$/.test(l))
        ?.slice(0, 64),
      sha = await digest(text);
    let m;
    try {
      m = JSON.parse(text);
    } catch {
      continue;
    }
    if (
      sha !== expected ||
      m.schema !== 1 ||
      m.project !== "matrix-admin" ||
      m.version !== v ||
      m.platform !== "linux/amd64" ||
      m.runtime !== "native-go-v1" ||
      m.control_protocol !== 1 ||
      m.mailbox_protocol !== 1 ||
      !m.recovery_readers?.includes(2)
    )
      continue;
    available.push({
      version: v,
      manifest_sha256: sha,
      commit: m.commit,
      published_at: entry.published_at,
      url:
        "https://github.com/xiao0yy/cf-worker-matrix-admin/releases/tag/v" + v,
    });
  }
  const value = {
    checked_at: new Date().toISOString(),
    releases: available,
    automatic_install: false,
  };
  await env.CONTROL.put(key, JSON.stringify(value), {
    httpMetadata: { contentType: "application/json", cacheControl: "no-store" },
  });
  return value;
}
