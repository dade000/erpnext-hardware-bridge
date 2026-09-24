#!/usr/bin/env python3
"""Simuliert eine PCE-PB-Waage an einem Pseudo-Terminal.

    python3 testdata/fake_pce_scale.py /tmp/waage

Legt /tmp/waage als Symlink auf das Slave-Ende an, damit die Bridge es wie
/dev/waage öffnen kann. Beantwortet "Sx" mit "+     1.56kg", "ST" tariert.
Das Gewicht schwankt alle paar Sekunden, damit man Stabil/Bewegt sieht.
"""
import os, pty, sys, time, tty, select

link = sys.argv[1] if len(sys.argv) > 1 else "/tmp/waage"
master, slave = pty.openpty()
tty.setraw(slave)
name = os.ttyname(slave)
try:
    os.unlink(link)
except FileNotFoundError:
    pass
os.symlink(name, link)
print(f"Waage an {name} ({link})", flush=True)

tare, buf, t0 = 0.0, b"", time.time()
def weight():
    phase = int(time.time() - t0) % 8
    return 1.56 if phase < 5 else 1.56 + 0.1 * (phase - 4)  # 5 s ruhig, 3 s Bewegung

while True:
    r, _, _ = select.select([master], [], [], 0.5)
    if not r:
        continue
    buf += os.read(master, 64)
    while b"\n" in buf:
        line, buf = buf.split(b"\n", 1)
        cmd = line.strip().decode(errors="ignore")
        if cmd == "Sx":
            w = weight() - tare
            os.write(master, f"{'+' if w >= 0 else '-'}{abs(w):10.2f}kg\r\n".encode())
        elif cmd == "ST":
            tare = weight()
