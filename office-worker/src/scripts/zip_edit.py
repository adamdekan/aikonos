#!/usr/bin/env python3
"""Fixed helper (NOT model-authored) applying declarative string/regex
replace ops to named members of a zip archive. Shared by docx.edit and
pptx.edit — both formats are OOXML zip archives, so one engine covers both
. Zero new node deps: python's stdlib zipfile covers zip
read/write (per the office-doc-tools brief's zero-new-dep steer).

argv: <input.zip> <ops.json> <output.zip>

ops.json is a JSON array of {"member": str, "find": str, "replace": str,
"regex": bool (optional)}. Ops targeting the same member apply in order.
"""
import json
import re
import sys
import zipfile


def apply_ops(text, ops):
    for op in ops:
        find = op["find"]
        replace = op["replace"]
        if op.get("regex"):
            text = re.sub(find, replace, text, flags=re.DOTALL)
        else:
            text = text.replace(find, replace)
    return text


def main():
    input_path, ops_path, output_path = sys.argv[1], sys.argv[2], sys.argv[3]
    with open(ops_path, "r", encoding="utf-8") as f:
        ops = json.load(f)

    by_member = {}
    for op in ops:
        by_member.setdefault(op["member"], []).append(op)

    with zipfile.ZipFile(input_path, "r") as zin:
        names = set(zin.namelist())
        missing = [m for m in by_member if m not in names]
        if missing:
            raise SystemExit(f"member(s) not found in archive: {', '.join(missing)}")

        with zipfile.ZipFile(output_path, "w", zipfile.ZIP_DEFLATED) as zout:
            for info in zin.infolist():
                data = zin.read(info.filename)
                if info.filename in by_member:
                    text = apply_ops(data.decode("utf-8"), by_member[info.filename])
                    data = text.encode("utf-8")
                zout.writestr(info, data)

    # Sanity-validate: the repacked zip must be readable and structurally
    # intact before the handler ships it back as an output artifact.
    with zipfile.ZipFile(output_path, "r") as check:
        bad_member = check.testzip()
        if bad_member is not None:
            raise SystemExit(f"repacked zip failed integrity check at member {bad_member}")


if __name__ == "__main__":
    main()
