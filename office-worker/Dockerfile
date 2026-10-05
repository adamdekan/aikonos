# office-worker image — docx/xlsx/pptx/pdf create/edit/extract/convert,
# executed in a no-egress, no-credential worker. Build context is office-worker/ (compose: context:
# office-worker).
FROM node:22-trixie-slim

ENV DEBIAN_FRONTEND=noninteractive

# System deps checkpoints 2-3 shell out to: pandoc (docx/pptx markdown
# extraction), libreoffice (xlsx.recalc, pptx.thumbnail, office.convert),
# poppler-utils (pdftoppm/pdftotext), qpdf (pdf.transform), tesseract-ocr with
# English only (pdf.extract OCR fallback — additional language packs are a
# later knob per the design's Open questions).
RUN apt-get update && apt-get upgrade -y --no-install-recommends \
    && apt-get install -y --no-install-recommends \
      pandoc \
      libreoffice \
      poppler-utils \
      qpdf \
      tesseract-ocr \
      tesseract-ocr-eng \
      ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# uv's blessed container idiom (planner/Dockerfile precedent): copy the static
# binary rather than installing via curl/pip.
COPY --from=ghcr.io/astral-sh/uv:0.12.23 /uv /usr/local/bin/uv

WORKDIR /app

# uv manages its own pinned Python 3.12 — trixie's system python3 is 3.13,
# and checkpoints 2-3's openpyxl/pandas/reportlab/pdfplumber/pytesseract
# scripts need to run under a consistent, uv-locked interpreter + dep set.
# Pinned to the patch release so a rebuild never quietly picks an older one;
# raise it (and uv above, which must know the build) for Python security fixes.
ENV UV_PYTHON_INSTALL_DIR=/opt/uv-python
RUN uv python install 3.12.15

# Node deps first for layer caching (production only — no test runner needed
# at runtime).
COPY package.json package-lock.json ./
RUN npm ci --omit=dev && npm cache clean --force
# npm, npx and corepack are build tools the service never runs, and their
# bundled dependencies were most of the image's fixable vulnerability findings.
RUN rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack \
      /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack

# Python deps via uv, frozen against the committed lock (fails closed on
# pyproject.toml/uv.lock drift). Empty today; checkpoints 2-3 populate
# pyproject.toml with the script-runtime libraries.
COPY pyproject.toml uv.lock ./
RUN uv sync --frozen --python 3.12.15 --no-install-project

COPY src ./src

# Non-root: node:22-trixie-slim ships a "node" user (uid 1000) — reuse it instead of
# adding a new one. Everything under /app is root-owned from the layers
# above, so chown once rather than per-COPY.
RUN chown -R node:node /app
USER node

# read_only:true + a tmpfs /tmp (compose) is the whole filesystem contract:
# every job writes under a per-job dir inside /tmp and nothing else. Python
# caches are redirected there too so any lazy write at runtime (there
# shouldn't be any — deps are baked at build time) can't hit a read-only /app.
ENV HOME=/tmp
ENV UV_CACHE_DIR=/tmp/uv-cache
ENV PATH="/app/.venv/bin:${PATH}"

ENV NODE_ENV=production
ENV PORT=8081
ENV OFFICE_JOB_TIMEOUT_MS=120000
ENV OFFICE_MAX_CONCURRENCY=4

EXPOSE 8081
CMD ["node", "src/index.js"]
