# Copyright 2026 ziul510
# SPDX-License-Identifier: Apache-2.0

"""Run the frozen corpus through the supplied Python reference without changing it."""
import ast
import json
from pathlib import Path
import sys
from types import SimpleNamespace

sys.dont_write_bytecode = True
reference = Path(sys.argv[1]).resolve()
sys.path.insert(0, str(reference))

import preset_manager
import sim_bridge
import stt_engine

repo = Path(__file__).resolve().parents[1]
# Use the same corpus as the Go differential tests
data = json.loads((repo / "testdata/corpus.json").read_text(encoding="utf8"))
words = json.loads((reference / "words.json").read_text(encoding="utf8"))
# Compile the reference bias rules from that snapshot's vocabulary
normalized = stt_engine.BiasNormalizer(words)
cfg = {"presets": data["presets"], "enable_menu_matching": True}
manager = preset_manager.PresetManager(cfg)
# Extract routing from the legacy class without launching its GUI
source = ast.parse((reference / "main.py").read_text(encoding="utf8"))
# Find only the old locked routing method The rest of the application isn't needed
command = next(
    method for cls in source.body if isinstance(cls, ast.ClassDef)
    for method in cls.body if isinstance(method, ast.FunctionDef) and method.name == "_logic_locked"
)
# Compile the extracted method with a small fake namespace for logging
module = ast.fix_missing_locations(ast.Module(body=[command], type_ignores=[]))
ns = {"debug_logger": SimpleNamespace(debug=lambda *args: None)}
exec(compile(module, str(reference / "main.py"), "exec"), ns)

def route(text, raw):
    result = {"Kind": "unknown", "Preset": "", "Services": None}
    # Capture the selected action as the same fields the Go fixture compares
    def select(kind, preset="", services=None):
        result.update(Kind=kind, Preset=preset, Services=services)
    # The reference signals ambiguous commands through its logger
    def log(sender, message):
        if message == "Multiple cmds detected":
            select("ambiguous")
    # Stub out simulator and audio actions so the fixture captures routing decisions
    state = SimpleNamespace(
        resource_error=None, resources_ready=True, _is_sim_ready=lambda: True, too_fast=False,
        cfg=cfg, _gsx_overlay_visible=False, preset_manager=manager, words=words,
        _execute_preset=lambda p: select("preset", p["name"], p["services"]),
        _execute_gsx=lambda service: select("service", "", [service]),
        _execute_gsx_menu_matching=lambda text: select("menu"),
        _log_async=log,
        audio=SimpleNamespace(play_greeting=lambda: select("greeting"), play_recog_error=lambda: None),
    )
    ns["_logic_locked"](state, text, raw_text=raw)
    return result

# Use the legacy order of text corrections, NATO filtering, and bias normalization
def normalize(raw):
    return normalized.normalize(stt_engine.apply_nato_phonetic_filter(stt_engine.apply_stt_text_corrections(raw)))

result = {
    "hotwords": sorted(stt_engine.collect_hotword_terms(words)),
    "commands": [{"Text": normalize(raw), "Route": route(normalize(raw), raw)} for raw in data["commands"]],
    "menus": [],
}
# Save both direct menu scores and the separate transcript correction results
for case in data["menus"]:
    idx, _, score = sim_bridge.find_best_menu_match(case["text"], case["options"])
    correction, similarity = sim_bridge.find_best_menu_text_correction(case["text"], case["options"])
    result["menus"].append({"Index": idx if idx is not None else -1, "Points": score, "Correction": correction or "", "Similarity": similarity})
print(json.dumps(result, ensure_ascii=True, indent=2))
