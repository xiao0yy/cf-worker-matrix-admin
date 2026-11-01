import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const context = vm.createContext({ window: {} });
vm.runInContext(
  readFileSync(new URL("../web/handover.js", import.meta.url), "utf8"),
  context,
);
const { derive, demonstration } = context.window.Handover;
const now = Date.parse("2026-01-01T12:00:00Z");
function state(phase) {
  const stamp = new Date(now).toISOString();
  const instance = (id) => ({
    id,
    report_fresh: true,
    agent_fresh: true,
    report: {
      reported_at: stamp,
      serving: false,
      bound_epoch: id === "a" ? 1 : 2,
    },
    agent: { reported_at: stamp },
  });
  const op = {
    id: "move-test",
    mode: "planned",
    source: "a",
    target: "b",
    source_epoch: 1,
    next_epoch: 2,
    phase,
  };
  const claimed = ["RESTORING", "VALIDATING", "STARTING", "ONLINE"].includes(
    phase,
  );
  const value = {
    control: {
      phase,
      owner: phase === "RELEASED" ? "" : claimed ? "b" : "a",
      epoch: claimed ? 2 : 1,
      operation: phase === "ONLINE" ? null : op,
      last_completed: phase === "ONLINE" ? op : null,
    },
    instances: [instance("a"), instance("b")],
  };
  if (phase === "ONLINE") value.instances[1].report.serving = true;
  return value;
}

test("handover topology follows the real phases, with ownership distinct from data transfer", () => {
  const expected = {
    REQUESTED: [0, "source", "none"],
    QUIESCING: [1, "source", "none"],
    QUIESCED: [2, "source", "none"],
    SNAPSHOTTING: [2, "source", "upload"],
    RELEASED: [3, "none", "none"],
    RESTORING: [4, "target", "restore"],
    VALIDATING: [4, "target", "restore"],
    STARTING: [5, "target", "none"],
    ONLINE: [6, "target", "none"],
  };
  for (const [phase, [step, owner, flow]] of Object.entries(expected)) {
    const model = derive(state(phase), true, now);
    assert.equal(model.step, step, phase);
    assert.equal(model.owner, owner, phase);
    assert.equal(model.flow, flow, phase);
  }
  assert.match(
    derive(state("VALIDATING"), true, now).description,
    /下载快照、恢复数据库/,
  );
});

test("verified READY under QUIESCED is not rendered as a new or regressed backup", () => {
  const s = state("QUIESCED");
  s.control.operation.phase = "READY";
  const m = derive(s, true, now);
  assert.equal(m.phase, "READY");
  assert.equal(m.step, 3);
  assert.equal(m.flow, "none");
  assert.equal(m.owner, "source");
});

test("expired reports or failed reads freeze motion instead of simulating progress", () => {
  const s = state("VALIDATING");
  assert.equal(derive(s, false, now).mode, "stale");
  s.instances[1].agent.reported_at = "2020-01-01T00:00:00Z";
  const m = derive(s, true, now);
  assert.equal(m.mode, "stale");
  assert.equal(m.flow, "none");
  assert.equal(m.step, 4);
});

test("source may go offline after release without hiding a fresh target recovery", () => {
  const s = state("VALIDATING");
  s.instances[0].report_fresh = false;
  s.instances[0].agent_fresh = false;
  assert.equal(derive(s, true, now).mode, "running");
});

test("pausing stops motion without hiding conflicting writers, stale reports or failures", () => {
  const s = state("VALIDATING");
  s.instances[1].agent.handover_paused = { paused: true };
  assert.equal(derive(s, true, now).mode, "paused");
  assert.equal(derive(s, true, now).flow, "none");
  s.instances[0].report.serving = true;
  let m = derive(s, true, now);
  assert.equal(m.mode, "error");
  assert.equal(m.title, "交接需要检查");
  assert.equal(m.flow, "none");
  s.instances[0].report.serving = false;
  s.instances[1].agent.reported_at = "2020-01-01T00:00:00Z";
  m = derive(s, true, now);
  assert.equal(m.mode, "stale");
  assert.equal(m.flow, "none");
  s.instances[1].agent.reported_at = new Date(now).toISOString();
  s.instances[1].report.error = "restore failed";
  assert.equal(derive(s, true, now).mode, "error");
  assert.equal(derive(s, true, now).flow, "none");
});

test("errors only from this operation stop the active path; clearing them resumes it", () => {
  const s = state("VALIDATING");
  s.instances[1].agent.worker_errors = {
    handover: { operation_id: "another-task", error: "old error" },
  };
  assert.equal(derive(s, true, now).mode, "running");
  s.instances[1].agent.worker_errors.handover.operation_id = "move-test";
  const m = derive(s, true, now);
  assert.equal(m.mode, "error");
  assert.equal(m.flow, "none");
  assert.equal(m.step, 4);
  s.instances[1].agent.worker_errors = {};
  assert.equal(derive(s, true, now).flow, "restore");
});

test("ONLINE needs matching target, generation and fresh serving evidence before a completion seal", () => {
  const s = state("ONLINE");
  assert.equal(derive(s, true, now).mode, "complete");
  s.instances[1].report.serving = false;
  assert.equal(derive(s, true, now).mode, "awaiting");
  assert.equal(derive(s, true, now).target.status, "上线状态待确认");
  s.instances[1].report.serving = true;
  s.instances[1].report_fresh = false;
  assert.equal(derive(s, true, now).mode, "stale");
  assert.equal(derive(s, true, now).target.status, "上线状态待确认");
  s.instances[1].report_fresh = true;
  s.instances[1].report.bound_epoch = 1;
  assert.equal(derive(s, true, now).mode, "error");
  s.instances[1].report.bound_epoch = 2;
  s.instances[0].report.serving = true;
  assert.equal(derive(s, true, now).mode, "error");
});

test("unknown protocol phases are not painted as completed or animated", () => {
  const s = state("FUTURE_STATE");
  const m = derive(s, true, now);
  assert.equal(m.mode, "unknown");
  assert.equal(m.step, -1);
  assert.equal(m.flow, "none");
  s.control.operation.mode = "final-backup";
  assert.equal(derive(s, true, now), null);
});

test("demo models remain explicitly illustrative and show failures without live actions", () => {
  assert.equal(demonstration("SNAPSHOTTING").id, "demo-only");
  assert.equal(demonstration("ERROR").mode, "error");
  assert.equal(demonstration("STALE").mode, "stale");
  assert.equal(demonstration("ONLINE").mode, "complete");
});

test("a later cancellation preserves the initial failure and its stage", () => {
  const s = state("SNAPSHOTTING");
  s.instances[0].agent.worker_errors = {
    final: {
      operation_id: "move-test",
      step: "final-backup",
      kind: "interrupted",
      error: "context canceled",
      first_failure: {
        step: "stage-backup",
        error: "unsupported configuration path: database/archive/001.log",
      },
    },
  };
  const m = derive(s, true, now);
  assert.match(m.description, /unsupported configuration path/);
  assert.equal(m.diagnostic.step, "stage-backup");
  assert.equal(m.diagnostic.latest.kind, "interrupted");
});

test("interventions follow ownership boundaries, not just an error badge", () => {
  const { interventions } = context.window.Handover;
  for (const phase of ["SNAPSHOTTING", "RELEASED", "VALIDATING", "STARTING"]) {
    const s = state(phase);
    s.instances[0].report.final_backup = { id: "move-test" };
    s.instances[0].agent.handover_abort = true;
    const plan = interventions(s, derive(s, true, now));
    assert.equal(
      plan.actions.some((a) => a.action === "source-resume"),
      phase === "SNAPSHOTTING",
      phase,
    );
    assert.equal(
      plan.actions.some((a) => a.action === "cleanup-plan"),
      false,
      phase,
    );
  }
  const s = state("ONLINE");
  assert.equal(
    interventions(s, derive(s, true, now)).actions[0].action,
    "cleanup-plan",
  );
});

test("authoritative rollback keeps routing and legacy target cancellation visible", () => {
  const s = state("SNAPSHOTTING");
  const op = s.control.operation;
  s.control.phase = "ONLINE";
  s.control.operation = null;
  s.instances[0].report.serving = true;
  s.instances[1].report.handover = { ...op, phase: "REQUESTED" };
  s.control.last_aborted = {...op, phase: "ABORTED"};
  const m = derive(s, true, now);
  assert.equal(m.mode, "rolled-back");
  assert.equal(m.source.status, "正在服务");
  assert.equal(m.flow, "none");
  const plan = context.window.Handover.interventions(s, m);
  assert.match(plan.note, /Cloudflare/);
  assert.equal(plan.actions[0].action, "handover-cancel");
  s.instances[1].report.handover.phase = "CANCELLED";
  assert.equal(
    context.window.Handover.interventions(s, derive(s, true, now)).actions
      .length,
    0,
  );
});

test("a newer running attempt keeps old diagnostics without showing the old failure as current", () => {
  const s = state("SNAPSHOTTING"), agent = s.instances[0].agent;
  agent.worker_errors = {final: {operation_id: "move-test", step: "prepare-backup", at: new Date(now - 5000).toISOString(), kind: "failed", error: "no space left on device"}};
  agent.handover_progress = {operation_id: "move-test", step: "upload-backup", at: new Date(now - 1000).toISOString(), state: "RUNNING"};
  const m = derive(s, true, now);
  assert.equal(m.mode, "running");
  assert.equal(m.flow, "upload");
  assert.match(m.title, /重试/);
  assert.equal(m.diagnostic.error, "no space left on device");
});

test("a stale target request cannot fabricate a rollback without authority", () => {
  const s=state("SNAPSHOTTING"), op=s.control.operation;
  s.control.phase="ONLINE";s.control.operation=null;
  s.instances[1].report.handover={...op,phase:"REQUESTED"};
  assert.equal(derive(s,true,now),null);
});

test("rollback tasks expose ongoing work, actual errors and the exact retry", () => {
  const s=state("SNAPSHOTTING"), op=s.control.operation;
  const task={request:{id:"job-rollback",action:"rollback-handover",parameters:{operation_id:op.id}},state:"RUNNING",stage:"stop-handover-worker"};
  s.instances[0].agent.handover_recovery=task;
  let m=derive(s,true,now);
  assert.equal(m.mode,"recovering");assert.match(m.description,/停止交接任务/);
  let plan=context.window.Handover.interventions(s,m);
  assert.equal(plan.actions[0].action,"task-cancel");
  assert.equal(plan.actions[0].parameters.task_id,"job-rollback");
  s.control.last_aborted={...op,phase:"ABORTED"};s.control.phase="ONLINE";s.control.operation=null;
  task.state="FAILED";task.error="systemctl: permission denied (private log: command-fixture.log)";
  m=derive(s,true,now);plan=context.window.Handover.interventions(s,m);
  assert.equal(m.mode,"error");assert.match(m.description,/permission denied/);
  assert.equal(plan.actions[0].action,"task-retry");
  task.state="SUCCEEDED";task.error="";
  s.instances[1].agent.durable_handover_abort=true;
  m=derive(s,true,now);plan=context.window.Handover.interventions(s,m);
  assert.equal(m.mode,"rolled-back");assert.equal(plan.actions.length,0);
});

test("new participants use the source rollback task even before final backup begins", () => {
 const s=state("REQUESTED");s.instances[0].agent.durable_handover_abort=true;s.instances[0].agent.handover_abort=true;
 const m=derive(s,true,now), plan=context.window.Handover.interventions(s,m);
 assert(plan.actions.some(a=>a.action==="source-resume"&&a.target==="a"));
 assert(!plan.actions.some(a=>a.action==="handover-cancel"));
});
