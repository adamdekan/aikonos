// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import remarkBaseLinks from './remark-base-links.mjs';

// Subpath deployment (on-prem, served at https://<host>/docs): set at image
// build time via `docker build --build-arg DOCS_BASE=/docs` → `ENV DOCS_BASE`.
// Unset for the manual cPanel root-upload build (docs-site/README.md).
const base = process.env.DOCS_BASE || undefined;

export default defineConfig({
  ...(base ? { base } : {}),
  integrations: [
    starlight({
      title: 'Aikonos Docs',
      // Wordmark replaces the site title in the header; `title` is kept for the
      // document <title>, and Starlight renders it as the logo's screen-reader text.
      logo: {
        dark: './src/assets/aikonos-wordmark.svg',
        light: './src/assets/aikonos-wordmark-light.svg',
        replacesTitle: true,
      },
      components: { SiteTitle: './src/components/SiteTitle.astro' },
      customCss: ['./src/styles/custom.css'],
      sidebar: [
        { label: 'Concepts', items: [{ autogenerate: { directory: 'concepts' } }] },
        { label: 'User Guide', items: [{ autogenerate: { directory: 'guides' } }] },
        { label: 'Administrator Guide', items: [{ autogenerate: { directory: 'admin' } }] },
      ],
    }),
  ],
  markdown: {
    // @astrojs/mdx (bundled by Starlight) extends this config by default, so
    // MDX bodies (e.g. index.mdx) get the same base-prefixing as plain .md.
    remarkPlugins: [[remarkBaseLinks, base]],
  },
});
