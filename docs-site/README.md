# Aikonos Docs

Astro Starlight site. Standalone: no dependency on the rest of this repo, no CI wiring.

## Prerequisites

- Node.js 18+

## Local dev

```
npm install
npm run dev
```

## Build

```
npm run build
```

Output: `dist/`, a static site, no server-side runtime required.

## Deploy (cPanel file upload)

1. `npm run build`.
2. Zip the contents of `dist/` (not the `dist/` folder itself, just its contents).
3. Upload the zip via cPanel File Manager into the target web root (e.g. `public_html/`).
4. Extract server-side in File Manager. Delete the zip afterward if cPanel leaves it in place.

## Deploying to a subfolder

If the site will live at `example.com/docs/` instead of the web root, build with `DOCS_BASE`
set instead of editing `astro.config.mjs` directly — it reads `process.env.DOCS_BASE` and
passes it as Astro's `base`, and a remark plugin (`remark-base-links.mjs`) prefixes it onto
every root-relative link/image written in the markdown/MDX content:

1. `DOCS_BASE=/docs npm run build`.
2. Upload the new `dist/` contents as above.

## On-prem deployment

On the on-prem stack, the site ships as the `docs-site` Docker Compose service (see
`compose.yaml` and `deploy/compose/compose.onprem.yaml`) at `https://<host>/docs`, built with
`DOCS_BASE=/docs` and rebuilt on every git-push deploy alongside the rest of the stack. The
manual cPanel upload flow above is unchanged and remains the supported path for a standalone
root deploy — the two are independent delivery mechanisms for the same `dist/` output.
