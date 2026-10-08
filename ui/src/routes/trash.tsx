import { Button, Card, Chip, Spinner } from "@heroui/react";
import { createFileRoute } from "@tanstack/react-router";
import { useState } from "react";
import { toast } from "sonner";
import FolderIcon from "~icons/gravity-ui/folder";
import RefreshIcon from "~icons/gravity-ui/arrow-rotate-left";
import RestoreIcon from "~icons/gravity-ui/arrow-rotate-left";
import TrashIcon from "~icons/gravity-ui/trash-bin";
import { userMessage } from "@/api/errors";
import type { FileEntry } from "@/api/types";
import { ConfirmDialog } from "@/components/dialogs/confirm-dialog";
import { FileTypeIcon } from "@/features/files/file-type-icon";
import { EmptyState, Page, PageContent, PageHeader } from "@/components/page";
import { useFileActions } from "@/features/files/mutations";
import { useFilePage } from "@/features/files/queries";
import { useI18n, type MessageKey } from "@/lib/i18n";

export const Route = createFileRoute("/trash")({
  component: TrashPage,
  pendingComponent: () => (
    <div className="flex min-h-[40vh] items-center justify-center">
      <Spinner size="lg" />
    </div>
  ),
});

const KIND_LABEL_KEYS: Record<string, MessageKey | undefined> = {
  file: "routes.trash.kind.file",
  folder: "routes.trash.kind.folder",
};

function TrashPage() {
  const { t } = useI18n();
  const query = useFilePage(
    {
      path: "/",
      sort: "updatedAt",
      order: "desc",
      view: "list",
    },
    "trashed",
  );
  const actions = useFileActions();
  const [purging, setPurging] = useState<FileEntry>();
  const [cleaningTrash, setCleaningTrash] = useState(false);
  const trashedFiles = query.data.pages.flatMap((page) => page.items);

  const restore = async (file: FileEntry) => {
    try {
      await actions.restore(file.id);
      toast.success(t("routes.trash.toast.restored", { name: file.name }));
    } catch (error) {
      toast.error(t("routes.trash.toast.restoreFailed"), { description: userMessage(error) });
    }
  };

  const purge = async (file: FileEntry) => {
    try {
      await actions.purge(file.id);
      toast.success(t("routes.trash.toast.purged", { name: file.name }));
    } catch (error) {
      toast.error(t("routes.trash.toast.purgeFailed"), { description: userMessage(error) });
    }
  };

  const cleanTrash = async () => {
    try {
      await actions.cleanTrash();
      toast.success(t("routes.trash.toast.cleaned"));
    } catch (error) {
      toast.error(t("routes.trash.toast.cleanFailed"), { description: userMessage(error) });
    }
  };

  return (
    <Page>
      <PageHeader
        title={t("routes.trash.title")}
        description={t("routes.trash.description")}
        actions={
          <div className="flex items-center gap-2">
            <Button
              size="sm"
              variant="danger"
              isDisabled={trashedFiles.length === 0 || actions.pending}
              onPress={() => setCleaningTrash(true)}
            >
              <TrashIcon className="size-4" /> {t("routes.trash.action.clean")}
            </Button>
            <Button
              size="sm"
              variant="tertiary"
              isDisabled={actions.pending}
              onPress={() => void query.refetch()}
            >
              <RefreshIcon className="size-4" /> {t("common.action.refresh")}
            </Button>
          </div>
        }
      />
      <PageContent>
        {trashedFiles.length === 0 ? (
          <EmptyState
            title={t("routes.trash.empty.title")}
            description={t("routes.trash.empty.description")}
          />
        ) : (
          <Card className="gap-0 overflow-hidden border border-border bg-surface/80 shadow-sm">
            {trashedFiles.map((file) => {
              const kindKey = KIND_LABEL_KEYS[file.kind];
              return (
                <div
                  key={file.id}
                  className="grid min-h-16 gap-3 border-b border-border px-4 py-3 last:border-b-0 sm:grid-cols-[minmax(0,1fr)_8rem_auto] sm:items-center"
                >
                  <div className="flex min-w-0 items-center gap-3">
                    <div className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-default/20 text-muted">
                      {file.kind === "folder" ? (
                        <FolderIcon className="size-4" />
                      ) : (
                        <FileTypeIcon file={file} className="size-4" />
                      )}
                    </div>
                    <div className="min-w-0">
                      <p className="truncate text-sm font-medium">{file.name}</p>
                      <p className="mt-0.5 text-xs text-muted">
                        {t("routes.trash.deletedAt", {
                          date: new Date(file.updatedAt).toLocaleString(),
                        })}
                      </p>
                    </div>
                  </div>
                  <Chip size="sm" variant="tertiary" className="w-fit capitalize">
                    {kindKey ? t(kindKey) : file.kind}
                  </Chip>
                  <div className="flex justify-end gap-2">
                    <Button
                      size="sm"
                      variant="secondary"
                      isDisabled={actions.pending}
                      onPress={() => void restore(file)}
                    >
                      <RestoreIcon className="size-4" /> {t("routes.trash.action.restore")}
                    </Button>
                    <Button
                      size="sm"
                      variant="danger"
                      isDisabled={actions.pending}
                      onPress={() => setPurging(file)}
                    >
                      <TrashIcon className="size-4" /> {t("routes.trash.action.deleteForever")}
                    </Button>
                  </div>
                </div>
              );
            })}
          </Card>
        )}
        {query.hasNextPage ? (
          <div className="mt-4 flex justify-center">
            <Button
              size="sm"
              variant="tertiary"
              isPending={query.isFetchingNextPage}
              onPress={() => void query.fetchNextPage()}
            >
              {t("routes.trash.loadMore")}
            </Button>
          </div>
        ) : null}
      </PageContent>

      <ConfirmDialog
        open={Boolean(purging)}
        onOpenChange={(open) => {
          if (!open) setPurging(undefined);
        }}
        title={t("routes.trash.confirm.purge.title")}
        message={t("routes.trash.confirm.purge.message")}
        confirmLabel={t("routes.trash.action.deleteForever")}
        isPending={actions.pending}
        onConfirm={() => {
          if (!purging) return;
          void purge(purging).finally(() => setPurging(undefined));
        }}
      />

      <ConfirmDialog
        open={cleaningTrash}
        onOpenChange={setCleaningTrash}
        title={t("routes.trash.confirm.clean.title")}
        message={t("routes.trash.confirm.clean.message")}
        confirmLabel={t("routes.trash.action.clean")}
        isPending={actions.pending}
        onConfirm={() => {
          void cleanTrash().finally(() => setCleaningTrash(false));
        }}
      />
    </Page>
  );
}
