using TypeSpec.Http;
using TypeSpec.OpenAPI;

namespace TelDrive;

/** Phone-number login request; the server asks Telegram to send a login code to that number. */
model TelegramLoginStartRequest {
  /** Phone number in E.164 format. */
  @pattern("^\\+[1-9][0-9]{7,14}$")
  phoneNumber: string;
}

/** Pending phone login flow, continued with the verify-code and verify-password operations. */
model TelegramLoginStartResponse {
  /**
   * Opaque identifier of the flow; pass it back to the verify operations while it is
   * unexpired.
   */
  flowId: Uuid;
  /** When the flow stops being usable; later verify calls answer 410. */
  expiresAt: utcDateTime;
  /**
   * True while the account's two-step password is still needed, so the client must call
   * the verify-password operation.
   */
  passwordRequired: boolean;
}

/** Input that a QR login flow is still waiting for. */
enum TelegramQRLoginState {
  /** The QR code has not been approved yet; the client polls again. */
  pending: "pending",
  /** The phone approved the code and the account's two-step password is now required. */
  passwordRequired: "password_required",
}

/** Current challenge of a QR login flow, repeated on every poll that has not completed. */
model TelegramQRLoginResponse {
  /** Opaque identifier of the flow; pass it back to the poll operations. */
  flowId: Uuid;
  /** When the flow stops being usable; polling after this instant answers 410. */
  expiresAt: utcDateTime;
  /** Which input the flow still needs. */
  state: TelegramQRLoginState;
  /** `tg://` link to render as a QR code; omitted once the password prompt was reached. */
  qrUrl?: string;
  /**
   * When Telegram stops accepting the QR token, after which a poll returns a fresh
   * link; omitted at the password prompt.
   */
  qrExpiresAt?: utcDateTime;
}

/** Poll request naming the QR flow to advance. */
model TelegramQRLoginPollRequest {
  /** Identifier returned by the QR start operation. */
  flowId: Uuid;
}
/** Login code that Telegram sent to the phone, submitted for a pending flow. */
model TelegramCodeVerifyRequest {
  /** Identifier returned by a start operation; it must still be unexpired. */
  flowId: Uuid;

  /** Code Telegram sent to the phone, 1 to 16 characters. */
  @minLength(1)
  @maxLength(16)
  code: string;
}

/** Two-step password submitted for a flow that reported `passwordRequired`. */
model TelegramPasswordVerifyRequest {
  /** Identifier of the flow that asked for the password. */
  flowId: Uuid;

  /** The account's Telegram two-step password. */
  @minLength(1)
  password: string;
}

/**
 * Token refresh request; the refresh token is itself the credential, so no
 * Authorization header is needed.
 */
model RefreshTokenRequest {
  /**
   * Refresh token from a previous login or refresh; it is rotated, so the value sent
   * stops working.
   */
  refreshToken: string;
}

/** Refresh token read from the HttpOnly `teldrive_refresh` cookie of a browser session. */
model RefreshCookie {
  /** Cookie value set by a cookie login or a cookie refresh. */
  @cookie("teldrive_refresh")
  refreshToken: string;
}

/** Credential pair issued by a completed login or a token refresh. */
model TokenPair {
  /** Signed bearer token to send as `Authorization: Bearer` on API requests. */
  accessToken: string;
  /**
   * Opaque secret that obtains the next pair; it is rotated on every refresh, so the
   * value just used becomes invalid.
   */
  refreshToken: string;
  /** Always `Bearer`. */
  tokenType: "Bearer";
  /** Access token lifetime in seconds. */
  expiresIn: int32;
}

/**
 * Session state returned by a browser cookie login; the tokens themselves stay in
 * HttpOnly cookies.
 */
model CookieSession {
  /** Always true: the model is only returned once a session was established. */
  authenticated: true;
  /** When the refresh session expires; the access cookie is renewed silently before then. */
  expiresAt: utcDateTime;
}

/** Cookie login response: sets both session cookies and returns the new session state. */
model CookieSessionResponse<Status extends int32> {
  /** HTTP status the session was established with. */
  @statusCode
  statusCode: Status;

  /** `Set-Cookie` values for the access and refresh cookies; all of them must be stored. */
  @header("Set-Cookie")
  setCookie: string[];

  /** State of the new session. */
  @body
  body: CookieSession;
}

/** Cookie logout response: clears both session cookies and carries no body. */
model CookieSessionNoContent<Status extends int32> {
  /** HTTP status the logout answered with (204). */
  @statusCode
  statusCode: Status;

  /** Already expired `Set-Cookie` values that delete both session cookies. */
  @header("Set-Cookie")
  setCookie: string[];
}

/** Account role, in descending privilege. */
enum UserRole {
  /**
   * Deployment owner: full access, and the only account that can never be demoted or
   * revoked.
   */
  owner: "owner",
  /** May manage accounts, jobs and queues in addition to file access. */
  admin: "admin",
  /** Regular account with file access only. */
  user: "user",
}

/**
 * Administrative patch of one account; at least one field must be present or the
 * request is rejected with 422.
 */
model UserAdminUpdateRequest {
  /**
   * New role; only `admin` and `user` are accepted, so no account can be promoted to
   * owner here.
   */
  role?: UserRole;
  /** True disables the account and revokes its sessions and API keys; false re-enables it. */
  disabled?: boolean;
}

/** Account row as shown in the admin console. */
model AdminUserSummary {
  /** Telegram user ID of the account. */
  userId: int64;
  /** Telegram display name; omitted when Telegram reports none. */
  displayName?: string;
  /** Telegram @username without the leading `@`; omitted when the account has none. */
  username?: string;
  /** Whether the Telegram account has a premium subscription. */
  premium: boolean;
  /** Current role of the account. */
  role: UserRole;
  /** Whether the account is disabled and therefore refuses to authenticate. */
  disabled: boolean;
  /** When the account first signed in to this deployment. */
  createdAt: utcDateTime;
  /** When the account row was last changed. */
  updatedAt: utcDateTime;
}

/** Lightweight account identity returned by user search. */
model UserSearchResult {
  /** Telegram user ID; pass it as `granteeUserId` when granting access. */
  userId: int64;
  /** Display name; omitted when unset. */
  displayName?: string;
  /** Telegram @username without the leading `@`; omitted when unset. */
  username?: string;
}

/** Profile of the authenticated account. */
model UserProfile {
  /** Telegram user ID of the account. */
  userId: int64;
  /** Display name; omitted when unset. */
  displayName?: string;
  /** Telegram @username without the leading `@`; omitted when unset. */
  username?: string;
  /** Whether the Telegram account has a premium subscription. */
  premium: boolean;
  /** Current role of the account. */
  role: UserRole;
  /**
   * Permission strings derived from the role, for example `files.write` or
   * `system.manageUsers`; clients use them to show or hide controls.
   */
  capabilities: string[];
  /** When the account first signed in to this deployment. */
  createdAt: utcDateTime;
}

/** Small Telegram profile photo of the authenticated account. */
model ProfilePhotoResponse {
  /** Always 200; an account without a photo answers 204 with no body instead. */
  @statusCode
  statusCode: 200;

  /** Photo size in bytes. */
  @header("Content-Length")
  contentLength: int64;

  /** Cache directive the server sends; the photo may be cached privately for a day. */
  @header("Cache-Control")
  cacheControl: string;

  /** Version of the photo, derived from the Telegram photo ID. */
  @header("Etag")
  ETag: ETag;

  /** Always `inline; filename="profile.jpeg"`. */
  @header("Content-Disposition")
  contentDisposition: string;

  /** Raw JPEG bytes of the photo. */
  @body
  body: bytes;
}
/** Request to mint an API key for the authenticated account. */
model ApiKeyCreateRequest {
  /** Label shown in key listings, 1 to 120 characters. */
  @minLength(1)
  @maxLength(120)
  name: string;

  /** When the key stops authenticating; omitted means it never expires. */
  expiresAt?: utcDateTime;
}

/** Newly minted API key; its plaintext secret is returned only in this response. */
model ApiKeyCreated {
  /** Key identifier used to revoke it. */
  id: Uuid;
  /** Label given at creation. */
  name: string;

  /** Returned once. The server stores only a hash. */
  secret: string;

  /** When the key was created. */
  createdAt: utcDateTime;
  /** Expiry given at creation; omitted when the key never expires. */
  expiresAt?: utcDateTime;
}

/** One API key of the account, without its secret. */
model ApiKeySummary {
  /** Key identifier used to revoke it. */
  id: Uuid;
  /** Label given at creation. */
  name: string;
  /** When the key was created. */
  createdAt: utcDateTime;
  /** When the key last authenticated a request; omitted while it has never been used. */
  lastUsedAt?: utcDateTime;
  /** When the key stops authenticating; a key past its expiry is never listed. */
  expiresAt?: utcDateTime;
  /** When the key was revoked; revoked keys are not listed, so this is normally absent. */
  revokedAt?: utcDateTime;
}

/** One TelDrive login session of the account. */
model SessionSummary {
  /** Session identifier used to revoke it. */
  id: Uuid;
  /** True for the session backing the credential that made this request. */
  current: boolean;
  /** When the session was created. */
  createdAt: utcDateTime;
  /** When the session last authenticated a request; omitted while it has not been used yet. */
  lastUsedAt?: utcDateTime;
  /** When the session stops being usable. */
  expiresAt: utcDateTime;
  /**
   * When the session was revoked; the listing only returns live sessions, so this is
   * normally absent.
   */
  revokedAt?: utcDateTime;
}

@tag("Authentication")
@route("/v1/auth")
@useAuth(NoAuth)
namespace Authentication {
  /** Begin Telegram authentication and send a login code. */
  @post
  @route("/telegram/start")
  @operationId("telegramLoginStart")
  op telegramStart(@body body: TelegramLoginStartRequest):
    | JsonResponse<201, TelegramLoginStartResponse>
    | ErrorResponse<400>
    | ErrorResponse<409>
    | ErrorResponse<422>
    | ErrorResponse<429>;

  /** Begin a resumable Telegram QR login flow. */
  @post
  @route("/telegram/qr/start")
  @operationId("telegramQRLoginStart")
  op qrStart():
    | JsonResponse<201, TelegramQRLoginResponse>
    | ErrorResponse<400>
    | ErrorResponse<409>
    | ErrorResponse<422>
    | ErrorResponse<429>;

  /** Poll a Telegram QR login flow. Any API replica can resume the encrypted flow state. */
  @post
  @route("/telegram/qr/poll")
  @operationId("telegramQRLoginPoll")
  op qrPoll(@body body: TelegramQRLoginPollRequest):
    | JsonResponse<200, TokenPair>
    | JsonResponse<202, TelegramQRLoginResponse>
    | ErrorResponse<400>
    | ErrorResponse<401>
    | ErrorResponse<410>
    | ErrorResponse<422>
    | ErrorResponse<429>;
  /** Verify the Telegram login code. */
  @post
  @route("/telegram/verify-code")
  @operationId("telegramLoginVerifyCode")
  op verifyCode(@body body: TelegramCodeVerifyRequest):
    | JsonResponse<200, TokenPair>
    | JsonResponse<202, TelegramLoginStartResponse>
    | ErrorResponse<400>
    | ErrorResponse<401>
    | ErrorResponse<410>
    | ErrorResponse<422>
    | ErrorResponse<429>;

  /** Complete Telegram two-step verification. */
  @post
  @route("/telegram/verify-password")
  @operationId("telegramLoginVerifyPassword")
  op verifyPassword(@body body: TelegramPasswordVerifyRequest):
    | JsonResponse<200, TokenPair>
    | ErrorResponse<400>
    | ErrorResponse<401>
    | ErrorResponse<410>
    | ErrorResponse<422>
    | ErrorResponse<429>;

  /** Rotate an access/refresh token pair. */
  @post
  @route("/refresh")
  @operationId("refreshSession")
  op refresh(@body body: RefreshTokenRequest):
    | JsonResponse<200, TokenPair>
    | ErrorResponse<401>
    | ErrorResponse<429>;

  /** Complete Telegram code verification and establish an HttpOnly cookie session. */
  @post
  @route("/cookie/telegram/verify-code")
  @operationId("cookieTelegramLoginVerifyCode")
  op cookieVerifyCode(@body body: TelegramCodeVerifyRequest):
    | CookieSessionResponse<200>
    | JsonResponse<202, TelegramLoginStartResponse>
    | ErrorResponse<400>
    | ErrorResponse<401>
    | ErrorResponse<410>
    | ErrorResponse<422>
    | ErrorResponse<429>;

  /** Complete Telegram password verification and establish an HttpOnly cookie session. */
  @post
  @route("/cookie/telegram/verify-password")
  @operationId("cookieTelegramLoginVerifyPassword")
  op cookieVerifyPassword(@body body: TelegramPasswordVerifyRequest):
    | CookieSessionResponse<200>
    | ErrorResponse<400>
    | ErrorResponse<401>
    | ErrorResponse<410>
    | ErrorResponse<422>
    | ErrorResponse<429>;

  /** Poll Telegram QR login and establish an HttpOnly cookie session when authorized. */
  @post
  @route("/cookie/telegram/qr/poll")
  @operationId("cookieTelegramQRLoginPoll")
  op cookieQrPoll(@body body: TelegramQRLoginPollRequest):
    | CookieSessionResponse<200>
    | JsonResponse<202, TelegramQRLoginResponse>
    | ErrorResponse<400>
    | ErrorResponse<401>
    | ErrorResponse<410>
    | ErrorResponse<422>
    | ErrorResponse<429>;

  /** Rotate session cookies using the HttpOnly refresh cookie. */
  @post
  @route("/cookie/refresh")
  @operationId("refreshCookieSession")
  op cookieRefresh(...RefreshCookie):
    | CookieSessionResponse<200>
    | ErrorResponse<401>
    | ErrorResponse<429>;

  /** Revoke the cookie session and clear its HttpOnly cookies. */
  @post
  @route("/cookie/logout")
  @operationId("logoutCookieSession")
  @useAuth(CookieAuth)
  op cookieLogout(): CookieSessionNoContent<204> | ErrorResponse<401>;
}

@tag("Account")
@route("/v1")
@useAuth(TelDriveAuth)
namespace Account {
  /** Return the authenticated user profile. */
  @get
  @route("/me")
  @operationId("getCurrentUser")
  op getCurrentUser(): JsonResponse<200, UserProfile> | ErrorResponse<401>;

  /** Return the authenticated Telegram user's small profile photo. */
  @get
  @route("/me/photo")
  @operationId("getProfilePhoto")
  op getProfilePhoto():
    | ProfilePhotoResponse
    | EmptyResponse<204>
    | ErrorResponse<401>
    | ErrorResponse<429>
    | ErrorResponse<413>;
  /** Revoke the current session. */
  @post
  @route("/auth/logout")
  @operationId("logoutSession")
  op logout(): EmptyResponse<204> | ErrorResponse<401>;
}

@tag("Users")
@route("/v1/users")
@useAuth(TelDriveAuth)
namespace Users {
  /**
   * Find other accounts by display name, username or exact user ID so a share owner can
   * pick a grantee; at most twenty matches are returned.
   */
  @get
  @route("/search")
  @operationId("searchUsers")
  op search(
    /** Text matched against display name and username, or an exact user ID. */
    @query
    @minLength(1)
    @maxLength(128)
    search: string,
  ):
    | JsonResponse<200, UserSearchResult[]>
    | ErrorResponse<401>
    | ErrorResponse<422>;
}

@tag("Administration")
@route("/v1/admin/users")
@useAuth(TelDriveAuth)
namespace Administration {
  /**
   * List accounts for the admin console, oldest first. The response is a plain
   * array capped at five hundred rows with no cursor, so an installation with more
   * accounts than that has to narrow the list with `search`; every listed account
   * is returned, and the order is stable.
   */
  @get
  @operationId("listAdminUsers")
  op list(@query @maxLength(128) search?: string):
    | JsonResponse<200, AdminUserSummary[]>
    | ErrorResponse<401>
    | ErrorResponse<403>;

  /**
   * Apply a role and/or disabled flag to one account; disabling also revokes its
   * sessions and API keys. Owner accounts answer 403 and an empty body 422.
   */
  @patch
  @route("/{userId}")
  @operationId("updateAdminUser")
  op update(@path userId: int64, @body body: UserAdminUpdateRequest):
    | JsonResponse<200, AdminUserSummary>
    | ErrorResponse<401>
    | ErrorResponse<403>
    | ErrorResponse<404>
    | ErrorResponse<422>;

  /**
   * Revoke every session and API key of one account so its credentials stop working
   * immediately. Owner accounts are refused with 403.
   */
  @post
  @route("/{userId}/revoke-access")
  @operationId("revokeAdminUserAccess")
  op revokeAccess(@path userId: int64):
    | EmptyResponse<204>
    | ErrorResponse<401>
    | ErrorResponse<403>
    | ErrorResponse<404>;
}

@tag("API keys")
@route("/v1/api-keys")
@useAuth(BearerOrCookieAuth)
namespace ApiKeys {
  /**
   * List the account's usable API keys, newest first, as a cursor page; revoked and
   * expired keys are omitted.
   */
  @get
  @operationId("listApiKeys")
  op list(...CursorQuery):
    | JsonResponse<200, CursorPage<ApiKeySummary>>
    | ErrorResponse<401>
    | ErrorResponse<422>;

  /** Mint an API key and return its plaintext secret once, because only a hash is stored. */
  @post
  @operationId("createApiKey")
  op create(@body body: ApiKeyCreateRequest):
    | JsonResponse<201, ApiKeyCreated>
    | ErrorResponse<401>
    | ErrorResponse<409>
    | ErrorResponse<422>;

  /** Revoke one API key; an unknown, foreign or already revoked key answers 404. */
  @delete
  @route("/{apiKeyId}")
  @operationId("revokeApiKey")
  op revoke(@path apiKeyId: Uuid):
    | EmptyResponse<204>
    | ErrorResponse<401>
    | ErrorResponse<404>;
}

@tag("Sessions")
@route("/v1/sessions")
@useAuth(BearerOrCookieAuth)
namespace Sessions {
  /** List all TelDrive sessions for the authenticated Telegram account. */
  @get
  @operationId("listSessions")
  op list(...CursorQuery):
    | JsonResponse<200, CursorPage<SessionSummary>>
    | ErrorResponse<401>
    | ErrorResponse<422>;

  /** Revoke one TelDrive session. Revoking the current session invalidates its bearer token immediately. */
  @delete
  @route("/{sessionId}")
  @operationId("revokeSession")
  op revoke(@path sessionId: Uuid):
    | EmptyResponse<204>
    | ErrorResponse<401>
    | ErrorResponse<404>;
}
