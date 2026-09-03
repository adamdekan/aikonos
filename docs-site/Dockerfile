# Aikonos docs site: builds the static Astro Starlight site, then serves the
# build under /docs/ via an unprivileged, non-root nginx (no app runtime).
FROM node:22-slim AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
# DOCS_BASE (e.g. /docs) becomes the Astro `base` (astro.config.mjs) for the
# on-prem subpath deploy. Unset here for the manual cPanel root-upload build.
ARG DOCS_BASE
ENV DOCS_BASE=${DOCS_BASE}
RUN npm run build

FROM nginxinc/nginx-unprivileged:1.27-alpine
# dist/ lands at html/docs/ so a request for /docs/... maps directly to a
# file — no nginx rewrite or strip-prefix config needed. The base image
# already listens on 8080 and runs non-root with pid/cache under /tmp.
COPY --from=build /app/dist /usr/share/nginx/html/docs
