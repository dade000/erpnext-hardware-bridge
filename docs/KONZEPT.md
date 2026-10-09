# ERPNext Hardware Bridge (Go) – Konzept

Stand: 2026-09-24 (Rev. 3: Konfiguration über Bridge-Oberfläche, Desk verbindet oder weicht aus, Tunnel ohne Config, IPv4/IPv6, Tunnel-Härtung). Ergebnis des Interviews vom selben Tag; Entscheidungen sind
in Abschnitt 2 festgehalten, offene Punkte in Abschnitt 13.

## 1. Ziel

Eine kleine Go-Anwendung (»Bridge«), die auf dem Rechner läuft, an dem die
Hardware hängt, und diese Hardware dem ERPNext-Desk (und der POS-Kasse) im
Browser über einen lokalen WebSocket zur Verfügung stellt. Sie ersetzt die
Flask-App `app.py` (Kamera + Waage), die heute auf der Linux-Box hinter dem
Reverse Proxy `cameraserver.holzschuhe.at` läuft.

Erster Meilenstein: **Live-Gewicht der PCE-PB 60N in der Parcel Station.**

Später: Kamera (Photo Station), serielle Scanner, WebSocket-Tunnel zum
Worldline-Terminal (POS), Arbeitsplatzdrucker für künftige Funktionen.

### 1.1 Abgrenzung zu bestehenden Lösungen

| Lösung | Deckt ab | Warum nicht |
|---|---|---|
| QZ Tray (Java, localhost-WS) | Drucker, Serial, HID, USB | Java-Laufzeit auf jeder Station; stiller Betrieb nur mit kostenpflichtig signiertem Zertifikat; keine Kamera, kein Tunnel |
| Web Serial / WebUSB im Browser | Waage, Scanner (nur Chromium) | Kein Firefox; keine gphoto2-Kamera; Terminal-Tunnel unmöglich (Mixed Content bleibt) |
| PrintNode u.ä. | Drucken, cloud-vermittelt | Nur Drucken; das ist mit CUPS bereits gelöst |

Eigenbau, weil zwei Bausteine nirgends fertig existieren: die Kamera mit
Dauer-Session (Verhalten aus `app.py`) und der lokale Tunnel zum
Worldline-Terminal. Beides zusammen mit Waage und Scanner in einem Binary
ohne Laufzeitumgebung liefert keine der Lösungen.

## 2. Entscheidungen aus dem Interview

| Thema | Entscheidung |
|---|---|
| Topologie | **Browser → ws://127.0.0.1** . Die Bridge läuft am Stations-PC, das Desk-JS verbindet sich lokal. Kein Reverse Proxy für den Live-Pfad. |
| Plattform | OS-neutral bauen (Linux + Windows), Linux ist die bevorzugte Zielplattform. Heute läuft Flask auf einer eigenen Linux-Box. |
| Waage | PCE-PB 60N, Polling per `Sx` reicht. |
| Kamera | Bleibt Linux (libgphoto2). Windows-Build ohne Kamera-Modul. |
| Sicherheit | Bind nur 127.0.0.1, Origin-Allowlist der ERPNext-/POS-Sites. Kein Pairing-Token (Begründung in 7.1); Token bleibt als optionale Härtung vorgesehen. |
| Drucken | Ursprünglich: CUPS-Serverdruck bleibt, Druckmodul später. **Geändert am 2026-10-01**, siehe Abschnitt 16: Die Bridge druckt Labels roh (ZPL) auf einen Drucker der Station; CUPS bleibt als zweiter Weg, PDF über den Browser-Dialog als Notlösung. |
| POS-Terminal | Bridge leitet den WebSocket 1:1 an das Worldline-Terminal weiter (ersetzt den heutigen nginx-Umweg). Tunnel ohne Config: Kasse öffnet ihn zur Laufzeit mit Ziel aus ERPNext, Bridge vergibt den lokalen Port. |
| Konfigurationsquelle | Lokale Datei `bridge.yaml` am Stations-PC, gepflegt über die **Konfigurationsoberfläche der Bridge** auf dem eigenen Webport. ERPNext hält keine Stations- oder Gerätekonfiguration. |
| Übergang | Die Bridge bedient zusätzlich die alten HTTP-Routen `/weight`, `/shot`, `/health` mit Basic-Auth, der Reverse Proxy zeigt auf die Bridge statt auf Flask. Bis zur Kamera-Portierung (Phase 2) reicht die Bridge alles außer `/weight` an Flask weiter (`legacy_upstream`), danach wird Flask abgeschaltet. |
| Scanner | Serielle Scanner über die Bridge, HID-Tastaturmodus bleibt parallel als Fallback. |
| Betrieb | Ein Binary pro OS/Arch, läuft als Dienst (systemd / Windows-Dienst), Status- und Konfigurationsoberfläche unter http://127.0.0.1:PORT/. Selbst-Update nicht in Phase 1. |
| Netz | **IPv4 und IPv6 durchgängig**: Loopback-Bind auf 127.0.0.1 und ::1, Tunnelziele und HTTP-Kompat dual-stack, Zielprüfung für beide Familien. |
| Desk-Verhalten | **Verbinden oder ausweichen.** Das Desk konfiguriert nichts, es versucht die Verbindung zur Bridge. Gelingt sie nicht oder fehlt das Gerät, zeigt es eine Meldung und bietet die Alternative an: Waage → Gewicht selbst eingeben, Kamera → Foto hochladen, Scanner → Tastatur, Terminal → Kartenzahlung nicht anbieten. |

## 3. Ist-Zustand (was die Bridge ablöst)

* `app.py` (Flask, Python, gphoto2 + pyserial) auf der Linux-Box:
  `GET /shot[?preview=1]`, `GET /health`, `GET|POST /reset`, `GET /weight`.
  Erreichbar über `https://cameraserver.holzschuhe.at` mit Basic-Auth.
* **Waage**: Frappe-Server ruft `read_scale_weight()`
  (`erpnext_parcel_station/parcel/api/scale.py`) auf, das per `requests`
  gegen `/weight` geht. Aufrufer: der Button im Shipment-Formular
  (`public/js/shipment.js`) und serverseitig `_apply_scale_weight()` in
  `parcel/api/core.py` beim Anlegen der Sendung. Zugangsdaten stehen im
  Single-DocType **Scale Settings**.
* **Kamera**: `capture_item_photo_server()` in
  `holzschuherzeugung_devich/photo_station/api.py` holt das JPEG serverseitig
  von `/shot` und legt es in MinIO ab. Daneben existiert bereits
  `save_browser_captured_photo()` für im Browser aufgenommene Bilder.
* **Terminal**: Die POS-Kasse (Next.js) nutzt Worldline TIM API JS. Die
  Bibliothek baut ihre URL fest als `ws://IP:PORT/SIXml`. Weil die Kasse per
  HTTPS läuft, blockt der Browser `ws://` zu einer LAN-IP (Mixed Content),
  deshalb hängt heute ein nginx dazwischen, der `wss://` annimmt und an das
  Terminal weiterreicht.
* **Drucken**: Server → CUPS hinter Reverse Proxy (`cups_print.py`). Bleibt.

## 4. Zielarchitektur

```
Stations-PC (Linux bevorzugt, Windows möglich)
┌──────────────────────────────────────────────────────────────────┐
│ Browser                                                          │
│  ERPNext Desk (Parcel Station, Photo Station)   POS (Next.js)    │
│        │ ws://127.0.0.1:8735/ws                    │ ws://127.0.0.1:<port>/SIXml
│        ▼                                           ▼             │
│ ┌──────────────────────────── Bridge (Go) ─────────────────────┐ │
│ │ WS-API (JSON)   HTTP-Kompat (/weight /shot /health, Basic)   │ │
│ │ Statusseite     Tunnel (WS→WS, WS→TCP)                       │ │
│ │ ── Treiber ──────────────────────────────────────────────── │ │
│ │  scale.pce_pb  camera.gphoto2 (linux)  scanner.serial  print │ │
│ └──────┬──────────────┬────────────────────┬──────────────┬────┘ │
│        │ RS-232/USB   │ USB/PTP            │ USB-CDC      │ LAN   │
└────────┼──────────────┼────────────────────┼──────────────┼──────┘
     PCE-PB 60N     Canon DSLR           Scanner     Worldline-Terminal
                                                     Drucker (später)

Frappe-Server ──https (Reverse Proxy)──▶ Bridge:/weight, /shot   (Übergang)
```

Grundsätze:

* **Ein Prozess besitzt die Hardware.** Serielle Ports und die Kamera werden
  von genau einer Bridge exklusiv geöffnet; Flask muss vor dem ersten Start
  der Bridge gestoppt sein.
* **Der Browser ist der Client.** Live-Daten (Gewicht, Scans) kommen als
  Events über den WebSocket, Aktionen (Foto, Tara) als Request/Response.
* **Der Frappe-Server bleibt hardwarefrei**, bis auf die Übergangsroute
  `/weight` und `/shot`, die schrittweise abgebaut wird.
* **Dual-Stack von Anfang an.** Jeder Loopback-Listener (WS-API,
  Oberfläche, Tunnel) bindet 127.0.0.1 **und** ::1 auf demselben Port. Der
  Browser-Client spricht `localhost` an, damit das Betriebssystem die
  Familie wählt; auf manchen Windows-Installationen löst `localhost` nur
  noch nach ::1 auf, ein reiner IPv4-Bind wäre dort unsichtbar. Die
  HTTP-Kompat-Route bindet `:5000` (Go bindet damit beide Familien). Ziele
  von Tunneln dürfen IPv4, IPv6-Literale in eckigen Klammern oder Hostnamen
  sein; der Dial läuft dual-stack (Go-Standard, Happy Eyeballs).
* **Warum `ws://localhost` von einer HTTPS-Seite geht:** Chrome und Firefox
  behandeln `localhost`, `127.0.0.1` und `[::1]` als vertrauenswürdigen
  Ursprung und nehmen sie vom Mixed-Content-Blocking aus. Kein TLS, kein Zertifikat auf
  den Stations-PCs. (Punkt 13.1 zum Nachweis auf den echten Kassen-PCs.)

## 5. Go-Projekt

Repo: `erpnext_hardware_bridge` (dieses Verzeichnis). Modul `github.com/<org>/erpnext-hardware-bridge`.

```
cmd/bridge/main.go            Einstieg, Flags: -config, -service install|uninstall|run
internal/config/              bridge.yaml laden/validieren, Geräte-Matching
internal/server/              HTTP-Mux: /ws, /status, /healthz, /weight, /shot, /health
internal/ws/                  Verbindungs-Handling, Envelope, Subscriptions, Broadcast
internal/auth/                Origin-Allowlist, Token, Basic-Auth (Kompat)
internal/device/              Treiber-Interface, Registry, Lebenszyklus, Reconnect
internal/device/scale/pce/    PCE-PB N Protokoll (Sx, ST), Polling, Stabil-Erkennung
internal/device/camera/gphoto/ cgo-Bindung libgphoto2, Build-Tag `camera` (nur Linux)
internal/device/scanner/serial/
internal/tunnel/              WS→WS und WS→TCP Weiterleitung
internal/status/              Statusseite (html/template), Log-Ringpuffer
client/hwbridge.js            Browser-Client (eine Quelle, wird in die Frappe-Apps kopiert)
deploy/systemd/bridge.service, deploy/udev/99-bridge.rules, deploy/windows/
```

Bibliotheken (alle ohne cgo außer Kamera):

| Zweck | Wahl | Grund |
|---|---|---|
| Seriell | `go.bug.st/serial` | Reines Go, Linux/Windows/macOS, Enumeration mit VID/PID/Seriennummer |
| WebSocket | `github.com/coder/websocket` (ehem. nhooyr) | Klein, Context-basiert, keine Abhängigkeiten |
| Dienst | `github.com/kardianos/service` | systemd + Windows-Dienst aus einem Binary (`-service install`) |
| Config | `gopkg.in/yaml.v3` | |
| Kamera | eigene dünne cgo-Bindung auf `libgphoto2` | Die vorhandenen Go-Bindungen sind unbetreut; wir brauchen ~10 Funktionen (init, exit, get/set_config, trigger_capture, wait_for_event, file_get, capture_preview, get_summary). Build-Tag `camera`, Windows-Build ohne. |
| Logging | `log/slog` | Standardbibliothek, JSON- oder Textausgabe |

Treiber-Interface (vereinfacht):

```go
type Device interface {
    ID() string
    Kind() string                 // "scale", "camera", "scanner"
    Start(ctx context.Context) error
    Stop() error
    State() State                 // online/offline/error + letzter Fehler
    Handle(ctx context.Context, req Request) (any, error)   // Kommandos
    Events() <-chan Event         // Live-Daten
}
```

Jeder Treiber läuft in einer eigenen Goroutine mit Reconnect-Schleife
(Backoff 1 s → 30 s). Ein ausgestecktes Kabel führt zu Zustand `offline` und
einem `device.state`-Event, nie zum Absturz des Prozesses.

Builds: `linux/amd64`, `linux/arm64` (mit `camera`, cgo, in Docker mit
`libgphoto2-dev`), `windows/amd64` (ohne `camera`, `CGO_ENABLED=0`).
Versionsnummer per `-ldflags -X`.

## 6. Konfiguration (`bridge.yaml`)

Quelle der Wahrheit ist `bridge.yaml` neben dem Binary (Linux:
`/etc/erpnext-bridge/bridge.yaml`, Windows: `%ProgramData%\ERPNextBridge\bridge.yaml`).
Gepflegt wird sie über die Oberfläche der Bridge (Abschnitt 12), von Hand
editierbar bleibt sie trotzdem.

```yaml
station: parcel-station-1          # nur Anzeigename (Statusseite, Logs)
listen_port: 8735                   # immer auf 127.0.0.1 UND ::1; keine Adresse wählbar
allowed_origins:
  - https://erp.holzschuhe.at
  - https://erp-dev.holzschuhe.at
  - https://pos.holzschuhe.at
# token: "…"                        # optional, siehe 7.1

devices:
  - id: waage
    kind: scale
    driver: pce_pb
    port:                           # entweder path ODER match
      path: /dev/waage              # Linux: udev-Symlink wie bisher; Windows: COM5
      # match: {vid: "067b", pid: "2303", serial: "…"}  # plattformneutral
    baud: 9600
    poll_ms: 250
    stable_samples: 4               # gleich viele identische Messungen = stabil
    stable_tolerance_kg: 0.005

  - id: kamera
    kind: camera
    driver: gphoto2
    port: usb:001,005               # optional, sonst auto
    keepalive_sec: 30

  - id: scanner1
    kind: scanner
    driver: serial_line
    port: { match: { vid: "05e0", pid: "1200" } }
    baud: 115200
    terminator: "\r"

  - id: labeldrucker                # Abschnitt 16
    kind: printer
    driver: raw_tcp                 # raw_tcp | raw_file | system
    address: 192.168.1.60:9100      # raw_tcp
    # port: /dev/usb/lp0            # raw_file
    # queue: Zebra_ZD421            # system: Name des Druckers im Betriebssystem

http_compat:                        # Übergang für den Frappe-Server
  enabled: true
  listen: 0.0.0.0:5000              # dahinter der bestehende Reverse Proxy
  basic_auth: { user: "…", password: "…" }
```

`port.match` löst das Portproblem plattformneutral: die Bridge sucht per
`serial.GetDetailedPortsList()` das Gerät mit passender VID/PID/Seriennummer.
Auf Linux bleibt der udev-Symlink `/dev/waage` weiter nutzbar.

Tunnel (z.B. zum Terminal) tauchen in der Config nicht auf, sie werden zur
Laufzeit per `tunnel.open` angelegt (Abschnitt 11).

Für die WS-API ist nur der Port konfigurierbar, nie die Adresse: Sie
bindet immer beide Loopbacks. So kann niemand die API versehentlich auf
0.0.0.0 oder :: legen.

Der Port 8735 ist eine feste Konvention.
Das Desk kennt ihn als Konstante; so braucht ERPNext keine
Stationskonfiguration. Wer ihn ändert, muss es an beiden Enden tun, deshalb
warnt die Oberfläche beim Ändern.

## 7. WebSocket-Protokoll

Endpunkt `ws://127.0.0.1:8735/ws`. Textframes mit JSON, Binärframes nur für
Bilddaten. Drei Nachrichtentypen:

```jsonc
// Client → Bridge
{"id": 7, "type": "req", "method": "scale.subscribe", "params": {"device": "waage"}}
// Bridge → Client (Antwort auf id)
{"id": 7, "type": "res", "ok": true, "result": {…}}
{"id": 7, "type": "res", "ok": false, "error": {"code": "scale_offline", "message": "…"}}
// Bridge → Client (ohne id)
{"type": "event", "event": "scale.weight", "device": "waage", "data": {…}}
```

Handshake: erste Nachricht des Clients ist `hello` mit `client`
(`"parcel_station"`, `"photo_station"`, `"pos"`) und `protocol: 1`, optional
`token`. Antwort enthält Bridge-Version, Stationsname, die Geräteliste mit
Zustand. Ohne `hello` innerhalb
von 3 s wird die Verbindung geschlossen. Die Origin-Prüfung passiert schon
beim HTTP-Upgrade.

### 7.1 Warum ohne Pairing-Token

Das Desk soll nichts konfigurieren, also gibt es keinen Ort, an dem es einen
stationsspezifischen Token herbekäme. Die Schutzwirkung der beiden Maßnahmen:

* **Origin-Allowlist** hält fremde Websites im selben Browser ab. Das ist der
  eigentliche Angriffsweg bei einem localhost-Dienst.
* **Token** würde zusätzlich lokale Prozesse am Stations-PC abhalten. Ein
  solcher Prozess kann aber ohnehin den seriellen Port und die Kamera selbst
  öffnen; der Token schützt hier praktisch nichts.

Falls später doch gewünscht, ist ein **site-weiter** Token vorgesehen: ein
Single-DocType in ERPNext liefert ihn an eingeloggte Benutzer, in jeder
Bridge wird derselbe Wert eingetragen. Das bleibt ohne Stationswahl im Desk
machbar. Der Handshake trägt das Feld deshalb schon jetzt optional.

Methoden und Events:

| Methode / Event | Richtung | Inhalt |
|---|---|---|
| `hello` | req | Token, Client, Protokollversion → Geräteliste |
| `devices.list` | req | Alle Geräte mit Kind, Treiber, Zustand |
| `device.state` | event | `{state: online\|offline\|error, message}` bei jeder Änderung |
| `scale.subscribe` / `scale.unsubscribe` | req | Live-Stream ein/aus (pro Verbindung) |
| `scale.weight` | event | `{kg: 1.56, raw: "+     1.56kg", stable: true, ts: "…"}` |
| `scale.read` | req | Eine frische Messung (Timeout 2 s) |
| `scale.tare` | req | Sendet `ST` |
| `camera.capture` | req | `{preview: false}` → JSON-Antwort mit `{bytes, mime, seq}` gefolgt von **einem Binärframe** mit dem JPEG |
| `camera.reset` | req | Session schließen und neu öffnen |
| `printer.print` | req | `{device?, format: "zpl"\|"pdf", data: "<base64>", title?}` → `{device, bytes, title, format}`; die Daten gehen unverändert an den Drucker (Abschnitte 16, 17). Ohne `device`: Standarddrucker des Arbeitsplatzes für das Format |
| `printer.test` | req | Druckt ein eigenes Testetikett |
| `scan.event` | event | `{code: "…", device: "scanner1", ts: "…"}` |
| `tunnel.open` | req | `{target: "ws://192.168.1.50:80", subprotocols: ["SIXml"]}` → `{port: 41235}`; die Bridge öffnet einen lokalen Listener auf einem freien Port, gebunden an diese WS-Verbindung |
| `tunnel.close` | req | Listener schließen (passiert automatisch, wenn die WS-Verbindung endet) |
| `tunnel.state` | event | `{port, target, reachable: true\|false}` |
| `bridge.info` | req | Version, Uptime, Plattform |

Fehlercodes spiegeln die heutigen Codes in `scale.py`, damit die Desk-Texte
weiterverwendet werden können: `not_configured`, `scale_offline`, `timeout`,
`bad_response`, `busy`, `unauthorized`, `unknown_device`, `unexpected`.

Polling nur bei Bedarf: Die Waage wird abgefragt, solange mindestens ein
Abonnent verbunden ist oder eine HTTP-Kompat-Anfrage läuft. Ohne Abonnenten
ein Lebenszeichen alle 5 s, damit `device.state` stimmt.

## 8. Waage PCE-PB 60N (Phase 1, Kern)

Protokoll (Handbuch PCE-PB N, siehe Quellen): RS-232, 9600 Baud, 8N1 (wie in
`app.py`); `Sx` + CR LF liefert die aktuelle Anzeige, `ST` + CR LF tariert.
Antwortformat wie heute beobachtet: `+     1.56kg`. Der Treiber parst
Vorzeichen, Zahl und Einheit und rechnet `g` nach `kg` um, falls die Waage
umgestellt ist.

Stabil-Erkennung in Software: `stable = true`, wenn die letzten
`stable_samples` Messungen innerhalb `stable_tolerance_kg` liegen. Die Waage
liefert im `Sx`-Format keinen Stabil-Marker; falls das Handbuch einen
Stabil-Befehl bietet, kann der Treiber später darauf umstellen, das Event
bleibt gleich.

Fehlerfälle: Port nicht vorhanden → `offline` + Reconnect; keine Antwort
innerhalb 2 s → `timeout` (Event `device.state: error`), nach 3 Timeouts Port
schließen und neu öffnen; unparsbare Zeile → verwerfen, Zähler auf der
Statusseite.

HTTP-Kompat `GET /weight`: liefert exakt das Flask-Format
`{"status":"success","weight_kg":1.56,"raw":"…"}` bzw. HTTP 500 mit
`{"status":"error","message":"…"}`. Intern eine frische `scale.read`, kein
Cache, damit das Verhalten für `_apply_scale_weight()` identisch bleibt.

## 9. Desk-Integration (ERPNext-Seite)

Leitlinie: **Verbinden, sonst ausweichen.** Das Desk hält keine
Stationskonfiguration. Es versucht beim Öffnen einer Hardware-Seite die
Verbindung zu `ws://localhost:8735/ws`; was daraus wird, entscheidet die
Anzeige.

**Client `hwbridge.js`** (Quelle im Go-Repo unter `client/`, wird nach
`holzschuherzeugung_devich/public/js/` kopiert und über `app_include_js`
geladen): Verbindung mit kurzem Timeout (1,5 s), danach Reconnect im
Hintergrund mit Backoff bis 30 s, Request/Response als Promises,
Event-Emitter. Zustände, die jede Seite abfragen kann:

| Zustand | Bedeutung | Anzeige im Desk |
|---|---|---|
| `no_bridge` | Verbindung zu localhost:8735 kommt nicht zustande | »Auf diesem PC läuft keine Hardware Bridge« + Alternative |
| `device_missing` | Bridge läuft, Gerät ist nicht konfiguriert | »Waage an dieser Station nicht eingerichtet« + Link `http://127.0.0.1:8735/` + Alternative |
| `device_offline` | Gerät konfiguriert, aber nicht erreichbar (Kabel, Strom) | »Waage nicht verbunden« + Alternative, wechselt automatisch zurück, sobald das Gerät da ist |
| `ready` | Gerät liefert | Live-Anzeige |

Die drei Fehlzustände führen zur selben Alternative, aber zu verschiedenen
Texten. Der Unterschied spart Fehlersuche: »keine Bridge« heißt Dienst
starten, »nicht eingerichtet« heißt Oberfläche öffnen, »nicht verbunden«
heißt Kabel prüfen.

```js
const hw = frappe.hwbridge;            // global, eine Verbindung pro Tab
hw.on("state", s => …);                // no_bridge | connecting | ready
hw.on("scale.weight", d => …);
await hw.call("scale.subscribe", {device: "waage"});
const {blob} = await hw.call("camera.capture", {});
```

Kommt die Bridge später hinzu (Dienst gestartet, Waage eingesteckt), schaltet
die Seite ohne Neuladen von der Alternative auf den Live-Modus um.

Alternativen je Gerät:

* **Waage → selbst eingeben.** Die Parcel Station zeigt statt der
  Live-Anzeige ein Gewichtsfeld, vorbelegt mit dem bisherigen Fallback aus
  den Artikelgewichten, und einen Hinweisbanner. Wichtig: Das Ausweichen darf
  **nicht still** passieren, ein Paket mit Artikel-Rechengewicht statt
  Wiegegewicht ist heute schon ein bekanntes Risiko (siehe die Warnung
  »silently offline scale« im Parcel-Station-JS). Deshalb muss der Bediener
  das Gewicht im Ausweichmodus bestätigen, bevor die Sendung angelegt wird.
* **Kamera → Foto hochladen.** Photo Station bietet Dateiupload bzw. den
  bestehenden `getUserMedia`-Pfad; beide landen in
  `save_browser_captured_photo`.
* **Scanner → Tastatur.** Der HID-Pfad bleibt ohnehin bestehen; ohne Bridge
  ändert sich nichts. Mit Bridge kommen Scan-Events zusätzlich herein.
* **Terminal → Kartenzahlung nicht anbieten.** Die Kasse prüft beim Start,
  ob der Tunnel steht und die Bridge das Terminal erreicht (Feld im
  `hello` bzw. `device.state` für Tunnel). Fehlt eines, blendet sie die
  Kartenzahlung aus und zeigt im Kopf »Terminal nicht verfügbar«, damit das
  Personal es bemerkt und nicht nur der Kunde. Der POS ist eine eigene
  Next.js-App und bekommt denselben Client aus `client/` als Modul.

Gewicht in die Sendung übernehmen:

* `create_shipment_from_barcode` bekommt optional `client_weight_kg` und
  `client_weight_source` (`scale` | `manual`). `_apply_scale_weight()` nimmt
  dieses Gewicht, wenn es vorhanden ist, und fällt sonst wie bisher auf die
  HTTP-Route zurück. Damit ist der Serverpfad ab Tag 1 nur noch Fallback, die
  Quelle wird am Shipment protokolliert.
* Der Button »Gewicht von Waage« im Shipment-Formular nutzt zuerst die
  Bridge (`scale.read`) und erst ohne Verbindung den bisherigen Serveraufruf.

In ERPNext entsteht damit **kein neuer DocType**. `Scale Settings` bleibt
für die HTTP-Kompat-Route bestehen, bis sie abgebaut wird.

## 10. Kamera (Phase 2, Linux)

Portierung der bewährten Logik aus `app.py`, Verhalten unverändert:

* Session dauerhaft offen halten, Keepalive per `get_summary`, bei Fehler
  Session schließen und neu öffnen.
* Beim Öffnen: `capturetarget` → RAM, kleines JPEG, JPEG statt RAW, EVF aus.
* Capture: Event-Queue leeren, `trigger_capture`, bis 3 s `FILE_ADDED`
  sammeln, JPEG bevorzugen, sonst Preview des ersten Files; Download; bei
  Fehler `-105` einmal Session neu aufbauen und wiederholen.
* Preview ohne Auslösen über `capture_preview`.
* Mutex: parallele Captures → `busy` (wie heute 503).
* HTTP-Kompat: `GET /shot[?preview=1]` liefert `image/jpeg`, `GET /health`
  liefert das heutige JSON (`session_open`, `reachable`, `model`, `port`,
  `libgphoto2`), `/reset` wie gehabt.

Build nur mit Tag `camera`; ohne Tag registriert die Bridge das Gerät nicht
und meldet auf der Statusseite »Kamera in diesem Build nicht enthalten«.

## 11. Weitere Module

**Scanner (Phase 3)**: Treiber `serial_line` liest zeilenweise vom
USB-CDC-Port und sendet `scan.event`. Im Desk gibt es eine gemeinsame
Scan-Quelle: Bridge-Events und der bestehende HID-Pfad münden in denselben
Handler. Gleicher Code aus beiden Quellen innerhalb von 300 ms wird als
Doppelung verworfen. Der HID-Pfad mit seinen Keypress-Guards bleibt
unverändert erhalten.

**Tunnel / Terminal (Phase 3)**: Der Tunnel braucht **keine Konfiguration**.
Die Kasse liest die Terminal-IP wie heute aus ERPNext, verbindet sich mit
der Bridge und ruft `tunnel.open` mit dem Ziel und dem Subprotokoll `SIXml`.
Die Bridge öffnet einen Listener auf einem freien Port von 127.0.0.1 und
antwortet mit der Portnummer. Die Kasse setzt
`connectionIPString = "127.0.0.1"` und `connectionIPPort = <port>`; die
Bibliothek baut daraus `ws://127.0.0.1:<port>/SIXml`, was der Browser von
einer HTTPS-Seite aus zulässt. Frames werden in beide Richtungen unverändert
durchgereicht.

Lebensdauer: Der Listener gehört der WS-Verbindung, die ihn geöffnet hat,
und verschwindet mit ihr (kurze Nachfrist von 10 s, damit ein Seitenreload
der Kasse nicht den Terminal-Kontext verliert). Damit gibt es nie einen
verwaisten Tunnel und kein Ziel, das jemand am Kassen-PC umbiegen könnte;
die TIM-API vertraut den Antworten des Terminals, ein falsches Ziel könnte
»genehmigt« vorspielen. Nur erlaubte Origins dürfen `tunnel.open` rufen.
Der nginx-Umweg entfällt. Gleicher Mechanismus später für Raw-TCP-Geräte
(`tunnel.open {target: "tcp://…"}`, WS→TCP).

### 11.1 Tunnel-Härtung

Ein generischer Tunnel ist ein Proxy, deshalb gelten fest eingebaute Regeln
ohne Konfigurationsmöglichkeit:

1. **Origin auch am Tunnel-Listener.** Der Listener nimmt nur Verbindungen
   an, deren Origin der Session entspricht, die ihn geöffnet hat. Sonst
   könnte eine fremde Website die Loopback-Ports durchprobieren und
   `ws://localhost:<port>/SIXml` direkt mit dem Terminal verbinden.
2. **Bindung an die Session.** Der Listener lebt nur, solange die öffnende
   WS-Verbindung lebt (10 s Nachfrist für Seitenreload). Höchstens zwei
   offene Tunnel pro Session, `tunnel.open` erst nach `hello`.
3. **Zielprüfung für IPv4 und IPv6.** Hostnamen werden aufgelöst, **alle**
   Adressen geprüft und die geprüfte Adresse für den Dial festgehalten
   (kein erneutes Auflösen, sonst DNS-Rebinding). Erlaubt ist ein Ziel, wenn
   es *on-link* ist, also im Präfix eines eigenen Netzwerk-Interfaces liegt
   (deckt IPv4-LANs ebenso wie IPv6-LANs mit Provider-Präfix ab), **oder**
   in einem privaten Bereich: RFC 1918 (10/8, 172.16/12, 192.168/16) bzw.
   ULA (fc00::/7). Immer verboten: Loopback (127/8, ::1) und
   IPv4-mapped-Loopback (::ffff:127.0.0.1, der klassische Umgehungstrick),
   Link-Local (169.254/16, fe80::/10), Multicast, Unspecified, und alles
   Öffentliche. Damit ist die Bridge kein offener Proxy ins Internet und
   erreicht nicht ihre eigene Oberfläche.
4. **Loopback nur für die Bridge selbst.** WS-API, Oberfläche und
   Tunnel-Listener binden ausschließlich 127.0.0.1 und ::1; eine Adresse ist
   nicht konfigurierbar. Die HTTP-Kompat-Route ist der einzige nach außen
   sichtbare Listener und trägt weder `/ws` noch Tunnel.
5. **Nur Frames, keine Deutung.** Der Tunnel reicht Frames byteweise durch
   und schreibt nur Verbindungsauf- und -abbau ins Log, nie Inhalte
   (Zahlungsdaten).

Restrisiko: Schadcode auf einer erlaubten Site (XSS in ERPNext oder POS)
könnte Tunnel ins LAN der Station öffnen. Die Regeln 2 und 3 begrenzen das
auf wenige Ziele im eigenen Netz; ein solcher Angreifer hätte über ERPNext
selbst ohnehin mehr Reichweite.

**Drucker (Phase 4, nur für neue Funktionen)**: Raw-9100 und IPP für
Netzwerkdrucker, OS-Spooler für USB (`lp`/CUPS auf Linux, Win32-Spooler auf
Windows). Desk schickt fertige Bytes (ZPL, ESC/POS, PDF) an
`print.raw`/`print.pdf`. Der bestehende Serverdruck über CUPS bleibt für
alles, was es heute gibt.

## 12. Betrieb

* **Oberfläche** `http://127.0.0.1:8735/` (in das Binary eingebettet, kein
  Build-Schritt im Browser nötig, `html/template` + wenig JS):
  * *Status*: Geräte mit Zustand und letztem Wert, offene Tunnel mit
    Ziel und Erreichbarkeit, verbundene Clients (Origin, Client-Typ),
    Log-Ringpuffer (letzte 500 Zeilen), Version/Build.
  * *Konfiguration*: Formulare für Allgemein (Stationsname, Origins, Port),
    Geräte (Hinzufügen/Entfernen, Treiberwahl, Port aus der Liste der
    erkannten seriellen Geräte mit VID/PID/Seriennummer, Baud, Poll),
    und HTTP-Kompat. Je Gerät ein **Test**-Knopf (Waage: einmal lesen,
    Kamera: Modell abfragen). Offene Tunnel erscheinen nur im Status.
  * *Speichern* schreibt `bridge.yaml` atomar (Temp-Datei + Rename), behält
    Kommentare nicht (Hinweis in der Oberfläche) und lädt die betroffenen
    Treiber neu, ohne den Dienst neu zu starten. Vorherige Version wird als
    `bridge.yaml.bak` behalten.
  * *Schutz der Konfigurations-API*: Sie ist nur von 127.0.0.1 erreichbar,
    das reicht aber nicht, denn eine fremde Website könnte per
    Formular-POST an `http://127.0.0.1:8735/config` schreiben. Deshalb prüft
    die Bridge bei allen schreibenden Anfragen `Origin`/`Sec-Fetch-Site`
    (nur `same-origin`) und ein CSRF-Token aus der Seite. Keine Admin-PIN:
    Wer am Stations-PC sitzt, könnte Waage oder Kamera ohnehin ausstecken,
    und das einzige sicherheitsrelevante Datum, das Tunnelziel zum
    Terminal, existiert nur zur Laufzeit in der Session der Kasse
    (Abschnitt 11).
  * Die Oberfläche bietet für die WS-API nur den Port an; die
    HTTP-Kompat-Route hat ein Adressfeld, und dort warnt sie, wenn nicht ein
    Reverse Proxy davor steht.
  * `GET /healthz` als JSON für Monitoring.
* **Rechte**: Der Dienst läuft als eigener Benutzer mit Schreibrecht auf das
  Config-Verzeichnis und Mitgliedschaft in `dialout` (Linux); der Dienst
  muss die Config schreiben können, weil die Oberfläche in seinem Prozess
  läuft.
* **Dienst**: `bridge -service install` legt systemd-Unit bzw. Windows-Dienst
  an. Linux zusätzlich udev-Regel (Symlink + Gruppe `dialout`) und
  Kamera-Regel (kein `gvfs-gphoto2`, das die Kamera blockiert).
* **Logs**: `slog` nach stdout (journald) und in eine rotierende Datei neben
  der Config; Log-Level per Config.
* **Updates**: über die Oberfläche, »Version & Update« (Abschnitt 18):
  prüfen, signiertes Release laden, einspielen, zurück.
* **ERPNext-Kompatibilität**: Die Bridge meldet ihre Protokollversion im
  `hello`; der Client lehnt unbekannte Hauptversionen mit klarer Meldung ab.

## 13. Offene Punkte und Risiken

1. **Mixed Content auf den echten Kassen-PCs**: `ws://localhost` von einer
   HTTPS-Seite ist in aktuellen Chrome/Firefox erlaubt, für 127.0.0.1 und
   ::1 gleichermaßen. Auf den vorhandenen Kassen-Browsern einmal nachweisen,
   und dabei prüfen, in welche Familie `localhost` dort aufgelöst wird.
2. **PCE-PB-Befehlsumfang**: Bestätigt sind `Sx` (Gewicht) und `ST` (Tara).
   Nullstellen und ein Stabil-Marker sind im Handbuch zu prüfen, bevor
   `scale.zero` zugesagt wird.
3. **libgphoto2 per cgo**: Cross-Build für arm64 braucht ein Docker-Build mit
   passender Bibliotheksversion. Alternative im Notfall: `gphoto2`-CLI per
   `exec`, aber das öffnet die Kamera pro Foto neu und war genau der Grund
   für die heutige Dauer-Session.
4. **Ein Port, ein Prozess**: Flask und Bridge dürfen nie gleichzeitig laufen.
   Der Umstellungsablauf: Flask-Dienst stoppen und deaktivieren, Bridge
   starten, Reverse Proxy auf `http://<box>:5000` belassen (gleicher Port
   möglich) oder anpassen, `/health` prüfen.
5. **Tunnel-Zielport des Terminals**: Aus der nginx-Konfiguration des
   POS-Servers übernehmen; sie ist im Infra-Repo nicht enthalten. Landet
   dann als Port neben der Terminal-IP in der ERPNext-POS-Konfiguration.
6. **Content-Security-Policy**: Frappe setzt standardmäßig keine
   `connect-src`-CSP. Falls später eine eingeführt wird, muss
   `ws://127.0.0.1:*` erlaubt sein.
7. **Mehrere Tabs**: Jeder Tab hält eine eigene Verbindung; die Bridge
   broadcastet Events an alle Abonnenten. Kommandos mit Seiteneffekt
   (Capture, Tara) sind pro Gerät serialisiert.
8. **Ausweichen darf nicht unbemerkt bleiben**: Beim Gewicht ist der
   Ausweichmodus eine bewusste Bestätigung des Bedieners, nicht ein stiller
   Fallback. Für die Kasse ist »Kartenzahlung ausgeblendet« ein sichtbarer
   Hinweis im Kopf, kein stummes Fehlen der Option.

## 14. Phasenplan

| Phase | Inhalt | Ergebnis |
|---|---|---|
| 0 | Repo, Go-Skelett, Config-Laden, WS-Server dual-stack (127.0.0.1 + ::1), Origin-Prüfung, Status- und Konfigurationsoberfläche mit Hot-Reload, Dienst-Installation, CI-Builds | Bridge startet, wird im Browser konfiguriert, Desk kann `hello` sprechen |
| 1 | Treiber PCE-PB, `scale.*`, HTTP-Kompat `/weight` + `/health`, `hwbridge.js` mit Zustandsmodell, Live-Gewicht + Ausweichmodus (manuelle Eingabe mit Bestätigung) in der Parcel Station, `client_weight_kg` im Sendungsanlegen | Flask abgeschaltet, Live-Gewicht sichtbar, Station läuft auch ohne Bridge |
| 2 | Kamera (cgo, Build-Tag), `camera.capture`, HTTP-Kompat `/shot`, Photo Station über Bridge + `save_browser_captured_photo` | Kamera-Pfad komplett in Go |
| 3 | Serieller Scanner, gemeinsame Scan-Quelle im Desk; Tunnel für Worldline-Terminal (`tunnel.open` aus der Kasse, ohne Config) mit Erreichbarkeitsstatus, Kasse blendet Kartenzahlung ohne Terminal aus, nginx-Umweg entfernen | POS ohne Proxy, Scanner ohne Fokusprobleme |
| 4 | Druckmodul: Labels roh (ZPL) auf den Drucker der Station, `printer.print` (Abschnitt 16) | **gebaut 2026-10-01**, Windows-Weg nur kompiliert |
| 5 | Selbst-Update (Abschnitt 18), Windows-Installer | Update **gebaut 2026-10-02**; Neustart durch den Dienstverwalter an keiner echten Installation geprüft. Installer offen |

## Quellen

* PCE-PB N Series User Manual (RS-232-Befehle `Sx`, `ST`):
  https://www.pce-instruments.com/english/slot/2/download/385870/manual-electronic-balance-pce-pb-n-series_1017996.pdf
* Manuals+ Zusammenfassung PCE-PB N: https://manuals.plus/pce/pce-pb-n-series-platform-scales-manual

## 15. Umsetzungsstand und Abweichungen (2026-09-24)

Phase 0 und der Bridge-Teil von Phase 1 sind gebaut. Abweichungen vom
ursprünglichen Text:

* **Flask bleibt bis Phase 2.** Die Kamera ist noch nicht portiert, also
  kann Flask in Phase 1 nicht abgeschaltet werden. Flask zieht auf
  127.0.0.1:5001, die Bridge übernimmt :5000, beantwortet `/weight` selbst
  und reicht alles andere per `http_compat.legacy_upstream` weiter. Der
  Reverse Proxy bleibt unverändert. Kein Portkonflikt, weil Flask die Waage
  nur beim Aufruf von `/weight` öffnet.
* **Host-Header-Prüfung** auf allen Loopback-Routen zusätzlich zu Origin
  und CSRF: `localhost`, `127.0.0.1` oder `[::1]` mit dem eigenen Port.
  Ohne sie könnte eine fremde Domain per DNS-Rebinding auf 127.0.0.1 zeigen
  und die Oberfläche als »same-origin« bedienen.
* **Geräte-ID optional.** `scale.*` ohne `device` nimmt die erste Waage.
  Damit muss das Desk keine Geräte-IDs kennen.
* **Reconnect nach Ausstecken** höchstens alle 5 s statt 30 s, damit ein
  eingesteckte Waage schnell erkannt wird.
* **Konfiguration:** `listen_port` statt `listen`, eine Adresse ist nicht
  wählbar. Kommentare in `bridge.yaml` überleben das Speichern aus der
  Oberfläche nicht.
* **Topologie:** Für das Live-Gewicht muss die Waage an dem PC hängen, an
  dem der Parcel-Station-Browser läuft. Hängt sie an einer separaten
  Linux-Box, bleibt dort nur der Serverpfad `/weight`.
* **Ausweichmodus ohne Pflichtbestätigung (Parcel Station).** Solange die
  Waage noch an der Linux-Box hängt, liefert der Serverpfad `/weight` das
  Gewicht. Ein leeres Eingabefeld im Ausweichmodus heißt deshalb »Server
  entscheidet wie bisher« (Server-Waage, dann Artikelgewichte), statt die
  Sendung zu blockieren. Blockiert wird nur, wenn die Waage live ist und
  sich bewegt oder 0 kg zeigt. Sobald der Serverpfad entfällt, sollte ein
  leeres Feld ohne Live-Waage ebenfalls blockieren.
* **Quelle am Shipment:** Jede Sendung bekommt einen Timeline-Kommentar
  »Weight x kg from …« (Waage per Bridge, manuelle Eingabe, Server-Waage
  oder Artikelgewichte).

## 16. Druckmodul (2026-10-01)

**Entscheidung.** Die Druckerauswahl am Platz bestimmt, in welchem Format ein
Label beim Carrier angefordert wird, denn ein Label wird je Sendung einmal
ausgestellt:

| Auswahl in der Parcel Station | Format | Weg |
|---|---|---|
| Drucker der Hardware Bridge | ZPL | Desk → Bridge → Drucker, still |
| CUPS-Drucker | ZPL | ERPNext-Server → Druckserver, still (wie bisher) |
| „PDF (Druckdialog)“ | PDF vom Carrier | Druckdialog des Browsers – Notlösung für Plätze ohne Labeldrucker |

**Grundsatz: Ein Label wird nicht bearbeitet.** Weder die Bridge noch der
Server rendern ein Label, wandeln es zwischen ZPL und PDF um oder schreiben
darin etwas um. Es kommt so am Drucker an, wie der Carrier es geliefert hat
(Test `TestPrintPassesBytesThroughUnchanged`, `TestWebSocketPrintsLabelUnchanged`).
Deshalb gibt es keinen Kreuzdruck: Ein PDF-Label geht nicht auf einen
ZPL-Drucker, ein ZPL-Label nicht in den Druckdialog; das Desk sagt das dem
Benutzer, statt umzuwandeln. Die Bridge lehnt jedes andere Format als
`zpl`/`raw` mit `unsupported_format` ab.

**Treiber** (`internal/printer`, Klasse `printer`):

| Treiber | Ziel | Stand |
|---|---|---|
| `raw_tcp` | Netzwerkdrucker, Port 9100 | getestet gegen lokalen Listener |
| `raw_file` | Gerätedatei, z.B. `/dev/usb/lp0` | getestet gegen Datei |
| `system` | Druckwarteschlange des Betriebssystems, roh: CUPS (`lp -o raw`) bzw. Windows-Spooler (Datentyp `RAW`); unter CUPS mit `accept: pdf` auch PDF durch den Treiber (Abschnitt 21) | roh **nur kompiliert**, an keinem echten Drucker geprüft |

Der Zustand (`device.state`) kommt aus einer Erreichbarkeitsprüfung alle
15 s und nach jedem Auftrag: TCP-Verbindungsaufbau, Existenz der
Gerätedatei, `lpstat -p` bzw. `OpenPrinter`. Ein Auftrag ist auf 4 MB
begrenzt; das Leselimit der WebSocket-Verbindung wurde dafür auf 8 MB
angehoben. Aufträge an einen Drucker laufen nacheinander.

Die Oberfläche hat „+ Drucker“ und „Testetikett drucken“. Das Testetikett ist
ein eigenes kleines ZPL der Bridge, kein Carrier-Label. `GET /api/printers`
liefert die Drucker des Betriebssystems als Vorschlagsliste.

**Offen / Risiken**

* Chrome prüft seit 2025 den Zugriff öffentlicher Seiten auf das lokale Netz
  (»Local Network Access«). Das Desk (https) erreicht `ws://localhost:8735`
  erst, nachdem der Benutzer die Abfrage des Browsers erlaubt hat oder die
  Richtlinie `LocalNetworkAccessAllowedForUrls` die ERPNext-Adresse freigibt.
  Ohne Freigabe sieht das Desk »keine Bridge« – das betrifft auch die Waage.
* `system` unter Windows und unter CUPS an einem echten Drucker prüfen.
* Mehrere Kopien, Statusrückmeldung des Druckers (Papier leer) und USB ohne
  Betriebssystem-Treiber unter Windows sind nicht enthalten.

## 17. PDF über IPP und Standarddrucker (2026-10-02)

**Anlass.** PDFs sollen ohne Druckdialog gedruckt werden können: PDF-Labels
an Plätzen ohne Etikettendrucker, Zolldokumente, und Belege aus jedem
ERPNext-Formular über einen Schnelldruck-Knopf.

**Treiber `ipp`.** Spricht IPP 2.0 über http(s) (`ipp://` → Port 631,
`ipps://` mit TLS), ohne Fremdbibliothek (`internal/printer/ipp.go`).
`Get-Printer-Attributes` liefert Zustand und `document-format-supported`;
daraus entstehen die Formate des Druckers: `application/pdf` → `pdf`,
`application/vnd.cups-raw` → `zpl`. `Print-Job` schickt das Dokument
unverändert mit seinem Format; der Drucker rendert. PDF ist bei IPP
Everywhere optional – ein Drucker, der nur Rasterformate meldet, bietet kein
Format an und gilt als nicht nutzbar. Umgewandelt wird nichts.

Ein CUPS-Server meldet für jede Warteschlange PDF und Rohdaten, egal was
dahinter steht. `accept: pdf|zpl` legt deshalb fest, wofür ein Drucker
taugt. `media` und `print_scaling` gehen bei PDF als Auftragsattribute mit,
wenn gesetzt. `insecure_tls` schaltet die Zertifikatsprüfung je Drucker ab
(Drucker haben meist selbstsignierte Zertifikate). Anmeldung am Drucker
(HTTP-Auth) ist nicht eingebaut.

**Formate je Drucker.** `Status.formats` steht in `hello`, `devices.list`
und `device.state`. Die Rohtreiber melden `zpl`.

**Standarddrucker.** `default: true` an einem Drucker macht ihn zum
Standarddrucker des Arbeitsplatzes. `printer.print` ohne `device` wählt den
Standarddrucker, der das Format annimmt, sonst den ersten passenden
(`printer.Pick`). Die Zuordnung liegt wie alles andere in `bridge.yaml`:
ERPNext kennt keine Arbeitsplätze.

**Grenzen.** Ein Auftrag bis 16 MB, WebSocket-Nachricht bis 24 MB.

**Desk.** `hwbridge.js` kennt `printersFor(format)`, `defaultPrinter(format)`
und `standby()`: Seiten, die die Bridge nur gelegentlich brauchen, lassen
den Client nach dem ersten Fehlschlag ruhen, bis `retry()` gerufen wird.
Der Schnelldruck (`holzschuherzeugung_devich/public/js/quick_print.js`)
zeigt seinen Knopf nur, wenn ein PDF-Drucker erreichbar ist.

**Stand.** Getestet gegen einen IPP-Drucker als Attrappe (Formate, Auftrag
bytegleich, Fehler). Die Abfrage (`Get-Printer-Attributes`) lief über IPPS
gegen einen echten CUPS-Server. Ein echter Druckauftrag über IPP wurde nicht
abgeschickt.

## 18. Updates (2026-10-02)

**Ablauf.** In der Oberfläche unter »Version & Update«:

1. *Auf Updates prüfen* fragt das neueste GitHub-Release des Repos ab und
   zeigt Version und Release-Notiz. Dasselbe passiert still eine Minute nach
   dem Start und danach einmal am Tag (abschaltbar: `no_update_check`).
   Installiert wird nie von selbst.
2. *Installieren* lädt das Binary dieser Plattform, die Prüfsummen-Datei und
   deren Signatur, prüft beides und setzt das neue Binary an die Stelle des
   laufenden. Das bisherige bleibt als `<name>.old` liegen.
3. Die Bridge beendet sich; systemd (`Restart=always`) bzw. der
   Windows-Dienstverwalter (Wiederherstellung »Neu starten«) startet das neue
   Binary. Ohne Dienst sagt die Oberfläche, dass von Hand neu zu starten ist.
4. *Vorige Version wiederherstellen* tauscht zurück.

Die Konfiguration wird nicht angefasst.

**Signatur.** Der Release-Workflow unterschreibt `SHA256SUMS-<version>` mit
einem Ed25519-Schlüssel aus dem GitHub-Secret `RELEASE_SIGNING_KEY`
(`cmd/relsign`); die Bridge prüft mit dem einkompilierten öffentlichen
Schlüssel (`internal/update/pubkey.go`). Eine Prüfsumme allein, vom selben
Ort geladen wie das Binary, erkennt nur Übertragungsfehler – hier ersetzt
ein Dienst sein eigenes Programm, deshalb zählt die Herkunft. Abgelehnt
werden: fremder Schlüssel, geänderte Prüfsummen oder Binaries, fehlende
Signatur, ein altes Release unter neuem Namen (die Dateinamen tragen die
Version) und alles, was nicht neuer ist als die laufende Version. Ohne
Secret schlägt der Release-Lauf fehl, es entsteht kein unsigniertes Release.

Ein neues Schlüsselpaar (`relsign keygen`) heißt: Ausgelieferte Bridges
nehmen keine Updates mehr an, bis sie einmal von Hand auf eine Version mit
dem neuen öffentlichen Schlüssel gebracht wurden.

**Voraussetzungen.**

* Das Repo ist öffentlich (oder die Releases liegen öffentlich): Die
  Stationen haben kein GitHub-Token.
* Der Dienst darf im Programmordner schreiben. Linux: Das Installationsskript
  legt das Binary nach `/opt/erpnext-hardware-bridge/` (gehört dem
  Dienstbenutzer `hwbridge`, in der Unit unter `ReadWritePaths`). Windows: Der
  Dienst läuft als LocalSystem und darf nach `Program Files` schreiben; eine
  laufende `.exe` lässt sich umbenennen, nur nicht überschreiben. Kann der
  Dienst nicht schreiben, zeigt die Oberfläche das an und bietet kein
  Installieren.

**Abwägung.** Ein Dienst, der sein Programm selbst ersetzen darf, gibt einem
Angreifer, der den Dienst übernimmt, auch dessen Programmordner. Die
Alternative – ein Helfer mit Root-Rechten, der geprüfte Updates einspielt –
trennt das sauberer, bringt aber eine zweite Unit und einen zweiten
Prüfpfad. Entschieden für den einfachen Weg; der Dienst läuft ohnehin
unprivilegiert und gehärtet.

**Stand.** Getestet: Prüfen, Laden, Signatur, Tausch und Rückweg gegen einen
nachgestellten Release-Server, auch über die Oberfläche mit einem echt
signierten Test-Release. Nicht geprüft: der Neustart durch systemd bzw. den
Windows-Dienstverwalter an einer echten Installation und ein Lauf des
Release-Workflows mit dem Signier-Schritt.

## 19. Zahlungsterminal: feste Weiterleitung (2026-10-04)

**Entscheidung.** Abweichend von Abschnitt 11 öffnet die Kasse keinen Tunnel
per `tunnel.open`. Die Weiterleitung steht fest in `bridge.yaml` (Gerät
`kind: terminal`, `driver: ws_forward`, `address` = Terminal im LAN,
`local_port` = Port auf 127.0.0.1/::1) und lauscht, solange die Bridge läuft.
Im POS Profile steht als Terminal-Adresse `127.0.0.1` mit diesem Port; die
Kasse spricht die Bridge dafür nicht an. Der bisherige Weg über einen
Reverse Proxy mit `wss://` (dafür war `timapi.js` umgebaut) entfällt, die
Kasse benutzt wieder die unveränderte Bibliothek mit `ws://`. Ohne Bridge
gibt es an einer Kasse keine Kartenzahlung; die Kasse sagt das.

**Warum.** Vom Benutzer so gewünscht: das Terminal gehört zum Platz, wie
Waage und Drucker, und wird dort eingerichtet.

**Was aus 11.1 bleibt.** Origin-Prüfung (dieselben `allowed_origins` wie die
WS-API) und Host-Prüfung (DNS-Rebinding) am Listener, bevor das Terminal
überhaupt angesprochen wird; nur WebSocket-Handshakes; Ziel nur im eigenen
Netz (on-link, RFC 1918, ULA), aufgelöst und geprüft vor dem Verbinden; nur
Auf- und Abbau im Log, nie Inhalte. Entfallen sind die Bindung an eine
WS-Session und die Grenze von zwei Tunneln, weil es keine Session gibt.

**Umsetzung.** Der Handshake der Kasse wird gelesen, geprüft und unverändert
an das Terminal weitergegeben (nur `Host` zeigt aufs Terminal), danach
werden Bytes in beide Richtungen durchgereicht – ohne die Frames zu deuten.
Ins Log kommt die Statuszeile der Terminal-Antwort auf den Handshake (101
oder z. B. 404), damit ein falscher Port oder Pfad sofort auffällt.
Der Testknopf baut nur eine TCP-Verbindung auf und gleich wieder ab, weil
das Terminal nur eine Verbindung zulässt. Ist der Port belegt, versucht die
Bridge alle 5 s erneut und zeigt den Fehler in der Oberfläche.

**Stand.** Getestet mit Unit-Tests und im Browser: die Kasse auf
`https://erp-dev…/kassa` verbindet mit der echten TIM-Bibliothek über die
Bridge zu einem nachgebauten Terminal (Handshake mit Subprotokoll `SIXml`,
erste SIXml-Nachricht kommt an); eine fremde Seite wird abgewiesen. Nicht
geprüft: ein echtes Worldline-Terminal.

## 20. Bondrucker: ESC/POS (2026-10-05)

Die Kasse druckt Bon, Terminal-Beleg, A4-Rechnung und Kassenabschluss nicht
mehr über CUPS am Server, sondern über die Bridge am Kassen-PC. Dafür kennt
das Druckmodul eine dritte Sprache neben ZPL und PDF: **ESC/POS**.

Rohdaten sind Rohdaten – ob hinter Port 9100, einer Gerätedatei oder einer
Roh-Warteschlange ein Etikettendrucker oder ein Bondrucker steht, sieht die
Bridge nicht. Das sagt `accept` am Drucker: `zpl` (Vorgabe, wie bisher) oder
`escpos`; bei IPP zusätzlich `pdf`. Ein Bondrucker nimmt dann nur ESC/POS an,
ein Etikett oder eine Rechnung landet dort nicht. Der Testknopf schickt einen
kurzen Testbon (ASCII, Schnitt am Ende).

Welche Drucker die Kasse benutzt, steht – abweichend von Abschnitt 9 – im
POS Profile als Geräte-ID der Bridge (Entscheidung des Benutzers). Gerendert
wird weiter am Server (ESC/POS-Rasterbild bzw. PDF); die Kasse holt die Bytes
und reicht sie mit `printer.print` an die Bridge. Ohne Bridge oder Drucker
druckt die Kasse nicht und sagt, was fehlt.

## 21. PDF über den Treiber einer CUPS-Warteschlange (2026-10-08)

Anlass: Ein Linux-PC, der rund um die Uhr läuft, soll später die Drucker im
Haus für ERPNext teilen und den CUPS-Server ersetzen. Dafür muss er auch
Bürodrucker bedienen, die selbst kein PDF verstehen.

`system` mit `accept: pdf` (nur Linux/macOS) schickt ein PDF **ohne**
`-o raw` an die Warteschlange (`lp -d <queue> -t <titel>`). CUPS wandelt es
mit Filtern und Treiber der Warteschlange um, wie bei einem Druck aus jedem
anderen Programm. `media` und `print_scaling` gehen als `-o media=…` bzw.
`-o print-scaling=…` mit. Rohdaten (ZPL, ESC/POS) laufen wie bisher mit
`-o raw`. Ein Drucker hat weiter genau eine Verwendung; ZPL an einen
PDF-Drucker lehnt die Bridge ab.

Das weicht vom Grundsatz aus Abschnitt 16 ab („die Bridge wandelt nichts
um“): umgewandelt wird nicht von der Bridge, sondern vom Treiber des
Betriebssystems, den der Betreiber selbst eingerichtet hat.

Windows: Der Spooler druckt PDF nicht ohne eigenes Rendern (PDFium,
`Windows.Data.Pdf` oder ein Hilfsprogramm). Die Konfiguration lehnt
`accept: pdf` bei `system` unter Windows deshalb ab; PDF dort über `ipp` an
PDF-fähige Drucker.

Getestet: Unit-Tests mit nachgebautem `lp` (Argumente, Daten), dazu ein
echter Lauf in einem Container mit CUPS, Warteschlange mit PCL-Treiber
(`generpcl.ppd`) und Datei als Ziel: PDF über die WS-API gedruckt, heraus
kam PCL mit A4; ZPL an denselben Drucker abgelehnt. Nicht geprüft: echter
Drucker.

## 22. RFID-Reader metratec DeskID UHF v2 (2026-10-09)

Anlass: Die Fotostation soll den RFID-Tag am Paar lesen und ihn so
beschreiben, dass ein Handy `https://holzschuhe.at/u/<tid>` öffnet. Der
Reader ist UHF (868 MHz), Handys können nur NFC (13,56 MHz). Das geht nur mit
Dual-Frequenz-Tags, bei denen beide Seiten denselben Speicher sehen – hier
EM4425 (em|echo-V).

Treiber `rfid/metratec_uhf`: USB-C, virtueller COM-Port, 115200 Baud,
metratec-AT-Protokoll (Vorlage: `github.com/metratec/rfid-sdk-python`,
Klasse `DeskIdUhfv2`). Beim Verbinden: `ATE0`, `ATI` (Kennung muss "UHF"
enthalten, sonst Zustand `error`), `AT+BINV` (ein altes Dauer-Inventory
beenden), `AT+INVS` mit TID und RSSI, `AT+PWR` aus `power_dbm`. Mit Zuhörern
läuft im `poll_ms`-Takt `AT+INV`; `rfid.tags` geht nur bei Änderungen raus.
Ohne Zuhörer alle 5 s `AT` als Lebenszeichen.

Schreiben: genau ein Tag mit TID, dann `AT+MSK=TID,0,<tid>` (alle folgenden
Funkbefehle nur für diesen Tag), `AT+WRT=USR,<byte>,<hex>` in 16-Byte-Stücken,
`AT+READ` zur Kontrolle, `AT+MSK=OFF` auch im Fehlerfall. Adressen im
Nutzerspeicher sind Byte-Adressen (laut SDK). Der Inhalt ist ein NFC Forum
Type 5 Tag: CC `E1 4x MLEN 00` (MLEN = nur das Geschriebene, weil die Größe
des NFC-Bereichs unbekannt ist), NDEF-TLV mit einem URI-Record, Terminator,
aufgefüllt auf 4-Byte-Blöcke.

Offen, nur mit echten Tags zu klären:

- **Wo der NFC-Bereich im UHF-Nutzerspeicher beginnt.** Der EM4425 teilt
  seinen 2048-bit-Speicher in HF-User, UHF-User, EPC und Signatur auf; die
  Aufteilung ist einstellbar und steht nur im vollen Datenblatt (»on
  request«). Deshalb kommt der Offset vom Aufrufer, und »Testen« zeigt den
  Speicherauszug samt gefundener Adresse: mit einer Handy-App (z.B. NFC
  Tools) eine Adresse schreiben, dann in der Bridge testen – der angezeigte
  Offset ist der richtige. Ist der UHF-Nutzerspeicher leer partitioniert,
  schlägt schon das Lesen fehl; dann braucht es das volle Datenblatt.
- **Sperren.** `read_only` setzt nur das Zugriffsbyte im Capability
  Container; Handys und NFC-Apps halten sich daran, ein Angreifer mit eigener
  Software nicht. Eine echte Sperre (UHF-Access-Passwort + `AT+LCK`, NFC-seitig
  Blocksperre/Passwort des EM4425) ist nicht gebaut.
- Maximale Datenmenge je `AT+WRT`/`AT+READ` (16 bzw. 64 Byte sind
  vorsichtig gewählt), Sendeleistung für »nur das aufgelegte Paar«.

Getestet: Unit-Tests gegen einen nachgebauten Reader (Echo, Zeilen in
Häppchen, Maske, Speicherüberlauf, Schreibfehler, abgezogenes Kabel, stummer
Reader) und ein Lauf der echten Bridge gegen `testdata/fake_deskid_uhf.py` an
einem PTY, angesteuert über `hwbridge.js` im Browser. Nicht geprüft: echter
Reader, echter Tag, Handy.

Nachtrag nach dem ersten Test mit echtem Reader (2026-10-09): Die DeskID-
Firmware liefert die TID im Inventory mit der Vorgabe "1" nur mit 8 Byte
(`E280B11720007801`); der EPC eines frischen EM4425 ist eine Kopie der vollen
TID (`E280B117 20007801 144BF9A8`). 8 Byte sind nicht eindeutig. Die Bridge
fordert deshalb 12 Byte an (`AT+INVS=…,12,…`), liest bei älterer Firmware per
`AT+READ=TID,0,12` nach und beschreibt keinen Tag mit kürzerer TID
(`no_tid`). Außerdem waren die ersten 64 Byte des UHF-Nutzerspeichers leer,
obwohl ein Handy eine Adresse geschrieben hatte; »Testen« liest den
Nutzerspeicher jetzt bis zu seinem Ende (höchstens 512 Byte) und meldet die
lesbare Größe.

Zweiter Test (2026-10-09): Mit voller TID las der Reader nur 32 Byte
UHF-Nutzerspeicher, danach MEMORY OVERRUN, alles leer – obwohl ein Handy eine
Adresse in den NFC-Teil geschrieben hatte. Hinweis aus
stackoverflow.com/q/78100511 (Kommentar des Fragestellers): beim EM4425
liegt der NFC-Nutzerspeicher in der UHF-Nutzerbank ab **Wort A0h**, nicht im
Anschluss an den UHF-Bereich. »Testen« liest deshalb zusätzlich ab Adresse
320 (A0h als Byteadresse, wie das SDK Adressen zählt) und, falls dort nichts
lesbar ist, ab 160 (falls der Reader doch in Worten zählt). Die gefundene
Startadresse kommt in die Photo Station Settings (»NFC-Bereich ab Byte«).
Zählt der Reader tatsächlich in Worten, stimmt das stückweise Schreiben mit
Byte-Schritten nicht – dann muss der Treiber nachgezogen werden.

Dritter Test (2026-10-09): Der NFC-Bereich liegt tatsächlich ab Adresse 320
(Byteadressierung bestätigt), 184 Byte, mit dem Capability Container der
Handy-App `E1 40 17 09`. Zwei Folgerungen: (1) Die DeskID-Firmware nimmt die
TID-Länge im Inventory nicht an; das Nachlesen per `AT+READ=TID` bekam einmal
keine Antwort, obwohl der Tag mit -32 dBm auflag. Lese- und Schreibbefehle an
den Tag werden deshalb bis zu dreimal wiederholt, wenn kein Tag antwortet.
(2) Ein vorhandener Capability Container wird weiterverwendet (MLEN und
Merkmalsbyte), nur die Zugriffsbits setzt die Bridge selbst.

Vierter/fünfter Test: `AT+READ` lehnt schon 32 Byte mit `Read length too big`
ab (16 gehen; »Testen« hatte still halbiert und so 32 vorgetäuscht). Die
Leselänge wird deshalb ausprobiert – 32, bei genau diesem Fehler halbieren –
und für die Sitzung gemerkt.

Sechster Test: Kontrolllesen bekam `+READ: <EPC>,ERROR`. Lese-/Schreibbefehle
werden jetzt auch bei einem Fehlerstatus des Tags wiederholt (außer
Speicherende und Zugriffsfehler), mit kurzer Pause, falls das EEPROM nach dem
Schreiben noch beschäftigt ist. Bei Log-Level `debug` schneidet der Treiber
jeden AT-Befehl und jede Antwortzeile mit (ohne die Dauerabfrage).

Achter Test (AT-Konsole): Der NFC-Bereich des EM4425 will ganze 4-Byte-Blöcke.
16 Byte je `AT+WRT` kamen mit einzelnen falschen Bits zurück (immer Byte 5 und
9 des Stücks, Bit 0x02) oder machten den Block unlesbar (`ERROR`); ein halber
Block (2 Byte) ebenso. Je 4 Byte an einer 4-Byte-Grenze kamen exakt zurück.
Das erklärt alle vorherigen Befunde (Handy sah nichts, Lesen ab 320 ERROR).
Die Bridge schreibt deshalb in 4-Byte-Blöcken, `offset` muss durch 4 teilbar
sein; der DeskID meldet je geschriebenem Wort eine `+WRT`-Zeile, alle müssen
OK sein. Der Simulator bildet das Blockverhalten nach (`HFBlocks`).

