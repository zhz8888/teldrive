import { queryOptions } from "@tanstack/react-query";
import { fetchClient } from "@/api/client";
import { unwrap } from "@/api/errors";

/** Cache key of the current-user query, shared by every screen that reads the account. */
const currentUserQueryKey = ["auth", "current-user"] as const;

/**
 * Options for `GET /v1/me`. The root route awaits them in `beforeLoad`, so an
 * expired session turns into the login redirect before a screen renders, and the
 * login page and account screens reuse the same cached entry.
 *
 * Retrying is off: a failure here is an authentication answer rather than a
 * transient fault, and repeating it would delay the redirect to the login page.
 * `queryFn` unwraps the generated result, so a failure surfaces as the `ApiError`
 * that `isUnauthorized` classifies.
 */
export function currentUserQueryOptions() {
  return queryOptions({
    queryKey: currentUserQueryKey,
    queryFn: () => unwrap(fetchClient.GET("/v1/me")),
    staleTime: 30_000,
    gcTime: 10 * 60_000,
    retry: false,
  });
}
