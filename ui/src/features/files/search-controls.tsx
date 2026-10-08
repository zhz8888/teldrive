import { Button, Checkbox, Input, Label, ListBox, Popover, Select, TextField } from "@heroui/react";
import { useEffect, useState } from "react";
import type { FileCategory } from "@/api/types";
import { useI18n, type MessageKey } from "@/lib/i18n";
import { FolderPicker } from "./folder-picker";
import { invalidSearchDates, type SearchState, searchCategories, searchDate } from "./search-state";

/** Catalog key of each content category the filter offers, in the order shown. */
const categoryKeys = {
  archive: "routes.search.category.archive",
  audio: "routes.search.category.audio",
  document: "routes.search.category.document",
  image: "routes.search.category.image",
  video: "routes.search.category.video",
  other: "routes.search.category.other",
} as const satisfies Record<FileCategory, MessageKey>;

/**
 * Filter and sort controls of the drive search. The form edits a local draft so a
 * half-typed date range never reaches the URL, and it states why it refuses to
 * submit — an inverted range or a recursive scope without its folder — instead of
 * disabling the button silently. Applied filters are echoed as removable chips
 * below the bar.
 */
export function SearchControls({
  search,
  onChange,
  isOpen,
  onOpenChange,
}: {
  search: SearchState;
  onChange: (search: SearchState) => void;
  isOpen: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useI18n();
  const [draft, setDraft] = useState(search);
  useEffect(() => setDraft(search), [search, isOpen]);
  const invalidDates = invalidSearchDates(draft);
  const missingFolder = draft.scope === "recursive" && !draft.parentId;
  const setFilter = <K extends keyof SearchState>(key: K, value: SearchState[K]) =>
    setDraft((previous) => ({ ...previous, [key]: value }));
  const clear = () => {
    onChange({ q: search.q, sort: search.sort, order: search.order, view: search.view });
    onOpenChange(false);
  };
  const remove = (key: keyof SearchState) => onChange({ ...search, [key]: undefined });
  const filtered = Boolean(
    search.kind ||
      search.category?.length ||
      search.updatedAfter ||
      search.updatedBefore ||
      search.scope === "recursive",
  );

  return (
    <>
      <div className="flex flex-wrap items-center gap-3 rounded-2xl border border-border bg-surface/80 p-4 shadow-sm">
        <h1 className="mr-auto text-sm font-semibold">{t("routes.search.title")}</h1>
        <Popover isOpen={isOpen} onOpenChange={onOpenChange}>
          <Button variant="secondary" size="sm">
            {t("routes.search.filters")}
          </Button>
          <Popover.Content placement="bottom end" className="w-[min(92vw,26rem)]">
            <Popover.Dialog className="max-h-[min(80dvh,44rem)] overflow-y-auto p-4">
              <form
                className="space-y-4"
                onSubmit={(event) => {
                  event.preventDefault();
                  if (invalidDates || missingFolder) return;
                  onChange({ ...draft, q: search.q });
                  onOpenChange(false);
                }}
              >
                <Popover.Heading className="text-sm font-semibold">
                  {t("routes.search.refine")}
                </Popover.Heading>
                <SearchSelect
                  label={t("routes.search.scope.label")}
                  value={draft.scope ?? "drive"}
                  onChange={(scope) => setFilter("scope", scope)}
                  options={[
                    { value: "drive", label: t("routes.search.scope.drive") },
                    { value: "recursive", label: t("routes.search.scope.recursive") },
                  ]}
                />
                {draft.scope === "recursive" &&
                  (draft.parentId ? (
                    <div className="flex items-center justify-between gap-2 text-xs text-muted">
                      <span className="min-w-0 truncate" title={draft.folderPath}>
                        {t("routes.search.scope.folder", {
                          path: draft.folderPath ?? t("routes.search.scope.folderFallback"),
                        })}
                      </span>
                      <Button
                        size="sm"
                        variant="ghost"
                        onPress={() =>
                          setDraft((previous) => ({
                            ...previous,
                            parentId: undefined,
                            folderPath: undefined,
                          }))
                        }
                      >
                        {t("routes.search.scope.change")}
                      </Button>
                    </div>
                  ) : (
                    <div className="rounded-xl border border-border p-3">
                      <p className="mb-2 text-xs text-muted">{t("routes.search.scope.choose")}</p>
                      <FolderPicker
                        confirmLabel={t("routes.search.scope.confirm")}
                        requireFolder
                        onConfirm={(parentId, path) => {
                          if (parentId)
                            setDraft((previous) => ({ ...previous, parentId, folderPath: path }));
                        }}
                      />
                    </div>
                  ))}
                <SearchSelect
                  label={t("routes.search.kind.label")}
                  value={draft.kind ?? "all"}
                  onChange={(kind) => setFilter("kind", kind === "all" ? undefined : kind)}
                  options={[
                    { value: "all", label: t("routes.search.kind.all") },
                    { value: "file", label: t("routes.search.kind.file") },
                    { value: "folder", label: t("routes.search.kind.folder") },
                  ]}
                />
                <fieldset className="grid grid-cols-2 gap-2">
                  <legend className="mb-2 text-sm">{t("routes.search.category.legend")}</legend>
                  {searchCategories.map((category) => (
                    <Checkbox
                      key={category}
                      isSelected={draft.category?.includes(category) ?? false}
                      onChange={(checked) =>
                        setFilter(
                          "category",
                          checked
                            ? [...(draft.category ?? []), category]
                            : draft.category?.filter((value) => value !== category),
                        )
                      }
                    >
                      <Checkbox.Control>
                        <Checkbox.Indicator />
                      </Checkbox.Control>
                      <Label>{t(categoryKeys[category])}</Label>
                    </Checkbox>
                  ))}
                </fieldset>
                <div className="grid grid-cols-2 gap-3">
                  <TextField
                    value={draft.updatedAfter?.slice(0, 10) ?? ""}
                    onChange={(value) => setFilter("updatedAfter", searchDate(value))}
                  >
                    <Label className="text-xs">{t("routes.search.updatedAfter")}</Label>
                    <Input type="date" />
                  </TextField>
                  <TextField
                    value={draft.updatedBefore?.slice(0, 10) ?? ""}
                    onChange={(value) => setFilter("updatedBefore", searchDate(value))}
                  >
                    <Label className="text-xs">{t("routes.search.updatedBefore")}</Label>
                    <Input type="date" />
                  </TextField>
                </div>
                {invalidDates && (
                  <p role="alert" className="text-xs text-danger">
                    {t("routes.search.invalidDates")}
                  </p>
                )}
                {missingFolder && (
                  <p role="alert" className="text-xs text-danger">
                    {t("routes.search.missingFolder")}
                  </p>
                )}
                <div className="flex justify-between">
                  <Button variant="ghost" size="sm" onPress={clear}>
                    {t("routes.search.clear")}
                  </Button>
                  <Button
                    type="submit"
                    variant="primary"
                    size="sm"
                    isDisabled={invalidDates || missingFolder}
                  >
                    {t("routes.search.apply")}
                  </Button>
                </div>
              </form>
            </Popover.Dialog>
          </Popover.Content>
        </Popover>
        <div className="flex w-full gap-2 sm:w-auto">
          <SearchSelect
            compact
            label={t("routes.search.sort.label")}
            value={search.sort ?? "name"}
            onChange={(sort) => onChange({ ...search, sort })}
            options={[
              { value: "name", label: t("routes.search.sort.name") },
              { value: "updatedAt", label: t("routes.search.sort.updatedAt") },
              { value: "size", label: t("routes.search.sort.size") },
            ]}
          />
          <SearchSelect
            compact
            label={t("routes.search.order.label")}
            value={search.order ?? "asc"}
            onChange={(order) => onChange({ ...search, order })}
            options={[
              { value: "asc", label: t("routes.search.order.asc") },
              { value: "desc", label: t("routes.search.order.desc") },
            ]}
          />
        </div>
      </div>
      {(search.q || filtered) && (
        <section
          className="flex flex-wrap items-center gap-2"
          aria-label={t("routes.search.activeFilters")}
        >
          {search.q && (
            <FilterChip
              label={t("routes.search.chip.name", { name: search.q })}
              onRemove={() => remove("q")}
            />
          )}
          {search.scope === "recursive" && (
            <FilterChip
              label={t("routes.search.chip.scope", {
                path: search.folderPath ?? t("routes.search.chip.scopeFallback"),
              })}
              onRemove={() =>
                onChange({ ...search, scope: "drive", parentId: undefined, folderPath: undefined })
              }
            />
          )}
          {search.kind && (
            <FilterChip
              label={t(
                search.kind === "file" ? "routes.search.kind.file" : "routes.search.kind.folder",
              )}
              onRemove={() => remove("kind")}
            />
          )}
          {search.category?.map((category) => (
            <FilterChip
              key={category}
              label={t(categoryKeys[category])}
              onRemove={() =>
                onChange({
                  ...search,
                  category: search.category?.filter((value) => value !== category),
                })
              }
            />
          ))}
          {search.updatedAfter && (
            <FilterChip
              label={t("routes.search.chip.after", { date: search.updatedAfter.slice(0, 10) })}
              onRemove={() => remove("updatedAfter")}
            />
          )}
          {search.updatedBefore && (
            <FilterChip
              label={t("routes.search.chip.before", { date: search.updatedBefore.slice(0, 10) })}
              onRemove={() => remove("updatedBefore")}
            />
          )}
          {filtered && (
            <Button size="sm" variant="ghost" onPress={clear}>
              {t("routes.search.clear")}
            </Button>
          )}
        </section>
      )}
    </>
  );
}

/**
 * A labelled dropdown of the filter form. `compact` moves the label to the
 * accessible name only, which is how the sort controls fit next to the Filters
 * button without repeating their meaning on screen.
 */
function SearchSelect<T extends string>({
  label,
  value,
  options,
  onChange,
  compact = false,
}: {
  label: string;
  value: T;
  options: { value: T; label: string }[];
  onChange: (value: T) => void;
  compact?: boolean;
}) {
  return (
    <Select
      className={compact ? "min-w-0 flex-1 sm:w-40 sm:flex-none" : "w-full"}
      selectedKey={value}
      onSelectionChange={(key) => {
        const option = options.find((item) => item.value === key);
        if (option) onChange(option.value);
      }}
    >
      <Label className={compact ? "sr-only" : "text-sm"}>{label}</Label>
      <Select.Trigger>
        <Select.Value />
        <Select.Indicator />
      </Select.Trigger>
      <Select.Popover>
        <ListBox>
          {options.map((option) => (
            <ListBox.Item key={option.value} id={option.value} textValue={option.label}>
              {option.label}
              <ListBox.ItemIndicator />
            </ListBox.Item>
          ))}
        </ListBox>
      </Select.Popover>
    </Select>
  );
}

/** Removable chip for one applied filter; pressing it drops that filter. */
function FilterChip({ label, onRemove }: { label: string; onRemove: () => void }) {
  const { t } = useI18n();
  return (
    <Button
      size="sm"
      variant="secondary"
      className="max-w-full rounded-full"
      onPress={onRemove}
      aria-label={t("routes.search.chip.remove", { label })}
    >
      <span className="truncate">{label}</span>
      <span aria-hidden="true">×</span>
    </Button>
  );
}
