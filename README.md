# casino-review 🎰

The ultimate reviewer orchestrator you didn't even know you need! 

Watches a GitHub repo's pull requests for a `/casino-review` comment. When it
sees one, it posts a slot-machine GIF that lands on a randomly chosen review
(`tsetso-review`, `dimoreview`, `gigareview`, …). The GIF stays, and after a
wait it posts the real trigger comment (e.g. `/tsetso-review`) to kick off that
review.

<img width="400" height="300" alt="out" src="https://github.com/user-attachments/assets/0ab154db-f6d9-431f-b6c1-d57018bc5a69" />

(Insert your reviewer commands here)

## Flow

```
poll PR comments ──► sees "/casino-review" (not already processed)
   └► react 🚀 on the comment + persist its ID  (marks "processing")
   └► pick a review (random; see Milestone 2)
   └► render slot GIF landing on the winner   (internal/slots)
   └► commit GIF to assets branch + comment with just the GIF (it stays)
   └► wait DISPLAY_DURATION
   └► post "/<winner>" to trigger the real review
```

## The GIF

`internal/slots` builds a ~10-20s loop (every spin differs) in this beat order:
fade in from black → a held beat → a fast spin whose **speed and slowing moment
are randomised** → the reel comes to rest like a real reel, with a random number
of near-misses: it may stop short of the winner (0-3 stops, 3 rarest) and/or
**overshoot past it and tick back up** (0-2 stops, 2 rarest) → the winner gets
the longest pause, still plain white → only then the **reveal** (gold winner,
flashing band, sparkles, pulsing border, `* WINNER *` banner) → a long hold →
fade out to black for a clean loop seam. The deceleration is a latched,
monotonic ease-out (it never speeds back up). `DISPLAY_DURATION` is just how long
to wait before firing the real review — the GIF stays and loops regardless.

Two properties are enforced by tests ([slots_test.go](internal/slots/slots_test.go)):
motion never emits a duplicate (frozen) frame — what made the slow-down look
stuttery — and the **winner is never coloured before the reveal**, so its
position, dwell time, and the gold flash are the only signal. Combined with a
non-deterministic seed per spin, that's what stops a repeat viewer guessing the
result early.

## Quick start

```bash
# 1. preview a GIF with no GitHub access at all
go run . gen out.gif        # open out.gif to see the spin

# 2. set up credentials
cp .env.example .env        # fill in GITHUB_TOKEN + GITHUB_REPO
set -a; . ./.env; set +a

# 3. dry test: confirm the token can read the repo's PR comments (posts nothing)
go run . check

# 4. run the watcher (foreground)
go run .
```

## Running it for real (service)

It's a long-lived poller, so run **one** instance under a service manager with
auto-restart — not an interval cron (which would stack pollers). See
[deploy/README.md](deploy/README.md) for a Linux VM (systemd) and macOS (launchd)
setup, with prebuilt static Linux binaries in `dist/`.

## Configuration

All via environment variables — see [.env.example](.env.example).

## Notes & caveats

- **Token scope.** Monitoring needs only read, but reacting and posting/deleting
  the comment and GIF asset needs **write** (`repo`) scope. To preview the GIF
  with no GitHub at all, use `go run . gen out.gif`.
- **The 8-ball reaction.** GitHub's reactions API only accepts a fixed set
  (`+1 -1 laugh confused heart hooray rocket eyes`) — there is **no 🎱**. The
  default is `REACTION=rocket` (🚀); set `REACTION` to any of the valid values.
- **State is the reaction — no local file.** Dedup is durable on GitHub: a
  trigger comment we've handled carries our `REACTION`, so on the next poll (or
  after a restart) we see the reaction and skip it. The first poll looks back 24h,
  which is safe because already-handled comments are marked. An in-memory set is
  just a per-session fast path so we don't re-query reactions every poll.
- **GIF hosting / persistence.** GIFs are committed to `ASSETS_BRANCH` — an
  **orphan branch** (its own root history, just a README + the `casino/` folder, so
  it never carries the repo's main files) — and embedded via the
  `raw.githubusercontent.com` URL. For a **public** host repo the raw URL is
  permanent. For a **private** repo the URL carries a ~5-minute `?token=` and the
  embed goes blank once it expires — so if the monitored repo is private, set
  **`ASSETS_REPO`** to a **public** repo you own (the animations aren't sensitive;
  the same token must have write access to it).
- **GIF retention.** Each GIF's filename is timestamp-prefixed; a periodic cleanup
  prunes anything older than `ASSETS_TTL` (default 30 days) so the branch doesn't
  grow without bound. The comment stays; only the (long-expired) image goes.

## Milestone 2 (not yet built)

`internal/selector` is the seam. `selector.Context` already carries `PreviousIndex`;
extend it with signals like "did the previous review post any findings?" and
implement a weighted/gated `Selector` instead of `Random`. Gathering the
"findings" signal will likely need a small harness (e.g. Claude Code) reading the
review's output — wire that into the monitor and pass the result through `Context`.

## Layout

```
main.go                  entrypoint + `gen` subcommand
internal/config          env-var config
internal/github          tiny REST client (no SDK dep)
internal/slots           slot-machine GIF generator
internal/selector        review-picking logic (Milestone 2 seam)
internal/monitor         poll loop + spin orchestration
```
