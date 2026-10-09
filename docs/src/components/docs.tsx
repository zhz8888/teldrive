import { DocsLayout } from 'fumadocs-ui/layouts/docs';
import { DocsPage, type DocsPageProps } from 'fumadocs-ui/layouts/docs/page';
import type { Root } from 'fumadocs-core/page-tree';
import type { ReactNode } from 'react';
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
        // The layout's own control is a segmented light/dark/system switch; the
        // pair below replaces it with one theme button and one language button,
        // side by side and in the same place on every viewport.
        themeSwitch={{ enabled: false }}
        nav={{
          title: 'Teldrive',
          url: localizedPath(locale, import.meta.env.BASE_URL),
          children: (
            <div className="flex shrink-0 items-center justify-end gap-1">
              <ThemeToggle locale={locale} />
              <span aria-hidden="true" className="mx-0.5 h-4 w-px bg-fd-border" />
              <LanguageSelect pathname={pathname} />
            </div>
          ),
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
