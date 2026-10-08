import { QueryClient } from "@tanstack/react-query";
import { ApiError } from "./errors";

/**
 * The single query client shared by the whole interface. Queries are considered
 * fresh for 20 seconds and kept 10 minutes after their last observer, refetching
 * on focus is off (the app has its own refresh controls, and a background refetch
 * would fight them).
 *
 * Retries are reserved for failures that can heal on their own: an `ApiError`
 * that is not `retryable` (a 4xx decision, a response shape the interface
 * rejected) is surfaced immediately, and only the remaining failures get two more
 * attempts. A `Retry-After` delay is honoured verbatim; otherwise the wait
 * doubles from one second up to eight.
 *
 * Mutations are never retried, because a retried write could duplicate a
 * create/upload that actually succeeded but whose response was lost.
 */
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 20_000,
      gcTime: 10 * 60_000,
      refetchOnWindowFocus: false,
      retry(failures, error) {
        if (error instanceof ApiError && !error.retryable) return false;
        return failures < 2;
      },
      retryDelay(attempt, error) {
        if (error instanceof ApiError && error.retryAfterSeconds)
          return error.retryAfterSeconds * 1000;
        return Math.min(1000 * 2 ** attempt, 8000);
      },
    },
    mutations: { retry: false },
  },
});
