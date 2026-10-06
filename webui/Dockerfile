# aikonOS unified webui image: builds the Vue app, then runs the Node backend
# which proxies gateway + observability APIs and serves the built SPA.
FROM node:22-trixie-slim AS web
WORKDIR /app/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
# OIDC config is baked into the SPA at build time by Vite (import.meta.env.VITE_*)
# as a fallback: at runtime server.mjs serves the container's AIKONOS_WEBUI_OIDC_*
# values as /runtime-config.js, and those win. Release images are built without
# these ARGs, so one image serves any identity provider (docs/14-signed-releases.md).
# The ARGs must be declared here so Docker exposes them as env vars for npm run build.
ARG VITE_OIDC_AUTHORITY
ARG VITE_OIDC_CLIENT_ID
ARG VITE_OIDC_REDIRECT_URI
# Scope requests OIDC scopes only. Keycloak stamps aud=aikonos-broker via a
# client protocol mapper, so "openid profile" suffices (requesting "aikonos-broker"
# as a scope fails — no such client scope exists). Entra needs the broker's
# exposed API scope in the token audience: "openid profile api://<app-id>/access_as_user".
ARG VITE_OIDC_SCOPE
# Which token the SPA sends as the broker bearer: "access" (default; Keycloak +
# Entra apps that expose an API) or "id" (Entra login-only, no API exposed).
ARG VITE_OIDC_TOKEN
RUN npm run build

FROM node:22-trixie-slim AS run
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev && npm cache clean --force
# npm, npx and corepack are build tools the service never runs, and their
# bundled dependencies were most of the image's fixable vulnerability findings.
RUN rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack \
      /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack
# Security updates the base image has not picked up yet.
RUN apt-get update && apt-get upgrade -y --no-install-recommends \
    && rm -rf /var/lib/apt/lists/*
COPY server.mjs ./
COPY --from=web /app/web/dist ./web/dist
ENV NODE_ENV=production
ENV PORT=4200
EXPOSE 4200
USER node
CMD ["node", "server.mjs"]
