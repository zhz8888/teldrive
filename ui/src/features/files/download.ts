import type { FileEntry } from "@/api/types";

/**
 * Builds the API path that serves a file's bytes; `download` appends the query flag that
 * makes the server send a Content-Disposition attachment instead of inline content.
 */
export function fileContentUrl(file: Pick<FileEntry, "id" | "name">, download = false) {
  const path = `/api/v1/files/${encodeURIComponent(file.id)}/content/${encodeURIComponent(file.name)}`;
  return download ? `${path}?download=1` : path;
}

/** Triggers a browser download of one file through a synthetic anchor click. */
export function startFileDownload(file: Pick<FileEntry, "id" | "name">) {
  const anchor = document.createElement("a");
  anchor.href = fileContentUrl(file, true);
  anchor.download = file.name;
  anchor.click();
}

/**
 * Copies text to the clipboard, falling back to a hidden textarea plus
 * `document.execCommand("copy")` when the async Clipboard API is unavailable (it needs a
 * secure context, which fails on plain-HTTP host/IP deployments). Rejects when both paths
 * fail, so callers can surface an error.
 */
export async function copyText(value: string) {
  try {
    await navigator.clipboard.writeText(value);
    return;
  } catch {
    // Clipboard API requires a secure context; the textarea path also works on HTTP IP hosts.
  }

  const activeElement =
    document.activeElement instanceof HTMLElement ? document.activeElement : undefined;
  const textarea = document.createElement("textarea");
  textarea.value = value;
  textarea.setAttribute("readonly", "");
  textarea.style.position = "fixed";
  textarea.style.opacity = "0";
  document.body.append(textarea);
  textarea.select();
  const copied = document.execCommand("copy");
  textarea.remove();
  activeElement?.focus();
  if (!copied) throw new Error("Clipboard access is unavailable");
}

/** Absolute form of {@link fileContentUrl} for sharing outside the current origin. */
export function absoluteFileDownloadUrl(file: Pick<FileEntry, "id" | "name">) {
  return new URL(fileContentUrl(file, true), window.location.origin).toString();
}
