import type { FileEntry } from "@/api/types";

// Ebook and comic extensions foliate-js can parse; the MIME checks in `readerKind` also
// accept a file whose name carries none of them.
const ebookExtensions = [".epub", ".mobi", ".azw", ".azw3", ".fb2", ".fbz", ".cbz"];

/** Which in-app reader renders a file: the foliate ebook viewer or the PDF viewer. */
export type ReaderKind = "ebook" | "pdf";

/**
 * Classifies a file for the in-app reader by MIME type first, then by file-name
 * extension; returns undefined when neither viewer can open it.
 */
export function readerKind(file: FileEntry): ReaderKind | undefined {
  const name = file.name.toLowerCase();
  const mime = (file.mimeType || "").toLowerCase();
  if (mime === "application/pdf" || name.endsWith(".pdf")) return "pdf";
  if (
    mime.includes("epub") ||
    mime.includes("mobipocket") ||
    mime.includes("fictionbook") ||
    mime.includes("comicbook") ||
    ebookExtensions.some((extension) => name.endsWith(extension))
  ) {
    return "ebook";
  }
  return undefined;
}

/** True when one of the in-app readers can render the file. */
export function supportsReader(file: FileEntry) {
  return readerKind(file) !== undefined;
}
