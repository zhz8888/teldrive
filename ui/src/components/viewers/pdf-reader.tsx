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

GlobalWorkerOptions.workerSrc = workerSrc;

type PdfReaderProps = {
  file: FileEntry;
  url: string;
  onClose: () => void;
};

type SidebarTab = "thumbnails" | "outline";
type AnnotationTool = "select" | "highlight" | "text" | "ink";
type OutlineItem = {
  title: string;
  dest: string | unknown[] | null;
  url?: string | null;
  items?: OutlineItem[];
};

type PdfRuntime = {
  eventBus: EventBus;
  linkService: PDFLinkService;
  findController: PDFFindController;
  viewer: PDFViewer;
  document: PDFDocumentProxy;
};

type FindCount = { current: number; total: number };
type PasswordChallenge = {
  incorrect: boolean;
  submit: (password: string) => void;
};

const HIGHLIGHT_COLORS = ["#facc15", "#4ade80", "#60a5fa", "#f472b6"] as const;

/** Zoom presets offered by the zoom menu, in display order. */
const SCALE_PRESETS: ReadonlyArray<{ labelKey: MessageKey; value: string }> = [
  { labelKey: "components.pdfReader.fitWidth", value: "page-width" },
  { labelKey: "components.pdfReader.fitPage", value: "page-fit" },
  { labelKey: "components.pdfReader.actualSize", value: "page-actual" },
];
const PDFJS_HIGHLIGHT_COLORS = "yellow=#facc15,green=#4ade80,blue=#60a5fa,pink=#f472b6";

export function PdfReader({ file, url, onClose }: PdfReaderProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const viewerRef = useRef<HTMLDivElement>(null);
  const runtimeRef = useRef<PdfRuntime | null>(null);
  const closeRef = useRef(onClose);
  const drawerState = useOverlayState();
  const { t } = useI18n();

  const initialPage = 1;
  const initialScaleValue = "page-width";
  const initialRotation = 0;
  const initialSidebarOpen = true;
  const initialSidebarTab: SidebarTab = "thumbnails";

  const [document, setDocument] = useState<PDFDocumentProxy>();
  const [outline, setOutline] = useState<OutlineItem[]>([]);
  const [ready, setReady] = useState(false);
  const [error, setError] = useState<string>();
  const [loadingProgress, setLoadingProgress] = useState<number>();
  const [pageNumber, setPageNumber] = useState(initialPage);
  const [pageDraft, setPageDraft] = useState(String(initialPage));
  const [numPages, setNumPages] = useState(0);
  const [scale, setScale] = useState(1);
  const [scaleValue, setScaleValue] = useState(initialScaleValue);
  const [_rotation, setRotation] = useState(initialRotation);
  const [sidebarOpen, setSidebarOpen] = useState(initialSidebarOpen);
  const [sidebarTab, setSidebarTab] = useState<SidebarTab>(initialSidebarTab);
  const [searchOpen, setSearchOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [caseSensitive, setCaseSensitive] = useState(false);
  const [wholeWord, setWholeWord] = useState(false);
  const [findCount, setFindCount] = useState<FindCount>({ current: 0, total: 0 });
  const [findState, setFindState] = useState<number>(FindState.FOUND);
  const [annotationTool, setAnnotationTool] = useState<AnnotationTool>("select");
  const [annotationColor, setAnnotationColorState] = useState<(typeof HIGHLIGHT_COLORS)[number]>(
    HIGHLIGHT_COLORS[0],
  );
  const [saving, setSaving] = useState(false);
  const [passwordChallenge, setPasswordChallenge] = useState<PasswordChallenge>();
  const [passwordDraft, setPasswordDraft] = useState("");

  closeRef.current = onClose;

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
      loadingTask = getDocument({
        url,
        withCredentials: true,
        cMapUrl: "/pdfjs/cmaps/",
        standardFontDataUrl: "/pdfjs/standard_fonts/",
        wasmUrl: "/pdfjs/wasm/",
        iccUrl: "/pdfjs/iccs/",
      });
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

      const pagesInitialized = new Promise<void>((resolve) => {
        eventBus.on("pagesinit", () => resolve(), { once: true });
      });
      viewer.setDocument(pdf);
      await pagesInitialized;
      if (!active) return;

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
      viewer.cleanup();
      runtimeRef.current = null;
      setDocument(undefined);
      if (loadingTask) void loadingTask.destroy();
    };
  }, [file.id, url]);

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

  useEffect(() => {
    if (!ready || !searchOpen) return;
    const timer = window.setTimeout(() => dispatchFind(""), 180);
    return () => window.clearTimeout(timer);
  }, [dispatchFind, ready, searchOpen]);

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "f") {
        event.preventDefault();
        event.stopPropagation();
        setSearchOpen(true);
        return;
      }
      if (event.key === "Escape" && searchOpen) {
        event.preventDefault();
        event.stopPropagation();
        setSearchOpen(false);
        setQuery("");
        runtimeRef.current?.eventBus.dispatch("findbarclose", { source: containerRef.current });
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

  const commitPageDraft = () => {
    const next = Number(pageDraft);
    if (Number.isFinite(next)) goToPage(next);
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
    setAnnotationTool(tool);
    viewer.annotationEditorMode = { mode: annotationEditorMode(tool) };
    if (tool !== "select") setAnnotationColor(annotationColor, tool);
  };

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
            className="lg:hidden"
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
        {sidebarOpen ? (
          <aside className="hidden w-64 shrink-0 border-r border-border bg-surface/80 lg:block xl:w-72">
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
              {Array.from({ length: document.numPages }, (_, index) => index + 1).map((page) => (
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

// One IntersectionObserver serves every thumbnail. The sidebar renders a tile for
// each page, so a long document used to mount one observer per page, and that
// registration cost dominated opening the panel. Entries stay observed, because a
// thumbnail has to notice when it scrolls back into view.
const thumbnailCallbacks = new WeakMap<Element, (visible: boolean) => void>();
let thumbnailObserver: IntersectionObserver | undefined;

function observeThumbnail(element: Element, onVisibilityChange: (visible: boolean) => void) {
  if (typeof IntersectionObserver === "undefined") {
    onVisibilityChange(true);
    return () => {};
  }
  thumbnailObserver ??= new IntersectionObserver(
    (entries) => {
      for (const entry of entries) {
        thumbnailCallbacks.get(entry.target)?.(entry.isIntersecting);
      }
    },
    { rootMargin: "300px" },
  );
  thumbnailCallbacks.set(element, onVisibilityChange);
  thumbnailObserver.observe(element);
  return () => {
    thumbnailCallbacks.delete(element);
    thumbnailObserver?.unobserve(element);
  };
}

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
  const [ratio, setRatio] = useState(1.294);
  const [loaded, setLoaded] = useState(false);

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;
    return observeThumbnail(host, setVisible);
  }, []);

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

function SidebarEmpty({ label }: { label: string }) {
  return <p className="px-3 py-8 text-center text-xs text-muted">{label}</p>;
}

function annotationEditorMode(tool: AnnotationTool) {
  if (tool === "highlight") return AnnotationEditorType.HIGHLIGHT;
  if (tool === "text") return AnnotationEditorType.FREETEXT;
  if (tool === "ink") return AnnotationEditorType.INK;
  return AnnotationEditorType.NONE;
}

function normalizedRotation(value: unknown) {
  const number = typeof value === "number" && Number.isFinite(value) ? value : 0;
  return (((Math.round(number / 90) * 90) % 360) + 360) % 360;
}

function positiveInt(value: unknown, fallback: number) {
  const number = typeof value === "number" ? value : Number(value);
  return Number.isFinite(number) && number > 0 ? Math.round(number) : fallback;
}

function positiveNumber(value: unknown, fallback: number) {
  return typeof value === "number" && Number.isFinite(value) && value > 0 ? value : fallback;
}

function outlineDestinationKey(item: OutlineItem) {
  if (item.url) return item.url;
  if (typeof item.dest === "string") return item.dest;
  return String(item.dest ?? "section");
}

function editedPdfName(name: string) {
  const index = name.toLowerCase().lastIndexOf(".pdf");
  return index >= 0 ? `${name.slice(0, index)}-edited.pdf` : `${name}-edited.pdf`;
}

function isEditableTarget(target: EventTarget | null) {
  return (
    target instanceof HTMLInputElement ||
    target instanceof HTMLTextAreaElement ||
    target instanceof HTMLSelectElement ||
    (target instanceof HTMLElement && target.isContentEditable)
  );
}

function formatBytes(value: number) {
  if (!value) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const index = Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1);
  return `${(value / 1024 ** index).toFixed(index === 0 ? 0 : 1)} ${units[index]}`;
}
