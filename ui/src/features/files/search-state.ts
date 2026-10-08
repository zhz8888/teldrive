import type { FileCategory, FileSort } from "@/api/types";
import type { DriveSearchOptions } from "./queries";

/**
 * Categories offered by the filter panel, in display order. Validation also accepts a
 * category only if it appears here, so this list is the allow-list for the URL parameter.
 */
export const searchCategories: FileCategory[] = [
  "archive",
  "audio",
  "document",
  "image",
  "video",
  "other",
];

/** Validated `/search` route state; every field is optional and reflects one URL parameter. */
export type SearchState = {
  /** Free-text query; trimmed, capped at 512 characters. */
  q?: string;
  /** "recursive" searches inside `parentId`, "drive" searches the whole drive. */
  scope?: "drive" | "recursive";
  /** Folder to search in; only meaningful together with the recursive scope. */
  parentId?: string;
  /** Display path of `parentId`, shown by the search controls; never sent to the API. */
  folderPath?: string;
  /** Restricts results to files or to folders; unset means both. */
  kind?: "file" | "folder";
  /** Selected categories; unknown values are dropped and the order is canonicalized. */
  category?: FileCategory[];
  /** Inclusive lower bound on the modification time, as a canonical ISO instant. */
  updatedAfter?: string;
  /** Exclusive upper bound on the modification time, as a canonical ISO instant. */
  updatedBefore?: string;
  /** Sort key; only "updatedAt" and "size" are accepted, anything else reads as "name". */
  sort?: FileSort;
  /** Sort direction; anything other than "desc" reads as ascending. */
  order?: "asc" | "desc";
  /** Result layout; defaults to the list view. */
  view?: "list" | "grid";
};

/**
 * Canonicalizes a date or date-time from the URL into an ISO instant. A bare date
 * means midnight UTC; anything the calendar does not have (2026-02-30) or that
 * carries an out-of-range time is rejected rather than silently rolled over, so a
 * saved search cannot start filtering on a different day than it shows.
 */
export function searchDate(value: unknown): string | undefined {
  if (typeof value !== "string") return undefined;
  const match =
    /^(\d{4}-\d{2}-\d{2})(?:T([01]\d|2[0-3]):[0-5]\d:[0-5]\d(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2}))?$/.exec(
      value,
    );
  if (!match) return undefined;
  const midnight = new Date(`${match[1]}T00:00:00.000Z`);
  if (Number.isNaN(midnight.valueOf()) || midnight.toISOString().slice(0, 10) !== match[1])
    return undefined;
  const date = match[2] ? new Date(value) : midnight;
  return Number.isNaN(date.valueOf()) ? undefined : date.toISOString();
}

/**
 * Reports whether the range is inverted. The comparison is on the canonical ISO
 * values, so it holds for mixed date and date-time inputs as well.
 */
export function invalidSearchDates(search: SearchState): boolean {
  return Boolean(
    search.updatedAfter && search.updatedBefore && search.updatedAfter >= search.updatedBefore,
  );
}

/**
 * Validates the `/search` parameters. Every field is optional and the scope
 * defaults to `drive`, so a hand-written or stale URL still resolves to a search
 * that can run; a folder path is kept only next to the folder id it describes,
 * because the id is what the API filters on.
 */
export function validateDriveSearch(value: Record<string, unknown>): SearchState {
  const requestedCategories =
    typeof value.category === "string" ? [value.category] : value.category;
  const category = searchCategories.filter(
    (category) => Array.isArray(requestedCategories) && requestedCategories.includes(category),
  );
  const parentId =
    typeof value.parentId === "string" &&
    /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(
      value.parentId,
    )
      ? value.parentId
      : undefined;
  return {
    q: typeof value.q === "string" ? value.q.trim().slice(0, 512) || undefined : undefined,
    scope: value.scope === "recursive" ? "recursive" : "drive",
    parentId,
    folderPath:
      parentId && typeof value.folderPath === "string" && value.folderPath.startsWith("/")
        ? value.folderPath.slice(0, 4096)
        : undefined,
    kind: value.kind === "file" || value.kind === "folder" ? value.kind : undefined,
    category: category.length ? category : undefined,
    updatedAfter: searchDate(value.updatedAfter),
    updatedBefore: searchDate(value.updatedBefore),
    sort: value.sort === "updatedAt" || value.sort === "size" ? value.sort : "name",
    order: value.order === "desc" ? "desc" : "asc",
    view: value.view === "grid" ? "grid" : "list",
  };
}

/**
 * Maps the URL state onto the listing query. The folder id is sent only for the
 * recursive scope: the drive-wide listing is the owner's whole drive, so passing
 * a folder beside it would silently narrow the results.
 */
export function driveSearchOptions(search: SearchState): DriveSearchOptions {
  return {
    q: search.q,
    scope: search.scope ?? "drive",
    parentId: search.scope === "recursive" ? search.parentId : undefined,
    kind: search.kind,
    category: search.category,
    updatedAfter: search.updatedAfter,
    updatedBefore: search.updatedBefore,
    sort: search.sort ?? "name",
    order: search.order ?? "asc",
  };
}

/**
 * Whether the search has anything to run for. Without criteria the page shows the
 * "find anything" hint instead of asking the API for the whole drive.
 */
export function hasSearchCriteria(search: SearchState): boolean {
  return Boolean(
    search.q ||
      search.kind ||
      search.category?.length ||
      search.updatedAfter ||
      search.updatedBefore ||
      search.scope === "recursive",
  );
}
