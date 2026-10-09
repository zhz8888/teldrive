import { Button, cn, Modal, Spinner } from "@heroui/react";
import { lazy, Suspense, useEffect, useRef, useState } from "react";
import { normalizeApiError, userMessage } from "@/api/errors";
import type { FileEntry } from "@/api/types";
import { fileContentUrl, startFileDownload } from "@/features/files/download";
import { previewMedia, supportsCodePreview } from "@/features/files/preview-support";
import { readerKind } from "@/features/files/reader-support";
/** The preview surfaces the dialog can choose from for a file. */
type ViewerKind = "image" | "video" | "audio" | "pdf" | "ebook" | "text";
import { type MessageKey, useI18n } from "@/lib/i18n";
import DownloadIcon from "~icons/gravity-ui/arrow-down-to-line";
import RotateIcon from "~icons/gravity-ui/arrow-rotate-left";
import ZoomOutIcon from "~icons/gravity-ui/magnifier-minus";
import ZoomInIcon from "~icons/gravity-ui/magnifier-plus";
import CloseIcon from "~icons/gravity-ui/xmark";

// The three heavy viewers are loaded on demand: the player and the two reader
// engines would otherwise be part of the bundle every page loads.
/** Lazily loaded video player. */
const VideoViewer = lazy(() =>
  import("@/components/viewers/video-viewer").then((module) => ({ default: module.VideoViewer })),
);
/** Lazily loaded pdf.js reader. */
const PdfReader = lazy(() =>
  import("@/components/viewers/pdf-reader").then((module) => ({ default: module.PdfReader })),
);
/** Lazily loaded foliate reader for EPUB files. */
const EpubReader = lazy(() =>
  import("@/components/viewers/epub-reader").then((module) => ({ default: module.EpubReader })),
);

/**
 * Full-screen preview of one file. `file` selects the surface through
 * {@link viewerKind}; without a file, or for a type nothing can render, the
 * dialog renders nothing at all and the caller's `onOpenChange` is not called.
 */
export function FilePreviewDialog({
  file,
  onOpenChange,
}: {
  file?: FileEntry;
  onOpenChange: (open: boolean) => void;
}) {
  // Closing a reader is deferred by two animation frames, and the id of the
  // pending frame is held here so a second close request cancels the first
  // instead of reporting the close twice.
  const closeFrame = useRef<number>(undefined);
  const { t } = useI18n();
  const contentUrl = file ? fileContentUrl(file) : "";
  const kind = file ? viewerKind(file) : undefined;
  const isReader = kind === "pdf" || kind === "ebook";

  if (!file || !kind) return null;

  // Closing a reader is deferred by two animation frames: unmounting one
  // destroys a rendering engine, and doing that inside the dismiss event tears
  // it down mid-render. An opening close, or a plain viewer, passes through.
  const changeOpen = (open: boolean) => {
    if (open || !isReader) {
      onOpenChange(open);
      return;
    }
    cancelAnimationFrame(closeFrame.current || 0);
    closeFrame.current = requestAnimationFrame(() => {
      closeFrame.current = requestAnimationFrame(() => onOpenChange(false));
    });
  };

  if (kind === "pdf") {
    return (
      <Modal.Backdrop
        isOpen
        onOpenChange={changeOpen}
        isDismissable
        variant="opaque"
        data-content-viewer
        className="bg-background"
      >
        <Modal.Container size="full" scroll="inside" className="h-dvh max-h-dvh p-0">
          <Modal.Dialog className="h-dvh max-h-dvh w-screen max-w-none overflow-hidden rounded-none bg-background p-0 text-foreground">
            <Modal.Heading className="sr-only">{file.name}</Modal.Heading>
            <Suspense
              fallback={<ViewerLoading label={t("components.filePreview.loadingEngine")} />}
            >
              <PdfReader
                key={file.id}
                file={file}
                url={contentUrl}
                onClose={() => changeOpen(false)}
              />
            </Suspense>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    );
  }

  if (kind === "ebook") {
    return (
      <Modal.Backdrop
        isOpen
        isDismissable={false}
        variant="opaque"
        data-content-viewer
        className="bg-background"
      >
        <Modal.Container size="full" scroll="inside" className="h-dvh max-h-dvh p-0">
          <Modal.Dialog className="h-dvh max-h-dvh w-screen max-w-none overflow-hidden rounded-none bg-background p-0 text-foreground">
            <Modal.Heading className="sr-only">{file.name}</Modal.Heading>
            <Suspense fallback={<ViewerLoading label={t("components.filePreview.loadingEpub")} />}>
              <EpubReader
                key={file.id}
                file={file}
                url={contentUrl}
                onClose={() => changeOpen(false)}
              />
            </Suspense>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    );
  }

  return (
    <Modal.Backdrop
      isOpen
      onOpenChange={changeOpen}
      isDismissable
      variant="opaque"
      data-content-viewer
      className="bg-background"
    >
      <Modal.Container size="full" scroll="inside" className="h-dvh max-h-dvh p-0">
        <Modal.Dialog className="flex h-dvh max-h-dvh w-screen max-w-none flex-col overflow-hidden rounded-none bg-background text-foreground">
          <Modal.Header
            data-reader-header={isReader || undefined}
            className={cn(
              "relative z-30 flex h-16 shrink-0 flex-row items-center gap-3 border-x-0 border-t-0 px-3 py-0 sm:px-5",
              isReader ? "border-b border-border bg-surface/90 backdrop-blur-xl" : "glass-panel",
            )}
          >
            <Button
              isIconOnly
              variant="ghost"
              size="sm"
              aria-label={t("components.filePreview.close")}
              onPress={() => changeOpen(false)}
            >
              <CloseIcon className="size-5" />
            </Button>
            <div className="min-w-0 flex-1">
              <Modal.Heading className="truncate text-sm font-semibold tracking-[-0.01em]">
                {file.name}
              </Modal.Heading>
              <p className="truncate text-[11px] text-muted">
                {t(formatLabelKey(kind))} · {formatBytes(file.size || 0)}
              </p>
            </div>
            <Button
              variant={isReader ? "ghost" : "secondary"}
              size="sm"
              onPress={() => startFileDownload(file)}
            >
              <DownloadIcon className="size-4" />
              <span className="hidden sm:inline">{t("common.action.download")}</span>
            </Button>
          </Modal.Header>
          <Modal.Body
            className={cn(
              "relative min-h-0 flex-1 overflow-hidden p-0",
              isReader
                ? "bg-background"
                : "bg-[radial-gradient(circle_at_50%_20%,color-mix(in_oklch,var(--muted-background)_70%,transparent),var(--background)_65%)]",
            )}
          >
            {kind === "image" ? <ImageViewer file={file} url={contentUrl} /> : null}
            {kind === "video" ? (
              <Suspense
                fallback={<ViewerLoading label={t("components.filePreview.loadingVideo")} />}
              >
                <VideoViewer file={file} url={contentUrl} />
              </Suspense>
            ) : null}
            {kind === "audio" ? <AudioViewer file={file} url={contentUrl} /> : null}
            {kind === "text" ? <TextViewer url={contentUrl} /> : null}
          </Modal.Body>
        </Modal.Dialog>
      </Modal.Container>
    </Modal.Backdrop>
  );
}

/** Image surface with zoom in 25 % steps (0.25x-5x) and 90° rotation. */
function ImageViewer({ file, url }: { file: FileEntry; url: string }) {
  const { t } = useI18n();
  const [zoom, setZoom] = useState(1);
  const [rotation, setRotation] = useState(0);
  return (
    <div className="relative flex h-full items-center justify-center overflow-auto p-5 sm:p-10">
      <img
        src={url}
        alt={file.name}
        draggable={false}
        className="max-h-full max-w-full select-none object-contain shadow-2xl transition-transform duration-200"
        style={{ transform: `scale(${zoom}) rotate(${rotation}deg)` }}
      />
      <div className="glass-panel absolute bottom-4 left-1/2 flex -translate-x-1/2 items-center gap-1 rounded-full p-1.5">
        <Button
          isIconOnly
          size="sm"
          variant="ghost"
          aria-label={t("components.filePreview.zoomOut")}
          onPress={() => setZoom((value) => Math.max(0.25, value - 0.25))}
        >
          <ZoomOutIcon className="size-4" />
        </Button>
        <Button size="sm" variant="ghost" onPress={() => setZoom(1)}>
          {Math.round(zoom * 100)}%
        </Button>
        <Button
          isIconOnly
          size="sm"
          variant="ghost"
          aria-label={t("components.filePreview.zoomIn")}
          onPress={() => setZoom((value) => Math.min(5, value + 0.25))}
        >
          <ZoomInIcon className="size-4" />
        </Button>
        <Button
          isIconOnly
          size="sm"
          variant="ghost"
          aria-label={t("components.filePreview.rotate")}
          onPress={() => setRotation((value) => value + 90)}
        >
          <RotateIcon className="size-4" />
        </Button>
      </div>
    </div>
  );
}

/** Audio surface: the browser's own transport under the file name. */
function AudioViewer({ file, url }: { file: FileEntry; url: string }) {
  return (
    <div className="flex h-full items-center justify-center p-6">
      <div className="glass-panel w-full max-w-xl rounded-3xl p-8 text-center">
        <div className="mx-auto mb-6 grid size-28 place-items-center rounded-full border border-accent/20 bg-accent/10 text-4xl">
          ♪
        </div>
        <h3 className="truncate text-lg font-semibold">{file.name}</h3>
        {/* biome-ignore lint/a11y/useMediaCaption: user-provided audio does not have a separate caption resource */}
        <audio className="mt-7 w-full" src={url} controls />
      </div>
    </div>
  );
}

/** Leading bytes of a text file the preview reads; the rest of it stays unread. */
const TEXT_PREVIEW_BYTES = 1_048_576;

/**
 * Reads the leading bytes of `url` as UTF-8 text. The request asks for exactly
 * that byte range, and a server that ignores the header and answers 200 with the
 * whole body is cut off client-side once the same number of bytes has arrived,
 * so a gigabyte log cannot fill the tab's memory before the preview truncates it.
 */
async function fetchTextPreview(url: string, signal: AbortSignal) {
  const response = await fetch(url, {
    signal,
    headers: { Range: `bytes=0-${TEXT_PREVIEW_BYTES - 1}` },
  });
  // A range that starts at byte 0 is only unsatisfiable for a file that has no
  // bytes at all, and an empty file previews as empty rather than as an error.
  if (response.status === 416) return "";
  // A failed request answers with an error envelope; showing it as the
  // document body would make the reader look like the file's content.
  if (!response.ok) {
    const body = await response.json().catch(() => undefined);
    throw normalizeApiError(body, response);
  }
  const decoder = new TextDecoder("utf-8");
  if (!response.body) return decoder.decode(await response.arrayBuffer());
  const reader = response.body.getReader();
  let remaining = TEXT_PREVIEW_BYTES;
  let text = "";
  while (remaining > 0) {
    const { done, value } = await reader.read();
    // The response ended on its own, so the decoder may flush: a trailing
    // multi-byte character cannot have been cut in half by the preview limit.
    if (done) {
      text += decoder.decode();
      break;
    }
    const chunk = value.byteLength > remaining ? value.subarray(0, remaining) : value;
    remaining -= chunk.byteLength;
    // Decoding as a stream keeps a multi-byte character that a chunk boundary
    // splits, or that the limit cuts short, out of the text instead of rendering
    // it as U+FFFD: the bytes held back at the limit are simply dropped.
    text += decoder.decode(chunk, { stream: true });
    if (remaining === 0) await reader.cancel();
  }
  return text;
}

/**
 * Shows the file's opening bytes in a monospaced block, so a huge log cannot
 * lock up the tab.
 */
function TextViewer({ url }: { url: string }) {
  const { t } = useI18n();
  const [text, setText] = useState<string>();
  const [error, setError] = useState<string>();
  useEffect(() => {
    const controller = new AbortController();
    void fetchTextPreview(url, controller.signal)
      .then(setText)
      .catch((reason: unknown) => {
        if (!controller.signal.aborted) setError(userMessage(reason));
      });
    return () => controller.abort();
  }, [url]);
  if (error) return <ViewerError message={error} />;
  if (text === undefined)
    return <ViewerLoading label={t("components.filePreview.loadingDocument")} />;
  return (
    <div className="h-full overflow-auto p-4 sm:p-8">
      <pre className="mx-auto min-h-full max-w-5xl whitespace-pre-wrap rounded-2xl border border-border bg-surface p-5 font-mono text-xs leading-6 shadow-xl sm:p-8">
        {text}
      </pre>
    </div>
  );
}
/** Centred spinner shown while a viewer or its engine is still loading. */
function ViewerLoading({ label }: { label: string }) {
  return (
    <div className="grid h-full place-items-center">
      <div className="text-center">
        <Spinner size="lg" aria-label={label} />
        <p className="mt-3 text-xs text-muted">{label}</p>
      </div>
    </div>
  );
}
/** Failure surface for a viewer that could not load its content. */
function ViewerError({ message }: { message: string }) {
  const { t } = useI18n();
  return (
    <div className="grid h-full place-items-center p-6 text-center">
      <div>
        <p className="font-semibold">{t("components.filePreview.errorTitle")}</p>
        <p className="mt-2 max-w-lg text-sm text-muted">{message}</p>
      </div>
    </div>
  );
}
/**
 * Picks the surface for a file: the reader kind wins where the readers apply,
 * then the media kind, then the code/text preview. Undefined means nothing can
 * render the file.
 */
function viewerKind(file: FileEntry): ViewerKind | undefined {
  const reader = readerKind(file);
  if (reader) return reader;
  const media = previewMedia(file);
  if (media) return media.kind;
  if (supportsCodePreview(file)) return "text";
}
/** Catalog key of the label shown next to the file size in the header. */
const KIND_LABEL_KEYS = {
  image: "components.filePreview.kindImage",
  video: "components.filePreview.kindVideo",
  audio: "components.filePreview.kindAudio",
  pdf: "components.filePreview.kindPdf",
  ebook: "components.filePreview.kindEbook",
  text: "components.filePreview.kindText",
} as const;

/** Maps a viewer kind to its catalog key; the map covers every kind. */
function formatLabelKey(kind: ViewerKind): MessageKey {
  return KIND_LABEL_KEYS[kind];
}
/** Formats a byte count in binary units; a falsy value renders as "0 B". */
function formatBytes(value: number) {
  if (!value) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const index = Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1);
  return `${(value / 1024 ** index).toFixed(index === 0 ? 0 : 1)} ${units[index]}`;
}

/** Whether the preview dialog has a surface for this file. */
export function isPreviewable(file: FileEntry) {
  return viewerKind(file) !== undefined;
}
