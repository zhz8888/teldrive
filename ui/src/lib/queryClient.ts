import { queryClient } from "@/api/query-client";

/**
 * The application's query cache, for code that runs outside a component: route
 * `beforeLoad` loaders, event handlers and the sign-out path, which call
 * `ensureQueryData`, `invalidateQueries` or `clear` on it. `useQueryClient` is the
 * hook form; `api/query-client.ts` owns the instance and its retry policy.
 */
export function getQueryClient() {
  return queryClient;
}
