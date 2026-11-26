# IIP – Internet Informations Protokoll

Ein bewusst winziges UDP-Protokoll, mit dem ein Client **öffentliche, nicht vertrauliche Infos** von einem Server abfragen kann: IP-Adressen, Organisation, öffentlich erreichbare Dienste und Ähnliches.

Typischer Anwendungsfall: Ein Client kennt nur die IPv4-Adresse eines Servers und möchte darüber dessen IPv6-Adresse oder laufende Game-Server herausfinden.

Eine einzige Go-Binary ist gleichzeitig **Server** und **Client**. Sie hat keine Abhängigkeiten außerhalb der Go-Standardbibliothek.

```
$ iip 203.0.113.10
hostname: linuxmc.tech
ipv4: 203.0.113.10
ipv6: 2001:db8::1
organisation: Beispiel Org
services:
  - name: tf2-server
    protocol: udp
    port: 27015
  - name: hugetube
    protocol: https
    port: 443
updated: 2026-09-20T00:00:00Z
```

## Protokoll

| Richtung | Inhalt |
|---|---|
| Client → Server | Ein UDP-Datagramm mit dem Text `GET` (Groß-/Kleinschreibung egal, Leerzeichen und Zeilenumbruch am Rand werden ignoriert) |
| Server → Client | Ein UDP-Datagramm mit dem Inhalt der `iip.json` (UTF-8, gültiges JSON) |

Alles andere beantwortet der Server nicht, er verwirft es stillschweigend. Es gibt keine Sitzungen, keine Authentifizierung und keine Verschlüsselung: Die Daten sind für die Öffentlichkeit gedacht.

Der Standardport ist **65535/UDP** und über die Konfiguration änderbar.

Ohne eigenen Client geht es auch mit netcat:

```sh
printf 'GET' | nc -u -w1 203.0.113.10 65535
```

## Installation

Voraussetzung: Go 1.22 oder neuer.

```sh
git clone <dein-repo> iip
cd iip
make build          # erzeugt ./iip (statisch gelinkt, CGO_ENABLED=0)
sudo make install   # Binary, systemd-Unit und Beispiel-Config nach /etc/iip
sudo systemctl daemon-reload
sudo systemctl enable --now iip
```

`make install` überschreibt eine vorhandene Konfiguration unter `/etc/iip` nicht. Ohne `make` geht es auch von Hand:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o iip .
```

## Konfiguration

Zwei Dateien unter `/etc/iip/`:

- `iip.conf` enthält die Einstellungen des Daemons
- `iip.json` enthält die Daten, die ausgeliefert werden

### iip.conf

Einfaches `key = value`-Format, Kommentare beginnen mit `#`. Beispiel: [`examples/iip.conf`](examples/iip.conf)

| Schlüssel | Standard | Bedeutung |
|---|---|---|
| `listen_addr` | `0.0.0.0` | Adresse, an die der Server bindet (siehe unten zu IPv4/IPv6) |
| `listen_port` | `65535` | UDP-Port |
| `data_file` | `/etc/iip/iip.json` | Datei, die als Antwort geschickt wird |
| `rate_limit` | `5` | Erlaubte Anfragen pro Sekunde und Quell-IP |
| `rate_limit_burst` | `10` | Burst-Kapazität pro Quell-IP |
| `max_response_size` | `8192` | Maximale Größe der `iip.json` in Bytes. Größere Dateien werden beim Laden abgelehnt |

### iip.json

Beliebiges, gültiges JSON. Der Server schickt die Datei unverändert zurück, die Struktur legst du also selbst fest. Beispiel: [`examples/iip.json`](examples/iip.json)

```json
{
  "hostname": "linuxmc.tech",
  "ipv4": "203.0.113.10",
  "ipv6": "2001:db8::1",
  "organisation": "Beispiel Org",
  "services": [
    { "name": "tf2-server", "protocol": "udp", "port": 27015 }
  ]
}
```

Ungültiges JSON oder eine zu große Datei wird beim Laden abgelehnt. Beim Start bricht der Server dann ab, bei einem Reload bleiben die zuletzt gültigen Daten aktiv.

### Reload ohne Neustart

```sh
sudo systemctl reload iip     # sendet SIGHUP
```

Ein Reload liest die `iip.json` neu ein und wertet dabei `data_file` und `max_response_size` aus. `listen_addr`, `listen_port` und die Rate-Limit-Werte werden nur beim Start gelesen, dafür ist ein Neustart nötig.

### IPv4 und IPv6

`listen_addr` legt die Adressfamilie fest:

| `listen_addr` | Verhalten |
|---|---|
| `0.0.0.0` | nur IPv4, alle Interfaces |
| `::` | nur IPv6, alle Interfaces |
| konkrete Adresse | nur diese Adresse (Familie ergibt sich aus der Adresse) |

Für IPv4 und IPv6 gleichzeitig startest du zwei Instanzen mit je einer eigenen Config (z. B. `iip-v4.conf` und `iip-v6.conf`).

## Client

Dieselbe Binary dient als Client. Ein erstes Argument ohne `/` wird als Zieladresse verstanden (Hostnamen und IP-Adressen enthalten nie einen Schrägstrich, Dateipfade schon):

```sh
iip 2001:db8::1                 # lesbare Ausgabe, Port 65535
iip 2001:db8::1 12345           # anderer Port
iip 203.0.113.10 -json          # rohe JSON-Antwort
iip 203.0.113.10 12345 -json    # Port und Rohausgabe kombiniert
```

- Ohne `-json` wird die Antwort als `key: value` ausgegeben, in der Feldreihenfolge der `iip.json`. Verschachtelte Objekte werden eingerückt, Listen als `- `-Einträge dargestellt.
- Mit `-json` kommt die Antwort byteweise so, wie der Server sie geschickt hat.
- Der Client wartet 3 Sekunden auf eine Antwort.
- Bei Hostnamen mit A- und AAAA-Eintrag bevorzugt der Client IPv4. Für IPv6 gibst du die Adresse direkt an.

## Server starten (ohne systemd)

```sh
iip                       # liest /etc/iip/iip.conf
iip /pfad/zur/iip.conf    # eigene Config (jedes Argument mit "/" gilt als Pfad)
iip ./iip.conf            # relative Pfade brauchen ein "./"
```

## systemd

Die Unit [`contrib/iip.service`](contrib/iip.service) startet den Daemon als unprivilegierten `DynamicUser` mit Hardening (`ProtectSystem=strict`, `NoNewPrivileges` und weiteren Optionen). Die Dateien unter `/etc/iip` müssen für alle lesbar sein (`chmod 644`), weil der Dienst unter einem temporären Benutzer läuft.

Für Ports unter 1024 (z. B. 53 oder 1) braucht der Dienst `CAP_NET_BIND_SERVICE`. In der Unit stehen die dafür nötigen Zeilen auskommentiert bereit.

## Sicherheit

Der Server antwortet ohne Authentifizierung auf jedes passende UDP-Paket. Daraus folgt:

- **Reflection/Amplification:** Die Quelladresse von UDP-Paketen lässt sich fälschen, die Antwort geht dann an ein Opfer. Eine Anfrage hat nur 3 Bytes, die Antwort so viele wie deine `iip.json`. Das Rate-Limit pro Quell-IP begrenzt, wie viel Verkehr du für eine einzelne fremde Adresse erzeugst, es verhindert das Problem aber nicht vollständig. Halte die `iip.json` klein (möglichst unter etwa 1400 Bytes, dann passt sie in ein Paket ohne Fragmentierung) und überlege, den Port per Firewall einzuschränken, wenn der Dienst nicht für alle erreichbar sein muss.
- **Keine Vertraulichkeit:** Lege nichts in die `iip.json`, was nicht jeder lesen darf.
- **Keine Integrität:** Es gibt weder Signatur noch TLS. Ein Angreifer auf dem Netzwerkpfad kann Antworten fälschen. Verlasse dich nicht auf IIP für Entscheidungen, bei denen eine gefälschte Adresse Schaden anrichten würde.

## Hinweise zum Port

65535 liegt im Bereich der Dynamic/Private Ports (49152–65535), die IANA grundsätzlich nicht vergibt. IIP ist deshalb nicht bei IANA registriert und kann es auf diesem Port auch nicht werden. Für den privaten Einsatz spielt das keine Rolle. Soll das Protokoll einmal offiziell registriert werden, braucht es einen Port im Bereich der User Ports (1024–49151).

## Entwicklung

```sh
make test     # Unit-Tests (Config-Parser, Ausgabeformat, Rate-Limiter, Datenladen)
make vet      # go vet
```

examples/            Beispiel-Config und Beispiel-Daten
contrib/iip.service  systemd-Unit
Makefile             build / test / install
```
