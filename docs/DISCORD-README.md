<!-- Message 1 of 5: paste as its own Discord message -->
# 📣 sourcewatch — game server alerts

sourcewatch watches Source game servers (CS:S, CS2, TF2 and more) and tells you when something happens: a map change, a player joining, the server filling up, going offline, or its name changing.

## Quick start
1️⃣ An admin adds a game server: `/server add address:1.2.3.4:27015`
2️⃣ Anyone makes a notification: `/notify map`, `player`, `playercount`, `status` or `title-query`
3️⃣ Show your servers and who is online: `/status`

Only you see the bot's replies, except `/status` and `/notify show`, which everyone in the channel sees. Type `/help` for a short reminder.

## 🖥️ Game servers — `/server`
• `/server add address:…` (a host name works too, the port defaults to 27015) and `/server remove address:…`
• In a Discord server you need **Manage Server**. In a DM with the bot you manage your own list.
• Up to **3** game servers per Discord server (and per DM). IPv4 only, no private or local addresses.

## 📊 `/status`
Every game server you watch: online or offline, players, map, and who is online. Blacklisted players are left out.

<!-- Message 2 of 5: paste as its own Discord message -->
## 🔔 Notifications — `/notify <type>`
Each fires **once, when the thing becomes true**, then stays quiet until it stops being true and happens again. Making one never fires it for something already true.

**`/notify map map-name:de_dust2`** — the server changes to that map
**`/notify player username:Alice`** — that player joins (exact name, not case-sensitive)
**`/notify playercount value:>=16`** — the number of players: `>=10` at least · `>10` more than · `<=5` at most · `<5` fewer than · `=20` exactly · `!=3` anything but. A plain `10` means at least 10 (0 to 64). Bots and SourceTV are not counted.
**`/notify status value:offline`** — the server goes `offline`, or comes back `online`
**`/notify title-query value:event`** — the server name gets that word, like `[EVENT]`

The reply says **Notification saved as ID: 12**. IDs are your own (1, 2, 3…), and a deleted one's number is reused.

**Options:** `server` limits it to one game server. Without it you get **one** notification for **all servers**, including ones added later.

## 🔗 Combine with `id`
`id:12` adds the condition to notification 12 instead of making a new one (up to **8**). All must hold, and it fires once when they all become true.
`/notify map map-name:de_dust2` → ID 12
`/notify playercount value:>=10 id:12` → fires only on de_dust2 **and** 10+ players
**Players are AND** (all must be online), **maps are OR** (any one), different types AND. An `id` you don't have makes a new notification with that number.

<!-- Message 3 of 5: paste as its own Discord message -->
## 📬 Delivery and cooldown
A new notification arrives in the channel where you made it (or by DM if made in a DM), with a **60 min** cooldown after it fires. For DMs the bot sends a test message first; if Discord blocks it, open a DM with the bot or allow DMs from server members. If a notification can't be delivered later (DM refused, or the bot lost access to the channel) it is paused and you are told the next time you use `/notify`.

## 🗂️ Manage — `/notify …`
• `list` — yours, grouped by server · `show` — the same, for everyone in the channel
• `remove id:12` — delete one · `reset` — delete **all** yours here (asks first)
• `settings` — `id` (one number; leave out for **all** yours), `active` (Enabled/Disabled), `cooldown` (minutes), `delivery` (In this channel / By DM)
  e.g. `/notify settings active:Disabled` pauses all · `/notify settings id:12 cooldown:30`

## 🕒 Schedule — `/notify schedule`
Only let notifications fire at certain times: `/notify schedule time:20:00-23:30 day:0000011`
• `time` is **UTC**, `HH:MM-HH:MM` (may pass midnight, like `22:00-02:00`). `time:off` removes it. The reply shows the window as Discord timestamps in **your own timezone**.
• `day` is 7 digits, Monday first: `0000011` = Sat+Sun, `1111100` = weekdays. Leave out for every day.
• No `id` = all your notifications. With `id:12` = just that one, and it **overrides** the one for all.
• Outside the window nothing fires. When it opens, a notification waits for its conditions to become true again.

<!-- Message 4 of 5: paste as its own Discord message -->
## 🎭 Role notifications — `/notify-role`
For people with **Manage Server**. Same subcommands as `/notify` (types, `list`, `show`, `remove`, `settings`, `schedule`, `reset`), but the alert **pings a role** and the notifications are **shared by the whole Discord server**.
`/notify-role map map-name:de_dust2 role:@Players`
• `role` is **required** on every type. With `id` it must be the role that notification already has.
• One numbering for the server; any manager can list, change or remove any of them. Your own `/notify` ones are separate.
• Always posted in a channel (a role can't get a DM). Up to **25** per Discord server.
• Only that role is pinged. `@everyone` is refused, and `@here`/`@everyone` in game-server names never ping. Discord only pings a role that is **mentionable**, or if the bot has **Mention Everyone**.
• `/notify-role settings` is the **server-level** `/notify settings`. Without `id`, for all role notifications: `cooldown` and `delivery` (the channel you run it in) are applied to existing ones **and** kept for new ones; `active` switches them all on/off. With `id` it changes just one. No options shows the server's settings.

<!-- Message 5 of 5: paste as its own Discord message -->
## 🛡️ Admins — `/admin settings`
Needs **Manage Server**; for this Discord server only. No options shows the current ones.
• `channel-delivery` — enabled (default) or disabled (notifications then go by DM)
• `default-delivery` — `channel` (default) or `dm`, for new notifications
• `default-cooldown` — minutes for new notifications (default 60)
• `active` — disabled turns sourcewatch off here: every command except `/admin` says so. DMs aren't affected.
• `blacklist-add` / `blacklist-remove` — a player name nobody here can track: no `player` notification can be made for it, existing ones stop firing, and `/status` hides it.
Defaults apply to notifications made afterwards.

## ℹ️ Good to know
• Up to **25** personal notifications; each Discord server and each DM has its own list.
• Servers are checked about once a minute, so very short events can be missed. An offline server only matches `status`.
• If several people have the same notification in a channel and it fires at once, one message mentions all of them. DMs are one person each.
• Names from game servers can look odd; they are shown in `code style` so they can't ping anyone.

## 🔒 What is stored
Your Discord user ID and notification settings (delete with `/notify remove` or `reset`), each server's admin settings and blacklist, and the players, maps and server names seen on watched servers over time. That history is kept and can't be deleted with these commands.

Something not working? Ask an admin.
