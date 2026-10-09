import { DocsLayout } from 'fumadocs-ui/layouts/docs';
import { DocsPage, type DocsPageProps } from 'fumadocs-ui/layouts/docs/page';
import type { Root } from 'fumadocs-core/page-tree';
import { useMemo, type ReactNode } from 'react';
import { navigate } from 'astro:transitions/client';
import { RootProvider } from 'fumadocs-ui/provider/astro';
import type { AstroProviderProps } from 'fumadocs-core/framework/astro';
import SearchDialog from './search';
import { LanguageSelect } from './language-select';
import { ThemeToggle } from './theme-toggle';
import { localizedPath } from '../lib/i18n';

/**
 * Docs is the documentation shell for one rendered page: it supplies fumadocs'
 * root context (theme, Astro router bridge, search dialog), the docs layout
 * with the sidebar tree and GitHub link, and this site's own navigation
 * controls. The `tree`, `pathname` and `params` all come from the route, so
 * navigation between pages stays client-side.
 */
export function Docs({
  tree,
  children,
  pathname,
  params,
  locale,
  page,
}: {
  tree: Root;
  children: ReactNode;
  pathname: string;
  params: AstroProviderProps['params'];
  locale: 'en' | 'zh';
  page?: DocsPageProps;
}) {
  // The layout calls the theme-switch slot with a className of its own: right
  // alignment inside the desktop footer's icon row, and no padding in the narrow
  // mobile drawer row. Merging it keeps both placements where the layout puts
  // its own switch. The element identity is memoized because the slot is a
  // component type: a fresh one on every render would remount the controls.
  const navControls = useMemo(
    () =>
      function NavControls({ className }: { className?: string }) {
        return (
          <div className={`flex items-center gap-1 ${className ?? ''}`}>
            <ThemeToggle locale={locale} />
            <span aria-hidden="true" className="mx-0.5 h-4 w-px bg-fd-border" />
            <LanguageSelect pathname={pathname} />
          </div>
        );
      },
    [locale, pathname],
  );

  return (
    <RootProvider
      pathname={pathname}
      params={params}
      navigate={navigate}
      theme={{ enabled: true, attribute: 'class', defaultTheme: 'system', enableSystem: true }}
      search={{ SearchDialog }}
    >
      <DocsLayout
        tree={tree}
        githubUrl="https://github.com/zhz8888/teldrive"
        // The layout's own control is a segmented light/dark/system switch, and
        // its slot is the only footer control the layout renders on both the
        // desktop sidebar and the mobile drawer. Claiming it puts the buttons
        // below where that switch sat, next to the icon links, instead of
        // crowding the title row at the top of the sidebar.
        slots={{ themeSwitch: navControls }}
        nav={{
          title: 'Teldrive',
          url: localizedPath(locale, import.meta.env.BASE_URL),
        }}
        sidebar={{
          defaultOpenLevel: 1,
        }}
      >
        <DocsPage {...page}>{children}</DocsPage>
      </DocsLayout>
    </RootProvider>
  );
}
