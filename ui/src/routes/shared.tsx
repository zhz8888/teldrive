import { createFileRoute } from "@tanstack/react-router";
import {
  SharedFileBrowser,
  SharedPageSpinner,
  sharedBrowserSearch,
} from "@/features/files/shared-file-browser";

/**
 * `/shared` — the shares the signed-in user has handed out. Search parameters are the
 * shared-browser vocabulary and are validated by `sharedBrowserSearch`, so a deep link
 * into a shared folder survives a reload.
 */
export const Route = createFileRoute("/shared")({
  validateSearch: sharedBrowserSearch,
  component: SharedPage,
  pendingComponent: SharedPageSpinner,
});

/** Renders the browser in "shared" mode and pushes every navigation back into the URL. */
function SharedPage() {
  const search = Route.useSearch();
  const navigate = Route.useNavigate();

  return (
    <SharedFileBrowser
      mode="shared"
      search={search}
      navigate={(next, replace) => void navigate({ search: next, replace })}
    />
  );
}
