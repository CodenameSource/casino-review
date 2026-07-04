# Deploying casino-review (docker compose)

The stack is compose-managed: `postgres` + `core` + `runner`. No public URL is
required (GitHub is polled; Slack — from P2 — uses Socket Mode).

## First deploy on a VM

```bash
git clone https://github.com/CodenameSource/casino-review.git ~/casino-review
cd ~/casino-review
cp .env.example .env            # fill GITHUB_TOKEN, GITHUB_REPO, POSTGRES_PASSWORD
cp reviews.example.json reviews.json   # define your engine pool + the static addon
docker compose up -d --build
```

For the market surface, also set `SLACK_BOT_TOKEN`, `SLACK_APP_TOKEN`,
`SLACK_CHANNEL` in `.env` (Slack app setup: README → "The market").

Verify:

```bash
docker compose ps                              # all services healthy/running
docker compose logs -f core                    # "watching owner/repo for /casino-review …"
docker compose logs -f runner                  # "runner: N engines loaded …"
docker compose logs -f slackbot                # "honoring /casino only in channel C…"
docker compose exec core casino check          # read-only GitHub smoke test
docker compose exec core casino market board   # the market board from the CLI
```

## Upgrade

```bash
cd ~/casino-review && git pull
docker compose build && docker compose up -d   # migrations run automatically at core startup
```

## Useful ops

```bash
docker compose exec core casino cleanup                        # prune old GIFs now
docker compose exec runner casino review run eslint --pr 42    # dry-run an engine in the runner env
docker compose exec postgres psql -U casino casino             # poke the DB
docker compose exec postgres pg_dump -U casino casino > backup.sql   # backup
```

Prometheus metrics are on `:9090/metrics` inside each service container
(`docker compose exec core wget -qO- localhost:9090/metrics`). Point a
prometheus at the compose network or publish the ports if you want dashboards.

## Notes

- The runner image bundles git and node 20 (for `npx eslint` / `npx tsc`).
  LLM reviewers are sunset — when they return, the claude CLI goes back into
  `Dockerfile.runner` and `ANTHROPIC_API_KEY` into `.env`.
- The old systemd/launchd deployment is retired — if you still have the
  `casino-review` systemd unit enabled from before, `sudo systemctl disable --now
  casino-review` before starting compose, or you'll have two bots reacting.
- State lives in the `pgdata` volume + the GitHub reactions; containers are
  disposable.
