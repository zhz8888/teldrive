import { createFileRoute } from "@tanstack/react-router";
import {
  SharedFileBrowser,
  SharedPageSpinner,
  sharedBrowserSearch,
} from "@/features/files/shared-file-browser";

/**
 * `/shared-with-me` — the shares other users have granted to the signed-in user. Same
 * search vocabulary and browser component as `/shared`, only the listing source and the
 * write permissions differ.
 */
export const Route = createFileRoute("/shared-with-me")({
  validateSearch: sharedBrowserSearch,
  component: SharedWithMePage,
  pendingComponent: SharedPageSpinner,
});

/**
 * Renders the browser in "with-me" mode and pushes every navigation back into the URL.
 */
function SharedWithMePage() {
  const search = Route.useSearch();
  const navigate = Route.useNavigate();

  return (
    <SharedFileBrowser
      mode="with-me"
      search={search}
      navigate={(next, replace) => void navigate({ search: next, replace })}
    />
  );
}
