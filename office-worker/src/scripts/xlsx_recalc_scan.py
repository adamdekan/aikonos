#!/usr/bin/env python3
"""Fixed helper (NOT model-authored): scans a workbook already recalculated
by soffice (xlsx.recalc's handler runs this after a `--convert-to xlsx`
round-trip under the always-recalculate registry profile — src/handlers/
soffice.js) for cells whose cached value is an Excel error token.

argv: <recalculated.xlsx> <output.json>

output.json: {"errors": [{"sheet": str, "cell": str, "error": str}],
"error_count": int}
"""
import json
import sys

import openpyxl

# The fixed set of error tokens Excel/Calc can leave as a formula's cached
# value — openpyxl surfaces these as plain strings under data_only=True.
ERROR_TOKENS = {
    "#REF!",
    "#DIV/0!",
    "#VALUE!",
    "#NAME?",
    "#NULL!",
    "#NUM!",
    "#N/A",
    "#GETTING_DATA",
    "#SPILL!",
    "#CALC!",
}


def main():
    input_path, output_path = sys.argv[1], sys.argv[2]
    wb = openpyxl.load_workbook(input_path, data_only=True)

    errors = []
    for sheet in wb.worksheets:
        for row in sheet.iter_rows():
            for cell in row:
                if isinstance(cell.value, str) and cell.value in ERROR_TOKENS:
                    errors.append({"sheet": sheet.title, "cell": cell.coordinate, "error": cell.value})

    result = {"errors": errors, "error_count": len(errors)}
    with open(output_path, "w", encoding="utf-8") as f:
        json.dump(result, f)


if __name__ == "__main__":
    main()
