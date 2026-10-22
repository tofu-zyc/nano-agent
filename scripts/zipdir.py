#!/usr/bin/env python3
"""把一个目录打成 zip，保留目录名作为压缩包里的顶层目录。"""
import os
import sys
import zipfile

d, z = sys.argv[1], sys.argv[2]
base = os.path.dirname(os.path.abspath(d))
with zipfile.ZipFile(z, "w", zipfile.ZIP_DEFLATED) as f:
    for root, _, files in os.walk(d):
        for name in files:
            p = os.path.join(root, name)
            f.write(p, os.path.relpath(p, base))
