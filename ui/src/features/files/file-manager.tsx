import { Button, Dropdown, Input, Label, TextField } from "@heroui/react";
import { useQuery } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { useKeyboard } from "react-aria/useKeyboard";
import { DropZone, FileTrigger, type Selection } from "react-aria-components";
import { toast } from "sonner";
import { normalizeApiError, userMessage } from "@/api/errors";
import type { FileEntry } from "@/api/types";
import { currentUserQueryOptions } from "@/auth/queries";
import { BackgroundUploadDialog } from "@/components/background-upload-dialog";
import { AppDialog } from "@/components/dialogs/app-dialog";
import { isPreviewable } from "@/components/file-preview-dialog";
import { Page, PageContent } from "@/components/page";
import { resolveFolderIdByPath, useUploadStore } from "@/features/uploads/store";
import { useI18n } from "@/lib/i18n";
import PasteIcon from "~icons/gravity-ui/arrow-right-to-square";
import UploadIcon from "~icons/gravity-ui/arrow-up-from-line";
import FileIcon from "~icons/gravity-ui/file";
import FolderIcon from "~icons/gravity-ui/folder";
import SplitIcon from "~icons/gravity-ui/layout-split-columns";
import PlusIcon from "~icons/gravity-ui/plus";
import CloseIcon from "~icons/gravity-ui/xmark";

import { useFileClipboardStore } from "./clipboard-store";
import { absoluteFileDownloadUrl, copyText, startFileDownload } from "./download";
import { FileActionDialogs } from "./file-action-dialogs";
import { FileBrowser } from "./file-browser";
import { FileSelectionToolbar } from "./file-selection-toolbar";
import { useFileActions } from "./mutations";
import { useInfiniteFilePages } from "./queries";
import { SearchControls } from "./search-controls";
import {
  driveSearchOptions,
  hasSearchCriteria,
  invalidSearchDates,
  type SearchState,
} from "./search-state";

/** Layout of a pane's entry list; the same two values the route accepts. */
type FileBrowserView = "list" | "grid";
/** Which of the two panes an action applies to; both are shown in the split view. */
type PaneId = "primary" | "secondary";

/** Route state of one pane; the secondary pane mirrors the primary one when unsplit. */
type PaneLocation = {
  /** Path the pane lists, or the folder a search was scoped to. */
  path: string;
  /** Id of the folder at `path`; absent at the drive root or before the path is resolved. */
  parentId?: string;
  /** Text the pane filters its listing by. */
  query: string;
  /** Layout of this pane; the two panes may use different layouts. */
  view: FileBrowserView;
};

/** Everything the route encodes about the file manager, split view included. */
export type FilesLocation = PaneLocation & {
  /** Whether the second pane is shown at all. */
  split?: boolean;
  /** Path of the secondary pane; only read while `split` is set. */
  secondaryPath?: string;
  /** Id of the folder the secondary pane lists. */
  secondaryParentId?: string;
  /** Search text of the secondary pane. */
  secondaryQuery?: string;
  /** Layout of the secondary pane; falls back to the primary pane's layout. */
  secondaryView?: FileBrowserView;
};

/**
 * The file manager both `/files` and `/search` render. Without `searchMode` it is
 * the drive browser: one or two panes, uploads, clipboard operations and folder
 * dialogs. With `searchMode` the same panes show drive-wide results, the folder
 * controls give way to the search controls, and the actions that would need a
 * source folder (cut, duplicate, paste) are not offered.
 *
 * The component owns no route state: the caller passes the validated search
 * parameters in `location` and receives every navigation through
 * `onLocationChange`, so the `/files` and `/search` routes stay the single place
 * that decides what a URL means.
 */
export function FileManagerPage({
  location: search,
  onLocationChange,
  searchMode,
}: {
  location: FilesLocation;
  onLocationChange: (location: FilesLocation, replace?: boolean) => void;
  searchMode?: {
    criteria: SearchState;
    onChange: (search: SearchState, replace?: boolean) => void;
  };
}) {
  const navigateTo = useNavigate();
  const { t } = useI18n();
  const navigate = ({ search: next, replace }: { search: FilesLocation; replace?: boolean }) =>
    onLocationChange(next, replace);
  const criteria = searchMode?.criteria;
  const missingFolder = criteria?.scope === "recursive" && !criteria.parentId;
  const invalidDates = criteria ? invalidSearchDates(criteria) : false;
  const activeSearch = criteria ? hasSearchCriteria(criteria) : true;
  /**
   * Whether the listing behind a pane is the one that pane can show right now. Both pane
   * queries stay mounted while their request is held back — the secondary one is unused in
   * search mode, and a search without usable criteria is not sent — so a state they kept
   * from an earlier key, an error included, must not be reported for the visible pane.
   */
  const paneQueryEnabled = (pane: PaneId) =>
    pane === "secondary"
      ? Boolean(search.split) && !searchMode
      : activeSearch && !missingFolder && !invalidDates;
  const [filtersOpen, setFiltersOpen] = useState(false);
  const { data: currentUser } = useQuery(currentUserQueryOptions());
  const canLocalImport = Boolean(currentUser?.capabilities.includes("system.localImport"));

  const primaryLocation: PaneLocation = {
    path: search.path,
    parentId: search.parentId,
    query: search.query,
    view: search.view,
  };
  const secondaryLocation: PaneLocation = search.secondaryPath
    ? {
        path: search.secondaryPath,
        parentId: search.secondaryParentId,
        query: search.secondaryQuery ?? "",
        view: search.secondaryView ?? search.view,
      }
    : primaryLocation;

  const [activePane, setActivePane] = useState<PaneId>("primary");
  const activePaneRef = useRef<PaneId>("primary");
  const [primarySelectedKeys, setPrimarySelectedKeys] = useState<Selection>(new Set());
  const [secondarySelectedKeys, setSecondarySelectedKeys] = useState<Selection>(new Set());
  const [folderDialogOpen, setFolderDialogOpen] = useState(false);
  const [backgroundUploadOpen, setBackgroundUploadOpen] = useState(false);
  const [folderName, setFolderName] = useState("");
  const [previewFile, setPreviewFile] = useState<FileEntry>();
  const [renameFile, setRenameFile] = useState<FileEntry>();
  const [renameName, setRenameName] = useState("");
  const [destination, setDestination] = useState<{
    mode: "move" | "copy";
    files: FileEntry[];
    pane: PaneId;
  }>();
  const [actionError, setActionError] = useState<string>();
  const operationLock = useRef(false);
  const [operationPending, setOperationPending] = useState(false);
  const [shareFile, setShareFile] = useState<FileEntry>();
  const [pasteConflictPane, setPasteConflictPane] = useState<PaneId>();
  const primaryUploadFilesTriggerRef = useRef<HTMLButtonElement>(null);
  const primaryUploadFolderTriggerRef = useRef<HTMLButtonElement>(null);
  const secondaryUploadFilesTriggerRef = useRef<HTMLButtonElement>(null);
  const secondaryUploadFolderTriggerRef = useRef<HTMLButtonElement>(null);
  const enqueue = useUploadStore((state) => state.enqueue);
  const fileActions = useFileActions();
  const pending = operationPending || fileActions.pending;

  const clipboardMode = useFileClipboardStore((state) => state.mode);
  const clipboardItems = useFileClipboardStore((state) => state.items);
  const clipboardSourceParentId = useFileClipboardStore((state) => state.sourceParentId);
  const clipboardSourcePath = useFileClipboardStore((state) => state.sourcePath);
  const clipboardSourcePane = useFileClipboardStore((state) => state.sourcePane);
  const setClipboard = useFileClipboardStore((state) => state.set);
  const clearClipboard = useFileClipboardStore((state) => state.clear);

  const primaryFileQuery = useInfiniteFilePages(
    {
      path: primaryLocation.path,
      parentId: primaryLocation.parentId,
      q: primaryLocation.query || undefined,
      sort: "name",
      order: "asc",
      view: primaryLocation.view,
      ...(criteria ? driveSearchOptions(criteria) : {}),
    },
    "active",
    paneQueryEnabled("primary"),
  );
  const secondaryFileQuery = useInfiniteFilePages(
    {
      path: secondaryLocation.path,
      parentId: secondaryLocation.parentId,
      q: secondaryLocation.query || undefined,
      sort: "name",
      order: "asc",
      view: secondaryLocation.view,
    },
    "active",
    paneQueryEnabled("secondary"),
  );
  const primaryFiles = primaryFileQuery.data?.pages.flatMap((page) => page.items) ?? [];
  const secondaryFiles = secondaryFileQuery.data?.pages.flatMap((page) => page.items) ?? [];

  const primaryCriteriaKey = JSON.stringify({
    path: primaryLocation.path,
    ...(criteria
      ? driveSearchOptions(criteria)
      : {
          parentId: primaryLocation.parentId,
          q: primaryLocation.query,
        }),
  });
  useEffect(() => setPrimarySelectedKeys(new Set()), [primaryCriteriaKey]);
  useEffect(
    () => setSecondarySelectedKeys(new Set()),
    [secondaryLocation.parentId, secondaryLocation.path, secondaryLocation.query],
  );
  useEffect(() => {
    if (!search.split) {
      activePaneRef.current = "primary";
      if (activePane === "secondary") setActivePane("primary");
    }
  }, [activePane, search.split]);

  const primarySelectedIds = selectionIds(primarySelectedKeys, primaryFiles);
  const secondarySelectedIds = selectionIds(secondarySelectedKeys, secondaryFiles);
  const primarySelectedFiles = primaryFiles.filter((file) => primarySelectedIds.includes(file.id));
  const secondarySelectedFiles = secondaryFiles.filter((file) =>
    secondarySelectedIds.includes(file.id),
  );
  const activeLocation =
    activePane === "secondary" && search.split ? secondaryLocation : primaryLocation;

  const cutIds =
    clipboardMode === "cut" ? new Set(clipboardItems.map((file) => file.id)) : undefined;
  const hasClipboard = Boolean(clipboardMode && clipboardItems.length > 0);

  /**
   * Whether a pane already shows the folder a cut came from. The path recorded
   * with the clipboard is authoritative, because breadcrumb navigation keeps no
   * folder id; the id is the fallback when no path was recorded.
   */
  const isClipboardSource = (location: PaneLocation) =>
    clipboardSourcePath !== undefined
      ? normalizeFolderPath(clipboardSourcePath) === normalizeFolderPath(location.path)
      : clipboardSourceParentId !== undefined && clipboardSourceParentId === location.parentId;

  /**
   * parentIdFor resolves the folder a location shows. A pane reached through the
   * breadcrumb or the parent shortcut carries only its path, and a request
   * without a parent writes to the drive root, so every write aimed at "the
   * folder on screen" has to resolve the path first. The result is undefined when
   * the path names no folder, which callers report instead of falling back to the
   * root.
   */
  const parentIdFor = async (location: PaneLocation) => {
    if (location.parentId !== undefined || location.path === "/") return location.parentId;
    return await resolveFolderIdByPath(location.path);
  };

  const paneLocation = (pane: PaneId) =>
    pane === "secondary" && search.split ? secondaryLocation : primaryLocation;
  const paneFiles = (pane: PaneId) =>
    pane === "secondary" && search.split ? secondaryFiles : primaryFiles;
  /** Listing behind a pane; the secondary pane is only rendered while the view is split. */
  const paneQuery = (pane: PaneId) =>
    pane === "secondary" && search.split ? secondaryFileQuery : primaryFileQuery;
  const paneSelectedKeys = (pane: PaneId) =>
    pane === "secondary" && search.split ? secondarySelectedKeys : primarySelectedKeys;
  const paneSelectedFiles = (pane: PaneId) =>
    pane === "secondary" && search.split ? secondarySelectedFiles : primarySelectedFiles;
  const setPaneSelectedKeys = (pane: PaneId, selection: Selection) => {
    if (searchMode && primaryFileQuery.isPlaceholderData) return;
    if (selection === "all") selection = new Set(paneFiles(pane).map((file) => file.id));
    activePaneRef.current = pane;
    if (pane === "secondary") setSecondarySelectedKeys(selection);
    else setPrimarySelectedKeys(selection);
  };

  const navigatePane = (pane: PaneId, location: PaneLocation, replace = false) => {
    if (pane === "secondary") {
      navigate({
        search: {
          ...search,
          split: true,
          secondaryPath: location.path,
          secondaryParentId: location.parentId,
          secondaryQuery: location.query,
          secondaryView: location.view,
        },
        replace,
      });
      return;
    }
    navigate({
      search: {
        ...search,
        path: location.path,
        parentId: location.parentId,
        query: location.query,
        view: location.view,
      },
      replace,
    });
  };

  const openSplitView = () => {
    if (search.split) return;
    setSecondarySelectedKeys(new Set());
    navigate({
      search: {
        ...search,
        split: true,
        secondaryPath: primaryLocation.path,
        secondaryParentId: primaryLocation.parentId,
        secondaryQuery: primaryLocation.query,
        secondaryView: primaryLocation.view,
      },
    });
  };

  const closeSplitView = () => {
    setActivePane("primary");
    setSecondarySelectedKeys(new Set());
    navigate({
      search: {
        path: primaryLocation.path,
        parentId: primaryLocation.parentId,
        query: primaryLocation.query,
        view: primaryLocation.view,
        split: false,
      },
    });
  };

  const createFolder = async () => {
    const name = folderName.trim();
    // The submit button is disabled while an action runs, and Enter has to be
    // guarded here too: without it a second keypress creates a second folder.
    if (!name || pending) return;
    const location = activeLocation;
    let parentId: string | undefined;
    try {
      parentId = await parentIdFor(location);
    } catch (error) {
      toast.error(t("routes.files.toast.folderCreateFailed"), { description: userMessage(error) });
      return;
    }
    if (parentId === undefined && location.path !== "/") {
      toast.error(t("routes.files.toast.folderCreateFailed"), {
        description: t("features.uploads.folderMissing", { path: location.path }),
      });
      return;
    }
    try {
      await fileActions.createFolder(name, parentId);
      setFolderName("");
      setFolderDialogOpen(false);
      toast.success(t("routes.files.toast.folderCreated"));
    } catch (error) {
      toast.error(t("routes.files.toast.folderCreateFailed"), { description: userMessage(error) });
    }
  };

  const performAction = async (
    operation: () => Promise<unknown>,
    successMessage: string,
    failureMessage: string,
    onSuccess: () => void,
    inlineError = false,
  ) => {
    if (operationLock.current) return;
    operationLock.current = true;
    setOperationPending(true);
    setActionError(undefined);
    try {
      await operation();
      onSuccess();
      toast.success(successMessage);
    } catch (error) {
      if (inlineError) setActionError(`${failureMessage}. ${userMessage(error)}`);
      else toast.error(failureMessage, { description: userMessage(error) });
    } finally {
      operationLock.current = false;
      setOperationPending(false);
    }
  };

  const trashFiles = async (ids: string[], pane: PaneId) => {
    if (ids.length === 0 || pending) return;
    await performAction(
      () => fileActions.bulkTrash(ids),
      t("routes.files.toast.trashed", { count: ids.length }),
      t("routes.files.toast.trashFailed"),
      () => setPaneSelectedKeys(pane, new Set()),
    );
  };

  const trashSelected = async (pane: PaneId) => {
    const files = paneSelectedFiles(pane);
    await trashFiles(
      files.map((file) => file.id),
      pane,
    );
  };

  const renameSelected = async () => {
    if (!renameFile || !renameName.trim() || pending) return;
    await performAction(
      () => fileActions.rename(renameFile, renameName.trim()),
      t("routes.files.toast.renamed"),
      t("routes.files.toast.renameFailed"),
      () => {
        setRenameFile(undefined);
        setRenameName("");
        setPaneSelectedKeys(activePane, new Set());
      },
      true,
    );
  };

  const duplicateFile = async (file: FileEntry, pane: PaneId) => {
    const location = paneLocation(pane);
    let parentId: string | undefined;
    try {
      parentId = await parentIdFor(location);
    } catch (error) {
      toast.error(t("routes.files.toast.duplicateFailed"), { description: userMessage(error) });
      return;
    }
    if (parentId === undefined && location.path !== "/") {
      toast.error(t("routes.files.toast.duplicateFailed"), {
        description: t("features.uploads.folderMissing", { path: location.path }),
      });
      return;
    }
    try {
      await fileActions.copy(file, parentId, `${file.name} copy`, "rename");
      setPaneSelectedKeys(pane, new Set());
      toast.success(t("routes.files.toast.duplicated"));
    } catch (error) {
      toast.error(t("routes.files.toast.duplicateFailed"), { description: userMessage(error) });
    }
  };

  const duplicateSelected = async (pane: PaneId) => {
    const selected = paneSelectedFiles(pane);
    if (selected.length === 1) await duplicateFile(selected[0], pane);
  };

  const transferSelected = async (parentId?: string) => {
    const target = destination;
    if (!target || target.files.length === 0 || pending) return;
    await performAction(
      async () => {
        if (target.mode === "copy") return fileActions.copyMany(target.files, parentId, "rename");
        if (target.files.length === 1) return fileActions.move(target.files[0], parentId, "rename");
        return fileActions.bulkMove(
          target.files.map((file) => file.id),
          parentId,
        );
      },
      t(target.mode === "copy" ? "routes.files.toast.copied" : "routes.files.toast.moved", {
        count: target.files.length,
      }),
      t(target.mode === "copy" ? "routes.files.toast.copyFailed" : "routes.files.toast.moveFailed"),
      () => {
        setDestination(undefined);
        setPaneSelectedKeys(target.pane, new Set());
      },
      true,
    );
  };

  const stageClipboard = (mode: "copy" | "cut", pane: PaneId) => {
    const files = paneSelectedFiles(pane);
    if (files.length === 0) return;
    if (searchMode && mode === "copy") {
      setActionError(undefined);
      setDestination({ mode: "copy", files: [...files], pane });
      return;
    }
    const location = paneLocation(pane);
    setClipboard(mode, files, location.parentId, location.path, pane);
    setPaneSelectedKeys(pane, new Set());
  };

  const pasteClipboard = async (
    pane: PaneId,
    cutConflictPolicy: "fail" | "rename" | "replace" = "fail",
  ) => {
    if (!clipboardMode || clipboardItems.length === 0) return;
    const location = paneLocation(pane);
    if (clipboardMode === "cut" && isClipboardSource(location)) {
      toast.info(t("routes.files.toast.alreadyInFolder"));
      return;
    }
    // A pane reached by breadcrumb records only its path, so the folder id has to
    // be looked up before pasting: without it the items would land in the root.
    // The lookup hits the API, so a failure is reported like any other paste
    // failure instead of rejecting the promise nobody awaits.
    let targetParentId: string | undefined;
    try {
      targetParentId = await parentIdFor(location);
    } catch (error) {
      toast.error(t("routes.files.toast.pasteFailed"), { description: userMessage(error) });
      return;
    }
    if (targetParentId === undefined && location.path !== "/") {
      toast.error(t("routes.files.toast.pasteFailed"), {
        description: t("features.uploads.folderMissing", { path: location.path }),
      });
      return;
    }
    try {
      if (clipboardMode === "copy") {
        await fileActions.copyMany(clipboardItems, targetParentId, "rename");
      } else if (clipboardItems.length === 1) {
        await fileActions.move(clipboardItems[0], targetParentId, cutConflictPolicy);
      } else {
        await fileActions.bulkMove(
          clipboardItems.map((file) => file.id),
          targetParentId,
          cutConflictPolicy,
        );
      }
      const count = clipboardItems.length;
      setPasteConflictPane(undefined);
      if (clipboardMode === "cut") clearClipboard();
      setPaneSelectedKeys(pane, new Set());
      toast.success(
        t(clipboardMode === "copy" ? "routes.files.toast.copied" : "routes.files.toast.moved", {
          count,
        }),
      );
    } catch (error) {
      const normalized = normalizeApiError(error);
      if (clipboardMode === "cut" && cutConflictPolicy === "fail" && normalized.status === 409) {
        setPasteConflictPane(pane);
        return;
      }
      toast.error(t("routes.files.toast.pasteFailed"), { description: userMessage(error) });
    }
  };

  const navigateToParent = (pane: PaneId) => {
    const location = paneLocation(pane);
    if (location.path === "/") return;
    const parts = location.path.split("/").filter(Boolean);
    const parentPath = parts.length <= 1 ? "/" : `/${parts.slice(0, -1).join("/")}`;
    navigatePane(pane, { path: parentPath, query: "", view: location.view });
  };

  const { keyboardProps } = useKeyboard({
    onKeyDown: (event) => {
      if (
        event.defaultPrevented ||
        event.nativeEvent.isComposing ||
        pending ||
        primaryFileQuery.isPlaceholderData ||
        folderDialogOpen ||
        renameFile ||
        destination ||
        shareFile ||
        previewFile ||
        pasteConflictPane ||
        (event.target instanceof HTMLElement && event.target.closest('[role="dialog"]')) ||
        isEditableTarget(event.target)
      ) {
        event.continuePropagation();
        return;
      }
      const pane = activePaneRef.current;
      const selectedFiles = paneSelectedFiles(pane);
      const selectedIds = selectedFiles.map((file) => file.id);
      const singleSelectedFile = selectedFiles.length === 1 ? selectedFiles[0] : undefined;
      const command = event.ctrlKey || event.metaKey;

      const key = event.key.toLowerCase();
      if (command && key === "c" && selectedFiles.length > 0) {
        event.preventDefault();
        stageClipboard("copy", pane);
        return;
      }
      if (!searchMode && command && key === "x" && selectedFiles.length > 0) {
        event.preventDefault();
        stageClipboard("cut", pane);
        return;
      }
      if (!searchMode && command && key === "v" && hasClipboard) {
        event.preventDefault();
        void pasteClipboard(pane);
        return;
      }
      if (event.key === "F2" && singleSelectedFile) {
        event.preventDefault();
        setActivePane(pane);
        setRenameFile(singleSelectedFile);
        setActionError(undefined);
        setRenameName(singleSelectedFile.name);
        return;
      }
      if (event.key === "Delete" && selectedIds.length > 0) {
        event.preventDefault();
        void trashSelected(pane);
        return;
      }
      if (!searchMode && command && event.shiftKey && event.key.toLowerCase() === "n") {
        event.preventDefault();
        setActivePane(pane);
        setFolderDialogOpen(true);
        return;
      }
      if (!searchMode && event.altKey && event.key === "ArrowUp") {
        event.preventDefault();
        navigateToParent(pane);
        return;
      }
      event.continuePropagation();
    },
  });

  const openFile = (file: FileEntry, pane: PaneId) => {
    activePaneRef.current = pane;
    setActivePane(pane);
    if (file.kind === "folder") {
      const location = paneLocation(pane);
      if (searchMode) {
        void navigateTo({
          to: "/files",
          search: {
            path: joinPath(file.parentPath ?? "/", file.name),
            parentId: file.id,
            query: "",
            view: location.view,
          },
        });
        return;
      }
      navigatePane(pane, {
        path: joinPath(location.path, file.name),
        parentId: file.id,
        query: "",
        view: location.view,
      });
      setPaneSelectedKeys(pane, new Set());
      return;
    }
    if (isPreviewable(file)) {
      setPreviewFile(file);
      return;
    }
    startFileDownload(file);
  };

  const copyDownloadLinks = async (pane: PaneId) => {
    const selectedFiles = paneSelectedFiles(pane);
    try {
      await copyText(selectedFiles.map(absoluteFileDownloadUrl).join("\n"));
      toast.success(t("routes.files.toast.linksCopied", { count: selectedFiles.length }));
    } catch (error) {
      toast.error(t("routes.files.toast.linksCopyFailed"), { description: userMessage(error) });
    }
  };

  const renderToolbar = (pane: PaneId) => {
    if (searchMode) return undefined;
    const location = paneLocation(pane);
    const uploadFilesTriggerRef =
      pane === "secondary" ? secondaryUploadFilesTriggerRef : primaryUploadFilesTriggerRef;
    const uploadFolderTriggerRef =
      pane === "secondary" ? secondaryUploadFolderTriggerRef : primaryUploadFolderTriggerRef;
    return (
      <>
        {pane === "primary" && !search.split ? (
          <Button
            isIconOnly
            size="sm"
            variant="secondary"
            aria-label={t("routes.files.action.openSplit")}
            onPress={openSplitView}
          >
            <SplitIcon className="size-4" />
          </Button>
        ) : pane === "secondary" && search.split ? (
          <Button
            isIconOnly
            size="sm"
            variant="ghost"
            aria-label={t("routes.files.action.closeSplit")}
            onPress={closeSplitView}
          >
            <SplitIcon className="size-4" />
          </Button>
        ) : null}
        <Button
          isIconOnly
          size="sm"
          variant="secondary"
          aria-label={t("routes.files.action.newFolder")}
          onPress={() => {
            setActivePane(pane);
            setFolderDialogOpen(true);
          }}
        >
          <PlusIcon className="size-4" />
        </Button>
        <Dropdown>
          <Button isIconOnly size="sm" variant="primary" aria-label={t("common.action.upload")}>
            <UploadIcon className="size-4" />
          </Button>
          <Dropdown.Popover className="min-w-52">
            <Dropdown.Menu
              aria-label={t("common.action.upload")}
              onAction={(key) => {
                setActivePane(pane);
                if (key === "files") uploadFilesTriggerRef.current?.click();
                if (key === "folder") uploadFolderTriggerRef.current?.click();
                if (key === "background") setBackgroundUploadOpen(true);
              }}
            >
              <Dropdown.Item id="files" textValue={t("routes.files.upload.files")}>
                <FileIcon className="size-4" />
                <Label>{t("routes.files.upload.files")}</Label>
              </Dropdown.Item>
              <Dropdown.Item id="folder" textValue={t("routes.files.upload.folder")}>
                <FolderIcon className="size-4" />
                <Label>{t("routes.files.upload.folder")}</Label>
              </Dropdown.Item>
              {canLocalImport ? (
                <Dropdown.Item id="background" textValue={t("routes.files.upload.background")}>
                  <UploadIcon className="size-4" />
                  <Label>{t("routes.files.upload.background")}</Label>
                </Dropdown.Item>
              ) : null}
            </Dropdown.Menu>
          </Dropdown.Popover>
        </Dropdown>
        <span className="hidden" aria-hidden="true">
          <FileTrigger
            allowsMultiple
            onSelect={(list) => {
              if (list?.length) enqueue(Array.from(list), location.parentId, location.path);
            }}
          >
            <Button ref={uploadFilesTriggerRef}>{t("routes.files.upload.chooseFiles")}</Button>
          </FileTrigger>
          <FileTrigger
            acceptDirectory
            allowsMultiple
            onSelect={(list) => {
              if (list?.length) enqueue(Array.from(list), location.parentId, location.path);
            }}
          >
            <Button ref={uploadFolderTriggerRef}>{t("routes.files.upload.chooseFolder")}</Button>
          </FileTrigger>
        </span>
      </>
    );
  };

  const renderSelectionOverlay = (pane: PaneId) => {
    const selectedFiles = paneSelectedFiles(pane);
    const clipboardTargetPane =
      search.split && clipboardSourcePane
        ? clipboardSourcePane === "primary"
          ? "secondary"
          : "primary"
        : "primary";
    const showClipboard =
      !searchMode && hasClipboard && (!search.split || pane === clipboardTargetPane);
    const canPasteHere =
      showClipboard &&
      !(clipboardMode === "cut" && isClipboardSource(paneLocation(pane)));
    if (showClipboard) {
      return (
        <div className="pointer-events-none absolute inset-x-0 bottom-4 z-30 flex justify-center px-4">
          <div className="pointer-events-auto flex max-w-full items-center gap-1.5 rounded-full border border-border bg-surface/95 p-1.5 shadow-xl backdrop-blur">
            <span className="shrink-0 rounded-full bg-default/40 px-3 py-2 text-sm font-medium text-foreground">
              {t(
                clipboardMode === "cut"
                  ? "routes.files.clipboard.cut"
                  : "routes.files.clipboard.copied",
                {
                  count: clipboardItems.length,
                },
              )}
            </span>
            <Button
              isIconOnly
              size="sm"
              variant="ghost"
              aria-label={t("routes.files.clipboard.paste", { count: clipboardItems.length })}
              isDisabled={fileActions.pending || !canPasteHere}
              onPress={() => void pasteClipboard(pane)}
            >
              <PasteIcon className="size-4" />
            </Button>
            <Button
              isIconOnly
              size="sm"
              variant="ghost"
              aria-label={t(
                clipboardMode === "cut"
                  ? "routes.files.clipboard.cancelCut"
                  : "routes.files.clipboard.clearCopied",
              )}
              onPress={clearClipboard}
            >
              <CloseIcon className="size-4" />
            </Button>
          </div>
        </div>
      );
    }
    return (
      <FileSelectionToolbar
        selectedFiles={selectedFiles}
        pending={pending}
        onCut={searchMode ? undefined : () => stageClipboard("cut", pane)}
        onCopy={() => stageClipboard("copy", pane)}
        onRename={() => {
          const file = selectedFiles[0];
          setActivePane(pane);
          setRenameFile(file);
          setRenameName(file.name);
          setActionError(undefined);
        }}
        onDuplicate={searchMode ? undefined : () => void duplicateSelected(pane)}
        onShare={() => {
          setActivePane(pane);
          setShareFile(selectedFiles[0]);
        }}
        onDownload={() => startFileDownload(selectedFiles[0])}
        onCopyDownloadLinks={() => void copyDownloadLinks(pane)}
        onMove={() => {
          setActivePane(pane);
          setActionError(undefined);
          setDestination({ mode: "move", files: [...selectedFiles], pane });
        }}
        onTrash={() => void trashSelected(pane)}
        onClear={() => setPaneSelectedKeys(pane, new Set())}
      />
    );
  };

  /**
   * Banner for a pane whose listing failed, rendered above the panes. Every pane shows its
   * own failure in both modes, so a plain `/files` request that never arrived reads as an
   * error with a retry instead of an empty folder; when only a further page failed, the
   * pages already on screen stay in place and the retry asks for that page again.
   */
  const renderPaneError = (pane: PaneId) => {
    const fileQuery = paneQuery(pane);
    if (!paneQueryEnabled(pane) || !fileQuery.isError) return null;
    return (
      <div role="alert" className="mb-3 rounded-xl border border-danger/30 p-4 text-sm">
        <p className="font-medium">
          {searchMode
            ? t(
                fileQuery.isFetchNextPageError
                  ? "routes.search.error.more"
                  : "routes.search.error.failed",
              )
            : t("common.state.error")}
        </p>
        <p className="mt-1 text-muted">{userMessage(fileQuery.error)}</p>
        <Button
          size="sm"
          variant="secondary"
          className="mt-2"
          isDisabled={fileQuery.isFetching}
          onPress={() =>
            void (fileQuery.isFetchNextPageError ? fileQuery.fetchNextPage() : fileQuery.refetch())
          }
        >
          {t("common.action.retry")}
        </Button>
      </div>
    );
  };

  const renderPane = (pane: PaneId) => {
    const location = paneLocation(pane);
    const files = paneFiles(pane);
    const selectedKeys = paneSelectedKeys(pane);
    const fileQuery = paneQuery(pane);
    const browser = (
      <FileBrowser
        files={files}
        path={location.path}
        rootLabel={t(searchMode ? "routes.search.title" : "routes.files.rootLabel")}
        view={location.view}
        loading={fileQuery.isPending}
        onNavigatePath={(path) => navigatePane(pane, { path, query: "", view: location.view })}
        onViewChange={(view) => navigatePane(pane, { ...location, view }, true)}
        onOpen={(file) => openFile(file, pane)}
        onBack={searchMode ? undefined : () => window.history.back()}
        selection={{
          selectedKeys,
          onSelectionChange: (selection) => setPaneSelectedKeys(pane, selection),
          onClearSelection: () => setPaneSelectedKeys(pane, new Set()),
        }}
        selectionDisabled={fileQuery.isPlaceholderData || pending}
        hasNextPage={
          fileQuery.hasNextPage && !fileQuery.isPlaceholderData && !fileQuery.isFetchNextPageError
        }
        isLoadingMore={fileQuery.isFetchingNextPage}
        onLoadMore={() => {
          if (fileQuery.hasNextPage && !fileQuery.isFetching && !fileQuery.isPlaceholderData)
            void fileQuery.fetchNextPage();
        }}
        toolbar={renderToolbar(pane)}
        selectionOverlay={fileQuery.isPlaceholderData ? undefined : renderSelectionOverlay(pane)}
        dimmedIds={cutIds}
        hideFolderControls={Boolean(searchMode)}
        showLocations={Boolean(searchMode)}
        emptyTitle={searchMode ? t("routes.search.emptyTitle") : undefined}
        emptyHint={searchMode ? t("routes.search.emptyHint") : undefined}
        onOpenContainingFolder={
          searchMode
            ? (file) => {
                void navigateTo({
                  to: "/files",
                  search: {
                    path: file.parentPath ?? "/",
                    parentId: file.parentId,
                    query: "",
                    view: location.view,
                  },
                });
              }
            : undefined
        }
      />
    );
    // A pane whose first page never arrived has no listing: rendering the browser anyway
    // would present the failure as an empty folder, so the banner above the panes stands in
    // for it until a retry succeeds. A pane that still holds earlier pages keeps them, and a
    // held-back query is left alone because its pane is not showing that listing.
    const hasNothingToList = paneQueryEnabled(pane) && fileQuery.isError && !fileQuery.data;
    if (searchMode)
      return (
        <div data-testid={`file-pane-${pane}`} className="flex min-h-0 min-w-0 flex-1">
          {hasNothingToList ? null : browser}
        </div>
      );
    return (
      <div data-testid={`file-pane-${pane}`} className="flex min-h-0 min-w-0 flex-1 rounded-xl">
        <DropZone
          data-testid={pane === "primary" ? "file-drop-zone" : "file-drop-zone-secondary"}
          aria-label={t("routes.files.dropzone.label", { path: location.path })}
          getDropOperation={() => "copy"}
          className="flex min-h-0 min-w-0 flex-1 flex-col overflow-x-hidden outline-none"
          onDrop={async (event) => {
            const dropped = await Promise.all(
              event.items.filter((item) => item.kind === "file").map((item) => item.getFile()),
            );
            if (dropped.length) enqueue(dropped, location.parentId, location.path);
          }}
        >
          {({ isDropTarget }) => (
            <div className="flex min-h-0 min-w-0 flex-1 flex-col gap-3 overflow-x-hidden">
              {isDropTarget ? (
                <div className="shrink-0 rounded-xl border-2 border-dashed border-accent bg-accent/10 px-4 py-4 text-center text-sm font-medium text-accent sm:px-6 sm:py-6">
                  {t("routes.files.dropzone.hint", { path: location.path })}
                </div>
              ) : null}
              {hasNothingToList ? null : browser}
            </div>
          )}
        </DropZone>
      </div>
    );
  };

  return (
    <Page className={`h-full min-h-0 overflow-x-hidden ${searchMode ? "gap-4" : "gap-0"}`}>
      {searchMode && (
        <SearchControls
          search={searchMode.criteria}
          onChange={searchMode.onChange}
          isOpen={filtersOpen}
          onOpenChange={setFiltersOpen}
        />
      )}
      {searchMode && activeSearch && !missingFolder && !invalidDates && (
        <p aria-live="polite" className="text-xs text-muted">
          {primaryFileQuery.isFetching
            ? t("routes.search.updating")
            : t("routes.search.loaded", { count: primaryFiles.length })}
        </p>
      )}
      <PageContent className="flex min-h-0 flex-1 overflow-x-hidden">
        <div {...keyboardProps} className="flex min-h-0 min-w-0 flex-1 flex-col overflow-x-hidden">
          {renderPaneError("primary")}
          {search.split ? renderPaneError("secondary") : null}
          {searchMode && (missingFolder || invalidDates || !activeSearch) ? (
            <div className="flex flex-1 flex-col items-center justify-center rounded-2xl border border-dashed border-border p-8 text-center">
              <p className="text-lg font-semibold">
                {missingFolder
                  ? t("routes.search.folder.title")
                  : invalidDates
                    ? t("routes.search.dates.title")
                    : t("routes.search.start.title")}
              </p>
              <p className="mt-2 text-sm text-muted">
                {missingFolder
                  ? t("routes.search.folder.description")
                  : invalidDates
                    ? t("routes.search.dates.description")
                    : t("routes.search.start.description")}
              </p>
              {(missingFolder || invalidDates) && (
                <Button variant="secondary" className="mt-3" onPress={() => setFiltersOpen(true)}>
                  {t(missingFolder ? "routes.search.folder.action" : "routes.search.dates.action")}
                </Button>
              )}
            </div>
          ) : (
            <div
              className={
                search.split
                  ? "grid min-h-0 min-w-0 flex-1 grid-cols-1 gap-3 lg:grid-cols-2"
                  : "flex min-h-0 min-w-0 flex-1"
              }
            >
              {renderPane("primary")}
              {search.split ? renderPane("secondary") : null}
            </div>
          )}
        </div>
      </PageContent>

      <AppDialog
        open={folderDialogOpen}
        onOpenChange={setFolderDialogOpen}
        title={t("routes.files.folder.title")}
        description={t("routes.files.folder.description", { path: activeLocation.path })}
        size="md"
        footer={
          <>
            <Button variant="secondary" onPress={() => setFolderDialogOpen(false)}>
              {t("common.action.cancel")}
            </Button>
            <Button
              variant="primary"
              isDisabled={!folderName.trim() || fileActions.pending}
              onPress={() => void createFolder()}
            >
              {t("routes.files.folder.submit")}
            </Button>
          </>
        }
      >
        <TextField
          autoFocus
          value={folderName}
          onChange={setFolderName}
          onKeyDown={(event) => {
            if (event.key === "Enter") {
              event.preventDefault();
              if (!pending) void createFolder();
            } else event.continuePropagation();
          }}
        >
          <Label>{t("routes.files.folder.name")}</Label>
          <Input placeholder={t("routes.files.folder.placeholder")} />
        </TextField>
      </AppDialog>

      <BackgroundUploadDialog
        open={backgroundUploadOpen}
        onOpenChange={setBackgroundUploadOpen}
        currentPath={activeLocation.path}
      />

      <FileActionDialogs
        renameFile={renameFile}
        renameName={renameName}
        onRenameNameChange={setRenameName}
        onRenameClose={() => {
          if (!pending) {
            setRenameFile(undefined);
            setActionError(undefined);
          }
        }}
        onRename={() => void renameSelected()}
        pending={pending}
        error={actionError}
        destinationAction={
          destination
            ? {
                mode: destination.mode,
                count: destination.files.length,
                onClose: () => {
                  if (!pending) {
                    setDestination(undefined);
                    setActionError(undefined);
                  }
                },
                onConfirm: (parentId) => void transferSelected(parentId),
              }
            : undefined
        }
        shareFile={shareFile}
        onShareClose={() => setShareFile(undefined)}
        previewFile={previewFile}
        onPreviewClose={() => setPreviewFile(undefined)}
      />

      <AppDialog
        open={pasteConflictPane !== undefined}
        onOpenChange={(open) => {
          if (!open) setPasteConflictPane(undefined);
        }}
        title={t("routes.files.conflict.title")}
        description={t("routes.files.conflict.description")}
        size="md"
        footer={
          <>
            <Button variant="secondary" onPress={() => setPasteConflictPane(undefined)}>
              {t("common.action.cancel")}
            </Button>
            <Button
              variant="secondary"
              isDisabled={fileActions.pending || pasteConflictPane === undefined}
              onPress={() => {
                if (pasteConflictPane) void pasteClipboard(pasteConflictPane, "rename");
              }}
            >
              {t("routes.files.conflict.keepBoth")}
            </Button>
            <Button
              variant="danger"
              isDisabled={fileActions.pending || pasteConflictPane === undefined}
              onPress={() => {
                if (pasteConflictPane) void pasteClipboard(pasteConflictPane, "replace");
              }}
            >
              {t("routes.files.conflict.replace")}
            </Button>
          </>
        }
      >
        <p className="text-sm text-muted">{t("routes.files.conflict.hint")}</p>
      </AppDialog>
    </Page>
  );
}

/**
 * Expands a react-aria selection into entry ids. "all" is the library's select-everything
 * sentinel rather than a set, so it is resolved against the rows currently listed.
 */
function selectionIds(selection: Selection, files: FileEntry[]) {
  return selection === "all" ? files.map((file) => file.id) : Array.from(selection, String);
}

/** Folder identity ignores a trailing slash, so "/a" and "/a/" are one folder. */
function normalizeFolderPath(path: string) {
  const trimmed = path.replace(/\/+$/, "");
  return trimmed === "" ? "/" : trimmed;
}

/** Appends a folder name to a path and collapses repeated slashes; root stays "/". */
function joinPath(parent: string, name: string) {
  return `${parent === "/" ? "" : parent}/${name}`.replace(/\/+/g, "/") || "/";
}

/**
 * Whether a key event came from somewhere the user is typing. The shortcut handler skips
 * those events, so Delete or F2 pressed inside a text field edits the text instead of the
 * file selection.
 */
function isEditableTarget(target: EventTarget | null) {
  return (
    target instanceof HTMLElement &&
    (target.isContentEditable ||
      Boolean(target.closest("input, textarea, select, [contenteditable='true']")))
  );
}
