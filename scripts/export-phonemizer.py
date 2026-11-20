# Copyright 2026 ziul510
# SPDX-License-Identifier: Apache-2.0

"""Export the pinned checkpoint for the Kokoro worker. Build-time only."""

import hashlib
import json
from pathlib import Path
import sys

import torch
from dp.model.model import load_checkpoint
from dp.preprocessing.text import LanguageTokenizer, Preprocessor, SequenceTokenizer

ORIGINAL_SHA256 = "5bc24893f458b8d7ee7cf18011e6b38be3453e3c83eda291c1296d153423cdb4"

checkpoint_path, output_path = map(Path, sys.argv[1:])
# Check the exact checkpoint before deserializing any Python objects.
with checkpoint_path.open("rb") as source:
    if hashlib.file_digest(source, "sha256").hexdigest() != ORIGINAL_SHA256:
        raise SystemExit("Unexpected OpenPhonemizer checkpoint")
output_path.mkdir(parents=True, exist_ok=True)
# The verified checkpoint contains Python tokenizer objects.
with torch.serialization.safe_globals([LanguageTokenizer, Preprocessor, SequenceTokenizer]):
    model, checkpoint = load_checkpoint(checkpoint_path, device="cpu")
prep = checkpoint["preprocessor"]

# Adapt the checkpoint's dictionary input and logits to the worker's tensor interface.
class Graph(torch.nn.Module):
    def __init__(self, model):
        super().__init__()
        # Register the checkpoint as a child module so export includes its weights.
        self.model = model

    def forward(self, sequence):
        values = self.model({"text": sequence})
        # The Go decoder needs token IDs and can leave the logits inside the graph.
        return values.argmax(dim=-1)

# The Go runtime assumes the fixed transformer export layout.
if checkpoint["config"]["model"]["type"] != "transformer":
    raise SystemExit("Unexpected checkpoint graph architecture")

# Keep the English tokenizer IDs, case rules, boundaries, and repeat count used by the checkpoint.
def tokenizer(tok):
    return dict(
        tokens=tok.token_to_idx, end=tok.end_index, start=tok._get_start_index("en_us"),
        repeated=tok.char_repeats, lower=tok.lowercase, bounded=tok.append_start_end,
        special=sorted(tok.special_tokens),
    )

# Bundle the English dictionary for words that can skip model inference.
data = dict(
    text=tokenizer(prep.text_tokenizer), phones=tokenizer(prep.phoneme_tokenizer),
    dictionary=(checkpoint.get("phoneme_dict") or {}).get("en_us", {}),
)
# The worker needs the tokenizer rules and dictionary at runtime.
(output_path / "phonemizer.json").write_text(json.dumps(data, ensure_ascii=False), encoding="utf8")
# Keep the export width in sync with contextSize in the Kokoro worker.
sequence = torch.zeros((1, 256), dtype=torch.long)
ids = prep.text_tokenizer("jetway", "en_us")
sequence[0, :len(ids)] = torch.tensor(ids)
torch.set_num_threads(2)
# Export a self-contained graph that can load from the installed worker directory.
torch.onnx.export(
    Graph(model).eval(), (sequence,), str(output_path / "phonemizer.onnx"),
    input_names=["sequence"], output_names=["tokens"], opset_version=17,
    external_data=False, dynamo=False,
)
