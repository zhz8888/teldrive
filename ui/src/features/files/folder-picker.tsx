import { Button, Spinner } from "@heroui/react";
import { useRef, useState } from "react";
import { GridList, GridListItem } from "react-aria-components";
import ChevronIcon from "~icons/gravity-ui/chevron-right";
import FolderIcon from "~icons/gravity-ui/folder";
import HomeIcon from "~icons/gravity-ui/house";
import { useI18n } from "@/lib/i18n";
import { useFolderChildren } from "./queries";

/**
 * Breadcrumb-driven folder browser used to choose a destination. It walks the drive one
 * folder at a time (the current folder's children come from {@link useFolderChildren}) and
 * reports the chosen folder through `onConfirm`. `requireFolder` keeps the confirm button
 * disabled until a folder id is known, which the drive root never has.
 */
export function FolderPicker({
  initialPath = "/",
  initialParentId,
  onConfirm,
  confirmLabel,
  isDisabled = false,
  requireFolder = false,
}: {
  initialPath?: string;
  initialParentId?: string;
  onConfirm: (parentId?: string, path?: string) => void;
  confirmLabel?: string;
  isDisabled?: boolean;
  requireFolder?: boolean;
}) {
  const { t } = useI18n();
  const [path, setPath] = useState(initialPath);
  const [parentId, setParentId] = useState<string | undefined>(initialParentId);
  // Paths are remembered with the folder id they resolved to, because the API lists by id
  // and a breadcrumb click only has the path.
  const folderIds = useRef(new Map<string, string | undefined>([[initialPath, initialParentId]]));
  // A folder is listed by id when one is known and by path otherwise; the drive root has
  // neither.
  const folders = useFolderChildren(parentId, parentId ? undefined : path);
  const folderItems = folders.data?.pages.flatMap((page) => page.items) ?? [];
  const crumbs = path.split("/").filter(Boolean);

  const openPath = (nextPath: string, nextParentId?: string) => {
    if (nextParentId) folderIds.current.set(nextPath, nextParentId);
    setPath(nextPath);
    setParentId(nextParentId ?? folderIds.current.get(nextPath));
  };

  return (
    <div className="grid gap-3">
      <nav
        aria-label={t("features.folderPicker.destination")}
        className="flex min-w-0 items-center gap-1 overflow-x-auto text-sm"
      >
        <Button
          isIconOnly
          size="sm"
          variant="ghost"
          aria-label={t("features.folderPicker.driveRoot")}
          onPress={() => openPath("/")}
        >
          <HomeIcon className="size-4" />
        </Button>
        {crumbs.map((crumb, index) => {
          const crumbPath = `/${crumbs.slice(0, index + 1).join("/")}`;
          return (
            <span key={crumbPath} className="flex min-w-0 items-center gap-1">
              <ChevronIcon className="size-3 shrink-0 text-muted" />
              <Button
                size="sm"
                variant="ghost"
                className="min-w-0"
                onPress={() => openPath(crumbPath)}
              >
                <span className="truncate">{crumb}</span>
              </Button>
            </span>
          );
        })}
      </nav>

      <div className="min-h-56 max-h-80 overflow-auto rounded-xl border border-border bg-background/40">
        {folders.isLoading ? (
          <div className="grid min-h-56 place-items-center">
            <Spinner size="lg" />
          </div>
        ) : folders.isError ? (
          <div className="grid min-h-56 place-items-center gap-3 p-6 text-center">
            <p className="text-sm text-danger">{t("features.folderPicker.loadFailed")}</p>
            <Button size="sm" onPress={() => void folders.refetch()}>
              {t("common.action.retry")}
            </Button>
          </div>
        ) : (
          <>
            <GridList
              aria-label={t("features.folderPicker.folders")}
              items={folderItems}
              selectionMode="none"
              renderEmptyState={() => (
                <div className="grid min-h-52 place-items-center text-sm text-muted">
                  {t("features.folderPicker.empty")}
                </div>
              )}
              className="outline-none"
            >
              {(folder) => (
                <GridListItem
                  id={folder.id}
                  textValue={folder.name}
                  onAction={() => openPath(joinPath(path, folder.name), folder.id)}
                  className={({ isFocusVisible }) =>
                    [
                      "flex min-h-11 items-center gap-3 border-b border-border px-3 text-sm outline-none last:border-b-0 hover:bg-default/20",
                      isFocusVisible && "ring-2 ring-inset ring-accent/30",
                    ]
                      .filter(Boolean)
                      .join(" ")
                  }
                >
                  <FolderIcon className="size-4 text-accent" />
                  <span className="truncate">{folder.name}</span>
                </GridListItem>
              )}
            </GridList>
            {folders.hasNextPage ? (
              <div className="border-border border-t p-2">
                <Button
                  size="sm"
                  variant="tertiary"
                  className="w-full"
                  isPending={folders.isFetchingNextPage}
                  onPress={() => void folders.fetchNextPage()}
                >
                  {t("features.folderPicker.loadMore")}
                </Button>
              </div>
            ) : null}
          </>
        )}
      </div>

      <div className="flex justify-end">
        <Button
          variant="primary"
          isDisabled={
            isDisabled || folders.isLoading || folders.isError || (requireFolder && !parentId)
          }
          onPress={() => onConfirm(parentId, path)}
        >
          {confirmLabel ?? t("features.folderPicker.confirm")}
        </Button>
      </div>
    </div>
  );
}

/** Appends a folder name to a path and collapses repeated slashes; root stays "/". */
function joinPath(parent: string, name: string) {
  return `${parent === "/" ? "" : parent}/${name}`.replace(/\/+/g, "/") || "/";
}
