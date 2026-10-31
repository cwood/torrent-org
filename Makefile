PREFIX  ?= $(HOME)/.local
BINDIR  ?= $(PREFIX)/bin
DATADIR ?= $(PREFIX)/share
UNITDIR ?= $(HOME)/.config/systemd/user

BIN := bin/torrent-org

.PHONY: build test lint generate install uninstall enable disable

build:
	go build -o $(BIN) ./cmd/torrent-org

test:
	go test ./...

lint:
	go vet ./...
	go tool golangci-lint run ./...
	go tool deadcode -test ./...

generate:
	go generate ./...

# install copies the binary, desktop entry, icon and systemd user unit, and
# restarts the service if it is already running so it picks up the new build.
install: build
	install -Dm755 $(BIN) $(DESTDIR)$(BINDIR)/torrent-org
	install -d $(DESTDIR)$(DATADIR)/applications $(DESTDIR)$(UNITDIR)
	sed 's|@BINDIR@|$(BINDIR)|g' packaging/torrent-org.desktop.in > $(DESTDIR)$(DATADIR)/applications/torrent-org.desktop
	sed 's|@BINDIR@|$(BINDIR)|g' packaging/torrent-org.service.in > $(DESTDIR)$(UNITDIR)/torrent-org.service
	install -Dm644 internal/web/static/icon.svg $(DESTDIR)$(DATADIR)/icons/hicolor/scalable/apps/torrent-org.svg
	-update-desktop-database $(DESTDIR)$(DATADIR)/applications
	-systemctl --user daemon-reload
	-systemctl --user try-restart torrent-org.service

# enable makes torrent-org the handler for .torrent files and starts the
# service at login. Kept separate from install because it changes defaults.
enable:
	xdg-mime default torrent-org.desktop application/x-bittorrent
	systemctl --user enable --now torrent-org.service

disable:
	-systemctl --user disable --now torrent-org.service

uninstall: disable
	rm -f $(DESTDIR)$(BINDIR)/torrent-org
	rm -f $(DESTDIR)$(DATADIR)/applications/torrent-org.desktop
	rm -f $(DESTDIR)$(DATADIR)/icons/hicolor/scalable/apps/torrent-org.svg
	rm -f $(DESTDIR)$(UNITDIR)/torrent-org.service
	-update-desktop-database $(DESTDIR)$(DATADIR)/applications
	-systemctl --user daemon-reload
