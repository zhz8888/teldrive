using TypeSpec.Http;
using TypeSpec.OpenAPI;

namespace TelDrive;

/** Whether a catalog entry holds content or groups other entries. */
enum FileKind {
  /** A regular file; it owns the content parts stored on Telegram. */
  file: "file",
  /** A folder; it has no content of its own and never carries a size. */
  folder: "folder",
}

/**
 * Lifecycle of a catalog entry. Trashing only flips the status and keeps the
 * content parts; the purge sweep later deletes the parts of a deletion-pending
 * entry.
 */
enum FileStatus {
  /** Readable, listed and mutable. */
  active: "active",
  /** In the trash: restorable, and excluded from active listings and totals. */
  trashed: "trashed",
  /** Marked for permanent removal; its Telegram parts are being deleted. */
  deletionPending: "deletion_pending",
}

/** What a move, copy or create does when an entry of that name already exists at the destination. */
enum NameConflictPolicy {
  /** Reject the request and change nothing (the default). */
  fail: "fail",
  /** Delete the existing entry and its subtree, then store the new one in its place. */
  replace: "replace",
  /** Keep the existing entry and store the new one under the first free " (n)" suffix. */
  rename: "rename",
}

/** Bucket the server derives from an entry's MIME type, falling back to the file name. */
enum FileCategory {
  /** Compressed archives: zip, rar, 7z, tar and similar. */
  archive: "archive",
  /** Audio content ("audio/*" MIME types). */
  audio: "audio",
  /** Text, PDF and office documents. */
  document: "document",
  /** Image content ("image/*" MIME types). */
  image: "image",
  /** Video content ("video/*" MIME types). */
  video: "video",
  /** Everything else, including folders when the category is used as a listing filter. */
  other: "other",
}

/** How the search term is matched against entry names. */
enum FileSearchType {
  /** Case-insensitive substring match, or a trigram similarity match (the default). */
  text: "text",
  /** PostgreSQL case-insensitive regular expression match. */
  regex: "regex",
}

/** Region of the drive a listing covers. */
enum FileListScope {
  /** Direct children of one folder, or of the drive root (the default). */
  folder: "folder",
  /** Every active entry anywhere in the drive; rejects a parent, a path and any status but active. */
  drive: "drive",
  /** Every active entry below one folder, or below the whole drive when no folder is given. */
  recursive: "recursive",
}

/** Key a listing is ordered by; a cursor only continues a listing produced with the same key and order. */
enum FileSort {
  /** Entry name, compared as text (the default). */
  name: "name",
  /** Time the metadata was last updated; the cursor carries an RFC 3339 timestamp. */
  updatedAt: "updatedAt",
  /** Content size in bytes, with entries that carry none ordered as -1; the cursor carries a decimal byte count. */
  size: "size",
  /** Entry UUID; the cursor needs no value because the IDs order on their own. */
  id: "id",
}

/** Direction a sorted listing is returned in. */
enum SortOrder {
  /** Smallest or earliest first, according to the active sort key (the default). */
  ascending: "asc",
  /** Largest or latest first. */
  descending: "desc",
}

/** One catalog entry with its metadata, as returned by every file endpoint. */
model FileEntry {
  /** Entry UUID; this is the fileId path parameter of the single-entry endpoints. */
  id: Uuid;
  /** Containing folder; absent for an entry sitting in the drive root. */
  parentId?: Uuid;
  /** Entry name, unique among the active entries of the same parent and never blank. */
  name: string;
  /** Whether this entry is a file or a folder. */
  kind: FileKind;
  /** Media type recorded at upload time; absent when the upload supplied none. */
  mimeType?: string;
  /** Content size in bytes; absent for folders, which never carry a size. */
  size?: int64;
  /** Content checksum (BLAKE3 tree hash) recorded on completion; absent when no part hash was stored. */
  hash?: FileHash;
  /** Whether the content parts are stored encrypted and decrypted by the server on read. */
  encryption: boolean;
  /** Lifecycle state of the entry. */
  status: FileStatus;
  /** Modification time of the content, as supplied when the entry was created or last updated. */
  modTime: utcDateTime;
  /** Revision of the metadata; it is incremented by every rename, move, trash and restore. */
  generation: int64;
  /** Time the entry row was created. */
  createdAt: utcDateTime;
  /** Time the entry row was last changed. */
  updatedAt: utcDateTime;
  /** Location path, included for drive and recursive listings. */
  parentPath?: string;
}

/** Viewer a saved reader state belongs to. */
enum ViewerKind {
  /** Still-image viewer. */
  image: "image",
  /** Video player. */
  video: "video",
  /** Audio player. */
  audio: "audio",
  /** Paged PDF reader. */
  pdf: "pdf",
  /** Reflowable e-book reader. */
  ebook: "ebook",
  /** Plain-text reader. */
  text: "text",
}

/** Reader state one user saved for one file; the server treats every payload as opaque JSON. */
model FileViewState {
  /** File the state belongs to. */
  fileId: Uuid;
  /** Viewer the state was saved by. */
  kind: ViewerKind;
  /** Opaque JSON object, for example the reading or playback position. */
  position: Record<unknown>;
  /** Opaque JSON object with the viewer settings chosen by the user. */
  preferences: Record<unknown>;
  /** Reader bookmarks, each an opaque JSON value; at most 500 entries. */
  bookmarks: Record<unknown>[];
  /** Time the state was last written. */
  updatedAt: utcDateTime;
}

/**
 * Reader state to store for one file. The write replaces the whole stored state:
 * an omitted position, preferences or bookmarks is stored as an empty object or
 * list rather than merged with the previous value.
 */
model FileViewStateUpdate {
  /** Viewer the state is saved by. */
  kind: ViewerKind;
  /** Opaque JSON object; omitted stores an empty object. */
  position?: Record<unknown>;
  /** Opaque JSON object; omitted stores an empty object. */
  preferences?: Record<unknown>;

  /** Reader bookmarks; omitted stores an empty list, and more than 500 entries are rejected. */
  @maxItems(500)
  bookmarks?: Record<unknown>[];
}

/** Filters and paging options of a listing or search. */
model FileListQuery {
  /** Region of the drive to list; the wider scopes list active entries only. */
  @query
  scope?: FileListScope = FileListScope.folder;

  /** Folder to list, or the root of a recursive listing; absent means the drive root. */
  @query
  parentId?: Uuid;

  /** Resolve the listing root relative to the user's drive root. */
  @query
  @maxLength(4096)
  path?: string;

  /** Keep only files or only folders; absent lists both. */
  @query
  kind?: FileKind;

  /** Lifecycle state to list; the drive and recursive scopes accept only active. */
  @query
  status?: FileStatus = FileStatus.active;

  /** File-name search. Text search is case-insensitive; regex uses PostgreSQL-compatible regular expressions. */
  @query
  @maxLength(512)
  search?: string;

  /** Match mode of search; it has no effect on a listing without a search term. */
  @query
  searchType?: FileSearchType = FileSearchType.text;

  /** Keep only entries of these derived categories; absent disables the filter. */
  @query
  category?: FileCategory[];

  /** Keep entries whose metadata was updated at or after this instant. */
  @query
  updatedAfter?: utcDateTime;

  /** Keep entries whose metadata was updated strictly before this instant. */
  @query
  updatedBefore?: utcDateTime;

  /** Sort key of the listing; a value that is not one of the enum members is rejected. */
  @query
  sort?: FileSort = FileSort.name;

  /** Sort direction; a value that is not one of the enum members is rejected. */
  @query
  order?: SortOrder = SortOrder.ascending;
}

/** Batch trash request; the whole batch is applied in a single transaction. */
model FileBulkTrashRequest {
  /** Entries to trash, 1 to 500 IDs; the root of a share the caller does not own cannot be trashed. */
  @minItems(1)
  @maxItems(500)
  fileIds: Uuid[];
}

/** Batch move request; the whole batch is applied in a single transaction. */
model FileBulkMoveRequest {
  /** Entries to move, 1 to 500 IDs; every entry must belong to the destination's owner. */
  @minItems(1)
  @maxItems(500)
  fileIds: Uuid[];

  /** Destination folder; absent moves the entries to the caller's own drive root. */
  parentId?: Uuid;
  /** How to resolve a name already taken at the destination; the default fails the whole batch. */
  conflictPolicy?: NameConflictPolicy = NameConflictPolicy.fail;
}

/** Entries a bulk operation affected. */
model FileBulkResult {
  /** Moved entries in request order, or every trashed entry including the descendants of the roots. */
  items: FileEntry[];
}

/** File count and bytes of one category bucket of a drive. */
model FileCategoryStatistics {
  /** Bucket the counted files fall into. */
  category: FileCategory;
  /** Number of active files in the bucket; folders are never counted. */
  totalFiles: int64;
  /** Summed size of those files in bytes. */
  totalSize: int64;
}

/** Aggregate counters of one user's drive. */
model DriveStatistics {
  /** Number of active files. */
  totalFiles: int64;
  /** Number of active folders. */
  totalFolders: int64;
  /** Summed size of the active files in bytes. */
  totalBytes: int64;
  /** Number of trashed files; trashed folders are not counted. */
  trashedFiles: int64;
  /** Shares that are neither revoked nor expired. */
  activeShares: int64;
  /** Upload sessions still open or completing. */
  openUploads: int64;
}

/** Body of a folder creation request. */
model FolderCreateRequest {
  /** Parent folder; absent creates the folder in the caller's drive root. */
  parentId?: Uuid;

  /** Folder name, 1 to 255 characters and not blank. */
  @minLength(1)
  @maxLength(255)
  name: string;

  /** Only fail is accepted here; replace and rename are rejected as invalid input. */
  conflictPolicy?: NameConflictPolicy = NameConflictPolicy.fail;
  /** Modification time to record; absent records the current time. */
  modTime?: utcDateTime;
}

/** Partial metadata change of one entry; at least one of name and modTime must be set. */
model FileUpdateRequest {
  /** New name, 1 to 255 characters and not blank; absent keeps the current name. */
  @minLength(1)
  @maxLength(255)
  name?: string;

  /** New modification time; absent keeps the current one. */
  modTime?: utcDateTime;
}

/** Destination of a single-entry move within one owner's drive. */
model FileMoveRequest {
  /** Destination folder; absent moves the entry to the caller's drive root. */
  parentId?: Uuid;
  /** How to resolve a name already taken at the destination. */
  conflictPolicy?: NameConflictPolicy = NameConflictPolicy.fail;
}

/** Destination and naming of a copy. */
model FileCopyRequest {
  /** Destination folder; absent copies into the source owner's drive root. */
  parentId?: Uuid;

  /** Name of the copied root, 1 to 255 characters; absent keeps the source name. */
  @minLength(1)
  @maxLength(255)
  name?: string;

  /** How to resolve a name already taken at the destination. */
  conflictPolicy?: NameConflictPolicy = NameConflictPolicy.fail;
}

/** Optional precondition header carrying the generation ETag of an earlier response. */
model OptionalIfMatchHeader {
  /**
   * Generation ETag the change is conditional on, for example "7"; a value that
   * no longer matches the stored generation fails the request with 412, and an
   * absent header applies the change unconditionally.
   */
  @header("If-Match")
  ifMatch?: ETag;
}

/** A changed entry: 200 with the entry body and its fresh generation ETag. */
model FileMutationResponse {
  /** HTTP status of the response. */
  @statusCode
  statusCode: 200;

  /** Generation ETag of the entry after the change. */
  @header("Etag")
  ETag: ETag;

  /** The changed entry. */
  @body
  body: FileEntry;
}

/** A created entry: 201 with the entry body, its generation ETag and its location. */
model FileCreatedResponse {
  /** HTTP status of the response. */
  @statusCode
  statusCode: 201;

  /** Generation ETag of the created entry. */
  @header("Etag")
  ETag: ETag;

  /** Server-relative location of the created entry. */
  @header("Location")
  location: Uri;

  /** The created entry. */
  @body
  body: FileEntry;
}

/** Options accepted by the content download operations. */
model FileContentRequestOptions {
  /**
   * Byte range to stream in the single "bytes=start-end", "bytes=start-" or
   * "bytes=-suffix" form; absent streams the whole file and a range that cannot
   * be served is rejected with 416.
   */
  @header("Range")
  range?: string;

  /**
   * Content ETag of an earlier response; a full-file request that still matches
   * is answered with 304 and no body, while a ranged request ignores it.
   */
  @header("If-None-Match")
  ifNoneMatch?: ETag;

  /** Force a browser download instead of inline display. */
  @query
  download?: "1";
}

/** A complete file body with its caching and disposition metadata. */
model FileContentResponse {
  /** HTTP status of the response. */
  @statusCode
  statusCode: 200;

  /** Length of the delivered body in bytes. */
  @header("Content-Length")
  contentLength: int64;

  /** The server always accepts byte ranges. */
  @header("Accept-Ranges")
  acceptRanges: "bytes";

  /** Strong ETag of the content hash, or of the generation when no hash is recorded. */
  @header("Etag")
  ETag: ETag;

  /** Modification time of the content, sent as an HTTP date. */
  @header("Last-Modified")
  lastModified: utcDateTime;

  /** "inline" or "attachment" plus the RFC 2231 encoded file name. */
  @header("Content-Disposition")
  contentDisposition: string;

  /** The file bytes. */
  @body
  body: bytes;
}

/** A single requested byte range: 206 with the range headers of the file. */
model FilePartialContentResponse {
  /** HTTP status of the response. */
  @statusCode
  statusCode: 206;

  /** Length of the delivered range in bytes, not of the whole file. */
  @header("Content-Length")
  contentLength: int64;

  /** Delivered range and total file size, for example "bytes 0-99/1000". */
  @header("Content-Range")
  contentRange: string;

  /** The server always accepts byte ranges. */
  @header("Accept-Ranges")
  acceptRanges: "bytes";

  /** Strong ETag of the content hash, or of the generation when no hash is recorded. */
  @header("Etag")
  ETag: ETag;

  /** Modification time of the content, sent as an HTTP date. */
  @header("Last-Modified")
  lastModified: utcDateTime;

  /** "inline" or "attachment" plus the RFC 2231 encoded file name. */
  @header("Content-Disposition")
  contentDisposition: string;

  /** The requested file bytes. */
  @body
  body: bytes;
}

/** Content metadata without a body, used to probe a download before requesting it. */
model FileHeadResponse {
  /** HTTP status of the response. */
  @statusCode
  statusCode: 200;

  /** Length of the whole file in bytes. */
  @header("Content-Length")
  contentLength: int64;

  /** The server always accepts byte ranges. */
  @header("Accept-Ranges")
  acceptRanges: "bytes";

  /** Strong ETag of the content hash, or of the generation when no hash is recorded. */
  @header("Etag")
  ETag: ETag;

  /** Modification time of the content, sent as an HTTP date. */
  @header("Last-Modified")
  lastModified: utcDateTime;

  /** "inline" or "attachment" plus the RFC 2231 encoded file name. */
  @header("Content-Disposition")
  contentDisposition: string;
}

@tag("Files")
@route("/v1/files")
@useAuth(TelDriveAuth)
namespace Files {
  /** List one folder or search within the authenticated user's drive. */
  @get
  @operationId("listFiles")
  op list(...CursorQuery, ...FileListQuery):
    | JsonResponse<200, CursorPage<FileEntry>>
    | ErrorResponse<400>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<422>;

  /** Return file or folder metadata. */
  @get
  @route("/{fileId}")
  @operationId("getFile")
  op get(@path fileId: Uuid):
    | FileMutationResponse
    | ErrorResponse<401>
    | ErrorResponse<404>;

  /** Rename a file/folder or update its modification time. */
  @patch
  @route("/{fileId}")
  @operationId("updateFile")
  op update(
    ...OptionalIfMatchHeader,
    @path fileId: Uuid,
    @body body: FileUpdateRequest,
  ):
    | FileMutationResponse
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>
    | ErrorResponse<412>
    | ErrorResponse<422>;

  /** Move a file or folder using metadata only. */
  @post
  @route("/{fileId}/move")
  @operationId("moveFile")
  op move(
    ...OptionalIfMatchHeader,
    @path fileId: Uuid,
    @body body: FileMoveRequest,
  ):
    | FileMutationResponse
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>
    | ErrorResponse<412>
    | ErrorResponse<422>
    | ErrorResponse<403>;

  /** Copy a file or folder to another destination. */
  @post
  @route("/{fileId}/copy")
  @operationId("copyFile")
  op copy(@path fileId: Uuid, @body body: FileCopyRequest):
    | FileCreatedResponse
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>
    | ErrorResponse<422>
    | ErrorResponse<403>;

  /** Move a file or folder to trash. */
  @delete
  @route("/{fileId}")
  @operationId("trashFile")
  op trash(@path fileId: Uuid):
    | EmptyResponse<204>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>
    | ErrorResponse<403>;

  /** Restore a trashed file or folder. */
  @post
  @route("/{fileId}/restore")
  @operationId("restoreFile")
  op restore(@path fileId: Uuid):
    | FileMutationResponse
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>;

  /** Permanently delete a trashed file or folder from the catalog. */
  @delete
  @route("/{fileId}/purge")
  @operationId("purgeFile")
  op purge(@path fileId: Uuid):
    | EmptyResponse<204>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>;

  /** Move every trashed file and folder owned by the authenticated user to deletion pending. */
  @delete
  @route("/trash")
  @operationId("cleanTrash")
  op cleanTrash(): EmptyResponse<204> | ErrorResponse<401>;

  /** Stream complete or partial file content. The filename path segment gives command-line clients a useful output name. */
  @get
  @route("/{fileId}/content/{fileName}")
  @operationId("downloadFile")
  op download(
    ...FileContentRequestOptions,
    @path fileId: Uuid,
    @path fileName: string,
  ):
    | FileContentResponse
    | FilePartialContentResponse
    | EmptyResponse<304>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<416>
    | ErrorResponse<429>
    | ErrorResponse<503>;

  /** Return content metadata without streaming the file. */
  @head
  @route("/{fileId}/content/{fileName}")
  @operationId("headFile")
  op head(@path fileId: Uuid, @path fileName: string):
    | FileHeadResponse
    | ErrorResponse<401>
    | ErrorResponse<404>;

  /** Stream file content using the legacy URL without a filename segment. */
  @get
  @route("/{fileId}/content")
  @operationId("downloadFileLegacy")
  op downloadLegacy(...FileContentRequestOptions, @path fileId: Uuid):
    | FileContentResponse
    | FilePartialContentResponse
    | EmptyResponse<304>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<416>
    | ErrorResponse<429>
    | ErrorResponse<503>;

  /** Return file metadata using the legacy URL without a filename segment. */
  @head
  @route("/{fileId}/content")
  @operationId("headFileLegacy")
  op headLegacy(@path fileId: Uuid):
    | FileHeadResponse
    | ErrorResponse<401>
    | ErrorResponse<404>;

  /** Return the authenticated user's synchronized viewer state for a file. */
  @get
  @route("/{fileId}/view-state")
  @operationId("getFileViewState")
  op getViewState(@path fileId: Uuid):
    | JsonResponse<200, FileViewState>
    | EmptyResponse<204>
    | ErrorResponse<401>
    | ErrorResponse<404>;

  /** Create or replace the authenticated user's synchronized viewer state. */
  @put
  @route("/{fileId}/view-state")
  @operationId("putFileViewState")
  op putViewState(@path fileId: Uuid, @body body: FileViewStateUpdate):
    | JsonResponse<200, FileViewState>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<422>;

  /** Clear the authenticated user's synchronized viewer state. */
  @delete
  @route("/{fileId}/view-state")
  @operationId("deleteFileViewState")
  op deleteViewState(@path fileId: Uuid):
    | EmptyResponse<204>
    | ErrorResponse<401>;

  /** Transactionally move multiple files or folders. */
  @post
  @route("/bulk/move")
  @operationId("bulkMoveFiles")
  op bulkMove(@body body: FileBulkMoveRequest):
    | JsonResponse<200, FileBulkResult>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>
    | ErrorResponse<422>
    | ErrorResponse<403>;

  /** Transactionally move multiple files or folders to trash. */
  @post
  @route("/bulk/trash")
  @operationId("bulkTrashFiles")
  op bulkTrash(@body body: FileBulkTrashRequest):
    | JsonResponse<200, FileBulkResult>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>
    | ErrorResponse<422>
    | ErrorResponse<403>;

  /** Aggregate active file counts and bytes by category. */
  @get
  @route("/statistics/categories")
  @operationId("getFileCategoryStatistics")
  op categoryStatistics():
    | JsonResponse<200, FileCategoryStatistics[]>
    | ErrorResponse<401>;

  /** Aggregate drive-level counters for the authenticated user. */
  @get
  @route("/statistics/drive")
  @operationId("getDriveStatistics")
  op driveStatistics(): JsonResponse<200, DriveStatistics> | ErrorResponse<401>;
}

@tag("Folders")
@route("/v1/folders")
@useAuth(TelDriveAuth)
namespace Folders {
  /** Create a folder in an editable parent folder and return it with its generation ETag and location. */
  @post
  @operationId("createFolder")
  op create(@body body: FolderCreateRequest):
    | FileCreatedResponse
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>
    | ErrorResponse<422>;
}
