import { QueryClientProvider } from "@tanstack/react-query";
import { createRouter, RouterProvider } from "@tanstack/react-router";
import ReactDOM from "react-dom/client";
import { Toaster } from "sonner";
import { CommandPaletteProvider } from "./components/command-palette-context";
import { I18nProvider } from "./lib/i18n";
import { getQueryClient } from "./lib/queryClient";
import { ThemeProvider, useTheme } from "./lib/theme";
import { routeTree } from "./routeTree.gen";
import "./styles/globals.css";

const queryClient = getQueryClient();

const router = createRouter({
  routeTree,
  context: { queryClient },
  defaultPreload: "intent",
});

declare module "@tanstack/react-router" {
  interface Register {
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

async function startApp() {
  const rootElement = document.getElementById("root")!;

  if (!rootElement.innerHTML) {
    const root = ReactDOM.createRoot(rootElement);
    root.render(
      <QueryClientProvider client={queryClient}>
        <ThemeProvider>
          <I18nProvider>
            <CommandPaletteProvider>
              <RouterProvider router={router} />
            </CommandPaletteProvider>
          </I18nProvider>
          <ThemedToaster />
        </ThemeProvider>
      </QueryClientProvider>,
    );
  }
}

startApp();
