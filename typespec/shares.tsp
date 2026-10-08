using TypeSpec.Http;
using TypeSpec.OpenAPI;

namespace TelDrive;

/** Access level a share or grant confers. */
enum SharePermission {
  /** View and download; write operations are refused with 403. */
  read: "read",
  /** Also create, rename, trash and upload inside the shared tree. */
  edit: "edit",
}

/** Request to mint a public share link for one file or folder the caller owns. */
model ShareCreateRequest {
  /** Optional password. The server stores only a password hash. */
  @minLength(1)
  password?: string;

  /** When the link stops working; must be in the future, and omitted means it never expires. */
  expiresAt?: utcDateTime;

  /**
   * Downloads allowed before the link is exhausted; must be positive, and omitted means
   * unlimited.
   */
  @minValue(1)
  maxDownloads?: int64;

  /** Access level of the link; omitted means `read`. */
  permission?: SharePermission = SharePermission.read;
}

/**
 * Partial update of a share; omitted fields keep their value, the `clear*` flags remove
 * one, and a request that sets nothing is rejected.
 */
model ShareUpdateRequest {
  /** Replacement password; must not be combined with `clearPassword`. */
  @minLength(1)
  password?: string;

  /**
   * True removes the password so the link becomes public; must not be combined with
   * `password`.
   */
  clearPassword?: boolean = false;
  /**
   * Replacement expiry; must be in the future and must not be combined with
   * `clearExpiresAt`.
   */
  expiresAt?: utcDateTime;
  /**
   * True removes the expiry so the link no longer ages out; must not be combined with
   * `expiresAt`.
   */
  clearExpiresAt?: boolean = false;

  /**
   * Replacement download limit, which cannot be set below the downloads already
   * counted; must not be combined with `clearMaxDownloads`.
   */
  @minValue(1)
  maxDownloads?: int64;

  /** True removes the download limit; must not be combined with `maxDownloads`. */
  clearMaxDownloads?: boolean = false;
  /** Replacement access level. */
  permission?: SharePermission;
}

/** Query parameters that narrow a public folder listing. */
model PublicShareListQuery {
  /** Folder path relative to the shared folder root. */
  @query
  @maxLength(4096)
  path?: string;

  /**
   * Text matched as a substring or similarity against entry names; omitted lists the
   * whole folder.
   */
  @query
  @maxLength(512)
  search?: string;
}

/** A freshly created share, including the token that is returned only here. */
model ShareCreated {
  /** Share identifier used to update or revoke the link. */
  id: Uuid;
  /** File or folder the link exposes. */
  fileId: Uuid;

  /** Opaque public token returned only when the share is created. */
  token: string;

  /**
   * Path of the link on the deployment host, `/share/<token>`; resolve it against the
   * origin to build a URL.
   */
  publicUrl: Uri;
  /** True when the link asks for a password. */
  passwordProtected: boolean;
  /** When the link stops working; omitted when it never expires. */
  expiresAt?: utcDateTime;
  /** Download limit; omitted when unlimited. */
  maxDownloads?: int64;
  /** Access level the link confers. */
  permission: SharePermission;
  /** When the link was created. */
  createdAt: utcDateTime;
}

/** One share of a file, without its token. */
model ShareSummary {
  /** Share identifier used to update or revoke the link. */
  id: Uuid;
  /** File or folder the link exposes. */
  fileId: Uuid;
  /** True when the link asks for a password. */
  passwordProtected: boolean;
  /** When the link stops working; omitted when it never expires. */
  expiresAt?: utcDateTime;
  /** Download limit; omitted when unlimited. */
  maxDownloads?: int64;
  /** Downloads already charged against the link. */
  downloadCount: int64;
  /** Access level the link confers. */
  permission: SharePermission;
  /** When the link was created. */
  createdAt: utcDateTime;
  /** When the link was revoked; revoked shares stay listed with their state. */
  revokedAt?: utcDateTime;
}

/** Public view of a share, returned to whoever holds its token. */
model PublicShare {
  /** Share identifier. */
  id: Uuid;
  /** The shared file, or the folder acting as the share root. */
  file: FileEntry;
  /** True when the link requires the `X-Share-Password` header. */
  passwordProtected: boolean;
  /** When the link stops working; omitted when it never expires. */
  expiresAt?: utcDateTime;
  /** Access level the link confers. */
  permission: SharePermission;
}

/** Request to grant another account access to one of the caller's files. */
model FileAccessGrantCreateRequest {
  /**
   * Telegram user ID of the account to grant; it must exist, be enabled and differ from
   * the caller.
   */
  granteeUserId: int64;
  /** Access level to grant; omitted means `read`. */
  permission?: SharePermission = SharePermission.read;
  /**
   * When the grant stops applying; must be in the future, and omitted means it never
   * expires.
   */
  expiresAt?: utcDateTime;
}

/**
 * Partial update of a grant; at least one field must be present or the request is
 * rejected with 422.
 */
model FileAccessGrantUpdateRequest {
  /** Replacement access level. */
  permission?: SharePermission;
  /**
   * Replacement expiry, which must be in the future and must not be combined with
   * `clearExpiresAt`.
   */
  expiresAt?: utcDateTime;
  /** True removes the expiry so the grant no longer ages out. */
  clearExpiresAt?: boolean = false;
}

/** One access grant on a file, including the grantee identity. */
model FileAccessGrantSummary {
  /** Grant identifier used to update or revoke it. */
  id: Uuid;
  /** File the grant applies to. */
  fileId: Uuid;
  /** Account that owns the file. */
  ownerId: int64;
  /** Telegram user ID of the account that received access. */
  granteeUserId: int64;
  /** Grantee display name; omitted when unset. */
  granteeDisplayName?: string;
  /** Grantee @username without the leading `@`; omitted when unset. */
  granteeUsername?: string;
  /** Access level the grant confers. */
  permission: SharePermission;
  /** When the grant stops applying; omitted when it never expires. */
  expiresAt?: utcDateTime;
  /** When the grant was created. */
  createdAt: utcDateTime;
}

/** A file another owner granted the caller, with the level granted. */
model SharedWithMeEntry {
  /** The granted file. */
  file: FileEntry;
  /** Access level the caller holds on it. */
  permission: SharePermission;
}

/**
 * Optional password of a public share, sent as a header because the caller is
 * unauthenticated.
 */
model SharePasswordHeader {
  /** Password of the link; omit it for a share that is not password protected. */
  @header("X-Share-Password")
  password?: string;
}

@tag("Shares")
@route("/v1")
@useAuth(TelDriveAuth)
namespace Shares {
  /**
   * Create a public share link for one of the caller's active files; the plaintext
   * token is returned only here.
   */
  @post
  @route("/files/{fileId}/shares")
  @operationId("createShare")
  op create(@path fileId: Uuid, @body body: ShareCreateRequest):
    | JsonResponse<201, ShareCreated>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>
    | ErrorResponse<422>;

  /**
   * List every share created for one file, newest first, including revoked and expired
   * ones so the owner sees their state.
   */
  @get
  @route("/files/{fileId}/shares")
  @operationId("listFileShares")
  op listForFile(...CursorQuery, @path fileId: Uuid):
    | JsonResponse<200, CursorPage<ShareSummary>>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<422>;

  /**
   * Apply a partial update to a share; omitted fields keep their value and the `clear*`
   * flags remove one.
   */
  @patch
  @route("/shares/{shareId}")
  @operationId("updateShare")
  op update(@path shareId: Uuid, @body body: ShareUpdateRequest):
    | JsonResponse<200, ShareSummary>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<422>;

  /**
   * Revoke a share so every later use of its token fails with 410; unknown, foreign or
   * already revoked shares answer 404.
   */
  @delete
  @route("/shares/{shareId}")
  @operationId("revokeShare")
  op revoke(@path shareId: Uuid):
    | EmptyResponse<204>
    | ErrorResponse<401>
    | ErrorResponse<404>;

  /**
   * Grant another account access to one of the caller's files, replacing any live grant
   * for the same file and grantee.
   */
  @post
  @route("/files/{fileId}/grants")
  @operationId("createFileAccessGrant")
  op createGrant(@path fileId: Uuid, @body body: FileAccessGrantCreateRequest):
    | JsonResponse<201, FileAccessGrantSummary>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<409>
    | ErrorResponse<422>;

  /**
   * List the grants on one of the caller's files, newest first; expired grants stay
   * listed and revoked ones are omitted.
   */
  @get
  @route("/files/{fileId}/grants")
  @operationId("listFileAccessGrants")
  op listGrants(@path fileId: Uuid):
    | JsonResponse<200, FileAccessGrantSummary[]>
    | ErrorResponse<401>
    | ErrorResponse<404>;

  /**
   * Patch a live grant with a new permission, a new expiry, or `clearExpiresAt` to drop
   * the expiry; an empty patch is rejected with 422.
   */
  @patch
  @route("/grants/{grantId}")
  @operationId("updateFileAccessGrant")
  op updateGrant(
    @path grantId: Uuid,
    @body body: FileAccessGrantUpdateRequest,
  ):
    | JsonResponse<200, FileAccessGrantSummary>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<422>;

  /**
   * Revoke a grant, removing the grantee's access immediately; unknown, foreign or
   * already revoked grants answer 404.
   */
  @delete
  @route("/grants/{grantId}")
  @operationId("revokeFileAccessGrant")
  op revokeGrant(@path grantId: Uuid):
    | EmptyResponse<204>
    | ErrorResponse<401>
    | ErrorResponse<404>;

  /** List the owner's files that a live share or grant makes reachable, newest first. */
  @get
  @route("/shared")
  @operationId("listShared")
  op shared(...CursorQuery):
    | JsonResponse<200, CursorPage<FileEntry>>
    | ErrorResponse<401>
    | ErrorResponse<422>;

  /** List the files other owners granted the caller, newest grant first. */
  @get
  @route("/shared/with-me")
  @operationId("listSharedWithMe")
  op sharedWithMe(...CursorQuery):
    | JsonResponse<200, CursorPage<SharedWithMeEntry>>
    | ErrorResponse<401>
    | ErrorResponse<422>;
}

@tag("Public shares")
@route("/v1/public/shares")
@useAuth(NoAuth)
namespace PublicShares {
  /**
   * Resolve a share token and return the shared entry; a missing or wrong password
   * answers 401 and a revoked, expired or exhausted link 410.
   */
  @get
  @route("/{token}")
  @operationId("getPublicShare")
  op get(...SharePasswordHeader, @path token: string):
    | JsonResponse<200, PublicShare>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<410>
    | ErrorResponse<429>;

  /** List children within a publicly shared folder. */
  @get
  @route("/{token}/files")
  @operationId("listPublicShareFiles")
  op listFiles(
    ...SharePasswordHeader,
    ...CursorQuery,
    ...PublicShareListQuery,
    @path token: string,
  ):
    | JsonResponse<200, CursorPage<FileEntry>>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<410>
    | ErrorResponse<422>
    | ErrorResponse<429>;

  /**
   * Download one file inside a shared folder; a successful call consumes one download
   * of the link's quota.
   */
  @get
  @route("/{token}/files/{fileId}/content/{fileName}")
  @operationId("downloadPublicShareFile")
  op downloadFile(
    ...SharePasswordHeader,
    ...FileContentRequestOptions,
    @path token: string,
    @path fileId: Uuid,
    @path fileName: string,
  ):
    | FileContentResponse
    | FilePartialContentResponse
    | EmptyResponse<304>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<410>
    | ErrorResponse<416>
    | ErrorResponse<429>
    | ErrorResponse<503>;

  /**
   * Return download metadata for one file inside a shared folder without a body, under
   * the same token and password rules as the download.
   */
  @head
  @route("/{token}/files/{fileId}/content/{fileName}")
  @operationId("headPublicShareFile")
  op headFile(
    ...SharePasswordHeader,
    @path token: string,
    @path fileId: Uuid,
    @path fileName: string,
  ):
    | FileHeadResponse
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<410>
    | ErrorResponse<429>
    | ErrorResponse<503>;

  /**
   * Download the file a share token points at; a successful call consumes one download
   * of the link's quota.
   */
  @get
  @route("/{token}/content/{fileName}")
  @operationId("downloadPublicShare")
  op download(
    ...SharePasswordHeader,
    ...FileContentRequestOptions,
    @path token: string,
    @path fileName: string,
  ):
    | FileContentResponse
    | FilePartialContentResponse
    | EmptyResponse<304>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<410>
    | ErrorResponse<416>
    | ErrorResponse<429>
    | ErrorResponse<503>;

  /**
   * Return download metadata for the share root without a body, under the same token
   * and password rules as the download.
   */
  @head
  @route("/{token}/content/{fileName}")
  @operationId("headPublicShare")
  op head(...SharePasswordHeader, @path token: string, @path fileName: string):
    | FileHeadResponse
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<410>
    | ErrorResponse<429>
    | ErrorResponse<503>;

  /**
   * Download one file inside a shared folder through the pre-v1 URL form that omits the
   * filename; otherwise identical to the current download.
   */
  @get
  @route("/{token}/files/{fileId}/content")
  @operationId("downloadPublicShareFileLegacy")
  op downloadFileLegacy(
    ...SharePasswordHeader,
    ...FileContentRequestOptions,
    @path token: string,
    @path fileId: Uuid,
  ):
    | FileContentResponse
    | FilePartialContentResponse
    | EmptyResponse<304>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<410>
    | ErrorResponse<416>
    | ErrorResponse<429>
    | ErrorResponse<503>;

  /** HEAD form of the legacy single-file download path, kept for older clients. */
  @head
  @route("/{token}/files/{fileId}/content")
  @operationId("headPublicShareFileLegacy")
  op headFileLegacy(
    ...SharePasswordHeader,
    @path token: string,
    @path fileId: Uuid,
  ):
    | FileHeadResponse
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<410>
    | ErrorResponse<429>
    | ErrorResponse<503>;

  /**
   * Download the share root through the pre-v1 URL form that omits the filename;
   * otherwise identical to the current download.
   */
  @get
  @route("/{token}/content")
  @operationId("downloadPublicShareLegacy")
  op downloadLegacy(
    ...SharePasswordHeader,
    ...FileContentRequestOptions,
    @path token: string,
  ):
    | FileContentResponse
    | FilePartialContentResponse
    | EmptyResponse<304>
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<410>
    | ErrorResponse<416>
    | ErrorResponse<429>
    | ErrorResponse<503>;

  /** HEAD form of the legacy share-root download path, kept for older clients. */
  @head
  @route("/{token}/content")
  @operationId("headPublicShareLegacy")
  op headLegacy(...SharePasswordHeader, @path token: string):
    | FileHeadResponse
    | ErrorResponse<401>
    | ErrorResponse<404>
    | ErrorResponse<410>
    | ErrorResponse<429>
    | ErrorResponse<503>;

  /**
   * Create a folder inside an edit-enabled share, attributed to the share owner; a
   * read-only share answers 403 and a duplicate name is a conflict.
   */
  @post
  @route("/{token}/folders")
  @operationId("createPublicShareFolder")
  op createFolder(
    ...SharePasswordHeader,
    @path token: string,
    @body body: FolderCreateRequest,
  ):
    | JsonResponse<201, FileEntry>
    | ErrorResponse<401>
    | ErrorResponse<403>
    | ErrorResponse<404>
    | ErrorResponse<410>
    | ErrorResponse<422>;

  /**
   * Rename one entry inside an edit-enabled share; `If-Match` must carry the entry's
   * current generation or the call answers 412.
   */
  @patch
  @route("/{token}/files/{fileId}")
  @operationId("updatePublicShareFile")
  op updateFile(
    ...SharePasswordHeader,
    @path token: string,
    @path fileId: Uuid,
    @header("If-Match") ifMatch: ETag,
    @body body: FileUpdateRequest,
  ):
    | JsonResponse<200, FileEntry>
    | ErrorResponse<401>
    | ErrorResponse<403>
    | ErrorResponse<404>
    | ErrorResponse<410>
    | ErrorResponse<412>
    | ErrorResponse<422>;

  /**
   * Move one entry inside an edit-enabled share to the owner's trash; the share root
   * itself is refused with 403.
   */
  @delete
  @route("/{token}/files/{fileId}")
  @operationId("trashPublicShareFile")
  op trashFile(
    ...SharePasswordHeader,
    @path token: string,
    @path fileId: Uuid,
  ):
    | EmptyResponse<204>
    | ErrorResponse<401>
    | ErrorResponse<403>
    | ErrorResponse<404>
    | ErrorResponse<410>;

  /**
   * Open an upload session inside an edit-enabled share; the finished file belongs to
   * the share owner and a name conflict answers 409.
   */
  @post
  @route("/{token}/uploads")
  @operationId("createPublicShareUpload")
  op createUpload(
    ...SharePasswordHeader,
    @path token: string,
    @body body: UploadCreateRequest,
  ):
    | JsonResponse<201, UploadSession>
    | ErrorResponse<401>
    | ErrorResponse<403>
    | ErrorResponse<404>
    | ErrorResponse<409>
    | ErrorResponse<410>
    | ErrorResponse<422>;

  /**
   * Store one part of a public-share upload; a repeated part with the same size and
   * checksum returns 200 instead of 201.
   */
  @put
  @route("/{token}/uploads/{uploadId}/parts/{partNo}")
  @operationId("putPublicShareUploadPart")
  op putUploadPart(
    ...SharePasswordHeader,
    ...UploadPartHeaders,
    @path token: string,
    @path uploadId: Uuid,
    @path @minValue(1) partNo: int32,
    @body body: bytes,
  ):
    | UploadPartStoredResponse
    | ErrorResponse<400>
    | ErrorResponse<401>
    | ErrorResponse<403>
    | ErrorResponse<404>
    | ErrorResponse<409>
    | ErrorResponse<410>
    | ErrorResponse<422>
    | ErrorResponse<503>;

  /**
   * Finalize a public-share upload into a file owned by the share owner and return the
   * entry with its ETag; missing parts answer 409.
   */
  @post
  @route("/{token}/uploads/{uploadId}/complete")
  @operationId("completePublicShareUpload")
  op completeUpload(
    ...SharePasswordHeader,
    @path token: string,
    @path uploadId: Uuid,
  ):
    | UploadCompletedResponse
    | ErrorResponse<401>
    | ErrorResponse<403>
    | ErrorResponse<404>
    | ErrorResponse<409>
    | ErrorResponse<410>
    | ErrorResponse<422>;

  /** Discard a public-share upload session and its stored parts. */
  @delete
  @route("/{token}/uploads/{uploadId}")
  @operationId("abortPublicShareUpload")
  op abortUpload(
    ...SharePasswordHeader,
    @path token: string,
    @path uploadId: Uuid,
  ):
    | EmptyResponse<204>
    | ErrorResponse<401>
    | ErrorResponse<403>
    | ErrorResponse<404>
    | ErrorResponse<410>;
}
