<p align="center">
  <img src="assets/logo.png" alt="sourcewatch" width="160">
</p>

# sourcewatch — game server alerts

sourcewatch watches Source game servers (CS:S, CS2, TF2 and more) and tells you on Discord when something happens: a map change, a player joining, the server filling up, going offline, or its name changing.

## Contents

- [Quick start](#quick-start)
- [Commands at a glance](#commands-at-a-glance)
- [Game servers: `/server`](#game-servers--server)
- [What is going on: `/status`](#what-is-going-on--status)
- [Notifications: `/notify`](#notifications--notify)
- [Where it arrives, and how often](#where-it-arrives-and-how-often)
- [Combine conditions with `id`](#combine-conditions-with-id)
- [Admin settings: `/admin settings`](#admin-settings--admin-settings)
- [Manage your notifications](#manage-your-notifications)
- [Good to know](#good-to-know)
- [What sourcewatch stores](#what-sourcewatch-stores)
- [Run your own sourcewatch](#run-your-own-sourcewatch)

## Quick start

1. An admin adds a game server: `/server add address:1.2.3.4:27015`.
2. Anyone makes a notification: `/notify map`, `/notify player`, `/notify playercount`, `/notify status` or `/notify title-query`.
3. Show your servers and who is online any time: `/status`.

Only you can see the bot's replies to your commands, except `/status` and `/notify show`, which everyone in the channel sees. Alerts arrive in the channel where you made the notification (mentioning only you), or in your DMs if you choose that. Type `/help` for a short reminder.

## Commands at a glance

| Command | What it does |
|---|---|
| `/server add` · `/server remove` | Start or stop watching a game server (needs **Manage Server** in a Discord server) |
| `/status` | What every game server you watch is doing right now and who is online. Everyone in the channel sees it |
| `/notify map` · `player` · `playercount` · `status` · `title-query` | Make a notification |
| `/notify list` · `/notify show` | Show your notifications to you, or to everyone in the channel |
| `/notify remove` | Delete one notification by its ID |
| `/notify settings` | See or change the cooldown, on/off and delivery of one notification, or all of them |
| `/notify reset` | Delete all your notifications here (it asks first) |
| `/admin settings` | For admins: channel delivery, default delivery, default cooldown, switching the bot off, and a player blacklist (needs **Manage Server**) |
| `/help` | A short reminder of all this |
| `/version` | Which version of sourcewatch is running |

## Game servers — `/server`

- **Add:** `/server add address:1.2.3.4:27015`. A host name works too, and the port defaults to 27015.
- **Remove:** `/server remove address:…` and pick the game server from the list that appears.
- In a Discord server you need the **Manage Server** permission. In a DM with the bot you manage your own list.
- Each Discord server (and each DM) can watch up to **3** game servers.
- Only IPv4 game servers can be watched, and private and local addresses are refused.

## What is going on — `/status`

`/status` has no options. It shows every game server you watch: online or offline, players, map, how fresh the data is, and the names of the players who are online. The reply is **public**, so everyone in the channel sees it, which makes it good for answering "is anyone on?". Players on the Discord server's blacklist are left out of the names.

If a server blocks direct queries, sourcewatch falls back to GameTracker (an admin can turn that off with `/admin settings gametracker`). The line then says **Via GameTracker, scanned N min ago**, and that data can be a few minutes old.

## Notifications — `/notify`

Each notification fires **once, when the thing becomes true**. After that it stays quiet until the thing stops being true and happens again. Making a notification never fires it for something that is already true.

| Command | Fires when |
|---|---|
| `/notify map map-name:de_dust2` | The server changes to that map. |
| `/notify player username:Alice` | That player joins. The name must match exactly, not case-sensitive. |
| `/notify playercount value:>=16` | The number of players matches the comparison (below). |
| `/notify status value:offline` | The server goes `offline`, or comes back `online`. |
| `/notify title-query value:event` | The server name gets that word, not case-sensitive, like `[EVENT]` in the name. |

**Player count comparisons.** The `value` of `/notify playercount` is a number from 0 to 64, with an optional comparison in front:

| Value | Means |
|---|---|
| `>=10` | at least 10 |
| `>10` | more than 10 |
| `<=5` | at most 5 |
| `<5` | fewer than 5 |
| `=20` | exactly 20 |
| `!=3` | anything but 3 |
| `10` | a plain number means at least 10 |

When a notification is saved, the bot replies **Notification saved as ID: 12**. Keep that number. The numbers are your own: your notifications are numbered 1, 2, 3… separately in each Discord server and in DMs, whatever other people have. When you delete one its number is free again, and the next one takes your lowest free number.

**Options (optional).** `server` limits it to one game server. If you leave it out, you get a single notification for **all servers**: one ID, listed under **All servers**, that covers every game server you watch, including ones you add later. It keeps its own state and cooldown for each game server. `id` is explained below.

## Where it arrives, and how often

A new notification arrives in the channel where you made it (or by DM if you made it in a DM with the bot), and has a cooldown of **60 mins** after it fires. An admin can change both defaults for a Discord server (see below). Change either with `/notify settings`: `delivery` (**In this channel** or **By DM**) and `cooldown` (minutes).

For DMs the bot sends you a test message first. If Discord blocks it, open a DM with the bot or allow DMs from server members, then try again. If a notification cannot be delivered later, because Discord refuses the DM or the bot can no longer post in the channel (it was deleted, or the bot lost its permissions there), it is paused, and you are told the next time you use `/notify`.

## Combine conditions with `id`

Add `id:12` to any `/notify` type to put that condition into notification **12** instead of making a new one. **All** of its conditions must be true together (up to 5, maps are the exception, see below), and it fires once when they all become true.

Example:

1. `/notify map map-name:de_dust2` is saved as ID 12.
2. `/notify playercount value:>=10 id:12` makes it fire only when the map is de_dust2 **and** there are at least 10 players.

A combined notification keeps its game server, delivery and cooldown.

**AND and OR.** Conditions of different types are always **AND**: all of them must hold. Two types follow a fixed rule inside one notification:

- Several **players** are **AND**. Every one of those players must be online.
- Several **maps** are **OR**. The server being on any one of those maps is enough.

So `{de_dust2 or de_inferno} AND {Alice and Bob}` fires only when the server is on one of those two maps and both Alice and Bob are online. Moving from one of the maps to another keeps it true, so it does not fire again until it has stopped being true. The other types are plain AND.

If you have no notification with the `id` you give, sourcewatch makes a new one with that number instead of refusing. The reply says so.

## Manage your notifications

- `/notify list` shows your notifications to you, grouped by server, with **All servers** first. Each reads like `(via channel, cooldown 60 mins)`.
- `/notify show` is the same list, shown to **everyone** in the channel.
- `/notify remove id:12` deletes one.
- `/notify settings` shows or changes how they behave:
  - `id` is one notification's number. Leave it out to mean **all** of yours.
  - `active` is **Enabled** or **Disabled**. A disabled one never fires.
  - `cooldown` is the minutes to stay quiet.
  - `delivery` is **In this channel** (the channel you run it in) or **By DM**.
- `/notify reset` deletes **all** your notifications here. It asks first.

Examples:

- `/notify settings active:Disabled` pauses all of yours.
- `/notify settings id:12 cooldown:60` changes just one.
- `/notify settings` on its own shows how many are on or off.

## Admin settings — `/admin settings`

For people with **Manage Server**, in a Discord server. The settings are for that Discord server only. With no options, `/admin settings` shows the current ones.

| Option | What it does |
|---|---|
| `channel-delivery` | **enabled** (default) or **disabled**. When disabled, notifications are never posted in channels and go by DM instead. |
| `default-delivery` | **channel** (default) or **dm**: how a new notification is delivered. |
| `default-cooldown` | Minutes: the cooldown of a new notification (default 60). |
| `active` | **enabled** (default) or **disabled**. When disabled, sourcewatch is completely off in this Discord server: no notifications are sent and every command except `/admin` says so. DMs with the bot are not affected. |
| `gametracker` | **enabled** (default) or **disabled**. When disabled, the game servers this Discord server watches are only ever queried directly and never looked up on GameTracker. They are declared offline after a few failed tries, and GameTracker data is not used here. |
| `blacklist-add` | A player name nobody here can track. |
| `blacklist-remove` | Take a name off the blacklist. |

The default delivery and cooldown only apply to notifications made afterwards. Each person can still change their own with `/notify settings`.

**Blacklist.** A blacklisted player, say someone who does not want to be tracked, is matched exactly (not case-sensitive). Nobody in that Discord server can make a `player` notification for the name, notifications that already track it stop firing, and `/status` leaves it out of the player list. It does not affect other Discord servers.

## Good to know

- You can have up to **25** notifications in total. Each Discord server and each DM has its own list of notifications.
- sourcewatch checks each server about once a minute, so very short events can be missed.
- A server that is offline only matches `status` notifications.
- If several people have the same notification in a channel and it fires at once, one message mentions all of them, like **@Kritz @Player2 @Player3**. DMs are always one person each.
- Names of players and servers come straight from the game server and can look odd. sourcewatch shows them in `code style` so links and @mentions in names cannot ping anyone.

## What sourcewatch stores

Your Discord user ID and your notification settings (delete them with `/notify remove` or `/notify reset`), each Discord server's admin settings and blacklist, and the players, maps and server names it sees on watched servers over time. That history is kept and cannot be deleted with these commands.

Something not working? Ask an admin.

## Run your own sourcewatch

This part is for whoever runs the bot. sourcewatch is a single program for Linux. It keeps its data in one SQLite file, needs no database server, and opens no port: it only connects out, to Discord and to the game servers.

### The Discord application

1. In the [Discord Developer Portal](https://discord.com/developers/applications), make a **New Application**. Its **Application ID** (on **General Information**) goes in `application_id`.
2. On **Bot**, use **Reset Token** and copy the token. It goes in `discord_token` and is a secret: anyone who has it controls the bot. sourcewatch needs none of the privileged gateway intents.
3. Invite the bot to a Discord server with this link, after putting your application ID in it:
   `https://discord.com/oauth2/authorize?client_id=APPLICATION_ID&scope=bot+applications.commands&permissions=3072`
   It asks for **View Channels** and **Send Messages**, which channel notifications need. If people use `/notify` in threads, also give it **Send Messages in Threads**.

The commands are registered at every start, for all Discord servers and for DMs with the bot. A new or changed command can take a moment to show up in Discord.

### Install

On a Debian server:

1. Get the program. Either download `sourcewatch-linux-amd64` (or `-arm64`) from the [releases](../../releases) and rename it `sourcewatch`, or build it from a checkout of this repository with Go 1.26: `CGO_ENABLED=0 go build -o sourcewatch ./cmd/sourcewatch`.
2. In a checkout of this repository, which has the example config and the service file, install the program, a user to run it as, the config and the service:

<!-- install step 1 -->
```sh
sudo install -m 755 sourcewatch /usr/local/bin/sourcewatch
sudo useradd --system --no-create-home --shell /usr/sbin/nologin sourcewatch
sudo install -d -m 750 -g sourcewatch /etc/sourcewatch
sudo install -m 600 -o sourcewatch -g sourcewatch config.example.json /etc/sourcewatch/config.json
sudo install -m 644 deploy/sourcewatch.service /etc/systemd/system/sourcewatch.service
```

3. Put the bot token and the application ID in the config with `sudoedit /etc/sourcewatch/config.json` (see [Configuration](#configuration)). The file must keep mode `600`, or sourcewatch refuses to start.
4. Start it, now and at every boot:

<!-- install step 2 -->
```sh
sudo systemctl daemon-reload
sudo systemctl enable --now sourcewatch
systemctl status sourcewatch --no-pager
```

The service runs as the `sourcewatch` user and keeps its database in `/var/lib/sourcewatch`. It starts again 5 seconds after a crash or a failed connection to Discord.

### Configuration

sourcewatch reads the JSON file named by `SOURCEWATCH_CONFIG`, which must have mode `600`. Then the `SOURCEWATCH_*` variables below replace what the file says. The bot token is a secret: keep it in the file (or in the environment), never in the repository.

| In the file | Variable | What it is |
|---|---|---|
| | `SOURCEWATCH_CONFIG` | The path of the config file. Without it, only the variables are read. |
| `discord_token` | `SOURCEWATCH_DISCORD_TOKEN` | The bot token (secret). Without one, sourcewatch only polls and stores, with no Discord bot. |
| `application_id` | | The application ID. When empty, the bot's own user ID is used, which is normally the same. |
| `servers` | `SOURCEWATCH_SERVERS` | Game servers to poll even when nobody in Discord watches them, as `host:port`. The variable takes them comma-separated. These are trusted, so a server on your own network works; servers added with `/server add` must have a public address. |
| `database` | `SOURCEWATCH_DATABASE` | The SQLite file. Default: `sourcewatch.db` in the working directory, which is `/var/lib/sourcewatch` under the service. |
| `disable_gametracker` | | `true` turns off the GameTracker fallback for servers that block queries. Default: `false`. |
| `guild_id` | | Only to clean up after old versions, which registered the commands for one Discord server: its ID removes that copy at startup. Leave it empty otherwise. |
| `webhook_url` | `SOURCEWATCH_WEBHOOK_URL` | Not used. The bot posts its messages itself. |

sourcewatch needs at least a token or a server to start. Variables named `PAWNZ_*`, from before the project was renamed, are still read, and an existing `pawnz.db` is kept in use.

### Running it

- **Logs**: `journalctl -u sourcewatch -f`. Each start logs the version and the settings, with the secrets left out.
- **Check it**: `/status` in a Discord server where the bot is, after `/server add`.
- **Upgrade**: replace `/usr/local/bin/sourcewatch` and run `sudo systemctl restart sourcewatch`. The database is updated at startup by itself.
- **Back up**: the database is `/var/lib/sourcewatch/sourcewatch.db`. Copy it while the service is stopped, or while it runs with `sqlite3 /var/lib/sourcewatch/sourcewatch.db ".backup /path/to/backup.db"`.
- **Without systemd**: `SOURCEWATCH_DISCORD_TOKEN=… ./sourcewatch` works too. It stops cleanly on Ctrl+C or SIGTERM.
