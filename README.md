# casino-review 🎰

The ultimate reviewer orchestrator you didn't even know you need! 

Watches a GitHub repo's pull requests for a `/casino-review` comment. When it
sees one, it posts a slot-machine GIF that lands on a randomly chosen review —
then actually runs it. Review entries are typed engines:

- **dispatch** — post `/<name>` and let an external review bot take it (the original behavior)
- **claude** — run a Claude Code persona headlessly over a checkout of the PR and post its findings
- **analyzer** — run a static tool (eslint, tsc, …) over the checkout and post parsed findings

Casino is in the name: the reel mixes all three. Every spin is a *random
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
cp .env.example .env          # fill GITHUB_TOKEN, GITHUB_REPO, ANTHROPIC_API_KEY
cp reviews.example.json reviews.json   # edit your engine pool + personas/
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
go run ./cmd/core &  go run ./cmd/runner          # the real thing (needs DATABASE_URL)
```

## Services

| service  | binary       | role |
|---|---|---|
| postgres | —            | source of truth: jobs, review runs, events (and markets, from P2) |
| core     | `cmd/core`   | trigger poller, dedup, job enqueue, GIF TTL cleanup, migrations |
| runner   | `cmd/runner` | job executor: GIF spin + engines; image bundles git/node/claude CLI |
| (casino) | `cmd/casino` | admin/dry-run CLI: `gen`, `check`, `cleanup`, `db migrate`, `review run` |

## Reviews registry

`REVIEWS=a,b,c` (legacy) still works — every name becomes a dispatch engine.
For typed engines, set `REVIEWS_FILE` (compose mounts `./reviews.json`):
see [reviews.example.json](reviews.example.json) and [personas/](personas/).
Claude engines run with read-only tools (`Read,Grep,Glob,Bash(git diff/log/show)`),
a turn cap, and a timeout; they must answer in strict JSON or the run errors —
the bot never fabricates findings.

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

## Telemetry — the experiment

Three planes, each doing what it's best at:

1. **`events` table (Postgres)** — the scientific record. Append-only; spin
   events log the full assignment (candidate pool, selector, chosen index) so
   the randomized-assignment experiment (which engine works best?) is analyzable.
2. **PostHog** (optional, `POSTHOG_API_KEY`) — behavioral analytics: who
   triggers, engine outcomes, claude cost per run. Fire-and-forget.
3. **Prometheus** (`METRICS_ADDR`) — ops: job durations, findings histograms,
   poll lag, GitHub rate headroom, claude spend counters.

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

Phase 2: markets & positions (bounty / merge-by / findings-count over PRs) +
Slack Socket-Mode bot (`/casino fund|bet|board`). Phase 3: merge & findings
oracles — bounties pay on merge. Phase 4: disputed resolutions are judged by a
slot-machine-selected LLM judge. Phase 5: weighted selector, stats digest.
See the plan in the repo history / pr-market-spec.
