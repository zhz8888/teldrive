import type { FileEntry } from "@/api/types";

// Extensions rendered with syntax highlighting by the code viewer; "dockerfile" is listed
// because it is matched as a bare name, without a dot.
const previewExtensions = new Set([
  "bash",
  "c",
  "cc",
  "cpp",
  "cs",
  "css",
  "dockerfile",
  "go",
  "graphql",
  "h",
  "hpp",
  "html",
  "java",
  "js",
  "json",
  "jsx",
  "kt",
  "kts",
  "md",
  "php",
  "py",
  "rb",
  "rs",
  "scss",
  "sh",
  "sql",
  "swift",
  "toml",
  "ts",
  "tsx",
  "vue",
  "xml",
  "yaml",
  "yml",
]);

// Fallback MIME types for files whose stored mimeType is missing or does not start with
// image/, video/ or audio/, so the extension is what decides the media type.
const mediaTypes: Record<string, string> = {
  aac: "audio/aac",
  avif: "image/avif",
  bmp: "image/bmp",
  flac: "audio/flac",
  gif: "image/gif",
  ico: "image/x-icon",
  jpeg: "image/jpeg",
  jpg: "image/jpeg",
  m4a: "audio/mp4",
  m4v: "video/x-m4v",
  mkv: "video/x-matroska",
  mov: "video/quicktime",
  mp3: "audio/mpeg",
  mp4: "video/mp4",
  oga: "audio/ogg",
  ogg: "audio/ogg",
  ogv: "video/ogg",
  opus: "audio/ogg",
  png: "image/png",
  svg: "image/svg+xml",
  wav: "audio/wav",
  webm: "video/webm",
  webp: "image/webp",
};

/** Top-level media family the inline preview should render the file with. */
export type PreviewMediaKind = "image" | "video" | "audio";

/**
 * Resolves the media kind and MIME type an inline `<img>`/`<video>`/`<audio>` needs,
 * preferring the stored mimeType and otherwise deriving the type from the file extension.
 * Returns undefined for anything that is not image, video or audio.
 */
export function previewMedia(
  file: FileEntry,
): { kind: PreviewMediaKind; type: string } | undefined {
  const mime = file.mimeType?.toLowerCase() || "";
  for (const kind of ["image", "video", "audio"] as const) {
    if (mime.startsWith(`${kind}/`)) return { kind, type: mime };
  }
  const extension = file.name.toLowerCase().split(".").pop() || "";
  const type = mediaTypes[extension];
  if (!type) return undefined;
  return { kind: type.split("/", 1)[0] as PreviewMediaKind, type };
}

/**
 * True when the file should open in the code/text viewer: only active files (never folders
 * or trashed entries) whose extension or MIME type is recognised as source or markup.
 */
export function supportsCodePreview(file: FileEntry) {
  if (file.kind !== "file" || file.status !== "active") return false;
  const lower = file.name.toLowerCase();
  const extension = lower === "dockerfile" ? "dockerfile" : lower.split(".").pop() || "";
  const mime = file.mimeType?.toLowerCase() || "";
  return (
    previewExtensions.has(extension) ||
    mime.startsWith("text/") ||
    mime.includes("json") ||
    mime.includes("javascript") ||
    mime.includes("typescript") ||
    mime.includes("xml") ||
    mime.includes("yaml")
  );
}
