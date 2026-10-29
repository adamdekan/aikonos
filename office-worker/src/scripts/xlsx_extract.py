#!/usr/bin/env python3
"""Fixed helper (NOT model-authored): xlsx.extract — sheet/range -> CSV/JSON
summary via openpyxl (cell/range reads) + pandas (serialization). Declarative
params only, no script execution.

argv: <input.xlsx> <params.json> <output.json>

params.json: {"sheet": str (optional, default first sheet), "range": str
(optional, e.g. "A1:C10"), "format": "csv"|"json" (default "csv")}
"""
import json
import sys

import openpyxl
import pandas as pd
from openpyxl.utils.cell import range_boundaries

# Read-only iter_rows pads every empty cell of the requested rectangle, so a
# caller range like A1:XFD1048576 materialises ~17 billion cells. The range
# is clamped to the sheet's used area when it is known, and the cell count is
# capped while rows stream in, because read-only mode can report that area as
# unknown (None) or wrong.
MAX_CELLS = 1_000_000


def main():
    input_path, params_path, output_path = sys.argv[1], sys.argv[2], sys.argv[3]
    with open(params_path, "r", encoding="utf-8") as f:
        params = json.load(f)

    sheet_name = params.get("sheet")
    range_str = params.get("range")
    fmt = params.get("format", "csv")

    # data_only=True reads cached formula results, not formula source text —
    # correct for an extract (recalculating live is xlsx.recalc's job, CP3).
    wb = openpyxl.load_workbook(input_path, data_only=True, read_only=True)
    ws = wb[sheet_name] if sheet_name else wb.worksheets[0]

    bounds = {}
    if range_str:
        min_col, min_row, max_col, max_row = range_boundaries(range_str)
        # A whole-column/row range ("A:C", "1:5") leaves the open side None.
        if ws.max_row is not None and max_row is not None:
            max_row = min(max_row, ws.max_row)
        if ws.max_column is not None and max_col is not None:
            max_col = min(max_col, ws.max_column)
        bounds = {"min_row": min_row, "max_row": max_row, "min_col": min_col, "max_col": max_col}

    rows = []
    cells = 0
    for row in ws.iter_rows(values_only=True, **bounds):
        cells += max(len(row), 1)
        if cells > MAX_CELLS:
            raise SystemExit(f"extract exceeds the {MAX_CELLS}-cell cap; pass a smaller range")
        rows.append(row)

    df = pd.DataFrame(rows)
    content = df.to_json(orient="values") if fmt == "json" else df.to_csv(index=False, header=False)

    result = {"sheet": ws.title, "rows": len(rows), "format": fmt, "content": content}
    with open(output_path, "w", encoding="utf-8") as f:
        json.dump(result, f)


if __name__ == "__main__":
    main()
