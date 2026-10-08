import { useSuspenseQuery } from "@tanstack/react-query";
import { currentUserQueryOptions } from "./queries";

/**
 * The signed-in account, suspending until it is available. For screens that cannot
 * render without it (the account settings page, which declares a pending
 * component): the root route's `beforeLoad` has usually already resolved the same
 * query, so the suspension ends on the first render. Screens that can render
 * without the account use `useQuery(currentUserQueryOptions())` instead.
 */
export function useCurrentUser() {
  return useSuspenseQuery(currentUserQueryOptions());
}
