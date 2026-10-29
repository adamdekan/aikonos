# Agent-gateway image. Runs the TS directly via tsx (no compile step).
#
# Build context is the REPO ROOT (compose: context: .) so the Dockerfile can
# reach proto/ and generate the gen/ts gRPC stubs in-image — a clean checkout
# builds with no host `task proto:gen` prep (H6).
#
# Two stages: the builder holds protoc, ts-proto and the devDependencies and
# only generates gen/ts; the runtime stage installs production dependencies
# with no lifecycle scripts and copies the stubs in. The process that
# supervises model-driven children therefore ships no compiler toolchain.
#
# Non-root; under compose the SVID is the dev-CA PEMs bind-mounted at /tls — the
# gateway only reads those files, so it needs no socket access or root.
FROM node:22-trixie-slim AS builder
WORKDIR /app

# protoc + the well-known .proto types (libprotobuf-dev) for in-image gen.
RUN apt-get update \
    && apt-get install -y --no-install-recommends protobuf-compiler libprotobuf-dev \
    && rm -rf /var/lib/apt/lists/*

# Full install for the ts-proto plugin; scripts off — generation needs none.
COPY agent-gateway/package.json agent-gateway/package-lock.json ./
RUN npm ci --ignore-scripts

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

FROM node:22-trixie-slim
WORKDIR /app

# The security updates the base image has not picked up yet.
RUN apt-get update && apt-get upgrade -y --no-install-recommends \
    && rm -rf /var/lib/apt/lists/*

# Production dependencies only (tsx is one: it runs the server and the forked
# children). --ignore-scripts: no package's install hook runs in the build;
# esbuild finds its platform binary without its postinstall.
COPY agent-gateway/package.json agent-gateway/package-lock.json ./
RUN npm ci --omit=dev --ignore-scripts && npm cache clean --force

COPY agent-gateway/tsconfig.json ./
COPY agent-gateway/src ./src
COPY agent-gateway/public ./public
COPY --from=builder /app/gen ./gen

# npm, npx and corepack are build tools the service never runs, and their
# bundled dependencies were most of the image's fixable vulnerability findings.
RUN rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack \
      /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack

ENV NODE_ENV=production
ENV PORT=8080
EXPOSE 8080
USER node
# What `npm start` runs, without npm.
CMD ["node_modules/.bin/tsx", "src/server.ts"]
