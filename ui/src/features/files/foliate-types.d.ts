// foliate-js ships no type declarations; this shorthand makes the module importable and
// `any`-typed, while the interfaces below describe only what the reader wrapper touches.
declare module "foliate-js/view.js";

/** Payload of the view's `relocate` event, emitted whenever the reading position changes. */
interface FoliateRelocateDetail {
  /** EPUB CFI of the current position; foliate also keeps it as the view's last location. */
  cfi?: string;
  /** Position as a 0-1 fraction of the whole book, used for the progress bar. */
  fraction?: number;
  /**
   * Estimated position in foliate's synthetic location units; the reader displays them as
   * "Page current of total".
   */
  location?: { current?: number; total?: number };
  /** Table-of-contents entry the current position falls under. */
  tocItem?: { label?: string };
}

/** The `<foliate-view>` custom element created by `foliate-js/view.js`. */
interface FoliateViewElement extends HTMLElement {
  /** Set by `open()`; absent before the book is loaded and after `destroy()`. */
  book?: {
    /** Parsed publication metadata, e.g. `title` and `language`. */
    metadata?: Record<string, unknown>;
    /** Navigation tree; `subitems` mirrors the nested TOC structure. */
    toc?: Array<{ label: string; href: string; subitems?: unknown[] }>;
    /** Releases the book's resources, notably its per-chapter object URLs. */
    destroy?: () => void;
  };
  /** Foliate's renderer element; `setStyles` injects CSS into every chapter document. */
  renderer?: HTMLElement & {
    setStyles?: (css: string) => void;
  };
  /** Loads a `File` (or an already-parsed book object) and attaches a renderer. */
  open: (book: File | object) => Promise<void>;
  /** Must be called after `open`; restores `lastLocation` or jumps to the text start. */
  init: (options: {
    lastLocation?: string | { fraction: number };
    showTextStart?: boolean;
  }) => Promise<void>;
  /** Destroys the renderer and clears progress state; the book itself survives. */
  close: () => void;
  /** Navigates to a CFI string, an href, a spine index or a `{ fraction }` object. */
  goTo: (target: string | number | object) => Promise<void>;
  /** Navigates to a 0-1 fraction of the whole book. */
  goToFraction: (fraction: number) => Promise<void>;
  /** Moves back one page (or one scroll step) in the current flow. */
  goLeft: () => Promise<void>;
  /** Moves forward one page (or one scroll step) in the current flow. */
  goRight: () => Promise<void>;
}

// Declaration merging with the DOM lib, so `createElement("foliate-view")` is typed.
interface HTMLElementTagNameMap {
  /** Typed lookup for the reader's custom element. */
  "foliate-view": FoliateViewElement;
}
