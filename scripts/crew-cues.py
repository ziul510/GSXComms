# Copyright 2026 ziul510
# SPDX-License-Identifier: Apache-2.0

"""Generate the English fallback crew recordings from the pin in the model manifest."""

import base64
import gzip
import json
from pathlib import Path
import struct
import subprocess
import sys

worker_path, model_root, output = (Path(p).resolve() for p in sys.argv[1:])
output.mkdir(parents=True, exist_ok=True)
# Keep cue keys aligned with the service IDs and status events used in playback
lines = {
    "greeting": "Ground crew here. Go ahead.",
    "connected": "Ground crew standing by.",
    "error": "Unable to understand. Please say again.",
    "gpu": "Ground power request received.",
    "jetway": "Jetway request received.",
    "stairs": "Passenger stairs request received.",
    "catering": "Catering request received.",
    "refueling": "Refueling request received.",
    "boarding": "Boarding request received.",
    "deboarding": "Deboarding request received.",
    "pushback": "Pushback request received.",
    "deice": "De icing request received.",
    "lavatory": "Lavatory service request received.",
    "water": "Water service request received.",
    "cleaning": "Cleaning request received.",
}
# Use the normal speech worker protocol to generate the same PCM the app plays
p = subprocess.Popen([str(worker_path)], stdin=subprocess.PIPE, stdout=subprocess.PIPE)
index = 0

def call(op, **data):
    global index
    # Give each request its own ID so a mismatched reply can't be saved as a cue
    index += 1
    payload = json.dumps(dict(version=1, id=index, op=op, **data)).encode()
    # Match the worker's little-endian frame length before each JSON message
    p.stdin.write(struct.pack("<I", len(payload)) + payload)
    p.stdin.flush()
    size = struct.unpack("<I", p.stdout.read(4))[0]
    # Apply the same response size limit as the Go framing code
    assert 0 < size <= 16 << 20
    reply = json.loads(p.stdout.read(size))
    if reply["id"] != index or reply["op"] != op or reply.get("error"):
        raise RuntimeError(reply.get("error") or "unexpected worker reply")
    return reply

try:
    call("hello")
    paths = dict()
    # The worker indexes model files by basename even when the cache uses subdirectories
    for relative in ("onnx/model.onnx","voices/am_adam.bin","tokenizer.json"):
        path = model_root / relative
        paths[path.name] = str(path)
    call("load", paths=paths)
    for name, text in lines.items():
        reply = call("synthesize", text=text)
        pcm = base64.b64decode(reply["pcm"])
        # Require the app's 24 kHz rate and at least a quarter second of PCM16
        assert reply["sample_rate"] == 24000 and len(pcm) >= 12000
        # Keep gzip timestamps fixed so identical PCM produces identical assets
        (output / f"{name}.pcm.gz").write_bytes(gzip.compress(pcm, mtime=0))
        print(f"Generated {name}", flush=True)
    call("shutdown")
    # Give the worker a moment to release its model after the shutdown reply
    p.wait(timeout=3)
finally:
    # A failed generation can leave the worker waiting for another request
    if p.poll() is None:
        p.kill()
        p.wait()
