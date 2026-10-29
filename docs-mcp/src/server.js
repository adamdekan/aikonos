/**
 * server.js — aikonOS docs MCP server.
 *
 * Per-request fresh McpServer + StreamableHTTPServerTransport in stateless mode.
 * corpusRoot is captured from createApp closure — never re-read from env per call.
 */

import { createMcpExpressApp } from '@modelcontextprotocol/sdk/server/express.js';
import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StreamableHTTPServerTransport } from '@modelcontextprotocol/sdk/server/streamableHttp.js';
import { z } from 'zod';
import { listDocs, readDoc, searchDocs, CorpusError, MAX_QUERY_LENGTH } from './corpus.js';

// Hostnames a request's Host header may name (port-agnostic). The server has
// no auth and serves the docs corpus, so a browser on the operator's machine
// must not reach it through DNS rebinding (attacker.example resolving to
// 127.0.0.1): its Host header still says attacker.example. aikonos-docs-mcp
// is the compose service name mesh callers dial.
export const DEFAULT_ALLOWED_HOSTS = ['localhost', '127.0.0.1', '[::1]', 'aikonos-docs-mcp'];

const LIST_PAGE_MAX = 200;

function csv(value) {
  return value ? value.split(',').map((v) => v.trim()).filter(Boolean) : undefined;
}

// MCP clients here are server-side (the broker); only a browser sends
// Origin, so any Origin not explicitly allowed is refused.
function originValidation(allowedOrigins) {
  return (req, res, next) => {
    const origin = req.headers.origin;
    if (origin !== undefined && !allowedOrigins.includes(origin)) {
      process.stderr.write(`[docs-mcp] denied request with Origin ${JSON.stringify(origin)}\n`);
      res.status(403).json({ jsonrpc: '2.0', error: { code: -32000, message: 'Origin not allowed' }, id: null });
      return;
    }
    next();
  };
}

// ---------------------------------------------------------------------------
// Factory — exported so tests can mount on an ephemeral port.
// ---------------------------------------------------------------------------

export function createApp({ corpusRoot, allowedHosts = DEFAULT_ALLOWED_HOSTS, allowedOrigins = [] }) {
  // Host validation runs before the JSON body parser, so a rebinding request
  // is refused (403) without its body being read.
  const app = createMcpExpressApp({ allowedHosts });
  app.disable('x-powered-by');
  app.use(originValidation(allowedOrigins));

  // Health probe — Node socket connect in compose healthcheck (no curl in slim image).
  app.get('/healthz', (_req, res) => {
    res.json({ status: 'ok' });
  });

  // MCP endpoint — fresh server + transport per request (stateless mode).
  app.post('/mcp', async (req, res) => {
    const server = new McpServer({ name: 'aikonos-docs', version: '0.1.0' });

    // search_docs — ranked full-text search across the corpus.
    server.registerTool(
      'search_docs',
      {
        description:
          'Search aikonOS documentation by keyword. Returns ranked results with snippets. ' +
          'Use this to find docs relevant to a topic before reading them in full.',
        inputSchema: z.object({
          query: z.string().max(MAX_QUERY_LENGTH),
          limit: z.number().int().min(1).max(20).optional(),
        }),
        annotations: { readOnlyHint: true },
      },
      async ({ query, limit }) => {
        const results = await searchDocs(corpusRoot, query, limit ?? 8);
        if (results.length === 0) {
          return { content: [{ type: 'text', text: 'No results found.' }] };
        }
        const text = results
          .map(
            (r, i) =>
              `${i + 1}. ${r.path} (score: ${r.score})\n   ${r.snippet}`,
          )
          .join('\n\n');
        return { content: [{ type: 'text', text }] };
      },
    );

    // list_docs — enumerate all docs in the corpus.
    server.registerTool(
      'list_docs',
      {
        description:
          'List all Markdown documents available in the aikonOS documentation corpus. ' +
          `Returns path and title for each file, at most ${LIST_PAGE_MAX} per call; ` +
          'pass offset to page through a larger corpus.',
        inputSchema: z.object({
          offset: z.number().int().min(0).optional(),
          limit: z.number().int().min(1).max(LIST_PAGE_MAX).optional(),
        }),
        annotations: { readOnlyHint: true },
      },
      async ({ offset = 0, limit = LIST_PAGE_MAX }) => {
        const docs = await listDocs(corpusRoot);
        const page = docs.slice(offset, offset + limit);
        let text = page.map((d) => `${d.path} — ${d.title}`).join('\n');
        if (offset + limit < docs.length) {
          text += `\n\n(${docs.length - offset - limit} more; call again with offset ${offset + limit})`;
        }
        return { content: [{ type: 'text', text: text || 'No documents found.' }] };
      },
    );

    // read_doc — fetch a single document by relative path.
    server.registerTool(
      'read_doc',
      {
        description:
          'Read the full content of an aikonOS documentation file by its relative path ' +
          '(e.g. "docs/ONBOARDING.md"). Use list_docs to discover available paths.',
        inputSchema: z.object({
          path: z.string(),
        }),
        annotations: { readOnlyHint: true },
      },
      async ({ path: relPath }) => {
        try {
          const doc = await readDoc(corpusRoot, relPath);
          return { content: [{ type: 'text', text: doc.content }] };
        } catch (err) {
          // Return a safe tool-level error; never throw (would become an MCP protocol error).
          const msg =
            err instanceof CorpusError ? err.message : 'Document could not be read.';
          return {
            content: [{ type: 'text', text: msg }],
            isError: true,
          };
        }
      },
    );

    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined });
    await server.connect(transport);

    // Early client-disconnect guard: if the client drops before handleRequest
    // completes, close the transport so the SDK stops writing to a dead socket.
    // The finally block below handles the normal (non-early) cleanup path.
    let clientGone = false;
    res.on('close', () => { clientGone = true; });

    try {
      await transport.handleRequest(req, res, req.body);
    } catch (err) {
      // handleRequest should not throw for protocol errors (it writes them to
      // res), but guard anyway so the caller gets an HTTP error rather than a
      // hung request.
      if (!res.headersSent) {
        res.status(500).json({ jsonrpc: '2.0', error: { code: -32603, message: 'Internal error' }, id: null });
      }
      process.stderr.write(`[docs-mcp] handleRequest error: ${err}\n`);
    } finally {
      // Always clean up after the response is fully handled, regardless of
      // whether the client disconnected early or the handler threw.
      await transport.close().catch(() => {});
      await server.close().catch(() => {});
    }
  });

  return app;
}

// ---------------------------------------------------------------------------
// Main — only executes when run directly (import.meta main guard).
// ---------------------------------------------------------------------------

if (process.argv[1] === new URL(import.meta.url).pathname) {
  const corpusRoot = process.env.CORPUS_ROOT ?? '/corpus';
  const port = parseInt(process.env.PORT ?? '8060', 10);
  // Loopback unless the deployment opts in (compose sets HOST=0.0.0.0 so
  // mesh peers can dial aikonos-docs-mcp:8060).
  const host = process.env.HOST || '127.0.0.1';
  const app = createApp({
    corpusRoot,
    allowedHosts: csv(process.env.ALLOWED_HOSTS) ?? DEFAULT_ALLOWED_HOSTS,
    allowedOrigins: csv(process.env.ALLOWED_ORIGINS) ?? [],
  });
  app.listen(port, host, () => {
    process.stderr.write(`aikonos-docs-mcp listening on ${host}:${port} (corpus: ${corpusRoot})\n`);
  });
}
