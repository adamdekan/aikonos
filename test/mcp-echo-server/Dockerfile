# DEV ONLY — test MCP echo server image. Not for production.
FROM node:22-alpine
WORKDIR /app
COPY server.js .
ENV PORT=8000
USER node
EXPOSE 8000
CMD ["node", "server.js"]
