# office-worker wire contract

Internal HTTP API, `office` network only. Every route except `/healthz`
requires the header `X-Aikonos-Office-Token` to equal the worker's
`AIKONOS_OFFICE_WORKER_TOKEN` (the broker sends the same env value); a
missing or wrong token is a `401`, checked before the body is read. The
worker refuses to start with the token unset unless `AIKONOS_DEV_MODE=true`.
Checkpoint 4's Go
toolproxy client is written against this file; anything here is pinned by
this service's own tests (`test/*.test.js`).

## Routes

| Route | Notes |
|---|---|
| `GET /healthz` | Always 200 `{"ok":true}`, no token needed. Compose healthcheck + `compose:verify`. |
| `POST /v1/<format>/<op>` | One route per tool op, 15 total (see table below). |

### Ops

| format | ops |
|---|---|
| `docx` | `create`, `edit`, `extract` |
| `xlsx` | `create`, `edit`, `extract`, `recalc` |
| `pptx` | `create`, `edit`, `extract`, `thumbnail` |
| `pdf` | `create`, `transform`, `extract` |
| `office` | `convert` |

A known-but-unimplemented op (checkpoints 2-3 fill these in) returns `501`
`{"error": "\"<format>/<op>\" is not implemented yet"}`. An op or format not in
the table above returns `404` `{"error": "unknown operation \"<format>/<op>\""}`.
A non-`POST`/non-matching path also returns `404`.

## Request shape

`multipart/form-data`:

- One or more file parts — document bytes. Field name and `filename` are
  op-specific (checkpoints 2-4 define them per op); the worker does not
  inspect field names beyond what a given op's handler reads.
- A `params` **field** (not a file part) — a JSON-encoded string with the
  op's parameters. Always includes `max_output_bytes`: the workspace file-size
  cap the broker enforces (`workspacefs.MaxFileBytes`), checked against every
  output artifact before the response is built.

Malformed `params` JSON, `params` that is not a JSON object, or a request the
multipart parser rejects, returns `400` `{"error": "..."}`. The whole body
must arrive within 30 s or the connection is dropped (`408`); it is read
**before** a job slot is taken, so a slow sender never holds one.

## Response shape

**Success (200):** `multipart/form-data` — zero or more output file parts,
plus a final `result` part (`Content-Type: application/json`) carrying the
op's structured result (e.g. `{"path": "...", "size": ...}`). File part names
and content types are op-specific, defined by checkpoints 2-4's handlers.

**Error (4xx/5xx):** plain JSON, always `{"error": "<message>"}` — never
multipart, never partial output parts.

| Status | Meaning |
|---|---|
| `400` | Malformed request (bad multipart, invalid `params` JSON or a param value outside its allowlist/range). |
| `401` | Missing or wrong `X-Aikonos-Office-Token`. |
| `404` | Unknown route / unknown op. |
| `408` | The request body did not arrive within 30 s. |
| `413` | An ingress cap was exceeded (see "Ingress caps"), or an output artifact exceeded `max_output_bytes`. Error names the offending part and the cap. |
| `500` | Handler error / unexpected failure. |
| `501` | Op is in the route table but not yet implemented. |
| `503` | Every job slot is busy and the wait queue (2 × `OFFICE_MAX_CONCURRENCY`) is full, or the request bodies already held in memory leave no room for this one (see "Ingress caps"); retry later. |
| `504` | Job exceeded its wall-clock timeout; any subprocess (group) the handler spawned was killed. |

## Ingress caps

Request parsing is bounded before any handler runs. Every cap **rejects with
`413`** rather than truncating — busboy's default is to silently drop the
excess, and a truncated document that parses into a plausible-looking wrong
result is worse than an error.

| Cap | Default | Env |
|---|---|---|
| Bytes per file part | 11 MiB (the broker's 10 MiB `workspacefs.MaxFileBytes` + 1 MiB slack, so a request the broker accepted is never rejected here) | `OFFICE_MAX_UPLOAD_BYTES` |
| File parts per request | 16 (every `pdf.transform` op sends one part per `paths` entry; `merge` is just the one with a reason to send several) | `OFFICE_MAX_UPLOAD_FILES` |
| Bytes per form field | 2 MiB | — |
| Form fields per request | 8 | — |

Both `OFFICE_*` keys are read from the process environment but are **not**
substituted by `compose.yaml` — using them means editing the service's
`environment:` block. That is deliberate: a compose key would have to be
mirrored across `scripts/check-env-drift.sh`'s three `.env` templates for a
knob no deployment has needed to move.

The broker's cap covers the **compressed** input only. The fixed zip helpers
(`docx/pptx.edit`, `format:"xml"` extract) therefore refuse any archive member
over 50 MiB uncompressed, an archive over 200 MiB uncompressed, and an edit
that would grow a member past 50 M characters. The 50 MiB holds even when a
forged header declares a smaller size (no member is ever decompressed past
it), only stored and deflated members are read, and a regex `replace` with
group references (`\1`, `\g<0>`) is bounded before it is expanded.
`xlsx.extract` clamps `range` to the sheet's used area and refuses more than
1 M cells; `pptx.thumbnail` clamps `dpi` to [50, 300] and `max_pages` to
[1, 50].

Request bodies are held in memory from parsing until their job ends. A
request whose declared `Content-Length` (or, without one, the most a request
can carry: 16 × 11 MiB + 8 × 2 MiB) would take the bodies held at once past
512 MiB gets `503` before its body is read.

Model-authored scripts (`*.create`, `xlsx.edit`) run under `prlimit`:
`RLIMIT_AS` of `OFFICE_SCRIPT_MAX_AS_MB` (default 1024) for python, and that
plus the V8 heap for node, which also gets `--max-old-space-size`
(`OFFICE_NODE_MAX_OLD_SPACE_MB`, default 256; it bounds the JS heap only —
Buffers live outside it, which is why the address-space limit is the real
bound); `RLIMIT_FSIZE` of 64 MiB per file written. The python programs
that parse a caller document — the fixed helpers and `markitdown`
(`pptx.extract`) — run under the same python limits, and `pandoc`
(`docx.extract`) under a GHC heap cap of the same size (`+RTS -M`; its
runtime cannot start under `RLIMIT_AS`), as a backstop for any
decompression the caps above miss.
There is no `RLIMIT_NPROC`: it counts every task of the uid host-wide (no
userns-remap), so it aborted node on busy dev hosts; compose's `pids_limit`
is the fork-bomb bound. Same not-in-compose caveat for both knobs.
These are **per job**: `OFFICE_MAX_CONCURRENCY` jobs at their limits can still
sum past the container's 2g `mem_limit`, which stays the backstop, and the
512 MiB `/tmp` tmpfs is shared by every job (no per-job disk quota). Each
captured stdout/stderr stream keeps only its last 16 KiB.

## Size-cap semantics

Before any success response is built, every output artifact's byte length is
checked against the request's `params.max_output_bytes`. The **first**
oversize artifact fails the **whole** job with a single `413` error — no
partial multipart response, no output parts at all reach the caller. An
absent `max_output_bytes` applies the server default (50 MiB); any other
non-positive or non-numeric value is a `400`. The outputs together may not
exceed 80 MiB (also `413`), under the broker's 88 MiB whole-response cap — a
50-range `split` or 50-page `thumbnail` of large pages can reach it.

## Timeout semantics

Each job runs under a wall-clock timeout: `OFFICE_JOB_TIMEOUT_MS`
(env, default `120000`) for most ops; ops that shell out to LibreOffice
(`xlsx/recalc`, `pptx/thumbnail`, `office/convert`) get a longer fixed
`180000`ms budget instead (not independently configurable — see
`src/server.js`'s `SOFFICE_TIMEOUT_MS`). On expiry the job fails `504` and any
subprocess the handler spawned via the injected `spawn` helper is killed as a
process group (not just the immediate child) — a handler that shells out
**must** use the `spawn` passed into it, not `node:child_process` directly, or
timeout-kill won't reach grandchild processes.

When a job ends — success, error or timeout — the runner also kills every
process it left behind. Each spawned process carries a random
`AIKONOS_JOB_ID` in its (otherwise minimal: `PATH`, `HOME`, `LANG`, `LC_ALL`,
`TZ`, `TMPDIR`) environment; after the tracked process groups are killed, the
runner scans `/proc` and SIGKILLs any process whose environment still carries
that id, which catches `setsid`/`nohup` escapes from the group.

## Concurrency

`OFFICE_MAX_CONCURRENCY` (env, default `4`) bounds in-flight jobs; requests
beyond the cap queue FIFO and are served as slots free up. At most
2 × `OFFICE_MAX_CONCURRENCY` requests wait; past that a request gets `503`.
The server accepts at most running + queued + 2 connections at once.

## Job isolation

Each request gets a fresh, uniquely-named directory under `/tmp` (the
container's `read_only:true` + tmpfs `/tmp` filesystem contract), passed to
the op handler as `jobDir`. The directory is always removed after the
request — on success, on handler error, and on timeout. Output files are
read with `O_NOFOLLOW` and must resolve (by real path) inside the job
directory, so a script cannot return a symlink to another job's file.

**Residual (shared uid):** every job, and every model-authored script, runs as
the same uid in one container. The job dir's `0700` mode keeps other uids
out but not a concurrent job's script, which can read or overwrite another
in-flight job's files while both run. The post-job sweep stops a script from
persisting into later jobs, but a process that rewrites its own environment
before the sweep (`exec env -i ...`) escapes it. A script can also read the
worker's own environment, including `AIKONOS_OFFICE_WORKER_TOKEN`, from
`/proc/<pid>/environ` (the minimal job env only keeps it from being
inherited); the token authenticates calls to this worker only, on its
internal network. A per-job sandbox (PID and mount namespace, e.g.
bwrap/nsjail, or gVisor) is the fix for all three.

## Checkpoint 2: script-runtime ops

Implemented so far: `docx.create/edit/extract`, `xlsx.create/edit/extract`,
`pptx.create/edit/extract`, `pdf.create/extract`. All non-`*.create` ops
require an input file part named `file`. Every op's response file part (when
present) is also named `file`. `*.create`/`xlsx.edit`/`pdf.create` scripts run
under a pinned interpreter with a pinned library set (`docx`/`pptxgenjs` for
node; `openpyxl`/`reportlab` for python) — a missing `params.script` is a
`400`. A script/subprocess failure (non-zero exit, or its expected output file
never appears) is a `500`.

### `docx.create` (node + `docx`)

Request: no file part. `params`: `{ "script": "<docx-js source>",
"output_filename"?: "output.docx" }`. The script's cwd is the job dir — it
must write its file to `output_filename` (relative path) itself, e.g.
`writeFileSync('output.docx', await Packer.toBuffer(doc))`. `docx`'s
`node_modules` is resolvable from the script via a symlink into the job dir
(no `NODE_PATH` — ESM resolution ignores it). Response: `file` part (the
`.docx`) + `result: { filename, size }`.

### `docx.edit` (declarative, no script execution — shared with `pptx.edit`)

Request: `file` part (the archive) + `params: { "ops": [{ "member": string,
"find": string, "replace": string, "regex"?: boolean }], "output_filename"?:
string }`. Each op replaces `find` in the named archive member (literal
substring by default, `regex: true` treats `find` as a Python regex, `re.sub`
with `DOTALL`). At most 200 ops, and a regex `find` of at most 1024 chars
(Python `re` has no timeout; backtracking is bounded by the job timeout).
Applied via a fixed helper (`src/scripts/zip_edit.py`, python
stdlib `zipfile` — no new node zip dependency), which re-validates the
repacked archive's integrity (`ZipFile.testzip()`) before it's returned. A
`member` absent from the archive currently surfaces as a `500` (the fixed
helper's own validation failure is a script error, not a JS-side input
check — see the implementation report's "contract decisions" for CP3/CP4).
Response: `file` part + `result: { filename, size }`.

### `docx.extract` (pandoc by default; shared `format:"xml"` mode with `pptx.extract`)

Request: `file` part + `params`. Two modes:

- **Default (markdown):** `params: { "track_changes"?: "accept"|"reject"|"all" }`
  — pandoc `-f docx -t markdown`, passing `--track-changes=<value>` through
  when given; any other value is a `400`.
  Response: no file parts, `result: { content: "<markdown>" }`.
- **`format: "xml"`:** `params: { "format": "xml", "member": string
  (required, e.g. "word/document.xml"), "pattern"?: string (Python regex,
  at most 1024 chars), "context_chars"?: integer in [0, 2000] (default 200),
  "echo_cap"?: integer in [0, 200000] (default 20000 chars) }`; a value
  outside its range is a `400`. Without
  `pattern`, returns the member's content truncated to `echo_cap`. With `pattern`, returns each match with `context_chars` of
  surrounding context, accumulating until `echo_cap` would be exceeded (then
  stops, `truncated: true`) — the full member is never returned for a large
  file. Response: no file parts, `result: { member, member_length,
  content?, matches?: [{start, end, text}], truncated }`.

### `xlsx.create` / `xlsx.edit` (python + `openpyxl`)

`xlsx.create` request: no file part, `params: { "script": "<openpyxl
source>", "output_filename"?: "output.xlsx" }` — script's cwd is the job dir,
writes its own output file.

`xlsx.edit` request: `file` part (existing workbook) + `params: { "script":
"<openpyxl source>", "output_filename"?: "output.xlsx" }`. The input workbook
is written into the job dir as `input.xlsx` before the script runs; the
script opens `"input.xlsx"` and writes `output_filename`.

Both respond: `file` part (`.xlsx`) + `result: { filename, size }`.

### `xlsx.extract` (declarative — python + `openpyxl`/`pandas`, no script execution)

Request: `file` part + `params: { "sheet"?: string (default: first sheet),
"range"?: string (e.g. "A1:C10"), "format"?: "csv"|"json" (default "csv") }`.
Response: no file parts, `result: { sheet, rows, format, content }` —
`content` is a CSV string (`format: "csv"`) or a JSON string of row arrays
(`format: "json"`, `JSON.parse` it). Reads cached formula values
(`data_only=True`), not formula source — recalculation is `xlsx.recalc`
(checkpoint 3).

### `pptx.create` (node + `pptxgenjs`)

Same shape as `docx.create`: `params: { "script": "<pptxgenjs source>",
"output_filename"?: "output.pptx" }`, script writes its own output file
relative to its cwd (the job dir).

### `pptx.edit`

Identical wire shape to `docx.edit` (shared engine — pptx is also an OOXML
zip archive; ops typically target `ppt/slides/slideN.xml`).

### `pptx.extract` (markitdown by default; shared `format:"xml"` mode with `docx.extract`)

Default mode request: `file` part, no extra params. Uses `markitdown`
(pandoc has no pptx reader) — response: no file parts, `result: { content:
"<markdown>" }`. `format: "xml"` mode: identical params/response shape to
`docx.extract`'s xml mode.

### `pdf.create` (python + `reportlab`)

Request: no file part, `params: { "script": "<reportlab source>",
"output_filename"?: "output.pdf" }`, script writes its own output file.
Response: `file` part (`.pdf`) + `result: { filename, size }`.

### `pdf.extract` (declarative — pdfplumber → pdftotext → OCR fallback, no script execution)

Request: `file` part + `params: { "ocr"?: true | "true" | "auto" (default
"auto") }`. `"auto"`: tries `pdfplumber` (layout-preserving text + tables),
falls back to `pdftotext -layout` if that yields empty text, falls back to
OCR (`pdf2image` rasterize + `pytesseract`) if still empty. `true`/`"true"`
forces OCR regardless of the text layer. Response: no file parts, `result: {
text, tables: [[...]], method: "pdfplumber"|"pdftotext"|"ocr" }`.

## Checkpoint 3: LibreOffice / poppler / qpdf ops

Implemented: `xlsx/recalc`, `pptx/thumbnail`, `office/convert` (all soffice
headless), `pdf/transform` (qpdf + pypdf, no soffice). The three soffice ops
run under the `SOFFICE_TIMEOUT_MS` (180000ms) budget (see "Timeout
semantics" above).

### Macro hardening (every `soffice` invocation)

All three soffice ops (`xlsx/recalc`, `pptx/thumbnail`, `office/convert`) go
through one shared helper, `src/handlers/soffice.js`'s `runSoffice`, which
never lets a caller forget the hardening: every job gets its own throwaway
`-env:UserInstallation` profile under the job dir (isolated from any shared
profile), pre-seeded with a `registrymodifications.xcu` that pins
`MacroSecurityLevel` to `3` ("Very high" — no macro executes, not even a
signed one), disables macro execution outright (`DisableMacrosExecution`),
DDE and embedded OLE (`DisableActiveContent`), blocks links from untrusted
locations (`BlockUntrustedRefererLinks`), and sets link updates on load to
never in Calc and Writer (`Content/Update/Link`). Verified empirically (LibreOffice 7.4.7.2, via a
soffice-equipped Docker build of this service's own Dockerfile) that
`soffice --convert-to` batch conversion already refuses to execute a
document's embedded macro unconditionally regardless of this setting — the
explicit hardening is deliberate belt-and-suspenders documented in
`soffice.js`'s header comment, not proof the refusal depends on it. The same
profile also forces Calc formula recalculation on load
(`OOXMLRecalcMode`/`ODFRecalcMode` = `2`), the mechanism `xlsx.recalc` relies
on.

### `xlsx.recalc` (soffice headless — upstream `recalc.py` port)

Request: `file` part (workbook) + `params: { "output_filename"?:
"output.xlsx" }`. The workbook is round-tripped through `soffice --convert-to
xlsx` under the always-recalculate hardened profile, then a fixed helper
(`src/scripts/xlsx_recalc_scan.py`, openpyxl `data_only=True`) scans every
cell for an Excel error token (`#REF!`, `#DIV/0!`, `#VALUE!`, `#NAME?`,
`#NULL!`, `#NUM!`, `#N/A`, `#GETTING_DATA`, `#SPILL!`, `#CALC!`). Response:
`file` part (the recalculated `.xlsx`) + `result: { filename, size, errors:
[{sheet, cell, error}], error_count }`.

### `pptx.thumbnail` (soffice → pdf → `pdftoppm`)

Request: `file` part + `params: { "dpi"?: number (default 150), "max_pages"?:
number (default 20), "output_filename"? — unused, retained for symmetry }`.
Converts to PDF via soffice, reads the page count via `pdfinfo`, then renders
at most `max_pages` pages via `pdftoppm -f 1 -l <rendered>` — pages beyond
the cap are **skipped, not errored**. Response: one image file part per
rendered page (`Content-Type: image/jpeg`) + `result: { count, skipped,
total_pages, dpi }`.

### `office.convert` (soffice headless — any↔any, incl. legacy `.doc`→`.docx`)

Request: `file` part + `params: { "target_format": string (required, e.g.
"docx", "pdf"), "source_extension"?: string (required only if the file part
carries no filename extension), "output_filename"?: string }`. The input
filter is selected from the uploaded part's own filename extension (or
`source_extension` as an override) — this is how a `.doc` input round-trips
to `.docx`. Response: `file` part (named per `output_filename`, default
`output.<target_format>`) + `result: { filename, size }`. A missing
`target_format` or missing file part is `400`.

### `pdf.transform` (qpdf + pypdf, fully declarative — no soffice, no script)

Request: `params: { "op": "merge"|"split"|"rotate"|"decrypt"|"fill-forms",
... op-specific params }`. Per op:

- **`merge`**: one or more `file` parts, combined in request order via
  `qpdf --empty --pages`. `params: { output_filename? }`.
- **`split`**: one `file` part + `params: { ranges: [{ pages: string (a
  qpdf page-range expression, e.g. "1-2"), output_filename? }] }` (at most
  50 ranges) — one output file per range, via `qpdf --pages`.
- **`rotate`**: one `file` part + `params: { angle: 90|180|270|-90|-180|-270,
  pages?: string (qpdf range, default "1-z" = all), output_filename? }`, via
  `qpdf --rotate`. Every `pages` value must match the allowlisted qpdf range
  syntax (`n`, `rN`, `z`, `a-b`, optional `:even`/`:odd`, comma-separated, at
  most 256 chars) or the op is a `400` — a value reaching qpdf's argv
  starting with `@` would otherwise be read as an argument file.
- **`decrypt`**: one `file` part + `params: { password: string
  (required, no line breaks) }`, via `qpdf --password-file=<job file>
  --decrypt` (never argv, which other processes can read). A wrong password
  is a `500`.
- **`fill-forms`**: one `file` part + `params: { fields: { <name>: <value>,
  ... } }` — AcroForm field values, applied via the fixed helper
  `src/scripts/pdf_fill_forms.py` (pypdf
  `update_page_form_field_values`). Field values are data, never a script.

Response (all ops): `file` part(s) (named `output.pdf` or per
`output_filename`/`ranges[].output_filename`) + `result` (shape varies:
`{filename, size}` for merge/rotate/decrypt/fill-forms, `{files: [{filename,
size, pages}]}` for split). An unknown `params.op` is a `400`.
