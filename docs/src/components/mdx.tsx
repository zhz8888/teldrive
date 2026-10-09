import type { ComponentProps } from 'react';
import defaultMdxComponents from 'fumadocs-ui/mdx';
import { defaultLocale, isUnlocalizedPath, prefixPath, type LocaleCode } from '../lib/i18n';

/**
 * withPrefix resolves a documentation href for one locale. Site-rooted hrefs
 * (/getting-started/quick-start) are re-rooted under the locale prefix; relative
 * hrefs, external URLs, pure anchors and asset paths are left alone, because a
 * Chinese page must not link a reader back to the English page.
 */
function withPrefix(locale: LocaleCode, href: string | undefined) {
  if (!href || locale === defaultLocale) return href;
  if (!href.startsWith('/') || href.startsWith('//')) return href;
  // Routes published in one language only keep their site-rooted path.
  if (isUnlocalizedPath(href)) return href;
  // Asset paths such as /og/docs/x/image.webp are served by the site root.
  if (/\.[a-zA-Z0-9]+$/.test(href)) return href;

  const [path, hash] = href.split('#');
  return `${prefixPath(locale, path)}${hash ? `#${hash}` : ''}`;
}

/**
 * withBase prefixes a site-rooted href with the configured BASE_URL, so links
 * keep resolving when the docs are served from a sub-path. Anything that is not
 * an absolute site path is returned unchanged.
 */
function withBase(href: string | undefined) {
  if (!href || !href.startsWith('/') || href.startsWith('//')) return href;

  const base = import.meta.env.BASE_URL.replace(/\/$/, '');
  return `${base}${href}`;
}

/** The anchor fumadocs maps MDX links to; the wrapper below builds on it. */
const DefaultLink = defaultMdxComponents.a;
/** The card fumadocs maps MDX `<Card>` to; the wrapper below builds on it. */
const DefaultCard = defaultMdxComponents.Card;

/**
 * getMdxComponents builds the MDX component map for one locale. Internal links
 * are locale-rooted so a Chinese page links to the Chinese page of the same
 * slug instead of falling back to English.
 */
export function getMdxComponents(locale: LocaleCode) {
  /** Anchor that roots its href in this locale and applies BASE_URL. */
  function Link(props: ComponentProps<typeof DefaultLink>) {
    return <DefaultLink {...props} href={withBase(withPrefix(locale, props.href))} />;
  }

  /** Card whose href is localized the same way as a link. */
  function Card(props: ComponentProps<typeof DefaultCard>) {
    return <DefaultCard {...props} href={withBase(withPrefix(locale, props.href))} />;
  }

  /** Landing-page call to action; styles an anchor and localizes href as Link does. */
  function ButtonLink({ className, ...props }: ComponentProps<'a'>) {
    return (
      <a
        {...props}
        href={withBase(withPrefix(locale, props.href))}
        className={`not-prose inline-flex items-center rounded-lg bg-fd-primary px-4 py-2.5 font-medium text-fd-primary-foreground no-underline hover:opacity-90 ${className ?? ''}`}
      />
    );
  }

  return {
    ...defaultMdxComponents,
    a: Link,
    Card,
    ButtonLink,
  };
}
