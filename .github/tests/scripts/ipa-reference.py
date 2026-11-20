# Copyright 2026 ziul510
# SPDX-License-Identifier: Apache-2.0

"""Capture the reference IPA output for 300 fixed menu announcements."""

from pathlib import Path
import json
import sys

# Import the legacy engine without writing bytecode into the reference tree.
sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(sys.argv[1]).resolve()))

from phonemizer_engine import phonemize

actions = [
    "Request boarding", "Connect jetway", "Disconnect ground power",
    "Select the operator", "Request potable water", "Request de-icing",
    "Prepare for pushback", "Close passenger stairs", "Open catering menu",
    "Cancel refueling", "Set parking position", "Request baggage unloading",
]
contexts = [
    "at stand 3", "at gate B12", "for runway 04", "via terminal A",
    "with Swissport", "with Menzies Aviation", "with dnata", "with AviaPartner",
    "near São Paulo", "at Reykjavik", "at LaGuardia", "next to Côte d’Azur",
    "at gate 19B", "use the GPU", "at position 42", "for flight 101",
    "next to the A320", "at the auxiliary apron", "for the Boeing 737",
    "at Ponta Delgada", "with AeroGround", "near Isavia", "at stand X-2",
    "at gate Zulu 8", "at parking 250",
]

# Pair each action with every context so each pronunciation challenge repeats across commands.
samples = [f"{action} {context}." for action in actions for context in contexts]
assert len(samples) == 300 and len(set(samples)) == 300
records = [{"text": text, "ipa": phonemize(text, disable_gpu=True)} for text in samples]
print(json.dumps(records, ensure_ascii=True, indent=2))
