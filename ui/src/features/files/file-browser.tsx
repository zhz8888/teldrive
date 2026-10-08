import { Button, Card, Checkbox, Spinner } from "@heroui/react";
import type { ReactNode } from "react";
import {
  Collection,
  GridLayout,
  GridList,
  GridListItem,
  GridListLoadMoreItem,
  ListLayout,
  type Selection,
  Virtualizer,
} from "react-aria-components";
import type { FileEntry } from "@/api/types";
import BackIcon from "~icons/gravity-ui/arrow-left";
import UpIcon from "~icons/gravity-ui/chevron-up";
import FolderIcon from "~icons/gravity-ui/folder";
import GridIcon from "~icons/gravity-ui/layout-cells";
import ListIcon from "~icons/gravity-ui/list-ul";
import { FileTypeIcon } from "./file-type-icon";
import { useI18n } from "@/lib/i18n";

/** Layout of the entry list: stacked rows or an auto-filling grid of cards. */
export type FileBrowserView = "list" | "grid";

/** Everything the browser needs to render a listing; it owns no data or route state. */
type FileBrowserProps = {
  /** Entries to render, in the order the caller wants them shown. */
  files: FileEntry[];
  /** Folder the list represents; "/" is the drive root and disables the up button. */
  path: string;
  /** Label of the root crumb, e.g. the drive or share name. */
  rootLabel: string;
  /** Layout the caller has stored for this pane. */
  view: FileBrowserView;
  /** Replaces the list with a spinner; used for the first load of a listing. */
  loading: boolean;
  /** Navigates to a breadcrumb or up-button target. */
  onNavigatePath: (path: string) => void;
  /** Requests a layout switch; the caller persists it. */
  onViewChange: (view: FileBrowserView) => void;
  /** Called on double-click/Enter of an entry, or a single click in grid view. */
  onOpen: (file: FileEntry) => void;
  /** Controls rendered in the header, next to the view toggles. */
  toolbar?: ReactNode;
  /** When set, shows a back button before the breadcrumb. */
  onBack?: () => void;
  /**
   * Selection wiring. Omitting it makes the list read-only: rows are not selectable and
   * no checkboxes are rendered.
   */
  selection?: {
    selectedKeys: Selection;
    onSelectionChange: (selection: Selection) => void;
    /** Called when the click lands outside any row, so the page can drop the selection. */
    onClearSelection: () => void;
  };
  /** Floating bar rendered over the list (selection actions); the list pads for it. */
  selectionOverlay?: ReactNode;
  /** Disables selecting every row without hiding the checkboxes. */
  selectionDisabled?: boolean;

  /** Entries drawn at reduced opacity, e.g. the ones a pending "cut" would move. */
  dimmedIds?: ReadonlySet<string>;
  /** Enables the load-more row at the end of the list. */
  hasNextPage?: boolean;
  /** Shows the load-more row's spinner while the next page is in flight. */
  isLoadingMore?: boolean;
  /** Requests the next page; ignored when omitted. */
  onLoadMore?: () => void;
  /** Second line of the empty state. */
  emptyHint?: string;
  /** Headline of the empty state; the folder wording is the default. */
  emptyTitle?: string;
  /**
   * Hides the up button and the breadcrumb. Search results are not a folder, so
   * the drive search turns them off and shows the location of each hit instead.
   */
  hideFolderControls?: boolean;
  /** Shows the folder each entry lives in, in place of its size column. */
  showLocations?: boolean;
  /** Opens the folder of an entry and closes the overlay around it. */
  onOpenContainingFolder?: (file: FileEntry) => void;
};

/**
 * Renders one folder or result list with its header controls, its empty state and
 * the selection overlay passed by the page. It owns no data: the caller decides
 * what is listed, which entries are dimmed and what a selection can do, so the
 * same component serves the drive, share and search screens.
 */
export function FileBrowser({
  files,
  path,
  rootLabel,
  view,
  loading,
  onNavigatePath,
  onViewChange,
  onOpen,
  toolbar,
  onBack,
  selection,
  selectionOverlay,
  selectionDisabled = false,

  dimmedIds,
  hasNextPage = false,
  isLoadingMore = false,
  onLoadMore,
  emptyHint,
  emptyTitle,
  hideFolderControls = false,
  showLocations = false,
  onOpenContainingFolder,
}: FileBrowserProps) {
  const { t } = useI18n();
  return (
    <Card className="@container/file-browser relative flex min-h-0 min-w-0 flex-1 flex-col gap-0 overflow-hidden border border-border bg-surface/80 shadow-sm">
      <Card.Header className="shrink-0 border-b border-border px-3 py-3 sm:px-4">
        <div className="flex min-w-0 items-center gap-1.5">
          {onBack ? (
            <Button
              isIconOnly
              size="sm"
              variant="ghost"
              aria-label={t("common.action.back")}
              onPress={onBack}
            >
              <BackIcon className="size-4" />
            </Button>
          ) : null}
          {!hideFolderControls && (
            <Button
              isIconOnly
              size="sm"
              variant="ghost"
              aria-label={t("features.fileBrowser.upOneFolder")}
              isDisabled={path === "/"}
              onPress={() => {
                const parts = path.split("/").filter(Boolean);
                const parentPath = parts.length <= 1 ? "/" : `/${parts.slice(0, -1).join("/")}`;
                onNavigatePath(parentPath);
              }}
            >
              <UpIcon className="size-4" />
            </Button>
          )}
          {!hideFolderControls && (
            <div className="min-w-0 flex-1 overflow-hidden">
              <FileBrowserBreadcrumb
                path={path}
                rootLabel={rootLabel}
                onNavigatePath={onNavigatePath}
              />
            </div>
          )}
          <div className="flex shrink-0 items-center gap-1">
            {toolbar}
            <Button
              isIconOnly
              size="sm"
              variant={view === "list" ? "secondary" : "ghost"}
              aria-label={t("features.fileBrowser.listView")}
              onPress={() => onViewChange("list")}
            >
              <ListIcon className="size-4" />
            </Button>
            <Button
              isIconOnly
              size="sm"
              variant={view === "grid" ? "secondary" : "ghost"}
              aria-label={t("features.fileBrowser.gridView")}
              onPress={() => onViewChange("grid")}
            >
              <GridIcon className="size-4" />
            </Button>
          </div>
        </div>
      </Card.Header>
      <Card.Content className="min-h-0 flex-1 overflow-hidden p-0">
        {loading ? (
          <div className="flex h-full min-h-64 items-center justify-center">
            <Spinner size="lg" />
          </div>
        ) : (
          <div className="h-full min-h-0">
            <FileCollection
              files={files}
              view={view}
              selection={selection}
              selectionDisabled={selectionDisabled}
              onOpen={onOpen}
              dimmedIds={dimmedIds}
              hasNextPage={hasNextPage}
              isLoadingMore={isLoadingMore}
              onLoadMore={onLoadMore}
              emptyHint={emptyHint ?? t("features.fileBrowser.emptyHint")}
              emptyTitle={emptyTitle ?? t("features.fileBrowser.emptyTitle")}
              showLocations={showLocations}
              onOpenContainingFolder={onOpenContainingFolder}
            />
          </div>
        )}
      </Card.Content>
      {selectionOverlay}
    </Card>
  );
}

/**
 * The scrollable list itself: virtualized, switchable between grid and list rows, with an
 * empty state and a load-more row. The `dependencies` array tells the collection to
 * re-render rows when the highlight set or the location props change, because those are
 * read through closures rather than passed to the row.
 */
function FileCollection({
  files,
  view,
  selection,
  selectionDisabled,
  onOpen,

  dimmedIds,
  hasNextPage,
  isLoadingMore,
  onLoadMore,
  emptyHint,
  emptyTitle,
  showLocations,
  onOpenContainingFolder,
}: {
  files: FileEntry[];
  view: FileBrowserView;
  selection?: FileBrowserProps["selection"];
  selectionDisabled: boolean;
  onOpen: (file: FileEntry) => void;

  dimmedIds?: ReadonlySet<string>;
  hasNextPage: boolean;
  isLoadingMore: boolean;
  onLoadMore?: () => void;
  emptyHint: string;
  emptyTitle: string;
  showLocations: boolean;
  onOpenContainingFolder?: (file: FileEntry) => void;
}) {
  const { t } = useI18n();
  const grid = view === "grid";
  const selectedKeys = selection?.selectedKeys ?? new Set<React.Key>();
  const hasSelection = selection ? selectedKeys === "all" || selectedKeys.size > 0 : false;

  return (
    <Virtualizer key={view} layout={grid ? GridLayout : ListLayout}>
      <GridList
        aria-label={t("features.fileBrowser.filesAndFolders")}
        layout={grid ? "grid" : "stack"}
        selectionMode={selection ? "multiple" : "none"}
        selectionBehavior="replace"
        selectedKeys={selection?.selectedKeys}
        onSelectionChange={selection?.onSelectionChange}
        disabledKeys={selectionDisabled ? files.map((file) => file.id) : undefined}
        disabledBehavior="selection"
        onClick={(event) => {
          if (!selection) return;
          const target = event.target as HTMLElement;
          if (!target.closest('[role="row"]')) selection.onClearSelection();
        }}
        onAction={(key) => {
          const file = files.find((item) => item.id === key);
          if (file) onOpen(file);
        }}
        renderEmptyState={() => (
          <div className="flex min-h-80 flex-col items-center justify-center px-6 text-center">
            <div className="mb-3 flex size-11 items-center justify-center rounded-xl bg-default/30 text-muted">
              <FolderIcon className="size-5" />
            </div>
            <p className="text-sm font-medium">
              {emptyTitle ?? t("features.fileBrowser.emptyTitle")}
            </p>
            <p className="mt-1 text-xs text-muted">{emptyHint}</p>
          </div>
        )}
        className={
          grid
            ? `grid h-full min-h-0 grid-cols-[repeat(auto-fill,minmax(8.5rem,1fr))] gap-2 overflow-x-hidden overflow-y-auto p-2 outline-none sm:grid-cols-[repeat(auto-fill,minmax(11rem,1fr))] sm:gap-3 sm:p-4 ${hasSelection ? "pb-24 sm:pb-24" : ""}`
            : `h-full min-h-0 overflow-x-hidden overflow-y-auto outline-none ${hasSelection ? "pb-24" : ""}`
        }
      >
        <Collection items={files} dependencies={[dimmedIds, showLocations, onOpenContainingFolder]}>
          {(file) => (
            <GridListItem
              id={file.id}
              textValue={file.name}
              className={(state) =>
                grid
                  ? [
                      "group flex min-h-36 cursor-default flex-col justify-between rounded-xl border border-border bg-background/35 p-3 outline-none transition",
                      "hover:-translate-y-0.5 hover:border-accent/30 hover:shadow-md",
                      state.isSelected && "border-accent bg-accent/10",

                      dimmedIds?.has(file.id) && "opacity-45",
                      state.isFocusVisible && "ring-2 ring-accent/30",
                    ]
                      .filter(Boolean)
                      .join(" ")
                  : [
                      "group grid min-h-14 cursor-default grid-cols-[minmax(0,1fr)] items-center gap-2 border-b border-border px-3 outline-none transition last:border-b-0 sm:grid-cols-[minmax(0,1fr)_6rem] sm:px-4 lg:grid-cols-[minmax(0,1fr)_8rem_10rem] lg:gap-4",
                      "hover:bg-default/20",
                      state.isSelected && "bg-accent/10",

                      dimmedIds?.has(file.id) && "opacity-45",
                      state.isFocusVisible && "ring-2 ring-inset ring-accent/30",
                    ]
                      .filter(Boolean)
                      .join(" ")
              }
            >
              {(state) =>
                grid ? (
                  <GridFile
                    file={file}
                    selectable={Boolean(selection)}
                    showCheckbox={state.isHovered || state.isFocusVisible || state.isSelected}
                    location={showLocations ? file.parentPath : undefined}
                    onOpenContainingFolder={
                      onOpenContainingFolder ? () => onOpenContainingFolder(file) : undefined
                    }
                  />
                ) : (
                  <ListFile
                    file={file}
                    selectable={Boolean(selection)}
                    showCheckbox={state.isHovered || state.isFocusVisible || state.isSelected}
                    location={showLocations ? file.parentPath : undefined}
                    onOpenContainingFolder={
                      onOpenContainingFolder ? () => onOpenContainingFolder(file) : undefined
                    }
                  />
                )
              }
            </GridListItem>
          )}
        </Collection>
        {hasNextPage ? (
          <GridListLoadMoreItem
            onLoadMore={() => onLoadMore?.()}
            isLoading={isLoadingMore}
            className="flex min-h-14 items-center justify-center p-4"
          >
            <Spinner size="sm" />
          </GridListLoadMoreItem>
        ) : null}
      </GridList>
    </Virtualizer>
  );
}

/**
 * Row checkbox. It occupies the row's `selection` slot, which is what positions it, and
 * stays invisible until the row is hovered, focused or selected — except on touch devices,
 * where the hover media query never matches and it is always shown.
 */
function SelectionCheckbox({ file, isVisible }: { file: FileEntry; isVisible: boolean }) {
  const { t } = useI18n();
  return (
    <Checkbox
      slot="selection"
      aria-label={t("features.fileBrowser.selectFile", { name: file.name })}
      className={({ isSelected }) =>
        [
          "shrink-0 transition-opacity",
          isSelected || isVisible ? "opacity-100" : "opacity-0 [@media(hover:none)]:opacity-100",
        ].join(" ")
      }
    >
      <Checkbox.Content>
        <Checkbox.Control>
          <Checkbox.Indicator />
        </Checkbox.Control>
      </Checkbox.Content>
    </Checkbox>
  );
}

/**
 * Grid-view card content: icon and checkbox on top, name, optional location and the size
 * (or the word "Folder") below. The row's own action handler opens the entry, so nothing
 * here is clickable except the location button.
 */
function GridFile({
  file,
  selectable,
  showCheckbox,
  location,
  onOpenContainingFolder,
}: {
  file: FileEntry;
  selectable: boolean;
  showCheckbox: boolean;
  location?: string;
  onOpenContainingFolder?: () => void;
}) {
  const { t } = useI18n();
  return (
    <>
      <div className="flex items-start gap-2">
        {selectable ? <SelectionCheckbox file={file} isVisible={showCheckbox} /> : null}
        <div className="flex size-11 items-center justify-center rounded-xl bg-accent/10 text-accent">
          {file.kind === "folder" ? (
            <FolderIcon className="size-5" />
          ) : (
            <FileTypeIcon file={file} className="size-5" />
          )}
        </div>
      </div>
      <div className="min-w-0">
        <p className="truncate text-sm font-medium group-hover:text-accent">{file.name}</p>
        {location && (
          <LocationButton location={location} onPress={onOpenContainingFolder} className="mt-1" />
        )}
        <p className="mt-1 text-[11px] text-muted">
          {file.kind === "folder"
            ? t("features.fileBrowser.folderKind")
            : formatFileBytes(file.size ?? 0)}
        </p>
      </div>
    </>
  );
}

/**
 * List-view row content. The columns are responsive: below `sm` only the name shows, the
 * size column appears at `sm`, and at `lg` a third column carries the location (when the
 * caller provides one) or the modification time. The inline location button under the name
 * is the small-width equivalent and is hidden from `lg` up.
 */
function ListFile({
  file,
  selectable,
  showCheckbox,
  location,
  onOpenContainingFolder,
}: {
  file: FileEntry;
  selectable: boolean;
  showCheckbox: boolean;
  location?: string;
  onOpenContainingFolder?: () => void;
}) {
  const { t } = useI18n();
  return (
    <>
      <div className="flex min-w-0 items-center gap-3">
        {selectable ? <SelectionCheckbox file={file} isVisible={showCheckbox} /> : null}
        <div className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-accent/10 text-accent">
          {file.kind === "folder" ? (
            <FolderIcon className="size-4" />
          ) : (
            <FileTypeIcon file={file} className="size-4" />
          )}
        </div>
        <div className="min-w-0">
          <span
            className="block truncate text-sm font-medium group-hover:text-accent"
            title={file.name}
          >
            {file.name}
          </span>
          {location ? (
            <LocationButton
              location={location}
              onPress={onOpenContainingFolder}
              className="lg:hidden"
            />
          ) : null}
        </div>
      </div>
      <span className="hidden text-xs text-muted sm:block">
        {file.kind === "folder"
          ? t("features.fileBrowser.folderKind")
          : formatFileBytes(file.size ?? 0)}
      </span>
      <span className="hidden min-w-0 text-xs text-muted lg:block">
        {location ? (
          <LocationButton location={location} onPress={onOpenContainingFolder} />
        ) : (
          new Date(file.modTime).toLocaleString()
        )}
      </span>
    </>
  );
}

/**
 * Shows where a search hit lives and opens that folder. The label names the path
 * so a row of results is readable without the breadcrumb the search hides.
 */
function LocationButton({
  location,
  onPress,
  className = "",
}: {
  location: string;
  onPress?: () => void;
  className?: string;
}) {
  const { t } = useI18n();
  return (
    <Button
      size="sm"
      variant="ghost"
      className={`h-auto min-h-0 max-w-full justify-start rounded-sm px-0 py-0 text-[11px] text-muted ${className}`}
      aria-label={t("features.fileBrowser.openContainingFolder", { path: location })}
      onPress={onPress}
    >
      <span className="truncate">{location}</span>
    </Button>
  );
}

/**
 * Breadcrumb for the current path. The trail is trimmed by container width rather than
 * viewport width: the current folder is always shown, its parent from the `@md` container
 * width and the ancestors only from `@4xl`, so a narrow pane never overflows.
 */
function FileBrowserBreadcrumb({
  path,
  rootLabel,
  onNavigatePath,
}: {
  path: string;
  rootLabel: string;
  onNavigatePath: (path: string) => void;
}) {
  const { t } = useI18n();
  const parts = path.split("/").filter(Boolean);
  const lastIndex = parts.length - 1;
  return (
    <nav
      aria-label={t("features.fileBrowser.currentFolder")}
      className="flex min-w-0 items-center gap-1 overflow-hidden text-sm"
    >
      <Button
        size="sm"
        variant="ghost"
        aria-label={rootLabel}
        className="h-8 max-w-9 shrink-0 gap-1.5 overflow-hidden rounded-lg px-2 text-muted hover:text-foreground @lg/file-browser:max-w-44"
        onPress={() => onNavigatePath("/")}
      >
        <FolderIcon className="size-4 shrink-0" />
        <span className="hidden truncate @lg/file-browser:inline">{rootLabel}</span>
      </Button>
      {parts.map((part, index) => {
        const currentPath = `/${parts.slice(0, index + 1).join("/")}`;
        const isCurrent = index === lastIndex;
        const isParent = index === lastIndex - 1;
        const visibility = isCurrent
          ? "flex"
          : isParent
            ? "hidden @md/file-browser:flex"
            : "hidden @4xl/file-browser:flex";
        return (
          <span key={currentPath} className={`${visibility} min-w-0 items-center gap-1`}>
            <span className="shrink-0 text-muted/60">/</span>
            {isCurrent ? (
              <span
                aria-current="page"
                className="min-w-0 truncate px-1 text-sm font-medium text-foreground"
                title={part}
              >
                {part}
              </span>
            ) : (
              <Button
                size="sm"
                variant="ghost"
                className="h-8 min-w-0 max-w-40 truncate rounded-lg px-2 text-muted hover:text-foreground"
                onPress={() => onNavigatePath(currentPath)}
              >
                {part}
              </Button>
            )}
          </span>
        );
      })}
    </nav>
  );
}

/**
 * Formats a byte count as at most one decimal with a binary unit, e.g. "1.5 MB". Non-finite
 * and non-positive counts (a folder reports no size) render as "0 B"; byte counts are shown
 * without a decimal. TB is the largest unit, so bigger values keep growing in TB.
 */
export function formatFileBytes(bytes: number) {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const index = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  return `${(bytes / 1024 ** index).toFixed(index === 0 ? 0 : 1)} ${units[index]}`;
}
