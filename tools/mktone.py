#!/usr/bin/env python3
"""Writes the selftest tone: 6 s, 440 Hz, -20 dBFS, 48 kHz stereo s32le.

The low 16 bits of every sample are zero, so the tone stays quiet even if a
device misreads it as 16-bit audio.
Usage: mktone.py OUTFILE
"""
import math
import struct
import sys

RATE, SECONDS, AMP16 = 48000, 6, 3277  # 3277/32768 = -20 dBFS

out = bytearray()
for n in range(RATE * SECONDS):
    v = int(round(AMP16 * math.sin(2 * math.pi * 440 * n / RATE))) << 16
    out += struct.pack("<ii", v, v)
with open(sys.argv[1], "wb") as f:
    f.write(out)
