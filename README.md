# casino-review 🎰

The ultimate reviewer orchestrator you didn't even know you need! 

Watches a GitHub repo's pull requests for a `/casino-review` comment. When it
sees one, it posts a slot-machine GIF that lands on a randomly chosen review —
then actually runs it. Review entries are typed engines:

- **dispatch** — post `/<name>` and let an external review bot take it (the original behavior)
- **analyzer** — run a static tool (eslint, tsc, …) over a checkout of the PR and post parsed findings
- **claude** — *sunset for now* (see below): a Claude Code persona over the PR checkout

Casino is in the name: the reel mixes engine types. Every spin is a *random
assignment* of an engine to a PR, and the system records assignments and
outcomes — the whole project doubles as a statistics & behavioral experiment
(see Telemetry). This is Phase 1 of the [PR Market plan](#roadmap): the same
plumbing grows into a market where people stake on questions about PRs.

<img width="400" height="300" alt="out" src="https://github.com/user-attachments/assets/0ab154db-f6d9-431f-b6c1-d57018bc5a69" />

(Insert your reviewer commands here)

## Flow

```
core:   poll PR comments ──► sees "/casino-review" (command, not substring)
           └► react 🚀 (visible ack + durable dedup) ──► enqueue spin job (Postgres)
runner: claim job ──► selector picks engine (assignment logged to events)
           └► render slot GIF landing on the winner ──► commit to assets branch ──► post GIF comment (stays)
           └► wait DISPLAY_DURATION (let the reel play)
           └► run the engine: dispatch comment | claude persona | static analyzer
           └► post findings comment ──► record review_run (findings feed the selector + future markets)
```

## Quick start (docker compose)

```bash
cp .env.example .env          # fill GITHUB_TOKEN + GITHUB_REPO
cp reviews.example.json reviews.json   # edit your engine pool
docker compose up -d --build
docker compose logs -f core runner
```

No public URL needed anywhere: GitHub is polled, and (from P2) Slack connects
via Socket Mode.

### Local dev (no docker)

```bash
go run ./cmd/casino gen out.gif                   # preview a spin, no GitHub needed
set -a; . ./.env; set +a
go run ./cmd/casino check                         # read-only GitHub smoke test
go run ./cmd/casino review run eslint --pr 42     # dry-run one engine (add --post to publish)
go run ./cmd/casino prs                           # PRs /casino-review has acted on
go run ./cmd/core &  go run ./cmd/runner          # the real thing (needs DATABASE_URL)
```

## Services

| service  | binary         | role |
|---|---|---|
| postgres | —              | source of truth: jobs, review runs, events, markets/positions/payouts |
| core     | `cmd/core`     | trigger poller, dedup, job enqueue, GIF TTL cleanup, migrations |
| runner   | `cmd/runner`   | job executor: GIF spin + engines; image bundles git + node (analyzers) |
| slackbot | `cmd/slackbot` | market surface: `/casino` commands (one channel) + notification tailer |
| (casino) | `cmd/casino`   | admin/dry-run CLI: `gen`, `check`, `cleanup`, `db migrate`, `review run`, `market …` |

## Reviews registry

`REVIEWS=a,b,c` (legacy) still works — every name becomes a dispatch engine.
For typed engines, set `REVIEWS_FILE` (compose mounts `./reviews.json`):
see [reviews.example.json](reviews.example.json).

### LLM reviewers (sunset)

The claude engine is **gated off for now** — the hosting infra isn't ready to
run it properly (CLI + key management in the runner image, cost controls), and
a half-supported LLM reviewer is worse than none. The registry rejects
`"engine": "claude"` with a pointer here. The **foundations remain** for its
return: the `Engine` interface, the full runner + strict-JSON findings parser
([internal/review/claude.go](internal/review/claude.go)) with its security
posture already designed (read-only tools, minimal env, token-free checkouts,
blockquoted output), the persona files ([personas/](personas/)), and their
tests. Bringing it back = remove the gate in `registry.validate`, restore the
CLI in [Dockerfile.runner](Dockerfile.runner), and set `ANTHROPIC_API_KEY`.

### The bonus round (static addon)

The all-in-one static reviewer is **not on the reel** — it's an addon that
fires with probability `addon.chance` after the winner is chosen (`1.0` =
permanent). All its analyzer steps (eslint, tsc, …) run over one checkout and
post **one merged comment**; a broken step degrades to a finding instead of
sinking the pass. When the roll hits, the GIF plays a **bonus round**: the
winner reveal finishes as usual, then the machine lights up again — flashing
pink `* BONUS: STATIC *` banner, sparkle burst, steady dual-banner hold. The
roll happens before the GIF renders (animation always matches reality) and is
revealed only *after* the winner, so there's no early tell. For the experiment,
`spin.assigned` logs `addon_chance` + `addon_hit` (Bernoulli assignment with
propensity), and addon runs are excluded from the selector's signal.
Preview: `go run ./cmd/casino gen out.gif --bonus static`.

## The market (P2)

People stake USDC-denominated positions on questions about PRs — from Slack,
in one channel. Three market kinds:

| kind | question | payout |
|---|---|---|
| `bounty` | pool pays the PR author when it merges | whole pool → solver |
| `merge-by` | will this PR merge by \<deadline\>? | parimutuel (winners split the pool pro-rata) |
| `findings-count` | how many findings will the casino review produce? | parimutuel over buckets `0 / 1-2 / 3-5 / 6+` |

Slack (Socket Mode, honored **only** in `SLACK_CHANNEL`):
`/casino fund #123 25` · `/casino market #123 merge-by 72h` · `/casino bet 7 yes 10`
· `/casino board` · `/casino refund 7` · `/casino link <github-login>`.
The settling verbs (`lock`/`resolve`/`void`) move other people's money and are
restricted to `SLACK_ADMINS` (unset = disabled in Slack; the CLI on the host is
the admin path). The CLI mirrors everything
(`casino market …`), and a notification tailer over the events spine posts
channel updates for actions from any surface — which is also how the P3
oracles' resolutions will reach the channel with zero extra wiring.

Money integrity: amounts are int64 micro-USDC (on-chain USDC scaling — the
Base escrow seam); every state transition is a single guarded transaction
(concurrent double-resolve/refund is impossible — tested with racing
goroutines against real Postgres); parimutuel splits are exact big-int math
with dust audited to a `house` payout row; every money event is written in the
same transaction as the state change. Resolution is admin-only until the P3
oracles land (merge → bounty pays; review findings → findings-count resolves).

Setting up the Slack app: create an app, enable **Socket Mode** (app-level
token with `connections:write`), add a `/casino` slash command, grant the bot
`chat:write`, `commands`, `channels:read`, install it, invite it to your
channel, set the three `SLACK_*` envs.

## Telemetry — the experiment

Two planes, each doing what it's best at:

1. **`events` table (Postgres)** — the scientific record. Append-only; spin
   events log the full assignment (candidate pool, selector, chosen index) so
   the randomized-assignment experiment (which engine works best?) is analyzable.
   Market/money events are written in the same transaction as the state change,
   so the log can never drift from the ledger.
2. **Prometheus** (`METRICS_ADDR`) — ops: job durations, findings histograms,
   poll lag, GitHub rate headroom.

A third plane — **PostHog** behavioral analytics — was dropped for now: its
client pulled in a compile-heavy dependency that OOM-killed builds on 1 GB VMs.
The `telemetry.Track` seam remains a no-op, so restoring it is re-adding the
client without touching call sites.

## Notes & caveats

- **Trigger matching is command-style** (first token of a line). Never make it
  a substring match: the bot's own GIF comment contains `casino-review-assets`
  in its URL and the bot will loop on itself. `TestTemplatesNeverRetrigger`
  enforces this for every comment template.
- **GIF hosting.** GIFs are committed to an orphan `ASSETS_BRANCH`. If the
  monitored repo is private, set `ASSETS_REPO` to a public repo — private raw
  URLs expire in ~5 minutes. A TTL cleanup prunes GIFs older than `ASSETS_TTL`.
- **State is Postgres + the GitHub reaction.** The reaction is the durable
  "already handled" marker on each trigger comment; jobs are deduped by comment
  ID; restarts are safe on both sides.

## Roadmap

~~Phase 2: markets & positions + Slack bot~~ ✅. Phase 3: merge & findings
oracles — bounties pay on merge, findings-count locks at spin start and
resolves from `review_runs`. Phase 4: disputed resolutions judged by a
slot-machine-selected LLM judge (blocked on un-sunsetting LLM reviewers; human
`resolve` is the mechanism until then). Phase 5: weighted selector, stats
digest, static board publish.
