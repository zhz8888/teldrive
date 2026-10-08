import FileIcon from "~icons/gravity-ui/file";
import AudioFileIcon from "~icons/icon-park-outline/audio-file";
import BookIcon from "~icons/icon-park-outline/book-one";
import FileCodeIcon from "~icons/icon-park-outline/file-code";
import FileExcelIcon from "~icons/icon-park-outline/file-excel";
import FilePdfIcon from "~icons/icon-park-outline/file-pdf";
import FilePptIcon from "~icons/icon-park-outline/file-ppt";
import FileTextIcon from "~icons/icon-park-outline/file-text";
import FileWordIcon from "~icons/icon-park-outline/file-word";
import FileZipIcon from "~icons/icon-park-outline/file-zip";
import PictureIcon from "~icons/icon-park-outline/picture-one";
import VideoFileIcon from "~icons/icon-park-outline/video-file";

/** Minimal shape of the entry the icon needs; both fields are matched case-insensitively. */
export type FileTypeLike = {
  /** File name, used for the extension fallback when the MIME type is unhelpful. */
  name: string;
  /** Server-reported MIME type; absent or null when the server has none. */
  mimeType?: string | null;
};

// Extension lists below are dot-prefixed because they are matched with `name.endsWith`.
const ebookExtensions = [".epub", ".mobi", ".azw", ".azw3", ".fb2", ".fbz", ".cbz", ".cbr"];

// Archives have no MIME-type check; only the extension decides.
const archiveExtensions = [
  ".zip",
  ".rar",
  ".7z",
  ".tar",
  ".gz",
  ".tgz",
  ".bz2",
  ".xz",
];

// Office document families; each is also recognised by a MIME-type substring.
const wordExtensions = [".doc", ".docx", ".odt", ".rtf"];
/** Spreadsheet extensions; `text/csv` is matched as a MIME type instead. */
const sheetExtensions = [".xls", ".xlsx", ".ods", ".csv"];
/** Presentation extensions, including the Keynote one. */
const slideExtensions = [".ppt", ".pptx", ".odp", ".key"];

/** Image extensions, used when the MIME type does not start with `image/`. */
const imageExtensions = [
  ".png",
  ".jpg",
  ".jpeg",
  ".gif",
  ".svg",
  ".webp",
  ".avif",
  ".bmp",
  ".ico",
  ".heic",
  ".heif",
];

/** Video extensions, used when the MIME type does not start with `video/`. */
const videoExtensions = [".mp4", ".mov", ".avi", ".mkv", ".webm", ".m4v", ".ogv"];
/** Audio extensions, checked only after `audio/` MIME types have been ruled out. */
const audioExtensions = [".mp3", ".wav", ".flac", ".ogg", ".oga", ".opus", ".m4a", ".aac"];

// Source extensions checked against `extensionOf`, so they carry no leading dot;
// "dockerfile" is a whole file name rather than an extension.
const codeExtensions = new Set([
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

/** Plain-text extensions used when the MIME type does not start with `text/`. */
const textExtensions = [".txt", ".md", ".mdx", ".log"];

/**
 * Lowercase extension of a file name without the dot; a name with no dot yields an empty
 * string, except `dockerfile`, which is reported as its own "extension".
 */
function extensionOf(name: string) {
  const lower = name.toLowerCase();
  if (lower === "dockerfile") return "dockerfile";
  const dot = lower.lastIndexOf(".");
  return dot >= 0 ? lower.slice(dot + 1) : "";
}

/** True when the (already lowercased) name ends with any of the dot-prefixed extensions. */
function endsWithAny(name: string, extensions: string[]) {
  return extensions.some((extension) => name.endsWith(extension));
}

// Per-type file icons, following the same idea as OpenChamber's
// FileTypeIcon: resolve by mime type first, file extension second,
// with a generic file icon as fallback.
export function FileTypeIcon({ file, className }: { file: FileTypeLike; className?: string }) {
  const name = file.name.toLowerCase();
  const mime = (file.mimeType || "").toLowerCase();
  const extension = extensionOf(file.name);

  if (mime === "application/pdf" || name.endsWith(".pdf"))
    return <FilePdfIcon className={className} />;
  if (
    mime.includes("epub") ||
    mime.includes("mobipocket") ||
    mime.includes("fictionbook") ||
    mime.includes("comicbook") ||
    endsWithAny(name, ebookExtensions)
  )
    return <BookIcon className={className} />;
  if (mime.startsWith("image/") || endsWithAny(name, imageExtensions))
    return <PictureIcon className={className} />;
  if (mime.startsWith("video/") || endsWithAny(name, videoExtensions))
    return <VideoFileIcon className={className} />;
  if (mime.startsWith("audio/") || endsWithAny(name, audioExtensions))
    return <AudioFileIcon className={className} />;
  if (endsWithAny(name, archiveExtensions)) return <FileZipIcon className={className} />;
  if (
    mime.includes("wordprocessing") ||
    mime.includes("msword") ||
    mime.includes("wordprocessingml") ||
    endsWithAny(name, wordExtensions)
  )
    return <FileWordIcon className={className} />;
  if (mime.includes("spreadsheet") || mime === "text/csv" || endsWithAny(name, sheetExtensions))
    return <FileExcelIcon className={className} />;
  if (mime.includes("presentation") || endsWithAny(name, slideExtensions))
    return <FilePptIcon className={className} />;
  if (
    codeExtensions.has(extension) ||
    mime.includes("json") ||
    mime.includes("javascript") ||
    mime.includes("typescript") ||
    mime.includes("xml") ||
    mime.includes("yaml")
  )
    return <FileCodeIcon className={className} />;
  if (mime.startsWith("text/") || endsWithAny(name, textExtensions))
    return <FileTextIcon className={className} />;
  return <FileIcon className={className} />;
}
