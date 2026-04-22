# Dress To Impress Voting Chatbot for Twitch

Lets Twitch chat rate players' outfits during a Dress to Impress stream. The broadcaster opens a round with `!startvote player1 player2`, chat rates each player 1–5 stars with `!vote player 4`, and the bot tallies per-player results.

```
Twitch chat 
  → EventSub WebSocket
    → bot (tallies per-player votes)
      → HTTP GET /api/votes
        → optional: external consumer polls results
```

---

## Prerequisites

- [Go 1.22+](https://go.dev/dl/)
- A dedicated Twitch account for the bot (see below)
- A [Twitch application](https://dev.twitch.tv/console) registered under any account you control

## 1. Create a Twitch application

1. Go to [dev.twitch.tv/console](https://dev.twitch.tv/console) and click **Register Your Application**
2. Set **OAuth Redirect URL** to `http://localhost:3000`
3. Set **Category** to Chat Bot
4. Copy the **Client ID** and **Client Secret**

The application can be registered under your main account, so this doesn't need to be on the bot account.

## 2. Create a bot account

Create a separate Twitch account for the bot (`VoteBot` for instance). This is the identity that will appear in chat. Using a dedicated account keeps things clean and avoids confusion with your personal account.

## 3. Get an OAuth token

The token must belong to the **bot account**.

Using the [Twitch CLI](https://dev.twitch.tv/docs/cli):

```sh
twitch configure # enter the Client ID and Client Secret from step 1
twitch token -u -s 'user:bot user:read:chat user:write:chat'
```

This opens a browser. **Make sure you're logged into the bot account**, not your main account. Authorize, and the CLI prints the access token.

Alternatively, use https://github.com/twitchdev/authentication-go-sample 
with scopes set to 
```sh
user:bot user:read:chat user:write:chat
```

## 4. Get User IDs

Twitch API calls use numeric User IDs, not login names. Look up the bot account and the broadcaster's account:

```sh
twitch api get /users -q login=YOUR_BOT_LOGIN
twitch api get /users -q login=BROADCASTER_LOGIN
```

The `id` field from each response goes into `BOT_USER_ID` and `BROADCASTER_USER_ID` respectively. If you're streaming from your own channel, the broadcaster is your main account.

## 5. Authorize the bot in the broadcaster's channel

The bot can listen for messages with just the token from step 3, but Twitch will reject outbound messages unless the broadcaster grants permission. Pick one of these two options:

### Authorize the `channel:bot` scope (no mod badge)

The **broadcaster** (not the bot account) visits this URL in a browser where they are logged into their broadcaster Twitch account:

```
https://id.twitch.tv/oauth2/authorize?response_type=code&client_id=YOUR_CLIENT_ID&redirect_uri=http://localhost:3000&scope=channel:bot&force_verify=true
```

Replace `YOUR_CLIENT_ID` with the Client ID from step 1. The broadcaster authorizes the request, which grants the bot's application permission to send messages in their channel. The redirect will fail (there's nothing listening) — that's fine, the authorization is already saved.

This approach gives the bot a Chat Bot badge instead of a moderator badge and doesn't grant it any moderation powers.

## 6. Configure the bot

```sh
cd voting-bot
cp .env.example .env
```

Open `.env` and fill in the values:

```sh
BOT_USER_ID=123456789          # numeric ID of the bot account (from step 4)
OAUTH_TOKEN=xxxxxxxxxxxx       # token from step 3
CLIENT_ID=xxxxxxxxxxxxxxxxxxxx # from step 1

BROADCASTER_USER_ID=987654321  # numeric ID of the broadcaster's channel (from step 4)

BRIDGE_PORT=3000               # port the HTTP bridge listens on
BRIDGE_SECRET=choose-a-secret  # arbitrary string for HTTP bridge auth

MIN_VOTE=1
MAX_VOTE=5
```

# Running the bot locally

```sh
cd voting-bot
go run .
```

You should see:

```
{"level":"info","msg":"Twitch auth token validated."}
{"level":"info","msg":"HTTP bridge listening.","addr":":3000"}
{"level":"info","msg":"Connected to EventSub WebSocket."}
{"level":"info","msg":"Subscribed to channel chat messages."}
```

The HTTP bridge is available at `http://localhost:3000/api/votes`. For production deployment, see [Deploying to a server](#deploying-to-a-server) below.

# Usage

### Starting a round

When players are on the runway, the broadcaster or a mod types:

```
!startvote Player1 Player2 Player3
```

The bot announces voting is open and lists the players. Player names are Roblox usernames — alphanumeric and underscores only, no spaces, case-insensitive when voting. Up to 20 players per round.

Starting a new round automatically clears the previous round's players and votes.

### Voting

Viewers rate each player 1–5 stars:

```
!vote Player1 4
!vote Player2 5
!vote Player3 2
```

Each viewer can vote once per player and can change their vote by sending the command again. Viewers can rate as many or as few players as they like. Voting only counts while a round is open.

If a viewer names a player that isn't in the current round, the bot replies with the valid player list.

### Checking standings

Anyone can type `!votes` to see current per-player averages at any time.

### Resetting votes

The broadcaster or a mod can type `!resetvote` to clear all votes while keeping the round open with the same players.

### Ending a round

The broadcaster or a mod types:

```
!endvote
```

The bot posts final results sorted by average:

```
Voting closed! Player2: 4.3/5 (12 votes) | Player1: 3.8/5 (12 votes) | Player3: 2.5/5 (11 votes)
```

## Chat commands

| Command | Who | What it does |
|---|---|---|
| `!startvote player1 player2 ...` | Broadcaster / Mods | Opens voting for the listed players, resets any previous round |
| `!endvote` | Broadcaster / Mods | Closes voting, posts per-player results to chat |
| `!resetvote` | Broadcaster / Mods | Clears all votes, keeps round open with same players |
| `!vote <player> <1–5>` | Anyone | Rate a player (one vote per viewer per player, can change) |
| `!votes` | Anyone | Show current per-player standings |

---

## HTTP bridge API

The bot exposes an HTTP API for reading vote state and managing rounds programmatically. All endpoints except `GET /api/votes` require the `X-Bridge-Secret` header.

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/votes` | Current vote state (no auth required) |
| `POST` | `/api/votes/start` | Open voting — body: `{"players":["player1","player2"]}` |
| `POST` | `/api/votes/cast` | Cast a vote — body: `{"player":"player1","username":"viewer1","value":4}` |
| `POST` | `/api/votes/end` | Close voting, returns final tally |
| `POST` | `/api/votes/reset` | Clear tallies without closing |

Example response from `GET /api/votes`:

```json
{
  "open": true,
  "players": ["Player1", "Player2"],
  "startedAt": 1712700000000,
  "results": [
    {
      "player": "Player1",
      "counts": {"1": 0, "2": 1, "3": 2, "4": 4, "5": 3},
      "total": 10,
      "average": 3.9
    },
    {
      "player": "Player2",
      "counts": {"1": 1, "2": 2, "3": 3, "4": 2, "5": 0},
      "total": 8,
      "average": 2.8
    }
  ]
}
```

# Server Configuration

> **Why you need a domain:** The HTTP bridge should be served over HTTPS with a valid certificate. A subdomain of something you already own is fine (e.g. `bot.yourdomain.com`).

The stack is Docker Compose + Traefik + Let's Encrypt via HTTP challenge. Traefik obtains and renews the cert automatically; port 80 must be open so Let's Encrypt can reach the challenge endpoint during issuance and renewal. All non-challenge HTTP traffic is redirected to HTTPS.

### What you need

- Linux VPS - this is very lightweight, so most micro/nano sized VPS are suitable.
- Rootless Podman and `podman-compose` installed on the server
- A domain with an A record pointing at your server's IP
- Ports 22, 80, and 443 open inbound (via your provider's cloud firewall or host-level iptables)

### 1. Create a dedicated user

The bot runs as a dedicated user with no login shell and no sudo access. Rootless Podman means its containers are also confined to that user's privileges.

```sh
# Home dir is required. Podman uses it for container storage,
# and systemd user services live under ~/.config/systemd/user/
sudo useradd -m -s /usr/sbin/nologin bot-user

# Rootless Podman needs subordinate UID/GID ranges for user namespace mapping.
# useradd allocates these automatically on most distros. to verify:
grep bot /etc/subuid /etc/subgid
# Expected output (ranges may differ):
#   /etc/subuid: bot-user:100000:65536
#   /etc/subgid: bot-user:100000:65536
# If missing, allocate them:
sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 bot-user

# Allow the user's systemd session to persist without an active login.
# This also ensures /run/user/<uid> (XDG_RUNTIME_DIR) is created at boot,
# which is where the Podman socket lives.
sudo loginctl enable-linger bot-user

# Allow rootless containers to bind to ports 80 and 443:
echo "net.ipv4.ip_unprivileged_port_start=80" | sudo tee /etc/sysctl.d/99-podman-ports.conf
sudo sysctl --system

# Enable the Podman socket for the bot-user. Traefik's socket-proxy
# connects to this socket for container discovery.
RUN_DIR=/run/user/$(id -u bot-user)
sudo -u bot-user bash -c "XDG_RUNTIME_DIR=$RUN_DIR DBUS_SESSION_BUS_ADDRESS=unix:path=$RUN_DIR/bus systemctl --user enable --now podman.socket"
```

All subsequent commands that run as the `bot-user` user need `RUN_DIR` set. If you're in a new shell:

```sh
RUN_DIR=/run/user/$(id -u bot-user)
```

### 2. DNS

Add an **A record** for `voting-bot.example.com` pointing at your server's IP. If using Cloudflare, set it to **DNS only** (grey cloud) — Traefik handles TLS at the origin.

### 3. Configure and deploy files

Copy the repo to the dedicated user's home and fill in the environment files:

```sh
sudo cp -r /path/to/twitch-voting /home/bot-user/twitch-voting
sudo chown -R bot-user:bot-user /home/bot-user/twitch-voting
```

Fill in `/home/bot-user/twitch-voting/.env`:

```sh
DOMAIN=voting-bot.example.com
LETSENCRYPT_EMAIL=you@example.com
```

Fill in `/home/bot-user/twitch-voting/voting-bot/.env` (see step 6 in the local setup above).

Restrict both env files so only the `bot-user` user can read them:

```sh
sudo chmod 600 /home/bot-user/twitch-voting/.env
sudo chmod 600 /home/bot-user/twitch-voting/voting-bot/.env
```

### 4. Firewall

Only expose 22, 80, and 443. Port 3000 stays internal — only Traefik talks to the bot directly. Port 80 is required for Let's Encrypt HTTP challenge validation and also serves HTTPS redirects.

If your provider has a cloud firewall (Linode, DigitalOcean, etc.), configure it there:

| Label | Protocol | Port | Source |
|---|---|---|---|
| SSH | TCP | 22 | All IPv4, All IPv6 |
| HTTP | TCP | 80 | All IPv4, All IPv6 |
| HTTPS | TCP | 443 | All IPv4, All IPv6 |

Set the default inbound policy to **Drop**. Outbound can stay **Accept** (the default).

### 5. Set up the systemd user service

Create `/home/bot-user/.config/systemd/user/voting-bot.service`:

```sh
sudo -u bot-user mkdir -p /home/bot-user/.config/systemd/user
sudo -u bot-user tee /home/bot-user/.config/systemd/user/voting-bot.service << 'EOF'
[Unit]
Description=Dress to Impress Voting Twitch Chatbot
Requires=podman.socket
After=network.target podman.socket

[Service]
Type=oneshot
RemainAfterExit=yes
WorkingDirectory=%h/voting-bot
ExecStart=podman-compose up -d
ExecStop=podman-compose down

[Install]
WantedBy=default.target
EOF
```

Enable and start it:

```sh
sudo -u bot-user bash -c "XDG_RUNTIME_DIR=$RUN_DIR DBUS_SESSION_BUS_ADDRESS=unix:path=$RUN_DIR/bus systemctl --user daemon-reload"
sudo -u bot-user bash -c "XDG_RUNTIME_DIR=$RUN_DIR DBUS_SESSION_BUS_ADDRESS=unix:path=$RUN_DIR/bus systemctl --user enable --now voting-bot"
```

The first start pulls images and builds the bot, so it may take a few minutes. Traefik will issue the Let's Encrypt certificate on the first HTTPS request (usually under 30 seconds after startup). Check logs:

```sh
sudo -u bot-user bash -c "XDG_RUNTIME_DIR=$RUN_DIR DBUS_SESSION_BUS_ADDRESS=unix:path=$RUN_DIR/bus journalctl --user -u bot-user -f"
```

### Security notes

**Rootless Podman**

The compose file assumes rootless Podman. This matters for two reasons:

- *Socket*: the Podman socket is user-scoped, so a compromised Traefik gains user-level host access rather than root. The `socket-proxy` is still used as defence-in-depth.
- *Firewall*: rootless Podman uses user-space networking (`slirp4netns`/`pasta`) rather than kernel iptables. This means host-level firewall rules (iptables, nftables, or cloud firewalls) apply to published ports as you'd expect — there's no kernel-level bypass.

If you switch to rootful Podman or Docker, the networking model reverts to kernel iptables (same bypass behaviour as Docker), and the socket risk goes back to root-level.

**Podman socket path**

The compose file uses `${XDG_RUNTIME_DIR}/podman/podman.sock`, which resolves to `/run/user/<uid>/podman/podman.sock`. Because `loginctl enable-linger` is set for the `bot-user` user, systemd creates and owns that runtime directory at boot — `XDG_RUNTIME_DIR` is always set correctly when the user service starts.

**Socket proxy**

Traefik reaches the Podman socket through `tecnativa/docker-socket-proxy`, which only exposes `CONTAINERS: 1` (label reads). The `socket-proxy` network is `internal: true` so the proxy has no external network access.

**`GET /api/votes` is unauthenticated**

Intentional — the endpoint returns read-only vote state, so exposure is low risk. This makes it easy to build external consumers (stream overlays, etc.) without distributing the secret. If you'd rather lock it down, add the `X-Bridge-Secret` check to `handleVotes` in `bridge.go`.

### Operations

All commands below assume `RUN_DIR` is set:

```sh
RUN_DIR=/run/user/$(id -u bot-user)
```

**Updating the bot**

```sh
cd /home/bot-user/voting-bot
sudo git pull
sudo chown -R bot-user:bot-user .

sudo -u bot-user bash -c "cd /home/bot-user/twitch-voting/voting-bot && XDG_RUNTIME_DIR=$RUN_DIR podman-compose build bot"
sudo -u bot-user bash -c "cd /home/bot-user/twitch-voting/voting-bot && XDG_RUNTIME_DIR=$RUN_DIR podman-compose up -d --force-recreate bot"
```

**Rotating secrets or changing env vars**

Edit the `.env` file, then recreate the container to pick up the new values (no rebuild needed):

```sh
sudo -u bot-user bash -c "cd /home/bot-user/twitch-voting/voting-bot && XDG_RUNTIME_DIR=$RUN_DIR podman-compose up -d --force-recreate voting-bot"
```

**Restarting all services**

```sh
sudo -u bot-user bash -c "cd /home/bot-user/twitch-voting/voting-bot && XDG_RUNTIME_DIR=$RUN_DIR podman-compose down"
sudo -u bot-user bash -c "cd /home/bot-user/twitch-voting/voting-bot && XDG_RUNTIME_DIR=$RUN_DIR podman-compose up -d"
```

**Viewing logs**

```sh
# Bot logs
sudo -u bot-user bash -c "cd /home/bot-user && XDG_RUNTIME_DIR=$RUN_DIR podman logs -f voting-bot_bot_1"

# Traefik logs
sudo -u bot-user bash -c "cd /home/bot-user && XDG_RUNTIME_DIR=$RUN_DIR podman logs -f voting-bot_traefik_1"

# Systemd service logs
sudo -u bot-user bash -c "XDG_RUNTIME_DIR=$RUN_DIR DBUS_SESSION_BUS_ADDRESS=unix:path=$RUN_DIR/bus journalctl --user -u voting-bot -f"
```

**Checking container status**

```sh
sudo -u bot-user bash -c "cd /home/bot-user && XDG_RUNTIME_DIR=$RUN_DIR podman ps -a"
```

---

## File overview

```
voting-bot/
  main.go      startup and wiring
  config.go    env var loading
  votes.go     per-player vote state and tallying
  twitch.go    EventSub WebSocket, chat commands, Twitch API calls
  bridge.go    HTTP API for vote management
```
