import {
  Button,
  cn,
  Drawer,
  ListBox,
  Popover,
  Slider,
  Spinner,
  Tabs,
  useOverlayState,
} from "@heroui/react";
import { useCallback, useEffect, useRef, useState } from "react";

import type { FileEntry } from "@/api/types";
import { startFileDownload } from "@/features/files/download";
import {
  applyPublicationAppearance,
  closePublication,
  openPublication,
  type ReaderPreferences,
} from "@/features/files/foliate-reader";
// `translate` resolves against the locale that is active when it is called. The
// publication loader below uses it because its effect must not re-run on a locale
// change: reloading the book would lose the reading position.
import { type MessageKey, t as translate, useI18n } from "@/lib/i18n";
import { useMediaQuery } from "@/lib/use-media-query";
import DownloadIcon from "~icons/gravity-ui/arrow-down-to-line";
import MenuIcon from "~icons/gravity-ui/bars";
import LeftIcon from "~icons/gravity-ui/chevron-left";
import RightIcon from "~icons/gravity-ui/chevron-right";
import CloseIcon from "~icons/gravity-ui/xmark";

/** Theme, font, layout and column choices offered by the reading settings. */
const THEME_OPTIONS: ReadonlyArray<readonly [MessageKey, string]> = [
  ["components.epubReader.themeWhite", "white"],
  ["components.epubReader.themePaper", "paper"],
  ["components.epubReader.themeGray", "gray"],
  ["components.epubReader.themeNight", "night"],
];
/** Value foliate receives: keep the publisher's fonts, or force serif or sans. */
const FONT_OPTIONS: ReadonlyArray<readonly [MessageKey, string]> = [
  ["components.epubReader.fontOriginal", "publisher"],
  ["components.epubReader.fontSerif", "serif"],
  ["components.epubReader.fontSans", "sans"],
];
/** Value foliate receives: paginated columns, or one scrolling column. */
const FLOW_OPTIONS: ReadonlyArray<readonly [MessageKey, string]> = [
  ["components.epubReader.layoutPages", "paginated"],
  ["components.epubReader.layoutScroll", "scrolled"],
];
/** Column counts offered for the paginated flow, as foliate's attribute strings. */
const COLUMN_OPTIONS: ReadonlyArray<readonly [MessageKey, string]> = [
  ["components.epubReader.columnsSingle", "1"],
  ["components.epubReader.columnsDouble", "2"],
];

/** Props of {@link EpubReader}; the parent mounts it only while it is shown. */
export type EpubReaderProps = {
  /** Entry shown in the header and in the navigation details tab. */
  file: FileEntry;
  /** Authenticated content URL the publication is fetched from. */
  url: string;
  /** Called after teardown finished, which is when the parent may unmount. */
  onClose: () => void;
};

/** One navigation row, flattened out of foliate's nested table of contents. */
type TocItem = {
  /** Key unique in the flattened list, built from the path to the entry. */
  id: string;
  /** Chapter title shown in the navigation list. */
  label: string;
  /** Publication-relative target handed to the view's `goTo`. */
  href: string;
  /** Nesting level used for the row indent; the top level is 0. */
  depth: number;
};

/**
 * Position foliate reports for the current reading spot: its synthetic
 * "locations" scheme, not page numbers and not a fraction of the book.
 */
type Location = { current?: number; total?: number };

/**
 * EPUB reader built on the foliate `foliate-view` custom element, which is
 * created imperatively because it is not a React component.
 *
 * One effect owns the publication for as long as the reader is mounted: it
 * fetches `url`, opens the book and destroys it again on unmount, including
 * when the reader is left by navigation rather than through the close button.
 * Everything the close path has to wait for (navigation, the in-flight open,
 * chapter fonts) is tracked in refs, because teardown must stay bounded: a
 * promise that never settles must not trap the reader open.
 */
export function EpubReader({ file, url, onClose }: EpubReaderProps) {
  const hostRef = useRef<HTMLDivElement>(null);
  // The custom element lives outside React, so it is reached through a ref.
  const viewRef = useRef<FoliateViewElement | undefined>(undefined);
  // False from the first close request on: every await in the loader re-checks
  // it before it touches state or the DOM.
  const activeRef = useRef(true);
  // Promises returned by foliate's navigation calls. Closing waits for them, so
  // a navigation that is still running is not cut off by the publication being
  // destroyed underneath it.
  const navigationTasksRef = useRef(new Set<Promise<unknown>>());
  // Latest appearance settings, for the loader effect, which must not re-run
  // when one of them changes.
  const preferencesRef = useRef<ReaderPreferences | undefined>(undefined);
  // Kept current so the teardown closure calls the latest handler without
  // listing it as an effect dependency.
  const onCloseRef = useRef(onClose);
  // The in-flight open, so a close can wait for a book that is still parsing.
  const openingRef = useRef<Promise<void> | undefined>(undefined);
  // Chapter documents foliate currently has mounted: key handlers are attached
  // to them and detached again when the next chapter replaces them.
  const loadedDocumentsRef = useRef(new Set<Document>());
  // Closing is idempotent; the first request wins and later ones are ignored.
  const closingRef = useRef(false);
  // Marks the publication as destroyed, so the unmount cleanup and the close
  // path cannot destroy the same book twice.
  const closedRef = useRef(false);
  onCloseRef.current = onClose;
  const isDesktop = useMediaQuery("(min-width: 1024px)");
  const drawerState = useOverlayState();
  const { t } = useI18n();
  const [settingsOpen, setSettingsOpen] = useState(false);

  // `closing` hides the loading overlay while the reader tears down; `title`
  // starts as the file name until the book's metadata supplies the real one.
  const [ready, setReady] = useState(false);
  const [error, setError] = useState<string>();
  const [closing, setClosing] = useState(false);
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [toc, setToc] = useState<TocItem[]>([]);
  const [title, setTitle] = useState(file.name);
  const [chapter, setChapter] = useState<string>();
  const [progress, setProgress] = useState(0);
  const [location, setLocation] = useState<Location>({});
  const [theme, setTheme] = useState("paper");
  const [flow, setFlow] = useState("paginated");
  const [font, setFont] = useState("publisher");
  const [fontSize, setFontSize] = useState(100);
  const [lineHeight, setLineHeight] = useState(1.55);
  const [margin, setMargin] = useState(48);
  const [columns, setColumns] = useState(2);

  // The appearance settings, mirrored into `preferencesRef` below. The loader
  // effect reads the ref so that changing a setting only re-styles the open
  // book: reloading it would lose the reading position.
  const preferences: ReaderPreferences = {
    theme,
    flow,
    font,
    fontSize,
    lineHeight,
    margin,
    columns,
  };
  preferencesRef.current = preferences;

  // A navigation that fails (a torn-down renderer, an href the book does not
  // have) must not surface as an unhandled rejection, and the close path needs
  // a settled promise, so the tracked promise swallows its rejection.
  const trackNavigation = useCallback((task: Promise<unknown>) => {
    const tracked = Promise.resolve(task).catch(() => undefined);
    navigationTasksRef.current.add(tracked);
    void tracked.finally(() => navigationTasksRef.current.delete(tracked));
  }, []);

  const navigate = useCallback(
    (direction: "previous" | "next") => {
      const view = viewRef.current;
      if (!ready || !view?.book) return;
      trackNavigation(direction === "previous" ? view.goLeft() : view.goRight());
    },
    [ready, trackNavigation],
  );

  const goTo = useCallback(
    (href: string) => {
      const view = viewRef.current;
      if (!ready || !view?.book) return;
      trackNavigation(view.goTo(href));
      drawerState.close();
    },
    [drawerState, ready, trackNavigation],
  );

  const toggleNavigation = () => {
    if (isDesktop) setSidebarOpen((value) => !value);
    else drawerState.open();
  };

  useEffect(() => {
    // Re-armed on every run: the cleanup of a previous run, and React's
    // double-invoked mount in development, both set it to false.
    activeRef.current = true;
    const host = hostRef.current;
    if (!host) return;

    let element: FoliateViewElement | undefined;
    const loadedDocuments = loadedDocumentsRef.current;

    const onReaderKeyDown = (event: KeyboardEvent) => {
      if (isEditableTarget(event.target)) return;
      if (event.key === "ArrowLeft" || event.key === "PageUp") {
        event.preventDefault();
        const view = viewRef.current;
        if (view?.book) trackNavigation(view.goLeft());
      } else if (event.key === "ArrowRight" || event.key === "PageDown" || event.key === " ") {
        event.preventDefault();
        const view = viewRef.current;
        if (view?.book) trackNavigation(view.goRight());
      }
    };

    const onLoad = (event: CustomEvent<{ doc: Document }>) => {
      if (!activeRef.current) return;
      const doc = event.detail.doc;
      // foliate keeps one chapter frame at a time: the next chapter replaces the
      // previous document, so dropping the reference here is what stops every
      // visited chapter (and its blob URL) from staying alive with the reader.
      for (const previous of loadedDocuments) {
        if (previous === doc) continue;
        previous.removeEventListener("keydown", onReaderKeyDown);
        loadedDocuments.delete(previous);
      }
      loadedDocuments.add(doc);
      // A chapter is a separate document, and a key pressed inside its frame
      // never reaches the reader's own window listener, so each chapter
      // document gets its own handler.
      doc.addEventListener("keydown", onReaderKeyDown);
      const updateRenderedContent = () => {
        const text = doc.body?.innerText.trim();
        const imageLabel = doc.querySelector("img")?.getAttribute("alt");
        if (text || imageLabel) element!.dataset.renderedContent = text || imageLabel || "";
        return Boolean(text || imageLabel);
      };
      if (!updateRenderedContent() && doc.body) {
        const observer = new MutationObserver(() => {
          if (!activeRef.current || updateRenderedContent()) observer.disconnect();
        });
        observer.observe(doc.body, { childList: true, subtree: true });
      }
    };

    const onRelocate = (event: CustomEvent<FoliateRelocateDetail>) => {
      if (!activeRef.current) return;
      const fraction = event.detail.fraction || 0;
      setProgress(fraction);
      setChapter(event.detail.tocItem?.label);
      setLocation(event.detail.location || {});
    };

    const open = async () => {
      setReady(false);
      setError(undefined);
      // Dynamic import: the foliate engine is only pulled in once a book is
      // actually opened.
      await import("foliate-js/view.js");
      if (!activeRef.current) return;

      element = document.createElement("foliate-view");
      // The custom element brings no styles of its own, so the host gives it
      // the full size of the reading pane.
      element.className = "block h-full min-h-0 w-full";
      host.replaceChildren(element);
      viewRef.current = element;

      // The content URL is authenticated by the session cookie, and the body is
      // wrapped in a File carrying the entry's name and MIME type so foliate can
      // detect the publication format.
      const response = await fetch(url);
      if (!response.ok) throw new Error(`Unable to load publication (${response.status}).`);
      const bookFile = new File([await response.blob()], file.name, { type: file.mimeType });
      if (!activeRef.current) return;

      await openPublication({
        element,
        file: bookFile,
        preferences: preferencesRef.current!,
        onLoad,
        onRelocate,
      });
      if (!activeRef.current) return;

      const metadata = element.book?.metadata || {};
      const metadataTitle = typeof metadata.title === "string" ? metadata.title.trim() : "";
      if (metadataTitle) setTitle(metadataTitle);
      setToc(flattenToc(element.book?.toc || []));
      setReady(true);
    };

    // Assigned synchronously, so a close that arrives while the book is still
    // loading always has a promise to wait for.
    openingRef.current = open().catch((reason: unknown) => {
      if (activeRef.current) {
        setError(
          reason instanceof Error
            ? reason.message
            : translate("components.epubReader.openFailedFallback"),
        );
      }
    });

    return () => {
      activeRef.current = false;
      const current = element;
      for (const doc of loadedDocuments) doc.removeEventListener("keydown", onReaderKeyDown);
      loadedDocuments.clear();
      current?.removeEventListener("load", onLoad as EventListener);
      current?.removeEventListener("relocate", onRelocate as EventListener);
      if (closedRef.current) {
        current?.remove();
        return;
      }
      // Leaving the reader by navigation tears it down without the interactive
      // close, so the publication is destroyed here too: foliate would otherwise
      // keep the book and its per-chapter blob URLs alive past the last render.
      closedRef.current = true;
      viewRef.current = undefined;
      if (current) closePublication(current);
    };
  }, [file.id, file.mimeType, file.name, trackNavigation, url]);

  /**
   * Starts the teardown and calls `onClose` once it has settled: pending
   * navigation and the in-flight open are awaited, chapter fonts are given a
   * chance to finish, two frames are allowed to paint, and only then is the
   * publication destroyed. Every wait is bounded by {@link settleSoon}, so a
   * promise that never settles still closes the reader.
   */
  const requestClose = useCallback(() => {
    if (closingRef.current) return;
    closingRef.current = true;
    activeRef.current = false;
    setClosing(true);
    setReady(false);
    setSettingsOpen(false);
    drawerState.close();

    const finishClose = async () => {
      try {
        const pending = [...navigationTasksRef.current];
        if (openingRef.current) pending.push(openingRef.current);
        await settleSoon(pending);

        const fontLoads = [...loadedDocumentsRef.current].flatMap((doc) =>
          doc.fonts?.ready ? [doc.fonts.ready.catch(() => undefined)] : [],
        );
        // Bounded like the navigation wait: the frames below are what actually
        // lets the last layout settle before the documents go away.
        await settleSoon(fontLoads);
        await nextAnimationFrame();
        await nextAnimationFrame();

        const current = viewRef.current;
        if (current && !closedRef.current) {
          try {
            await closePublication(current);
          } catch {
            // Best effort: a reader teardown failure must never trap the dialog open.
          } finally {
            closedRef.current = true;
            viewRef.current = undefined;
          }
        }

        await nextAnimationFrame();
      } finally {
        onCloseRef.current();
      }
    };

    void finishClose();
  }, [drawerState]);

  // Appearance changes re-style the live publication instead of reloading it;
  // the loader effect never re-runs for a setting.
  useEffect(() => {
    const view = viewRef.current;
    if (view) applyPublicationAppearance(view, preferences);
  }, [columns, flow, font, fontSize, lineHeight, margin, theme]);

  // Keys pressed while the focus is on the reader chrome, or on the page body,
  // are handled here; keys pressed inside a chapter frame are handled by the
  // per-document listener the loader attaches.
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (isEditableTarget(event.target)) return;
      if (event.key === "ArrowLeft" || event.key === "PageUp") navigate("previous");
      else if (event.key === "ArrowRight" || event.key === "PageDown") navigate("next");
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [navigate]);

  // Escape is handled on capture: the reader modal is not keyboard-dismissable,
  // so a bubble listener would never fire. Nested settings/drawer close first,
  // and portalled overlays (role=dialog outside the reader root) own Escape.
  useEffect(() => {
    const onEscape = (event: KeyboardEvent) => {
      if (event.key !== "Escape" || isEditableTarget(event.target)) return;
      const overlay =
        event.target instanceof HTMLElement ? event.target.closest('[role="dialog"]') : null;
      if (overlay && !overlay.querySelector("[data-epub-reader]")) return;
      event.preventDefault();
      event.stopPropagation();
      if (settingsOpen) setSettingsOpen(false);
      else if (drawerState.isOpen) drawerState.close();
      else requestClose();
    };
    window.addEventListener("keydown", onEscape, true);
    return () => window.removeEventListener("keydown", onEscape, true);
  }, [drawerState, requestClose, settingsOpen]);

  const navigation = (
    <EpubNavigation file={file} toc={toc} activeChapter={chapter} onNavigate={goTo} />
  );

  return (
    <div
      data-epub-reader
      data-reader-theme={theme}
      className="reader-shell flex h-dvh min-h-0 flex-col overflow-hidden"
    >
      <header
        data-epub-header
        className="reader-chrome z-20 flex h-14 shrink-0 items-center gap-2 border-b px-2 sm:h-16 sm:px-3"
      >
        <Button
          isIconOnly
          size="sm"
          variant="ghost"
          aria-label={t("components.epubReader.openNavigation")}
          onPress={toggleNavigation}
        >
          <MenuIcon className="size-5" />
        </Button>

        <div className="min-w-0 flex-1 px-1 sm:px-2">
          <p className="truncate text-sm font-semibold tracking-[-0.01em]">{title}</p>
          <p className="truncate text-[11px] text-(--reader-muted)">
            {chapter || t("components.epubReader.subtitle")}
          </p>
        </div>

        <div className="hidden min-w-28 text-center text-[11px] text-(--reader-muted) md:block">
          {locationLabel(location, progress)}
        </div>

        <EpubSettings
          isOpen={settingsOpen}
          onOpenChange={setSettingsOpen}
          theme={theme}
          flow={flow}
          font={font}
          fontSize={fontSize}
          lineHeight={lineHeight}
          margin={margin}
          columns={columns}
          onTheme={setTheme}
          onFlow={setFlow}
          onFont={setFont}
          onFontSize={setFontSize}
          onLineHeight={setLineHeight}
          onMargin={setMargin}
          onColumns={setColumns}
        />

        <Button
          isIconOnly
          size="sm"
          variant="ghost"
          aria-label={t("components.epubReader.download")}
          className="hidden sm:inline-flex"
          onPress={() => startFileDownload(file)}
        >
          <DownloadIcon className="size-4" />
        </Button>
        <Button
          isIconOnly
          size="sm"
          variant="ghost"
          aria-label={t("components.epubReader.close")}
          onPress={requestClose}
        >
          <CloseIcon className="size-5" />
        </Button>
      </header>

      <div className="flex min-h-0 flex-1">
        {isDesktop && sidebarOpen ? (
          <aside
            data-epub-sidebar
            className="reader-chrome hidden w-72 shrink-0 border-r lg:block xl:w-80"
          >
            {navigation}
          </aside>
        ) : null}

        <main
          data-epub-canvas
          className="relative min-w-0 flex-1 overflow-hidden bg-(--reader-canvas)"
        >
          {!ready && !error && !closing ? (
            <div className="absolute inset-0 z-10 grid place-items-center bg-(--reader-canvas)/90">
              <div className="text-center">
                <Spinner size="lg" aria-label={t("components.epubReader.loading")} />
                <p className="mt-3 text-xs text-(--reader-muted)">
                  {t("components.epubReader.opening")}
                </p>
              </div>
            </div>
          ) : null}
          {error ? (
            <div className="absolute inset-0 z-10 grid place-items-center p-6 text-center">
              <div className="max-w-lg">
                <p className="font-semibold">{t("components.epubReader.openFailed")}</p>
                <p className="mt-2 text-sm text-(--reader-muted)">{error}</p>
              </div>
            </div>
          ) : null}
          <div className="mx-auto h-full min-h-0 w-full max-w-[1680px] px-0 sm:px-2 lg:px-4">
            <div ref={hostRef} className="reader-page h-full min-h-0 w-full overflow-hidden" />
          </div>
        </main>
      </div>

      <footer
        data-epub-footer
        className="reader-chrome z-20 grid h-12 shrink-0 grid-cols-[1fr_auto_1fr] items-center border-t px-2 sm:px-4"
      >
        <Button
          size="sm"
          variant="ghost"
          aria-label={t("components.epubReader.previousAria")}
          className="justify-self-start"
          isDisabled={!ready}
          onPress={() => navigate("previous")}
        >
          <LeftIcon className="size-4" />
          <span className="hidden sm:inline">{t("components.epubReader.previous")}</span>
        </Button>
        <div className="max-w-[52vw] text-center text-[10px] tabular-nums text-(--reader-muted) sm:text-[11px]">
          <p className="truncate">
            {chapter ? `${chapter} · ` : ""}
            {locationLabel(location, progress)}
          </p>
        </div>
        <Button
          size="sm"
          variant="ghost"
          aria-label={t("components.epubReader.nextAria")}
          className="justify-self-end"
          isDisabled={!ready}
          onPress={() => navigate("next")}
        >
          <span className="hidden sm:inline">{t("components.epubReader.next")}</span>
          <RightIcon className="size-4" />
        </Button>
      </footer>

      {!isDesktop ? (
        <Drawer state={drawerState}>
          <Drawer.Backdrop variant="blur">
            <Drawer.Content placement="left" className="w-[min(88vw,22rem)]">
              <Drawer.Dialog data-reader-theme={theme} className="reader-chrome">
                <Drawer.Header className="border-b border-(--reader-border)">
                  <Drawer.Heading>{t("components.epubReader.navigation")}</Drawer.Heading>
                  <Drawer.CloseTrigger />
                </Drawer.Header>
                <Drawer.Body className="p-0">{navigation}</Drawer.Body>
              </Drawer.Dialog>
            </Drawer.Content>
          </Drawer.Backdrop>
        </Drawer>
      ) : null}
    </div>
  );
}

/**
 * Navigation panel shown in the desktop sidebar or the mobile drawer: the file
 * name over a contents / details tab pair. `activeChapter` highlights the entry
 * the reading position currently falls under.
 */
function EpubNavigation({
  file,
  toc,
  activeChapter,
  onNavigate,
}: {
  file: FileEntry;
  toc: TocItem[];
  activeChapter?: string;
  onNavigate: (href: string) => void;
}) {
  const { t } = useI18n();
  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="border-b border-(--reader-border) px-5 py-4">
        <p className="text-[10px] font-semibold uppercase tracking-[0.18em] text-(--reader-muted)">
          {t("components.epubReader.library")}
        </p>
        <p className="mt-1 truncate text-sm font-semibold">{file.name}</p>
      </div>
      <Tabs defaultSelectedKey="contents" className="flex min-h-0 flex-1 flex-col px-3 pt-3">
        <Tabs.ListContainer>
          <Tabs.List aria-label={t("components.epubReader.tabsAria")} className="w-full">
            <Tabs.Tab id="contents" className="flex-1">
              {t("components.epubReader.tabContents")}
            </Tabs.Tab>
            <Tabs.Tab id="details" className="flex-1">
              {t("components.epubReader.tabDetails")}
            </Tabs.Tab>
          </Tabs.List>
        </Tabs.ListContainer>
        <Tabs.Panel id="contents" className="min-h-0 flex-1 overflow-y-auto py-3">
          {toc.length ? (
            <ListBox
              aria-label={t("components.epubReader.tocAria")}
              selectionMode="none"
              className="w-full gap-1 p-0"
              onAction={(key) => {
                const item = toc.find((candidate) => candidate.id === String(key));
                if (item) onNavigate(item.href);
              }}
            >
              {toc.map((item) => (
                <ListBox.Item
                  key={item.id}
                  id={item.id}
                  textValue={item.label}
                  className={cn(
                    "text-sm",
                    activeChapter === item.label && "bg-default/60 font-medium",
                  )}
                  style={{ paddingInlineStart: `${12 + item.depth * 18}px` }}
                >
                  {item.label}
                </ListBox.Item>
              ))}
            </ListBox>
          ) : (
            <p className="px-2 py-4 text-xs text-(--reader-muted)">
              {t("components.epubReader.noToc")}
            </p>
          )}
        </Tabs.Panel>
        <Tabs.Panel id="details" className="space-y-4 overflow-y-auto px-2 py-5 text-sm">
          <Detail label={t("components.epubReader.detailTitle")} value={file.name} />
          <Detail label={t("components.epubReader.detailFormat")} value="EPUB" />
          <Detail
            label={t("components.epubReader.detailFileSize")}
            value={formatBytes(file.size || 0)}
          />
        </Tabs.Panel>
      </Tabs>
    </div>
  );
}

/** Label and value pair of the navigation details tab. */
function Detail({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <p className="text-[10px] uppercase tracking-wider text-(--reader-muted)">{label}</p>
      <p className="mt-1 wrap-break-word font-medium">{value}</p>
    </div>
  );
}

/**
 * Appearance popover behind the "Aa" button: theme, font, text size, line
 * spacing, margins, layout and column count. Every change is applied to the
 * live publication by the caller, so the reader keeps its position.
 */
function EpubSettings({
  isOpen,
  onOpenChange,
  theme,
  flow,
  font,
  fontSize,
  lineHeight,
  margin,
  columns,
  onTheme,
  onFlow,
  onFont,
  onFontSize,
  onLineHeight,
  onMargin,
  onColumns,
}: ReaderPreferences & {
  isOpen: boolean;
  onOpenChange: (open: boolean) => void;
  onTheme: (value: string) => void;
  onFlow: (value: string) => void;
  onFont: (value: string) => void;
  onFontSize: (value: number) => void;
  onLineHeight: (value: number) => void;
  onMargin: (value: number) => void;
  onColumns: (value: number) => void;
}) {
  const { t } = useI18n();
  return (
    <Popover isOpen={isOpen} onOpenChange={onOpenChange}>
      <Button
        size="sm"
        variant="ghost"
        aria-label={t("components.epubReader.settingsAria")}
        className="min-w-9 px-2 font-serif text-base"
      >
        Aa
      </Button>
      <Popover.Content placement="bottom end" offset={8} className="w-[min(92vw,22rem)]">
        <Popover.Dialog className="p-0">
          <div className="flex items-start justify-between gap-3 border-b border-border px-4 py-3">
            <div>
              <Popover.Heading className="text-sm font-semibold">
                {t("components.epubReader.appearance")}
              </Popover.Heading>
              <p className="mt-0.5 text-xs text-muted">
                {t("components.epubReader.appearanceDescription")}
              </p>
            </div>
            <Button size="sm" variant="ghost" onPress={() => onOpenChange(false)}>
              {t("common.action.done")}
            </Button>
          </div>
          <div className="max-h-[min(72vh,36rem)] space-y-5 overflow-y-auto p-4">
            <SettingButtons
              label="components.epubReader.theme"
              value={theme}
              options={THEME_OPTIONS}
              onChange={onTheme}
            />
            <SettingButtons
              label="components.epubReader.font"
              value={font}
              options={FONT_OPTIONS}
              onChange={onFont}
            />
            <SettingSlider
              label="components.epubReader.textSize"
              value={fontSize}
              min={80}
              max={180}
              step={5}
              output={`${fontSize}%`}
              onChange={onFontSize}
            />
            <SettingSlider
              label="components.epubReader.lineSpacing"
              value={lineHeight}
              min={1.2}
              max={2}
              step={0.05}
              output={lineHeight.toFixed(2)}
              onChange={onLineHeight}
            />
            <SettingSlider
              label="components.epubReader.pageMargins"
              value={margin}
              min={16}
              max={96}
              step={4}
              output={`${margin}px`}
              onChange={onMargin}
            />
            <SettingButtons
              label="components.epubReader.layout"
              value={flow}
              options={FLOW_OPTIONS}
              onChange={onFlow}
            />
            <SettingButtons
              label="components.epubReader.columns"
              value={String(columns)}
              options={COLUMN_OPTIONS}
              onChange={(value) => onColumns(Number(value))}
            />
          </div>
        </Popover.Dialog>
      </Popover.Content>
    </Popover>
  );
}

/**
 * Setting rendered as a row of mutually exclusive buttons; `options` pairs each
 * catalog label with the value foliate receives.
 */
function SettingButtons({
  label,
  value,
  options,
  onChange,
}: {
  label: MessageKey;
  value: string;
  options: ReadonlyArray<readonly [MessageKey, string]>;
  onChange: (value: string) => void;
}) {
  const { t } = useI18n();
  return (
    <div>
      <p className="mb-2 text-xs font-medium">{t(label)}</p>
      <div className="grid grid-cols-2 gap-1.5">
        {options.map(([labelKey, option]) => (
          <Button
            key={option}
            size="sm"
            variant={value === option ? "primary" : "secondary"}
            onPress={() => onChange(option)}
          >
            {t(labelKey)}
          </Button>
        ))}
      </div>
    </div>
  );
}

/**
 * Setting rendered as a slider, with the current value shown as `output` at the
 * end of the label row.
 */
function SettingSlider({
  label,
  value,
  min,
  max,
  step,
  output,
  onChange,
}: {
  label: MessageKey;
  value: number;
  min: number;
  max: number;
  step: number;
  output: string;
  onChange: (value: number) => void;
}) {
  const { t } = useI18n();
  return (
    <Slider
      aria-label={t(label)}
      value={value}
      minValue={min}
      maxValue={max}
      step={step}
      onChange={(next) => onChange(Number(next))}
    >
      <div className="mb-2 flex justify-between text-xs">
        <span>{t(label)}</span>
        <span className="tabular-nums text-muted">{output}</span>
      </div>
      <Slider.Track>
        <Slider.Fill />
        <Slider.Thumb />
      </Slider.Track>
    </Slider>
  );
}

/**
 * Flattens foliate's nested table of contents into the list the panel renders.
 * Each id repeats the path to the entry, because labels and hrefs alone are not
 * unique across a book.
 */
function flattenToc(
  items: Array<{ label: string; href: string; subitems?: unknown[] }>,
  depth = 0,
  parent = "root",
): TocItem[] {
  return items.flatMap((item) => {
    const id = `${parent}:${item.href}:${item.label}`;
    return [
      { id, label: item.label, href: item.href, depth },
      ...flattenToc(
        (item.subitems || []) as Array<{ label: string; href: string; subitems?: unknown[] }>,
        depth + 1,
        id,
      ),
    ];
  });
}

/**
 * Position shown in the header and footer: foliate's location counter when it
 * has one, otherwise the percentage of the book read.
 */
function locationLabel(location: Location, progress: number) {
  if (location.current !== undefined && location.total)
    return translate("components.epubReader.pageLocation", {
      current: location.current + 1,
      total: location.total,
    });
  return `${Math.round(progress * 100)}%`;
}

/** Formats a byte count in binary units; a falsy value renders as "0 B". */
function formatBytes(value: number) {
  if (!value) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const index = Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1);
  return `${(value / 1024 ** index).toFixed(index === 0 ? 0 : 1)} ${units[index]}`;
}

/** Whether a key event came from a field that keeps its own navigation keys. */
function isEditableTarget(target: EventTarget | null) {
  return (
    target instanceof HTMLInputElement ||
    target instanceof HTMLTextAreaElement ||
    target instanceof HTMLSelectElement ||
    (target instanceof HTMLElement && target.isContentEditable)
  );
}

/** Resolves on the next animation frame, used to let a paint finish mid-teardown. */
function nextAnimationFrame() {
  return new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
}

// Closing must be bounded: an unsettled navigation or font promise must never
// trap the reader open after the user asked to leave.
function settleSoon(tasks: Promise<unknown>[], ms = 1500) {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const timeout = new Promise<void>((resolve) => {
    timer = setTimeout(resolve, ms);
  });
  return Promise.race([Promise.allSettled(tasks).then(() => undefined), timeout]).finally(() =>
    clearTimeout(timer),
  );
}
