import { t } from "@/lib/i18n";

/** Free-form server payload from `error.details`, whose shape is defined per error code. */
export type ApiErrorDetails = Record<string, unknown>;

/**
 * A failure the interface can render: everything the API layer throws is one of
 * these (or an abort), so screens can branch on `status`/`code` instead of
 * inspecting responses. Instances carry the request id and any server-requested
 * retry delay, which is what makes an error reportable and a retry schedulable.
 */
export class ApiError extends Error {
  /** HTTP status, or 0 when no response arrived (offline, DNS, connection reset). */
  readonly status: number;
  /** Machine-readable code: the server's `error.code`, `http_<status>`, or `network_error`. */
  readonly code: string;
  /** Structured server payload, keyed by `code`. */
  readonly details?: ApiErrorDetails;
  /** Server's `X-Request-ID`, quoted when a failure is reported. */
  readonly requestId?: string;
  /** Server-requested wait from `Retry-After`, capped at one hour. */
  readonly retryAfterSeconds?: number;

  /**
   * Fills in a failure from the fields above. Prefer `normalizeApiError`, which
   * derives them from a response; construct directly only where the failure is
   * authored without one (the upload transfer, which reports its own network
   * errors).
   */
  constructor({
    status,
    code,
    message,
    details,
    requestId,
    retryAfterSeconds,
  }: {
    status: number;
    code: string;
    message: string;
    details?: ApiErrorDetails;
    requestId?: string;
    retryAfterSeconds?: number;
  }) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.details = details;
    this.requestId = requestId;
    this.retryAfterSeconds = retryAfterSeconds;
  }

  /**
   * Whether repeating the request could plausibly succeed: a timeout, a too-early
   * request, a rate limit or a server fault. Anything else is an answer that would
   * not change, so the query client must not retry it. A request that never reached
   * the server carries status 0 and is therefore not retried either.
   */
  get retryable() {
    return this.status === 408 || this.status === 425 || this.status === 429 || this.status >= 500;
  }
}

/**
 * Extracts the `error` envelope of a response body, or undefined when the body is
 * not an object carrying one. The input is whatever reached `normalizeApiError` —
 * a rejected `fetch`, or a parsed payload that need not match the envelope — so
 * each field is returned untyped for the caller to check.
 */
function envelope(
  error: unknown,
): { code?: unknown; message?: unknown; details?: unknown } | undefined {
  if (!error || typeof error !== "object") return undefined;
  const candidate = error as { error?: unknown };
  if (!candidate.error || typeof candidate.error !== "object") return undefined;
  return candidate.error as { code?: unknown; message?: unknown; details?: unknown };
}

/**
 * Caps how long a `Retry-After` may park an action. A server that answers with a
 * far-future date would otherwise disable the control for as long as the tab
 * lives.
 */
const MAX_RETRY_AFTER_SECONDS = 60 * 60;

/**
 * Parses a `Retry-After` header into seconds. The header is either a delay in
 * seconds or an HTTP date, and a missing, empty or unparsable value stays
 * undefined rather than becoming zero, which would read as "retry immediately".
 */
function parseRetryAfter(value: string | null | undefined): number | undefined {
  if (value == null) return undefined;
  const trimmed = value.trim();
  if (trimmed === "") return undefined;
  const seconds = Number(trimmed);
  const delay = Number.isFinite(seconds) ? seconds : (Date.parse(trimmed) - Date.now()) / 1000;
  if (!Number.isFinite(delay) || delay <= 0) return undefined;
  return Math.min(delay, MAX_RETRY_AFTER_SECONDS);
}

/**
 * Wraps a failure as an `ApiError`. A `response` makes it an HTTP failure; the
 * wrapper in `api/client.ts` calls this without one when `fetch` itself rejects,
 * which is the only case that carries the `network_error` code a caller may use
 * to tell an unreachable server from an error the interface raised locally.
 */
export function normalizeApiError(error: unknown, response?: Response): ApiError {
  if (error instanceof ApiError) return error;
  const parsed = envelope(error);
  const status = response?.status ?? 0;
  const requestId = response?.headers.get("X-Request-ID") ?? undefined;
  const retryAfterSeconds = parseRetryAfter(response?.headers.get("Retry-After"));
  const fallback =
    error instanceof Error
      ? error.message
      : status
        ? `Request failed with status ${status}`
        : "Network request failed";
  return new ApiError({
    status,
    code:
      typeof parsed?.code === "string" ? parsed.code : status ? `http_${status}` : "network_error",
    message: typeof parsed?.message === "string" ? parsed.message : fallback,
    details:
      parsed?.details && typeof parsed.details === "object"
        ? (parsed.details as ApiErrorDetails)
        : undefined,
    requestId,
    retryAfterSeconds,
  });
}

/**
 * The shape `openapi-fetch` returns for one call. `data` and `error` are mutually
 * exclusive and `response` is always present; `unwrap` is the usual reader.
 */
type ApiResult<T> = {
  /** Decoded success body; undefined for a 204 and whenever the call failed. */
  data?: T;
  /**
   * Failure body, filled only when `openapi-fetch` itself classifies the response
   * as failed. The wrapped fetch in `api/client.ts` throws before that, so this
   * stays undefined for the calls that go through it.
   */
  error?: unknown;
  /** The response itself, needed for the status, headers and empty-body cases. */
  response: Response;
};

/**
 * Turns a generated-client result into the body, or throws the matching
 * `ApiError`. A 2xx without a body is accepted only for 204: any other success
 * missing its payload means the server and the OpenAPI contract disagree, which is
 * reported as `invalid_response` rather than handed on as undefined data.
 */
export async function unwrap<T>(result: ApiResult<T> | Promise<ApiResult<T>>): Promise<T> {
  const { data, error, response } = await result;
  if (error !== undefined || !response.ok) throw normalizeApiError(error, response);
  if (data === undefined && response.status !== 204) {
    throw new ApiError({
      status: response.status,
      code: "invalid_response",
      message: "The server returned an incomplete response.",
    });
  }
  return data as T;
}

/** Whether a failure is the server refusing an absent or expired session. */
export function isUnauthorized(error: unknown) {
  return error instanceof ApiError && error.status === 401;
}

/**
 * Builds the error for a 2xx response the interface refuses to use: the body
 * parsed but does not match the OpenAPI contract (a listing without ids, an empty
 * page where entries were expected). It keeps the success status so callers can
 * tell this mismatch from a request the server rejected, and the message names
 * what was wrong for the reader.
 */
export function invalidResponse(
  message = "The server returned data that does not match the current OpenAPI contract.",
) {
  return new ApiError({ status: 200, code: "invalid_response", message });
}

/**
 * Turns a failure into wording for the interface. Server-authored messages and
 * the diagnostics of `normalizeApiError` are returned unchanged; the branches
 * below are the phrases the interface writes itself, so they are translated.
 */
export function userMessage(error: unknown): string {
  // Only a request that never reached the server is a connectivity problem.
  // An error the interface raised itself (`"name" already exists and is not a
  // folder`, a refused clipboard write) keeps the wording it was created with;
  // the request wrapper reports its own network failures as an `ApiError`.
  if (error instanceof Error && !(error instanceof ApiError)) return error.message;
  const normalized = normalizeApiError(error);
  switch (normalized.status) {
    case 0:
      return normalized.code === "network_error"
        ? t("errors.networkUnreachable")
        : normalized.message;
    case 400:
      return normalized.message || t("errors.badRequest");
    case 401:
      return t("errors.sessionExpired");
    case 403:
      return t("errors.forbidden");
    case 404:
      return t("errors.notFound");
    case 409:
      return normalized.message || t("errors.conflict");
    case 410:
      return t("errors.gone");
    case 412:
      return t("errors.preconditionFailed");
    case 413:
      return t("errors.payloadTooLarge");
    case 416:
      return t("errors.rangeNotSatisfiable");
    case 422:
      return normalized.message || t("errors.unprocessable");
    case 429:
      return t("errors.tooManyRequests");
    default:
      return normalized.status >= 500 ? t("errors.serverError") : normalized.message;
  }
}
