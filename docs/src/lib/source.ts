import type { StaticSource } from 'fumadocs-core/source';
import { loader } from 'fumadocs-core/source';
import { type CollectionEntry, getCollection } from 'astro:content';
import * as path from 'node:path';
import { structure, type StructuredData } from 'fumadocs-core/mdx-plugins';
import type { Node, Root } from 'fumadocs-core/page-tree';
import { defaultLocale, isUnlocalizedPath, type LocaleCode, prefixPath } from './i18n';

/** One locale's fumadocs loader together with the page tree built from it. */
export interface LocaleSource {
  /** The locale this source serves. */
  locale: LocaleCode;
  /** The loader over that locale's content, with BASE_URL applied. */
  source: ReturnType<typeof loader>;
  /** The tree the loader produced, before locale prefixes are applied. */
  tree: Root;
}

/**
 * Each locale gets its own loader over the same content collection, filtered to
 * the files under `content/docs/<locale>/` and re-rooted so the locale
 * directory is not part of the virtual path. Keeping one source per locale
 * rather than enabling fumadocs' own i18n keeps the English URLs unprefixed;
 * the `fumadocs-core/i18n/middleware` helper is Next-only anyway.
 */
const sources = new Map<LocaleCode, LocaleSource>();
// Cache of locale-prefixed trees, filled by localizedTree on first use so a
// request does not rebuild the tree for every page it renders.
const localizedTrees = new Map<LocaleCode, Root>();

/**
 * Returns the cached source of a locale, building it on first request. Every
 * locale reads the shared collection but only the files under its own
 * directory, and the loader is kept so the tree is built once per build.
 */
export async function getLocaleSource(locale: LocaleCode): Promise<LocaleSource> {
  const cached = sources.get(locale);
  if (cached) return cached;

  const source = loader({
    source: await createSource(locale),
    baseUrl: import.meta.env.BASE_URL,
  });

  const created: LocaleSource = { locale, source, tree: buildTree(source) };
  sources.set(locale, created);
  return created;
}

/** Extracts the heading and content structure the search index is built from. */
export function getStructuredData(entry: CollectionEntry<'docs'>): StructuredData {
  return structure(entry.body);
}

/** Applies BASE_URL to every URL of a freshly built page tree. */
function buildTree(source: LocaleSource['source']): Root {
  const tree = source.getPageTree();
  return { ...tree, children: tree.children.map(withBaseUrl) };
}

/**
 * localizedTree returns the page tree of one locale with every URL carrying the
 * locale prefix. The per-locale tree omits it for English, so anything that has
 * to line a page up with its own tree — breadcrumbs, the search index — needs
 * this rather than the bare tree.
 */
export function localizedTree(locale: LocaleCode): Root {
  const cached = localizedTrees.get(locale);
  if (cached) return cached;

  const entry = sources.get(locale);
  if (!entry) throw new Error(`locale source for ${locale} was not initialized`);

  const tree = mapURLs(entry.tree, (url) => localizedURL(locale, url));
  localizedTrees.set(locale, tree);
  return tree;
}

/** Rewrites every URL of a tree, descending into folders and their indexes. */
function mapURLs(node: Root, map: (url: string) => string): Root {
  return { ...node, children: node.children.map(mapNode) };

  /** Rewrites one child node: a page URL, or a folder's index and children. */
  function mapNode(child: Node): Node {
    if (child.type === 'page') {
      return { ...child, url: map(child.url) };
    }

    if (child.type === 'folder') {
      return {
        ...child,
        index: child.index ? { ...child.index, url: map(child.index.url) } : undefined,
        children: child.children.map(mapNode),
      };
    }

    return child;
  }
}

/** Copies a node with BASE_URL applied to its own and its children's URLs. */
function withBaseUrl(node: Node): Node {
  if (node.type === 'page') {
    return { ...node, url: prefixBase(node.url) };
  }

  if (node.type === 'folder') {
    return {
      ...node,
      index: node.index ? { ...node.index, url: prefixBase(node.index.url) } : undefined,
      children: node.children.map(withBaseUrl),
    };
  }

  return node;
}

/**
 * Adds BASE_URL to a site-rooted URL, once: a URL that already carries the
 * prefix, and anything that is not an absolute site path, is returned as is.
 */
function prefixBase(url: string) {
  if (!url.startsWith('/') || url.startsWith('//')) return url;

  const base = import.meta.env.BASE_URL.replace(/\/$/, '');
  if (!base || url === base || url.startsWith(`${base}/`)) return url;
  return `${base}${url}`;
}

/**
 * localizedURL rewrites a page-tree URL so it carries the locale prefix the
 * English source omits. Without it the previous/next footer would jump from a
 * Chinese page to the English page of the same slug.
 */
export function localizedURL(locale: LocaleCode, url: string): string {
  const base = import.meta.env.BASE_URL.replace(/\/$/, '');
  const path = url.startsWith(base) ? url.slice(base.length) : url;
  // The API reference and the OG images live at the site root in one language
  // only, so they keep the path the tree already carries: the Chinese sidebar's
  // API entry links to /api/, and a locale prefix would send it to /zh/api/,
  // which is never built.
  if (locale === defaultLocale || isUnlocalizedPath(path)) return url;

  return prefixBase(prefixPath(locale, path === '/' ? '' : path));
}

/**
 * Builds the static source fumadocs' loader consumes: page entries carry their
 * frontmatter plus the raw collection entry the search index needs, meta
 * entries carry the sidebar metadata. Entries of other locales are skipped,
 * which is what keeps the per-locale trees apart.
 */
async function createSource(locale: LocaleCode) {
  const out: StaticSource<{
    metaData: CollectionEntry<'meta'>['data'];
    pageData: CollectionEntry<'docs'>['data'] & {
      _raw: CollectionEntry<'docs'>;
    };
  }> = {
    files: [],
  };

  for (const page of await getCollection('docs')) {
    const relative = relativeVirtualPath(page.filePath!, locale);
    if (relative === undefined) continue;

    out.files.push({
      type: 'page',
      path: relative,
      data: {
        ...page.data,
        _raw: page,
      },
    });
  }

  for (const meta of await getCollection('meta')) {
    const relative = relativeVirtualPath(meta.filePath!, locale);
    if (relative === undefined) continue;

    out.files.push({
      type: 'meta',
      path: relative,
      data: meta.data,
    });
  }

  return out;
}

/**
 * relativeVirtualPath maps an absolute content file path onto one locale's
 * virtual path, or returns undefined when the file belongs to another locale.
 */
function relativeVirtualPath(filePath: string, locale: LocaleCode) {
  const relative = path.relative('content/docs', filePath);
  const prefix = `${locale}${path.sep}`;
  if (!relative.startsWith(prefix)) return undefined;
  return relative.slice(prefix.length);
}
