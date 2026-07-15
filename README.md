# zot-rtk — zot extension for Rust Token Killer

Automatically routes bash commands through [rtk](https://github.com/rtk-ai/rtk),
reducing token consumption by 60–90% for 100+ commands: git, ls, cat, grep,
cargo, pytest, docker, kubectl, aws, and many more.

## How it works

- **Bash interception** — when the LLM calls `bash` with a command, `rtk`
  decides via `rtk rewrite` whether to rewrite the invocation. If yes — the
  command is automatically replaced with `rtk <command>`. Unsupported commands
  pass through unchanged.
- **`rtk` tool** — the LLM can explicitly call `rtk` for compact output.
- **`/rtk` command** — manual invocation from chat.
- **Auto-install** — if `rtk` is not on the system, the latest release is
  downloaded from GitHub for the system architecture.

## Installation

```bash
zot ext install ./zot-rtk
```

Or run ad-hoc:

```bash
zot --ext ./zot-rtk
```

## Structure

```
zot-rtk/
├── extension.json    # manifest
├── main.go           # source code
├── go.mod            # Go module
└── README.md
```

## Build

```bash
cd zot-rtk && go build -o zot-rtk .
```

## Requirements

- Go 1.22+ (to build)
- zot 0.2.x
- Linux/macOS/Windows
