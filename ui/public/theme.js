/**
 * Applies the stored colour theme before the first paint, so the interface
 * never flashes the wrong mode while the application bundle loads.
 *
 * The stored value and its meaning match what next-themes reads and writes:
 * localStorage "theme" holds "light", "dark" or "system", and an explicit
 * choice wins over the system preference. The class, data attribute and
 * color-scheme written here are the same ones next-themes maintains once React
 * mounts, so the two never disagree.
 *
 * It is a separate file rather than an inline script because the server sends
 * `script-src 'self'`, which refuses inline scripts; `e2e/source-audit.spec.ts`
 * guards that boundary.
 */
(() => {
  const root = document.documentElement;
  const media = window.matchMedia("(prefers-color-scheme: dark)");
  const THEME_COLORS = { light: "#fafafa", dark: "#1a1a1a" };

  const storedTheme = () => {
    try {
      return window.localStorage.getItem("theme");
    } catch {
      return null;
    }
  };

  const resolveTheme = () => {
    const stored = storedTheme();
    if (stored === "light" || stored === "dark") return stored;
    return media.matches ? "dark" : "light";
  };

  const paintThemeColor = (theme) => {
    const meta = document.querySelector('meta[name="theme-color"]');
    if (meta) meta.setAttribute("content", THEME_COLORS[theme]);
  };

  const applyTheme = () => {
    const theme = resolveTheme();
    root.classList.remove("light", "dark");
    root.classList.add(theme);
    root.dataset.theme = theme;
    root.style.colorScheme = theme;
    paintThemeColor(theme);
  };

  applyTheme();
  media.addEventListener("change", applyTheme);
  // A runtime switch only changes the class, so the browser chrome colour
  // follows it without re-resolving the stored value.
  new MutationObserver(() => {
    paintThemeColor(root.classList.contains("dark") ? "dark" : "light");
  }).observe(root, { attributes: true, attributeFilter: ["class"] });
})();
