import { useInfiniteQuery, useSuspenseInfiniteQuery } from "@tanstack/react-query";
import { z } from "zod";
import { $api, fetchClient } from "@/api/client";
import { invalidResponse } from "@/api/errors";
import type { paths } from "@/api/schema";
import type { FileCategory, FileSort, FileStatus } from "@/api/types";

/** The 200 body of `GET /v1/files`, taken from the generated schema. */
type FileListResponse = paths["/v1/files"]["get"]["responses"][200]["content"]["application/json"];

// Only the fields every listing relies on are validated; `.passthrough()` keeps the rest
// of the entry (size, times, paths) available to the UI unchanged.
const fileListSchema = z
  .object({
    items: z.array(
      z
        .object({
          id: z.string().uuid(),
          name: z.string(),
          kind: z.enum(["file", "folder"]),
          status: z.enum(["active", "trashed", "deletion_pending"]),
        })
        .passthrough(),
    ),
    nextCursor: z.string().optional(),
  })
  .passthrough();

/**
 * Guards the listing against a server whose response shape no longer matches the UI.
 * Throwing here (instead of returning the data) turns the mismatch into a query error the
 * caller renders as a load failure with a retry, rather than a tree rendered from a page
 * whose entries are missing ids or names.
 */
function validateFileList(data: FileListResponse): FileListResponse {
  const result = fileListSchema.safeParse(data);
  if (!result.success) {
    throw invalidResponse(
      "Teldrive received data from an incompatible API version. Refresh after the server and UI are upgraded together.",
    );
  }
  return data;
}

/** Route action whose dialog `/files` opens on top of the listing. */
export type FileRouteAction =
  | "new-folder"
  | "rename"
  | "move"
  | "copy"
  | "trash"
  | "restore"
  | "purge";

/**
 * Search parameters of the `/files` and `/search` routes that the listing functions here
 * read. The dialog and history fields at the bottom are part of the route vocabulary but no
 * function in this module reads them; `cursorHistory` is likewise only carried along.
 */
export type FileRouteSearch = {
  /** Current folder path; "/" means the drive root. */
  path: string;
  /** Id of the current folder, sent instead of `path` once the path has been resolved. */
  parentId?: string;
  /** Free-text query, sent as the `search` parameter. */
  q?: string;
  /** Sort key requested from the server. */
  sort: FileSort;
  /** Sort direction requested from the server. */
  order: "asc" | "desc";
  /** One category or several; `filePageInit` normalises both to an array. */
  category?: FileCategory | FileCategory[];
  /** "folder" lists one folder, "drive" the whole drive, "recursive" below `parentId`. */
  scope?: "folder" | "drive" | "recursive";
  /** Restricts the listing to files or to folders. */
  kind?: "file" | "folder";
  /** Inclusive lower bound on the modification time, as an ISO instant. */
  updatedAfter?: string;
  /** Exclusive upper bound on the modification time, as an ISO instant. */
  updatedBefore?: string;
  /** Opaque cursor the server returned for the page to fetch; omit for the first page. */
  cursor?: string;
  /** Pagination history kept by the route for a "previous page" control. */
  cursorHistory?: string;
  /** Result layout the browser should use; not sent to the server. */
  view: "list" | "grid";
  /** Id of the file the route shows in the preview dialog. */
  preview?: string;
  /** Id of the file the route opens in the reader. */
  read?: string;
  /** Dialog the route opens on top of the listing. */
  action?: FileRouteAction;
};

/**
 * Builds the listing request for one cursor position. A folder is addressed either by
 * `parentId` or by `path`, never both, and the drive root is expressed by sending neither;
 * `scope` and `status` select which slice of the drive to list. Pages are capped at 100
 * entries.
 */
export function filePageInit(search: FileRouteSearch, status: FileStatus, cursor = search.cursor) {
  return {
    params: {
      query: {
        parentId: search.parentId,
        path:
          search.scope && search.scope !== "folder"
            ? undefined
            : search.parentId
              ? undefined
              : search.path === "/"
                ? undefined
                : search.path,
        scope: search.scope,
        kind: search.kind,
        status,
        search: search.q || undefined,
        category: Array.isArray(search.category)
          ? search.category
          : search.category
            ? [search.category]
            : undefined,
        updatedAfter: search.updatedAfter,
        updatedBefore: search.updatedBefore,
        sort: search.sort,
        order: search.order,
        cursor,
        limit: 100,
      },
    },
  };
}

/**
 * One listing page as plain query options rather than a hook, with the same 15-second
 * staleness and response validation as `useFilePage` and `useInfiniteFilePages`.
 */
export function filePageQueryOptions(search: FileRouteSearch, status: FileStatus) {
  return $api.queryOptions("get", "/v1/files", filePageInit(search, status), {
    staleTime: 15_000,
    select: validateFileList,
  });
}

/**
 * Cursor pages of the files the caller has shared, so `fetchNextPage` walks the
 * server's `nextCursor` and every entry stays reachable instead of only the
 * first page. The listings are disabled until the shared browser needs them.
 */
export function useSharedFilePages(enabled: boolean) {
  return useInfiniteQuery({
    queryKey: ["get", "/v1/shared", "pages"] as const,
    enabled,
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam, signal }) => {
      const result = await fetchClient.GET("/v1/shared", {
        params: { query: { cursor: pageParam, limit: 500 } },
        signal,
      });
      if (!result.data) {
        throw invalidResponse("Teldrive returned an empty shared-file response.");
      }
      return result.data;
    },
    getNextPageParam: (lastPage) => lastPage.nextCursor,
    staleTime: 15_000,
  });
}

/** Cursor pages of the files other owners granted the caller. */
export function useSharedWithMePages(enabled: boolean) {
  return useInfiniteQuery({
    queryKey: ["get", "/v1/shared/with-me", "pages"] as const,
    enabled,
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam, signal }) => {
      const result = await fetchClient.GET("/v1/shared/with-me", {
        params: { query: { cursor: pageParam, limit: 500 } },
        signal,
      });
      if (!result.data) {
        throw invalidResponse("Teldrive returned an empty shared-with-me response.");
      }
      return result.data;
    },
    getNextPageParam: (lastPage) => lastPage.nextCursor,
    staleTime: 15_000,
  });
}

/**
 * Cursor pages of one listing. Trash uses this instead of the first page alone,
 * so `fetchNextPage` walks the `nextCursor` the server returns and every entry
 * stays reachable rather than only the first hundred.
 */
export function useFilePage(search: FileRouteSearch, status: FileStatus) {
  const queryKey = [
    "get",
    "/v1/files",
    "pages",
    {
      path: search.path,
      parentId: search.parentId,
      q: search.q,
      sort: search.sort,
      order: search.order,
      category: search.category,
      status,
    },
  ] as const;

  return useSuspenseInfiniteQuery({
    queryKey,
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam, signal }) => {
      const result = await fetchClient.GET("/v1/files", {
        ...filePageInit(search, status, pageParam),
        signal,
      });
      if (!result.data) {
        throw invalidResponse("Teldrive returned an empty file-list response.");
      }
      return validateFileList(result.data);
    },
    getNextPageParam: (lastPage) => lastPage.nextCursor,
    staleTime: 15_000,
  });
}

/**
 * Non-suspense cursor paging for one listing, used by the file manager. The query key
 * holds every field that can change the result set, so a filter change starts a new cache
 * entry instead of appending pages from the previous filters. `enabled` lets callers hold
 * the request back (empty or invalid search criteria); for the drive-wide and recursive
 * scopes the previous page is kept on screen as placeholder data while the new one loads,
 * so retyping a query does not blank the list.
 */
export function useInfiniteFilePages(search: FileRouteSearch, status: FileStatus, enabled = true) {
  const queryKey = [
    "get",
    "/v1/files",
    "infinite",
    {
      path: search.path,
      parentId: search.parentId,
      q: search.q,
      sort: search.sort,
      order: search.order,
      category: search.category,
      scope: search.scope,
      kind: search.kind,
      updatedAfter: search.updatedAfter,
      updatedBefore: search.updatedBefore,
      status,
    },
  ] as const;

  return useInfiniteQuery({
    queryKey,
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam, signal }) => {
      const result = await fetchClient.GET("/v1/files", {
        ...filePageInit(search, status, pageParam),
        signal,
      });
      if (!result.data) {
        throw invalidResponse("Teldrive returned an empty file-list response.");
      }
      return validateFileList(result.data);
    },
    getNextPageParam: (lastPage) => lastPage.nextCursor,
    staleTime: 15_000,
    enabled,
    placeholderData: search.scope && search.scope !== "folder" ? (previous) => previous : undefined,
  });
}

/**
 * Cursor pages of the folders under one destination. The folder picker pages
 * through them with `fetchNextPage`, so a folder beyond the first page is still
 * selectable.
 */

/**
 * Filters the drive-wide search sends to `GET /v1/files`. The two wide scopes
 * are the ones the listing endpoint added: "drive" covers every active entry of
 * the owner, "recursive" the entries below one folder.
 */
export type DriveSearchOptions = {
  /** Free-text query. */
  q?: string;
  /** Which wide listing to run; see the type comment above. */
  scope: "drive" | "recursive";
  /** Folder the recursive scope searches below; ignored by the drive scope. */
  parentId?: string;
  /** Restricts results to files or to folders. */
  kind?: "file" | "folder";
  /** Categories to include; an empty list means no category filter. */
  category?: FileCategory[];
  /** Inclusive lower bound on the modification time, as an ISO instant. */
  updatedAfter?: string;
  /** Exclusive upper bound on the modification time, as an ISO instant. */
  updatedBefore?: string;
  /** Sort key the search results come back in. */
  sort: FileSort;
  /** Sort direction of the search results. */
  order: "asc" | "desc";
};

/**
 * Cursor pages of the folders directly under one destination, active ones only, sorted by
 * name with 200 per page. The destination is given as `parentId` when known and otherwise
 * as `path`; the drive root passes neither.
 */
export function useFolderChildren(parentId?: string, path?: string) {
  const queryKey = ["get", "/v1/files", "folders", { parentId, path }] as const;

  return useInfiniteQuery({
    queryKey,
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam, signal }) => {
      const result = await fetchClient.GET("/v1/files", {
        params: {
          query: {
            parentId,
            path: parentId ? undefined : path === "/" ? undefined : path,
            kind: "folder",
            status: "active",
            limit: 200,
            sort: "name",
            order: "asc",
            cursor: pageParam,
          },
        },
        signal,
      });
      if (!result.data) {
        throw invalidResponse("Teldrive returned an empty file-list response.");
      }
      return validateFileList(result.data);
    },
    getNextPageParam: (lastPage) => lastPage.nextCursor,
    staleTime: 20_000,
  });
}
