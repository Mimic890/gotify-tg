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
4. Get your numeric chat ID. Put the token into its secret file first (see
   step 3 below), then ask Telegram once for the updates the bot received:

   ```fish
   curl -s "https://api.telegram.org/bot"(cat secrets/telegram_bot_token)"/getUpdates" | string match -r '"chat":\{"id":-?[0-9]+'
   ```

   The number after `"id":` is your `TELEGRAM_CHAT_ID`. (This is a one-off
   manual call; the service itself never reads updates.)

### 2. Create a Gotify client token

In the Gotify web UI open **Clients**, click **Create Client**, name it e.g.
`telegram-bridge` and copy the token. Since Gotify 3 the token is shown only
once, on creation. A *client* token is needed (not an application token),
because only clients can read messages.

### 3. Configure and start

```fish
git clone https://github.com/mimic890/gotify-tg.git
cd gotify-tg

cp .env.example .env
$EDITOR .env            # GOTIFY_URL, TELEGRAM_CHAT_ID, optional filters

mkdir -p secrets data
chmod 700 secrets
umask 077
read -s -P 'Gotify client token: ' tok; and printf '%s\n' $tok > secrets/gotify_client_token
read -s -P 'Telegram bot token: ' tok; and printf '%s\n' $tok > secrets/telegram_bot_token
set -e tok
umask 022

# The container runs as UID/GID 65532 and must read the secrets and write data/.
sudo chown 65532:65532 secrets/gotify_client_token secrets/telegram_bot_token data
sudo chmod 0400 secrets/gotify_client_token secrets/telegram_bot_token
sudo chmod 0700 data

docker compose up -d --build
docker compose logs -f
docker compose ps       # STATUS should become "healthy"
```

Send a test notification to Gotify and it should arrive in Telegram.

### Gotify on the same Docker host

If Gotify runs in another Compose project, you can talk to it over a shared
Docker network instead of the public URL. Add to `compose.yaml`:

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

Non-secret settings live in `.env` (read by Compose), secrets in `./secrets/`.

| Variable | Default | Description |
| --- | --- | --- |
| `GOTIFY_URL` | required | Base URL of Gotify, `https://` or `wss://`. A sub-path (`https://example.com/gotify`) is supported. Must not contain credentials or a query. |
| `GOTIFY_CLIENT_TOKEN_FILE` | set by Compose | File containing the Gotify client token (`/run/secrets/gotify_client_token`). |
| `TELEGRAM_BOT_TOKEN_FILE` | set by Compose | File containing the Telegram bot token (`/run/secrets/telegram_bot_token`). |
| `TELEGRAM_CHAT_ID` | required | Numeric chat ID to send to (non-zero integer). |
| `MIN_PRIORITY` | `0` | Forward only messages with priority >= this (0 to 1000). Missing priority counts as 0. |
| `APP_IDS` | empty = all | Comma-separated Gotify application IDs to forward, e.g. `1,4`. |
| `ALLOW_INSECURE_GOTIFY` | `false` | Allow `http://`/`ws://` for Gotify. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `DATA_DIR` | `/data` | Directory for `state.json` and the health timestamp. |

`GOTIFY_CLIENT_TOKEN` and `TELEGRAM_BOT_TOKEN` can also be set directly as
environment variables. This is discouraged: plain environment variables show
up in `docker inspect`, in the process environment and easily end up in
shell history or backups. Setting both the variable and its `*_FILE` variant
is an error.

The configuration is validated at startup; on any problem the process prints
all errors and exits with code 2.

## Security notes

- **Tokens.** Secrets are mounted as files via Compose `secrets:` from
  `./secrets/` (git-ignored). The Gotify token is sent only in the
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

## Development

```fish
go test -race ./...
go vet ./...
staticcheck ./...
govulncheck ./...
docker build -t gotify-tg:local .
```

## Updating

```fish
git pull
docker compose up -d --build
```

To bump the base images, update the tags and `@sha256:` digests in the
`Dockerfile` (`docker buildx imagetools inspect golang:<version>` prints the
digest).
