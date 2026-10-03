import { Button, Input, Label, Spinner, TextField } from "@heroui/react";
import { useQuery } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { FileTrigger, type Selection } from "react-aria-components";
import { toast } from "sonner";
import DownloadIcon from "~icons/gravity-ui/arrow-down-to-line";
import UploadIcon from "~icons/gravity-ui/arrow-up-from-line";
import PencilIcon from "~icons/gravity-ui/pencil";
import PlusIcon from "~icons/gravity-ui/plus";
import LinkIcon from "~icons/gravity-ui/link";
import TrashIcon from "~icons/gravity-ui/trash-bin";
import CloseIcon from "~icons/gravity-ui/xmark";
import { $api, fetchClient } from "@/api/client";
import { userMessage } from "@/api/errors";
import type { FileEntry } from "@/api/types";
import { AppDialog } from "@/components/dialogs/app-dialog";
import { FilePreviewDialog, isPreviewable } from "@/components/file-preview-dialog";
import { Page, PageContent } from "@/components/page";
import { startFileDownload } from "@/features/files/download";
import { FileBrowser, type FileBrowserView } from "@/features/files/file-browser";
import { useFileActions } from "@/features/files/mutations";
import { useSharedFilePages, useSharedWithMePages } from "@/features/files/queries";
import { ShareDialog } from "@/features/files/share-dialog";
import { useUploadStore } from "@/features/uploads/store";
import { useI18n } from "@/lib/i18n";
import { getQueryClient } from "@/lib/queryClient";

type Permission = "read" | "edit";

export type SharedBrowserSearch = {
  path: string;
  parentId?: string;
  query: string;
  view: FileBrowserView;
  permission?: Permission;
};

type SharedFileBrowserProps = {
  mode: "shared" | "with-me";
  search: SharedBrowserSearch;
  navigate: (search: SharedBrowserSearch, replace?: boolean) => void;
};

export function sharedBrowserSearch(search: Record<string, unknown>): SharedBrowserSearch {
  return {
    path: typeof search.path === "string" && search.path ? search.path : "/",
    parentId: typeof search.parentId === "string" ? search.parentId : undefined,
    query: typeof search.query === "string" ? search.query : "",
    view: search.view === "grid" ? "grid" : "list",
    permission:
      search.permission === "edit" ? "edit" : search.permission === "read" ? "read" : undefined,
  };
}

export function SharedFileBrowser({ mode, search, navigate }: SharedFileBrowserProps) {
  const { t } = useI18n();
  const [previewFile, setPreviewFile] = useState<FileEntry>();
  const [selectedKeys, setSelectedKeys] = useState<Selection>(new Set());
  const [folderDialogOpen, setFolderDialogOpen] = useState(false);
  const [folderName, setFolderName] = useState("");
  const [renameFile, setRenameFile] = useState<FileEntry>();
  const [renameName, setRenameName] = useState("");
  const [shareFile, setShareFile] = useState<FileEntry>();
  const uploadTriggerRef = useRef<HTMLButtonElement>(null);
  const enqueue = useUploadStore((state) => state.enqueue);
  const fileActions = useFileActions();
  const atRoot = !search.parentId;
  // Ancestor ids learned while walking down. Keys are the displayed path relative
  // to the share root; the root itself is known without an id.
  const ancestry = useRef(
    new Map<string, { parentId?: string; permission?: SharedBrowserSearch["permission"] }>([
      ["/", {}],
    ]),
  );

  const sharedQuery = useSharedFilePages(atRoot && mode === "shared");
  const sharedWithMeQuery = useSharedWithMePages(atRoot && mode === "with-me");
  const childrenQuery = useQuery({
    ...$api.queryOptions("get", "/v1/files", {
      params: {
        query: {
          parentId: search.parentId,
          search: search.query || undefined,
          sort: "name",
          order: "asc",
          status: "active",
          limit: 200,
        },
      },
    }),
    enabled: !atRoot,
  });

  useEffect(() => setSelectedKeys(new Set()), [search.parentId, search.path, search.query]);

  const incomingEntries = sharedWithMeQuery.data?.pages.flatMap((page) => page.items) ?? [];
  const incomingPermission = new Map(
    incomingEntries.map((entry) => [entry.file.id, entry.permission]),
  );
  const roots =
    mode === "with-me"
      ? incomingEntries.map((entry) => entry.file)
      : (sharedQuery.data?.pages.flatMap((page) => page.items) ?? []);
  // The listing is cursor-paged, so the control that walks the remaining pages
  // follows whichever of the two queries the current mode uses.
  const rootQuery = mode === "with-me" ? sharedWithMeQuery : sharedQuery;
  const moreRoots = atRoot && rootQuery.hasNextPage;
  const loadingMoreRoots = rootQuery.isFetchingNextPage;
  const rootFiles = roots.filter(
    (file) =>
      !search.query || file.name.toLocaleLowerCase().includes(search.query.toLocaleLowerCase()),
  );
  const files = atRoot ? rootFiles : (childrenQuery.data?.items ?? []);
  const loading = atRoot
    ? mode === "with-me"
      ? sharedWithMeQuery.isPending
      : sharedQuery.isPending
    : childrenQuery.isPending;
  const rootLabel =
    mode === "with-me"
      ? t("features.fileBrowser.root.sharedWithMe")
      : t("features.fileBrowser.root.shared");

  const permissionFor = (file: FileEntry): Permission => {
    if (mode === "shared") return "edit";
    if (!atRoot) return search.permission ?? "read";
    return incomingPermission.get(file.id) ?? "read";
  };

  const selectedIds =
    selectedKeys === "all" ? files.map((file) => file.id) : Array.from(selectedKeys, String);
  const selectedFiles = files.filter((file) => selectedIds.includes(file.id));
  const selectedCount = selectedFiles.length;
  const singleSelected = selectedFiles.length === 1 ? selectedFiles[0] : undefined;
  const selectedWritable =
    selectedFiles.length > 0 && selectedFiles.every((file) => permissionFor(file) === "edit");
  const currentFolderEditable = !atRoot && (mode === "shared" || search.permission === "edit");

  const refreshRoots = async () => {
    if (mode === "with-me") await sharedWithMeQuery.refetch();
    else await sharedQuery.refetch();
  };

  const createFolder = async () => {
    const name = folderName.trim();
    if (!name || !search.parentId || !currentFolderEditable) return;
    try {
      await fileActions.createFolder(name, search.parentId);
      setFolderName("");
      setFolderDialogOpen(false);
      await childrenQuery.refetch();
      toast.success(t("features.fileBrowser.toast.folderCreated"));
    } catch (error) {
      toast.error(t("features.fileBrowser.toast.folderCreateFailed"), {
        description: userMessage(error),
      });
    }
  };

  const renameSelected = async () => {
    if (!renameFile || !renameName.trim() || permissionFor(renameFile) !== "edit") return;
    try {
      await fileActions.rename(renameFile, renameName.trim());
      setRenameFile(undefined);
      setRenameName("");
      setSelectedKeys(new Set());
      if (atRoot) await refreshRoots();
      else await childrenQuery.refetch();
      toast.success(t("features.fileBrowser.toast.renamed"));
    } catch (error) {
      toast.error(t("features.fileBrowser.toast.renameFailed"), {
        description: userMessage(error),
      });
    }
  };

  const trashSelected = async () => {
    if (!selectedWritable) return;
    try {
      await Promise.all(selectedFiles.map((file) => fileActions.trash(file.id)));
      setSelectedKeys(new Set());
      if (atRoot) await refreshRoots();
      else await childrenQuery.refetch();
      toast.success(t("features.fileBrowser.toast.trashed", { count: selectedFiles.length }));
    } catch (error) {
      toast.error(t("features.fileBrowser.toast.trashFailed"), {
        description: userMessage(error),
      });
    }
  };

  const stopSharingSelected = async () => {
    if (mode !== "shared" || !atRoot || !selectedFiles.length) return;
    try {
      for (const file of selectedFiles) {
        const { data: grants } = await fetchClient.GET("/v1/files/{fileId}/grants", {
          params: { path: { fileId: file.id } },
        });

        const shareIds: string[] = [];
        let cursor: string | undefined;
        do {
          const { data: page } = await fetchClient.GET("/v1/files/{fileId}/shares", {
            params: {
              path: { fileId: file.id },
              query: { limit: 200, cursor },
            },
          });
          shareIds.push(...(page?.items ?? []).map((share) => share.id));
          cursor = page?.nextCursor;
        } while (cursor);

        await Promise.all([
          ...(grants ?? []).map((grant) =>
            fetchClient.DELETE("/v1/grants/{grantId}", {
              params: { path: { grantId: grant.id } },
            }),
          ),
          ...shareIds.map((shareId) =>
            fetchClient.DELETE("/v1/shares/{shareId}", {
              params: { path: { shareId } },
            }),
          ),
        ]);

        const queryClient = getQueryClient();
        await Promise.all([
          queryClient.invalidateQueries({
            queryKey: $api.queryOptions("get", "/v1/files/{fileId}/grants", {
              params: { path: { fileId: file.id } },
            }).queryKey,
          }),
          queryClient.invalidateQueries({
            queryKey: $api.queryOptions("get", "/v1/files/{fileId}/shares", {
              params: { path: { fileId: file.id }, query: { limit: 200 } },
            }).queryKey,
          }),
        ]);
      }

      setSelectedKeys(new Set());
      await refreshRoots();
      toast.success(t("features.fileBrowser.toast.unshared", { count: selectedFiles.length }));
    } catch (error) {
      toast.error(t("features.fileBrowser.toast.shareRemoveFailed"), {
        description: userMessage(error),
      });
    }
  };

  const openFile = (file: FileEntry) => {
    if (file.kind === "folder") {
      const path = joinPath(search.path, file.name);
      const permission = mode === "with-me" ? permissionFor(file) : undefined;
      ancestry.current.set(path, { parentId: file.id, permission });
      navigate({ path, parentId: file.id, query: "", view: search.view, permission });
      return;
    }
    if (isPreviewable(file)) {
      setPreviewFile(file);
      return;
    }
    startFileDownload(file);
  };

  return (
    <Page className="h-full min-h-0 gap-0 overflow-x-hidden">
      <PageContent className="flex min-h-0 flex-1 overflow-x-hidden">
        <FileBrowser
          files={files}
          path={search.path}
          rootLabel={rootLabel}
          view={search.view}
          loading={loading}
          onNavigatePath={(path) => {
            const target = path === "" ? "/" : path;
            // The listing calls are addressed by folder id and a shared subtree
            // does not expose the ids of its ancestors, so only a folder the user
            // walked through in this session can be opened again; anything else
            // falls back to the share root, which is what a deep link starts at.
            const known = ancestry.current.get(target);
            if (target !== "/" && !known) {
              navigate({ path: "/", query: "", view: search.view }, true);
              return;
            }
            navigate({ path: target, query: "", view: search.view, ...known }, true);
          }}
          onViewChange={(view) => navigate({ ...search, view }, true)}
          onOpen={openFile}
          onBack={() => window.history.back()}
          selection={{
            selectedKeys,
            onSelectionChange: setSelectedKeys,
            onClearSelection: () => setSelectedKeys(new Set()),
          }}
          toolbar={
            currentFolderEditable || moreRoots ? (
              <>
                {currentFolderEditable ? (
                  <>
                    <Button
                      isIconOnly
                      size="sm"
                      variant="secondary"
                      aria-label={t("features.fileBrowser.action.newFolder")}
                      isDisabled={fileActions.pending}
                      onPress={() => setFolderDialogOpen(true)}
                    >
                      <PlusIcon className="size-4" />
                    </Button>
                    <Button
                      isIconOnly
                      size="sm"
                      variant="primary"
                      aria-label={t("features.fileBrowser.action.uploadFiles")}
                      isDisabled={fileActions.pending}
                      onPress={() => uploadTriggerRef.current?.click()}
                    >
                      <UploadIcon className="size-4" />
                    </Button>
                    <span className="hidden" aria-hidden="true">
                      <FileTrigger
                        allowsMultiple
                        onSelect={(list) => {
                          if (list?.length) enqueue(Array.from(list), search.parentId, search.path);
                        }}
                      >
                        <Button ref={uploadTriggerRef}>
                          {t("features.fileBrowser.action.chooseFiles")}
                        </Button>
                      </FileTrigger>
                    </span>
                  </>
                ) : null}
                {moreRoots ? (
                  <Button
                    size="sm"
                    variant="ghost"
                    isDisabled={loadingMoreRoots}
                    onPress={() => void rootQuery.fetchNextPage()}
                  >
                    {t("features.fileBrowser.action.loadMore")}
                  </Button>
                ) : null}
              </>
            ) : undefined
          }
          selectionOverlay={
            selectedCount > 0 ? (
              <div className="pointer-events-none absolute inset-x-0 bottom-4 z-30 flex justify-center px-4">
                <div className="pointer-events-auto flex max-w-full items-center gap-1.5 overflow-x-auto rounded-full border border-border bg-surface/95 p-1.5 shadow-xl backdrop-blur">
                  <span className="shrink-0 rounded-full bg-accent/10 px-3 py-2 text-sm font-medium text-accent">
                    {t("features.fileBrowser.selected", { count: selectedCount })}
                  </span>
                  {singleSelected && selectedWritable ? (
                    <Button
                      isIconOnly
                      size="sm"
                      variant="ghost"
                      aria-label={t("features.fileBrowser.action.rename")}
                      isDisabled={fileActions.pending}
                      onPress={() => {
                        setRenameFile(singleSelected);
                        setRenameName(singleSelected.name);
                      }}
                    >
                      <PencilIcon className="size-4" />
                    </Button>
                  ) : null}
                  {singleSelected && mode === "shared" ? (
                    <Button
                      isIconOnly
                      size="sm"
                      variant="ghost"
                      aria-label={t("features.fileBrowser.action.share")}
                      onPress={() => setShareFile(singleSelected)}
                    >
                      <LinkIcon className="size-4" />
                    </Button>
                  ) : null}
                  {singleSelected?.kind === "file" ? (
                    <Button
                      isIconOnly
                      size="sm"
                      variant="ghost"
                      aria-label={t("features.fileBrowser.action.download")}
                      onPress={() => startFileDownload(singleSelected)}
                    >
                      <DownloadIcon className="size-4" />
                    </Button>
                  ) : null}
                  {mode === "shared" && atRoot && selectedCount > 0 ? (
                    <Button
                      isIconOnly
                      size="sm"
                      variant="danger"
                      aria-label={t("features.fileBrowser.action.stopSharing")}
                      onPress={() => void stopSharingSelected()}
                    >
                      <LinkIcon className="size-4" />
                    </Button>
                  ) : mode === "with-me" && selectedWritable ? (
                    <Button
                      isIconOnly
                      size="sm"
                      variant="danger"
                      aria-label={t("features.fileBrowser.action.trash")}
                      isDisabled={fileActions.pending}
                      onPress={() => void trashSelected()}
                    >
                      <TrashIcon className="size-4" />
                    </Button>
                  ) : null}
                  <Button
                    isIconOnly
                    size="sm"
                    variant="ghost"
                    aria-label={t("features.fileBrowser.action.clearSelection")}
                    onPress={() => setSelectedKeys(new Set())}
                  >
                    <CloseIcon className="size-4" />
                  </Button>
                </div>
              </div>
            ) : undefined
          }
          emptyHint={
            atRoot
              ? mode === "with-me"
                ? t("features.fileBrowser.emptySharedWithMe")
                : t("features.fileBrowser.emptyShared")
              : t("features.fileBrowser.emptySharedFolder")
          }
        />
      </PageContent>

      <AppDialog
        open={folderDialogOpen}
        onOpenChange={setFolderDialogOpen}
        title={t("features.fileBrowser.dialog.createFolderTitle")}
        footer={
          <>
            <Button variant="secondary" onPress={() => setFolderDialogOpen(false)}>
              {t("common.action.cancel")}
            </Button>
            <Button
              variant="primary"
              isDisabled={!folderName.trim()}
              onPress={() => void createFolder()}
            >
              {t("features.fileBrowser.dialog.createFolderTitle")}
            </Button>
          </>
        }
      >
        <TextField value={folderName} onChange={setFolderName}>
          <Label>{t("common.label.name")}</Label>
          <Input autoFocus />
        </TextField>
      </AppDialog>

      <AppDialog
        open={Boolean(renameFile)}
        onOpenChange={(open) => {
          if (!open) setRenameFile(undefined);
        }}
        title={t("features.fileBrowser.dialog.renameTitle")}
        footer={
          <>
            <Button variant="secondary" onPress={() => setRenameFile(undefined)}>
              {t("common.action.cancel")}
            </Button>
            <Button
              variant="primary"
              isDisabled={!renameName.trim()}
              onPress={() => void renameSelected()}
            >
              {t("common.action.rename")}
            </Button>
          </>
        }
      >
        <TextField value={renameName} onChange={setRenameName}>
          <Label>{t("common.label.name")}</Label>
          <Input autoFocus />
        </TextField>
      </AppDialog>

      <ShareDialog
        file={shareFile}
        onOpenChange={(open) => {
          if (!open) setShareFile(undefined);
        }}
      />

      <FilePreviewDialog
        file={previewFile}
        onOpenChange={(open) => {
          if (!open) setPreviewFile(undefined);
        }}
      />
    </Page>
  );
}

export function SharedPageSpinner() {
  return (
    <div className="flex min-h-[40vh] items-center justify-center">
      <Spinner size="lg" />
    </div>
  );
}

function joinPath(parent: string, name: string) {
  return `${parent === "/" ? "" : parent}/${name}`.replace(/\/+/g, "/") || "/";
}
