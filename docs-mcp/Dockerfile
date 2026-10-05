# docs-mcp image — serves Aikonos Markdown docs over MCP (streamable HTTP).
# No proto/gRPC generation: this service has no south RPCs.
# Build context is docs-mcp/ (compose: context: docs-mcp).
FROM node:22-trixie-slim
WORKDIR /app

# Production deps only; devDeps (test runner) are not needed at runtime.
COPY package.json package-lock.json ./
RUN npm ci --omit=dev && npm cache clean --force
# npm, npx and corepack are build tools the service never runs, and their
# bundled dependencies were most of the image's fixable vulnerability findings.
RUN rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack \
      /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack
# Security updates the base image has not picked up yet.
RUN apt-get update && apt-get upgrade -y --no-install-recommends \
    && rm -rf /var/lib/apt/lists/*

COPY src ./src

ENV NODE_ENV=production
ENV PORT=8060
EXPOSE 8060
USER node
CMD ["node", "src/server.js"]
