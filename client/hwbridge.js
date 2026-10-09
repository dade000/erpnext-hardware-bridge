/*!
 * hwbridge.js – Browser-Client der ERPNext Hardware Bridge
 * Protokoll 1 · Quelle: erpnext-hardware-bridge/client/hwbridge.js
 *
 * Diese Datei wird in die Apps kopiert, die mit der Bridge sprechen
 * (Parcel Station, holzschuherzeugung_devich, POS). Änderungen immer hier
 * machen und dann kopieren; die Versionszeile oben zeigt, welcher Stand liegt.
 *
 * Grundsatz: verbinden oder ausweichen. Der Client konfiguriert nichts. Er
 * versucht ws://localhost:8735/ws und meldet einen von drei Zuständen:
 *   "connecting" – Verbindungsversuch läuft
 *   "ready"      – Bridge verbunden, Geräteliste bekannt
 *   "no_bridge"  – auf diesem PC läuft keine Bridge (wird im Hintergrund
 *                  weiter versucht, bis zu alle 30 s)
 * Pro Gerät zusätzlich deviceState(kind): "no_bridge" | "device_missing" |
 * "device_offline" | "ready".
 *
 *   const hw = HardwareBridge.shared({ client: "parcel_station" });
 *   hw.on("state", ({ state }) => …);
 *   hw.on("scale.weight", (w) => …);          // {kg, raw, stable, unit, ts}
 *   hw.subscribe("scale");                     // bleibt über Reconnects bestehen
 *   const w = await hw.call("scale.read");
 *   hw.devicesOfKind("printer");               // [{id, kind, driver, state, formats, default, …}]
 *   await hw.print("labeldrucker", base64Zpl, "SHIPMENT-00150");
 *   hw.defaultPrinter("pdf");                  // Standarddrucker des Arbeitsplatzes für PDF
 *   await hw.print(null, base64Pdf, "SAL-ORD-2026-01224", "pdf");
 *   hw.on("rfid.tags", (d) => …);              // {tags: [{epc, tid, rssi}], ts}
 *   hw.subscribe("rfid");
 *   await hw.call("rfid.write_uri", { uri: "https://…/u/{tid}" }, 20000);
 *
 * Seiten, die die Bridge nur gelegentlich brauchen (Schnelldruck im Desk),
 * rufen nach dem ersten Fehlschlag standby(): dann wird nicht weiter
 * versucht, bis jemand retry() ruft. So bleibt ein PC ohne Bridge ruhig.
 */
(function (root) {
	"use strict";

	var PROTOCOL = 1;
	var DEFAULT_URL = "ws://localhost:8735/ws";
	var CONNECT_TIMEOUT_MS = 1500;
	var CALL_TIMEOUT_MS = 6000;
	// Ein Druckauftrag darf länger dauern als eine Messung: die Bridge wartet
	// selbst bis zu 20 s auf den Drucker.
	var PRINT_TIMEOUT_MS = 25000;
	var BACKOFF_MS = [1000, 2000, 5000, 10000, 30000];

	function HardwareBridge(opts) {
		opts = opts || {};
		this.url = opts.url || DEFAULT_URL;
		this.client = opts.client || "desk";
		this.token = opts.token || "";
		this.state = "connecting";
		this.info = null;
		this.devices = {}; // id -> {id, kind, driver, state, message, last}
		this._ws = null;
		this._handlers = {};
		this._pending = {};
		this._nextId = 1;
		this._subs = {}; // kind oder id -> Anzahl
		this._attempt = 0;
		this._timer = null;
		this._closed = false;
		this._standby = false;
		this._lastError = "";
	}

	HardwareBridge.PROTOCOL = PROTOCOL;

	/** Eine Verbindung pro Browser-Tab, egal wie viele Seiten sie nutzen. */
	HardwareBridge.shared = function (opts) {
		if (!root.__hwbridgeShared) {
			root.__hwbridgeShared = new HardwareBridge(opts);
			root.__hwbridgeShared.connect();
		}
		return root.__hwbridgeShared;
	};

	var P = HardwareBridge.prototype;

	P.on = function (name, fn) {
		(this._handlers[name] = this._handlers[name] || []).push(fn);
		return this;
	};

	P.off = function (name, fn) {
		var list = this._handlers[name] || [];
		this._handlers[name] = list.filter(function (x) { return x !== fn; });
		return this;
	};

	P._emit = function (name, data) {
		(this._handlers[name] || []).slice().forEach(function (fn) {
			try { fn(data); } catch (e) { console.error("[hwbridge] Handler " + name, e); }
		});
	};

	P._setState = function (state, error) {
		this._lastError = error || "";
		if (this.state === state) return;
		this.state = state;
		this._emit("state", { state: state, error: this._lastError });
	};

	/** Erstes Gerät einer Klasse (oder Gerät mit dieser ID). */
	P.findDevice = function (kindOrId) {
		if (this.devices[kindOrId]) return this.devices[kindOrId];
		for (var id in this.devices) {
			if (this.devices[id].kind === kindOrId) return this.devices[id];
		}
		return null;
	};

	/** Alle Geräte einer Klasse, in der Reihenfolge der Bridge. */
	P.devicesOfKind = function (kind) {
		var out = [];
		for (var id in this.devices) {
			if (this.devices[id].kind === kind) out.push(this.devices[id]);
		}
		return out;
	};

	/** Online-Drucker, die dieses Format annehmen ("zpl", "pdf"). */
	P.printersFor = function (format) {
		return this.devicesOfKind("printer").filter(function (d) {
			return d.state === "online" && (d.formats || []).indexOf(format) !== -1;
		});
	};

	/**
	 * Der Drucker, den die Bridge für einen Auftrag ohne Geräteangabe nimmt:
	 * der Standarddrucker des Arbeitsplatzes für dieses Format, sonst der
	 * erste passende. null, wenn es keinen gibt.
	 */
	P.defaultPrinter = function (format) {
		var list = this.printersFor(format);
		for (var i = 0; i < list.length; i++) {
			if (list[i].default) return list[i];
		}
		return list[0] || null;
	};

	/** Zustand aus Sicht einer Seite, die ein Gerät dieser Klasse braucht. */
	P.deviceState = function (kind) {
		if (this.state !== "ready") return "no_bridge";
		var d = this.findDevice(kind);
		if (!d) return "device_missing";
		if (d.state !== "online") return "device_offline";
		return "ready";
	};

	P.connect = function () {
		var self = this;
		if (this._ws || this._closed) return;
		if (typeof WebSocket === "undefined") { this._setState("no_bridge", "kein WebSocket im Browser"); return; }
		this._setState("connecting");
		var ws;
		try {
			ws = new WebSocket(this.url);
		} catch (e) {
			this._fail(String(e));
			return;
		}
		this._ws = ws;
		var opened = false;
		var guard = setTimeout(function () {
			if (!opened) { try { ws.close(); } catch (e) {} }
		}, CONNECT_TIMEOUT_MS);

		ws.onopen = function () {
			opened = true;
			clearTimeout(guard);
			self._send({ type: "req", id: self._nextId++, method: "hello",
				params: { client: self.client, protocol: PROTOCOL, token: self.token } }, function (err, res) {
				if (err) {
					console.warn("[hwbridge] hello abgelehnt:", err.code, err.message);
					self._fail(err.message);
					try { ws.close(); } catch (e) {}
					return;
				}
				self.info = res;
				self.devices = {};
				(res.devices || []).forEach(function (d) { self.devices[d.id] = d; });
				self._attempt = 0;
				self._setState("ready");
				self._emit("devices", self.devices);
				self._resubscribe();
			});
		};
		ws.onmessage = function (ev) {
			var m;
			try { m = JSON.parse(ev.data); } catch (e) { return; }
			if (m.type === "res") {
				var cb = self._pending[m.id];
				if (cb) {
					delete self._pending[m.id];
					clearTimeout(cb.timer);
					cb.fn(m.ok ? null : (m.error || { code: "unexpected", message: "?" }), m.result);
				}
				return;
			}
			if (m.type === "event") {
				if (m.event === "device.state") {
					var d = self.devices[m.device] || { id: m.device };
					d.kind = m.data.kind || d.kind;
					d.state = m.data.state;
					d.message = m.data.message;
					if (m.data.formats) d.formats = m.data.formats;
					d.default = !!m.data.default;
					if (d.state === "disabled") delete self.devices[m.device];
					else self.devices[m.device] = d;
					self._emit("devices", self.devices);
					// Gerät neu eingerichtet oder neu gestartet: Abo erneuern
					// (serverseitig idempotent).
					if (d.state !== "disabled" && (self._subs[m.device] || self._subs[d.kind])) {
						self._subscribeOne(self._subs[m.device] ? m.device : d.kind);
					}
				} else if (m.device && self.devices[m.device]) {
					self.devices[m.device].last = m.data;
				}
				self._emit(m.event, Object.assign({ device: m.device }, m.data));
			}
		};
		// Browser feuern bei einem Fehler "error" und danach "close", manche
		// Laufzeiten (Node/undici) beim gescheiterten Aufbau nur "error".
		// Beides landet hier, genau einmal pro Socket.
		var ended = false;
		function end() {
			if (ended) return;
			ended = true;
			clearTimeout(guard);
			if (self._ws === ws) self._ws = null;
			for (var id in self._pending) {
				clearTimeout(self._pending[id].timer);
				self._pending[id].fn({ code: "no_bridge", message: "Verbindung zur Bridge getrennt" });
			}
			self._pending = {};
			self._fail(opened ? "Verbindung getrennt" : "Bridge nicht erreichbar");
		}
		ws.onclose = end;
		ws.onerror = function () {
			if (!opened) end();
		};
	};

	P._fail = function (why) {
		var self = this;
		this._ws = null;
		this._setState("no_bridge", why);
		// Erst nach dem Zustandswechsel prüfen: ein Handler darf darin
		// standby() rufen.
		if (this._closed || this._standby) return;
		var wait = BACKOFF_MS[Math.min(this._attempt, BACKOFF_MS.length - 1)];
		this._attempt++;
		clearTimeout(this._timer);
		this._timer = setTimeout(function () { self.connect(); }, wait);
	};

	/** Nicht weiter verbinden, bis retry() gerufen wird. */
	P.standby = function () {
		this._standby = true;
		clearTimeout(this._timer);
	};

	/** Sofort neu versuchen (z.B. Knopf „Erneut verbinden“); beendet standby. */
	P.retry = function () {
		this._standby = false;
		this._attempt = 0;
		clearTimeout(this._timer);
		if (!this._ws) this.connect();
	};

	P.close = function () {
		this._closed = true;
		clearTimeout(this._timer);
		if (this._ws) this._ws.close();
	};

	P._send = function (msg, fn, timeoutMs) {
		var self = this;
		if (!this._ws || this._ws.readyState !== 1) {
			if (fn) fn({ code: "no_bridge", message: "Keine Verbindung zur Hardware Bridge" });
			return;
		}
		if (fn) {
			this._pending[msg.id] = {
				fn: fn,
				timer: setTimeout(function () {
					delete self._pending[msg.id];
					fn({ code: "timeout", message: "Bridge hat nicht geantwortet" });
				}, timeoutMs || CALL_TIMEOUT_MS),
			};
		}
		this._ws.send(JSON.stringify(msg));
	};

	/**
	 * Rohdaten (ZPL) an einen Drucker der Station schicken. data ist Base64
	 * und kommt unverändert am Drucker an. device leer = erster Drucker.
	 */
	P.print = function (device, data, title, format) {
		var params = { format: format || "zpl", data: data, title: title || "" };
		if (device) params.device = device;
		return this.call("printer.print", params, PRINT_TIMEOUT_MS);
	};

	/** Kommando senden. Liefert ein Promise; Fehler haben {code, message}. */
	P.call = function (method, params, timeoutMs) {
		var self = this;
		return new Promise(function (resolve, reject) {
			if (self.state !== "ready") {
				reject({ code: "no_bridge", message: "Keine Verbindung zur Hardware Bridge" });
				return;
			}
			self._send({ type: "req", id: self._nextId++, method: method, params: params || {} }, function (err, res) {
				if (err) reject(err); else resolve(res);
			}, timeoutMs);
		});
	};

	/**
	 * Live-Daten einer Geräteklasse abonnieren ("scale") oder eines Geräts
	 * (ID). Bleibt über Reconnects bestehen. Liefert eine Abmeldefunktion.
	 */
	P.subscribe = function (kindOrId) {
		var self = this;
		this._subs[kindOrId] = (this._subs[kindOrId] || 0) + 1;
		if (this._subs[kindOrId] === 1 && this.state === "ready") this._subscribeOne(kindOrId);
		var done = false;
		return function unsubscribe() {
			if (done) return;
			done = true;
			self._subs[kindOrId]--;
			if (self._subs[kindOrId] <= 0) {
				delete self._subs[kindOrId];
				var kind = kindOrId.indexOf(".") === -1 && !self.devices[kindOrId] ? kindOrId : (self.devices[kindOrId] || {}).kind;
				if (self.state === "ready" && kind) {
					self.call(kind + ".unsubscribe", self.devices[kindOrId] ? { device: kindOrId } : {}).catch(function () {});
				}
			}
		};
	};

	P._subscribeOne = function (kindOrId) {
		var dev = this.devices[kindOrId];
		var kind = dev ? dev.kind : kindOrId;
		var params = dev ? { device: kindOrId } : {};
		var self = this;
		this.call(kind + ".subscribe", params).then(function (res) {
			if (res && res.device && self.devices[res.device] && res.last) self.devices[res.device].last = res.last;
		}).catch(function (err) {
			// device_missing ist ein normaler Zustand (Station ohne Gerät).
			if (err.code !== "device_missing") console.warn("[hwbridge] subscribe " + kindOrId, err);
		});
	};

	P._resubscribe = function () {
		for (var k in this._subs) this._subscribeOne(k);
	};

	root.HardwareBridge = HardwareBridge;
	if (typeof module !== "undefined" && module.exports) module.exports = HardwareBridge;
})(typeof window !== "undefined" ? window : globalThis);
