/**
 * Types shared by the interface catalogs.
 *
 * English under `messages/en` is the source of truth: it defines the key set and
 * the kind of every entry. A translated catalog is typed with `AreaMessages` over
 * its English counterpart, so a missing key, an extra key or a plural turned into
 * a fixed string is a compile error rather than a silent fallback.
 */

/** Locales the interface ships with. */
export type Locale = "en" | "zh";

/**
 * Plural entry: `one` is used when the `count` parameter is exactly 1 and `other`
 * otherwise, so a language without plural agreement repeats the same text.
 */
export type Plural = { one: string; other: string };

/** One catalog entry: fixed text, or text chosen by a count. */
export type Message = string | Plural;

/** Catalog shape required of a translation of T. */
export type AreaMessages<T> = { [K in keyof T]: T[K] extends string ? string : Plural };

/** Values substituted into the `{placeholder}` slots of a message. */
export type MessageParams = Record<string, string | number>;
