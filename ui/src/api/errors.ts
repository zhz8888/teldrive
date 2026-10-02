import { t } from "@/lib/i18n";

export type ApiErrorDetails = Record<string, unknown>;

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly details?: ApiErrorDetails;
  readonly requestId?: string;
  readonly retryAfterSeconds?: number;

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

  get retryable() {
    return this.status === 408 || this.status === 425 || this.status === 429 || this.status >= 500;
  }
}

function envelope(
  error: unknown,
): { code?: unknown; message?: unknown; details?: unknown } | undefined {
  if (!error || typeof error !== "object") return undefined;
  const candidate = error as { error?: unknown };
  if (!candidate.error || typeof candidate.error !== "object") return undefined;
  return candidate.error as { code?: unknown; message?: unknown; details?: unknown };
}

export function normalizeApiError(error: unknown, response?: Response): ApiError {
  if (error instanceof ApiError) return error;
  const parsed = envelope(error);
  const status = response?.status ?? 0;
  const requestId = response?.headers.get("X-Request-ID") ?? undefined;
  const retryAfter = Number(response?.headers.get("Retry-After"));
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
    retryAfterSeconds: Number.isFinite(retryAfter) ? retryAfter : undefined,
  });
}

type ApiResult<T> = { data?: T; error?: unknown; response: Response };

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

export function isUnauthorized(error: unknown) {
  return error instanceof ApiError && error.status === 401;
}

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
  const normalized = normalizeApiError(error);
  switch (normalized.status) {
    case 0:
      return t("errors.networkUnreachable");
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
