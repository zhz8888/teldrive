import { createFileRoute, redirect } from "@tanstack/react-router";

/**
 * `/` — there is no landing page: the root redirects to the drive browser, seeding it
 * with the explicit defaults so the first history entry already carries a full URL.
 */
export const Route = createFileRoute("/")({
  beforeLoad: () => {
    throw redirect({ to: "/files", search: { path: "/", query: "", view: "list" } });
  },
});
