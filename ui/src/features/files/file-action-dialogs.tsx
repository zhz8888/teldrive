import { Button, Input, Label, TextField } from "@heroui/react";
import type { FileEntry } from "../../api/types";
import { AppDialog } from "../../components/dialogs/app-dialog";
import { FilePreviewDialog } from "../../components/file-preview-dialog";
import { FolderPicker } from "./folder-picker";
import { ShareDialog } from "./share-dialog";
import { useI18n } from "../../lib/i18n";

/**
 * Groups the dialogs a file action opens — rename, move or copy, share and
 * preview — so the page keeps one piece of state per dialog and this component
 * only decides how they are worded and which one is on top. Rename and the
 * destination picker stay open while their request runs, because a failure is
 * reported inside the dialog that caused it rather than as a toast.
 */
export function FileActionDialogs({
  renameFile,
  renameName,
  onRenameNameChange,
  onRenameClose,
  onRename,
  pending,
  error,
  destinationAction,
  shareFile,
  onShareClose,
  previewFile,
  onPreviewClose,
}: {
  renameFile?: FileEntry;
  renameName: string;
  onRenameNameChange: (name: string) => void;
  onRenameClose: () => void;
  onRename: () => void;
  pending: boolean;
  error?: string;
  destinationAction?: {
    mode: "move" | "copy";
    count: number;
    onClose: () => void;
    onConfirm: (parentId?: string) => void;
  };
  shareFile?: FileEntry;
  onShareClose: () => void;
  previewFile?: FileEntry;
  onPreviewClose: () => void;
}) {
  const { t } = useI18n();
  return (
    <>
      <AppDialog
        open={Boolean(renameFile)}
        onOpenChange={(open) => {
          if (!open) onRenameClose();
        }}
        title={t("routes.files.rename.title")}
        isDismissable={!pending}
        isCloseDisabled={pending}
        size="md"
        footer={
          <>
            <Button variant="secondary" isDisabled={pending} onPress={onRenameClose}>
              {t("common.action.cancel")}
            </Button>
            <Button variant="primary" isDisabled={!renameName.trim() || pending} onPress={onRename}>
              {t("common.action.rename")}
            </Button>
          </>
        }
      >
        <TextField
          isDisabled={pending}
          autoFocus
          value={renameName}
          onChange={onRenameNameChange}
          onKeyDown={(event) => {
            if (event.key === "Enter" && renameName.trim() && !pending) {
              event.preventDefault();
              onRename();
            } else event.continuePropagation();
          }}
        >
          <Label>{t("routes.files.rename.label")}</Label>
          <Input />
        </TextField>
        {error && (
          <p role="alert" className="text-sm text-danger">
            {error}
          </p>
        )}
      </AppDialog>

      {destinationAction ? (
        <AppDialog
          open
          onOpenChange={(open) => {
            if (!open) destinationAction.onClose();
          }}
          title={t(
            destinationAction.mode === "move"
              ? "routes.files.move.title"
              : "routes.files.copy.title",
            { count: destinationAction.count },
          )}
          description={t("routes.files.move.description")}
          isDismissable={!pending}
          isCloseDisabled={pending}
        >
          {error && (
            <p role="alert" className="mb-3 text-sm text-danger">
              {error}
            </p>
          )}
          <FolderPicker
            initialPath="/"
            confirmLabel={t(
              destinationAction.mode === "move"
                ? "features.folderPicker.confirm"
                : "routes.files.copy.confirm",
            )}
            isDisabled={pending}
            onConfirm={(parentId) => destinationAction.onConfirm(parentId)}
          />
        </AppDialog>
      ) : null}

      <ShareDialog
        file={shareFile}
        onOpenChange={(open) => {
          if (!open) onShareClose();
        }}
      />
      <FilePreviewDialog
        file={previewFile}
        onOpenChange={(open) => {
          if (!open) onPreviewClose();
        }}
      />
    </>
  );
}
