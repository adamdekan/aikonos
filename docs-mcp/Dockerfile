# docs-mcp image — serves Aikonos Markdown docs over MCP (streamable HTTP).
# No proto/gRPC generation: this service has no south RPCs.
# Build context is docs-mcp/ (compose: context: docs-mcp).
FROM node:22-slim
WORKDIR /app

# Production deps only; devDeps (test runner) are not needed at runtime.
COPY package.json package-lock.json ./
RUN npm ci --omit=dev && npm cache clean --force

COPY src ./src

ENV NODE_ENV=production
ENV PORT=8060
EXPOSE 8060
USER node
CMD ["node", "src/server.js"]
