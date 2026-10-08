using TypeSpec.Http;

namespace TelDrive;

/** Identifier in canonical UUID textual form. */
@format("uuid")
scalar Uuid extends string;

/** URI in RFC 3986 form, percent-encoded. */
@format("uri")
scalar Uri extends string;

/**
 * Opaque pagination cursor returned as `nextCursor`; clients pass it back unchanged and
 * never parse it.
 */
scalar Cursor extends string;

/**
 * Strong entity tag of a resource: a quoted generation number, or a quoted content hash
 * on download responses.
 */
scalar ETag extends string;

/** BLAKE3 digest of plaintext content, 64 lowercase hex characters. */
scalar Checksum extends string;

/** Auth scheme for the HttpOnly session cookie set by the `/v1/auth/cookie/*` endpoints. */
model CookieAuth is ApiKeyAuth<ApiKeyLocation.cookie, "teldrive_access">;
/** Auth scheme for long-lived API keys, sent in the `X-Api-Key` header. */
model ExternalApiKeyAuth is ApiKeyAuth<ApiKeyLocation.header, "X-Api-Key">;

alias BearerOrCookieAuth = BearerAuth | CookieAuth;

alias TelDriveAuth = BearerAuth | CookieAuth | ExternalApiKeyAuth;

/**
 * One failure: a stable code for clients, a message for humans and optional structured
 * context.
 */
model ErrorDetail {
  /** Stable machine-readable error code. */
  code: string;

  /** Human-readable error message. */
  message: string;

  /** Optional structured context for the caller. */
  details?: Record<unknown>;
}

/** Payload every failing request returns, whatever the status code. */
model ErrorEnvelope {
  /** Machine-readable error code of the failure. */
  error: ErrorDetail;
}

/**
 * Declared error response: `statusCode` selects the status, `body` carries the error
 * envelope.
 */
@error
model ErrorResponse<Status extends int32> {
  /** HTTP status the failure is returned with. */
  @statusCode
  statusCode: Status;

  /** Envelope carrying the error code and message. */
  @body
  body: ErrorEnvelope;
}

/** Declared success response carrying a JSON body of type `Body`. */
model JsonResponse<Status extends int32, Body> {
  /** HTTP status the success is returned with. */
  @statusCode
  statusCode: Status;

  /** Response payload of type `Body`. */
  @body
  body: Body;
}

/** Declared success response with the given status and no body. */
model EmptyResponse<Status extends int32> {
  /** HTTP status the success is returned with. */
  @statusCode
  statusCode: Status;
}

/** One page of a keyset-paginated listing. */
model CursorPage<T> {
  /** Rows of this page, possibly empty but never null. */
  items: T[];
  /** Cursor to send as `cursor` on the next request; omitted when the listing is exhausted. */
  nextCursor?: Cursor;
}

/** Query parameters shared by the keyset-paginated listings. */
model CursorQuery {
  /** Cursor from the previous page's `nextCursor`; a malformed value is rejected with 422. */
  @query
  cursor?: Cursor;

  /**
   * Requested page size, 1-500; omitted means 100 and the server caps the effective
   * size, so a full page can be shorter.
   */
  @query
  @minValue(1)
  @maxValue(500)
  limit?: int32 = 100;
}

/**
 * Request header carrying a generation precondition; mixed into the operations that
 * need one.
 */
model GenerationHeader {
  /** Optimistic concurrency precondition. */
  @header("If-Match")
  ifMatch: ETag;
}

/** Content-digest algorithms the contract accepts; only BLAKE3 is advertised today. */
enum HashAlgorithm {
  /** BLAKE3, a 32-byte digest rendered as lowercase hex. */
  blake3: "blake3",
}

/** Content digest of a file or upload, used to verify transfers. */
model FileHash {
  /** Algorithm that produced `value`. */
  algorithm: HashAlgorithm;
  /** Digest of the plaintext content in that algorithm's encoding. */
  value: Checksum;
}
