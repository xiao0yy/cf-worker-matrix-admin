# Matrix Admin

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

Self-hosters managing a small personal Matrix deployment across a few VPS instances. They open the console occasionally to inspect backups, move service, or diagnose an interrupted operation. Machine names and IP addresses must be legible without memorizing internal instance IDs.

## Product Purpose

Make service ownership, recoverable data and the next safe action understandable. A planned handover stops the source, verifies its final backup, releases ownership, and restores the target before bringing service online. Availability may be sacrificed to preserve data and avoid concurrent writers.

## Operating Context

Cloudflare Access protects the web console. The Worker reads reports and submits commands through R2. VPS agents poll independently; the browser does not need to stay open. B2 stores database snapshots, while configuration and coordination are retrieved through R2. Media already lives in object storage and is not duplicated in database backups.

## Capabilities and Constraints

- Native HTML, CSS and JavaScript frontend, deployed with the existing Cloudflare Worker through Git.
- Service status, automatic public IPv4 discovery with manual override, backup history and restore drills, explicit runtime upgrades, durable maintenance tasks, planned handover and externally fenced disaster takeover.
- Destructive storage or retired-instance cleanup always starts with a reviewed, expiring plan; media remains untouched.
- DNS editing is available only when configured; manual routing remains supported.
- Stale reports do not prove a machine is stopped. Command acceptance is not service completion. A successful handover requires matching ownership, generation and a fresh serving report.
- No automatic failover, automatic updates or unreviewed cleanup. No fabricated backup history, throughput, uptime or message-delivery claims. Container health and a time-bounded operator delivery confirmation are separate facts.
- Public code and examples must contain no deployment addresses, account identifiers, bucket names or secrets.
- UI work must not mutate production services or send test messages.

## Brand Commitments

Matrix Admin; Chinese operator-facing language. The user explicitly requested a full contemporary UI/UX redesign using Impeccable, replacing the previous generic page layout, and meaningful state-machine animation.

## Evidence on Hand

The implemented API and tests, `docs/worker.md`, `docs/architecture.md`, and the native controller under `internal/`. Browser previews use clearly illustrative data with reserved IP addresses.

## Product Principles

- Show what is known, what is uncertain, and what action is available.
- Keep the common inspection path short; reveal technical records on demand.
- Use animation to explain actual transitions, never to invent progress.
- Preserve command identity, confirmation and stale-state guards through visual changes.
