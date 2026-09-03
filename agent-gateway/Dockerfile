# Agent-gateway image. Runs the TS directly via tsx (no compile step).
#
# Build context is the REPO ROOT (compose: context: .) so the Dockerfile can
# reach proto/ and generate the gen/ts gRPC stubs in-image — a clean checkout
# builds with no host `task proto:gen` prep (H6). protoc + the ts-proto plugin
# (a devDependency, so the full `npm ci` below provides it) do the generation.
#
# Non-root; under compose the SVID is the dev-CA PEMs bind-mounted at /tls — the
# gateway only reads those files, so it needs no socket access or root.
FROM node:22-slim
WORKDIR /app

# protoc + the well-known .proto types (libprotobuf-dev) for in-image gen.
RUN apt-get update && apt-get install -y --no-install-recommends protobuf-compiler libprotobuf-dev \
    && rm -rf /var/lib/apt/lists/*

# Full install (incl. devDeps: tsx runs the server, ts-proto generates the stubs)
COPY agent-gateway/package.json agent-gateway/package-lock.json ./
RUN npm ci && npm cache clean --force

COPY agent-gateway/tsconfig.json ./
COPY agent-gateway/src ./src
COPY agent-gateway/public ./public

# Generate the gRPC stubs into gen/ts/proto/* (the layout the source imports).
# --proto_path=. makes `import "proto/...";` resolve.
COPY proto ./proto
RUN mkdir -p gen/ts && for f in proto/*.proto; do \
    protoc --proto_path=. --proto_path=/usr/include \
    --plugin=protoc-gen-ts_proto=./node_modules/.bin/protoc-gen-ts_proto \
    --ts_proto_out=gen/ts \
    --ts_proto_opt=outputServices=grpc-js,esModuleInterop=true,useExactTypes=false,useOptionals=messages \
    "$f"; \
    done

ENV NODE_ENV=production
ENV PORT=8080
EXPOSE 8080
USER node
CMD ["npm", "start"]
