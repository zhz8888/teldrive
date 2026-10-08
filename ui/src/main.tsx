import { QueryClientProvider } from "@tanstack/react-query";
import { createRouter, RouterProvider } from "@tanstack/react-router";
import ReactDOM from "react-dom/client";
import { Toaster } from "sonner";
import { I18nProvider } from "./lib/i18n";
import { getQueryClient } from "./lib/queryClient";
import { ThemeProvider, useTheme } from "./lib/theme";
import { routeTree } from "./routeTree.gen";
import "./styles/globals.css";

/**
 * The cache every provider and the router context share; imperative callers reach
 * the same instance through `getQueryClient()`.
 */
const queryClient = getQueryClient();

/**
 * The router for the session. `context` hands the query client to `beforeLoad`
 * hooks so a route can await its data, and `defaultPreload: "intent"` starts
 * loading a route as soon as a link is hovered or focused, which is what makes
 * navigation feel instant without eagerly loading every screen.
 */
const router = createRouter({
  routeTree,
  context: { queryClient },
  defaultPreload: "intent",
});

/**
 * Registers this router's type with TanStack Router, so `Link`, `useNavigate` and
 * the generated route hooks are typed against the routes that actually exist.
 */
declare module "@tanstack/react-router" {
  /** TanStack Router's type registry; the property below is its extension point. */
  interface Register {
    /** The router instance every route type is derived from. */
    router: typeof router;
  }
}

// ThemedToaster keeps notifications in the theme the interface is showing. The
// resolved theme is known during the first render, so the toaster starts in the
// mode the interface already displays instead of asking sonner to follow the
// system preference first.
function ThemedToaster() {
  const { resolvedTheme } = useTheme();
  return (
    <Toaster
      position="bottom-right"
      richColors
      closeButton
      theme={resolvedTheme}
      toastOptions={{
        style: {
          background: "var(--overlay)",
          border: "1px solid var(--border)",
          color: "var(--overlay-foreground)",
          backdropFilter: "blur(16px)",
        },
      }}
    />
  );
}

/**
 * Mounts the application. Providers are nested so a screen can read the theme, the
 * locale and the query cache, and the toaster sits inside the theme provider so its
 * notifications follow the displayed mode.
 */
async function startApp() {
  // index.html always renders this mount point before the entry module runs.
  const rootElement = document.getElementById("root")!;

  // The guard skips mounting when the element already has content, so a second
  // evaluation of the module does not create a second React root.
  if (!rootElement.innerHTML) {
    const root = ReactDOM.createRoot(rootElement);
    root.render(
      <QueryClientProvider client={queryClient}>
        <ThemeProvider>
          <I18nProvider>
            <RouterProvider router={router} />
          </I18nProvider>
          <ThemedToaster />
        </ThemeProvider>
      </QueryClientProvider>,
    );
  }
}

startApp();
