/**
 * Interface localization.
 *
 * Every user-visible phrase lives in a catalog under `messages/`, keyed the same
 * way in English and Chinese; components ask for a key with `useI18n().t` instead
 * of embedding the text. English is the source of truth for the key set, so a
 * translation cannot silently drop or invent an entry.
 *
 * What is deliberately *not* translated: log output of any kind. `console.*`
 * messages, the job and task traces the API returns, and everything the server
 * writes stay in English, because they are read by operators and pasted into bug
 * reports. `ui/e2e/source-audit.spec.ts` enforces that boundary by rejecting CJK
 * characters outside this directory.
 */
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { en, type MessageKey } from "./messages/en";
import { zh } from "./messages/zh";
import type { AreaMessages, Locale, Message, MessageParams } from "./types";

export type { Locale, MessageParams } from "./types";
export type { MessageKey } from "./messages/en";

/** Catalogs by locale. Each one is complete, so a lookup never falls back. */
const CATALOGS: Record<Locale, AreaMessages<typeof en>> = { en, zh };

/** Locales offered by the language switcher, in display order. */
export const LOCALES: readonly Locale[] = ["en", "zh"];

/**
 * Endonyms of the locales: a language is always listed in its own language, so
 * someone who cannot read the current interface language can still find theirs.
 */
export const LOCALE_LABELS: Record<Locale, string> = {
  en: "English",
  zh: "简体中文",
};

/** Where an explicit choice is remembered between visits. */
export const LOCALE_STORAGE_KEY = "teldrive.locale";

/** BCP 47 tag applied to <html lang> for each locale. */
const HTML_LANG: Record<Locale, string> = { en: "en", zh: "zh-CN" };

/** Reports whether a string is one of the supported locales. */
export function isLocale(value: unknown): value is Locale {
  return value === "en" || value === "zh";
}

/**
 * Picks the locale to start with: the remembered choice when there is one,
 * otherwise the first supported language the browser asks for, otherwise English.
 */
export function detectLocale(): Locale {
  if (typeof window === "undefined") return "en";
  try {
    const stored = window.localStorage.getItem(LOCALE_STORAGE_KEY);
    if (isLocale(stored)) return stored;
  } catch {
    // Storage can be unavailable (private mode, blocked cookies); the browser
    // preference below still applies and the choice is simply not remembered.
  }
  const requested = window.navigator.languages?.length
    ? window.navigator.languages
    : [window.navigator.language];
  for (const language of requested) {
    if (language?.toLowerCase().startsWith("zh")) return "zh";
  }
  return "en";
}

/** Substitutes `{name}` slots with the matching parameter, leaving unknown ones. */
function interpolate(template: string, params?: MessageParams): string {
  if (!params) return template;
  return template.replace(/\{(\w+)\}/g, (match, name: string) => {
    const value = params[name];
    return value === undefined ? match : String(value);
  });
}

/**
 * Looks a key up in a catalog. The catalogs are declared complete, but the lookup
 * is typed as possibly missing so a stale key can be reported instead of breaking
 * the screen that renders it.
 */
function lookup(locale: Locale, key: MessageKey): Message | undefined {
  const catalog: Partial<Record<MessageKey, Message>> = CATALOGS[locale] ?? en;
  return catalog[key];
}

/**
 * Resolves a key for a locale, substituting params. A plural entry picks `one`
 * when `count` is exactly 1 and `other` otherwise. The key itself is returned when
 * a catalog has no entry for it, which keeps a stale key visible instead of
 * rendering an empty label.
 */
export function translate(locale: Locale, key: MessageKey, params?: MessageParams): string {
  const entry = lookup(locale, key);
  if (entry === undefined) return key;
  if (typeof entry === "string") return interpolate(entry, params);
  return interpolate(params?.count === 1 ? entry.one : entry.other, params);
}

type I18nValue = {
  /** Locale currently rendered. */
  locale: Locale;
  /** Switches locale and remembers the choice for the next visit. */
  setLocale: (locale: Locale) => void;
  /** Resolves a message key for the current locale. */
  t: (key: MessageKey, params?: MessageParams) => string;
};

/**
 * Locale used by `t` outside React. The provider keeps it in step with the
 * rendered locale, so a store or a plain helper that produces interface text (an
 * upload error, an API failure message) can translate without a hook.
 */
let activeLocale: Locale = detectLocale();

/** Returns the locale `t` resolves against outside React. */
export function currentLocale(): Locale {
  return activeLocale;
}

/** Records the rendered locale for `t`; called by the provider, not by screens. */
function setActiveLocale(locale: Locale) {
  activeLocale = locale;
  if (typeof document !== "undefined") document.documentElement.lang = HTML_LANG[locale];
}

/**
 * Resolves a message key against the active locale. Components use `useI18n().t`
 * so they re-render when the locale changes; this form exists for code that runs
 * outside React, such as the upload store.
 */
export function t(key: MessageKey, params?: MessageParams): string {
  return translate(activeLocale, key, params);
}

const I18nContext = createContext<I18nValue | null>(null);

/**
 * Provides the current locale and the `t` lookup to the tree below. It must wrap
 * every screen, because `useI18n` throws outside it.
 */
export function I18nProvider({ children }: { children: ReactNode }) {
  const [locale, setLocaleState] = useState<Locale>(activeLocale);

  useEffect(() => {
    setActiveLocale(locale);
  }, [locale]);

  const setLocale = useCallback((next: Locale) => {
    setActiveLocale(next);
    setLocaleState(next);
    try {
      window.localStorage.setItem(LOCALE_STORAGE_KEY, next);
    } catch {
      // Not remembering the choice is acceptable; it still applies to this visit.
    }
  }, []);

  const value = useMemo<I18nValue>(
    () => ({ locale, setLocale, t: (key, params) => translate(locale, key, params) }),
    [locale, setLocale],
  );

  return <I18nContext.Provider value={value}>{children}</I18nContext.Provider>;
}

/**
 * Returns the current locale, the switcher and the message lookup. Throws when the
 * component is rendered outside `I18nProvider`, which would otherwise show keys.
 */
export function useI18n(): I18nValue {
  const value = useContext(I18nContext);
  if (!value) throw new Error("useI18n must be used inside I18nProvider");
  return value;
}
