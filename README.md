# gotify-tg

A small, send-only bridge that forwards every message from a
[Gotify](https://gotify.net) server (v3.x) to your Telegram DM.

- Listens on Gotify's `/stream` WebSocket with a client token.
- Sends each message via the Telegram Bot API `sendMessage` (HTML: bold
  title, app name, body). Messages longer than 4096 characters are split.
- Remembers the last forwarded message ID in `/data/state.json`. After a
  restart or reconnect it fetches missed messages via `GET /message`,
  forwards them in order and never sends the same ID twice.
- The bot only sends. It never calls `getUpdates` and has no webhook, so it
  accepts no input at all.

Single static binary, one dependency (`github.com/coder/websocket`),
distroless non-root image.

## How it works

1. On start, open `/stream`, then fetch everything newer than the saved ID
   through `GET /message` (paging backwards, 200 per page) and queue it
   oldest first.
2. Every message from the stream is queued unless its ID was already queued.
3. A sender drains the in-memory queue (1000 messages) to Telegram and saves
   the ID after each message. If the queue is full, the stream is dropped and
   the messages are picked up again by the catch-up on reconnect, so a slow
   Telegram never blocks the stream and nothing is lost.

On the very first start (no `state.json`) the bridge starts *after* the
newest existing message; old history is not replayed.

Reliability details:

- Reconnects with exponential backoff and jitter (1s to 60s).
- Pings the server every 30s; a pong missing for 10s drops the connection.
- Telegram `429` waits for `retry_after`; `5xx`, network errors, `401/403/404`
  are retried with backoff (up to 60s), so a temporary misconfiguration does
  not lose messages. A `400` (message refused) is logged and skipped.
- `SIGTERM` stops reading, drains the queue for up to 15s and exits. Anything
  unsent is sent after the next start.
- `gotify-tg healthcheck` exits 0 if the stream was confirmed alive in the
  last 90s (timestamp file `/data/last_connected`).

## Setup

All commands below are for the fish shell.

### 1. Create the Telegram bot and get your chat ID

1. In Telegram, open [@BotFather](https://t.me/BotFather), send `/newbot` and
   follow the prompts. You get a token like `123456789:AAH...`.
2. Recommended: in BotFather send `/setjoingroups`, pick the bot and choose
   **Disable**, so nobody can add it to a group.
3. Open a chat with your new bot and press **Start** (send `/start`). A bot
   cannot write to you before you do this.
4. Get your numeric chat ID by asking Telegram once for the updates the bot
   received:

   ```fish
   read -s -P 'Telegram bot token: ' tok
   curl -s "https://api.telegram.org/bot$tok/getUpdates" | string match -r '"chat":\{"id":-?[0-9]+'
   set -e tok
   ```

   The number after `"id":` is your `TELEGRAM_CHAT_ID`. (This is a one-off
   manual call; the service itself never reads updates.)

### 2. Create a Gotify client token

In the Gotify web UI open **Clients**, click **Create Client**, name it e.g.
`telegram-bridge` and copy the token. Since Gotify 3 the token is shown only
once, on creation. A *client* token is needed (not an application token),
because only clients can read messages.

### 3. Deploy with the prebuilt image

Every push to `main` that passes the tests publishes a multi-arch image
(`linux/amd64`, `linux/arm64`) to
[`ghcr.io/mimic890/gotify-tg`](https://github.com/Mimic890/gotify-tg/pkgs/container/gotify-tg).
You do not need to clone the repository to run it.

Create a directory for the deployment (`.env` and state live next to the
compose file):

```fish
mkdir -p ~/gotify-tg
cd ~/gotify-tg
```

Save this as `docker-compose.yml`:

```yaml
services:
  gotify-tg:
    image: ghcr.io/mimic890/gotify-tg:latest
    container_name: gotify-tg
    restart: unless-stopped
    environment:
      GOTIFY_URL: ${GOTIFY_URL:?set GOTIFY_URL in .env}
      GOTIFY_CLIENT_TOKEN: ${GOTIFY_CLIENT_TOKEN:?set GOTIFY_CLIENT_TOKEN in .env}
      TELEGRAM_BOT_TOKEN: ${TELEGRAM_BOT_TOKEN:?set TELEGRAM_BOT_TOKEN in .env}
      TELEGRAM_CHAT_ID: ${TELEGRAM_CHAT_ID:?set TELEGRAM_CHAT_ID in .env}
      MIN_PRIORITY: ${MIN_PRIORITY:-0}
      APP_IDS: ${APP_IDS:-}
      ALLOW_INSECURE_GOTIFY: ${ALLOW_INSECURE_GOTIFY:-false}
      LOG_LEVEL: ${LOG_LEVEL:-info}
      DATA_DIR: /data
    volumes:
      - ./data:/data
    user: "65532:65532"
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    mem_limit: 64m
    pids_limit: 64
    stop_grace_period: 30s
    healthcheck:
      test: ["CMD", "/gotify-tg", "healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 60s
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
```

Save this as `.env` and fill in your values:

```dotenv
# Base URL of your Gotify server (https:// or wss://; a sub-path like /gotify is fine).
GOTIFY_URL=https://gotify.example.com

# Gotify client token (Gotify web UI -> Clients -> Create Client).
GOTIFY_CLIENT_TOKEN=

# Telegram bot token from @BotFather.
TELEGRAM_BOT_TOKEN=

# Numeric Telegram chat ID of your DM with the bot.
TELEGRAM_CHAT_ID=123456789

# Forward only messages with at least this priority.
MIN_PRIORITY=0

# Comma-separated Gotify application IDs to forward. Empty = all.
APP_IDS=

# Set to true only for plain http/ws on a trusted network (docker network, mesh VPN).
ALLOW_INSECURE_GOTIFY=false

# debug, info, warn or error.
LOG_LEVEL=info
```

Protect `.env`, create the data directory and start:

```fish
chmod 600 .env
mkdir -p data
# The container runs as UID/GID 65532 and must be able to write data/.
sudo chown 65532:65532 data
sudo chmod 0700 data

docker compose pull
docker compose up -d
docker compose logs -f
docker compose ps       # STATUS should become "healthy"
```

Send a test notification to Gotify and it should arrive in Telegram.

`latest` follows `main`. To pin a version, use a release tag such as
`ghcr.io/mimic890/gotify-tg:1.0.0` (published for `v*` git tags) or a digest
(`ghcr.io/mimic890/gotify-tg@sha256:...`).

### Gotify on the same Docker host

If Gotify runs in another Compose project, you can talk to it over a shared
Docker network instead of the public URL. Add to `docker-compose.yml`:

```yaml
services:
  gotify-tg:
    networks: [default, gotify]
networks:
  gotify:
    external: true
    name: gotify_default   # the network of your Gotify project
```

and set `GOTIFY_URL=http://gotify:80` plus `ALLOW_INSECURE_GOTIFY=true`.
Only do this on a network you trust (a Docker network on the same host or an
encrypted mesh such as WireGuard/Tailscale).

## Configuration

All settings, including the tokens, live in `.env` (read by Compose).
`DATA_DIR` is already set in the compose file.

| Variable | Default | Description |
| --- | --- | --- |
| `GOTIFY_URL` | required | Base URL of Gotify, `https://` or `wss://`. A sub-path (`https://example.com/gotify`) is supported. Must not contain credentials or a query. |
| `GOTIFY_CLIENT_TOKEN` | required | Gotify client token. |
| `TELEGRAM_BOT_TOKEN` | required | Telegram bot token from @BotFather. |
| `TELEGRAM_CHAT_ID` | required | Numeric chat ID to send to (non-zero integer). |
| `MIN_PRIORITY` | `0` | Forward only messages with priority >= this (0 to 1000). Missing priority counts as 0. |
| `APP_IDS` | empty = all | Comma-separated Gotify application IDs to forward, e.g. `1,4`. |
| `ALLOW_INSECURE_GOTIFY` | `false` | Allow `http://`/`ws://` for Gotify. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `DATA_DIR` | `/data` | Directory for `state.json` and the health timestamp. |

Alternatively, each token can be read from a file via `GOTIFY_CLIENT_TOKEN_FILE`
/ `TELEGRAM_BOT_TOKEN_FILE` (e.g. with Docker secrets). Setting both a token
and its `*_FILE` variant is an error.

The configuration is validated at startup; on any problem the process prints
all errors and exits with code 2.

## Security notes

- **Tokens.** Tokens live in `.env` (git-ignored, keep it `chmod 600`). Note
  that environment variables are visible to anyone who can run
  `docker inspect` on the host. The Gotify token is sent only in the
  `X-Gotify-Key` header, never in a URL. Tokens are never logged; transport
  errors are stripped of URLs (the Telegram API URL contains the bot token).
- **Transport.** `https://`/`wss://` is required for Gotify unless
  `ALLOW_INSECURE_GOTIFY=true`. TLS 1.2 is the minimum. HTTP redirects are
  not followed, so credentials are never sent to another location. All HTTP
  calls have timeouts and response size limits.
- **Telegram.** Send-only: no `getUpdates`, no webhook, the bot processes no
  input. All Gotify content (title, app name, body) is HTML-escaped. Link
  previews are disabled, so Telegram does not fetch URLs from your
  notifications.
- **Container.** Static binary (`CGO_ENABLED=0`) in
  `gcr.io/distroless/static-debian12:nonroot` (no shell, no package manager),
  base images pinned by digest. Runs as `65532:65532` with a read-only root
  filesystem, all capabilities dropped, `no-new-privileges`, memory and PID
  limits, no published ports. The only writable path is the `./data` bind
  mount.
- **Gotify client token scope.** A client token has broad access to its
  user's account (reading and deleting messages, managing applications); since
  Gotify 3 only a few destructive actions require elevation. Treat it like a
  password and revoke it by deleting the client in the Gotify UI. The bridge
  only uses `GET /stream`, `GET /message` and `GET /application`.

## Container image

The [`ci`](.github/workflows/ci.yml) workflow runs `gofmt`, `go vet`,
`staticcheck`, `govulncheck` and `go test -race` on every push and pull
request. Only if they pass, the image is built for `linux/amd64` and
`linux/arm64` and, for pushes to `main` and `v*` tags, pushed to GHCR:

| Git event | Image tags |
| --- | --- |
| push to `main` | `latest`, `sha-<short sha>` |
| tag `v1.2.3` | `1.2.3`, `1.2`, `sha-<short sha>` |
| pull request | built only, not pushed |

Images carry an SBOM, build provenance and a GitHub artifact attestation.
To verify an image with the GitHub CLI:

```fish
gh attestation verify oci://ghcr.io/mimic890/gotify-tg:latest --owner Mimic890
```

## Development

The `compose.yaml` in this repository builds the image locally from source
(`gotify-tg:local`) instead of pulling it, with the same settings as the
deployment file above:

```fish
git clone https://github.com/Mimic890/gotify-tg.git
cd gotify-tg
cp .env.example .env
$EDITOR .env
# create data/ as in step 3
docker compose up -d --build
```

Checks (the same as in CI):

```fish
gofmt -l .
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
go test -race ./...
```

## Updating

Prebuilt image:

```fish
docker compose pull
docker compose up -d
```

From source: `git pull` and `docker compose up -d --build`.

To bump the base images, update the tags and `@sha256:` digests in the
`Dockerfile` (`docker buildx imagetools inspect golang:<version>` prints the
digest). GitHub Actions are pinned by commit SHA in
`.github/workflows/ci.yml`.
