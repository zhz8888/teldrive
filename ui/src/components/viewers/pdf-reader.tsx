import {
  AnnotationEditorParamsType,
  AnnotationEditorType,
  AnnotationMode,
  GlobalWorkerOptions,
  getDocument,
  PasswordResponses,
  type PDFDocumentProxy,
} from "pdfjs-dist";
import workerSrc from "pdfjs-dist/build/pdf.worker.min.mjs?url";
import {
  EventBus,
  FindState,
  PDFFindController,
  PDFLinkService,
  PDFViewer,
} from "pdfjs-dist/web/pdf_viewer.mjs";
import "pdfjs-dist/web/pdf_viewer.css";

import {
  Button,
  cn,
  Drawer,
  InputGroup,
  Popover,
  Spinner,
  Tabs,
  useOverlayState,
} from "@heroui/react";
import {
  type KeyboardEvent as ReactKeyboardEvent,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { toast } from "sonner";

import type { FileEntry } from "@/api/types";
// `translate` resolves against the locale that is active when it is called. The
// document loader below uses it because its effect must not re-run on a locale
// change: reloading the file would lose the reader position.
import { type MessageKey, t as translate, useI18n } from "@/lib/i18n";
import { useMediaQuery } from "@/lib/use-media-query";
import DownloadIcon from "~icons/gravity-ui/arrow-down-to-line";
import RotateIcon from "~icons/gravity-ui/arrow-rotate-right";
import MenuIcon from "~icons/gravity-ui/bars";
import BookmarkIcon from "~icons/gravity-ui/bookmark";
import BrushIcon from "~icons/gravity-ui/brush";
import LeftIcon from "~icons/gravity-ui/chevron-left";
import RightIcon from "~icons/gravity-ui/chevron-right";
import EllipsisIcon from "~icons/gravity-ui/ellipsis";
import SaveIcon from "~icons/gravity-ui/floppy-disk";
import HandIcon from "~icons/gravity-ui/hand";
import SearchIcon from "~icons/gravity-ui/magnifier";
import ZoomOutIcon from "~icons/gravity-ui/magnifier-minus";
import ZoomInIcon from "~icons/gravity-ui/magnifier-plus";
import PencilIcon from "~icons/gravity-ui/pencil";
import TextIcon from "~icons/gravity-ui/text";
import CloseIcon from "~icons/gravity-ui/xmark";

// pdf.js loads its worker from a URL rather than importing it into the bundle,
// so the `?url` import is what makes the bundler emit it as a separate asset.
GlobalWorkerOptions.workerSrc = workerSrc;

/** Props of {@link PdfReader}; the parent mounts it only while it is shown. */
type PdfReaderProps = {
  /** Entry shown in the toolbar and sidebar header. */
  file: FileEntry;
  /** Authenticated content URL the document is fetched from. */
  url: string;
  /** Called to close the reader, from the toolbar and from Escape. */
  onClose: () => void;
};

/** Which of the two sidebar panels is showing. */
type SidebarTab = "thumbnails" | "outline";
/** Annotation tool the toolbar has selected; `select` keeps the text layer live. */
type AnnotationTool = "select" | "highlight" | "text" | "ink";
/** One outline row as pdf.js reports it, nested through `items`. */
type OutlineItem = {
  /** Section title; empty for the sections that have none. */
  title: string;
  /** Named or explicit destination inside the document. */
  dest: string | unknown[] | null;
  /** External address, when the section points outside the document. */
  url?: string | null;
  /** Nested subsections. */
  items?: OutlineItem[];
};

/**
 * The pdf.js objects that must stay alive together for one loaded document.
 * They are created once per file and reached through a ref, because the toolbar
 * acts on them directly instead of through React state.
 */
type PdfRuntime = {
  /** Carries the viewer's events, including the find and annotation dispatches. */
  eventBus: EventBus;
  /** Resolves outline destinations and internal links to pages. */
  linkService: PDFLinkService;
  /** Runs the find controller behind the search bar. */
  findController: PDFFindController;
  /** The viewer that renders pages into the container. */
  viewer: PDFViewer;
  /** The loaded document itself, needed for page counts and saving. */
  document: PDFDocumentProxy;
};

/** Search progress shown next to the query: match number and match count. */
type FindCount = { current: number; total: number };
/** Password prompt state while pdf.js waits for a protected document's password. */
type PasswordChallenge = {
  /** True when pdf.js rejected the previous attempt. */
  incorrect: boolean;
  /** Hands the entered password back to the pending loading task. */
  submit: (password: string) => void;
};

/** Annotation colours offered by the colour picker, in display order. */
const HIGHLIGHT_COLORS = ["#facc15", "#4ade80", "#60a5fa", "#f472b6"] as const;

/** Zoom presets offered by the zoom menu, in display order. */
const SCALE_PRESETS: ReadonlyArray<{ labelKey: MessageKey; value: string }> = [
  { labelKey: "components.pdfReader.fitWidth", value: "page-width" },
  { labelKey: "components.pdfReader.fitPage", value: "page-fit" },
  { labelKey: "components.pdfReader.actualSize", value: "page-actual" },
];
// Same palette as HIGHLIGHT_COLORS, in the `name=#hex` form pdf.js expects for
// `annotationEditorHighlightColors`.
const PDFJS_HIGHLIGHT_COLORS = "yellow=#facc15,green=#4ade80,blue=#60a5fa,pink=#f472b6";

/**
 * Full-screen PDF reader: pdf.js viewer, sidebar with page thumbnails and the
 * outline, find bar, annotation tools, zoom and rotation, and saving a copy
 * with the annotations baked in.
 *
 * The pdf.js viewer is imperative, so one effect builds it for the current
 * `url`, publishes it through `runtimeRef` for the toolbar, and tears it down
 * again on unmount: the loading task is destroyed and the viewer cleaned up so
 * the document and its worker resources do not outlive the reader.
 */
export function PdfReader({ file, url, onClose }: PdfReaderProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const viewerRef = useRef<HTMLDivElement>(null);
  // The pdf.js objects of the loaded document; null until one is ready.
  const runtimeRef = useRef<PdfRuntime | null>(null);
  // Kept current so the key handler can close the reader without re-registering
  // every time the parent renders.
  const closeRef = useRef(onClose);
  const drawerState = useOverlayState();
  // The sidebar is an aside on a wide viewport and a drawer on a narrow one. Only
  // the arrangement the viewport uses is mounted: rendering both put the whole
  // thumbnail list in the tree twice on a phone, and the sidebar draws a tile for
  // every page of the document.
  const isDesktop = useMediaQuery("(min-width: 1024px)");
  const { t } = useI18n();

  // Starting view state. The loader re-applies it to the viewer once
  // `pagesinit` fires, because pdf.js starts every new document on its own
  // defaults rather than on the state the previous one left behind.
  const initialPage = 1;
  const initialScaleValue = "page-width";
  const initialRotation = 0;
  const initialSidebarOpen = true;
  const initialSidebarTab: SidebarTab = "thumbnails";

  const [document, setDocument] = useState<PDFDocumentProxy>();
  const [outline, setOutline] = useState<OutlineItem[]>([]);
  const [ready, setReady] = useState(false);
  const [error, setError] = useState<string>();
  // Download percentage while the document loads; undefined until pdf.js
  // reports a total size it can measure against.
  const [loadingProgress, setLoadingProgress] = useState<number>();
  const [pageNumber, setPageNumber] = useState(initialPage);
  // Text of the page box. It is committed on blur or Enter only, so typing a
  // number does not jump the document on the first digit.
  const [pageDraft, setPageDraft] = useState(String(initialPage));
  const [numPages, setNumPages] = useState(0);
  const [scale, setScale] = useState(1);
  // Named zoom preset ("page-width", "page-fit", "page-actual") or "custom"
  // once the viewer reports a numeric scale; `scale` is what is shown as a
  // percentage.
  const [scaleValue, setScaleValue] = useState(initialScaleValue);
  // Rotation is owned by the viewer: only the setter is used, to mirror its
  // `rotationchanging` event, and the stored value is never read.
  const [_rotation, setRotation] = useState(initialRotation);
  const [sidebarOpen, setSidebarOpen] = useState(initialSidebarOpen);
  const [sidebarTab, setSidebarTab] = useState<SidebarTab>(initialSidebarTab);
  const [searchOpen, setSearchOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [caseSensitive, setCaseSensitive] = useState(false);
  const [wholeWord, setWholeWord] = useState(false);
  const [findCount, setFindCount] = useState<FindCount>({ current: 0, total: 0 });
  // pdf.js FindState of the last search, which is what tells "no matches" apart
  // from "nothing searched yet".
  const [findState, setFindState] = useState<number>(FindState.FOUND);
  const [annotationTool, setAnnotationTool] = useState<AnnotationTool>("select");
  // Mirrors `annotationTool` for the Escape handler, whose effect does not
  // re-run when the tool changes.
  const annotationToolRef = useRef<AnnotationTool>("select");
  // The state setter is the plain one; `setAnnotationColor` below sets the
  // colour and also tells pdf.js about it.
  const [annotationColor, setAnnotationColorState] = useState<(typeof HIGHLIGHT_COLORS)[number]>(
    HIGHLIGHT_COLORS[0],
  );
  const [saving, setSaving] = useState(false);
  // Set while pdf.js waits for a password: `submit` is what resumes the pending
  // loading task, and `incorrect` marks a rejected attempt.
  const [passwordChallenge, setPasswordChallenge] = useState<PasswordChallenge>();
  const [passwordDraft, setPasswordDraft] = useState("");

  closeRef.current = onClose;

  // One effect owns the pdf.js viewer for the current `url`. The event handlers
  // below only mirror viewer events into React state; the viewer is the source
  // of truth for the page, scale and rotation.
  useEffect(() => {
    const container = containerRef.current;
    const viewerElement = viewerRef.current;
    if (!container || !viewerElement) return;

    let active = true;
    let loadingTask: ReturnType<typeof getDocument> | undefined;
    const eventBus = new EventBus();
    const linkService = new PDFLinkService({ eventBus });
    const findController = new PDFFindController({ eventBus, linkService });
    const viewer = new PDFViewer({
      container,
      viewer: viewerElement,
      eventBus,
      linkService,
      findController,
      textLayerMode: 1,
      annotationMode: AnnotationMode.ENABLE_FORMS,
      annotationEditorMode: AnnotationEditorType.NONE,
      annotationEditorHighlightColors: PDFJS_HIGHLIGHT_COLORS,
      removePageBorders: true,
    });
    linkService.setViewer(viewer);

    const onPageChanging = (event: { pageNumber?: number }) => {
      const next = positiveInt(event.pageNumber, viewer.currentPageNumber || 1);
      setPageNumber(next);
      setPageDraft(String(next));
    };
    const onScaleChanging = (event: { scale?: number; presetValue?: string }) => {
      setScale(positiveNumber(event.scale, viewer.currentScale || 1));
      setScaleValue(event.presetValue || viewer.currentScaleValue || "custom");
    };
    const onRotationChanging = (event: { pagesRotation?: number }) => {
      setRotation(normalizedRotation(event.pagesRotation));
    };
    const onFindCount = (event: { matchesCount?: FindCount }) =>
      setFindCount(event.matchesCount || { current: 0, total: 0 });
    const onFindState = (event: { state?: number; matchesCount?: FindCount }) => {
      if (typeof event.state === "number") setFindState(event.state);
      if (event.matchesCount) setFindCount(event.matchesCount);
    };

    eventBus.on("pagechanging", onPageChanging);
    eventBus.on("scalechanging", onScaleChanging);
    eventBus.on("rotationchanging", onRotationChanging);
    eventBus.on("updatefindmatchescount", onFindCount);
    eventBus.on("updatefindcontrolstate", onFindState);

    const open = async () => {
      setReady(false);
      setError(undefined);
      setLoadingProgress(undefined);
      setPasswordChallenge(undefined);
      setPasswordDraft("");
      // The content URL is authenticated by the session cookie, and the asset
      // URLs point at the `pdfjs/{cmaps,standard_fonts,wasm,iccs}` tree the Vite
      // build emits: pdf.js fetches cmaps, standard fonts, the wasm decoders and
      // ICC profiles from there instead of bundling them.
      loadingTask = getDocument({
        url,
        withCredentials: true,
        cMapUrl: "/pdfjs/cmaps/",
        standardFontDataUrl: "/pdfjs/standard_fonts/",
        wasmUrl: "/pdfjs/wasm/",
        iccUrl: "/pdfjs/iccs/",
      });
      // pdf.js asks for a password instead of rejecting a protected document;
      // `updatePassword` is the only way to let the pending promise continue.
      loadingTask.onPassword = (updatePassword: (password: string) => void, reason: number) => {
        if (!active) return;
        setPasswordDraft("");
        setPasswordChallenge({
          incorrect: reason === PasswordResponses.INCORRECT_PASSWORD,
          submit: updatePassword,
        });
      };
      loadingTask.onProgress = (progress: { loaded: number; total?: number }) => {
        if (!active || !progress.total) return;
        setLoadingProgress(Math.min(100, Math.round((progress.loaded / progress.total) * 100)));
      };
      const pdf = await loadingTask.promise;
      if (!active) return;

      runtimeRef.current = { eventBus, linkService, findController, viewer, document: pdf };
      setDocument(pdf);
      setNumPages(pdf.numPages);
      linkService.setDocument(pdf);
      findController.setDocument(pdf);

      const loadedOutline = await pdf.getOutline();
      if (active) setOutline((loadedOutline || []) as OutlineItem[]);

      // The listener is attached before `setDocument`, because `pagesinit` fires
      // during that call and a listener attached afterwards would miss it.
      const pagesInitialized = new Promise<void>((resolve) => {
        eventBus.on("pagesinit", () => resolve(), { once: true });
      });
      viewer.setDocument(pdf);
      await pagesInitialized;
      if (!active) return;

      // Re-apply the starting view: pdf.js resets page, scale and rotation for
      // every document.
      viewer.pagesRotation = initialRotation;
      viewer.currentScaleValue = initialScaleValue;
      viewer.currentPageNumber = Math.min(Math.max(initialPage, 1), pdf.numPages);
      setPageNumber(viewer.currentPageNumber);
      setPageDraft(String(viewer.currentPageNumber));
      setScale(viewer.currentScale);
      setScaleValue(viewer.currentScaleValue || initialScaleValue);
      setRotation(viewer.pagesRotation);
      setReady(true);
      setLoadingProgress(100);
    };

    void open().catch((reason: unknown) => {
      if (!active) return;
      setError(
        reason instanceof Error
          ? reason.message
          : translate("components.pdfReader.openFailedFallback"),
      );
    });

    return () => {
      active = false;
      setReady(false);
      eventBus.off("pagechanging", onPageChanging);
      eventBus.off("scalechanging", onScaleChanging);
      eventBus.off("rotationchanging", onRotationChanging);
      eventBus.off("updatefindmatchescount", onFindCount);
      eventBus.off("updatefindcontrolstate", onFindState);
      // `cleanup` releases this viewer's page resources and `destroy` stops the
      // worker and its network requests; without both, the document outlives the
      // reader that opened it.
      viewer.cleanup();
      runtimeRef.current = null;
      setDocument(undefined);
      if (loadingTask) void loadingTask.destroy();
    };
  }, [file.id, url]);

  // Forwards the search settings to pdf.js: `type` is "" for a new search and
  // "again" for the next or previous match, and an empty query closes the find
  // controller so its highlights disappear.
  const dispatchFind = useCallback(
    (type: "" | "again" | "highlightallchange" = "", previous = false) => {
      const eventBus = runtimeRef.current?.eventBus;
      if (!eventBus) return;
      if (!query.trim()) {
        setFindCount({ current: 0, total: 0 });
        eventBus.dispatch("findbarclose", { source: containerRef.current });
        return;
      }
      eventBus.dispatch("find", {
        source: containerRef.current,
        type,
        query,
        phraseSearch: true,
        caseSensitive,
        entireWord: wholeWord,
        highlightAll: true,
        findPrevious: previous,
        matchDiacritics: false,
      });
    },
    [caseSensitive, query, wholeWord],
  );

  // Search runs while the user types: the delay batches keystrokes into one
  // dispatch instead of one per character.
  useEffect(() => {
    if (!ready || !searchOpen) return;
    const timer = window.setTimeout(() => dispatchFind(""), 180);
    return () => window.clearTimeout(timer);
  }, [dispatchFind, ready, searchOpen]);

  // Registered on capture, and the keys it consumes are stopped, so the reader
  // sees them before the modal's own dismissal handling does.
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "f") {
        event.preventDefault();
        event.stopPropagation();
        setSearchOpen(true);
        return;
      }
      if (event.key === "Escape" && searchOpen && !inNestedOverlay(event.target)) {
        event.preventDefault();
        event.stopPropagation();
        setSearchOpen(false);
        setQuery("");
        runtimeRef.current?.eventBus.dispatch("findbarclose", { source: containerRef.current });
        return;
      }
      if (
        event.key === "Escape" &&
        annotationToolRef.current !== "select" &&
        !inNestedOverlay(event.target)
      ) {
        // Escape leaves the annotation tool before it closes the reader.
        event.preventDefault();
        event.stopPropagation();
        setTool("select");
        return;
      }
      if (
        event.key === "Escape" &&
        !isEditableTarget(event.target) &&
        !inNestedOverlay(event.target) &&
        (!window.document.activeElement || window.document.activeElement === window.document.body)
      ) {
        // Focus escaped the modal (e.g. after triggering a download), in which
        // case the modal ignores Escape. Close explicitly instead.
        event.preventDefault();
        event.stopPropagation();
        closeRef.current();
        return;
      }
      if (isEditableTarget(event.target)) return;
      const runtime = runtimeRef.current;
      if (!runtime) return;
      if (event.key === "+" || event.key === "=") {
        event.preventDefault();
        setPdfScale(runtime.viewer.currentScale * 1.1);
      } else if (event.key === "-") {
        event.preventDefault();
        setPdfScale(runtime.viewer.currentScale / 1.1);
      } else if (event.key === "PageDown" || event.key === "ArrowRight") {
        event.preventDefault();
        runtime.viewer.currentPageNumber = Math.min(
          runtime.viewer.currentPageNumber + 1,
          runtime.document.numPages,
        );
      } else if (event.key === "PageUp" || event.key === "ArrowLeft") {
        event.preventDefault();
        runtime.viewer.currentPageNumber = Math.max(runtime.viewer.currentPageNumber - 1, 1);
      }
    };
    window.addEventListener("keydown", onKeyDown, true);
    return () => window.removeEventListener("keydown", onKeyDown, true);
  }, [searchOpen]);

  // Zoom is applied to the viewer instead of to state: the `scalechanging`
  // event writes `scale` back, and the clamp holds it inside 0.25x-5x.
  const setPdfScale = (next: number) => {
    const viewer = runtimeRef.current?.viewer;
    if (!viewer) return;
    viewer.currentScale = Math.min(5, Math.max(0.25, next));
  };

  const setPdfScaleValue = (next: string) => {
    const viewer = runtimeRef.current?.viewer;
    if (!viewer) return;
    viewer.currentScaleValue = next;
  };

  const goToPage = (next: number) => {
    const runtime = runtimeRef.current;
    if (!runtime) return;
    runtime.viewer.currentPageNumber = Math.min(
      Math.max(Math.round(next), 1),
      runtime.document.numPages,
    );
  };

  // Commit the page box: an empty box, a value that is not a number or one that
  // falls outside the document reverts to the page the viewer is actually on.
  const commitPageDraft = () => {
    const total = runtimeRef.current?.document.numPages ?? 0;
    const next = Number(pageDraft);
    if (pageDraft.trim() && Number.isInteger(next) && next >= 1 && next <= total) goToPage(next);
    else setPageDraft(String(pageNumber));
  };

  const rotate = () => {
    const viewer = runtimeRef.current?.viewer;
    if (!viewer) return;
    viewer.pagesRotation = normalizedRotation(viewer.pagesRotation + 90);
  };

  const setTool = (tool: AnnotationTool) => {
    const viewer = runtimeRef.current?.viewer;
    if (!viewer) return;
    annotationToolRef.current = tool;
    setAnnotationTool(tool);
    viewer.annotationEditorMode = { mode: annotationEditorMode(tool) };
    // The tool is passed explicitly because `annotationTool` still holds the
    // previous value in this render, and pdf.js colours the editor being opened.
    if (tool !== "select") setAnnotationColor(annotationColor, tool);
  };

  // Sets the picked colour and, for a drawing tool, tells pdf.js about it: the
  // dispatch type differs per tool, and `select` has no editor to colour.
  const setAnnotationColor = (
    color: (typeof HIGHLIGHT_COLORS)[number],
    tool: AnnotationTool = annotationTool,
  ) => {
    setAnnotationColorState(color);
    const eventBus = runtimeRef.current?.eventBus;
    if (!eventBus || tool === "select") return;
    const type =
      tool === "highlight"
        ? AnnotationEditorParamsType.HIGHLIGHT_COLOR
        : tool === "ink"
          ? AnnotationEditorParamsType.INK_COLOR
          : AnnotationEditorParamsType.FREETEXT_COLOR;
    eventBus.dispatch("switchannotationeditorparams", {
      source: containerRef.current,
      type,
      value: color,
    });
  };

  // Downloads the document with the annotations baked in, as a `-edited.pdf`
  // copy. The original file is left untouched.
  const saveModified = async () => {
    const pdf = runtimeRef.current?.document;
    if (!pdf || saving) return;
    setSaving(true);
    try {
      const data = await pdf.saveDocument();
      const copy = new Uint8Array(data);
      const objectUrl = URL.createObjectURL(new Blob([copy.buffer], { type: "application/pdf" }));
      const anchor = window.document.createElement("a");
      anchor.href = objectUrl;
      anchor.download = editedPdfName(file.name);
      anchor.click();
      // The blob URL is revoked once the browser has had time to start the
      // download, so it does not stay alive for the rest of the session.
      window.setTimeout(() => URL.revokeObjectURL(objectUrl), 1_000);
    } catch (reason) {
      // `saveDocument()` rejects when the edited annotations cannot be
      // serialised, so the copy never downloads: say so instead of only
      // stopping the spinner on the button.
      toast.error(t("components.pdfReader.saveFailed"), {
        description: reason instanceof Error ? reason.message : undefined,
      });
    } finally {
      setSaving(false);
    }
  };

  const downloadOriginal = () => {
    const anchor = window.document.createElement("a");
    anchor.href = url;
    anchor.download = file.name;
    anchor.click();
  };

  // The zoom button names the active preset; otherwise it shows the numeric
  // zoom as a percentage.
  const zoomLabel = useMemo(() => {
    if (scaleValue === "page-width") return t("components.pdfReader.fitWidth");
    if (scaleValue === "page-fit") return t("components.pdfReader.fitPage");
    if (scaleValue === "page-actual") return t("components.pdfReader.actualSize");
    return `${Math.round(scale * 100)}%`;
  }, [scale, scaleValue, t]);

  const sidebar = (
    <PdfSidebar
      file={file}
      document={document}
      pageNumber={pageNumber}
      outline={outline}
      selectedTab={sidebarTab}
      onTabChange={setSidebarTab}
      onPage={goToPage}
      onOutline={(item) => {
        const runtime = runtimeRef.current;
        if (!runtime) return;
        // An outline row either points outside the document or names a
        // destination inside it, which only the link service can resolve.
        if (item.url) {
          window.open(item.url, "_blank", "noopener,noreferrer");
          return;
        }
        if (item.dest) void runtime.linkService.goToDestination(item.dest as string | unknown[]);
      }}
      onNavigateMobile={() => drawerState.close()}
    />
  );

  return (
    <div
      data-pdf-reader
      className="flex h-dvh min-h-0 w-full flex-col overflow-hidden bg-background text-foreground"
    >
      <header
        data-pdf-toolbar
        className="relative z-30 flex min-h-14 shrink-0 items-center gap-1.5 border-b border-border bg-background/95 px-2 backdrop-blur-xl sm:px-3"
      >
        <Button
          isIconOnly
          size="sm"
          variant="ghost"
          aria-label={t("components.pdfReader.close")}
          onPress={() => closeRef.current()}
        >
          <CloseIcon className="size-4" />
        </Button>
        <Button
          isIconOnly
          size="sm"
          variant={sidebarOpen ? "secondary" : "ghost"}
          aria-label={t("components.pdfReader.toggleSidebar")}
          className="hidden lg:inline-flex"
          onPress={() => setSidebarOpen((value) => !value)}
        >
          <MenuIcon className="size-4" />
        </Button>
        <Drawer state={drawerState}>
          <Button
            isIconOnly
            size="sm"
            variant="ghost"
            aria-label={t("components.pdfReader.openSidebar")}
            className={isDesktop ? "hidden" : undefined}
          >
            <MenuIcon className="size-4" />
          </Button>
          <Drawer.Backdrop variant="blur">
            <Drawer.Content placement="left" className="w-[min(88vw,20rem)]">
              <Drawer.Dialog>
                <Drawer.Header className="border-b border-border">
                  <Drawer.Heading>{t("components.pdfReader.navigation")}</Drawer.Heading>
                  <Drawer.CloseTrigger />
                </Drawer.Header>
                <Drawer.Body className="min-h-0 p-0">{sidebar}</Drawer.Body>
              </Drawer.Dialog>
            </Drawer.Content>
          </Drawer.Backdrop>
        </Drawer>

        <div className="mr-1 hidden min-w-0 max-w-64 lg:block xl:max-w-80">
          <p className="truncate text-xs font-semibold">{file.name}</p>
          <p className="text-[10px] text-muted">PDF · {formatBytes(file.size || 0)}</p>
        </div>

        <div className="hidden h-6 w-px bg-border sm:block" />

        <Button
          isIconOnly
          size="sm"
          variant="ghost"
          aria-label={t("components.pdfReader.previousPage")}
          isDisabled={!ready || pageNumber <= 1}
          onPress={() => goToPage(pageNumber - 1)}
        >
          <LeftIcon className="size-4" />
        </Button>
        <div className="flex items-center gap-1 text-xs tabular-nums">
          <InputGroup className="w-14" variant="secondary">
            <InputGroup.Input
              aria-label={t("components.pdfReader.pageNumber")}
              inputMode="numeric"
              value={pageDraft}
              onChange={(event) => setPageDraft(event.target.value.replace(/[^0-9]/g, ""))}
              onBlur={commitPageDraft}
              onKeyDown={(event: ReactKeyboardEvent<HTMLInputElement>) => {
                if (event.key === "Enter") {
                  commitPageDraft();
                  event.currentTarget.blur();
                }
              }}
              className="h-8 text-center text-xs tabular-nums"
            />
          </InputGroup>
          <span className="min-w-8 text-muted">/ {numPages || "—"}</span>
        </div>
        <Button
          isIconOnly
          size="sm"
          variant="ghost"
          aria-label={t("components.pdfReader.nextPage")}
          isDisabled={!ready || pageNumber >= numPages}
          onPress={() => goToPage(pageNumber + 1)}
        >
          <RightIcon className="size-4" />
        </Button>

        <div className="hidden h-6 w-px bg-border md:block" />
        <Button
          isIconOnly
          size="sm"
          variant="ghost"
          aria-label={t("components.pdfReader.zoomOut")}
          className="hidden md:inline-flex"
          isDisabled={!ready}
          onPress={() => setPdfScale(scale / 1.1)}
        >
          <ZoomOutIcon className="size-4" />
        </Button>
        <Popover>
          <Button
            size="sm"
            variant="ghost"
            className="hidden min-w-20 px-2 text-xs md:inline-flex"
            isDisabled={!ready}
          >
            {zoomLabel}
          </Button>
          <Popover.Content placement="bottom" offset={8} className="w-44">
            <Popover.Dialog className="space-y-1 p-1.5">
              {SCALE_PRESETS.map(({ labelKey, value }) => (
                <Button
                  key={value}
                  size="sm"
                  variant={scaleValue === value ? "secondary" : "ghost"}
                  className="w-full justify-start"
                  onPress={() => setPdfScaleValue(value)}
                >
                  {t(labelKey)}
                </Button>
              ))}
              <div className="border-t border-border pt-1">
                {[75, 100, 125, 150, 200].map((percent) => (
                  <Button
                    key={percent}
                    size="sm"
                    variant="ghost"
                    className="w-full justify-start"
                    onPress={() => setPdfScale(percent / 100)}
                  >
                    {percent}%
                  </Button>
                ))}
              </div>
            </Popover.Dialog>
          </Popover.Content>
        </Popover>
        <Button
          isIconOnly
          size="sm"
          variant="ghost"
          aria-label={t("components.pdfReader.zoomIn")}
          className="hidden md:inline-flex"
          isDisabled={!ready}
          onPress={() => setPdfScale(scale * 1.1)}
        >
          <ZoomInIcon className="size-4" />
        </Button>
        <Button
          isIconOnly
          size="sm"
          variant="ghost"
          aria-label={t("components.pdfReader.rotate")}
          className="hidden md:inline-flex"
          isDisabled={!ready}
          onPress={rotate}
        >
          <RotateIcon className="size-4" />
        </Button>

        <div className="hidden h-6 w-px bg-border xl:block" />
        <div
          className="hidden items-center gap-0.5 xl:flex"
          role="toolbar"
          aria-label={t("components.pdfReader.annotationTools")}
        >
          <ToolButton
            label={t("components.pdfReader.selectText")}
            active={annotationTool === "select"}
            onPress={() => setTool("select")}
          >
            <HandIcon className="size-4" />
          </ToolButton>
          <ToolButton
            label={t("components.pdfReader.highlight")}
            active={annotationTool === "highlight"}
            onPress={() => setTool("highlight")}
          >
            <BrushIcon className="size-4" />
          </ToolButton>
          <ToolButton
            label={t("components.pdfReader.addText")}
            active={annotationTool === "text"}
            onPress={() => setTool("text")}
          >
            <TextIcon className="size-4" />
          </ToolButton>
          <ToolButton
            label={t("components.pdfReader.draw")}
            active={annotationTool === "ink"}
            onPress={() => setTool("ink")}
          >
            <PencilIcon className="size-4" />
          </ToolButton>
          {annotationTool !== "select" ? (
            <Popover>
              <Button
                isIconOnly
                size="sm"
                variant="ghost"
                aria-label={t("components.pdfReader.annotationColor")}
              >
                <span
                  className="size-3.5 rounded-full border border-black/15"
                  style={{ background: annotationColor }}
                />
              </Button>
              <Popover.Content placement="bottom" offset={8} className="w-auto">
                <Popover.Dialog className="flex gap-1.5 p-2">
                  {HIGHLIGHT_COLORS.map((color) => (
                    <Button
                      key={color}
                      isIconOnly
                      size="sm"
                      variant={annotationColor === color ? "secondary" : "ghost"}
                      aria-label={t("components.pdfReader.useAnnotationColor", { color })}
                      onPress={() => setAnnotationColor(color)}
                    >
                      <span
                        className="size-4 rounded-full border border-black/15"
                        style={{ background: color }}
                      />
                    </Button>
                  ))}
                </Popover.Dialog>
              </Popover.Content>
            </Popover>
          ) : null}
        </div>

        <Popover>
          <Button
            isIconOnly
            size="sm"
            variant="ghost"
            className="xl:hidden"
            aria-label={t("components.pdfReader.tools")}
            isDisabled={!ready}
          >
            <EllipsisIcon className="size-4" />
          </Button>
          <Popover.Content placement="bottom end" offset={8} className="w-[min(92vw,17rem)]">
            <Popover.Dialog className="p-2">
              <p className="px-2 pb-1.5 text-[10px] font-semibold uppercase tracking-[0.12em] text-muted">
                {t("components.pdfReader.viewSection")}
              </p>
              <div className="grid grid-cols-2 gap-1">
                <Button
                  size="sm"
                  variant={scaleValue === "page-width" ? "secondary" : "ghost"}
                  onPress={() => setPdfScaleValue("page-width")}
                >
                  {t("components.pdfReader.fitWidth")}
                </Button>
                <Button
                  size="sm"
                  variant={scaleValue === "page-fit" ? "secondary" : "ghost"}
                  onPress={() => setPdfScaleValue("page-fit")}
                >
                  {t("components.pdfReader.fitPage")}
                </Button>
                <Button size="sm" variant="ghost" onPress={() => setPdfScale(scale / 1.1)}>
                  <ZoomOutIcon className="size-4" /> {t("components.pdfReader.zoomOut")}
                </Button>
                <Button size="sm" variant="ghost" onPress={() => setPdfScale(scale * 1.1)}>
                  <ZoomInIcon className="size-4" /> {t("components.pdfReader.zoomIn")}
                </Button>
                <Button size="sm" variant="ghost" className="col-span-2" onPress={rotate}>
                  <RotateIcon className="size-4" /> {t("components.pdfReader.rotateMenu")}
                </Button>
              </div>

              <div className="mt-2 border-t border-border pt-2">
                <p className="px-2 pb-1.5 text-[10px] font-semibold uppercase tracking-[0.12em] text-muted">
                  {t("components.pdfReader.annotateSection")}
                </p>
                <div className="grid grid-cols-2 gap-1">
                  <Button
                    size="sm"
                    variant={annotationTool === "select" ? "secondary" : "ghost"}
                    onPress={() => setTool("select")}
                  >
                    <HandIcon className="size-4" /> {t("components.pdfReader.selectTool")}
                  </Button>
                  <Button
                    size="sm"
                    variant={annotationTool === "highlight" ? "secondary" : "ghost"}
                    onPress={() => setTool("highlight")}
                  >
                    <BrushIcon className="size-4" /> {t("components.pdfReader.highlight")}
                  </Button>
                  <Button
                    size="sm"
                    variant={annotationTool === "text" ? "secondary" : "ghost"}
                    onPress={() => setTool("text")}
                  >
                    <TextIcon className="size-4" /> {t("components.pdfReader.addText")}
                  </Button>
                  <Button
                    size="sm"
                    variant={annotationTool === "ink" ? "secondary" : "ghost"}
                    onPress={() => setTool("ink")}
                  >
                    <PencilIcon className="size-4" /> {t("components.pdfReader.draw")}
                  </Button>
                </div>
                {annotationTool !== "select" ? (
                  <div className="mt-2 flex items-center justify-between rounded-lg bg-default/25 px-2 py-1.5">
                    <span className="text-[11px] text-muted">
                      {t("components.pdfReader.color")}
                    </span>
                    <div className="flex gap-1">
                      {HIGHLIGHT_COLORS.map((color) => (
                        <Button
                          key={color}
                          isIconOnly
                          size="sm"
                          variant={annotationColor === color ? "secondary" : "ghost"}
                          aria-label={t("components.pdfReader.useAnnotationColor", { color })}
                          onPress={() => setAnnotationColor(color)}
                        >
                          <span
                            className="size-3.5 rounded-full border border-black/15"
                            style={{ background: color }}
                          />
                        </Button>
                      ))}
                    </div>
                  </div>
                ) : null}
              </div>

              <div className="mt-2 grid grid-cols-2 gap-1 border-t border-border pt-2">
                <Button
                  size="sm"
                  variant="ghost"
                  isDisabled={saving}
                  onPress={() => void saveModified()}
                >
                  {saving ? <Spinner size="sm" /> : <SaveIcon className="size-4" />}{" "}
                  {t("components.pdfReader.saveCopy")}
                </Button>
                <Button size="sm" variant="ghost" onPress={downloadOriginal}>
                  <DownloadIcon className="size-4" /> {t("components.pdfReader.original")}
                </Button>
              </div>
            </Popover.Dialog>
          </Popover.Content>
        </Popover>

        <div className="flex-1" />
        <Button
          isIconOnly
          size="sm"
          variant={searchOpen ? "secondary" : "ghost"}
          aria-label={t("components.pdfReader.search")}
          onPress={() => setSearchOpen((value) => !value)}
        >
          <SearchIcon className="size-4" />
        </Button>
        <Button
          isIconOnly
          size="sm"
          variant="ghost"
          className="hidden xl:inline-flex"
          aria-label={t("components.pdfReader.saveCopyAria")}
          isDisabled={!ready || saving}
          onPress={() => void saveModified()}
        >
          {saving ? <Spinner size="sm" /> : <SaveIcon className="size-4" />}
        </Button>
        <Button
          isIconOnly
          size="sm"
          variant="ghost"
          className="hidden xl:inline-flex"
          aria-label={t("components.pdfReader.downloadOriginal")}
          onPress={downloadOriginal}
        >
          <DownloadIcon className="size-4" />
        </Button>
      </header>

      {searchOpen ? (
        <PdfFindBar
          query={query}
          onQuery={setQuery}
          count={findCount}
          state={findState}
          caseSensitive={caseSensitive}
          wholeWord={wholeWord}
          onCaseSensitive={setCaseSensitive}
          onWholeWord={setWholeWord}
          onPrevious={() => dispatchFind("again", true)}
          onNext={() => dispatchFind("again", false)}
          onClose={() => {
            setSearchOpen(false);
            setQuery("");
            runtimeRef.current?.eventBus.dispatch("findbarclose", { source: containerRef.current });
          }}
        />
      ) : null}

      <div className="flex min-h-0 flex-1 overflow-hidden">
        {isDesktop && sidebarOpen ? (
          <aside className="w-64 shrink-0 border-r border-border bg-surface/80 xl:w-72">
            {sidebar}
          </aside>
        ) : null}
        <main className="relative min-w-0 flex-1 overflow-hidden bg-default/35">
          {!ready && !error && !passwordChallenge ? (
            <div className="absolute inset-0 z-20 grid place-items-center bg-background/70 backdrop-blur-sm">
              <div className="text-center">
                <Spinner size="lg" aria-label={t("components.pdfReader.loading")} />
                <p className="mt-3 text-xs text-muted">
                  {loadingProgress === undefined
                    ? t("components.pdfReader.opening")
                    : t("components.pdfReader.loadingProgress", { progress: loadingProgress })}
                </p>
              </div>
            </div>
          ) : null}
          {passwordChallenge ? (
            <div className="absolute inset-0 z-30 grid place-items-center bg-background/80 p-5 backdrop-blur-md">
              <div className="w-full max-w-sm rounded-2xl border border-border bg-surface p-5 shadow-2xl">
                <p className="text-sm font-semibold">{t("components.pdfReader.protectedTitle")}</p>
                <p className="mt-1 text-xs leading-5 text-muted">
                  {passwordChallenge.incorrect
                    ? t("components.pdfReader.incorrectPassword")
                    : t("components.pdfReader.passwordPrompt")}
                </p>
                <InputGroup className="mt-4" variant="secondary">
                  <InputGroup.Input
                    autoFocus
                    type="password"
                    aria-label={t("components.pdfReader.passwordLabel")}
                    placeholder={t("components.pdfReader.passwordPlaceholder")}
                    value={passwordDraft}
                    onChange={(event) => setPasswordDraft(event.target.value)}
                    onKeyDown={(event: ReactKeyboardEvent<HTMLInputElement>) => {
                      if (event.key === "Enter" && passwordDraft) {
                        passwordChallenge.submit(passwordDraft);
                        setPasswordChallenge(undefined);
                      }
                    }}
                  />
                </InputGroup>
                <div className="mt-4 flex justify-end gap-2">
                  <Button size="sm" variant="ghost" onPress={() => closeRef.current()}>
                    {t("common.action.cancel")}
                  </Button>
                  <Button
                    size="sm"
                    variant="primary"
                    isDisabled={!passwordDraft}
                    onPress={() => {
                      passwordChallenge.submit(passwordDraft);
                      setPasswordChallenge(undefined);
                    }}
                  >
                    {t("components.pdfReader.unlock")}
                  </Button>
                </div>
              </div>
            </div>
          ) : null}
          {error ? (
            <div className="absolute inset-0 z-20 grid place-items-center p-6 text-center">
              <div className="max-w-lg">
                <p className="font-semibold">{t("components.pdfReader.openFailed")}</p>
                <p className="mt-2 text-sm text-muted">{error}</p>
              </div>
            </div>
          ) : null}
          <div
            ref={containerRef}
            data-pdf-viewer-container
            className="absolute inset-0 overflow-auto"
          >
            <div ref={viewerRef} className="pdfViewer teldrive-pdf-viewer" />
          </div>
        </main>
      </div>
    </div>
  );
}

/**
 * Find bar above the viewer: query box with the match counter, previous/next,
 * case and whole-word toggles, and the close button. Enter moves to the next
 * match, Shift+Enter to the previous one.
 */
function PdfFindBar({
  query,
  onQuery,
  count,
  state,
  caseSensitive,
  wholeWord,
  onCaseSensitive,
  onWholeWord,
  onPrevious,
  onNext,
  onClose,
}: {
  query: string;
  onQuery: (value: string) => void;
  count: FindCount;
  state: number;
  caseSensitive: boolean;
  wholeWord: boolean;
  onCaseSensitive: (value: boolean) => void;
  onWholeWord: (value: boolean) => void;
  onPrevious: () => void;
  onNext: () => void;
  onClose: () => void;
}) {
  const { t } = useI18n();
  return (
    <div
      data-pdf-findbar
      className="z-20 flex min-h-12 shrink-0 items-center gap-1.5 border-b border-border bg-surface/95 px-2 backdrop-blur-xl sm:px-3"
    >
      <SearchIcon className="hidden size-4 text-muted sm:block" />
      <InputGroup className="max-w-md flex-1" variant="secondary">
        <InputGroup.Input
          autoFocus
          aria-label={t("components.pdfReader.findLabel")}
          placeholder={t("components.pdfReader.findPlaceholder")}
          value={query}
          onChange={(event) => onQuery(event.target.value)}
          onKeyDown={(event: ReactKeyboardEvent<HTMLInputElement>) => {
            if (event.key === "Enter") {
              event.preventDefault();
              if (event.shiftKey) onPrevious();
              else onNext();
            }
          }}
          className="h-8 text-sm"
        />
        <InputGroup.Suffix className="text-[11px] tabular-nums text-muted">
          {query && state === FindState.NOT_FOUND
            ? t("components.pdfReader.noMatches")
            : query
              ? `${count.current} / ${count.total}`
              : ""}
        </InputGroup.Suffix>
      </InputGroup>
      <Button
        isIconOnly
        size="sm"
        variant="ghost"
        aria-label={t("components.pdfReader.previousResult")}
        isDisabled={!count.total}
        onPress={onPrevious}
      >
        <LeftIcon className="size-4" />
      </Button>
      <Button
        isIconOnly
        size="sm"
        variant="ghost"
        aria-label={t("components.pdfReader.nextResult")}
        isDisabled={!count.total}
        onPress={onNext}
      >
        <RightIcon className="size-4" />
      </Button>
      <Button
        size="sm"
        variant={caseSensitive ? "secondary" : "ghost"}
        className="hidden min-w-8 px-2 text-xs font-semibold sm:inline-flex"
        aria-label={t("components.pdfReader.matchCase")}
        onPress={() => onCaseSensitive(!caseSensitive)}
      >
        Aa
      </Button>
      <Button
        size="sm"
        variant={wholeWord ? "secondary" : "ghost"}
        className="hidden px-2 text-xs sm:inline-flex"
        aria-label={t("components.pdfReader.matchWholeWords")}
        onPress={() => onWholeWord(!wholeWord)}
      >
        {t("components.pdfReader.matchWholeWordsBadge")}
      </Button>
      <Button
        isIconOnly
        size="sm"
        variant="ghost"
        aria-label={t("components.pdfReader.closeSearch")}
        onPress={onClose}
      >
        <CloseIcon className="size-4" />
      </Button>
    </div>
  );
}

/** Pages the thumbnail list adds at a time. */
const pageBatch = 40;

/**
 * Sidebar panel with the page thumbnails and the document outline. Thumbnails
 * are added in batches as the list is scrolled, and picking a row closes the
 * mobile drawer through `onNavigateMobile`; `document` stays undefined until
 * the document has loaded.
 */
function PdfSidebar({
  file,
  document,
  pageNumber,
  outline,
  selectedTab,
  onTabChange,
  onPage,
  onOutline,
  onNavigateMobile,
}: {
  file: FileEntry;
  document?: PDFDocumentProxy;
  pageNumber: number;
  outline: OutlineItem[];
  selectedTab: SidebarTab;
  onTabChange: (tab: SidebarTab) => void;
  onPage: (page: number) => void;
  onOutline: (item: OutlineItem) => void;
  onNavigateMobile: () => void;
}) {
  const { t } = useI18n();
  // The panel draws a tile per page, and a long document would otherwise mount one
  // component for every page while the panel opens. The list grows by a batch when
  // the reader reaches its end, and a page opened from the outline brings its
  // neighbours with it so the list around it is ready.
  const [renderedPages, setRenderedPages] = useState(pageBatch);
  const sentinelRef = useRef<HTMLDivElement>(null);
  const hasMorePages = Boolean(document) && renderedPages < (document?.numPages ?? 0);

  useEffect(() => {
    setRenderedPages((value) => Math.max(value, pageNumber + pageBatch));
  }, [pageNumber]);

  useEffect(() => {
    setRenderedPages(pageBatch);
  }, [document]);

  useEffect(() => {
    const sentinel = sentinelRef.current;
    if (!sentinel) return;
    return observeVisibility(sentinel, (visible) => {
      if (visible) setRenderedPages((value) => value + pageBatch);
    });
  }, [hasMorePages]);

  return (
    <div className="flex h-full min-h-0 flex-col bg-surface/75">
      <div className="border-b border-border px-4 py-3">
        <p className="truncate text-xs font-semibold">{file.name}</p>
        <p className="mt-0.5 text-[10px] text-muted">
          {document
            ? t("components.pdfReader.pageCount", { count: document.numPages })
            : t("components.pdfReader.loadingDocument")}
        </p>
      </div>
      <Tabs
        selectedKey={selectedTab}
        onSelectionChange={(key) => onTabChange(key === "outline" ? "outline" : "thumbnails")}
        className="flex min-h-0 flex-1 flex-col px-2 pt-2"
      >
        <Tabs.ListContainer>
          <Tabs.List aria-label={t("components.pdfReader.sidebarAria")} className="w-full">
            <Tabs.Tab id="thumbnails" className="flex-1 gap-1.5 text-xs">
              <MenuIcon className="size-3.5" /> {t("components.pdfReader.tabPages")}
            </Tabs.Tab>
            <Tabs.Tab id="outline" className="flex-1 gap-1.5 text-xs">
              <BookmarkIcon className="size-3.5" /> {t("components.pdfReader.tabOutline")}
            </Tabs.Tab>
          </Tabs.List>
        </Tabs.ListContainer>
        <Tabs.Panel id="thumbnails" className="min-h-0 flex-1 overflow-y-auto py-2">
          {document ? (
            <div className="grid grid-cols-1 gap-2 px-1 pb-3">
              {Array.from(
                { length: Math.min(renderedPages, document.numPages) },
                (_, index) => index + 1,
              ).map((page) => (
                <PdfThumbnail
                  key={page}
                  document={document}
                  pageNumber={page}
                  current={page === pageNumber}
                  onPress={() => {
                    onPage(page);
                    onNavigateMobile();
                  }}
                />
              ))}
              {hasMorePages ? <div ref={sentinelRef} aria-hidden className="h-1" /> : null}
            </div>
          ) : (
            <SidebarEmpty label={t("components.pdfReader.preparingPages")} />
          )}
        </Tabs.Panel>
        <Tabs.Panel id="outline" className="min-h-0 flex-1 overflow-y-auto py-2">
          {outline.length ? (
            <div className="space-y-0.5 px-1 pb-3">
              <OutlineItems
                items={outline}
                depth={0}
                onSelect={(item) => {
                  onOutline(item);
                  onNavigateMobile();
                }}
              />
            </div>
          ) : (
            <SidebarEmpty label={t("components.pdfReader.noOutline")} />
          )}
        </Tabs.Panel>
      </Tabs>
    </div>
  );
}

// One IntersectionObserver serves every element that needs to know when it is on
// screen. The sidebar renders a tile for each page, so a long document used to
// mount one observer per page, and that registration cost dominated opening the
// panel. Entries stay observed, because a tile has to notice when it scrolls back
// into view.
const visibilityCallbacks = new WeakMap<Element, (visible: boolean) => void>();
/** Created on first use, then shared by every observed element. */
let visibilityObserver: IntersectionObserver | undefined;

/**
 * Calls `onVisibilityChange` whenever the element enters or leaves the
 * viewport, with a 300 px margin on every side so work starts just before the
 * element is on screen. Returns the cleanup that stops observing. Without
 * IntersectionObserver support the callback fires once and nothing is observed,
 * which keeps the content visible instead of dropping it.
 */
function observeVisibility(element: Element, onVisibilityChange: (visible: boolean) => void) {
  if (typeof IntersectionObserver === "undefined") {
    onVisibilityChange(true);
    return () => {};
  }
  visibilityObserver ??= new IntersectionObserver(
    (entries) => {
      for (const entry of entries) {
        visibilityCallbacks.get(entry.target)?.(entry.isIntersecting);
      }
    },
    { rootMargin: "300px" },
  );
  visibilityCallbacks.set(element, onVisibilityChange);
  visibilityObserver.observe(element);
  return () => {
    visibilityCallbacks.delete(element);
    visibilityObserver?.unobserve(element);
  };
}

/**
 * One thumbnail tile. The page is only fetched and rendered once the tile is
 * near the viewport, the canvas is sized for the display's pixel ratio, and
 * `current` marks the page the reader is on.
 */
function PdfThumbnail({
  document,
  pageNumber,
  current,
  onPress,
}: {
  document: PDFDocumentProxy;
  pageNumber: number;
  current: boolean;
  onPress: () => void;
}) {
  const { t } = useI18n();
  const hostRef = useRef<HTMLDivElement>(null);
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const pageRef = useRef<Awaited<ReturnType<PDFDocumentProxy["getPage"]>> | undefined>(undefined);
  const [visible, setVisible] = useState(false);
  // Aspect ratio of the last page rendered here, kept in state so the tile
  // reserves the right height before its canvas has any pixels.
  const [ratio, setRatio] = useState(1.294);
  const [loaded, setLoaded] = useState(false);

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;
    return observeVisibility(host, setVisible);
  }, []);

  // `active` and the cancelled render task are what stop a page that resolves
  // after the tile scrolled away from drawing into a detached canvas.
  useEffect(() => {
    if (!visible || loaded) return;
    const canvas = canvasRef.current;
    if (!canvas) return;
    let active = true;
    let renderTask:
      | ReturnType<Awaited<ReturnType<PDFDocumentProxy["getPage"]>>["render"]>
      | undefined;
    void document
      .getPage(pageNumber)
      .then((page) => {
        if (!active) return;
        pageRef.current = page;
        const natural = page.getViewport({ scale: 1 });
        setRatio(natural.height / natural.width);
        // Tiles are a fixed 142 CSS px wide. The canvas is drawn at the display
        // pixel ratio (capped at 2) so the thumbnail stays sharp without
        // allocating a 3x bitmap on a high-density screen.
        const cssWidth = 142;
        const scale = cssWidth / natural.width;
        const dpr = Math.min(window.devicePixelRatio || 1, 2);
        const viewport = page.getViewport({ scale: scale * dpr });
        canvas.width = Math.ceil(viewport.width);
        canvas.height = Math.ceil(viewport.height);
        canvas.style.width = `${cssWidth}px`;
        canvas.style.height = `${Math.round(cssWidth * (natural.height / natural.width))}px`;
        renderTask = page.render({ canvas, viewport });
        return renderTask.promise;
      })
      .then(() => {
        if (active) setLoaded(true);
      })
      .catch(() => undefined);
    return () => {
      active = false;
      renderTask?.cancel();
    };
  }, [document, loaded, pageNumber, visible]);

  // One rendered thumbnail bitmap per page would stay allocated for as long as
  // the sidebar lives, so a page is released as soon as its thumbnail leaves the
  // viewport (and unmounts) and drawn again when it comes back: `page.cleanup()`
  // drops the operator list and the canvas backing store goes with it.
  useEffect(() => {
    if (!visible) return;
    return () => {
      pageRef.current?.cleanup();
      pageRef.current = undefined;
      const canvas = canvasRef.current;
      if (canvas) {
        canvas.width = 0;
        canvas.height = 0;
        canvas.style.width = "";
        canvas.style.height = "";
      }
      setLoaded(false);
    };
  }, [visible]);

  return (
    <div ref={hostRef} className="flex justify-center py-1">
      <Button
        variant={current ? "secondary" : "ghost"}
        className={cn(
          "h-auto w-full flex-col gap-2 rounded-xl px-2 py-2",
          current && "ring-1 ring-accent/40",
        )}
        aria-label={t("components.pdfReader.goToPage", { page: pageNumber })}
        onPress={onPress}
      >
        <div
          className="relative w-35.5 max-w-full overflow-hidden rounded-sm border border-border bg-white shadow-sm"
          style={{ aspectRatio: `1 / ${ratio}` }}
        >
          <canvas ref={canvasRef} className={cn("block max-w-full", !loaded && "opacity-0")} />
          {!loaded ? <div className="absolute inset-0 animate-pulse bg-default/20" /> : null}
        </div>
        <span className="text-[10px] font-medium tabular-nums text-muted">{pageNumber}</span>
      </Button>
    </div>
  );
}

/**
 * Outline rows rendered recursively, indented by `depth`. Rows without a title
 * show a placeholder, since pdf.js does not require one.
 */
function OutlineItems({
  items,
  depth,
  onSelect,
}: {
  items: OutlineItem[];
  depth: number;
  onSelect: (item: OutlineItem) => void;
}) {
  const { t } = useI18n();
  return items.map((item) => (
    <div key={`${depth}-${item.title}-${outlineDestinationKey(item)}`}>
      <Button
        size="sm"
        variant="ghost"
        className="h-auto w-full justify-start whitespace-normal py-2 text-left text-xs"
        style={{ paddingInlineStart: `${10 + depth * 14}px` }}
        onPress={() => onSelect(item)}
      >
        <span className="line-clamp-2">
          {item.title || t("components.pdfReader.untitledSection")}
        </span>
      </Button>
      {item.items?.length ? (
        <OutlineItems items={item.items} depth={depth + 1} onSelect={onSelect} />
      ) : null}
    </div>
  ));
}

/** Icon button of the annotation toolbar; `active` marks the selected tool. */
function ToolButton({
  label,
  active,
  onPress,
  children,
}: {
  label: string;
  active: boolean;
  onPress: () => void;
  children: React.ReactNode;
}) {
  return (
    <Button
      isIconOnly
      size="sm"
      variant={active ? "secondary" : "ghost"}
      aria-label={label}
      onPress={onPress}
    >
      {children}
    </Button>
  );
}

/** Placeholder shown in a sidebar panel that has nothing to list yet. */
function SidebarEmpty({ label }: { label: string }) {
  return <p className="px-3 py-8 text-center text-xs text-muted">{label}</p>;
}

/** pdf.js editor mode that matches the selected annotation tool. */
function annotationEditorMode(tool: AnnotationTool) {
  if (tool === "highlight") return AnnotationEditorType.HIGHLIGHT;
  if (tool === "text") return AnnotationEditorType.FREETEXT;
  if (tool === "ink") return AnnotationEditorType.INK;
  return AnnotationEditorType.NONE;
}

/**
 * Normalizes a rotation to a quarter turn in 0-359 degrees. Rotation arrives
 * from viewer events typed loosely, so a non-number counts as 0 and a negative
 * or oversized value is folded back into the range instead of propagating.
 */
function normalizedRotation(value: unknown) {
  const number = typeof value === "number" && Number.isFinite(value) ? value : 0;
  return (((Math.round(number / 90) * 90) % 360) + 360) % 360;
}

/**
 * Rounds a viewer-reported count (a page number) to a positive integer, falling
 * back when the event carries nothing usable.
 */
function positiveInt(value: unknown, fallback: number) {
  const number = typeof value === "number" ? value : Number(value);
  return Number.isFinite(number) && number > 0 ? Math.round(number) : fallback;
}

/** Returns a positive finite number from a viewer event, or the fallback. */
function positiveNumber(value: unknown, fallback: number) {
  return typeof value === "number" && Number.isFinite(value) && value > 0 ? value : fallback;
}

/**
 * React key for an outline row: pdf.js titles repeat, so the destination (or
 * the external address) is what distinguishes two rows.
 */
function outlineDestinationKey(item: OutlineItem) {
  if (item.url) return item.url;
  if (typeof item.dest === "string") return item.dest;
  return String(item.dest ?? "section");
}

/** Name of the downloaded copy: a `.pdf` suffix is replaced by `-edited.pdf`. */
function editedPdfName(name: string) {
  const index = name.toLowerCase().lastIndexOf(".pdf");
  return index >= 0 ? `${name.slice(0, index)}-edited.pdf` : `${name}-edited.pdf`;
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

// A portalled overlay (popover, drawer, nested dialog) that is not the reader
// itself owns Escape while it is open.
function inNestedOverlay(target: EventTarget | null) {
  if (!(target instanceof HTMLElement)) return false;
  const overlay = target.closest('[role="dialog"]');
  return overlay !== null && overlay.querySelector("[data-pdf-reader]") === null;
}

/** Formats a byte count in binary units; a falsy value renders as "0 B". */
function formatBytes(value: number) {
  if (!value) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const index = Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1);
  return `${(value / 1024 ** index).toFixed(index === 0 ? 0 : 1)} ${units[index]}`;
}
