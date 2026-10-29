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

# Decompressed-size caps, checked against each member's declared size before
# anything is read: the 10 MiB upload cap bounds only the compressed bytes,
# and a deflate bomb expands ~1000:1 inside the shared container. zipfile
# stops at the declared size (and fails the CRC check on a lie), so the
# header value is safe to trust here.
MAX_MEMBER_BYTES = 50 << 20
MAX_TOTAL_BYTES = 200 << 20
# Length bound on a member after each op: an empty-match pattern (or empty
# plain find) with a large replace grows the text multiplicatively.
MAX_OUTPUT_CHARS = MAX_MEMBER_BYTES


def check_sizes(infolist):
    total = 0
    for info in infolist:
        if info.file_size > MAX_MEMBER_BYTES:
            raise SystemExit(
                f"member {info.filename} is {info.file_size} bytes uncompressed, "
                f"over the {MAX_MEMBER_BYTES}-byte cap"
            )
        total += info.file_size
    if total > MAX_TOTAL_BYTES:
        raise SystemExit(f"archive is {total} bytes uncompressed, over the {MAX_TOTAL_BYTES}-byte cap")


def too_long(member):
    return SystemExit(f"edit on member {member} would grow it past {MAX_OUTPUT_CHARS} characters")


def bounded_sub(pattern, replace, text, member):
    # re.sub with a running length check: the result is refused while it is
    # being built, never after it already filled memory.
    out = []
    size = 0
    last = 0
    for m in re.finditer(pattern, text, flags=re.DOTALL):
        piece = text[last:m.start()] + m.expand(replace)
        size += len(piece)
        if size > MAX_OUTPUT_CHARS:
            raise too_long(member)
        out.append(piece)
        last = m.end()
    out.append(text[last:])
    if size + len(text) - last > MAX_OUTPUT_CHARS:
        raise too_long(member)
    return "".join(out)


def apply_ops(text, ops, member=""):
    for op in ops:
        find = op["find"]
        replace = op["replace"]
        if op.get("regex"):
            text = bounded_sub(find, replace, text, member)
        else:
            # str.replace is computed in one step, so project its length first.
            grown = len(text) + text.count(find) * (len(replace) - len(find))
            if grown > MAX_OUTPUT_CHARS:
                raise too_long(member)
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
        check_sizes(zin.infolist())
        names = set(zin.namelist())
        missing = [m for m in by_member if m not in names]
        if missing:
            raise SystemExit(f"member(s) not found in archive: {', '.join(missing)}")

        with zipfile.ZipFile(output_path, "w", zipfile.ZIP_DEFLATED) as zout:
            for info in zin.infolist():
                data = zin.read(info.filename)
                if info.filename in by_member:
                    text = apply_ops(data.decode("utf-8"), by_member[info.filename], info.filename)
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
