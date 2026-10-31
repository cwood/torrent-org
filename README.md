# torrent-org

A rules engine for [Transmission](https://transmissionbt.com/), modeled on the
Groups feature of the macOS client that the Linux and Windows clients lack.
Rules match torrents by name, tracker or file extension and decide where their
files go, so downloads land in a layout like `~/torrents/<tracker>/<type>/`.

It runs as a small local web service alongside Transmission and talks to it
over RPC:

- **Rules** in priority order, first match wins, each with a name (emoji
  welcome), colour and destination such as `{root}/{tracker}/audio`.
- **Add flow**: open a `.torrent` and preview the rule it matches, pick which
  files to download, or write a new rule on the spot.
- **File association**: double-clicking a `.torrent` opens that preview.
- **Live list** of downloading and completed torrents grouped by rule, with
  speeds, ETAs and a "Show in folder" button.
- **Moves**: saving a rule offers to move the torrents it now matches.
  Torrents are stopped, moved, verified and restarted.

Everything, including the UI, is a single binary with an embedded SQLite
database.

## Requirements

- Transmission 4.0 or newer with RPC enabled (labels need RPC version 16).
- Go 1.27+ to build from source.
- Linux for `make install`. The binary also runs on macOS.

## Install

```sh
make install   # binary to ~/.local/bin, desktop entry, icon, systemd user unit
make enable    # handle .torrent files and start the service at login
```

`make uninstall` reverses both. Prebuilt binaries are attached to each
[release](https://github.com/cwood/torrent-org/releases).

Then open <http://127.0.0.1:9092>, or run `torrent-org open` to open it in
your browser. `torrent-org open FILE.torrent` previews a file.

## Configure

Everything has a default, so the config file is optional. It lives at
`~/.config/torrent-org/config.yaml`:

```yaml
listen: 127.0.0.1:9092
root: ~/torrents            # where rules place downloads, as Transmission sees it
database: ~/.local/share/torrent-org/torrent-org.db
poll_interval: 3s
transmission:
  url: http://localhost:9091/transmission/rpc
  username: ""
  password: ""              # or set TORRENT_ORG_TRANSMISSION_PASSWORD
```

Each setting can also be passed as a flag; see `torrent-org -h`.

On its first run, torrent-org records the torrents already in Transmission
where they are. After that, every new torrent is placed by the rules.

## Running Transmission headless

torrent-org works with any Transmission, but since it is the UI, the daemon
is a good fit. Run it as your own user so it can write to your home
directory, in `~/.config/systemd/user/transmission-daemon.service`:

```ini
[Unit]
Description=Transmission BitTorrent daemon
After=network-online.target

[Service]
Type=notify
ExecStart=/usr/bin/transmission-daemon -f --log-level=info
ExecReload=/bin/kill -s HUP $MAINPID
Restart=on-failure

[Install]
WantedBy=default.target
```

Then run `systemctl --user enable --now transmission-daemon`.

## Development

```sh
make test       # go test ./...
make lint       # go vet, golangci-lint, deadcode
make generate   # sqlc and mockery
```

## License

MIT
