# Security and trust boundaries

The public repository and release publisher are trusted software suppliers.
Release bootstrap uses HTTPS and SHA-256 checksums. A checksum proves integrity
relative to the selected manifest; it is not an independent publisher signature.
Protect the release account and never replace assets of an existing version.

Deployment identifiers are private operational information even when they are
not authentication secrets. Keep real IP addresses, hostnames, origin domains,
Access team/AUD identifiers and bucket names out of source, examples and tests.
Use reserved example domains and documentation IP ranges in fixtures. Worker
deployment inherits the existing CONTROL binding and preserves dashboard-managed
Access variables. Local development uses ignored `.dev.vars` and a separate local
R2 store. Neither real values nor deployment logs belong in public issues or Git.
The source audit rejects tracked private configuration, including forced adds.

B2 stores encrypted database snapshots only. Private R2 stores the recovery
catalog, deployment configuration capsules, B2 access credentials and generated
restic passwords. R2 read credentials therefore grant the ability to retrieve
and decrypt the database backup; restic encryption is not a second factor against
that holder. Never make either bucket public. Access to R2 control state must
also be limited to trusted administrators and instances. The Worker binding has
bucket access, but its API does not expose recovery credentials or configuration
capsules. Media stays in its existing native S3 namespace.

R2 is the live ownership authority. A database backup does not grant permission
to run. A new VPS creates a new identity and starts fenced. The management Worker
authenticates with Cloudflare Access and writes bounded mailbox commands; the
controller performs conditional ownership transitions. Do not infer ownership
from stale UI reports, DNS, a backup, or Agent observation files.

Downloaded archives have explicit file/size/hash contracts. Links, traversal,
unknown paths, missing files and unknown protocols are rejected. Data payloads
have a separate allowlist and cannot overwrite public tools. Runtime private
files are excluded from the software update inventory. After STARTING, never
restore over a generation that may have accepted writes.

Report suspected vulnerabilities privately to the repository maintainers before
publishing reproduction details. Do not attach real s3.env files, Matrix tokens,
Telegram sessions, database samples or raw private diagnostic logs to issues.
