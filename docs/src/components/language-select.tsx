'use client';

import { Check, ChevronDown } from 'lucide-react';
import { Popover, PopoverContent, PopoverTrigger } from 'fumadocs-ui/components/ui/popover';
import { localeCodes, localeFromPath, locales, localizedPath, type LocaleCode } from '../lib/i18n';

/** Accessible name of the trigger button, per locale. */
const labels: Record<LocaleCode, string> = {
  en: 'Change language',
  zh: '切换语言',
};

/**
 * Language switcher for the docs navigation: a single button that opens the list
 * of published locales, because a row of links spends navbar width on a choice
 * most readers make once. The button carries the locale in use, and every locale
 * keeps the same slug, so picking one rewrites only the locale prefix and the
 * reader stays on the page they were reading.
 */
export function LanguageSelect({ pathname }: { pathname: string }) {
  const current = localeFromPath(pathname);

  return (
    <Popover>
      <PopoverTrigger
        aria-label={`${labels[current]}: ${locales[current].label}`}
        className="inline-flex h-8 shrink-0 items-center gap-1.5 rounded-md px-2 text-sm whitespace-nowrap text-fd-muted-foreground transition-colors hover:bg-fd-accent hover:text-fd-accent-foreground"
      >
        <span>{locales[current].label}</span>
        <ChevronDown className="size-3.5" aria-hidden="true" />
      </PopoverTrigger>
      <PopoverContent align="end" className="min-w-40 p-1">
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
                  ? 'flex items-center gap-2 rounded-md px-2 py-1.5 font-medium'
                  : 'flex items-center gap-2 rounded-md px-2 py-1.5 text-fd-muted-foreground hover:bg-fd-accent hover:text-fd-accent-foreground'
              }
            >
              <span>{locales[code].label}</span>
              {active ? <Check className="ms-auto size-3.5" aria-hidden="true" /> : null}
            </a>
          );
        })}
      </PopoverContent>
    </Popover>
  );
}
