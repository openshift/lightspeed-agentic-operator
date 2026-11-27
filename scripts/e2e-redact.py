#!/usr/bin/env python3
"""Redact known CI credentials from runner output.

Filters stdin to stdout by default. With ``--in-place FILE...``, rewrites the
given files instead.
"""
import os
from pathlib import Path
import sys

keys = [os.environ.get(name, "") for name in ("VLLM_API_KEY", "HUGGING_FACE_HUB_TOKEN")]
path = os.environ.get("E2E_PROVIDER_KEY_PATH") or os.environ.get("OPENAI_PROVIDER_KEY_PATH")
if path:
    keys.append(Path(path).read_text().strip())
keys = sorted(filter(None, keys), key=len, reverse=True)


def redact(text):
    for key in keys:
        text = text.replace(key, "[REDACTED]")
    return text


if sys.argv[1:2] == ["--in-place"]:
    for name in sys.argv[2:]:
        target = Path(name)
        original = target.read_text(errors="surrogateescape")
        redacted = redact(original)
        if redacted != original:
            target.write_text(redacted, errors="surrogateescape")
else:
    for line in sys.stdin:
        sys.stdout.write(redact(line))
