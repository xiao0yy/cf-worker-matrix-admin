import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
import { parseHTML } from "linkedom";

const html = readFileSync(
  new URL("../web/index.html", import.meta.url),
  "utf8",
);
const script = readFileSync(new URL("../web/app.js", import.meta.url), "utf8");
const evaluate = (context, script) => vm.runInContext(script, context);
const clone = (data) => JSON.parse(JSON.stringify(data));

function browser(fetch = async () => Response.json({}), saved = null) {
  const { document, HTMLElement } = parseHTML(html);
  let activeElement = document.body;
  // Linkedom implements parsing and DOM trees, but not browser focus/dialogs or
  // the select value setter. Only these platform primitives need a small shim.
  Object.defineProperty(document, "activeElement", {
    get: () => activeElement,
  });
  HTMLElement.prototype.focus = function () {
    activeElement = this;
  };
  HTMLElement.prototype.scrollIntoView = () => {};
  for (const node of document.querySelectorAll("dialog")) {
    node.showModal = () => {
      node.open = true;
    };
    node.close = () => {
      node.open = false;
    };
  }
  for (const node of document.querySelectorAll("select")) {
    Object.defineProperty(node, "value", {
      get() {
        return (
          this.querySelector("option[selected]")?.value ||
          this.querySelector("option")?.value ||
          ""
        );
      },
      set(value) {
        for (const option of this.querySelectorAll("option")) {
          if (option.value === value) option.setAttribute("selected", "");
          else option.removeAttribute("selected");
        }
      },
    });
  }
  const location = { protocol: "file:", origin: "null", hash: "" };
  const storage = new Map(
    saved ? [["matrix-admin-v1:null", JSON.stringify(saved)]] : [],
  );
  const context = vm.createContext({
    document,
    location,
    crypto,
    AbortSignal,
    fetch,
    console,
    history: {
      replaceState(_state, _title, url) {
        location.hash = url;
      },
    },
    window: { addEventListener() {} },
    confirm: () => true,
    localStorage: {
      getItem: (key) => storage.get(key),
      setItem: (key, value) => storage.set(key, value),
    },
    setInterval() {
      throw Error("file pages must not poll authenticated API");
    },
  });
  vm.runInContext(
    readFileSync(new URL("../web/handover.js", import.meta.url), "utf8"),
    context,
  );
  vm.runInContext(script, context);
  return context;
}
function fixture() {
  const now = new Date().toISOString();
  const make = (id, name, serving) => ({
    id,
    info: { name, public_ip: id === "node-a" ? "192.0.2.10" : "192.0.2.20" },
    info_etag: id + "-etag",
    report_fresh: true,
    agent_fresh: true,
    report: { reported_at: now, serving, bound_epoch: serving ? 1 : 0 },
    agent: {
      reported_at: now,
      software: { version: "0.2.1" },
      preflight: {ready:true,epoch:1,checked_at:now},
      host: { hostname: name },
    },
    pending: null,
    result: null,
  });
  const a = make("node-a", "主实例", true),
    b = make("node-b", "备用实例", false);
  a.agent.backup_observation = {
    data_time: now,
    verified_at: now,
    snapshot_id: "a".repeat(64),
    verification: "database-tree",
    database_startup_verified: false,
    telegram_included: true,
    media_included: false,
    consistency: "independent-online-snapshots",
  };
  return {
    at: now,
    control: {
      schema: 1,
      cluster_id: "test-cluster",
      revision: 4,
      epoch: 1,
      owner: a.id,
      phase: "ONLINE",
    },
    instances: [a, b],
    capabilities: { planned_handover: true, dns: false },
  };
}
function setSnapshot(c, state = fixture()) {
  c.testState = clone(state);
  evaluate(
    c,
    'snapshot=testState;statusFetchedAt=Date.now();statusError="";render()',
  );
}
function review(c) {
  evaluate(
    c,
    'switchTarget="node-b";startReview();$("route").checked=true;$("switch-confirm").value="确认";updateSwitchSubmit()',
  );
}

test("browser API uses the same-origin Access session without a bearer token", async () => {
  let captured;
  const c = browser(async (path, options) => {
    captured = { path, options };
    return Response.json({ ok: true });
  });
  await evaluate(c, "api('/api/status')");
  assert.equal(captured.options.credentials, "same-origin");
  assert.equal(captured.options.redirect, "manual");
  assert.equal(captured.options.cache, "no-store");
  assert.equal(captured.options.headers.Authorization, undefined);
});

test("expired Access login stops polling and never automatically replays a write", async () => {
  for (const response of [
    { type: "opaqueredirect", status: 0 },
    new Response("", { status: 401 }),
    new Response("", { status: 403 }),
  ]) {
    let calls = 0;
    const c = browser(async () => {
      calls++;
      return response;
    });
    await assert.rejects(
      evaluate(c, "api('/api/command','POST',{id:'original-command'})"),
      /重新登录/,
    );
    await evaluate(c, "refresh()");
    assert.equal(calls, 1);
    assert.equal(c.document.getElementById("login-link").hidden, false);
  }
});

test("overview separates backup file verification from a restore and startup test", () => {
  const c = browser();
  setSnapshot(c);
  assert.equal(
    c.document.getElementById("health-title").textContent,
    "当前服务正常",
  );
  assert.equal(
    c.document.getElementById("backup-verification").textContent,
    "数据库文件校验已通过",
  );
  evaluate(c, "openBackup()");
  assert.match(
    c.document.getElementById("backup-detail").textContent,
    /未包含数据库启动校验/,
  );
  assert.match(
    c.document.getElementById("backup-detail").textContent,
    /未打包，继续使用原 S3/,
  );
  assert.equal(c.document.getElementById("page-tools").hidden, true);
});

test("stale report, wrong ownership and failed refresh cannot display a healthy service or enable a switch", async () => {
  const c = browser(async () => {
    throw new Error("network unavailable");
  });
  const state = fixture();
  state.instances[0].report.reported_at = "2020-01-01T00:00:00Z";
  setSnapshot(c, state);
  assert.notEqual(
    c.document.getElementById("health-title").textContent,
    "当前服务正常",
  );
  assert.match(evaluate(c, 'switchBlocker(findInstance("node-b"))'), /源实例/);
  setSnapshot(c);
  evaluate(c, "snapshot.instances[1].report.serving=true;render()");
  assert.match(
    c.document.getElementById("health-title").textContent,
    /归属不一致/,
  );
  setSnapshot(c);
  review(c);
  await evaluate(c, "refresh()");
  assert.match(
    c.document.getElementById("health-title").textContent,
    /无法确认/,
  );
  assert.equal(c.document.getElementById("switch-submit").disabled, true);
  assert.equal(evaluate(c, "snapshot.instances.length"), 2); // Preserve last observation, clearly marked stale.
});

test("switch wizard requires a target, route acknowledgement, typed confirmation and unchanged state", async () => {
  let writes = 0;
  const c = browser(async () => {
    writes++;
    return Response.json({});
  });
  setSnapshot(c);
  assert.equal(c.document.getElementById("switch-next").disabled, true);
  evaluate(c, 'switchTarget="node-b";startReview()');
  assert.equal(c.document.getElementById("switch-submit").disabled, true);
  evaluate(c, '$("switch-confirm").value="确认";updateSwitchSubmit()');
  assert.equal(c.document.getElementById("switch-submit").disabled, true);
  evaluate(c, '$("route").checked=true;updateSwitchSubmit()');
  assert.equal(c.document.getElementById("switch-submit").disabled, false);
  evaluate(c, "snapshot.control.revision++;renderSwitch()");
  assert.equal(c.document.getElementById("switch-submit").disabled, true);
  await evaluate(c, "submitSwitch()");
  assert.equal(writes, 0);
});

test("switch command carries exact state and uncertain delivery retries the identical command", async () => {
  const state = fixture(),
    writes = [];
  const c = browser(async (path, options) => {
    if (path === "/api/status") return Response.json(state);
    writes.push(JSON.parse(options.body));
    throw new Error("response lost");
  });
  setSnapshot(c, state);
  review(c);
  await evaluate(c, "submitSwitch()");
  assert.equal(writes.length, 1);
  assert.equal(writes[0].target, "node-b");
  assert.equal(writes[0].source_epoch, 1);
  assert.equal(writes[0].bound_epoch, 0);
  assert.deepEqual(writes[0].expected, {
    revision: 4,
    epoch: 1,
    owner: "node-a",
    phase: "ONLINE",
  });
  assert.match(writes[0].operation_id, /^move-/);
  assert.equal(evaluate(c, "reviewContext"), null);
  assert.equal(c.document.getElementById("receipt").hidden, false);
  const retry = evaluate(c, "retryCommand()");
  assert.equal(writes.length, 1);
  evaluate(c, '$("confirm-accept").onclick()');
  await retry;
  assert.equal(writes.length, 2);
  assert.deepEqual(writes[0], writes[1]);
});

test("APPLIED does not mean a completed handover; completion requires matching operation, owner, epoch and fresh serving report", () => {
  const c = browser();
  setSnapshot(c);
  evaluate(
    c,
    'lastCommand=makeCommand(findInstance("node-b"),"handover-request",expectedState(),"move-test");snapshot.instances[1].pending=lastCommand;snapshot.instances[1].result={state:"APPLIED"};renderReceipt()',
  );
  assert.equal(
    c.document.getElementById("receipt-badge").textContent,
    "指令已处理",
  );
  evaluate(
    c,
    'snapshot.control={...snapshot.control,owner:"node-b",epoch:2,last_completed:{id:"move-test"}};snapshot.instances[1].report={...snapshot.instances[1].report,serving:true,bound_epoch:2};renderReceipt()',
  );
  assert.equal(
    c.document.getElementById("receipt-badge").textContent,
    "切换完成",
  );
  evaluate(c, "snapshot.instances[1].report_fresh=false;renderReceipt()");
  assert.notEqual(
    c.document.getElementById("receipt-badge").textContent,
    "切换完成",
  );
});

test("background refresh preserves edited machine identity and its original concurrency token", () => {
  const c = browser();
  setSnapshot(c);
  evaluate(
    c,
    'openInstance("node-a",true);$("instance-name").value="my edit";$("instance-name").oninput();snapshot.instances[0].info.name="remote edit";snapshot.instances[0].info_etag="new-etag";render()',
  );
  assert.equal(c.document.getElementById("instance-name").value, "my edit");
  assert.equal(evaluate(c, "infoETag"), "node-a-etag");
  evaluate(c, 'openInstance("node-b",true)');
  assert.equal(c.document.getElementById("instance-name").value, "备用实例");
  assert.equal(evaluate(c, "infoETag"), "node-b-etag");
});

test("instance names are rendered as text, never executable HTML", () => {
  const c = browser(),
    state = fixture();
  state.instances[0].info.name = "<img src=x onerror=alert(1)>";
  setSnapshot(c, state);
  assert.equal(c.document.querySelectorAll("#instances img").length, 0);
  assert.match(
    c.document.getElementById("instances").textContent,
    /<img src=x/,
  );
});

test("disabled DNS capability and claimed command cannot offer unsupported intervention", () => {
  const c = browser(),
    state = fixture();
  state.instances[0].pending = {
    id: "cmd-old",
    expires_at: "2020-01-01T00:00:00Z",
  };
  state.instances[0].result = { state: "CLAIMED" };
  setSnapshot(c, state);
  assert.equal(c.document.getElementById("dns-open").disabled, true);
  assert.equal(c.document.getElementById("abandon").disabled, true);
  assert.match(
    c.document.getElementById("abandon-description").textContent,
    /已被认领/,
  );
});

test("operation confirmation is bound to the reviewed machine and authority", async () => {
  let writes = 0;
  const c = browser(async () => {
    writes++;
    return Response.json({});
  });
  setSnapshot(c);
  evaluate(
    c,
    'openAction("fence-local");$("action-confirm").value="确认";snapshot.control.revision++',
  );
  await evaluate(c, "submitAction()");
  assert.equal(writes, 0);
  assert.match(
    c.document.getElementById("action-error").textContent,
    /状态已变化/,
  );
});

test("loading a saved command offers inspection, but never submits it automatically", () => {
  let calls = 0;
  const c = browser(
    async () => {
      calls++;
      return Response.json({});
    },
    {
      schema: 1,
      id: "cmd-saved",
      action: "probe",
      target: "node-a",
      expires_at: "2020-01-01T00:00:00Z",
    },
  );
  setSnapshot(c);
  assert.equal(calls, 0);
  assert.equal(c.document.getElementById("receipt").hidden, false);
  assert.equal(c.document.getElementById("retry").disabled, true);
});

test("a target IP change invalidates the route acknowledgement", () => {
  const c = browser();
  setSnapshot(c);
  review(c);
  evaluate(
    c,
    'snapshot.instances[1].info.public_ip="192.0.2.21";renderSwitch()',
  );
  assert.equal(c.document.getElementById("switch-submit").disabled, true);
  assert.match(
    c.document.getElementById("review-error").textContent,
    /重新核对/,
  );
});

test("single-instance cluster explains how to prepare a standby without offering a self-switch", () => {
  const c = browser(),
    state = fixture();
  state.instances.pop();
  setSnapshot(c, state);
  evaluate(c, 'navigate("switch")');
  assert.match(
    c.document.getElementById("switch-targets").textContent,
    /还没有备用实例/,
  );
  assert.equal(c.document.getElementById("switch-next").disabled, true);
});

test("losing Access during a write immediately marks the previous observation stale", async () => {
  const c = browser(async () => new Response("", { status: 401 }));
  setSnapshot(c);
  await assert.rejects(
    evaluate(c, 'api("/api/command","POST",{id:"original"})'),
    /重新登录/,
  );
  assert.match(
    c.document.getElementById("health-title").textContent,
    /无法确认/,
  );
  assert.equal(c.document.getElementById("dns-open").disabled, true);
});

test("demo playback is isolated from the live wizard and does not call any API", () => {
  let calls = 0;
  const c = browser(async () => {
    calls++;
    return Response.json({});
  });
  setSnapshot(c);
  evaluate(
    c,
    'navigate("switch");$("handover-demo-open").onclick();$("handover-demo-phase").value="SNAPSHOTTING";$("handover-demo-phase").onchange()',
  );
  assert.equal(c.document.getElementById("switch-wizard").hidden, true);
  assert.equal(
    c.document.querySelector("#handover-demo-view .handover-scene").dataset
      .flow,
    "upload",
  );
  assert.equal(calls, 0);
  evaluate(c, '$("handover-demo-close").onclick()');
  assert.equal(c.document.getElementById("switch-wizard").hidden, false);
});

test("identical status polls preserve topology nodes and animations", () => {
  const c = browser(),
    s = fixture();
  s.control.phase = "SNAPSHOTTING";
  s.control.operation = {
    id: "move-test",
    mode: "planned",
    source: "node-a",
    target: "node-b",
    source_epoch: 1,
    next_epoch: 2,
    phase: "SNAPSHOTTING",
  };
  s.instances[0].report.serving = false;
  setSnapshot(c, s);
  const before = c.document.querySelector(
    "#handover-live-view .ownership-marker",
  );
  evaluate(c, "render()");
  assert.equal(
    c.document.querySelector("#handover-live-view .ownership-marker"),
    before,
  );
  assert.equal(
    c.document.querySelectorAll("#handover-live-view .handover-stages li")
      .length,
    7,
  );
});

test("confirmation can be cancelled without submitting and retains protected focus", async () => {
  let writes = 0;
  const c = browser(async () => {
    writes++;
    return Response.json({});
  });
  setSnapshot(c);
  evaluate(c, 'lastCommand=makeCommand(findInstance("node-a"),"probe")');
  const pending = evaluate(c, "retryCommand()");
  assert.equal(c.document.getElementById("confirm-dialog").open, true);
  assert.equal(c.document.activeElement.id, "confirm-cancel");
  evaluate(c, '$("confirm-cancel").onclick()');
  await pending;
  assert.equal(writes, 0);
  assert.equal(c.document.getElementById("confirm-dialog").open, false);
});

test("connection check sends only a probe while service interventions still require confirmation", async () => {
  const writes = [],
    state = fixture();
  const c = browser(async (path, options) => {
    if (path === "/api/status") return Response.json(state);
    writes.push(JSON.parse(options.body));
    return Response.json({ submitted: true });
  });
  setSnapshot(c);
  await evaluate(c, 'document.querySelector("[data-action=probe]").onclick()');
  assert.equal(writes.length, 1);
  assert.equal(writes[0].action, "probe");
  assert.equal(writes[0].target, "node-a");
  evaluate(c, 'document.querySelector("[data-action=fence-local]").onclick()');
  assert.equal(c.document.getElementById("action-dialog").open, true);
  assert.equal(c.document.getElementById("action-submit").disabled, true);
  assert.equal(writes.length, 1);
});

test("empty clusters explain setup instead of inventing an expired report or completed release", () => {
  const c = browser(),
    state = fixture();
  state.instances = [];
  state.control.owner = "";
  setSnapshot(c, state);
  assert.equal(
    c.document.getElementById("health-title").textContent,
    "等待实例接入",
  );
  assert.equal(
    c.document.getElementById("owner-name").textContent,
    "暂无服务归属",
  );
  assert.equal(c.document.getElementById("switch-next").disabled, true);
});

test("skip navigation keeps the selected page and focuses main content", () => {
  const c = browser();
  evaluate(
    c,
    'navigate("tools");$("skip-content").onclick({preventDefault(){}})',
  );
  assert.equal(c.document.getElementById("page-tools").hidden, false);
  assert.equal(c.document.activeElement.id, "main-content");
  assert.equal(c.location.hash, "#tools");
});

test('automatic public address is displayed while an explicit override wins',()=>{
 const ctx=browser(),data=fixture();delete data.instances[0].info.public_ip;data.instances[0].agent.public_address={ip:'198.51.100.10'};
 ctx.data=data;assert.equal(evaluate(ctx,'publicIP(data.instances[0])'),'198.51.100.10');data.instances[0].info.public_ip='203.0.113.10';assert.equal(evaluate(ctx,'publicIP(data.instances[0])'),'203.0.113.10');
});
test('maintenance shows unsupported nodes instead of pretending remote upgrade exists',()=>{
 const ctx=browser();ctx.data=fixture();evaluate(ctx,'snapshot=data;');
 vm.runInContext(readFileSync(new URL('../web/management.js',import.meta.url),'utf8'),ctx);
 assert.match(ctx.document.getElementById('management-capability').textContent,/先由 Agent 升级/);
 assert.equal(ctx.document.getElementById('runtime-upgrade').disabled,true);
});
test('merely viewing runtime versions never submits an upgrade command',async()=>{
 const calls=[];const data=fixture();for(const i of data.instances){i.agent.management_protocol=1;i.agent.actions=['upgrade-runtime'];i.report.blocked=!i.report.serving;}
 const ctx=browser(async(path)=>{calls.push(path);return Response.json(path==='/api/status'?data:path==='/api/releases'?{releases:[{version:'0.3.0',manifest_sha256:'a'.repeat(64)}]}:{points:[],tasks:[]});});
 vm.runInContext(readFileSync(new URL('../web/management.js',import.meta.url),'utf8'),ctx);
 await evaluate(ctx,'refresh()');evaluate(ctx,'window.Management.selectTab("updates")');await new Promise(r=>setImmediate(r));
 assert(calls.includes('/api/releases'));assert(!calls.some(x=>x==='/api/command'));
 assert.equal(ctx.document.getElementById('runtime-upgrade').disabled,true,'online owner must first be fenced');
});
