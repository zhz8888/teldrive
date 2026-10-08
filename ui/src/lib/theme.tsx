import {
  createContext,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";
import { useMediaQuery } from "./use-media-query";

/**
 * Colour-theme state for the interface.
 *
 * This replaces next-themes. The one thing that library does beyond holding state
 * is render an inline script that sets the theme before hydration, and that script
 * cannot help here: the interface is client-rendered, the server answers with
 * `script-src 'self'` and so refuses inline scripts, and public/theme.js already
 * applies the stored theme before the first paint.
 *
 * The storage key, the attributes and the resolution rules below repeat what
 * theme.js does before React mounts — the same contract next-themes followed. The
 * stored value is "light", "dark" or "system", an explicit choice wins over the
 * system preference, and the mode is written to <html> as a class, as data-theme
 * and as color-scheme, so the pre-paint script and this provider never disagree.
 */

/**
 * What the user picked for the interface: an explicit mode, or `system` to follow
 * the operating system.
 */
export type Theme = "light" | "dark" | "system";
/** The mode that ends up on the document; `system` has been resolved away by then. */
export type ResolvedTheme = "light" | "dark";

/**
 * localStorage key holding the choice, and the same literal `public/theme.js` reads
 * before the first paint.
 */
const STORAGE_KEY = "theme";
/** Follow the operating system until the user makes an explicit choice. */
const DEFAULT_THEME: Theme = "system";
/** Media query that resolves `system` into a concrete mode. */
const DARK_QUERY = "(prefers-color-scheme: dark)";
/** Every value the storage key may hold; anything else is treated as no choice at all. */
const THEMES: readonly string[] = ["light", "dark", "system"];

/** Theme state shared through the context: the stored choice and what it resolves to. */
type ThemeContextValue = {
  /** The stored choice, or the default while no choice has been made. */
  theme: Theme;
  /** What the choice resolves to; never "system". */
  resolvedTheme: ResolvedTheme;
  /** Stores the choice and applies it immediately; the choice is per browser. */
  setTheme: (theme: Theme) => void;
};

/**
 * Null outside the provider, which is what makes `useTheme` fail instead of
 * rendering a tree that never had a theme applied.
 */
const ThemeContext = createContext<ThemeContextValue | null>(null);

/**
 * Reads the stored choice, falling back to the default when nothing usable is
 * stored. Storage access can throw where it is disabled, and a value written by an
 * older build must be ignored rather than applied.
 */
function readStoredTheme(): Theme {
  try {
    const stored = window.localStorage.getItem(STORAGE_KEY);
    if (stored && THEMES.includes(stored)) return stored as Theme;
  } catch {
    // Storage is unavailable; the system preference still decides the mode.
  }
  return DEFAULT_THEME;
}

/**
 * Writes the resolved mode to the same three places theme.js writes it. Repeating
 * the writes is harmless — the values are already correct for the mode that was
 * restored — and it keeps them correct on every later switch.
 */
function applyTheme(resolved: ResolvedTheme) {
  const root = document.documentElement;
  root.classList.remove("light", "dark");
  root.classList.add(resolved);
  root.dataset.theme = resolved;
  root.style.colorScheme = resolved;
}

/**
 * Keeps the theme choice for the tree below and writes the resolved mode to the
 * document. It must wrap anything that calls `useTheme`, and it resolves `system`
 * through a live media query, so an operating-system change while the page is open
 * follows the system mode without a reload.
 */
export function ThemeProvider({ children }: { children: ReactNode }) {
  // The stored choice is read during the first render, so the appearance page
  // marks the active choice immediately instead of after an effect.
  const [theme, setThemeState] = useState<Theme>(readStoredTheme);
  const systemTheme: ResolvedTheme = useMediaQuery(DARK_QUERY) ? "dark" : "light";
  const resolvedTheme: ResolvedTheme = theme === "system" ? systemTheme : theme;

  useEffect(() => {
    applyTheme(resolvedTheme);
  }, [resolvedTheme]);

  // A choice made in another tab arrives as a storage event, which is the only
  // notification this window gets about it.
  useEffect(() => {
    const onStorage = (event: StorageEvent) => {
      if (event.key !== STORAGE_KEY) return;
      setThemeState(readStoredTheme());
    };
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, []);

  const setTheme = useCallback((next: Theme) => {
    try {
      window.localStorage.setItem(STORAGE_KEY, next);
    } catch {
      // A choice that cannot be remembered still applies for this session.
    }
    setThemeState(next);
  }, []);

  const value = useMemo(
    () => ({ theme, resolvedTheme, setTheme }),
    [theme, resolvedTheme, setTheme],
  );

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

/**
 * The theme choice, its resolved mode and the setter. Throws outside
 * `ThemeProvider`, because a component that silently got no theme would render
 * against a document mode it never applied.
 */
export function useTheme() {
  const value = useContext(ThemeContext);
  if (!value) throw new Error("useTheme must be used inside ThemeProvider");
  return value;
}
