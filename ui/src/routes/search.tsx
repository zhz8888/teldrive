import { createFileRoute } from "@tanstack/react-router";
import { FileManagerPage } from "@/features/files/file-manager";
import { validateDriveSearch } from "@/features/files/search-state";

/**
 * `/search` — drive-wide results. The filters live in the URL, so a search can be
 * shared, reloaded and walked back through history; the page passes them to the
 * same file manager the drive screen uses.
 */
export const Route = createFileRoute("/search")({
  validateSearch: validateDriveSearch,
  component: SearchPage,
});

/**
 * Renders the results pane for the validated criteria. The listing itself is a
 * folder-shaped view over the search scope, so navigating inside a result only
 * rewrites the view mode and leaves the filters alone.
 */
function SearchPage() {
  const search = Route.useSearch();
  const navigate = Route.useNavigate();
  return (
    <FileManagerPage
      location={{ path: "/", query: search.q ?? "", view: search.view ?? "list" }}
      onLocationChange={(location, replace) =>
        void navigate({ search: { ...search, view: location.view }, replace })
      }
      searchMode={{
        criteria: search,
        onChange: (next, replace) => void navigate({ search: next, replace }),
      }}
    />
  );
}
