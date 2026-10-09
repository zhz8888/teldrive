// Locales the documentation site is published in. The English pages keep the
// unprefixed URLs, so links that predate the Chinese edition keep working;
// every other locale lives under `/<code>/`.
export const locales = {
  en: { code: 'en', label: 'English', htmlLang: 'en' },
  zh: { code: 'zh', label: '简体中文', htmlLang: 'zh-CN' },
} as const;

/** The identifiers of the locales the site is published in. */
export type LocaleCode = keyof typeof locales;

/** The locale published without a URL prefix, which is also the fallback. */
export const defaultLocale: LocaleCode = 'en';

/** Locale codes in declaration order; switchers and path builders iterate this. */
export const localeCodes = Object.keys(locales) as LocaleCode[];

/** Narrows a path segment or query value to a locale code we publish. */
export function isLocaleCode(value: string | undefined): value is LocaleCode {
  return value !== undefined && value in locales;
}

/** Returns the display metadata (label, htmlLang) of a locale. */
export function localeOf(locale: LocaleCode) {
  return locales[locale];
}

/**
 * prefixPath adds the locale prefix to a documentation path. The default locale
 * is published without one, so this returns the path unchanged for English.
 * BASE_URL is applied separately, by deploymentBase below or by prefixBase in
 * lib/source.ts.
 */
export function prefixPath(locale: LocaleCode, path: string): string {
  if (locale === defaultLocale) return path;
  return `/${locale}${path === '/' ? '' : path}`;
}

/**
 * deploymentBase is BASE_URL without its trailing slash: the sub-path the site
 * is served under, which is /<repository> on GitHub Pages and nothing on
 * Cloudflare Pages or a local build. Site-rooted links carry it.
 */
function deploymentBase(): string {
  return import.meta.env.BASE_URL.replace(/\/$/, '');
}

/**
 * withoutBase removes the deployment prefix from a site pathname, once. A
 * prerendered page reaches the switcher either as a route pathname or with
 * BASE_URL already applied, and both forms have to be site-relative before the
 * locale is rewritten.
 */
function withoutBase(pathname: string): string {
  const base = deploymentBase();
  if (!base) return pathname;
  if (pathname === base) return '/';
  return pathname.startsWith(`${base}/`) ? pathname.slice(base.length) : pathname;
}

/**
 * localeFromPath extracts the locale from a site pathname, defaulting to the
 * default locale when the first segment is not a locale code. This is what lets
 * the language switcher work on an unprefixed English URL, with or without the
 * deployment prefix.
 */
export function localeFromPath(pathname: string): LocaleCode {
  const [first] = withoutBase(pathname).replace(/^\//, '').split('/');
  return isLocaleCode(first) ? first : defaultLocale;
}

/**
 * localizedPath rewrites a documentation path so it points at another locale,
 * and roots the result at the deployment prefix. The switcher renders this into
 * an href, so a path that dropped BASE_URL would send the reader to the domain
 * root: on GitHub Pages, where the site lives under the repository name, that is
 * a 404 instead of the Chinese page.
 */
export function localizedPath(locale: LocaleCode, pathname: string): string {
  const segments = withoutBase(pathname).replace(/^\//, '').split('/');
  if (isLocaleCode(segments[0])) segments.shift();
  return `${deploymentBase()}${prefixPath(locale, `/${segments.join('/')}`)}`;
}
