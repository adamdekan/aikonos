#!/usr/bin/env python3
"""Fixed helper (NOT model-authored): docx.extract/pptx.extract's
`format:"xml"` mode. Reads one named archive member and, if a `pattern` is
given, returns matching regions with surrounding context; otherwise returns
the member content. Always bounded by `echo_cap` — never the whole member
for a large file.

argv: <input.zip> <params.json> <output.json>

params.json: {"member": str, "pattern": str (optional, regex), "context_chars":
int (default 200), "echo_cap": int (default 20000, chars)}
"""
import json
import re
import sys
import zipfile

DEFAULT_CONTEXT_CHARS = 200
DEFAULT_ECHO_CAP = 20000


def main():
    input_path, params_path, output_path = sys.argv[1], sys.argv[2], sys.argv[3]
    with open(params_path, "r", encoding="utf-8") as f:
        params = json.load(f)

    member = params["member"]
    pattern = params.get("pattern")
    context_chars = int(params.get("context_chars") or DEFAULT_CONTEXT_CHARS)
    echo_cap = int(params.get("echo_cap") or DEFAULT_ECHO_CAP)

    with zipfile.ZipFile(input_path, "r") as z:
        if member not in z.namelist():
            raise SystemExit(f"member not found in archive: {member}")
        text = z.read(member).decode("utf-8")

    result = {"member": member, "member_length": len(text)}

    if pattern:
        regions = []
        total = 0
        truncated = False
        for m in re.finditer(pattern, text, re.DOTALL):
            start = max(0, m.start() - context_chars)
            end = min(len(text), m.end() + context_chars)
            region = text[start:end]
            if total + len(region) > echo_cap:
                truncated = True
                break
            regions.append({"start": start, "end": end, "text": region})
            total += len(region)
        result["matches"] = regions
        result["truncated"] = truncated
    else:
        result["content"] = text[:echo_cap]
        result["truncated"] = len(text) > echo_cap

    with open(output_path, "w", encoding="utf-8") as f:
        json.dump(result, f)


if __name__ == "__main__":
    main()
