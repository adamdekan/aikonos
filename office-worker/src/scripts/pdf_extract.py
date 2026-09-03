#!/usr/bin/env python3
"""Fixed helper (NOT model-authored): pdf.extract — layout-preserving
text/tables via pdfplumber, pdftotext fallback, OCR fallback (pytesseract +
pdf2image) when the text layer is empty. Declarative params only.

argv: <input.pdf> <params.json> <output.json>

params.json: {"ocr": true|false|"auto" (default "auto")}

Subprocess calls here (pdftotext, poppler via pdf2image) inherit this
process's process group — the injected `spawn` that launched this script
already made it a group leader (job.js), so a job timeout's group-kill
reaches these grandchildren too. Do not start them in a new session.
"""
import json
import subprocess
import sys


def try_pdfplumber(path):
    import pdfplumber

    text_parts = []
    tables = []
    with pdfplumber.open(path) as pdf:
        for page in pdf.pages:
            text_parts.append(page.extract_text(layout=True) or "")
            tables.extend(page.extract_tables())
    return "\n".join(text_parts), tables


def try_pdftotext(path):
    out = subprocess.run(["pdftotext", "-layout", path, "-"], capture_output=True, check=True)
    return out.stdout.decode("utf-8", errors="replace")


def try_ocr(path):
    from pdf2image import convert_from_path
    import pytesseract

    images = convert_from_path(path)
    return "\n".join(pytesseract.image_to_string(img) for img in images)


def main():
    input_path, params_path, output_path = sys.argv[1], sys.argv[2], sys.argv[3]
    with open(params_path, "r", encoding="utf-8") as f:
        params = json.load(f)
    ocr_mode = params.get("ocr", "auto")
    force_ocr = ocr_mode is True or ocr_mode == "true"

    text = ""
    tables = []
    method = None

    if not force_ocr:
        try:
            text, tables = try_pdfplumber(input_path)
            method = "pdfplumber"
        except Exception:
            text = ""
        if not text.strip():
            try:
                text = try_pdftotext(input_path)
                method = "pdftotext"
            except Exception:
                text = ""

    if force_ocr or (ocr_mode == "auto" and not text.strip()):
        text = try_ocr(input_path)
        method = "ocr"

    result = {"text": text, "tables": tables, "method": method}
    with open(output_path, "w", encoding="utf-8") as f:
        json.dump(result, f)


if __name__ == "__main__":
    main()
