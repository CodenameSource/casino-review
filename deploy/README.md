# Deploying on a Linux VM (systemd)

`casino-review` is a long-lived poller, so run it as **one** systemd service with
`Restart=always` — **not** a cron/timer (an interval cron would launch a new
forever-running poller every tick and stack them).

These steps assume the checkout lives at `~/casino-review` (i.e.
`/root/casino-review` when you're root). If your home is elsewhere, change
`ExecStart=`/`WorkingDirectory=` in `casino-review.service` to match.

## 1. Get the binary + config in place

The project dir should contain the binary as `casino-review`, your filled-in
`.env`, and `scripts/casino-review.sh`.

```bash
cd ~/casino-review

# binary: either build it on the VM (needs Go)...
CGO_ENABLED=0 go build -o casino-review .
# ...or copy the prebuilt static one from your machine (no Go needed):
#   scp dist/casino-review-linux-amd64  <vm>:~/casino-review/casino-review   # x86_64
#   scp dist/casino-review-linux-arm64  <vm>:~/casino-review/casino-review   # aarch64

chmod +x casino-review scripts/casino-review.sh
chmod 600 .env          # the token lives here

# dry test (the wrapper loads .env, then runs `check` — confirms it can read PRs).
# Running ./casino-review directly would NOT see .env; the wrapper sources it.
./scripts/casino-review.sh check
```

## 2. Install, enable, start

```bash
sudo cp ~/casino-review/deploy/casino-review.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now casino-review
```

## 3. Verify

```bash
systemctl status casino-review            # active (running)
journalctl -u casino-review -f            # live logs

# expect: authenticated as "<user>" on <owner>/<repo>
#         watching <owner>/<repo> for "/casino-review" comments every 30s
```

## Updating

```bash
cd ~/casino-review && git pull && CGO_ENABLED=0 go build -o casino-review .
sudo systemctl restart casino-review
# (or scp a fresh dist/ binary over ~/casino-review/casino-review, then restart)
```

## Notes

- Runs as root with this unit (matching `~/casino-review` = `/root`). To run as a
  less-privileged user instead, put the checkout in that user's home and add
  `User=`/`Group=` to the unit.
- The VM needs **outbound HTTPS** to `api.github.com` and `raw.githubusercontent.com`.
- State is the GitHub reaction, so restarts/redeploys never re-trigger handled
  comments — nothing to migrate.

## Running locally on macOS instead

`ai.mandel.casino-review.plist` is a launchd LaunchAgent for the same thing on a
Mac (its paths point at this checkout — edit them if you move it). Install with:

```bash
cp deploy/ai.mandel.casino-review.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/ai.mandel.casino-review.plist   # start (and at login)
tail -f ~/Library/Logs/casino-review.log                              # logs
launchctl unload ~/Library/LaunchAgents/ai.mandel.casino-review.plist # stop
```
