import { Button, Input, Label, Spinner, TextField } from "@heroui/react";
import { createFileRoute } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { FileTrigger, type Selection } from "react-aria-components";
import { toast } from "sonner";
import DownloadIcon from "~icons/gravity-ui/arrow-down-to-line";
import UploadIcon from "~icons/gravity-ui/arrow-up-from-line";
import LogoIcon from "~icons/gravity-ui/layers";
import LinkIcon from "~icons/gravity-ui/link";
import PencilIcon from "~icons/gravity-ui/pencil";
import PlusIcon from "~icons/gravity-ui/plus";
import TrashIcon from "~icons/gravity-ui/trash-bin";
import CloseIcon from "~icons/gravity-ui/xmark";
import { apiFetch } from "@/api/client";
import { ApiError, userMessage } from "@/api/errors";
import type { FileEntry, PublicShare } from "@/api/types";
import { AppDialog } from "@/components/dialogs/app-dialog";
import { Page, PageContent } from "@/components/page";
import { FileBrowser, formatFileBytes, type FileBrowserView } from "@/features/files/file-browser";
import { copyText } from "@/features/files/download";
import { useI18n } from "@/lib/i18n";

export const Route = createFileRoute("/share/$token")({
  component: PublicSharePage,
});

type ShareFilePage = { items: FileEntry[] };
type UploadSession = { id: string; partSize: number };

function PublicSharePage() {
  const { token } = Route.useParams();
  const { t } = useI18n();
  const [password, setPassword] = useState("");
  const [activePassword, setActivePassword] = useState("");
  const [share, setShare] = useState<PublicShare>();
  const [items, setItems] = useState<FileEntry[]>([]);
  const [path, setPath] = useState("/");
  const [pathIds, setPathIds] = useState<Record<string, string>>({});
  const [view, setView] = useState<FileBrowserView>("list");
  const [loading, setLoading] = useState(true);
  const [needsPassword, setNeedsPassword] = useState(false);
  const [error, setError] = useState<string>();
  const [selectedKeys, setSelectedKeys] = useState<Selection>(new Set());
  const [folderDialogOpen, setFolderDialogOpen] = useState(false);
  const [folderName, setFolderName] = useState("");
  const [renameFile, setRenameFile] = useState<FileEntry>();
  const [renameName, setRenameName] = useState("");
  const [uploading, setUploading] = useState(false);
  const editable = share?.permission === "edit" && share.file.kind === "folder";
  const selectedIds =
    selectedKeys === "all" ? items.map((item) => item.id) : Array.from(selectedKeys, String);
  const selectedFiles = items.filter((item) => selectedIds.includes(item.id));
  const singleSelected = selectedFiles.length === 1 ? selectedFiles[0] : undefined;
  const currentParentId = share ? (pathIds[path] ?? share.file.id) : undefined;

  const loadShare = async (signal: AbortSignal) => {
    const response = await apiFetch(`/v1/public/shares/${encodeURIComponent(token)}`, {
      headers: shareHeaders(activePassword),
      signal,
    });
    const next = (await response.json()) as PublicShare;
    if (signal.aborted) return;
    setShare(next);
    setPathIds({ "/": next.file.id });
    setNeedsPassword(false);
    setPath("/");
  };

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError(undefined);
    void loadShare(controller.signal)
      .catch((cause) => {
        if (controller.signal.aborted) return;
        if (cause instanceof ApiError && cause.status === 401) {
          setNeedsPassword(true);
          setShare(undefined);
          return;
        }
        setError(userMessage(cause));
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [token, activePassword]);

  const refreshItems = async (signal?: AbortSignal) => {
    if (share?.file.kind !== "folder") {
      setItems(share ? [share.file] : []);
      return;
    }
    const params = new URLSearchParams({ limit: "200" });
    if (path !== "/") params.set("path", path.slice(1));
    const response = await apiFetch(
      `/v1/public/shares/${encodeURIComponent(token)}/files?${params.toString()}`,
      {
        headers: shareHeaders(activePassword),
        signal,
      },
    );
    const page = (await response.json()) as ShareFilePage;
    if (!signal?.aborted) setItems(page.items ?? []);
  };

  useEffect(() => {
    if (!share) return;
    const controller = new AbortController();
    setLoading(true);
    setError(undefined);
    void refreshItems(controller.signal)
      .catch((cause) => {
        if (!controller.signal.aborted) setError(userMessage(cause));
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [share, token, activePassword, path]);

  useEffect(() => setSelectedKeys(new Set()), [path]);

  const publicContentUrl = (file: FileEntry) => {
    const isRootFile = share?.file.kind === "file" && file.id === share.file.id;
    const fileName = encodeURIComponent(file.name);
    const endpoint = isRootFile
      ? `/v1/public/shares/${encodeURIComponent(token)}/content/${fileName}`
      : `/v1/public/shares/${encodeURIComponent(token)}/files/${encodeURIComponent(file.id)}/content/${fileName}`;
    return new URL(`/api${endpoint}`, window.location.origin).toString();
  };

  const publicDownloadUrl = (file: FileEntry) => {
    const url = new URL(publicContentUrl(file));
    url.searchParams.set("download", "1");
    return url.toString();
  };

  const download = async (file: FileEntry) => {
    setError(undefined);
    try {
      const response = await apiFetch(
        new URL(publicDownloadUrl(file)).pathname + new URL(publicDownloadUrl(file)).search,
        {
          headers: shareHeaders(activePassword),
        },
      );
      const blob = await response.blob();
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement("a");
      anchor.href = url;
      anchor.download = file.name;
      // The anchor has to be in the document for the click to start the download
      // in every browser, and revoking the object URL straight away can abort it
      // (Firefox and Safari read the blob after the click returns).
      document.body.append(anchor);
      anchor.click();
      anchor.remove();
      window.setTimeout(() => URL.revokeObjectURL(url), 1_000);
    } catch (cause) {
      setError(userMessage(cause));
    }
  };

  const copyDownloadLink = async (file: FileEntry) => {
    if (activePassword) {
      toast.error(t("routes.share.toast.directLinkUnavailable"));
      return;
    }
    try {
      await copyText(publicContentUrl(file));
      toast.success(t("routes.share.toast.directLinkCopied"));
    } catch (cause) {
      toast.error(t("routes.share.toast.directLinkCopyFailed"), {
        description: userMessage(cause),
      });
    }
  };

  const openFile = (file: FileEntry) => {
    if (file.kind === "folder") {
      const nextPath = joinPath(path, file.name);
      setPathIds((current) => ({ ...current, [nextPath]: file.id }));
      setPath(nextPath);
      return;
    }
    void download(file);
  };

  const createFolder = async () => {
    const name = folderName.trim();
    if (!name || !currentParentId) return;
    try {
      await apiFetch(`/v1/public/shares/${encodeURIComponent(token)}/folders`, {
        method: "POST",
        headers: jsonShareHeaders(activePassword),
        body: JSON.stringify({
          parentId: currentParentId,
          name,
          conflictPolicy: "fail",
        }),
      });
      setFolderName("");
      setFolderDialogOpen(false);
      await refreshItems();
      toast.success(t("routes.share.toast.folderCreated"));
    } catch (cause) {
      toast.error(t("routes.share.toast.folderCreateFailed"), {
        description: userMessage(cause),
      });
    }
  };

  const renameSelected = async () => {
    if (!renameFile || !renameName.trim()) return;
    try {
      const response = await apiFetch(
        `/v1/public/shares/${encodeURIComponent(token)}/files/${encodeURIComponent(renameFile.id)}`,
        {
          method: "PATCH",
          headers: {
            ...jsonShareHeaders(activePassword),
            "If-Match": `"${renameFile.generation}"`,
          },
          body: JSON.stringify({ name: renameName.trim() }),
        },
      );
      if (!response.ok) return;
      setRenameFile(undefined);
      setRenameName("");
      setSelectedKeys(new Set());
      await refreshItems();
      toast.success(t("routes.share.toast.renamed"));
    } catch (cause) {
      toast.error(t("routes.share.toast.renameFailed"), {
        description: userMessage(cause),
      });
    }
  };

  const trashSelected = async () => {
    if (!selectedFiles.length) return;
    // One refusal must not strand the entries that were already deleted, so the
    // deletions run together and the listing is refreshed for whatever the
    // server actually removed.
    const results = await Promise.allSettled(
      selectedFiles.map((file) =>
        apiFetch(
          `/v1/public/shares/${encodeURIComponent(token)}/files/${encodeURIComponent(file.id)}`,
          {
            method: "DELETE",
            headers: shareHeaders(activePassword),
          },
        ),
      ),
    );
    const removedIds = new Set(
      selectedFiles
        .filter((_, index) => results[index]?.status === "fulfilled")
        .map((file) => file.id),
    );
    setSelectedKeys((current) => {
      const ids =
        current === "all" ? selectedFiles.map((file) => file.id) : Array.from(current, String);
      return new Set(ids.filter((id) => !removedIds.has(id)));
    });
    try {
      await refreshItems();
    } catch (cause) {
      toast.error(t("routes.share.toast.trashFailed"), {
        description: userMessage(cause),
      });
    }
    const failed = selectedFiles.filter((file) => !removedIds.has(file.id));
    if (failed.length) {
      const failure = results.find((result) => result.status === "rejected");
      toast.error(
        failed.length === selectedFiles.length
          ? t("routes.share.toast.trashFailed")
          : t("routes.share.toast.trashPartial", { count: failed.length }),
        { description: failure ? userMessage(failure.reason) : undefined },
      );
      return;
    }
    toast.success(t("routes.share.toast.trashed", { count: removedIds.size }));
  };

  const uploadFile = async (file: File) => {
    if (!currentParentId || uploading) return;
    setUploading(true);
    let uploadId = "";
    try {
      const createResponse = await apiFetch(
        `/v1/public/shares/${encodeURIComponent(token)}/uploads`,
        {
          method: "POST",
          headers: {
            ...jsonShareHeaders(activePassword),
          },
          body: JSON.stringify({
            parentId: currentParentId,
            name: file.name,
            size: file.size,
            mimeType: file.type || undefined,
            modTime: new Date(file.lastModified || Date.now()).toISOString(),
            conflictPolicy: "rename",
          }),
        },
      );
      const session = (await createResponse.json()) as UploadSession;
      uploadId = session.id;
      const partSize = Math.max(1, session.partSize);
      let partNo = 1;
      for (let offset = 0; offset < file.size; offset += partSize, partNo += 1) {
        const chunk = file.slice(offset, Math.min(file.size, offset + partSize));
        await apiFetch(
          `/v1/public/shares/${encodeURIComponent(token)}/uploads/${encodeURIComponent(uploadId)}/parts/${partNo}`,
          {
            method: "PUT",
            headers: shareHeaders(activePassword),
            body: chunk,
          },
        );
      }
      await apiFetch(
        `/v1/public/shares/${encodeURIComponent(token)}/uploads/${encodeURIComponent(uploadId)}/complete`,
        {
          method: "POST",
          headers: {
            ...(shareHeaders(activePassword) ?? {}),
          },
        },
      );
      await refreshItems();
      toast.success(t("routes.share.toast.uploaded", { name: file.name }));
    } catch (cause) {
      if (uploadId) {
        void apiFetch(
          `/v1/public/shares/${encodeURIComponent(token)}/uploads/${encodeURIComponent(uploadId)}`,
          {
            method: "DELETE",
            headers: shareHeaders(activePassword),
          },
        ).catch(() => undefined);
      }
      toast.error(t("routes.share.toast.uploadFailed"), { description: userMessage(cause) });
    } finally {
      setUploading(false);
    }
  };

  return (
    <div className="flex h-dvh min-h-0 flex-col overflow-hidden bg-background text-foreground">
      <header className="shrink-0 border-b border-border bg-surface/80 backdrop-blur-xl">
        <div className="mx-auto flex h-16 w-full max-w-[1600px] items-center gap-3 px-4 sm:px-6">
          <div className="flex size-9 items-center justify-center rounded-xl bg-accent/10 text-accent">
            <LogoIcon className="size-5" />
          </div>
          <div className="min-w-0 flex-1">
            <p className="text-sm font-semibold">{t("common.app.name")}</p>
            <p className="truncate text-xs text-muted">
              {share
                ? t("routes.share.header.subtitle", {
                    name: share.file.name,
                    kind:
                      share.file.kind === "folder"
                        ? t("routes.share.kind.folder")
                        : formatFileBytes(share.file.size ?? 0),
                  })
                : t("routes.share.header.placeholder")}
            </p>
          </div>
          {share ? (
            <span className="text-xs font-medium capitalize text-muted">
              {t("routes.share.permission.access", { permission: share.permission })}
            </span>
          ) : null}
        </div>
      </header>

      <main className="flex min-h-0 w-full flex-1 overflow-hidden p-4 sm:p-6">
        {loading && !share && !needsPassword ? (
          <div className="flex min-h-72 flex-1 items-center justify-center">
            <Spinner aria-label={t("routes.share.loading")} />
          </div>
        ) : needsPassword ? (
          <div className="mx-auto mt-8 h-fit w-full max-w-md rounded-2xl border border-border bg-surface p-6 shadow-sm">
            <h1 className="text-lg font-semibold">{t("routes.share.password.title")}</h1>
            <p className="mt-1 text-sm text-muted">{t("routes.share.password.description")}</p>
            <form
              className="mt-5 space-y-4"
              onSubmit={(event) => {
                event.preventDefault();
                setActivePassword(password);
              }}
            >
              <TextField value={password} onChange={setPassword}>
                <Label>{t("common.label.password")}</Label>
                <Input type="password" autoFocus />
              </TextField>
              <Button
                type="submit"
                variant="primary"
                className="w-full"
                isDisabled={!password.trim()}
              >
                {t("routes.share.password.submit")}
              </Button>
            </form>
          </div>
        ) : share ? (
          <Page className="h-full min-h-0 max-w-7xl gap-0 overflow-x-hidden">
            <PageContent className="flex min-h-0 flex-1 overflow-x-hidden">
              <div className="flex min-h-0 min-w-0 flex-1 flex-col gap-3 overflow-hidden">
                {error ? (
                  <div className="shrink-0 rounded-xl border border-danger/30 bg-danger/10 px-4 py-3 text-sm text-danger">
                    {error}
                  </div>
                ) : null}
                <FileBrowser
                  files={items}
                  path={path}
                  rootLabel={share.file.name}
                  view={view}
                  loading={loading}
                  onNavigatePath={(nextPath) => setPath(nextPath)}
                  onViewChange={setView}
                  onOpen={openFile}
                  selection={{
                    selectedKeys,
                    onSelectionChange: setSelectedKeys,
                    onClearSelection: () => setSelectedKeys(new Set()),
                  }}
                  toolbar={
                    editable ? (
                      <>
                        <Button
                          isIconOnly
                          size="sm"
                          variant="secondary"
                          aria-label={t("routes.share.action.newFolder")}
                          onPress={() => setFolderDialogOpen(true)}
                        >
                          <PlusIcon className="size-4" />
                        </Button>
                        <FileTrigger
                          onSelect={(list) => {
                            const file = list?.item(0);
                            if (file) void uploadFile(file);
                          }}
                        >
                          <Button
                            isIconOnly
                            size="sm"
                            variant="primary"
                            aria-label={t("routes.share.action.upload")}
                            isDisabled={uploading}
                          >
                            {uploading ? <Spinner size="sm" /> : <UploadIcon className="size-4" />}
                          </Button>
                        </FileTrigger>
                      </>
                    ) : undefined
                  }
                  selectionOverlay={
                    selectedFiles.length ? (
                      <div className="pointer-events-none absolute inset-x-0 bottom-4 z-30 flex justify-center px-4">
                        <div className="pointer-events-auto flex max-w-full items-center gap-1.5 overflow-x-auto rounded-full border border-border bg-surface/95 p-1.5 shadow-xl backdrop-blur">
                          <span className="shrink-0 rounded-full bg-accent/10 px-3 py-2 text-sm font-medium text-accent">
                            {t("routes.share.selected", { count: selectedFiles.length })}
                          </span>
                          {editable && singleSelected ? (
                            <Button
                              isIconOnly
                              size="sm"
                              variant="ghost"
                              aria-label={t("routes.share.action.rename")}
                              onPress={() => {
                                setRenameFile(singleSelected);
                                setRenameName(singleSelected.name);
                              }}
                            >
                              <PencilIcon className="size-4" />
                            </Button>
                          ) : null}
                          {singleSelected?.kind === "file" ? (
                            <Button
                              isIconOnly
                              size="sm"
                              variant="ghost"
                              aria-label={t("routes.share.action.download")}
                              onPress={() => void download(singleSelected)}
                            >
                              <DownloadIcon className="size-4" />
                            </Button>
                          ) : null}
                          {singleSelected?.kind === "file" ? (
                            <Button
                              isIconOnly
                              size="sm"
                              variant="ghost"
                              aria-label={t("routes.share.action.copyLink")}
                              onPress={() => void copyDownloadLink(singleSelected)}
                            >
                              <LinkIcon className="size-4" />
                            </Button>
                          ) : null}
                          {editable ? (
                            <Button
                              isIconOnly
                              size="sm"
                              variant="danger"
                              aria-label={t("routes.share.action.trash")}
                              onPress={() => void trashSelected()}
                            >
                              <TrashIcon className="size-4" />
                            </Button>
                          ) : null}
                          <Button
                            isIconOnly
                            size="sm"
                            variant="ghost"
                            aria-label={t("routes.share.action.clearSelection")}
                            onPress={() => setSelectedKeys(new Set())}
                          >
                            <CloseIcon className="size-4" />
                          </Button>
                        </div>
                      </div>
                    ) : undefined
                  }
                  emptyHint={t("routes.share.empty")}
                />
              </div>
            </PageContent>
          </Page>
        ) : error ? (
          <div className="mx-auto mt-8 h-fit max-w-lg rounded-2xl border border-border bg-surface p-6 text-center">
            <h1 className="text-lg font-semibold">{t("routes.share.unavailable.title")}</h1>
            <p className="mt-2 text-sm text-muted">{error}</p>
          </div>
        ) : null}
      </main>

      <AppDialog
        open={folderDialogOpen}
        onOpenChange={setFolderDialogOpen}
        title={t("routes.share.folder.title")}
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
              {t("routes.share.folder.submit")}
            </Button>
          </>
        }
      >
        <TextField value={folderName} onChange={setFolderName}>
          <Label>{t("routes.share.folder.name")}</Label>
          <Input autoFocus placeholder={t("routes.share.folder.placeholder")} />
        </TextField>
      </AppDialog>

      <AppDialog
        open={Boolean(renameFile)}
        onOpenChange={(open) => {
          if (!open) setRenameFile(undefined);
        }}
        title={t("routes.share.rename.title")}
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
          <Label>{t("routes.share.rename.label")}</Label>
          <Input autoFocus />
        </TextField>
      </AppDialog>
    </div>
  );
}

function shareHeaders(password: string): HeadersInit | undefined {
  return password ? { "X-Share-Password": password } : undefined;
}

function jsonShareHeaders(password: string): Record<string, string> {
  return {
    "Content-Type": "application/json",
    ...(password ? { "X-Share-Password": password } : {}),
  };
}

function joinPath(parent: string, name: string) {
  return `${parent === "/" ? "" : parent}/${name}`.replace(/\/+/g, "/") || "/";
}
