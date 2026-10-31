import {execFileSync} from 'node:child_process';

const begin = '<!-- matrix-admin:generated-notes:start -->';
const end = '<!-- matrix-admin:generated-notes:end -->';
const maxCommits = 50;

function version(tag) {
  const match = /^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$/.exec(tag);
  return match && {core:match.slice(1, 4).map(BigInt), pre:match[4]?.split('.')};
}

function compare(a, b) {
  for (let i = 0; i < 3; i++) {
    if (a.core[i] !== b.core[i]) return a.core[i] < b.core[i] ? -1 : 1;
  }
  if (!a.pre || !b.pre) return a.pre ? -1 : b.pre ? 1 : 0;
  for (let i = 0; i < Math.max(a.pre.length, b.pre.length); i++) {
    const left = a.pre[i], right = b.pre[i];
    if (left === right) continue;
    if (left === undefined || right === undefined) return left === undefined ? -1 : 1;
    const ln = /^\d+$/.test(left), rn = /^\d+$/.test(right);
    if (ln && rn) {
      if (BigInt(left) !== BigInt(right)) return BigInt(left) < BigInt(right) ? -1 : 1;
    } else if (ln !== rn) return ln ? -1 : 1;
    else return left < right ? -1 : 1;
  }
  return 0;
}

function text(value) {
  return value.replace(/[\x00-\x1f\x7f]/g, ' ').slice(0, 500)
    .replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;')
    .replace(/[\\`*_{}\[\]()#+.!|~-]/g, '\\$&').replaceAll('@', '&#64;');
}

// No PR dependency: this also describes direct commits mirrored from Gitea.
// Use only lower version tags on this commit's ancestry, never an unrelated
// branch or a newer tag added after this release. Links pin the resolved SHAs.
export function generateReleaseNotes(context, cwd) {
  const current = version(context.tag);
  if (!current || context.tag !== `v${context.version}` || !/^[a-f0-9]{40}$/.test(context.commit)
      || !/^[a-zA-Z0-9_-]+\/[a-zA-Z0-9_.-]+$/.test(context.repository)) throw new Error('Invalid release notes context');
  const git = (...args) => execFileSync('git', args, {cwd, encoding:'utf8', maxBuffer:16 * 1024 * 1024}).trimEnd();
  if (git('rev-parse', '--is-shallow-repository') !== 'false') throw new Error('Release notes require full Git history; use fetch-depth: 0');
  const previous = git('tag', '--merged', context.commit, '--list', 'v*').split('\n')
    .map(tag => ({tag, version:version(tag)}))
    .filter(item => item.version && compare(item.version, current) < 0)
    .sort((a, b) => compare(b.version, a.version) || a.tag.localeCompare(b.tag))[0]?.tag;
  const previousCommit = previous ? git('rev-parse', '--verify', `refs/tags/${previous}^{commit}`) : null;
  if (previousCommit && !/^[a-f0-9]{40}$/.test(previousCommit)) throw new Error('Invalid previous release commit');
  const range = previousCommit ? `${previousCommit}..${context.commit}` : context.commit;
  const entries = git('log', '--format=%H%x09%s', `--max-count=${maxCommits + 1}`, range, '--').split('\n').filter(Boolean);
  const url = `https://github.com/${context.repository}`;
  const historyURL = previousCommit ? `${url}/compare/${previousCommit}...${context.commit}` : `${url}/commits/${context.commit}`;
  const changes = entries.slice(0, maxCommits).map(entry => {
    const sha = entry.slice(0, 40);
    if (!/^[a-f0-9]{40}\t/.test(entry)) throw new Error('Invalid Git log entry');
    return `- ${text(entry.slice(41))} ([${sha.slice(0, 7)}](${url}/commit/${sha}))`;
  });
  if (!changes.length) changes.push('- 此范围没有新增提交。');
  if (entries.length > maxCommits) changes.push(`\n仅展示最近 ${maxCommits} 条提交，其余见完整变更链接。`);
  return [
    '## 更新内容',
    previous ? `变更范围：[${previous} → ${context.tag}](${historyURL})。` : `首次版本：[提交记录](${historyURL})。`,
    changes.join('\n'),
    '## 下载与使用',
    '提供 Linux amd64 运行包、独立下载校验器和源码；下载后使用 `SHA256SUMS` 校验附件。',
    `[部署与恢复说明](${url}/blob/${context.commit}/node/backup/AGENT.md) · [升级说明](${url}/blob/${context.commit}/docs/releases.md)`,
    `源码提交：[\`${context.commit}\`](${url}/commit/${context.commit})。`,
  ].join('\n\n');
}

// These delimiters only bound generated prose; they never establish ownership
// of a release or bypass the independent commit/asset checks.
export function mergeReleaseNotes(body, notes) {
  if (body == null) body = '';
  if (typeof body !== 'string' || typeof notes !== 'string' || !notes.trim()
      || notes.includes(begin) || notes.includes(end)) throw new Error('Invalid release notes');
  const start = body.indexOf(begin), stop = body.indexOf(end);
  const block = `${begin}\n${notes.trim()}\n${end}`;
  if (start === -1 && stop === -1) return body.trim() ? `${body}${body.endsWith('\n') ? '\n' : '\n\n'}${block}` : block;
  if (start < 0 || stop < start || body.indexOf(begin, start + begin.length) !== -1
      || body.indexOf(end, stop + end.length) !== -1) throw new Error('Generated release notes delimiters are incomplete or duplicated; existing description was preserved');
  return `${body.slice(0, start)}${block}${body.slice(stop + end.length)}`;
}
