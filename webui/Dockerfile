# Aikonos unified webui image: builds the Vue app, then runs the Node backend
# which proxies gateway + observability APIs and serves the built SPA.
FROM node:22-slim AS web
WORKDIR /app/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
# OIDC config is baked into the SPA at build time by Vite (import.meta.env.VITE_*).
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

FROM node:22-slim AS run
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev && npm cache clean --force
COPY server.mjs ./
COPY --from=web /app/web/dist ./web/dist
ENV NODE_ENV=production
ENV PORT=4200
EXPOSE 4200
USER node
CMD ["node", "server.mjs"]
