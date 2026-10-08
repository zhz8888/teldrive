import { z } from "zod";

/**
 * Zod schema for the file-browser URL state. Every field is tolerant rather than
 * strict: `.catch(...)` swaps an unparseable value for the documented fallback and
 * `.default(...)` fills in a key the URL omits, so redirects, history entries and
 * hand-written links all resolve to a renderable browser instead of an error.
 */
export const fileSearchSchema = z.object({
  /** Folder path being listed; "/" is the drive root. */
  path: z.string().catch("/").default("/"),
  /** Folder id, preferred over `path` once the drive has resolved the folder. */
  parentId: z.string().uuid().optional().catch(undefined),
  /** Free-text query narrowing the listing. */
  q: z.string().optional().catch(undefined),
  /** Sort key; an unknown or missing value reads as "name". */
  sort: z.enum(["name", "updatedAt", "size", "id"]).catch("name").default("name"),
  /** Sort direction; anything other than "desc" reads as ascending. */
  order: z.enum(["asc", "desc"]).catch("asc").default("asc"),
  /** Restricts the listing to one file category; unset means every category. */
  category: z
    .enum(["archive", "audio", "document", "image", "video", "other"])
    .optional()
    .catch(undefined),
  /** Opaque cursor the server returned for the page to fetch; unset is the first page. */
  cursor: z.string().optional().catch(undefined),
  /** Pagination history kept for a "previous page" control. */
  cursorHistory: z.string().optional().catch(undefined),
  /** Result layout; a missing or unknown value reads as the list view. */
  view: z.enum(["list", "grid"]).catch("list").default("list"),
  /** Id of the file shown in the preview dialog. */
  preview: z.string().uuid().optional().catch(undefined),
  /** Id of the file opened in the reader. */
  read: z.string().uuid().optional().catch(undefined),
  /** Dialog the route should open on load; unset means no dialog. */
  action: z
    .enum(["new-folder", "rename", "move", "copy", "trash", "restore", "purge"])
    .optional()
    .catch(undefined),
});

/** Validated file-browser URL state, inferred from {@link fileSearchSchema}. */
export type FileSearch = z.infer<typeof fileSearchSchema>;

/** All-defaults state, i.e. what an empty URL parses to: the drive root in list view. */
export const defaultFileSearch: FileSearch = fileSearchSchema.parse({});
