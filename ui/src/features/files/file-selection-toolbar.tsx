import { Button } from "@heroui/react";
import DownloadIcon from "~icons/gravity-ui/arrow-down-to-line";
import CopyIcon from "~icons/gravity-ui/copy";
import CopyLinkIcon from "~icons/gravity-ui/copy-arrow-right";
import MoveIcon from "~icons/gravity-ui/folder-arrow-right";
import LinkIcon from "~icons/gravity-ui/link";
import PencilIcon from "~icons/gravity-ui/pencil";
import CutIcon from "~icons/gravity-ui/scissors";
import TrashIcon from "~icons/gravity-ui/trash-bin";
import CloseIcon from "~icons/gravity-ui/xmark";
import type { FileEntry } from "../../api/types";
import { useI18n } from "../../lib/i18n";

/**
 * Floating action bar for the current selection. It renders only the actions the
 * page handed it, which is how the drive search keeps folder-bound operations
 * (cut, duplicate, paste) out of its results, and it renders the single-item
 * actions only when exactly one entry is selected.
 */
export function FileSelectionToolbar({
  selectedFiles,
  pending,
  onCut,
  onCopy,
  onRename,
  onDuplicate,
  onShare,
  onDownload,
  onCopyDownloadLinks,
  onMove,
  onTrash,
  onClear,
}: {
  selectedFiles: FileEntry[];
  pending: boolean;
  onCut?: () => void;
  onCopy?: () => void;
  onRename?: () => void;
  onDuplicate?: () => void;
  onShare?: () => void;
  onDownload?: () => void;
  onCopyDownloadLinks?: () => void;
  onMove?: () => void;
  onTrash?: () => void;
  onClear?: () => void;
}) {
  const { t } = useI18n();
  if (selectedFiles.length === 0) return null;
  const selectedCount = selectedFiles.length;
  const singleSelectedFile = selectedCount === 1 ? selectedFiles[0] : undefined;
  const selectedOnlyFiles = selectedFiles.every((file) => file.kind === "file");

  return (
    <div className="pointer-events-none absolute inset-x-0 bottom-4 z-30 flex justify-center px-4">
      <fieldset
        aria-label={t("routes.files.selection.label")}
        className="pointer-events-auto flex max-w-full items-center gap-1.5 overflow-x-auto rounded-full border border-border bg-surface/95 p-1.5 shadow-xl backdrop-blur"
      >
        <span className="shrink-0 rounded-full bg-accent/10 px-3 py-2 text-sm font-medium text-accent">
          {t("routes.files.selected", { count: selectedCount })}
        </span>
        {onCut ? (
          <Button
            isIconOnly
            size="sm"
            variant="ghost"
            aria-label={t("routes.files.action.cut")}
            isDisabled={pending}
            onPress={onCut}
          >
            <CutIcon className="size-4" />
          </Button>
        ) : null}
        {onCopy ? (
          <Button
            isIconOnly
            size="sm"
            variant="ghost"
            aria-label={t("routes.files.action.copy")}
            isDisabled={pending}
            onPress={onCopy}
          >
            <CopyIcon className="size-4" />
          </Button>
        ) : null}
        {singleSelectedFile && onRename ? (
          <Button
            isIconOnly
            size="sm"
            variant="ghost"
            aria-label={t("routes.files.action.rename")}
            isDisabled={pending}
            onPress={onRename}
          >
            <PencilIcon className="size-4" />
          </Button>
        ) : null}
        {singleSelectedFile && onDuplicate ? (
          <Button
            isIconOnly
            size="sm"
            variant="ghost"
            aria-label={t("routes.files.action.duplicate")}
            isDisabled={pending}
            onPress={onDuplicate}
          >
            <CopyIcon className="size-4" />
          </Button>
        ) : null}
        {singleSelectedFile && onShare ? (
          <Button
            isIconOnly
            size="sm"
            variant="ghost"
            aria-label={t("routes.files.action.share")}
            isDisabled={pending}
            onPress={onShare}
          >
            <LinkIcon className="size-4" />
          </Button>
        ) : null}
        {singleSelectedFile?.kind === "file" && onDownload ? (
          <Button
            isIconOnly
            size="sm"
            variant="ghost"
            aria-label={t("routes.files.action.download")}
            isDisabled={pending}
            onPress={onDownload}
          >
            <DownloadIcon className="size-4" />
          </Button>
        ) : null}
        {selectedOnlyFiles && onCopyDownloadLinks ? (
          <Button
            isIconOnly
            size="sm"
            variant="ghost"
            aria-label={t(
              selectedCount === 1
                ? "routes.files.action.copyLinkOne"
                : "routes.files.action.copyLinkMany",
            )}
            onPress={onCopyDownloadLinks}
            isDisabled={pending}
          >
            <CopyLinkIcon className="size-4" />
          </Button>
        ) : null}
        {onMove ? (
          <Button
            isIconOnly
            size="sm"
            variant="ghost"
            aria-label={t("routes.files.action.move")}
            isDisabled={pending}
            onPress={onMove}
          >
            <MoveIcon className="size-4" />
          </Button>
        ) : null}
        {onTrash ? (
          <Button
            isIconOnly
            size="sm"
            variant="danger"
            aria-label={t("routes.files.action.trash")}
            isDisabled={pending}
            onPress={onTrash}
          >
            <TrashIcon className="size-4" />
          </Button>
        ) : null}
        {onClear ? (
          <Button
            isIconOnly
            size="sm"
            variant="ghost"
            aria-label={t("routes.files.action.clearSelection")}
            isDisabled={pending}
            onPress={onClear}
          >
            <CloseIcon className="size-4" />
          </Button>
        ) : null}
      </fieldset>
    </div>
  );
}
