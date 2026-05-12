# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
go build ./...          # build all packages
go run ircflu.go        # run the bot
go test ./...           # run all tests
go test ./msgsystem/catserver/...  # run a single package's tests
```

Run the bot (minimum flags):
```bash
go run ircflu.go -irchost irc.example.org:6667 -ircnick mybot -ircchannel "#mychannel"
```

## Architecture

The app is structured around two central channels in `msgsystem`:
- `CommandsIn` — inbound messages from IRC/Jabber/etc. flow here; **Command** handlers read from it
- `MessagesOut` — outbound messages are sent here; subsystems consume and deliver them

**Subsystems** (`msgsystem/MsgSubSystem`) handle transport (IRC, Jabber, catserver, web). Each registers itself via `msgsystem.RegisterSubSystem()` in its `init()`. They receive messages from `MessagesOut` via `Handle()` and inject incoming messages into `CommandsIn`.

**Commands** (`commands/Command`) implement bot behaviour. Each registers via `commands.RegisterCommand()` in its `init()`, is opted-in via the `-commands` flag, and processes messages by implementing `Parse(msg)`.

**Registration is by blank import** — `ircflu.go` imports subsystems and commands with `_` so their `init()` functions fire. Adding a new subsystem or command requires a blank import there.

**`app.CliFlag`** is the mechanism all packages use to declare their own CLI flags; flags are registered during `init()` and parsed once by `app.Run()` before subsystems start.

**Web hooks** (`msgsystem/web/hooks/`) register HTTP handlers via `http.HandleFunc` in their `init()`. The web subsystem runs `http.ListenAndServe` on `-webaddr`.

## Trivia subsystem (branch: trivia)

The trivia feature lives in `commands/trivia/` and is registered as a command. It:
- Parses a trivia file with `Category/Question/Answer/Regexp` blocks
- Picks a random question and sends it to the IRC channel via `MessagesOut`
- Waits for `Parse()` calls with channel messages, checks answers (exact match or regexp), and sends correct/incorrect feedback
- Tracks per-nick scores in memory
