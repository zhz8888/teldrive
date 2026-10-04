import { localeCodes, locales, localizedPath, type LocaleCode } from '../lib/i18n';

/**
 * Language switcher for the docs navbar. Every locale keeps the same slug, so
 * switching rewrites only the locale prefix and the reader stays on the page
 * they were reading, in the other language.
 */
export function LocaleSwitcher({ pathname }: { pathname: string }) {
  const current = currentLocale(pathname);

  return (
    <nav aria-label="Language" className="flex items-center gap-1 text-sm">
      {localeCodes.map((code) => {
        const active = code === current;
        return (
          <a
            key={code}
            href={localizedPath(code, pathname)}
            hrefLang={locales[code].htmlLang}
            lang={locales[code].htmlLang}
            aria-current={active ? 'true' : undefined}
            className={
              active
                ? 'rounded-md px-2 py-1 font-medium text-fd-foreground'
                : 'rounded-md px-2 py-1 text-fd-muted-foreground transition-colors hover:bg-fd-accent hover:text-fd-foreground'
            }
          >
            {locales[code].label}
          </a>
        );
      })}
    </nav>
  );
}

function currentLocale(pathname: string): LocaleCode {
  const [, first] = pathname.replace(/^\//, '').split('/');
  return first in locales ? (first as LocaleCode) : 'en';
}
