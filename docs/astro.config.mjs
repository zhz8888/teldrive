// @ts-check
import { defineConfig } from 'astro/config';
import react from '@astrojs/react';
import tailwindcss from '@tailwindcss/vite';
import mdx from '@astrojs/mdx';
import { unified } from '@astrojs/markdown-remark';
import {
  rehypeCode,
  remarkCodeTab,
  remarkHeading,
  remarkNpm,
  remarkStructure,
} from 'fumadocs-core/mdx-plugins';

// site and base are injected by the deploy workflow: DOCS_SITE is the canonical
// URL the pages are built for, DOCS_BASE the sub-path the site is served under
// (Cloudflare Pages uses a sub-path for previews, so both must stay configurable).
const site = process.env.DOCS_SITE ?? 'http://localhost:4321';
const base = process.env.DOCS_BASE ?? '/';

// The Fumadocs remark plugins build the heading anchors, the package-manager tabs
// and the structured data the search index is generated from; rehypeCode applies
// the syntax highlighting.
const remarkPlugins = [
  remarkHeading,
  remarkCodeTab,
  remarkNpm,
  [remarkStructure, { exportAs: 'structuredData' }],
];
const rehypePlugins = [rehypeCode];

export default defineConfig({
  site,
  base,
  markdown: {
    processor: unified({
      syntaxHighlight: false,
      remarkPlugins,
      rehypePlugins,
    }),
  },
  integrations: [
    react(),
    mdx({
      extendMarkdownConfig: true,
      syntaxHighlight: false,
    }),
  ],
  vite: {
    plugins: [tailwindcss()],
  },
});
