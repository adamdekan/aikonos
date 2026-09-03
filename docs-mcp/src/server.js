/**
 * server.js — Aikonos docs MCP server.
 *
 * Per-request fresh McpServer + StreamableHTTPServerTransport in stateless mode.
 * corpusRoot is captured from createApp closure — never re-read from env per call.
 */

import express from 'express';
import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StreamableHTTPServerTransport } from '@modelcontextprotocol/sdk/server/streamableHttp.js';
import { z } from 'zod';
import { listDocs, readDoc, searchDocs, CorpusError } from './corpus.js';

// ---------------------------------------------------------------------------
// Factory — exported so tests can mount on an ephemeral port.
// ---------------------------------------------------------------------------

export function createApp({ corpusRoot }) {
  const app = express();
  app.use(express.json());

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
          'Search Aikonos documentation by keyword. Returns ranked results with snippets. ' +
          'Use this to find docs relevant to a topic before reading them in full.',
        inputSchema: z.object({
          query: z.string(),
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
          'List all Markdown documents available in the Aikonos documentation corpus. ' +
          'Returns path and title for each file.',
        inputSchema: z.object({}),
        annotations: { readOnlyHint: true },
      },
      async () => {
        const docs = await listDocs(corpusRoot);
        const text = docs.map((d) => `${d.path} — ${d.title}`).join('\n');
        return { content: [{ type: 'text', text: text || 'No documents found.' }] };
      },
    );

    // read_doc — fetch a single document by relative path.
    server.registerTool(
      'read_doc',
      {
        description:
          'Read the full content of a Aikonos documentation file by its relative path ' +
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
  const app = createApp({ corpusRoot });
  app.listen(port, () => {
    process.stderr.write(`aikonos-docs-mcp listening on :${port} (corpus: ${corpusRoot})\n`);
  });
}
