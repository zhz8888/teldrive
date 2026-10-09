/** User-controlled appearance of the ebook reader; persisted by the caller, not here. */
export interface ReaderPreferences {
  /** Named colour scheme: "paper" renders light, "night" dark. */
  theme: string;
  /** Foliate layout flow: "paginated" or "scrolled". */
  flow: string;
  /** Font family key: "publisher" keeps the book's own fonts, "serif" or "sans" override it. */
  font: string;
  /** Text size as a percentage of the book's own size (100 keeps it unchanged). */
  fontSize: number;
  /** Unitless `line-height` multiplier applied to body text. */
  lineHeight: number;
  /** Page margin in pixels, passed to foliate's renderer as its `margin`. */
  margin: number;
  /** Maximum column count the paginated renderer may use. */
  columns: number;
}

/** The slice of a foliate resource descriptor this module inspects. */
type PublicationResource = {
  /** Path of the resource inside the publication (`detail.name` in foliate). */
  name?: string;
  /** Manifest media type of the resource. */
  type?: string;
  /** Resource payload as foliate has loaded it; markup arrives as a string. */
  data: unknown;
};

/**
 * Media types whose payload is markup that has to be sanitised before it is rendered.
 * `image/svg+xml` belongs here: foliate treats a spine item of that type like a chapter and
 * loads it into the same sandboxed frame, where an SVG `script` element or `onload`
 * attribute runs with the reader's session, so an SVG is not an opaque image either. CSS,
 * fonts and raster images are still passed through untouched.
 */
const MARKUP_MEDIA_TYPES = new Set(["application/xhtml+xml", "text/html", "image/svg+xml"]);
// Path fallback for resources whose manifest omits or mislabels the media type; tested
// against the resource path with any query string or fragment stripped.
const MARKUP_PATH_PATTERN = /\.(?:x?html?|xhtm|svg)$/;
/** Elements that can load or run another document inside the chapter frame. */
const DANGEROUS_ELEMENTS = "script, iframe, object, embed";
/** URL schemes that execute markup or script instead of fetching a resource. */
const DANGEROUS_URL_PREFIXES = ["javascript:", "data:text/html"];
/** Attributes whose value is a list of comma-separated URLs, not one URL. */
const URL_LIST_ATTRIBUTES = new Set(["srcset", "imagesrcset"]);

/**
 * Whether a resource is markup that has to be sanitised before foliate renders it. An SVG
 * counts even though its media type is an image: a spine item of that type is loaded into a
 * chapter frame, where a `<script>` or an `on*` handler would run with the reader's session.
 * The same check therefore also covers SVG resources that are only referenced as an image,
 * which costs one parse and saves the two cases from having to be told apart.
 */
export function isPublicationMarkup(type: unknown, name: unknown): boolean {
  const mediaType = typeof type === "string" ? mediaTypeOf(type) : "";
  if (MARKUP_MEDIA_TYPES.has(mediaType)) return true;
  const path = typeof name === "string" ? (name.split(/[?#]/)[0] ?? "").toLowerCase() : "";
  return MARKUP_PATH_PATTERN.test(path);
}

/**
 * Removes everything a shared publication could use to run script in its
 * chapter frame: foliate renders each chapter in a same-origin iframe
 * (`sandbox="allow-same-origin allow-scripts"`), so a chapter that ships a
 * script element, an inline event handler or a `javascript:` link would run
 * with the reader's session. An SVG chapter is cleaned by the same code: it is
 * XML, so it parses as one and its elements and attributes are treated exactly
 * like a chapter's. Pure function: markup in, sanitised markup out.
 *
 * A chapter the XML parser rejects is handled by
 * {@link sanitizePublicationMarkupFallback} instead of being dropped, because
 * foliate itself falls back to parsing such chapters as HTML.
 */
export function sanitizePublicationMarkup(markup: string): string {
  const doc = parsePublicationMarkup(markup);
  if (!doc) return sanitizePublicationMarkupFallback(markup);
  for (const element of [...doc.querySelectorAll(DANGEROUS_ELEMENTS)]) element.remove();
  for (const element of doc.querySelectorAll("*")) sanitizeElementAttributes(element);
  return new XMLSerializer().serializeToString(doc);
}

/**
 * String-level counterpart of {@link sanitizePublicationMarkup} for markup the
 * XML parser refuses. It removes the same constructs textually, which is weaker
 * than parsing but still keeps the chapter (and nothing else) on screen.
 */
export function sanitizePublicationMarkupFallback(markup: string): string {
  return markup
    .replace(/<\s*(script|iframe|object|embed)\b[\s\S]*?<\s*\/\s*\1\s*>/gi, "")
    .replace(/<\s*(?:script|iframe|object|embed)\b[^>]*>/gi, "")
    .replace(/\son[a-z-]+\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+)/gi, "")
    .replace(
      /\b(href|src|srcset|imagesrcset|poster|data|action|formaction|xlink:href)\s*=\s*(?:"([^"]*)"|'([^']*)')/gi,
      (match, name: string, doubleQuoted?: string, singleQuoted?: string) => {
        const value = doubleQuoted ?? singleQuoted ?? "";
        const sanitized = sanitizeUrlAttribute(name.toLowerCase(), value);
        if (sanitized === value) return match;
        return sanitized ? `${name}="${sanitized}"` : "";
      },
    );
}

/**
 * Parses publication markup as XML, returning undefined for anything the XML parser rejects
 * (malformed XHTML is common in real books) so the caller can fall back to string-level
 * sanitising instead of losing the chapter. Both a chapter and an SVG are XML, so one parse
 * handles either.
 */
function parsePublicationMarkup(markup: string): Document | undefined {
  try {
    const doc = new DOMParser().parseFromString(markup, "application/xhtml+xml");
    // A rejected chapter parses into a `parsererror` document whose root has no
    // namespace; serialising that back would replace the chapter with the error.
    if (!doc.documentElement?.namespaceURI || doc.querySelector("parsererror")) return undefined;
    return doc;
  } catch {
    return undefined;
  }
}

/**
 * Strips the event handlers and dangerous URLs from one element in place: every `on*`
 * attribute is removed; other attributes are rewritten only if their value is dangerous.
 */
function sanitizeElementAttributes(element: Element) {
  for (const attribute of [...element.attributes]) {
    const name = attribute.name.toLowerCase();
    if (name.startsWith("on")) {
      element.removeAttribute(attribute.name);
      continue;
    }
    if (attribute.namespaceURI) {
      // Rewriting a namespaced attribute (`xlink:href`) through `setAttribute`
      // would drop its namespace, so it is only removed when it is dangerous.
      if (isDangerousUrl(attribute.value)) element.removeAttribute(attribute.name);
      continue;
    }
    const value = attribute.value;
    const sanitized = sanitizeUrlAttribute(name, value);
    if (sanitized === value) continue;
    if (sanitized) element.setAttribute(attribute.name, sanitized);
    else element.removeAttribute(attribute.name);
  }
}

/**
 * Drops the URL (or URLs) in an attribute value that would execute markup
 * instead of loading a resource. `srcset`-style attributes hold several
 * comma-separated candidates; every other URL attribute holds exactly one, and
 * an inline `style` only has to be dropped when it embeds such a URL at all.
 */
function sanitizeUrlAttribute(name: string, value: string): string {
  if (name === "style") return containsDangerousUrl(value) ? "" : value;
  if (!URL_LIST_ATTRIBUTES.has(name)) return isDangerousUrl(value) ? "" : value;
  const candidates = value.split(",");
  if (!candidates.some(isDangerousUrl)) return value;
  return candidates.filter((candidate) => !isDangerousUrl(candidate)).join(",");
}

/** True when a dangerous URL appears anywhere in the value (used for inline `style`). */
function containsDangerousUrl(value: string): boolean {
  return DANGEROUS_URL_PREFIXES.some((prefix) => normalizeUrl(value).includes(prefix));
}

/** True when the value itself is a dangerous URL, as opposed to merely containing one. */
function isDangerousUrl(value: string): boolean {
  return DANGEROUS_URL_PREFIXES.some((prefix) => normalizeUrl(value).startsWith(prefix));
}

/** Drops the parameters from a media type (`text/html; charset=utf-8` -> `text/html`). */
function mediaTypeOf(type: string): string {
  return (type.split(";")[0] ?? "").trim().toLowerCase();
}

/**
 * Whitespace and control characters are ignored inside a URL scheme, so
 * `java\tscript:` still runs: they are dropped before the scheme is compared.
 */
function normalizeUrl(value: string): string {
  let normalized = "";
  for (const character of value) {
    if ((character.codePointAt(0) ?? 0) > 0x20) normalized += character;
  }
  return normalized.toLowerCase();
}

/**
 * Loads a book file into a `<foliate-view>` element and shows it: it sanitises every markup
 * resource on its way in (see {@link sanitizePublicationMarkup}), attaches the `load` and
 * `relocate` listeners, opens the book, applies the appearance and initialises the position
 * (the saved `lastLocation` when one is given, otherwise the text start). The listeners stay
 * attached to `element`, so the caller removes them on teardown. Resolves with the foliate
 * book object, whose metadata and TOC the caller reads before {@link closePublication}
 * destroys it.
 */
export async function openPublication({
  element,
  file,
  preferences,
  lastLocation,
  onLoad,
  onRelocate,
}: {
  element: FoliateViewElement;
  file: File;
  preferences: ReaderPreferences;
  lastLocation?: string | { fraction: number };
  onLoad: (event: CustomEvent<{ doc: Document; index: number }>) => void;
  onRelocate: (event: CustomEvent<FoliateRelocateDetail>) => void;
}) {
  const { makeBook } = await import("foliate-js/view.js");
  const book = await makeBook(file);
  // Chapters and SVGs reach the reader as a chapter frame's document, so every markup
  // resource is sanitised here, before foliate turns it into a blob URL. Foliate hands
  // markup over as a string (it re-serialises chapters, SVG and CSS after rewriting their
  // links) while fonts and raster images arrive as bytes, which is why only a string
  // payload is a document this module has to clean.
  book.transformTarget?.addEventListener("data", ({ detail }: CustomEvent<PublicationResource>) => {
    detail.data = Promise.resolve(detail.data)
      .then((data: unknown) =>
        typeof data === "string" && isPublicationMarkup(detail.type, detail.name)
          ? sanitizePublicationMarkup(data)
          : data,
      )
      .catch((error: unknown) => {
        console.error(new Error(`Failed to load ${detail.name}`, { cause: error }));
        return "";
      });
  });

  element.addEventListener("load", onLoad as EventListener);
  element.addEventListener("relocate", onRelocate as EventListener);
  await element.open(book);
  applyPublicationAppearance(element, preferences);
  await element.init({ lastLocation, showTextStart: true });
  return book;
}

/**
 * Pushes the appearance preferences into an already-open view: it re-reads the reader's
 * CSS custom properties from the host element so the chapter frames match the app theme,
 * configures foliate's renderer attributes, and injects a stylesheet into each chapter.
 * Safe to call repeatedly — the caller re-runs it whenever a preference changes.
 */
export function applyPublicationAppearance(
  element: FoliateViewElement,
  preferences: ReaderPreferences,
) {
  const { theme, flow, font, fontSize, lineHeight, margin, columns } = preferences;
  element.dataset.readerTheme = theme;
  const styles = getComputedStyle(element);
  const page = styles.getPropertyValue("--reader-page").trim();
  const text = styles.getPropertyValue("--reader-foreground").trim();
  const link = styles.getPropertyValue("--reader-link").trim();
  element.renderer?.setAttribute("flow", flow);
  element.renderer?.setAttribute("gap", "6%");
  element.renderer?.setAttribute("margin", `${margin}px`);
  element.renderer?.setAttribute("max-inline-size", "720px");
  element.renderer?.setAttribute("max-block-size", "1440px");
  element.renderer?.setAttribute("max-column-count", String(columns));
  element.renderer?.setAttribute("animated", "");
  element.renderer?.setStyles?.(`
    @namespace epub "http://www.idpf.org/2007/ops";
    :root { color-scheme: ${theme === "night" ? "dark" : "light"}; }
    html {
      color: ${text};
      line-height: ${lineHeight};
      hanging-punctuation: allow-end last;
      orphans: 2;
      widows: 2;
    }
    html, body { background: ${page} !important; }
    body {
      font-family: ${readerFont(font)} !important;
      font-size: ${fontSize}% !important;
      text-rendering: optimizeLegibility;
    }
    p, li, blockquote, dd { line-height: ${lineHeight}; }
    [align="left"] { text-align: left; }
    [align="right"] { text-align: right; }
    [align="center"] { text-align: center; }
    [align="justify"] { text-align: justify; }
    h1, h2, h3, h4, h5, h6, hgroup, th { text-wrap: balance; }
    pre { white-space: pre-wrap !important; }
    img, svg, video { max-width: 100%; }
    a:any-link { color: ${link}; }
  `);
}

/**
 * Tears a view down: closes foliate's renderer and destroys the book, releasing the
 * per-chapter blob URLs it created. Both calls are synchronous, which is what lets the
 * caller run it while unmounting.
 */
export function closePublication(element: FoliateViewElement) {
  element.close();
  element.book?.destroy?.();
}

/** CSS font stack for a font preference; anything unrecognised keeps the book's own fonts. */
function readerFont(font: string) {
  if (font === "serif") return 'Iowan Old Style, Charter, "Bitstream Charter", Georgia, serif';
  if (font === "sans") return 'Avenir Next, Avenir, "Segoe UI", sans-serif';
  return "inherit";
}
