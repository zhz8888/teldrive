import { DocsLayout } from 'fumadocs-ui/layouts/docs';
import { DocsPage, type DocsPageProps } from 'fumadocs-ui/layouts/docs/page';
import type { Root } from 'fumadocs-core/page-tree';
import type { ReactNode } from 'react';
import { navigate } from 'astro:transitions/client';
import { RootProvider } from 'fumadocs-ui/provider/astro';
import type { AstroProviderProps } from 'fumadocs-core/framework/astro';
import SearchDialog from './search';
import { LocaleSwitcher } from './language-select';
import { localizedPath } from '../lib/i18n';

/**
 * Docs is the documentation shell for one rendered page: it supplies fumadocs'
 * root context (theme, Astro router bridge, search dialog), the docs layout
 * with the sidebar tree and GitHub link, and the locale switcher. The `tree`,
 * `pathname` and `params` all come from the route, so navigation between pages
 * stays client-side.
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
        themeSwitch={{ enabled: true }}
        nav={{
          title: 'Teldrive',
          url: localizedPath(locale, import.meta.env.BASE_URL),
          children: <LocaleSwitcher pathname={pathname} />,
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
