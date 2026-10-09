#!/usr/bin/env python3
"""Simuliert einen metratec DeskID UHF v2 (AT-Protokoll) an einem Pseudo-Terminal.

    python3 testdata/fake_deskid_uhf.py /tmp/rfid

Legt /tmp/rfid als Symlink auf das Slave-Ende an. Ein Tag liegt auf; nachgebaut
ist nur, was der Treiber benutzt (ATE0, ATI, AT+BINV, AT+INVS, AT+PWR, AT+INV,
AT+MSK, AT+READ, AT+WRT). Speicher wie ein gelieferter EM4425: 32 Byte
UHF-Nutzerspeicher ab 0, der NFC-Bereich (160 Byte) ab Adresse 320 (Wort A0h),
dort steht schon eine Adresse, wie von einer Handy-App geschrieben.
"""
import binascii, os, pty, sys, tty

link = sys.argv[1] if len(sys.argv) > 1 else "/tmp/rfid"
master, slave = pty.openpty()
tty.setraw(slave)
name = os.ttyname(slave)
try:
    os.unlink(link)
except FileNotFoundError:
    pass
os.symlink(name, link)
print(f"DeskID-Simulator an {link} -> {name}", flush=True)

TID = "E2801191A5030060A1B2C3D4"
EPC = "3034257BF468D480000003EC"
usr = bytearray(32)
HF_START = 320
hf = bytearray(160)
_payload = b"\x04" + b"example.com/handy"  # 04 = "https://"
_msg = bytes([0xD1, 0x01, len(_payload), ord("U")]) + _payload
_tlv = bytes([0x03, len(_msg)]) + _msg + b"\xfe"
_ndef = bytes([0xE1, 0x40, (len(_tlv) + 7) // 8, 0x00]) + _tlv
hf[:len(_ndef)] = _ndef


def region(s, n):
    """(Puffer, Index) für [s, s+n) oder None."""
    if 0 <= s and s + n <= len(usr):
        return usr, s
    if HF_START <= s and s + n <= HF_START + len(hf):
        return hf, s - HF_START
    return None


echo = True
invs = "0,1,0,0,0,ALL,DUAL,-100"
buf = b""


def send(s):
    os.write(master, s.encode())


while True:
    buf += os.read(master, 1024)
    while b"\r" in buf:
        line, buf = buf.split(b"\r", 1)
        cmd = line.decode(errors="replace").strip()
        if not cmd:
            continue
        name_, _, arg = cmd.partition("=")
        out, ok = [], True
        if name_ == "ATE0":
            echo = False
        elif name_ == "ATI":
            out = ["+SW: DeskID_UHF_v2_E 0105", "+HW: DeskID_UHF_v2_E 0100", "+SERIAL: 2026100900000001"]
        elif name_ == "AT+BINV":
            ok, out = False, ["+BINV: <Not running>"]
        elif name_ == "AT+INVS?":
            out = ["+INVS: " + invs]
        elif name_ == "AT+INVS":
            invs = arg
        elif name_ in ("AT", "AT+PWR", "AT+MSK"):
            pass
        elif name_ == "AT+INV":
            out = [f"+INV: {EPC},{TID},-51", "+INV: <ROUND FINISHED, ANT=1>"]
        elif name_ == "AT+READ":
            _, s, n = arg.split(",")
            s, n = int(s), int(n)
            if n > 16:  # wie der echte DeskID UHF v2: 32 Byte sind schon zu viel
                ok, out = False, ["+READ: <Read length too big>"]
            elif arg.startswith("TID"):
                out = [f"+READ: {EPC},OK,{TID[2 * s:2 * (s + n)]}"]
            else:
                r = region(s, n)
                out = [f"+READ: {EPC},OK,{binascii.hexlify(r[0][r[1]:r[1] + n]).decode().upper()}" if r
                       else f"+READ: {EPC},MEMORY OVERRUN"]
        elif name_ == "AT+WRT":
            _, s, d = arg.split(",")
            s, d = int(s), binascii.unhexlify(d)
            r = region(s, len(d))
            if r:
                r[0][r[1]:r[1] + len(d)] = d
                out = [f"+WRT: {EPC},OK"]
            else:
                out = [f"+WRT: {EPC},MEMORY OVERRUN"]
        else:
            ok, out = False, ["+ERR: <unknown command>"]
        if echo and ok:
            send(cmd + "\r\n")
        for l in out:
            send(l + "\r\n")
        send("OK\r\n" if ok else "ERROR\r\n")
