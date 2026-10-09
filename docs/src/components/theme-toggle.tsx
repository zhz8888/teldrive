'use client';

import { Moon, Sun } from 'lucide-react';
import { useTheme } from 'fumadocs-ui/provider/base';
import type { LocaleCode } from '../lib/i18n';

/**
 * Button wording per locale. The visible icon shows the mode the reader is in,
 * while the accessible name describes what the click does, so a screen reader
 * announces the action instead of the state.
 */
const labels: Record<LocaleCode, { light: string; dark: string; unknown: string }> = {
  en: { light: 'Switch to light mode', dark: 'Switch to dark mode', unknown: 'Switch theme' },
  zh: { light: '切换到浅色模式', dark: '切换到深色模式', unknown: '切换深浅色模式' },
};

/**
 * The docs navigation's light/dark switch: one button, and a click moves to the
 * other mode. The site's theme provider keeps `system` as the starting point, so
 * the first click pins an explicit mode rather than returning to the system one.
 *
 * Both icons are rendered and the dark variant decides which is visible, which
 * keeps the glyph correct before hydration; only the accessible name depends on
 * the resolved theme, and next-themes reports that after mount, so the server
 * markup and the first client render still agree.
 */
export function ThemeToggle({ locale }: { locale: LocaleCode }) {
  const { resolvedTheme, setTheme } = useTheme();
  const text = labels[locale];
  const label =
    resolvedTheme === 'dark' ? text.light : resolvedTheme === 'light' ? text.dark : text.unknown;

  return (
    <button
      type="button"
      aria-label={label}
      title={label}
      data-theme-toggle=""
      onClick={() => setTheme(resolvedTheme === 'dark' ? 'light' : 'dark')}
      className="inline-flex size-8 items-center justify-center rounded-md text-fd-muted-foreground transition-colors hover:bg-fd-accent hover:text-fd-accent-foreground"
    >
      <Sun className="size-4 dark:hidden" aria-hidden="true" />
      <Moon className="hidden size-4 dark:block" aria-hidden="true" />
    </button>
  );
}
