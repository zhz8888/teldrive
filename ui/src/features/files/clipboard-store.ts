import { create } from "zustand";
import type { FileEntry } from "@/api/types";

/** Which operation a paste performs on the entries held in the clipboard. */
export type FileClipboardMode = "copy" | "cut";
/** Split-view pane an entry was copied from, so a paste can target the other pane. */
export type FileClipboardPane = "primary" | "secondary";

/** Clipboard contents plus the origin metadata a paste needs to resolve its target. */
type FileClipboardState = {
  /** Undefined while the clipboard is empty, which is what disables paste actions. */
  mode?: FileClipboardMode;
  /** Entries captured when the user cut or copied; stored as a snapshot, not a live list. */
  items: FileEntry[];
  /** Folder the entries were taken from, used to keep a cut/paste inside it a no-op. */
  sourceParentId?: string;
  /** Path of the source folder, shown as the clipboard's origin. */
  sourcePath?: string;
  /** Pane the entries were taken from; a paste into the other pane stays available. */
  sourcePane?: FileClipboardPane;
  /** Replaces the clipboard contents; the entries are copied into a fresh array. */
  set: (
    mode: FileClipboardMode,
    items: FileEntry[],
    sourceParentId?: string,
    sourcePath?: string,
    sourcePane?: FileClipboardPane,
  ) => void;
  /** Empties the clipboard, clearing every origin field along with the mode. */
  clear: () => void;
};

/** In-memory (never persisted) cut/copy buffer shared by both file-manager panes. */
export const useFileClipboardStore = create<FileClipboardState>((set) => ({
  items: [],
  set: (mode, items, sourceParentId, sourcePath, sourcePane) =>
    set({ mode, items: [...items], sourceParentId, sourcePath, sourcePane }),
  clear: () =>
    set({
      mode: undefined,
      items: [],
      sourceParentId: undefined,
      sourcePath: undefined,
      sourcePane: undefined,
    }),
}));
