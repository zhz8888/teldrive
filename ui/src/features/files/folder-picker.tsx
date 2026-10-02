import { Button, Spinner } from "@heroui/react";
import { useState } from "react";
import { GridList, GridListItem } from "react-aria-components";
import ChevronIcon from "~icons/gravity-ui/chevron-right";
import FolderIcon from "~icons/gravity-ui/folder";
import HomeIcon from "~icons/gravity-ui/house";
import { useI18n } from "@/lib/i18n";
import { useFolderChildren } from "./queries";

export function FolderPicker({
  initialPath = "/",
  initialParentId,
  onConfirm,
  confirmLabel,
}: {
  initialPath?: string;
  initialParentId?: string;
  onConfirm: (parentId?: string, path?: string) => void;
  confirmLabel?: string;
}) {
  const { t } = useI18n();
  const [path, setPath] = useState(initialPath);
  const [parentId, setParentId] = useState<string | undefined>(initialParentId);
  const folders = useFolderChildren(parentId, parentId ? undefined : path);
  const folderItems = folders.data?.pages.flatMap((page) => page.items) ?? [];
  const crumbs = path.split("/").filter(Boolean);

  const openPath = (nextPath: string, nextParentId?: string) => {
    setPath(nextPath);
    setParentId(nextParentId);
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
        <Button variant="primary" onPress={() => onConfirm(parentId, path)}>
          {confirmLabel ?? t("features.folderPicker.confirm")}
        </Button>
      </div>
    </div>
  );
}

function joinPath(parent: string, name: string) {
  return `${parent === "/" ? "" : parent}/${name}`.replace(/\/+/g, "/") || "/";
}
