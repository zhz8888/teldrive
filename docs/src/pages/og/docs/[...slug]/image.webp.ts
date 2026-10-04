import type { APIRoute, GetStaticPaths } from 'astro';
import { generateOGImage } from 'fumadocs-ui/og/takumi';
import { getLocaleSource } from '@/lib/source';
import { defaultLocale, isLocaleCode, localeCodes, type LocaleCode } from '@/lib/i18n';

export const getStaticPaths = (async () => {
  const paths: { params: { slug: string } }[] = [];

  for (const locale of localeCodes) {
    const { source } = await getLocaleSource(locale);

    for (const page of source.getPages()) {
      // A page with no slug is the index; it needs a path segment of its own.
      const slugs = page.slugs.length > 0 ? page.slugs : ['index'];
      paths.push({
        params: {
          slug: [...(locale === defaultLocale ? [] : [locale]), ...slugs, 'image.webp'].join('/'),
        },
      });
    }
  }

  return paths;
}) satisfies GetStaticPaths;

export const GET: APIRoute = async ({ params }) => {
  const segments = params.slug?.split('/').filter((item) => item.length > 0) ?? [];

  // The trailing "image.webp" is part of the route, not of the page slug.
  const pageSegments = segments.slice(0, -1);
  const locale: LocaleCode = isLocaleCode(pageSegments[0]) ? pageSegments.shift()! : defaultLocale;
  const slugs = pageSegments.length === 1 && pageSegments[0] === 'index' ? [] : pageSegments;

  const { source } = await getLocaleSource(locale);
  const page = source.getPage(slugs);

  if (!page) return new Response(undefined, { status: 404 });

  return generateOGImage({
    title: page.data.title,
    description: page.data.description,
    site: 'Astro',
    format: 'webp',
  });
};
