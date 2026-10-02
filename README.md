# ERPNext Hardware Bridge

Kleiner Dienst für Linux und Windows, der Hardware am Arbeitsplatz (Waage,
später Kamera, Scanner, Kartenterminal, Drucker) dem ERPNext-Desk im Browser
über einen lokalen WebSocket bereitstellt.

```
Browser (ERPNext Desk) ──ws://localhost:8735/ws──▶ Bridge ──RS-232/USB──▶ Waage
Frappe-Server ──https (Reverse Proxy)──▶ Bridge :5000/weight   (Übergang)
```

Konzept und Entscheidungen: [docs/KONZEPT.md](docs/KONZEPT.md).

## Stand

| Phase | Inhalt | Stand |
|---|---|---|
| 0 | Dienst, Konfiguration, Oberfläche, WS-API dual-stack, Origin-/Host-/CSRF-Schutz | fertig |
| 1 | Waage PCE-PB (Live-Gewicht, Lesen, Tara), `/weight` im Flask-Format, Browser-Client | fertig |
| 2 | Kamera (libgphoto2, nur Linux) | offen – bis dahin reicht die Bridge `/shot` und `/health` an die Flask-App weiter |
| 3 | Serieller Scanner, Terminal-Tunnel | offen |
| 4 | Druckmodul: Labels roh (ZPL) auf den Drucker der Station | gebaut; Netzwerkdrucker und Gerätedatei getestet, Betriebssystem-Warteschlange (CUPS, Windows-Spooler) nur kompiliert |

## Schnellstart

Fertige Binaries liegen unter **Releases** im GitHub-Repo
(`erpnext-hardware-bridge-<version>-linux-arm64`, `…-windows-amd64.exe` usw.).
Ein neues Release entsteht automatisch mit jedem Tag:

```sh
git tag -a v0.2.0 -m "…" && git push origin v0.2.0
```

Selbst bauen geht auch:

```sh
scripts/build.sh                                   # Binaries nach dist/
sudo scripts/install-linux.sh dist/erpnext-hardware-bridge-<version>-linux-arm64
```

Danach auf **demselben Rechner** `http://localhost:8735/` öffnen und unter
»Konfiguration« einrichten: erlaubte Origins (z.B. `https://erp.holzschuhe.at`),
Waage hinzufügen, Port wählen, speichern, »Testen«. Änderungen wirken sofort,
ohne Neustart (außer beim Port der WS-API).

Windows: Binary nach `C:\Program Files\ERPNextHardwareBridge\` kopieren und
in einer Administrator-Konsole `erpnext-hardware-bridge.exe -service install`
und `-service start` ausführen. Konfiguration liegt unter
`%ProgramData%\ERPNextHardwareBridge\bridge.yaml`.

Ohne Dienst zum Ausprobieren:

```sh
go run ./cmd/bridge -config ./bridge.yaml
```

## Konfiguration

`bridge.yaml` wird von der Oberfläche geschrieben (atomar, vorherige Fassung
als `.bak`). Von Hand geht auch, Kommentare überleben das nächste Speichern
aus der Oberfläche aber nicht.

```yaml
station: parcel-station-1
listen_port: 8735                 # bindet immer 127.0.0.1 und ::1
allowed_origins:
  - https://erp.holzschuhe.at
devices:
  - id: waage
    kind: scale
    driver: pce_pb
    port: /dev/waage              # oder COM5, oder:
    # port: {match: {vid: "067b", pid: "2303", serial: "…"}}
    baud: 9600
    poll_ms: 250                  # Takt, solange jemand zusieht; sonst alle 5 s
    stable_samples: 4             # so viele Werte innerhalb der Toleranz = stabil
    stable_tolerance_kg: 0.005
  - id: labeldrucker
    kind: printer
    driver: raw_tcp               # Netzwerkdrucker; oder raw_file / system
    address: 192.168.1.60:9100
    # port: /dev/usb/lp0          # raw_file: Gerätedatei
    # queue: Zebra_ZD421          # system: Drucker des Betriebssystems, roh
http_compat:
  enabled: true
  listen: ":5000"                 # IPv4 und IPv6
  basic_auth: {user: …, password: …}   # leer = Reverse Proxy authentifiziert
  legacy_upstream: http://127.0.0.1:5001  # alles außer /weight, z.B. Flask für /shot
```

## Umstieg von der Flask-App (Linux-Box)

1. In `app.py` die letzte Zeile auf `app.run(host="127.0.0.1", port=5001)`
   ändern und Flask neu starten. Die Kamera läuft dort unverändert weiter.
2. Bridge installieren, `http_compat` aktivieren mit `listen: ":5000"` und
   `legacy_upstream: http://127.0.0.1:5001`. Der Reverse Proxy bleibt, wie er ist.
3. Prüfen: `curl -u … https://cameraserver.holzschuhe.at/weight` liefert
   dasselbe JSON wie vorher, `/health` kommt weiter von Flask.

Die Flask-Route `/weight` wird danach nie mehr aufgerufen; Flask öffnet die
Waage nur bei diesem Aufruf, es gibt also keinen Portkonflikt.

**Live-Gewicht braucht die Bridge an dem PC, an dem der Browser läuft.**
Hängt die Waage an einer eigenen Linux-Box und die Parcel Station läuft auf
einem anderen PC, liefert die Box weiter `/weight` für den Server, aber der
Browser erreicht sie nicht über `localhost`. Für das Live-Gewicht die Waage
an den Parcel-Station-PC hängen und dort die Bridge installieren.

## WebSocket-Protokoll (Kurzfassung)

```jsonc
→ {"id":1,"type":"req","method":"hello","params":{"client":"parcel_station","protocol":1}}
← {"id":1,"type":"res","ok":true,"result":{"version":"…","station":"…","devices":[…]}}
→ {"id":2,"type":"req","method":"scale.subscribe"}            // ohne device: erste Waage
← {"type":"event","event":"scale.weight","device":"waage","data":{"kg":1.56,"raw":"+  1.56kg","stable":true,"unit":"kg","ts":"…"}}
← {"type":"event","event":"device.state","device":"waage","data":{"kind":"scale","state":"offline","message":"…"}}
```

Methoden: `hello`, `devices.list`, `bridge.info`, `scale.subscribe`,
`scale.unsubscribe`, `scale.read`, `scale.tare`, `printer.print`,
`printer.test`. Fehlercodes: `device_missing`, `scale_offline`, `timeout`,
`bad_response`, `busy`, `unauthorized`, `protocol_mismatch`,
`unknown_method`, `unsupported_format`, `print_failed`, `too_large`,
`bad_request`, `unexpected`.

### Drucken

```
→ {"id":3,"type":"req","method":"printer.print","params":{"device":"labeldrucker","format":"zpl","title":"SHIPMENT-00150","data":"<base64>"}}
← {"id":3,"type":"res","ok":true,"result":{"device":"labeldrucker","bytes":22759,"title":"SHIPMENT-00150"}}
```

Die Bridge reicht die Daten unverändert an den Drucker weiter; sie rendert
nichts und wandelt nichts um. Der Drucker muss die Sprache selbst verstehen
(ZPL). Anderes als `zpl`/`raw` lehnt sie mit `unsupported_format` ab – ein
PDF druckt das Desk über den Dialog des Browsers. Ohne `device` nimmt sie
den ersten Drucker der Station. Hintergrund in
[docs/KONZEPT.md](docs/KONZEPT.md), Abschnitt 16.

Hinweis zu Chrome: Der Zugriff einer https-Seite auf `localhost` braucht die
Freigabe »Geräte im lokalen Netzwerk« (einmalige Abfrage des Browsers oder
Richtlinie `LocalNetworkAccessAllowedForUrls`). Ohne sie meldet das Desk
»keine Bridge«.

Im Browser nicht selbst implementieren, sondern [client/hwbridge.js](client/hwbridge.js)
verwenden (wird in die Apps kopiert).

## Sicherheit

- WS-API, Oberfläche und JSON-API binden nur Loopback (127.0.0.1 und ::1).
- WebSocket nur von `allowed_origins`, exakt verglichen.
- Host-Header muss `localhost`/`127.0.0.1`/`[::1]` sein (Schutz vor DNS-Rebinding).
- Schreibende API-Aufrufe brauchen das CSRF-Token der Oberfläche und
  `Sec-Fetch-Site: same-origin`.
- `bridge.yaml` wird mit Rechten 0600 geschrieben (enthält Passwörter).
- Einziger Listener im Netz ist die HTTP-Kompat-Route; sie kennt nur
  `/weight` und die Weiterleitung an den Legacy-Upstream.

## Entwicklung

```sh
go test ./...
python3 testdata/fake_pce_scale.py /tmp/waage     # simulierte Waage an einem PTY
go run ./cmd/bridge -config ./bridge.yaml -log-file -
```

Unterstützte Ziele: Linux amd64/arm64/armv7 und Windows amd64, alle ohne cgo.
macOS wird nicht gebaut (die Port-Erkennung der Seriell-Bibliothek braucht
dort cgo).
