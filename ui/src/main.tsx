import { QueryClientProvider } from "@tanstack/react-query";
import { createRouter, RouterProvider } from "@tanstack/react-router";
import { ThemeProvider, useTheme } from "next-themes";
import ReactDOM from "react-dom/client";
import { Toaster } from "sonner";
import { CommandPaletteProvider } from "./components/command-palette-context";
import { I18nProvider } from "./lib/i18n";
import { getQueryClient } from "./lib/queryClient";
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
// resolved theme is undefined until next-themes has read the stored choice, so
// the first render asks sonner to follow the system preference instead of
// flashing the wrong mode.
function ThemedToaster() {
  const { resolvedTheme } = useTheme();
  return (
    <Toaster
      position="bottom-right"
      richColors
      closeButton
      theme={resolvedTheme === "dark" ? "dark" : resolvedTheme === "light" ? "light" : "system"}
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
        <ThemeProvider
          attribute={["class", "data-theme"]}
          defaultTheme="system"
          enableSystem
          storageKey="theme"
        >
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
