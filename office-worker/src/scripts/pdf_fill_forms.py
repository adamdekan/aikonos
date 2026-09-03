#!/usr/bin/env python3
"""Fixed helper (NOT model-authored): pdf.transform's declarative
"fill-forms" op — sets AcroForm field values via pypdf. No script execution;
field values are data, not code.

argv: <input.pdf> <fields.json> <output.pdf>

fields.json: a flat JSON object of field name -> value (string).
"""
import json
import sys

from pypdf import PdfReader, PdfWriter


def main():
    input_path, fields_path, output_path = sys.argv[1], sys.argv[2], sys.argv[3]
    with open(fields_path, "r", encoding="utf-8") as f:
        fields = json.load(f)

    reader = PdfReader(input_path)
    writer = PdfWriter()
    writer.append(reader)

    # update_page_form_field_values is per-page and silently skips fields not
    # present on that page — applying the full set to every page is correct
    # for the common single-page-form case and safe for multi-page forms too.
    for page in writer.pages:
        writer.update_page_form_field_values(page, fields)

    with open(output_path, "wb") as f:
        writer.write(f)


if __name__ == "__main__":
    main()
