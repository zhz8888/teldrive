import { createGetUrl } from 'fumadocs-core/source';
import { defaultLocale, type LocaleCode } from './i18n';

/** Site-rooted route that serves the generated Open Graph images of pages. */
export const docsImageRoute = `${import.meta.env.BASE_URL.replace(/\/$/, '')}/og/docs`;

/** Builds paths under docsImageRoute; getPageImageUrl appends the segments. */
const getImageUrl = createGetUrl(docsImageRoute);

/**
 * getPageImageUrl builds the OG-image URL for a page. The locale is part of the
 * route rather than a query value, mirroring the unprefixed English pages and
 * the /zh-prefixed Chinese ones.
 */
export function getPageImageUrl(page: { slugs: string[] }, locale: LocaleCode) {
  const segments = [
    ...(locale === defaultLocale ? [] : [locale]),
    ...page.slugs,
    'image.webp',
  ];

  return { segments, url: getImageUrl(segments) };
}
