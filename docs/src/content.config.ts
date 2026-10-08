import { glob } from 'astro/loaders';
import { defineCollection, z } from 'astro:content';

// The page collection: every .md/.mdx file under content/docs, both locales
// included, since lib/source.ts filters them per locale. Frontmatter carries a
// required title and an optional description and icon.
const docs = defineCollection({
  loader: glob({ pattern: '**/*.{md,mdx}', base: './content/docs' }),
  schema: z.object({
    title: z.string(),
    description: z.string().optional(),
    icon: z.string().optional(),
  }),
});

// The folder-metadata collection: the JSON/YAML files that label and order the
// sidebar folders. Every field is optional.
const meta = defineCollection({
  loader: glob({ pattern: '**/*.{json,yaml}', base: './content/docs' }),
  schema: z.object({
    title: z.string().optional(),
    description: z.string().optional(),
    pages: z.array(z.string()).optional(),
    icon: z.string().optional(),
  }),
});

/** Collection registry Astro reads; both collections must be listed here. */
export const collections = {
  docs,
  meta,
};
