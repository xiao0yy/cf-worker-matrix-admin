import {test} from 'node:test';
import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {mkdtempSync, rmSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {generateReleaseNotes, mergeReleaseNotes} from '../scripts/release-notes.mjs';

function repository(t) {
  const cwd = mkdtempSync(join(tmpdir(), 'matrix-notes-test-'));
  t.after(() => rmSync(cwd, {recursive:true, force:true}));
  const git = (...args) => execFileSync('git', args, {cwd, encoding:'utf8', stdio:['ignore', 'pipe', 'pipe']}).trim();
  git('init', '-q', '--initial-branch=main');
  git('config', 'user.name', 'Release fixture');
  git('config', 'user.email', 'release@example.invalid');
  const commit = subject => {
    git('-c', 'commit.gpgsign=false', 'commit', '--allow-empty', '-qm', subject);
    return git('rev-parse', 'HEAD');
  };
  const notes = tag => generateReleaseNotes({tag, version:tag.slice(1), commit:git('rev-parse', 'HEAD'), repository:'fixture/matrix-admin'}, cwd);
  return {cwd, git, commit, notes};
}

test('first release lists direct commits with escaped text and pinned documentation', t => {
  const r = repository(t);
  const sha = r.commit('Add [feature](https://example.invalid) <b> @everyone');
  const notes = r.notes('v0.1.0');
  assert.match(notes, /首次版本/);
  assert.ok(notes.includes(`https://github.com/fixture/matrix-admin/commit/${sha}`));
  assert.ok(notes.includes(`https://github.com/fixture/matrix-admin/blob/${sha}/node/backup/AGENT.md`));
  assert.ok(notes.includes('Add \\[feature\\]\\(https://example\\.invalid\\) &lt;b&gt; &#64;everyone'));
  assert.ok(!notes.includes('@everyone'));
  assert.equal(r.notes('v0.1.0'), notes);
});

test('release range uses the preceding ancestor version and ignores unrelated and future tags', t => {
  const r = repository(t);
  r.commit('Old change'); r.git('tag', 'v0.1.0');
  const previous = r.commit('Previous release'); r.git('-c', 'tag.gpgsign=false', 'tag', '-a', 'v0.2.0', '-m', 'Previous');
  r.git('checkout', '-qb', 'other'); r.commit('Unrelated branch'); r.git('tag', 'v0.2.9');
  r.git('checkout', '-q', 'main');
  const current = r.commit('Direct push change'); r.git('tag', 'v0.3.0'); r.git('tag', 'v9.0.0');
  const notes = r.notes('v0.3.0');
  assert.ok(notes.includes(`[v0.2.0 → v0.3.0](https://github.com/fixture/matrix-admin/compare/${previous}...${current})`));
  assert.ok(notes.includes('Direct push change'));
  assert.ok(!notes.includes('Old change'));
  assert.ok(!notes.includes('Unrelated branch'));
  assert.ok(!notes.includes('Previous release'));
});

test('numeric prerelease identifiers sort numerically before a stable release', t => {
  const r = repository(t);
  r.commit('Stable base'); r.git('tag', 'v0.9.0');
  r.commit('Beta nine'); r.git('tag', 'v1.0.0-beta.9');
  const previous = r.commit('Beta ten'); r.git('tag', 'v1.0.0-beta.10');
  r.commit('Stable change');
  const notes = r.notes('v1.0.0');
  assert.ok(notes.includes('v1.0.0-beta.10 → v1.0.0'));
  assert.ok(notes.includes(`/compare/${previous}...`));
  assert.ok(!notes.includes('Beta nine'));
});

test('large histories are bounded with a complete history link', t => {
  const r = repository(t);
  for (let i = 0; i < 52; i++) r.commit(`Change ${i}`);
  const notes = r.notes('v0.1.0');
  assert.equal(notes.split('\n').filter(line => line.startsWith('- Change')).length, 50);
  assert.ok(notes.includes('最近 50 条提交'));
  assert.ok(notes.includes('Change 51'));
  assert.ok(notes.includes('/commits/'));
});

test('shallow history is rejected instead of silently omitting changes', t => {
  const r = repository(t), sha = r.commit('Root');
  writeFileSync(join(r.cwd, '.git/shallow'), `${sha}\n`);
  assert.throws(() => r.notes('v0.1.0'), /full Git history/);
});

test('regeneration preserves manual prose before and after the generated section', () => {
  const first = mergeReleaseNotes('Manual introduction', 'First generated notes');
  const withFooter = `${first}\n\nManual footer`;
  const updated = mergeReleaseNotes(withFooter, 'Updated generated notes');
  assert.ok(updated.startsWith('Manual introduction\n\n'));
  assert.ok(updated.endsWith('\n\nManual footer'));
  assert.ok(!updated.includes('First generated notes'));
  assert.equal(mergeReleaseNotes(updated, 'Updated generated notes'), updated);
  assert.equal(mergeReleaseNotes(null, 'Notes'), mergeReleaseNotes('', 'Notes'));
});

test('malformed generated sections are preserved rather than partially replaced', () => {
  for (const body of [
    '<!-- matrix-admin:generated-notes:start -->Manual text',
    '<!-- matrix-admin:generated-notes:end -->Manual text',
    `${mergeReleaseNotes('', 'Notes')}\n${mergeReleaseNotes('', 'Duplicate')}`,
  ]) assert.throws(() => mergeReleaseNotes(body, 'Replacement'), /incomplete or duplicated/);
});
