#!/usr/bin/env python3
"""Simuliert einen metratec DeskID UHF v2 (AT-Protokoll) an einem Pseudo-Terminal.

    python3 testdata/fake_deskid_uhf.py /tmp/rfid

Legt /tmp/rfid als Symlink auf das Slave-Ende an. Ein Tag liegt auf; nachgebaut
ist nur, was der Treiber benutzt (ATE0, ATI, AT+BINV, AT+INVS, AT+PWR, AT+INV,
AT+MSK, AT+READ, AT+WRT). 64 Byte Nutzerspeicher, anfangs leer.
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
usr = bytearray(64)
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
            out = [f"+READ: {EPC},OK,{binascii.hexlify(usr[s:s + n]).decode().upper()}" if s + n <= len(usr)
                   else f"+READ: {EPC},MEMORY OVERRUN"]
        elif name_ == "AT+WRT":
            _, s, d = arg.split(",")
            s, d = int(s), binascii.unhexlify(d)
            if s + len(d) <= len(usr):
                usr[s:s + len(d)] = d
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
