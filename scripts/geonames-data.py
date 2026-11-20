# Copyright 2026 ziul510
# SPDX-License-Identifier: Apache-2.0

"""Convert the pinned GeoNames snapshot to an offline city index."""

import gzip
import hashlib
from pathlib import Path
import sys
import zipfile

source = Path(sys.argv[1])
out = Path(sys.argv[2])
snapshot = {
    "cities1000.zip": "a45382190ece72812dc0397d58100f69c1c5e4ae73d7a0e2d022b2f675db9581",
    "admin1CodesASCII.txt": "2e4867f5397acb056ea26377601b57af37cb4c81399770225018e9e439e91ad3",
}
for name, expected in snapshot.items():
    with (source / name).open("rb") as f:
        if hashlib.file_digest(f, "sha256").hexdigest() != expected:
            raise SystemExit(f"Unexpected snapshot for {name}")

admin = {}
for line in (source / "admin1CodesASCII.txt").read_text(encoding="utf8").splitlines():
    key, label, *_ = line.split("\t")
    # The city rows use country and admin codes. The accent rules use region names.
    admin[key] = label

with zipfile.ZipFile(source / "cities1000.zip") as archive:
    rows = []
    for line in archive.read("cities1000.txt").decode("utf8").splitlines():
        fields = line.split("\t")
        area = admin.get(f"{fields[8]}.{fields[10]}", "")
        # The Go loader expects latitude, longitude, country code, and region in this order.
        rows.append("\t".join((fields[4], fields[5], fields[8], area)))
# Omit the gzip timestamp so the same snapshot produces the same index.
out.write_bytes(gzip.compress(("\n".join(rows) + "\n").encode(), mtime=0))
