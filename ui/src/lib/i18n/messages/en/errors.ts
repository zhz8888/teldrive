/**
 * English entries of the messages the interface shows when a request fails.
 *
 * Keys are `errors.<name>`. Only the wording the interface itself authors lives
 * here: anything the server returns, and the diagnostics used to investigate a
 * failure, stay in English and are never catalog entries.
 */
export const errors = {
  // The friendly branches of `userMessage()` in `api/errors.ts`. A status that
  // carries a server-authored message keeps it instead of the entry below.
  "errors.networkUnreachable":
    "Teldrive could not reach the server. Check your connection and try again.",
  "errors.badRequest": "The request was not valid.",
  "errors.sessionExpired": "Your sign-in has expired. Sign in again to continue.",
  "errors.forbidden": "You do not have permission to perform this action.",
  "errors.notFound": "The requested item no longer exists.",
  "errors.conflict": "That change conflicts with an existing item.",
  "errors.gone": "This upload or share has expired.",
  "errors.preconditionFailed": "This item changed on another device. Refresh before trying again.",
  "errors.payloadTooLarge": "The selected file is larger than the server allows.",
  "errors.rangeNotSatisfiable": "The requested file range is not available.",
  "errors.unprocessable": "Some values need to be corrected.",
  "errors.tooManyRequests": "Teldrive is receiving too many requests. Try again shortly.",
  "errors.serverError": "Teldrive encountered a server error. Your data was not changed.",
} as const;
