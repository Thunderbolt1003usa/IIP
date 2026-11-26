BINARY  := iip
PREFIX  ?= /usr/local

.PHONY: build test vet install uninstall clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o $(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

# Binary + systemd-Unit installieren. Vorhandene Konfiguration unter /etc/iip
# wird NICHT überschrieben.
install: build
	install -Dm755 $(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)
	install -Dm644 contrib/iip.service $(DESTDIR)/etc/systemd/system/iip.service
	test -e $(DESTDIR)/etc/iip/iip.conf || install -Dm644 examples/iip.conf $(DESTDIR)/etc/iip/iip.conf
	test -e $(DESTDIR)/etc/iip/iip.json || install -Dm644 examples/iip.json $(DESTDIR)/etc/iip/iip.json
	@echo "Fertig. Danach: systemctl daemon-reload && systemctl enable --now iip"

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/$(BINARY) $(DESTDIR)/etc/systemd/system/iip.service
	@echo "Konfiguration unter /etc/iip bleibt erhalten."

clean:
	rm -f $(BINARY)
