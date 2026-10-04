import type { APIRoute } from 'astro';
import { createFromSource } from 'fumadocs-core/search/server';
import type { StructuredData } from 'fumadocs-core/mdx-plugins';
import type { Root } from 'fumadocs-core/page-tree';
import { getStructuredData, getLocaleSource, localizedTree, localizedURL } from '@/lib/source';
import { getPageImageUrl } from '@/lib/shared';
import { defaultLocale, localeCodes, type LocaleCode } from '@/lib/i18n';

interface MergedPage {
  url: string;
  slugs: string[];
  locale: LocaleCode;
  data: {
    title: string;
    description?: string;
    structuredData: StructuredData;
    image: string;
  };
}

/**
 * The static search index spans every locale, so one query returns hits in both
 * languages. The per-locale loaders stay separate for routing; this merges their
 * pages into one source whose URLs already carry the /zh prefix, and hands
 * back each page's own locale-prefixed tree so breadcrumbs still resolve.
 */
export const GET: APIRoute = async () => {
  const pages: MergedPage[] = [];
  const trees = new Map<LocaleCode, Root>();

  for (const locale of localeCodes) {
    const { source } = await getLocaleSource(locale);
    trees.set(locale, localizedTree(locale));

    for (const page of source.getPages()) {
      pages.push({
        url: localizedURL(locale, page.url),
        slugs: page.slugs,
        locale,
        data: {
          title: page.data.title,
          description: page.data.description,
          structuredData: getStructuredData(page.data._raw),
          image: getPageImageUrl(page, locale).url,
        },
      });
    }
  }

  const byURL = new Map(pages.map((page) => [page.url, page]));

  const server = createFromSource(
    {
      getPages: () => pages,
      getPage: (url: string) => byURL.get(url),
      getPageTree: (locale?: string) => trees.get(locale ?? defaultLocale)!,
    },
    {
      buildIndex(page) {
        const { title, description, structuredData, image } = page.data;
        return { id: page.url, url: page.url, title, description, structuredData, image };
      },
    },
  );

  return server.staticGET();
};
