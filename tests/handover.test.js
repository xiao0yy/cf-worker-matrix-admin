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
