<!-- Message 1 of 7: paste as its own Discord message -->
# 📣 sourcewatch — game server alerts

sourcewatch watches Source game servers (CS:S, CS2, TF2 and more) and tells you when something happens: a map change, a player joining, the server filling up, going offline, or its name changing.

## Quick start
1️⃣ An admin adds a game server: `/server add address:1.2.3.4:27015`
2️⃣ Anyone makes a notification: `/notify map`, `/notify player`, `/notify playercount`, `/notify status` or `/notify title-query`
3️⃣ Show your servers and who is online any time: `/status`

Only you can see the bot's replies to your commands, except `/status` and `/notify show`, which everyone in the channel sees. Alerts arrive in the channel where you made the notification (mentioning only you), or in your DMs if you choose that.

Type `/help` for a short reminder.

<!-- Message 2 of 7: paste as its own Discord message -->
## 🖥️ Game servers — `/server`
• **Add:** `/server add address:1.2.3.4:27015`. A host name works too, and the port defaults to 27015.
• **Remove:** `/server remove address:…` and pick the game server from the list that appears.
• In a Discord server you need the **Manage Server** permission. In a DM with the bot you manage your own list.
• Each Discord server (and each DM) can watch up to **3** game servers.
• Only IPv4 game servers can be watched, and private and local addresses are refused.

## 📊 What is going on — `/status`
`/status` has no options. It shows every game server you watch: online or offline, players, map, how fresh the data is, and the names of the players who are online. The reply is **public**, so everyone in the channel sees it. Players on the server's blacklist are left out.
If a server blocks direct queries, sourcewatch falls back to GameTracker. The line then says **Via GameTracker, scanned N min ago**, and that data can be a few minutes old.

<!-- Message 3 of 7: paste as its own Discord message -->
## 🔔 Notifications — `/notify <type>`
Each one fires **once, when the thing becomes true**. After that it stays quiet until the thing stops being true and happens again. Making a notification never fires it for something that is already true.

**`/notify map map-name:de_dust2`**
The server changes to that map.

**`/notify player username:Alice`**
That player joins. The name must match exactly, not case-sensitive.

**`/notify playercount value:>=16`**
The number of players, with a comparison:
`>=10` at least · `>10` more than · `<=5` at most · `<5` fewer than · `=20` exactly · `!=3` anything but
A plain number such as `10` means at least 10. Allowed: 0 to 64.

**`/notify status value:offline`**
The server goes `offline`, or comes back `online`.

**`/notify title-query value:event`**
The server name gets that word, not case-sensitive, like `[EVENT]` in the name.

When it is saved the bot replies **Notification saved as ID: 12**. Keep that number. A deleted notification's number is free again, and the next one takes the lowest free number.

<!-- Message 4 of 7: paste as its own Discord message -->
## ⚙️ Options (optional)
• `server` — limit it to one game server. Leave it out and you get **one** notification for **all servers**: a single ID, listed under **All servers**, that covers every game server you watch, including ones you add later.
• `id` — see below

## 📬 Where it arrives, and how often
A new notification arrives in the channel where you made it (or by DM if you made it in a DM with the bot), and has a cooldown of **60 mins** after it fires. An admin can change both defaults (see the admin message). Change either for one notification with `/notify settings`: `delivery` (**In this channel** or **By DM**) and `cooldown` (minutes). For DMs the bot sends you a test message first. If Discord blocks it, open a DM with the bot or allow DMs from server members, then try again. If a notification cannot be delivered later, because Discord refuses the DM or the bot can no longer post in the channel (it was deleted, or the bot lost its permissions there), it is paused, and you are told the next time you use `/notify`.

## 🔗 Combine conditions with `id`
Add `id:12` to any `/notify` type to put that condition into notification **12** instead of making a new one. **All** of its conditions must be true together (up to 5, maps are the exception, see below), and it fires once when they all become true.

Example:
`/notify map map-name:de_dust2` → saved as ID 12
`/notify playercount value:>=10 id:12` → now it only fires when the map is de_dust2 **and** there are at least 10 players

A combined notification keeps its game server, delivery and cooldown.
**Players are AND:** every player you add must be online. **Maps are OR:** any one of the maps is enough. Different types are always AND, so `{map a or map b} AND {player 1 and player 2}`.

<!-- Message 5 of 7: paste as its own Discord message -->
## 🗂️ Manage your notifications
• `/notify list` — your notifications, grouped by server, **All servers** first. Each reads like `(via channel, cooldown 60 mins)`.
• `/notify show` — the same list, shown to **everyone** in the channel
• `/notify remove id:12` — delete one
• `/notify settings` — see or change how they behave:
  – `id` — one notification's number. Leave it out to mean **all** of yours.
  – `active` — **Enabled** or **Disabled**. A disabled one never fires.
  – `cooldown` — minutes to stay quiet
  – `delivery` — **In this channel** (the channel you run it in) or **By DM**
• `/notify reset` — delete **all** your notifications here. It asks first.

Examples:
`/notify settings active:Disabled` pauses all of yours
`/notify settings id:12 cooldown:60` changes just one
`/notify settings` on its own shows how many are on or off

<!-- Message 6 of 7: paste as its own Discord message -->
## 🛡️ For admins — `/admin settings`
Needs **Manage Server**, in a Discord server. The settings are for that Discord server only. With no options it shows the current ones.
• `channel-delivery` — **enabled** (default) or **disabled**. When disabled, notifications go by DM, never in a channel.
• `default-delivery` — **channel** (default) or **dm**, for new notifications.
• `default-cooldown` — minutes for new notifications (default 60).
• `active` — **enabled** (default) or **disabled**. When disabled, sourcewatch is completely off in this Discord server and every command except `/admin` says so. DMs with the bot are not affected.
• `gametracker` — **enabled** (default) or **disabled**. When disabled, game servers here are only queried directly and never looked up on GameTracker.
• `blacklist-add` / `blacklist-remove` — a player name nobody here can track. For example someone who does not want to be tracked: nobody can make a `player` notification for the name, notifications that already track it stop firing, and `/status` leaves it out. It does not affect other Discord servers.

The default delivery and cooldown only apply to notifications made afterwards. Everyone can still change their own with `/notify settings`.

<!-- Message 7 of 7: paste as its own Discord message -->
## ℹ️ Good to know
• You can have up to **25** notifications in total. Each Discord server and each DM has its own list of notifications.
• sourcewatch checks each server about once a minute, so very short events can be missed.
• A server that is offline only matches `status` notifications.
• If several people have the same notification in a channel and it fires at once, one message mentions all of them, like **@Kritz @Player2 @Player3**. DMs are always one person each.
• Names of players and servers come straight from the game server and can look odd. sourcewatch shows them in `code style` so links and @mentions in names cannot ping anyone.

## 🔒 What sourcewatch stores
Your Discord user ID and your notification settings (delete them with `/notify remove` or `/notify reset`), each Discord server's admin settings and blacklist, and the players, maps and server names it sees on watched servers over time. That history is kept and cannot be deleted with these commands.

Something not working? Ask an admin.
