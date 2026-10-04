// Locales the documentation site is published in. The English pages keep the
// unprefixed URLs, so links that predate the Chinese edition keep working;
// every other locale lives under `/<code>/`.
export const locales = {
  en: { code: 'en', label: 'English', htmlLang: 'en' },
  zh: { code: 'zh', label: '简体中文', htmlLang: 'zh-CN' },
} as const;

export type LocaleCode = keyof typeof locales;

export const defaultLocale: LocaleCode = 'en';

export const localeCodes = Object.keys(locales) as LocaleCode[];

export function isLocaleCode(value: string | undefined): value is LocaleCode {
  return value !== undefined && value in locales;
}

export function localeOf(locale: LocaleCode) {
  return locales[locale];
}

/**
 * prefixPath adds the locale prefix to a documentation path. The default locale
 * is published without one, so this returns the path unchanged for English.
 * BASE_URL is applied separately by prefixBase in lib/source.ts.
 */
export function prefixPath(locale: LocaleCode, path: string): string {
  if (locale === defaultLocale) return path;
  return `/${locale}${path === '/' ? '' : path}`;
}

/**
 * localeFromPath extracts the locale from a site pathname, defaulting to the
 * default locale when the first segment is not a locale code. This is what lets
 * the language switcher work on an unprefixed English URL.
 */
export function localeFromPath(pathname: string): LocaleCode {
  const [, first] = pathname.replace(/^\//, '').split('/');
  return isLocaleCode(first) ? first : defaultLocale;
}

/** localizedPath rewrites a documentation path so it points at another locale. */
export function localizedPath(locale: LocaleCode, pathname: string): string {
  const base = import.meta.env.BASE_URL.replace(/\/$/, '');
  const path = pathname.startsWith(base) ? pathname.slice(base.length) : pathname;
  const segments = path.replace(/^\//, '').split('/');
  if (isLocaleCode(segments[0])) segments.shift();
  return prefixPath(locale, `/${segments.join('/')}`);
}
