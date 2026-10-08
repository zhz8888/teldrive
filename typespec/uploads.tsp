using TypeSpec.Http;
using TypeSpec.OpenAPI;

namespace TelDrive;

/**
 * Lifecycle of an upload session. A session is created open and only an open
 * session accepts parts; completion publishes the file and moves the session
 * through completing to completed inside one transaction, while abort and the
 * expiry sweep retire it as aborted or expired.
 */
enum UploadState {
  /** Accepts new parts and may be completed; the only state in which a part can be claimed. */
  open: "open",
  /**
   * Internal step of the completion transaction, so a session is only ever
   * observed as open or as completed.
   */
  completing: "completing",
  /** The file was published; the session is immutable and a repeated complete returns that file. */
  completed: "completed",
  /** The client aborted it; its stored parts are deleted by the cleanup sweep and retries are no-ops. */
  aborted: "aborted",
  /**
   * The deadline passed and the sweep retired it; its parts are cleaned up and
   * further writes or a completion are rejected with 409.
   */
  expired: "expired",
}

/**
 * Lifecycle of one part. A part is claimed under a lease as uploading, becomes
 * stored once its bytes are readable, or failed when the attempt was abandoned;
 * a stored or failed part can be claimed again under a new lease.
 */
enum UploadPartState {
  /** Column default only; a part is claimed directly as uploading, so no response carries this value. */
  reserved: "reserved",
  /**
   * Leased to a single writer; the lease is renewed while the upload runs and
   * otherwise lapses after about a minute, letting another attempt take over.
   */
  uploading: "uploading",
  /** The bytes are stored and the part is immutable; an identical retry is answered from the stored row. */
  stored: "stored",
  /** The attempt was abandoned with an error code; the part can be claimed again. */
  failed: "failed",
}

/**
 * Request that opens a durable upload session. The destination name is not
 * reserved here: a clash with an existing entry is resolved when the session is
 * completed, following the requested conflict policy.
 */
model UploadCreateRequest {
  /** Destination folder; absent uploads into the caller's own drive root. */
  parentId?: Uuid;

  /** Destination file name, 1 to 255 characters and not blank; stored verbatim. */
  @minLength(1)
  @maxLength(255)
  name: string;

  /** Expected size in bytes, or -1 when the size is unknown until completion. */
  @minValue(-1)
  size: int64;

  /** Media type to record on the published file; absent leaves the entry without one. */
  mimeType?: string;
  /** Modification time to record on the published file; the zero instant means the current time. */
  modTime: utcDateTime;
  /** Expected whole-file BLAKE3 tree hash, compared at completion; absent skips the comparison. */
  hash?: FileHash;
  /** Store the content encrypted under the caller's key; rejected when the deployment has no active key. */
  encryption?: boolean = false;
  /**
   * How completion resolves an occupied destination name: fail, replace or
   * rename; anything else is rejected, and replace overwrites only a regular file.
   */
  conflictPolicy?: NameConflictPolicy = NameConflictPolicy.fail;

  /** Optional preferred part size. The server may choose a different value. */
  @minValue(1048576)
  preferredPartSize?: int64;
}

/**
 * Durable upload session. It outlives the connection that created it: parts are
 * held in PostgreSQL and Telegram, so a client resumes by listing the parts it
 * already stored and continues from the first missing one.
 */
model UploadSession {
  /** Session UUID; the uploadId path parameter of the session endpoints. */
  id: Uuid;
  /** Destination folder; absent means the drive root. */
  parentId?: Uuid;
  /** Destination file name; a rename-policy completion replaces it with a free " (n)" variant. */
  name: string;
  /** Total plaintext size in bytes, or -1 while unknown; completion then derives it from the stored parts. */
  expectedSize: int64;
  /** Expected whole-file tree hash, compared at completion; absent skips that comparison. */
  expectedHash?: FileHash;
  /** Media type recorded at creation; absent when the request carried none. */
  mimeType?: string;
  /** Modification time recorded on the published file. */
  modTime: utcDateTime;
  /** Whether the parts are stored encrypted and decrypted by the server on read. */
  encryption: boolean;
  /** Policy completion applies to an occupied destination name (fail, replace or rename). */
  conflictPolicy: NameConflictPolicy;
  /** Plaintext size in bytes every part but the last must have; the last part holds the remainder. */
  partSize: int64;
  /** Lifecycle state of the session; see UploadState. */
  state: UploadState;
  /**
   * Deadline for claiming parts and completing, fixed at creation (the server's
   * session TTL, 7 days by default). A past deadline makes part writes and
   * completion fail with 410 while the session is still open; the expiry sweep
   * then moves it to expired.
   */
  expiresAt: utcDateTime;
  /** Time the session was created. */
  createdAt: utcDateTime;
  /** Time completion published the file; absent until the session reaches completed. */
  completedAt?: utcDateTime;
  /** The published file; absent until the session reaches completed. */
  fileId?: Uuid;
}

/**
 * One part of an upload session: a fixed slice of the plaintext, written under a
 * lease so a stalled writer cannot overwrite a newer attempt, and copied into the
 * published file's parts on completion.
 */
model UploadPart {
  /** Session the part belongs to. */
  uploadId: Uuid;
  /** 1-based part number; the stored parts of a session tile the file from 1 without gaps. */
  partNo: int32;
  /** Lease and write state of this part; see UploadPartState. */
  state: UploadPartState;
  /** Plaintext bytes this part covers, as declared when it was claimed. */
  plainSize: int64;
  /** Bytes the part occupies in storage, larger than plainSize once encrypted; absent until stored. */
  storedSize?: int64;
  /**
   * BLAKE3 tree hash of the part plaintext in lowercase hex; absent until the
   * part is stored, and for a part stored without hashing.
   */
  checksum?: Checksum;
  /** Time the part was first claimed. */
  createdAt: utcDateTime;
  /** Time the part row last changed, through claim, lease renewal, store or failure. */
  updatedAt: utcDateTime;
}

/**
 * One UTC day of completed upload activity; days without a completion are
 * reported as zero rows instead of being omitted.
 */
model UploadDailyStatistics {
  /** Day the totals cover, at midnight UTC. */
  date: plainDate;
  /** Summed plaintext bytes of the sessions completed that day. */
  uploadedBytes: int64;
  /** Number of sessions completed that day. */
  completedFiles: int64;
}

/** Kind of source a background import reads from, which decides the required field. */
enum UploadImportSourceType {
  /** Absolute server filesystem path; requires the admin role and a configured import root. */
  local: "local",
  /** http or https URL fetched by the server; private and loopback addresses are refused. */
  http: "http",
}

/**
 * One import source. The type decides whether path or url is required, and the
 * remaining fields narrow what the source expands to.
 */
model UploadImportSource {
  /** Source kind; selects whether path or url is used. */
  type: UploadImportSourceType;
  /** Absolute path of a file, or of a directory walked recursively; required for a local source. */
  path?: string;
  /** http or https URL of a single file; required for an http source. */
  url?: url;
  /** Extra request headers for this URL, merged over the batch headers with these winning. */
  headers?: Record<string>;
  /** Slash-separated path to store the file at inside the destination, overriding its derived name. */
  destinationPath?: string;
  /** Globs that skip matching files of this source, relative to the source or its URL. */
  exclude?: string[];
}

/**
 * Server-side batch import: the request is queued as a background job, so the
 * response is the job, not the imported files.
 */
model UploadImportRequest {
  /** Destination folder UUID or absolute drive path from root. */
  destination: string;

  /** Sources to import; at least one is required. */
  sources: UploadImportSource[];
  /** Default request headers for every remote source, overridden per source. */
  headers?: Record<string>;
  /** Globs that skip files of every source, applied on top of the per-source patterns. */
  exclude?: string[];
  /** Smallest file size to import, inclusive, written as a human-readable size such as "10MiB". */
  minSize?: string;
  /** Largest file size to import, inclusive and written like minSize; a smaller maxSize fails the job. */
  maxSize?: string;

  /** Parts of one file transferred at once; a value above 16 is rejected. */
  @minValue(1)
  @maxValue(16)
  partConcurrency?: int32 = 4;

  /** Preferred upload part size. Values are aligned to the nearest 16 MiB. */
  @minValue(67108864)
  @maxValue(2097152000)
  chunkSize?: int64 = 536870912;

  /** Store the imported files encrypted under the active key; requires a configured key version. */
  encryption?: boolean = false;
}

/** Headers and body contract of the single-part upload. */
model UploadPartHeaders {
  /** Media type of the part body; the contract fixes the only accepted value. */
  @header("Content-Type")
  contentType: "application/octet-stream";

  /**
   * Byte length of the plaintext part. It must equal the session's partSize
   * except for the final part, the body must yield exactly this many bytes, and
   * a zero length is rejected as invalid.
   */
  @header("Content-Length")
  @minValue(0)
  contentLength: int64;

  /** BLAKE3 checksum of the plaintext part. */
  @header("X-Part-Checksum")
  checksum?: Checksum;
}

/** Result of storing one part: 200 when it was already stored, 201 when it was written. */
model UploadPartStoredResponse {
  /** 200 for an idempotent retry of an identical part, 201 for a freshly stored one. */
  @statusCode
  statusCode: 200 | 201;

  /** The stored part as recorded by the session. */
  @body
  body: UploadPart;
}

/** Successful completion: the published entry, its generation ETag and its location. */
model UploadCompletedResponse {
  /** Status of a successful completion; the server currently always answers 201. */
  @statusCode
  statusCode: 200 | 201;

  /** Generation ETag of the published entry. */
  @header("Etag")
  ETag: ETag;

  /** Server-relative location of the published entry, "/files/{fileId}". */
  @header("Location")
  location: Uri;

  /** The published entry. */
  @body
  body: FileEntry;
}

@tag("Uploads")
@route("/v1/uploads")
@useAuth(TelDriveAuth)
namespace Uploads {
  /** List resumable upload sessions owned by the authenticated user. */
  @get
  @operationId("listUploads")
  op list(...CursorQuery, @query state?: UploadState):
    | JsonResponse<200, CursorPage<UploadSession>>
    | ErrorResponse<401>
    | ErrorResponse<422>;

  /** Return completed upload volume grouped by UTC day. */
  @get
  @route("/statistics")
  @operationId("getUploadStatistics")
  op statistics(
    /** Number of days to report, 1 to 366, ending with today in UTC; omitted means 30. */
    @query
    @minValue(1)
    @maxValue(366)
    days?: int32 = 30,
  ):
    | JsonResponse<200, UploadDailyStatistics[]>
    | ErrorResponse<401>
    | ErrorResponse<422>;

  /** Queue local paths, recursive folders, and HTTP URLs for server-side upload. */
  @post
  @route("/imports")
  @operationId("createUploadImport")
  op createImport(@body body: UploadImportRequest):
    | JsonResponse<202, Job>
    | ErrorResponse<400>
    | ErrorResponse<401>
    | ErrorResponse<422>
    | ErrorResponse<503>
    | ErrorResponse<403>;

  /** Create a durable upload session. */
  @post
  @operationId("createUpload")
  op create(@body body: UploadCreateRequest):
    | JsonResponse<201, UploadSession>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>
    | ErrorResponse<413>
    | ErrorResponse<422>
    | ErrorResponse<429>;

  /** Inspect an upload for resume and conflict handling. */
  @get
  @route("/{uploadId}")
  @operationId("getUpload")
  op get(@path uploadId: Uuid):
    | JsonResponse<200, UploadSession>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<410>
    | ErrorResponse<403>;

  /** List parts already known to the upload session. */
  @get
  @route("/{uploadId}/parts")
  @operationId("listUploadParts")
  op listParts(...CursorQuery, @path uploadId: Uuid):
    | JsonResponse<200, CursorPage<UploadPart>>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<410>
    | ErrorResponse<422>
    | ErrorResponse<403>;

  /**
   * Upload one part. The `(uploadId, partNo)` pair is the idempotency key.
   * An exact retry returns the existing stored part; conflicting size/checksum returns 409.
   */
  @put
  @route("/{uploadId}/parts/{partNo}")
  @operationId("putUploadPart")
  op putPart(
    ...UploadPartHeaders,
    @path uploadId: Uuid,
    @path @minValue(1) partNo: int32,
    @body body: bytes,
  ):
    | UploadPartStoredResponse
    | ErrorResponse<400>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>
    | ErrorResponse<410>
    | ErrorResponse<413>
    | ErrorResponse<422>
    | ErrorResponse<429>
    | ErrorResponse<503>
    | ErrorResponse<403>;

  /**
   * Validate all parts and transactionally publish the file. A successful response
   * guarantees immediate read-after-write visibility.
   */
  @post
  @route("/{uploadId}/complete")
  @operationId("completeUpload")
  op complete(@path uploadId: Uuid):
    | UploadCompletedResponse
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>
    | ErrorResponse<410>
    | ErrorResponse<422>
    | ErrorResponse<403>;

  /** Abort an upload and schedule physical Telegram cleanup. */
  @delete
  @route("/{uploadId}")
  @operationId("abortUpload")
  op abort(@path uploadId: Uuid):
    | EmptyResponse<204>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>
    | ErrorResponse<403>;
}
