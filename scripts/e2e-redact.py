#!/usr/bin/env python3
"""Redact known CI credentials from disconnected runner output."""
import os
from pathlib import Path
import sys

keys = [os.environ.get(name, "") for name in ("VLLM_API_KEY", "HUGGING_FACE_HUB_TOKEN")]
path = os.environ.get("E2E_PROVIDER_KEY_PATH") or os.environ.get("OPENAI_PROVIDER_KEY_PATH")
if path:
    keys.append(Path(path).read_text().strip())
for line in sys.stdin:
    for key in sorted(filter(None, keys), key=len, reverse=True):
        line = line.replace(key, "[REDACTED]")
    sys.stdout.write(line)
