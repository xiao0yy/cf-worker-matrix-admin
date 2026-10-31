// Copies upstream manifests and layers unchanged. No Docker build, local image,
// private instance files, application startup, or VPS deployment is involved.
import {spawnSync} from 'node:child_process';
import {createHash} from 'node:crypto';
import {appendFileSync, mkdirSync, mkdtempSync, rmSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join, resolve} from 'node:path';
import {fileURLToPath} from 'node:url';

const upstream = 'dock.mau.dev/mautrix/telegram';
const digestPattern = /^sha256:[a-f0-9]{64}$/;
const digestOf = bytes => 'sha256:' + createHash('sha256').update(bytes).digest('hex');
const asBuffer = value => Buffer.isBuffer(value) ? value : Buffer.from(value || '');

export function mirrorOptions({version, expectedDigest = '', repository, actor, token}) {
  if (!/^v[0-9]+(?:\.[0-9]+){1,2}(?:-[a-zA-Z0-9]+(?:[.-][a-zA-Z0-9]+)*)?$/.test(version || '')) {
    throw new Error('Use an explicit official version tag, e.g. v0.2610.0; latest is not accepted');
  }
  if (expectedDigest && !digestPattern.test(expectedDigest)) throw new Error('Invalid expected SHA-256 digest');
  if (!/^[a-zA-Z0-9_-]+\/[a-zA-Z0-9_.-]+$/.test(repository || '')) throw new Error('Invalid GitHub repository');
  if (!/^[a-zA-Z0-9_\[\]-]+$/.test(actor || '') || !token) throw new Error('GitHub Actions actor and GITHUB_TOKEN are required');
  return {version, expectedDigest, repository, actor, token,
    destination:`ghcr.io/${repository.split('/')[0].toLowerCase()}/matrix-telegram`};
}

function execute(args, input) {
  const env = {...process.env};
  delete env.GITHUB_TOKEN;
  const result = spawnSync('skopeo', args, {input, env, maxBuffer:16 * 1024 * 1024, timeout:10 * 60 * 1000});
  if (result.error) throw result.error;
  return {status:result.status, stdout:result.stdout, stderr:result.stderr};
}

function checked(result, stage) {
  if (result.status !== 0) throw new Error(`${stage}: ${asBuffer(result.stderr).toString().trim() || 'registry command failed'}`);
  return asBuffer(result.stdout);
}

// Resolve the version once, then use only its digest for all subsequent reads
// and the copy. A moved upstream version creates a different destination tag;
// it cannot overwrite an older version's content-addressed tag.
export async function mirrorTelegramImage(options, {run = execute, record = () => {}} = {}) {
  const {version, expectedDigest, repository, actor, token, destination} = mirrorOptions(options);
  const work = mkdtempSync(join(tmpdir(), 'matrix-telegram-mirror-'));
  const authFile = join(work, 'auth.json');
  const anonymousFile = join(work, 'anonymous.json');
  writeFileSync(anonymousFile, '{"auths":{}}\n', {mode:0o600});
  let report;
  try {
    const raw = checked(await run(['inspect', '--raw', '--no-creds', `docker://${upstream}:${version}`]), 'Read official image');
    const manifest = JSON.parse(raw);
    if (manifest.schemaVersion !== 2 || (!manifest.manifests && !manifest.config)) throw new Error('Unsupported upstream manifest');
    const digest = digestOf(raw);
    if (expectedDigest && digest !== expectedDigest) throw new Error('Official image differs from the expected digest; nothing was published');
    const source = `${upstream}@${digest}`;
    // Validate the architecture used by the current recovery tools before writing.
    const config = JSON.parse(checked(await run(['--override-os', 'linux', '--override-arch', 'amd64',
      'inspect', '--config', '--no-creds', `docker://${source}`]), 'Inspect official linux/amd64 image'));
    if (config.os !== 'linux' || config.architecture !== 'amd64') throw new Error('Official image does not support linux/amd64');
    const tag = `${version}-sha256-${digest.slice(7)}`;
    if (tag.length > 128) throw new Error('Destination tag is too long');
    report = {schema:1, upstream_tag:`${upstream}:${version}`, source, digest,
      destination_tag:`${destination}:${tag}`, pull_reference:`${destination}@${digest}`,
      workflow_repository:repository, copied:false, anonymous_pull_verified:false};
    record(report);
    checked(await run(['login', '--authfile', authFile, '--username', actor, '--password-stdin', 'ghcr.io'], token + '\n'), 'Log in to GHCR');
    checked(await run(['copy', '--all', '--preserve-digests', '--retry-times', '3',
      '--src-no-creds', '--dest-authfile', authFile,
      `docker://${source}`, `docker://${destination}:${tag}`]), 'Copy official image to GHCR');
    const mirrored = checked(await run(['inspect', '--raw', '--authfile', authFile,
      `docker://${destination}:${tag}`]), 'Verify GHCR manifest');
    if (digestOf(mirrored) !== digest) throw new Error('GHCR manifest digest mismatch');
    report = {...report, copied:true};
    record(report);
    // A newly created GHCR package is normally private. Do not confuse an
    // authenticated verification with the anonymous access recovery requires.
    const publicResult = await run(['inspect', '--raw', '--authfile', anonymousFile, '--no-creds',
      `docker://${destination}@${digest}`]);
    if (publicResult.status === 0) {
      if (digestOf(asBuffer(publicResult.stdout)) !== digest) throw new Error('Anonymous GHCR digest mismatch');
      const publicConfig = JSON.parse(checked(await run(['--override-os', 'linux', '--override-arch', 'amd64',
        'inspect', '--config', '--authfile', anonymousFile, '--no-creds',
        `docker://${destination}@${digest}`]), 'Verify anonymous linux/amd64 config access'));
      if (publicConfig.os !== 'linux' || publicConfig.architecture !== 'amd64') throw new Error('Mirrored platform mismatch');
      report = {...report, anonymous_pull_verified:true};
    } else if (/unauthorized|denied|authentication required|status(?: code)?:?\s*(?:401|403)\b/i.test(asBuffer(publicResult.stderr).toString())) {
      report = {...report, next_action:'Anonymous access was denied. Check that the matrix-telegram package is Public, then rerun this workflow with the recorded expected_digest.'};
    } else {
      checked(publicResult, 'Verify anonymous GHCR access');
    }
    record(report);
    return report;
  } catch (error) {
    // Never log the token, including on a failed credential helper invocation.
    throw new Error(String(error.message).replaceAll(token, '[REDACTED]'));
  } finally {
    rmSync(work, {recursive:true, force:true});
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const options = mirrorOptions({version:process.env.TELEGRAM_VERSION, expectedDigest:process.env.EXPECTED_DIGEST || '',
      repository:process.env.GITHUB_REPOSITORY, actor:process.env.GITHUB_ACTOR, token:process.env.GITHUB_TOKEN});
    const output = process.env.MIRROR_OUTPUT_DIR;
    if (!output || !output.startsWith('/')) throw new Error('Set MIRROR_OUTPUT_DIR to an absolute output directory');
    mkdirSync(output, {recursive:true, mode:0o700});
    const report = await mirrorTelegramImage(options, {record:value => writeFileSync(join(output, 'mirror.json'), JSON.stringify(value, null, 2)+'\n')});
    const summary = [
      '## Telegram official image mirror',
      `- Source: \`${report.source}\``,
      `- GHCR: \`${report.pull_reference}\``,
      '- Official manifests and layers copied without rebuilding; all source platforms retained.',
      `- Anonymous manifest and linux/amd64 config access verified: **${report.anonymous_pull_verified ? 'yes' : 'no — package visibility needs attention'}**`,
      report.next_action || '',
      '- This does not change the running VPS. The official image still needs validation against the existing deployment before adoption.',
    ].join('\n')+'\n';
    if (process.env.GITHUB_STEP_SUMMARY) appendFileSync(process.env.GITHUB_STEP_SUMMARY, summary);
    console.log(summary);
    if (!report.anonymous_pull_verified) console.log('::warning::GHCR copy succeeded, but anonymous recovery access is not yet verified. See the run summary.');
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
